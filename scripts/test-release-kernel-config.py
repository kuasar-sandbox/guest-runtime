"""Check release-only Kconfig metadata selection, without compiling Linux."""
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

SOURCE = Path(__file__).with_name("release.sh").read_text()
FUNCTION = re.search(r"(?ms)^prepare_release_kernel_config\(\) \{\n.*?^\}", SOURCE).group()


class KernelReleaseConfig(unittest.TestCase):
    def test_metadata_override_preserves_other_configuration_and_is_idempotent(self):
        for original in ("CONFIG_LOCALVERSION_AUTO=y\n", "# CONFIG_LOCALVERSION_AUTO is not set\n", ""):
            with self.subTest(original=original), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                fragment = root / "fresh/native-deps/deps/vmlinux/sandbox-common.config"
                fragment.parent.mkdir(parents=True)
                fragment.write_text('CONFIG_LOCALVERSION="-kuasar"\nCONFIG_SMP=y\n' + original)
                command = ["bash", "-c", 'set -euo pipefail\nWORK=$1\nfail() { exit 1; }\n'
                           + FUNCTION + '\nprepare_release_kernel_config "$2"', "_", directory, str(root / "fresh")]
                first = subprocess.run(command, text=True, capture_output=True, timeout=10)
                self.assertEqual(first.returncode, 0, first.stderr)
                result = fragment.read_text()
                self.assertIn('CONFIG_LOCALVERSION="-kuasar"\nCONFIG_SMP=y\n', result)
                self.assertNotIn("CONFIG_LOCALVERSION_AUTO=", result)
                self.assertEqual(result.count("# CONFIG_LOCALVERSION_AUTO is not set"), 1)
                second = subprocess.run(command, text=True, capture_output=True, timeout=10)
                self.assertEqual(second.returncode, 0, second.stderr)
                self.assertEqual(fragment.read_text(), result)
                self.assertEqual(fragment.read_text().splitlines().count("# CONFIG_LOCALVERSION_AUTO is not set"), 1)
                self.assertEqual(fragment.stat().st_mode & 0o777, 0o644)
        self.assertIn('LD="$kernel_ld" LOCALVERSION=', SOURCE)
        self.assertIn('release Kernel config retained Git-derived local versions', SOURCE)


if __name__ == "__main__":
    unittest.main()
