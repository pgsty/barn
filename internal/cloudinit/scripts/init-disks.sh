#!/bin/bash
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
      # A filesystem made by a newer e2fsprogs is healthy data this guest's tools
      # cannot read, never evidence of damage.
      if grep -Eqi 'unsupported feature|too new|revision too high|newer version' "${probe_error}"; then return 1; fi
      [[ "${status}" == 4 || "${status}" == 12 ]] || return 1
      grep -Eqi 'Filesystem still has errors|UNEXPECTED INCONSISTENCY|Root inode is not a directory|bad magic number|corrupt.*(inode|superblock)|invalid.*(inode|superblock)' "${probe_error}"
      ;;
    xfs)
      command -v xfs_repair >/dev/null 2>&1 || return 1
      LC_ALL=C timeout --kill-after=2s 15s xfs_repair -n "${dev}" >"${probe_error}" 2>&1 || status=$?
      # Operational read failures are not evidence that mkfs would help.
      if grep -Eqi 'I/O error|Input/output error|cannot read|could not read|Permission denied' "${probe_error}"; then return 1; fi
      # An older xfs_repair exits 1 for a healthy filesystem with features it
      # does not know, for example a persistent disk formatted by a newer
      # distribution. That is an unsupported filesystem, not damage.
      if grep -Eqi 'unknown .*features|unsupported .*feature|features not understood|more recent|newer|out of memory|allocat.*memory' "${probe_error}"; then return 1; fi
      [[ "${status}" == 1 ]] || return 1
      # Exit 1 also covers unsupported formats and operational failures.
      # Only recognized damage authorizes discarding existing test data.
      grep -Eqi 'bad magic number|would (reset|rebuild|clear|correct)|would have modified' "${probe_error}"
      ;;
    *) return 1 ;;
  esac
}
init_disk() {
  local serial="$1" mountpoint="$2" requested="$3" fresh="$4" dev="/dev/disk/by-id/$5" fstab_mountpoint="$6"
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
  # Only a device with no recognizable signature at all is initialized. A
  # partition table, or any signature without a filesystem UUID and type, is
  # data Barn does not manage; leave it for the user rather than mkfs it.
  if [[ -z "${metadata}" ]]; then
    reset_disk
  elif [[ -z "${fstype}" || -z "${uuid}" ]]; then
    echo 'data disk holds a partition table or a signature Barn does not manage; disk not reset' >&2
    return 1
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
  BARN_FSTAB_MOUNT="${fstab_mountpoint}" awk 'NF < 2 || $2 != ENVIRON["BARN_FSTAB_MOUNT"] { print }' /etc/fstab >"${fstab_tmp}"
  printf 'UUID=%s %s %s defaults,nofail 0 2\n' "${uuid}" "${fstab_mountpoint}" "${fstype}" >>"${fstab_tmp}"
  chmod 0644 "${fstab_tmp}"
  mv -f -- "${fstab_tmp}" /etc/fstab
  touch "/var/lib/barn/disk-${serial}.initialized"
}

# __BARN_DISK_CALLS__
