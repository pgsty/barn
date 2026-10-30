# Shipping the native Mac feature

The native app is part of the ordinary `barn_VERSION_darwin_arm64.tar.gz`
starting with 0.9.2. There is one installer and one
Homebrew formula. Other platform archives keep their existing contents.

Formal releases require the signed, notarized app. Without the signing secrets
below, the tag workflow skips automatic release creation; use the local release
path with an installed Developer ID identity and stored notary profile.
With all six secrets configured, the native job must succeed: a failed build,
signature or notarization stops the release, and a partial set of secrets fails
the release at once. The CLI and the app speak a
versioned runner protocol; an app with an incompatible protocol is refused.

## Build and identity

`packaging/mac-release.py build VERSION COMMIT SOURCE_EPOCH OUTPUT.tar.gz` runs
on Apple Silicon/macOS 27 with Xcode 27. It builds and signs the Darwin CLI,
hosts helper, and app, verifies their signatures, stamps the release version and
commit, and exports `MACOS.md`, `MACOS.json`, `bin/barn`, `bin/barn-hosts-helper`
and `Barn.app`. `MACOS.json` records the exact files, modes, digests and signing
state. This payload contains no VM state, credentials, installer image or
installed service.

The application is `Barn.app`, with display name `Barn` and bundle identifier
`com.pgsty.barn`. Its internal executable remains `barn-mac-runner`, matching
the CLI's existing runner protocol. The CLI also recognizes the previous
`Barn Mac.app` name when upgrading an existing installation.

The existing GoReleaser job attaches that payload to the Darwin arm64 archive,
requiring an identical version and source commit. It regenerates the archive's
SPDX document and checksums, then runs the existing archive/package verifiers.
The native archive is checked again after export, so a stapled ticket must survive
the same tar representation used for distribution.

`BARN_MAC_PAYLOAD=/absolute/native.tar.gz make release-snapshot` exercises this
complete composition locally. The payload version must match the snapshot
version selected by `.goreleaser.yaml`: the next patch after the latest tag
with `-next` (for example, `0.9.1-next`).
Use `uncommitted` for a dirty checkout, or its full commit for clean source.
Snapshot builds support ad-hoc signing and are explicitly not notarized releases.

## Formal signing

To build and verify a local app with a certificate installed in your login
keychain, select an identity from `security find-identity -v -p codesigning`:

```sh
export BARN_CODESIGN_IDENTITY='Developer ID Application: your organization (TEAMID)'
BARN_VERSION=0.9.1-next BARN_COMMIT=uncommitted bash packaging/build-mac-app.sh
codesign --verify --deep --strict bin/Barn.app
codesign -d --verbose=4 bin/Barn.app
bin/Barn.app/Contents/MacOS/barn-mac-runner probe
```

This signs `Barn.app` with hardened runtime and a secure timestamp. The complete
payload builder applies the same signing policy to `barn` and
`barn-hosts-helper`, signing the helper before embedding its digest into the
CLI. Certificate signing alone does not notarize the payload; complete the
following steps before distributing it to users.

Formal builds require a Developer ID Application identity and a stored
`notarytool` profile:

```sh
export BARN_CODESIGN_IDENTITY='Developer ID Application: your organization (TEAMID)'
export BARN_NOTARY_PROFILE=barn-release
export BARN_MAC_REQUIRE_NOTARIZATION=1
export SOURCE_DATE_EPOCH="$(git show -s --format=%ct HEAD)"
export BARN_BUILD_DATE="$(date -u -r "$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ)"
VERSION=0.9.2
COMMIT="$(git rev-parse HEAD)"
python3 packaging/mac-release.py build "$VERSION" "$COMMIT" "$SOURCE_DATE_EPOCH" /absolute/native.tar.gz
BARN_MAC_PAYLOAD=/absolute/native.tar.gz make release-local VERSION="$VERSION" OUTPUT=/absolute/new-release-directory
```

Use a clean checkout tagged `v$VERSION`; a snapshot or `uncommitted` payload
cannot be used for the formal local release. The local release path runs the
same archive, package, SBOM, checksum, and helper-pair checks as CI. After
validating the complete output, push the source and tag, upload its files to a
draft GitHub Release, check the uploaded bytes, and publish the draft.
Pre-1.0 releases keep the prerelease flag. Update the Homebrew tap only after
the public assets are available.

Create the profile with `xcrun notarytool store-credentials` using your own Apple
credentials. Keep private keys and passwords outside the repository. The app
uses hardened runtime; Developer ID signatures include a
secure timestamp. The builder submits a ZIP containing `barn`,
`barn-hosts-helper`, and `Barn.app` together, waits for Apple acceptance,
staples the app and checks both the stapled ticket and Gatekeeper before
exporting it. Missing signing configuration never produces an unsigned app:
automatic release creation is skipped, and local releases require a verified
payload. The Linux release
job replaces the Darwin archive's unsigned CLI/helper with the exact signed
bytes from this payload before regenerating its SBOM and checksums.

This follows [Apple's notarization requirements](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution).

## CI and release secrets

`.github/workflows/mac.yml` builds on GitHub's documented
[`xcode-27` Apple Silicon image](https://github.com/actions/runner-images).
CI runs Mac-specific Go and native tests. Packaging snapshots build an ad-hoc
native payload, attach it to the normal archive and verify the complete result.
The tag release workflow builds and attaches the notarized native payload when
all six secrets below are configured. With none configured, it reports that
automatic release creation was skipped; the local release path remains usable.

Configure these repository secrets for that tag workflow:

| Secret | Contents |
|---|---|
| `MAC_CERTIFICATE_BASE64` | Base64-encoded Developer ID Application `.p12`, including its private key |
| `MAC_CERTIFICATE_PASSWORD` | Password protecting that `.p12` |
| `MAC_CODESIGN_IDENTITY` | Imported Developer ID identity name or certificate fingerprint |
| `MAC_NOTARY_KEY_BASE64` | Base64-encoded App Store Connect API `.p8` key |
| `MAC_NOTARY_KEY_ID` | API key identifier |
| `MAC_NOTARY_ISSUER` | API issuer identifier |

Signing material is imported into an ephemeral runner keychain and removed at
the end of the job. It is not used for pull requests or snapshot builds.

## Homebrew tap

Release assembly generates `barn.rb` from `packaging/homebrew/barn.rb.tmpl`,
using the versioned archive URLs and their checksums. Keep the template's
install and test logic aligned with `pgsty/homebrew-infra`'s `Formula/barn.rb`.
The tap updater accepts Barn's prereleases and refreshes all four platform
URLs and checksums after publication; the generated formula can also be used
for the release's tap update. Version suffixes need an explicit `version`
line because Homebrew may infer `64` from the archive's architecture suffix.
Plain versions omit the redundant line to pass Homebrew audit.
The user-facing macOS installation command is
`brew install pgsty/infra/barn`; it requires the versioned formula in the tap.
The formula installs the CLI into `bin` and its matching helper and native app
into `libexec`. Apple Silicon releases from 0.9.2 onward must include the app
and metadata identifying a notarized Developer ID build. The public 0.9.1
archive lacks the app and therefore supports Linux guests only.

## Acceptance before publication

Run `make check`, `make mac-native-test`, and a full snapshot with the native
payload. Test first install, repeated install and upgrade using the standard
installer in an isolated directory, then validate the Homebrew layout. On a
supported physical host, verify startup, SSH, desktop and normal shutdown with
the packaged app. Preserve existing instance IDs, machine IDs, SSH keys and host
pins across app replacement. Release signatures and notarization need their own
real acceptance; an ad-hoc snapshot is not evidence of either.

CI configuration and local tests do not prove a hosted CI run or public download.
After a release is approved and published, verify the public archive/checksums,
the real installer and Homebrew upgrade against those exact released bytes.
