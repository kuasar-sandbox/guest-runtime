#!/usr/bin/env python3
"""Validate real CLI/OCI responses; invalid JSON is never a cache miss."""

import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import re
import sys

HEX = r"[0-9a-f]{64}"
ARTIFACT_TYPE = "application/vnd.kuasar.flatten-manifest.v1"
ANNOTATION = "vnd.kuasar.flatten-manifest."


def require(condition, message):
    if not condition:
        raise ValueError(message)


def object_pairs(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, f"duplicate JSON key: {key}")
        result[key] = value
    return result


def invalid_constant(value):
    raise ValueError(f"invalid JSON constant: {value}")


def load(path):
    value = json.loads(Path(path).read_text(), object_pairs_hook=object_pairs,
                       parse_constant=invalid_constant)
    require(isinstance(value, dict), "expected a JSON object")
    return value


def hex_id(value):
    return isinstance(value, str) and re.fullmatch(HEX, value) is not None


def lookup(value, subject, expected="any", manifest_id=None):
    require(value.get("supported") is True, "lookup must report supported=true")
    require(type(value.get("hit")) is bool, "lookup hit must be a boolean")
    require(value.get("subject") == subject, "lookup resolved an unexpected subject")
    require(re.fullmatch(r"[^\s@]+@sha256:" + HEX, subject), "invalid subject digest")
    hit = value["hit"]
    if hit:
        require(hex_id(value.get("manifest_id")), "hit is missing a valid manifest ID")
    else:
        require(value.get("manifest_id", "") == "", "miss must not carry a manifest ID")
    require(expected == "any" or hit == (expected == "hit"), f"expected lookup {expected}")
    if manifest_id is not None:
        require(value.get("manifest_id") == manifest_id, "lookup returned a different manifest ID")
    return ["hit" if hit else "miss", subject, value.get("manifest_id", "")]


def put(value, subject, manifest_id):
    require(value.get("written") is True, "put must report written=true")
    require(value.get("subject") == subject, "put resolved an unexpected subject")
    require(hex_id(manifest_id) and value.get("manifest_id") == manifest_id,
            "put returned a different/invalid manifest ID")


def info(value, fixture_arch=None):
    require(type(value.get("erofs_size")) is int and value["erofs_size"] > 0,
            "info must report a positive EROFS size")
    if fixture_arch:
        config = value.get("config")
        require(isinstance(config, dict), "info is missing the fixture runtime config")
        expected = {"Architecture": fixture_arch, "Os": "linux", "User": "10001:10002",
                    "WorkingDir": "/home/e2e", "Entrypoint": ["/usr/bin/fixture"],
                    "Env": ["PATH=/usr/bin"]}
        for key, field in expected.items():
            require(config.get(key) == field, f"fixture runtime config changed: {key}")


def timestamp(value):
    require(re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)", value),
            "invalid RFC3339 valid_at timestamp")
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def referrers(value, owner, manifest_id, *, expiring=False, record=None, expired=False):
    require(type(value.get("schemaVersion")) is int and value["schemaVersion"] == 2,
            "referrers response is not an OCI index")
    descriptors = value.get("manifests")
    require(isinstance(descriptors, list), "referrers manifests must be an array")
    matches = []
    for descriptor in descriptors:
        require(isinstance(descriptor, dict), "invalid referrer descriptor")
        digest = descriptor.get("digest", "")
        require(isinstance(digest, str) and re.fullmatch("sha256:" + HEX, digest),
                "invalid referrer digest")
        annotations = descriptor.get("annotations", {})
        require(isinstance(annotations, dict), "invalid referrer annotations")
        if (descriptor.get("artifactType") != ARTIFACT_TYPE or
                annotations.get(ANNOTATION + "owner") != owner or
                annotations.get(ANNOTATION + "id") != manifest_id or
                (record is not None and digest != record)):
            continue
        valid_at = annotations.get(ANNOTATION + "valid_at")
        require(isinstance(valid_at, str), "missing valid_at annotation")
        parts = valid_at.split(" ")
        require(len(parts) in (1, 2), "invalid valid_at annotation")
        times = [timestamp(part) for part in parts]
        if len(times) == 2:
            require(times[1] >= times[0], "expiry precedes import")
        if expiring or expired:
            require(len(times) == 2 and times[1] > times[0], "record must have a finite lifetime")
        if expired:
            require(times[1] <= datetime.now(timezone.utc), "indexed record has not expired")
        matches.append((times[0], digest))
    require(matches, "OCI Referrers API is missing the matching owner/id/valid_at record")
    return max(matches)[1]


def auth_denied(text):
    require(re.search(r"\b401\s+Unauthorized\b|\bUNAUTHORIZED\b|authentication required", text, re.I),
            "anonymous export failed without an authentication-denial diagnostic")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="kind", required=True)
    for kind in ("lookup", "put", "info", "referrers", "auth-denied"):
        command = sub.add_parser(kind)
        command.add_argument("path")
        if kind in ("lookup", "put"):
            command.add_argument("--subject", required=True)
            command.add_argument("--id", required=kind == "put")
        if kind == "lookup":
            command.add_argument("--expect", choices=("hit", "miss", "any"), default="any")
        if kind == "info":
            command.add_argument("--fixture-arch")
        if kind == "referrers":
            command.add_argument("--owner", required=True)
            command.add_argument("--id", required=True)
            command.add_argument("--expiring", action="store_true")
            command.add_argument("--record")
            command.add_argument("--expired", action="store_true")
    args = parser.parse_args()
    if args.kind == "auth-denied":
        auth_denied(Path(args.path).read_text())
        return
    value = load(args.path)
    if args.kind == "lookup":
        print("\n".join(lookup(value, args.subject, args.expect, args.id)))
    elif args.kind == "put":
        put(value, args.subject, args.id)
    elif args.kind == "info":
        info(value, args.fixture_arch)
    else:
        print(referrers(value, args.owner, args.id, expiring=args.expiring,
                        record=args.record, expired=args.expired))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError) as error:
        print(f"e2e assertion: {error}", file=sys.stderr)
        sys.exit(1)
