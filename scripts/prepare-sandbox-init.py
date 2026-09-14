#!/usr/bin/env python3
"""Install the exact sandbox-init from the selected sandboxer release."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import tarfile
import tempfile


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def prepare(release, directory, version, source_sha, output):
    require(re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-preview\.[0-9]{8})?", version), "invalid sandboxer version")
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
                   if member.name in ("bin/sandbox-init", "./bin/sandbox-init")]
        require(len(members) == 1 and members[0].isfile() and members[0].mode & 0o111,
                "sandboxer archive must contain one regular executable bin/sandbox-init")
        payload = bundle.extractfile(members[0]).read()
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
    args = parser.parse_args()
    try:
        prepare(json.loads(args.release_json.read_text()), args.directory,
                args.version, args.source_sha, args.output)
    except (ValueError, KeyError, OSError, tarfile.TarError) as error:
        parser.exit(1, f"release dependency: {error}\n")


if __name__ == "__main__":
    main()
