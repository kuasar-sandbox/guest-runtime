#!/usr/bin/env python3
import copy
import importlib.util
import io
import json
import os
import shutil
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location('prepare_sandbox_init', Path(__file__).with_name('prepare-sandbox-init.py'))
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)


class SelectedSandboxInitTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(); self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.version, self.sha = 'v1.2.3-preview.20261104.1', 'a' * 40
        self.archive = self.root / f'sandboxer-{self.version}-linux-x86_64.tar.gz'
        self.output, self.notices = self.root / 'bin/sandbox-init', self.root / 'selected-go'
        self.payload = b'\x7fELF\x02\x01\x01' + bytes(11) + b'\x3e\x00' + bytes(44) + b'exact selected binary'
        self.toolchain = 'go1.26.8'
        self.write_archive()

    def write_archive(self, name='bin/sandbox-init', mode=0o755, kind=tarfile.REGTYPE,
                      legacy=False, extras=(), omit_license=False):
        rows = [(name, self.payload, mode, kind)]
        if not omit_license:
            rows.append((f'share/licenses/sandboxer/go-toolchain/{self.toolchain}/LICENSE', b'Go license', 0o644, tarfile.REGTYPE))
        if legacy:
            for suffix in ('MATERIALS.sha256', 'SOURCES.tsv', 'GO-BUILD-INFO.tsv'):
                rows.append(('share/sources/sandboxer/' + suffix, b'ignored historical record', 0o644, tarfile.REGTYPE))
        rows.extend(extras)
        with tarfile.open(self.archive, 'w:gz') as bundle:
            for path, data, permissions, entry_type in rows:
                entry = tarfile.TarInfo('./' + path)
                entry.mode, entry.type = permissions, entry_type
                entry.size = len(data) if entry_type == tarfile.REGTYPE else 0
                entry.linkname = '/outside' if entry_type in (tarfile.SYMTYPE, tarfile.LNKTYPE) else ''
                bundle.addfile(entry, io.BytesIO(data) if entry.size else None)
        self.release = dict(tag_name=self.version, target_commitish='main', draft=False, prerelease=True,
                            body='Human notes only.', assets=[dict(id=123, name=self.archive.name,
                            size=self.archive.stat().st_size, digest='sha256:' + helper.digest(self.archive), state='uploaded')])
        if legacy:
            # No local checksum file is needed, and its invalid metadata is ignored.
            self.release['assets'].append(dict(name='SHA256SUMS'))
            self.release['body'] = '<!-- kuasar-release-source invalid legacy marker -->'

    def prepare(self, arch='x86_64'):
        with mock.patch.object(helper, '_binary_go_toolchain', return_value=self.toolchain):
            helper.prepare(self.release, self.root, self.version, self.sha, self.output, self.notices, arch)

    def test_installs_exact_new_and_old_packages_without_historical_records(self):
        for legacy in (False, True):
            with self.subTest(legacy=legacy):
                self.write_archive(legacy=legacy); self.prepare()
                self.assertEqual(self.output.read_bytes(), self.payload)
                self.assertEqual(self.output.stat().st_mode & 0o7777, 0o755)
                self.assertEqual((self.notices / self.toolchain / 'LICENSE').read_bytes(), b'Go license')
                self.assertFalse((self.root / 'SHA256SUMS').exists())

    def test_fetch_uses_one_native_asset_id_and_replaces_stale_local_binary(self):
        self.output.parent.mkdir()
        self.output.write_bytes(b"old development binary")
        commands = []
        def download(command, **kwargs):
            commands.append(command)
            kwargs["stdout"].write(self.archive.read_bytes())
            return subprocess.CompletedProcess(command, 0)
        with mock.patch.object(helper.subprocess, 'check_output', return_value=json.dumps(self.release)) as metadata, \
             mock.patch.object(helper.subprocess, 'run', side_effect=download), \
             mock.patch.object(helper, '_binary_go_toolchain', return_value=self.toolchain):
            helper.fetch(self.version, self.sha, self.output, self.notices)
        metadata.assert_called_once_with(['gh', 'api',
            f'repos/kuasar-sandbox/sandboxer/releases/tags/{self.version}'], text=True)
        self.assertEqual(commands, [['gh', 'api', '--allow-escape-sequences',
            '-H', 'Accept: application/octet-stream', 'repos/kuasar-sandbox/sandboxer/releases/assets/123']])
        self.assertEqual(self.output.read_bytes(), self.payload)
        self.assertEqual((self.notices / self.toolchain / 'LICENSE').read_bytes(), b'Go license')

    def test_fetch_failure_never_accepts_stale_input(self):
        for failure in ('download', 'digest', 'missing'):
            with self.subTest(failure=failure):
                self.output.parent.mkdir(exist_ok=True)
                self.output.write_bytes(b'old development binary')
                release = copy.deepcopy(self.release)
                if failure == 'missing': release['assets'] = []
                def download(command, **kwargs):
                    if failure == 'download': raise subprocess.CalledProcessError(1, command)
                    kwargs['stdout'].write(b'corrupt bytes')
                    return subprocess.CompletedProcess(command, 0)
                with mock.patch.object(helper.subprocess, 'check_output', return_value=json.dumps(release)), \
                     mock.patch.object(helper.subprocess, 'run', side_effect=download):
                    with self.assertRaises((ValueError, subprocess.CalledProcessError)):
                        helper.fetch(self.version, self.sha, self.output, self.notices)
                self.assertEqual(self.output.read_bytes(), b'old development binary')
                self.assertFalse(self.notices.exists())

    def test_local_make_release_preserves_selected_paths_and_stops_on_fetch_failure(self):
        make = shutil.which('make')
        self.assertIsNotNone(make)
        project, tools = self.root / 'runtime', self.root / 'tools'
        project.mkdir(); tools.mkdir(); (project / 'scripts').mkdir()
        (project / 'Makefile').write_bytes(Path(__file__).resolve().parents[1].joinpath('Makefile').read_bytes())
        log = self.root / 'calls'
        selected = self.root / 'custom sandboxer'
        selected.mkdir()
        (selected / 'bin').mkdir(); (selected / 'bin/sandbox-init').write_bytes(b'stale')
        python = tools / 'python3'
        python.write_text('#!/usr/bin/python3\n' + """import os,sys,pathlib
args=sys.argv[1:]
assert '--fetch' in args and args[args.index('--version')+1] == 'v1.2.3'
with open(os.environ['CALLS'],'a') as f:f.write('fetch\\n')
if os.environ.get('FETCH_FAIL'):raise SystemExit(23)
output=pathlib.Path(args[args.index('--output')+1]);output.parent.mkdir(parents=True,exist_ok=True);output.write_bytes(b'selected release')
pathlib.Path(args[args.index('--go-toolchain-output')+1]).mkdir()
""")
        python.chmod(0o755)
        git = tools / 'git'
        git.write_text('#!/bin/sh\ncase "$*" in *rev-parse*) printf "%040d\\n" 1;; *) echo 123;; esac\n');git.chmod(0o755)
        child = tools / 'child-make'
        child.write_text('#!/usr/bin/python3\n' + """import os,sys,pathlib
assert sys.argv[1]=='sandbox-runtime'
path=pathlib.Path(sys.argv[2].removeprefix('SANDBOX_INIT='))
assert path.read_bytes()==b'selected release'
with open(os.environ['CALLS'],'a') as f:f.write('assemble:'+str(path)+'\\n')
""")
        child.chmod(0o755)
        (project / 'scripts/release.sh').write_text("""#!/bin/bash
set -euo pipefail
[ "$(cat "$RELEASE_SANDBOX_INIT_BIN")" = 'selected release' ]
[ -d "$RELEASE_MATERIALS_GO_NOTICE_ROOT" ]
printf 'package:%s\\nsource:%s\\n' "$RELEASE_SANDBOX_INIT_BIN" "$RELEASE_SANDBOXER_SOURCE_DIR" >> "$CALLS"
""")
        environment = dict(os.environ, PATH=str(tools)+os.pathsep+os.environ['PATH'], CALLS=str(log))
        command = [make, '-s', 'release-runtime', 'SANDBOXER_VERSION=v1.2.3',
                   'SANDBOXER_DIR='+str(selected), 'MAKE='+str(child)]
        result = subprocess.run(command, cwd=project, env=environment, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        expected = project / 'build' / os.uname().machine / 'release-inputs/sandbox-init'
        self.assertEqual(log.read_text().splitlines(), ['fetch','assemble:'+str(expected),
            'package:'+str(expected),'source:'+str(selected)])
        log.unlink()
        result = subprocess.run(command, cwd=project, env=dict(environment, FETCH_FAIL='1'), text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(log.read_text().splitlines(), ['fetch'])

    def test_rejects_wrong_tag_or_publication_state(self):
        initial = copy.deepcopy(self.release)
        for key, value in (('tag_name', 'v1.2.2'), ('draft', True), ('prerelease', False)):
            self.release = dict(initial, **{key: value})
            with self.assertRaises(ValueError): self.prepare()
            self.assertFalse(self.output.exists())

    def test_only_local_architecture_is_required(self):
        arm = dict(self.release['assets'][0], id=124, name=self.archive.name.replace('x86_64', 'aarch64'))
        self.release['assets'].append(arm)
        self.prepare(); self.assertEqual(self.output.read_bytes(), self.payload)
        self.assertFalse((self.root / arm['name']).exists())

    def test_arm_binary_is_checked_independently(self):
        self.payload = self.payload[:18] + b'\xb7\x00' + self.payload[20:]
        self.archive = self.root / self.archive.name.replace('x86_64', 'aarch64')
        self.write_archive()
        self.release['assets'].append(dict(self.release['assets'][0], id=124,
            name=self.archive.name.replace('aarch64', 'x86_64')))
        self.prepare('aarch64')
        self.assertEqual(self.output.read_bytes(), self.payload)

    def test_incomplete_remote_architecture_set_is_rejected(self):
        self.release['assets'][0]['name'] = self.archive.name.replace('x86_64', 'aarch64')
        with self.assertRaisesRegex(ValueError, 'unexpected asset set'): self.prepare('aarch64')

    def test_wrong_architecture_is_rejected(self):
        self.payload = self.payload[:18] + b'\xb7\x00' + self.payload[20:]
        self.write_archive()
        with self.assertRaisesRegex(ValueError, 'wrong architecture'): self.prepare()
        self.assertFalse(self.output.exists())

    def test_corrupt_download_preserves_existing_binary(self):
        self.output.parent.mkdir(); self.output.write_bytes(b'existing')
        self.archive.write_bytes(self.archive.read_bytes() + b'corruption')
        with self.assertRaisesRegex(ValueError, 'size/state'): self.prepare()
        self.assertEqual(self.output.read_bytes(), b'existing')

    def test_digest_mismatch_is_rejected(self):
        self.release['assets'][0]['digest'] = 'sha256:' + '0'*64
        with self.assertRaisesRegex(ValueError, 'GitHub asset digest'): self.prepare()

    def test_missing_or_invalid_native_metadata_is_rejected(self):
        initial = copy.deepcopy(self.release)
        for key, value in (('id', None), ('id', True), ('size', 0), ('digest', None),
                           ('digest', 'sha256:wrong'), ('state', 'starter')):
            self.release = copy.deepcopy(initial); self.release['assets'][0][key] = value
            with self.assertRaises(ValueError): self.prepare()
            self.assertFalse(self.output.exists())

    def test_missing_file_or_duplicate_assets_are_rejected(self):
        original = copy.deepcopy(self.release)
        for assets in ([], original['assets'] * 2, [dict(original['assets'][0], name='unknown.tar.gz')]):
            self.release = dict(original, assets=assets)
            with self.assertRaises(ValueError): self.prepare()
        self.assertFalse(self.output.exists())

    def test_missing_init_or_license_is_rejected(self):
        self.write_archive(name='bin/other')
        with self.assertRaisesRegex(ValueError, 'one regular executable'): self.prepare()
        self.write_archive(omit_license=True)
        with self.assertRaisesRegex(ValueError, 'notices are missing'): self.prepare()
        self.assertFalse(self.output.exists())

    def test_dangerous_permissions_are_rejected_before_chmod(self):
        for mode in (0o4755, 0o2755, 0o1777, 0o775, 0o757, 0o644):
            self.write_archive(mode=mode)
            with self.assertRaises(ValueError): self.prepare()
            self.assertFalse(self.output.exists())

    def test_linked_duplicate_and_conflicting_members_are_rejected(self):
        for kind in (tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.FIFOTYPE):
            self.write_archive(kind=kind)
            with self.assertRaises(ValueError): self.prepare()
        for name in ('bin/sandbox-init', 'bin'):
            self.write_archive(extras=[(name, b'conflict', 0o644, tarfile.REGTYPE)])
            with self.assertRaises(ValueError): self.prepare()
        self.assertFalse(self.output.exists())

    def test_unsafe_paths_anywhere_in_archive_are_rejected(self):
        for name in ('../owned', '/absolute', 'foo//bar', 'foo/./bar', 'foo/../bar',
                     'foo\\bar', 'foo\nbar'):
            with self.subTest(name=name):
                self.write_archive(extras=[(name, b'bad', 0o644, tarfile.REGTYPE)])
                with self.assertRaisesRegex(ValueError, 'unsafe path'): self.prepare()
        self.assertFalse(self.output.exists())

    def test_toolchain_comes_from_binary_and_unsafe_value_is_rejected(self):
        for toolchain in ('go1.26.8/../../owned', 'unexpected'):
            self.toolchain = toolchain
            with self.assertRaisesRegex(ValueError, 'toolchain record'): self.prepare()
        self.assertFalse(self.output.exists())

    def test_missing_binary_toolchain_notices_are_rejected(self):
        self.toolchain = 'go1.26.9'
        with self.assertRaisesRegex(ValueError, 'notices are missing'): self.prepare()

    def test_output_symlink_ancestor_is_rejected(self):
        elsewhere = self.root / 'elsewhere'; elsewhere.mkdir()
        self.output.parent.symlink_to(elsewhere, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, 'symlink path'): self.prepare()
        self.assertEqual(list(elsewhere.iterdir()), [])

    def test_go_version_parser_checks_actual_output(self):
        with mock.patch.object(helper.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0,
                '/tmp/binary: go1.26.8 X:arenas\n', '')):
            self.assertEqual(helper._binary_go_toolchain(self.payload, self.root), 'go1.26.8')
        with mock.patch.object(helper.subprocess, 'run', return_value=subprocess.CompletedProcess([], 1, '', 'invalid')):
            with self.assertRaisesRegex(ValueError, 'cannot read'): helper._binary_go_toolchain(self.payload, self.root)


if __name__ == '__main__': unittest.main()
