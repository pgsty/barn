#!/usr/bin/env python3
"""Build the eight digest-pinned Barn official-image candidates."""

from __future__ import annotations

import argparse
import errno
import hashlib
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import urllib.request
from pathlib import Path
from typing import Any, Dict, Iterable, Mapping, Optional, Sequence, Tuple


HERE = Path(__file__).resolve().parent
MATRIX_PATH = HERE / "official-v1.json"
PIPELINE = HERE / "build.sh"
TARGETS = {
    "d12/amd64",
    "d12/arm64",
    "d13/amd64",
    "d13/arm64",
    "el8/amd64",
    "el8/arm64",
    "el9/amd64",
    "el9/arm64",
}
MAX_SOURCE_BYTES = 16 << 30
MAX_PACKAGE_BYTES = 512 << 20
ALIASES = {
    "d12": ["debian12", "bookworm"],
    "d13": ["debian13", "debian", "trixie"],
    "el8": ["rocky8"],
    "el9": ["rocky9", "rocky"],
}


class OfficialBuildError(RuntimeError):
    pass


def canonical_json(value: Any) -> bytes:
    return (json.dumps(value, indent=2, sort_keys=True, separators=(",", ": ")) + "\n").encode("utf-8")


def hash_file(path: Path, algorithms: Iterable[str]) -> Dict[str, str]:
    digests = {name: hashlib.new(name) for name in algorithms}
    with path.open("rb", buffering=0) as handle:
        while True:
            block = handle.read(8 << 20)
            if not block:
                break
            for digest in digests.values():
                digest.update(block)
    return {name: digest.hexdigest() for name, digest in digests.items()}


def safe_directory(value: str, label: str) -> Path:
    path = Path(value)
    if not path.is_absolute() or path.resolve(strict=True) != path:
        raise OfficialBuildError(f"{label} must be an existing canonical absolute directory")
    info = os.lstat(path)
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise OfficialBuildError(f"{label} must be a directory, not a symlink")
    return path


def safe_new_directory(value: str, label: str) -> Tuple[Path, Path]:
    path = Path(value)
    if not path.is_absolute() or path.name in ("", ".", "..") or path.resolve(strict=False) != path:
        raise OfficialBuildError(f"{label} must be a new canonical absolute directory")
    parent = path.parent.resolve(strict=True)
    if os.path.lexists(path):
        raise OfficialBuildError(f"{label} already exists: {path}")
    return path, parent


def load_matrix() -> Tuple[Dict[str, Any], Dict[str, Dict[str, Any]]]:
    try:
        data = MATRIX_PATH.read_bytes()
        matrix = json.loads(data)
    except (OSError, json.JSONDecodeError) as error:
        raise OfficialBuildError(f"load official matrix: {error}") from error
    if canonical_json(matrix) != data:
        raise OfficialBuildError("official matrix must use canonical pipeline JSON formatting")
    if not isinstance(matrix, dict) or set(matrix) != {
        "artifact_base_url",
        "manifest_version",
        "schema",
        "source_date_epoch",
        "targets",
    }:
        raise OfficialBuildError("official matrix has an invalid envelope")
    if matrix.get("schema") != 1 or not isinstance(matrix.get("targets"), list):
        raise OfficialBuildError("official matrix has an unsupported schema")
    indexed: Dict[str, Dict[str, Any]] = {}
    target_fields = {
        "arch",
        "boot",
        "license",
        "name",
        "package_lock",
        "profile",
        "release",
        "source_filename",
        "source_sha256",
        "source_sha512",
        "source_uri",
    }
    for target in matrix["targets"]:
        if not isinstance(target, dict) or set(target) != target_fields:
            raise OfficialBuildError("official matrix target has invalid fields")
        key = f"{target['name']}/{target['arch']}"
        if key in indexed:
            raise OfficialBuildError(f"official matrix repeats target {key}")
        indexed[key] = target
    if set(indexed) != TARGETS:
        raise OfficialBuildError(f"official matrix target set is {sorted(indexed)}, want {sorted(TARGETS)}")
    return matrix, indexed


def verify_regular(path: Path, maximum: int, expected: Mapping[str, str], label: str) -> None:
    try:
        before = os.lstat(path)
    except OSError as error:
        raise OfficialBuildError(f"inspect {label}: {error}") from error
    if stat.S_ISLNK(before.st_mode) or not stat.S_ISREG(before.st_mode) or before.st_size <= 0 or before.st_size > maximum:
        raise OfficialBuildError(f"{label} is not a bounded regular non-symlink file")
    actual = hash_file(path, expected.keys())
    after = os.lstat(path)
    if (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns, before.st_ctime_ns) != (
        after.st_dev,
        after.st_ino,
        after.st_size,
        after.st_mtime_ns,
        after.st_ctime_ns,
    ):
        raise OfficialBuildError(f"{label} changed while hashing")
    for algorithm, digest in expected.items():
        if actual[algorithm] != digest:
            raise OfficialBuildError(f"{label} {algorithm} mismatch: got {actual[algorithm]}, expected {digest}")


def fetch_locked(url: str, destination: Path, maximum: int, expected: Mapping[str, str]) -> None:
    if destination.exists():
        verify_regular(destination, maximum, expected, destination.name)
        return
    partial = destination.parent / f".{destination.name}.partial-{os.getpid()}"
    if os.path.lexists(partial):
        raise OfficialBuildError(f"refuse existing partial download: {partial}")
    request = urllib.request.Request(url, headers={"User-Agent": "barn-official-image-builder/1"})
    size = 0
    digests = {name: hashlib.new(name) for name in expected}
    try:
        with urllib.request.urlopen(request, timeout=60) as response, partial.open("xb", buffering=0) as output:
            while True:
                block = response.read(8 << 20)
                if not block:
                    break
                size += len(block)
                if size > maximum:
                    raise OfficialBuildError(f"download exceeds size bound: {url}")
                for digest in digests.values():
                    digest.update(block)
                output.write(block)
            output.flush()
            os.fsync(output.fileno())
        for algorithm, wanted in expected.items():
            actual = digests[algorithm].hexdigest()
            if actual != wanted:
                raise OfficialBuildError(f"downloaded {destination.name} {algorithm} mismatch: got {actual}, expected {wanted}")
        try:
            os.link(partial, destination)
        except FileExistsError as error:
            raise OfficialBuildError(f"download destination appeared concurrently: {destination}") from error
        partial.unlink()
    except Exception:
        try:
            partial.unlink()
        except FileNotFoundError:
            pass
        raise


def ensure_input(
    destination: Path,
    url: str,
    maximum: int,
    expected: Mapping[str, str],
    fetch: bool,
) -> None:
    if destination.exists():
        verify_regular(destination, maximum, expected, destination.name)
        return
    if not fetch:
        raise OfficialBuildError(f"missing locked input {destination}; rerun with --fetch or preseed the cache")
    fetch_locked(url, destination, maximum, expected)


def select_targets(requested: Sequence[str], indexed: Mapping[str, Dict[str, Any]]) -> Sequence[Tuple[str, Dict[str, Any]]]:
    keys = sorted(indexed) if not requested else list(requested)
    unknown = [key for key in keys if key not in indexed]
    if unknown:
        raise OfficialBuildError(f"unknown target(s): {', '.join(unknown)}")
    if len(set(keys)) != len(keys):
        raise OfficialBuildError("a target was requested more than once")
    return [(key, indexed[key]) for key in keys]


def build_target(
    matrix: Mapping[str, Any],
    key: str,
    target: Mapping[str, Any],
    source_cache: Path,
    package_cache: Path,
    output: Path,
    fetch: bool,
) -> None:
    source = source_cache / target["source_filename"]
    source_hashes = {"sha256": target["source_sha256"]}
    if target["source_sha512"] is not None:
        source_hashes["sha512"] = target["source_sha512"]
    ensure_input(source, target["source_uri"], MAX_SOURCE_BYTES, source_hashes, fetch)

    lock = target["package_lock"]
    package_arguments = []
    lock_directory: Optional[Path] = None
    try:
        if lock is not None:
            target_cache = package_cache / target["profile"] / target["arch"]
            target_cache.mkdir(mode=0o755, parents=True, exist_ok=True)
            target_cache = safe_directory(str(target_cache), f"package cache for {key}")
            for package in lock["packages"]:
                ensure_input(
                    target_cache / package["filename"],
                    package["url"],
                    MAX_PACKAGE_BYTES,
                    {"sha256": package["sha256"]},
                    fetch,
                )
            lock_directory = Path(tempfile.mkdtemp(prefix=f".{target['name']}-{target['arch']}-lock-", dir=output))
            lock_path = lock_directory / "package-lock.json"
            lock_path.write_bytes(canonical_json(lock))
            os.chmod(lock_path, 0o444)
            package_arguments = ["--package-lock", str(lock_path), "--package-cache", str(target_cache)]

        candidate = output / f"{target['name']}-{target['release']}-{target['arch']}"
        artifact_url = f"{matrix['artifact_base_url'].rstrip('/')}/images/{{sha256}}.qcow2"
        command = [
            str(PIPELINE),
            "--mode",
            "offline",
            "--source",
            str(source),
            "--expected-sha256",
            target["source_sha256"],
            "--output",
            str(candidate),
            "--name",
            target["name"],
            "--release",
            target["release"],
            "--arch",
            target["arch"],
            "--source-user",
            "rocky" if target["name"].startswith("el") else "debian",
            "--source-uri",
            target["source_uri"],
            "--artifact-url",
            artifact_url,
            "--boot",
            target["boot"],
            "--license",
            target["license"],
            "--source-date-epoch",
            str(matrix["source_date_epoch"]),
            "--manifest-version",
            str(matrix["manifest_version"]),
            "--profile",
            target["profile"],
            *package_arguments,
        ]
        subprocess.run(command, check=True)
    finally:
        if lock_directory is not None:
            shutil.rmtree(lock_directory)


def verify_bundle(
    bundle: Path,
    target: Mapping[str, Any],
    artifact_base_url: str,
) -> Tuple[Path, Dict[str, Any]]:
    if not bundle.is_dir() or bundle.is_symlink():
        raise OfficialBuildError(f"candidate bundle is missing or unsafe: {bundle}")
    checksum_path = bundle / "checksums.txt"
    try:
        lines = checksum_path.read_text(encoding="ascii").splitlines()
    except OSError as error:
        raise OfficialBuildError(f"read bundle checksums {bundle}: {error}") from error
    checksums: Dict[str, str] = {}
    for line in lines:
        fields = line.split("  ", 1)
        if len(fields) != 2 or len(fields[0]) != 64 or Path(fields[1]).name != fields[1]:
            raise OfficialBuildError(f"invalid checksum record in {bundle}: {line!r}")
        checksums[fields[1]] = fields[0]
    expected_files = {path.name for path in bundle.iterdir() if path.is_file() and path.name != "checksums.txt"}
    if set(checksums) != expected_files:
        raise OfficialBuildError(f"bundle checksum inventory differs from its files: {bundle}")
    for name, wanted in checksums.items():
        verify_regular(bundle / name, MAX_SOURCE_BYTES, {"sha256": wanted}, f"bundle file {name}")

    try:
        validation = json.loads((bundle / "validation.json").read_bytes())
        manifest = json.loads((bundle / "manifest-candidate.json").read_bytes())
    except (OSError, json.JSONDecodeError) as error:
        raise OfficialBuildError(f"read candidate evidence {bundle}: {error}") from error
    artifact = validation.get("artifact", {})
    marker = validation.get("native_mutation", {}).get("marker", {})
    records = validation.get("package_inputs", {}).get("records")
    locked_records = target["package_lock"]["packages"] if target["package_lock"] else []
    expected_marker = {
        "legacy_network": "removed" if target["profile"] in ("el8", "el9") else "not-requested",
        "locale_default": "C.UTF-8" if target["profile"] in ("d12", "d13") else "not-requested",
        "locale_en_us": "verified" if target["profile"] in ("d12", "d13") else "not-requested",
        "profile": target["profile"],
        "python3": "verified" if target["profile"] == "el8" else "not-requested",
        "sshd_include": "verified" if target["profile"] == "el8" else "upstream",
        "xfsprogs": "verified" if target["profile"] in ("d12", "d13") else "not-requested",
    }
    if (
        validation.get("native_mutation", {}).get("status") != "completed"
        or validation.get("inspection_after", {}).get("check", {}).get("corruptions") != 0
        or validation.get("inspection_after", {}).get("check", {}).get("check_errors") != 0
        or validation.get("inspection_after", {}).get("one_element_backing_chain") is not True
        or validation.get("signing", {}).get("performed") is not False
        or validation.get("promotion", {}).get("eligible") is not False
        or any(marker.get(field) != value for field, value in expected_marker.items())
        or records != locked_records
        or validation.get("source", {}).get("sha256") != target["source_sha256"]
        or validation.get("source", {}).get("uri") != target["source_uri"]
    ):
        raise OfficialBuildError(f"candidate evidence does not satisfy the testing boundary: {bundle}")
    artifact_name = artifact.get("name")
    artifact_digest = artifact.get("sha256")
    if not isinstance(artifact_name, str) or not isinstance(artifact_digest, str):
        raise OfficialBuildError(f"candidate artifact identity is missing: {bundle}")
    artifact_path = bundle / artifact_name
    verify_regular(artifact_path, MAX_SOURCE_BYTES, {"sha256": artifact_digest}, f"candidate artifact {artifact_name}")
    entry = manifest["images"][target["name"]]["releases"][target["release"]][target["arch"]]
    wanted_url = f"{artifact_base_url.rstrip('/')}/images/{artifact_digest}.qcow2"
    if entry.get("sha256") != artifact_digest or entry.get("url") != wanted_url or entry.get("status") != "testing":
        raise OfficialBuildError(f"candidate manifest is not bound to the official repository URL: {bundle}")
    return artifact_path, validation


def link_or_copy(source: Path, destination: Path) -> None:
    try:
        os.link(source, destination)
        return
    except OSError as error:
        if error.errno != errno.EXDEV:
            raise
    with source.open("rb", buffering=0) as input_handle, destination.open("xb", buffering=0) as output_handle:
        shutil.copyfileobj(input_handle, output_handle, length=8 << 20)
        output_handle.flush()
        os.fsync(output_handle.fileno())
    os.chmod(destination, 0o444)


def render_candidate_repo(
    matrix: Mapping[str, Any],
    candidates: Mapping[str, Mapping[str, Any]],
) -> bytes:
    lines = [
        "schema: 1",
        f"revision: {matrix['manifest_version']}",
        "defaults:",
        '  image: "d13"',
        '  channel: "candidate"',
        '  arch: "native"',
        '  boot: "uefi"',
        "images:",
    ]
    for name in ("d12", "d13", "el8", "el9"):
        records = [record for _, record in sorted(candidates.items()) if record["target"]["name"] == name]
        release = records[0]["target"]["release"]
        lines.extend(
            [
                f"  {name}:",
                f"    aliases: {json.dumps(ALIASES[name], separators=(',', ': '))}",
                '    boot: "uefi"',
                "    channels:",
                f"      candidate: {json.dumps(release)}",
                "    versions:",
                f"      {json.dumps(release)}:",
                '        status: "testing"',
                "        variants:",
            ]
        )
        for record in sorted(records, key=lambda item: item["target"]["arch"]):
            target = record["target"]
            validation = record["validation"]
            digest = validation["artifact"]["sha256"]
            package_lock_digest = validation["package_inputs"]["lock_sha256"]
            package_note = f"; package-lock sha256:{package_lock_digest}" if package_lock_digest else ""
            provenance = (
                f"Barn official-image candidate recipe v1 from {target['source_uri']} "
                f"sha256:{target['source_sha256']}{package_note}; upstream is build provenance only; "
                "unsigned testing candidate requiring native smoke and production signing"
            )
            lines.extend(
                [
                    f"          {target['arch']}:",
                    f"            file: {json.dumps(digest + '.qcow2')}",
                    '            source_user: "dba"',
                    f"            provenance: {json.dumps(provenance)}",
                ]
            )
    return ("\n".join(lines) + "\n").encode("utf-8")


def assemble_repository(
    matrix: Mapping[str, Any],
    indexed: Mapping[str, Dict[str, Any]],
    bundle_roots: Sequence[Path],
    output_value: str,
    barn: str,
) -> None:
    output, parent = safe_new_directory(output_value, "repository output")
    candidates: Dict[str, Dict[str, Any]] = {}
    for key, target in sorted(indexed.items()):
        bundle_name = f"{target['name']}-{target['release']}-{target['arch']}"
        matches = [root / bundle_name for root in bundle_roots if (root / bundle_name).exists()]
        if len(matches) != 1:
            raise OfficialBuildError(f"target {key} has {len(matches)} candidate bundles across the supplied roots")
        artifact, validation = verify_bundle(matches[0], target, matrix["artifact_base_url"])
        candidates[key] = {"artifact": artifact, "target": target, "validation": validation}

    staging = Path(tempfile.mkdtemp(prefix=f".{output.name}.partial-", dir=parent))
    try:
        os.chmod(staging, 0o700)
        images = staging / "images"
        images.mkdir(mode=0o755)
        for _, record in sorted(candidates.items()):
            artifact = record["artifact"]
            validation = record["validation"]
            digest = validation["artifact"]["sha256"]
            destination = images / f"{digest}.qcow2"
            link_or_copy(artifact, destination)
            os.chmod(destination, 0o444)
        repo_bytes = render_candidate_repo(matrix, candidates)
        (staging / "repo.yaml").write_bytes(repo_bytes)
        os.chmod(staging / "repo.yaml", 0o644)
        executable = shutil.which(barn) if os.sep not in barn else str(Path(barn).resolve(strict=True))
        if not executable:
            raise OfficialBuildError(f"required Barn executable is missing: {barn}")
        subprocess.run((executable, "repo", "build", str(staging)), check=True)
        subprocess.run((executable, "repo", "verify", str(staging)), check=True)
        checksum_files = [staging / "repo.yaml", staging / "catalog.json", *sorted(images.iterdir())]
        checksum_lines = []
        for path in checksum_files:
            relative = path.relative_to(staging).as_posix()
            checksum_lines.append(f"{hash_file(path, ('sha256',))['sha256']}  {relative}\n")
            os.utime(path, (matrix["source_date_epoch"], matrix["source_date_epoch"]), follow_symlinks=False)
        (staging / "SHA256SUMS").write_text("".join(checksum_lines), encoding="ascii")
        os.chmod(staging / "SHA256SUMS", 0o644)
        os.utime(staging / "SHA256SUMS", (matrix["source_date_epoch"], matrix["source_date_epoch"]), follow_symlinks=False)
        os.rename(staging, output)
        staging = Path()
    finally:
        if staging != Path() and staging.parent == parent and staging.name.startswith(f".{output.name}.partial-"):
            shutil.rmtree(staging, ignore_errors=True)


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--source-cache", help="canonical directory containing the eight upstream qcow2 files")
    result.add_argument("--package-cache", help="canonical root for locked RPM/DEB inputs")
    result.add_argument("--output", help="canonical existing directory for new candidate bundles")
    result.add_argument("--target", action="append", default=[], help="target name/arch; repeat as needed (default: all eight)")
    result.add_argument("--fetch", action="store_true", help="fetch missing inputs from their digest-pinned HTTPS URLs")
    result.add_argument("--list", action="store_true", help="list the fixed matrix without building")
    result.add_argument("--assemble-from", action="append", default=[], help="bundle root; repeat to assemble all eight into one candidate repository")
    result.add_argument("--barn", default="barn", help="Barn executable used for candidate repo build/verify")
    return result


def main(argv: Optional[Sequence[str]] = None) -> int:
    args = parser().parse_args(argv)
    try:
        matrix, indexed = load_matrix()
        selected = select_targets(args.target, indexed)
        if args.list:
            for key, target in selected:
                package_count = len(target["package_lock"]["packages"]) if target["package_lock"] else 0
                print(f"{key}\t{target['release']}\t{target['profile']}\tpackages={package_count}")
            return 0
        if args.assemble_from:
            if args.fetch or args.target or args.source_cache or args.package_cache or not args.output:
                raise OfficialBuildError("repository assembly accepts only --assemble-from, --output, and --barn")
            roots = [safe_directory(value, "bundle root") for value in args.assemble_from]
            assemble_repository(matrix, indexed, roots, args.output, args.barn)
            return 0
        if not args.source_cache or not args.package_cache or not args.output:
            raise OfficialBuildError("--source-cache, --package-cache, and --output are required for builds")
        source_cache = safe_directory(args.source_cache, "source cache")
        package_cache = safe_directory(args.package_cache, "package cache")
        output = safe_directory(args.output, "output")
        for key, target in selected:
            build_target(matrix, key, target, source_cache, package_cache, output, args.fetch)
        return 0
    except (OfficialBuildError, OSError, subprocess.CalledProcessError) as error:
        print(f"official-image-build: ERROR: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
