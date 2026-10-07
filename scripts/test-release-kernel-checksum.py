"""Package synthetic kernel headers; test checksum delivery, not native boot."""
import hashlib
import os
from pathlib import Path
import shutil
import struct
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class KernelChecksumRelease(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory(prefix="kernel-release-")
        cls.addClassCleanup(cls.temporary.cleanup)
        cls.work = Path(cls.temporary.name)
        cls.source = cls.work / "source"
        (cls.source / "scripts").mkdir(parents=True)
        for name in ("release.sh", "release-materials.sh", "release-native-materials.sh"):
            shutil.copy2(ROOT / "scripts" / name, cls.source / "scripts" / name)
        (cls.source / "LICENSE").write_text("synthetic project license fixture\n")
        (cls.source / ".gitignore").write_text("/native-deps/bin/\n")
        cls.linux = cls.work / "linux"
        (cls.linux / "LICENSES/preferred").mkdir(parents=True)
        (cls.linux / "COPYING").write_text("synthetic Linux license fixture\n")
        (cls.linux / "LICENSES/preferred/GPL-2.0").write_text("synthetic license fixture\n")
        for args in (("init", "-q"), ("config", "user.name", "Release test"),
                     ("config", "user.email", "release-test@example.invalid"),
                     ("add", "."), ("-c", "commit.gpgsign=false", "commit", "-qm", "fixture")):
            subprocess.run(["git", "-C", str(cls.source), *args], check=True,
                           stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        cls.sha = subprocess.check_output(
            ["git", "-C", str(cls.source), "rev-parse", "HEAD"], text=True).strip()
        cls.env = dict(os.environ, SOURCE_SHA=cls.sha, SOURCE_DATE_EPOCH="1700000000",
                       RELEASE_LINUX_SOURCE_DIR=str(cls.linux))
        # Architecture header validation must work without executing either image.
        ident = b"\x7fELF\x02\x01\x01" + bytes(9)
        elf = struct.pack("<16sHHIQQQIHHHHHH", ident, 2, 62, 1, 0, 0, 0, 0,
                          64, 56, 0, 64, 0, 0)
        arm = bytearray(64)
        struct.pack_into("<Q", arm, 16, 128)
        arm[56:60] = b"ARM\x64"
        cls.payloads = {"x86_64": elf + b"x" * 64, "aarch64": bytes(arm) + b"a" * 64}
        for arch, payload in cls.payloads.items():
            native = cls.source / "native-deps/bin" / arch
            native.mkdir(parents=True)
            (native / "vmlinux").write_bytes(payload)
            # The packager must generate from the copied bytes, not reuse this.
            (native / "vmlinux.sha256").write_text("stale build-side checksum\n")
        cls.bundles = {}
        for arch in cls.payloads:
            output = cls.work / ("baseline-" + arch)
            cls.package(arch, "vmlinux-v2.3.4", output)
            cls.bundles[arch] = output

    @classmethod
    def package(cls, arch, version, output):
        env = dict(cls.env, RELEASE_NATIVE_BIN_DIR=str(cls.source / "native-deps/bin" / arch))
        result = subprocess.run(["bash", str(cls.source / "scripts/release.sh"),
                                 "package", "vmlinux", version, arch, str(output)],
                                env=env, capture_output=True, text=True, timeout=30)
        if result.returncode:
            raise AssertionError(result.stdout + result.stderr)

    @staticmethod
    def archive(bundle):
        return next((bundle / "assets").glob("vmlinux-*.tar.gz"))

    def validate(self, bundle, arch):
        return subprocess.run(["bash", str(self.source / "scripts/release.sh"),
                               "validate", "vmlinux", "vmlinux-v2.3.4", arch, str(bundle)],
                              env=self.env, capture_output=True, text=True, timeout=30)

    def assert_package(self, bundle, arch):
        with tarfile.open(self.archive(bundle)) as archive:
            kernel = archive.extractfile("./bin/vmlinux").read()
            checksum = archive.extractfile("./bin/vmlinux.sha256").read()
            self.assertEqual(kernel, self.payloads[arch])
            self.assertEqual(checksum, (hashlib.sha256(kernel).hexdigest() + "  vmlinux\n").encode())
            entry = archive.getmember("./bin/vmlinux.sha256")
            self.assertTrue(entry.isfile())
            self.assertEqual((entry.mode, entry.uid, entry.gid), (0o644, 0, 0))
            self.assertEqual(archive.getnames().count("./bin/vmlinux.sha256"), 1)
        native = self.source / "native-deps/bin" / arch
        self.assertEqual((native / "vmlinux").read_bytes(), self.payloads[arch])
        self.assertEqual((native / "vmlinux.sha256").read_text(), "stale build-side checksum\n")

    def test_both_architectures_stable_and_preview_ship_checksum(self):
        for arch in self.payloads:
            with self.subTest(arch=arch, release="stable"):
                self.assert_package(self.bundles[arch], arch)
            with self.subTest(arch=arch, release="preview"):
                output = self.work / ("preview-" + arch)
                self.package(arch, "vmlinux-v2.3.4-preview.20260804", output)
                self.assert_package(output, arch)

    def test_identical_inputs_produce_identical_archives(self):
        for arch, original in self.bundles.items():
            with self.subTest(arch=arch):
                output = self.work / ("repeat-" + arch)
                self.package(arch, "vmlinux-v2.3.4", output)
                self.assertEqual(self.archive(original).read_bytes(), self.archive(output).read_bytes())

    def test_sidecar_validation_and_historical_absence(self):
        mutations = ("absent", "empty", "digest", "filename", "path", "duplicate",
                     "trailing-data", "kernel-bytes", "mode", "symlink", "directory")
        for arch, original in self.bundles.items():
            for mutation in mutations:
                with self.subTest(arch=arch, mutation=mutation):
                    candidate = self.work / (arch + "-" + mutation)
                    shutil.copytree(original, candidate)
                    tree = candidate / "root"
                    tree.mkdir()
                    subprocess.run(["tar", "--same-permissions", "-xzf", str(self.archive(original)),
                                    "-C", str(tree)], check=True)
                    checksum = tree / "bin/vmlinux.sha256"
                    data = checksum.read_bytes()
                    if mutation == "absent":
                        checksum.unlink()
                    elif mutation == "empty":
                        checksum.write_bytes(b"")
                    elif mutation == "digest":
                        checksum.write_bytes(b"0" * 64 + data[64:])
                    elif mutation == "filename":
                        checksum.write_bytes(data.replace(b"vmlinux", b"other"))
                    elif mutation == "path":
                        checksum.write_bytes(data.replace(b"vmlinux", b"../bin/vmlinux"))
                    elif mutation == "duplicate":
                        checksum.write_bytes(data + data)
                    elif mutation == "trailing-data":
                        checksum.write_bytes(data + b"ignored garbage\n")
                    elif mutation == "kernel-bytes":
                        with (tree / "bin/vmlinux").open("ab") as kernel:
                            kernel.write(b"changed kernel body, valid header\n")
                    elif mutation == "mode":
                        checksum.chmod(0o755)
                    elif mutation == "symlink":
                        checksum.unlink()
                        checksum.symlink_to("vmlinux")
                    elif mutation == "directory":
                        checksum.unlink()
                        checksum.mkdir(mode=0o755)
                    archive = self.archive(candidate)
                    subprocess.run(["tar", "--sort=name", "--owner=0", "--group=0", "--numeric-owner",
                                    "--mtime=@1700000000", "-czf", str(archive), "-C", str(tree), "."],
                                   check=True)
                    # Make outer integrity valid so failure must come from the
                    # new per-kernel check or the exact layout/type/mode gate.
                    (candidate / "assets/SHA256SUMS").write_text(
                        hashlib.sha256(archive.read_bytes()).hexdigest() + "  " + archive.name + "\n")
                    result = self.validate(candidate, arch)
                    if mutation == "absent":
                        self.assertEqual(result.returncode, 0, result.stderr)
                    else:
                        self.assertNotEqual(result.returncode, 0, mutation)
                        expected = ("unsafe type, mode or ownership" if mutation in ("mode", "symlink")
                                    else "outside the exact" if mutation == "directory"
                                    else "vmlinux.sha256 does not match")
                        self.assertIn(expected, result.stderr)


if __name__ == "__main__":
    unittest.main()
