"""Exercise trusted host EROFS readers with private synthetic images."""
import hashlib
import importlib.util
import io
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

    def pack(self):
        erofs = self.root / "raw.erofs"
        subprocess.run([str(TOOLS / "bin/mkfs.erofs"), "--all-root", "-T0", "-U", "00000000-0000-0000-0000-000000000000", str(erofs), str(self.tree)],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True, timeout=30)
        make_bundle(erofs, self.image)

    def read(self):
        READER.read_payloads(self.image, TOOLS / "bin/fsck.erofs",
                             TOOLS / "build/src/erofs-utils/dump/dump.erofs", self.root / "output")

    def test_real_erofs_reads_only_fixed_files_privately(self):
        self.pack()
        self.read()
        self.assertEqual((self.root / "output").stat().st_mode & 0o777, 0o700)
        for label, path in READER.PAYLOADS.items():
            output = self.root / "output" / label
            self.assertEqual(output.read_bytes(), (self.tree / path.lstrip("/")).read_bytes())
            self.assertEqual(output.stat().st_mode & 0o777, 0o600)

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
