#!/usr/bin/env python3
"""Install the exact sandbox-init from the selected sandboxer release."""

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import struct
import subprocess
import tarfile
import tempfile


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def _archive_name(member):
    return member.name[2:] if member.name.startswith("./") else member.name


def _member(bundle, name):
    matches = [member for member in bundle.getmembers()
               if _archive_name(member) == name]
    require(len(matches) == 1 and matches[0].isfile(),
            f"sandboxer archive must contain one regular {name}")
    return matches[0]


def _safe_members(bundle):
    members = {}
    for member in bundle.getmembers():
        name = _archive_name(member)
        if name == "." and member.isdir():
            continue
        require(name and not name.startswith("/") and "\\" not in name
                and not any(ord(char) < 32 or ord(char) == 127 for char in name)
                and all(part not in ("", ".", "..") for part in name.split("/")),
                "sandboxer archive has an unsafe path")
        require(name not in members, "sandboxer archive has duplicate paths")
        require(member.isdir() or member.isfile(), "sandboxer archive has an unsafe file type")
        require(not member.mode & 0o7022 and member.uid == member.gid == 0,
                "sandboxer archive has unsafe permissions or ownership")
        members[name] = member
    for name in members:
        require(all(str(parent) not in members or members[str(parent)].isdir()
                    for parent in PurePosixPath(name).parents), "sandboxer archive has a path conflict")
    return members


def _safe_output(path):
    require(not path.is_symlink() and all(not parent.is_symlink() for parent in path.parents),
            "sandbox-init output has a symlink path")


def _go_toolchain(value):
    match = re.fullmatch(
        r"(go1\.[0-9]+(?:(?:\.[0-9]+)|(?:beta|rc)[0-9]+))(?: X:[A-Za-z0-9_,.-]+)?",
        value)
    require(match is not None, "sandbox-init Go toolchain record is invalid")
    return match.group(1)


def _binary_go_toolchain(payload, directory):
    with tempfile.NamedTemporaryFile(dir=directory, delete=False) as stream:
        temporary = Path(stream.name)
        try:
            stream.write(payload)
            stream.flush()
            os.fchmod(stream.fileno(), 0o755)
        finally:
            stream.close()
    try:
        result = subprocess.run(
            ["go", "version", "-m", str(temporary)],
            check=False, capture_output=True, text=True)
        require(result.returncode == 0 and result.stdout,
                "cannot read sandbox-init Go build metadata")
        first = result.stdout.splitlines()[0]
        _, separator, version = first.partition(": ")
        require(separator, "sandbox-init Go build metadata is malformed")
        return _go_toolchain(version)
    finally:
        temporary.unlink(missing_ok=True)


def prepare(release, directory, version, source_sha, output, go_toolchain_output=None, arch="x86_64"):
    require(arch in ("x86_64", "aarch64"), "invalid sandbox-init target architecture")
    require(re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-preview\.[0-9]{8}(?:\.[1-9][0-9]*)?)?", version), "invalid sandboxer version")
    require(re.fullmatch(r"[0-9a-f]{40}", source_sha), "invalid sandboxer source SHA")
    require(release["tag_name"] == version, "sandboxer Release does not match the selected tag")
    require(release["draft"] is False and release["prerelease"] is ("-preview." in version),
            "sandboxer Release has the wrong publication state")
    archive_name = f"sandboxer-{version}-linux-{arch}.tar.gz"
    # Freeze native metadata from one Release response. Old checksum attachments
    # and body markers have no authority and need not be downloaded.
    assets = [asset for asset in release["assets"] if asset.get("name") != "SHA256SUMS"]
    names = {asset["name"] for asset in assets}
    native = f"sandboxer-{version}-linux-x86_64.tar.gz"
    arm = f"sandboxer-{version}-linux-aarch64.tar.gz"
    require(len(assets) == len(names) and names in ({native}, {native, arm})
            and archive_name in names, "sandboxer Release has an unexpected asset set")
    require(len({asset.get("id") for asset in assets}) == len(assets), "duplicate sandboxer asset ID")
    for asset in assets:
        require(type(asset.get("id")) is int and asset["id"] > 0
                and type(asset.get("size")) is int and asset["size"] > 0
                and isinstance(asset.get("digest"), str)
                and re.fullmatch(r"sha256:[0-9a-f]{64}", asset["digest"])
                and asset.get("state") == "uploaded", "sandboxer asset lacks valid native metadata")
        if asset["name"] != archive_name:
            continue
        path = directory / archive_name
        require(path.is_file() and not path.is_symlink() and path.stat().st_size == asset["size"],
                f"sandboxer asset size/state mismatch: {archive_name}")
        require(asset["digest"] == "sha256:" + digest(path),
                f"sandboxer GitHub asset digest mismatch: {archive_name}")
    archive = directory / archive_name
    _safe_output(output)
    if go_toolchain_output is not None:
        _safe_output(go_toolchain_output)
    with tarfile.open(archive, "r:gz") as bundle:
        safe = _safe_members(bundle)
        members = [safe["bin/sandbox-init"]] if "bin/sandbox-init" in safe else []
        require(len(members) == 1 and members[0].isfile() and members[0].mode & 0o111,
                "sandboxer archive must contain one regular executable bin/sandbox-init")
        payload = bundle.extractfile(members[0]).read()
        require(len(payload) >= 64 and payload[:7] == b"\x7fELF\x02\x01\x01"
                and struct.unpack_from("<H", payload, 18)[0] == {"x86_64": 62, "aarch64": 183}[arch],
                "selected sandbox-init has the wrong architecture")

        if go_toolchain_output is not None:
            # Legal notices follow the compiler recorded in the actual binary;
            # no historical source/build sidecar is a prerequisite.
            toolchain = _go_toolchain(_binary_go_toolchain(payload, directory))
            license_directory = f"share/licenses/sandboxer/go-toolchain/{toolchain}"

            prefix = license_directory + "/"
            notice_members = [member for member in bundle.getmembers()
                              if _archive_name(member).startswith(prefix)]
            require(notice_members, "sandbox-init Go toolchain notices are missing")
            files = []
            for member in notice_members:
                name = _archive_name(member)
                relative = name[len(prefix):]
                relative_path = PurePosixPath(relative)
                require(relative and not relative_path.is_absolute()
                        and ".." not in relative_path.parts
                        and relative_path.as_posix() == relative,
                        "sandbox-init Go toolchain notice has an unsafe path")
                require(member.isdir() or member.isreg(),
                        "sandbox-init Go toolchain notices must be regular files/directories")
                if member.isdir():
                    require(member.mode == 0o755,
                            "sandbox-init Go toolchain notice directory has an unsafe mode")
                    continue
                require(member.mode == 0o644,
                        "sandbox-init Go toolchain notice has an unsafe mode")
                data = bundle.extractfile(member).read()
                files.append((relative, data))
            require(any(relative == "LICENSE" for relative, _ in files),
                    "sandbox-init Go toolchain LICENSE is missing")

            go_toolchain_output.parent.mkdir(parents=True, exist_ok=True)
            temporary_root = Path(tempfile.mkdtemp(
                prefix=go_toolchain_output.name + ".", dir=go_toolchain_output.parent
            ))
            try:
                target = temporary_root / toolchain
                for relative, data in files:
                    destination = target.joinpath(*PurePosixPath(relative).parts)
                    require(destination.parent == target
                            or target in destination.parents,
                            "sandbox-init Go toolchain notice escapes its target")
                    destination.parent.mkdir(parents=True, exist_ok=True)
                    destination.write_bytes(data)
                    destination.chmod(0o644)
                if go_toolchain_output.exists():
                    shutil.rmtree(go_toolchain_output)
                temporary_root.replace(go_toolchain_output)
            finally:
                if temporary_root.exists():
                    shutil.rmtree(temporary_root)
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=output.parent, delete=False) as stream:
        temporary = Path(stream.name)
        try:
            stream.write(payload)
            stream.flush()
            os.fchmod(stream.fileno(), 0o755)
            temporary.replace(output)
        finally:
            temporary.unlink(missing_ok=True)
    print(f"selected sandboxer {version}@{source_sha}: sandbox-init sha256:{hashlib.sha256(payload).hexdigest()}")



def fetch(version, source_sha, output, go_toolchain_output=None, arch="x86_64"):
    """Fetch one native asset by its pinned GitHub ID, then verify before install."""
    require(re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-preview\.[0-9]{8}(?:\.[1-9][0-9]*)?)?", version),
            "invalid sandboxer version")
    require(arch in ("x86_64", "aarch64"), "invalid sandbox-init target architecture")
    repo = "repos/kuasar-sandbox/sandboxer"
    release = json.loads(subprocess.check_output(["gh", "api", f"{repo}/releases/tags/{version}"], text=True))
    name = f"sandboxer-{version}-linux-{arch}.tar.gz"
    matches = [asset for asset in release.get("assets", []) if asset.get("name") == name]
    require(len(matches) == 1 and type(matches[0].get("id")) is int and matches[0]["id"] > 0,
            "selected sandboxer asset is missing or ambiguous")
    with tempfile.TemporaryDirectory(prefix="selected-sandbox-init-") as directory:
        directory = Path(directory)
        with (directory / name).open("wb") as stream:
            subprocess.run(["gh", "api", "--allow-escape-sequences",
                            "-H", "Accept: application/octet-stream",
                            f"{repo}/releases/assets/{matches[0]['id']}"], stdout=stream, check=True)
        prepare(release, directory, version, source_sha, output, go_toolchain_output, arch)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release-json", type=Path)
    parser.add_argument("--directory", type=Path)
    parser.add_argument("--fetch", action="store_true", help="fetch the selected native GitHub asset before installing")
    parser.add_argument("--version", required=True)
    parser.add_argument("--source-sha", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--go-toolchain-output", type=Path)
    parser.add_argument("--arch", choices=("x86_64", "aarch64"), default="x86_64")
    args = parser.parse_args()
    if args.fetch and (args.release_json is not None or args.directory is not None):
        parser.error("--fetch cannot be combined with local release metadata or archives")
    if not args.fetch and (args.release_json is None or args.directory is None):
        parser.error("provide --fetch or both --release-json and --directory")
    try:
        if args.fetch:
            fetch(args.version, args.source_sha, args.output, args.go_toolchain_output, args.arch)
        else:
            prepare(json.loads(args.release_json.read_text()), args.directory,
                    args.version, args.source_sha, args.output, args.go_toolchain_output, args.arch)
    except (ValueError, KeyError, OSError, tarfile.TarError, subprocess.CalledProcessError) as error:
        parser.exit(1, f"release dependency: {error}\n")


if __name__ == "__main__":
    main()
