#!/usr/bin/env bash

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() {
  echo "test-release: $*" >&2
  exit 1
}

mkdir -p "$TMP/bin" "$TMP/native-bin"
printf 'runtime bundle\n' > "$TMP/bin/sandbox-runtime.bundle"
printf '#!/bin/sh\nexit 0\n' > "$TMP/bin/flatten-ctl"
printf '#!/bin/sh\nexit 0\n' > "$TMP/native-bin/mkfs.erofs"
printf 'kernel\n' > "$TMP/native-bin/vmlinux"
chmod +x "$TMP/bin/flatten-ctl" "$TMP/native-bin/mkfs.erofs"

{
  printf 'repository\trequested_ref\tresolved_sha\trole\n'
  printf 'kuasar-sandbox/accelerator\tmain\t%s\tdependency\n' \
    1111111111111111111111111111111111111111
  printf 'kuasar-sandbox/connector\tmain\t%s\tdependency\n' \
    2222222222222222222222222222222222222222
  printf 'kuasar-sandbox/guest-runtime\tmain\t%s\tprimary\n' \
    3333333333333333333333333333333333333333
  printf 'kuasar-sandbox/sandboxer\tv1.2.0\t%s\tdependency\n' \
    4444444444444444444444444444444444444444
} > "$TMP/runtime-revisions.tsv"

printf 'repository\trequested_ref\tresolved_sha\trole\n' > "$TMP/vmlinux-revisions.tsv"
printf 'kuasar-sandbox/guest-runtime\tmain\t%s\tprimary\n' \
  3333333333333333333333333333333333333333 >> "$TMP/vmlinux-revisions.tsv"

common_env=(
  RELEASE_BIN_DIR="$TMP/bin"
  RELEASE_NATIVE_BIN_DIR="$TMP/native-bin"
  SOURCE_DATE_EPOCH=1700000000
  RELEASE_WORKFLOW_REPOSITORY=kuasar-sandbox/guest-runtime
  RELEASE_WORKFLOW_RUN_ID=123
  RELEASE_WORKFLOW_RUN_ATTEMPT=1
  RELEASE_WORKFLOW_RUN_URL=https://github.com/kuasar-sandbox/guest-runtime/actions/runs/123
)

env "${common_env[@]}" "$ROOT/scripts/release.sh" package \
  runtime runtime-v1.2.3 x86_64 "$TMP/runtime-revisions.tsv" "$TMP/runtime-bundle"
env "${common_env[@]}" "$ROOT/scripts/release.sh" package \
  vmlinux vmlinux-v2.3.4 x86_64 "$TMP/vmlinux-revisions.tsv" "$TMP/vmlinux-bundle"
"$ROOT/scripts/release.sh" validate "$TMP/runtime-bundle"
"$ROOT/scripts/release.sh" validate "$TMP/vmlinux-bundle"

runtime_archive="$TMP/runtime-bundle/assets/sandbox-runtime-x86_64-runtime-v1.2.3.tar.gz"
for path in \
  ./bin/sandbox-runtime.bundle \
  ./bin/sandbox-runtime-x86_64-runtime-v1.2.3.bundle \
  ./bin/flatten-ctl \
  ./bin/mkfs.erofs \
  ./release/runtime.json; do
  tar -tzf "$runtime_archive" | grep -Fx "$path" >/dev/null \
    || fail "runtime archive is missing $path"
done

vmlinux_archive="$TMP/vmlinux-bundle/assets/vmlinux-x86_64-vmlinux-v2.3.4.tar.gz"
for path in ./bin/vmlinux ./bin/vmlinux-x86_64-vmlinux-v2.3.4 ./release/vmlinux.json; do
  tar -tzf "$vmlinux_archive" | grep -Fx "$path" >/dev/null \
    || fail "vmlinux archive is missing $path"
done

cp -a "$TMP/runtime-bundle" "$TMP/tampered"
printf 'tampered\n' >> "$TMP/tampered/assets/sandbox-runtime-x86_64-runtime-v1.2.3.tar.gz"
if "$ROOT/scripts/release.sh" validate "$TMP/tampered" >/dev/null 2>&1; then
  fail "validator accepted a tampered runtime archive"
fi

if env "${common_env[@]}" "$ROOT/scripts/release.sh" package \
  runtime v1.2.3 x86_64 "$TMP/runtime-revisions.tsv" "$TMP/invalid" >/dev/null 2>&1; then
  fail "packager accepted a runtime version without the runtime prefix"
fi

cp "$TMP/runtime-revisions.tsv" "$TMP/unversioned-sandboxer.tsv"
sed -i 's/sandboxer\tv1\.2\.0/sandboxer\tmain/' "$TMP/unversioned-sandboxer.tsv"
if env "${common_env[@]}" "$ROOT/scripts/release.sh" package \
  runtime runtime-v1.2.4 x86_64 "$TMP/unversioned-sandboxer.tsv" "$TMP/unversioned" \
  >/dev/null 2>&1; then
  fail "runtime packager accepted an unversioned sandboxer source"
fi

echo "test-release: PASS"
