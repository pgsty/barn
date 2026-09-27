#!/usr/bin/env bash
set -euo pipefail

script_directory=$(cd "$(dirname "$0")" && pwd -P)
# shellcheck disable=SC1091
source "${script_directory}/semver.sh"

if [[ $# -ne 2 ]]; then
  printf 'usage: %s <version> <absolute-goreleaser-dist>\n' "$0" >&2
  exit 2
fi
version=$1
directory=$2
farrow_is_semver "${version}" || { printf 'invalid version: %s\n' "${version}" >&2; exit 2; }
[[ ${directory} == /* && -d ${directory} ]] || { printf 'GoReleaser dist must be an absolute directory\n' >&2; exit 2; }
if [[ ! ${SOURCE_DATE_EPOCH:-} =~ ^[0-9]+$ ]] || (( SOURCE_DATE_EPOCH <= 0 )); then
  printf 'SOURCE_DATE_EPOCH must be positive\n' >&2
  exit 2
fi
# shellcheck disable=SC1091
source "${script_directory}/binary-format.sh"
for tool in awk cmp date diff file find go grep jq python3 sed shasum stat tar tr; do
  command -v "${tool}" >/dev/null || { printf 'required GoReleaser verification tool is missing: %s\n' "${tool}" >&2; exit 3; }
done
repo=$(cd "$(dirname "$0")/.." && pwd -P)
# shellcheck disable=SC1091
source "${repo}/packaging/payload-inventory.sh"
# shellcheck disable=SC1091
source "${repo}/packaging/toolchain.env"
if date -u -r "${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ >/dev/null 2>&1; then
  created=$(date -u -r "${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ)
else
  created=$(date -u -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ)
fi

temporary_parent=$(cd "${TMPDIR:-/tmp}" && pwd -P)
temporary=$(mktemp -d "${temporary_parent}/farrow-goreleaser-verify.XXXXXX")
temporary=$(cd "${temporary}" && pwd -P)
cleanup() {
  case ${temporary} in
    "${temporary_parent}"/farrow-goreleaser-verify.*) rm -rf -- "${temporary}" ;;
    *) printf 'refuse unsafe GoReleaser verification cleanup: %s\n' "${temporary}" >&2 ;;
  esac
}
trap cleanup EXIT

expected_paths=()
inventory=$(farrow_archive_payload_paths "${repo}")
[[ -n ${inventory} ]] || { printf 'release archive payload inventory is empty\n' >&2; exit 1; }
while IFS= read -r path; do
  expected_paths+=("${path}")
done <<<"${inventory}"
common_paths=("${expected_paths[@]}")

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

(cd "${directory}" && shasum -a 256 -c checksums.txt)
checksum_names=$(awk '{print $2}' "${directory}/checksums.txt" | LC_ALL=C sort)
expected_checksum_names=$(
  {
    for os_name in darwin linux; do
      for arch in amd64 arm64; do
        printf 'farrow_%s_%s_%s.tar.gz\n' "${version}" "${os_name}" "${arch}"
        printf 'farrow_%s_%s_%s.tar.gz.spdx.json\n' "${version}" "${os_name}" "${arch}"
      done
    done
    for arch in amd64 arm64; do
      for format in deb rpm; do
        printf 'farrow_%s_linux_%s.%s\n' "${version}" "${arch}" "${format}"
        printf 'farrow_%s_linux_%s.%s.spdx.json\n' "${version}" "${arch}" "${format}"
      done
    done
  } | LC_ALL=C sort
)
[[ ${checksum_names} == "${expected_checksum_names}" ]] || { printf 'unexpected GoReleaser checksum inventory\n' >&2; exit 1; }

host_os=$(uname -s | tr '[:upper:]' '[:lower:]')
host_arch=$(uname -m)
[[ ${host_arch} == x86_64 ]] && host_arch=amd64
[[ ${host_arch} == aarch64 ]] && host_arch=arm64
for os_name in darwin linux; do
  for arch in amd64 arm64; do
    archive_name=farrow_${version}_${os_name}_${arch}.tar.gz
    root_name=${archive_name%.tar.gz}
    archive=${directory}/${archive_name}
    sbom=${archive}.spdx.json
    [[ -s ${archive} && -s ${sbom} ]] || { printf 'missing archive/SBOM: %s\n' "${archive}" >&2; exit 1; }
    expected_paths=("${common_paths[@]}")
    if [[ ${os_name}/${arch} == darwin/arm64 ]]; then
      native_paths=$(python3 "${repo}/packaging/mac-release.py" inventory "${version}" "${archive}")
      if [[ -n ${native_paths} ]]; then
        while IFS= read -r path; do expected_paths+=("${path}"); done <<<"${native_paths}"
      fi
    fi
    expected_list=$(printf '%s\n' "${expected_paths[@]}" | LC_ALL=C sort)
    expected_archive_list=$(
      for path in "${expected_paths[@]}"; do
        printf '%s/%s\n' "${root_name}" "${path}"
      done | LC_ALL=C sort
    )
    actual_archive_list=$(tar -tzf "${archive}" | LC_ALL=C sort)
    [[ ${actual_archive_list} == "${expected_archive_list}" ]] || { printf 'unexpected raw archive member inventory in %s\n' "${archive}" >&2; diff -u <(printf '%s\n' "${expected_archive_list}") <(printf '%s\n' "${actual_archive_list}") >&2 || true; exit 1; }
    while IFS= read -r member; do
      [[ ${member} == "${root_name}" || ${member} == "${root_name}"/* ]] || {
        printf 'archive member %q escapes its %q root in %s\n' "${member}" "${root_name}" "${archive}" >&2
        exit 1
      }
      [[ ${member} != /* && ${member} != .. && ${member} != ../* && ${member} != */../* && ${member} != */.. && ${member} != *\\* ]] || { printf 'unsafe archive entry %q in %s\n' "${member}" "${archive}" >&2; exit 1; }
    done < <(tar -tzf "${archive}")
    if tar -tvzf "${archive}" | awk 'substr($1,1,1) != "-" {bad=1} END {exit bad}'; then
      :
    else
      printf 'archive contains a non-regular-file entry: %s\n' "${archive}" >&2
      exit 1
    fi
    extract=${temporary}/${os_name}-${arch}
    install -d -m 0700 "${extract}"
    tar -xzf "${archive}" -C "${extract}"
    root=${extract}/${root_name}
    [[ -z $(find "${root}" ! -type f ! -type d -print -quit) ]] || { printf 'archive contains a special entry: %s\n' "${archive}" >&2; exit 1; }
    actual_list=$(cd "${root}" && find . -type f -print | sed 's#^\./##' | LC_ALL=C sort)
    [[ ${actual_list} == "${expected_list}" ]] || { printf 'unexpected archive payload in %s\n' "${archive}" >&2; diff -u <(printf '%s\n' "${expected_list}") <(printf '%s\n' "${actual_list}") >&2 || true; exit 1; }
    [[ $(file_mode "${root}/bin/farrow") == 755 && $(file_mode "${root}/bin/farrow-hosts-helper") == 755 ]] || {
      printf 'archive binaries are not mode 755 in %s\n' "${archive}" >&2
      exit 1
    }
    for path in "${expected_paths[@]}"; do
      case ${path} in bin/farrow|bin/farrow-hosts-helper) continue ;; esac
      case ${path} in
        "bin/Farrow Mac.app/Contents/MacOS/farrow-mac-runner"|"bin/Farrow Mac.app/Contents/MacOS/farrow-mac-network"|"bin/Farrow Mac.app/Contents/Resources/farrow-mac-network-install")
          [[ $(file_mode "${root}/${path}") == 755 ]] || { printf 'native Mac entry point is not executable: %s\n' "${path}" >&2; exit 1; }
          continue ;;
      esac
      [[ $(file_mode "${root}/${path}") == 644 ]] || { printf 'unexpected archive mode: %s/%s\n' "${archive}" "${path}" >&2; exit 1; }
    done
    for path in "${expected_paths[@]}"; do
      [[ $(file_mtime "${root}/${path}") == "${SOURCE_DATE_EPOCH}" ]] || { printf 'archive member mtime is not the fixed source epoch: %s/%s\n' "${archive}" "${path}" >&2; exit 1; }
    done
    for binary in bin/farrow bin/farrow-hosts-helper; do
      farrow_verify_binary_format "${root}/${binary}" "${os_name}/${arch}"
      go version -m "${root}/${binary}" | sed -n '1p' | grep -F "go${FARROW_GO_VERSION}" >/dev/null || {
        printf '%s was not built with the pinned Go %s: %s\n' "${binary}" "${FARROW_GO_VERSION}" "$(go version -m "${root}/${binary}" | sed -n '1p')" >&2
        exit 1
      }
    done
    helper_sha=$(shasum -a 256 "${root}/bin/farrow-hosts-helper" | awk '{print $1}')
    grep -a -F "${helper_sha}" "${root}/bin/farrow" >/dev/null || {
      printf 'the CLI in %s does not carry the digest of its paired helper %s\n' "${archive}" "${helper_sha}" >&2
      exit 1
    }
    archive_sha=$(shasum -a 256 "${archive}" | awk '{print $1}')
    jq -e --arg name "${archive_name}" --arg created "${created}" --arg go_version "go${FARROW_GO_VERSION}" \
      --arg namespace "https://github.com/pgsty/farrow/sbom/${archive_name}/${archive_sha}" '
      .spdxVersion == "SPDX-2.3" and .name == $name and
      .creationInfo.created == $created and .documentNamespace == $namespace and
      any(.packages[]?; .name == "go.yaml.in/yaml/v3") and
      any(.packages[]?; .name == "stdlib" and .versionInfo == $go_version and .licenseDeclared == "BSD-3-Clause") and
      ((.packages | length) > 0) and ((.files | length) > 0)
    ' "${sbom}" >/dev/null || { printf 'SPDX document does not describe %s as expected: %s\n' "${archive_name}" "${sbom}" >&2; exit 1; }
    if [[ ${os_name} == "${host_os}" && ${arch} == "${host_arch}" ]]; then
      "${root}/bin/farrow" version | grep -F "farrow ${version}" >/dev/null || {
        printf 'the native archive binary does not report version %s: %s\n' "${version}" "$("${root}/bin/farrow" version)" >&2
        exit 1
      }
      if [[ -d ${root}/bin/Farrow\ Mac.app ]]; then
        codesign --verify --deep --strict "${root}/bin/Farrow Mac.app"
        if [[ ${FARROW_MAC_REQUIRE_NOTARIZATION:-0} == 1 ]]; then
          xcrun stapler validate "${root}/bin/Farrow Mac.app"
          spctl --assess --type execute "${root}/bin/Farrow Mac.app"
        fi
        "${root}/bin/Farrow Mac.app/Contents/MacOS/farrow-mac-runner" probe >/dev/null
      fi
    fi
    printf '%s verified; helper=%s\n' "${archive_name}" "${helper_sha}"
  done
done
printf 'verified GoReleaser archive/package inventory, paired helpers, SPDX documents, and checksums in %s\n' "${directory}"
