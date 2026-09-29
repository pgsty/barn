# macOS virtual machines

`barn mac` creates and runs macOS 27 virtual machines on an Apple Silicon Mac
running macOS 27 or later. Each machine is a clean, disposable macOS with an
administrator account, passwordless sudo, pinned SSH keys and a fixed address,
built for testing and building software. It uses Apple's Virtualization and
DiskImageKit frameworks directly.

Mac machines are independent of the Linux lab: they never read `barn.yml`,
never join the Pigsty inventory, and keep every file under `$BARN_HOME/mac`.
Linux `destroy` and `purge` leave them alone.

## Install

`barn mac` needs the native Mac component, `Barn Mac.app`, next to the
`barn` it serves. Releases will include it once it is signed and notarized
by Apple; until then, build both from a source checkout with Xcode 27:

```sh
make mac-build
export PATH="$PWD/bin/mac:$PATH"
barn mac doctor      # checks macOS, the component and free disk space
```

## Quick start

```sh
barn mac up          # create mac1 and wait until SSH works
barn mac open        # show its desktop
barn mac ssh         # open a shell in it
barn mac stop        # shut it down
```

The first `up` on a Mac needs the macOS restore image. Barn downloads it only
from Apple (`updates.cdn-apple.com`, about 27 GB) after showing what it will
download and asking, or takes one you already have with `--ipsw`. The download
resumes after an interruption and is verified against Apple's published SHA-256.
Barn then installs macOS once into an unbooted *base* (about 20 minutes).
Every machine is a copy-on-write clone of that base, so later machines start in
seconds and use disk only for what they change.

```sh
barn mac up --yes                                  # scripts: accept the download
barn mac up --ipsw ~/Downloads/UniversalMac_27.0_26A428_Restore.ipsw
barn mac setup                                     # prepare the base ahead of time
```

A local IPSW on the same APFS volume is cloned into the cache without copying
its blocks; on another volume it is verified and used where it is.

## Machines

Machines have names: lowercase letters, digits and inner hyphens, starting with
a letter. The default is `mac1`. A command without a name acts on the only
machine, or on `mac1`, and asks you to choose when that is ambiguous.

```sh
barn mac up dev --cpu 8 --memory 16G --share ~/src
barn mac up build --user ci --disk 200G
barn mac ls
```

Defaults are 4 CPUs, 8 GiB of memory and a 100 GiB disk, allocated only as it
is used. Another `--disk` size prepares one more base of the same macOS once
(about 20 minutes); the default base stays as it is. Apple allows two macOS virtual machines to run at a time on one Mac,
including those of other tools and macOS installation itself; you can create
more machines and start any two. When the limit is reached, Barn names the
running machines to stop instead of stopping anything itself.

| Command | What it does |
|---|---|
| `mac ls` | Every machine: state, address, SSH, macOS version, CPUs, memory, disk, shared folders. Creates and starts nothing. |
| `mac up [name]` | Create the machine if needed, start it and wait for SSH and sudo. Creation options apply only to a new machine; an existing machine refuses options that differ and names the command to use. With two machines already running, a new one is refused before anything is created. |
| `mac start [name...] [--all]` | Start existing machines. `--recovery` boots macOS Recovery with its desktop shown. |
| `mac stop [name...] [--all]` | Shut down normally through macOS. A machine still running after two minutes is powered off, and the result says so. `--force` powers off at once. |
| `mac restart [name]` | Stop and start again, applying configuration changes. |
| `mac open [name]` | Show the desktop, starting the machine first if needed. |
| `mac ssh [name] [-- cmd]` | Interactive shell, or a command line for the guest shell. |
| `mac exec [name] -- cmd args…` | Run a command, keeping argument boundaries and passing the exit status through. `--json` returns stdout, stderr and the exit code. |
| `mac configure name …` | Change CPUs, memory, shared folders, the network (while stopped) or clipboard sharing (anytime). |
| `mac recreate name [--update]` | Replace the machine with a fresh macOS of the same name, account, resources, disk size, folders and address. `--update` uses the default base's macOS; for another disk size, `setup --disk` prepares it first. |
| `mac destroy name…` | Delete machines: disk, keys, password and settings. The shared base is kept. |
| `mac password [name] [--copy]` | Show the login password, or copy it to the clipboard. |
| `mac ssh-config [--install\|--remove]` | Print, install or remove OpenSSH entries for every machine. After `--remove`, lifecycle commands leave `~/.ssh` alone until `--install`. |
| `mac logs [name]` | Recent runtime log: startup, network, shutdown and Virtualization errors. |
| `mac image ls\|update\|prune` | Restore images and bases. `update` prepares the newest macOS 27 from Apple; `prune` lists unused images and deletes them with `--yes`. |
| `mac doctor` | Check macOS, the Mac component, disk space, the base and every machine. |

`recreate` and `destroy` describe what they delete and ask you to type the
command name; `--force` confirms without a terminal. Sizes are binary:
`16G`, `16GiB` and `16GB` all mean 16 GiB, and a byte count also works; memory
is a whole number of MiB. Every command supports `--json`, and long
operations show progress on stderr with the same display as the Linux commands.

## Desktop, clipboard and shared folders

`mac open` shows the machine in a native window sized to your screen; resizing
the window changes the guest resolution, and **View → Enter Full Screen** works
as usual. Closing the window keeps the machine running in the background.
**Machine → Restart…** and **Machine → Shut Down…** act on the guest;
**Barn Mac → Quit Barn Mac…** asks whether to keep the machine running or
shut it down. Logging out, restarting or shutting down the Mac shuts running
machines down normally, powering off any that take more than a minute, and is
never held up. Keyboard shortcuts go to the guest while its window is focused.

The clipboard follows focus: plain text you copied on the host is available in
the guest when you click into its window, and text copied in the guest comes
back when you switch to another app. It travels over the machine's pinned SSH
connection; no agent is installed in the guest. Items that password managers
mark as concealed never leave the host. Turn it off per machine with
`barn mac configure NAME --clipboard off`, or for the session with
**Machine → Share Clipboard**.

Shared folders appear in the guest under `/Volumes/My Shared Files/<name>`:

```sh
barn mac up dev --share ~/src --share docs=~/Documents:ro
barn mac stop dev && barn mac configure dev --share data=/Volumes/Work/data --unshare docs
```

A share is a real host directory (not a symlink), read-write unless marked
`:ro`. Barn never creates or deletes shared directories. macOS guests can
serve stale file contents for a while after the host changes a file; use SSH
or `exec` when you need an immediately consistent view.

## SSH

Every machine has its own SSH key pair and a host key pinned on first contact.
`mac ssh` and `mac exec` always use them, never your personal keys or
`~/.ssh/config`. After the first machine is ready, Barn adds one marked
Include to `~/.ssh/config`, so `ssh mac1`, `scp`, `rsync` and editors with
Remote-SSH reach each machine by name. `up`, `recreate`, `configure` and
`destroy` keep those entries current; `mac ssh-config --remove` removes only
what Barn added and keeps it out until `mac ssh-config --install`. A name
that `~/.ssh/config` already uses for another host stays yours: that
machine's entry answers only to its address, and Barn says so. A config
managed by a dotfile tool through a link is never edited; Barn prints the
Include line to add instead.

macOS Local Network privacy blocks third-party programs from the machines'
private networks unless their app is allowed under **System Settings → Privacy &
Security → Local Network**; the symptom is "No route to host". Barn connects
through Apple's own `/usr/bin/nc` and `/usr/bin/ssh`, which are exempt, so
`mac ssh`, `mac exec` and plain `/usr/bin/ssh mac1` always work.

## Accounts and passwords

The administrator account defaults to your macOS user name; choose another with
`--user` when creating a machine. Apple's first-boot provisioning creates it,
turns on automatic desktop login and Remote Login. Barn then installs the SSH
public key, passwordless sudo and the machine's host name, and turns off SSH
password login. No Apple Account, host password or personal key is copied into
the guest.

The login password is random per machine and needed only for the lock screen
and administrator prompts in the desktop. It is stored in the machine's
directory, readable only by you, next to its SSH private key; that key already
grants passwordless sudo in the guest, so a separate store would not protect
anything more. Show it with `barn mac password`, or copy it with `--copy`.

## Network

Each machine has its own private network, created inside its runner process
when it starts and gone when it stops. The Mac is the gateway at `.1`; the
machine always receives `.10` through a DHCP reservation for its MAC address.
The first machine uses `10.10.20.0/24`, later ones the next free /24 in
`10.10.20.0`–`10.10.59.0`, avoiding every route on the host: your LAN, VPNs and
the Linux lab. `--subnet` chooses one explicitly.

Machines reach the internet through NAT and the host through its gateway
address. They are isolated from each other and are not exposed on your LAN.
Nothing runs as root: the private network needs no helper, daemon or
administrator prompt. If a VPN later claims a machine's subnet, `start` stops
before booting and suggests `barn mac configure NAME --subnet auto`.

First-contact SSH trust comes from this isolation: only the machine can answer
on its private network, so the host key it presents on first boot is pinned
and required from then on.

## Storage

```
$BARN_HOME/mac/
  config.json                      installation identity and default base
  images/ipsw/<build>.ipsw         Apple restore images (resumable .partial while downloading)
  images/base/<id>/                read-only, unbooted macOS bases
  slots/<name>/state.json          one machine's record
  slots/<name>/disk.asif           its copy-on-write disk layer over the base
  slots/<name>/id_ed25519          its SSH key; known_hosts pins its host key
  slots/<name>/password            its login password (0600)
```

Runtime sockets live in a short private directory under `/tmp`. `mac ls` and
`mac image ls` report allocated blocks; APFS clones share blocks, so these
numbers are not exclusive usage and should not be added up. `mac image prune`
lists bases that no machine uses and that are not the default, and deletes them
with `--yes`; `--installers` adds downloaded restore images.

Updates are explicit. `mac image update` asks Apple for the newest macOS 27 and
prepares it as the default base; existing machines keep theirs until
`mac recreate NAME --update`. `up` and `start` never change a machine's macOS.

## Requirements and limits

- Apple Silicon, macOS 27 or later, and a logged-in desktop session.
  Virtualization.framework needs the login keychain unlocked.
- Guests run macOS 27; older macOS versions are not supported as guests.
- Apple allows two running macOS virtual machines per Mac.
- Apple Account sign-in inside a virtual machine is unreliable, and USB
  devices, snapshots and suspending a machine are not supported.
- The Mac component is built from source until a release includes a signed,
  notarized one (see [Install](#install)); `barn mac doctor` explains a
  missing or mismatched component.

`make mac-native-test` runs the native tests; `go test ./internal/macvm
./cmd/barn` covers the CLI. Live acceptance evidence is recorded in
[the implementation log](mac-implementation-log.md).
