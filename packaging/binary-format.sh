#!/usr/bin/env bash

# Shared binary-format assertions for the release verifiers.
#
# `file` phrases the same binary differently on macOS and on GNU systems — the
# Mach-O word order alone differs — so matching one ordered sentence silently
# pins a verifier to whichever host it was written on. Require the identifying
# tokens instead, in any order, and say exactly what was seen when one is
# missing.

barn_binary_format_tokens() {
  case $1 in
    darwin/amd64) printf 'Mach-O 64-bit x86_64' ;;
    darwin/arm64) printf 'Mach-O 64-bit arm64' ;;
    linux/amd64) printf 'ELF 64-bit x86-64' ;;
    linux/arm64) printf 'ELF 64-bit aarch64' ;;
    *) printf 'unsupported binary target: %s\n' "$1" >&2; return 2 ;;
  esac
}

barn_verify_binary_format() {
  local binary=$1 target=$2 described tokens token
  # Resolve the expectation first: an unsupported target must fail the caller,
  # not disappear into a subshell that the loop then reads as "nothing to check".
  tokens=$(barn_binary_format_tokens "${target}") || return 1
  [[ -f ${binary} ]] || { printf 'binary is missing: %s\n' "${binary}" >&2; return 1; }
  described=$(file -b "${binary}")
  for token in ${tokens}; do
    [[ ${described} == *"${token}"* ]] || {
      printf 'binary %s is not %s: file reports %q (missing %q)\n' "${binary}" "${target}" "${described}" "${token}" >&2
      return 1
    }
  done
}
