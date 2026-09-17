#!/usr/bin/env python3
"""CI-only guard: Ubuntu's host libc must not raise the released Runtime ABI."""
import argparse
from pathlib import Path
import re
import subprocess


def validate(path, static=False):
    output = subprocess.run(
        ["readelf", "--wide", "--program-headers", "--dynamic", "--version-info", str(path)],
        check=True, capture_output=True, text=True,
    ).stdout
    if static and re.search(r"\bINTERP\b|\(NEEDED\)", output):
        raise ValueError(f"{path}: requires a fully static executable")
    versions = re.findall(r"\bGLIBC_(\d+)\.(\d+)(?:\.(\d+))?\b", output)
    if any(tuple(int(part or 0) for part in version) > (2, 38, 0) for version in versions):
        raise ValueError(f"{path}: exceeds the released glibc 2.38 baseline")
    print(f"Runtime ABI: {path}: {'static' if static else 'glibc <= 2.38'}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--static", action="append", default=[], type=Path)
    parser.add_argument("binaries", nargs="+", type=Path)
    args = parser.parse_args()
    for binary in args.static:
        validate(binary, static=True)
    for binary in args.binaries:
        validate(binary)
