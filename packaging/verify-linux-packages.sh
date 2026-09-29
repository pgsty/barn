#!/usr/bin/env bash
set -euo pipefail

script_directory=$(cd "$(dirname "$0")" && pwd -P)
# shellcheck disable=SC1091
source "${script_directory}/semver.sh"

if [[ $# -ne 4 ]]; then
  printf 'usage: %s <version> <commit-or-uncommitted> <source-epoch> <package-directory>\n' "$0" >&2
  exit 2
fi
version=$1
commit=$2
source_epoch=$3
directory=$4
barn_is_semver "${version}" || { printf 'invalid version: %s\n' "${version}" >&2; exit 2; }
[[ ${commit} == uncommitted || ${commit} =~ ^[0-9a-f]{40}$ ]] || { printf 'expected commit must be a full lowercase hash or uncommitted\n' >&2; exit 2; }
if [[ ! ${source_epoch} =~ ^[0-9]+$ ]] || (( source_epoch <= 0 )); then
  printf 'expected source epoch must be positive\n' >&2
  exit 2
fi
[[ ${directory} == /* && -d ${directory} ]] || { printf 'package directory must be absolute\n' >&2; exit 2; }
for tool in awk bsdtar cmp date diff file find go grep jq rpm sed shasum sort stat; do
  command -v "${tool}" >/dev/null || { printf 'required verification tool is missing: %s\n' "${tool}" >&2; exit 3; }
done
if date -u -r "${source_epoch}" +%Y-%m-%dT%H:%M:%SZ >/dev/null 2>&1; then
  build_date=$(date -u -r "${source_epoch}" +%Y-%m-%dT%H:%M:%SZ)
else
  build_date=$(date -u -d "@${source_epoch}" +%Y-%m-%dT%H:%M:%SZ)
fi
debian_version=${version}
if barn_is_prerelease_semver "${version}"; then
  debian_version=${version%%-*}~${version#*-}
fi
repo=$(cd "$(dirname "$0")/.." && pwd -P)
# shellcheck disable=SC1091
source "${repo}/packaging/payload-inventory.sh"
# shellcheck disable=SC1091
source "${repo}/packaging/toolchain.env"

temporary_parent=$(cd "${TMPDIR:-/tmp}" && pwd -P)
temporary=$(mktemp -d "${temporary_parent}/barn-package-verify.XXXXXX")
temporary=$(cd "${temporary}" && pwd -P)
cleanup() {
  case ${temporary} in
    "${temporary_parent}"/barn-package-verify.*) rm -rf -- "${temporary}" ;;
    *) printf 'refuse unsafe verification cleanup: %s\n' "${temporary}" >&2 ;;
  esac
}
trap cleanup EXIT

expected_paths=()
inventory=$(barn_linux_package_payload_paths "${repo}")
[[ -n ${inventory} ]] || { printf 'Linux package payload inventory is empty\n' >&2; exit 1; }
while IFS= read -r path; do
  expected_paths+=("${path}")
done <<<"${inventory}"
expected_list=$(printf '%s\n' "${expected_paths[@]}" | LC_ALL=C sort)

file_mode() {
  if stat -c '%a' "$1" >/dev/null 2>&1; then
    stat -c '%a' "$1"
  else
    stat -f '%Lp' "$1"
  fi
}

file_mtime() {
  if stat -c '%Y' "$1" >/dev/null 2>&1; then
    stat -c '%Y' "$1"
  else
    stat -f '%m' "$1"
  fi
}

verify_archive_member_mtimes() {
  local archive=$1
  local root=$2
  local label=$3
  local expected_mtime=$4
  local member normalized path
  while IFS= read -r member; do
    normalized=${member}
    while [[ ${normalized} == ./* ]]; do normalized=${normalized#./}; done
    normalized=${normalized#/}
    normalized=${normalized%/}
    [[ -n ${normalized} ]] || continue
    path=${root}/${normalized}
    [[ -e ${path} && ! -L ${path} ]] || { printf 'archive member was not extracted as a regular path: %s (%s)\n' "${member}" "${label}" >&2; exit 1; }
    [[ $(file_mtime "${path}") == "${expected_mtime}" ]] || {
      printf 'package member mtime differs from canonical value %s: %s (%s)\n' "${expected_mtime}" "${member}" "${label}" >&2
      exit 1
    }
  done < <(bsdtar -tf "${archive}")
}

extract_package() {
  local package=$1
  local format=$2
  local root=$3
  local container
  local -a data_archives
  install -d -m 0700 "${root}"
  if [[ ${format} == deb ]]; then
    container=${root}-container
    install -d -m 0700 "${container}"
    local control_count=0 data_count=0 binary_count=0 member
    while IFS= read -r member; do
      case ${member} in
        debian-binary) ((binary_count += 1)) ;;
        control.tar.*) ((control_count += 1)) ;;
        data.tar.*) ((data_count += 1)) ;;
        *) printf 'unsafe DEB container member %q in %s\n' "${member}" "${package}" >&2; exit 1 ;;
      esac
    done < <(bsdtar -tf "${package}")
    [[ ${binary_count} -eq 1 && ${control_count} -eq 1 && ${data_count} -eq 1 ]] || { printf 'unexpected DEB container inventory: %s\n' "${package}" >&2; exit 1; }
    bsdtar -xf "${package}" -C "${container}"
    verify_archive_member_mtimes "${package}" "${container}" "${package} outer container" "${source_epoch}"
    data_archives=("${container}"/data.tar.*)
    [[ ${#data_archives[@]} -eq 1 && -f ${data_archives[0]} ]] || { printf 'DEB must contain exactly one data archive: %s\n' "${package}" >&2; exit 1; }
    verify_payload_archive "${data_archives[0]}" "${package}"
    bsdtar -xf "${data_archives[0]}" -C "${root}"
    verify_archive_member_mtimes "${data_archives[0]}" "${root}" "${package} payload" "${source_epoch}"
  else
    verify_payload_archive "${package}" "${package}"
    bsdtar -xf "${package}" -C "${root}"
    # nFPM canonicalizes RPM cpio member mtimes to the Unix epoch. Binding that
    # format-specific zero is deterministic; BUILD_INFO separately binds the
    # requested source epoch and its derived UTC build date.
    verify_archive_member_mtimes "${package}" "${root}" "${package} payload" 0
  fi
}

verify_payload_archive() {
  local archive=$1
  local package=$2
  local member normalized expected allowed
  if ! bsdtar -tvf "${archive}" | awk 'substr($1, 1, 1) != "-" && substr($1, 1, 1) != "d" {bad=1} END {exit bad}'; then
    printf 'package payload contains a non-file/non-directory entry: %s\n' "${package}" >&2
    exit 1
  fi
  while IFS= read -r member; do
    normalized=${member}
    while [[ ${normalized} == ./* ]]; do normalized=${normalized#./}; done
    normalized=${normalized#/}
    normalized=${normalized%/}
    [[ -n ${normalized} ]] || continue
    [[ ${normalized} != .. && ${normalized} != ../* && ${normalized} != */../* && ${normalized} != */.. && ${normalized} != *\\* ]] || { printf 'unsafe payload path %q in %s\n' "${member}" "${package}" >&2; exit 1; }
    allowed=false
    for expected in "${expected_paths[@]}"; do
      if [[ ${normalized} == "${expected}" || ${expected} == "${normalized}"/* ]]; then
        allowed=true
        break
      fi
    done
    [[ ${allowed} == true ]] || { printf 'unexpected payload path %q in %s\n' "${member}" "${package}" >&2; exit 1; }
  done < <(bsdtar -tf "${archive}")
}

verify_deb_control() {
  local package=$1
  local arch=$2
  local container control control_root depends dependency deb_qemu deb_firmware
  local -a control_archives
  container=${temporary}/control-${arch}
  install -d -m 0700 "${container}"
  bsdtar -xf "${package}" -C "${container}"
  control_archives=("${container}"/control.tar.*)
  [[ ${#control_archives[@]} -eq 1 && -f ${control_archives[0]} ]] || { printf 'DEB must contain exactly one control archive: %s\n' "${package}" >&2; exit 1; }
  control_root=${container}/contents
  install -d -m 0700 "${control_root}"
  bsdtar -xf "${control_archives[0]}" -C "${control_root}"
  verify_archive_member_mtimes "${control_archives[0]}" "${control_root}" "${package} control" "${source_epoch}"
  control=${control_root}/control
  [[ -f ${control} && ! -L ${control} ]] || { printf 'DEB control file is missing or unsafe: %s\n' "${package}" >&2; exit 1; }
  grep -Fx 'Package: barn' "${control}" >/dev/null
  grep -Fx "Version: ${debian_version}-1" "${control}" >/dev/null
  grep -Fx "Architecture: ${arch}" "${control}" >/dev/null
  grep -Fx 'Maintainer: Pigsty <repo@pigsty.cc>' "${control}" >/dev/null
  case ${arch} in
    amd64)
      deb_qemu=qemu-system-x86
      deb_firmware=ovmf
      ;;
    arm64)
      deb_qemu=qemu-system-arm
      deb_firmware=qemu-efi-aarch64
      ;;
  esac
  depends=$(sed -n 's/^Depends: //p' "${control}")
  for dependency in "${deb_qemu}" qemu-utils "${deb_firmware}" openssh-client iproute2; do
    [[ ", ${depends}," == *", ${dependency},"* ]] || { printf 'DEB lacks required dependency %s: %s\n' "${dependency}" "${package}" >&2; exit 1; }
  done
}

verify_rpm_dependencies() {
  local package=$1
  local arch=$2
  local dependency rpm_firmware rpm_identity requirements expected_arch
  case ${arch} in
    amd64) rpm_firmware=edk2-ovmf; expected_arch=x86_64 ;;
    arm64) rpm_firmware=edk2-aarch64; expected_arch=aarch64 ;;
  esac
  rpm_identity=$(rpm -qp --qf '%{NAME}|%{VERSION}|%{RELEASE}|%{ARCH}\n' "${package}")
  [[ ${rpm_identity} == "barn|${debian_version}|1|${expected_arch}" ]] || {
    printf 'RPM identity is %s, expected barn|%s|1|%s: %s\n' "${rpm_identity}" "${debian_version}" "${expected_arch}" "${package}" >&2
    exit 1
  }
  requirements=$(rpm -qp --requires "${package}")
  for dependency in qemu-kvm qemu-img "${rpm_firmware}" openssh-clients iproute; do
    grep -Fx "${dependency}" <<<"${requirements}" >/dev/null || {
      printf 'RPM lacks hard Requires entry %s: %s\n' "${dependency}" "${package}" >&2
      exit 1
    }
  done
}

(cd "${directory}" && shasum -a 256 -c checksums.txt)
checksum_names=$(awk '{print $2}' "${directory}/checksums.txt" | LC_ALL=C sort)
expected_checksum_names=$(
  for arch in amd64 arm64; do
    for format in deb rpm; do
      printf 'barn_%s_linux_%s.%s\n' "${version}" "${arch}" "${format}"
      printf 'barn_%s_linux_%s.%s.spdx.json\n' "${version}" "${arch}" "${format}"
    done
  done | LC_ALL=C sort
)
package_checksum_names=$(
  while IFS= read -r name; do
    case ${name} in
      "barn_${version}_linux_amd64.deb"|"barn_${version}_linux_amd64.deb.spdx.json"|\
      "barn_${version}_linux_amd64.rpm"|"barn_${version}_linux_amd64.rpm.spdx.json"|\
      "barn_${version}_linux_arm64.deb"|"barn_${version}_linux_arm64.deb.spdx.json"|\
      "barn_${version}_linux_arm64.rpm"|"barn_${version}_linux_arm64.rpm.spdx.json") printf '%s\n' "${name}" ;;
    esac
  done <<<"${checksum_names}"
)
[[ ${package_checksum_names} == "${expected_checksum_names}" ]] || { printf 'unexpected Linux package checksum inventory\n' >&2; exit 1; }
for arch in amd64 arm64; do
  case ${arch} in
    amd64) architecture_pattern='x86-64' ;;
    arm64) architecture_pattern='ARM aarch64' ;;
  esac
  for format in deb rpm; do
    package=${directory}/barn_${version}_linux_${arch}.${format}
    sbom=${package}.spdx.json
    [[ -s ${package} && -s ${sbom} ]] || { printf 'missing package/SBOM: %s\n' "${package}" >&2; exit 1; }
    package_sha=$(shasum -a 256 "${package}" | awk '{print $1}')
    namespace=https://github.com/pgsty/barn/sbom/${version}/${arch}/${format}/${package_sha}
    jq -e --arg name "$(basename "${package}")" --arg version "${version}" --arg digest "${package_sha}" \
      --arg created "${build_date}" --arg namespace "${namespace}" --arg go_version "go${BARN_GO_VERSION}" '
      .spdxVersion == "SPDX-2.3" and
      .name == $name and
      .creationInfo.created == $created and .documentNamespace == $namespace and
      any(.packages[]?; .name == $name and .versionInfo == $version and any(.checksums[]?; .algorithm == "SHA256" and .checksumValue == $digest)) and
      any(.packages[]?; .name == "github.com/pgsty/barn") and
      any(.packages[]?; .name == "github.com/diskfs/go-diskfs") and
      any(.packages[]?; .name == "go.yaml.in/yaml/v3") and
      any(.packages[]?; .name == "stdlib" and .versionInfo == $go_version and .licenseDeclared == "BSD-3-Clause") and
      any(.files[]?; .fileName == "usr/bin/barn") and
      any(.files[]?; .fileName == "opt/barn/libexec/barn-hosts-helper") and
      ((.packages | length) >= 10) and ((.files | length) >= 2)
    ' "${sbom}" >/dev/null

    root=${temporary}/${arch}-${format}
    extract_package "${package}" "${format}" "${root}"
    [[ -z $(find "${root}" -type l -print -quit) ]] || { printf 'package contains a symbolic link: %s\n' "${package}" >&2; exit 1; }
    actual_list=$(cd "${root}" && find . -type f -print | sed 's#^\./##' | LC_ALL=C sort)
    [[ ${actual_list} == "${expected_list}" ]] || { printf 'unexpected package payload in %s\n' "${package}" >&2; diff -u <(printf '%s\n' "${expected_list}") <(printf '%s\n' "${actual_list}") >&2 || true; exit 1; }
    [[ $(file_mode "${root}/usr/bin/barn") == 755 ]]
    [[ $(file_mode "${root}/opt/barn/libexec/barn-hosts-helper") == 755 ]]
    for path in opt/barn opt/barn/libexec usr/share/doc/barn usr/share/doc/barn/licenses; do
      [[ -d ${root}/${path} && $(file_mode "${root}/${path}") == 755 ]] || { printf 'unexpected package directory mode for %s in %s\n' "${path}" "${package}" >&2; exit 1; }
    done
    for path in "${expected_paths[@]}"; do
      case ${path} in opt/barn/libexec/barn-hosts-helper|usr/bin/barn) continue ;; esac
      [[ $(file_mode "${root}/${path}") == 644 ]] || { printf 'unexpected mode for %s in %s\n' "${path}" "${package}" >&2; exit 1; }
    done
    file -b "${root}/usr/bin/barn" | grep -F "${architecture_pattern}" >/dev/null
    file -b "${root}/opt/barn/libexec/barn-hosts-helper" | grep -F "${architecture_pattern}" >/dev/null
    go version -m "${root}/usr/bin/barn" | sed -n '1p' | grep -F "go${BARN_GO_VERSION}" >/dev/null
    go version -m "${root}/opt/barn/libexec/barn-hosts-helper" | sed -n '1p' | grep -F "go${BARN_GO_VERSION}" >/dev/null
    barn_sha=$(shasum -a 256 "${root}/usr/bin/barn" | awk '{print $1}')
    helper_sha=$(shasum -a 256 "${root}/opt/barn/libexec/barn-hosts-helper" | awk '{print $1}')
    grep -a -F "${helper_sha}" "${root}/usr/bin/barn" >/dev/null
    grep -a -F "${commit}" "${root}/usr/bin/barn" >/dev/null
    grep -a -F "${build_date}" "${root}/usr/bin/barn" >/dev/null
    jq -e --arg version "${version}" --arg arch "${arch}" --arg commit "${commit}" --arg date "${build_date}" \
      --argjson source_epoch "${source_epoch}" --arg barn_sha "${barn_sha}" --arg helper_sha "${helper_sha}" '
      .schema == 1 and .version == $version and .goos == "linux" and .goarch == $arch and
      .commit == $commit and .date == $date and .source_date_epoch == $source_epoch and
      .cgo_enabled == false and .barn_sha256 == $barn_sha and .hosts_helper_sha256 == $helper_sha
    ' "${root}/usr/share/doc/barn/BUILD_INFO.json" >/dev/null
    if [[ ${format} == deb ]]; then
      verify_deb_control "${package}" "${arch}"
    else
      verify_rpm_dependencies "${package}" "${arch}"
    fi
  done
  for path in "${expected_paths[@]}"; do
    cmp "${temporary}/${arch}-deb/${path}" "${temporary}/${arch}-rpm/${path}"
  done
  archive=${directory}/barn_${version}_linux_${arch}.tar.gz
  if [[ -f ${archive} && ! -L ${archive} ]]; then
    archive_extract=${temporary}/${arch}-goreleaser-archive
    install -d -m 0700 "${archive_extract}"
    bsdtar -xf "${archive}" -C "${archive_extract}"
    archive_root=${archive_extract}/barn_${version}_linux_${arch}
    cmp "${archive_root}/bin/barn" "${temporary}/${arch}-deb/usr/bin/barn"
    cmp "${archive_root}/bin/barn-hosts-helper" "${temporary}/${arch}-deb/opt/barn/libexec/barn-hosts-helper"
  fi
done
printf 'verified checksums, SPDX, metadata, payload, modes, architecture, dependencies, and archive/package parity in %s\n' "${directory}"
