#!/usr/bin/env bash

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() {
  echo "test-release: $*" >&2
  exit 1
}

bash "$ROOT/scripts/test-preview-line.sh"
bash "$ROOT/scripts/test-delete-preview.sh"

mkdir -p "$TMP/source-bin"
cat > "$TMP/source-bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "${1:-}" = api ] || exit 2
printf '%s\n' "${FAKE_SOURCE_SHA:?}"
EOF
chmod +x "$TMP/source-bin/gh"
for request in \
  'runtime runtime-v1.2.3 release/v1.2.x' \
  'vmlinux vmlinux-v2.3.4 release/v2.3.x'
do
  read -r unit tag source_ref <<< "$request"
  PATH="$TMP/source-bin:$PATH" GITHUB_REPOSITORY=kuasar-sandbox/guest-runtime \
    FAKE_SOURCE_SHA=1111111111111111111111111111111111111111 \
    bash "$ROOT/scripts/validate-release-source.sh" "$source_ref" \
      1111111111111111111111111111111111111111 "$tag" "$unit" >/dev/null
done
if PATH="$TMP/source-bin:$PATH" GITHUB_REPOSITORY=kuasar-sandbox/guest-runtime \
  FAKE_SOURCE_SHA=1111111111111111111111111111111111111111 \
  bash "$ROOT/scripts/validate-release-source.sh" release/v1.2.x \
    1111111111111111111111111111111111111111 runtime-v1.3.0 runtime >/dev/null 2>&1; then
  fail "release source validator accepted a tag from another version line"
fi
bash -n "$ROOT/scripts/delete-preview.sh" "$ROOT/scripts/validate-release-source.sh"

WORKFLOW="$ROOT/.github/workflows/release-runtime.yml"
for input in accelerator_version connector_version sandboxer_version; do
  grep -Fq "      $input:" "$WORKFLOW" \
    || fail "runtime release workflow is missing required $input input"
  [ "$(grep -Fc "ref: \${{ needs.preflight.outputs.$input }}" "$WORKFLOW")" -eq 2 ] \
    || fail "runtime release workflow does not pin both $input checkouts"
done
grep -Fq "repos/kuasar-sandbox/\$repository/releases/tags/\$version" "$WORKFLOW" \
  || fail "runtime release workflow does not verify dependency releases"

[ "$(git -C "$ROOT" ls-files -s -- test/e2e/run_all.sh | awk '{print $1}')" = 100755 ] \
  || fail "test/e2e/run_all.sh is not executable in the Git index"

mkdir -p "$TMP/bin" "$TMP/native-bin" "$TMP/src"
printf 'package main\nfunc main() {}\n' > "$TMP/src/main.go"
GO111MODULE=off go build -o "$TMP/bin/flatten-ctl" "$TMP/src/main.go"
printf 'runtime bundle\n' > "$TMP/bin/sandbox-runtime.bundle"
printf '#!/bin/sh\nexit 0\n' > "$TMP/native-bin/mkfs.erofs"
printf 'kernel\n' > "$TMP/native-bin/vmlinux"
chmod +x "$TMP/native-bin/mkfs.erofs"

common_env=(
  RELEASE_BIN_DIR="$TMP/bin"
  RELEASE_NATIVE_BIN_DIR="$TMP/native-bin"
  SOURCE_DATE_EPOCH=1700000000
)

env "${common_env[@]}" "$ROOT/scripts/release.sh" package \
  runtime runtime-v1.2.3-preview.20260804 x86_64 "$TMP/runtime-bundle"
env "${common_env[@]}" "$ROOT/scripts/release.sh" package \
  vmlinux vmlinux-v2.3.4 x86_64 "$TMP/vmlinux-bundle"
"$ROOT/scripts/release.sh" validate \
  runtime runtime-v1.2.3-preview.20260804 x86_64 "$TMP/runtime-bundle"
"$ROOT/scripts/release.sh" validate \
  vmlinux vmlinux-v2.3.4 x86_64 "$TMP/vmlinux-bundle"

RELEASE_KIND=runtime bash "$ROOT/scripts/test-publisher.sh" \
  "$ROOT/scripts/publish-release.sh" "$TMP/runtime-bundle" \
  kuasar-sandbox/guest-runtime runtime-v1.2.3-preview.20260804 \
  1111111111111111111111111111111111111111 main
RELEASE_KIND=vmlinux bash "$ROOT/scripts/test-publisher.sh" \
  "$ROOT/scripts/publish-release.sh" "$TMP/vmlinux-bundle" \
  kuasar-sandbox/guest-runtime vmlinux-v2.3.4 \
  2222222222222222222222222222222222222222 main
RELEASE_KIND=vmlinux bash "$ROOT/scripts/test-publisher.sh" \
  "$ROOT/scripts/publish-release.sh" "$TMP/vmlinux-bundle" \
  kuasar-sandbox/guest-runtime vmlinux-v2.3.4 \
  2222222222222222222222222222222222222222 release/v2.3.x

runtime_archive="$TMP/runtime-bundle/assets/sandbox-runtime-x86_64-v1.2.3-preview.20260804.tar.gz"
for path in ./bin/sandbox-runtime.bundle ./bin/flatten-ctl ./bin/mkfs.erofs; do
  tar -tzf "$runtime_archive" | grep -Fx "$path" >/dev/null \
    || fail "runtime archive is missing $path"
done
vmlinux_archive="$TMP/vmlinux-bundle/assets/vmlinux-x86_64-v2.3.4.tar.gz"
for path in ./bin/vmlinux; do
  tar -tzf "$vmlinux_archive" | grep -Fx "$path" >/dev/null \
    || fail "vmlinux archive is missing $path"
done
if tar -tzf "$runtime_archive" | grep -E '^\./(docs|test/e2e)(/|$)' >/dev/null \
  || tar -tzf "$vmlinux_archive" | grep -E '^\./(docs|test/e2e)(/|$)' >/dev/null; then
  fail "component archive contains documentation or E2E sources"
fi
if tar -tzf "$runtime_archive" | grep -E 'sandbox-runtime-x86_64-|(^|/)release/' >/dev/null; then
  fail "runtime archive contains a duplicate versioned payload or release metadata"
fi
if tar -tzf "$vmlinux_archive" | grep -E 'vmlinux-x86_64-|(^|/)release/' >/dev/null; then
  fail "vmlinux archive contains a duplicate versioned payload or release metadata"
fi

env "${common_env[@]}" "$ROOT/scripts/release.sh" package \
  runtime runtime-v1.2.3-preview.20260804 x86_64 "$TMP/runtime-reproducible"
env "${common_env[@]}" "$ROOT/scripts/release.sh" package \
  vmlinux vmlinux-v2.3.4 x86_64 "$TMP/vmlinux-reproducible"
cmp -s "$runtime_archive" \
  "$TMP/runtime-reproducible/assets/sandbox-runtime-x86_64-v1.2.3-preview.20260804.tar.gz" \
  || fail "identical runtime inputs did not produce an identical archive"
cmp -s "$vmlinux_archive" \
  "$TMP/vmlinux-reproducible/assets/vmlinux-x86_64-v2.3.4.tar.gz" \
  || fail "identical vmlinux inputs did not produce an identical archive"

cp -a "$TMP/runtime-bundle" "$TMP/tampered"
printf 'tampered\n' >> \
  "$TMP/tampered/assets/sandbox-runtime-x86_64-v1.2.3-preview.20260804.tar.gz"
if "$ROOT/scripts/release.sh" validate runtime runtime-v1.2.3-preview.20260804 \
  x86_64 "$TMP/tampered" >/dev/null 2>&1; then
  fail "validator accepted a tampered runtime archive"
fi
cp -a "$TMP/vmlinux-bundle" "$TMP/extra"
touch "$TMP/extra/assets/release.json"
if "$ROOT/scripts/release.sh" validate vmlinux vmlinux-v2.3.4 \
  x86_64 "$TMP/extra" >/dev/null 2>&1; then
  fail "validator accepted an extra asset"
fi
if env "${common_env[@]}" "$ROOT/scripts/release.sh" package \
  runtime v1.2.3 x86_64 "$TMP/invalid-version" >/dev/null 2>&1; then
  fail "packager accepted a runtime version without the runtime prefix"
fi
if env "${common_env[@]}" "$ROOT/scripts/release.sh" package \
  vmlinux vmlinux-v1.2.3 aarch64 "$TMP/invalid-arch" >/dev/null 2>&1; then
  fail "packager accepted an unvalidated release architecture"
fi

echo "test-release: PASS"
