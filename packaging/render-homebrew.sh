#!/usr/bin/env bash
set -euo pipefail

script_directory=$(cd "$(dirname "$0")" && pwd -P)
# shellcheck disable=SC1091
source "${script_directory}/semver.sh"

if [[ $# -ne 4 ]]; then
  printf 'usage: %s <version> <HTTPS-release-base> <absolute-archive-directory> <absolute-formula-output>\n' "$0" >&2
  exit 2
fi
version=$1
release_base=$2
archive_directory=$3
output=$4
barn_is_semver "${version}" || { printf 'invalid formula version\n' >&2; exit 2; }
[[ ${release_base} =~ ^https://[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?(/[A-Za-z0-9._~/%+@=-]*)?$ ]] || { printf 'release base must be a narrowly encoded HTTPS URL\n' >&2; exit 2; }
[[ ${archive_directory} == /* && -d ${archive_directory} && ${output} == /* && ! -L ${output} ]] || { printf 'archive directory/output paths are unsafe\n' >&2; exit 2; }
for tool in ruby sed shasum; do
  command -v "${tool}" >/dev/null || { printf 'required formula tool is missing: %s\n' "${tool}" >&2; exit 3; }
done
archive_directory=$(cd "${archive_directory}" && pwd -P)
template=$(cd "$(dirname "$0")" && pwd -P)/homebrew/barn.rb.tmpl
[[ -f ${template} && ! -L ${template} ]] || { printf 'Homebrew template is missing or unsafe\n' >&2; exit 1; }

darwin_arm64=${archive_directory}/barn_${version}_darwin_arm64.tar.gz
darwin_amd64=${archive_directory}/barn_${version}_darwin_amd64.tar.gz
linux_arm64=${archive_directory}/barn_${version}_linux_arm64.tar.gz
linux_amd64=${archive_directory}/barn_${version}_linux_amd64.tar.gz
for archive in "${darwin_arm64}" "${darwin_amd64}" "${linux_arm64}" "${linux_amd64}"; do
  [[ -s ${archive} && ! -L ${archive} ]] || { printf 'missing formula archive: %s\n' "${archive}" >&2; exit 1; }
done
sha_darwin_arm64=$(shasum -a 256 "${darwin_arm64}" | awk '{print $1}')
sha_darwin_amd64=$(shasum -a 256 "${darwin_amd64}" | awk '{print $1}')
sha_linux_arm64=$(shasum -a 256 "${linux_arm64}" | awk '{print $1}')
sha_linux_amd64=$(shasum -a 256 "${linux_amd64}" | awk '{print $1}')

sed \
  -e "s|@VERSION@|${version}|g" \
  -e "s|@BASE_URL@|${release_base}|g" \
  -e "s|@DARWIN_ARM64_SHA@|${sha_darwin_arm64}|g" \
  -e "s|@DARWIN_AMD64_SHA@|${sha_darwin_amd64}|g" \
  -e "s|@LINUX_AMD64_SHA@|${sha_linux_amd64}|g" \
  -e "s|@LINUX_ARM64_SHA@|${sha_linux_arm64}|g" \
  "${template}" >"${output}"
chmod 0644 "${output}"
ruby -c "${output}" >/dev/null
printf 'rendered Homebrew formula %s\n' "${output}"
