"""Exercise the actual unit-specific source-key gate without fetching a source."""
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

SOURCE = Path(__file__).with_name("release.sh").read_text()
FUNCTION = re.search(r"(?ms)^validate_source_inventory\(\) \{\n.*?^\}", SOURCE).group()
HEADER = "payload\tname\tversion\tsource\tintegrity\tlicense_directory\n"
EROFS = "bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs"


class SourceInventory(unittest.TestCase):
    def validate(self, unit, row):
        with tempfile.TemporaryDirectory() as directory:
            table = Path(directory) / "share/sources" / unit / "SOURCES.tsv"
            table.parent.mkdir(parents=True)
            table.write_text(HEADER + "\t".join(row) + "\n")
            script = 'set -euo pipefail\nfail() { echo "$*" >&2; exit 1; }\n' + FUNCTION
            return subprocess.run(["bash", "-c", script + '\nvalidate_source_inventory "$1" "$2"',
                                   "_", directory, unit], capture_output=True, text=True, timeout=10)

    def test_known_unit_source_keys_pass_and_other_names_fail(self):
        for unit, payload, name in (
                ("runtime", "bin/sandbox-runtime.bundle,bin/flatten-ctl", "guest-runtime"),
                ("runtime", "bin/sandbox-runtime.bundle:/sbin/init", "sandboxer"),
                ("runtime", "bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd", "envd"),
                ("runtime", "bin/flatten-ctl", "Go toolchain"),
                ("runtime", EROFS, "erofs-utils"),
                ("runtime", EROFS, "guest-runtime-erofs-patches"),
                ("vmlinux", "bin/vmlinux", "linux"),
                ("vmlinux", "bin/vmlinux", "guest-runtime-kernel-inputs")):
            row = [payload, name, "fixture", "fixture", "fixture", "fixture"]
            self.assertEqual(self.validate(unit, row).returncode, 0)
            for column, value in ((0, "bin/other"), (1, "fabricated-source")):
                changed = list(row)
                changed[column] = value
                result = self.validate(unit, changed)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("unrecognized or inconsistent source inventory record", result.stderr)

    def test_native_input_identity_and_own_license_directory_are_required(self):
        for unit, payload, name in (("runtime", EROFS, "libgcc.a"),
                                    ("runtime", EROFS, "crtbeginT.o")):
            row = [payload, "system:" + name, "1.2.3", "deb-source:fixture@1.2.3",
                   "sha256:" + "a" * 64 + ";package:fixture", "share/licenses/" + unit + "/system/" + name]
            self.assertEqual(self.validate(unit, row).returncode, 0)
            rpm = list(row)
            rpm[3] = "rpm-source:fixture-1.2.3.src.rpm"
            self.assertEqual(self.validate(unit, rpm).returncode, 0)
            for column, value in ((0, "bin/other"), (1, "system:unknown"),
                                   (2, "wrong version"), (3, "https://example.invalid"),
                                   (4, "sha256:invalid;package:fixture"),
                                   (4, row[4] + ";extra:field"),
                                   (5, "share/licenses/" + unit + "/project")):
                changed = list(row)
                changed[column] = value
                self.assertNotEqual(self.validate(unit, changed).returncode, 0)


    def test_existing_source_built_libuuid_catalog_is_accepted(self):
        row = [EROFS, "system:libuuid.a", "util-linux-2.23.2.tar.xz",
               "https://example.invalid/util-linux-2.23.2-65.src.rpm",
               "sha256:" + "a" * 64 + ";tarball-sha256:" + "b" * 64 + ";srpm-sha256:" + "c" * 64,
               "share/licenses/runtime/system/libuuid.a"]
        self.assertEqual(self.validate("runtime", row).returncode, 0)
        for column, value in ((0, "bin/other"), (1, "system:libgcc.a"),
                              (3, "https://example.invalid/not-an-srpm"),
                              (4, row[4].replace("b" * 64, "invalid")),
                              (4, row[4] + ";extra:field"),
                              (5, "share/licenses/runtime/project")):
            changed = list(row)
            changed[column] = value
            self.assertNotEqual(self.validate("runtime", changed).returncode, 0)


if __name__ == "__main__":
    unittest.main()
