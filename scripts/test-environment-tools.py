#!/usr/bin/env python3
"""Release jobs consume the environment CLI without installing or selecting tools."""
import os
import re
from pathlib import Path
import shutil
import subprocess
import sys
import time
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class EnvironmentTools(unittest.TestCase):
    def cli_steps(self):
        steps = []
        for workflow in (ROOT / ".github/workflows").glob("*.yml"):
            lines = workflow.read_text().splitlines()
            for index, line in enumerate(lines):
                if line.strip() != "- name: Check environment GitHub CLI":
                    continue
                self.assertEqual(lines[index + 1].strip(), "run: |")
                prefix = line[:len(line) - len(line.lstrip())] + "    "
                command = []
                for candidate in lines[index + 2:]:
                    if not candidate.startswith(prefix):
                        break
                    command.append(candidate[len(prefix):])
                steps.append("\n".join(command))
        self.assertTrue(steps, "release jobs must check their environment CLI")
        return set(steps)

    def test_cli_accepts_environment_versions_without_mutation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            tools = root / "independent tools"
            tools.mkdir()
            cli = tools / "gh"
            for version in ("distro-build", "different-compatible-release"):
                cli.write_text("#!/bin/sh\n"
                               'if [ "$1:${2-}" = api:--help ]; then echo "  --slurp  collect pages"; exit 0; fi\n'
                               "echo " + version + "\n"
                               'printf "%s:%s\\n" "${GOTOOLCHAIN+x}" "${GOTOOLCHAIN-}" > "$OBSERVED"\n')
                cli.chmod(0o755)
                before = cli.read_bytes()
                for policy in (None, "local", "auto", "go1.99.1+path"):
                    env = dict(os.environ, PATH=str(tools), OBSERVED=str(root / "observed"),
                               GITHUB_PATH=str(root / "github-path"))
                    if policy is None:
                        env.pop("GOTOOLCHAIN", None)
                    else:
                        env["GOTOOLCHAIN"] = policy
                    for command in self.cli_steps():
                        result = subprocess.run([shutil.which("bash"), "-e", "-c", command],
                                                env=env, text=True, capture_output=True, timeout=10)
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertEqual(result.stdout.strip(), version)
                    self.assertEqual((root / "observed").read_text().strip(),
                                     ":" if policy is None else "x:" + policy)
                    self.assertEqual(cli.read_bytes(), before)
                    self.assertFalse((root / "github-path").exists())

    def test_missing_cli_fails_without_installing(self):
        with tempfile.TemporaryDirectory() as directory:
            for command in self.cli_steps():
                result = subprocess.run([shutil.which("bash"), "-e", "-c", command],
                                        env=dict(os.environ, PATH=directory),
                                        text=True, capture_output=True, timeout=10)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("gh", result.stderr)
            self.assertEqual(list(Path(directory).iterdir()), [])

    def test_incompatible_cli_fails_before_any_remote_operation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            cli = root / "gh"
            cli.write_text('#!/bin/sh\n'
                           'case "$1:${2-}" in --version:) echo old-distro-gh;; '
                           'api:--help) echo "  --paginate";; '
                           '*) echo REMOTE_OPERATION; exit 77;; esac\n')
            cli.chmod(0o755)
            for command in self.cli_steps():
                result = subprocess.run([shutil.which("bash"), "-e", "-c", command],
                                        env=dict(os.environ, PATH=directory),
                                        text=True, capture_output=True, timeout=10)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("must support api --slurp", result.stderr)
                self.assertNotIn("REMOTE_OPERATION", result.stdout)

    def test_release_jobs_do_not_pin_toolchain_policy_or_install_cli(self):
        for workflow in (ROOT / ".github/workflows").glob("*.yml"):
            source = workflow.read_text()
            self.assertNotIn("install-gh-cli.sh", source)
            self.assertNotIn("GOTOOLCHAIN: local", source)
        for directory in ("scripts", "release"):
            self.assertFalse((ROOT / directory / "install-gh-cli.sh").exists())


class RuntimeGeneratorInput(unittest.TestCase):
    def test_parallel_build_waits_for_cli_before_assembly(self):
        # An existing CLI must not let assembly inspect it while the requested
        # rebuild is still running. Standalone/prebuilt selection is covered by
        # check_generator, whose Go fixture rejects any unexpected build.
        for goal in ('build', 'all'):
            with self.subTest(goal=goal), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                binary = root / 'bin/x86_64/flatten-ctl'
                binary.parent.mkdir(parents=True)
                binary.write_text('old CLI')
                binary.chmod(0o755)
                payload = root / 'payload'
                payload.write_text('#!/bin/sh\nexit 91\n')
                payload.chmod(0o755)
                go = root / 'go'
                go.write_text('#!' + sys.executable + '\n' + r'''import pathlib,sys,time
root=pathlib.Path(__file__).parent
if sys.argv[1] == 'build':
    # The parent releases the build after giving an incorrectly parallel
    # metadata check a chance to run; no timing determines test success.
    (root/'building').touch()
    deadline=time.monotonic()+5
    while not (root/'release').exists():
        if time.monotonic() > deadline: sys.exit(92)
        time.sleep(0.01)
    pathlib.Path(sys.argv[sys.argv.index('-o')+1]).write_text('rebuilt CLI')
    (root/'built').touch()
elif sys.argv[1:3] == ['version','-m']:
    (root/'checked').touch()
    if not (root/'built').exists(): sys.exit(93)
    print('\tdep\tgithub.com/kuasar-sandbox/go-erofs\tv0.0.0-test\th1:sum')
elif sys.argv[1] == 'env':
    print('amd64')
elif sys.argv[1] == 'run':
    pathlib.Path(sys.argv[sys.argv.index('-output')+1]).write_text('bundle')
else:
    sys.exit(94)
''')
                go.chmod(0o755)
                packer = root / 'packer'
                packer.write_text('#!/bin/sh\ntouch "' + str(root / 'packed') + '"\n')
                packer.chmod(0o755)
                # Recursive make must use this same isolated Makefile.
                (root / 'Makefile').write_text((ROOT / 'Makefile').read_text())
                command = ['make', '--no-print-directory', '-j2', goal,
                           'TARGET_ARCH=x86_64', 'GO=' + str(go),
                           'BINDIR=' + str(binary.parent), 'BUILD_DIR=' + str(root / 'build'),
                           'BUILD_MKFS_EROFS=' + str(packer), 'SANDBOX_INIT=' + str(payload),
                           'ENVD=' + str(payload)]
                with (root / 'make.log').open('w+') as output:
                    process = subprocess.Popen(command, cwd=root, stdout=output, stderr=subprocess.STDOUT)
                    try:
                        deadline = time.monotonic() + 3
                        while not (root / 'building').exists() and process.poll() is None:
                            self.assertLess(time.monotonic(), deadline, 'CLI build did not start')
                            time.sleep(0.01)
                        # A broken dependency can inspect the old CLI here.
                        deadline = time.monotonic() + 0.3
                        while not (root / 'checked').exists() and time.monotonic() < deadline:
                            time.sleep(0.01)
                        (root / 'release').touch()
                        code = process.wait(timeout=10)
                    finally:
                        (root / 'release').touch()
                        process.wait(timeout=10)
                    output.seek(0)
                    self.assertEqual(code, 0, output.read())
                self.assertTrue((root / 'packed').exists())
                self.assertEqual((root / 'build/sandbox-runtime/opt/sandbox-runtime/bin/flatten-ctl').read_text(),
                                 'rebuilt CLI')

    def test_stale_incremental_cli_fails_before_staging_or_packing(self):
        self.check_generator('path\tlegacy/flatten\n', expected=False)

    def test_exact_external_generator_reaches_host_packer_without_running_payloads(self):
        self.check_generator('\tdep\tgithub.com/kuasar-sandbox/go-erofs\tv0.0.0-test\th1:fixedsum=\n', expected=True)

    def test_similarly_named_module_and_unversioned_replacement_fail(self):
        for metadata in ('\tdep\tgithub.com/kuasar-sandbox/go-erofs-extra\tv1\th1:sum\n',
                         '\tdep\tgithub.com/kuasar-sandbox/go-erofs\tv1\n=>\t../go-erofs\t(devel)\n'):
            with self.subTest(metadata=metadata):
                self.check_generator(metadata, expected=False)

    def check_generator(self, metadata, *, expected):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            payload = root / 'payload'
            payload.write_text('#!/bin/sh\ntouch "' + str(root / 'executed-payload') + '"\n')
            payload.chmod(0o755)
            metadata_file = root / 'metadata'
            metadata_file.write_text(
                str(payload) + ': go1.26.8\n'
                '\tpath\tgithub.com/kuasar-sandbox/guest-runtime/cmd/flatten-ctl\n'
                '\tmod\tgithub.com/kuasar-sandbox/guest-runtime\t(devel)\n'
                '\tbuild\tGOOS=linux\n\tbuild\tGOARCH=amd64\n' + metadata)
            go = root / 'go'
            go.write_text('#!/bin/sh\nif [ "$1" = env ]; then echo amd64; exit; fi\n'
                          'test "$1:$2:$3" = "version:-m:' + str(payload) + '" || exit 90\n'
                          'cat "' + str(metadata_file) + '"\n')
            go.chmod(0o755)
            packer = root / 'packer'
            packer.write_text('#!/bin/sh\ntouch "' + str(root / 'packed') + '"\nexit 23\n')
            packer.chmod(0o755)
            stage = root / 'build/sandbox-runtime'
            stage.mkdir(parents=True)
            (stage / 'unchanged').write_text('original staging')
            result = subprocess.run(['make', '--no-print-directory', '-f', str(ROOT / 'Makefile'),
                                     'sandbox-runtime', 'TARGET_ARCH=x86_64', 'GO=' + str(go),
                                     'BINDIR=' + str(root / 'bin'), 'BUILD_DIR=' + str(root / 'build'),
                                     'BUILD_MKFS_EROFS=' + str(packer), 'SANDBOX_INIT=' + str(payload),
                                     'ENVD=' + str(payload), 'FLATTEN_CTL=' + str(payload)],
                                    cwd=root, text=True, capture_output=True, timeout=15)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse((root / 'executed-payload').exists())
            self.assertEqual((root / 'packed').exists(), expected, result.stderr)
            self.assertEqual((stage / 'unchanged').exists(), not expected)
            if not expected:
                self.assertIn('lacks the linked Go EROFS generator', result.stderr)
            # Exercise the standalone package/validate gate independently of
            # Makefile staging, inspecting metadata without running a payload.
            source = (ROOT / 'scripts/release.sh').read_text()
            function = re.search(r'(?ms)^check_go_binary\(\) \{\n.*?^\}', source).group()
            script = ('set -euo pipefail\n'
                      'fail() { echo "$*" >&2; exit 1; }\n'
                      'check_target_binary() { :; }\n' + function +
                      '\ncheck_go_binary "$1" x86_64\n')
            result = subprocess.run(['bash', '-c', script, 'test', str(payload)],
                                    env=dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH']),
                                    text=True, capture_output=True, timeout=10)
            self.assertEqual(result.returncode == 0, expected, result.stderr)
            self.assertFalse((root / 'executed-payload').exists())
            if not expected:
                self.assertIn('lacks the linked Go EROFS generator', result.stderr)



if __name__ == "__main__":
    unittest.main()
