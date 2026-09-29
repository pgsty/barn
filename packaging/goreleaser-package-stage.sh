#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 8 ]]; then
  printf 'usage: %s <linux> <amd64|arm64> <binary> <version> <commit> <date> <source-epoch> <staging-root>\n' "$0" >&2
  exit 2
fi
goos=$1
goarch=$2
binary=$3
version=$4
commit=$5
build_date=$6
source_epoch=$7
staging_root=$8
[[ ${goos} == linux && (${goarch} == amd64 || ${goarch} == arm64) ]] || { printf 'unsupported package target: %s/%s\n' "${goos}" "${goarch}" >&2; exit 2; }
[[ ${binary} == /* && -f ${binary} && ! -L ${binary} ]] || { printf 'package binary must be an absolute regular file\n' >&2; exit 2; }
[[ ${commit} == uncommitted || ${commit} =~ ^[0-9a-f]{40}$ ]] || { printf 'package commit must be a full lowercase hash or uncommitted\n' >&2; exit 2; }
[[ ${build_date} =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || { printf 'package date must be RFC3339 UTC\n' >&2; exit 2; }
if [[ ! ${source_epoch} =~ ^[0-9]+$ ]] || (( source_epoch <= 0 )); then
  printf 'package source epoch must be positive\n' >&2
  exit 2
fi

script_directory=$(cd "$(dirname "$0")" && pwd -P)
repo=$(cd "${script_directory}/.." && pwd -P)
# shellcheck disable=SC1091
source "${script_directory}/semver.sh"
barn_is_semver "${version}" || { printf 'invalid package version: %s\n' "${version}" >&2; exit 2; }
if [[ ${staging_root} != /* ]]; then
  staging_root=${repo}/${staging_root}
fi
expected_root=${repo}/.goreleaser-companion
[[ ${staging_root} == "${expected_root}" && -d ${staging_root} && ! -L ${staging_root} && -O ${staging_root} ]] || { printf 'unsafe package staging root\n' >&2; exit 2; }
staging_root=$(cd "${staging_root}" && pwd -P)
[[ ${staging_root} == "${expected_root}" ]] || { printf 'package staging root resolves outside the repository\n' >&2; exit 2; }
stage=${staging_root}/${goos}_${goarch}
case ${stage} in
  "${staging_root}"/linux_amd64|"${staging_root}"/linux_arm64) ;;
  *) printf 'unsafe package stage: %s\n' "${stage}" >&2; exit 2 ;;
esac
[[ -d ${stage} && ! -L ${stage} && -O ${stage} ]] || { printf 'unsafe package target stage: %s\n' "${stage}" >&2; exit 2; }
resolved_stage=$(cd "${stage}" && pwd -P)
[[ ${resolved_stage} == "${expected_root}/${goos}_${goarch}" ]] || { printf 'package target stage resolves outside the repository\n' >&2; exit 2; }
stage=${resolved_stage}
helper=${stage}/barn-hosts-helper
[[ -f ${helper} && ! -L ${helper} ]] || { printf 'paired package helper is missing: %s\n' "${helper}" >&2; exit 1; }

for tool in awk chmod cp find grep install jq rm shasum touch; do
  command -v "${tool}" >/dev/null || { printf 'required package staging tool is missing: %s\n' "${tool}" >&2; exit 3; }
done
package_content=${stage}/package-content
sbom_root=${stage}/sbom-root
for path in "${package_content}" "${sbom_root}"; do
  case ${path} in
    "${stage}"/package-content|"${stage}"/sbom-root) rm -rf -- "${path}" ;;
    *) printf 'refuse unsafe package stage cleanup: %s\n' "${path}" >&2; exit 1 ;;
  esac
done
install -d -m 0755 \
  "${package_content}/opt/barn/libexec" \
  "${package_content}/usr/bin" \
  "${package_content}/usr/share/doc/barn/licenses"
install -m 0755 "${helper}" "${package_content}/opt/barn/libexec/barn-hosts-helper"
install -m 0644 "${repo}/LICENSE" "${repo}/README.md" "${package_content}/usr/share/doc/barn/"
"${repo}/packaging/dependency-licenses.sh" stage "${package_content}/usr/share/doc/barn/licenses"

binary_sha=$(shasum -a 256 "${binary}" | awk '{print $1}')
helper_sha=$(shasum -a 256 "${helper}" | awk '{print $1}')
grep -a -F "${helper_sha}" "${binary}" >/dev/null || { printf 'package binary does not contain its helper digest\n' >&2; exit 1; }
jq -n \
  --arg version "${version}" --arg commit "${commit}" --arg date "${build_date}" \
  --arg arch "${goarch}" --arg binary_sha "${binary_sha}" --arg helper_sha "${helper_sha}" \
  --argjson source_epoch "${source_epoch}" \
  '{schema:1,version:$version,commit:$commit,date:$date,source_date_epoch:$source_epoch,goos:"linux",goarch:$arch,cgo_enabled:false,barn_sha256:$binary_sha,hosts_helper_sha256:$helper_sha}' \
  >"${package_content}/usr/share/doc/barn/BUILD_INFO.json"
chmod 0644 "${package_content}/usr/share/doc/barn/BUILD_INFO.json"

touch_stamp=${build_date:0:4}${build_date:5:2}${build_date:8:2}${build_date:11:2}${build_date:14:2}.${build_date:17:2}
while IFS= read -r -d '' path; do
  TZ=UTC touch -t "${touch_stamp}" "${path}"
done < <(find "${package_content}" -depth -print0)

install -d -m 0755 "${sbom_root}"
cp -R "${package_content}/." "${sbom_root}/"
install -m 0755 "${binary}" "${sbom_root}/usr/bin/barn"
printf 'prepared GoReleaser package payload for %s/%s\n' "${goos}" "${goarch}"
