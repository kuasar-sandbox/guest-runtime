#!/usr/bin/env python3
"""Run real mkfs/fsck binaries; unsupported test prerequisites are failures."""
import hashlib
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tarfile

candidate, fsck, upstream, upstream_fsck, work = map(Path, sys.argv[1:])
work.mkdir(parents=True, exist_ok=True)
source = work / 'input'
scratch = work / 'scratch'
source.mkdir()
scratch.mkdir()
env = os.environ.copy()
env.pop('EROFS_INDEX_STORAGE', None)
env.pop('SOURCE_DATE_EPOCH', None)
env['LC_ALL'] = 'C'
env['TMPDIR'] = str(scratch)
common = ['-T', '1700000000', '-U', '01234567-89ab-cdef-0123-456789abcdef',
          '--all-root', '--chunksize=4096']


def run(args, environ=env, error=None):
    result = subprocess.run(list(map(str, args)), env=environ, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, timeout=90)
    if error is not None:
        assert result.returncode > 0, (args, result.returncode, result.stdout.decode())
        assert error.encode() in result.stdout, result.stdout.decode()
    else:
        assert result.returncode == 0, (args, result.returncode, result.stdout.decode())
    assert not list(scratch.iterdir()), 'named scratch files survived mkfs'
    return result


def block(i):
    # Reproducible unique chunks, full SHA-256 input rather than a sparse file.
    return hashlib.shake_256(str(i).encode()).digest(4096)


# Total logical input below 64 MiB, including hardlinks counted twice.
with (source / 'dense').open('wb') as f:
    for i in range(3072):
        f.write(block(i))
with (source / 'duplicate').open('wb') as f:
    for i in range(2560):
        f.write(block(i % 4))
with (source / 'sparse').open('wb') as f:
    f.write(block(0))
    f.seek(12 * 1024 * 1024 - 7)
    f.write(b'tail123')
with (source / 'holes').open('wb') as f:
    f.truncate(2 * 1024 * 1024)
(source / 'zeros').write_bytes(bytes(16384))
(source / 'empty').touch()
for n in (1, 4095, 4096, 4097, 8191):
    (source / ('tail-' + str(n))).write_bytes((block(8000) * 2)[:n])
os.link(source / 'dense', source / 'hardlink')
os.symlink('dense', source / 'symlink')
many = source / 'many'
many.mkdir()
for i in range(192):
    (many / f'{i:04}').write_bytes(block(i)[:1 + i * 41])
for root, dirs, files in os.walk(source):
    for name in dirs + files:
        os.utime(Path(root) / name, (1700000000, 1700000000), follow_symlinks=False)
os.utime(source, (1700000000, 1700000000))


def digest(path):
    h = hashlib.sha256()
    with path.open('rb') as f:
        for data in iter(lambda: f.read(65536), b''):
            h.update(data)
    return h.hexdigest()


def compare_tree(extracted, expected_tree=source):
    def entries(base):
        return sorted(str(p.relative_to(base)) for p in base.rglob('*'))
    assert entries(expected_tree) == entries(extracted)
    for path in expected_tree.rglob('*'):
        other = extracted / path.relative_to(expected_tree)
        assert stat.S_IFMT(path.lstat().st_mode) == stat.S_IFMT(other.lstat().st_mode)
        if path.is_symlink():
            assert path.readlink() == other.readlink()
        elif path.is_file():
            assert path.stat().st_size == other.stat().st_size, (str(path), path.stat().st_size, other.stat().st_size)
            assert digest(path) == digest(other), str(path)
    assert (extracted / 'dense').stat().st_ino == (extracted / 'hardlink').stat().st_ino


# Plain/default source compatibility and chunk-index/block-map formats. A tar
# source also exercises inode lifetimes without introducing a new read path.
archive = work / 'input.tar'
with tarfile.open(archive, 'w', format=tarfile.GNU_FORMAT) as tar:
    tar.add(source, arcname='.')
for label, args, inputs in (
        ('chunk', common, [source]),
        ('chunk-index', common + ['-Eforce-chunk-indexes'], [source]),
        ('plain', common[:-1], [source]),
        ('tar', common + ['--tar=f'], [archive])):
    reference = work / f'{label}-upstream.erofs'
    run([upstream, *args, reference, *inputs])
    run([upstream_fsck, reference])
    expected = digest(reference)
    expected_tree = source
    if label == 'chunk-index':
        # v1.9.1 fsck's forced-index extraction leaves fully sparse files at
        # size zero, even from pristine images. Compare its extraction exactly
        # with upstream here; normal chunk mode checks source data and lengths.
        expected_tree = work / 'upstream-extracted'
        run([upstream_fsck, '--extract=' + str(expected_tree), reference])
    for mode in (None, 'memory', 'disk'):
        output = work / f'{label}-{mode or "default"}.erofs'
        selected = dict(env)
        if mode is not None:
            selected['EROFS_INDEX_STORAGE'] = mode
        run([candidate, *args, output, *inputs], selected)
        assert digest(output) == expected, (label, mode, expected, digest(output))
        extracted = work / 'extracted'
        run([fsck, '--extract=' + str(extracted), output])
        compare_tree(extracted, expected_tree)
        shutil.rmtree(extracted)
        output.unlink()
    if expected_tree != source:
        shutil.rmtree(expected_tree)
    print(f'erofs-index-images: {label}: upstream/default/memory/disk bytes + fsck/extract PASS'
          + (' (extraction compared with upstream)' if label == 'chunk-index' else ''), flush=True)
    reference.unlink()
archive.unlink()

# Selection is validated even without --chunksize. Memory does not need TMPDIR.
output = work / 'selector.erofs'
for mode in ('', 'mmap', 'DISK', '1'):
    selected = dict(env, EROFS_INDEX_STORAGE=mode)
    run([candidate, output, source], selected,
        error='EROFS_INDEX_STORAGE must be memory or disk')
for value in (None, ''):
    selected = dict(env, EROFS_INDEX_STORAGE='disk')
    if value is None:
        selected.pop('TMPDIR', None)
    else:
        selected['TMPDIR'] = value
    run([candidate, *common, output, source], selected,
        error='EROFS_INDEX_STORAGE=disk requires TMPDIR')
selected = dict(env, EROFS_INDEX_STORAGE='disk', TMPDIR=str(work / 'missing'))
run([candidate, *common, output, source], selected, error='disk index directory')
# /dev/shm is only inspected/opened; the rejection must happen before creation.
assert Path('/dev/shm').is_dir(), 'Linux tmpfs fixture /dev/shm is required'
run([candidate, *common, output, source], dict(env, EROFS_INDEX_STORAGE='disk', TMPDIR='/dev/shm'),
    error='disk index TMPDIR must not use tmpfs or ramfs')
empty = work / 'empty-source'
empty.mkdir()
for mode in (None, 'memory'):
    selected = dict(env)
    selected.pop('TMPDIR', None)
    if mode:
        selected['EROFS_INDEX_STORAGE'] = mode
    run([candidate, *common, output, empty], selected)
    run([fsck, output])
print('erofs-index-images: selectors, missing TMPDIR, tmpfs refusal, scratch cleanup PASS', flush=True)
