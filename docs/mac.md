# Two macOS development slots

**Validated locally, 2026-09-26:** the packaged command passed real dual-guest acceptance on Apple Silicon/macOS 27.0 (26A428), including installation, fixed-address networking, SSH/sudo, native desktop access, stop/start, reset, destroy and base reuse. See [the implementation and acceptance log](https://github.com/pgsty/farrow/blob/main/docs/mac-implementation-log.md) for evidence and the development-signing limitations. That acceptance covers a local development bundle; formal release signing and notarization require separate verification.

**UX follow-up, 2026-09-27:** all six remaining checks passed: interrupting same-slot SSH readiness with stop, retained credentials across app signatures, real helper upgrade/uninstall/reinstall, GUI normal shutdown, online same-build image reuse, and retrying setup after cancellation during administrator authentication. The temporary retry network was removed; both original instances and their SSH identities were preserved. See the implementation log for evidence.

`farrow mac` manages `mac1` and `mac2` on an Apple Silicon Mac running macOS 27 or later. It uses Apple's Virtualization and DiskImageKit frameworks. It never reads `farrow.yml`, and its state is separate from Farrow's Linux inventory, QEMU processes, images and network.

The default guest is macOS 27.0 Golden Gate, build 26A428. Each slot starts with 4 virtual CPUs, 8 GiB memory and a 100 GiB sparse disk. The initial official IPSW download is approximately 26.63 GB. Keep about 100 GiB free for initial installation; guest software and development data need additional space.

## Build and use

The next Darwin arm64 release includes the native app in the normal Farrow archive.
The standard installer installs and upgrades it together with the CLI; Homebrew
keeps the app in its private `libexec` directory. On a supported host, use
`farrow mac up` and `farrow mac open` after installation. The currently published
v0.8.0 predates this feature; use the source build below until the new release is
published. Only building from source needs Xcode.

Build on Apple Silicon with Xcode 27 and the repository's Go toolchain:

```sh
make mac-build
bin/mac/farrow mac ls
bin/mac/farrow mac up
bin/mac/farrow mac up mac2
bin/mac/farrow mac exec mac1 -- sw_vers
bin/mac/farrow mac exec mac2 -- sudo -n id
bin/mac/farrow mac open mac1
```

The self-contained development bundle is in `bin/mac`; its tarball and checksum are in `bin`. Keep the CLI, Swift runner, network helper and installer together when moving the bundle. Native code is signed with a development ad-hoc signature by default; `FARROW_CODESIGN_IDENTITY` selects a real signing identity. A development archive is not a notarized public release. The network helper does not claim the restricted `com.apple.vm.networking` entitlement.

Opening the app directly explains how to start a guest from Terminal. Once a
guest is running, its Request Shutdown menu uses the normal CLI shutdown path,
including when installed by Homebrew. `farrow mac doctor` also checks saved SSH
material while a guest is stopped, so missing recovery files are reported before
the next boot. Release maintainers should follow [Mac release preparation](https://github.com/pgsty/farrow/blob/main/docs/mac-release.md).

The app bundle automatically retains its signed credential owner under `$FARROW_HOME/mac/credentials/owners`. New app builds continue to read and delete existing Keychain items through that verified copy. Ad-hoc rebuilds therefore do not require routine credential migration. Passwords remain in Keychain; the retained files contain only signed program code and public ownership metadata. Keep this directory with the rest of your Mac state.

For instances created before automatic owner retention, stop the affected slot and migrate once. The command finds the retained owner or the previous app saved by the local build:

```sh
farrow mac credentials migrate mac1
farrow mac credentials migrate mac2
# If the original app was stored elsewhere:
farrow mac credentials migrate mac1 --from "/absolute/path/to/previous/Farrow Mac.app"
```

Migration first retains the signed apps and keeps an encrypted Keychain recovery copy until the new password and its owner record are both saved. If interrupted or unable to save the record, rerun the same command; the retained app is discovered automatically, including when only final cleanup remains. Switching to a standalone runner removes the obsolete app reference. A missing original owner from an older installation still needs to be restored; Farrow preserves guest data and does not widen Keychain permissions. Custom standalone runners continue to use explicit `--from` migration after their signing identity changes. A consistent production signing identity remains recommended.

First setup requests macOS administrator authentication to install a small network LaunchDaemon. VM execution and the desktop run as the calling login user. Do not run the entire CLI with sudo. Once installed, daily operations use the helper's private Unix socket without another root prompt.

For an isolated trial, export an absolute `FARROW_HOME` before running any command:

```sh
export FARROW_HOME="$HOME/farrow-mac-trial"
bin/mac/farrow mac setup --ipsw /path/to/UniversalMac_27.0_26A428_Restore.ipsw
bin/mac/farrow mac up mac1
```

An existing local IPSW is copied into the private cache and validated with Apple's restore API. The source is preserved. Only macOS 27.x recovery images are accepted. Ordinary `up` reuses the recorded base and does not discover upgrades.

## Command behavior

An omitted target means `mac1`; `1` and `2` are accepted shorthand. `ls` always shows both slots, including empty ones, and does not create state or download anything.

| Command | Behavior |
|---|---|
| `mac setup` | Prepare network, download/import IPSW and restore an unbooted base; no user slot is started. |
| `mac init [slot]` | Create an independent disk overlay, Apple identity, auxiliary storage and credentials. Existing instances are preserved. |
| `mac up [slot]` | Fill in missing preparation, boot, configure a new guest and verify SSH/sudo. Repeated use preserves guest changes. |
| `mac start [slot]` | Start an existing initialized instance and verify SSH. Never create or download a missing instance. |
| `mac ls` / `mac status` | Show both slots with a live SSH/sudo readiness probe. An unavailable runtime is reported separately from a confirmed stop. |
| `mac configure [slot] --cpu 8 --memory 16G` | Change CPU/memory while stopped, preserving disk, user, identity and SSH trust. |
| `mac restart [slot]` | Gracefully stop and start an initialized guest. |
| `mac logs [slot] --lines 100` | Show recent native runtime diagnostics. |
| `mac ssh [slot]` | Interactive SSH using the instance key and pinned host key. |
| `mac exec [slot] -- command args…` | Execute over SSH, preserving argument boundaries and remote exit status. Use `sh -c` for shell expressions. |
| `mac open [slot]` | Show the running VM's native desktop. Closing the window leaves the VM running. |
| `mac password [slot]` | Explicitly reveal its local GUI password from Keychain. |
| `mac stop [slot]` | Request a graceful shutdown. `--force` explicitly powers off. |
| `mac reset [slot]` | Clear this guest and recreate a stopped, uninitialized instance from its original base. Follow with `up`. |
| `mac reset [slot] --update` | Recreate from the prepared default base instead. |
| `mac destroy [slot]` | Remove only this instance, leaving its stable address reservation and shared bases. |
| `mac image ls` | Inspect installers, bases, slot references, file lengths, allocated blocks and guest disk capacities. |
| `mac image update` | Explicitly discover Apple's latest supported macOS 27.x IPSW and prepare its base; existing instances keep their original base. `--ipsw` selects a local 27.x image instead. |
| `mac image prune` | Delete unused, non-default bases with no active image lock. |
| `mac image prune --installers` | Also delete idle IPSW caches and partial downloads. |
| `mac network status` | Show this environment's helper, subnet and available helper update. Use `--verbose` for paths. |
| `mac network uninstall` | Remove this environment's idle privileged service and network configuration; preserve guest disks, images, reservations and other environments. `setup` reinstalls it. |
| `mac credentials migrate [slot] [--from /path/to/previous/Farrow\ Mac.app]` | Migrate an older instance through its automatically discovered or explicitly selected original credential owner. |
| `mac doctor` | Inspect host support, runner, helper, disk, state and running guests' SSH/sudo. |

CPU and memory flags on `init`/`up` apply to first creation. Supplying different values for an existing guest is an explicit conflict, with a `configure` recovery command; they are never silently ignored. Disk, user and base remain creation settings. Sizes accept `16G` or `16GiB` (binary), `16GB` (decimal), and legacy integer byte counts. A newly configured slot inherits the prepared base's disk capacity; resource settings retained after destruction are reused.

```sh
farrow mac stop mac2
farrow mac configure mac2 --cpu 8 --memory 16G
farrow mac start mac2
```

`up`, `start` and `restart` wait for usable SSH by default. Add `--no-wait` (`-n`) to return after launching, then run `up` to finish readiness or inspect the desktop with `open`. Closing the desktop window leaves the VM running. The host menu **Farrow Mac → Quit Farrow Mac…** offers background execution, normal guest shutdown, or cancellation. The packaged GUI uses the same verified shutdown flow as `mac stop`, keeps the window open until the guest stops, and reports failure. If SSH is unavailable, the native shutdown fallback may need confirmation inside the guest. The host **Window → Keep Running in Background** menu also hides the desktop. Guest keyboard shortcuts remain captured by the guest.

Long operations use the same progress display and verbose phase timings as the default command path. Lock contention identifies the holding command and can be canceled with Ctrl-C. Shared image preparation does not block stopping a different running slot. `stop` also cancels an ongoing SSH-readiness wait for the same slot before shutting it down, so it does not wait for the 15-minute startup deadline. Permanent SSH authentication or host-key failures return actionable errors immediately; transient boot delays retain their wait behavior. Missing private keys and host pins are never silently regenerated for initialized guests.

`reset` and `destroy` describe their target and ask for confirmation. Use `--force` for noninteractive confirmation; this flag on these commands does not authorize killing another tool's VM. The other slot is preserved. Linux `purge` and `destroy` do not delete the `mac` subtree.

Global `--json` and `--yaml` work for structured commands. Installation progress goes to stderr. `exec --json` returns separate stdout, stderr and exit code. Interactive SSH requires ordinary terminal output. Secrets are not present in normal status or diagnostic output; `password` is the explicit exception.

Only `mac image update` performs update discovery. If Apple returns a different major version, the command refuses it and preserves the selected base. Neither ordinary `up` nor `start` discovers updates or changes an existing instance's base. When discovery identifies an already prepared compatible base, update reuses it without downloading the IPSW again, including after installer pruning. Explicit `setup` also safely upgrades an idle network helper; daily startup can keep using an already compatible helper and reports the available update.

## Accounts and network

The guest administrator defaults to the actual calling host user's short name; invalid or reserved names fall back to `farrow`. Set `--user` on first creation to choose another account. It is a normal local administrator, with SSH key authentication, passwordless sudo and desktop automatic login. No Apple Account, host password, SSH private key or Keychain is copied into the guest.

Apple's first-boot provisioning enables the account, automatic desktop login and Remote Login. Farrow then checks the running instance and its reserved IP/MAC association, authenticates with a random per-instance password, installs an SSH public key and pins the observed SSH host key. The dedicated network helper verifies source addresses in frames received from that slot's current connection; configuration or attachment alone is insufficient, and observations expire when the connection closes. IP/MAC matching is a first-contact trust boundary, not cryptographic server authentication. Later connections enforce the pinned host key. After public-key verification, SSH password and keyboard-interactive login are disabled; the GUI password remains in the host login Keychain.

macOS 27 can hide host ARP entries from applications without Network Topology Observation authorization. Farrow uses observations from its own network instead of requiring access to the host's entire ARP table. See [Apple's explanation of the macOS 27 behavior](https://developer.apple.com/forums/thread/822025?page=2).

The preferred private subnet is `10.10.20.0/24`, with `mac1` at `.10` and `mac2` at `.11`. First setup selects a nonconflicting private /24 and persists it, or accepts `--subnet`. Both DHCP reservations are configured before starting the network. Host-to-guest and guest-to-guest communication, NAT/DNS and public HTTPS passed with both real guests. Guests are not bridged onto the physical LAN, and no inbound port publication is configured. `up` and `doctor` check for conflicting routes, including VPN routes added after setup; a saved subnet is never silently changed.

The network daemon runs as root, validates peer UID and slot identity, and relays packets through a connected Unix datagram socket passed with `SCM_RIGHTS`. It does not pass QEMU's stream socket to VZ. Its configuration and executable are root-owned; its control socket is restricted to the owning user.

## Storage and recovery

Mac instance data lives under `$FARROW_HOME/mac`, including stable slot preferences, cached IPSWs, read-only unbooted ASIF bases, per-instance overlays and SSH trust. GUI passwords are in the login Keychain. A short, private `/tmp/farrow-mac-<uid>-<root-hash>` directory holds runtime sockets to respect Unix path length limits. The privileged network helper also has root-owned configuration under `/Library/Application Support/Farrow/mac-network`, a binary in `/Library/PrivilegedHelperTools`, a LaunchDaemon, and runtime sockets/logs under `/var/run/farrow-mac` and `/var/log/farrow-mac`. Destroying a slot leaves this shared host setup installed.

The two slots have independent Apple machine identifiers, auxiliary storage, writable layers, passwords, SSH client keys and SSH host keys. Stop/start preserves identity and data. Reset retains the slot's name, address and resource settings, but rotates the instance and trust identities.

`mac ls` shows guest disk capacity and each overlay's allocated blocks; `mac image ls` reports the installer and base separately. Allocated size comes from the host filesystem and is not exclusive physical ownership: APFS clones may share blocks. Do not add these numbers to estimate unique disk usage. `mac doctor` also reports available host space.

In the local acceptance run, the cached installer occupied 24.80 GiB of file data and the unbooted base reported 26.69 GiB of allocated blocks for a 100 GiB guest disk. Overlay usage grows as macOS initializes and indexes. After `image prune --installers`, a fresh slot reached SSH readiness in about 24 seconds using the existing base, with no installer download or restore. The base's hashes, inodes and modification times were unchanged.

Online `image update` reuses a ready base of the same build and capacity when Apple confirms its saved hardware model is supported, even if the online catalog prefers a different model. It does not redownload the IPSW solely because those model identifiers differ.

Interrupted downloads retain `.partial` data for validated Range resumption. Cached installers are checked against their recorded SHA-256, and the default build also has a pinned digest obtained from Apple's HTTPS metadata. A locally computed digest detects later cache corruption; it does not independently authenticate the source. An interrupted restore must restart; its failed base is never selected as default. An interrupted guest initialization can be retried with `up`, using the saved credentials and existing guest. Missing files in a previously booted instance are an error and are not silently recreated. If a slot's state or the shared configuration is missing while instance files remain, Farrow preserves those files and refuses to treat them as a fresh installation. Ready bases are immutable, and prune protects all slot references and the default base. Damaged installer cache metadata is shown as `damaged` and can be removed with `image prune --installers`. Setup checks local image validity, capacity and available space before asking for administrator installation; a canceled first authorization can be retried with a different subnet while no helper, base or guest exists.

The host-wide macOS VM limit includes other virtualization tools and base installation. Farrow does not stop unrelated VMs to free capacity. Stop one macOS VM before installing an updated base when both slots are running. Background execution is intended for a logged-in host session; unattended boot before login and locked-Keychain CI need separate validation.

Run `make mac-native-test` for native component tests and `go test ./...` for the cross-platform Go tests. Live acceptance evidence and remaining limitations are recorded separately in [the implementation log](https://github.com/pgsty/farrow/blob/main/docs/mac-implementation-log.md).
