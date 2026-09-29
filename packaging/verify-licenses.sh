#!/usr/bin/env bash
set -euo pipefail

for tool in diff find go jq sed sort; do
  command -v "${tool}" >/dev/null || { printf 'required license verification tool is missing: %s\n' "${tool}" >&2; exit 3; }
done
repo=$(cd "$(dirname "$0")/.." && pwd -P)
# shellcheck disable=SC1091
source "${repo}/packaging/toolchain.env"
[[ $(go env GOVERSION) == "go${BARN_GO_VERSION}" ]] || { printf 'Go toolchain version differs from the license-review pin\n' >&2; exit 1; }
expected=$(printf '%s\n' \
  $'aead.dev/minisign\tv0.3.0' \
  $'github.com/diskfs/go-diskfs\tv1.9.4' \
  $'github.com/djherbis/times\tv1.6.0' \
  $'github.com/spf13/cobra\tv1.10.2' \
  $'github.com/spf13/pflag\tv1.0.10' \
  $'go.yaml.in/yaml/v3\tv3.0.5' \
  $'golang.org/x/crypto\tv0.57.0' \
  $'golang.org/x/net\tv0.59.0' \
  $'golang.org/x/sys\tv0.48.0' \
  $'golang.org/x/term\tv0.46.0' \
  $'golang.org/x/text\tv0.42.0' | LC_ALL=C sort)
actual=$(
  cd "${repo}"
  go list -deps -json ./cmd/barn ./cmd/barn-hosts-helper |
    jq -r 'select(.Module != null and .Module.Main != true) | [.Module.Path,.Module.Version] | @tsv' |
    LC_ALL=C sort -u
)
[[ ${actual} == "${expected}" ]] || { printf 'reachable module inventory differs\n' >&2; diff -u <(printf '%s\n' "${expected}") <(printf '%s\n' "${actual}") >&2 || true; exit 1; }

temporary_parent=$(cd "${TMPDIR:-/tmp}" && pwd -P)
temporary=$(mktemp -d "${temporary_parent}/barn-license-verify.XXXXXX")
temporary=$(cd "${temporary}" && pwd -P)
cleanup() {
  case ${temporary} in
    "${temporary_parent}"/barn-license-verify.*) rm -rf -- "${temporary}" ;;
    *) printf 'refuse unsafe dependency-license verification cleanup: %s\n' "${temporary}" >&2 ;;
  esac
}
trap cleanup EXIT
license_corpus=${temporary}/licenses
"${repo}/packaging/dependency-licenses.sh" stage "${license_corpus}"
expected_license_list=$("${repo}/packaging/dependency-licenses.sh" list)
actual_license_list=$(find "${license_corpus}" -mindepth 1 -maxdepth 1 -type f -print | sed 's#^.*/##' | LC_ALL=C sort)
[[ ${actual_license_list} == "${expected_license_list}" ]] || {
  printf 'generated dependency license inventory differs\n' >&2
  diff -u <(printf '%s\n' "${expected_license_list}") <(printf '%s\n' "${actual_license_list}") >&2 || true
  exit 1
}
[[ -z $(find "${license_corpus}" -mindepth 1 -maxdepth 1 ! -type f -print -quit) ]] || {
  printf 'generated dependency license corpus contains a non-regular entry\n' >&2
  exit 1
}
printf 'verified eleven reachable module versions, Go %s stdlib license, and exact upstream license/notice bytes\n' "${BARN_GO_VERSION}"
