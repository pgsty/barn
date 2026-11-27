#!/bin/bash
set -Eeuo pipefail

# __BARN_SHARE_CONFIG__
fstab=/etc/fstab
fstab_tmp=
active_probe=

cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n "${active_probe}" ]]; then
    rm -f -- "${active_probe}" || true
  fi
  if [[ -n "${fstab_tmp}" ]]; then
    rm -f -- "${fstab_tmp}" || true
  fi
  exit "${status}"
}
trap cleanup EXIT

path_overlap() {
  local left="$1" right="$2"
  [[ "${left}" == "${right}" || "${left}" == "${right}"/* || "${right}" == "${left}"/* ]]
}

safe_guest_mount() {
  local path="$1" root canonical
  [[ "${path}" == /* && "${path}" != / && "${path}" != *[[:cntrl:]]* ]]
  canonical=$(realpath -m -- "${path}")
  [[ "${canonical}" == "${path}" ]]
  for root in /bin /boot /dev /etc /lib /lib64 /proc /root /run /sbin /sys /usr /var/lib/barn; do
    if path_overlap "${path}" "${root}"; then
      return 1
    fi
  done
  if path_overlap "${path}" "/home/${share_user}/.ssh"; then
    return 1
  fi
  return 0
}

fstab_unescape() {
  local value="$1"
  value="${value//\\040/ }"
  value="${value//\\011/$'\t'}"
  REPLY="${value//\\134/\\}"
}

for required in awk findmnt grep mktemp mount mountpoint realpath runuser stat sync umount; do
  command -v "${required}" >/dev/null
done
[[ -f "${fstab}" && ! -L "${fstab}" ]]
[[ "$(stat -c '%u:%g' "${fstab}")" == "0:0" ]]
fstab_mode=$(stat -c '%a' "${fstab}")
[[ "${fstab_mode}" =~ ^[0-7]{3,4}$ ]]
(( (8#${fstab_mode} & 8#22) == 0 ))
begin_count=$(grep -Fxc -- "${begin_marker}" "${fstab}" || true)
end_count=$(grep -Fxc -- "${end_marker}" "${fstab}" || true)
if (( begin_count != end_count || begin_count > 1 )); then
  echo 'malformed Barn share block in /etc/fstab' >&2
  exit 1
fi

declare -a old_shares=()
if (( begin_count == 1 )); then
  begin_line=$(grep -Fn -- "${begin_marker}" "${fstab}" | cut -d: -f1)
  end_line=$(grep -Fn -- "${end_marker}" "${fstab}" | cut -d: -f1)
  if (( begin_line >= end_line )); then
    echo 'misordered Barn share block in /etc/fstab' >&2
    exit 1
  fi
  while read -r tag guest fstype options dump pass extra; do
    if [[ -z "${tag}" || -n "${extra:-}" || ! "${tag}" =~ ^barn-[0-9a-f]{20}$ || "${fstype}" != 9p || "${dump}" != 0 || "${pass}" != 0 ]]; then
      echo 'unsafe entry in Barn share block' >&2
      exit 1
    fi
    fstab_unescape "${guest}"
    guest="${REPLY}"
    normalized_options=${options/access=any/access=client}
    if [[ "${normalized_options}" != "version=9p2000.L,trans=virtio,cache=none,msize=262144,access=client,nofail,nodev,nosuid,rw" && "${normalized_options}" != "version=9p2000.L,trans=virtio,cache=none,msize=262144,access=client,nofail,nodev,nosuid,ro" ]]; then
      echo 'unexpected options in Barn share block' >&2
      exit 1
    fi
    safe_guest_mount "${guest}" || { echo "unsafe old Barn share mount ${guest}" >&2; exit 1; }
    old_shares+=("${tag}|${guest}")
  done < <(awk -v begin="${begin_marker}" -v end="${end_marker}" '
    $0 == begin { inside=1; next }
    $0 == end { inside=0; next }
    inside { print }
  ' "${fstab}")
fi

for old_share in "${old_shares[@]}"; do
  old_tag=${old_share%%|*}
  old_guest=${old_share#*|}
  if ! mountpoint -q "${old_guest}"; then
    continue
  fi
  mounted_source=$(findmnt -n -o SOURCE --target "${old_guest}")
  mounted_type=$(findmnt -n -o FSTYPE --target "${old_guest}")
  if [[ "${mounted_type}" == 9p ]] && is_desired_share "${mounted_source}" "${old_guest}"; then
    continue
  fi
  if [[ "${mounted_source}" != "${old_tag}" || "${mounted_type}" != 9p ]] || is_desired_share "${old_tag}" "${old_guest}"; then
    echo "refuse to unmount unexpected filesystem at ${old_guest}" >&2
    exit 1
  fi
  umount "${old_guest}"
done

fstab_tmp=$(mktemp /etc/.fstab.barn-shares.XXXXXX)
awk -v begin="${begin_marker}" -v end="${end_marker}" '
  $0 == begin { inside=1; next }
  $0 == end { inside=0; next }
  !inside { print }
' "${fstab}" > "${fstab_tmp}"
printf '%s\n' "${begin_marker}" >> "${fstab_tmp}"
# __BARN_SHARE_FSTAB__
printf '%s\n' "${end_marker}" >> "${fstab_tmp}"
chown root:root "${fstab_tmp}"
chmod 0644 "${fstab_tmp}"
sync -f "${fstab_tmp}"
mv -f -- "${fstab_tmp}" "${fstab}"
fstab_tmp=

has_mount_option() {
  local options="$1" wanted="$2"
  [[ ",${options}," == *,"${wanted}",* ]]
}

verify_mount() {
  local tag="$1" mountpoint="$2" readonly="$3"
  local mounted_source mounted_type mounted_options
  mountpoint -q "${mountpoint}"
  mounted_source=$(findmnt -n -o SOURCE --target "${mountpoint}")
  mounted_type=$(findmnt -n -o FSTYPE --target "${mountpoint}")
  mounted_options=$(findmnt -n -o OPTIONS --target "${mountpoint}")
  [[ "${mounted_source}" == "${tag}" && "${mounted_type}" == 9p ]]
  has_mount_option "${mounted_options}" nodev
  has_mount_option "${mounted_options}" nosuid
  if [[ "${readonly}" == true ]]; then
    has_mount_option "${mounted_options}" ro
  else
    has_mount_option "${mounted_options}" rw
  fi
}

probe_share() {
  local mountpoint="$1" readonly="$2"
  runuser -u "${share_user}" -- test -r "${mountpoint}" || return 1
  runuser -u "${share_user}" -- test -x "${mountpoint}" || return 1
  if [[ "${readonly}" == true ]]; then
    if active_probe=$(runuser -u "${share_user}" -- mktemp "${mountpoint}/.barn-write-probe.XXXXXX" 2>/dev/null); then
      if ! rm -f -- "${active_probe}" || [[ -e "${active_probe}" || -L "${active_probe}" ]]; then
        echo "failed to clean unexpected read-only share probe ${active_probe}" >&2
        return 1
      fi
      active_probe=
      echo "read-only Barn share is writable by ${share_user}: ${mountpoint}" >&2
      return 1
    fi
    active_probe=
    return 0
  fi

  active_probe=$(runuser -u "${share_user}" -- mktemp "${mountpoint}/.barn-write-probe.XXXXXX") || return 1
  runuser -u "${share_user}" -- /bin/sh -c 'printf "%s\n" barn-share-probe > "$1"' barn-probe "${active_probe}" || return 1
  runuser -u "${share_user}" -- rm -f -- "${active_probe}" || return 1
  [[ ! -e "${active_probe}" && ! -L "${active_probe}" ]] || return 1
  active_probe=
}

check_share_access() {
  local tag="$1" mountpoint="$2" readonly="$3"
  if probe_share "${mountpoint}" "${readonly}"; then
    return 0
  fi
  if [[ -n "${active_probe}" ]]; then
    rm -f -- "${active_probe}"
    active_probe=
  fi
  if [[ "${readonly}" == false ]]; then
    mount -o remount,ro "${mountpoint}"
    verify_mount "${tag}" "${mountpoint}" true
    # Keep the actual fallback across reboots. A subsequent finalize rewrites
    # the desired options and retries writable access.
    fstab_tmp=$(mktemp /etc/.fstab.barn-shares.XXXXXX)
    awk -v tag="${tag}" -v begin="${begin_marker}" -v end="${end_marker}" '
      $0 == begin { inside=1 }
      $0 == end { inside=0 }
      inside && $1 == tag && $3 == "9p" { sub(/,rw$/, ",ro", $4) }
      { print }
    ' "${fstab}" >"${fstab_tmp}"
    chown root:root "${fstab_tmp}"
    chmod 0644 "${fstab_tmp}"
    sync -f "${fstab_tmp}"
    mv -f -- "${fstab_tmp}" "${fstab}"
    fstab_tmp=
    if runuser -u "${share_user}" -- test -r "${mountpoint}" && runuser -u "${share_user}" -- test -x "${mountpoint}"; then
      echo "${mountpoint}: read-only; ${share_user} cannot write with the host directory's permissions" >&2
      return 1
    fi
  fi
  echo "${mountpoint}: unavailable to ${share_user}; check host directory permissions" >&2
  return 1
}

init_share() {
  local tag="$1" mountpoint="$2" readonly="$3"
  safe_guest_mount "${mountpoint}"
  if [[ -e "${mountpoint}" || -L "${mountpoint}" ]]; then
    [[ -d "${mountpoint}" && ! -L "${mountpoint}" ]]
  else
    install -d -o root -g root -m 0755 "${mountpoint}"
  fi
  [[ "$(readlink -f -- "${mountpoint}")" == "${mountpoint}" ]]
  if mountpoint -q "${mountpoint}"; then
    # A previous boot may have downgraded this mount. Retry the requested
    # access after the user fixes permissions, without changing host metadata.
    mounted_source=$(findmnt -n -o SOURCE --target "${mountpoint}")
    mounted_type=$(findmnt -n -o FSTYPE --target "${mountpoint}")
    [[ "${mounted_source}" == "${tag}" && "${mounted_type}" == 9p ]]
    mounted_options=$(findmnt -n -o OPTIONS --target "${mountpoint}")
    if ! has_mount_option "${mounted_options}" access=client; then
      umount "${mountpoint}"
      mount "${mountpoint}"
    fi
    if [[ "${readonly}" == false ]]; then
      mount -o remount,rw "${mountpoint}"
    fi
    verify_mount "${tag}" "${mountpoint}" "${readonly}"
    check_share_access "${tag}" "${mountpoint}" "${readonly}"
    return 0
  fi
  if find "${mountpoint}" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
    echo "refuse non-empty Barn share mountpoint ${mountpoint}" >&2
    return 1
  fi
  mount "${mountpoint}"
  verify_mount "${tag}" "${mountpoint}" "${readonly}"
  check_share_access "${tag}" "${mountpoint}" "${readonly}"
}

# __BARN_SHARE_CALLS__
