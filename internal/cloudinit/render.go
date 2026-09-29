// Package cloudinit renders the validated NoCloud seed payload and ISO.
package cloudinit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pgsty/barn/internal/spec"

	"github.com/pgsty/barn/internal/naming"
	"golang.org/x/crypto/ssh"
)

type Host struct {
	Name    string
	Address string
}

type Disk struct {
	Fresh      bool
	Serial     string
	Mount      string
	Filesystem string
}

// Share is one QEMU virtio-9p export mounted inside the guest. Tag is a
// Barn-generated stable identity, never a user-selected mount option.
type Share struct {
	Tag      string
	Guest    string
	Readonly bool
}

type PrivateNetwork struct {
	MAC         string
	Address     string
	Prefix      int
	HostAddress string
}

type Input struct {
	Node       string
	Hostname   string
	Generation uint64
	SpecHash   string
	SSHUser    string
	PublicKey  string
	PrivateKey string
	Control    bool
	MgmtMAC    string
	Private    *PrivateNetwork
	Hosts      []Host
	Disks      []Disk
	Shares     []Share
}

// Files are the only three entries allowed at the seed root.
type Files struct {
	MetaData      []byte
	UserData      []byte
	NetworkConfig []byte
}

var (
	dnsNamePattern  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
	serialPattern   = regexp.MustCompile(`^[a-z2-7]{20}$`)
	shareTagPattern = regexp.MustCompile(`^barn-[0-9a-f]{20}$`)
	hashPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	userPattern     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	mountPattern    = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
)

func mountPathsOverlap(left, right string) bool {
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func reservedMount(path string) bool {
	for _, root := range []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/proc", "/root", "/run", "/sbin", "/sys", "/usr", "/var/lib/barn"} {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

func safeMount(path string) bool {
	return mountPattern.MatchString(path) && filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !reservedMount(path)
}

func safeShareMount(path, user string) bool {
	if !mountPattern.MatchString(path) || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return false
	}
	for _, reserved := range []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/proc", "/root", "/run", "/sbin", "/sys", "/usr", "/var/lib/barn", filepath.Join("/home", user, ".ssh")} {
		if mountPathsOverlap(path, reserved) {
			return false
		}
	}
	return true
}

func singleLine(label, value string) error {
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s must be a non-empty single line", label)
	}
	return nil
}

func validateInput(input Input) error {
	if !naming.ValidNodeName(input.Node) || !naming.ValidNodeName(input.Hostname) {
		return errors.New("node and hostname must be DNS labels")
	}
	if input.Generation == 0 {
		return errors.New("cloud-init generation must be positive")
	}
	if !hashPattern.MatchString(input.SpecHash) {
		return errors.New("spec hash must be 64 lowercase hexadecimal characters")
	}
	if !userPattern.MatchString(input.SSHUser) {
		return fmt.Errorf("invalid SSH user %q", input.SSHUser)
	}
	if err := singleLine("SSH public key", input.PublicKey); err != nil {
		return err
	}
	if !strings.HasPrefix(input.PublicKey, "ssh-ed25519 ") {
		return errors.New("M0 requires an Ed25519 SSH public key")
	}
	if _, err := net.ParseMAC(input.MgmtMAC); err != nil {
		return fmt.Errorf("invalid management MAC: %w", err)
	}
	if input.Private != nil {
		if _, err := net.ParseMAC(input.Private.MAC); err != nil {
			return fmt.Errorf("invalid private MAC: %w", err)
		}
		ip := net.ParseIP(input.Private.Address)
		hostIP := net.ParseIP(input.Private.HostAddress)
		if ip == nil || hostIP == nil || input.Private.Prefix < 1 || input.Private.Prefix > 32 {
			return errors.New("invalid private IPv4 address/prefix")
		}
		if ip.To4() == nil || hostIP.To4() == nil || ip.Equal(hostIP) {
			return errors.New("private network must be IPv4")
		}
		mask := net.CIDRMask(input.Private.Prefix, 32)
		if !ip.Mask(mask).Equal(hostIP.Mask(mask)) {
			return errors.New("private guest and host addresses are not in the same subnet")
		}
	}
	seenHost := make(map[string]struct{})
	for _, host := range input.Hosts {
		if !dnsNamePattern.MatchString(host.Name) || strings.Contains(host.Name, "..") || net.ParseIP(host.Address) == nil {
			return fmt.Errorf("invalid host mapping %q=%q", host.Name, host.Address)
		}
		if _, exists := seenHost[host.Name]; exists {
			return fmt.Errorf("duplicate host name %q", host.Name)
		}
		seenHost[host.Name] = struct{}{}
	}
	seenMount := make(map[string]struct{})
	seenSerial := make(map[string]struct{})
	for _, disk := range input.Disks {
		if !serialPattern.MatchString(disk.Serial) {
			return fmt.Errorf("invalid data disk serial %q", disk.Serial)
		}
		if !safeMount(disk.Mount) {
			return fmt.Errorf("unsafe data disk mount %q", disk.Mount)
		}
		if !spec.ValidFilesystem(disk.Filesystem) {
			return fmt.Errorf("unsupported filesystem %q", disk.Filesystem)
		}
		if _, exists := seenMount[disk.Mount]; exists {
			return fmt.Errorf("duplicate mount %q", disk.Mount)
		}
		if _, exists := seenSerial[disk.Serial]; exists {
			return fmt.Errorf("duplicate disk serial %q", disk.Serial)
		}
		seenMount[disk.Mount] = struct{}{}
		seenSerial[disk.Serial] = struct{}{}
	}
	seenTags := make(map[string]struct{}, len(input.Shares))
	seenGuests := make(map[string]struct{}, len(input.Shares))
	for _, share := range input.Shares {
		if !shareTagPattern.MatchString(share.Tag) {
			return fmt.Errorf("invalid Barn share tag %q", share.Tag)
		}
		if !safeShareMount(share.Guest, input.SSHUser) {
			return fmt.Errorf("unsafe guest share mount %q", share.Guest)
		}
		if _, exists := seenTags[share.Tag]; exists {
			return fmt.Errorf("duplicate Barn share tag %q", share.Tag)
		}
		for existing := range seenGuests {
			if mountPathsOverlap(existing, share.Guest) {
				return fmt.Errorf("overlapping guest share mounts %q and %q", existing, share.Guest)
			}
		}
		for diskMount := range seenMount {
			if mountPathsOverlap(diskMount, share.Guest) {
				return fmt.Errorf("guest share mount %q overlaps data disk mount %q", share.Guest, diskMount)
			}
		}
		seenTags[share.Tag] = struct{}{}
		seenGuests[share.Guest] = struct{}{}
	}
	if input.PrivateKey != "" {
		if !input.Control {
			return errors.New("private key may only be injected into the control guest")
		}
		if strings.ContainsRune(input.PrivateKey, '\x00') {
			return errors.New("private key contains NUL")
		}
		parsedPrivate, err := ssh.ParseRawPrivateKey([]byte(input.PrivateKey))
		if err != nil {
			return fmt.Errorf("parse control private key: %w", err)
		}
		signer, err := ssh.NewSignerFromKey(parsedPrivate)
		if err != nil || signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
			return errors.New("control private key must be Ed25519")
		}
		parsedPublic, _, _, _, err := ssh.ParseAuthorizedKey([]byte(input.PublicKey))
		if err != nil || !bytes.Equal(parsedPublic.Marshal(), signer.PublicKey().Marshal()) {
			return errors.New("control private key does not match the deployment public key")
		}
	}
	return nil
}

func yamlQuote(value string) string { return strconv.Quote(value) }

func indent(content string, spaces int) string {
	prefix := strings.Repeat(" ", spaces)
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}

func renderDiskScript(disks []Disk) string {
	var out strings.Builder
	out.WriteString(`#!/bin/bash
set -euo pipefail
check_only=false
[[ "${1:-}" != --check ]] || check_only=true
probe_disk() {
  local attempt status
  for attempt in 1 2 3; do
    status=0
    metadata=$(blkid -p -o export "${dev}" 2>"${probe_error}") || status=$?
    case "${status}" in
      0) uuid=$(printf '%s\n' "${metadata}" | sed -n 's/^UUID=//p'); fstype=$(printf '%s\n' "${metadata}" | sed -n 's/^TYPE=//p'); return 0 ;;
      2) ;; # No recognizable signature. Retry before treating it as unusable.
      *) cat "${probe_error}" >&2; echo "filesystem probe failed (${status}); disk not reset" >&2; return 1 ;;
    esac
    [[ "${attempt}" == 3 ]] || sleep 1
  done
  uuid= fstype=
}
reset_disk() {
  local format="${requested}" status=0
  if [[ "${format}" == auto ]]; then
    if command -v mkfs.xfs >/dev/null 2>&1; then format=xfs; else format=ext4; fi
  fi
  command -v "mkfs.${format}" >/dev/null 2>&1 || { echo "${format} requested but mkfs.${format} is unavailable" >&2; return 1; }
  # Only the configured whole data device may be reset, and never while any
  # filesystem on it is mounted. An I/O or read-only backend is not fixed by mkfs.
  [[ "$(lsblk -dn -o TYPE "${dev}")" == disk ]] || { echo 'data device is not a whole disk' >&2; return 1; }
  [[ -z "$(lsblk -nr -o MOUNTPOINT "${dev}" | sed '/^[[:space:]]*$/d')" ]] || { echo 'data disk is still in use; stop its users and run barn up again' >&2; return 1; }
  [[ "$(blockdev --getro "${dev}")" == 0 ]] || { echo 'data device is read-only; disk not reset' >&2; return 1; }
  dd if="${dev}" of=/dev/null bs=4096 count=1 status=none || { echo 'data device cannot be read; disk not reset' >&2; return 1; }
  printf 'Barn: initializing %s on %s (%s)\n' "${mountpoint}" "${dev}" "${format}"
  if [[ "${format}" == xfs ]]; then
    timeout --kill-after=5s 60s mkfs.xfs -f "${dev}" || status=$?
  else
    timeout --kill-after=5s 60s mkfs.ext4 -F "${dev}" || status=$?
  fi
  if (( status != 0 )); then
    echo 'data disk reset failed; previous contents may have been discarded' >&2
    return 1
  fi
  if [[ "${fresh}" != true || -e "/var/lib/barn/disk-${serial}.initialized" ]]; then
    /usr/local/libexec/barn-warning disk-reset "${mountpoint}: reset to an empty ${format} filesystem; previous data discarded"
  fi
  touch "/var/lib/barn/disk-${serial}.initialized"
  probe_disk
  [[ -n "${uuid}" && -n "${fstype}" ]]
}
filesystem_unusable() {
  local status=0
  case "${fstype}" in
    ext4)
      command -v e2fsck >/dev/null 2>&1 || return 1
      LC_ALL=C timeout --kill-after=2s 15s e2fsck -fn "${dev}" >"${probe_error}" 2>&1 || status=$?
      # Severe metadata damage (for example an invalid root inode) also sets
      # the abort bit: 4 + 8. Exclude operational read failures before resetting.
      if grep -Eqi 'I/O error|Input/output error|cannot read|could not read|Permission denied|allocat.*memory|out of memory' "${probe_error}"; then return 1; fi
      [[ "${status}" == 4 || "${status}" == 12 ]]
      ;;
    xfs)
      command -v xfs_repair >/dev/null 2>&1 || return 1
      LC_ALL=C timeout --kill-after=2s 15s xfs_repair -n "${dev}" >"${probe_error}" 2>&1 || status=$?
      # Operational read failures are not evidence that mkfs would help.
      if grep -Eqi 'I/O error|Input/output error|cannot read|could not read|Permission denied' "${probe_error}"; then return 1; fi
      [[ "${status}" == 1 ]]
      ;;
    *) return 1 ;;
  esac
}
init_disk() {
  local serial="$1" mountpoint="$2" requested="$3" fresh="$4" dev="/dev/disk/by-id/$5"
  local uuid= fstype= metadata= mounted_id= expected_id= attempt probe_error
  probe_error=$(mktemp /var/lib/barn/probe.err.XXXXXX)
  trap "rm -f -- '${probe_error}'" EXIT
  if [[ "${check_only}" == false ]]; then
    for attempt in $(seq 1 10); do
      [[ -b "${dev}" ]] && break
      sleep 1
    done
  fi
  [[ -b "${dev}" ]] || { echo "missing data device ${dev}; run barn up after it returns" >&2; return 1; }
  expected_id=$(lsblk -dn -o MAJ:MIN "${dev}")
  [[ -n "${expected_id}" ]] || return 1
  if mountpoint -q "${mountpoint}"; then
    mounted_id=$(findmnt -n -o MAJ:MIN --mountpoint "${mountpoint}")
    [[ "${mounted_id}" == "${expected_id}" ]] || { echo "another filesystem occupies ${mountpoint}; disk not reset" >&2; return 1; }
    # A directory read and the mount flags are a cheap check, not a full scrub.
    if [[ ",$(findmnt -n -o OPTIONS --mountpoint "${mountpoint}")," != *,ro,* ]] && ls -U "${mountpoint}" >/dev/null; then
      return 0
    fi
    [[ "${check_only}" == false ]] || return 1
    umount "${mountpoint}" || { echo 'data disk is busy; stop its users and run barn up again' >&2; return 1; }
  fi
  [[ "${check_only}" == false ]] || return 1
  probe_disk
  mkdir -p "${mountpoint}"
  [[ ! -L "${mountpoint}" ]] || { echo 'data mountpoint is a symlink' >&2; return 1; }
  if [[ -z "${fstype}" || -z "${uuid}" ]]; then
    reset_disk
  fi
  case "${fstype}" in ext4|xfs) ;; *) echo "unsupported existing filesystem ${fstype}; disk not reset" >&2; return 1 ;; esac
  local mounted=false
  for attempt in 1 2; do
    if mount -t "${fstype}" -o defaults "${dev}" "${mountpoint}"; then mounted=true; break; fi
    [[ "${attempt}" == 2 ]] || sleep 1
  done
  if [[ "${mounted}" == false ]]; then
    if filesystem_unusable; then
      reset_disk
      mount -t "${fstype}" -o defaults "${dev}" "${mountpoint}"
    else
      echo 'data disk mount failed without confirmed filesystem damage; disk not reset' >&2
      return 1
    fi
  fi
  local fstab_tmp
  fstab_tmp=$(mktemp /etc/fstab.barn.XXXXXX)
  awk -v mountpoint="${mountpoint}" 'NF < 2 || $2 != mountpoint { print }' /etc/fstab >"${fstab_tmp}"
  printf 'UUID=%s %s %s defaults,nofail 0 2\n' "${uuid}" "${mountpoint}" "${fstype}" >>"${fstab_tmp}"
  chmod 0644 "${fstab_tmp}"
  mv -f -- "${fstab_tmp}" /etc/fstab
  touch "/var/lib/barn/disk-${serial}.initialized"
}

`)
	for _, disk := range disks {
		fmt.Fprintf(&out, `disk_err=$(mktemp /var/lib/barn/disk.err.XXXXXX)
set +e
( set -euo pipefail; init_disk %s %s %s %t virtio-%s ) 2>"${disk_err}"
disk_status=$?
set -e
if (( disk_status != 0 )); then
  if [[ "${check_only}" == true ]]; then rm -f -- "${disk_err}"; exit 1; fi
  detail=$(tail -n 1 "${disk_err}")
  /usr/local/libexec/barn-warning data-disks "%s: ${detail:-disk setup unavailable}"
fi
cat "${disk_err}" >&2
rm -f -- "${disk_err}"
`, disk.Serial, disk.Mount, disk.Filesystem, disk.Fresh, disk.Serial, disk.Mount)
	}
	return out.String()
}

func renderShareScript(user string, shares []Share) string {
	if len(shares) == 0 {
		return ""
	}
	const (
		beginMarker = "# BEGIN BARN SHARES"
		endMarker   = "# END BARN SHARES"
		baseOptions = "version=9p2000.L,trans=virtio,cache=none,msize=262144,access=client,nofail,nodev,nosuid"
	)
	var out strings.Builder
	out.WriteString("#!/bin/bash\nset -Eeuo pipefail\n\n")
	fmt.Fprintf(&out, "share_user=%q\n", user)
	fmt.Fprintf(&out, "begin_marker=%q\n", beginMarker)
	fmt.Fprintf(&out, "end_marker=%q\n", endMarker)
	out.WriteString("is_desired_share() {\n  case \"$1|$2\" in\n")
	for _, share := range shares {
		fmt.Fprintf(&out, "    %q) return 0 ;;\n", share.Tag+"|"+share.Guest)
	}
	out.WriteString("    *) return 1 ;;\n  esac\n}\n\n")
	out.WriteString(`fstab=/etc/fstab
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
  [[ "${path}" =~ ^/[A-Za-z0-9._/-]+$ && "${path}" != / ]]
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
`)
	for _, share := range shares {
		options := baseOptions + ",rw"
		if share.Readonly {
			options = baseOptions + ",ro"
		}
		line := fmt.Sprintf("%s %s 9p %s 0 0", share.Tag, share.Guest, options)
		fmt.Fprintf(&out, "printf '%%s\\n' %q >> \"${fstab_tmp}\"\n", line)
	}
	out.WriteString(`printf '%s\n' "${end_marker}" >> "${fstab_tmp}"
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
    awk -v tag="${tag}" -v target="${mountpoint}" -v begin="${begin_marker}" -v end="${end_marker}" '
      $0 == begin { inside=1 }
      $0 == end { inside=0 }
      inside && $1 == tag && $2 == target && $3 == "9p" { sub(/,rw$/, ",ro", $4) }
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

`)
	for _, share := range shares {
		// Run each mount in an independent strict subshell. Putting a shell
		// function in an if/|| condition would disable errexit inside it.
		fmt.Fprintf(&out, `share_err=$(mktemp /var/lib/barn/share.err.XXXXXX)
set +e
( set -Eeuo pipefail; init_share %q %q %t ) 2>"${share_err}"
share_status=$?
set -e
if (( share_status != 0 )); then
  detail=$(tail -n 1 "${share_err}")
  /usr/local/libexec/barn-warning shares "${detail:-%s: mount unavailable}"
fi
cat "${share_err}" >&2
rm -f -- "${share_err}"
`, share.Tag, share.Guest, share.Readonly, share.Guest)
	}
	return out.String()
}

func RenderHostsScript(hosts []Host) string {
	sorted := append([]Host(nil), hosts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var out strings.Builder
	out.WriteString("#!/bin/bash\nset -euo pipefail\n")
	out.WriteString("sed -i.bak '/# barn-deployment-host$/d' /etc/hosts\n")
	for _, host := range sorted {
		fmt.Fprintf(&out, "printf '%%s %%s # barn-deployment-host\\n' %s %s >> /etc/hosts\n", host.Address, host.Name)
	}
	return out.String()
}

// RenderControlSSHConfig is shared by initial cloud-init and later topology updates.
func RenderControlSSHConfig(user string, hosts []Host) string {
	// Local lab nodes are routinely recreated at the same names and addresses.
	return "# BEGIN BARN\nHost " + strings.Join(hostNames(hosts), " ") + "\n  User " + user + "\n  IdentityFile ~/.ssh/id_ed25519\n  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n  LogLevel ERROR\nHost *\n# END BARN\n"
}

func renderNetworkCheckScript() string {
	return `#!/bin/bash
set -euo pipefail
for attempt in 1 2 3; do
  if response=$(timeout 10s /bin/bash -c '
exec 3<>/dev/tcp/example.com/80
printf "HEAD / HTTP/1.0\r\nHost: example.com\r\n\r\n" >&3
IFS= read -r response <&3
printf "%s\n" "${response}"
'); then
    if [[ "${response}" == HTTP/* ]]; then
      printf '%s\n' "${response}"
      exit 0
    fi
  fi
  if (( attempt < 3 )); then
    sleep 2
  fi
done
echo 'management egress probe failed after 3 bounded attempts' >&2
exit 1
`
}

func renderPrivateContractScript(network PrivateNetwork) string {
	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail
expected=%q
expected_host=%q
expected_mac=%q
interface=
for candidate in /sys/class/net/*; do
  [[ -r "${candidate}/address" ]] || continue
  read -r actual_mac <"${candidate}/address" || continue
  if [[ "${actual_mac,,}" == "${expected_mac}" ]]; then
    [[ -z "${interface}" ]] || { echo "multiple interfaces have private MAC ${expected_mac}" >&2; exit 1; }
    interface=${candidate##*/}
  fi
done
[[ -n "${interface}" ]] || { echo "private interface with MAC ${expected_mac} was not found" >&2; exit 1; }
ip link show dev "${interface}" >/dev/null
if ! ip link show dev "${interface}" | grep -Eq '<[^>]*UP[^>]*>'; then
  ip link set dev "${interface}" up
fi
if ! ip -4 -o address show dev "${interface}" scope global | awk '{print $4}' | grep -Fxq "${expected}"; then
  ip address add "${expected}" dev "${interface}"
fi
if ip -4 route show default dev "${interface}" | grep -q .; then
  echo "private interface ${interface} owns a default route" >&2
  exit 1
fi
if command -v resolvectl >/dev/null 2>&1; then
  dns=$(resolvectl dns "${interface}" 2>/dev/null || true)
  dns=${dns#*:}
	  dns=$(printf '%%s' "${dns}" | tr -d '[:space:]')
  if [[ -n "${dns}" && "${dns}" != "-" ]]; then
    echo "private interface ${interface} has DNS: ${dns}" >&2
    exit 1
  fi
elif command -v nmcli >/dev/null 2>&1; then
  dns=$(nmcli -g IP4.DNS device show "${interface}" 2>/dev/null || true)
  if [[ -n "${dns}" ]]; then
    echo "private interface ${interface} has DNS: ${dns}" >&2
    exit 1
  fi
fi
printf 'barn-arp-refresh\n' >"/dev/udp/${expected_host}/9"
printf 'private interface %%s %%s is up with no default route or DNS\n' "${interface}" "${expected}"
`, fmt.Sprintf("%s/%d", network.Address, network.Prefix), network.HostAddress, strings.ToLower(network.MAC))
}

func renderControlSSHInstallScript(user string) string {
	keySource := "/var/lib/barn/control-id_ed25519"
	configSource := "/var/lib/barn/control-ssh-config"
	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail
user=%q
entry=$(getent passwd "${user}")
IFS=: read -r _ _ uid gid _ home _ <<<"${entry}"
[[ "${uid}" =~ ^[0-9]+$ && "${gid}" =~ ^[0-9]+$ ]]
[[ "${home}" == /home/* && -d "${home}" && ! -L "${home}" ]]
[[ "$(readlink -f -- "${home}")" == "${home}" ]]
ssh_dir="${home}/.ssh"
if [[ -e "${ssh_dir}" || -L "${ssh_dir}" ]]; then
  [[ -d "${ssh_dir}" && ! -L "${ssh_dir}" ]]
fi
if [[ ! -e %[2]s && ! -e %[3]s ]]; then
  # The first successful installation consumes the staged secret. A later
  # repair must validate the installed files instead of requiring it again.
  if [[ ! -f "${ssh_dir}/id_ed25519" || -L "${ssh_dir}/id_ed25519" ]]; then
    echo "node-to-node SSH key is missing or unsafe; restore the original deployment private key to ${ssh_dir}/id_ed25519 (owner ${uid}:${gid}, mode 0600); management SSH remains available" >&2
    exit 1
  fi
  [[ ! -L "${ssh_dir}/id_ed25519" && ! -L "${ssh_dir}/config" ]]
  [[ "$(stat -c '%%u:%%g:%%a' "${ssh_dir}/id_ed25519")" == "${uid}:${gid}:600" ]]
  [[ "$(stat -c '%%u:%%g:%%a' "${ssh_dir}/config")" == "${uid}:${gid}:600" ]]
  exit 0
fi
[[ "$(stat -c '%%u:%%g:%%a' %[2]s)" == "0:0:600" && ! -L %[2]s ]]
[[ "$(stat -c '%%u:%%g:%%a' %[3]s)" == "0:0:600" && ! -L %[3]s ]]
install -d -o "${uid}" -g "${gid}" -m 0700 "${ssh_dir}"
install -o "${uid}" -g "${gid}" -m 0600 %[2]s "${ssh_dir}/id_ed25519"
install -o "${uid}" -g "${gid}" -m 0600 %[3]s "${ssh_dir}/config"
rm -f -- %[2]s %[3]s
[[ "$(stat -c '%%u:%%g:%%a' "${ssh_dir}/id_ed25519")" == "${uid}:${gid}:600" ]]
[[ "$(stat -c '%%u:%%g:%%a' "${ssh_dir}/config")" == "${uid}:${gid}:600" ]]
`, user, keySource, configSource)
}

func renderIdentityContractScript(user string) string {
	var contract string
	if user == "dba" {
		contract = `[[ "${uid}" == 88 && "${gid}" == 88 ]]
[[ "${home}" == /home/dba && "${shell}" == /bin/bash ]]
group=$(getent group admin)
IFS=: read -r group_name _ group_gid _ <<<"${group}"
[[ "${group_name}" == admin && "${group_gid}" == 88 ]]
[[ "$(stat -c '%u:%g' "${home}")" == 88:88 ]]
`
	} else {
		contract = `[[ "${uid}" =~ ^[0-9]+$ && "${gid}" =~ ^[0-9]+$ ]]
[[ "${home}" == /home/* && "${shell}" == /bin/bash ]]
`
	}
	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail
user=%q
entry=$(getent passwd "${user}")
IFS=: read -r _ _ uid gid _ home shell <<<"${entry}"
%s[[ -d "${home}" && ! -L "${home}" ]]
printf 'login identity %%s uid=%%s gid=%%s home=%%s\n' "${user}" "${uid}" "${gid}" "${home}"
`, user, contract)
}

func renderFinalizeScript(controlSSH, privateNetwork, shares bool) string {
	var out strings.Builder
	out.WriteString(`#!/bin/bash
set -euo pipefail
install -d -o root -g root -m 0755 /var/lib/barn
exec 9>/var/lib/barn/finalize.lock
flock -n 9 || { echo 'guest setup is already running' >&2; exit 1; }
# Empty selection runs initial setup; up passes only stages needing a retry.
selected=" $* "
previous_warnings=$(cat /var/lib/barn/warnings.jsonl 2>/dev/null || true)
stage=identity
stage_err=/var/lib/barn/stage.err
finalize_exit() {
  local status=$?
  local error_tmp= detail=
  trap - EXIT
  if (( status != 0 )); then
    if [[ -s "${stage_err}" ]]; then
      detail=$(tail -n 1 "${stage_err}" | LC_ALL=C tr -d '\r\n' | LC_ALL=C tr '\000-\037\177' ' ' | cut -c1-200 | sed 's/\\/\\\\/g; s/"/\\"/g')
    fi
    if error_tmp=$(mktemp /var/lib/barn/error.json.XXXXXX); then
      if printf '{"exit_status":%d,"stage":"%s","detail":"%s"}\n' "${status}" "${stage}" "${detail}" > "${error_tmp}" &&
        chown root:root "${error_tmp}" && chmod 0644 "${error_tmp}"; then
        mv -f -- "${error_tmp}" /var/lib/barn/error.json || rm -f -- "${error_tmp}" || true
      else
        rm -f -- "${error_tmp}" || true
      fi
    fi
  fi
  rm -f -- "${stage_err}" || true
`)
	// The installer consumes its root-only staged key on success. Retain it
	// after a failed optional install so an in-place repair can finish later.
	out.WriteString(`  exit "${status}"
}
# run_stage records the stage name and keeps its stderr so the error marker
# can carry the failing command's last line to the host.
run_stage() {
  stage=$1
  shift
  : >"${stage_err}"
  local rc=0
  "$@" 2>"${stage_err}" || rc=$?
  cat "${stage_err}" >&2
  return "${rc}"
}
run_optional() {
  local name=$1 budget=$2 detail
  shift 2
  if [[ "${selected}" != '  ' && "${selected}" != *" ${name} "* ]]; then
    # Retain unselected limitations, but reset notices describe only one run.
    printf '%s\n' "${previous_warnings}" | grep -F "\"stage\":\"${name}\"" >>/var/lib/barn/warnings.jsonl || true
    return 0
  fi
  if run_stage "${name}" timeout --kill-after=5s "${budget}" "$@"; then
    return 0
  fi
  detail=$(tail -n 1 "${stage_err}")
  /usr/local/libexec/barn-warning "${name}" "${detail:-setup unavailable or timed out}"
}
trap finalize_exit EXIT
rm -f -- /var/lib/barn/ready.json /var/lib/barn/error.json
: >/var/lib/barn/warnings.jsonl
chmod 0644 /var/lib/barn/warnings.jsonl
run_stage identity /usr/local/libexec/barn-identity-contract
run_optional hosts 30s /usr/local/libexec/barn-hosts
# Internet access is diagnostic only: an offline lab must still initialize its
# disks, shares and private network. barn-network-check remains available for
# an explicit guest connectivity check.
run_optional data-disks 90s /usr/local/libexec/barn-init-disks
`)
	if shares {
		out.WriteString("run_optional shares 30s /usr/local/libexec/barn-init-shares\n")
	}
	if controlSSH {
		out.WriteString("run_optional control-ssh 30s /usr/local/libexec/barn-install-control-ssh\n")
	}
	if privateNetwork {
		out.WriteString("run_optional private-network 30s /usr/local/libexec/barn-private-contract\n")
	}
	out.WriteString("run_stage ready /usr/local/libexec/barn-ready\n")
	return out.String()
}

func renderReadyScript(input Input) (string, error) {
	ready := struct {
		Node         string `json:"node"`
		Generation   uint64 `json:"generation"`
		SpecHash     string `json:"spec_hash"`
		SetupVersion string `json:"setup_version"`
	}{input.Node, input.Generation, input.SpecHash, SetupVersion}
	data, err := json.Marshal(ready)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail
install -d -m 0755 /var/lib/barn
warnings=
if [[ -f /var/lib/barn/warnings.jsonl ]]; then
  warnings=$(paste -sd, /var/lib/barn/warnings.jsonl)
fi
printf '%%s,"warnings":[%%s]}\n' %s "${warnings}" > /var/lib/barn/ready.json.tmp
chmod 0644 /var/lib/barn/ready.json.tmp
mv /var/lib/barn/ready.json.tmp /var/lib/barn/ready.json
`, yamlQuote(strings.TrimSuffix(string(data), "}"))), nil
}

func renderWarningScript() string {
	return `#!/bin/bash
set -euo pipefail
json_string() {
  printf '%s' "$1" | LC_ALL=C tr '\000-\037\177' ' ' | cut -c1-400 | sed 's/\\/\\\\/g; s/"/\\"/g'
}
printf '{"stage":"%s","detail":"%s"}\n' "$(json_string "$1")" "$(json_string "$2")" >>/var/lib/barn/warnings.jsonl
`
}

func renderMetaData(input Input) []byte {
	return []byte(fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", yamlQuote(fmt.Sprintf("barn-%s-g%d", input.Node, input.Generation)), yamlQuote(input.Hostname)))
}

func renderNetwork(input Input) []byte {
	var out strings.Builder
	out.WriteString("version: 2\nethernets:\n  management:\n    match:\n      macaddress: ")
	out.WriteString(yamlQuote(strings.ToLower(input.MgmtMAC)))
	out.WriteString("\n    dhcp4: true\n    dhcp6: false\n    dhcp4-overrides:\n      route-metric: 100\n")
	if input.Private != nil {
		out.WriteString("  private:\n    match:\n      macaddress: ")
		out.WriteString(yamlQuote(strings.ToLower(input.Private.MAC)))
		out.WriteString("\n    dhcp4: false\n    dhcp6: false\n    accept-ra: false\n    link-local: []\n    addresses:\n      - ")
		out.WriteString(yamlQuote(fmt.Sprintf("%s/%d", input.Private.Address, input.Private.Prefix)))
		out.WriteString("\n")
	}
	return []byte(out.String())
}

func renderUserData(input Input) ([]byte, error) {
	readyScript, err := renderReadyScript(input)
	if err != nil {
		return nil, err
	}
	var out strings.Builder
	out.WriteString("#cloud-config\npreserve_hostname: false\nhostname: ")
	out.WriteString(yamlQuote(input.Hostname))
	if input.SSHUser == "dba" {
		out.WriteString("\nbootcmd:\n  - ['/bin/sh', '-c', 'if /usr/bin/getent group admin >/dev/null; then current=$(/usr/bin/getent group admin | /usr/bin/cut -d: -f3); if test \"${current}\" != 88; then ! /usr/bin/getent group 88 >/dev/null; /usr/sbin/groupmod --gid 88 admin; fi; else ! /usr/bin/getent group 88 >/dev/null; /usr/sbin/groupadd --gid 88 admin; fi']")
	}
	out.WriteString("\nssh_pwauth: false\ndisable_root: true\nssh_deletekeys: false\nusers:\n  - default\n  - name: ")
	out.WriteString(yamlQuote(input.SSHUser))
	if input.SSHUser == "dba" {
		out.WriteString("\n    uid: 88\n    primary_group: admin\n    create_groups: false")
	}
	out.WriteString("\n    shell: /bin/bash\n    lock_passwd: true\n    sudo: ALL=(ALL) NOPASSWD:ALL\n    ssh_authorized_keys:\n      - ")
	out.WriteString(yamlQuote(input.PublicKey))
	out.WriteString("\ngrowpart:\n  mode: auto\n  devices: ['/']\nresize_rootfs: true\nntp:\n  enabled: true\nwrite_files:\n")

	writeFile := func(path, owner, permissions, content string) {
		out.WriteString("  - path: ")
		out.WriteString(yamlQuote(path))
		out.WriteString("\n    owner: ")
		out.WriteString(yamlQuote(owner))
		out.WriteString("\n    permissions: ")
		out.WriteString(yamlQuote(permissions))
		out.WriteString("\n    content: |\n")
		out.WriteString(indent(content, 6))
	}
	writeFile("/usr/local/libexec/barn-init-disks", "root:root", "0755", renderDiskScript(input.Disks))
	writeFile("/usr/local/libexec/barn-warning", "root:root", "0755", renderWarningScript())
	if len(input.Shares) != 0 {
		writeFile("/usr/local/libexec/barn-init-shares", "root:root", "0755", renderShareScript(input.SSHUser, input.Shares))
	}
	writeFile("/usr/local/libexec/barn-hosts", "root:root", "0755", RenderHostsScript(input.Hosts))
	writeFile("/usr/local/libexec/barn-network-check", "root:root", "0755", renderNetworkCheckScript())
	writeFile("/usr/local/libexec/barn-identity-contract", "root:root", "0755", renderIdentityContractScript(input.SSHUser))
	if input.Private != nil {
		writeFile("/usr/local/libexec/barn-private-contract", "root:root", "0755", renderPrivateContractScript(*input.Private))
	}
	writeFile("/usr/local/libexec/barn-ready", "root:root", "0755", readyScript)
	if input.PrivateKey != "" {
		// write_files runs before users-groups on supported cloud-init images.
		// Stage the secret as root, then install it only after the login user
		// exists. Referring to the future user as write_files owner makes the
		// entire module fail and can leave a deceptively usable guest.
		writeFile("/var/lib/barn/control-id_ed25519", "root:root", "0600", input.PrivateKey)
		sshConfig := RenderControlSSHConfig(input.SSHUser, input.Hosts)
		writeFile("/var/lib/barn/control-ssh-config", "root:root", "0600", sshConfig)
		writeFile("/usr/local/libexec/barn-install-control-ssh", "root:root", "0700", renderControlSSHInstallScript(input.SSHUser))
	}
	writeFile("/usr/local/libexec/barn-finalize", "root:root", "0755", renderFinalizeScript(input.PrivateKey != "", input.Private != nil, len(input.Shares) != 0))
	out.WriteString("runcmd:\n  - ['/usr/local/libexec/barn-finalize']\n")
	return []byte(out.String()), nil
}

func hostNames(hosts []Host) []string {
	names := make([]string, 0, len(hosts))
	for _, host := range hosts {
		names = append(names, host.Name)
	}
	sort.Strings(names)
	return names
}

// Render validates all interpolated values before producing YAML/shell text.
func Render(input Input) (Files, error) {
	if err := validateInput(input); err != nil {
		return Files{}, err
	}
	userData, err := renderUserData(input)
	if err != nil {
		return Files{}, err
	}
	return Files{MetaData: renderMetaData(input), UserData: userData, NetworkConfig: renderNetwork(input)}, nil
}
