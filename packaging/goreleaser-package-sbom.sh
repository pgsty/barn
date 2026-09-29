#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  printf 'usage: %s <package-basename> <document-basename>\n' "$0" >&2
  exit 2
fi
artifact=$1
document=$2
[[ ${artifact} == "$(basename "${artifact}")" && -f ${artifact} && ! -L ${artifact} ]] || { printf 'SBOM package must be a local regular basename\n' >&2; exit 2; }
[[ ${document} == "$(basename "${document}")" && ${document} == "${artifact}.spdx.json" && ! -e ${document} ]] || { printf 'unsafe or existing package SBOM path\n' >&2; exit 2; }
if [[ ! ${SOURCE_DATE_EPOCH:-} =~ ^[0-9]+$ ]] || (( SOURCE_DATE_EPOCH <= 0 )); then
  printf 'SOURCE_DATE_EPOCH must be positive for package SBOMs\n' >&2
  exit 2
fi

case ${artifact} in
  *.deb) format=deb; stem=${artifact%.deb} ;;
  *.rpm) format=rpm; stem=${artifact%.rpm} ;;
  *) printf 'unsupported package SBOM artifact: %s\n' "${artifact}" >&2; exit 2 ;;
esac
case ${stem} in
  barn_*_linux_amd64) goarch=amd64; version=${stem#barn_}; version=${version%_linux_amd64} ;;
  barn_*_linux_arm64) goarch=arm64; version=${stem#barn_}; version=${version%_linux_arm64} ;;
  *) printf 'unexpected Barn package name: %s\n' "${artifact}" >&2; exit 2 ;;
esac

script_directory=$(cd "$(dirname "$0")" && pwd -P)
repo=$(cd "${script_directory}/.." && pwd -P)
# shellcheck disable=SC1091
source "${script_directory}/semver.sh"
barn_is_semver "${version}" || { printf 'invalid package SBOM version: %s\n' "${version}" >&2; exit 2; }
sbom_root=${repo}/.goreleaser-companion/linux_${goarch}/sbom-root
[[ -d ${sbom_root} && ! -L ${sbom_root} && -O ${sbom_root} ]] || { printf 'package SBOM root is missing or unsafe: %s\n' "${sbom_root}" >&2; exit 1; }
resolved_sbom_root=$(cd "${sbom_root}" && pwd -P)
[[ ${resolved_sbom_root} == "${sbom_root}" ]] || { printf 'package SBOM root resolves outside the repository stage: %s\n' "${resolved_sbom_root}" >&2; exit 1; }
sbom_root=${resolved_sbom_root}
for tool in awk basename chmod date jq mv rm shasum syft; do
  command -v "${tool}" >/dev/null || { printf 'required package SBOM tool is missing: %s\n' "${tool}" >&2; exit 3; }
done

if date -u -r "${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ >/dev/null 2>&1; then
  created=$(date -u -r "${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ)
else
  created=$(date -u -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ)
fi
package_sha=$(shasum -a 256 "${artifact}" | awk '{print $1}')
raw=${document}.raw
normalized=${document}.normalized
cleanup() { rm -f -- "${raw}" "${normalized}"; }
trap cleanup EXIT
SYFT_CHECK_FOR_APP_UPDATE=false syft scan "dir:${sbom_root}" --source-name "${artifact}" --source-version "${version}" \
  --output "spdx-json=${raw}" >/dev/null
jq --arg name "${artifact}" --arg version "${version}" --arg package_sha "${package_sha}" \
  --arg namespace "https://github.com/pgsty/barn/sbom/${version}/${goarch}/${format}/${package_sha}" \
  --arg created "${created}" '
  .name = $name |
  .documentNamespace = $namespace |
  .creationInfo.created = $created |
  (.packages[] | select(.name == $name)) |= (
    .versionInfo = $version |
    .supplier = "Organization: Pigsty" |
    .filesAnalyzed = false |
    .checksums = [{algorithm: "SHA256", checksumValue: $package_sha}] |
    .licenseConcluded = "Apache-2.0" |
    .licenseDeclared = "Apache-2.0" |
    .primaryPackagePurpose = "INSTALL"
  )' "${raw}" >"${normalized}"
mv "${normalized}" "${document}"
chmod 0644 "${document}"
