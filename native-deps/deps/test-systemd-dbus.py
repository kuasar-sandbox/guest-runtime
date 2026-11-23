#!/usr/bin/env python3
"""Exercise the private D-Bus closure and traditional activation in isolation.

Requires native Docker. The temporary image contains static BusyBox, fixture
configuration and the verified payload, with no rootfs libc or systemd. It is
read-only and offline. This is a payload qualification test, not a Guest VM
integration test. Only resources created by this invocation are removed.
"""
import argparse
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import time
import uuid

PREFIX = "/run/sandbox-runtime/systemd"


def call(*args, check=True):
    result = subprocess.run(args, text=True, capture_output=True)
    if check and result.returncode:
        raise RuntimeError("command failed: " + " ".join(args) + "\n" + result.stderr)
    return result


def write(path, text, mode=0o644):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(mode)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--payload", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    payload, output = args.payload.resolve(), args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    root = output / "root"
    root.mkdir()
    manifest = json.loads((payload / "share/sandbox-runtime/systemd-manifest.json").read_text())
    if manifest["architecture"] != platform.machine():
        raise ValueError("D-Bus qualification must execute on the native architecture")
    shutil.copytree(payload, root / PREFIX.lstrip("/"), symlinks=True)
    (root / "bin").mkdir()
    shutil.copyfile(payload / "bin/busybox", root / "bin/busybox")
    (root / "bin/busybox").chmod(0o755)
    for name in ("sh", "mkdir", "cat", "stat"):
        (root / "bin" / name).symlink_to("busybox")
    for name in ("proc", "sys", "dev", "tmp", "run/dbus", "var/lib/dbus"):
        (root / name).mkdir(parents=True, exist_ok=True)
    write(root / "etc/passwd", "root:x:0:0:root:/root:/bin/sh\nmessagebus:x:81:81:bus:/:/bin/sh\nruntimeprobe:x:61500:61500:probe:/:/bin/sh\n")
    write(root / "etc/group", "root:x:0:\nmessagebus:x:81:\nruntimeprobe:x:61500:\n")
    write(root / "etc/nsswitch.conf", "passwd: files\ngroup: files\nhosts: files dns\n")
    write(root / "etc/machine-id", "00112233445566778899aabbccddeeff\n")
    (root / "var/lib/dbus/machine-id").symlink_to("/etc/machine-id")
    write(root / "usr/share/dbus-1/system-services/com.example.Traditional.service",
          "[D-BUS Service]\nName=com.example.Traditional\n"
          f"Exec={PREFIX}/bin/dbus-test-tool echo --system --name=com.example.Traditional\nUser=runtimeprobe\n")
    write(root / "etc/dbus-1/system.d/runtime-probe.conf",
          '<busconfig><policy user="runtimeprobe"><allow own="com.example.Traditional"/></policy>'
          '<policy context="default"><allow send_destination="com.example.Traditional"/></policy></busconfig>\n')
    write(root / "entrypoint", "#!/bin/sh\nset -eu\n"
          f"exec {PREFIX}/bin/dbus-daemon --config-file={PREFIX}/share/dbus-1/system.conf "
          "--address=unix:path=/run/dbus/system_bus_socket --nofork --nopidfile\n", 0o755)
    # Remote job runners may use umask 077. The in-image system bus drops to
    # messagebus and must be able to traverse the fixture's ordinary parents.
    # Normalize the image root as well as newly created /run, /usr and /etc.
    for directory in (root, *root.rglob("*")):
        if directory.is_dir() and not directory.is_symlink():
            directory.chmod(0o755)
    (root / "tmp").chmod(0o1777)
    archive = output / "root.tar"
    call("tar", "--owner=0", "--group=0", "-cf", str(archive), "-C", str(root), ".")
    image = call("docker", "import", str(archive)).stdout.strip()
    name = "systemd-dbus-" + uuid.uuid4().hex[:12]
    result = {"architecture": platform.machine(), "image": image, "container": name}
    try:
        call("docker", "run", "-d", "--name", name, "--network=none", "--read-only",
             "--tmpfs", "/run/dbus:rw,mode=0755", "--tmpfs", "/tmp:rw,mode=1777",
             image, "/entrypoint")
        def bus(method, *values):
            return call("docker", "exec", name, PREFIX + "/bin/dbus-send", "--system",
                        "--print-reply", "--dest=org.freedesktop.DBus", "/", method, *values, check=False)
        until = time.monotonic() + 15
        while True:
            ready = bus("org.freedesktop.DBus.ListNames")
            if ready.returncode == 0:
                break
            if time.monotonic() >= until:
                raise RuntimeError("private-prefix system bus did not become ready: " + ready.stderr)
            time.sleep(0.1)
        mode = call("docker", "exec", name, "/bin/stat", "-c", "%a %u %g",
                    PREFIX + "/lib/dbus-1.0/dbus-daemon-launch-helper").stdout.strip()
        if mode != "4755 0 0":
            raise AssertionError("helper effective mode/ownership differs: " + mode)
        denied = call("docker", "exec", "--user=61500:61500", name,
                      PREFIX + "/lib/dbus-1.0/dbus-daemon-launch-helper", "com.example.Traditional", check=False)
        # activation-exit-codes.h maps Spawn.PermissionsInvalid to 7. The
        # release helper intentionally emits no message for this known error.
        if denied.returncode != 7:
            raise AssertionError("helper did not reject the non-messagebus UID: " + str(denied.returncode))
        activation = bus("org.freedesktop.DBus.StartServiceByName", "string:com.example.Traditional", "uint32:0")
        if activation.returncode or "uint32 1" not in activation.stdout:
            raise AssertionError("traditional activation failed: " + activation.stdout + activation.stderr)
        uid = bus("org.freedesktop.DBus.GetConnectionUnixUser", "string:com.example.Traditional")
        if uid.returncode or "uint32 61500" not in uid.stdout:
            raise AssertionError("activation did not use the requested non-root account")
        pid = bus("org.freedesktop.DBus.GetConnectionUnixProcessID", "string:com.example.Traditional")
        match = re.search(r"uint32 ([0-9]+)", pid.stdout)
        if pid.returncode or not match:
            raise AssertionError("activated service PID is unavailable")
        maps = call("docker", "exec", "--user=61500:61500", name, "/bin/cat",
                    "/proc/" + match[1] + "/maps").stdout
        code = [line.split()[-1] for line in maps.splitlines()
                if "x" in line.split()[1] and line.split()[-1].startswith("/")]
        if not code or any(not path.startswith(PREFIX + "/") for path in code):
            raise AssertionError("activated dynamic program mapped code outside the private installation")
        mountinfo = call("docker", "exec", name, "/bin/cat", "/proc/self/mountinfo").stdout
        write(output / "maps.txt", maps)
        write(output / "mountinfo.txt", mountinfo)
        write(output / "helper-denial.txt", "exit_code=" + str(denied.returncode) + "\n" + denied.stdout + denied.stderr)
        result.update(passed=True, helper_mode=mode, activation_uid=61500, mapped_code=code,
                      checks=["rootfs has no libc", "matching prefix ELF closure", "traditional D-Bus activation",
                              "non-messagebus UID rejected by setuid helper", "non-root activated service"])
    finally:
        logs = call("docker", "logs", name, check=False)
        write(output / "console.log", logs.stdout + logs.stderr)
        call("docker", "rm", "-f", name, check=False)
        call("docker", "image", "rm", image, check=False)
        archive.unlink(missing_ok=True)
        write(output / "summary.json", json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    main()
