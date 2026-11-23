#!/usr/bin/env python3
"""Verify the complete systemd prefix without executing any target program.

The same contract is used by native assembly and trusted release validation.
The manifest binds every regular file, symlink, mode and ELF dependency. It is
installation metadata inside the Runtime, not an opt-out chosen by the image.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import posixpath
import re
import stat
import struct
import subprocess
import sys

PREFIX = "/run/sandbox-runtime/systemd"
MANIFEST = "share/sandbox-runtime/systemd-manifest.json"
HELPER = "lib/dbus-1.0/dbus-daemon-launch-helper"
LOADERS = {"x86_64": "ld-linux-x86-64.so.2", "aarch64": "ld-linux-aarch64.so.1"}
MACHINES = {"x86_64": 62, "aarch64": 183}
PINNED_SOURCE_IDS = {
    "systemd": ("257.13", "5adcf82e930c1554c964d07d2827095dafede19ebed0e4fd3dd028b099878253"),
    "dbus": ("1.14.10", "ba1f21d2bd9d339da2d4aa8780c09df32fea87998b73da24f49ab9df1e36a50f"),
}
PINNED_SYSTEMD_PATCHES = [{
    "file": "systemd-patches/0001-private-configuration-search-path.patch",
    "sha256": "e91ba8aaedc24b8fabe9a53c26d9d5f67ffe4e7461c16205800c8511cb1b1203",
}]
REQUIRED = (
    "lib/systemd/systemd", "lib/systemd/systemd-executor", "lib/systemd/systemd-shutdown",
    "lib/systemd/systemd-journald", "bin/systemctl", "bin/journalctl", "bin/systemd-sysusers",
    "bin/systemd-tmpfiles", "bin/dbus-daemon", "bin/busybox", HELPER,
    "lib/systemd/system/dbus.service", "lib/systemd/system/dbus.socket",
    "lib/systemd/system/basic.target", "lib/systemd/system/multi-user.target",
    "lib/systemd/system/shutdown.target", "lib/systemd/system/systemd-journald.service",
    "lib/systemd/system/systemd-sysusers.service", "lib/systemd/system/systemd-tmpfiles-setup.service",
    "share/dbus-1/system.conf", "lib/sysusers.d/dbus.conf",
    "lib/systemd/journald.conf.d/50-sandbox-runtime.conf",
    "lib/systemd/system.conf.d/50-sandbox-runtime.conf",
    "lib/systemd/system-preset/80-sandbox-runtime.preset",
)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def safe_path(path):
    require(isinstance(path, str) and 0 < len(path) < 4096
            and re.fullmatch(r"[A-Za-z0-9._+@:/~=\\-]+", path) is not None
            and not path.startswith("/") and path == posixpath.normpath(path)
            and all(part not in (".", "..") for part in path.split("/")),
            "unsafe systemd manifest path: " + repr(path))
    return path


def sha(data):
    return hashlib.sha256(data).hexdigest()


def erofs_inode(image, nid):
    # dump.erofs <= 1.9.1 deliberately prints i_mode & 0777. Read the original
    # mode from the already-fsck'ed inode to retain setuid/setgid/sticky bits.
    # Both compact and extended EROFS inodes store i_mode at byte four.
    with image.open("rb") as stream:
        stream.seek(1024)
        superblock = stream.read(128)
        require(len(superblock) == 128 and superblock[:4] == bytes.fromhex("e2e1f5e0"),
                "invalid systemd EROFS superblock")
        bits = superblock[12]
        incompat = struct.unpack_from("<I", superblock, 80)[0]
        require(9 <= bits <= 16 and not incompat & 0x100,
                "systemd permission verifier does not support compressed metadata")
        metadata_block = struct.unpack_from("<I", superblock, 40)[0]
        offset = (metadata_block << bits) + nid * 32
        require(0 <= nid and offset + 32 <= image.stat().st_size, "systemd EROFS inode exceeds image")
        stream.seek(offset)
        inode = stream.read(32)
    require(len(inode) == 32, "truncated systemd EROFS inode")
    return inode, bits


def erofs_inode_mode(image, nid):
    return struct.unpack_from("<H", erofs_inode(image, nid)[0], 4)[0]


def erofs_flat_extent(image, nid, expected_size):
    """Require the flat, non-inline, uncompressed layout used for Runtime DAX.

    EROFS chunking deduplicates by default. Linux 6.1 DAX associates a physical
    page with one inode mapping, so independent regular files cannot share data
    blocks. Real hardlinks intentionally share the same inode and are allowed.
    """
    inode, bits = erofs_inode(image, nid)
    form = struct.unpack_from("<H", inode)[0]
    require(bits == 12 and form in (0, 1),
            "Runtime DAX requires flat non-inline regular-file data")
    size = struct.unpack_from("<Q" if form & 1 else "<I", inode, 8)[0]
    require(size == expected_size, "Runtime EROFS inode size disagrees with trusted reader")
    start = struct.unpack_from("<I", inode, 16)[0] << bits
    end = start + ((size + (1 << bits) - 1) >> bits << bits)
    require(not size or 0 < start < end <= image.stat().st_size,
            "Runtime EROFS data extent exceeds image")
    return start, end


def require_disjoint_extents(extents):
    previous_end = 0
    for start, end in sorted(set(extents.values())):
        if start == end:
            continue
        require(start >= previous_end, "Runtime DAX data is shared between independent inodes")
        previous_end = end
    # Equal extents must also belong to one inode; deduplicating the sequence
    # above is useful for zero-length files, but cannot hide an exact alias.
    nonempty = [value for value in extents.values() if value[0] != value[1]]
    require(len(nonempty) == len(set(nonempty)), "Runtime DAX data is shared between independent inodes")


def elf_metadata(data, arch):
    """Read ELF64 little-endian PT_INTERP and dynamic strings with bounded offsets."""
    require(len(data) >= 64 and data[:6] == b"\x7fELF\x02\x01", "systemd payload must use ELF64 little-endian")
    require(struct.unpack_from("<H", data, 18)[0] == MACHINES[arch], "systemd ELF target differs from release architecture")
    phoff = struct.unpack_from("<Q", data, 32)[0]
    phentsize, phnum = struct.unpack_from("<HH", data, 54)
    require(phentsize == 56 and phnum <= 4096 and phoff + phnum * 56 <= len(data), "invalid systemd ELF program headers")
    loads, dynamic, interpreter = [], None, None
    for index in range(phnum):
        typ, _, offset, address, _, filesz, _, _ = struct.unpack_from("<II6Q", data, phoff + index * 56)
        require(offset + filesz <= len(data), "systemd ELF segment exceeds file")
        if typ == 1:
            loads.append((address, offset, filesz))
        elif typ == 2:
            dynamic = (offset, filesz)
        elif typ == 3:
            raw = data[offset:offset + filesz]
            require(raw.endswith(b"\0") and b"\0" not in raw[:-1], "invalid systemd ELF interpreter")
            interpreter = raw[:-1].decode("ascii")
    if dynamic is None:
        return {"interpreter": interpreter, "needed": [], "rpath": None, "dynamic": False}
    offset, size = dynamic
    require(size % 16 == 0 and size <= 16 * 65536, "invalid systemd ELF dynamic table")
    entries = []
    for position in range(offset, offset + size, 16):
        tag, value = struct.unpack_from("<qQ", data, position)
        if tag == 0:
            break
        entries.append((tag, value))
    tables = [value for tag, value in entries if tag == 5]
    lengths = [value for tag, value in entries if tag == 10]
    require(len(tables) == len(lengths) == 1, "missing systemd ELF string table")
    locations = [fileoff + tables[0] - address for address, fileoff, filesz in loads
                 if address <= tables[0] and tables[0] + lengths[0] <= address + filesz]
    require(len(locations) == 1, "systemd ELF string table is not file backed")
    strings = data[locations[0]:locations[0] + lengths[0]]

    def string(index):
        require(index < len(strings), "systemd ELF string offset exceeds table")
        end = strings.find(b"\0", index)
        require(end != -1, "unterminated systemd ELF dynamic string")
        return strings[index:end].decode("ascii")

    paths = [string(value) for tag, value in entries if tag in (15, 29)]
    require(len(paths) <= 1, "multiple systemd ELF search paths")
    return {"interpreter": interpreter, "needed": [string(v) for t, v in entries if t == 1],
            "rpath": paths[0] if paths else None, "dynamic": True}


class DirectoryTree:
    def __init__(self, root):
        self.root = root
        require(root.is_dir() and not root.is_symlink(), "systemd payload must be a directory")

    def entries(self):
        return {str(p.relative_to(self.root)) for p in self.root.rglob("*")}

    def metadata(self, name):
        path = self.root / safe_path(name)
        info = path.lstat()
        if stat.S_ISLNK(info.st_mode):
            return {"type": "symlink", "target": os.readlink(path)}
        if stat.S_ISDIR(info.st_mode):
            return {"type": "directory", "mode": stat.S_IMODE(info.st_mode)}
        require(stat.S_ISREG(info.st_mode), "unexpected systemd payload file type: " + name)
        return {"type": "file", "mode": stat.S_IMODE(info.st_mode), "size": info.st_size}

    def read(self, name):
        path = self.root / safe_path(name)
        require(path.is_file() and not path.is_symlink(), "systemd payload reader requires a regular file")
        return path.read_bytes()


class ErofsTree:
    """Read only validated fixed paths through a trusted host dump.erofs.

    No image executable runs, and no in-image symlink, ownership or mode is
    applied to the host filesystem. The caller has already fsck'ed the image.
    """
    ROOT = "/usr/lib/sandbox-runtime/systemd"

    def __init__(self, image, dump):
        self.image, self.dump = image, dump
        self.cache = {}
        self.flat_extents = {}

    def command(self, *options):
        return subprocess.check_output([str(self.dump), *options, str(self.image)],
                                       stderr=subprocess.DEVNULL, env=dict(os.environ, LC_ALL="C"))

    def path(self, name):
        return self.ROOT + ("/" + safe_path(name) if name else "")

    def metadata(self, name):
        if name in self.cache:
            return self.cache[name]
        text = self.command("--path=" + self.path(name)).decode("utf-8")
        sizes = re.findall(r"^Size: ([0-9]+)\s+On-disk size: [0-9]+\s+(regular file|directory|symlink file)$", text, re.M)
        modes = re.findall(r"^Uid: 0\s+Gid: 0\s+Access: ([0-7]{4})/[^\n]+$", text, re.M)
        nids = re.findall(r"^NID: ([0-9]+)\s+", text, re.M)
        require(len(sizes) == len(modes) == len(nids) == 1, "systemd EROFS entry has unsafe type or ownership: " + name)
        require(re.search(r"Xattr size: 0(?:\s|$)", text) is not None,
                "systemd EROFS entry must not carry unverified xattrs or capabilities")
        size, kind = sizes[0]
        inode_mode = erofs_inode_mode(self.image, int(nids[0]))
        mode = stat.S_IMODE(inode_mode)
        require(mode & 0o777 == int(modes[0], 8), "systemd EROFS mode disagrees with trusted reader")
        require(stat.S_IFMT(inode_mode) == {"directory": stat.S_IFDIR, "symlink file": stat.S_IFLNK,
                                          "regular file": stat.S_IFREG}[kind],
                "systemd EROFS type disagrees with trusted reader")
        if kind == "directory":
            value = {"type": "directory", "mode": mode}
        elif kind == "symlink file":
            require(int(size) < 4096, "systemd EROFS symlink exceeds limit")
            target = self.command("--cat", "--path=" + self.path(name)).decode("utf-8")
            require(len(target.encode()) == int(size), "systemd EROFS symlink size differs")
            value = {"type": "symlink", "target": target}
        else:
            value = {"type": "file", "mode": mode, "size": int(size)}
            nid = int(nids[0])
            self.flat_extents[nid] = erofs_flat_extent(self.image, nid, int(size))
        self.cache[name] = value
        return value

    def entries(self):
        require(self.metadata("") == {"type": "directory", "mode": 0o755}, "invalid systemd EROFS root")
        seen, queue = set(), [""]
        while queue:
            directory = queue.pop()
            listing = self.command("--ls", "--path=" + self.path(directory)).decode("utf-8")
            rows = re.findall(r"^\s*[0-9]+\s+([0-9]+)\s+([^\n]+)$", listing, re.M)
            require({".", ".."} <= {name for _, name in rows}, "invalid systemd EROFS directory listing")
            for kind, child in rows:
                if child in (".", ".."):
                    continue
                require("/" not in child, "invalid systemd EROFS directory entry")
                name = safe_path(posixpath.join(directory, child))
                require(name not in seen, "duplicate systemd EROFS entry")
                seen.add(name)
                require(len(seen) <= 10001, "systemd EROFS inventory exceeds limit")
                if kind == "2":
                    queue.append(name)
        return seen

    def read(self, name):
        metadata = self.metadata(name)
        require(metadata["type"] == "file" and metadata["size"] <= 256 * 1024 * 1024,
                "invalid systemd EROFS regular file")
        data = self.command("--cat", "--path=" + self.path(name))
        require(len(data) == metadata["size"], "systemd EROFS file size differs")
        return data


def verify(tree, arch, lock=None, inputs=None):
    manifest_data = tree.read(MANIFEST)
    require(len(manifest_data) <= 4 * 1024 * 1024, "systemd manifest exceeds limit")
    manifest = json.loads(manifest_data)
    require(manifest.get("schema") == 1 and manifest.get("prefix") == PREFIX,
            "unsupported systemd payload contract")
    require(manifest.get("architecture") == arch, "systemd manifest architecture differs")
    require(manifest.get("setuid_allowlist") == [HELPER], "systemd setuid whitelist differs")
    for name, (version, digest) in PINNED_SOURCE_IDS.items():
        source = manifest.get("sources", {}).get(name, {})
        require(source.get("version") == version and source.get("sha256") == digest,
                "systemd payload uses an unqualified upstream source")
    require(manifest["sources"]["systemd"].get("commit") == "70b5d110be7702afc4dbce012f60d49506d513da",
            "systemd upstream commit differs")
    require(manifest["sources"]["systemd"].get("patches") == PINNED_SYSTEMD_PATCHES,
            "systemd configuration-search patch identity differs")
    if lock is not None:
        require(manifest.get("sources") == {k: lock[k] for k in ("systemd", "dbus")},
                "systemd source identities differ from the selected recipe")
        require(manifest.get("dlopen") == ["lib/" + name for name in lock["dlopen"]],
                "systemd dlopen/NSS closure differs from the selected recipe")
    if inputs is not None:
        require(manifest.get("inputs") == inputs, "systemd build inputs changed")
    entries = manifest.get("files")
    require(isinstance(entries, dict) and 0 < len(entries) <= 10000 and MANIFEST not in entries,
            "invalid systemd file inventory")
    for name in entries:
        safe_path(name)
    require(tree.entries() == set(entries) | {MANIFEST}, "systemd payload inventory differs from manifest")
    require(tree.metadata(MANIFEST) == {"type": "file", "mode": 0o644, "size": len(manifest_data)},
            "systemd manifest mode/type differs")

    def resolve(name):
        visited = set()
        while name in entries and entries[name]["type"] == "symlink":
            require(name not in visited, "systemd payload contains a symlink cycle")
            visited.add(name)
            target = entries[name]["target"]
            require(isinstance(target, str) and len(target) < 4096, "invalid systemd symlink target")
            if name == "lib/environment.d/99-environment.conf" and target == "/etc/environment":
                return None  # Audited administrator configuration, never ELF code.
            if target == "/dev/null":
                return None
            if target.startswith(PREFIX + "/"):
                name = target[len(PREFIX) + 1:]
            else:
                require(not target.startswith("/"), "systemd symlink escapes private prefix")
                name = posixpath.normpath(posixpath.join(posixpath.dirname(name), target))
            safe_path(name)
        require(name in entries, "dangling systemd payload link: " + name)
        return name

    loader = "lib/" + LOADERS[arch]
    require(manifest.get("loader", {}).get("path") == loader, "systemd loader identity differs")
    regular = {}
    for name, expected in entries.items():
        metadata = tree.metadata(name)
        require(metadata == {k: v for k, v in expected.items() if k != "sha256"},
                "systemd file metadata differs: " + name)
        if expected["type"] == "symlink":
            resolve(name)
        elif expected["type"] == "directory":
            require(expected["mode"] == 0o755, "systemd directory has unsafe permissions")
        elif expected["type"] == "file":
            modes = (0o4755,) if name == HELPER else (0o644, 0o755)
            require(expected["mode"] in modes, "systemd executable privilege/mode not allowed: " + name)
            data = tree.read(name)
            require(sha(data) == expected.get("sha256"), "systemd file digest differs: " + name)
            if data.startswith(b"\x7fELF"):
                regular[name] = elf_metadata(data, arch)
            if name == loader:
                require(sha(data) == manifest["loader"]["original_sha256"], "systemd ELF loader was modified")
        else:
            raise ValueError("unknown systemd manifest file type")

    for name in (*REQUIRED, loader, *manifest.get("dlopen", [])):
        require(name in entries and entries[name]["type"] == "file", "missing required systemd component: " + name)
    for name in ("lib/systemd/system-generators", "lib/systemd/system-environment-generators", "share/licenses/systemd", "share/licenses/dbus"):
        require(name in entries and entries[name]["type"] == "directory", "missing required systemd directory: " + name)
    for name, metadata in regular.items():
        if metadata["interpreter"]:
            require(metadata["interpreter"] == PREFIX + "/" + loader,
                    "systemd interpreter escapes private prefix: " + name)
        if metadata["dynamic"] and name != loader:
            require(metadata["rpath"] == PREFIX + "/lib:" + PREFIX + "/lib/systemd",
                    "systemd DSO search path escapes private prefix: " + name)
        for needed in metadata["needed"]:
            require("/" not in needed, "systemd DT_NEEDED must use a private SONAME")
            candidates = ["lib/" + needed, "lib/systemd/" + needed]
            matches = [resolve(p) for p in candidates if p in entries]
            require(any(p in regular for p in matches), "missing systemd ELF dependency: " + needed)
    require(loader in regular and "bin/busybox" in regular and not regular["bin/busybox"]["dynamic"],
            "systemd mount helper must be a static target ELF")
    require(isinstance(manifest.get("dependencies"), dict) and manifest["dependencies"],
            "systemd dependency identities are missing")
    for record in manifest["dependencies"].values():
        require(all(isinstance(record.get(key), str) and record[key] for key in
                    ("path", "original_sha256", "package", "version", "source_package", "source_version", "architecture", "license")),
                "systemd dependency source identity is incomplete")
        require(record["path"] in regular and record["license"] in entries,
                "systemd dependency or its license is missing")
    require(tree.read("lib/systemd/system/dbus.service").find((PREFIX + "/bin/dbus-daemon").encode()) >= 0,
            "D-Bus unit does not reference the private-prefix daemon")
    return {"architecture": arch, "files": len(entries), "elfs": len(regular),
            "dependencies": len(manifest["dependencies"]), "manifest_sha256": sha(manifest_data)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("verify", "erofs"))
    parser.add_argument("directory", type=Path)
    parser.add_argument("--arch", choices=LOADERS, required=True)
    parser.add_argument("--lock", type=Path)
    parser.add_argument("--inputs", type=Path)
    parser.add_argument("--dump", type=Path)
    args = parser.parse_args()
    lock = json.loads(args.lock.read_text()) if args.lock else None
    inputs = json.loads(args.inputs.read_text()) if args.inputs else None
    if args.command == "erofs":
        require(args.dump is not None, "EROFS verification requires trusted --dump")
        tree = ErofsTree(args.directory, args.dump)
    else:
        tree = DirectoryTree(args.directory)
    print(json.dumps(verify(tree, args.arch, lock, inputs), sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError, struct.error, subprocess.SubprocessError) as error:
        raise SystemExit("systemd payload validation: " + str(error))
