#!/usr/bin/env bash

# Strict SemVer 2.0.0 validation shared by release scripts. The v prefix is
# deliberately excluded because callers validate tags and versions separately.
barn_is_semver() {
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

barn_is_prerelease_semver() {
  local value=${1:-}
  barn_is_semver "${value}" && [[ ${value%%+*} == *-* ]]
}

# barn_is_stable_release is the single definition of "finished release": 1.0.0
# or later with no prerelease suffix. Everything under 1.0.0 is a pre-1.0
# developer release however the tag is spelled. The release channel recorded in
# release.json and the GitHub pre-release flag both follow this one rule, so the
# artifacts and the Release page can never disagree about what was shipped.
barn_is_stable_release() {
  local value=${1:-}
  barn_is_semver "${value}" || return 1
  barn_is_prerelease_semver "${value}" && return 1
  [[ ${value%%.*} != 0 ]]
}
