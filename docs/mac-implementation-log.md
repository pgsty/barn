# Farrow Mac implementation and acceptance log

This log records how the current `farrow mac` was validated and what is not
yet covered. The user contract is [the Mac guide](mac.md).

No release before 0.9.0 contained `farrow mac`. Development builds from
2026-09-21 to 2026-09-28 used two fixed slots, passwords in the login Keychain
and a root network daemon. That design was replaced on 2026-09-29; its
proposals, reviews and acceptance records remain in git history up to commit
`45f931b`. The hidden `farrow mac migrate` command converts machines those
builds created. Raw evidence (logs, JSON results, screenshots) stays on the
test host and is summarized here.

## Design, 2026-09-29

The redesign kept `farrow mac` as its own command domain, unified its contract
with the Linux commands rather than its inventory, removed the root network
helper and the Keychain, and decoupled the Mac app from the release.

- **Named machines.** Schema 2 records under `slots/<name>`; any number of
  machines, two running at a time (Apple's limit, reported with the machines to
  stop).
- **Network without root.** Each runner creates its own shared-mode vmnet /24
  (gateway `.1`, DHCP reservation `.10`) with only the virtualization
  entitlement; the network ends with the runner. Machines are isolated from
  each other.
- **Local Network privacy.** macOS 27 refuses connections from non-Apple
  processes to these subnets with "No route to host" unless the responsible
  terminal app is authorized; Apple's tools are exempt. The CLI's SSH runs over
  `/usr/bin/nc`, and `ssh`/`exec` always use `/usr/bin/ssh`.
- **Secrets.** The desktop password is a 0600 file beside the machine's SSH
  key; host keys are pinned by instance alias, so an address change keeps trust.
- **Runner protocol 2.** A Go Unix-socket RPC client with peer-UID checks;
  VirtioFS shares, Recovery boot, a screen-sized display, window frame
  autosave, full screen, and focus-following text clipboard over pinned SSH.
- **Stop policy.** Normal shutdown through macOS, powered off after two minutes
  with `forced` in the result; `--force` powers off at once.
- **Release.** The native app is an optional release asset: without signing
  secrets a release ships the CLI alone, and with them a failed Mac build stops
  the release.

## Acceptance, 2026-09-29

Host: Apple Silicon, macOS 27.0 (26A428), ad-hoc signed development build of
the Mac component. No restore image was downloaded: every test home used an
APFS clone of an existing 26A428 base.

| Check | Result |
|---|---|
| Unit and repository gates | `make check` (unit and race tests, vet, Staticcheck, deadcode, errcheck, govulncheck, four-target cross builds, installer and license checks); `make mac-native-test` (bridge, image and menu tests); `make mac-build` with the extracted-bundle checksum, signature and probe checks. |
| Automated live acceptance | `tests/mac-live-acceptance.py --gui`: all 16 phases passed in 244.9 s on a fresh home — creation from the base (47.8 s for mac1 including a base fingerprint), a named machine with a share, independent identities and sshd policy, disk isolation, per-machine networks with peer isolation, DNS and HTTPS 200, exact exit codes and argument boundaries, bidirectional shared-folder writes, normal stop/start of both, `configure` (3 CPUs, 5 GiB applied), refusal of a third running VM (`mac_vm_limit`), `recreate` rotating every identity while keeping the address, desktop open with clipboard host→guest and guest→host, repeated `up` reusing the unchanged base, and `destroy` preserving the other machine. |
| Manual live checks | First provisioning completed in under a second once SSH answered; normal stop 6.5 s, start to SSH-ready 6–12 s, restart 17.8 s, forced stop 0.7 s; `start --recovery` showed Recovery; a subnet change kept the alias-pinned host key; `ssh mac1` through the installed fragment; the fragment and its Include were removed with the last machine. |
| Documentation review | Every command in the public guide was run against the real binary. Two defects were found and fixed: a new machine was created before the two-VM limit refused it (now refused first: exit 6, nothing created), and `up` with a changed `--user`, `--disk` or `--ipsw` suggested `recreate`, which keeps those settings (now it names the command that applies them). |
| Migration of a development home | `migrate` converted two machines with `.v1` backups, moved both desktop passwords from the Keychain into 0600 files, re-pinned host keys, kept both MAC addresses and moved mac2 to 10.10.21.10; mac2 then booted in 12 s and its migrated password worked in the guest. Removing the development root daemon needs the owner's sudo password and has not been run. |

## Pre-release review, 2026-09-29

Two independent reviewers read the Go side and the native app with its
packaging before release. They found no critical or high issue; the medium and
low findings were fixed with regression tests where the code allows:

- A machine named `X-ready` shared its RPC socket path with machine `X`'s
  readiness endpoint; the endpoint now uses a dot, which names cannot contain.
- `up NEW --disk N` restored the pinned 27.0 build and made it the default;
  another capacity now keeps the default base's macOS and leaves the default
  alone. `recreate --update` keeps the machine's disk size and names the
  `setup --disk` command when that size is not prepared.
- `ssh-config --remove` is remembered until `--install`; names that
  `~/.ssh/config` already uses for another host are published by address
  only; `--install` with no ready machine no longer touches another home's
  entries; a `FARROW_HOME` with spaces works with `mac ssh` and `mac exec`.
- `16GB` means 16 GiB, memory must be whole MiB, `--subnet` is checked before
  macOS is prepared, `recreate` checks shares and the network before erasing,
  a digest mismatch exits 7 with a next step, and a discovered restore image
  without Apple's SHA-256 is refused.
- A machine directory left without a record no longer blocks other commands,
  and `destroy` removes it; Finder files in the image store no longer break
  `image ls`; a partial stop, start or destroy keeps its per-machine report;
  `stop` powers off after the grace period even when neither shutdown request
  was accepted.
- The runner no longer cancels a logout, restart or shutdown of the Mac; the
  guest screen no longer sits under the title bar; Restart… accepts the
  connection a rebooting guest drops; signals and a desktop without its CLI
  shut the guest down over SSH; clipboard copies no longer overwrite a newer
  host copy, failed pushes are retried, and large guest text is read in order;
  closing a full-screen window leaves no empty Space.
- Packaging: the Homebrew caveat mentions `farrow mac` only when the app is
  installed, `install.sh` under Rosetta installs the native Apple Silicon
  build, and `make mac-build` no longer accumulates old app copies. Without
  signing secrets the release job skips the Mac app cleanly; with them a
  failed Mac build stops the release.

Known and left for later: another local user can create the predictable
`/tmp/farrow-mac-<uid>-<hash>` runtime directory first and block `farrow mac`
for that user (no takeover is possible), and IPv6 isolation between machines
is not tested.

## Findings that shaped the design

Development builds established these facts, and the current design relies on
them:

- Apple's 26A428 restore image is 26,626,436,228 bytes. It downloaded in about
  880 seconds, matched the SHA-256 from Apple's HTTPS metadata, and passed
  Apple's restore-image validation.
- A full restore into a new base took 190–350 seconds on the test host.
  Installation can report completion while the installer VM still runs, so the
  runner waits for it to stop before freezing the base.
- vmnet's IPv4 subnet call takes the gateway address. The network address is
  accepted at configuration time but fails when the interface starts.
- Apple's online catalog can name another hardware model for the same build
  than a local restore image does. `image update` checks the saved model with
  Apple before reusing a same-build base.
- A shutdown request through Virtualization did not stop an initialized guest.
  Normal stop runs `sudo -n /sbin/shutdown -h now` over the pinned SSH
  connection and then waits for the runner to exit.
- With two macOS VMs running, Apple refuses another start or restore within a
  second (`virtual_machine_limit`) without affecting the running ones.

## Not yet verified

- A first `up` that downloads macOS from Apple and restores it through the
  current CLI. Development builds verified the download and restore on
  2026-09-26; the current code reuses them, but every 2026-09-29 test home
  cloned an existing base.
- `image update` with the current CLI, and to a newer macOS 27 build.
- The desktop's **Machine → Restart…** and **Shut Down…** menu items after the
  redesign, and a logout, restart or shutdown of the Mac with a machine
  running.
- `migrate` removing the development root daemon.
- Developer ID signing, notarization, and installing the component from a
  release archive or Homebrew.
- Other host macOS releases, guests other than 27.0 (26A428), a host before
  anyone logs in, and a locked login keychain.

## Running the live acceptance

`tests/mac-live-acceptance.py` needs an explicitly marked, disposable
`FARROW_HOME` (`--allow-test-root`), the bundle's `farrow` and an empty
evidence directory. It creates, recreates and destroys machines there, keeps
guest state when a phase fails, and never reads passwords or private keys.
`--gui` adds the desktop and clipboard phases; `--allow-download` lets `up`
fetch macOS from Apple when no base is prepared; `--plan` prints the sequence
without running anything. Use a scratch `HOME` as well: a separate
`FARROW_HOME` does not isolate `~/.ssh/config`.
