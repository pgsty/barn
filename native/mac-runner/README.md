# Barn macOS VM runner

This is the macOS 27 / Apple Silicon process behind `barn mac`. Go owns
downloads, cache manifests, machine records, SSH provisioning and lifecycle
policy. The runner owns Apple restore validation, DiskImageKit disks, the
Virtualization lifecycle, each machine's private vmnet network, and the native
desktop window with its clipboard sharing. It never reads `barn.yml` or Linux
VM state, and it never needs root.

`Barn.icns` is the Compute Barn brand mark at macOS icon sizes.
`packaging/build-mac-app.sh` installs it into `Contents/Resources` before
signing the app; `Info.plist` selects it for the desktop and Dock.

Build with Xcode 27 on Apple Silicon:

```sh
bash native/mac-runner/build.sh
bash native/mac-runner/test.sh
```

The default output is `bin/barn-mac-runner`. `BARN_CODESIGN_IDENTITY` selects
a signing identity; development defaults to ad hoc. The only entitlement is
`com.apple.security.virtualization`. Creating a private shared-mode vmnet
network needs no other entitlement and no privilege.

## Protocol 2

Commands print one JSON object on stdout: `{"ok":true,...}`, or
`{"ok":false,"error":{"code":"...","message":"..."}}` with exit status 1.
Progress is timestamped JSONL on stderr. No password appears in arguments,
progress or files the runner writes. The CLI refuses a runner whose `probe`
reports another protocol version.

| Command | Result |
|---|---|
| `probe` | Protocol version, host macOS version, virtualization support, CPU and memory. Starts nothing. |
| `metadata --ipsw PATH` | Apple validates a local restore image; returns `version`, `build`, `hardware_model_sha256`, minimum CPU and memory. Only macOS 27 is accepted. |
| `discover` | Apple's newest supported restore image with its `url`; refuses another major version. |
| `hardware --path PATH` | Validate a saved hardware model and report whether this host supports it. |
| `restore --ipsw PATH --base DIR` | Create the ASIF disk, auxiliary storage and identity, install macOS with Apple's installer, then freeze the stopped, unbooted base. |
| `clone --base DIR --slot DIR` | Create a machine's overlay, auxiliary-storage copy and fresh machine identifier. Never overwrites existing files. |
| `run --base DIR --slot DIR --socket PATH --instance UUID --mac MAC --gateway IP --address IP` | Own one running VM. Accepts `--cpu`, `--memory` (bytes) and `--recovery`. Prints its status once the VM runs, then stays alive. |
| `rpc --socket PATH --instance UUID --method status\|open\|stop [--force]` | Diagnostic client for the RPC below. |

`restore` accepts `--cpu` (default 4), `--memory` (default 8589934592) and
`--disk-size` (default 107374182400).

### run

`run` creates the machine's private network before the VM: a shared-mode
vmnet network whose host gateway is `--gateway` (the subnet's `.1`), with one
DHCP reservation giving `--mac` the address `--address` (`.10`). The network
belongs to this process and disappears when it exits. Each runner attaches only its own VM to its own logical network.

stdin carries exactly one JSON line, then closes:

```json
{"name":"mac1","subtitle":"macOS 27.0 · me@10.10.20.10",
 "provision":{"username":"me","password":"INSTANCE_SECRET","full_name":"me"},
 "shares":[{"name":"src","path":"/Users/me/src","readonly":false}],
 "guest":{"ssh":["/usr/bin/ssh","-F","/dev/null","…","me@10.10.20.10"],"known_hosts":"/…/known_hosts"},
 "clipboard":true,"window":false}
```

- `provision` appears only on the first boot. Apple's provisioning creates the
  account, enables Remote Login and automatic desktop login; it acts only on the
  first boot after restore.
- `shares` become one VirtioFS device with the macOS automount tag; the guest
  mounts them under `/Volumes/My Shared Files/<name>`.
- `guest` is how the runner reaches the guest for clipboard sharing and the
  desktop Restart command: the OpenSSH argv up to and including `user@host`,
  pinned to the instance host key. The runner appends only fixed commands
  (`pbcopy`, `pbpaste`, `shutdown -r now`) and waits until `known_hosts` exists.
- `window` shows the desktop as soon as the VM runs; `--recovery` always does.

### RPC

The runner listens on `--socket`, mode 0600, and accepts only its own user,
checked with `getpeereid`; the CLI checks the runner's user the same way.
Requests are one JSON line and must name the instance:

```json
{"instance":"UUID","method":"status","force":false}
```

`status` returns `{"ok":true,"instance":"UUID","pid":123,"state":"running","window_visible":false}`.
`open` shows the desktop. `stop` requests a normal stop; with `force` it powers
the VM off and the runner exits.

## Files and process boundaries

A base holds `disk.asif`, `hardware-model.bin`, `auxiliary-storage.bin`,
`machine-id.bin` (installer identity) and `base.json`. `base.json` is written
only after installation completes with the VM stopped; `clone` and `run`
require `ready:true` and `first_boot:false`. Base files are mode 0400.

A machine directory holds `disk.asif`, `auxiliary-storage.bin`,
`machine-id.bin` and `runner.lock`; Go keeps its record, keys and password
beside them. `clone` and `run` hold the same non-blocking lock, so neither can
touch a running disk, and the CLI proves a machine stopped by taking that lock.
DiskImageKit validates the base and overlay UUIDs when it assembles the stack.

The desktop window uses a display sized to the screen at its pixel density and
reconfigures the guest resolution when resized. Closing it hides it; the VM
keeps running. Quitting asks whether to keep the VM running in the background
or shut it down through `barn mac stop`, which applies the CLI's normal and
forced shutdown policy.
