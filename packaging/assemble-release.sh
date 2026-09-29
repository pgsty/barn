#!/usr/bin/env bash
set -euo pipefail

script_directory=$(cd "$(dirname "$0")" && pwd -P)
# shellcheck disable=SC1091
source "${script_directory}/semver.sh"

if [[ $# -ne 6 ]]; then
  printf 'usage: %s <version> <commit> <source-epoch> <goreleaser-dist> <linux-package-dir> <new-output-dir>\n' "$0" >&2
  exit 2
fi
version=$1
commit=$2
source_epoch=$3
goreleaser_dist=$4
package_directory=$5
output=$6
if ! barn_is_semver "${version}" || [[ ! ${commit} =~ ^[0-9a-f]{40}$ || ! ${source_epoch} =~ ^[0-9]+$ ]] || (( source_epoch <= 0 )); then
  printf 'invalid release identity\n' >&2
  exit 2
fi
[[ ${goreleaser_dist} == /* && -d ${goreleaser_dist} && ${package_directory} == /* && -d ${package_directory} && ${output} == /* ]] || { printf 'release input/output paths must be absolute\n' >&2; exit 2; }
[[ ! -e ${output} ]] || { printf 'release output must not already exist: %s\n' "${output}" >&2; exit 1; }
for tool in jq shasum; do
  command -v "${tool}" >/dev/null || { printf 'required release assembly tool is missing: %s\n' "${tool}" >&2; exit 3; }
done
repo=$(cd "$(dirname "$0")/.." && pwd -P)
# shellcheck disable=SC1091
source "${repo}/packaging/toolchain.env"
if date -u -r "${source_epoch}" +%Y-%m-%dT%H:%M:%SZ >/dev/null 2>&1; then
  build_date=$(date -u -r "${source_epoch}" +%Y-%m-%dT%H:%M:%SZ)
else
  build_date=$(date -u -d "@${source_epoch}" +%Y-%m-%dT%H:%M:%SZ)
fi
channel=stable
barn_is_stable_release "${version}" || channel=prerelease
install -d -m 0755 "${output}"

for os_name in darwin linux; do
  for arch in amd64 arm64; do
    archive=barn_${version}_${os_name}_${arch}.tar.gz
    for source in "${goreleaser_dist}/${archive}" "${goreleaser_dist}/${archive}.spdx.json"; do
      [[ -s ${source} && ! -L ${source} ]] || { printf 'missing GoReleaser asset: %s\n' "${source}" >&2; exit 1; }
      install -m 0644 "${source}" "${output}/"
    done
  done
done
for arch in amd64 arm64; do
  for format in deb rpm; do
    package=barn_${version}_linux_${arch}.${format}
    for source in "${package_directory}/${package}" "${package_directory}/${package}.spdx.json"; do
      [[ -s ${source} && ! -L ${source} ]] || { printf 'missing Linux package asset: %s\n' "${source}" >&2; exit 1; }
      install -m 0644 "${source}" "${output}/"
    done
  done
done

release_base=${BARN_RELEASE_BASE_URL:-https://github.com/pgsty/barn/releases/download/v${version}}
"${repo}/packaging/render-homebrew.sh" "${version}" "${release_base}" "${output}" "${output}/barn.rb"
install -m 0755 "${repo}/packaging/install.sh" "${output}/install.sh"
jq -n \
  --arg version "${version}" --arg commit "${commit}" --arg date "${build_date}" --arg channel "${channel}" \
  --arg go "${BARN_GO_VERSION}" --arg goreleaser "${BARN_GORELEASER_VERSION}" \
  --arg nfpm "${BARN_GORELEASER_NFPM_VERSION}" --arg nfpm_standalone "${BARN_NFPM_VERSION}" \
  --arg syft "${BARN_SYFT_VERSION}" \
  --arg staticcheck "${BARN_STATICCHECK_VERSION}" --arg govulncheck "${BARN_GOVULNCHECK_VERSION}" \
  --argjson epoch "${source_epoch}" \
  '{schema:1,version:$version,commit:$commit,date:$date,source_date_epoch:$epoch,channel:$channel,
    targets:["darwin/amd64","darwin/arm64","linux/amd64","linux/arm64"],
    packages:["deb/amd64","deb/arm64","rpm/amd64","rpm/arm64"],
    toolchain:{go:$go,goreleaser:$goreleaser,nfpm:$nfpm,nfpm_engine:"goreleaser-embedded",nfpm_standalone:$nfpm_standalone,syft:$syft,staticcheck:$staticcheck,govulncheck:$govulncheck},
    signed:false,attested:false}' >"${output}/release.json"
chmod 0644 "${output}/release.json"

(
  cd "${output}"
  shasum -a 256 barn_* barn.rb install.sh release.json | LC_ALL=C sort >checksums.txt
)
chmod 0644 "${output}/checksums.txt"
[[ $(wc -l <"${output}/checksums.txt" | tr -d ' ') -eq 19 ]] || { printf 'unexpected final release asset count\n' >&2; exit 1; }
printf 'assembled 19 checksummed release assets in %s\n' "${output}"
