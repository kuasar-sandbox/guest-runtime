#!/usr/bin/env python3
"""Qualify private vendor configuration with native tools in an offline image.

The fixture has no rootfs libc or systemd and initially no messagebus account.
This checks the installed tools' real default search paths; it does not claim
PID 1 integration coverage. Only this invocation's image/container are removed.
"""
import argparse
import hashlib
import json
from pathlib import Path
import platform
import shutil
import subprocess
import uuid

PREFIX = "/run/sandbox-runtime/systemd"


def write(path, text, mode=0o644):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(mode)


def call(*args, check=True):
    result = subprocess.run(args, text=True, capture_output=True)
    if check and result.returncode:
        raise RuntimeError("command failed: " + " ".join(args) + "\n" + result.stdout + result.stderr)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--payload", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    payload, output = args.payload.resolve(), args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    root = output / "root"
    root.mkdir()
    manifest_bytes = (payload / "share/sandbox-runtime/systemd-manifest.json").read_bytes()
    manifest = json.loads(manifest_bytes)
    if manifest["architecture"] != platform.machine():
        raise ValueError("configuration qualification must use the native architecture")
    shutil.copytree(payload, root / PREFIX.lstrip("/"), symlinks=True)
    (root / "bin").mkdir()
    shutil.copyfile(payload / "bin/busybox", root / "bin/busybox")
    (root / "bin/busybox").chmod(0o755)
    for name in ("sh", "mkdir", "cat", "grep", "stat", "awk"):
        (root / "bin" / name).symlink_to("busybox")
    for name in ("proc", "sys", "dev", "tmp", "var/tmp"):
        (root / name).mkdir(parents=True, exist_ok=True)
    write(root / "etc/passwd", "root:x:0:0:root:/root:/bin/sh\n")
    write(root / "etc/group", "root:x:0:\n")
    write(root / "etc/nsswitch.conf", "passwd: files\ngroup: files\nhosts: files dns\n")
    write(root / "etc/machine-id", "00112233445566778899aabbccddeeff\n")
    unit = "[Service]\nType=oneshot\nExecStart=/bin/true\n[Install]\nWantedBy=multi-user.target\n"
    for name in ("image-allowed", "image-blocked"):
        write(root / ("usr/lib/systemd/system/" + name + ".service"), unit)
    write(root / "usr/lib/systemd/system-preset/95-image-probe.preset",
          "enable image-allowed.service\ndisable image-blocked.service\n")
    script = r'''#!/bin/sh
set -eu
export PATH=/run/sandbox-runtime/systemd/bin:/bin
export SYSTEMD_UNIT_PATH=/etc/systemd/system:/run/systemd/system:/run/sandbox-runtime/systemd/lib/systemd/system:/usr/local/lib/systemd/system:/usr/lib/systemd/system
! grep -q '^messagebus:' /etc/passwd
systemd-sysusers
awk -F: '$1 == "messagebus" && $3 > 0 { found=1; print "SYSUSERS_PASS uid=" $3 } END { exit !found }' /etc/passwd
systemd-tmpfiles --create --boot --prefix=/tmp --prefix=/var/tmp
test "$(stat -c %a /tmp)" = 1777
test "$(stat -c %a /var/tmp)" = 1777
echo TMPFILES_PASS
systemctl --root=/ preset-all
test -L /etc/systemd/system/multi-user.target.wants/image-allowed.service
test ! -e /etc/systemd/system/multi-user.target.wants/image-blocked.service
test ! -e /etc/systemd/system/sysinit.target.wants/debug-shell.service
test ! -e /etc/systemd/system/getty.target.wants/getty@tty1.service
echo PRESET_PASS
systemd-analyze cat-config systemd/journald.conf > /journal-vendor.txt
cat /journal-vendor.txt
grep -q 'Storage=volatile' /journal-vendor.txt
grep -q 'RuntimeMaxUse=16M' /journal-vendor.txt
systemd-analyze cat-config systemd/system.conf > /manager-vendor.txt
grep -q '^DefaultEnvironment=PATH=/run/sandbox-runtime/systemd/bin:' /manager-vendor.txt
mkdir -p /etc/systemd/journald.conf.d
echo '[Journal]' > /etc/systemd/journald.conf.d/50-sandbox-runtime.conf
echo 'RuntimeMaxUse=7M' >> /etc/systemd/journald.conf.d/50-sandbox-runtime.conf
systemd-analyze cat-config systemd/journald.conf > /journal-admin.txt
grep -q 'RuntimeMaxUse=7M' /journal-admin.txt
! grep -q 'RuntimeMaxUse=16M' /journal-admin.txt
echo CONFIG_PRECEDENCE_PASS
'''
    write(root / "entrypoint", script, 0o755)
    for directory in (root, *root.rglob("*")):
        if directory.is_dir() and not directory.is_symlink():
            directory.chmod(0o755)
    archive = output / "root.tar"
    call("tar", "--owner=0", "--group=0", "-cf", str(archive), "-C", str(root), ".")
    image = call("docker", "import", str(archive)).stdout.strip()
    name = "systemd-config-" + uuid.uuid4().hex[:12]
    summary = {"architecture": platform.machine(), "manifest_sha256": hashlib.sha256(manifest_bytes).hexdigest(),
               "image": image, "container": name, "passed": False}
    try:
        result = call("docker", "run", "--name", name, "--network=none", image, "/entrypoint", check=False)
        write(output / "console.log", result.stdout + result.stderr)
        if result.returncode:
            raise RuntimeError("private configuration qualification failed:\n" + result.stdout + result.stderr)
        for item in ("SYSUSERS_PASS", "TMPFILES_PASS", "PRESET_PASS", "CONFIG_PRECEDENCE_PASS"):
            if item not in result.stdout:
                raise AssertionError("missing configuration result " + item)
        for source in ("/etc/passwd", "/journal-vendor.txt", "/journal-admin.txt", "/manager-vendor.txt"):
            call("docker", "cp", name + ":" + source, str(output / Path(source).name))
        summary.update(passed=True, checks=["private sysusers creates messagebus", "private tmpfiles sets sticky temporary directories",
                                           "private presets suppress debug-shell and unused getty", "rootfs vendor presets remain active",
                                           "private journald and manager defaults found", "administrator configuration takes precedence"])
    finally:
        call("docker", "rm", "-f", name, check=False)
        call("docker", "image", "rm", image, check=False)
        archive.unlink(missing_ok=True)
        write(output / "summary.json", json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, sort_keys=True))


if __name__ == "__main__":
    main()
