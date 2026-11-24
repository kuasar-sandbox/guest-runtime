"""Read fixed Runtime payloads using trusted host tools, never image executables."""
import hashlib
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import stat
import struct
import subprocess
import sys
import zipfile


PAYLOADS = {
    "init": "/sbin/init",
    "envd": "/opt/sandbox-runtime/bin/envd",
    "mkfs.erofs": "/opt/sandbox-runtime/bin/mkfs.erofs",
    "flatten-ctl": "/opt/sandbox-runtime/bin/flatten-ctl",
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def verify_bundle(image):
    size = image.stat().st_size
    require(0 < size and size % (2 << 20) == 0,
            "Runtime bundle must be nonempty and 2 MiB aligned")
    with image.open("rb") as data:
        data.seek(1024)
        require(data.read(4) == bytes.fromhex("e2e1f5e0"), "missing offset-zero EROFS")
        data.seek(-22, os.SEEK_END)
        end = struct.unpack("<4s4H2IH", data.read(22))
        require(end[:5] == (b"PK\x05\x06", 0, 0, 1, 1) and end[7] == 0,
                "Runtime bundle must contain one digest marker without a ZIP comment")
        require(46 <= end[5] <= 512 and 30 <= end[6] <= 512,
                "invalid Runtime digest directory bounds")
        prefix = size - 22 - end[5] - end[6]
        require(prefix > 1028, "invalid Runtime EROFS prefix bounds")
        with zipfile.ZipFile(data) as archive:
            entries = archive.infolist()
            require(len(entries) == 1, "unexpected Runtime digest entries")
            marker = entries[0]
            require(marker.header_offset == prefix and marker.compress_type == zipfile.ZIP_STORED
                    and marker.file_size == marker.compress_size == marker.CRC == 0
                    and marker.date_time == (1980, 1, 1, 0, 0, 0)
                    and stat.S_IMODE(marker.external_attr >> 16) == 0o444
                    and re.fullmatch(r"\.kuasar\.digest\.[0-9a-f]{64}", marker.filename),
                    "invalid Runtime digest marker")
            require(archive.read(marker) == b"", "nonempty Runtime digest marker")
        data.seek(0)
        digest = hashlib.sha256()
        remaining = prefix
        while remaining:
            block = data.read(min(1024 * 1024, remaining))
            require(block, "truncated Runtime EROFS prefix")
            digest.update(block)
            remaining -= len(block)
        require(marker.filename == ".kuasar.digest." + digest.hexdigest(),
                "Runtime EROFS prefix digest differs from its marker")


def run_reader(command, output):
    # Executables come from the trusted host. Image paths are data only.
    subprocess.run(command, stdout=output, stderr=subprocess.DEVNULL, check=True)


def read_payloads(image, fsck, dump, destination, *, without_mkfs=False, systemd_arch=None, systemd_lock=None):
    verify_bundle(image)
    destination.mkdir(mode=0o700)
    # --extract without a destination validates every file but creates no host
    # filesystem tree. In-image links and ownership are never applied to the host.
    with open(os.devnull, "wb") as output:
        run_reader([str(fsck), "--extract", str(image)], output)
    if without_mkfs:
        # Listing a known required directory distinguishes absence from a
        # failed lookup. Validate the listing before checking the retired name.
        listing = subprocess.run([str(dump), "--ls", "--path=/opt/sandbox-runtime/bin", str(image)],
                                 stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                 text=True, check=True, env=dict(os.environ, LC_ALL="C"))
        names = re.findall(r"^\s*[0-9]+\s+[0-9]+\s+([^\n]+)$", listing.stdout, re.M)
        require({".", "..", "envd", "flatten-ctl"} <= set(names),
                "cannot verify Runtime tool directory")
        require("mkfs.erofs" not in names, "Go Runtime must not contain obsolete mkfs.erofs")
    selected = dict(PAYLOADS)
    flat_extents = {}
    if systemd_arch:
        selected["executor"] = "/opt/sandbox-runtime/bin/sandbox-init"
        source = Path(__file__).with_name("release-systemd-payload.py")
        specification = importlib.util.spec_from_file_location("systemd_payload", source)
        validator = importlib.util.module_from_spec(specification)
        specification.loader.exec_module(validator)
    for label, path in selected.items():
        if without_mkfs and label == "mkfs.erofs":
            continue
        metadata = destination / (label + ".metadata")
        with metadata.open("xb") as output:
            os.chmod(metadata, 0o600)
            run_reader([str(dump), "--path=" + path, str(image)], output)
        text = metadata.read_text()
        sizes = re.findall(r"^Size: ([0-9]+)\s+On-disk size: [0-9]+\s+regular file$", text, re.M)
        require(len(sizes) == 1 and 0 < int(sizes[0]),
                "required Runtime payload is not a regular file: " + label)
        require(len(re.findall(r"^Uid: 0\s+Gid: 0\s+Access: 0755/rwxr-xr-x$", text, re.M)) == 1,
                "required Runtime payload must be root-owned mode 0755: " + label)
        if systemd_arch:
            nids = re.findall(r"^NID: ([0-9]+)\s+", text, re.M)
            require(len(nids) == 1 and validator.erofs_inode_mode(image, int(nids[0])) == stat.S_IFREG | 0o755,
                    "required Runtime tool has unexpected privilege bits: " + label)
            require(re.search(r"Xattr size: 0(?:\s|$)", text) is not None,
                    "required Runtime tool has unverified xattrs or capabilities: " + label)
        size = int(sizes[0])
        if systemd_arch:
            flat_extents[int(nids[0])] = validator.erofs_flat_extent(image, int(nids[0]), size)
        target = destination / label
        with target.open("xb") as output:
            os.chmod(target, 0o600)
            run_reader([str(dump), "--cat", "--path=" + path, str(image)], output)
        require(target.stat().st_size == size, "Runtime payload read size differs: " + label)
    if systemd_arch:
        init_info = (destination / "init.metadata").read_text()
        executor_info = (destination / "executor.metadata").read_text()
        init_nid = re.findall(r"^NID: ([0-9]+)\s+Links: ([0-9]+)\s+", init_info, re.M)
        executor_nid = re.findall(r"^NID: ([0-9]+)\s+Links: ([0-9]+)\s+", executor_info, re.M)
        require(len(init_nid) == len(executor_nid) == 1 and init_nid == executor_nid
                and int(init_nid[0][1]) >= 2,
                "Runtime executor entry must be a hardlink to /sbin/init")
        require((destination / "init").read_bytes() == (destination / "executor").read_bytes(),
                "Runtime executor entry differs from /sbin/init")
        tree = validator.ErofsTree(image, dump)
        lock = json.loads(Path(systemd_lock).read_text()) if systemd_lock else None
        validator.verify(tree, systemd_arch, lock)
        flat_extents.update(tree.flat_extents)
        validator.require_disjoint_extents(flat_extents)
        manifest = json.loads(tree.read(validator.MANIFEST))
        (destination / "systemd-manifest.json").write_text(json.dumps(manifest, sort_keys=True))
        # Copy only manifest-verified regular license bytes into private paths;
        # never materialize in-image links or apply image ownership on the host.
        for name, entry in manifest["files"].items():
            if name.startswith("share/licenses/") and entry["type"] == "file":
                target = destination / "systemd-licenses" / name.removeprefix("share/licenses/")
                target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                target.write_bytes(tree.read(name))
                target.chmod(0o600)


if __name__ == "__main__":
    try:
        parser = argparse.ArgumentParser(description=__doc__)
        parser.add_argument("--without-mkfs", action="store_true")
        parser.add_argument("--systemd-arch", choices=("x86_64", "aarch64"))
        parser.add_argument("--systemd-lock", type=Path)
        for name in ("image", "fsck", "dump", "destination"):
            parser.add_argument(name, type=Path)
        arguments = parser.parse_args()
        # The trusted caller selects the payload contract. Historical callers
        # still require mkfs; an image cannot opt itself out of this check.
        read_payloads(arguments.image, arguments.fsck, arguments.dump, arguments.destination,
                      without_mkfs=arguments.without_mkfs, systemd_arch=arguments.systemd_arch,
                      systemd_lock=arguments.systemd_lock)
    except (ValueError, OSError, subprocess.SubprocessError, zipfile.BadZipFile) as error:
        # Do not forward raw image-controlled native diagnostics to a CI log.
        message = str(error) if isinstance(error, ValueError) else type(error).__name__
        print("release: Runtime embedded payload verification failed: " + message, file=sys.stderr)
        sys.exit(1)
