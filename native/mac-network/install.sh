#!/bin/bash
# Reviewed administrator entry point. Install once per Farrow Mac data root.
# It does not change Linux socket_vmnet, sudoers, or existing VM configuration.
set -euo pipefail
uninstall=false
if [[ ${1:-} == --uninstall ]]; then uninstall=true; shift; fi
if [[ $# != 4 || $1 != --binary || $3 != --config ]]; then
  echo "usage: sudo install.sh [--uninstall] --binary ABSOLUTE_BINARY --config ABSOLUTE_CONFIG" >&2
  exit 64
fi
if [[ $EUID != 0 ]]; then
  echo "Administrator authorization is required to install the Mac network daemon." >&2
  exit 77
fi
source_binary=$2
source_config=$4
[[ $source_binary == /* && $source_config == /* && -f $source_binary && -f $source_config ]] || exit 64

# Only these fixed administrator-owned directories are writable by this script.
ensure_directory() {
  local directory=$1
  if [[ -L $directory ]]; then echo "Refusing symlink directory: $directory" >&2; exit 77; fi
  if [[ ! -e $directory ]]; then /usr/bin/install -d -o root -g wheel -m 0755 "$directory"; fi
  [[ -d $directory ]] || exit 77
  local owner mode
  owner=$(/usr/bin/stat -f %u "$directory")
  mode=$(/usr/bin/stat -f %Lp "$directory")
  if [[ $owner != 0 ]] || (( (8#$mode & 0022) != 0 )); then
    echo "Refusing writable or non-root directory: $directory" >&2; exit 77
  fi
}
wait_service_removed() {
  local service_label=$1
  local _attempt result
  for _attempt in {1..100}; do
    if /bin/launchctl print "system/$service_label" >/dev/null 2>&1; then
      /bin/sleep 0.1
    else
      result=$?
      # 113 is launchctl's service-not-found result. Other read failures do
      # not establish removal and must not authorize file replacement.
      if [[ $result == 113 ]]; then return 0; fi
      return 75
    fi
  done
  return 75
}
ensure_directory /Library
ensure_directory "/Library/Application Support"
ensure_directory "/Library/Application Support/Farrow"
ensure_directory "/Library/Application Support/Farrow/mac-network"
ensure_directory /Library/PrivilegedHelperTools
ensure_directory /Library/LaunchDaemons
ensure_directory /var/run/farrow-mac
ensure_directory /var/log/farrow-mac

staging=$(/usr/bin/mktemp -d "/Library/Application Support/Farrow/mac-network/.install.XXXXXXXX")
repair_pid=
admin_pid=
release_repair_lock() {
  if [[ -n $repair_pid ]]; then
    exec 9>&-
    wait "$repair_pid" || true
    repair_pid=
  fi
}
cleanup() {
  release_repair_lock
  if [[ -n $admin_pid ]]; then exec 8>&-; wait "$admin_pid" || true; fi
  /bin/rm -rf "$staging"
}
trap cleanup EXIT
# Copy before execution/validation. Subsequent work reads only this root-owned
# snapshot, eliminating source-directory replacement races during installation.
/usr/bin/install -o root -g wheel -m 0755 "$source_binary" "$staging/helper"
/usr/bin/install -o root -g wheel -m 0600 "$source_config" "$staging/config.json"
"$staging/helper" validate-config --config "$staging/config.json" > "$staging/validated.json"
owner_uid=$(/usr/bin/plutil -extract uid raw -o - "$staging/validated.json")
installation_id=$(/usr/bin/plutil -extract installation_id raw -o - "$staging/validated.json")
socket=$(/usr/bin/plutil -extract socket raw -o - "$staging/validated.json")
label=$(/usr/bin/plutil -extract label raw -o - "$staging/validated.json")
[[ $owner_uid =~ ^[0-9]+$ && $installation_id =~ ^[0-9a-f-]{36}$ ]] || exit 65
# A sudo caller may install only its own network. An interactive root/admin
# Authorization Services installer has no SUDO_UID and uses the validated UID.
if [[ -n ${SUDO_UID:-} && $SUDO_UID != 0 && $SUDO_UID != "$owner_uid" ]]; then
  echo "The network UID does not match the invoking user." >&2; exit 77
fi
uid_root="/Library/Application Support/Farrow/mac-network/$owner_uid"
config_root="$uid_root/$installation_id"
ensure_directory "$uid_root"
ensure_directory "$config_root"
binary=/Library/PrivilegedHelperTools/io.pgsty.farrow.mac-network
config="$config_root/config.json"
plist="/Library/LaunchDaemons/$label.plist"
log="/var/log/farrow-mac/$owner_uid-$installation_id.log"
for destination in "$binary" "$config" "$plist" "$log"; do
  if [[ -L $destination ]]; then echo "Refusing symlink destination: $destination" >&2; exit 77; fi
  if [[ -e $destination ]]; then
    owner=$(/usr/bin/stat -f %u "$destination")
    mode=$(/usr/bin/stat -f %Lp "$destination")
    if [[ $owner != 0 || ! -f $destination ]] || (( (8#$mode & 0022) != 0 )); then
      echo "Refusing unsafe destination: $destination" >&2; exit 77
    fi
  fi
done
hold_lease() {
  local property=$1
  shift
  local fifo="$staging/$property-input" ready_file="$staging/$property-ready.json" lease_pid
  /bin/rm -f "$ready_file"
  /usr/bin/mkfifo -m 0600 "$fifo"
  if [[ $property == admin_lock_held ]]; then
    exec 8<> "$fifo"
    "$staging/helper" "$@" < "$fifo" 8>&- 9>&- > "$ready_file" &
    admin_pid=$!
    lease_pid=$admin_pid
  else
    exec 9<> "$fifo"
    "$staging/helper" "$@" < "$fifo" 8>&- 9>&- > "$ready_file" &
    repair_pid=$!
    lease_pid=$repair_pid
  fi
  local ready=false
  for _attempt in {1..30}; do
    if [[ -s $ready_file ]]; then
      if [[ $(/usr/bin/plutil -extract "$property" raw -o - "$ready_file" 2>/dev/null || true) == true ]]; then ready=true; fi
      break
    fi
    if ! kill -0 "$lease_pid" 2>/dev/null; then break; fi
    /bin/sleep 0.1
  done
  if [[ $ready != true ]]; then
    echo "Network safety lease could not be acquired; refusing to stop the daemon." >&2
    /bin/cat "$ready_file" >&2
    exit 75
  fi
}
# Serialize administrative transactions for this installation, including the
# interval after the old daemon exits. The lock inode is intentionally retained.
hold_lease admin_lock_held admin-lock --config "$staging/config.json" --socket "$socket"
# Changing a live network's subnet/MAC reservations is never implicit.
if [[ -f $config ]] && ! /usr/bin/cmp -s "$config" "$staging/config.json"; then
  echo "Existing network configuration differs; keep the saved subnet and slot identities." >&2
  exit 65
fi
running=false
if /bin/launchctl print "system/$label" >/dev/null 2>&1; then
  running=true
else
  code=$?
  if [[ $code != 113 ]]; then echo "Cannot establish network service state; no changes made." >&2; exit 75; fi
fi
if [[ $uninstall == true && ( $running == true || -f $plist ) && ! -f $config ]]; then
  echo "Installed network configuration is missing; cannot authenticate this installation for removal." >&2; exit 65
fi
replace_binary=true
if [[ -f $binary ]] && /usr/bin/cmp -s "$binary" "$staging/helper"; then replace_binary=false; fi
"$staging/helper" --version > "$staging/version.json"
expected_build=$(/usr/bin/plutil -extract build_id raw -o - "$staging/version.json")
if [[ $running == true ]]; then
  if "$staging/helper" status --socket "$socket" > "$staging/status.json"; then
    running_build=$(/usr/bin/plutil -extract build_id raw -o - "$staging/status.json" 2>/dev/null || true)
    if [[ $uninstall == false && $replace_binary == false && $running_build == "$expected_build" ]]; then /bin/cat "$staging/status.json"; exit 0; fi
    first=$(/usr/bin/plutil -extract connected.0 raw -o - "$staging/status.json")
    second=$(/usr/bin/plutil -extract connected.1 raw -o - "$staging/status.json")
    if [[ $first == true || $second == true ]]; then
      echo "Stop both Farrow Mac slots before upgrading or uninstalling the network helper." >&2; exit 75
    fi
    # Confirmation and exclusion of new attaches are atomic on the daemon's
    # network queue. A status-only check followed by bootout has an up race.
    hold_lease maintenance_lease_held maintenance --socket "$socket"
  else
    # Hold the same exclusive lock as serve before looking at launchd. A
    # scheduled restart cannot bring up a network between this check and
    # bootout. A live daemon's lock makes this operation fail safely.
    hold_lease repair_lock_held repair-lock --config "$config" --socket "$socket"
    if ! /bin/launchctl print "system/$label" > "$staging/launch-state.txt" 2>&1; then
      echo "Could not verify the registered network service state; repair refused." >&2
      exit 75
    fi
    live_pid=$(/usr/bin/awk '$1 == "pid" && $2 == "=" && $3 ~ /^[1-9][0-9]*$/ { print $3 }' "$staging/launch-state.txt")
    if [[ -n $live_pid ]]; then
      echo "Registered network daemon still has PID $live_pid; repair refused to protect running VMs." >&2
      exit 75
    fi
    echo "Repairing an exited network daemon (no launchd PID; exclusive network lock held)." >&2
  fi
  /bin/launchctl bootout "system/$label"
  # bootout returns before the daemon's graceful shutdown and launchd's label
  # removal finish. Bootstrapping immediately can report EIO while launchd's
  # actual error is EALREADY (operation already in progress).
  if ! wait_service_removed "$label"; then
    echo "The idle network daemon has not finished removal; refusing overlapping replacement." >&2
    exit 75
  fi
fi
if [[ $uninstall == true ]]; then
  # Exclude an unregistered daemon too. Do not remove a lock inode while held;
  # another administrative transaction must continue to see this exact lock.
  if [[ -z $repair_pid ]]; then
    hold_lease repair_lock_held repair-lock --config "$staging/config.json" --socket "$socket"
  else
    release_repair_lock
    /bin/rm -f "$staging/repair_lock_held-input"
    hold_lease repair_lock_held repair-lock --config "$staging/config.json" --socket "$socket"
  fi
  if [[ -e $socket || -L $socket ]]; then
    if [[ -L $socket || ! -S $socket || $(/usr/bin/stat -f %u "$socket") != "$owner_uid" ]]; then
      echo "Unexpected network socket path; removal refused." >&2; exit 77
    fi
    /bin/rm -f "$socket"
  fi
  /bin/rm -f "$plist" "$config"
  /bin/rmdir "$config_root"
  # Shared executable, logs and lock inodes are retained. Other installations
  # may use the executable now or on their next launch.
  printf '{"ok":true,"uninstalled":true,"installation_id":"%s","shared_binary_retained":true,"log_retained":true}\n' "$installation_id"
  exit 0
fi
/usr/bin/install -o root -g wheel -m 0755 "$staging/helper" "$staging/published-helper"
/bin/mv -f "$staging/published-helper" "$binary"
/usr/bin/install -o root -g wheel -m 0600 "$staging/config.json" "$config"
# Every interpolation below is fixed, numeric, or UUID-validated above.
/bin/cat > "$staging/service.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>$label</string>
<key>ProgramArguments</key><array><string>$binary</string><string>serve</string><string>--config</string><string>$config</string><string>--socket</string><string>$socket</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>ThrottleInterval</key><integer>30</integer>
<key>ProcessType</key><string>Background</string>
<key>StandardOutPath</key><string>$log</string>
<key>StandardErrorPath</key><string>$log</string>
<key>Umask</key><integer>63</integer>
</dict></plist>
EOF
/usr/bin/plutil -lint "$staging/service.plist" >&2
/usr/bin/install -o root -g wheel -m 0644 "$staging/service.plist" "$plist"
release_repair_lock
/bin/launchctl bootstrap system "$plist"
for _attempt in {1..30}; do
  if "$binary" status --socket "$socket" > "$staging/status.json"; then
    /bin/cat "$staging/status.json"
    exit 0
  fi
  /bin/sleep 1
done
echo "Network daemon was installed but did not become ready. Inspect $log." >&2
exit 75
