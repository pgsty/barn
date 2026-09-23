# Farrow

Farrow turns one Pigsty-compatible Ansible inventory into one fixed-IP local
QEMU deployment on macOS or Linux. The inventory you hand to Pigsty is the same
file that describes the virtual machines, so there is no second project format
to keep in sync.

Authoritative documentation: <https://farrow.pgsty.com/>

```bash
farrow up            # prepare the host and start your first VM
farrow ssh           # connect
```

On first use in a terminal, `up` creates a one-node `farrow.yml` if no inventory
or applied deployment exists. To customize it first, run `farrow init` and edit
the file; use `farrow init full` for the four-node template.

## What it is

- **One inventory, one deployment.** `vm_*` host variables describe the guests;
  everything else in the file stays opaque and is passed through to Pigsty
  untouched. There is no separate VM manifest.
- **Fixed IPs, not DHCP.** Nodes get the addresses the inventory names, on a
  host-global private network Farrow installs once. `10.10.10.10` is
  `10.10.10.10` across reboots and recreates.
- **Declarative, but never surprising.** `farrow plan` shows the difference
  between the inventory and the applied state. Removing a host from the file
  never destroys a machine — deletion is always an explicit `farrow destroy`.
- **Ubuntu 24.04 by default.** New inventories use `u24:stable`; set
  `vm_image` explicitly to choose another supported distribution.
- **Verified images.** Guest images come from a signed catalog with SHA-256,
  qcow2, and virtual-size verification on every fetch. The selected
  repository supplies the final image bytes; immutable upstream URLs remain
  build-provenance markers.
- **Recoverable lifecycle.** Repeating `up` continues unfinished work and keeps
  successful nodes available. QMP/process identity, image digests, and disk
  ownership are still verified before changing resources.

Farrow is a local development-lab runtime. It is not a cluster manager, not a
cloud provisioner, and not a container runtime.

`plan` reads local configuration and catalog data without requiring host setup.
It shows exact images, resources, and disk effects; `up` checks host capabilities
before applying changes. Starting commands also refresh the Farrow-managed
hosts and SSH entries inside running guests. `--no-wait` skips readiness and
that guest refresh; a later `up` completes them.

Fast checks stay quiet. Longer operations share one live progress area with
elapsed time, download bytes/speed/ETA, and individual node readiness. A
successful start ends with a short result and a connection command. Partial
starts list usable and pending nodes, group repeated errors, and give a retry
command that retains the inventory path. `status` keeps the detailed table;
`--verbose` exposes diagnostic detail and time spent in each foreground stage,
including waits and prompts. Redirected output uses occasional plain
progress lines on stderr, and `--json`/`--yaml` keep stdout machine-readable.
Bare `farrow` shows the next few actions; `farrow --help` is the full reference.

`up` can install missing host tools and restore an inactive, verified Farrow
network during an interactive session. The hosts-file helper is installed only
when `farrow hosts install` needs it. A fresh, untouched default template can use
an available subnet; its original is saved as `farrow.yml.before-network-change`.
Explicit `-f` files, edited templates, and existing deployments keep their subnet.
For unattended first setup, run `farrow setup --yes` explicitly.

Interrupted image downloads retry and resume automatically. Official image
repositories can fail over to their counterpart; custom repositories stay
exclusive. Writable cache files are made read-only only after verification.
Damaged, unreferenced cache files are preserved as `.corrupt-<timestamp>` before
replacement; referenced backing files stay in place. Ctrl-C preserves completed
work and resumable downloads, so the same command can continue later.
An offline node-prepare failure can also be retried with `up`: recognized
uncommitted artifacts are cleaned first, while committed nodes and unexpected
files are preserved.

Guest readiness requires working management SSH and the expected login identity.
Data disks, shared directories, guest hostnames, node-to-node SSH, and private
network checks run independently with bounded waits. Unavailable features produce
`ready · with limitations` and exit 0, while other features remain usable. Each
failed disk or share is named; an unavailable data disk is never reported as
mounted. Internet access is optional, so offline labs can finish setup.

Repeating `farrow up [node...]` retries unfinished guest setup and updates old
guest helpers in place. Running VMs keep their process and root disk; healthy
setup stages are skipped. `--no-wait` skips these guest checks as well as waiting
for readiness. No separate repair command is needed.

Each node gets a 128 GiB `/data` disk unless `vm_disks` says otherwise;
`vm_disks: []` gives a node none. `farrow plan` lists every node's data disks.

Data disks are disposable test storage. Working filesystems are reused. If a
configured data disk has no recognizable filesystem, or cannot mount and a
filesystem check confirms damage, `up` resets it to an empty filesystem and
reports that previous data was discarded. **This can erase a persistent data
disk too:** `persistent` retains it across destroy/recreate, not after filesystem
failure. A missing device, failed probe, busy mount, or underlying I/O error is
reported without formatting; other guest features remain available. Root disks
and host shared directories are never reset by this recovery.

Writable shares use the guest user's identity for newly created files. If the
host directory does not permit guest writes, Farrow mounts it read-only and
reports the limitation. It does not recursively change host project ownership.
After fixing access, repeat `farrow up` to retry the writable mount.

Host-side share failures are different from a guest mount limitation: QEMU must
open each configured source before that node can start. `up` and `start` keep
independent nodes progressing if one source is missing. Restore the original
directory or its mount and retry the affected node; Farrow does not create an
empty source. Restart/reload/recreate check the selected sources before stopping
or deleting existing VMs.

**Known macOS limitation:** QEMU's 9p backend cannot reopen the directory
descriptor used by Farrow on the tested macOS/QEMU 11.1.1 host. Such nodes cannot
start; the CLI now diagnoses the limitation without bypassing path identity
checks. Omit `vm_shares` for new macOS labs. Changing an existing node's shares
requires explicit recreation and replaces its root disk, so preserve needed
data first. Linux sharing and macOS support have separate acceptance gates.

If another process takes an automatically allocated management SSH port while
a VM is stopped, the next start chooses another free port and updates VM state
and SSH aliases together. Explicit application forwards keep their assigned
ports. Running VMs keep their port and process.

SSH trust is keyed by VM instance UUID, allowing a recreated VM to reuse an SSH
port without inheriting its predecessor's host key. Changed keys for the same
instance still fail verification. Existing labs adopt this namespace on their
first connection with the new binary; legacy port entries remain until destroy.
SSH aliases, guest hostname refresh, and diagnostic event writes are optional
after a successful lifecycle operation: their failures produce warnings with a
retry command, while the VM result remains successful. Explicit commands such
as `ssh-config --install` still report their own failures. Lifecycle integration
waits have separate budgets: 2 seconds for the SSH configuration snapshot,
5 seconds for guest metadata, and 1 second for diagnostic events. A timeout
preserves the VM result and gives a warning; a later `up` retries the refresh.

## Requirements

| Host | Accelerator | Minimum QEMU | Tier |
|---|---|---|---|
| macOS arm64 | HVF | 8.2.1 | 1 |
| macOS amd64 | HVF | 8.2.1 | 2 |
| Linux amd64 | KVM | 6.2 | 1 |
| Linux arm64 | KVM | 6.2 | 2 |

Tier 1 is the dated, natively validated matrix; tier 2 is cross-built and
package-verified against the narrower status published at
<https://farrow.pgsty.com/docs/about/status/>. You also need `qemu-img`, UEFI
firmware for arm64 guests, and an OpenSSH client.

`farrow doctor` reports host dependencies, persisted state, and network setup;
`farrow status` audits live QMP/process identity and safely converges interrupted
transitions. `farrow setup` installs what it can and asks for administrator
access only when a host transaction genuinely needs it.

## Install

Farrow is pre-1.0. A successful build from source is not evidence of a tagged
release, a published package, or a supported guest image.

Version 0.8.0 improves startup recovery and refreshes the operating-system
catalog. See the [0.8.0 notes](.github/releases/0.8.0.md) for the verified paths
and remaining limitations.

Download `install.sh`, `farrow.rb`, or the native package from the
[Farrow 0.8.0 release](https://github.com/pgsty/farrow/releases/tag/v0.8.0).

```bash
# From a release: user-scoped, no sudo, checksum-verified
curl -fLO https://github.com/pgsty/farrow/releases/download/v0.8.0/install.sh
chmod +x install.sh
FARROW_VERSION=0.8.0 ./install.sh

# Homebrew formula (shipped as a release asset)
brew install --formula ./farrow.rb

# Debian/Ubuntu and RHEL-family packages are release assets too
sudo apt install ./farrow_<version>_linux_amd64.deb
sudo dnf install ./farrow_<version>_linux_amd64.rpm
```

GitHub does not expose pre-1.0 prereleases through `/releases/latest`, so
`FARROW_VERSION` is required until a stable release exists. The installer
always verifies the selected archive against the `checksums.txt` produced by
the GitHub release workflow.

When upgrading an existing Debian lab whose inventory omits `vm_image`, set
`vm_image: d13` in `all.vars` to keep that choice. Upgrading Farrow does not
replace healthy VM disks; `farrow plan` shows configuration changes before you
apply them. Since 0.7.0, `up` can reset damaged data filesystems, including
persistent disks, as described above. The policy is unchanged in 0.8.0.

From source:

```bash
make build
export PATH="$PWD/bin:$PATH"
```

## Everyday commands

```bash
farrow init full             # a four-node inventory instead of one
farrow validate              # parse and resolve without touching anything
farrow plan                  # what would change, and why
farrow up                    # converge
farrow update                # fetch and activate a newer image catalog
farrow status                # audit/converge selected runtime state, from anywhere
farrow ssh meta -- uptime    # run something in a guest
farrow hosts install --yes   # publish node names into the host hosts file
farrow destroy               # explicit, confirmed teardown
farrow purge                 # no-confirmation disposal; images/network remain
```

Every command accepts `--json` or `--yaml` for stable machine-readable output.
Presentation flags never change an exit status.

`farrow exec meta -- command arg...` preserves argument boundaries, including
quoted spaces and empty values. Use `sh -c 'script'` for shell expressions.
The single-string shorthand (`farrow exec meta -- 'uptime; id'`) and ordinary
`farrow ssh` shell semantics remain available.

### Exit codes and errors

| Code | `error` | Meaning |
|---|---|---|
| 0 | | success |
| 1 | `runtime` | the operation ran and failed (a tool, download, or guest failed) |
| 2 | `usage` | the command line or inventory is wrong |
| 3 | `capability` | the host lacks a tool, the Farrow network, or a privilege |
| 4 | `conflict` | the deployment's current state forbids it, or another farrow command holds it |
| 5 | `partial` | some nodes succeeded and some failed |
| 6 | `resource` | a host address, port, subnet, or disk is taken |
| 7 | `integrity` | a verified digest, signature, identity, or ownership did not match |
| 130 | `cancelled` | interrupted by `SIGINT`/`SIGTERM`, or a confirmation was declined |

A failure prints `error: <message>` on stderr, the failing program's last
stderr lines when an external tool failed, and a `next:` line when there is
one clear thing to do. With `--json`, stdout carries the same failure:

```json
{"error": "conflict", "reason": "node_not_running", "message": "node meta is not running",
 "next": "farrow start meta", "operation_id": "…"}
```

`reason` is a stable identifier for automation and is present only where it
matters. `command` (`name`, `argv`, `exit_status`, `signal`, `timed_out`,
`stderr`) describes an external program that failed. Lifecycle and setup
failures keep their full result document, as before.

`ssh`, `exec`, and a single-node `provision` pass the guest command's own exit
status through unchanged, so their non-zero codes are the remote program's,
not one of the categories above. Farrow's own failures on those paths still use
the table.

## State

Everything Farrow owns lives under `$FARROW_HOME` (default `~/.farrow`): the
applied deployment, per-node state and journals, the verified image cache, and
the signed catalog. Applied-state commands therefore work from any directory.
Outside that tree Farrow writes only three marked things: the host network
(`farrow network uninstall`), the hosts-file block (`farrow hosts uninstall`),
and `~/.ssh/farrow_config` with one `Include` line in `~/.ssh/config`
(`farrow ssh-config --remove`). When `~/.ssh/config` is a symlink or hard link,
as dotfile managers create, Farrow leaves it alone and asks you to add the
`Include` line once.

The image catalog ships inside each Farrow release, and Farrow never refreshes
it implicitly. `farrow update` fetches the configured repository's catalog;
`farrow image sync` activates an exact URL or file. Ordinary commands use the
active local catalog. The default repository is `https://repo.pigsty.io/farrow`;
`--mirror` selects `https://repo.pigsty.cc/farrow`, while an explicit `--repo`
overrides `--mirror`, `FARROW_REPO`, and the default.

Optional integration warnings appear in structured lifecycle results as
`warnings` with `code`, `message`, `detail`, and an optional `next` command.
Per-node `ready: true` records a successful guest readiness check in that startup
operation; ordinary `status` reports runtime state without claiming SSH readiness.
Per-node `warnings` contain `{stage, detail}` for limited guest features; these
survive CLI invocations and are refreshed on the next readiness check. These
limitations use a disposable cache separate from core VM state, so diagnostic
metadata does not prevent rolling back to 0.6.0. Per-node
`repairs` describe automatic actions taken in that operation, such as a changed
SSH port or a reset data disk. Automation that requires every configured guest feature should check
`nodes[].warnings` as well as the exit code.

Setup and lifecycle retries reuse one `operation_id`. After a failed first setup,
`farrow logs --source events --json` works even without deployment state. Event
files are bounded; setup traces contain phase/category summaries, while the
command output retains the actual cause. Missing deployment public keys are
derived from the original private key during startup. If that private key is
lost, restore it from backup; an existing VM is never silently given a new key.

## Development

```bash
make build     # build into ./bin
make test      # unit tests
make check     # the complete source gate CI runs
```

`make check` covers module integrity, shell syntax, unit and race tests, `vet`,
Staticcheck, `govulncheck`, cross-compilation for all four targets, image and
installer trust boundaries, and the dependency-license inventory. See
[CONTRIBUTING.md](CONTRIBUTING.md).

## Security

Farrow asks for root when installing Linux host packages, setting up the
private network, or publishing node names into the system hosts file through
a separate helper binary. See
[SECURITY.md](SECURITY.md) for the privilege boundary and how to report a
vulnerability.

## License

Apache-2.0. Release tooling reconstructs dependency license texts from the
exact module versions pinned by `go.mod`, and ships them inside every archive
and package.
