#!/usr/bin/env python3
"""Bounded local qcow2 normalization and release-candidate evidence builder."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any, Dict, List, Mapping, Optional, Sequence, Tuple
from urllib.parse import urlsplit


PIPELINE_SCHEMA = 1
PIPELINE_VERSION = "1"
HERE = Path(__file__).resolve().parent
CONFIG_PATH = HERE / "recipe-v1.json"
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
NAME_RE = re.compile(r"^[a-z][a-z0-9-]{0,31}$")
USER_RE = re.compile(r"^[a-z_][a-z0-9_-]{0,31}$")
RELEASE_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$")
PACKAGE_NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9+._-]{0,127}$")
PACKAGE_FILENAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9+._~-]{0,255}\.(?:deb|rpm)$")
MOVING_SEGMENTS = {"latest", "current", "release"}


class PipelineError(RuntimeError):
    pass


def canonical_json(value: Any) -> bytes:
    return (json.dumps(value, indent=2, sort_keys=True, separators=(",", ": ")) + "\n").encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def hash_file(path: Path, chunk_bytes: int) -> Tuple[str, int]:
    digest = hashlib.sha256()
    size = 0
    with path.open("rb", buffering=0) as handle:
        while True:
            block = handle.read(chunk_bytes)
            if not block:
                break
            digest.update(block)
            size += len(block)
    return digest.hexdigest(), size


def fixed_time(epoch: int) -> str:
    return dt.datetime.fromtimestamp(epoch, tz=dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def validate_single_line(label: str, value: str, maximum: int = 1024) -> str:
    if not value or len(value) > maximum or any(character in value for character in "\r\n\0"):
        raise PipelineError(f"{label} must be a non-empty bounded single line")
    return value


def validate_immutable_https(label: str, value: str, allow_digest_template: bool = False) -> str:
    candidate = value
    if allow_digest_template:
        if candidate.count("{sha256}") > 1:
            raise PipelineError(f"{label} may contain {{sha256}} at most once")
        remainder = candidate.replace("{sha256}", "")
        if "{" in remainder or "}" in remainder:
            raise PipelineError(f"{label} contains an unknown template")
        candidate = candidate.replace("{sha256}", "0" * 64)
    parsed = urlsplit(candidate)
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
        or not parsed.path
        or parsed.path.endswith("/")
    ):
        raise PipelineError(f"{label} must be an immutable absolute HTTPS artifact URL")
    for segment in parsed.path.lower().split("/"):
        if segment in MOVING_SEGMENTS or ".latest." in segment:
            raise PipelineError(f"{label} contains moving release path segment {segment!r}")
    return value


def validate_source_uri(value: Optional[str], expected_sha256: str) -> str:
    if value is None:
        return f"urn:sha256:{expected_sha256}"
    validate_single_line("source URI", value, 2048)
    if value == f"urn:sha256:{expected_sha256}":
        return value
    return validate_immutable_https("source URI", value)


def resolve_tool(label: str, value: str) -> Path:
    validate_single_line(label, value, 4096)
    if os.sep in value:
        candidate = Path(value)
        if not candidate.is_absolute():
            raise PipelineError(f"{label} path must be absolute when it contains a separator")
        resolved = candidate.resolve(strict=True)
    else:
        found = shutil.which(value)
        if found is None:
            raise PipelineError(f"required tool is missing: {value}")
        resolved = Path(found).resolve(strict=True)
    info = resolved.stat()
    if not stat.S_ISREG(info.st_mode) or not os.access(resolved, os.X_OK):
        raise PipelineError(f"{label} is not an executable regular file: {resolved}")
    return resolved


def load_config() -> Tuple[Dict[str, Any], bytes, Path, bytes]:
    try:
        config_bytes = CONFIG_PATH.read_bytes()
        config = json.loads(config_bytes)
    except (OSError, json.JSONDecodeError) as error:
        raise PipelineError(f"load pipeline recipe: {error}") from error
    if config.get("schema") != PIPELINE_SCHEMA or not isinstance(config.get("recipe_id"), str):
        raise PipelineError("pipeline recipe has an unsupported schema or missing ID")
    for key in ("max_source_bytes", "max_package_bytes", "max_package_bundle_bytes", "copy_chunk_bytes"):
        if not isinstance(config.get(key), int) or config[key] <= 0:
            raise PipelineError(f"pipeline recipe has invalid {key}")
    timeouts = config.get("timeouts_seconds")
    if not isinstance(timeouts, dict) or any(
        not isinstance(timeouts.get(key), int) or timeouts[key] <= 0
        for key in ("qemu_img", "virt_customize", "virt_cat")
    ):
        raise PipelineError("pipeline recipe has invalid tool timeouts")
    normalization = config.get("normalization")
    if not isinstance(normalization, dict) or normalization.get("network") is not False:
        raise PipelineError("pipeline recipe must disable guest customization networking")
    if normalization.get("locale_policy") != {
        "available": ["C.UTF-8", "en_US.UTF-8"],
        "default": "C.UTF-8",
        "profiles": ["d12", "d13"],
    }:
        raise PipelineError("pipeline recipe has an invalid locale policy")
    profiles = normalization.get("profiles")
    if not isinstance(profiles, dict) or set(profiles) != {"base", "el8", "el9", "d12", "d13"}:
        raise PipelineError("pipeline recipe has invalid customization profiles")
    for profile, policy in profiles.items():
        if not isinstance(policy, dict) or policy.get("package_format") not in (None, "deb", "rpm"):
            raise PipelineError(f"pipeline recipe profile {profile} has an invalid package format")
        required = policy.get("required_packages")
        locked = policy.get("locked_packages")
        if not isinstance(required, list) or not isinstance(locked, list) or any(
            not isinstance(name, str) or not PACKAGE_NAME_RE.fullmatch(name) for name in required + locked
        ):
            raise PipelineError(f"pipeline recipe profile {profile} has invalid required packages")
        if len(set(locked)) != len(locked) or any(name not in locked for name in required):
            raise PipelineError(f"pipeline recipe profile {profile} has an invalid locked package closure")
        if (policy.get("package_format") is None) != (len(required) == 0):
            raise PipelineError(f"pipeline recipe profile {profile} package policy is inconsistent")
    guest_name = normalization.get("guest_script")
    if not isinstance(guest_name, str) or Path(guest_name).name != guest_name:
        raise PipelineError("pipeline recipe guest script must be a local basename")
    guest_script = (HERE / guest_name).resolve(strict=True)
    try:
        guest_script.relative_to(HERE)
    except ValueError as error:
        raise PipelineError("pipeline recipe guest script escaped its directory") from error
    guest_bytes = guest_script.read_bytes()
    return config, config_bytes, guest_script, guest_bytes


def safe_input_file(path_text: str, maximum: int, label: str) -> Path:
    path = Path(path_text)
    if not path.is_absolute():
        raise PipelineError(f"{label} must be an absolute path")
    try:
        raw = os.lstat(path)
    except OSError as error:
        raise PipelineError(f"inspect {label}: {error}") from error
    if stat.S_ISLNK(raw.st_mode) or not stat.S_ISREG(raw.st_mode):
        raise PipelineError(f"{label} must be a regular non-symlink file")
    if raw.st_nlink < 1 or raw.st_size <= 0 or raw.st_size > maximum:
        raise PipelineError(f"{label} size {raw.st_size} is outside the recipe bound 1..{maximum}")
    resolved = path.resolve(strict=True)
    if resolved != path:
        raise PipelineError(f"{label} path must already be canonical")
    return resolved


def safe_source(path_text: str, maximum: int) -> Path:
    return safe_input_file(path_text, maximum, "source")


def safe_input_directory(path_text: str, label: str) -> Path:
    path = Path(path_text)
    if not path.is_absolute():
        raise PipelineError(f"{label} must be an absolute path")
    try:
        raw = os.lstat(path)
    except OSError as error:
        raise PipelineError(f"inspect {label}: {error}") from error
    if stat.S_ISLNK(raw.st_mode) or not stat.S_ISDIR(raw.st_mode):
        raise PipelineError(f"{label} must be a directory, not a symlink")
    resolved = path.resolve(strict=True)
    if resolved != path:
        raise PipelineError(f"{label} path must already be canonical")
    return resolved


def load_package_inputs(
    args: argparse.Namespace,
    config: Mapping[str, Any],
) -> Tuple[Optional[Dict[str, Any]], bytes, List[Tuple[Dict[str, str], Path]]]:
    profiles = config["normalization"]["profiles"]
    if args.profile not in profiles:
        raise PipelineError(f"unknown customization profile: {args.profile}")
    policy = profiles[args.profile]
    package_format = policy["package_format"]
    if package_format is None:
        if args.package_lock is not None or args.package_cache is not None:
            raise PipelineError(f"profile {args.profile} does not accept package inputs")
        return None, b"", []
    if args.package_lock is None or args.package_cache is None:
        raise PipelineError(f"profile {args.profile} requires --package-lock and --package-cache")

    lock_path = safe_input_file(args.package_lock, 1 << 20, "package lock")
    cache = safe_input_directory(args.package_cache, "package cache")
    try:
        lock_bytes = lock_path.read_bytes()
        lock = json.loads(lock_bytes)
    except (OSError, json.JSONDecodeError) as error:
        raise PipelineError(f"load package lock: {error}") from error
    if not isinstance(lock, dict) or set(lock) != {"arch", "install", "packages", "profile", "schema"}:
        raise PipelineError("package lock has an invalid envelope")
    if canonical_json(lock) != lock_bytes:
        raise PipelineError("package lock must use canonical pipeline JSON formatting")
    if lock.get("schema") != 1 or lock.get("profile") != args.profile or lock.get("arch") != args.arch:
        raise PipelineError("package lock schema/profile/architecture does not match the invocation")
    required = policy["required_packages"]
    if lock.get("install") != required:
        raise PipelineError(f"package lock install roots must be exactly {required!r}")
    records = lock.get("packages")
    if not isinstance(records, list) or not records or len(records) > 64:
        raise PipelineError("package lock must contain 1..64 package records")

    seen_names = set()
    seen_filenames = set()
    inputs: List[Tuple[Dict[str, str], Path]] = []
    total_bytes = 0
    for index, raw_record in enumerate(records):
        if not isinstance(raw_record, dict) or set(raw_record) != {"arch", "filename", "name", "sha256", "url", "version"}:
            raise PipelineError(f"package lock record {index} has invalid fields")
        record: Dict[str, str] = {}
        for key in ("arch", "filename", "name", "sha256", "url", "version"):
            value = raw_record.get(key)
            if not isinstance(value, str):
                raise PipelineError(f"package lock record {index} field {key} must be a string")
            record[key] = value
        if not PACKAGE_NAME_RE.fullmatch(record["name"]):
            raise PipelineError(f"package lock record {index} has an invalid package name")
        validate_single_line(f"package lock record {index} version", record["version"], 256)
        if record["arch"] not in (args.arch, "all", "noarch"):
            raise PipelineError(f"package lock record {index} architecture does not match {args.arch}")
        if not PACKAGE_FILENAME_RE.fullmatch(record["filename"]) or not record["filename"].endswith(f".{package_format}"):
            raise PipelineError(f"package lock record {index} has an invalid {package_format} filename")
        if not SHA256_RE.fullmatch(record["sha256"]):
            raise PipelineError(f"package lock record {index} has an invalid SHA-256")
        validate_immutable_https(f"package lock record {index} URL", record["url"])
        if record["filename"] in seen_filenames:
            raise PipelineError(f"package lock repeats filename {record['filename']}")
        if record["name"] in seen_names:
            raise PipelineError(f"package lock repeats package name {record['name']}")
        seen_names.add(record["name"])
        seen_filenames.add(record["filename"])
        package_path = safe_input_file(
            str(cache / record["filename"]),
            int(config["max_package_bytes"]),
            f"package {record['filename']}",
        )
        total_bytes += package_path.stat().st_size
        if total_bytes > int(config["max_package_bundle_bytes"]):
            raise PipelineError("package bundle exceeds the recipe size bound")
        inputs.append((record, package_path))
    missing = [name for name in required if name not in seen_names]
    if missing:
        raise PipelineError(f"package lock is missing install roots: {missing!r}")
    if seen_names != set(policy["locked_packages"]):
        raise PipelineError(
            f"package lock names must be exactly the minimal closure {policy['locked_packages']!r}"
        )
    return lock, lock_bytes, inputs


def safe_output(path_text: str) -> Tuple[Path, Path]:
    path = Path(path_text)
    if not path.is_absolute() or path.name in ("", ".", ".."):
        raise PipelineError("output must be a new absolute directory path")
    if path.resolve(strict=False) != path:
        raise PipelineError("output path must already be canonical and contain no symlinked parent")
    try:
        parent = path.parent.resolve(strict=True)
    except OSError as error:
        raise PipelineError(f"resolve output parent: {error}") from error
    if not parent.is_dir():
        raise PipelineError("output parent must be an existing directory")
    final = parent / path.name
    if os.path.lexists(final):
        raise PipelineError(f"output already exists: {final}")
    return final, parent


def stable_stat(before: os.stat_result, after: os.stat_result) -> bool:
    fields = ("st_dev", "st_ino", "st_mode", "st_nlink", "st_size", "st_mtime_ns", "st_ctime_ns")
    return all(getattr(before, field) == getattr(after, field) for field in fields)


def safe_copy(source: Path, destination: Path, expected: str, chunk_bytes: int) -> int:
    read_flags = os.O_RDONLY
    for optional in ("O_CLOEXEC", "O_NOFOLLOW"):
        read_flags |= getattr(os, optional, 0)
    write_flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    write_flags |= getattr(os, "O_CLOEXEC", 0)
    source_fd = os.open(source, read_flags)
    destination_fd: Optional[int] = None
    digest = hashlib.sha256()
    copied = 0
    try:
        before = os.fstat(source_fd)
        if not stat.S_ISREG(before.st_mode):
            raise PipelineError("opened source is not a regular file")
        destination_fd = os.open(destination, write_flags, 0o600)
        while True:
            block = os.read(source_fd, chunk_bytes)
            if not block:
                break
            digest.update(block)
            copied += len(block)
            view = memoryview(block)
            while view:
                written = os.write(destination_fd, view)
                if written <= 0:
                    raise PipelineError("short write while staging source")
                view = view[written:]
        os.fsync(destination_fd)
        after = os.fstat(source_fd)
        if not stable_stat(before, after) or copied != before.st_size:
            raise PipelineError("source changed while it was being copied")
    finally:
        if destination_fd is not None:
            os.close(destination_fd)
        os.close(source_fd)
    actual = digest.hexdigest()
    if actual != expected:
        raise PipelineError(f"source SHA-256 mismatch: got {actual}, expected {expected}")
    copied_digest, copied_size = hash_file(destination, chunk_bytes)
    if copied_digest != expected or copied_size != copied:
        raise PipelineError("staged copy did not re-verify against the immutable source digest")
    return copied


def bounded_message(data: bytes) -> str:
    text = data.decode("utf-8", errors="replace").strip().replace("\x00", "?")
    return text[-4000:]


def tool_environment(work: Path, epoch: int) -> Dict[str, str]:
    home = work / ".tool-home"
    cache = work / ".libguestfs-cache"
    temporary = work / ".tool-tmp"
    for directory in (home, cache, temporary):
        directory.mkdir(mode=0o700, exist_ok=True)
    environment = {
        "HOME": str(home),
        "LANG": "C",
        "LC_ALL": "C",
        "LIBGUESTFS_BACKEND": "direct",
        "LIBGUESTFS_CACHEDIR": str(cache),
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "SOURCE_DATE_EPOCH": str(epoch),
        "TMPDIR": str(temporary),
        "TZ": "UTC",
    }
    backend_settings = os.environ.get("LIBGUESTFS_BACKEND_SETTINGS", "")
    if backend_settings:
        if backend_settings != "force_tcg":
            raise PipelineError("LIBGUESTFS_BACKEND_SETTINGS may only be force_tcg")
        environment["LIBGUESTFS_BACKEND_SETTINGS"] = backend_settings
    return environment


def run_tool(arguments: Sequence[str], timeout: int, environment: Mapping[str, str]) -> bytes:
    try:
        completed = subprocess.run(
            list(arguments),
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            timeout=timeout,
            env=dict(environment),
        )
    except subprocess.TimeoutExpired as error:
        raise PipelineError(f"tool timed out after {timeout}s: {Path(arguments[0]).name}") from error
    except OSError as error:
        raise PipelineError(f"execute {Path(arguments[0]).name}: {error}") from error
    if completed.returncode != 0:
        detail = bounded_message(completed.stderr) or bounded_message(completed.stdout) or "no diagnostic"
        raise PipelineError(f"{Path(arguments[0]).name} exited {completed.returncode}: {detail}")
    return completed.stdout


def tool_version(tool: Path, timeout: int, environment: Mapping[str, str]) -> str:
    output = run_tool((str(tool), "--version"), timeout, environment)
    lines = [line.strip() for line in output.decode("utf-8", errors="replace").splitlines() if line.strip()]
    if not lines:
        raise PipelineError(f"{tool.name} did not report a version")
    return validate_single_line(f"{tool.name} version", lines[0], 256)


def parse_json_output(label: str, data: bytes) -> Any:
    try:
        return json.loads(data)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise PipelineError(f"decode {label} JSON: {error}") from error


def is_truthy_feature(value: Any) -> bool:
    return value not in (None, False, 0, "", [], {})


def reject_unsafe_features(value: Any, location: str = "qemu-img info") -> None:
    if isinstance(value, dict):
        for raw_key, child in value.items():
            key = str(raw_key).lower().replace("_", "-")
            unsafe = (
                "backing" in key
                or key == "data-file"
                or key.startswith("data-file-")
                or "encrypt" in key
                or "incompatible" in key
                or key in {"extended-l2", "corrupt", "dirty-flag"}
            )
            if unsafe and is_truthy_feature(child):
                raise PipelineError(f"unsafe qcow2 feature {location}.{raw_key}={child!r}")
            reject_unsafe_features(child, f"{location}.{raw_key}")
    elif isinstance(value, list):
        for index, child in enumerate(value):
            reject_unsafe_features(child, f"{location}[{index}]")


def validate_info(info: Any) -> Dict[str, Any]:
    if not isinstance(info, dict):
        raise PipelineError("qemu-img info did not return one JSON object")
    if info.get("format") != "qcow2":
        raise PipelineError(f"qemu-img format is {info.get('format')!r}, want qcow2")
    virtual_size = info.get("virtual-size")
    if isinstance(virtual_size, bool) or not isinstance(virtual_size, int) or virtual_size <= 0:
        raise PipelineError(f"qemu-img virtual size is invalid: {virtual_size!r}")
    format_specific = info.get("format-specific")
    if format_specific is not None:
        if not isinstance(format_specific, dict) or format_specific.get("type") != "qcow2":
            raise PipelineError("qemu-img format-specific type is not qcow2")
    reject_unsafe_features(info)
    summary: Dict[str, Any] = {"format": "qcow2", "virtual_size": virtual_size}
    for source_key, output_key in (("cluster-size", "cluster_size"), ("dirty-flag", "dirty_flag")):
        if source_key in info:
            summary[output_key] = info[source_key]
    if format_specific is not None:
        summary["format_specific"] = format_specific
    return summary


def inspect_image(
    qemu_img: Path,
    image: Path,
    timeout: int,
    environment: Mapping[str, str],
) -> Dict[str, Any]:
    info_output = run_tool(
        (str(qemu_img), "info", "--output=json", "-f", "qcow2", str(image)),
        timeout,
        environment,
    )
    info = parse_json_output("qemu-img info", info_output)
    summary = validate_info(info)
    chain_output = run_tool(
        (str(qemu_img), "info", "--output=json", "--backing-chain", "-f", "qcow2", str(image)),
        timeout,
        environment,
    )
    chain = parse_json_output("qemu-img backing chain", chain_output)
    if not isinstance(chain, list) or len(chain) != 1:
        length = len(chain) if isinstance(chain, list) else "non-list"
        raise PipelineError(f"managed base backing chain length is {length}, want exactly 1")
    chain_summary = validate_info(chain[0])
    if chain_summary["virtual_size"] != summary["virtual_size"]:
        raise PipelineError("qemu-img info and backing-chain virtual sizes disagree")
    check_output = run_tool(
        (str(qemu_img), "check", "--output=json", "-f", "qcow2", str(image)),
        timeout,
        environment,
    )
    check = parse_json_output("qemu-img check", check_output)
    if not isinstance(check, dict):
        raise PipelineError("qemu-img check did not return one JSON object")
    for key in ("corruptions", "check-errors"):
        value = check.get(key, 0)
        if isinstance(value, bool) or not isinstance(value, int) or value != 0:
            raise PipelineError(f"qemu-img check reported {key}={value!r}")
    return {
        "check": {"check_errors": check.get("check-errors", 0), "corruptions": check.get("corruptions", 0)},
        "info": summary,
        "one_element_backing_chain": True,
    }


def normalize_offline(
    virt_customize: Path,
    virt_cat: Path,
    image: Path,
    guest_script: Path,
    source_user: str,
    profile: str,
    packages: Sequence[Path],
    epoch: int,
    timeouts: Mapping[str, int],
    environment: Mapping[str, str],
) -> Dict[str, Any]:
    remote_script = "/usr/local/sbin/barn-image-normalize"
    remote_packages = "/var/tmp/barn-image-packages" if packages else "-"
    command = f"{remote_script} {source_user} {epoch} {profile} {remote_packages}"
    arguments = [
        str(virt_customize),
        "--format=qcow2",
        "-a",
        str(image),
        "--no-network",
        "--upload",
        f"{guest_script}:{remote_script}",
        "--chmod",
        f"0700:{remote_script}",
    ]
    if packages:
        arguments.extend(("--mkdir", remote_packages))
        for package in packages:
            arguments.extend(("--upload", f"{package}:{remote_packages}/{package.name}"))
    arguments.extend(("--run-command", command, "--delete", remote_script))
    run_tool(arguments, timeouts["virt_customize"], environment)
    marker_output = run_tool(
        (
            str(virt_cat),
            "--format=qcow2",
            "-a",
            str(image),
            "/var/lib/barn-image/normalization.json",
        ),
        timeouts["virt_cat"],
        environment,
    )
    marker = parse_json_output("guest normalization marker", marker_output)
    expected = {
        "admin_gid": 88,
        "credential_hygiene": "applied",
        "dba_uid": 88,
        "legacy_network": "removed" if profile in ("el8", "el9") else "not-requested",
        "locale_default": "C.UTF-8" if profile in ("d12", "d13") else "not-requested",
        "locale_en_us": "verified" if profile in ("d12", "d13") else "not-requested",
        "profile": profile,
        "python3": "verified" if profile == "el8" else "not-requested",
        "recipe": "barn-official-image-normalization-v1",
        "schema": 1,
        "sshd_include": "verified" if profile == "el8" else "upstream",
        "source_date_epoch": epoch,
        "source_user": source_user,
        "xfsprogs": "verified" if profile in ("d12", "d13") else "not-requested",
    }
    if marker != expected:
        raise PipelineError(f"guest normalization marker mismatch: {marker!r}")
    passwd = run_tool(
        (str(virt_cat), "--format=qcow2", "-a", str(image), "/etc/passwd"),
        timeouts["virt_cat"],
        environment,
    ).decode("utf-8", errors="strict")
    groups = run_tool(
        (str(virt_cat), "--format=qcow2", "-a", str(image), "/etc/group"),
        timeouts["virt_cat"],
        environment,
    ).decode("utf-8", errors="strict")
    dba_rows = [line.split(":") for line in passwd.splitlines() if line.startswith("dba:")]
    admin_rows = [line.split(":") for line in groups.splitlines() if line.startswith("admin:")]
    if len(dba_rows) != 1 or len(dba_rows[0]) < 7 or dba_rows[0][2:4] != ["88", "88"]:
        raise PipelineError("offline identity verification did not find exactly dba UID/GID 88")
    if dba_rows[0][5:7] != ["/home/dba", "/bin/bash"]:
        raise PipelineError("offline identity verification found an invalid dba home or shell")
    if len(admin_rows) != 1 or len(admin_rows[0]) < 4 or admin_rows[0][2] != "88":
        raise PipelineError("offline identity verification did not find exactly admin GID 88")
    return marker


def write_bytes(path: Path, data: bytes, epoch: int, mode: int = 0o644) -> None:
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_CLOEXEC", 0)
    descriptor = os.open(path, flags, mode)
    try:
        view = memoryview(data)
        while view:
            count = os.write(descriptor, view)
            if count <= 0:
                raise PipelineError(f"short write for {path.name}")
            view = view[count:]
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    os.chmod(path, mode)
    os.utime(path, (epoch, epoch), follow_symlinks=False)


def write_json(path: Path, value: Any, epoch: int) -> None:
    write_bytes(path, canonical_json(value), epoch)


def artifact_file_name(name: str, release: str, arch: str, digest: str) -> str:
    return f"{name}-{release}-{arch}-{digest[:16]}.qcow2"


def build_manifest(
    args: argparse.Namespace,
    artifact_url: str,
    output_digest: str,
    output_size: int,
    virtual_size: int,
    mode: str,
) -> Dict[str, Any]:
    if mode == "offline":
        source_user = "dba"
        locale_note = (
            " en_US.UTF-8 generated while C.UTF-8 remains the default;"
            if args.profile in ("d12", "d13")
            else ""
        )
        provenance = (
            f"Barn offline normalization recipe v1 profile {args.profile} from {args.source_uri} "
            f"sha256:{args.expected_sha256}; digest-locked offline package inputs applied where required; "
            f"credential hygiene and dba UID/GID 88 applied;{locale_note} "
            "candidate requires owner hosting, signing, and native smoke"
        )
    else:
        source_user = args.source_user
        provenance = (
            f"UNPUBLISHABLE VALIDATION-ONLY copy of {args.source_uri} sha256:{args.expected_sha256}; "
            "credential normalization was not run"
        )
    return {
        "generated_at": fixed_time(args.source_date_epoch),
        "images": {
            args.name: {
                "releases": {
                    args.release: {
                        args.arch: {
                            "artifact_size": output_size,
                            "boot": args.boot,
                            "format": "qcow2",
                            "provenance": provenance,
                            "sha256": output_digest,
                            "source_user": source_user,
                            "status": "testing",
                            "url": artifact_url,
                            "virtual_size": virtual_size,
                        }
                    }
                }
            }
        },
        "schema": 1,
        "version": args.manifest_version,
    }


def build_sbom(
    args: argparse.Namespace,
    artifact_name: str,
    output_digest: str,
    output_size: int,
    mode: str,
    package_records: Sequence[Mapping[str, str]],
) -> Dict[str, Any]:
    created = fixed_time(args.source_date_epoch)
    normalized_comment = (
        "Offline credential/identity normalization completed and every build package input is digest-locked here. "
        "This artifact-boundary SBOM does not claim a complete guest package inventory; collect one before promotion."
        if mode == "offline"
        else "Validation-only byte copy; no guest mutation was run. Guest package inventory is not asserted."
    )
    packages = [
        {
            "SPDXID": "SPDXRef-Package-SourceImage",
            "checksums": [{"algorithm": "SHA256", "checksumValue": args.expected_sha256}],
            "copyrightText": "NOASSERTION",
            "downloadLocation": args.source_uri,
            "filesAnalyzed": False,
            "licenseConcluded": "NOASSERTION",
            "licenseDeclared": args.license,
            "name": f"upstream-{args.name}-{args.release}-{args.arch}",
            "versionInfo": args.release,
        },
        {
            "SPDXID": "SPDXRef-Package-NormalizedImage",
            "checksums": [{"algorithm": "SHA256", "checksumValue": output_digest}],
            "comment": normalized_comment,
            "copyrightText": "NOASSERTION",
            "downloadLocation": "NOASSERTION",
            "filesAnalyzed": False,
            "hasFiles": ["SPDXRef-File-QCOW2"],
            "licenseConcluded": "NOASSERTION",
            "licenseDeclared": args.license,
            "name": f"barn-{args.name}-{args.release}-{args.arch}",
            "packageFileName": artifact_name,
            "versionInfo": args.release,
        },
    ]
    relationships = [
        {
            "relatedSpdxElement": "SPDXRef-Package-NormalizedImage",
            "relationshipType": "DESCRIBES",
            "spdxElementId": "SPDXRef-DOCUMENT",
        },
        {
            "relatedSpdxElement": "SPDXRef-Package-SourceImage",
            "relationshipType": "GENERATED_FROM",
            "spdxElementId": "SPDXRef-Package-NormalizedImage",
        },
        {
            "relatedSpdxElement": "SPDXRef-File-QCOW2",
            "relationshipType": "CONTAINS",
            "spdxElementId": "SPDXRef-Package-NormalizedImage",
        },
    ]
    for index, record in enumerate(package_records, start=1):
        identifier = f"SPDXRef-Package-BuildInput-{index}"
        packages.append(
            {
                "SPDXID": identifier,
                "checksums": [{"algorithm": "SHA256", "checksumValue": record["sha256"]}],
                "copyrightText": "NOASSERTION",
                "downloadLocation": record["url"],
                "filesAnalyzed": False,
                "licenseConcluded": "NOASSERTION",
                "licenseDeclared": "NOASSERTION",
                "name": record["name"],
                "packageFileName": record["filename"],
                "versionInfo": record["version"],
            }
        )
        relationships.append(
            {
                "relatedSpdxElement": identifier,
                "relationshipType": "GENERATED_FROM",
                "spdxElementId": "SPDXRef-Package-NormalizedImage",
            }
        )
    return {
        "SPDXID": "SPDXRef-DOCUMENT",
        "creationInfo": {"created": created, "creators": [f"Tool: barn-image-pipeline-{PIPELINE_VERSION}"]},
        "dataLicense": "CC0-1.0",
        "documentDescribes": ["SPDXRef-Package-NormalizedImage"],
        "documentNamespace": f"https://github.com/pgsty/barn/sbom/image/{output_digest}",
        "files": [
            {
                "SPDXID": "SPDXRef-File-QCOW2",
                "checksums": [{"algorithm": "SHA256", "checksumValue": output_digest}],
                "copyrightText": "NOASSERTION",
                "fileName": artifact_name,
                "licenseConcluded": "NOASSERTION",
                "licenseInfoInFiles": ["NOASSERTION"],
            }
        ],
        "name": f"barn-image-{args.name}-{args.release}-{args.arch}",
        "packages": packages,
        "relationships": relationships,
        "spdxVersion": "SPDX-2.3",
    }


def build_provenance(
    args: argparse.Namespace,
    artifact_name: str,
    output_digest: str,
    output_size: int,
    recipe_digest: str,
    config_digest: str,
    guest_script_digest: str,
    package_lock_digest: Optional[str],
    package_records: Sequence[Mapping[str, str]],
    tools: Mapping[str, str],
    mode: str,
) -> Dict[str, Any]:
    parameters = {
        "arch": args.arch,
        "boot": args.boot,
        "customizationProfile": args.profile,
        "expectedSourceSha256": args.expected_sha256,
        "license": args.license,
        "mode": mode,
        "name": args.name,
        "packageLockSha256": package_lock_digest,
        "recipeSha256": recipe_digest,
        "release": args.release,
        "sourceDateEpoch": args.source_date_epoch,
        "sourceUser": args.source_user,
    }
    invocation_id = sha256_bytes(canonical_json({"parameters": parameters, "subject": output_digest, "tools": tools}))
    timestamp = fixed_time(args.source_date_epoch)
    resolved_dependencies = [
        {"digest": {"sha256": args.expected_sha256}, "uri": args.source_uri},
        {
            "digest": {"sha256": config_digest},
            "uri": "git+https://github.com/pgsty/barn@packaging/image-pipeline/recipe-v1.json",
        },
        {
            "digest": {"sha256": guest_script_digest},
            "uri": "git+https://github.com/pgsty/barn@packaging/image-pipeline/normalize-guest.sh",
        },
    ]
    if package_lock_digest is not None:
        resolved_dependencies.append(
            {"digest": {"sha256": package_lock_digest}, "uri": f"urn:sha256:{package_lock_digest}"}
        )
    for record in package_records:
        resolved_dependencies.append({"digest": {"sha256": record["sha256"]}, "uri": record["url"]})
    return {
        "_type": "https://in-toto.io/Statement/v1",
        "predicate": {
            "buildDefinition": {
                "buildType": "https://github.com/pgsty/barn/packaging/image-pipeline/v1",
                "externalParameters": parameters,
                "internalParameters": {
                    "networkDuringMutation": False,
                    "outputBytes": output_size,
                    "packageArtifactCount": len(package_records),
                    "toolVersions": dict(tools),
                },
                "resolvedDependencies": resolved_dependencies,
            },
            "runDetails": {
                "builder": {"id": "https://github.com/pgsty/barn/packaging/image-pipeline/pipeline.py"},
                "metadata": {
                    "finishedOn": timestamp,
                    "invocationId": f"urn:sha256:{invocation_id}",
                    "startedOn": timestamp,
                },
            },
        },
        "predicateType": "https://slsa.dev/provenance/v1",
        "subject": [{"digest": {"sha256": output_digest}, "name": artifact_name}],
    }


def write_checksums(directory: Path, epoch: int, chunk_bytes: int) -> None:
    records: List[str] = []
    for path in sorted(directory.iterdir(), key=lambda item: item.name.encode("utf-8")):
        if not path.is_file() or path.name == "checksums.txt":
            continue
        digest, _ = hash_file(path, chunk_bytes)
        records.append(f"{digest}  {path.name}\n")
    write_bytes(directory / "checksums.txt", "".join(records).encode("ascii"), epoch)


def fsync_directory(path: Path) -> None:
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(
        description="Validate or offline-normalize one immutable qcow2 source into a checksummed candidate bundle."
    )
    result.add_argument("--source", required=True, help="absolute canonical regular qcow2 source path")
    result.add_argument("--expected-sha256", required=True, help="independently obtained lowercase source digest")
    result.add_argument("--output", required=True, help="new absolute output directory")
    result.add_argument("--mode", choices=("validate", "offline"), default="validate")
    result.add_argument("--name", required=True, help="canonical manifest image name, for example u24")
    result.add_argument("--release", required=True, help="immutable release identifier")
    result.add_argument("--arch", choices=("amd64", "arm64"), required=True)
    result.add_argument("--source-user", required=True, help="upstream bootstrap account")
    result.add_argument("--source-uri", help="immutable HTTPS provenance URL; defaults to a digest URN")
    result.add_argument("--artifact-url", required=True, help="candidate HTTPS URL; optional {sha256} path template")
    result.add_argument("--boot", choices=("bios", "uefi"), required=True)
    result.add_argument("--license", required=True, help="source SPDX license expression or NOASSERTION")
    result.add_argument("--source-date-epoch", required=True, type=int)
    result.add_argument("--manifest-version", required=True, type=int)
    result.add_argument(
        "--profile",
        choices=("base", "el8", "el9", "d12", "d13"),
        default="base",
        help="bounded guest customization profile (default: base normalization only)",
    )
    result.add_argument("--package-lock", help="canonical JSON lock for the profile's offline package closure")
    result.add_argument("--package-cache", help="absolute directory containing the digest-locked package files")
    result.add_argument("--qemu-img", default=os.environ.get("QEMU_IMG", "qemu-img"))
    result.add_argument("--virt-customize", default=os.environ.get("VIRT_CUSTOMIZE", "virt-customize"))
    result.add_argument("--virt-cat", default=os.environ.get("VIRT_CAT", "virt-cat"))
    return result


def validate_arguments(args: argparse.Namespace, config: Mapping[str, Any]) -> Tuple[Path, Path, Path]:
    if not SHA256_RE.fullmatch(args.expected_sha256):
        raise PipelineError("expected SHA-256 must be 64 lowercase hexadecimal characters")
    if not NAME_RE.fullmatch(args.name):
        raise PipelineError("manifest image name must match [a-z][a-z0-9-]{0,31}")
    if not RELEASE_RE.fullmatch(args.release):
        raise PipelineError("release contains unsafe or unsupported characters")
    if not USER_RE.fullmatch(args.source_user):
        raise PipelineError("source user is invalid")
    validate_single_line("license", args.license, 256)
    if args.source_date_epoch < 0 or args.source_date_epoch > 4_102_444_800:
        raise PipelineError("source date epoch must be between 1970 and 2100 UTC")
    if args.manifest_version <= 0 or args.manifest_version > 18_446_744_073_709_551_615:
        raise PipelineError("manifest version must be a positive uint64")
    if args.mode == "validate" and args.profile != "base":
        raise PipelineError("validation-only mode accepts only the base profile")
    args.source_uri = validate_source_uri(args.source_uri, args.expected_sha256)
    validate_immutable_https("artifact URL", args.artifact_url, allow_digest_template=True)
    source = safe_source(args.source, int(config["max_source_bytes"]))
    output, parent = safe_output(args.output)
    return source, output, parent


def pipeline(args: argparse.Namespace) -> Path:
    config, config_bytes, guest_script, guest_bytes = load_config()
    source, output, output_parent = validate_arguments(args, config)
    package_lock, package_lock_bytes, package_inputs = load_package_inputs(args, config)
    package_records = [record for record, _ in package_inputs]
    qemu_img = resolve_tool("qemu-img", args.qemu_img)
    virt_customize: Optional[Path] = None
    virt_cat: Optional[Path] = None
    if args.mode == "offline":
        virt_customize = resolve_tool("virt-customize", args.virt_customize)
        virt_cat = resolve_tool("virt-cat", args.virt_cat)

    lock_path = output_parent / f".{output.name}.image-pipeline.lock"
    lock_fd: Optional[int] = None
    temporary: Optional[Path] = None
    try:
        try:
            lock_fd = os.open(
                lock_path,
                os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_CLOEXEC", 0),
                0o600,
            )
        except OSError as error:
            raise PipelineError(f"acquire output lock {lock_path}: {error}") from error
        if os.path.lexists(output):
            raise PipelineError(f"output appeared while acquiring its lock: {output}")
        temporary = Path(tempfile.mkdtemp(prefix=f".{output.name}.partial-", dir=output_parent))
        os.chmod(temporary, 0o700)
        environment = tool_environment(temporary, args.source_date_epoch)
        timeouts = config["timeouts_seconds"]
        tools: Dict[str, str] = {
            "pipeline": f"barn-image-pipeline {PIPELINE_VERSION}",
            "qemu_img": tool_version(qemu_img, timeouts["qemu_img"], environment),
        }
        if args.mode == "offline":
            assert virt_customize is not None and virt_cat is not None
            tools["libguestfs_backend_settings"] = environment.get("LIBGUESTFS_BACKEND_SETTINGS", "default")
            tools["virt_customize"] = tool_version(
                virt_customize, timeouts["virt_customize"], environment
            )
            tools["virt_cat"] = tool_version(virt_cat, timeouts["virt_cat"], environment)

        staged = temporary / ".candidate.qcow2"
        source_size = safe_copy(
            source,
            staged,
            args.expected_sha256,
            int(config["copy_chunk_bytes"]),
        )
        before = inspect_image(qemu_img, staged, timeouts["qemu_img"], environment)
        staged_packages: List[Path] = []
        if package_inputs:
            package_staging = temporary / ".package-bundle"
            package_staging.mkdir(mode=0o700)
            for record, package_source in package_inputs:
                staged_package = package_staging / record["filename"]
                safe_copy(
                    package_source,
                    staged_package,
                    record["sha256"],
                    int(config["copy_chunk_bytes"]),
                )
                staged_packages.append(staged_package)
        marker: Optional[Dict[str, Any]] = None
        if args.mode == "offline":
            assert virt_customize is not None and virt_cat is not None
            marker = normalize_offline(
                virt_customize,
                virt_cat,
                staged,
                guest_script,
                args.source_user,
                args.profile,
                staged_packages,
                args.source_date_epoch,
                timeouts,
                environment,
            )
        after = inspect_image(qemu_img, staged, timeouts["qemu_img"], environment)
        output_digest, output_size = hash_file(staged, int(config["copy_chunk_bytes"]))
        if args.mode == "validate" and (output_digest != args.expected_sha256 or output_size != source_size):
            raise PipelineError("validation mode changed the staged source bytes")

        artifact_name = artifact_file_name(args.name, args.release, args.arch, output_digest)
        artifact = temporary / artifact_name
        os.rename(staged, artifact)
        os.chmod(artifact, 0o444)
        os.utime(artifact, (args.source_date_epoch, args.source_date_epoch), follow_symlinks=False)
        artifact_url = args.artifact_url.replace("{sha256}", output_digest)
        validate_immutable_https("rendered artifact URL", artifact_url)
        virtual_size = int(after["info"]["virtual_size"])
        config_digest = sha256_bytes(config_bytes)
        guest_script_digest = sha256_bytes(guest_bytes)
        package_lock_digest = sha256_bytes(package_lock_bytes) if package_lock is not None else None
        recipe_digest = sha256_bytes(config_bytes + b"\0" + guest_bytes + b"\0" + package_lock_bytes)
        manifest = build_manifest(
            args,
            artifact_url,
            output_digest,
            output_size,
            virtual_size,
            args.mode,
        )
        sbom = build_sbom(args, artifact_name, output_digest, output_size, args.mode, package_records)
        provenance = build_provenance(
            args,
            artifact_name,
            output_digest,
            output_size,
            recipe_digest,
            config_digest,
            guest_script_digest,
            package_lock_digest,
            package_records,
            tools,
            args.mode,
        )
        native_status = "completed" if args.mode == "offline" else "not-run-validation-only"
        validation = {
            "artifact": {"bytes": output_size, "name": artifact_name, "sha256": output_digest},
            "inspection_after": after,
            "inspection_before": before,
            "manifest_status": "testing",
            "native_mutation": {
                "marker": marker,
                "mode": args.mode,
                "status": native_status,
            },
            "package_inputs": {
                "count": len(package_records),
                "lock_sha256": package_lock_digest,
                "network_during_mutation": False,
                "records": package_records,
            },
            "promotion": {
                "eligible": False,
                "remaining_gates": (
                    ["offline credential/identity normalization", "owner-controlled hosting", "production signing", "native smoke"]
                    if args.mode == "validate"
                    else ["repeat-build comparison", "owner-controlled hosting", "production signing", "native smoke"]
                ),
            },
            "qemu_contract": "info --output=json -f qcow2; one-element --backing-chain; check --output=json",
            "recipe_sha256": recipe_digest,
            "schema": 1,
            "signing": {
                "performed": False,
                "reason": "pipeline accepts no signing key; production custody is a separate owner gate",
            },
            "source": {"bytes": source_size, "sha256": args.expected_sha256, "uri": args.source_uri},
            "tools": tools,
        }
        build_recipe = {
            "config": config,
            "config_sha256": config_digest,
            "guest_script": {"name": guest_script.name, "sha256": guest_script_digest},
            "invocation": {
                "arch": args.arch,
                "boot": args.boot,
                "expected_source_sha256": args.expected_sha256,
                "license": args.license,
                "manifest_version": args.manifest_version,
                "mode": args.mode,
                "name": args.name,
                "package_lock_sha256": package_lock_digest,
                "profile": args.profile,
                "release": args.release,
                "source_date_epoch": args.source_date_epoch,
                "source_uri": args.source_uri,
                "source_user": args.source_user,
            },
            "result": {
                "artifact": artifact_name,
                "artifact_sha256": output_digest,
                "native_mutation_status": native_status,
            },
            "schema": 1,
            "package_inputs": package_records,
            "tools": tools,
        }

        # Remove tool state before creating the auditable output inventory.
        for tool_directory in (
            temporary / ".tool-home",
            temporary / ".libguestfs-cache",
            temporary / ".tool-tmp",
            temporary / ".package-bundle",
        ):
            shutil.rmtree(tool_directory, ignore_errors=True)
        write_bytes(temporary / "normalize-guest.sh", guest_bytes, args.source_date_epoch, 0o555)
        if package_lock is not None:
            write_bytes(temporary / "package-lock.json", package_lock_bytes, args.source_date_epoch)
        write_json(temporary / "build-recipe.json", build_recipe, args.source_date_epoch)
        write_json(temporary / "manifest-candidate.json", manifest, args.source_date_epoch)
        write_json(temporary / "provenance.intoto.json", provenance, args.source_date_epoch)
        write_json(temporary / "sbom.spdx.json", sbom, args.source_date_epoch)
        write_json(temporary / "validation.json", validation, args.source_date_epoch)
        write_checksums(temporary, args.source_date_epoch, int(config["copy_chunk_bytes"]))
        os.utime(temporary, (args.source_date_epoch, args.source_date_epoch), follow_symlinks=False)
        fsync_directory(temporary)
        if os.path.lexists(output):
            raise PipelineError(f"refuse to replace output created concurrently: {output}")
        os.rename(temporary, output)
        temporary = None
        os.chmod(output, 0o755)
        os.utime(output, (args.source_date_epoch, args.source_date_epoch), follow_symlinks=False)
        fsync_directory(output_parent)
        return output
    finally:
        if temporary is not None:
            expected_prefix = f".{output.name}.partial-"
            if temporary.parent == output_parent and temporary.name.startswith(expected_prefix):
                shutil.rmtree(temporary, ignore_errors=True)
        if lock_fd is not None:
            os.close(lock_fd)
        try:
            lock_path.unlink()
        except FileNotFoundError:
            pass
        except OSError:
            pass


def main(argv: Optional[Sequence[str]] = None) -> int:
    args = parser().parse_args(argv)
    try:
        output = pipeline(args)
    except PipelineError as error:
        print(f"image-pipeline: ERROR: {error}", file=sys.stderr)
        if getattr(args, "mode", None) == "offline":
            print("image-pipeline: NATIVE MUTATION NOT COMPLETED; no candidate bundle was published", file=sys.stderr)
        return 1
    if args.mode == "offline":
        print(f"image-pipeline: offline normalization completed; unsigned testing candidate: {output}")
    else:
        print(f"image-pipeline: validation completed; NATIVE MUTATION NOT RUN; unpublishable bundle: {output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
