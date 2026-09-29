#!/usr/bin/env bash
set -euo pipefail

# User-scoped release installer. It installs no package manager, never uses
# sudo, and verifies the selected archive against the checksum file produced by
# the GitHub release workflow.

is_semver() {
  local value=${1:-} base metadata prerelease core identifier
  local -a identifiers
  [[ -n ${value} && ${value} != v* ]] || return 1
  base=${value}
  if [[ ${base} == *+* ]]; then
    metadata=${base#*+}
    base=${base%%+*}
    [[ ${metadata} =~ ^[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*$ ]] || return 1
    IFS=. read -r -a identifiers <<<"${metadata}"
    for identifier in "${identifiers[@]}"; do
      [[ -n ${identifier} && ${identifier} =~ ^[0-9A-Za-z-]+$ ]] || return 1
    done
  fi
  core=${base}
  if [[ ${core} == *-* ]]; then
    prerelease=${core#*-}
    core=${core%%-*}
    [[ ${prerelease} =~ ^[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*$ ]] || return 1
    IFS=. read -r -a identifiers <<<"${prerelease}"
    for identifier in "${identifiers[@]}"; do
      [[ -n ${identifier} && ${identifier} =~ ^[0-9A-Za-z-]+$ ]] || return 1
      if [[ ${identifier} =~ ^[0-9]+$ && ${#identifier} -gt 1 && ${identifier} == 0* ]]; then
        return 1
      fi
    done
  fi
  [[ ${core} =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
}

repository=${FARROW_RELEASE_REPOSITORY:-pgsty/farrow}
home_directory=$(cd "${HOME:?HOME is required}" && pwd -P)
install_directory=${FARROW_INSTALL_DIR:-${home_directory}/.local/bin}
version=${FARROW_VERSION:-}
install_keep=${FARROW_INSTALL_KEEP:-3}
if [[ ! ${install_keep} =~ ^[0-9]+$ || ${#install_keep} -gt 9 ]]; then
  printf 'FARROW_INSTALL_KEEP must be a non-negative integer of at most nine digits\n' >&2
  exit 2
fi
install_keep=$((10#${install_keep}))
case /${install_directory}/ in
  */../*|*/./*)
  printf 'FARROW_INSTALL_DIR must not contain dot path segments\n' >&2
  exit 2
  ;;
esac
case ${install_directory} in
  "${home_directory}"/*) ;;
  *) printf 'FARROW_INSTALL_DIR must be an absolute directory inside HOME\n' >&2; exit 2 ;;
esac

# Validate the nearest existing ancestor before creating anything. This keeps a
# HOME-internal symlink from redirecting mkdir into /etc, /usr, or another
# user's tree before the final real-path check runs.
existing_parent=${install_directory}
while [[ ! -e ${existing_parent} && ! -L ${existing_parent} ]]; do
  parent=${existing_parent%/*}
  [[ -n ${parent} && ${parent} != "${existing_parent}" ]] || {
    printf 'cannot resolve an existing installation parent: %s\n' "${install_directory}" >&2
    exit 7
  }
  existing_parent=${parent}
done
[[ -d ${existing_parent} ]] || {
  printf 'installation parent is not a directory: %s\n' "${existing_parent}" >&2
  exit 7
}
resolved_existing_parent=$(cd "${existing_parent}" && pwd -P)
case ${resolved_existing_parent} in
  "${home_directory}"|"${home_directory}"/*) ;;
  *) printf 'installation parent resolves outside HOME: %s\n' "${resolved_existing_parent}" >&2; exit 7 ;;
esac

case $(uname -s) in
  Darwin) goos=darwin ;;
  Linux) goos=linux ;;
  *) printf 'Farrow supports native macOS and Linux only\n' >&2; exit 3 ;;
esac
case $(uname -m) in
  x86_64|amd64) goarch=amd64 ;;
  arm64|aarch64) goarch=arm64 ;;
  *) printf 'Farrow supports amd64 and arm64 only\n' >&2; exit 3 ;;
esac
# A shell under Rosetta reports x86_64 on Apple Silicon; install the native build.
if [[ ${goos} == darwin && ${goarch} == amd64 && $(/usr/sbin/sysctl -in hw.optional.arm64 2>/dev/null) == 1 ]]; then
  goarch=arm64
fi

for tool in awk chmod cmp curl install ln mkdir mktemp mv readlink rm rmdir stat tar; do
  command -v "${tool}" >/dev/null || { printf 'required installer tool is missing: %s\n' "${tool}" >&2; exit 3; }
done
secure_path_to_home() {
  local secure_parent=$1 secure_mode group_mode other_mode
  while true; do
    [[ -d ${secure_parent} && ! -L ${secure_parent} && -O ${secure_parent} ]] || {
      printf 'installation path component is not an owned real directory: %s\n' "${secure_parent}" >&2
      return 1
    }
    if stat -c '%a' "${secure_parent}" >/dev/null 2>&1; then
      secure_mode=$(stat -c '%a' "${secure_parent}")
    else
      secure_mode=$(stat -f '%Lp' "${secure_parent}")
    fi
    group_mode=${secure_mode: -2:1}
    other_mode=${secure_mode: -1}
    if [[ ${group_mode} == [2367] || ${other_mode} == [2367] ]]; then
      printf 'installation path is writable by group or others: %s; run chmod go-w on it, then retry\n' "${secure_parent}" >&2
      return 1
    fi
    [[ ${secure_parent} == "${home_directory}" ]] && break
    secure_parent=${secure_parent%/*}
    case ${secure_parent} in
      "${home_directory}"|"${home_directory}"/*) ;;
      *) printf 'installation path escaped HOME while checking parents\n' >&2; return 1 ;;
    esac
  done
}

prune_retained_releases() {
  local release_root_directory=$1 active_release=$2 keep=$3
  local resolved_root entry resolved_entry candidate oldest release_count
  if (( keep == 0 )); then
    return 0
  fi
  if [[ -L ${release_root_directory} || ! -d ${release_root_directory} || ! -O ${release_root_directory} ]]; then
    printf 'refuse unsafe retained-release root: %s\n' "${release_root_directory}" >&2
    return 7
  fi
  resolved_root=$(cd "${release_root_directory}" && pwd -P)
  if [[ ${resolved_root} != "${release_root_directory}" ]]; then
    printf 'retained-release root changed after validation: %s\n' "${release_root_directory}" >&2
    return 7
  fi
  case ${active_release} in
    "${resolved_root}"/*) ;;
    *) printf 'active release resolves outside the retained-release root: %s\n' "${active_release}" >&2; return 7 ;;
  esac
  if [[ -L ${active_release} || ! -d ${active_release} || ! -O ${active_release} || $(cd "${active_release}" && pwd -P) != "${active_release}" ]]; then
    printf 'active release is not an owned real directory: %s\n' "${active_release}" >&2
    return 7
  fi

  release_count=0
  for entry in "${resolved_root}"/*; do
    [[ -e ${entry} || -L ${entry} ]] || continue
    if [[ -L ${entry} || ! -d ${entry} || ! -O ${entry} ]]; then
      printf 'refuse unsafe retained release: %s\n' "${entry}" >&2
      return 7
    fi
    resolved_entry=$(cd "${entry}" && pwd -P)
    if [[ ${resolved_entry} != "${entry}" ]]; then
      printf 'retained release changed after validation: %s\n' "${entry}" >&2
      return 7
    fi
    ((release_count += 1))
  done

  while (( release_count > keep )); do
    oldest=
    for candidate in "${resolved_root}"/*; do
      [[ -e ${candidate} || -L ${candidate} ]] || continue
      if [[ -L ${candidate} || ! -d ${candidate} || ! -O ${candidate} ]]; then
        printf 'refuse unsafe retained release: %s\n' "${candidate}" >&2
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
      printf 'cannot select an inactive retained release without touching current\n' >&2
      return 7
    fi
    case ${oldest} in
      "${resolved_root}"/*) ;;
      *) printf 'refuse retained release outside validated root: %s\n' "${oldest}" >&2; return 7 ;;
    esac
    if [[ ${oldest} == "${active_release}" || -L ${oldest} || ! -d ${oldest} || ! -O ${oldest} ]]; then
      printf 'refuse unsafe retained release removal: %s\n' "${oldest}" >&2
      return 7
    fi
    rm -rf -- "${oldest}"
    ((release_count -= 1))
  done
}

secure_path_to_home "${resolved_existing_parent}" || exit 7
if command -v sha256sum >/dev/null 2>&1; then
  sha256_file() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_file() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  printf 'required installer tool is missing: sha256sum or shasum\n' >&2
  exit 3
fi

resolved_latest=false
if [[ -z ${version} ]]; then
  resolved_latest=true
  if latest=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/${repository}/releases/latest"); then
    :
  else
    curl_status=$?
    # curl -f exits 22 on an HTTP error: GitHub answered, usually because no
    # stable release exists yet (or it is rate-limiting). Anything else never
    # reached GitHub.
    if [[ ${curl_status} -eq 22 ]]; then
      printf 'GitHub returned an HTTP error for https://github.com/%s/releases/latest: no stable release is published yet, or GitHub is rate-limiting; set FARROW_VERSION explicitly (pre-releases always need it)\n' "${repository}" >&2
      exit 2
    fi
    printf 'cannot reach github.com to find the latest Farrow release (curl exit %s); check the network or proxy settings, or set FARROW_VERSION\n' "${curl_status}" >&2
    exit 1
  fi
  tag=${latest##*/}
  version=${tag#v}
fi
if ! is_semver "${version}"; then
  if [[ ${resolved_latest} == true ]]; then
    printf 'no stable Farrow release is published at https://github.com/%s/releases/latest (pre-1.0 versions are GitHub pre-releases); set FARROW_VERSION explicitly to the pre-release version you want\n' "${repository}" >&2
  else
    printf 'cannot resolve a valid Farrow release version: %s\n' "${version}" >&2
  fi
  exit 2
fi

asset=farrow_${version}_${goos}_${goarch}.tar.gz
base=https://github.com/${repository}/releases/download/v${version}
temporary_parent=$(cd "${TMPDIR:-/tmp}" && pwd -P)
temporary=$(mktemp -d "${temporary_parent}/farrow-install.XXXXXX")
temporary=$(cd "${temporary}" && pwd -P)
release_stage=
link_stage=
install_lock=
created_entry_points=()
cleanup() {
  case ${temporary} in
    "${temporary_parent}"/farrow-install.*) rm -rf -- "${temporary}" ;;
    *) printf 'refuse unsafe installer cleanup: %s\n' "${temporary}" >&2 ;;
  esac
  if [[ -n ${release_stage} ]]; then
    case ${release_stage} in
      "${install_directory}"/.farrow-release.next.*) rm -rf -- "${release_stage}" ;;
      *) printf 'refuse unsafe release staging cleanup: %s\n' "${release_stage}" >&2 ;;
    esac
  fi
  if [[ -n ${link_stage} ]]; then
    case ${link_stage} in
      "${install_directory}"/.farrow-links.next.*) rm -rf -- "${link_stage}" ;;
      *) printf 'refuse unsafe link staging cleanup: %s\n' "${link_stage}" >&2 ;;
    esac
  fi
  # Bash 3.2 treats an empty array expansion as unbound under nounset. The
  # +guard keeps the EXIT trap safe on stock macOS and on early failures.
  for target in ${created_entry_points[@]+"${created_entry_points[@]}"}; do
    name=${target##*/}
    if [[ -L ${target} && $(readlink "${target}") == ".farrow-current/${name}" ]]; then
      rm -f -- "${target}"
    fi
  done
  if [[ -n ${install_lock} ]]; then
    rmdir "${install_lock}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

printf 'Downloading Farrow %s for %s/%s...\n' "${version}" "${goos}" "${goarch}"
curl -fsSLo "${temporary}/${asset}" "${base}/${asset}"
curl -fsSLo "${temporary}/checksums.txt" "${base}/checksums.txt"
expected=$(awk -v asset="${asset}" '$2 == asset {print $1}' "${temporary}/checksums.txt")
[[ ${expected} =~ ^[0-9a-f]{64}$ ]] || { printf 'release checksum entry is missing or ambiguous: %s\n' "${asset}" >&2; exit 7; }
actual=$(sha256_file "${temporary}/${asset}")
[[ ${actual} == "${expected}" ]] || { printf 'release archive checksum mismatch\n' >&2; exit 7; }

root=farrow_${version}_${goos}_${goarch}
while IFS= read -r member; do
  normalized=${member#./}
  case ${normalized} in
    "${root}"|"${root}"/*) ;;
    *) printf 'release archive contains an unexpected path: %s\n' "${member}" >&2; exit 7 ;;
  esac
  case /${normalized}/ in */../*) printf 'release archive contains a parent path: %s\n' "${member}" >&2; exit 7 ;; esac
done < <(tar -tzf "${temporary}/${asset}")
if ! tar -tvzf "${temporary}/${asset}" | awk 'substr($1,1,1) != "-" && substr($1,1,1) != "d" {bad=1} END {exit bad}'; then
  printf 'release archive contains a non-file/non-directory entry\n' >&2
  exit 7
fi
tar -xzf "${temporary}/${asset}" -C "${temporary}"
for source in bin/farrow bin/farrow-hosts-helper; do
  [[ -f ${temporary}/${root}/${source} && ! -L ${temporary}/${root}/${source} ]] || {
    printf 'release archive is missing %s\n' "${source}" >&2
    exit 7
  }
done

mac_app="${temporary}/${root}/bin/Farrow Mac.app"
has_mac_app=false
if [[ -e ${mac_app} || -e ${temporary}/${root}/MACOS.json ]]; then
  [[ ${goos} == darwin && ${goarch} == arm64 && -d ${mac_app} && -f ${temporary}/${root}/MACOS.json && -f ${temporary}/${root}/MACOS.md ]] || {
    printf 'native Mac payload is incomplete or for the wrong platform\n' >&2; exit 7;
  }
  /usr/bin/codesign --verify --deep --strict "${mac_app}" || { printf 'native Mac app signature is invalid\n' >&2; exit 7; }
  has_mac_app=true
fi

verify_mac_app() {
  local destination=$1
  [[ -d ${destination}/Farrow\ Mac.app && ! -L ${destination}/Farrow\ Mac.app ]] || return 1
  /usr/bin/diff -qr "${mac_app}" "${destination}/Farrow Mac.app" >/dev/null || return 1
  for name in farrow-mac-runner; do
    [[ -x ${destination}/Farrow\ Mac.app/Contents/MacOS/${name} ]] || return 1
  done
  cmp -s "${temporary}/${root}/MACOS.json" "${destination}/MACOS.json" || return 1
  cmp -s "${temporary}/${root}/MACOS.md" "${destination}/MACOS.md" || return 1
  /usr/bin/codesign --verify --deep --strict "${destination}/Farrow Mac.app"
}

if [[ -L ${install_directory} || (-e ${install_directory} && ! -d ${install_directory}) ]]; then
  printf 'installation target is not a real directory: %s\n' "${install_directory}" >&2
  exit 7
fi
if [[ ! -e ${install_directory} ]]; then
  install -d -m 0755 "${install_directory}"
fi
if [[ -L ${install_directory} || ! -d ${install_directory} ]]; then
  printf 'installation target is not a real directory: %s\n' "${install_directory}" >&2
  exit 7
fi
resolved_install_directory=$(cd "${install_directory}" && pwd -P)
case ${resolved_install_directory} in
  "${home_directory}"/*) install_directory=${resolved_install_directory} ;;
  *) printf 'installation target resolves outside HOME: %s\n' "${resolved_install_directory}" >&2; exit 7 ;;
esac
[[ -O ${install_directory} ]] || { printf 'installation target is not owned by the current user: %s\n' "${install_directory}" >&2; exit 7; }
secure_path_to_home "${install_directory}" || exit 7

lock_path=${install_directory}/.farrow-install.lock
if ! mkdir -m 0700 "${lock_path}"; then
  printf 'another Farrow installer is active, or a stale lock remains: %s\nif no installer is running, remove it with: rmdir %s\n' "${lock_path}" "${lock_path}" >&2
  exit 4
fi
install_lock=${lock_path}
releases=${install_directory}/.farrow-releases
if [[ -L ${releases} || (-e ${releases} && ! -d ${releases}) ]]; then
  printf 'versioned installation root is unsafe: %s\n' "${releases}" >&2
  exit 7
fi
if [[ ! -e ${releases} ]]; then
  mkdir -m 0700 "${releases}"
fi
if [[ -L ${releases} || ! -d ${releases} ]]; then
  printf 'versioned installation root is unsafe: %s\n' "${releases}" >&2
  exit 7
fi
resolved_releases=$(cd "${releases}" && pwd -P)
[[ ${resolved_releases} == "${install_directory}/.farrow-releases" && -O ${resolved_releases} ]] || {
  printf 'versioned installation root resolves outside the owned install directory: %s\n' "${resolved_releases}" >&2
  exit 7
}
chmod 0700 "${resolved_releases}"
releases=${resolved_releases}

for name in farrow farrow-hosts-helper; do
  target=${install_directory}/${name}
  if [[ -e ${target} || -L ${target} ]]; then
    if [[ ! -L ${target} || $(readlink "${target}") != ".farrow-current/${name}" ]]; then
      printf 'refuse to replace an unmanaged entry point: %s\n' "${target}" >&2
      exit 7
    fi
  fi
done
current=${install_directory}/.farrow-current
if [[ -e ${current} && ! -L ${current} ]]; then
  printf 'current release pointer is not a symlink: %s\n' "${current}" >&2
  exit 7
fi

release_id=${version}-${actual:0:16}
release_root=${releases}/${release_id}
if [[ -e ${release_root} || -L ${release_root} ]]; then
  if [[ -L ${release_root} || ! -d ${release_root} || ! -O ${release_root} ]]; then
    printf 'existing versioned release root is unsafe: %s\n' "${release_root}" >&2
    exit 7
  fi
  chmod 0700 "${release_root}"
  for name in farrow farrow-hosts-helper; do
    if [[ ! -f ${release_root}/${name} || -L ${release_root}/${name} || ! -O ${release_root}/${name} ]] || ! cmp -s "${temporary}/${root}/bin/${name}" "${release_root}/${name}"; then
      printf 'existing versioned release differs from verified archive: %s\n' "${release_root}" >&2
      exit 7
    fi
    chmod 0755 "${release_root}/${name}"
  done
  if [[ ${has_mac_app} == true ]] && ! verify_mac_app "${release_root}"; then
    printf 'existing native Mac installation differs from the verified archive\n' >&2; exit 7
  fi
else
  release_stage=$(mktemp -d "${install_directory}/.farrow-release.next.XXXXXX")
  chmod 0700 "${release_stage}"
  for name in farrow farrow-hosts-helper; do
    install -m 0755 "${temporary}/${root}/bin/${name}" "${release_stage}/${name}"
    cmp -s "${temporary}/${root}/bin/${name}" "${release_stage}/${name}" || {
      printf 'staged entry point differs from verified archive: %s\n' "${name}" >&2
      exit 7
    }
  done
  if [[ ${has_mac_app} == true ]]; then
    /usr/bin/ditto "${mac_app}" "${release_stage}/Farrow Mac.app"
    install -m 0644 "${temporary}/${root}/MACOS.json" "${temporary}/${root}/MACOS.md" "${release_stage}/"
    verify_mac_app "${release_stage}" || { printf 'staged native Mac app differs from verified archive\n' >&2; exit 7; }
  fi
  mv "${release_stage}" "${release_root}"
  release_stage=
fi

link_stage=$(mktemp -d "${install_directory}/.farrow-links.next.XXXXXX")
chmod 0700 "${link_stage}"
for name in farrow farrow-hosts-helper; do
  if [[ ! -L ${install_directory}/${name} ]]; then
    ln -s ".farrow-current/${name}" "${link_stage}/${name}"
    mv "${link_stage}/${name}" "${install_directory}/${name}"
    created_entry_points+=("${install_directory}/${name}")
  fi
done
ln -s ".farrow-releases/${release_id}" "${link_stage}/current"
if [[ ${goos} == linux ]]; then
  mv -Tf "${link_stage}/current" "${current}"
else
  mv -fh "${link_stage}/current" "${current}"
fi
rmdir "${link_stage}"
link_stage=
created_entry_points=()
for name in farrow farrow-hosts-helper; do
  [[ -L ${install_directory}/${name} && $(readlink "${install_directory}/${name}") == ".farrow-current/${name}" && -x ${install_directory}/${name} ]] || {
    printf 'installed entry point is not executable through the current release: %s\n' "${install_directory}/${name}" >&2
    exit 7
  }
done
if [[ ! -L ${current} || $(readlink "${current}") != ".farrow-releases/${release_id}" ]]; then
  printf 'current release pointer does not name the verified release: %s\n' "${current}" >&2
  exit 7
fi
current_release=$(cd "${current}" && pwd -P)
if [[ ${current_release} != "${release_root}" ]]; then
  printf 'current release pointer resolves to the wrong release: %s\n' "${current_release}" >&2
  exit 7
fi
prune_retained_releases "${releases}" "${current_release}" "${install_keep}"
printf 'Installed Farrow %s in %s\n' "${version}" "${install_directory}"
# Being present in PATH does not mean this installation is the one selected.
# Compare file identity as paths such as ~/.local/bin may have equivalent aliases.
# Do not execute a shadowing binary just to ask it for its version.
selected_farrow=$(type -P farrow || true)
if [[ -z ${selected_farrow} || ! ${selected_farrow} -ef ${install_directory}/farrow ]]; then
  if [[ -n ${selected_farrow} ]]; then
    printf 'Your PATH currently selects: %s\n' "${selected_farrow}"
  fi
  printf "For this shell, run: export PATH=%q:\"\$PATH\"\n" "${install_directory}"
fi
printf 'Start your lab: %q up\n' "${install_directory}/farrow"
if [[ ${has_mac_app} == true ]]; then
  printf 'Start a macOS guest (Apple Silicon/macOS 27+): %q mac up\n' "${install_directory}/farrow"
fi
