#!/usr/bin/env python3
"""Install the exact sandbox-init from the selected sandboxer release."""

import argparse
import csv
import hashlib
import json
import io
import os
from pathlib import Path, PurePosixPath
import re
import shutil
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


def _verified_material(bundle, checksums, name):
    member = _member(bundle, name)
    payload = bundle.extractfile(member).read()
    require(checksums.get(name) == hashlib.sha256(payload).hexdigest(),
            f"sandboxer material checksum mismatch: {name}")
    return payload


def _go_toolchain(value):
    require(re.fullmatch(r"go1\.[0-9]+(?:(?:\.[0-9]+)|(?:beta|rc)[0-9]+)", value),
            "sandbox-init Go toolchain record is invalid")
    return value


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


def prepare(release, directory, version, source_sha, output, go_toolchain_output=None):
    require(re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-preview\.[0-9]{8}(?:\.[1-9][0-9]*)?)?", version), "invalid sandboxer version")
    require(re.fullmatch(r"[0-9a-f]{40}", source_sha), "invalid sandboxer source SHA")
    require(release["tag_name"] == version and release["target_commitish"] == source_sha,
            "sandboxer Release does not match the selected tag/source")
    require(release["draft"] is False and release["prerelease"] is ("-preview." in version),
            "sandboxer Release has the wrong publication state")
    archive_name = f"sandboxer-{version}-linux-x86_64.tar.gz"
    assets = release["assets"]
    require(len(assets) == 2 and {asset["name"] for asset in assets} == {archive_name, "SHA256SUMS"},
            "sandboxer Release has an unexpected asset set")
    for asset in assets:
        path = directory / asset["name"]
        require(asset["state"] == "uploaded" and path.stat().st_size == asset["size"],
                f"sandboxer asset size/state mismatch: {asset['name']}")
        require(asset["digest"] == "sha256:" + digest(path),
                f"sandboxer GitHub asset digest mismatch: {asset['name']}")
    archive = directory / archive_name
    require((directory / "SHA256SUMS").read_text().splitlines() == [f"{digest(archive)}  {archive_name}"],
            "sandboxer SHA256SUMS does not match its archive")
    with tarfile.open(archive, "r:gz") as bundle:
        members = [member for member in bundle.getmembers()
                   if _archive_name(member) == "bin/sandbox-init"]
        require(len(members) == 1 and members[0].isfile() and members[0].mode & 0o111,
                "sandboxer archive must contain one regular executable bin/sandbox-init")
        payload = bundle.extractfile(members[0]).read()

        if go_toolchain_output is not None:
            material_member = _member(bundle, "share/sources/sandboxer/MATERIALS.sha256")
            checksums = {}
            for raw in bundle.extractfile(material_member).read().decode().splitlines():
                digest_value, separator, name = raw.partition("  ")
                require(separator and re.fullmatch(r"[0-9a-f]{64}", digest_value),
                        "sandboxer MATERIALS.sha256 is malformed")
                name = name.removeprefix("./")
                require(name not in checksums,
                        "sandboxer MATERIALS.sha256 has duplicate paths")
                checksums[name] = digest_value

            info_name = "share/sources/sandboxer/GO-BUILD-INFO.tsv"
            sources_name = "share/sources/sandboxer/SOURCES.tsv"
            info = _verified_material(bundle, checksums, info_name).decode()
            sources = _verified_material(bundle, checksums, sources_name).decode()
            rows = list(csv.DictReader(io.StringIO(info), delimiter="\t"))
            selected = [row["version_or_value"] for row in rows
                        if row["payload"] == "bin/sandbox-init"
                        and row["record"] == "toolchain" and row["name"] == "go"]
            require(len(selected) == 1,
                    "sandbox-init must declare exactly one Go toolchain")
            toolchain = _go_toolchain(selected[0])
            require(_binary_go_toolchain(payload, directory) == toolchain,
                    "sandbox-init binary Go toolchain does not match release metadata")
            license_directory = f"share/licenses/sandboxer/go-toolchain/{toolchain}"
            source_rows = list(csv.DictReader(io.StringIO(sources), delimiter="\t"))
            selected_sources = [
                row for row in source_rows
                if row["payload"] == "bin/sandbox-init"
                and row["name"] == "Go toolchain"
            ]
            require(len(selected_sources) == 1,
                    "sandbox-init must declare exactly one Go toolchain source")
            source = selected_sources[0]
            require(source["version"] == toolchain
                    and source["source"] == f"https://go.dev/dl/#{toolchain}"
                    and source["integrity"] == "-"
                    and source["license_directory"] == license_directory,
                    "sandbox-init Go toolchain source record is not canonical")

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
                data = _verified_material(bundle, checksums, name)
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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release-json", type=Path, required=True)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--source-sha", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--go-toolchain-output", type=Path)
    args = parser.parse_args()
    try:
        prepare(json.loads(args.release_json.read_text()), args.directory,
                args.version, args.source_sha, args.output, args.go_toolchain_output)
    except (ValueError, KeyError, OSError, tarfile.TarError) as error:
        parser.exit(1, f"release dependency: {error}\n")


if __name__ == "__main__":
    main()

[executed on device: VM-16-4-ubuntu (ece80c39-2a6a-48a6-8bca-2dd73fc629dd)]