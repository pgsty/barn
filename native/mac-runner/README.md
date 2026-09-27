# Farrow macOS VM runner

This is the macOS 27 / Apple Silicon process behind `farrow mac`. Go owns download,
cache manifests, slot state, bootstrap SSH and lifecycle policy. This runner owns
Apple restore validation, DiskImageKit disks, Virtualization lifecycle, native GUI,
and Keychain calls. It never reads `farrow.yml` or Linux VM state.

Build with Xcode 27 on Apple Silicon:

```sh
bash native/mac-runner/build.sh
bash native/mac-runner/test.sh
```

The default output is `bin/farrow-mac-runner`. `FARROW_CODESIGN_IDENTITY` selects a
signing identity; development defaults to ad hoc. The entitlement is only
`com.apple.security.virtualization`. This does **not** grant the restricted
`com.apple.vm.networking` entitlement. The network helper is a separate, privileged
component, providing a connected datagram descriptor through SCM_RIGHTS.

## Protocol 1

Commands emit one JSON object on stdout, with `ok: true`, or
`{"ok":false,"error":{"code":"...","message":"..."}}` and exit 1.
Progress is timestamped JSONL on stderr. No password appears in arguments, progress
or persisted metadata. `secret get` is the deliberately private exception to stdout
secret avoidance: its output must be captured by Go, never copied to a log.

| Command | Result / behavior |
|---|---|
| `probe` | Host support, protocol, CPU and physical memory. Does not start a VM. |
| `metadata --ipsw PATH` | Apple validates the local image; requires supported macOS 27. Returns `version`, `build`, `hardware_model_sha256`, `minimum_cpu`, `minimum_memory`. |
| `discover` | Apple latest-supported metadata plus `url`. Fails if latest is not macOS 27; never silently selects another major version. |
| `hardware --path PATH` | Validate a saved hardware model and return its SHA-256 plus current host support, without starting a VM. |
| `restore --ipsw PATH --base DIR` | Creates ASIF disk, auxiliary storage and installer identity; restores using Apple installer, then freezes the stopped, unbooted base. |
| `clone --base DIR --slot DIR` | Creates overlay, an auxiliary-storage copy, and fresh machine ID. Existing image files are never overwritten. |
| `run --base DIR --slot DIR --socket PATH --instance UUID --mac MAC --network-socket PATH --network-slot 1\|2` | Stays alive as VM owner; emits startup status after VZ successfully starts. |
| `rpc --socket PATH --instance UUID --method status\|open\|stop [--force]` | Checks instance identity; returns status or sends stop/open. |
| `secret set\|get\|delete --service farrow.mac.ROOT_HASH --account UUID` | Keychain generic password scoped to the root and instance. `set` reads `{"password":"..."}` from stdin. |

`restore` accepts `--cpu` (default 4), `--memory` in bytes (default 8589934592), and
`--disk-size` in bytes (default 107374182400). `run` accepts the same CPU/memory
options. Diagnostic `run --nat` replaces the network helper attachment; its output
must never be counted as fixed-address network acceptance.

For first boot, pass exactly one line to `run` stdin then close it:

```json
{"provision":true,"username":"example","password":"INSTANCE_SECRET","full_name":"Example"}
```

Normal restart passes empty stdin or `{}`. The official provisioning options enable
Remote Login and automatic desktop login. They only act on the first boot following
restore. Go must persist bootstrap phase and reuse the stored secret when recovering
a partial initial setup; resending options does not reconfigure an initialized OS.

## State and process boundaries

A base contains `disk.asif`, `hardware-model.bin`, `auxiliary-storage.bin`,
`machine-id.bin` (installer identity), and `base.json`. The last file is published
only after restore completes with VM state stopped. `first_boot:false`, `ready:true`
are required to clone/run; image files are mode 0400. Existing `metadata.json`, `manifest.json`,
`restore.log`, and `restore.lock` are permitted before restore. Other existing files
cause refusal. Failed restore files are retained for diagnosis; Go cleans only its
known incomplete artifacts under its preparation lock before retrying.

A slot contains `disk.asif`, `auxiliary-storage.bin`, `machine-id.bin`, and
`runner.lock`. Go may create its state/SSH files first. Clone and run hold the same
nonblocking flock, so they cannot modify an active disk. Parent and overlay UUIDs
are validated when DiskImageKit reconstructs the stack. Changing the base file
behind a live slot is unsupported.

The runner socket is mode 0600, accepts only its own UID, and every request must
match `instance`. RPC is a newline-terminated JSON object:

```json
{"instance":"UUID","method":"status","force":false}
```

Status is `{"ok":true,"instance":"UUID","pid":123,"state":"running","window_visible":false}`.
`running` is VZ execution state, never an SSH readiness assertion. Graceful stop
acknowledges the request and Go polls process/socket state until stopped. A timeout
must not implicitly trigger force. `stop --force` uses VZ stop explicitly.

Go must start `run` detached from its terminal, close stdin after one request, and
redirect stdout/stderr to private runtime logs. The runner also ignores SIGHUP.
`open` creates/reuses a native VZVirtualMachineView window in the owning process.
Closing the window hides it; the VM continues. The guarantee assumes an active
host login session. Host reboot and locked login Keychain are separate conditions.

## Network descriptor contract

The runner connects to the helper using Unix SOCK_STREAM, sends eight bytes
(`FMN1`, slot byte 1 or 2, then three zeros), and reads an eight-byte response
(`FMN1`, big-endian uint32 status). Success is status zero plus exactly one
SCM_RIGHTS descriptor whose SO_TYPE is SOCK_DGRAM. The runner keeps the control
connection open throughout VM life. Helper EOF releases the vmnet interface. It
does not feed QEMU's stream protocol into VZ.

## Validation boundary

`test.sh` exercises empty ASIF parent/overlay assembly, independent machine IDs,
auxiliary copy, preservation of Go state, overwrite refusal, and rejection of a
booted base without starting a VM. It does not prove a macOS restore/clone can
boot. Full acceptance separately requires restore, two fresh-user boots, DHCP,
SSH and sudo, disk isolation, GUI, stop/start, reset and cache reuse.

API references: [Apple guest provisioning](https://developer.apple.com/documentation/virtualization/vzmacguestprovisioningoptions),
[DiskImageKit](https://developer.apple.com/documentation/diskimagekit), and
[VZ file-handle network attachment](https://developer.apple.com/documentation/virtualization/vzfilehandlenetworkdeviceattachment).
