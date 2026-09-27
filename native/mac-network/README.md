# Farrow Mac network helper

`farrow-mac-network` owns one vmnet shared/NAT network for a single Farrow Mac
installation and UID. The ordinary VM runner receives a connected `SOCK_DGRAM`
descriptor and attaches it using `VZFileHandleNetworkDeviceAttachment`. There is
no dependency on the Linux `socket_vmnet` service or its stream protocol.

The helper runs as root. It deliberately does **not** embed the restricted
`com.apple.vm.networking` entitlement. Apple describes that entitlement as
allowing virtualization software to manage networks without escalating to root;
an ad-hoc signature is not evidence of entitlement approval.

## Build and administrator preparation

Requires Apple Silicon, macOS 27, and Xcode 27 to build:

```sh
bash native/mac-network/build.sh /absolute/output
/absolute/output/farrow-mac-network validate-config --config /absolute/network.json
sudo /bin/bash native/mac-network/install.sh --binary /absolute/output/farrow-mac-network --config /absolute/network.json
```

The installer copies and validates a root-owned configuration snapshot, installs
the binary under `/Library/PrivilegedHelperTools`, and installs a launch daemon.
The VM process itself stays under the invoking user's account. Routine status
and attachment require no sudo. No sudoers rule is installed. Production builds
should supply `FARROW_CODESIGN_IDENTITY` as part of the existing signing pipeline;
local ad-hoc signing establishes no distribution/notarization claim.

Configuration schema (all five top-level fields are required; no others):

```json
{
  "schema_version": 1,
  "uid": 501,
  "installation_id": "f60e8690-61aa-4a6f-b044-2a1ff03b4b2f",
  "subnet": "10.10.20.0/24",
  "slots": [
    {"name": "mac1", "mac": "02:6a:37:00:00:01", "ip": "10.10.20.10"},
    {"name": "mac2", "mac": "02:6a:37:00:00:02", "ip": "10.10.20.11"}
  ]
}
```

These example MACs must be replaced by generated persistent slot MACs. Go owns
route/VPN conflict selection and persists the subnet before generating this
config. The helper rejects public subnets, non-/24 networks, duplicate or
multicast MACs, changed slot IP offsets, and unknown configuration fields.
Network creation itself can still reject a conflicting subnet; it never falls
back to a new subnet behind the CLI's back.

The persisted subnet uses its canonical `.0/24` network address. The native
`vmnet_network_configuration_set_ipv4_subnet` call receives the `.1` host
gateway: Golden Gate accepts `.0` during configuration but rejects it when DHCP
actually starts ("can't have the gateway at the subnet address"). This is why
configuration-only probes do not establish runtime networking support.

`validate-config --config FILE` emits JSON with the derived socket and service
label. The socket is `/var/run/farrow-mac/<uid>-<lowercase-uuid>.sock`, and the
service is `io.pgsty.farrow.mac-network.<uid>.<lowercase-uuid>`. Installation state
is root-owned under `/Library/Application Support/Farrow/mac-network/<uid>/<uuid>`.
Different `$FARROW_HOME` roots use different installation UUIDs; they must also
select non-conflicting subnets. Config changes and live helper upgrades are
refused rather than silently changing running VM connectivity.

Readiness is an authenticated RPC, not merely socket existence:

```sh
/Library/PrivilegedHelperTools/io.pgsty.farrow.mac-network status --socket /var/run/farrow-mac/501-UUID.sock
```

Status JSON reports protocol, UID, installation UUID, subnet, PID, two
attachment booleans, `network_active`, and active-slot packet counters.
`ok: true` proves a root helper has started the configured vmnet network's
anchor interface; it does **not** prove a guest got DHCP, SSH, DNS or internet service.
The CLI should compare UID, UUID and subnet to its own saved state.

`--version` and status both return `build_id`, a SHA-256 of the native helper's
source, build recipe and maintenance script. This identifies the running code
even when another environment has already replaced the shared executable.
Explicit `setup` compares this identity and upgrades only while the network is
idle; a compatible older helper may continue serving daily operations with an
update notice. The identifier does not claim a signing identity or notarization.

The installer also accepts `--uninstall --binary FILE --config FILE`. Removal
requires an exact match with the root-owned installation configuration and the
same maintenance/repair locks as upgrades. Administrative transactions have a
separate per-installation lock, which also excludes simultaneous install/remove
operations after the old daemon exits. Only the selected LaunchDaemon, root
configuration and socket are removed. Shared helper executable, diagnostic log
and lock inodes remain; removing a held lock inode would defeat mutual exclusion.
User VM/base/configuration files are untouched, so `setup` restores the network.
The Go caller additionally holds both slot operation locks, shared state and
existing runner locks before requesting this privileged removal.

`slot_sources` reports the two ordered slots with `slot` (`mac1` or `mac2`),
`connected`, `reserved_ip`, `reserved_mac`, and `observed_source`. The last field
is `null` until that current connection actually sends a valid IPv4 packet or
Ethernet/IPv4 ARP request/reply whose source IP and Ethernet source MAC match
the slot reservation. ARP also requires its sender hardware address to match;
IPv4 requires a complete header, valid length and header checksum. DHCP offers,
reservations, zero-source ARP probes and attachment alone establish no evidence.
An observed value is `{ "ip": "10.10.20.10", "mac": "02:...", "protocol":
"ipv4" }` (or `"arp"`). Closing the connection immediately clears its evidence,
including while asynchronous vmnet teardown is still pending. Each new
connection begins unobserved. The helper does not query other host interfaces
or the host ARP table. This slot association supplements SSH host-key capture
and pinning; it is not cryptographic authentication of an SSH server.

This avoids macOS 27's intentional filtering of ARP/routing topology APIs.
Apple's `com.apple.developer.networking.topology-observation` capability is a
restricted entitlement requiring a provisioning profile, so an ad-hoc claim
is not a supported solution. No topology entitlement is used by this helper.

For an isolated, temporary privileged capability check, `probe --config FILE`
creates the actual network, starts one interface, tests same-process network
serialization, stops the interface and exits. It does not create a VM. Do not
probe an already active installation's subnet; use a separate conflict-checked
configuration. A successful probe is not cross-privilege serialization proof.

## Runner integration and wire protocol

Compile `client.c` and import `client.h` from the runner's Swift bridging header.
`farrow_mac_network_connect(path, slot, &controlFD, error, errorSize)` returns the
data descriptor. Keep `controlFD` and the VZ data `FileHandle` alive while the VM
runs. Closing the control socket ends only that slot's network interface. The
client verifies the server's real peer UID is root, the received FD is a
connected Unix datagram socket, and the reply contains exactly one FD.

The control transport is `AF_UNIX/SOCK_STREAM`; **it is never passed to VZ**.
The complete v1 request is eight bytes: `FMN1`, one slot byte (`1` or `2`), and
three zero bytes. Reply is `FMN1` plus big-endian uint32 status. Zero status
carries exactly one `SCM_RIGHTS` data FD; nonzero carries no FD. Errors use errno
or native vmnet status values (1000+). The control connection carries no further
VM traffic and remains open as the lifetime lease. Slot `0` requests status:
after the successful eight-byte header comes bounded JSON and EOF, without a
descriptor. The configured UID may attach; that UID and root may query status.

The helper creates a single shared-mode vmnet network with both DHCP reservations
and starts a network-only anchor interface before reporting readiness. The
anchor keeps DHCP active when both VM slots are disconnected and drains its
own broadcast frames in bounded batches; it does not create a virtual machine
or consume a macOS VM runtime allowance. On Golden Gate, a repeated real packet
test observed a fixed offer on first activation and a dynamic offer after all
interfaces stopped, so subsequent activations must be verified explicitly.
Each slot owns a separate vmnet interface and
Unix datagram socketpair. Packet paths validate frame size and guest source MAC,
disable offload framing, process at most 64 frames per callback, and drop/count
packets when bounded kernel socket buffers are exhausted. No unbounded packet
queue is allocated. Both socket ends use 64 KiB send/256 KiB receive buffers and
MTU 1500. TCP provides ordinary congestion recovery; packet drops remain visible
in detach logs. The connected FD preserves one Ethernet frame per datagram.

The privileged service accepts no filesystem paths, executables, interface
names, arbitrary subnets, DHCP edits or port forwards from clients. It checks
peer UID, limits pending handshakes to 16, allows one client per slot, and times
out incomplete handshakes. The binary, config and their ancestor paths must be
root-owned and not group/world writable. Socket namespaces have a root-owned
parent; socket access is limited to the configured UID. SIGTERM releases both
interfaces; it does not stop either VM process.

macOS's precise `/private/var/run` system ancestor is accepted with its default
`root:daemon` ownership and `0775` mode; this exception does not apply to the
Farrow socket directory or any other group-writable path. The helper does not
change system directory permissions. `check-path --path DIR` is a read-only
diagnostic for this policy.

An installed launchd registration whose daemon has exited can be repaired by
rerunning the installer. The installer first holds the daemon's exclusive
namespace lock via `repair-lock` and then verifies that launchd reports no PID.
It keeps the lock through bootout and replacement, preventing a scheduled
restart from starting a network between the check and bootout. An active lock,
a live PID, or an unreadable service state prevents repair. The private FIFO
lease releases the repair lock if installation exits or is interrupted.

Healthy-service upgrades acquire a root-only maintenance lease first. It
atomically checks for no connected slots and prevents new attachments until
the lease closes or the idle daemon is replaced. This avoids an `up` racing a
status check and service replacement. Wire request slot byte `3` is reserved
for that administrative lease; it returns only the normal status header, with
no descriptor. An older helper without this capability refuses the upgrade
rather than risking a live connection.

`launchctl bootout` is asynchronous with respect to final service removal. The
installer waits for the old label to disappear before bootstrapping its
replacement; otherwise macOS can report bootstrap EIO while the actual launchd
failure is "Operation already in progress".

## Evidence boundaries and sources

`tests.py` covers malformed privileged input and rejection of an ordinary-user
impersonation server. It never creates an actual vmnet network. True data-plane
acceptance requires the administrator installation, actual two-slot guest DHCP,
host/guest and guest/guest traffic, DNS/NAT, stop/start and release/reconnect.

The official network-object serialization API is documented, but its existence
does not demonstrate that an unentitled user process can import a root-created
object and start a VZ attachment. This implementation intentionally uses the
public root vmnet plus datagram path until that separate experiment succeeds.

- [Apple networking entitlement](https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.vm.networking)
- [Apple file-handle attachment](https://developer.apple.com/documentation/virtualization/vzfilehandlenetworkdeviceattachment)
- [Apple custom vmnet attachment](https://developer.apple.com/documentation/virtualization/vzvmnetnetworkdeviceattachment)
- [Apple network serialization](https://developer.apple.com/documentation/vmnet/vmnet_network_create_with_serialization(_:_:))
- [Apple DHCP reservation](https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_add_dhcp_reservation(_:_:_:))
- [Apple DTS: macOS 27 topology privacy and provisioning](https://developer.apple.com/forums/thread/841958)
- [Apple DTS: VZ guest IP discovery and filtered ARP](https://developer.apple.com/forums/thread/822025?page=2)

The SDK headers are the compile-time contract. UTM's network manager was read as
an architectural reference; no UTM implementation was copied into this helper.
