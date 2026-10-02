#!/usr/bin/env python3
"""Exercise build-vmlinux's resolved-config guard without downloading a kernel."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent
COMMON = ['CONFIG_ZONE_DEVICE=y', 'CONFIG_FS_DAX=y',
          'CONFIG_VIRTIO_PMEM=y', 'CONFIG_BLK_DEV_PMEM=y']
ARM = ['CONFIG_ARM64_PMEM=y', 'CONFIG_ARCH_HAS_PMEM_API=y',
       'CONFIG_ARCH_HAS_UACCESS_FLUSHCACHE=y']
X86 = ['CONFIG_SMP=y', 'CONFIG_NR_CPUS=4', 'CONFIG_X86_X2APIC=y']


class ResolvedKernelConfigTests(unittest.TestCase):
    def run_build(self, arch, config):
        with tempfile.TemporaryDirectory(prefix='vmlinux-config-') as directory:
            root = Path(directory)
            tools = root / 'tools'
            tools.mkdir()
            source = root / 'source'
            source.mkdir()
            output = root / 'objects'
            binary = root / 'bin'
            # These stand-ins model Kconfig dropping a requested dependency.
            # They validate the build-script guard, not actual Kconfig or KVM.
            for name in ('bc', 'bison', 'flex', 'gcc', 'pkg-config'):
                path = tools / name
                path.write_text('#!/bin/sh\nexit 0\n')
                path.chmod(0o755)
            make = tools / 'make'
            make.write_text(r'''#!/usr/bin/env python3
import os
from pathlib import Path
import sys
args = sys.argv[1:]
out = Path(next(a[2:] for a in args if a.startswith('O=')))
out.mkdir(parents=True, exist_ok=True)
if 'sandbox_defconfig' in args:
    (out / '.config').write_text(os.environ['TEST_RESOLVED_CONFIG'])
elif args[-1] in ('Image', 'vmlinux'):
    target = 'arch/arm64/boot/Image' if args[-1] == 'Image' else 'vmlinux'
    path = out / target
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(b'test-only-kernel-build-output')
    (out / 'compiled').write_text('yes')
''')
            make.chmod(0o755)
            env = dict(os.environ, STAGE='build', KERNEL_ARCH=arch, CROSS_PREFIX='',
                       LINUX_BUILD_SRC=str(source), LINUX_BUILD_OUT=str(output),
                       BINDIR=str(binary), BUILD_DIR=str(root / 'build'),
                       TEST_RESOLVED_CONFIG='\n'.join(config) + '\n',
                       PATH=str(tools) + os.pathsep + os.environ['PATH'])
            result = subprocess.run(['bash', str(ROOT / 'build-vmlinux.sh')],
                                    env=env, capture_output=True, text=True, timeout=15)
            return result, (output / 'compiled').exists(), (binary / 'vmlinux').exists()

    def test_arm_fragment_explicitly_selects_pmem(self):
        self.assertIn('CONFIG_ARM64_PMEM=y',
                      (ROOT / 'vmlinux/sandbox-arm64.config').read_text().splitlines())

    def test_complete_arm_and_x86_configs_build(self):
        for arch, selected in (('arm64', COMMON + ARM), ('x86_64', COMMON + X86)):
            with self.subTest(arch=arch):
                result, compiled, copied = self.run_build(arch, selected)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertTrue(compiled)
                self.assertTrue(copied)

    def test_missing_arm_dependency_fails_before_compilation(self):
        for missing in COMMON + ARM:
            with self.subTest(missing=missing):
                result, compiled, copied = self.run_build(
                    'arm64', [item for item in COMMON + ARM if item != missing])
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('missing ' + missing, result.stderr)
                self.assertFalse(compiled)
                self.assertFalse(copied)

    def test_x86_does_not_require_arm_symbols(self):
        result, compiled, copied = self.run_build('x86_64', COMMON + X86)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(compiled and copied)


if __name__ == '__main__':
    unittest.main()
