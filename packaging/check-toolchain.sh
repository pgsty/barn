#!/usr/bin/env bash
set -euo pipefail

mode=${1:-all}
case ${mode} in go|packages|goreleaser|quality|all) ;; *) printf 'usage: %s go|packages|goreleaser|quality|all\n' "$0" >&2; exit 2 ;; esac
root=$(cd "$(dirname "$0")" && pwd -P)
# shellcheck disable=SC1091
source "${root}/toolchain.env"

require_version() {
  local tool=$1 actual=$2 expected=$3
  [[ ${actual} == "${expected}" ]] || { printf '%s version %s does not match pinned %s\n' "${tool}" "${actual}" "${expected}" >&2; exit 3; }
  printf '%s %s\n' "${tool}" "${actual}"
}

check_go() {
  command -v go >/dev/null || { printf 'go is missing\n' >&2; exit 3; }
  require_version go "$(go env GOVERSION | sed 's/^go//')" "${BARN_GO_VERSION}"
}
check_goreleaser() {
  command -v goreleaser >/dev/null || { printf 'goreleaser is missing\n' >&2; exit 3; }
  command -v jq >/dev/null || { printf 'jq is missing\n' >&2; exit 3; }
  local binary embedded_nfpm
  binary=$(command -v goreleaser)
  require_version goreleaser "$(goreleaser --version | awk '/GitVersion:/ {gsub(/^v/, "", $2); print $2; exit}')" "${BARN_GORELEASER_VERSION}"
  embedded_nfpm=$(go version -m "${binary}" | awk '$1 == "dep" && $2 == "github.com/goreleaser/nfpm/v2" {sub(/^v/, "", $3); print $3; exit}')
  require_version 'goreleaser embedded nFPM' "${embedded_nfpm}" "${BARN_GORELEASER_NFPM_VERSION}"
  printf 'jq %s\n' "$(jq --version | sed 's/^jq-//')"
}
check_nfpm() {
  command -v nfpm >/dev/null || { printf 'nfpm is missing\n' >&2; exit 3; }
  command -v go >/dev/null || { printf 'go is required to identify nfpm\n' >&2; exit 3; }
  local binary version
  binary=$(command -v nfpm)
  # `go install module@vX` does not apply nfpm's release ldflags, so the binary
  # reports GitVersion "dev" while its module graph still records the exact tag.
  # Trust the module first, and fall back to the stamped version for a binary
  # built from an official nfpm release, where the module reads (devel) instead.
  version=$(go version -m "${binary}" | awk '$1 == "mod" && $2 == "github.com/goreleaser/nfpm/v2" && $3 != "(devel)" {sub(/^v/, "", $3); print $3; exit}')
  if [[ -z ${version} ]]; then
    version=$(nfpm --version | awk '/GitVersion:/ && $2 != "dev" {gsub(/^v/, "", $2); print $2; exit}')
  fi
  [[ -n ${version} ]] || {
    printf 'cannot identify the nfpm version of %s; install the pinned one with: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v%s\n' \
      "${binary}" "${BARN_NFPM_VERSION}" >&2
    exit 3
  }
  require_version nfpm "${version}" "${BARN_NFPM_VERSION}"
}
check_syft() {
  command -v syft >/dev/null || { printf 'syft is missing\n' >&2; exit 3; }
  require_version syft "$(syft version | awk '/^Version:/ {gsub(/^v/, "", $2); print $2; exit}')" "${BARN_SYFT_VERSION}"
}
check_staticcheck() {
  command -v staticcheck >/dev/null || { printf 'staticcheck is missing\n' >&2; exit 3; }
  require_version staticcheck "$(staticcheck -version | awk '{print $2; exit}')" "${BARN_STATICCHECK_VERSION}"
}
check_go_module_tool() {
  local tool=$1 module=$2 expected=$3 install_path=$4 binary version
  command -v "${tool}" >/dev/null || { printf '%s is missing; install it with: go install %s@v%s\n' "${tool}" "${install_path}" "${expected}" >&2; exit 3; }
  binary=$(command -v "${tool}")
  version=$(go version -m "${binary}" | awk -v module="${module}" '$1 == "mod" && $2 == module && $3 != "(devel)" {sub(/^v/, "", $3); print $3; exit}')
  [[ -n ${version} ]] || { printf 'cannot identify the %s module version from %s\n' "${tool}" "${binary}" >&2; exit 3; }
  require_version "${tool}" "${version}" "${expected}"
}
check_deadcode() {
  check_go_module_tool deadcode golang.org/x/tools "${BARN_DEADCODE_VERSION}" golang.org/x/tools/cmd/deadcode
}
check_golangci_lint() {
  check_go_module_tool golangci-lint github.com/golangci/golangci-lint/v2 "${BARN_GOLANGCI_LINT_VERSION}" github.com/golangci/golangci-lint/v2/cmd/golangci-lint
}
check_govulncheck() {
  command -v govulncheck >/dev/null || { printf 'govulncheck is missing\n' >&2; exit 3; }
  require_version govulncheck "$(govulncheck -version | awk -F'@v' '/^Scanner:/ {print $2; exit}')" "${BARN_GOVULNCHECK_VERSION}"
}

case ${mode} in
  go) check_go ;;
  packages) check_go; check_nfpm; check_syft ;;
  goreleaser) check_go; check_goreleaser; check_syft ;;
  quality) check_go; check_staticcheck; check_deadcode; check_golangci_lint; check_govulncheck ;;
  all) check_go; check_goreleaser; check_nfpm; check_syft; check_staticcheck; check_deadcode; check_golangci_lint; check_govulncheck ;;
esac
