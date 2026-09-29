#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  printf 'usage: %s <artifact-basename> <document-basename>\n' "$0" >&2
  exit 2
fi
artifact=$1
document=$2
[[ ${artifact} == "$(basename "${artifact}")" && -f ${artifact} && ! -L ${artifact} ]] || { printf 'SBOM artifact must be a local regular basename\n' >&2; exit 2; }
[[ ${document} == "$(basename "${document}")" && ${document} == "${artifact}.spdx.json" && ! -e ${document} ]] || { printf 'unsafe or existing SBOM document path\n' >&2; exit 2; }
if [[ ! ${SOURCE_DATE_EPOCH:-} =~ ^[0-9]+$ ]] || (( SOURCE_DATE_EPOCH <= 0 )); then
  printf 'SOURCE_DATE_EPOCH must be positive for reproducible SBOMs\n' >&2
  exit 2
fi
for tool in jq syft; do
  command -v "${tool}" >/dev/null || { printf 'required SBOM tool is missing: %s\n' "${tool}" >&2; exit 3; }
done

if date -u -r "${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ >/dev/null 2>&1; then
  created=$(date -u -r "${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ)
else
  created=$(date -u -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ)
fi
if command -v sha256sum >/dev/null 2>&1; then
  artifact_sha=$(sha256sum "${artifact}" | awk '{print $1}')
else
  artifact_sha=$(shasum -a 256 "${artifact}" | awk '{print $1}')
fi
[[ ${artifact_sha} =~ ^[0-9a-f]{64}$ ]] || { printf 'invalid artifact SHA-256\n' >&2; exit 1; }

raw=${document}.raw
normalized=${document}.normalized
cleanup() {
  rm -f -- "${raw}" "${normalized}"
}
trap cleanup EXIT
SYFT_CHECK_FOR_APP_UPDATE=false syft scan "${artifact}" --output "spdx-json=${raw}" --enrich all >/dev/null
jq --arg namespace "https://github.com/pgsty/barn/sbom/${artifact}/${artifact_sha}" \
  --arg created "${created}" \
  '.documentNamespace = $namespace | .creationInfo.created = $created' \
  "${raw}" >"${normalized}"
mv "${normalized}" "${document}"
chmod 0644 "${document}"
