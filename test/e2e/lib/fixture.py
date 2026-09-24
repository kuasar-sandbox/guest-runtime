#!/usr/bin/env python3
"""Build a tiny, deterministic two-layer docker-archive without a base image."""

import argparse
import hashlib
import io
import json
import tarfile


def entry(name, data=b"", *, mode=0o644, uid=0, gid=0, kind=tarfile.REGTYPE, link=""):
    header = tarfile.TarInfo(name)
    header.mode, header.uid, header.gid = mode, uid, gid
    header.type, header.linkname = kind, link
    header.mtime = 0
    header.size = len(data)
    return header, data


def directory(name, **kwargs):
    return entry(name, mode=0o755, kind=tarfile.DIRTYPE, **kwargs)


def tar_bytes(entries):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w", format=tarfile.USTAR_FORMAT) as archive:
        for header, data in entries:
            archive.addfile(header, io.BytesIO(data))
    return output.getvalue()


def json_bytes(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode() + b"\n"


def build(output, tag, architecture):
    architecture = {"x86_64": "amd64", "aarch64": "arm64"}.get(architecture, architecture)
    if architecture not in ("amd64", "arm64"):
        raise ValueError(f"unsupported fixture architecture: {architecture}")
    owner = {"uid": 10001, "gid": 10002}
    layers = [
        tar_bytes([
            directory("usr"), directory("usr/bin"),
            entry("usr/bin/fixture", b"#!/bin/sh\nprintf 'guest-runtime fixture\\n'\n", mode=0o755),
            directory("home"), directory("home/e2e", **owner),
            entry("home/e2e/message", b"lower layer\n", **owner),
            entry("home/e2e/remove-me", b"must be whiteouted\n", **owner),
            directory("home/e2e/opaque", **owner),
            entry("home/e2e/opaque/old", b"must be hidden\n", **owner),
            directory("home/e2e/opaque/nested", **owner),
            entry("home/e2e/opaque/nested/old", b"also hidden\n", **owner),
        ]),
        tar_bytes([
            entry("home/e2e/.wh.remove-me"),
            entry("home/e2e/opaque/.wh..wh..opq"),
            entry("home/e2e/message", b"upper layer\n", **owner),
            entry("home/e2e/opaque/new", b"visible upper file\n", **owner),
            entry("home/e2e/current", mode=0o777, kind=tarfile.SYMTYPE, link="message", **owner),
        ]),
    ]
    config = json_bytes({
        "created": "1970-01-01T00:00:00Z", "architecture": architecture, "os": "linux",
        "config": {"User": "10001:10002", "WorkingDir": "/home/e2e",
                   "Entrypoint": ["/usr/bin/fixture"], "Env": ["PATH=/usr/bin"]},
        "rootfs": {"type": "layers", "diff_ids": [
            "sha256:" + hashlib.sha256(layer).hexdigest() for layer in layers]},
        "history": [{"created": "1970-01-01T00:00:00Z", "created_by": description}
                    for description in ("fixture base", "fixture whiteouts and replacement")],
    })
    config_name = hashlib.sha256(config).hexdigest() + ".json"
    layer_names = [f"layer-{index}/layer.tar" for index in range(len(layers))]
    manifest = [{"Config": config_name, "RepoTags": [tag], "Layers": layer_names}]
    archive = tar_bytes([
        entry("manifest.json", json_bytes(manifest)), entry(config_name, config),
        *(entry(name, data) for name, data in zip(layer_names, layers)),
    ])
    with open(output, "wb") as destination:
        destination.write(archive)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output")
    parser.add_argument("--tag", required=True)
    parser.add_argument("--architecture", required=True)
    arguments = parser.parse_args()
    build(arguments.output, arguments.tag, arguments.architecture)
