#!/usr/bin/env python3
import copy
import hashlib
import importlib.util
import io
from pathlib import Path
import tarfile
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("prepare_sandbox_init", Path(__file__).with_name("prepare-sandbox-init.py"))
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)


class SelectedSandboxInitTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.version = "v1.2.3-preview.20260914"
        self.sha = "a" * 40
        self.archive = self.root / f"sandboxer-{self.version}-linux-x86_64.tar.gz"
        self.output = self.root / "bin/sandbox-init"
        self.go_materials = self.root / "selected-go"
        self.payload = b"selected release binary; do not rebuild"
        self.toolchain = "go1.26.7"
        self.write_archive()

    def write_archive(self, kind=tarfile.REGTYPE, duplicate=False):
        with tarfile.open(self.archive, "w:gz") as bundle:
            for _ in range(2 if duplicate else 1):
                entry = tarfile.TarInfo("./bin/sandbox-init")
                entry.mode, entry.type = 0o755, kind
                entry.size = len(self.payload) if kind == tarfile.REGTYPE else 0
                entry.linkname = "/outside" if kind == tarfile.SYMTYPE else ""
                bundle.addfile(entry, io.BytesIO(self.payload) if entry.size else None)
            if kind == tarfile.REGTYPE and not duplicate:
                materials = {
                    "share/sources/sandboxer/GO-BUILD-INFO.tsv":
                        ("payload\trecord\tname\tversion_or_value\tchecksum\n"
                         f"bin/sandbox-init\ttoolchain\tgo\t{self.toolchain}\t-\n").encode(),
                    "share/sources/sandboxer/SOURCES.tsv":
                        ("payload\tname\tversion\tsource\tintegrity\tlicense_directory\n"
                         f"bin/sandbox-init\tGo toolchain\t{self.toolchain}\t"
                         f"https://go.dev/dl/#{self.toolchain}\t-\t"
                         f"share/licenses/sandboxer/go-toolchain/{self.toolchain}\n").encode(),
                    f"share/licenses/sandboxer/go-toolchain/{self.toolchain}/LICENSE":
                        b"selected compiler license\n",
                    f"share/licenses/sandboxer/go-toolchain/{self.toolchain}/PATENTS":
                        b"selected compiler patents\n",
                }
                checksum_lines = []
                for name, data in materials.items():
                    entry = tarfile.TarInfo("./" + name)
                    entry.mode, entry.size = 0o644, len(data)
                    bundle.addfile(entry, io.BytesIO(data))
                    checksum_lines.append(
                        f"{hashlib.sha256(data).hexdigest()}  {name}\n")
                data = "".join(checksum_lines).encode()
                entry = tarfile.TarInfo("./share/sources/sandboxer/MATERIALS.sha256")
                entry.mode, entry.size = 0o644, len(data)
                bundle.addfile(entry, io.BytesIO(data))
        sums = self.root / "SHA256SUMS"
        sums.write_text(f"{helper.digest(self.archive)}  {self.archive.name}\n")
        self.release = {"tag_name": self.version, "target_commitish": self.sha,
                        "draft": False, "prerelease": True,
                        "assets": [{"name": path.name, "size": path.stat().st_size,
                                    "digest": "sha256:" + helper.digest(path), "state": "uploaded"}
                                   for path in (self.archive, sums)]}

    def prepare(self):
        helper.prepare(self.release, self.root, self.version, self.sha,
                       self.output, self.go_materials)

    def test_installs_exact_published_bytes(self):
        self.prepare()
        self.assertEqual(self.output.read_bytes(), self.payload)
        self.assertEqual(self.output.stat().st_mode & 0o777, 0o755)
        self.assertEqual(
            (self.go_materials / self.toolchain / "LICENSE").read_bytes(),
            b"selected compiler license\n")
        self.assertEqual(
            (self.go_materials / self.toolchain / "PATENTS").read_bytes(),
            b"selected compiler patents\n")

    def test_installs_exact_bytes_from_a_numbered_preview(self):
        self.version += ".1"
        self.archive = self.root / f"sandboxer-{self.version}-linux-x86_64.tar.gz"
        self.write_archive()
        self.prepare()
        self.assertEqual(self.output.read_bytes(), self.payload)

    def test_rejects_wrong_selection_and_publication_state(self):
        original = copy.deepcopy(self.release)
        for key, value in (("tag_name", "v1.2.2"), ("target_commitish", "b" * 40),
                           ("draft", True), ("prerelease", False)):
            with self.subTest(key=key):
                self.release = {**copy.deepcopy(original), key: value}
                with self.assertRaises(ValueError):
                    self.prepare()
                self.assertFalse(self.output.exists())

    def test_rejects_corrupt_asset_without_replacing_existing_binary(self):
        self.output.parent.mkdir()
        self.output.write_bytes(b"existing")
        self.archive.write_bytes(self.archive.read_bytes() + b"corruption")
        with self.assertRaisesRegex(ValueError, "size/state"):
            self.prepare()
        self.assertEqual(self.output.read_bytes(), b"existing")

    def test_rejects_github_digest_and_checksum_disagreement(self):
        self.release["assets"][0]["digest"] = "sha256:" + "0" * 64
        with self.assertRaisesRegex(ValueError, "GitHub asset digest"):
            self.prepare()
        self.write_archive()
        sums = self.root / "SHA256SUMS"
        sums.write_text(f"{'0' * 64}  {self.archive.name}\n")
        self.release["assets"][1]["digest"] = "sha256:" + helper.digest(sums)
        with self.assertRaisesRegex(ValueError, "SHA256SUMS"):
            self.prepare()

    def test_rejects_tampered_selected_toolchain_material(self):
        self.write_archive()
        with tarfile.open(self.archive, "r:gz") as source:
            members = source.getmembers()
            payloads = {
                member.name: source.extractfile(member).read()
                for member in members if member.isfile()
            }
        target = "./share/licenses/sandboxer/go-toolchain/" + self.toolchain + "/LICENSE"
        payloads[target] = b"tampered compiler license\n"
        with tarfile.open(self.archive, "w:gz") as bundle:
            for member in members:
                data = payloads.get(member.name)
                bundle.addfile(member, io.BytesIO(data) if data is not None else None)
        sums = self.root / "SHA256SUMS"
        sums.write_text(f"{helper.digest(self.archive)}  {self.archive.name}\n")
        self.release["assets"] = [
            {"name": path.name, "size": path.stat().st_size,
             "digest": "sha256:" + helper.digest(path), "state": "uploaded"}
            for path in (self.archive, sums)
        ]
        with self.assertRaisesRegex(ValueError, "material checksum mismatch"):
            self.prepare()
        self.assertFalse(self.output.exists())


    def test_rejects_ambiguous_or_linked_archive_member(self):
        for kind, duplicate in ((tarfile.SYMTYPE, False), (tarfile.REGTYPE, True)):
            with self.subTest(kind=kind, duplicate=duplicate):
                self.write_archive(kind, duplicate)
                with self.assertRaisesRegex(ValueError, "one regular executable"):
                    self.prepare()
                self.assertFalse(self.output.exists())


if __name__ == "__main__":
    unittest.main()
