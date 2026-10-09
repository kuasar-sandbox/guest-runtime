#!/usr/bin/env python3
"""Exercise build-vmlinux's resolved-config guard without downloading a kernel."""
import os
import io
import shutil
from pathlib import Path
import subprocess
import tempfile
import tarfile
import unittest

ROOT = Path(__file__).resolve().parent
COMMON = ['CONFIG_ZONE_DEVICE=y', 'CONFIG_FS_DAX=y',
          'CONFIG_VIRTIO_PMEM=y', 'CONFIG_BLK_DEV_PMEM=y']
ARM = ['CONFIG_ARM64_PMEM=y', 'CONFIG_ARCH_HAS_PMEM_API=y',
       'CONFIG_ARCH_HAS_UACCESS_FLUSHCACHE=y']
X86 = ['CONFIG_SMP=y', 'CONFIG_NR_CPUS=4', 'CONFIG_X86_X2APIC=y']


class ResolvedKernelConfigTests(unittest.TestCase):
    def test_synthetic_source_commits_have_stable_scoped_timestamps(self):
        with tempfile.TemporaryDirectory(prefix='vmlinux-source-dates-') as directory:
            root = Path(directory)
            archive = root / 'kernel.tar.gz'
            with tarfile.open(archive, 'w:gz') as output:
                member = tarfile.TarInfo('linux/COPYING')
                content = b'one\n'
                member.size = len(content)
                output.addfile(member, io.BytesIO(content))
            patches = root / 'patches'
            patches.mkdir()
            (patches / '0001-fixture.patch').write_text('''From: Fixture <fixture@example.test>
Date: Thu, 1 Jan 1970 00:02:03 +0000
Subject: [PATCH] native source fixture

diff --git a/COPYING b/COPYING
--- a/COPYING
+++ b/COPYING
@@ -1 +1 @@
-one
+two
''')
            commits = []
            for index in range(3):
                source = root / str(index) / 'source'
                env = dict(os.environ, LINUX_TARBALL=str(archive), LINUX_TARBALL_SHA256='',
                           LINUX_BUILD_SRC=str(source), BUILD_DIR=str(root / str(index) / 'build'),
                           TARBALL_CACHE=str(root / str(index) / 'tarballs'),
                           LINUX_PATCHES_DIR=str(patches), GIT_CONFIG_NOSYSTEM='1',
                           GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_COUNT='1',
                           GIT_CONFIG_KEY_0='commit.gpgsign', GIT_CONFIG_VALUE_0='false')
                for name in ('GIT_AUTHOR_DATE', 'GIT_COMMITTER_DATE', 'SOURCE_DATE_EPOCH'):
                    env.pop(name, None)
                if index == 2:
                    env.update(GIT_AUTHOR_DATE='@123 +0000', GIT_COMMITTER_DATE='@456 +0000')
                for stage in ('fetch', 'patches-apply'):
                    result = subprocess.run(['bash', str(ROOT / 'build-vmlinux.sh')],
                                            env=dict(env, STAGE=stage), text=True,
                                            capture_output=True, timeout=15)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    dates = subprocess.check_output(['git', '-C', str(source), 'show',
                                                     '-s', '--format=%at %ct', 'HEAD'], text=True).strip()
                    author = 123 if stage == 'patches-apply' or index == 2 else 0
                    committer = 456 if index == 2 else 0
                    self.assertEqual(dates, f'{author} {committer}')
                commits.append(subprocess.check_output(['git', '-C', str(source), 'rev-parse', 'HEAD'],
                                                       text=True).strip())
            self.assertEqual(commits[0], commits[1])
            self.assertNotEqual(commits[0], commits[2])

    def run_build(self, arch, config, budget=None):
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
            nproc = tools / 'nproc'
            nproc.write_text('#!/bin/sh\nprintf "88\\n"\n')
            nproc.chmod(0o755)
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
    assert '-j' + os.environ['TEST_EXPECTED_JOBS'] in args, args
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
                       TEST_EXPECTED_JOBS=budget if budget is not None else '88',
                       PATH=str(tools) + os.pathsep + os.environ['PATH'])
            env.pop('KUASAR_BUILD_JOBS', None)
            if budget is not None:
                env['KUASAR_BUILD_JOBS'] = budget
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

    def test_task_budget_overrides_host_processor_count(self):
        for arch, selected in (('arm64', COMMON + ARM), ('x86_64', COMMON + X86)):
            with self.subTest(arch=arch):
                result, compiled, copied = self.run_build(arch, selected, budget='4')
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertTrue(compiled and copied)

    def test_invalid_task_budget_fails_before_compilation(self):
        for budget in ('', '0', '-1', '1.5', 'unlimited'):
            with self.subTest(budget=budget):
                result, compiled, copied = self.run_build('x86_64', COMMON + X86, budget=budget)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('KUASAR_BUILD_JOBS must be a positive integer', result.stderr)
                self.assertFalse(compiled or copied)

    def test_missing_arm_dependency_fails_before_compilation(self):
        for missing in COMMON + ARM:
            with self.subTest(missing=missing):
                result, compiled, copied = self.run_build(
                    'arm64', [item for item in COMMON + ARM if item != missing])
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('missing ' + missing, result.stderr)
                self.assertFalse(compiled)
                self.assertFalse(copied)

    def test_arm_fragment_change_invalidates_existing_kernel_target(self):
        with tempfile.TemporaryDirectory(prefix='kernel-inputs-') as directory:
            root = Path(directory)
            shutil.copytree(ROOT, root / 'deps')
            shutil.copy2(ROOT.parent / 'Makefile', root / 'Makefile')
            for path in sorted(root.rglob('*'), reverse=True):
                os.utime(path, (1, 1))
            binary = root / 'bin/aarch64/vmlinux'
            binary.parent.mkdir(parents=True)
            binary.write_bytes(b'previous-kernel-output')
            os.utime(binary, (10, 10))
            command = ['make', '-n', '-C', str(root), 'TARGET_ARCH=aarch64', 'vmlinux']
            before = subprocess.run(command, capture_output=True, text=True, check=True, timeout=10)
            self.assertNotIn('STAGE=build', before.stdout)
            os.utime(root / 'deps/vmlinux/sandbox-arm64.config', (20, 20))
            after = subprocess.run(command, capture_output=True, text=True, check=True, timeout=10)
            self.assertIn('STAGE=build', after.stdout)
            self.assertEqual(binary.read_bytes(), b'previous-kernel-output')

    def test_x86_does_not_require_arm_symbols(self):
        result, compiled, copied = self.run_build('x86_64', COMMON + X86)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(compiled and copied)


if __name__ == '__main__':
    unittest.main()
