#!/usr/bin/env python3
"""Build and attach the native Mac payload to the ordinary arm64 release.

Only build requires macOS. Attach and inventory run in the existing Linux
GoReleaser job; no downloaded code is executed there.
"""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import plistlib
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

REPO = Path(__file__).resolve().parents[1]
APP = "bin/Barn.app"
MANIFEST = "MACOS.json"
CLI = "bin/barn"
HELPER = "bin/barn-hosts-helper"
EXECUTABLES = {CLI, HELPER, APP + "/Contents/MacOS/barn-mac-runner"}


def identity(version, commit):
    if not re.fullmatch(r"\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.+-]+)?", version):
        raise ValueError("invalid Mac release version")
    if commit != "uncommitted" and not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("invalid Mac release commit")


def members(path):
    result = {}
    with tarfile.open(path, "r:gz") as archive:
        entries = archive.getmembers()
        if len(entries) > 1000 or sum(item.size for item in entries) > 256 << 20:
            raise ValueError("release archive exceeds expected size")
        for item in entries:
            name = item.name
            if not item.isfile() or name != str(PurePosixPath(name)) or ".." in PurePosixPath(name).parts or name.startswith("/") or "\\" in name or "\n" in name or name in result:
                raise ValueError(f"unsafe or duplicate release entry: {name!r}")
            result[name] = (archive.extractfile(item).read(), item.mode)
    return result


def write_archive(path, files, epoch):
    with path.open("wb") as raw, gzip.GzipFile(fileobj=raw, mode="wb", filename="", mtime=epoch) as compressed, tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as archive:
        for name, (data, mode) in sorted(files.items()):
            info = tarfile.TarInfo(name)
            info.size, info.mode, info.mtime = len(data), mode, epoch
            info.uid = info.gid = 0
            info.uname = info.gname = "root"
            archive.addfile(info, io.BytesIO(data))


def validate(files, version, commit=None):
    manifest = json.loads(files[MANIFEST][0])
    if manifest.get("schema") != 1 or manifest.get("version") != version or manifest.get("target") != "darwin/arm64":
        raise ValueError("Mac payload does not match the release version/target")
    identity(version, manifest["commit"])
    if commit is not None and manifest["commit"] != commit:
        raise ValueError("Mac payload does not match the CLI source commit")
    if os.environ.get("BARN_MAC_REQUIRE_NOTARIZATION") == "1" and (manifest.get("signing") != "developer_id" or manifest.get("notarized") is not True):
        raise ValueError("formal Mac release requires a Developer ID signed, notarized app")
    required = {
        CLI,
        HELPER,
        APP + "/Contents/Info.plist",
        APP + "/Contents/MacOS/barn-mac-runner",
        APP + "/Contents/_CodeSignature/CodeResources",
        "MACOS.md",
    }
    declared = manifest["files"]
    if not isinstance(declared, dict):
        raise ValueError("invalid Mac payload inventory")
    if not required <= declared.keys() or set(files) != set(declared) | {MANIFEST}:
        raise ValueError("incomplete or unexpected Mac payload inventory")
    for name, expected in declared.items():
        if name != "MACOS.md" and name not in {CLI, HELPER} and not name.startswith(APP + "/Contents/"):
            raise ValueError("Mac payload escapes its app bundle")
        data, mode = files[name]
        executable = name in EXECUTABLES
        if mode != (0o755 if executable else 0o644) or expected != {"sha256": hashlib.sha256(data).hexdigest(), "mode": mode}:
            raise ValueError(f"Mac payload content or mode changed: {name}")
    info = plistlib.loads(files[APP + "/Contents/Info.plist"][0])
    if info.get("BarnVersion") != version or info.get("BarnCommit") != manifest["commit"] or info.get("LSMinimumSystemVersion") != "27.0":
        raise ValueError("Mac app identity does not match its manifest")
    if (info.get("CFBundleIdentifier") != "com.pgsty.barn" or
            info.get("CFBundleName") != "Barn" or info.get("CFBundleDisplayName") != "Barn" or
            info.get("CFBundleExecutable") != "barn-mac-runner"):
        raise ValueError("Mac app bundle identity does not match Barn")
    helper_sha = hashlib.sha256(files[HELPER][0]).hexdigest().encode()
    if helper_sha not in files[CLI][0]:
        raise ValueError("Mac CLI does not carry the digest of its paired helper")
    return manifest


def build(args):
    identity(args.version, args.commit)
    if sys.platform != "darwin" or os.uname().machine != "arm64":
        raise ValueError("native Mac payload must be built on Apple Silicon/macOS 27")
    output = Path(args.output).absolute()
    if output.exists() or output.is_symlink():
        raise ValueError("refuse existing Mac payload output")
    bin_root = REPO / "bin"
    if bin_root.is_symlink():
        raise ValueError("unsafe bin directory")
    bin_root.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".mac-release-", dir=bin_root) as temporary:
        stage = Path(temporary)
        (stage / "bin").mkdir()
        env = dict(os.environ, BARN_VERSION=args.version, BARN_COMMIT=args.commit)
        if env.get("BARN_NOTARY_PROFILE"):
            if env.get("BARN_CODESIGN_IDENTITY", "-") == "-":
                raise ValueError("notarization requires BARN_CODESIGN_IDENTITY")
            # The final submission must contain the CLI, helper, and app together.
            env["BARN_NOTARY_DEFER"] = "1"
        go_output = stage / "go"
        subprocess.run(
            ["bash", str(REPO / "packaging/build-dev.sh"), "darwin", "arm64", str(go_output)],
            env=env,
            check=True,
        )
        for name in (CLI, HELPER):
            source = go_output / Path(name).name
            destination = stage / name
            shutil.copyfile(source, destination)
            destination.chmod(0o755)
        shutil.rmtree(go_output)
        subprocess.run(["bash", str(REPO / "packaging/build-mac-app.sh"), str(stage / APP)], env=env, check=True)
        shutil.copyfile(REPO / "docs/mac.md", stage / "MACOS.md")
        signatures = []
        for name in (CLI, HELPER):
            subprocess.run(["codesign", "--verify", "--strict", str(stage / name)], check=True)
            signatures.append(
                subprocess.run(
                    ["codesign", "-d", "--verbose=4", str(stage / name)],
                    capture_output=True,
                    text=True,
                    check=True,
                ).stderr
            )
        subprocess.run(["codesign", "--verify", "--deep", "--strict", str(stage / APP)], check=True)
        signatures.append(
            subprocess.run(
                ["codesign", "-d", "--verbose=4", str(stage / APP)],
                capture_output=True,
                text=True,
                check=True,
            ).stderr
        )
        notarized = False
        if env.get("BARN_NOTARY_PROFILE"):
            notarize_root = stage / "Barn-native"
            notarize_bin = notarize_root / "bin"
            notarize_bin.mkdir(parents=True)
            for name in (CLI, HELPER):
                shutil.copyfile(stage / name, notarize_root / name)
            subprocess.run(["ditto", str(stage / APP), str(notarize_root / APP)], check=True)
            notarize_zip = stage / "notarize.zip"
            subprocess.run(
                ["ditto", "-c", "-k", "--keepParent", str(notarize_root), str(notarize_zip)],
                check=True,
            )
            notary = subprocess.run(
                [
                    "xcrun",
                    "notarytool",
                    "submit",
                    str(notarize_zip),
                    "--keychain-profile",
                    env["BARN_NOTARY_PROFILE"],
                    "--wait",
                    "--timeout",
                    "20m",
                    "--output-format",
                    "json",
                ],
                capture_output=True,
                text=True,
                check=True,
            )
            notary_result = json.loads(notary.stdout)
            if notary_result.get("status") != "Accepted":
                raise ValueError("Apple did not accept the complete Mac payload for notarization")
            print(f"Apple notarization accepted: {notary_result['id']}", flush=True)
            subprocess.run(["xcrun", "stapler", "staple", str(stage / APP)], check=True)
            subprocess.run(["xcrun", "stapler", "validate", str(stage / APP)], check=True)
            subprocess.run(["spctl", "--assess", "--type", "execute", str(stage / APP)], check=True)
            shutil.rmtree(notarize_root)
            notarize_zip.unlink()
            notarized = True
        developer_id = all("Authority=Developer ID Application:" in signature for signature in signatures)
        manifest = {"schema": 1, "version": args.version, "commit": args.commit, "target": "darwin/arm64", "signing": "developer_id" if developer_id else "ad_hoc", "notarized": notarized, "files": {}}
        files = {}
        for path in sorted(stage.rglob("*")):
            if path.is_symlink():
                raise ValueError("Mac payload cannot contain symlinks")
            if not path.is_file():
                continue
            name = path.relative_to(stage).as_posix()
            mode = 0o755 if os.access(path, os.X_OK) else 0o644
            data = path.read_bytes()
            files[name] = data, mode
            manifest["files"][name] = {"sha256": hashlib.sha256(data).hexdigest(), "mode": mode}
        files[MANIFEST] = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode(), 0o644
        validate(files, args.version, args.commit)
        candidate = stage / "payload.tar.gz"
        write_archive(candidate, files, args.epoch)
        # Verify the exported bytes, including preservation of a stapled ticket.
        verify = stage / "verify"
        verify.mkdir()
        for name, (data, mode) in members(candidate).items():
            path = verify / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
            path.chmod(mode)
        subprocess.run(["codesign", "--verify", "--strict", str(verify / CLI)], check=True)
        subprocess.run(["codesign", "--verify", "--strict", str(verify / HELPER)], check=True)
        subprocess.run(["codesign", "--verify", "--deep", "--strict", str(verify / APP)], check=True)
        if notarized:
            subprocess.run(["xcrun", "stapler", "validate", str(verify / APP)], check=True)
            subprocess.run(["spctl", "--assess", "--type", "execute", str(verify / APP)], check=True)
        output.parent.mkdir(parents=True, exist_ok=True)
        with output.open("xb") as target:
            target.write(candidate.read_bytes())
    print(f"Mac payload: {output}")


def attach(args):
    identity(args.version, args.commit)
    dist = Path(args.dist).resolve(strict=True)
    root = f"barn_{args.version}_darwin_arm64"
    name = root + ".tar.gz"
    payload = members(args.payload)
    validate(payload, args.version, args.commit)
    files = members(dist / name)
    if any(not key.startswith(root + "/") for key in files):
        raise ValueError("unexpected Darwin archive root")
    for path, value in payload.items():
        key = root + "/" + path
        if key in files and path not in {CLI, HELPER}:
            raise ValueError("Darwin archive already contains a native payload")
        files[key] = value
    with tempfile.TemporaryDirectory(prefix=".mac-attach-", dir=dist) as temporary:
        stage = Path(temporary)
        write_archive(stage / name, files, args.epoch)
        env = dict(os.environ, SOURCE_DATE_EPOCH=str(args.epoch))
        subprocess.run([str(REPO / "packaging/goreleaser-sbom.sh"), name, name + ".spdx.json"], cwd=stage, env=env, check=True)
        os.replace(stage / name, dist / name)
        os.replace(stage / (name + ".spdx.json"), dist / (name + ".spdx.json"))
        checksum_names = [line.split(None, 1)[1].strip() for line in (dist / "checksums.txt").read_text().splitlines()]
        if any(Path(item).name != item for item in checksum_names):
            raise ValueError("unsafe checksum member")
        checksums = sorted(f"{hashlib.sha256((dist / item).read_bytes()).hexdigest()}  {item}\n" for item in checksum_names)
        (stage / "checksums.txt").write_text("".join(checksums))
        os.replace(stage / "checksums.txt", dist / "checksums.txt")
    print(f"Attached native Mac payload to {dist / name}")


def inventory(args):
    files = members(args.archive)
    root = f"barn_{args.version}_darwin_arm64/"
    payload = {
        name[len(root):]: value
        for name, value in files.items()
        if name.startswith(root)
        and (
            name[len(root):].startswith(APP + "/")
            or name[len(root):] in (CLI, HELPER, MANIFEST, "MACOS.md")
        )
    }
    if not (set(payload) - {CLI, HELPER}):
        if os.environ.get("BARN_MAC_REQUIRE_PAYLOAD") == "1":
            raise ValueError("Darwin arm64 release is missing the native Mac payload")
        return
    validate(payload, args.version, args.commit)
    print("\n".join(sorted(set(payload) - {CLI, HELPER})))


def check(args):
    identity(args.version, args.commit)
    manifest = validate(members(args.payload), args.version, args.commit)
    print(f"Mac payload verified: {manifest['version']} ({manifest['signing']}, notarized={manifest['notarized']})")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    for name in ("build", "attach", "inventory", "check"):
        child = commands.add_parser(name)
        child.add_argument("version")
        if name != "inventory":
            child.add_argument("commit")
            if name != "check":
                child.add_argument("epoch", type=int)
        else:
            child.add_argument("archive")
            child.add_argument("--commit")
        if name == "build":
            child.add_argument("output")
        if name == "attach":
            child.add_argument("dist")
        if name in ("attach", "check"):
            child.add_argument("payload")
    args = parser.parse_args()
    if hasattr(args, "epoch") and args.epoch <= 0:
        parser.error("source epoch must be positive")
    globals()[args.command](args)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError, tarfile.TarError) as error:
        sys.exit(f"Mac release: {error}")
