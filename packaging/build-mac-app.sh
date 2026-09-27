#!/bin/bash
# Build the GUI-capable native app. No service installation or VM operations.
set -euo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd -P)"
if [[ $# -gt 1 ]]; then echo "usage: $0 [OUTPUT_APP]" >&2; exit 2; fi
output=${1:-"$repo/bin/Farrow Mac.app"}
identity=${FARROW_CODESIGN_IDENTITY:--}
if [[ ${FARROW_MAC_REQUIRE_NOTARIZATION:-0} == 1 && ( $identity == - || -z ${FARROW_NOTARY_PROFILE:-} ) ]]; then
  echo "A release app requires FARROW_CODESIGN_IDENTITY and FARROW_NOTARY_PROFILE; use a Developer ID certificate and a stored notarytool profile." >&2; exit 2
fi
if [[ $(uname -s) != Darwin || $(uname -m) != arm64 || $(sw_vers -productVersion | cut -d. -f1) -lt 27 ]]; then
  echo "Farrow Mac.app requires Apple Silicon and macOS 27+." >&2; exit 2
fi
[[ ! -L $repo/bin && ( ! -e $repo/bin || -d $repo/bin ) ]] || { echo "Unsafe bin directory" >&2; exit 2; }
mkdir -p "$repo/bin"
[[ $(cd "$repo/bin" && pwd -P) == "$repo/bin" ]] || { echo "Unsafe bin directory ancestry" >&2; exit 2; }
stage="$(mktemp -d "$repo/bin/.mac-native-build.XXXXXXXX")"
cleanup() { case $stage in "$repo"/bin/.mac-native-build.*) rm -rf -- "$stage" ;; esac; }
trap cleanup EXIT
app="$stage/Farrow Mac.app"
macos="$app/Contents/MacOS"
native="$stage/native"
mkdir -p "$macos" "$native" "$app/Contents/Resources"
install -m 0644 "$repo/native/mac-runner/Info.plist" "$app/Contents/Info.plist"
version=${FARROW_VERSION:-dev}
[[ $version =~ ^[0-9A-Za-z][0-9A-Za-z._+-]*$ ]] || { echo "Invalid FARROW_VERSION" >&2; exit 2; }
short_version=0.0.0
if [[ $version =~ ^([0-9]+\.[0-9]+\.[0-9]+)([-+].*)?$ ]]; then short_version=${BASH_REMATCH[1]}; fi
commit=${FARROW_COMMIT:-uncommitted}
[[ $commit == uncommitted || $commit =~ ^[0-9a-f]{40}$ ]] || { echo "Invalid FARROW_COMMIT" >&2; exit 2; }
plutil -replace CFBundleShortVersionString -string "$short_version" "$app/Contents/Info.plist"
plutil -replace CFBundleVersion -string "$short_version" "$app/Contents/Info.plist"
plutil -insert FarrowVersion -string "$version" "$app/Contents/Info.plist"
plutil -insert FarrowCommit -string "$commit" "$app/Contents/Info.plist"
plutil -lint "$app/Contents/Info.plist"
bash "$repo/native/mac-runner/build.sh" "$native/farrow-mac-runner"
bash "$repo/native/mac-network/build.sh" "$native"
install -m 0755 "$native/farrow-mac-runner" "$native/farrow-mac-network" "$macos/"
install -m 0755 "$repo/native/mac-network/install.sh" "$app/Contents/Resources/farrow-mac-network-install"
# Sign inner Mach-O files first, then seal the complete app with the runner's
# virtualization entitlement. The root-only network helper has no vmnet grant.
codesign --verify --strict "$native/farrow-mac-runner"
codesign --verify --strict "$native/farrow-mac-network"
sign_args=(--force --sign "$identity" --options runtime)
if [[ $identity != - ]]; then sign_args+=(--timestamp); fi
codesign "${sign_args[@]}" "$macos/farrow-mac-network"
codesign "${sign_args[@]}" --entitlements "$repo/native/mac-runner/entitlements.plist" "$app"
codesign --verify --deep --strict "$app"
if [[ -n ${FARROW_NOTARY_PROFILE:-} ]]; then
  [[ $identity != - ]] || { echo "Notarization requires a Developer ID identity" >&2; exit 2; }
  ditto -c -k --keepParent "$app" "$stage/notarize.zip"
  xcrun notarytool submit "$stage/notarize.zip" --keychain-profile "$FARROW_NOTARY_PROFILE" --wait --timeout 20m --output-format json >"$stage/notary.json"
  [[ $(plutil -extract status raw -o - "$stage/notary.json") == Accepted ]] || { echo "Apple did not accept this app for notarization" >&2; exit 1; }
  xcrun stapler staple "$app"
  xcrun stapler validate "$app"
  spctl --assess --type execute "$app"
fi
bash "$repo/packaging/install-mac-app.sh" "$app" "$output"
