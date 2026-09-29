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
install -m 0755 "$bundle/Farrow Mac.app/Contents/MacOS/farrow-mac-runner" "$bundle/"
codesign --force --sign "${FARROW_CODESIGN_IDENTITY:--}" --entitlements native/mac-runner/entitlements.plist "$bundle/farrow-mac-runner"
install -m 0644 LICENSE "$bundle/LICENSE"
install -m 0644 docs/mac.md "$bundle/USER_GUIDE.md"
install -m 0644 docs/mac-implementation-log.md "$bundle/mac-implementation-log.md"
install -m 0644 native/mac-runner/README.md "$bundle/RUNNER_PROTOCOL.md"
cat > "$bundle/README.md" <<'EOF'
# Farrow Mac local bundle

Requires Apple Silicon and macOS 27 or later. Keep farrow, farrow-hosts-helper,
Farrow Mac.app and farrow-mac-runner together in one directory. The CLI prefers
the runner inside Farrow Mac.app so the desktop has a native application
identity. This developer bundle is signed with FARROW_CODESIGN_IDENTITY, or ad
hoc when none was supplied; it is not a notarized public release.

Run ./farrow mac up to create and start mac1. The first run downloads macOS
from Apple after you confirm, or uses a restore image you pass with --ipsw.
./farrow mac open shows the desktop; closing its window keeps the machine
running. ./farrow mac stop shuts it down. Nothing runs as root: each machine's
private network lives inside its own runner process.

State is isolated under $FARROW_HOME/mac (default ~/.farrow/mac). The Linux
inventory is never read by these commands. ./farrow mac ls --json works before
anything exists and creates nothing.

USER_GUIDE.md explains commands, persistent data and limitations.
mac-implementation-log.md records implementation and live acceptance evidence.
RUNNER_PROTOCOL.md describes the native process and its JSON interface.
SHA256SUMS checks the included files: shasum -a 256 -c SHA256SUMS.
EOF
codesign --verify --strict "$bundle/farrow-mac-runner"
codesign --verify --deep --strict "$bundle/Farrow Mac.app"
"$bundle/farrow-mac-runner" probe > "$stage/probe.json"
"$bundle/farrow" mac --help > "$stage/mac-help.txt"
FARROW_HOME="$stage/empty-home" "$bundle/farrow" mac ls --json > "$stage/empty-slots.json"
(
  cd "$bundle"
  shasum -a 256 farrow farrow-hosts-helper farrow-mac-runner LICENSE README.md USER_GUIDE.md mac-implementation-log.md RUNNER_PROTOCOL.md \
    "Farrow Mac.app/Contents/Info.plist" "Farrow Mac.app/Contents/MacOS/"* "Farrow Mac.app/Contents/_CodeSignature/CodeResources" > SHA256SUMS
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
# Earlier bundles shipped a root network helper; none of it is used anymore.
for obsolete in farrow-mac-network farrow-mac-network-install NETWORK_HELPER.md; do
  if [[ -f $repo/bin/mac/$obsolete && ! -L $repo/bin/mac/$obsolete ]]; then rm -f -- "$repo/bin/mac/$obsolete"; fi
done
bash packaging/install-mac-app.sh "$bundle/Farrow Mac.app" "$repo/bin/mac/Farrow Mac.app"
install -m 0644 "$archive" "$repo/bin/farrow-mac-darwin-arm64.tar.gz"
(
  cd "$repo/bin"
  shasum -a 256 farrow-mac-darwin-arm64.tar.gz > farrow-mac-darwin-arm64.tar.gz.sha256
)
printf 'Native Mac bundle: %s\nArchive: %s\n' "$repo/bin/mac" "$repo/bin/farrow-mac-darwin-arm64.tar.gz"
