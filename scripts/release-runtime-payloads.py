"""Read fixed Runtime payloads using trusted host tools, never image executables."""
import hashlib
import os
from pathlib import Path
import re
import resource
import stat
import struct
import subprocess
import sys
import zipfile


PAYLOADS = {
    "init": "/sbin/init",
    "envd": "/opt/sandbox-runtime/bin/envd",
    "mkfs.erofs": "/opt/sandbox-runtime/bin/mkfs.erofs",
}
MAX_IMAGE = 1024 * 1024 * 1024
MAX_PAYLOAD = 256 * 1024 * 1024


def require(condition, message):
    if not condition:
        raise ValueError(message)


def verify_bundle(image):
    size = image.stat().st_size
    require(0 < size <= MAX_IMAGE and size % (2 << 20) == 0,
            "Runtime bundle must be bounded and 2 MiB aligned")
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


def run_reader(command, output, limit, timeout):
    # All command executables are supplied by the trusted validator's fresh
    # checksum-pinned native build. Image paths are data arguments only.
    def restrict_output():
        resource.setrlimit(resource.RLIMIT_FSIZE, (limit, limit))

    subprocess.run(command, stdout=output, stderr=subprocess.DEVNULL,
                   check=True, timeout=timeout, preexec_fn=restrict_output)


def read_payloads(image, fsck, dump, destination):
    verify_bundle(image)
    destination.mkdir(mode=0o700)
    # --extract without a destination validates every file but creates no host
    # filesystem tree. In-image links and ownership are never applied to the host.
    with open(os.devnull, "wb") as output:
        run_reader([str(fsck), "--extract", str(image)], output, 1024 * 1024, 120)
    for label, path in PAYLOADS.items():
        metadata = destination / (label + ".metadata")
        with metadata.open("xb") as output:
            os.chmod(metadata, 0o600)
            run_reader([str(dump), "--path=" + path, str(image)], output, 16384, 15)
        text = metadata.read_text()
        sizes = re.findall(r"^Size: ([0-9]+)\s+On-disk size: [0-9]+\s+regular file$", text, re.M)
        require(len(sizes) == 1 and 0 < int(sizes[0]) <= MAX_PAYLOAD,
                "required Runtime payload is not a bounded regular file: " + label)
        require(len(re.findall(r"^Uid: 0\s+Gid: 0\s+Access: 0755/rwxr-xr-x$", text, re.M)) == 1,
                "required Runtime payload must be root-owned mode 0755: " + label)
        size = int(sizes[0])
        target = destination / label
        with target.open("xb") as output:
            os.chmod(target, 0o600)
            run_reader([str(dump), "--cat", "--path=" + path, str(image)], output, size, 60)
        require(target.stat().st_size == size, "Runtime payload read size differs: " + label)


if __name__ == "__main__":
    try:
        require(len(sys.argv) == 5, "expected bundle, trusted fsck, trusted dump and private output")
        read_payloads(*(Path(argument) for argument in sys.argv[1:]))
    except (ValueError, OSError, subprocess.SubprocessError, zipfile.BadZipFile) as error:
        # Do not forward raw image-controlled native diagnostics to a CI log.
        message = str(error) if isinstance(error, ValueError) else type(error).__name__
        print("release: Runtime embedded payload verification failed: " + message, file=sys.stderr)
        sys.exit(1)
