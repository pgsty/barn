#!/usr/bin/env python3
"""Destructive two-slot acceptance, restricted to an explicitly marked test root.

Never calls mac password or reads SSH private keys. Failures preserve all guest
state. --plan prints the sequence without executing Farrow or creating files.
"""
from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import platform
import re
import signal
import stat
import subprocess
import sys
import time
import uuid


PHASES = [
    "preflight-and-two-slot-list", "up-mac1-and-base-fingerprint", "up-mac2-and-readiness",
    "guest-accounts-and-independent-identities", "disk-write-isolation", "host-guest-and-peer-network",
    "dns-and-public-https", "remote-exit-and-argument-boundaries", "stop-start-preserves-both-slots",
    "reset-mac1-rotates-identities", "repeated-up-reuses-base", "destroy-mac1-preserves-mac2",
]
MARKER = ".farrow-live-acceptance.json"
BASE_FILES = ["disk.asif", "hardware-model.bin", "auxiliary-storage.bin", "machine-id.bin", "base.json", "metadata.json"]


class AcceptanceFailure(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise AcceptanceFailure(message)


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def scrub(value):
    """Defense in depth; the command allowlist never requests secret material."""
    if isinstance(value, dict):
        return {k: "[redacted]" if k.lower() in {"password", "private_key", "privatekey"} else scrub(v) for k, v in value.items()}
    if isinstance(value, list):
        return [scrub(v) for v in value]
    if isinstance(value, str):
        value = re.sub(r"-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----", "[redacted private key]", value, flags=re.S)
        return re.sub(r'("password"\s*:\s*)"(?:\\.|[^"\\])*"', r'\1"[redacted]"', value, flags=re.I)
    return value


def private_json(path, value):
    data = json.dumps(scrub(value), ensure_ascii=False, indent=2) + "\n"
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w") as stream:
        stream.write(data)
    os.chmod(path, 0o600)


def canonical_path(raw, label):
    path = Path(raw)
    require(path.is_absolute(), f"{label} must be an absolute path")
    require(".." not in path.parts, f"{label} must not contain '..'")
    # macOS /tmp and /var are documented system aliases, not user symlinks.
    for prefix in (Path("/tmp"), Path("/var")):
        if path == prefix or prefix in path.parents:
            path = prefix.resolve() / path.relative_to(prefix)
            break
    for component in reversed([path, *path.parents]):
        require(not component.is_symlink(), f"{label} contains a symlink: {component}")
    return path


def prepare_test_root(args):
    require(os.geteuid() != 0, "Run as the ordinary macOS login user, not root")
    require(platform.system() == "Darwin" and platform.machine() == "arm64", "Live acceptance requires Apple Silicon/macOS")
    home = canonical_path(args.home, "--home")
    output = canonical_path(args.output, "--output")
    login_home = Path.home().resolve()
    require(home not in {Path("/"), Path("/tmp").resolve(), Path("/var").resolve(), login_home, login_home / ".farrow"}, "Refusing a normal user/system data root")
    require(len(home.parts) >= 4 and not (home / ".git").exists(), "Choose a dedicated test data directory")
    require(output != home and home not in output.parents and output not in home.parents, "Evidence output and test data root must be separate directories")
    if home.exists():
        info = home.stat()
        require(stat.S_ISDIR(info.st_mode) and info.st_uid == os.getuid() and info.st_mode & 0o022 == 0, "Test root must be an owned directory without group/world write access")
        require(all(entry.name == "mac" for entry in home.iterdir()), "Test root contains unrelated data; use a dedicated FARROW_HOME containing only mac/")
    mac = canonical_path(str(home / "mac"), "Mac data directory")
    marker = mac / MARKER
    if marker.exists():
        require(marker.is_file() and not marker.is_symlink(), "Unsafe test-root marker")
        saved = json.loads(marker.read_text())
        require(saved.get("schema_version") == 1 and saved.get("home") == str(home) and saved.get("uid") == os.getuid() and saved.get("destructive_test_root") is True, "Test-root marker does not authorize this directory/UID")
    else:
        require(args.allow_test_root, "Missing test-root marker: pass --allow-test-root only for a disposable, dedicated root")
        home.mkdir(mode=0o700, parents=True, exist_ok=True)
        mac.mkdir(mode=0o700, exist_ok=True)
        require(not mac.is_symlink(), "Mac data directory cannot be a symlink")
        private_json(marker, {"schema_version": 1, "home": str(home), "uid": os.getuid(), "destructive_test_root": True, "created_at": now()})
    require(not output.exists() or not any(output.iterdir()), "Evidence output must be a new or empty directory; old evidence is never overwritten")
    output.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(output, 0o700)
    return home, output


class Harness:
    def __init__(self, args):
        self.args = args
        self.farrow = canonical_path(args.farrow, "--farrow")
        require(self.farrow.is_file() and os.access(self.farrow, os.X_OK), "--farrow must name an executable regular file")
        self.home, self.output = prepare_test_root(args)
        self.mac = self.home / "mac"
        self.env = os.environ.copy()
        self.env["FARROW_HOME"] = str(self.home)
        # Test the selected bundle's companions, not ambient developer overrides.
        for name in ("FARROW_MAC_RUNNER", "FARROW_MAC_NETWORK", "FARROW_MAC_NETWORK_INSTALLER"):
            self.env.pop(name, None)
        self.run_id = uuid.uuid4().hex
        self.steps = []
        self.current = None
        self.command_number = 0
        self.started = time.monotonic()
        self.summary = {"schema_version": 1, "run_id": self.run_id, "home": str(self.home), "farrow": str(self.farrow), "started_at": now(), "status": "running", "steps": self.steps,
                        "gui": {"status": "pending", "required": ["open a native VM desktop and visually verify login", "close the window and verify the VM/SSH remain running"]}}
        private_json(self.output / "summary.json", self.summary)

    def event(self, value):
        value = scrub({"time": now(), **value})
        with (self.output / "events.jsonl").open("a") as stream:
            stream.write(json.dumps(value, ensure_ascii=False) + "\n")
        os.chmod(self.output / "events.jsonl", 0o600)
        print(json.dumps(value, ensure_ascii=False), flush=True)

    def storage(self):
        usage = os.statvfs(self.mac)
        disks = {}
        for name in ("mac1", "mac2"):
            disk = self.mac / "slots" / name / "disk.asif"
            if disk.is_file():
                info = disk.stat()
                disks[name] = {"file_bytes": info.st_size, "allocated_bytes": info.st_blocks * 512}
        return {"host_available_bytes": usage.f_bavail * usage.f_frsize, "slot_disks": disks}

    @contextlib.contextmanager
    def step(self, name):
        number = len(self.steps) + 1
        self.current = self.output / f"{number:02d}-{name}"
        self.current.mkdir(mode=0o700)
        record = {"name": name, "status": "running", "started_at": now(), "storage_before": self.storage()}
        self.steps.append(record)
        self.command_number = 0
        started = time.monotonic()
        self.event({"step": name, "status": "started"})
        try:
            yield record
        except BaseException as error:
            record.update(status="failed", error=str(error), elapsed_seconds=round(time.monotonic() - started, 3))
            private_json(self.current / "step.json", record)
            raise
        else:
            record.update(status="passed", elapsed_seconds=round(time.monotonic() - started, 3))
            private_json(self.current / "step.json", record)
            self.event({"step": name, "status": "passed", "elapsed_seconds": record["elapsed_seconds"]})
        finally:
            record["storage_after"] = self.storage()
            private_json(self.current / "step.json", record)
            private_json(self.output / "summary.json", self.summary)

    def command(self, argv, expected=0, timeout=240, parse_json=False):
        self.command_number += 1
        evidence = self.current / f"command-{self.command_number:02d}.json"
        record = {"argv": [str(v) for v in argv], "expected_exit": expected, "started_at": now()}
        private_json(evidence, record)
        start = time.monotonic()
        process = subprocess.Popen(argv, env=self.env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            while True:
                remaining = timeout - (time.monotonic() - start)
                if remaining <= 0:
                    raise TimeoutError(f"Command exceeded {timeout}s; guest state is retained")
                try:
                    stdout, stderr = process.communicate(timeout=min(15, remaining))
                    break
                except subprocess.TimeoutExpired:
                    self.event({"step": self.steps[-1]["name"], "status": "command_running", "pid": process.pid, "elapsed_seconds": round(time.monotonic() - start), "storage": self.storage()})
        except BaseException:
            # Signal only this foreground CLI. Never kill a process group, a
            # detached runner, another VM, or the host's network daemon.
            if process.poll() is None:
                process.send_signal(signal.SIGINT)
            try:
                stdout, stderr = process.communicate(timeout=10)
            except subprocess.TimeoutExpired:
                process.terminate()
                try:
                    stdout, stderr = process.communicate(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    stdout, stderr = process.communicate()
            record.update(exit_code=process.returncode, stdout=stdout, stderr=stderr, interrupted=True)
            private_json(evidence, record)
            raise
        record.update(exit_code=process.returncode, stdout=stdout, stderr=stderr, elapsed_seconds=round(time.monotonic() - start, 3))
        private_json(evidence, record)
        require(process.returncode == expected, f"Command exited {process.returncode}, expected {expected}; inspect {evidence}")
        if parse_json:
            try:
                result = json.loads(stdout)
            except ValueError as error:
                raise AcceptanceFailure(f"Invalid CLI JSON; inspect {evidence}") from error
            return result
        return stdout

    def cli(self, *args, expected=0, timeout=240):
        require(args and args[0] != "password", "Acceptance must never request GUI passwords")
        return self.command([str(self.farrow), "mac", *args, "--json"], expected, timeout, True)

    def remote(self, slot, argv, expected=0, timeout=90):
        result = self.command([str(self.farrow), "--json", "mac", "exec", slot, "--", *argv], expected, timeout, True)
        require(isinstance(result, dict) and result.get("slot") == slot and result.get("exit_code") == expected, "Remote JSON must preserve slot and exact exit code")
        return result

    def remote_shell(self, slot, script, *args, expected=0):
        return self.remote(slot, ["/bin/sh", "-c", script, "farrow-acceptance", *args], expected)

    def listing(self, states=None):
        result = self.cli("ls")
        require(result.get("root") == str(self.mac), "CLI used a different data root")
        slots = result.get("slots", [])
        require([s.get("name") for s in slots] == ["mac1", "mac2"], "ls must always contain exactly mac1 and mac2")
        if states:
            for slot in slots:
                require(slot["state"] == states[slot["name"]], f"Unexpected state for {slot['name']}: {slot['state']}")
        return result, {slot["name"]: slot for slot in slots}

    def fingerprint(self, path):
        path = canonical_path(str(path), "fingerprinted file")
        require(self.mac in path.parents and path.name not in {"id_ed25519", "id_rsa"}, "Fingerprint is outside Mac data or names a private key")
        before = path.stat()
        require(stat.S_ISREG(before.st_mode), "Fingerprint target is not a regular file")
        digest = hashlib.sha256()
        with path.open("rb") as stream:
            while block := stream.read(8 * 1024 * 1024):
                digest.update(block)
        after = path.stat()
        require((before.st_ino, before.st_size, before.st_mtime_ns) == (after.st_ino, after.st_size, after.st_mtime_ns), "File changed while it was fingerprinted")
        return {"sha256": digest.hexdigest(), "size": before.st_size, "mtime_ns": before.st_mtime_ns, "inode": before.st_ino, "mode": stat.S_IMODE(before.st_mode)}

    def base_snapshot(self, base_id, filename):
        require(re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", base_id) is not None, "Invalid base identity")
        self.event({"step": self.steps[-1]["name"], "status": "hashing_base", "base_id": base_id})
        result = {name: self.fingerprint(self.mac / "images" / "base" / base_id / name) for name in BASE_FILES}
        private_json(self.current / filename, result)
        return result

    def identity(self, name, slot):
        script = r'''set -eu
printf 'user='; /usr/bin/id -un
printf 'uid='; /usr/bin/id -u
printf 'groups='; /usr/bin/id -Gn
printf 'sudo_uid='; /usr/bin/sudo -n /usr/bin/id -u
printf 'version='; /usr/bin/sw_vers -productVersion
printf 'build='; /usr/bin/sw_vers -buildVersion
printf 'platform_uuid='; /usr/sbin/ioreg -rd1 -c IOPlatformExpertDevice | /usr/bin/awk -F '"' '/"IOPlatformUUID"/ {print $(NF-1)}'
printf 'ssh_host_fingerprint='; /usr/bin/ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub -E sha256 | /usr/bin/awk '{print $2}'
printf 'ipv4='; /usr/sbin/ipconfig getifaddr en0
'''
        result = self.remote_shell(name, script)
        values = dict(line.split("=", 1) for line in result["stdout"].splitlines() if "=" in line)
        require(values.get("user") == slot["user"] and values.get("uid") != "0", f"{name} did not log in as its ordinary user")
        require("admin" in values.get("groups", "").split() and values.get("sudo_uid") == "0", f"{name} admin/passwordless sudo failed")
        require(values.get("version", "").startswith("27.") and values.get("build") == self.args.expected_build, f"{name} guest OS build differs from the acceptance target")
        require(values.get("ipv4") == slot["ip"], f"{name} DHCP address does not match its reserved IP")
        try:
            uuid.UUID(values.get("platform_uuid", ""))
        except ValueError as error:
            raise AcceptanceFailure(f"{name} has no valid IOPlatformUUID") from error
        require(values.get("ssh_host_fingerprint", "").startswith("SHA256:"), "Missing public SSH host fingerprint")
        identity = {"instance_id": slot["instance_id"], "platform_uuid": values["platform_uuid"], "ssh_host_fingerprint": values["ssh_host_fingerprint"],
                    "machine_id_sha256": self.fingerprint(self.mac / "slots" / name / "machine-id.bin")["sha256"],
                    "client_public_key_sha256": self.fingerprint(self.mac / "slots" / name / "id_ed25519.pub")["sha256"]}
        private_json(self.current / f"{name}-identity.json", {"identity": identity, "guest": values})
        return identity

    def sentinel(self, name, token):
        result = self.remote_shell(name, 'set -eu; test -f "$1"; /bin/cat "$1"', self.sentinel_path)
        require(result["stdout"] == token, f"{name} disk content changed or crossed into another slot")

    def account_policy(self, name, slot):
        script = r'''set -eu
printf 'auto_login='; /usr/bin/sudo -n /usr/bin/defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser
printf 'console_user='; /usr/bin/stat -f '%Su' /dev/console
/usr/bin/sudo -n /usr/sbin/sshd -T | /usr/bin/awk '$1 == "passwordauthentication" || $1 == "kbdinteractiveauthentication" || $1 == "permitrootlogin" {print $1 "=" $2}'
plist="$HOME/Library/Preferences/MobileMeAccounts.plist"
if test -f "$plist" && /usr/libexec/PlistBuddy -c 'Print :Accounts:0' "$plist" >/dev/null 2>&1; then
  printf 'apple_account_preferences=present\n'
else
  printf 'apple_account_preferences=empty\n'
fi
'''
        result = self.remote_shell(name, script)
        values = dict(line.split("=", 1) for line in result["stdout"].splitlines() if "=" in line)
        require(values.get("auto_login") == slot["user"] and values.get("console_user") == slot["user"], f"{name} automatic desktop login is not established")
        for setting in ("passwordauthentication", "kbdinteractiveauthentication", "permitrootlogin"):
            require(values.get(setting) == "no", f"{name} effective sshd policy still permits {setting}")
        require(values.get("apple_account_preferences") == "empty", f"{name} has Apple Account preferences")
        return values

    def run(self):
        with self.step(PHASES[0]) as record:
            result, _ = self.listing()
            record["initial"] = result
            self.command([str(self.farrow), "version"], timeout=30)
        with self.step(PHASES[1]) as record:
            self.cli("up", "mac1", timeout=self.args.operation_timeout)
            _, slots = self.listing()
            require(slots["mac1"]["state"] == "ready", "mac1 up did not reach ready")
            base_id = slots["mac1"]["base_id"]
            original_base = self.base_snapshot(base_id, "base-before-mac2.json")
            record["base_id"] = base_id
        with self.step(PHASES[2]) as record:
            self.cli("up", "mac2", timeout=self.args.operation_timeout)
            status, slots = self.listing({"mac1": "ready", "mac2": "ready"})
            subnet = ipaddress.ip_network(status["network"]["subnet"])
            require(subnet.prefixlen == 24 and subnet.is_private, "Expected a dedicated private /24")
            require(slots["mac1"]["ip"] == str(subnet.network_address + 10) and slots["mac2"]["ip"] == str(subnet.network_address + 11), "Stable .10/.11 reservations are not reflected in JSON")
            require(slots["mac1"]["base_id"] == slots["mac2"]["base_id"] == base_id, "Both fresh slots must reuse one base")
            require(self.base_snapshot(base_id, "base-after-mac2.json") == original_base, "Creating mac2 changed/replaced the read-only base")
            record.update(subnet=str(subnet), gateway=status["network"]["gateway"], slots=slots)
        with self.step(PHASES[3]) as record:
            identities = {name: self.identity(name, slots[name]) for name in ("mac1", "mac2")}
            for key in identities["mac1"]:
                require(identities["mac1"][key] != identities["mac2"][key], f"The two slots share {key}")
            record["identities"] = identities
            record["account_policy"] = {name: self.account_policy(name, slots[name]) for name in ("mac1", "mac2")}
            auxiliary = {name: self.fingerprint(self.mac / "slots" / name / "auxiliary-storage.bin") for name in ("mac1", "mac2")}
            require(auxiliary["mac1"]["inode"] != auxiliary["mac2"]["inode"], "Both VMs share one writable auxiliary-storage file")
            record["independent_auxiliary_storage"] = auxiliary
        self.sentinel_path = f"/private/var/tmp/farrow-acceptance-{self.run_id}"
        tokens = {name: f"{self.run_id}:{name}:independent-disk" for name in ("mac1", "mac2")}
        with self.step(PHASES[4]):
            for name in ("mac1", "mac2"):
                self.remote_shell(name, 'set -eu; umask 077; test ! -e "$1"; /usr/bin/printf "%s" "$2" > "$1"', self.sentinel_path, tokens[name])
            for name in ("mac1", "mac2"):
                self.sentinel(name, tokens[name])
        with self.step(PHASES[5]):
            gateway = status["network"]["gateway"]
            ipaddress.ip_address(gateway)
            for name, other in (("mac1", "mac2"), ("mac2", "mac1")):
                self.command(["/sbin/ping", "-n", "-c", "2", "-W", "2000", slots[name]["ip"]], timeout=15)
                self.remote(name, ["/sbin/ping", "-n", "-c", "2", "-W", "2000", gateway])
                self.remote(name, ["/sbin/ping", "-n", "-c", "2", "-W", "2000", slots[other]["ip"]])
        with self.step(PHASES[6]):
            for name in ("mac1", "mac2"):
                dns = self.remote(name, ["/usr/bin/dscacheutil", "-q", "host", "-a", "name", "www.apple.com"])
                require("ip_address:" in dns["stdout"] or "ipv6_address:" in dns["stdout"], f"{name} DNS produced no addresses")
                https = self.remote(name, ["/usr/bin/curl", "--noproxy", "*", "--fail", "--silent", "--show-error", "--location", "--connect-timeout", "10", "--max-time", "30", "--output", "/dev/null", "--write-out", "%{http_code}\\n", "https://www.apple.com/"])
                require(https["stdout"].strip() == "200", f"{name} public HTTPS did not return 200")
        with self.step(PHASES[7]):
            for name in ("mac1", "mac2"):
                remote = self.remote_shell(name, 'printf "remote-stdout"; printf "remote-stderr" >&2; exit 37', expected=37)
                require(remote["stdout"] == "remote-stdout" and remote["stderr"] == "remote-stderr", "Remote exit output streams changed")
                arguments = ["", "two words", "single'quote", 'double"quote', "$HOME", "$(printf SUBSTITUTED)", "line1\nline2", "--leading-dash", "中文参数"]
                boundaries = self.remote(name, ["/usr/bin/printf", "%s\\0", *arguments])
                require(boundaries["stdout"] == "\0".join(arguments) + "\0", "Remote argument boundaries were not preserved")
        with self.step(PHASES[8]):
            for name, other in (("mac1", "mac2"), ("mac2", "mac1")):
                self.cli("stop", name)
                self.listing({name: "stopped", other: "ready"})
                self.cli("start", name, timeout=self.args.operation_timeout)
                _, slots = self.listing({"mac1": "ready", "mac2": "ready"})
                for guest in ("mac1", "mac2"):
                    require(self.identity(guest, slots[guest]) == identities[guest], f"Stopping/restarting {name} changed {guest} identity")
                    self.sentinel(guest, tokens[guest])
        with self.step(PHASES[9]) as record:
            original_ip, original_mac = slots["mac1"]["ip"], slots["mac1"]["mac"]
            self.cli("reset", "mac1", "--force", timeout=self.args.operation_timeout)
            _, reset_slots = self.listing({"mac1": "created", "mac2": "ready"})
            require(reset_slots["mac1"]["instance_id"] != identities["mac1"]["instance_id"], "Reset reused instance UUID")
            require(reset_slots["mac1"]["base_id"] == base_id, "Ordinary reset changed base version")
            self.cli("up", "mac1", timeout=self.args.operation_timeout)
            _, slots = self.listing({"mac1": "ready", "mac2": "ready"})
            require(slots["mac1"]["ip"] == original_ip and slots["mac1"]["mac"] == original_mac, "Reset changed stable slot IP/MAC")
            renewed = self.identity("mac1", slots["mac1"])
            for key, value in renewed.items():
                require(value != identities["mac1"][key], f"Reset did not rotate {key}")
            self.remote_shell("mac1", 'test ! -e "$1"', self.sentinel_path)
            require(self.identity("mac2", slots["mac2"]) == identities["mac2"], "Resetting mac1 changed mac2 identity")
            self.sentinel("mac2", tokens["mac2"])
            identities["mac1"] = renewed
            record["new_identity"] = renewed
        with self.step(PHASES[10]) as record:
            for name in ("mac1", "mac2"):
                self.cli("up", name, timeout=self.args.operation_timeout)
            _, slots = self.listing({"mac1": "ready", "mac2": "ready"})
            for name in ("mac1", "mac2"):
                require(self.identity(name, slots[name]) == identities[name], f"Repeated up recreated {name}")
            self.sentinel("mac2", tokens["mac2"])
            require(self.base_snapshot(base_id, "base-after-reset-and-repeated-up.json") == original_base, "Reset/up modified or rebuilt the shared base")
            record["images"] = self.cli("image", "ls")
        with self.step(PHASES[11]) as record:
            self.cli("destroy", "mac1", "--force")
            final, slots = self.listing({"mac1": "empty", "mac2": "ready"})
            require(self.identity("mac2", slots["mac2"]) == identities["mac2"], "Destroying mac1 changed mac2 identity")
            self.sentinel("mac2", tokens["mac2"])
            require((self.mac / "images" / "base" / base_id / "disk.asif").is_file(), "Destroy removed the referenced base")
            require(not (self.mac / "slots" / "mac1").exists(), "Destroy left mac1 instance data")
            if self.args.stop_survivor:
                self.cli("stop", "mac2")
                final, _ = self.listing({"mac1": "empty", "mac2": "stopped"})
            record["final"] = final
            private_json(self.output / "final-state.json", final)
        self.summary.update(status="automated_passed_gui_pending", completed_at=now(), elapsed_seconds=round(time.monotonic() - self.started, 3), surviving_slot="mac2", surviving_state="stopped" if self.args.stop_survivor else "ready")
        private_json(self.output / "summary.json", self.summary)
        self.event({"status": self.summary["status"], "summary": str(self.output / "summary.json")})


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--farrow", required=True, help="absolute path to the macOS bundle's farrow executable")
    parser.add_argument("--home", required=True, help="absolute disposable FARROW_HOME; mac1 is reset and destroyed")
    parser.add_argument("--output", required=True, help="absolute new/empty directory for structured evidence")
    parser.add_argument("--allow-test-root", action="store_true", help="explicitly mark this dedicated root for destructive acceptance; never use normal ~/.farrow")
    parser.add_argument("--expected-build", default="26A428", help="expected installed macOS 27 build (default: 26A428)")
    parser.add_argument("--operation-timeout", type=int, default=5400, help="up/start/reset timeout in seconds, including preparation")
    parser.add_argument("--stop-survivor", action="store_true", help="leave mac2 stopped after success; default keeps it ready for manual GUI acceptance")
    parser.add_argument("--plan", action="store_true", help="print the sequence without executing Farrow or touching files")
    args = parser.parse_args()
    if args.plan:
        print(json.dumps({"phases": PHASES, "destructive_scope": "mac1 reset and destroy within the explicitly marked test root", "gui": "separate manual acceptance", "vm_operations_executed": False}, indent=2))
        return 0
    harness = None
    try:
        require(args.operation_timeout > 0, "--operation-timeout must be positive")
        os.umask(0o077)
        harness = Harness(args)
        harness.run()
        return 0
    except BaseException as error:
        if harness is not None:
            harness.summary.update(status="failed_state_preserved", error=str(error), ended_at=now(), elapsed_seconds=round(time.monotonic() - harness.started, 3))
            private_json(harness.output / "summary.json", harness.summary)
            harness.event({"status": "failed_state_preserved", "error": str(error), "summary": str(harness.output / "summary.json"), "cleanup_performed": False})
        else:
            print(json.dumps({"status": "preflight_failed", "error": str(error), "vm_operations_executed": False}), file=sys.stderr)
        return 130 if isinstance(error, KeyboardInterrupt) else 1


if __name__ == "__main__":
    sys.exit(main())
