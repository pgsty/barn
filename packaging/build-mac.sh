#!/bin/bash
# Local Apple Silicon/macOS 27 bundle; does not install services or publish.
set -euo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd -P)"
cd "$repo"
if [[ $(uname -s) != Darwin || $(uname -m) != arm64 || $(sw_vers -productVersion | cut -d. -f1) -lt 27 ]]; then
  echo "The native Mac bundle requires Apple Silicon, macOS 27+, and Xcode 27+." >&2
  exit 2
fi
if [[ $# != 0 ]]; then echo "usage: $0" >&2; exit 2; fi
for tool in go xcrun codesign tar shasum ditto plutil; do
  command -v "$tool" >/dev/null || { echo "Required build tool is missing: $tool" >&2; exit 2; }
done
for directory in "$repo/bin" "$repo/bin/mac"; do
  if [[ -L $directory || ( -e $directory && ! -d $directory ) ]]; then
    echo "Refusing unsafe build output: $directory" >&2; exit 2
  fi
  mkdir -p "$directory"
done
stage="$(mktemp -d "$repo/bin/.mac-build.XXXXXXXX")"
cleanup() { case "$stage" in "$repo"/bin/.mac-build.*) rm -rf -- "$stage" ;; esac; }
trap cleanup EXIT
bundle="$stage/farrow-mac-darwin-arm64"
mkdir "$bundle"
# Reuse the existing Go build's version stamping and hosts-helper hash contract,
# then materialize regular files beside the native companions.
./packaging/build-dev.sh darwin arm64 "$stage/go"
cp -L "$stage/go/farrow" "$stage/go/farrow-hosts-helper" "$bundle/"
bash packaging/build-mac-app.sh "$bundle/Farrow Mac.app"
install -m 0755 "$bundle/Farrow Mac.app/Contents/MacOS/farrow-mac-runner" "$bundle/Farrow Mac.app/Contents/MacOS/farrow-mac-network" "$bundle/"
install -m 0755 native/mac-network/install.sh "$bundle/farrow-mac-network-install"
codesign --force --sign "${FARROW_CODESIGN_IDENTITY:--}" --entitlements native/mac-runner/entitlements.plist "$bundle/farrow-mac-runner"
install -m 0644 LICENSE "$bundle/LICENSE"
install -m 0644 docs/mac.md "$bundle/USER_GUIDE.md"
install -m 0644 docs/mac-implementation-log.md "$bundle/mac-implementation-log.md"
install -m 0644 native/mac-runner/README.md "$bundle/RUNNER_PROTOCOL.md"
install -m 0644 native/mac-network/README.md "$bundle/NETWORK_HELPER.md"
cat > "$bundle/README.md" <<'EOF'
# Farrow Mac local bundle

Requires Apple Silicon and macOS 27 or later. Keep farrow, farrow-hosts-helper,
Farrow Mac.app and the compatible standalone native tools together in one
directory. The CLI prefers the runner inside Farrow Mac.app so the desktop has
a native application identity. The companions in this local developer bundle are signed
with FARROW_CODESIGN_IDENTITY, or ad hoc when it was not supplied. This local
bundle is not a notarized public release.

The app bundle retains its verified signed Keychain owner with the private Mac
state, so subsequent ad-hoc rebuilds can still access existing GUI credentials.
Older instances can adopt this behavior once with `mac credentials migrate`;
it automatically finds the previous app retained by the local build. Use --from
if the original app lives elsewhere. Keep the credentials/owners directory with
your state. Custom standalone runners require explicit migration after a signing
identity change; read USER_GUIDE.md for recovery details.

Run ./farrow mac --help, then ./farrow mac setup. Setup downloads the pinned Apple
restore image and prepares a private network after the required administrator
authorization. ./farrow mac up starts mac1; ./farrow mac up mac2 starts mac2.
./farrow mac open mac1 displays its desktop. Closing that window keeps the VM
running. Use ./farrow mac stop mac1 to shut it down normally.

State is isolated under $FARROW_HOME/mac (default ~/.farrow/mac). Existing Linux
inventory is not read by these commands. Empty slots are visible without setup:
./farrow mac ls --json. No services are installed by extracting this archive.

USER_GUIDE.md explains commands, persistent data and operational limitations.
mac-implementation-log.md records implementation and live acceptance evidence.
RUNNER_PROTOCOL.md describes the native process and JSON interface.
NETWORK_HELPER.md describes privilege boundaries and helper installation.
SHA256SUMS checks the included files. Check with shasum -a 256 -c SHA256SUMS.
EOF
for binary in farrow-mac-runner farrow-mac-network; do codesign --verify --strict "$bundle/$binary"; done
codesign --verify --deep --strict "$bundle/Farrow Mac.app"
"$bundle/farrow-mac-runner" probe > "$stage/probe.json"
"$bundle/farrow" mac --help > "$stage/mac-help.txt"
FARROW_HOME="$stage/empty-home" "$bundle/farrow" mac ls --json > "$stage/empty-slots.json"
(
  cd "$bundle"
  shasum -a 256 farrow farrow-hosts-helper farrow-mac-runner farrow-mac-network farrow-mac-network-install LICENSE README.md USER_GUIDE.md mac-implementation-log.md RUNNER_PROTOCOL.md NETWORK_HELPER.md \
    "Farrow Mac.app/Contents/Info.plist" "Farrow Mac.app/Contents/MacOS/"* "Farrow Mac.app/Contents/Resources/"* "Farrow Mac.app/Contents/_CodeSignature/CodeResources" > SHA256SUMS
  shasum -a 256 -c SHA256SUMS
)
archive="$stage/farrow-mac-darwin-arm64.tar.gz"
COPYFILE_DISABLE=1 tar -czf "$archive" -C "$stage" farrow-mac-darwin-arm64
# Validate the extracted archive, not just its source directory.
mkdir "$stage/extracted"
tar -xzf "$archive" -C "$stage/extracted"
(
  cd "$stage/extracted/farrow-mac-darwin-arm64"
  shasum -a 256 -c SHA256SUMS
  codesign --verify --deep --strict "Farrow Mac.app"
  ./farrow-mac-runner probe
  "./Farrow Mac.app/Contents/MacOS/farrow-mac-runner" probe
  ./farrow mac --help >/dev/null
  FARROW_HOME="$stage/extracted-empty-home" ./farrow mac ls --json
  [[ ! -e $stage/extracted-empty-home ]]
)
for source in "$bundle"/*; do
  [[ ${source##*/} != "Farrow Mac.app" ]] || continue
  destination="$repo/bin/mac/${source##*/}"
  if [[ -L $destination || ( -e $destination && ! -f $destination ) ]]; then
    echo "Refusing unsafe bundle destination: $destination" >&2; exit 2
  fi
done
for source in "$bundle"/*; do
  [[ ${source##*/} != "Farrow Mac.app" ]] || continue
  destination="$repo/bin/mac/${source##*/}"
  if [[ -x $source ]]; then install -m 0755 "$source" "$destination"; else install -m 0644 "$source" "$destination"; fi
done
bash packaging/install-mac-app.sh "$bundle/Farrow Mac.app" "$repo/bin/mac/Farrow Mac.app"
install -m 0644 "$archive" "$repo/bin/farrow-mac-darwin-arm64.tar.gz"
(
  cd "$repo/bin"
  shasum -a 256 farrow-mac-darwin-arm64.tar.gz > farrow-mac-darwin-arm64.tar.gz.sha256
)
printf 'Native Mac bundle: %s\nArchive: %s\n' "$repo/bin/mac" "$repo/bin/farrow-mac-darwin-arm64.tar.gz"
