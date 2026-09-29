#!/usr/bin/env bash
set -euo pipefail

script_directory=$(cd "$(dirname "$0")" && pwd -P)
# shellcheck disable=SC1091
source "${script_directory}/semver.sh"

usage() {
  printf 'usage: %s <version> [new-output-directory]\n' "$0" >&2
  printf 'the checked-out clean commit must be tagged v<version>\n' >&2
}

if [[ $# -lt 1 || $# -gt 2 ]]; then
  usage
  exit 2
fi
version=$1
farrow_is_semver "${version}" || {
  printf 'invalid release version: %s\n' "${version}" >&2
  exit 2
}

repo=$(cd "$(dirname "$0")/.." && pwd -P)
output=${2:-dist/releases/${version}}
if [[ ${output} != /* ]]; then
  output=${repo}/${output}
fi
case /${output}/ in
  */../*|*/./*) printf 'release output must not contain dot path segments: %s\n' "${output}" >&2; exit 2 ;;
esac
case ${output} in
  *//*) printf 'release output must not contain repeated path separators: %s\n' "${output}" >&2; exit 2 ;;
esac
goreleaser_dist=${repo}/.goreleaser-dist
companion_stage=${repo}/.goreleaser-companion
license_stage=${repo}/.goreleaser-licenses
case ${output} in
  "${goreleaser_dist}"|"${goreleaser_dist}"/*|"${companion_stage}"|"${companion_stage}"/*|"${license_stage}"|"${license_stage}"/*)
    printf 'release output must be outside GoReleaser temporary roots: %s\n' "${output}" >&2
    exit 2
    ;;
esac
if [[ -e ${output} || -L ${output} ]]; then
  printf 'refuse to overwrite local release output: %s\n' "${output}" >&2
  exit 1
fi
existing_parent=${output}
while [[ ! -e ${existing_parent} && ! -L ${existing_parent} ]]; do
  parent=${existing_parent%/*}
  [[ -n ${parent} && ${parent} != "${existing_parent}" ]] || { printf 'cannot resolve release output parent: %s\n' "${output}" >&2; exit 2; }
  existing_parent=${parent}
done
[[ -d ${existing_parent} ]] || { printf 'release output parent is not a directory: %s\n' "${existing_parent}" >&2; exit 2; }
resolved_parent=$(cd "${existing_parent}" && pwd -P)
output=${resolved_parent}${output#"${existing_parent}"}
case ${output} in
  "${goreleaser_dist}"|"${goreleaser_dist}"/*|"${companion_stage}"|"${companion_stage}"/*|"${license_stage}"|"${license_stage}"/*)
    printf 'release output resolves inside a GoReleaser temporary root: %s\n' "${output}" >&2
    exit 2
    ;;
esac

command -v tr >/dev/null || { printf 'required local release tool is missing: tr\n' >&2; exit 3; }
folded_output=$(printf '%s' "${output}" | tr '[:upper:]' '[:lower:]')
folded_dist=$(printf '%s' "${goreleaser_dist}" | tr '[:upper:]' '[:lower:]')
folded_companion=$(printf '%s' "${companion_stage}" | tr '[:upper:]' '[:lower:]')
folded_licenses=$(printf '%s' "${license_stage}" | tr '[:upper:]' '[:lower:]')
case ${folded_output} in
  "${folded_dist}"|"${folded_dist}"/*|"${folded_companion}"|"${folded_companion}"/*|"${folded_licenses}"|"${folded_licenses}"/*)
    printf 'release output case-folds into a reserved GoReleaser root: %s\n' "${output}" >&2
    exit 2
    ;;
esac
for tool in awk bsdtar cmp date diff file find git go goreleaser grep jq python3 rpm ruby sed shasum sort stat syft tar; do
  command -v "${tool}" >/dev/null || {
    printf 'required local release tool is missing: %s\n' "${tool}" >&2
    exit 3
  }
done
"${repo}/packaging/check-toolchain.sh" goreleaser

git -C "${repo}" diff --quiet || {
  printf 'local release requires a clean working tree\n' >&2
  exit 1
}
git -C "${repo}" diff --cached --quiet || {
  printf 'local release requires a clean index\n' >&2
  exit 1
}
[[ -z $(git -C "${repo}" status --short --untracked-files=normal) ]] || {
  printf 'local release refuses untracked files; commit, ignore, or move them first\n' >&2
  exit 1
}
origin=$(git -C "${repo}" remote get-url origin 2>/dev/null || true)
[[ -n ${origin} ]] || {
  printf 'local release requires an origin remote for GoReleaser repository metadata\n' >&2
  exit 1
}

commit=$(git -C "${repo}" rev-parse --verify HEAD)
tag_commit=$(git -C "${repo}" rev-parse --verify "refs/tags/v${version}^{commit}" 2>/dev/null || true)
[[ ${tag_commit} == "${commit}" ]] || {
  printf 'HEAD %s is not the v%s release tag\n' "${commit}" "${version}" >&2
  exit 1
}
source_epoch=$(git -C "${repo}" show -s --format=%ct "${commit}")
[[ ${source_epoch} =~ ^[0-9]+$ ]] && (( source_epoch > 0 ))
# The native Mac app is optional. When supplied it must be notarized; without
# it the release ships the CLI alone and farrow mac explains what is missing.
if [[ -n ${FARROW_MAC_PAYLOAD:-} ]]; then
  FARROW_MAC_REQUIRE_NOTARIZATION=1 python3 "${repo}/packaging/mac-release.py" check \
    "${version}" "${commit}" "${FARROW_MAC_PAYLOAD}"
else
  printf 'FARROW_MAC_PAYLOAD is not set; this release will not include the native Mac app (see docs/mac-release.md)\n' >&2
fi
if build_date=$(date -u -r "${source_epoch}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null); then
  :
else
  build_date=$(date -u -d "@${source_epoch}" +%Y-%m-%dT%H:%M:%SZ)
fi

[[ ! -e ${goreleaser_dist} && ! -L ${goreleaser_dist} ]] || {
  printf 'refuse existing GoReleaser work output: %s\n' "${goreleaser_dist}" >&2
  exit 1
}
[[ ! -e ${companion_stage} && ! -L ${companion_stage} ]] || {
  printf 'refuse existing GoReleaser companion stage: %s\n' "${companion_stage}" >&2
  exit 1
}
[[ ! -e ${license_stage} && ! -L ${license_stage} ]] || {
  printf 'refuse existing generated license stage: %s\n' "${license_stage}" >&2
  exit 1
}
cleanup() {
  if [[ -e ${goreleaser_dist} || -L ${goreleaser_dist} ]]; then
    case ${goreleaser_dist} in
      "${repo}"/.goreleaser-dist) rm -rf -- "${goreleaser_dist}" ;;
      *) printf 'refuse unsafe local release cleanup: %s\n' "${goreleaser_dist}" >&2 ;;
    esac
  fi
  "${repo}/packaging/goreleaser-companion.sh" cleanup darwin amd64 .goreleaser-companion
  "${repo}/packaging/goreleaser-companion.sh" cleanup darwin arm64 .goreleaser-companion
  "${repo}/packaging/goreleaser-companion.sh" cleanup linux amd64 .goreleaser-companion
  "${repo}/packaging/goreleaser-companion.sh" cleanup linux arm64 .goreleaser-companion
  "${repo}/packaging/dependency-licenses.sh" clean .goreleaser-licenses
}
trap cleanup EXIT

printf 'building GoReleaser archives, RPMs, and DEBs for Farrow %s\n' "${version}"
(
  cd "${repo}"
  SOURCE_DATE_EPOCH=${source_epoch} FARROW_COMMIT=${commit} FARROW_BUILD_DATE=${build_date} \
    goreleaser release --skip=publish --parallelism 1
)
postbuild_parent=${output}
while [[ ! -e ${postbuild_parent} && ! -L ${postbuild_parent} ]]; do
  parent=${postbuild_parent%/*}
  [[ -n ${parent} && ${parent} != "${postbuild_parent}" ]] || { printf 'cannot re-resolve release output parent: %s\n' "${output}" >&2; exit 2; }
  postbuild_parent=${parent}
done
[[ -d ${postbuild_parent} ]] || { printf 'release output parent became unsafe: %s\n' "${postbuild_parent}" >&2; exit 2; }
resolved_parent=$(cd "${postbuild_parent}" && pwd -P)
output=${resolved_parent}${output#"${postbuild_parent}"}
folded_output=$(printf '%s' "${output}" | tr '[:upper:]' '[:lower:]')
case ${folded_output} in
  "${folded_dist}"|"${folded_dist}"/*|"${folded_companion}"|"${folded_companion}"/*|"${folded_licenses}"|"${folded_licenses}"/*)
    printf 'release output resolves inside a live GoReleaser temporary root: %s\n' "${output}" >&2
    exit 2
    ;;
esac
if [[ -n ${FARROW_MAC_PAYLOAD:-} ]]; then
  SOURCE_DATE_EPOCH=${source_epoch} \
    FARROW_MAC_REQUIRE_NOTARIZATION=1 python3 "${repo}/packaging/mac-release.py" attach \
      "${version}" "${commit}" "${source_epoch}" "${goreleaser_dist}" "${FARROW_MAC_PAYLOAD}"
  SOURCE_DATE_EPOCH=${source_epoch} FARROW_MAC_REQUIRE_PAYLOAD=1 FARROW_MAC_REQUIRE_NOTARIZATION=1 \
    "${repo}/packaging/verify-goreleaser.sh" "${version}" "${goreleaser_dist}"
else
  SOURCE_DATE_EPOCH=${source_epoch} "${repo}/packaging/verify-goreleaser.sh" "${version}" "${goreleaser_dist}"
fi
"${repo}/packaging/verify-linux-packages.sh" \
  "${version}" "${commit}" "${source_epoch}" "${goreleaser_dist}"

printf 'assembling local release in %s\n' "${output}"
SOURCE_DATE_EPOCH=${source_epoch} \
  "${repo}/packaging/assemble-release.sh" "${version}" "${commit}" "${source_epoch}" \
    "${goreleaser_dist}" "${goreleaser_dist}" "${output}"
printf 'local release ready: %s\n' "${output}"
