#!/usr/bin/env python3
"""Unprivileged privileged-input and IPC boundary tests; never starts vmnet."""
import argparse
import array
import copy
import ctypes
import json
import os
import re
from pathlib import Path
import socket
import struct
import subprocess
import tempfile
import threading

parser = argparse.ArgumentParser()
parser.add_argument("--helper", required=True)
parser.add_argument("--workdir", required=True)
args = parser.parse_args()
work = Path(args.workdir).resolve()
work.mkdir(parents=True, exist_ok=True)
source = Path(__file__).resolve().parent
checks = []
version = json.loads(subprocess.check_output([args.helper, "--version"]))
assert re.fullmatch(r"[0-9a-f]{64}", version["build_id"])
checks.append("helper-source-build-identity")
installer_checks = json.loads(subprocess.check_output(["python3", str(source / "test_installer.py"), "--workdir", str(work / "installer-simulation")]))
assert installer_checks["ok"] and installer_checks["real_system_changed"] is False
checks.extend(installer_checks["checks"])

# Exercise the exact parser linked into the root helper. It only reads a
# complete frame; every truncation, unsupported protocol and mismatched identity
# must remain unobserved. These tests need neither vmnet nor elevated privileges.
parser_dylib = work / "source-parser.dylib"
subprocess.run(["xcrun", "clang", "-dynamiclib", "-Wall", "-Wextra", "-Werror", str(source / "source.c"), "-o", str(parser_dylib)], check=True)
observe = ctypes.CDLL(str(parser_dylib)).fmn_observe_source
observe.argtypes = [ctypes.c_char_p, ctypes.c_size_t, ctypes.c_char_p, ctypes.c_char_p]
observe.restype = ctypes.c_int
mac = bytes.fromhex("026a37000001")
ip = socket.inet_aton("10.10.20.10")
ether = bytes([255]) * 6 + mac


def ip_header(options=b"", data=b"payload"):
    head = bytearray(struct.pack("!BBHHHBBH4s4s", 0x45 + len(options) // 4, 0, 20 + len(options) + len(data), 123, 0, 64, 17, 0, ip, socket.inet_aton("10.10.20.1")) + options)
    value = sum(struct.unpack("!" + "H" * (len(head) // 2), head))
    while value >> 16:
        value = (value & 65535) + (value >> 16)
    head[10:12] = struct.pack("!H", (~value) & 65535)
    return ether + b"\x08\x00" + head + data


def arp_frame(operation=1):
    return ether + b"\x08\x06" + struct.pack("!HHBBH6s4s6s4s", 1, 0x0800, 6, 4, operation, mac, ip, bytes(6), socket.inet_aton("10.10.20.1"))


def frame_check(name, frame, expected):
    assert observe(frame, len(frame), mac, ip) == expected, name
    checks.append(name)


ipv4, arp = ip_header(), arp_frame()
frame_check("source-ipv4", ipv4, 1)
frame_check("source-ipv4-options", ip_header(bytes(40)), 1)
frame_check("source-ipv4-padding", ipv4 + bytes(30), 1)
frame_check("source-arp-request", arp, 2)
frame_check("source-arp-reply", arp_frame(2), 2)
frame_check("source-arp-padding", arp + bytes(18), 2)
for label, valid_frame in [("ipv4", ipv4), ("ipv4-options", ip_header(bytes(40))), ("arp", arp)]:
    for length in range(len(valid_frame)):
        assert observe(valid_frame[:length], length, mac, ip) == 0, (label, length)
    checks.append("reject-every-truncated-" + label)
for name, original, offset, replacement in [
    ("ethernet-mac", ipv4, 6, b"\x06"),
    ("ipv4-source", ipv4, 26, bytes(4)),
    ("ipv4-version", ipv4, 14, b"\x65"),
    ("ipv4-short-header", ipv4, 14, b"\x44"),
    ("ipv4-long-header", ipv4, 14, b"\x4f"),
    ("ipv4-total-short", ipv4, 16, b"\x00\x13"),
    ("ipv4-total-long", ipv4, 16, b"\xff\xff"),
    ("ipv4-checksum", ipv4, 24, bytes(2)),
    ("ipv6-ethertype", ipv4, 12, b"\x86\xdd"),
    ("vlan-ethertype", ipv4, 12, b"\x81\x00"),
    ("arp-hardware", arp, 14, b"\x00\x02"),
    ("arp-protocol", arp, 16, b"\x86\xdd"),
    ("arp-hardware-size", arp, 18, b"\x05"),
    ("arp-protocol-size", arp, 19, b"\x10"),
    ("arp-operation", arp, 20, b"\x00\x03"),
    ("arp-ethernet-mac", arp, 6, b"\x06"),
    ("arp-sender-mac", arp, 22, b"\x06"),
    ("arp-probe-zero-ip", arp, 28, bytes(4)),
    ("arp-other-slot-ip", arp, 28, socket.inet_aton("10.10.20.11")),
]:
    changed = bytearray(original)
    changed[offset:offset + len(replacement)] = replacement
    frame_check("reject-source-" + name, bytes(changed), 0)
assert observe(None, 0, mac, ip) == 0
assert observe(ipv4, len(ipv4), None, ip) == 0
assert observe(ipv4, len(ipv4), mac, None) == 0
checks.append("reject-source-null-input")

# Execute the installer's actual bounded-removal function with a fake launchctl
# and fake time. This reproduces asynchronous bootout without root or launchd
# mutation and checks that unrelated read errors cannot count as "removed".
installer = (source / "install.sh").read_text()
removal_function = re.search(r"wait_service_removed\(\) \{.*?\n\}", installer, re.S).group(0)
launch_mock = work / "launchctl-mock.sh"
launch_mock.write_text('''#!/bin/bash
count=0
if [[ -f "$FMN_MOCK_COUNT" ]]; then read -r count < "$FMN_MOCK_COUNT"; fi
count=$((count + 1))
printf '%s\\n' "$count" > "$FMN_MOCK_COUNT"
printf '%s %s\\n' "$1" "$2" >> "$FMN_MOCK_CALLS"
if [[ $FMN_MOCK_MODE == gone && $count -gt 3 ]]; then exit 113; fi
if [[ $FMN_MOCK_MODE == error ]]; then exit 1; fi
exit 0
''')
launch_mock.chmod(0o700)
function_for_test = removal_function.replace("/bin/launchctl", '"' + str(launch_mock) + '"').replace("/bin/sleep 0.1", ":")
for mode, expected_status, expected_calls in [("gone", 0, 4), ("busy", 75, 100), ("error", 75, 1)]:
    count_path, calls_path = work / "mock-count", work / "mock-calls"
    count_path.unlink(missing_ok=True)
    calls_path.unlink(missing_ok=True)
    environment = dict(os.environ, FMN_MOCK_COUNT=str(count_path), FMN_MOCK_CALLS=str(calls_path), FMN_MOCK_MODE=mode)
    result = subprocess.run(["/bin/bash", "-c", function_for_test + '\nwait_service_removed "io.pgsty.farrow.test-only"\n'], env=environment)
    assert result.returncode == expected_status and int(count_path.read_text()) == expected_calls
    assert set(calls_path.read_text().splitlines()) == {"print system/io.pgsty.farrow.test-only"}
    checks.append("launchd-removal-" + mode)

valid = {
    "schema_version": 1,
    "uid": os.getuid(),
    "installation_id": "f60e8690-61aa-4a6f-b044-2a1ff03b4b2f",
    "subnet": "10.10.20.0/24",
    "slots": [
        {"name": "mac1", "mac": "02:6a:37:00:00:01", "ip": "10.10.20.10"},
        {"name": "mac2", "mac": "02:6a:37:00:00:02", "ip": "10.10.20.11"},
    ],
}


def verify(name, config, ok):
    path = work / "config-test.json"
    path.write_text(json.dumps(config))
    result = subprocess.run([args.helper, "validate-config", "--config", str(path)], capture_output=True, text=True)
    reply = json.loads(result.stdout)
    assert (result.returncode == 0) == ok, (name, result.stdout, result.stderr)
    assert reply["ok"] == ok, (name, reply)
    checks.append(name)


verify("valid-two-slot-config", valid, True)
for name, change in [
    ("reject-root-uid", lambda c: c.update(uid=0)),
    ("reject-boolean-uid", lambda c: c.update(uid=True)),
    ("reject-fractional-uid", lambda c: c.update(uid=os.getuid() + 0.5)),
    ("reject-schema-boolean", lambda c: c.update(schema_version=True)),
    ("reject-public-subnet", lambda c: c.update(subnet="8.8.8.0/24")),
    ("reject-broad-subnet", lambda c: c.update(subnet="10.10.0.0/16")),
    ("reject-host-as-subnet", lambda c: c.update(subnet="10.10.20.1/24")),
    ("reject-unknown-fields", lambda c: c.update(command="/bin/sh")),
    ("reject-invalid-uuid", lambda c: c.update(installation_id="../other")),
    ("reject-duplicate-macs", lambda c: c["slots"][1].update(mac=c["slots"][0]["mac"])),
    ("reject-multicast-mac", lambda c: c["slots"][0].update(mac="03:6a:37:00:00:01")),
    ("reject-global-mac", lambda c: c["slots"][0].update(mac="00:6a:37:00:00:01")),
    ("reject-short-mac", lambda c: c["slots"][0].update(mac="2:6a:37:00:00:01")),
    ("reject-outside-reservation", lambda c: c["slots"][0].update(ip="10.10.21.10")),
    ("reject-wrong-slot-offset", lambda c: c["slots"][0].update(ip="10.10.20.12")),
    ("reject-third-slot", lambda c: c["slots"].append(c["slots"][0])),
    ("reject-wrong-slot-order", lambda c: c["slots"].reverse()),
]:
    altered = copy.deepcopy(valid)
    change(altered)
    verify(name, altered, False)

config_path = work / "network.json"
config_path.write_text(json.dumps(valid, indent=2) + "\n")
symlink = work / "config-symlink.json"
symlink.unlink(missing_ok=True)
symlink.symlink_to(config_path)
result = subprocess.run([args.helper, "validate-config", "--config", str(symlink)], capture_output=True, text=True)
assert result.returncode != 0 and json.loads(result.stdout)["ok"] is False
checks.append("reject-symlink-config")
for name, path, expected in [
    ("accept-macos-system-run-parent", "/var/run/farrow-mac", True),
    ("reject-user-owned-privileged-path", str(work), False),
    ("reject-world-writable-system-path", "/private/tmp", False),
]:
    if not Path(path).exists():
        continue
    result = subprocess.run([args.helper, "check-path", "--path", path], capture_output=True, text=True)
    assert (result.returncode == 0) == expected, (name, result.stdout, result.stderr)
    checks.append(name)

# A test-only replacement for the root-peer check lets us exercise ancillary
# parsing as an ordinary user. The shipped object always uses real getpeereid.
shim = work / "test-peer.c"
shim.write_text("#include <sys/types.h>\nint fmn_test_getpeereid(int fd, uid_t *u, gid_t *g) {(void)fd; *u=0; *g=0; return 0;}\n")
actual_dylib, mock_dylib = work / "client-actual.dylib", work / "client-wire-test.dylib"
subprocess.run(["xcrun", "clang", "-dynamiclib", "-Wall", "-Wextra", "-Werror", str(source / "client.c"), "-o", str(actual_dylib)], check=True)
subprocess.run(["xcrun", "clang", "-dynamiclib", "-Wall", "-Wextra", "-Werror", "-Dgetpeereid=fmn_test_getpeereid", str(source / "client.c"), str(shim), "-o", str(mock_dylib)], check=True)


def load(path):
    lib = ctypes.CDLL(str(path))
    fn = lib.farrow_mac_network_connect
    fn.argtypes = [ctypes.c_char_p, ctypes.c_int, ctypes.POINTER(ctypes.c_int), ctypes.c_char_p, ctypes.c_size_t]
    fn.restype = ctypes.c_int
    return fn


actual, mock = load(actual_dylib), load(mock_dylib)
# Darwin sockaddr_un max is 104, so keep the socket below that limit even when
# the caller's evidence directory is long. The temporary directory is private.
socket_dir = tempfile.TemporaryDirectory(prefix="fmn-wire-", dir="/tmp")


def wire(name, function, passed_fds=(), response=b"FMN1" + struct.pack("!I", 0), expect=True, fragment=False, peer_test=False):
    path = os.path.join(socket_dir.name, "control")
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(path)
    server.listen(1)
    server_errors = []
    control_gone = threading.Event()

    def serve():
        try:
            conn, _ = server.accept()
            with conn:
                request = conn.recv(8, socket.MSG_WAITALL)
                if peer_test:
                    assert request == b""
                else:
                    assert request == b"FMN1\x01\x00\x00\x00", request
                    ancillary = [(socket.SOL_SOCKET, socket.SCM_RIGHTS, array.array("i", passed_fds))] if passed_fds else []
                    conn.sendmsg([response[:3] if fragment else response], ancillary)
                    if fragment:
                        conn.sendall(response[3:])
                    conn.shutdown(socket.SHUT_WR)
                    conn.recv(1)
                control_gone.set()
        except BaseException as exc:
            server_errors.append(str(exc))

    thread = threading.Thread(target=serve)
    thread.start()
    control = ctypes.c_int(-1)
    error = ctypes.create_string_buffer(512)
    fd = function(path.encode(), 1, ctypes.byref(control), error, len(error))
    assert (fd >= 0) == expect, (name, fd, error.value)
    if fd >= 0:
        adopted = socket.socket(fileno=fd)
        assert adopted.type == socket.SOCK_DGRAM
        adopted.close()
    if control.value >= 0:
        os.close(control.value)
    thread.join(timeout=3)
    assert not thread.is_alive() and not server_errors and control_gone.is_set(), (name, server_errors)
    server.close()
    os.unlink(path)
    checks.append(name)


wire("reject-user-impersonation-server", actual, expect=False, peer_test=True)
left, right = socket.socketpair(socket.AF_UNIX, socket.SOCK_DGRAM)
try:
    wire("receive-connected-datagram-fd", mock, [left.fileno()])
    wire("receive-fragmented-control-reply", mock, [left.fileno()], fragment=True)
    wire("reject-missing-fd", mock, expect=False)
    wire("reject-extra-fds", mock, [left.fileno(), right.fileno()], expect=False)
    wire("reject-wrong-protocol", mock, [left.fileno()], response=b"BAD1\x00\x00\x00\x00", expect=False)
    wire("reject-busy-status", mock, response=b"FMN1" + struct.pack("!I", 16), expect=False)
    # The receiving side must preserve arbitrary Ethernet packet boundaries.
    left.send(b"first-frame")
    left.send(b"second-frame-is-longer")
    assert right.recv(65536) == b"first-frame" and right.recv(65536) == b"second-frame-is-longer"
    checks.append("connected-datagram-frame-boundaries")
finally:
    left.close()
    right.close()
left, right = socket.socketpair(socket.AF_UNIX, socket.SOCK_STREAM)
try:
    wire("reject-stream-fd", mock, [left.fileno()], expect=False)
finally:
    left.close()
    right.close()
unconnected = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
try:
    wire("reject-unconnected-datagram-fd", mock, [unconnected.fileno()], expect=False)
finally:
    unconnected.close()
socket_dir.cleanup()
result = {"ok": True, "checks": checks, "count": len(checks), "real_vmnet_started": False}
(work / "tests.json").write_text(json.dumps(result, indent=2) + "\n")
print(json.dumps(result))
