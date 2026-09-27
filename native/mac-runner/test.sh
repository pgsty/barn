#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
temporary="$(mktemp -d "${TMPDIR:-/tmp}/farrow-mac-runner-test.XXXXXX")"
trap 'rm -rf "$temporary"' EXIT
xcrun clang -O2 -Wall -Wextra -Werror -mmacosx-version-min=27.0 -c Bridge.c -o "$temporary/Bridge.o"
xcrun clang -O2 -Wall -Wextra -Werror -mmacosx-version-min=27.0 Tests/BridgeTest.c "$temporary/Bridge.o" -o "$temporary/bridge-test"
"$temporary/bridge-test"
xcrun swiftc -swift-version 5 -parse-as-library -target arm64-apple-macosx27.0 \
  -import-objc-header Bridge.h Sources/Support.swift Sources/Images.swift Tests/ImagesTest.swift "$temporary/Bridge.o" \
  -framework Virtualization -framework DiskImageKit -framework Security -o "$temporary/images-test"
codesign --force --sign - --entitlements entitlements.plist "$temporary/images-test"
"$temporary/images-test"
xcrun swiftc -swift-version 5 -parse-as-library -target arm64-apple-macosx27.0 \
  Sources/ExitPrompt.swift Sources/RuntimeMenu.swift Tests/ExitPromptTest.swift -framework AppKit -o "$temporary/exit-prompt-test"
"$temporary/exit-prompt-test"
