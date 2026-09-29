# Changelog

Notable user-visible changes. This project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); everything below
1.0.0 is a pre-1.0 developer release and may break between minor versions.

## Unreleased

Barn 0.9.0 candidate: one error model, recoverable interrupted operations,
messages that say what went wrong and what to do, and a preview of macOS
guests.

Barn uses `barn`, `barn.yml`, `BARN_*`, and `~/.barn` throughout the CLI,
packages, host resources, and documentation.

### Added

- Preview: `barn mac` runs named macOS 27 virtual machines on Apple Silicon
  with macOS 27 or later. `mac up [name]` creates and starts a machine and
  waits for SSH and sudo; `mac open` shows its desktop; `mac ssh`/`exec` reach
  it with its own pinned key. Machines are copy-on-write clones of one unbooted
  base, have their own private network with a fixed address, share host
  folders and the text clipboard, and never read the Linux inventory. See
  [the Mac guide](docs/mac.md).
- The macOS restore image comes only from Apple, after a confirmation that
  shows the download (or `--yes`), resumable and verified against Apple's
  SHA-256; `--ipsw` uses a local image, cloned on the same APFS volume instead
  of copied. Nothing in the Mac path runs as root.
- `mac stop` shuts down through macOS and powers off a machine that is still
  running after two minutes, saying so; `--force` powers off at once. `mac
  start --recovery` boots macOS Recovery. `mac configure` changes CPUs, memory,
  shared folders, the network and clipboard sharing; `mac recreate` replaces a
  machine with a fresh macOS of the same settings and address.
- `mac ssh-config --install` adds the machines to `~/.ssh/config`, and the first
  ready machine installs it automatically, so `ssh mac1` and editors with
  Remote-SSH work; lifecycle commands keep it current.
- `barn mac` needs its native component, `Barn Mac.app`. Releases include
  it only once it is Developer ID signed and notarized by Apple; until then
  `make mac-build` (Xcode 27) builds both into `bin/mac`, and a release without
  it still publishes.

### Changed

- Embed Catalog `2026092901` with Debian 12 `20260923.2610.1`, Ubuntu
  22.04/24.04 `20260926.0.0`, and Ubuntu 26.04 `20260927.0.0` on amd64 and
  arm64 (45 artifacts, retaining all 37 previous artifacts). Debian 12 still
  needs offline XFS tools and `en_US.UTF-8` generation; Ubuntu keeps the
  upstream image bytes. Both retain `C.UTF-8` as the default locale.
- Every failure has one shape: `error: <message>`, the failing program's last
  stderr lines when an external tool failed, and a `next:` line when there is
  one clear action. External failures name the program (`apt-get exited with
  status 100: …`, `brew timed out after 20m0s`) instead of dropping its stderr
  or printing its full argv; the argv is shown with `--verbose`.
- Exit codes keep their numbers with sharper meanings: `integrity` (7) now means
  only a failed digest, identity, or ownership check, and unclassified failures
  are `runtime` (1). Notable moves: no inventory for `up`/`plan` 4→2; unknown
  image, channel, or version 1→2 (and `validate` now reports it); import name
  collision 7→2; missing QEMU from image commands 1→3; node not running 1→4;
  setup and network download, sudo, and launchd failures 7→1/3; hosts name
  taken 7→4; vmnet busy 4→6.
- **JSON:** failures carry `error` from a closed set (`runtime`, `usage`,
  `capability`, `conflict`, `partial`, `resource`, `integrity`, `cancelled`,
  plus `remote_exit` for ssh/exec), an optional stable `reason`, `next`, and
  `command` for an external program. `recreate_required` and `nodes_removed`
  moved from `error` to `reason` (class `conflict`); network-preflight and
  persistent-disk deletion failures now carry the class of their cause, and
  `ssh-config` failures use the same envelope. `resource_conflict` is now
  `resource`. JSON output no longer escapes
  `<`, `>`, and `&`.
- A second barn command waits up to 10 minutes behind a running one and says
  which (`barn up (pid 4821, since 14:02:31)`); on timeout it exits 4 with
  reason `deployment_busy`. `status`, `ssh`, `exec`, `ssh-config`, and `hosts`
  do not wait: they show the recorded state with a `note:`.
- `network` and `hosts` install/uninstall ask for confirmation on a terminal
  (`[Y/n]` for install, `[y/N]` for uninstall). `hosts` plans without sudo, and
  `network install` on a Mac without a Barn network points to `barn setup`
  before any sudo prompt. Setup's plan states when it will use sudo and where
  socket_vmnet comes from; if it must switch to another /24 after the
  confirmation, it says so and asks again (unless `--yes`).
- `plan` lists each node's data disks, including the implicit 128 GiB `/data`.
  Sizes drop a trailing `.0` and use TiB from 1024 GiB.
- `logs --source events|qemu` prints one readable line per record; the QEMU
  argv appears only with `--verbose`.
- Bare `barn` and a plain `destroy` without a deployment succeed;
  `destroy --delete-persistent/--purge` without state points to `barn purge`,
  which removes retained disks. `--version` is accepted. `-f` completes YAML
  files only. `validate` accepts `--repo` to check images against that
  repository's catalog; an unreadable catalog is a warning, and imported local
  images are left to `up`.
- Inventory errors state the rule, value, and line (`line 3: host 10.10.10.10
  vm_mem = 10: must be at least 512 MiB …`), suggest the nearest `vm_*` key,
  and explain reserved addresses and paths. macOS warns about `vm_shares` at
  `validate` and `plan`. A share path through a symlink is still refused, now
  with the real path to write instead of a raw `too many levels of symbolic
  links`.

### Removed

- The `rm` alias for `purge`. Two letters no longer discard a lab.

### Fixed

- A node whose recorded QEMU PID now belongs to an unrelated process (typically
  after a host reboot) is recognized as stopped instead of blocking every
  command with advice to stop that process by hand.
- An interrupted `stop` whose VM kept running resumes as running; interrupted
  transitions name the finishing command instead of looping on `barn status`.
  `destroy` settles interrupted transitions itself.
- A failed first `up` recovers after the inventory is edited; unfinished node
  directories are rolled back by their journaled artifacts, not their old spec.
- A destroy interrupted after preserving a persistent disk can be retried.
- A guest error whose last stderr line contains a tab or escape sequence is
  reported with its stage and detail instead of an unreadable marker.
- `ssh` and `exec` refuse a near-miss node name (`did you mean pg-meta-1?`)
  instead of running it as a command on the default node. Only words shaped
  like node names (with a digit or `-`) are checked, so `ls`, `df`, or `wc`
  still run.
- A mistyped destroy/recreate confirmation says what was typed and that nothing
  changed.
- A symlinked or hard-linked `~/.ssh/config` (dotfile managers) is never edited;
  Barn publishes its fragment and asks once for the `Include` line.
- Doctor no longer recommends `destroy --force` for unreadable state or
  `recreate --force` for an unfinished create; `barn up` finishes the latter.
- `install.sh` reports an unreachable GitHub as a network failure instead of
  "no stable release is published".
