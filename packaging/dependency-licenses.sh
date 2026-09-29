#!/usr/bin/env bash
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd -P)

license_source_mappings() {
  printf '%s\n' \
    'aead.dev-minisign-LICENSE|module|aead.dev/minisign@v0.3.0/LICENSE' \
    'github.com-diskfs-go-diskfs-LICENSE|module|github.com/diskfs/go-diskfs@v1.9.4/LICENSE' \
    'github.com-djherbis-times-LICENSE|module|github.com/djherbis/times@v1.6.0/LICENSE' \
    'github.com-spf13-cobra-LICENSE|module|github.com/spf13/cobra@v1.10.2/LICENSE.txt' \
    'github.com-spf13-pflag-LICENSE|module|github.com/spf13/pflag@v1.0.10/LICENSE' \
    'go.yaml.in-yaml-v3-LICENSE|module|go.yaml.in/yaml/v3@v3.0.5/LICENSE' \
    'go.yaml.in-yaml-v3-NOTICE|module|go.yaml.in/yaml/v3@v3.0.5/NOTICE' \
    'golang.org-go-stdlib-LICENSE|go|LICENSE' \
    'golang.org-x-crypto-LICENSE|module|golang.org/x/crypto@v0.57.0/LICENSE' \
    'golang.org-x-net-LICENSE|module|golang.org/x/net@v0.59.0/LICENSE' \
    'golang.org-x-sys-LICENSE|module|golang.org/x/sys@v0.48.0/LICENSE' \
    'golang.org-x-term-LICENSE|module|golang.org/x/term@v0.46.0/LICENSE' \
    'golang.org-x-text-LICENSE|module|golang.org/x/text@v0.42.0/LICENSE'
}

license_names() {
  local name kind source
  while IFS='|' read -r name kind source; do
    printf '%s\n' "${name}"
  done < <(license_source_mappings)
}

action=${1:-}
case ${action} in
  list)
    [[ $# -eq 1 ]] || { printf 'usage: %s list\n' "$0" >&2; exit 2; }
    license_names | LC_ALL=C sort
    exit 0
    ;;
  clean)
    [[ $# -eq 2 ]] || { printf 'usage: %s clean .goreleaser-licenses\n' "$0" >&2; exit 2; }
    target=$2
    if [[ ${target} != /* ]]; then
      target=${repo}/${target}
    fi
    expected=${repo}/.goreleaser-licenses
    [[ ${target} == "${expected}" ]] || { printf 'refuse unsupported generated-license cleanup: %s\n' "${target}" >&2; exit 2; }
    [[ -e ${target} || -L ${target} ]] || exit 0
    [[ -d ${target} && ! -L ${target} && -O ${target} ]] || { printf 'generated-license directory is unsafe: %s\n' "${target}" >&2; exit 1; }
    resolved=$(cd "${target}" && pwd -P)
    [[ ${resolved} == "${expected}" ]] || { printf 'generated-license directory resolves outside the repository: %s\n' "${resolved}" >&2; exit 1; }
    rm -rf -- "${resolved}"
    exit 0
    ;;
  stage)
    [[ $# -eq 2 ]] || { printf 'usage: %s stage <empty-destination-directory>\n' "$0" >&2; exit 2; }
    ;;
  *)
    printf 'usage: %s list | stage <empty-destination-directory> | clean .goreleaser-licenses\n' "$0" >&2
    exit 2
    ;;
esac

for tool in find go install sed sort; do
  command -v "${tool}" >/dev/null || { printf 'required dependency-license tool is missing: %s\n' "${tool}" >&2; exit 3; }
done
# shellcheck disable=SC1091
source "${repo}/packaging/toolchain.env"
[[ $(go env GOVERSION) == "go${BARN_GO_VERSION}" ]] || { printf 'Go toolchain version differs from the license-review pin\n' >&2; exit 1; }
(
  cd "${repo}"
  GOFLAGS=-mod=readonly go mod download
)
module_cache=$(go env GOMODCACHE)
go_root=$(go env GOROOT)
go_license=${go_root}/LICENSE
if [[ ! -f ${go_license} ]]; then
  go_license=$(cd "${go_root}/.." && pwd -P)/LICENSE
fi
[[ -f ${go_license} && ! -L ${go_license} ]] || { printf 'Go standard-library LICENSE is missing from the pinned toolchain\n' >&2; exit 1; }

destination=$2
if [[ ${destination} != /* ]]; then
  destination=${repo}/${destination}
fi
if [[ -e ${destination} || -L ${destination} ]]; then
  [[ -d ${destination} && ! -L ${destination} && -O ${destination} ]] || { printf 'dependency-license destination is unsafe: %s\n' "${destination}" >&2; exit 1; }
  [[ -z $(find "${destination}" -mindepth 1 -maxdepth 1 -print -quit) ]] || { printf 'dependency-license destination is not empty: %s\n' "${destination}" >&2; exit 1; }
else
  install -d -m 0755 "${destination}"
fi
destination=$(cd "${destination}" && pwd -P)
[[ ${destination} != "${repo}" && ${destination} != "${repo}/.git" && ${destination} != / ]] || { printf 'dependency-license destination is too broad: %s\n' "${destination}" >&2; exit 1; }

while IFS='|' read -r name kind relative_source; do
  [[ ${name} =~ ^[A-Za-z0-9._-]+$ ]] || { printf 'unsafe dependency-license name: %s\n' "${name}" >&2; exit 1; }
  case ${kind} in
    module) source=${module_cache}/${relative_source} ;;
    go) source=${go_license} ;;
    *) printf 'unsupported dependency-license source kind: %s\n' "${kind}" >&2; exit 1 ;;
  esac
  [[ -f ${source} && ! -L ${source} ]] || { printf 'dependency-license source is missing or unsafe: %s\n' "${source}" >&2; exit 1; }
  install -m 0644 "${source}" "${destination}/${name}"
done < <(license_source_mappings)

expected=$(license_names | LC_ALL=C sort)
actual=$(find "${destination}" -mindepth 1 -maxdepth 1 -type f -print | sed 's#^.*/##' | LC_ALL=C sort)
[[ ${actual} == "${expected}" ]] || { printf 'staged dependency-license inventory differs\n' >&2; exit 1; }
[[ -z $(find "${destination}" -mindepth 1 -maxdepth 1 ! -type f -print -quit) ]] || { printf 'staged dependency-license corpus contains a non-regular entry\n' >&2; exit 1; }
