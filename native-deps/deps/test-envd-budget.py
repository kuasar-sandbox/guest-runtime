#!/usr/bin/env python3
"""Check that the actual Envd recipe carries the task budget to Go."""
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent


class EnvdBudgetTests(unittest.TestCase):
    def run_build(self, budget):
        with tempfile.TemporaryDirectory(prefix='envd-budget-') as directory:
            work = Path(directory)
            source = work / 'source/packages/envd'
            source.mkdir(parents=True)
            (source / 'main.go').write_text('package main\nfunc main() {}\n')
            archive = work / 'envd.tar.gz'
            with tarfile.open(archive, 'w:gz') as tar:
                tar.add(work / 'source', arcname='source')
            tools = work / 'tools'
            tools.mkdir()
            go = tools / 'go'
            go.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
assert sys.argv[1] == 'build', sys.argv
pathlib.Path(os.environ['TEST_ENVD_CALL']).write_text(json.dumps({
    'args': sys.argv[1:], 'goflags': os.environ['GOFLAGS'],
    'gowork': os.environ['GOWORK'], 'goarch': os.environ['GOARCH'],
    'cgo': os.environ['CGO_ENABLED']}))
pathlib.Path(sys.argv[sys.argv.index('-o') + 1]).write_bytes(b'test-only-envd')
''')
            go.chmod(0o755)
            call = work / 'call.json'
            env = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ['PATH'],
                       BUILD_DIR=str(work / 'build'), BINDIR=str(work / 'bin'),
                       ENVD_SRC=str(work / 'native-source'), ENVD_TARBALL=str(archive),
                       ENVD_TARBALL_SHA256='', TARBALL_CACHE=str(work / 'tarballs'),
                       GO_ARCH='amd64', ENVD_GOFLAGS='-mod=mod -p=88', GOFLAGS='-p=4',
                       TEST_ENVD_CALL=str(call))
            env.pop('KUASAR_BUILD_JOBS', None)
            if budget is not None:
                env['KUASAR_BUILD_JOBS'] = budget
            result = subprocess.run(['bash', str(ROOT / 'build-envd.sh')], env=env,
                                    text=True, capture_output=True, timeout=15)
            return result, json.loads(call.read_text()) if call.exists() else None

    def test_budget_survives_envd_specific_flags(self):
        result, call = self.run_build('4')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(call['args'][1:3], ['-p', '4'])
        self.assertEqual(call['goflags'], '-mod=mod -p=88')
        self.assertEqual((call['gowork'], call['goarch'], call['cgo']), ('off', 'amd64', '0'))

    def test_cli_without_budget_preserves_go_defaults(self):
        result, call = self.run_build(None)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('-p', call['args'])

    def test_invalid_budget_never_invokes_compiler(self):
        for budget in ('', '0', '-1', '1.5', 'unlimited'):
            with self.subTest(budget=budget):
                result, call = self.run_build(budget)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('KUASAR_BUILD_JOBS must be a positive integer', result.stderr)
                self.assertIsNone(call)


if __name__ == '__main__':
    unittest.main()
