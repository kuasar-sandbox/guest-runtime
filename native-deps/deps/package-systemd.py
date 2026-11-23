#!/usr/bin/env python3
"""Assemble a native private-prefix installation and record its complete closure.

Only source-built files and the native builder's package-managed dependencies
are inputs. readelf inspects ELF metadata; no payload is executed to find DSOs.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import stat
import subprocess
import sys

MANIFEST = "share/sandbox-runtime/systemd-manifest.json"
HELPER = "lib/dbus-1.0/dbus-daemon-launch-helper"
LOADERS = {"x86_64": "ld-linux-x86-64.so.2", "aarch64": "ld-linux-aarch64.so.1"}
HERE = Path(__file__).resolve().parent


def run(*args):
    return subprocess.check_output(args, text=True, env=dict(os.environ, LC_ALL="C")).strip()


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write(path, text, mode=0o644):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(mode)


def json_write(path, value):
    write(path, json.dumps(value, sort_keys=True, indent=2) + "\n")


def elf(path):
    if path.is_symlink() or not path.is_file():
        return False
    with path.open("rb") as stream:
        return stream.read(4) == b"\x7fELF"


def metadata(path):
    dynamic = run("readelf", "-dW", str(path))
    headers = run("readelf", "-lW", str(path))
    needed = re.findall(r"\(NEEDED\).*?\[(.*?)\]", dynamic)
    interpreter = re.findall(r"Requesting program interpreter: (.*?)\]", headers)
    return needed, interpreter[0] if interpreter else None, "(STRTAB)" in dynamic


def collect_inputs(lock):
    root = HERE.parents[1]
    lock = lock.resolve()
    recipe = [HERE / "build-systemd.sh", HERE / "common.sh", Path(__file__), lock,
              root / "scripts/release-systemd-payload.py"]
    for patch in json.loads(lock.read_text())["systemd"]["patches"]:
        name = patch["file"]
        if not re.fullmatch(r"systemd-patches/[A-Za-z0-9._-]+\.patch", name):
            raise ValueError("unsafe systemd patch path")
        path = HERE / name
        if digest(path) != patch["sha256"]:
            raise ValueError("systemd source patch differs from the locked digest: " + name)
        recipe.append(path)
    value = {
        "architecture": platform.machine(),
        "recipe": {str(p.relative_to(root)): digest(p) for p in recipe},
        "packages": run("dpkg-query", "-W", "-f=${binary:Package}\t${Version}\t${Architecture}\n").splitlines(),
        "tools": {name: run(*command).splitlines()[0] for name, command in {
            "gcc": ["gcc", "--version"], "meson": ["meson", "--version"],
            "ninja": ["ninja", "--version"], "patchelf": ["patchelf", "--version"],
            "readelf": ["readelf", "--version"],
        }.items()},
        "compiler_environment": {k: os.environ.get(k, "") for k in
                                 ("CC", "CFLAGS", "CPPFLAGS", "LDFLAGS", "SOURCE_DATE_EPOCH")},
    }
    value["sha256"] = hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()
    return value


def license_tree(source, destination):
    matched = []
    for child in sorted(source.iterdir()):
        if child.is_file() and re.match(r"^(COPYING|LICENSE|NOTICE|AUTHORS|PATENTS)", child.name, re.I):
            matched.append(child)
    if (source / "LICENSES").is_dir():
        matched.extend(p for p in (source / "LICENSES").rglob("*") if p.is_file())
    if not matched:
        raise ValueError("source has no license material: " + str(source))
    for path in matched:
        if path.is_symlink():
            raise ValueError("source license must be a regular file: " + str(path))
        target = destination / path.relative_to(source)
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(path, target)
        target.chmod(0o644)


def package_owner(path):
    # dpkg records either /lib or /usr/lib on merged-/usr installations.
    names = [str(path), str(path.resolve())]
    for name in list(names):
        if name.startswith("/usr/"):
            names.append(name[4:])
        elif name.startswith(("/lib/", "/lib64/", "/bin/", "/sbin/")):
            names.append("/usr" + name)
    package = None
    for name in dict.fromkeys(names):
        result = subprocess.run(["dpkg-query", "-S", name], text=True, capture_output=True)
        for line in result.stdout.splitlines():
            if ": " in line and line.rsplit(": ", 1)[1] == name:
                package = line.rsplit(": ", 1)[0]
                break
        if package:
            break
    if not package:
        raise ValueError("dependency is not owned by a native builder package: " + str(path))
    fields = run("dpkg-query", "-W", "-f=${binary:Package}\t${Version}\t${source:Package}\t${source:Version}\t${Architecture}", package).split("\t")
    if len(fields) != 5 or any(not item for item in fields):
        raise ValueError("incomplete source package identity: " + package)
    return dict(zip(("package", "version", "source_package", "source_version", "architecture"), fields))


def package_license(package, destination):
    candidates = [Path(p) for p in run("dpkg-query", "-L", package).splitlines()
                  if p.endswith("/copyright") and Path(p).is_file()]
    if not candidates:
        # Development and shared packages may share the base documentation.
        base = package.split(":", 1)[0]
        candidate = Path("/usr/share/doc") / base / "copyright"
        if candidate.is_file():
            candidates.append(candidate)
    if not candidates:
        raise ValueError("dependency package has no copyright file: " + package)
    destination.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(candidates[0].resolve(), destination / "copyright")
    (destination / "copyright").chmod(0o644)


def assemble(args):
    lock = json.loads(args.lock.read_text())
    prefix = lock["prefix"]
    output = args.output
    if output.exists() and any(output.iterdir()):
        raise ValueError("assembly output must be an empty directory")
    shutil.copytree(args.stage / prefix.lstrip("/"), output, symlinks=True, dirs_exist_ok=True)
    # Headers, static link archives and build-only pkg-config files are not
    # runtime dependencies. Remove them before closure discovery.
    for relative in ("include", "lib/pkgconfig", "share/pkgconfig", "share/man", "share/doc"):
        shutil.rmtree(output / relative, ignore_errors=True)
    for path in list(output.rglob("*")):
        if path.is_file() and path.suffix in (".a", ".la"):
            path.unlink()
    for directory in ("bin", "lib", "lib/systemd", "share/dbus-1/system.d", "share/dbus-1/system-services"):
        (output / directory).mkdir(parents=True, exist_ok=True)
    # This upstream compatibility link intentionally reads administrator data
    # outside the prefix. Keep the single audited exception explicit.
    environment = output / "lib/environment.d/99-environment.conf"
    if environment.is_symlink():
        environment.unlink()
        environment.symlink_to("/etc/environment")
    shutil.copyfile("/bin/busybox", output / "bin/busybox")
    (output / "bin/busybox").chmod(0o755)
    if metadata(output / "bin/busybox")[1]:
        raise ValueError("mount/umount helper must come from busybox-static")
    for name in ("mount", "umount"):
        target = output / "bin" / name
        target.unlink(missing_ok=True)
        target.symlink_to("busybox")

    units = output / "lib/systemd/system"
    for unit in units.glob("*.service"):
        if unit.is_symlink():
            continue
        def anchor(match):
            key, modifier, command, rest = match.groups()
            if "/" not in command and (output / "bin" / command).exists():
                command = prefix + "/bin/" + command
            return key + modifier + command + rest
        text = re.sub(r"^(Exec(?:Start|StartPre|StartPost|Stop|StopPost|Reload|Condition)=)([-+!:@]*)([^\s]+)(.*)$",
                      anchor, unit.read_text(), flags=re.M)
        unit.write_text(text)
    for folder in units.glob("*.wants"):
        for link in folder.iterdir():
            name = link.name
            if (name.endswith(".mount") or name.startswith("systemd-udev")
                    or name.startswith("systemd-update-utmp")
                    or name in ("ldconfig.service", "systemd-sysext.socket", "systemd-pstore.service")):
                link.unlink()
    # A deterministic container preset. Administrative /etc and /run remain
    # higher priority in the manager's independently configured search paths.
    write(units / "dbus.service", f"""[Unit]
Description=System Message Bus
Requires=dbus.socket
After=dbus.socket
[Service]
Type=notify
NotifyAccess=main
ExecStart={prefix}/bin/dbus-daemon --config-file={prefix}/share/dbus-1/system.conf --address=systemd: --nofork --nopidfile --systemd-activation
OOMScoreAdjust=-900
""")
    write(units / "dbus.socket", "[Unit]\nDescription=System Message Bus Socket\n[Socket]\nListenStream=/run/dbus/system_bus_socket\nSocketMode=0666\nRemoveOnStop=yes\n")
    wants = units / "sockets.target.wants"
    wants.mkdir(exist_ok=True)
    (wants / "dbus.socket").unlink(missing_ok=True)
    (wants / "dbus.socket").symlink_to("../dbus.socket")
    write(output / "lib/sysusers.d/dbus.conf", 'u messagebus - "System Message Bus" /nonexistent\n')
    write(output / "lib/systemd/journald.conf.d/50-sandbox-runtime.conf",
          "[Journal]\nStorage=volatile\nRuntimeMaxUse=16M\nForwardToConsole=no\n")
    write(output / "lib/systemd/system.conf.d/50-sandbox-runtime.conf",
          "[Manager]\nDefaultEnvironment=PATH=" + prefix + "/bin:"
          "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n")
    # GETTY_AUTO=no suppresses generator-created terminals. First-boot preset
    # handling independently enables getty@tty1, so keep this small vendor
    # override in the private prefix as well. Other image service policy stays
    # in the ordinary preset cascade, including administrator /etc overrides.
    write(output / "lib/systemd/system-preset/80-sandbox-runtime.preset",
          "disable getty@.service\ndisable serial-getty@.service\ndisable console-getty.service\n")
    config = output / "share/dbus-1/system.conf"
    text = config.read_text().replace("<standard_system_servicedirs/>",
           f"<servicedir>{prefix}/share/dbus-1/system-services</servicedir>"
           "<servicedir>/usr/local/share/dbus-1/system-services</servicedir>"
           "<servicedir>/usr/share/dbus-1/system-services</servicedir>")
    text = text.replace("<includedir>system.d</includedir>",
                        "<includedir>system.d</includedir><includedir>/usr/share/dbus-1/system.d</includedir>")
    config.write_text(text)

    libraries = {}
    for line in run("ldconfig", "-p").splitlines():
        match = re.match(r"\s*(\S+)\s+\([^)]*\)\s+=>\s+(/.+)$", line)
        if match:
            libraries.setdefault(match[1], Path(match[2]))
    dependencies = {}
    queue = [p for p in output.rglob("*") if elf(p)]

    def add_dependency(soname):
        for relative in ("lib/" + soname, "lib/systemd/" + soname):
            candidate = output / relative
            if candidate.is_file():
                return candidate
        source = libraries.get(soname)
        if source is None:
            raise ValueError("native dependency is unavailable: " + soname)
        target = output / "lib" / soname
        shutil.copyfile(source.resolve(), target)
        target.chmod(0o755 if source.stat().st_mode & 0o111 else 0o644)
        record = package_owner(source)
        record.update(path="lib/" + soname, original_sha256=digest(source.resolve()))
        dependencies[soname] = record
        queue.append(target)
        return target

    for soname in lock["dlopen"]:
        add_dependency(soname)
    loader_name = LOADERS[args.arch]
    seen = set()
    while queue:
        path = queue.pop()
        if path in seen:
            continue
        seen.add(path)
        needed, interpreter, dynamic = metadata(path)
        if interpreter:
            if Path(interpreter).name != loader_name:
                raise ValueError("wrong architecture ELF interpreter: " + str(path))
            libraries.setdefault(loader_name, Path(interpreter))
            add_dependency(loader_name)
        for soname in needed:
            add_dependency(soname)
    loader = output / "lib" / loader_name
    loader_sha = digest(loader)
    for path in sorted(seen):
        if path == loader:
            continue  # The loader relocates itself. Never patchelf its bytes.
        _, interpreter, dynamic = metadata(path)
        if dynamic:
            run("patchelf", "--set-rpath", prefix + "/lib:" + prefix + "/lib/systemd", str(path))
            if interpreter:
                run("patchelf", "--set-interpreter", prefix + "/lib/" + loader_name, str(path))
    if digest(loader) != loader_sha:
        raise ValueError("ELF loader changed during closure assembly")

    material = output / "share/licenses"
    license_tree(args.systemd_source, material / "systemd")
    license_tree(args.dbus_source, material / "dbus")
    busybox = package_owner(Path("/bin/busybox"))
    busybox.update(path="bin/busybox", original_sha256=digest(Path("/bin/busybox")))
    dependencies["busybox"] = busybox
    for record in dependencies.values():
        label = record["package"].replace(":", "_")
        record["license"] = "share/licenses/dependencies/" + label + "/copyright"
        package_license(record["package"], material / "dependencies" / label)
    for path in Path("/usr/share/common-licenses").iterdir():
        if path.is_file():
            target = material / "common-licenses" / path.name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(path.resolve(), target)
    # The helper verifies real UID == configured messagebus and effective UID
    # == root before executing a root-owned activation definition. A fixed
    # messagebus GID cannot be encoded in an all-root EROFS installation.
    # This is the only permitted setuid file; the active bind must not be nosuid.
    helper_source = (args.dbus_source / "bus/activation-helper.c").read_text()
    if not all(token in helper_source for token in
               ("uid = getuid()", "pw->pw_uid != uid", "euid = geteuid()", "euid != 0")):
        raise ValueError("D-Bus helper caller guard differs from the reviewed source")
    for path in output.rglob("*"):
        if path.is_symlink():
            continue
        if path.is_dir():
            path.chmod(0o755)
        elif path.is_file():
            path.chmod(0o755 if path.stat().st_mode & 0o111 else 0o644)
        else:
            raise ValueError("unexpected native payload file type: " + str(path))
    (output / HELPER).chmod(0o4755)
    output.chmod(0o755)
    manifest = {
        "schema": 1, "architecture": args.arch, "prefix": prefix,
        "sources": {key: lock[key] for key in ("systemd", "dbus")},
        "inputs": json.loads(args.inputs.read_text()),
        "loader": {"path": "lib/" + loader_name, "original_sha256": loader_sha},
        "dlopen": ["lib/" + name for name in lock["dlopen"]],
        "dependencies": dependencies, "setuid_allowlist": [HELPER], "files": {},
    }
    # Parent directories must be represented as well, including the directory
    # that contains the self-describing manifest (the manifest excludes itself).
    (output / MANIFEST).parent.mkdir(parents=True, exist_ok=True)
    for path in sorted(output.rglob("*")):
        name = str(path.relative_to(output))
        mode = stat.S_IMODE(path.lstat().st_mode)
        if path.is_symlink():
            value = {"type": "symlink", "target": os.readlink(path)}
        elif path.is_dir():
            value = {"type": "directory", "mode": mode}
        else:
            value = {"type": "file", "mode": mode, "size": path.stat().st_size, "sha256": digest(path)}
        manifest["files"][name] = value
    json_write(output / MANIFEST, manifest)
    print(json.dumps({"architecture": args.arch, "elfs": len(seen), "dependencies": len(dependencies),
                      "manifest_sha256": digest(output / MANIFEST), "payload": str(output)}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    inputs = sub.add_parser("inputs")
    inputs.add_argument("--lock", type=Path, required=True)
    inputs.add_argument("--output", type=Path, required=True)
    build = sub.add_parser("assemble")
    for name in ("stage", "output", "lock", "inputs", "systemd-source", "dbus-source"):
        build.add_argument("--" + name, type=Path, required=True)
    build.add_argument("--arch", choices=LOADERS, required=True)
    args = parser.parse_args()
    if args.command == "inputs":
        json_write(args.output, collect_inputs(args.lock))
    else:
        assemble(args)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        raise SystemExit("systemd packaging: " + str(error))
