#!/usr/bin/env python3
"""Native payload boundary tests; no macOS, Keychain or release credentials."""
import copy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import plistlib
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("mac_release", Path(__file__).resolve().parents[1] / "packaging/mac-release.py")
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class PayloadTests(unittest.TestCase):
    def fixture(self):
        files = {"MACOS.md": (b"guide", 0o644)}
        files[release.APP + "/Contents/Info.plist"] = plistlib.dumps({"FarrowVersion": "0.8.1-next", "FarrowCommit": "uncommitted", "LSMinimumSystemVersion": "27.0"}), 0o644
        for name in ("MacOS/farrow-mac-runner", "_CodeSignature/CodeResources"):
            files[release.APP + "/Contents/" + name] = b"test bytes", 0o644 if name.startswith("_CodeSignature/") else 0o755
        manifest = {"schema": 1, "version": "0.8.1-next", "commit": "uncommitted", "target": "darwin/arm64", "signing": "ad_hoc", "notarized": False,
                    "files": {name: {"sha256": hashlib.sha256(data).hexdigest(), "mode": mode} for name, (data, mode) in files.items()}}
        files[release.MANIFEST] = json.dumps(manifest).encode(), 0o644
        return files

    def test_exact_source_and_export_round_trip(self):
        files = self.fixture()
        release.validate(files, "0.8.1-next", "uncommitted")
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / "native.tar.gz"
            release.write_archive(archive, files, 1700000000)
            self.assertEqual(files, release.members(archive))
            before = archive.read_bytes()
            release.write_archive(archive, files, 1700000000)
            self.assertEqual(before, archive.read_bytes())
        for version, commit in (("0.8.2", "uncommitted"), ("0.8.1-next", "a" * 40)):
            with self.assertRaises(ValueError):
                release.validate(files, version, commit)

    def test_missing_corrupt_and_nonexecutable_payloads(self):
        original = self.fixture()
        path = release.APP + "/Contents/MacOS/farrow-mac-runner"
        for kind in ("missing", "content", "mode", "unexpected"):
            with self.subTest(kind=kind):
                files = copy.deepcopy(original)
                if kind == "missing":
                    del files[path]
                elif kind == "content":
                    files[path] = b"tampered", 0o755
                elif kind == "mode":
                    files[path] = files[path][0], 0o644
                else:
                    files["bin/unexpected-program"] = b"never execute", 0o755
                with self.assertRaises(ValueError):
                    release.validate(files, "0.8.1-next")

    def test_formal_release_rejects_development_signing(self):
        with patch.dict(os.environ, FARROW_MAC_REQUIRE_NOTARIZATION="1"):
            with self.assertRaisesRegex(ValueError, "notarized"):
                release.validate(self.fixture(), "0.8.1-next")

    def test_unsafe_archive_members(self):
        for kind in ("parent", "absolute", "symlink", "duplicate", "oversized"):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / "unsafe.tar.gz"
                with tarfile.open(path, "w:gz") as archive:
                    name = "../escape" if kind == "parent" else "/escape" if kind == "absolute" else "safe"
                    item = tarfile.TarInfo(name)
                    if kind == "symlink":
                        item.type, item.linkname = tarfile.SYMTYPE, "/tmp/escape"
                    if kind == "oversized":
                        item.size = 257 << 20
                        # A header alone is enough; reject without extracting.
                        archive.fileobj.write(item.tobuf())
                    else:
                        archive.addfile(item, io.BytesIO())
                    if kind == "duplicate":
                        archive.addfile(item, io.BytesIO())
                with self.assertRaises((ValueError, tarfile.TarError)):
                    release.members(path)


if __name__ == "__main__":
    unittest.main()
