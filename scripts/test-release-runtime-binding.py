"""Repack real EROFS fixtures and exercise the complete archive validator."""
import hashlib
import importlib.util
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("runtime_fixture", ROOT / "scripts/test-release-runtime-payloads.py")
FIXTURE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(FIXTURE)


def digest(path):
    with path.open("rb") as data:
        return hashlib.file_digest(data, "sha256").hexdigest()


def run(source, original, readers):
    archive_name = "sandbox-runtime-x86_64-v1.2.3-preview.20260804.tar.gz"
    with tempfile.TemporaryDirectory(prefix="runtime-binding-") as temporary:
        work = Path(temporary)
        base = work / "base"
        base.mkdir()
        subprocess.run(["tar", "-xzf", str(original / "assets" / archive_name), "-C", str(base)], check=True)
        FIXTURE.READER.read_payloads(base / "bin/sandbox-runtime.bundle", readers / "bin/fsck.erofs",
                                     readers / "build/src/erofs-utils/dump/dump.erofs", work / "payloads")
        marker = work / "must-not-execute"
        mutations = (
            ("arbitrary-bundle", "offset-zero EROFS"),
            ("prefix-tamper", "prefix digest differs"),
            ("missing-init", "Runtime embedded payload verification failed"),
            ("linked-init", "Runtime embedded payload verification failed"),
            ("wrong-mode", "mode 0755"),
            ("swapped-init", "Go payload must be built from the clean selected commit"),
            ("non-go-envd", "embedded"),
            ("different-mkfs", "embedded mkfs.erofs differs"),
            ("forged-envd-record", "Go build records differ from embedded Runtime payloads"),
            ("corrupt-erofs", "Runtime embedded payload verification failed"),
        )
        for mutation, expected in mutations:
            candidate = work / mutation
            shutil.copytree(original, candidate)
            tree = candidate / "root"
            shutil.copytree(base, tree)
            image = tree / "bin/sandbox-runtime.bundle"
            if mutation == "arbitrary-bundle":
                image.write_bytes(bytes(2 << 20))
            elif mutation == "prefix-tamper":
                with image.open("r+b") as data:
                    data.seek(4096)
                    value = data.read(1)
                    data.seek(4096)
                    data.write(bytes([value[0] ^ 1]))
            elif mutation == "forged-envd-record":
                with (tree / "share/sources/runtime/GO-BUILD-INFO.tsv").open("a") as data:
                    data.write("bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd\tbuild-setting\t-tags\tfixture-forged\t-\n")
            else:
                guest = candidate / "guest"
                for label, path in FIXTURE.READER.PAYLOADS.items():
                    target = guest / path.lstrip("/")
                    target.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copyfile(work / "payloads" / label, target)
                    target.chmod(0o755)
                init = guest / "sbin/init"
                envd = guest / "opt/sandbox-runtime/bin/envd"
                if mutation == "missing-init":
                    init.unlink()
                elif mutation == "linked-init":
                    init.unlink()
                    init.symlink_to("/opt/sandbox-runtime/bin/envd")
                elif mutation == "wrong-mode":
                    init.chmod(0o644)
                elif mutation == "swapped-init":
                    shutil.copyfile(envd, init)
                elif mutation in ("non-go-envd", "different-mkfs"):
                    target = envd if mutation == "non-go-envd" else guest / "opt/sandbox-runtime/bin/mkfs.erofs"
                    target.write_text("#!/bin/sh\ntouch " + str(marker) + "\n")
                erofs = candidate / "raw.erofs"
                subprocess.run([str(readers / "bin/mkfs.erofs"), "--all-root", "-T0", "-U",
                                "00000000-0000-0000-0000-000000000000", str(erofs), str(guest)],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True, timeout=30)
                if mutation == "corrupt-erofs":
                    # Retain the superblock/magic but remove actual inode/data
                    # blocks, then create a valid new outer prefix marker.
                    with erofs.open("r+b") as data:
                        data.truncate(4096)
                FIXTURE.make_bundle(erofs, image)
            source_files = sorted((path for parent in (tree / "share/licenses/runtime", tree / "share/sources/runtime")
                                   for path in parent.rglob("*") if path.is_file()
                                   and path != tree / "share/sources/runtime/MATERIALS.sha256"),
                                  key=lambda path: str(path.relative_to(tree)))
            (tree / "share/sources/runtime/MATERIALS.sha256").write_text("".join(
                digest(path) + "  " + str(path.relative_to(tree)) + "\n" for path in source_files))
            archive = candidate / "assets" / archive_name
            subprocess.run(["tar", "--sort=name", "--owner=0", "--group=0", "--numeric-owner",
                            "-czf", str(archive), "-C", str(tree), "."], check=True)
            (candidate / "assets/SHA256SUMS").write_text(digest(archive) + "  " + archive_name + "\n")
            result = subprocess.run(["bash", str(source / "scripts/release.sh"), "validate", "runtime",
                                     "runtime-v1.2.3-preview.20260804", "x86_64", str(candidate)],
                                    text=True, capture_output=True, timeout=180)
            if result.returncode == 0 or expected not in result.stderr:
                # Fixture-only diagnostics, without propagating arbitrary binary bytes.
                raise AssertionError(f"{mutation}: expected rejection {expected!r}; exit={result.returncode}; stderr={result.stderr}")
            if marker.exists():
                raise AssertionError("an embedded fixture executable was run")
            print("test-runtime-binding: rejected " + mutation, flush=True)
        print("test-runtime-binding: 10 rechecksummed EROFS/payload mutations rejected; no payload execution")


if __name__ == "__main__":
    if len(sys.argv) != 4:
        raise SystemExit("expected private fixture source, bundle and trusted readers")
    run(*(Path(argument).resolve() for argument in sys.argv[1:]))
