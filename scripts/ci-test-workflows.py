#!/usr/bin/env python3
"""Offline release/PR workflow and Runtime ABI migration contracts."""
import importlib.util
from pathlib import Path
import re
import subprocess
import sys
from unittest.mock import patch

import yaml

ROOT = Path(__file__).resolve().parents[1]
sys.dont_write_bytecode = True


def check():
    pins = set()
    workflows = {}
    profiles = {
        "release-runtime.yml": {"preflight": "release-control", "build": "runtime", "publish": "runtime-publish", "cleanup": "control"},
        "release-vmlinux.yml": {"preflight": "release-control", "build": "kernel", "publish": "release-control", "cleanup": "control"},
        "reconcile-latest.yml": {"reconcile": "control"},
        "delete-preview.yml": {"delete": "release-control"},
    }
    for filename, expected in profiles.items():
        text = (ROOT / ".github/workflows" / filename).read_text()
        jobs = yaml.safe_load(text)["jobs"]
        workflows[filename] = jobs
        assert jobs.keys() == expected.keys(), (filename, jobs.keys())
        for name, job in jobs.items():
            assert job["runs-on"] == "ubuntu-24.04", (filename, name)
            assert job["permissions"].get("contents", "read") == ("write" if name in ("publish", "reconcile", "delete") else "read")
            steps = {step["name"]: step for step in job["steps"]}
            checkout = steps["Check out pinned hosted bootstrap"]["with"]
            assert checkout["repository"] == "kuasar-sandbox/kuasar-sandbox"
            assert re.fullmatch(r"[0-9a-f]{40}", checkout["ref"]), "bootstrap rollout needs a full platform commit SHA"
            pins.add(checkout["ref"])
            assert checkout["persist-credentials"] is False
            assert "token" not in checkout, "public tooling must not need a cross-repository App token"
            bootstrap = steps["Bootstrap standard runner"]
            assert bootstrap["run"] == f"bash trusted/platform/ci/hosted/bootstrap.sh --profile {expected[name]}"
            if name == "build":
                assert list(steps).index("Bootstrap standard runner") < list(steps).index("Check out exact guest-runtime source")
            for step in job["steps"]:
                assert "actions/cache@" not in step.get("uses", "")
                if "actions/checkout@" in step.get("uses", ""):
                    assert step["with"]["persist-credentials"] is False
                if "actions/upload-artifact@" in step.get("uses", ""):
                    assert step["with"]["path"] == "src/guest-runtime/release-bundle"
                    assert step["with"]["retention-days"] == 1
                if "Install pinned GitHub CLI" == step["name"]:
                    assert list(steps).index("Bootstrap standard runner") < list(steps).index(step["name"])
        for forbidden in ("self-hosted", "kuasar-control", "kuasar-e2e", "/var/cache/kuasar", "/var/lib/kuasar-ci", "goproxy.cn", "tsinghua.edu.cn", "GOTOOLCHAIN: local"):
            assert forbidden not in text, (filename, forbidden)

    pr = yaml.safe_load((ROOT / ".github/workflows/integration-tests.yml").read_text())
    uses = pr["jobs"]["ci"]["uses"]
    assert uses.startswith("kuasar-sandbox/kuasar-sandbox/.github/workflows/ci-entry.yml@")
    assert uses.endswith("@main"), "retain the shared current-main PR authority"
    assert len(pins) == 1, "release bootstrap checkouts must share one immutable rollout"
    pin = pins.pop()
    assert re.fullmatch(r"[0-9a-f]{40}", pin)
    if len(sys.argv) > 1:
        # Optional local rollout verification, without fetching or changing refs.
        platform = Path(sys.argv[1]).resolve()
        for path in ("ci/hosted/bootstrap.sh", ".github/workflows/ci-entry.yml", ".github/workflows/integration-tests.yml"):
            subprocess.run(["git", "-C", str(platform), "cat-file", "-e", f"{pin}:{path}"], check=True)

    runtime = {s["name"]: s for s in workflows["release-runtime.yml"]["build"]["steps"]}
    names = list(runtime)
    assert names.index("Bootstrap standard runner") < names.index("Create read-only source token")
    assert names.index("Revoke source token before code execution") < names.index("Verify and install the selected sandbox-init")
    assert runtime["Check out trusted build checks"]["with"]["ref"] == "${{ github.sha }}"
    native = runtime["Build runtime native inputs and retain their material sources"]
    assert "make -C src/guest-runtime erofs envd" in native["run"]
    assert native["shell"] == "bash"
    assert 'taskset -pc "$KUASAR_BUILD_CPUS" "$$"' in native["run"]
    assert "ci/native-cache" not in native["run"]
    for goal in ("test", "vet", "flatten-ctl sandbox-runtime"):
        assert f"make -C src/guest-runtime {goal}" in runtime["Build and test runtime image"]["run"]
    abi = runtime["Check the released Runtime ABI"]["run"]
    assert "trusted/guest-runtime/scripts/ci-check-runtime-abi.py" in abi
    assert abi.count("--static ") == 4
    assert "src/sandboxer/bin/x86_64/sandbox-init" in abi
    assert names.index("Check the released Runtime ABI") < names.index("Package runtime release")
    kernel = workflows["release-vmlinux.yml"]["build"]
    assert all("create-github-app-token" not in s.get("uses", "") for s in kernel["steps"])
    kernel_steps = {s["name"]: s for s in kernel["steps"]}
    assert "trusted/platform/ci/native-cache/native-cache.sh restore-or-build vmlinux" in kernel_steps["Restore or build guest kernel"]["run"]
    assert kernel_steps["Validate native dependency scripts"]["run"] == "make -C src/guest-runtime/native-deps test"
    print("guest hosted workflows: runners, profiles, pins, trust boundary and native material preservation PASS")


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
