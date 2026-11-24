#!/usr/bin/env python3
"""Reject forged native-installation metadata without executing fixture ELFs."""
import importlib.util
import json
from pathlib import Path
import struct
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("systemd_payload", ROOT / "scripts/release-systemd-payload.py")
V = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(V)
LOCK = json.loads((ROOT / "native-deps/deps/systemd-lock.json").read_text())


def elf(machine=62, interpreter=None, needed="libc.so.6", rpath=None):
    dynamic = interpreter is not None or rpath is not None
    phnum = 1 + (2 if dynamic else 0)
    offset = 64 + 56 * phnum
    body = b""
    headers = []
    if dynamic:
        strings = needed.encode() + b"\0" + (rpath or V.PREFIX + "/lib:" + V.PREFIX + "/lib/systemd").encode() + b"\0"
        strings_offset = offset + 16 * 5
        body = b"".join(struct.pack("<qQ", tag, value) for tag, value in
                        ((5, strings_offset), (10, len(strings)), (1, 0), (29, len(needed) + 1), (0, 0)))
        body += strings
        headers.append(struct.pack("<II6Q", 2, 6, offset, offset, offset, 80, 80, 8))
        interp = (interpreter or V.PREFIX + "/lib/ld-linux-x86-64.so.2").encode() + b"\0"
        interp_offset = offset + len(body)
        body += interp
        headers.append(struct.pack("<II6Q", 3, 4, interp_offset, interp_offset, interp_offset, len(interp), len(interp), 1))
    size = offset + len(body)
    load = struct.pack("<II6Q", 1, 5, 0, 0, 0, size, size, 4096)
    header = struct.pack("<16sHHIQQQIHHHHHH", b"\x7fELF\x02\x01\x01" + bytes(9),
                         3, machine, 1, 0, 64, 0, 0, 64, 56, phnum, 0, 0, 0)
    return header + load + b"".join(headers) + body


class InstallationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name in V.REQUIRED:
            binary = name.startswith("bin/") or name in (
                "lib/systemd/systemd", "lib/systemd/systemd-executor", "lib/systemd/systemd-shutdown",
                "lib/systemd/systemd-journald", V.HELPER)
            self.put(name, elf() if binary else b"fixture data\n", 0o755 if binary else 0o644)
        self.put("lib/systemd/systemd", elf(interpreter=V.PREFIX + "/lib/ld-linux-x86-64.so.2"), 0o755)
        self.put("lib/ld-linux-x86-64.so.2", elf(), 0o755)
        self.put("lib/libc.so.6", elf(), 0o755)
        for name in LOCK["dlopen"]:
            self.put("lib/" + name, elf(), 0o644)
        self.put("lib/systemd/system/dbus.service", ("ExecStart=" + V.PREFIX + "/bin/dbus-daemon\n").encode())
        for name in ("lib/systemd/system-generators", "lib/systemd/system-environment-generators",
                     "share/licenses/systemd", "share/licenses/dbus", "share/sandbox-runtime"):
            (self.root / name).mkdir(parents=True, exist_ok=True)
        self.put("share/licenses/dependencies/libc/copyright", b"fixture notice\n")
        (self.root / V.HELPER).chmod(0o4755)
        self.manifest = {
            "schema": 1, "architecture": "x86_64", "prefix": V.PREFIX,
            "sources": {k: LOCK[k] for k in ("systemd", "dbus")},
            "setuid_allowlist": [V.HELPER],
            "loader": {"path": "lib/ld-linux-x86-64.so.2", "original_sha256": V.sha(elf())},
            "dlopen": ["lib/" + name for name in LOCK["dlopen"]],
            "dependencies": {"libc": {"path": "lib/libc.so.6", "original_sha256": V.sha(elf()),
                "package": "libc6", "version": "fixture", "source_package": "glibc", "source_version": "fixture",
                "architecture": "amd64", "license": "share/licenses/dependencies/libc/copyright"}},
        }
        self.refresh()

    def put(self, name, data, mode=0o644):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        path.chmod(mode)

    def refresh(self):
        tree = V.DirectoryTree(self.root)
        self.manifest["files"] = {}
        for name in sorted(tree.entries() - {V.MANIFEST}):
            metadata = tree.metadata(name)
            if metadata["type"] == "file":
                metadata["sha256"] = V.sha(tree.read(name))
            self.manifest["files"][name] = metadata
        (self.root / V.MANIFEST).write_text(json.dumps(self.manifest))
        (self.root / V.MANIFEST).chmod(0o644)

    def verify(self):
        return V.verify(V.DirectoryTree(self.root), "x86_64", LOCK)

    def test_complete_installation_is_inspected_without_execution(self):
        self.assertGreater(self.verify()["elfs"], 10)

    def test_modified_file_is_rejected(self):
        self.put("bin/systemctl", b"changed bytes", 0o755)
        with self.assertRaisesRegex(ValueError, "differs"):
            self.verify()

    def test_unlisted_file_is_rejected(self):
        self.put("bin/unlisted", b"anything")
        with self.assertRaisesRegex(ValueError, "inventory differs"):
            self.verify()

    def test_rehashed_foreign_interpreter_is_rejected(self):
        self.put("lib/systemd/systemd", elf(interpreter="/lib64/ld-linux-x86-64.so.2"), 0o755)
        self.refresh()
        with self.assertRaisesRegex(ValueError, "interpreter escapes"):
            self.verify()

    def test_rehashed_foreign_search_path_is_rejected(self):
        self.put("lib/systemd/systemd", elf(rpath="/usr/lib"), 0o755)
        self.refresh()
        with self.assertRaisesRegex(ValueError, "search path escapes"):
            self.verify()

    def test_rehashed_absolute_needed_is_rejected(self):
        self.put("lib/systemd/systemd", elf(needed="/usr/lib/libc.so.6", rpath=V.PREFIX + "/lib:" + V.PREFIX + "/lib/systemd"), 0o755)
        self.refresh()
        with self.assertRaisesRegex(ValueError, "private SONAME"):
            self.verify()

    def test_rehashed_wrong_target_is_rejected(self):
        self.put("bin/systemctl", elf(machine=183), 0o755)
        self.refresh()
        with self.assertRaisesRegex(ValueError, "ELF target differs"):
            self.verify()

    def test_loader_original_bytes_are_independently_bound(self):
        self.put("lib/ld-linux-x86-64.so.2", elf() + b"changed", 0o755)
        self.refresh()
        with self.assertRaisesRegex(ValueError, "loader was modified"):
            self.verify()

    def test_privilege_cannot_expand_beyond_helper(self):
        (self.root / "bin/systemctl").chmod(0o4755)
        self.refresh()
        with self.assertRaisesRegex(ValueError, "privilege/mode not allowed"):
            self.verify()

    def test_helper_must_retain_its_reviewed_privilege(self):
        (self.root / V.HELPER).chmod(0o755)
        self.refresh()
        with self.assertRaisesRegex(ValueError, "privilege/mode not allowed"):
            self.verify()

    def test_external_config_exception_cannot_be_used_for_code(self):
        (self.root / "bin/systemctl").unlink()
        (self.root / "bin/systemctl").symlink_to("/usr/bin/systemctl")
        self.refresh()
        with self.assertRaisesRegex(ValueError, "symlink escapes"):
            self.verify()

    def test_administrator_environment_link_is_explicitly_supported(self):
        target = self.root / "lib/environment.d/99-environment.conf"
        target.parent.mkdir()
        target.symlink_to("/etc/environment")
        self.refresh()
        self.verify()

    def test_symlink_cycle_is_rejected(self):
        (self.root / "bin/a").symlink_to("b")
        (self.root / "bin/b").symlink_to("a")
        self.refresh()
        with self.assertRaisesRegex(ValueError, "symlink cycle"):
            self.verify()

    def test_missing_dlopen_dependency_is_not_silently_omitted(self):
        (self.root / "lib/libnss_dns.so.2").unlink()
        self.refresh()
        with self.assertRaisesRegex(ValueError, "missing required"):
            self.verify()

    def test_malformed_elf_offsets_are_rejected(self):
        data = bytearray(elf())
        struct.pack_into("<Q", data, 32, 1 << 63)
        with self.assertRaisesRegex(ValueError, "program headers"):
            V.elf_metadata(data, "x86_64")

    def test_source_lock_matches_standalone_release_pins(self):
        self.assertEqual({k: (v["version"], v["sha256"]) for k, v in LOCK.items()
                          if k in V.PINNED_SOURCE_IDS}, V.PINNED_SOURCE_IDS)
        self.assertEqual(LOCK["systemd"]["patches"], V.PINNED_SYSTEMD_PATCHES)

    def test_omitting_configuration_patch_identity_is_rejected(self):
        self.manifest["sources"]["systemd"] = dict(self.manifest["sources"]["systemd"], patches=[])
        self.refresh()
        with self.assertRaisesRegex(ValueError, "configuration-search patch identity"):
            self.verify()

    def test_raw_erofs_mode_keeps_setuid_hidden_by_dump_output(self):
        data = bytearray(8192)
        data[1024:1028] = bytes.fromhex("e2e1f5e0")
        data[1036] = 12
        struct.pack_into("<I", data, 1064, 1)
        struct.pack_into("<H", data, 4096 + 3 * 32 + 4, 0o104755)
        image = self.root / "raw.erofs"
        image.write_bytes(data)
        self.assertEqual(V.erofs_inode_mode(image, 3), 0o104755)
        with self.assertRaisesRegex(ValueError, "inode exceeds"):
            V.erofs_inode_mode(image, 100000)
        struct.pack_into("<I", data, 1104, 0x100)
        image.write_bytes(data)
        with self.assertRaisesRegex(ValueError, "compressed metadata"):
            V.erofs_inode_mode(image, 3)


if __name__ == "__main__":
    unittest.main()
