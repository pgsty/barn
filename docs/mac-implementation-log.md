# Farrow Mac implementation and acceptance log

Product contract: `MACOS-VM-PRODUCT-20260921.md`. The older research document is retained as background; its shared Linux inventory proposal is superseded.

## Accepted local result

The actual development bundle passed all 12 automated dual-guest stages on 2026-09-26, followed by independent native-desktop and cache-pruning acceptance. Both guests run macOS 27.0 / 26A428. This establishes local functionality on the tested host; it is not a public release, notarization or an upgrade test with a Developer ID identity.

Evidence paths below are relative to `/Users/vonng/.codex/visualizations/2026/09/26/01a0dd9d-b1fd-7dc0-be29-485ed130c965/`. The combined closeout is `acceptance/closeout.json`; individual failed attempts remain available with their original status.

| Acceptance | Result and evidence |
|---|---|
| Official image and unbooted base | Complete 26,626,436,228-byte download, pinned SHA-256, Apple restore validation, CLI restore and unchanged repeat setup; `acceptance/download-verified.json`, `acceptance/setup-cli-result.json`, `acceptance/setup-repeat.json`. |
| Two independent guests | Both ready simultaneously, distinct Apple UUIDs, machine IDs, auxiliary files, SSH client/host keys and disk contents; `acceptance/live-package-run/summary.json`, stages 2–5. |
| Accounts | Ordinary `vonng` administrator, passwordless sudo, key-authenticated SSH, effective password/keyboard-interactive/root SSH disabled, desktop auto-login and no Apple Account preference entry; stage 4. |
| Network | Stable 10.10.20.10/.11 after reconnection, host ↔ guest and guest ↔ guest ping, DNS, public HTTPS 200; stages 3, 6–7 and repeated lifecycle stages. |
| SSH command semantics | Separate stdout/stderr, remote exit 37, empty arguments, quotes, shell metacharacters, newlines and Chinese arguments preserved; stage 8. |
| Lifecycle | Both guests gracefully stop/start with identity and data preserved; reset rotates mac1 identities and removes its sentinel while retaining its address and mac2; destroy removes only mac1; stages 9–12. |
| Desktop and detached execution | Native Finder desktop visually observed. Closing mac2's window changed `window_visible` to false while retaining running PID 41583, the same instance UUID and working SSH; `acceptance/gui-package-mac2.json`. |
| Cache reuse and pruning | Default/referenced base retained; installer removed; fresh mac1 reaches SSH readiness in 23.825 seconds with no installer present. All base hashes, inodes and mtimes unchanged; `acceptance/cache-final/summary.json`. |
| Host-wide VM quota | Extra restore with two macOS guests active fails with `virtual_machine_limit` in 0.99 seconds and preserves guests; `acceptance/quota-restore.json`. |
| Regression and packaging | Full Go suite, race checks on the Mac/CLI packages, four OS/architecture builds, go vet, native tests and extracted-bundle checksum/signature/probe checks passed; `acceptance/final-build/`, `acceptance/go-race-current.log`. |

The 12-stage final package run took 140.305 seconds. It started with initialized guests; the earlier first boots took about 42 and 46 seconds. The final cache test independently created and provisioned a fresh guest. Raw harness output deliberately retains `automated_passed_gui_pending`; the later GUI evidence closes that separate gate in the combined report.

Measured storage is not exclusive APFS ownership: the installer contains 24.80 GiB of file data, the immutable base reports 26.69 GiB allocated, and both virtual disks have 100 GiB capacity. After cache-based creation, mac1's overlay reported 0.41 GiB allocated and the older mac2 overlay 7.47 GiB; background macOS initialization continues to change these values. The external original IPSW was preserved when the private installer cache was pruned.

## 2026-09-27 release readiness review

The normal Darwin arm64 archive now includes the native app, guide and an exact
file manifest. The standard installer verifies the app before switching its
current-release pointer; CLI and app share one retained release directory.
Homebrew installs the app in `libexec`, which both runner discovery and desktop
shutdown now resolve. The native payload must match the Go CLI's version and
source commit; its archive, SPDX document and checksums are verified together.
The app reports its real build version, uses hardened runtime, and the formal
release workflow requires Developer ID signing, secure timestamps, Apple
notarization and a stapled ticket. See [release setup](mac-release.md).

Doctor now checks SSH recovery files even for stopped guests and uses the same
runtime and lock evidence as status. Missing keys, host pins or stopped-state
proof no longer produce a healthy diagnosis. Opening the helper app directly
now explains the terminal commands instead of silently exiting.

The full `make check` initially exposed staticcheck and unchecked-error failures
that the earlier Go-only validation had missed. These were fixed and the full
gate passed, as did native component tests and real disposable Keychain tests
across two app signatures. The complete four-platform GoReleaser snapshot with
the Mac payload passed archive, package, SPDX, helper pairing and checksum
verification. Standard-installer first/repeated installs and upgrade from the
previously downloaded public 0.8.0 archive passed in isolated directories. A
tampered app with a recomputed test archive checksum was rejected before changing
the existing installation. Local download fixtures do not establish public
network delivery.

The packaged Homebrew layout reached SSH readiness and displayed the native
mac1 desktop without a runner override. Computer-use menu actions repeatedly
timed out (`-10005`), so this attempt does not establish GUI shutdown from that
layout. The VM was stopped through the normal packaged CLI and both slots'
identities, SSH material and credentials were preserved. The earlier adjacent
CLI layout has separate successful GUI shutdown evidence in the remaining-UX
goal section.

Evidence: `release-readiness-20260927/ACCEPTANCE.md`, `make-check-final.log`,
`mac-native-tests.log`, `snapshot-build.log`, `installer-acceptance.json`,
`keychain-native.log`, `homebrew-live-acceptance.json` and
`homebrew-cli-preservation.json`. Actual Developer ID signing/notarization,
hosted CI and public installation remain separate pre-publication gates. No
signing identity was available on this host, and source remains uncommitted.

## 2026-09-27 credential recovery review

A further review found that credential migration removed its encrypted backup before retaining the new signed app and publishing its owner record. A later filesystem failure could therefore leave the record pointing to the retired owner with no recovery backup. Migration now verifies retained copies before changing Keychain, publishes both the new owner and the backup's original owner before deleting the backup, and resumes final metadata cleanup after interruption. App-to-standalone migration also clears the old app reference; deleting a partially published credential removes its recovery item as well.

Fault injection covers failed publication, publication that completed before reporting an error, cancellation after publication and failed backup deletion. Native tests use disposable Keychain UUIDs with two differently signed app builds. They passed failed-write recovery, automatic retry after publication, cleanup after backup removal, deletion during interrupted publication, bidirectional app/standalone migration and blocked owner-retention preflight. Normal passwords and deletion permissions were checked. The native test's first standalone fixture was invalid because it copied a bundle-signed executable without re-signing it; using the same standalone signing step as packaging resolved that fixture failure. No original VM credential was changed by these tests.

Evidence is in `post-closeout-review-20260927/`: `credential-native-final.log`, the Go test/race/vet results, cross-build results and the rebuilt-package checks. Previous acceptance records remain separate.

## 2026-09-27 remaining UX goal

The second live pass found and fixed two gaps that fixtures had missed. Apple's online catalog reports hardware hash `18d57e…`, whereas the identical local 26A428 IPSW and prepared base use `976e66…`. Online update now validates the saved hardware model with Apple before reusing the same-build base. The real unqualified `image update` completed in 1.273 seconds without a new download or restore. The GUI's original native shutdown request left the guest running; the packaged Request Shutdown button now delegates to the normal CLI shutdown flow. A real click stopped mac1 in about five seconds and released its runner lock.

Same-slot stop now cancels only the active readiness wait through a private nonce-bound socket, retaining lifecycle write exclusion. Live `up mac2` exited with cancellation in 0.038 seconds after stop arrived; normal shutdown completed in 13.777 seconds. Shutdown tolerates the brief period before sshd starts during boot, while retaining host-key checks.

The network helper was upgraded, uninstalled and reinstalled successfully against the isolated canonical home. The new daemon reports the expected build ID; both initialized guests subsequently reached SSH readiness. Credential migration discovered the previous build automatically for both existing instances and preserved their passwords. The app now retains its signed credential owner privately with the installation. Actual Keychain set/read/delete across two different app signatures passed without another migration, with a separate disposable UUID.

First setup in a second isolated home was cancelled with Ctrl-C while the system administrator prompt was pending: exit 130, no installed helper, and no base or guest. Retrying with 10.10.31.0/24 instead of 10.10.30.0/24 then completed successfully after system authentication, preserving both installation and network UUIDs. The actual restore took 347.190 seconds; the total command time includes the wait for user authentication. The restored base was ready and unbooted, both slots remained empty, and repeated setup reused it. The temporary helper, launchd service, configuration and socket were subsequently removed through the public uninstall command. The canonical Mac network, Linux socket_vmnet and Docker backend remained available. Clicking the system Cancel button has separate -128 classification tests; the live interruption used Ctrl-C.

Evidence is under `remaining-20260927/`: `online-update-fixed.*`, `network-*.result.json`, `credential-retention.json`, `credential-after-rebuild.json`, `credential-native-test.log`, `gui-fixed-stopped.json`, `same-slot-stop.result.json`, and `setup-cancel.*` / `setup-retry.*`. `setup-retry-verified.json`, `retry-network-removed.stdout` and `final-preservation.json` close the installation, cleanup and preservation checks. The earlier failures remain recorded. Full Go, Mac/CLI race, native component tests, four cross-platform builds and go vet passed. The final archive was rebuilt with this documentation and verified after extraction.

## 2026-09-27 UX follow-up

The Mac path now shares the main CLI progress renderer and phase timings, uses human-readable resource flags, reports creation conflicts with executable recovery steps, and adds stopped-instance configuration, restart, logs, network maintenance and credential migration commands. Runtime checks distinguish an unavailable runner from a proven stop and verify SSH live. Image preparation no longer blocks an unrelated slot's shutdown; compatible same-version bases are reused before download.

The updated local bundle passed another dual-guest run: configured resources matched guest `sysctl`, both slots reached SSH/sudo readiness, restart preserved identity, and `stop mac2` completed in 8.619 seconds while the global image lock remained held and mac1 stayed ready. Same-base `image update --ipsw` completed in 0.616 seconds without a download, restore, or compatible-helper replacement. A too-short status probe failed during the first attempt; the corrected 5-second probes run concurrently and returned both guests ready in 0.591 seconds in the accepted run.

The public credential migration command succeeded for both actual Keychain items. GUI inspection verified the new host menu, three-choice quit dialog, cancel, and continued SSH after choosing background operation. Missing SSH material was tested live: the error provided `start --no-wait` then `open`, the VM launched for recovery, and restoring the original key allowed normal readiness. All original instance UUIDs, machine-ID hashes, private-key hashes and host pins were preserved. Both initialized slots were stopped afterward, with mac1 restored to 4 CPUs / 8 GiB and mac2 unchanged.

Evidence: `ux-20260927/summary.json`, `ux-20260927/UX-ACCEPTANCE.md`, `ux-20260927/gui-exit.json`. The failed initial status attempt remains recorded separately. Full Go tests, Mac/CLI race tests, native component tests and four OS/architecture builds were rerun for this work. At this earlier checkpoint, successful privileged helper upgrade/uninstall/reinstall had only isolated installer simulations; live acceptance verified refusal while a guest was active. The later remaining-UX goal above closes the real upgrade/uninstall/reinstall checks.

## 2026-09-26 baseline

- Repository HEAD `b91ec37`; only the two pre-existing, untracked design documents. No AGENTS.md found in the repository or ancestors.
- Host: Apple Silicon, macOS 27.0 / 26A428, 128 GiB RAM, 18 logical CPUs; about 1.1 TiB free on the data volume.
- Existing Linux socket_vmnet: 10.10.10.0/24, PID 618. Existing VZ service corresponds to Docker. Neither is modified by this work.
- `sudo -n true` requires administrator authentication. Network entitlement possession is not inferred from a signing declaration.
- Official Apple IPSW HEAD: 26,626,436,228 bytes; SHA-256 response metadata `2a5d3c695d501022b7fad9adaffcf2627bcb867d993fb5662dcd41bac99a2836`; ETag `4371cc79b2c458d57073c9662334eef3-3174`.
- Isolated evidence/data location: `/Users/vonng/.codex/visualizations/2026/09/26/01a0dd9d-b1fd-7dc0-be29-485ed130c965/`.

## Implemented and automated checks

- Independent Go command/state/download/SSH layer; ordinary-user Swift VM/desktop runner; dedicated privileged vmnet datagram helper; local development bundle and documentation.
- Go tests include Linux purge/destroy preservation, download interruption and HTTP Range validation, state/path validation, reference-safe pruning, CLI argument/exit handling and interrupted-clone recovery. The final repository test suite passed after the live-test fixes (11.111 seconds).
- Darwin/Linux × arm64/amd64 Go builds passed. Native compilation and component tests passed; development packaging uses the virtualization entitlement on the runner and does not claim a networking entitlement. This is not a notarized public release.

## Development findings and resolved failures

- The complete official 26,626,436,228-byte IPSW downloaded in approximately 880 seconds. Its SHA-256 matches the value obtained from Apple's HTTPS metadata. Apple restore-image validation confirms 27.0 / 26A428 and host compatibility. Evidence: `acceptance/download-verified.json`, `acceptance/ipsw-metadata.json`.
- The first full restore exposed an installer lifecycle issue: installation can finish while the VM still reports running. Publishing was correctly refused. The runner now waits for the installer VM to stop and explicitly stops it if necessary before publishing a base.
- The second full restore succeeded in 193.49 seconds. The installer VM stopped naturally within three seconds of completion; the base was published read-only with `first_boot=false`, without a guest-start call. Evidence: `acceptance/restore-probe-2.json`, `acceptance/restore-probe-2.log`, `acceptance/restore-probe-2/base.json`. This direct-runner result does not yet prove the complete CLI setup path.
- The complete CLI `mac setup --ipsw` then passed in 212.59 seconds, including local import and a 189.50-second restore. It left both slots empty and selected a ready, unbooted base. Repeated `mac setup` took 0.042 seconds with installer/base file sizes, inode identities and modification times unchanged. Evidence: `acceptance/setup-cli-result.json`, `acceptance/setup-cli.json`, `acceptance/setup-repeat.json`.
- Administrator-authorized vmnet creation, interface start/stop and daemon installation succeeded. The real runtime requires the gateway address as the IPv4 subnet API argument; passing the canonical network address was accepted at configuration time but failed during interface start. Persisted Farrow configuration still uses the canonical /24 network.
- Reattachment initially exposed DHCP reservation instability (.2/.3 instead of .10/.11). The helper now keeps a network-only anchor interface active. Two complete real datagram test rounds passed: OFFER/ACK .10 and .11, gateway ARP, DNS answers and bidirectional peer frames. Between rounds both slot connections were released while the same daemon/anchor remained active. Evidence: `network/dataplane-anchor-round1.json`, `network/dataplane-anchor-round2.json`. This proves the helper/client packet path; real macOS guest networking acceptance remains separate.
- Live helper replacement exposed asynchronous launchd removal: bootstrap failed while the old label was still being removed. The installer now waits, with a bounded timeout and explicit service-not-found check, before registering the replacement. The revised helper was installed successfully. Evidence: `network/anchor-upgrade-retry.txt`; 35 native boundary/installer tests passed.
- An isolated NAT guest has booted from a base overlay and opened TCP/22. This is a diagnostic for Apple provisioning and SSH, not fixed-network product acceptance.
- That NAT diagnostic exposed macOS 27 host ARP filtering, consistent with [Apple DTS's explanation](https://developer.apple.com/forums/thread/822025?page=2). Its initial SSH trust gate correctly refused an invisible ARP entry. The formal path now uses source frames on the helper's own slot connections, without a host topology entitlement. Real tests passed for both slots: attachment/DHCP alone leave no evidence; wrong MAC/IP and ARP probes are rejected; valid ARP/IPv4 creates evidence; detach/reconnect clears it. Evidence: `network/dataplane-source-evidence.json`. Native boundary tests now total 64.
- The NAT diagnostic's normal shutdown request did not converge within two minutes; its explicitly forced shutdown was confined to that test instance and verified by process exit and release of its runner lock. Canonical initialized-guest shutdown remains a separate acceptance gate.
- `live-run-1` passed both real guest startups, independent identities/keys/auxiliary files, disk write isolation, host/guest and guest/guest ping, DNS, public HTTPS, remote exit codes and argument boundaries. First `up` took approximately 42 seconds for mac1 and 46 seconds for mac2. Both guests were ordinary `vonng` administrators with key authentication, passwordless sudo, automatic desktop login, disabled SSH password/keyboard-interactive/root login, and no Apple Account preference entries.
- The same run reproduced the normal-stop issue on an initialized guest. Normal stop now requests `sudo -n /sbin/shutdown -h now` over pinned SSH and still waits for runner termination and lock release. The first corrected shutdown succeeded in 8.30 seconds. `live-run-2` subsequently passed stop/start preservation for both guests in 42.87 seconds.
- With both guests running, another restore request failed in 0.99 seconds with the actionable `virtual_machine_limit` error. Neither guest was stopped to obtain capacity. Evidence: `acceptance/quota-restore.json`, `acceptance/after-quota-and-mac1-shutdown.json`.
- Native GUI packaging now includes `Farrow Mac.app`. CUA visually confirmed the mac1 and mac2 Finder desktops after automatic login. The final close-window check passed outside the lifecycle harness, as recorded above.
- `live-run-2` stopped safely during reset: the new app identity could not delete a credential created by the earlier bare development runner. The original slot files and identities were preserved. Same-build Keychain set/get/delete passed; cross-identity deletion reproduced `-25244`. The two disposable test credentials were migrated in memory with their original creator, without logging passwords or widening Keychain access. The final app then passed reset/destroy. Evidence: `acceptance/development-credential-migration.json`, `acceptance/live-package-run/summary.json`.

## Delivery boundaries

- This is a locally built, ad-hoc-signed development archive. The app retains each credential's signed owner for subsequent ad-hoc upgrades, with a real cross-signature Keychain test. Legacy instances need a one-time migration through the original creator. No Developer ID upgrade or notarization is claimed.
- Validation covers the current Apple Silicon/macOS 27.0 host and guest. Other host releases, unattended boot before host login and locked-Keychain CI have not been accepted.
- No newer Apple build was installed: online Apple discovery and same-build cached-model reuse passed live. Major-version rejection and reference-safe image handling have automated coverage. The live installation target remains 26A428.
- Local source acceptance and Git commits do not establish public publication. The existing Linux socket_vmnet and Docker virtualization processes were preserved.
- Both initialized test slots are retained in the isolated test home and normally stopped at closeout. The shared network helper remains installed; destroying a slot does not uninstall it. `remaining-20260927/final-preservation.json` records the latest state; the earlier baseline is in `acceptance/final-state.json` and `acceptance/final-shutdown.json`. The temporary retry network has been uninstalled; its unbooted base is retained as an acceptance artifact.

The live harness is `tests/mac-live-acceptance.py`. It requires an explicitly allowed, isolated test root, retains evidence on failure and leaves the surviving slot available for separate desktop inspection.


## Complexity review, 2026-09-28

The Mac command path now shares argument/manager/output handling, existing SSH
identity loading, slot-write identity validation and native VM state names.
Redundant SSH command wait goroutines and the unused production store-lock entry
were removed. The production code is 180 lines smaller; command entry files are
19.1% smaller. See [the complexity review](mac-complexity-review.md) for the
scope, retained recovery mechanisms and measurements.

The review also fixed doctor misclassifying a healthy `ready` guest and stopped
initialized readiness checks from rewriting SSH public-key files. Full
`make check`, native tests, app compilation/signature verification and a real
start/SSH/sudo/doctor/remote-exit/normal-stop cycle passed. An initial SSH shutdown
handshake timeout under high guest load is retained in the report; the complete
new-build retry passed, with normal stop taking 10.5 seconds. Both guests ended
stopped, with instance identities, SSH material, base files, network configuration
and the second guest's state preserved. These remain local, unreleased changes.
