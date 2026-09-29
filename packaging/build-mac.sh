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
bundle="$stage/barn-mac-darwin-arm64"
mkdir "$bundle"
# Reuse the existing Go build's version stamping and hosts-helper hash contract,
# then materialize regular files beside the native companions.
./packaging/build-dev.sh darwin arm64 "$stage/go"
cp -L "$stage/go/barn" "$stage/go/barn-hosts-helper" "$bundle/"
bash packaging/build-mac-app.sh "$bundle/Barn Mac.app"
install -m 0755 "$bundle/Barn Mac.app/Contents/MacOS/barn-mac-runner" "$bundle/"
codesign --force --sign "${BARN_CODESIGN_IDENTITY:--}" --entitlements native/mac-runner/entitlements.plist "$bundle/barn-mac-runner"
install -m 0644 LICENSE "$bundle/LICENSE"
install -m 0644 docs/mac.md "$bundle/USER_GUIDE.md"
install -m 0644 native/mac-runner/README.md "$bundle/RUNNER_PROTOCOL.md"
cat > "$bundle/README.md" <<'EOF'
# Barn Mac local bundle

Requires Apple Silicon and macOS 27 or later. Keep barn, barn-hosts-helper,
Barn Mac.app and barn-mac-runner together in one directory. The CLI prefers
the runner inside Barn Mac.app so the desktop has a native application
identity. This developer bundle is signed with BARN_CODESIGN_IDENTITY, or ad
hoc when none was supplied; it is not a notarized public release.

Run ./barn mac up to create and start mac1. The first run downloads macOS
from Apple after you confirm, or uses a restore image you pass with --ipsw.
./barn mac open shows the desktop; closing its window keeps the machine
running. ./barn mac stop shuts it down. Nothing runs as root: each machine's
private network lives inside its own runner process.

State is isolated under $BARN_HOME/mac (default ~/.barn/mac). The Linux
inventory is never read by these commands. ./barn mac ls --json works before
anything exists and creates nothing.

USER_GUIDE.md explains commands, persistent data and limitations.
RUNNER_PROTOCOL.md describes the native process and its JSON interface.
SHA256SUMS checks the included files: shasum -a 256 -c SHA256SUMS.
EOF
codesign --verify --strict "$bundle/barn-mac-runner"
codesign --verify --deep --strict "$bundle/Barn Mac.app"
"$bundle/barn-mac-runner" probe > "$stage/probe.json"
"$bundle/barn" mac --help > "$stage/mac-help.txt"
BARN_HOME="$stage/empty-home" "$bundle/barn" mac ls --json > "$stage/empty-slots.json"
(
  cd "$bundle"
  shasum -a 256 barn barn-hosts-helper barn-mac-runner LICENSE README.md USER_GUIDE.md RUNNER_PROTOCOL.md \
    "Barn Mac.app/Contents/Info.plist" "Barn Mac.app/Contents/MacOS/"* \
    "Barn Mac.app/Contents/Resources/Barn.icns" "Barn Mac.app/Contents/_CodeSignature/CodeResources" > SHA256SUMS
  shasum -a 256 -c SHA256SUMS
)
archive="$stage/barn-mac-darwin-arm64.tar.gz"
COPYFILE_DISABLE=1 tar -czf "$archive" -C "$stage" barn-mac-darwin-arm64
# Validate the extracted archive, not just its source directory.
mkdir "$stage/extracted"
tar -xzf "$archive" -C "$stage/extracted"
(
  cd "$stage/extracted/barn-mac-darwin-arm64"
  shasum -a 256 -c SHA256SUMS
  codesign --verify --deep --strict "Barn Mac.app"
  ./barn-mac-runner probe
  "./Barn Mac.app/Contents/MacOS/barn-mac-runner" probe
  ./barn mac --help >/dev/null
  BARN_HOME="$stage/extracted-empty-home" ./barn mac ls --json
  [[ ! -e $stage/extracted-empty-home ]]
)
for source in "$bundle"/*; do
  [[ ${source##*/} != "Barn Mac.app" ]] || continue
  destination="$repo/bin/mac/${source##*/}"
  if [[ -L $destination || ( -e $destination && ! -f $destination ) ]]; then
    echo "Refusing unsafe bundle destination: $destination" >&2; exit 2
  fi
done
for source in "$bundle"/*; do
  [[ ${source##*/} != "Barn Mac.app" ]] || continue
  destination="$repo/bin/mac/${source##*/}"
  if [[ -x $source ]]; then install -m 0755 "$source" "$destination"; else install -m 0644 "$source" "$destination"; fi
done
bash packaging/install-mac-app.sh "$bundle/Barn Mac.app" "$repo/bin/mac/Barn Mac.app"
install -m 0644 "$archive" "$repo/bin/barn-mac-darwin-arm64.tar.gz"
(
  cd "$repo/bin"
  shasum -a 256 barn-mac-darwin-arm64.tar.gz > barn-mac-darwin-arm64.tar.gz.sha256
)
printf 'Native Mac bundle: %s\nArchive: %s\n' "$repo/bin/mac" "$repo/bin/barn-mac-darwin-arm64.tar.gz"
