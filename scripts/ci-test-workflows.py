#!/usr/bin/env python3
"""Offline release/PR workflow and Runtime ABI migration contracts."""
import importlib.util
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from unittest.mock import patch

import yaml

ROOT = Path(__file__).resolve().parents[1]
sys.dont_write_bytecode = True


def check():
    workflows = {}
    for filename in ("release-runtime.yml", "release-vmlinux.yml", "reconcile-latest.yml", "delete-preview.yml"):
        text = (ROOT / ".github/workflows" / filename).read_text()
        jobs = yaml.safe_load(text)["jobs"]
        workflows[filename] = jobs
        for name, job in jobs.items():
            assert job["runs-on"] == "ubuntu-24.04", (filename, name)
            assert "github.event.repository.visibility == 'public'" in job["if"]
            assert "github.event.repository.full_name == github.repository" in job["if"]
            assert job["permissions"].get("contents", "read") == ("write" if name in ("publish", "reconcile", "delete") else "read")
            for step in job["steps"]:
                assert "create-github-app-token" not in step.get("uses", "")
                assert "actions/cache@" not in step.get("uses", "")
                if "actions/checkout@" in step.get("uses", ""):
                    assert step["with"]["persist-credentials"] is False
                if "actions/upload-artifact@" in step.get("uses", ""):
                    assert step["with"]["path"] == "src/guest-runtime/release-bundle"
                    assert "matrix.arch" in step["with"]["name"]
                    assert step["with"]["retention-days"] == 1
                if "run" in step:
                    subprocess.run(["bash", "-n"], input=step["run"], text=True, check=True)
        for forbidden in ("self-hosted", "/var/cache/kuasar", "/var/lib/kuasar-ci", "goproxy.cn", "tsinghua.edu.cn", "GOTOOLCHAIN: local"):
            assert forbidden not in text, (filename, forbidden)
        if "build" in jobs:
            assert jobs["build"]["strategy"] == {"fail-fast": False, "matrix": {"arch": ["x86_64", "aarch64"]}}
            assert jobs["build"]["env"]["TARGET_ARCH"] == "${{ matrix.arch }}"
            assert jobs["publish"]["needs"] == ["preflight", "build"]
            for stage in ("build", "publish"):
                steps = {s["name"]: s for s in jobs[stage]["steps"]}
                assert steps["Check out trusted platform tooling"]["with"]["ref"] == "${{ needs.preflight.outputs.framework_sha }}"
            publish = {s["name"]: s for s in jobs["publish"]["steps"]}
            assert "publish-release.sh assemble" in publish["Assemble the two validated architecture archives"]["run"]
            for arch in ("x86_64", "aarch64"):
                assert f"Download validated {arch} bundle" in publish
    pr = yaml.safe_load((ROOT / ".github/workflows/integration-tests.yml").read_text())
    assert pr["jobs"]["ci"]["uses"] == "kuasar-sandbox/kuasar-sandbox/.github/workflows/ci-entry.yml@main"
    runtime = {s["name"]: s for s in workflows["release-runtime.yml"]["build"]["steps"]}
    names = list(runtime)
    assert names.index("Bootstrap native or cross build") < names.index("Check out exact guest-runtime source")
    assert runtime["Check out trusted build checks"]["with"]["ref"] == "${{ github.sha }}"
    for name in ("accelerator", "sandboxer"):
        checkouts = [s for s in runtime.values() if s.get("with", {}).get("repository") == "kuasar-sandbox/" + name]
        assert len(checkouts) == 2
        assert all(s["with"]["ref"] == "${{ needs.preflight.outputs." + name + "_sha }}" for s in checkouts)
    assert '--arch "$TARGET_ARCH"' in runtime["Verify and install the selected sandbox-init"]["run"]
    native = runtime["Build runtime native inputs and retain their material sources"]
    assert "make -C src/guest-runtime erofs envd" in [line.strip() for line in native["run"].splitlines()]
    assert 'taskset -pc "$KUASAR_BUILD_CPUS" "$$"' in native["run"]
    assert "ci/native-cache" not in native["run"]
    assert "BUILD_MKFS_EROFS" in str(runtime)
    for goal in ("test", "vet", "flatten-ctl sandbox-runtime"):
        assert f"make -C src/guest-runtime {goal}" in runtime["Build and test runtime image"]["run"]
    abi = runtime["Check the released Runtime ABI"]["run"]
    assert "trusted/guest-runtime/scripts/ci-check-runtime-abi.py" in abi and abi.count("--static ") == 4
    assert "src/sandboxer/bin/$TARGET_ARCH/sandbox-init" in abi
    assert names.index("Check the released Runtime ABI") < names.index("Package runtime release")
    publish = {s["name"]: s for s in workflows["release-runtime.yml"]["publish"]["steps"]}
    objects = publish["Fetch selected source objects for material validation"]
    assert objects["env"]["SOURCE_SHA"] == "${{ needs.preflight.outputs.source_sha }}"
    assert list(publish).index("Fetch selected source objects for material validation") < list(publish).index("Assemble the two validated architecture archives")
    check_publish_source_objects(objects["run"])
    kernel = {s["name"]: s for s in workflows["release-vmlinux.yml"]["build"]["steps"]}
    assert "trusted/platform/ci/native-cache/native-cache.sh restore-or-build vmlinux" in kernel["Restore or build guest kernel"]["run"]
    assert kernel["Validate native dependency scripts"]["run"] == "make -C src/guest-runtime/native-deps test-scripts"
    print("guest workflows: public callers, dual target identity, source/material/ABI boundaries PASS")



def check_publish_source_objects(script):
    """Exercise the actual publish step against a shallow local source remote."""
    with tempfile.TemporaryDirectory(prefix="runtime-source-objects-") as directory:
        work = Path(directory)
        origin, publisher = work / "origin", work / "publisher"
        env = dict(os.environ, GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM="1")
        for name in ("GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"):
            env.pop(name, None)

        def git(path, *args):
            return subprocess.run(["git", "-C", str(path), *args], env=env,
                                  text=True, capture_output=True, check=True).stdout.strip()

        origin.mkdir()
        git(origin, "init", "-q")
        git(origin, "config", "user.name", "Runtime fixture")
        git(origin, "config", "user.email", "runtime-fixture@example.invalid")
        git(origin, "config", "commit.gpgsign", "false")
        (origin / "source.txt").write_text("selected source\n")
        git(origin, "add", "source.txt")
        git(origin, "commit", "-qm", "selected source")
        selected = git(origin, "rev-parse", "HEAD")
        (origin / "source.txt").write_text("trusted publisher\n")
        git(origin, "commit", "-qam", "publisher tooling")
        subprocess.run(["git", "clone", "-q", "--depth=1", origin.as_uri(), str(publisher)], env=env, check=True)
        before = git(publisher, "rev-parse", "HEAD")
        missing = subprocess.run(["git", "-C", str(publisher), "cat-file", "-e", selected + "^{commit}"], env=env, capture_output=True)
        assert missing.returncode != 0, "fixture must start without the selected commit"
        for source, valid in (("invalid", False), (selected, True), (selected, True)):
            result = subprocess.run(["bash", "-c", script], cwd=publisher,
                                    env=dict(env, SOURCE_SHA=source), text=True,
                                    capture_output=True, timeout=30)
            assert (result.returncode == 0) == valid, result.stderr
            assert git(publisher, "rev-parse", "HEAD") == before
            assert git(publisher, "status", "--porcelain") == ""
            assert (publisher / "source.txt").read_text() == "trusted publisher\n"
        git(publisher, "cat-file", "-e", selected + "^{commit}")
    print("Runtime publish: selected objects fetched without changing trusted checkout PASS")


def check_abi():
    spec = importlib.util.spec_from_file_location("runtime_abi", ROOT / "scripts/ci-check-runtime-abi.py")
    abi = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(abi)
    for output, static, valid in (
        ("There is no dynamic section in this file.", True, True),
        ("INTERP\nNEEDED libc.so.6\nName: GLIBC_2.38", False, True),
        ("INTERP\nName: GLIBC_2.38", True, False),
        ("(NEEDED) libc.so.6\nName: GLIBC_2.2.5", True, False),
        ("Name: GLIBC_2.39", False, False),
        ("Name: GLIBC_2.38\nName: GLIBC_2.40", False, False),
    ):
        with patch.object(abi.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, output)):
            try:
                abi.validate(Path("fixture"), static=static)
                accepted = True
            except ValueError:
                accepted = False
        assert accepted == valid, (output, static)
    with patch.object(abi.subprocess, "run", side_effect=subprocess.CalledProcessError(1, "readelf")):
        try:
            abi.validate(Path("unreadable"))
        except subprocess.CalledProcessError:
            pass
        else:
            raise AssertionError("readelf failure must fail closed")
    print("Runtime ABI: static inputs and glibc 2.38 ceiling regression PASS")


if __name__ == "__main__":
    check()
    check_abi()
