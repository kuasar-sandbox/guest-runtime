"""Exercise the actual standalone archive-path gate before extraction."""
import io
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SOURCE = (ROOT / "scripts/release.sh").read_text()
FUNCTION = re.search(r"(?ms)^validate_archive_paths\(\) \{\n.*?^\}", SOURCE).group()


class ArchiveLayout(unittest.TestCase):
    def validate(self, kind, extra=None):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = root / "test.tar.gz"
            entries = [("./", True), ("./bin/", True), ("./share/", True),
                       ("./share/licenses/", True), ("./share/sources/", True),
                       ("./share/licenses/" + kind + "/", True),
                       ("./share/licenses/" + kind + "/project/", True),
                       ("./share/licenses/" + kind + "/project/LICENSE", False),
                       ("./share/sources/" + kind + "/", True)]
            entries += [("./bin/" + name, False) for name in (
                ("sandbox-runtime.bundle", "flatten-ctl", "mkfs.erofs") if kind == "runtime" else ("vmlinux",))]
            if extra is not None:
                entries.append(extra)
            with tarfile.open(archive, "w:gz", format=tarfile.GNU_FORMAT) as output:
                for name, is_directory in entries:
                    item = tarfile.TarInfo(name)
                    item.type = tarfile.DIRTYPE if is_directory else tarfile.REGTYPE
                    item.mode = 0o755 if is_directory or name in ("./bin/flatten-ctl", "./bin/mkfs.erofs") else 0o644
                    item.size = 0 if is_directory else 8
                    output.addfile(item, None if is_directory else io.BytesIO(b"fixture\n"))
            script = 'set -euo pipefail\nWORK=$1\nROOT=$4\nfail() { echo "$*" >&2; exit 1; }\n'
            return subprocess.run(["bash", "-c", script + FUNCTION + '\nvalidate_archive_paths "$2" "$3"',
                                   "_", directory, str(archive), kind, str(ROOT)],
                                  text=True, capture_output=True, timeout=10)

    def test_exact_unit_layouts_pass(self):
        for kind in ("runtime", "vmlinux"):
            result = self.validate(kind)
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_foreign_directories_cannot_change_deployment_root_modes(self):
        for kind in ("runtime", "vmlinux"):
            for name in ("./root/", "./etc/", "./bin/extra/", "./share/licenses/foreign/", "./share/sources/foreign/"):
                with self.subTest(kind=kind, name=name):
                    result = self.validate(kind, (name, True))
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("outside the exact", result.stderr)

    def test_runtime_and_kernel_payloads_are_not_interchangeable(self):
        for kind, name in (("runtime", "./bin/vmlinux"), ("runtime", "./bin/other"),
                           ("vmlinux", "./bin/sandbox-runtime.bundle"), ("vmlinux", "./bin/flatten-ctl")):
            result = self.validate(kind, (name, False))
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("outside the exact", result.stderr)

    def test_aliases_and_duplicate_entries_are_rejected(self):
        for name in ("bin/vmlinux", "./bin//vmlinux", "./bin/./vmlinux", "./bin/../vmlinux"):
            result = self.validate("vmlinux", (name, False))
            self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
