#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf 'usage: %s <darwin|linux> <amd64|arm64> [output-directory]\n' "$0" >&2
}

if [[ $# -lt 2 || $# -gt 3 ]]; then
  usage
  exit 2
fi

goos=$1
goarch=$2
case ${goos} in darwin|linux) ;; *) usage; exit 2 ;; esac
case ${goarch} in amd64|arm64) ;; *) usage; exit 2 ;; esac

repo=$(cd "$(dirname "$0")/.." && pwd -P)
output=${3:-bin/${goos}_${goarch}}
if [[ ${output} != /* ]]; then
  output=${repo}/${output}
fi
case /${output}/ in */../*|*/./*)
  printf 'development output must not contain dot path segments: %s\n' "${output}" >&2
  exit 2
  ;;
esac
case ${output} in
  "${repo}"/bin|"${repo}"/bin/*) ;;
  *)
    printf 'development output must be bin or one of its subdirectories: %s\n' "${output}" >&2
    exit 2
    ;;
esac

for tool in awk chmod cmp date git go grep install ln mkdir mktemp mv rm rmdir; do
  command -v "${tool}" >/dev/null || {
    printf 'required development build tool is missing: %s\n' "${tool}" >&2
    exit 3
  }
done

bin_root=${repo}/bin
if [[ -L ${bin_root} || (-e ${bin_root} && ! -d ${bin_root}) ]]; then
  printf 'development bin root must be a real directory: %s\n' "${bin_root}" >&2
  exit 7
fi
install -d -m 0755 "${bin_root}"
resolved_bin_root=$(cd "${bin_root}" && pwd -P)
[[ ${resolved_bin_root} == "${bin_root}" ]] || {
  printf 'development bin root must not resolve through a symlink: %s\n' "${bin_root}" >&2
  exit 7
}
existing_parent=${output}
while [[ ! -e ${existing_parent} && ! -L ${existing_parent} ]]; do
  parent=${existing_parent%/*}
  [[ -n ${parent} && ${parent} != "${existing_parent}" ]] || {
    printf 'cannot resolve development output parent: %s\n' "${output}" >&2
    exit 7
  }
  existing_parent=${parent}
done
[[ -d ${existing_parent} ]] || {
  printf 'development output parent is not a directory: %s\n' "${existing_parent}" >&2
  exit 7
}
resolved_existing_parent=$(cd "${existing_parent}" && pwd -P)
case ${resolved_existing_parent} in
  "${resolved_bin_root}"|"${resolved_bin_root}"/*) ;;
  *) printf 'development output parent resolves outside bin: %s\n' "${resolved_existing_parent}" >&2; exit 7 ;;
esac
if [[ -L ${output} || (-e ${output} && ! -d ${output}) ]]; then
  printf 'development output must be a real directory: %s\n' "${output}" >&2
  exit 7
fi
if [[ ! -e ${output} ]]; then
  install -d -m 0755 "${output}"
fi
if [[ -L ${output} || ! -d ${output} ]]; then
  printf 'development output must be a real directory: %s\n' "${output}" >&2
  exit 7
fi
output=$(cd "${output}" && pwd -P)
case ${output} in
  "${resolved_bin_root}"|"${resolved_bin_root}"/*) ;;
  *) printf 'development output resolves outside bin: %s\n' "${output}" >&2; exit 7 ;;
esac
if command -v sha256sum >/dev/null 2>&1; then
  sha256_file() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_file() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  printf 'required development build tool is missing: sha256sum or shasum\n' >&2
  exit 3
fi

version=${BARN_VERSION:-dev}
[[ ${version} =~ ^[0-9A-Za-z][0-9A-Za-z._+-]*$ ]] || {
  printf 'BARN_VERSION contains unsupported characters: %s\n' "${version}" >&2
  exit 2
}
install_keep=${BARN_INSTALL_KEEP:-3}
if [[ ! ${install_keep} =~ ^[0-9]+$ || ${#install_keep} -gt 9 ]]; then
  printf 'BARN_INSTALL_KEEP must be a non-negative integer of at most nine digits\n' >&2
  exit 2
fi
install_keep=$((10#${install_keep}))

commit=${BARN_COMMIT:-}
if [[ -n ${commit} && ! ${commit} =~ ^[0-9a-f]{40}$ && ${commit} != uncommitted ]]; then
  printf 'BARN_COMMIT must be a full lowercase Git hash or uncommitted\n' >&2
  exit 2
fi
if [[ -z ${commit} ]]; then
  commit=$(git -C "${repo}" rev-parse --verify HEAD 2>/dev/null || true)
  if [[ -n $(git -C "${repo}" status --short --untracked-files=normal 2>/dev/null) ]]; then
    commit=uncommitted
  fi
fi
[[ ${commit} =~ ^[0-9a-f]{40}$ ]] || commit=uncommitted

build_date=${BARN_BUILD_DATE:-}
if [[ -z ${build_date} ]]; then
  build_date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
fi
[[ ${build_date} =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || {
  printf 'BARN_BUILD_DATE must be an RFC3339 UTC timestamp\n' >&2
  exit 2
}

temporary_parent=$(cd "${TMPDIR:-/tmp}" && pwd -P)
temporary=$(mktemp -d "${temporary_parent}/barn-dev-build.XXXXXX")
temporary=$(cd "${temporary}" && pwd -P)
release_stage=
link_stage=
cleanup() {
  if [[ -n ${release_stage} ]]; then
    case ${release_stage} in
      "${output}"/.barn-release.next.*) rm -rf -- "${release_stage}" ;;
      *) printf 'refuse unsafe development release cleanup: %s\n' "${release_stage}" >&2 ;;
    esac
  fi
  if [[ -n ${link_stage} ]]; then
    case ${link_stage} in
      "${output}"/.barn-links.next.*) rm -rf -- "${link_stage}" ;;
      *) printf 'refuse unsafe development link cleanup: %s\n' "${link_stage}" >&2 ;;
    esac
  fi
  case ${temporary} in
    "${temporary_parent}"/barn-dev-build.*) rm -rf -- "${temporary}" ;;
    *) printf 'refuse unsafe development build cleanup: %s\n' "${temporary}" >&2 ;;
  esac
}
trap cleanup EXIT

prune_retained_releases() {
  local release_root_directory=$1 active_release=$2 keep=$3
  local resolved_root entry resolved_entry candidate oldest release_count
  if (( keep == 0 )); then
    return 0
  fi
  if [[ -L ${release_root_directory} || ! -d ${release_root_directory} || ! -O ${release_root_directory} ]]; then
    printf 'refuse unsafe development retained-release root: %s\n' "${release_root_directory}" >&2
    return 7
  fi
  resolved_root=$(cd "${release_root_directory}" && pwd -P)
  if [[ ${resolved_root} != "${release_root_directory}" ]]; then
    printf 'development retained-release root changed after validation: %s\n' "${release_root_directory}" >&2
    return 7
  fi
  case ${active_release} in
    "${resolved_root}"/*) ;;
    *) printf 'active development release resolves outside the retained-release root: %s\n' "${active_release}" >&2; return 7 ;;
  esac
  if [[ -L ${active_release} || ! -d ${active_release} || ! -O ${active_release} || $(cd "${active_release}" && pwd -P) != "${active_release}" ]]; then
    printf 'active development release is not an owned real directory: %s\n' "${active_release}" >&2
    return 7
  fi

  release_count=0
  for entry in "${resolved_root}"/*; do
    [[ -e ${entry} || -L ${entry} ]] || continue
    if [[ -L ${entry} || ! -d ${entry} || ! -O ${entry} ]]; then
      printf 'refuse unsafe development retained release: %s\n' "${entry}" >&2
      return 7
    fi
    resolved_entry=$(cd "${entry}" && pwd -P)
    if [[ ${resolved_entry} != "${entry}" ]]; then
      printf 'development retained release changed after validation: %s\n' "${entry}" >&2
      return 7
    fi
    ((release_count += 1))
  done

  while (( release_count > keep )); do
    oldest=
    for candidate in "${resolved_root}"/*; do
      [[ -e ${candidate} || -L ${candidate} ]] || continue
      if [[ -L ${candidate} || ! -d ${candidate} || ! -O ${candidate} ]]; then
        printf 'refuse unsafe development retained release: %s\n' "${candidate}" >&2
        return 7
      fi
      [[ ${candidate} == "${active_release}" ]] && continue
      if [[ -z ${oldest} || ${candidate} -ot ${oldest} ]]; then
        oldest=${candidate}
      elif [[ ! ${oldest} -ot ${candidate} && ${candidate} < ${oldest} ]]; then
        oldest=${candidate}
      fi
    done
    if [[ -z ${oldest} ]]; then
      printf 'cannot select an inactive development release without touching current\n' >&2
      return 7
    fi
    case ${oldest} in
      "${resolved_root}"/*) ;;
      *) printf 'refuse development retained release outside validated root: %s\n' "${oldest}" >&2; return 7 ;;
    esac
    if [[ ${oldest} == "${active_release}" || -L ${oldest} || ! -d ${oldest} || ! -O ${oldest} ]]; then
      printf 'refuse unsafe development retained release removal: %s\n' "${oldest}" >&2
      return 7
    fi
    rm -rf -- "${oldest}"
    ((release_count -= 1))
  done
}

helper=${temporary}/barn-hosts-helper
binary=${temporary}/barn
common_ldflags="-buildid= -X github.com/pgsty/barn/internal/version.Version=${version} -X github.com/pgsty/barn/internal/version.Commit=${commit} -X github.com/pgsty/barn/internal/version.Date=${build_date}"

(
  cd "${repo}"
  CGO_ENABLED=0 GOOS=${goos} GOARCH=${goarch} GOFLAGS=-mod=readonly \
    go build -trimpath -buildvcs=false -ldflags "${common_ldflags}" \
      -o "${helper}" ./cmd/barn-hosts-helper
)
helper_sha=$(sha256_file "${helper}")
(
  cd "${repo}"
  CGO_ENABLED=0 GOOS=${goos} GOARCH=${goarch} GOFLAGS=-mod=readonly \
    go build -trimpath -buildvcs=false \
      -ldflags "${common_ldflags} -X github.com/pgsty/barn/internal/hostconfig.ExpectedHelperSHA256=${helper_sha}" \
      -o "${binary}" ./cmd/barn
)
grep -a -F "${helper_sha}" "${binary}" >/dev/null || {
  printf 'built CLI does not contain the companion helper digest\n' >&2
  exit 1
}

binary_sha=$(sha256_file "${binary}")
releases=${output}/.barn-releases
if [[ -L ${releases} || (-e ${releases} && ! -d ${releases}) ]]; then
  printf 'development release root is unsafe: %s\n' "${releases}" >&2
  exit 7
fi
if [[ ! -e ${releases} ]]; then
  mkdir -m 0700 "${releases}"
fi
if [[ -L ${releases} || ! -d ${releases} ]]; then
  printf 'development release root is unsafe: %s\n' "${releases}" >&2
  exit 7
fi
resolved_releases=$(cd "${releases}" && pwd -P)
[[ ${resolved_releases} == "${output}/.barn-releases" && -O ${resolved_releases} ]] || {
  printf 'development release root resolves outside the owned output: %s\n' "${resolved_releases}" >&2
  exit 7
}
chmod 0700 "${resolved_releases}"
releases=${resolved_releases}
release_id=${goos}_${goarch}-${binary_sha:0:16}
release_root=${releases}/${release_id}
if [[ -e ${release_root} || -L ${release_root} ]]; then
  if [[ -L ${release_root} || ! -d ${release_root} || ! -O ${release_root} ]] ||
    ! cmp -s "${binary}" "${release_root}/barn" ||
    ! cmp -s "${helper}" "${release_root}/barn-hosts-helper"; then
    printf 'existing development release differs from the built pair: %s\n' "${release_root}" >&2
    exit 7
  fi
  chmod 0700 "${release_root}"
  chmod 0755 "${release_root}/barn" "${release_root}/barn-hosts-helper"
else
  release_stage=$(mktemp -d "${output}/.barn-release.next.XXXXXX")
  chmod 0700 "${release_stage}"
  install -m 0755 "${binary}" "${release_stage}/barn"
  install -m 0755 "${helper}" "${release_stage}/barn-hosts-helper"
  cmp -s "${binary}" "${release_stage}/barn"
  cmp -s "${helper}" "${release_stage}/barn-hosts-helper"
  mv "${release_stage}" "${release_root}"
  release_stage=
fi

for name in barn barn-hosts-helper; do
  target=${output}/${name}
  if [[ -e ${target} || -L ${target} ]]; then
    if [[ -d ${target} || (! -f ${target} && ! -L ${target}) ]]; then
      printf 'refuse to replace a non-file development entry point: %s\n' "${target}" >&2
      exit 7
    fi
  fi
done
link_stage=$(mktemp -d "${output}/.barn-links.next.XXXXXX")
chmod 0700 "${link_stage}"
for name in barn barn-hosts-helper; do
  ln -s ".barn-releases/${release_id}/${name}" "${link_stage}/${name}"
done
# barn resolves the companion beside its real versioned path, so publishing
# the CLI first cannot expose a mismatched helper even if this process stops.
for name in barn barn-hosts-helper; do
  mv -f "${link_stage}/${name}" "${output}/${name}"
done
rmdir "${link_stage}"
link_stage=

installed_helper_sha=$(sha256_file "${output}/barn-hosts-helper")
[[ ${installed_helper_sha} == "${helper_sha}" ]]
grep -a -F "${installed_helper_sha}" "${output}/barn" >/dev/null
for name in barn barn-hosts-helper; do
  [[ -L ${output}/${name} && $(readlink "${output}/${name}") == ".barn-releases/${release_id}/${name}" && -x ${output}/${name} ]] || {
    printf 'development entry point does not name the verified release: %s\n' "${output}/${name}" >&2
    exit 7
  }
done
prune_retained_releases "${releases}" "${release_root}" "${install_keep}"
printf 'built Barn for %s/%s in %s\n' "${goos}" "${goarch}" "${output}"
printf 'helper sha256 %s\n' "${installed_helper_sha}"
