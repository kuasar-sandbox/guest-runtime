#!/usr/bin/env bash
# Real pinned native builds in two distinct directories; not a fixture or VM test.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d)"
trap 'chmod -R u+w "$test_root"; rm -rf "$test_root"' EXIT
fail() { printf 'test-erofs-reproducibility: %s\n' "$*" >&2; exit 1; }
[ "$(uname -m)" = x86_64 ] || fail "this release check requires an x86_64 host"
# Read the actual release policy, without invoking the release CLI or candidate binaries.
# shellcheck disable=SC1090
source <(sed -n '/^release_runtime_native_cflags() {/,/^}/p; /^native_make_default() {/,/^}/p' "$ROOT/scripts/release.sh")
spec="$(native_make_default EROFS_TARBALL)"
spec="${spec//\\#/\#}"
checksum="$(native_make_default EROFS_TARBALL_SHA256)"
[[ "$checksum" =~ ^[0-9a-f]{64}$ ]] || fail "the native source checksum is missing"
for iteration in first second; do
  workspace="$test_root/$iteration"
  mkdir -p "$workspace/home"
  chmod 0700 "$workspace/home"
  env -i PATH="$PATH" HOME="$workspace/home" LANG=C SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-0}" \
    CFLAGS="$(release_runtime_native_cflags "$workspace")" TARGET_ARCH=x86_64 \
    BUILD_DIR="$workspace/guest-runtime/native-deps/build/x86_64" \
    BINDIR="$workspace/guest-runtime/native-deps/bin/x86_64" \
    TARBALL_CACHE="$ROOT/native-deps/build/tarball" \
    EROFS_TARBALL="$spec" EROFS_TARBALL_SHA256="$checksum" \
    bash "$ROOT/native-deps/deps/build-erofs.sh"
done
for binary in mkfs.erofs fsck.erofs; do
  first="$test_root/first/guest-runtime/native-deps/bin/x86_64/$binary"
  second="$test_root/second/guest-runtime/native-deps/bin/x86_64/$binary"
  cmp -s "$first" "$second" || fail "$binary differs between fresh source directories"
  if LC_ALL=C grep -Fq "$test_root" "$first"; then
    fail "$binary retains the random build directory"
  fi
  printf 'test-erofs-reproducibility: %s sha256=%s identical across two fresh builds\n' \
    "$binary" "$(sha256sum "$first" | awk '{print $1}')"
done
