#!/usr/bin/env bash
set -euo pipefail

script_directory=$(cd "$(dirname "$0")" && pwd -P)
# shellcheck disable=SC1091
source "${script_directory}/semver.sh"
# shellcheck disable=SC1091
source "${script_directory}/binary-format.sh"

version=${1:-}
directory=${2:-}
if ! barn_is_prerelease_semver "${version}" || [[ ${directory} != /* || ! -d ${directory} ]]; then
  printf 'usage: %s <prerelease-version> <absolute-release-directory>\n' "$0" >&2
  exit 2
fi
for tool in awk cmp diff file find go grep jq ruby sed shasum stat tar tr; do
  command -v "${tool}" >/dev/null || { printf 'required archive verification tool is missing: %s\n' "${tool}" >&2; exit 3; }
done
directory=$(cd "${directory}" && pwd -P)
repo=$(cd "$(dirname "$0")/.." && pwd -P)
# shellcheck disable=SC1091
source "${repo}/packaging/payload-inventory.sh"
# shellcheck disable=SC1091
source "${repo}/packaging/toolchain.env"

(cd "${directory}" && shasum -a 256 -c checksums.txt)
expected_checksum_names=$(
  for target in darwin_arm64 darwin_amd64 linux_arm64 linux_amd64; do
    printf 'barn_%s_%s.tar.gz\n' "${version}" "${target}"
  done
  printf 'barn.rb\nrelease.json\n'
)
expected_checksum_names=$(printf '%s' "${expected_checksum_names}" | LC_ALL=C sort)
actual_checksum_names=$(awk '{print $2}' "${directory}/checksums.txt" | LC_ALL=C sort)
[[ ${actual_checksum_names} == "${expected_checksum_names}" ]] || { printf 'unexpected development checksum inventory\n' >&2; exit 1; }
ruby -c "${directory}/barn.rb" >/dev/null
jq -e --arg version "${version}" '.schema == 1 and .version == $version and .signed == false and .attested == false and .channel == "development"' "${directory}/release.json" >/dev/null

temporary_parent=$(cd "${TMPDIR:-/tmp}" && pwd -P)
temporary=$(mktemp -d "${temporary_parent}/barn-verify.XXXXXX")
temporary=$(cd "${temporary}" && pwd -P)
cleanup() {
  case ${temporary} in
    "${temporary_parent}"/barn-verify.*) rm -rf -- "${temporary}" ;;
    *) printf 'refuse unsafe archive verification cleanup: %s\n' "${temporary}" >&2 ;;
  esac
}
trap cleanup EXIT

host_os=$(uname -s | tr '[:upper:]' '[:lower:]')
host_arch=$(uname -m)
[[ ${host_arch} == x86_64 ]] && host_arch=amd64
[[ ${host_arch} == aarch64 ]] && host_arch=arm64
expected_paths=()
inventory=$(barn_development_archive_payload_paths "${repo}")
[[ -n ${inventory} ]] || { printf 'development archive payload inventory is empty\n' >&2; exit 1; }
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

for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  goos=${target%/*}
  goarch=${target#*/}
  root_name=barn_${version}_${goos}_${goarch}
  archive=${directory}/${root_name}.tar.gz
  [[ -s ${archive} ]] || { printf 'missing development archive: %s\n' "${archive}" >&2; exit 1; }
  while IFS= read -r member; do
    [[ ${member} == "${root_name}" || ${member} == "${root_name}"/* ]]
    [[ ${member} != /* && ${member} != .. && ${member} != ../* && ${member} != */../* && ${member} != */.. && ${member} != *\\* ]] || { printf 'unsafe archive entry %q in %s\n' "${member}" "${archive}" >&2; exit 1; }
  done < <(tar -tzf "${archive}")
  if ! tar -tvzf "${archive}" | awk 'substr($1, 1, 1) != "-" && substr($1, 1, 1) != "d" {bad=1} END {exit bad}'; then
    printf 'archive contains a non-file/non-directory entry: %s\n' "${archive}" >&2
    exit 1
  fi
  extract=${temporary}/${root_name}.extract
  install -d -m 0700 "${extract}"
  tar -xzf "${archive}" -C "${extract}"
  root=${extract}/${root_name}
  [[ -z $(find "${root}" ! -type f ! -type d -print -quit) ]] || { printf 'archive contains a special entry: %s\n' "${archive}" >&2; exit 1; }
  actual_list=$(cd "${root}" && find . -type f -print | sed 's#^\./##' | LC_ALL=C sort)
  [[ ${actual_list} == "${expected_list}" ]] || { printf 'unexpected development archive payload: %s\n' "${archive}" >&2; diff -u <(printf '%s\n' "${expected_list}") <(printf '%s\n' "${actual_list}") >&2 || true; exit 1; }
  [[ $(file_mode "${root}/bin/barn") == 755 && $(file_mode "${root}/bin/barn-hosts-helper") == 755 ]]
  for path in "${expected_paths[@]}"; do
    case ${path} in bin/barn|bin/barn-hosts-helper) continue ;; esac
    [[ $(file_mode "${root}/${path}") == 644 ]] || { printf 'unexpected mode for %s in %s\n' "${path}" "${archive}" >&2; exit 1; }
  done
  barn_verify_binary_format "${root}/bin/barn" "${target}"
  barn_verify_binary_format "${root}/bin/barn-hosts-helper" "${target}"
  go version -m "${root}/bin/barn" | sed -n '1p' | grep -F "go${BARN_GO_VERSION}" >/dev/null
  go version -m "${root}/bin/barn-hosts-helper" | sed -n '1p' | grep -F "go${BARN_GO_VERSION}" >/dev/null
  barn_sha=$(shasum -a 256 "${root}/bin/barn" | awk '{print $1}')
  helper_sha=$(shasum -a 256 "${root}/bin/barn-hosts-helper" | awk '{print $1}')
  grep -a -F "${helper_sha}" "${root}/bin/barn" >/dev/null
  jq -e --arg version "${version}" --arg goos "${goos}" --arg goarch "${goarch}" \
    --arg barn_sha "${barn_sha}" --arg helper_sha "${helper_sha}" '
    .schema == 1 and .version == $version and .goos == $goos and .goarch == $goarch and
    .cgo_enabled == false and .barn_sha256 == $barn_sha and .hosts_helper_sha256 == $helper_sha
  ' "${root}/BUILD_INFO.json" >/dev/null
  if [[ ${goos} == "${host_os}" && ${goarch} == "${host_arch}" ]]; then
    "${root}/bin/barn" version | grep -F "barn ${version}" >/dev/null
  fi
done

printf 'verified exact development archives, paired helpers, metadata, formula, and checksums in %s\n' "${directory}"
