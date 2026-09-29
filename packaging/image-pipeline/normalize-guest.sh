#!/bin/bash
set -euo pipefail

# This script runs only inside virt-customize's offline appliance. It accepts
# deliberately narrow, pre-validated values; it never downloads packages or
# reads credentials from the host.
if [[ $# -ne 4 ]]; then
  printf 'usage: %s <source-user> <source-date-epoch> <profile> <package-dir|->\n' "$0" >&2
  exit 2
fi
source_user=$1
source_date_epoch=$2
profile=$3
package_dir=$4
[[ ${source_user} =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || { printf 'invalid source user\n' >&2; exit 2; }
[[ ${source_date_epoch} =~ ^[0-9]{1,10}$ ]] || { printf 'invalid source epoch\n' >&2; exit 2; }
case ${profile} in
  base|el8|el9|d12|d13) ;;
  *) printf 'invalid customization profile: %s\n' "${profile}" >&2; exit 2 ;;
esac
export LC_ALL=C
export TZ=UTC
PATH=/usr/sbin:/usr/bin:/sbin:/bin

for command in awk cat chmod chown find getent grep groupadd groupmod install ln rm sed stat touch useradd usermod; do
  command -v "${command}" >/dev/null || { printf 'required guest command missing: %s\n' "${command}" >&2; exit 3; }
done

# Refuse a profile/image mismatch before making any changes. The input image is
# separately digest-pinned by the host pipeline, but this makes accidental
# cross-profile invocations fail closed as well.
# shellcheck disable=SC1091
source /etc/os-release
case ${profile} in
  base) ;;
  el8) [[ ${ID:-} == rocky && ${VERSION_ID:-} == 8.* ]] || { printf 'el8 profile requires Rocky Linux 8\n' >&2; exit 3; } ;;
  el9) [[ ${ID:-} == rocky && ${VERSION_ID:-} == 9.* ]] || { printf 'el9 profile requires Rocky Linux 9\n' >&2; exit 3; } ;;
  d12) [[ ${ID:-} == debian && ${VERSION_ID:-} == 12 ]] || { printf 'd12 profile requires Debian 12\n' >&2; exit 3; } ;;
  d13) [[ ${ID:-} == debian && ${VERSION_ID:-} == 13 ]] || { printf 'd13 profile requires Debian 13\n' >&2; exit 3; } ;;
esac

install_locked_packages() {
  local packages=()
  case ${profile} in
    el8)
      [[ ${package_dir} == /var/tmp/barn-image-packages && -d ${package_dir} ]] || {
        printf 'el8 profile requires the locked RPM bundle\n' >&2
        exit 3
      }
      command -v rpm >/dev/null || { printf 'required guest command missing: rpm\n' >&2; exit 3; }
      shopt -s nullglob
      packages=("${package_dir}"/*.rpm)
      shopt -u nullglob
      [[ ${#packages[@]} -gt 0 ]] || { printf 'locked RPM bundle is empty\n' >&2; exit 3; }
      rocky_key=/etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-8
      [[ -f ${rocky_key} ]] || { printf 'Rocky package signing key is missing\n' >&2; exit 3; }
      key_database=/var/tmp/barn-rpm-signature-db
      [[ ! -e ${key_database} ]] || { printf 'temporary RPM signature database already exists\n' >&2; exit 3; }
      install -d -o root -g root -m 0700 "${key_database}"
      rpm --dbpath "${key_database}" --initdb
      rpm --dbpath "${key_database}" --import "${rocky_key}"
      rpm --dbpath "${key_database}" --checksig "${packages[@]}"
      rm -rf -- "${key_database}"
      # Signatures were checked against the temporary trusted database above;
      # keep the upstream image's system RPM key database unchanged.
      rpm -Uvh --replacepkgs --nosignature "${packages[@]}"
      ;;
    d12|d13)
      [[ ${package_dir} == /var/tmp/barn-image-packages && -d ${package_dir} ]] || {
        printf '%s profile requires the locked DEB bundle\n' "${profile}" >&2
        exit 3
      }
      command -v dpkg >/dev/null || { printf 'required guest command missing: dpkg\n' >&2; exit 3; }
      shopt -s nullglob
      packages=("${package_dir}"/*.deb)
      shopt -u nullglob
      [[ ${#packages[@]} -gt 0 ]] || { printf 'locked DEB bundle is empty\n' >&2; exit 3; }
      dpkg -i "${packages[@]}"
      ;;
    base|el9)
      [[ ${package_dir} == - ]] || { printf '%s profile does not accept a package bundle\n' "${profile}" >&2; exit 3; }
      ;;
  esac
  if [[ ${package_dir} != - ]]; then
    rm -rf -- "${package_dir}"
  fi
}

configure_debian_locales() {
  local default_locale
  for command in locale locale-gen; do
    command -v "${command}" >/dev/null || { printf 'required locale command missing: %s\n' "${command}" >&2; exit 3; }
  done
  [[ -f /etc/locale.gen ]] || { printf 'Debian locale.gen is missing\n' >&2; exit 3; }
  sed -i -E 's/^#[[:space:]]*(en_US\.UTF-8[[:space:]]+UTF-8)[[:space:]]*$/\1/' /etc/locale.gen
  grep -Eq '^en_US\.UTF-8[[:space:]]+UTF-8[[:space:]]*$' /etc/locale.gen || {
    printf 'failed to enable en_US.UTF-8 in locale.gen\n' >&2
    exit 5
  }
  locale-gen en_US.UTF-8
  install -d -o root -g root -m 0755 /etc/default
  printf 'LANG=C.UTF-8\n' >/etc/default/locale
  chmod 0644 /etc/default/locale
  locale -a | grep -Fqx 'en_US.utf8' || { printf 'en_US.UTF-8 locale was not generated\n' >&2; exit 5; }
  [[ $(LC_ALL=en_US.UTF-8 locale charmap) == UTF-8 ]] || { printf 'en_US.UTF-8 charmap is invalid\n' >&2; exit 5; }
  [[ $(LC_ALL=C.UTF-8 locale charmap) == UTF-8 ]] || { printf 'C.UTF-8 charmap is invalid\n' >&2; exit 5; }
  default_locale=$(sed -n 's/^LANG=//p' /etc/default/locale)
  [[ ${default_locale} == C.UTF-8 ]] || { printf 'default locale changed from C.UTF-8\n' >&2; exit 5; }
}

strip_boot_argument() {
  local file=$1 argument=$2
  [[ -f ${file} ]] || return 0
  # Run twice so adjacent arguments are both removed without depending on GNU
  # sed's handling of an already-consumed separator.
  sed -i -E \
    -e "s/(^|[[:space:]])${argument}([[:space:]]|$)/\\1\\2/g" \
    -e "s/(^|[[:space:]])${argument}\"/\\1\"/g" "${file}"
  sed -i -E \
    -e "s/(^|[[:space:]])${argument}([[:space:]]|$)/\\1\\2/g" \
    -e "s/(^|[[:space:]])${argument}\"/\\1\"/g" "${file}"
}

remove_legacy_el_networking() {
  local entry token kernelopts clean=() file
  command -v grubby >/dev/null && grubby --update-kernel=ALL --remove-args='net.ifnames=0 biosdevname=0'
  for file in /etc/default/grub /etc/sysconfig/grub /etc/sysconfig/bootloader; do
    strip_boot_argument "${file}" 'net\.ifnames=0'
    strip_boot_argument "${file}" 'biosdevname=0'
  done
  if [[ -d /boot/loader/entries ]]; then
    while IFS= read -r file; do
      strip_boot_argument "${file}" 'net\.ifnames=0'
      strip_boot_argument "${file}" 'biosdevname=0'
    done < <(find /boot/loader/entries -maxdepth 1 -type f -name '*.conf' -print)
  fi
  for entry in /boot/grub2/grubenv /boot/efi/EFI/rocky/grubenv; do
    [[ -e ${entry} ]] || continue
    command -v grub2-editenv >/dev/null || { printf 'grub2-editenv is required for %s\n' "${entry}" >&2; exit 3; }
    kernelopts=$(grub2-editenv "${entry}" list | sed -n 's/^kernelopts=//p')
    [[ -n ${kernelopts} ]] || continue
    clean=()
    # The official inputs contain only ordinary kernel tokens here.
    read -r -a tokens <<<"${kernelopts}"
    for token in "${tokens[@]}"; do
      [[ ${token} == net.ifnames=0 || ${token} == biosdevname=0 ]] || clean+=("${token}")
    done
    grub2-editenv "${entry}" set "kernelopts=${clean[*]}"
  done

  # These fixed upstream images carry interface-name-bound profiles that become
  # stale once predictable names are restored. Cloud-init will author the
  # deployment-specific profiles on first boot.
  if [[ -d /etc/sysconfig/network-scripts ]]; then
    find /etc/sysconfig/network-scripts -maxdepth 1 -type f -name 'ifcfg-*' ! -name 'ifcfg-lo' -delete
  fi
  if [[ -d /etc/NetworkManager/system-connections ]]; then
    while IFS= read -r file; do
      if grep -Eq '^[[:space:]]*interface-name=(eth[0-9]+|enp[0-9].*)[[:space:]]*$' "${file}"; then
        rm -f -- "${file}"
      fi
    done < <(find /etc/NetworkManager/system-connections -maxdepth 1 -type f -print)
  fi

  for file in /etc/default/grub /etc/sysconfig/grub /etc/sysconfig/bootloader; do
    [[ -f ${file} ]] || continue
    ! grep -aEq '(^|[[:space:]"])(net\.ifnames=0|biosdevname=0)([[:space:]"]|$)' "${file}" || {
      printf 'legacy kernel network argument remains in %s\n' "${file}" >&2
      exit 5
    }
  done
  if [[ -d /boot/loader/entries ]]; then
    ! grep -R -aEq '(^|[[:space:]])(net\.ifnames=0|biosdevname=0)([[:space:]]|$)' /boot/loader/entries || {
      printf 'legacy kernel network argument remains in a BLS entry\n' >&2
      exit 5
    }
  fi
  for entry in /boot/grub2/grubenv /boot/efi/EFI/rocky/grubenv; do
    [[ -e ${entry} ]] || continue
    ! grub2-editenv "${entry}" list | grep -Eq '(^|[[:space:]])(net\.ifnames=0|biosdevname=0)([[:space:]]|$)' || {
      printf 'legacy kernel network argument remains in %s\n' "${entry}" >&2
      exit 5
    }
  done
  if [[ -d /etc/sysconfig/network-scripts ]]; then
    residual=$(find /etc/sysconfig/network-scripts -maxdepth 1 -type f -name 'ifcfg-*' ! -name 'ifcfg-lo' -print -quit)
    [[ -z ${residual} ]] || { printf 'legacy interface profile remains: %s\n' "${residual}" >&2; exit 5; }
  fi
}

ensure_sshd_dropin_include() {
  local config=/etc/ssh/sshd_config
  [[ -f ${config} ]] || { printf 'missing sshd configuration\n' >&2; exit 3; }
  if ! awk '
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*Match[[:space:]]/ { if (!seen) bad=1; exit }
    /^[[:space:]]*Include[[:space:]]+\/etc\/ssh\/sshd_config\.d\/\*\.conf([[:space:]]|$)/ { seen=1 }
    END { exit (!seen || bad) }
  ' "${config}"; then
    sed -i '1i Include /etc/ssh/sshd_config.d/*.conf' "${config}"
  fi
  awk '
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*Match[[:space:]]/ { if (!seen) bad=1; exit }
    /^[[:space:]]*Include[[:space:]]+\/etc\/ssh\/sshd_config\.d\/\*\.conf([[:space:]]|$)/ { seen=1 }
    END { exit (!seen || bad) }
  ' "${config}" || { printf 'sshd drop-in Include is missing or scoped under Match\n' >&2; exit 5; }
}

install_locked_packages
case ${profile} in
  el8)
    remove_legacy_el_networking
    ensure_sshd_dropin_include
    command -v python3 >/dev/null || { printf 'python3 executable is missing\n' >&2; exit 5; }
    python3 -c 'import json, ssl, sys; assert sys.version_info >= (3, 6)'
    ;;
  el9)
    remove_legacy_el_networking
    ;;
  d12|d13)
    command -v mkfs.xfs >/dev/null || { printf 'mkfs.xfs executable is missing\n' >&2; exit 5; }
    mkfs.xfs -V >/dev/null 2>&1
    configure_debian_locales
    ;;
esac

# Fail closed rather than silently reassigning an unrelated numeric identity.
if getent group admin >/dev/null; then
  admin_gid=$(getent group admin | awk -F: 'NR == 1 { print $3 }')
  if [[ ${admin_gid} != 88 ]]; then
    ! getent group 88 >/dev/null || { printf 'GID 88 is occupied by a group other than admin\n' >&2; exit 4; }
    groupmod --gid 88 admin
  fi
else
  ! getent group 88 >/dev/null || { printf 'GID 88 is occupied and admin is absent\n' >&2; exit 4; }
  groupadd --gid 88 admin
fi

if getent passwd dba >/dev/null; then
  dba_uid=$(getent passwd dba | awk -F: 'NR == 1 { print $3 }')
  if [[ ${dba_uid} != 88 ]]; then
    ! getent passwd 88 >/dev/null || { printf 'UID 88 is occupied by a user other than dba\n' >&2; exit 4; }
    usermod --uid 88 dba
  fi
else
  ! getent passwd 88 >/dev/null || { printf 'UID 88 is occupied and dba is absent\n' >&2; exit 4; }
  useradd --uid 88 --gid 88 --home-dir /home/dba --shell /bin/bash --no-user-group dba
fi
usermod --gid 88 --home /home/dba --shell /bin/bash --password '!' dba
install -d -o dba -g admin -m 0700 /home/dba /home/dba/.ssh
chown -R 88:88 /home/dba

# Remove authentication material from every conventional home/skel location.
# This is intentionally stronger than deleting one known development key.
for root in /root /home /etc/skel; do
  [[ -e ${root} ]] || continue
  find "${root}" -xdev -type f \( \
    -name authorized_keys -o -name authorized_keys2 -o \
    -name 'id_rsa*' -o -name 'id_dsa*' -o -name 'id_ecdsa*' -o -name 'id_ed25519*' -o \
    -name known_hosts -o -name known_hosts.old -o \
    -name .bash_history -o -name .zsh_history -o -name .mysql_history -o -name .psql_history \
  \) -delete
done

# Replace legacy password hashes, rather than merely prefixing a still-present
# hash. Lock root, the declared bootstrap user, dba, and all normal UID>=1000
# accounts. Duplicate names are harmless.
mapfile -t normal_users < <(awk -F: '$3 >= 1000 { print $1 }' /etc/passwd)
for user in root "${source_user}" dba "${normal_users[@]}"; do
  [[ -n ${user} ]] || continue
  getent passwd "${user}" >/dev/null || continue
  usermod --password '!' "${user}"
done

install -d -o root -g root -m 0755 /etc/sudoers.d
printf 'dba ALL=(ALL) NOPASSWD: ALL\n' >/etc/sudoers.d/90-barn-dba
chown root:root /etc/sudoers.d/90-barn-dba
chmod 0440 /etc/sudoers.d/90-barn-dba
if command -v visudo >/dev/null; then
  visudo -cf /etc/sudoers >/dev/null
fi

install -d -o root -g root -m 0755 /etc/ssh/sshd_config.d
cat >/etc/ssh/sshd_config.d/99-barn-image.conf <<'EOF'
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
PermitRootLogin no
PubkeyAuthentication yes
EOF
chmod 0644 /etc/ssh/sshd_config.d/99-barn-image.conf
if [[ ${profile} == el8 ]]; then
  ensure_sshd_dropin_include
fi
rm -f -- /etc/ssh/ssh_host_*

: >/etc/machine-id
if [[ -d /var/lib/dbus ]]; then
  rm -f -- /var/lib/dbus/machine-id
  ln -s /etc/machine-id /var/lib/dbus/machine-id
fi
rm -f -- /var/lib/systemd/random-seed
if [[ -d /var/lib/cloud ]]; then
  find /var/lib/cloud -mindepth 1 -delete
fi
rm -f -- /var/log/cloud-init.log /var/log/cloud-init-output.log

# Verify the security/identity postconditions inside the guest before writing
# the completion marker consumed by the host pipeline.
getent passwd dba | awk -F: '$1 == "dba" && $3 == 88 && $4 == 88 && $6 == "/home/dba" && $7 == "/bin/bash" { ok=1 } END { exit !ok }'
getent group admin | awk -F: '$1 == "admin" && $3 == 88 { ok=1 } END { exit !ok }'
[[ $(stat -c '%u:%g:%a' /home/dba) == 88:88:700 ]]
for user in root "${source_user}" dba "${normal_users[@]}"; do
  [[ -n ${user} ]] || continue
  getent passwd "${user}" >/dev/null || continue
  [[ $(getent shadow "${user}" | awk -F: 'NR == 1 { print $2 }') == '!' ]]
done
for root in /root /home /etc/skel; do
  [[ -e ${root} ]] || continue
  residual=$(find "${root}" -xdev -type f \( \
    -name authorized_keys -o -name authorized_keys2 -o \
    -name 'id_rsa*' -o -name 'id_dsa*' -o -name 'id_ecdsa*' -o -name 'id_ed25519*' \
  \) -print -quit)
  [[ -z ${residual} ]] || { printf 'residual SSH credential: %s\n' "${residual}" >&2; exit 5; }
done
residual=$(find /etc/ssh -maxdepth 1 -type f -name 'ssh_host_*' -print -quit)
[[ -z ${residual} ]] || { printf 'residual SSH host key: %s\n' "${residual}" >&2; exit 5; }
[[ ! -s /etc/machine-id ]]
if [[ -d /var/lib/cloud ]]; then
  residual=$(find /var/lib/cloud -mindepth 1 -print -quit)
  [[ -z ${residual} ]] || { printf 'residual cloud-init cache: %s\n' "${residual}" >&2; exit 5; }
fi

install -d -o root -g root -m 0755 /var/lib/barn-image
python3_status=not-requested
xfsprogs_status=not-requested
legacy_network_status=not-requested
sshd_include_status=upstream
locale_default_status=not-requested
locale_en_us_status=not-requested
[[ ${profile} == el8 ]] && python3_status=verified
if [[ ${profile} == d12 || ${profile} == d13 ]]; then
  xfsprogs_status=verified
  locale_default_status=C.UTF-8
  locale_en_us_status=verified
fi
[[ ${profile} == el8 || ${profile} == el9 ]] && legacy_network_status=removed
[[ ${profile} == el8 ]] && sshd_include_status=verified
printf '{"schema":1,"recipe":"barn-official-image-normalization-v1","profile":"%s","source_user":"%s","source_date_epoch":%s,"dba_uid":88,"admin_gid":88,"credential_hygiene":"applied","python3":"%s","xfsprogs":"%s","locale_default":"%s","locale_en_us":"%s","legacy_network":"%s","sshd_include":"%s"}\n' \
  "${profile}" "${source_user}" "${source_date_epoch}" "${python3_status}" "${xfsprogs_status}" \
  "${locale_default_status}" "${locale_en_us_status}" "${legacy_network_status}" "${sshd_include_status}" \
  >/var/lib/barn-image/normalization.json
chmod 0644 /var/lib/barn-image/normalization.json

# Normalize the metadata of files created by this recipe. Filesystem journals
# and allocation are still part of the pinned native toolchain boundary; the
# host records the exact output digest and requires a repeat build comparison.
touch -d "@${source_date_epoch}" \
  /home/dba /home/dba/.ssh \
  /etc/machine-id \
  /etc/sudoers.d/90-barn-dba \
  /etc/ssh/sshd_config.d/99-barn-image.conf \
  /var/lib/barn-image /var/lib/barn-image/normalization.json
if [[ ${profile} == d12 || ${profile} == d13 ]]; then
  touch -d "@${source_date_epoch}" /etc/locale.gen /etc/default/locale
  [[ ! -e /usr/lib/locale/locale-archive ]] || touch -d "@${source_date_epoch}" /usr/lib/locale/locale-archive
fi

# EL guests enforce SELinux labels. A blanket virt-customize
# --selinux-relabel is not valid for the Debian/Ubuntu inputs, so relabel only
# the paths this cross-distribution recipe creates or replaces when restorecon
# is present. Native boot/readiness remains a promotion gate.
if command -v restorecon >/dev/null; then
  restorecon -F /etc/passwd /etc/group /etc/shadow /etc/gshadow /etc/machine-id
  restorecon -RF /home/dba /etc/sudoers.d/90-barn-dba \
    /etc/ssh/sshd_config.d/99-barn-image.conf /var/lib/barn-image
  [[ ! -e /var/lib/dbus/machine-id ]] || restorecon -F /var/lib/dbus/machine-id
fi
