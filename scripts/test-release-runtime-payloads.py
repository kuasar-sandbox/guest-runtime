"""Exercise trusted host EROFS readers with private synthetic images."""
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
import zipfile

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("runtime_payloads", ROOT / "scripts/release-runtime-payloads.py")
READER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(READER)


def build_tools(work):
    tools = work / "runtime-verifier"
    for name, relative in (
            ("mkfs.erofs", "bin/mkfs.erofs"),
            ("fsck.erofs", "bin/fsck.erofs"),
            ("dump.erofs", "build/src/erofs-utils/dump/dump.erofs")):
        executable = shutil.which(name)
        if executable is None:
            raise RuntimeError("Runtime tests require trusted host " + name + " on PATH")
        target = tools / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(executable, target)
    return tools


def make_bundle(erofs, output):
    def footer(digest):
        data = io.BytesIO()
        with zipfile.ZipFile(data, "w") as archive:
            marker = zipfile.ZipInfo(".kuasar.digest." + digest, (1980, 1, 1, 0, 0, 0))
            marker.external_attr = 0o100444 << 16
            archive.writestr(marker, b"")
        return data.getvalue()
    content = erofs.read_bytes()
    marker_size = len(footer("0" * 64))
    size = ((len(content) + marker_size + (2 << 20) - 1) // (2 << 20)) * (2 << 20)
    prefix = content + bytes(size - marker_size - len(content))
    output.write_bytes(prefix + footer(hashlib.sha256(prefix).hexdigest()))


class RuntimePayloads(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.tree = self.root / "tree"
        for label, path in READER.PAYLOADS.items():
            file = self.tree / path.lstrip("/")
            file.parent.mkdir(parents=True, exist_ok=True)
            file.write_bytes((label + " fixture payload\n").encode())
            file.chmod(0o755)
        self.image = self.root / "runtime.bundle"

    def pack(self, *extra):
        erofs = self.root / "raw.erofs"
        subprocess.run([str(TOOLS / "bin/mkfs.erofs"), "-Enoinline_data", "--all-root", "-x-1", "-T0", "-U", "00000000-0000-0000-0000-000000000000", *extra, str(erofs), str(self.tree)],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True, timeout=30)
        make_bundle(erofs, self.image)

    def read(self, *, without_mkfs=False, systemd_arch=None):
        READER.read_payloads(self.image, TOOLS / "bin/fsck.erofs",
                             TOOLS / "build/src/erofs-utils/dump/dump.erofs", self.root / "output",
                             without_mkfs=without_mkfs, systemd_arch=systemd_arch)

    def install_systemd(self):
        # Synthetic, non-runnable ELF bytes exercise the real installation
        # verifier after a genuine EROFS packing/readback, without a native build.
        specification = importlib.util.spec_from_file_location(
            "systemd_fixture", ROOT / "native-deps/deps/test-systemd-payload.py")
        fixture_module = importlib.util.module_from_spec(specification)
        specification.loader.exec_module(fixture_module)
        fixture = fixture_module.InstallationTests()
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        target = self.tree / "usr/lib/sandbox-runtime/systemd"
        shutil.copytree(fixture.root, target, symlinks=True)
        target.chmod(0o755)
        fixture.root = target
        (self.tree / "opt/sandbox-runtime/bin/mkfs.erofs").unlink()
        os.link(self.tree / "sbin/init", self.tree / "opt/sandbox-runtime/bin/sandbox-init")
        return fixture

    def test_systemd_contract_reads_full_installation_and_real_hardlink(self):
        fixture = self.install_systemd()
        self.pack()
        self.read(without_mkfs=True, systemd_arch="x86_64")
        output = self.root / "output"
        self.assertEqual((output / "init").read_bytes(), (output / "executor").read_bytes())
        manifest = json.loads((output / "systemd-manifest.json").read_text())
        self.assertEqual(manifest["files"]["lib/dbus-1.0/dbus-daemon-launch-helper"]["mode"], 0o4755)
        self.assertEqual((output / "systemd-licenses/dependencies/libc/copyright").read_bytes(),
                         (fixture.root / "share/licenses/dependencies/libc/copyright").read_bytes())

    def test_systemd_executor_copy_is_not_a_hardlink(self):
        self.install_systemd()
        target = self.tree / "opt/sandbox-runtime/bin/sandbox-init"
        target.unlink()
        shutil.copy2(self.tree / "sbin/init", target)
        self.pack()
        with self.assertRaisesRegex(ValueError, "must be a hardlink"):
            self.read(without_mkfs=True, systemd_arch="x86_64")

    def test_systemd_common_tool_setuid_is_not_hidden_by_dump(self):
        self.install_systemd()
        (self.tree / "opt/sandbox-runtime/bin/envd").chmod(0o4755)
        self.pack()
        with self.assertRaisesRegex(ValueError, "unexpected privilege bits"):
            self.read(without_mkfs=True, systemd_arch="x86_64")

    def test_systemd_rehashed_extra_setuid_is_rejected_after_packing(self):
        fixture = self.install_systemd()
        (fixture.root / "bin/systemctl").chmod(0o4755)
        fixture.refresh()
        self.pack()
        with self.assertRaisesRegex(ValueError, "privilege/mode not allowed"):
            self.read(without_mkfs=True, systemd_arch="x86_64")

    def test_systemd_helper_cannot_lose_its_required_setuid(self):
        fixture = self.install_systemd()
        (fixture.root / "lib/dbus-1.0/dbus-daemon-launch-helper").chmod(0o755)
        fixture.refresh()
        self.pack()
        with self.assertRaisesRegex(ValueError, "privilege/mode not allowed"):
            self.read(without_mkfs=True, systemd_arch="x86_64")

    def test_systemd_manifest_cannot_opt_out_of_the_contract(self):
        fixture = self.install_systemd()
        (fixture.root / "share/sandbox-runtime/systemd-manifest.json").unlink()
        self.pack()
        with self.assertRaises((ValueError, subprocess.SubprocessError)):
            self.read(without_mkfs=True, systemd_arch="x86_64")

    def test_systemd_dax_rejects_chunked_deduplication(self):
        self.install_systemd()
        self.pack("--chunksize=4096")
        with self.assertRaisesRegex(ValueError, "flat non-inline"):
            self.read(without_mkfs=True, systemd_arch="x86_64")

    def test_systemd_dax_rejects_inline_regular_files(self):
        self.install_systemd()
        self.pack("-Einline_data")
        with self.assertRaisesRegex(ValueError, "flat non-inline"):
            self.read(without_mkfs=True, systemd_arch="x86_64")

    def test_systemd_dax_rejects_cross_inode_physical_aliases(self):
        self.install_systemd()
        # Two byte-identical tools still require distinct physical data pages.
        # Keep bytes and filesystem checksums valid while redirecting the second
        # inode to the first tool's extent. fsck alone accepts this shared data.
        first = self.tree / "opt/sandbox-runtime/bin/envd"
        second = self.tree / "opt/sandbox-runtime/bin/flatten-ctl"
        second.write_bytes(first.read_bytes())
        self.pack()
        dump = TOOLS / "build/src/erofs-utils/dump/dump.erofs"
        import re
        import struct
        nids = []
        for path in ("envd", "flatten-ctl"):
            text = subprocess.check_output([str(dump), "--path=/opt/sandbox-runtime/bin/" + path, str(self.image)], text=True)
            nids.append(int(re.search(r"^NID: ([0-9]+)", text, re.M)[1]))
        raw = self.root / "raw.erofs"
        with raw.open("r+b") as stream:
            stream.seek(1024)
            header = stream.read(128)
            base = struct.unpack_from("<I", header, 40)[0] << header[12]
            stream.seek(base + nids[0] * 32 + 16)
            block = stream.read(4)
            stream.seek(base + nids[1] * 32 + 16)
            stream.write(block)
        make_bundle(raw, self.image)
        with self.assertRaisesRegex(ValueError, "shared between independent inodes"):
            self.read(without_mkfs=True, systemd_arch="x86_64")

    def test_real_erofs_reads_only_fixed_files_privately(self):
        self.pack()
        self.read()
        self.assertEqual((self.root / "output").stat().st_mode & 0o777, 0o700)
        for label, path in READER.PAYLOADS.items():
            output = self.root / "output" / label
            self.assertEqual(output.read_bytes(), (self.tree / path.lstrip("/")).read_bytes())
            self.assertEqual(output.stat().st_mode & 0o777, 0o600)

    def test_go_generator_contract_needs_only_runtime_go_payloads(self):
        (self.tree / "opt/sandbox-runtime/bin/mkfs.erofs").unlink()
        self.pack()
        self.read(without_mkfs=True)
        self.assertFalse((self.root / "output/mkfs.erofs").exists())
        for label in ("init", "envd", "flatten-ctl"):
            self.assertEqual((self.root / "output" / label).read_bytes(),
                             (label + " fixture payload\n").encode())

    def test_go_generator_contract_rejects_retained_native_payload(self):
        self.pack()
        with self.assertRaisesRegex(ValueError, "must not contain obsolete mkfs.erofs"):
            self.read(without_mkfs=True)

    def test_legacy_contract_still_requires_mkfs(self):
        (self.tree / "opt/sandbox-runtime/bin/mkfs.erofs").unlink()
        self.pack()
        with self.assertRaises((ValueError, subprocess.SubprocessError)):
            self.read()

    def test_go_generator_contract_still_requires_flatten(self):
        (self.tree / "opt/sandbox-runtime/bin/flatten-ctl").unlink()
        self.pack()
        with self.assertRaises((ValueError, subprocess.SubprocessError)):
            self.read(without_mkfs=True)

    def test_missing_payload_rejected(self):
        (self.tree / "sbin/init").unlink()
        self.pack()
        with self.assertRaises((ValueError, subprocess.SubprocessError)):
            self.read()

    def test_link_payload_rejected(self):
        target = self.tree / "sbin/init"
        target.unlink()
        target.symlink_to("/opt/sandbox-runtime/bin/envd")
        self.pack()
        with self.assertRaises((ValueError, subprocess.SubprocessError)):
            self.read()

    def test_nonexecutable_payload_rejected(self):
        (self.tree / "sbin/init").chmod(0o644)
        self.pack()
        with self.assertRaisesRegex(ValueError, "mode 0755"):
            self.read()

    def test_prefix_tamper_rejected(self):
        self.pack()
        with self.image.open("r+b") as data:
            data.seek(4096)
            value = data.read(1)
            data.seek(4096)
            data.write(bytes([value[0] ^ 1]))
        with self.assertRaisesRegex(ValueError, "prefix digest"):
            self.read()

    def test_arbitrary_aligned_payload_rejected(self):
        self.image.write_bytes(bytes(2 << 20))
        with self.assertRaisesRegex(ValueError, "offset-zero EROFS"):
            self.read()


if __name__ == "__main__":
    if len(sys.argv) == 4 and sys.argv[1] == "--pack":
        make_bundle(Path(sys.argv[2]), Path(sys.argv[3]))
    elif len(sys.argv) == 3 and sys.argv[1] == "--prepare":
        work = Path(sys.argv[2])
        work.mkdir()
        TOOLS = build_tools(work)
        unittest.main(argv=[sys.argv[0]])
    else:
        with tempfile.TemporaryDirectory(prefix="runtime-reader-tests-") as temporary:
            TOOLS = build_tools(Path(temporary))
            unittest.main()
