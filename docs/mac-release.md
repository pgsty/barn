# Shipping the native Mac feature

The native app is part of the ordinary `farrow_VERSION_darwin_arm64.tar.gz`
when a signed, notarized build is available. There is one installer and one
Homebrew formula. Other platform archives keep their existing contents.

The app is optional. Without the signing secrets below, the tag workflow skips
the native job and ships every CLI archive and package without the Mac app;
`farrow mac` then explains that its component is not installed. With all six
secrets configured, the native job must succeed: a failed build, signature or
notarization stops the release instead of leaving the app out, and a partial
set of secrets fails the release at once. The CLI and the app speak a
versioned runner protocol, so an app from another build is refused rather than
misused.

## Build and identity

`packaging/mac-release.py build VERSION COMMIT SOURCE_EPOCH OUTPUT.tar.gz` runs
on Apple Silicon/macOS 27 with Xcode 27. It builds the app, verifies its signature,
stamps the release version and commit, and exports `MACOS.md`, `MACOS.json` and the
app. `MACOS.json` records the exact files, modes, digests and signing state. This
payload contains no VM state, credentials, installer image or installed service.

The existing GoReleaser job attaches that payload to the Darwin arm64 archive,
requiring an identical version and source commit. It regenerates the archive's
SPDX document and checksums, then runs the existing archive/package verifiers.
The native archive is checked again after export, so a stapled ticket must survive
the same tar representation used for distribution.

`FARROW_MAC_PAYLOAD=/absolute/native.tar.gz make release-snapshot` exercises this
complete composition locally. The payload version must match the snapshot
version selected by `.goreleaser.yaml`: the next patch after the latest tag
with `-next` (after `v0.8.0`, `0.8.1-next`).
Use `uncommitted` for a dirty checkout, or its full commit for clean source.
Snapshot builds support ad-hoc signing and are explicitly not notarized releases.

## Formal signing

Formal builds require a Developer ID Application identity and a stored
`notarytool` profile:

```sh
export FARROW_CODESIGN_IDENTITY='Developer ID Application: your organization (TEAMID)'
export FARROW_NOTARY_PROFILE=farrow-release
export FARROW_MAC_REQUIRE_NOTARIZATION=1
python3 packaging/mac-release.py build "$VERSION" "$COMMIT" "$SOURCE_DATE_EPOCH" /absolute/native.tar.gz
FARROW_MAC_PAYLOAD=/absolute/native.tar.gz make release-local VERSION="$VERSION"
```

Create the profile with `xcrun notarytool store-credentials` using your own Apple
credentials. Keep private keys and passwords outside the repository. The app
uses hardened runtime; Developer ID signatures include a
secure timestamp. The builder waits for Apple acceptance, staples the app and
checks both the stapled ticket and Gatekeeper before exporting it. Missing
signing configuration never produces an unsigned app: the tag workflow skips
the native job and ships the CLI alone. The ordinary
Go CLI and hosts-helper retain the existing GoReleaser delivery contract; the
notarization claim covers the native app and its enclosed executables.

This follows [Apple's notarization requirements](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution).

## CI and release secrets

`.github/workflows/mac.yml` builds on GitHub's documented
[`xcode-27` Apple Silicon image](https://github.com/actions/runner-images).
CI runs Mac-specific Go and native tests. Packaging snapshots build an ad-hoc
native payload, attach it to the normal archive and verify the complete result.
The tag release workflow builds and attaches the notarized native payload when
all six secrets below are configured, and creates the draft release without it,
with a notice, when none are.

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
