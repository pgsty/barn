#!/bin/bash
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
out="${1:-$here/build}"
mkdir -p "$out"
build_id=$(cat "$here/main.m" "$here/source.c" "$here/source.h" "$here/build.sh" "$here/install.sh" | shasum -a 256 | awk '{print $1}')
xcrun clang -arch arm64 -mmacosx-version-min=27.0 -Wall -Wextra -Werror \
  -DFARROW_NETWORK_BUILD_ID=\""$build_id"\" \
  -fobjc-arc -fblocks "$here/main.m" "$here/source.c" -framework Foundation -framework vmnet \
  -o "$out/farrow-mac-network"
codesign --force --sign "${FARROW_CODESIGN_IDENTITY:--}" "$out/farrow-mac-network"
# No com.apple.vm.networking entitlement is embedded or claimed. The daemon
# runs as root; the ordinary runner only needs its virtualization entitlement.
