#!/usr/bin/env python3
"""Exact upstream/candidate images and extracted data with small disk fixtures."""
import hashlib
import os
from pathlib import Path
import stat
import subprocess
import sys

baseline, candidate, work = map(Path, sys.argv[1:])
source = work / "source"
(source / "dir").mkdir(parents=True)
data = b"".join(hashlib.sha256(str(i).encode()).digest() * 128 for i in range(256))
(source / "dense").write_bytes(data)
(source / "duplicate").write_bytes(data)
with (source / "sparse").open("wb") as out:
    out.write(b"head" * 1024)
    out.seek(16 * 1024 * 1024 - 4096)
    out.write(b"tail" * 1024)
for size in (0, 1, 17, 4095, 4096, 4097, 8193):
    (source / "dir" / str(size)).write_bytes((b"tail-data-" * 1024)[:size])
os.link(source / "dense", source / "hardlink")
os.symlink("../dense", source / "dir/link")
os.chmod(source / "dir/17", 0o751)
flags = ["-Ededupe", "--chunksize=4096", "-T0", "-b4096", "-x-1", "-U", "00000000-0000-0000-0000-000000000000", "--quiet"]


def inventory(root):
    result = {}
    for path in sorted(root.rglob("*")):
        info = path.lstat()
        value = [stat.S_IFMT(info.st_mode), stat.S_IMODE(info.st_mode)]
        if path.is_symlink():
            value += [os.readlink(path)]
        elif path.is_file():
            with path.open("rb") as stream:
                value += [info.st_size, hashlib.file_digest(stream, "sha256").hexdigest()]
        result[str(path.relative_to(root))] = value
    assert (root / "dense").stat().st_ino == (root / "hardlink").stat().st_ino
    return result


for label, tools in (("baseline", baseline), ("candidate", candidate)):
    image = work / (label + ".erofs")
    subprocess.run([str(tools / "mkfs.erofs"), *flags, str(image), str(source)], check=True)
    extracted = work / label
    subprocess.run([str(tools / "fsck.erofs"), "--extract=" + str(extracted), str(image)], check=True)
    assert inventory(extracted) == inventory(source), label + " extraction mismatch"
assert (work / "baseline.erofs").read_bytes() == (work / "candidate.erofs").read_bytes()
print("test-erofs-images: exact images and extraction PASS", hashlib.sha256((work / "candidate.erofs").read_bytes()).hexdigest())
