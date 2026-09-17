#!/usr/bin/env python3
"""Focused, offline regressions for E2E inputs and shell lifecycle helpers."""

import base64
import copy
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest

import assertions
import fixture

HERE = Path(__file__).resolve().parent
SUBJECT = "127.0.0.1:12345/e2e/app@sha256:" + "a" * 64
MANIFEST_ID = "b" * 64

# Test processes only: a child in a separate session proves cleanup is based
# on ownership, not just the leader's PID or process group.
TREE = r'''
import json, os, pathlib, signal, subprocess, sys, time
root, mode, stubborn = sys.argv[1:]
if stubborn == "yes":
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
child_code = """
import os, pathlib, signal, sys, time
if sys.argv[2] == 'yes':
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
pathlib.Path(sys.argv[1]).write_text(str(os.getpid()))
time.sleep(60)
"""
child = subprocess.Popen([sys.executable, "-c", child_code, root + "/child", stubborn],
                         start_new_session=True)
deadline = time.monotonic() + 5
while not pathlib.Path(root, "child").exists():
    if time.monotonic() > deadline:
        sys.exit(99)
    time.sleep(0.01)
pathlib.Path(root, "ready").write_text(json.dumps([os.getpid(), child.pid]))
if mode != "wait":
    sys.exit(int(mode))
time.sleep(60)
'''


class TemporaryTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="e2e-regression-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def execute(self, command, **kwargs):
        return subprocess.run(command, text=True, capture_output=True, timeout=10, **kwargs)

    def wait_file(self, path, process):
        deadline = time.monotonic() + 5
        while not path.exists():
            self.assertIsNone(process.poll(), "process exited before readiness")
            self.assertLess(time.monotonic(), deadline, "test process failed readiness")
            time.sleep(0.01)

    def start(self, command, **kwargs):
        process = subprocess.Popen(command, text=True, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, start_new_session=True, **kwargs)

        def cleanup():
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=6)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=2)
            process.stdout.close()
            process.stderr.close()
        self.addCleanup(cleanup)
        return process

    def assert_reaped(self, pids):
        for pid in pids:
            self.assertFalse(Path(f"/proc/{pid}").exists(), f"owned PID {pid} survived (or is a zombie)")


class FixtureTests(TemporaryTest):
    def test_archive_integrity_and_layer_semantics(self):
        for arch in ("amd64", "arm64"):
            with self.subTest(architecture=arch):
                output = self.root / (arch + ".tar")
                fixture.build(output, "guest-runtime-e2e:fixture", arch)
                self.assertLess(output.stat().st_size, 100_000)
                with tarfile.open(output) as archive:
                    manifest, = json.load(archive.extractfile("manifest.json"))
                    self.assertEqual(manifest["RepoTags"], ["guest-runtime-e2e:fixture"])
                    config_bytes = archive.extractfile(manifest["Config"]).read()
                    self.assertEqual(manifest["Config"], hashlib.sha256(config_bytes).hexdigest() + ".json")
                    config = json.loads(config_bytes)
                    self.assertEqual(config["architecture"], arch)
                    self.assertEqual(config["os"], "linux")
                    self.assertEqual(config["config"]["User"], "10001:10002")
                    self.assertEqual(config["config"]["Entrypoint"], ["/usr/bin/fixture"])
                    self.assertEqual(config["rootfs"]["type"], "layers")
                    self.assertEqual(len(manifest["Layers"]), 2)
                    self.assertEqual(len(config["history"]), 2)
                    merged, markers = {}, set()
                    for index, name in enumerate(manifest["Layers"]):
                        layer_bytes = archive.extractfile(name).read()
                        self.assertEqual(config["rootfs"]["diff_ids"][index],
                                         "sha256:" + hashlib.sha256(layer_bytes).hexdigest())
                        with tarfile.open(fileobj=io.BytesIO(layer_bytes)) as layer:
                            members = layer.getmembers()
                            self.assertEqual(len({member.name for member in members}), len(members))
                            for member in members:
                                path = PurePosixPath(member.name)
                                self.assertFalse(path.is_absolute())
                                self.assertNotIn("..", path.parts)
                                self.assertEqual(member.mtime, 0)
                                if path.name.startswith(".wh."):
                                    self.assertTrue(member.isfile())
                                    self.assertEqual(member.size, 0)
                                    markers.add(member.name)
                                    victim = str(path.parent / path.name[4:])
                                    if path.name == ".wh..wh..opq":
                                        victims = [key for key in merged if key.startswith(str(path.parent) + "/")]
                                    else:
                                        victims = [key for key in merged if key == victim or key.startswith(victim + "/")]
                                    for key in victims:
                                        del merged[key]
                                else:
                                    content = layer.extractfile(member).read() if member.isfile() else None
                                    merged[member.name] = (member, content)
                    self.assertEqual(markers, {"home/e2e/.wh.remove-me", "home/e2e/opaque/.wh..wh..opq"})
                    self.assertNotIn("home/e2e/remove-me", merged)
                    self.assertNotIn("home/e2e/opaque/old", merged)
                    self.assertNotIn("home/e2e/opaque/nested", merged)
                    self.assertNotIn("home/e2e/opaque/nested/old", merged)
                    self.assertEqual(merged["home/e2e/message"][1], b"upper layer\n")
                    self.assertEqual(merged["home/e2e/opaque/new"][1], b"visible upper file\n")
                    for name in ("home/e2e", "home/e2e/message", "home/e2e/opaque/new", "home/e2e/current"):
                        member = merged[name][0]
                        self.assertEqual((member.uid, member.gid), (10001, 10002))
                    self.assertEqual(merged["home/e2e/message"][0].mode, 0o644)
                    self.assertEqual(merged["usr/bin/fixture"][0].mode, 0o755)
                    self.assertTrue(merged["usr/bin/fixture"][1].startswith(b"#!/bin/sh\n"))
                    link = merged["home/e2e/current"][0]
                    self.assertTrue(link.issym())
                    self.assertEqual(link.linkname, "message")

    def test_reproducible_bytes_and_architecture_aliases(self):
        first, second = self.root / "one.tar", self.root / "two.tar"
        for arch, alias in (("amd64", "x86_64"), ("arm64", "aarch64")):
            fixture.build(first, "guest-runtime-e2e:fixed", arch)
            fixture.build(second, "guest-runtime-e2e:fixed", alias)
            self.assertEqual(first.read_bytes(), second.read_bytes())
        with self.assertRaises(ValueError):
            fixture.build(first, "guest-runtime-e2e:fixed", "unknown")


class AssertionTests(TemporaryTest):
    def test_malformed_or_wrong_types_cannot_be_misses(self):
        path = self.root / "lookup.json"
        for body in ('', '{', '[]', 'null', '{"hit":false,"hit":true}', '{"hit":NaN}'):
            path.write_text(body)
            with self.subTest(body=body), self.assertRaises(ValueError):
                assertions.load(path)
        valid = {"supported": True, "subject": SUBJECT, "hit": False}
        path.write_text(json.dumps(valid, indent=2))
        self.assertEqual(assertions.lookup(assertions.load(path), SUBJECT, "miss")[0], "miss")
        for patch in ({"supported": False}, {"supported": "true"}, {"hit": "false"},
                      {"hit": 0}, {"hit": None}, {"subject": "wrong"}, {"manifest_id": MANIFEST_ID}):
            with self.subTest(patch=patch), self.assertRaises(ValueError):
                assertions.lookup(dict(valid, **patch), SUBJECT, "miss")
        with self.assertRaises(ValueError):
            assertions.lookup({"supported": True, "subject": SUBJECT}, SUBJECT)

    def test_hit_put_info_and_auth_assertions(self):
        hit = {"supported": True, "subject": SUBJECT, "hit": True, "manifest_id": MANIFEST_ID}
        assertions.lookup(hit, SUBJECT, "hit", MANIFEST_ID)
        for patch in ({"manifest_id": ""}, {"manifest_id": "c" * 64}, {"hit": False}):
            with self.subTest(patch=patch), self.assertRaises(ValueError):
                assertions.lookup(dict(hit, **patch), SUBJECT, "hit", MANIFEST_ID)
        put = {"written": True, "subject": SUBJECT, "manifest_id": MANIFEST_ID}
        assertions.put(put, SUBJECT, MANIFEST_ID)
        for patch in ({"written": False}, {"written": "true"}, {"subject": "wrong"}, {"manifest_id": ""}):
            with self.subTest(patch=patch), self.assertRaises(ValueError):
                assertions.put(dict(put, **patch), SUBJECT, MANIFEST_ID)
        assertions.info({"erofs_size": 4096})
        for size in (0, -1, True, "4096", None):
            with self.assertRaises(ValueError):
                assertions.info({"erofs_size": size})
        assertions.auth_denied("GET /v2/: unexpected status code 401 Unauthorized: authentication required")
        for error in ("mkfs.erofs not found", "connection refused", "no CAP_CHOWN", "bad config", "signal: killed"):
            with self.assertRaises(ValueError):
                assertions.auth_denied(error)

    def test_referrer_identity_and_expiry(self):
        owner = 'owner with "quotes"'
        digest = "sha256:" + "c" * 64
        descriptor = {"digest": digest, "artifactType": assertions.ARTIFACT_TYPE, "annotations": {
            assertions.ANNOTATION + "owner": owner,
            assertions.ANNOTATION + "id": MANIFEST_ID,
            assertions.ANNOTATION + "valid_at": "2020-01-01T00:00:00Z 2020-01-01T00:00:10Z"}}
        index = {"schemaVersion": 2, "manifests": [descriptor]}
        self.assertEqual(assertions.referrers(index, owner, MANIFEST_ID, expiring=True, record=digest, expired=True), digest)
        with self.assertRaises(ValueError):
            assertions.referrers(index, owner, MANIFEST_ID, record="sha256:" + "d" * 64)
        for value in ("garbage", "2020-01-01T00:00:00Z", "2020-01-02T00:00:00Z 2020-01-01T00:00:00Z"):
            bad = copy.deepcopy(index)
            bad["manifests"][0]["annotations"][assertions.ANNOTATION + "valid_at"] = value
            with self.subTest(value=value), self.assertRaises(ValueError):
                assertions.referrers(bad, owner, MANIFEST_ID, expired=True)


class ProcessTests(TemporaryTest):
    def supervisor(self, root, mode="wait", timeout=0):
        root.mkdir()
        runner = "import process,sys; sys.exit(process.supervise(sys.argv[1:], grace=0.05, timeout=" + str(timeout) + "))"
        return self.start([sys.executable, "-c", runner, sys.executable, "-c", TREE,
                           str(root), mode, "yes"], cwd=HERE)

    def test_reaps_detached_stubborn_children_after_success_and_partial_failure(self):
        for status in (0, 23):
            with self.subTest(status=status):
                root = self.root / str(status)
                process = self.supervisor(root, mode=str(status))
                out, err = process.communicate(timeout=5)
                self.assertEqual(process.returncode, status, out + err)
                self.assert_reaped(json.loads((root / "ready").read_text()))

    def test_int_term_and_timeout_are_bounded_and_leave_unrelated_processes(self):
        unrelated = self.start([sys.executable, "-c", "import time; time.sleep(60)"])
        for sig, expected in ((signal.SIGINT, 130), (signal.SIGTERM, 143), (None, 124)):
            with self.subTest(signal=sig):
                root = self.root / str(expected)
                process = self.supervisor(root, timeout=0.25 if sig is None else 0)
                self.wait_file(root / "ready", process)
                pids = json.loads((root / "ready").read_text())
                started = time.monotonic()
                if sig is not None:
                    process.send_signal(sig)
                out, err = process.communicate(timeout=5)
                self.assertEqual(process.returncode, expected, out + err)
                self.assertLess(time.monotonic() - started, 2)
                self.assert_reaped(pids)
                self.assertIsNone(unrelated.poll())


class ShellTests(TemporaryTest):
    def fake_tools(self):
        binary = self.root / "bin"
        binary.mkdir()
        for name in ("bash", "dirname", "uname", "mktemp", "mkdir", "rm", "sleep", "cat", "timeout"):
            (binary / name).symlink_to(shutil.which(name))
        (binary / "python3").symlink_to(sys.executable)
        for name in ("curl", "docker", "flatten-ctl", "store-ctl", "zot", "mkfs.erofs"):
            path = binary / name
            path.write_text("#!/bin/bash\nexit 0\n")
            path.chmod(0o755)
        (binary / "id").write_text("#!/bin/bash\necho 0\n")
        (binary / "id").chmod(0o755)
        return binary

    def environment(self, binary):
        return {"PATH": str(binary), "HOME": str(self.root), "TMPDIR": str(self.root),
                "REQUIRE_GUEST_RUNTIME": "1", "PYTHONDONTWRITEBYTECODE": "1",
                "FLATTEN_CTL": str(binary / "flatten-ctl"), "STORE_CTL": str(binary / "store-ctl"),
                "ZOT_BIN": str(binary / "zot"), "MKFS_EROFS_PATH": str(binary / "mkfs.erofs"),
                "BIN": str(binary)}

    def test_every_required_prerequisite_fails_and_optional_skip_is_explicit(self):
        binary = self.fake_tools()
        env = self.environment(binary)
        for name in ("python3", "curl", "docker", "timeout", "flatten-ctl", "store-ctl", "zot", "mkfs.erofs"):
            with self.subTest(tool=name):
                path, hidden = binary / name, binary / (name + ".hidden")
                path.rename(hidden)
                try:
                    result = self.execute(["/bin/bash", str(HERE / "e2e_flatten.sh")], env=env)
                    self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                    self.assertIn("prerequisite not found", result.stderr)
                    self.assertNotIn("[SKIP]", result.stdout)
                finally:
                    hidden.rename(path)
        (binary / "curl").unlink()
        result = self.execute(["/bin/bash", str(HERE / "e2e_flatten.sh")], env=dict(env, REQUIRE_GUEST_RUNTIME="0"))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("[SKIP]", result.stdout)

    def test_unusable_daemon_and_binaries_fail_and_remove_work(self):
        binary = self.fake_tools()
        env = self.environment(binary)
        for name in ("docker", "flatten-ctl", "store-ctl", "zot", "mkfs.erofs"):
            with self.subTest(tool=name):
                (binary / name).write_text("#!/bin/bash\nexit 17\n")
                result = self.execute(["/bin/bash", str(HERE / "e2e_flatten.sh")], env=env)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertIn("[FAIL]", result.stderr)
                self.assertFalse(list(self.root.glob("guest-runtime-e2e.*")))
                (binary / name).write_text("#!/bin/bash\nexit 0\n")

    def test_copied_package_is_self_contained_and_rejects_missing_helpers(self):
        binary = self.fake_tools()
        env = self.environment(binary)
        package = self.root / "assembled" / "test" / "e2e" / "guest-runtime"
        shutil.copytree(HERE, package)
        result = self.execute([sys.executable, str(package / "fixture.py"), str(self.root / "fixture.tar"),
                               "--tag", "guest-runtime-e2e:copy", "--architecture", "amd64"], cwd=self.root)
        self.assertEqual(result.returncode, 0, result.stderr)
        (binary / "curl").unlink()
        result = self.execute(["/bin/bash", str(package / "run_all.sh")], cwd=self.root,
                              env=dict(env, REQUIRE_GUEST_RUNTIME="0"))
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("prerequisite not found: curl", result.stderr)
        for helper in ("e2e_flatten.sh", "common.sh", "fixture.py", "assertions.py", "process.py"):
            with self.subTest(helper=helper):
                path = package / helper
                original = path.read_bytes()
                path.unlink()
                result = self.execute(["/bin/bash", str(package / "run_all.sh")], cwd=self.root, env=env)
                self.assertEqual(result.returncode, 1)
                self.assertIn("incomplete E2E package: missing " + helper, result.stderr)
                path.write_bytes(original)

    def test_private_credentials_and_only_owned_tags_are_cleaned(self):
        binary = self.fake_tools()
        caller = self.root / "caller-docker"
        caller.mkdir()
        original = b'{"auths":{"unrelated":{"auth":"do-not-use"}},"credsStore":"do-not-call"}\n'
        (caller / "config.json").write_bytes(original)
        docker = binary / "docker"
        docker.write_text('#!/bin/bash\nprintf "%s|%s\\n" "$DOCKER_CONFIG" "$*" >>"$TRACE"\n')
        env = dict(self.environment(binary), DOCKER_CONFIG=str(caller), TRACE=str(self.root / "docker-trace"),
                   FLATTEN_REGISTRY_TOKEN="ambient-must-be-cleared", DOCKER_AUTH_CONFIG="ambient")
        script = r'''
set -euo pipefail
source "$1/common.sh"
init_work
[[ "$DOCKER_CONFIG" == "$WORK/docker" ]]
[[ -z "${FLATTEN_REGISTRY_TOKEN:-}" && -z "${DOCKER_AUTH_CONFIG:-}" ]]
printf '%s\n' "$WORK" >"$2/work"
cp "$DOCKER_CONFIG/config.json" "$2/empty.json"
write_docker_auth "$WORK/docker-auth/config.json" localhost:12345 e2euser e2epass
cp "$WORK/docker-auth/config.json" "$2/auth.json"
stat -c '%a' "$WORK" "$DOCKER_CONFIG" "$DOCKER_CONFIG/config.json" >"$2/modes"
DOCKER_TAGS=(owned-one owned-two)
run docker info
DOCKER_CONFIG="$WORK/docker-auth" run docker push owned-two
'''
        for name in ("cp", "stat"):
            (binary / name).symlink_to(shutil.which(name))
        result = self.execute(["/bin/bash", "-c", script, "_", str(HERE), str(self.root)], env=env)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        work = Path((self.root / "work").read_text().strip())
        self.assertFalse(work.exists())
        self.assertEqual((caller / "config.json").read_bytes(), original)
        self.assertEqual(json.loads((self.root / "empty.json").read_text()), {"auths": {}})
        auth = json.loads((self.root / "auth.json").read_text())
        self.assertEqual(base64.b64decode(auth["auths"]["localhost:12345"]["auth"]), b"e2euser:e2epass")
        self.assertEqual((self.root / "modes").read_text().split(), ["700", "700", "600"])
        calls = (self.root / "docker-trace").read_text().splitlines()
        self.assertEqual(calls, [f"{work}/docker|info", f"{work}/docker-auth|push owned-two",
                                 f"{work}/docker|image rm owned-one", f"{work}/docker|image rm owned-two"])

    def test_owned_proc_record_survives_concurrent_state_changes(self):
        # The old per-line reads can tear PPid while proc regenerates a changing
        # record. A single captured record must retain direct-child ownership.
        script = r'''
set -euo pipefail
source "$1/common.sh"
python3 - "$2/ready" <<'PY' &
import ctypes, pathlib, sys, time
libc = ctypes.CDLL(None)
pathlib.Path(sys.argv[1]).touch()
while True:
    libc.prctl(15, b'a', 0, 0, 0)
    time.sleep(.0005)
    libc.prctl(15, b'long-child-name', 0, 0, 0)
    time.sleep(.0005)
PY
child=$!
trap 'kill -TERM "$child" 2>/dev/null || true; wait "$child" 2>/dev/null || true' EXIT
for _ in $(seq 1 200); do
    [ ! -e "$2/ready" ] || break
    sleep .01
done
[ -e "$2/ready" ]
for _ in $(seq 1 300); do owned_alive "$child" || exit 51; done
'''
        result = self.execute(["/bin/bash", "-c", script, "_", str(HERE), str(self.root)])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_shell_traps_preserve_status_stop_services_and_repeat_with_keep(self):
        script = r'''
set -euo pipefail
source "$1/common.sh"
init_work
printf '%s\n' "$WORK" >"$2/work"
start_owned service_pid 0 python3 "$2/tree.py" "$2" wait no
while [ ! -f "$2/ready" ]; do sleep 0.01; done
printf '%s\n' "${PIDS[@]}" >"$2/supervisors"
if [ "$3" != wait ]; then exit "$3"; fi
run sleep 60
'''
        for mode, sig, keep, expected in (("0", None, "0", 0), ("23", None, "0", 23),
                                         ("wait", signal.SIGINT, "0", 130),
                                         ("wait", signal.SIGTERM, "1", 143), ("0", None, "1", 0)):
            with self.subTest(mode=mode, signal=sig, keep=keep):
                root = self.root / f"{expected}-{keep}"
                root.mkdir()
                (root / "tree.py").write_text(TREE)
                env = dict(os.environ, TMPDIR=str(root), E2E_KEEP=keep, CI="true")
                process = self.start(["/bin/bash", "-c", script, "_", str(HERE), str(root), mode], env=env)
                if sig is not None:
                    self.wait_file(root / "supervisors", process)
                    process.send_signal(sig)
                out, err = process.communicate(timeout=8)
                self.assertEqual(process.returncode, expected, out + err)
                self.assert_reaped(json.loads((root / "ready").read_text()))
                self.assert_reaped([int(pid) for pid in (root / "supervisors").read_text().split()])
                work = Path((root / "work").read_text().strip())
                self.assertEqual(work.exists(), keep == "1")


if __name__ == "__main__":
    unittest.main(verbosity=2)
