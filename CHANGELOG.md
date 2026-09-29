# Changelog

Notable user-visible changes. This project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); everything below
1.0.0 is a pre-1.0 developer release and may break between minor versions.

## [Unreleased]

Barn 0.9.0 candidate: one error model, recoverable interrupted operations,
messages that say what went wrong and what to do, and a preview of macOS
guests.

The project is renamed from Farrow to **Barn** in one cutover. Executables,
configuration discovery, environment variables, state fields, host resources,
package names, Go module paths and current documentation use Barn. There are
no Farrow aliases, old-state readers, compatibility paths or Mac migration
command. Existing internal labs must be retired with their original binary
and recreated under Barn after saving needed data. Historical release notes
and signed image provenance retain the identity of their original builds.

**For scripts:** exit codes and the JSON error envelope changed, and the `rm`
alias is gone; see Changed and Removed.

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

## [0.8.0] - 2026-09-21

### Changed

- Embed Catalog `2026092001` with September 2026 Debian 12/13 and Ubuntu
  22.04/24.04/26.04 images (37 artifacts, including all 27 previous artifacts). Debian retains the offline XFS and
  `en_US.UTF-8` adjustments with `C.UTF-8` as its default; Ubuntu retains the
  upstream cloud-image bytes. Existing pinned versions are retained.

### Fixed

- macOS setup prepares the socket_vmnet source before requesting administrator
  authentication, so Homebrew cannot invalidate the credential needed for the
  ensuing network installation. Failed or cancelled downloads do not prompt.
- Include the previously published Debian locale adjustment in the checked-in
  image recipe and verify both locale postconditions before accepting a build.
- A missing host share fails only the affected node during `up` and `start`;
  independent nodes continue, including stopped peers when a new node cannot
  prepare. The error names the source, guest path, and required restoration;
  Farrow never creates an empty replacement directory.
- macOS directory sharing now detects the unsupported directory-descriptor
  reopen before QEMU launch, and before destructive restart/reload/recreate.
  This improves diagnosis; macOS 9p sharing support is still unresolved. Source
  paths are not substituted in ways that bypass directory identity checks.
- Existing deployments recover a missing public key from their original private
  key during startup. Missing private keys never create a new VM login identity.
- `up` rechecks the installed control-node SSH key instead of trusting an old
  ready marker. A missing guest key reports a node-to-node SSH limitation while
  management access remains usable; restoring the original key clears it.
- Partial `start` and `restart` results still update SSH aliases for successful
  peers, including repaired management ports. Scoped retry instructions preserve
  the inventory, repository and applicable flags; `start` retries remain `start`.
- Setup and lifecycle retries share one operation ID. Bounded event logs remain
  readable before deployment state exists, including after failed first setup.
  Setup traces record phases and error categories without persisting arguments
  or authentication data; `setup --dry-run` does not create a trace.
- Successful persistent-disk deletion and purge report their final resource
  retention accurately, without contradictory intermediate preservation claims.
- Destroying the remaining deployment now preserves owned disks left by a
  previously removed node. Those disks no longer block later destroy/purge;
  unknown artifacts and incompatible identities are still rejected.
- Interactive `start`, `restart`, and `reload` now prepare missing host tools
  and repair an intact Farrow network through the same setup flow as `up`.
- A missing or restrictive root-owned macOS vmnet log directory is recoverable
  without losing network ownership evidence. Diagnosis reports its actual mode
  and an exact repair command instead of false interface and route conflicts.
- `image info` can describe an uncached image without QEMU. Unknown aliases and
  unsupported architectures retain their catalog errors; registered local image
  failures retain their actual cause, including the integrity exit code.
- A missing deployment SSH public key is restored from the existing private key.
  A lost private key with a surviving public key now gives backup recovery
  guidance instead of generating a replacement identity.
- Setup retries temporary socket_vmnet download failures up to three attempts
  per source, cleans incomplete files, and honors cancellation and server
  cooldowns. Archive verification failures remain errors.

## [0.7.0] - 2026-09-16

### Changed

- Test labs remain usable when individual data disks, shares, guest hostnames,
  node-to-node SSH, or private-network checks fail. Guest limitations are grouped
  in the result, persisted for `status`, and included in JSON; usable limited
  guests return success. Guest identity and management SSH remain required.
- Guest limitations are cached separately from the schema-2 node documents,
  keeping those documents readable and destroyable by 0.6.0. A missing or
  damaged warning cache cannot block VM management.
- `farrow up [node...]` automatically retries unfinished guest setup and updates
  old helpers without restarting running VMs. Healthy stages are skipped; there
  is no separate repair command.
- Writable shares fall back to read-only when guest access is insufficient,
  preserve that fallback on reboot, and retry writable access on the next `up`.
  Explicitly selected group-writable project directories are accepted without
  changing host permissions.
- Fast setup checks stay quiet; longer operations use one terminal progress
  area with elapsed time, download metrics, and per-node readiness. No-color
  terminals still redraw; pipes use throttled plain progress on stderr.
- Successful starts print a compact result and connection command. Partial
  results retain ready nodes and offer a scoped retry, including the inventory
  path. Narrow terminals use readable cards, with Unicode-aware alignment.
- SSH aliases, guest metadata refresh, and event-log failures are warnings after
  successful VM operations. Explicit integration commands still fail normally.
  Their lifecycle waits have separate time budgets, so an unavailable optional
  integration cannot consume the whole startup timeout. Verbose output records
  the elapsed time attributed to each foreground stage.
- First-use help and Homebrew caveats lead with `up` and `ssh`; `init` remains
  available for customization and `setup --yes` for unattended preparation.
- Setup checks host capabilities separately from deployment diagnostics, repairs
  an intact inactive Farrow network, and installs the hosts helper on demand.
  Fresh untouched default inventories can select an available subnet with a
  backup of the original. Default templates are shorter; size errors include the
  file, line, input value, and a valid example.

### Fixed

- Unusable test data filesystems can be reset automatically by `up`, with an
  explicit notice that previous data was discarded (including persistent data
  disks). Healthy disks are reused; probe/tool failures, missing devices, busy
  mounts and backend I/O failures do not trigger formatting. Healthy repeated
  starts no longer run filesystem growth tools.
- Stopped VMs recover automatically from occupied management SSH ports; saved
  invocations, connection details and SSH aliases use the replacement port.
  Application forwards and running VM identities are preserved.
- Guest shares use per-user access checks, fixing newly created files that
  belonged to the wrong guest identity and could not subsequently be written.
- One failed disk/share does not skip the remaining independent mounts. Failed
  control-key installation retains the root-only staged key for retry; successful
  installation consumes it, and later retries validate the installed key.
- New qcow2 disks always use mode 0600 regardless of the host umask. Recreate
  and destroy can retain older owned disks created as 0644/0640, narrowing their
  permissions instead of refusing disks Farrow itself created.
- Repeating `up` after an offline node-prepare failure safely clears recognized
  uncommitted artifacts before trying again, instead of failing with "file
  exists". Committed nodes, runtime artifacts and unrecognized files are kept.
- Guest initialization no longer depends on `example.com` being reachable.
  Offline guests finish their disks, shares and private-network setup. Bootstrap
  failures show `setup failed` with inspection/rebuild guidance instead of an
  ineffective SSH retry; rebuilding remains explicitly confirmed.
- `exec` preserves multiple remote arguments, including `sh -c` scripts, spaces
  and empty values. Single-string shell commands and `ssh` retain their syntax.
- `doctor` no longer scans unrelated guests across the entire shared subnet or
  reports other users' running VMs as conflicts. Lifecycle commands still probe
  the addresses they will actually use. Linux setup names its bridge correctly.
- Subsecond operations omit transient progress. Unattended first use points to
  `setup --yes` followed by `up`. Legacy-layout guidance preserves existing VM
  disks instead of suggesting deletion of the whole data directory.
- Failed lifecycle commands show the tool, exit code and cause without flooding
  the summary with QEMU arguments; verbose and JSON retain the full diagnostic.
- The installer detects an earlier Farrow entry in PATH even when its own
  directory is already present, and prints a command for the installed binary.
- SSH host-key trust follows the VM UUID across port reuse. Permanent SSH access
  errors fail promptly, while readiness timeouts retain the last useful cause.
  An owned empty `known_hosts` permits the first connection after `--no-wait`;
  private keys, ownership, file modes, and link checks remain required.
- Transient image failures retry with resume and bounded backoff. Official
  repositories fail over to each other without bypassing digest checks; custom
  repositories remain exclusive. Server cooldowns above the retry budget skip
  that source. Resumed download speeds count only newly transferred bytes.
- Verified cache permissions can be repaired; damaged unreferenced cache files
  are preserved before replacement. Backing files referenced by nodes or pending
  transactions are never moved automatically.
- Starting from applied state can prepare the host without creating a new
  inventory. Repeating interrupted starts preserves readiness facts and partial
  results instead of discarding successful nodes.

## [0.6.0] - 2026-09-05

### Changed

- New inventories and the built-in image catalog default to Ubuntu 24.04
  (`u24:stable`). Existing disks are not replaced automatically. Set
  `vm_image: d13` in old inventories that relied on the implicit Debian default.
- Embedded Catalog `2026090501` includes the already-published Debian 12/13 and
  Rocky Linux 8/9 image updates, keeping its stable channels aligned with the
  official repositories.
- `plan` works before host setup and shows exact image versions, total resources,
  configuration changes, pending starts, and disk replacement effects.
- Default status tables show image and resource information. One degraded node
  no longer hides the rest of the deployment; per-node errors remain visible.
- Starting commands refresh Farrow-managed guest hostnames and control-node SSH
  configuration, including existing peers after expansion or removal. Managed
  guest SSH entries accept recreated lab nodes without manual known_hosts cleanup.

### Fixed

- Selected recreate checks remaining configuration conflicts before deleting
  disks; reload checks startup dependencies before stopping existing guests.
- Destroy and stop read their state under the deployment lock. Runtime cleanup
  preserves listening sockets/live pidfiles, and lifecycle tests use isolated
  runtime directories.
- Invalid integer sizes, fractional CPU counts, and extra YAML documents are
  rejected without truncation or overflow.
- Corrupt deployment state retains its error/path, failed image digest checks
  return integrity errors, and SSH status 255 is passed through consistently.
- ALL_PROXY/all_proxy now supplies a fallback while honoring scheme-specific
  proxies and NO_PROXY. Custom init output paths have usable next commands.
- SSH shorthand help matches its convenience behavior, presentation flags
  respect option values, and --no-wait no longer claims guest readiness.

## [0.5.0]

### Added

- `farrow purge` (alias `farrow rm`) discards the complete applied deployment
  without a confirmation prompt. It deletes every VM, persistent data disks,
  deployment keys and state, and the default SSH client fragment while keeping
  the verified image cache and host-global network. The command is idempotent
  when no deployment exists and still refuses unidentifiable residual node
  artifacts instead of deleting them by path alone.
- The official-image pipeline has an eight-target, digest-pinned candidate
  matrix for Debian 12/13 and Rocky Linux 8/9 on amd64/arm64. Package inputs
  are installed offline from an exact minimal closure, recorded in the SBOM
  and provenance, and can be assembled into a separate unsigned `testing`
  repository without an upstream artifact fallback.

### Changed

- The pinned build and CI toolchain now uses Go 1.27.1, GoReleaser 2.18.0,
  golangci-lint 2.13.2, `golang.org/x/crypto` 0.56.0, and the latest selected
  transitive archive/image modules.
- The default official image repository is now `https://repo.pigsty.io/farrow`.
  Downloading commands accept long-only `--mirror` to select
  `https://repo.pigsty.cc/farrow`; an explicit `--repo` remains highest
  priority, ahead of `--mirror`, `FARROW_REPO`, and the default.
- Catalog upstream URLs are provenance metadata rather than fallback download
  sources when a repository is selected. Catalog SHA-256, artifact size, and
  virtual size identify the final repository qcow2.

### Fixed

- Rocky Linux 8 candidates provide a working `/usr/bin/python3`, remove legacy
  interface naming state, and make the SSH drop-in Include effective. Rocky
  Linux 9 candidates remove the same legacy networking state. Debian 12/13
  candidates include the XFS userspace needed by Farrow data disks.

## [0.4.0]

### Changed

- Pigsty inventory `vm_disks[].fs` now defaults to `auto`: a blank disk is
  formatted XFS when the guest has `mkfs.xfs` and ext4 otherwise, the same
  choice the Pigsty Vagrant flow makes, so the default `/data` disk works on
  Debian and Ubuntu images that ship without xfsprogs. Explicit `xfs` and
  `ext4` never fall back. Deployments created with the old `xfs` default are
  not reported as drift and their persistent disks stay compatible: `auto`
  matches whatever the guest already formatted.
- On a terminal, `farrow up` runs `farrow setup` itself when the fixed-IP
  network has never been installed on the host, then continues. `farrow init`
  points at `farrow up` as the next step.
- One output style across commands: lifecycle commands and `status` print a
  node table (`NAME STATE ADDRESS SSH ARCH PID`) and, after `up`, a
  `next: farrow ssh <node>` hint; `plan` prints one `no changes` line or only
  the rows that change; `validate` prints one line; `image list` prints the
  catalog revision and origin plus an aligned table; `network status`,
  `doctor`, and preflight errors use plain sentences without machine codes;
  `spec hash` and other internals moved to `--json`.
- `farrow ssh <node> -- '<shell line>'` and `farrow exec` pass the remote
  command exactly as OpenSSH does, so `farrow ssh meta -- 'df -h /data; id'`
  works.

### Fixed

- Guest bootstrap failures carry the failing command's last message, so
  readiness reports `guest bootstrap failed during data-disks: xfs requested
  but mkfs.xfs is unavailable` instead of only an exit status.
- Errors about an unknown node name list the nodes the deployment has.

## [0.3.0]

### Added

- `farrow update` fetches the configured image repository's catalog, verifies
  it, and activates it; `image sync` remains the exact-URL/file recovery path.
  Neither operation updates the Farrow executable.
- Pigsty inventory `vm_disks[].fs` accepts explicit `auto`: blank disks prefer
  XFS when its formatter is available and otherwise use ext4. Omitting `fs`
  retains the XFS default.
- Partial multi-node failures name each failed node with its stage and error
  (`2 of 3 node(s) failed: ...`), in text and in the JSON `failures` array,
  and point at `farrow logs <node>` when a guest did not become ready.

### Changed

- Farrow no longer refreshes the image catalog automatically. `up`, `plan`,
  and ordinary `image` commands use the catalog embedded in the installed
  release or the one last activated by explicit `farrow update`/`image sync`,
  so normal resolution never touches the network for catalog metadata.
- Guest network configuration matches interfaces by MAC address and no longer
  renames them to `mgmt0`/`private0`; interface names inside the guest are not
  a Farrow contract.
- `up` and `start` recheck readiness for already-running guests without
  restarting QEMU; `--no-wait` skips the check.
- The management-network egress check retries three times instead of hanging
  on one connection.
- `image reset-manifest` is now `image reset` (the old name remains as an
  alias). Image output and help say "catalog" and "revision" consistently.
- The `ss` command is gone; `up` already installs the SSH client
  configuration, and `ssh-config --install` remains.
- Short flags: commands that read an Inventory use `-f` only for `--file`;
  `logs -f` retains the conventional `--follow`. `--force`, `--rollback`, and
  ssh-config `--remove` have no shorthand, and `--name` no longer takes `-n`.
  Root aliases `cp`, `h`, `i`, and `p` were removed.
- Drift and plan hints suggest `farrow recreate <node>` and
  `farrow destroy <node>` without `--force`; the interactive confirmation
  covers terminals.
- Error messages drop the internal `private` prefix; the missing-inventory
  error points at `farrow init`; `hosts install` without a deployment reports
  the shared `no deployment state found` message with exit 4; an SSH
  configuration failure after a successful lifecycle step exits 5.
- `status` no longer prints a trailing `deployment status` line, and
  `image list` no longer prints digests (use `image info` or `--json`).

### Fixed

- Guest bootstrap failures publish a stage marker, so readiness returns
  `guest bootstrap failed during <stage>` instead of waiting for the full SSH
  timeout, and readiness timeouts report ssh's last error instead of the
  probe's argument list.
- A partially successful lifecycle operation still reconciles the SSH client
  configuration for every committed node.
- Multi-node create no longer reports every guest ready before inspecting the
  per-node readiness outcomes.
- Network preflight failures include the suggested fix in text output, not
  only in JSON.
- `hosts install` now distinguishes a missing deployment (exit 4) from corrupt
  or unsafe deployment state (exit 7) instead of masking every read failure as
  "no deployment state found".

## [0.2.0]

A correctness and hygiene release: every command boundary behaves the same
way under signals, structured output, and misspelled input, and the tree now
carries the gates that keep it that way. The engine, the inventory format, the
state layout, and the embedded image catalog (revision 2026082903) are
unchanged.

### Fixed

- Fixed-IP network preflight now permits less-specific VPN exclusion routes,
  such as a physical-interface `10.0.0.0/8` surrounding Farrow's owned
  `10.10.10.0/24`; equal or more-specific foreign routes, overlapping
  interfaces, and a missing owned route remain hard failures.
- New and recreated guests remove both the released
  `# farrow-project-host` marker and its replacement before writing only
  `# farrow-deployment-host`, so the 0.2 terminology migration converges
  without duplicate `/etc/hosts` rows.
- Selected `up` and incremental scale-out now resolve/download images only for
  nodes being created, preserve committed peer ports and UUIDs, show desired
  nodes without state as `absent`, and generate SSH/host integrations from the
  committed node set instead of failing on uncreated inventory peers.
- Whole start/stop/restart/destroy and persistent-disk validation now tolerate
  desired nodes that have not been created while retaining fail-closed checks
  for every committed node and retained disk.
- `SIGINT`/`SIGTERM` now cancel long-running commands through Cobra's context,
  stop progress output, release normal deferred resources, and exit 130 with
  one structured cancellation result; Ctrl-C inside SSH remains remote.
- `farrow reload` is one engine operation: definition drift and removed
  nodes are refused before any node stops, and `--json`/`--yaml` emit a
  single document instead of one per phase.
- `farrow ssh` and `farrow exec` refuse a misspelled node before `--` instead
  of running it as a command on the control node.
- Unknown or repeated node selectors are usage errors (exit 2) reported before
  host preflight or a confirmation prompt, not runtime errors after them.
- `farrow destroy` asks for the same `destroy` token whether or not nodes are
  selected, and prints the exact scope (nodes, disks, keys, state) first.
- Refusing or leaving `farrow setup` confirmation unanswered, and mismatching
  the `destroy`/`recreate` token, now return one cancellation result and exit
  130; only an answered setup prompt defaults to yes. Running `setup` without
  `--yes` on a pipe is a usage error (exit 2), like `destroy` without
  `--force`.
- An interrupted `up`, `recreate`, or `destroy` still appends its event to
  `events.jsonl`, so the audit trail shows what the signal cut short.
- `farrow logs --source events` rejects a node argument instead of silently
  returning the deployment-wide log under that name.
- Combined verbose shorthands (`-vv`, `-nv`) enable diagnostics.
- Structured failure payloads keep the real reason for usage errors that were
  previously written straight to stderr.
- Verbose diagnostics redact credentials and query strings from source URLs.
- `farrow-hosts-helper --help` exits 0; its non-root and positional-argument
  tests no longer pass for the wrong reason.

### Changed

- Marker-owned guest `/etc/hosts` rows now use deployment terminology; the
  0.1.0 marker remains cleanup-only compatibility through the documented
  migration window.
- Inventory validation and migration diagnostics now consistently call the
  single owner-scoped state a deployment rather than a project.
- The post-`up` reminder to run `farrow hosts install --yes` is printed as a
  warning line.
- Ambiguous `farrow ssh`/`farrow exec` invocations without `--` now warn once
  when the first token is treated as a remote command; the existing
  compatibility grammar and exit status remain unchanged.
- Installer and development builds retain at most three verified release
  directories by default; set `FARROW_INSTALL_KEEP=N` to choose another bound
  or `FARROW_INSTALL_KEEP=0` to disable pruning.
- The presentation environment variables `FARROW_OUTPUT` and `FARROW_VERBOSE`
  are read directly; Viper and its nine transitive modules are gone from the
  dependency graph and the shipped license inventory.
- Retired the unreachable user-NAT setup branches and four dead APIs.
- Application releases are built and checksummed by GitHub Actions without a
  separate release-signature or provenance bundle; guest-image Catalog
  Minisign verification remains unchanged.

## [0.1.0]

First public, pre-1.0 developer release of the native Go/QEMU runtime for
Pigsty-compatible local labs.

### Added

- One Pigsty-compatible inventory is both the VM specification and the Pigsty
  deployment inventory; there is no second project format.
- One owner-scoped deployment with fixed-IP additive scale-out on macOS and
  Linux, and explicit drift, recreate, and deletion boundaries.
- QMP plus full process identity, atomic state, transaction journals, and
  manifest-scoped host networking, so lifecycle recovery is fail-closed.
- Signed image catalogs with SHA-256 and qcow2 verification, immutable upstream
  fallback, local imports, and bounded cache pruning.
- Static image repositories: a source-controlled `repo.yaml`, a generated
  schema-3 `catalog.json`, immutable artifact names, and
  `farrow repo scan/build/verify`.
- The signed public repository at `https://repo.pigsty.cc/farrow` is the default
  catalog and artifact mirror; `--repo` and `FARROW_REPO` remain explicit
  overrides, and immutable distribution upstreams remain the fallback.
- Image selection by family, channel, exact release, or numeric version prefix,
  so a Pigsty inventory can keep `vm_image: el9` and pick the newest matching
  9.x build with `vm_version`.
- Interrupted image downloads resume from the staged bytes instead of
  restarting, and are bounded by an inactivity watchdog rather than an overall
  deadline, so a slow link finishes rather than failing partway.
- Release assets: four platform archives, amd64/arm64 DEB and RPM packages,
  dependency licenses, SPDX SBOMs, checksums, a Homebrew formula, a user-scoped
  installer, and keyless Sigstore signature and provenance bundles.

### Fixed

- Inventory parsing now rejects duplicate mapping keys, expands YAML merge keys
  with Ansible-compatible precedence, and refuses alias cycles instead of
  silently resolving a different deployment from the same Pigsty inventory.
- Process birth identity is numeric and independent of locale/timezone; matching
  pre-release development state migrates in place, interrupted QEMU starts are
  adopted only from QMP/UUID/pidfile/invocation proof, and failed starts perform
  bounded compensation.
- Selected `status`, `ssh`, `start`, `stop`, and `restart` operations no longer
  fail because an unrelated peer is degraded; whole-deployment operations retain
  full identity auditing.
- The release installer refuses a missing or invalid Sigstore bundle when
  Cosign is available, and explains that pre-1.0 GitHub pre-releases require an
  explicit `FARROW_VERSION`.

[Unreleased]: https://github.com/pgsty/barn/compare/v0.8.0...HEAD
[0.8.0]: https://github.com/pgsty/barn/releases/tag/v0.8.0
[0.7.0]: https://github.com/pgsty/barn/releases/tag/v0.7.0
[0.6.0]: https://github.com/pgsty/barn/releases/tag/v0.6.0
[0.5.0]: https://github.com/pgsty/barn/releases/tag/v0.5.0
[0.4.0]: https://github.com/pgsty/barn/releases/tag/v0.4.0
[0.3.0]: https://github.com/pgsty/barn/releases/tag/v0.3.0
[0.2.0]: https://github.com/pgsty/barn/releases/tag/v0.2.0
[0.1.0]: https://github.com/pgsty/barn/releases/tag/v0.1.0
