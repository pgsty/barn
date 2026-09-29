#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
output="${1:-../../bin/farrow-mac-runner}"
mkdir -p "$(dirname "$output")"
temporary="$(mktemp -d "${TMPDIR:-/tmp}/farrow-mac-runner-build.XXXXXX")"
trap 'rm -rf "$temporary"' EXIT
xcrun clang -O2 -Wall -Wextra -Werror -mmacosx-version-min=27.0 -c Bridge.c -o "$temporary/Bridge.o"
xcrun swiftc -O -swift-version 5 -parse-as-library -target arm64-apple-macosx27.0 \
  -import-objc-header Bridge.h Sources/*.swift "$temporary/Bridge.o" \
  -framework Virtualization -framework DiskImageKit -framework vmnet -framework AppKit -o "$output"
codesign --force --sign "${FARROW_CODESIGN_IDENTITY:--}" --entitlements entitlements.plist "$output"
codesign --verify --strict "$output"
