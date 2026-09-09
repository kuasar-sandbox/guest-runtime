#!/usr/bin/env bash

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() {
  echo "test-release: $*" >&2
  exit 1
}

# shellcheck source=scripts/release-materials.sh
source "$ROOT/scripts/release-materials.sh"

init_fixture_repo() {
  local directory="$1"
  shift
  git -C "$directory" init -q
  git -C "$directory" config --local user.name "Chen Xiaohui"
  git -C "$directory" config --local user.email "graych@gmail.com"
  git -C "$directory" add -- "$@"
  git -C "$directory" commit -q -m "test: create release source fixture"
  git -C "$directory" rev-parse HEAD
}

mkdir -p "$TMP/git-source" \
  "$TMP/material-hash/share/licenses/hash-test/LICENSES" \
  "$TMP/material-hash/share/sources/hash-test"
printf 'fixture license\n' > "$TMP/git-source/LICENSE"
fixture_git_sha="$(init_fixture_repo "$TMP/git-source" LICENSE)"
[ "$(release_materials_git_version "$TMP/git-source" v1.2.3 "$fixture_git_sha")" = "git:$fixture_git_sha" ] \
  || fail "untagged source was recorded as a component release"
git -C "$TMP/git-source" tag v1.2.3 "$fixture_git_sha"
[ "$(release_materials_git_version "$TMP/git-source" v1.2.3 "$fixture_git_sha")" = v1.2.3 ] \
  || fail "matching source tag was not retained"
if (release_materials_git_version "$TMP/git-source" v1.2.3 \
  0000000000000000000000000000000000000000 >/dev/null 2>&1); then
  fail "source version resolver accepted a tag for another commit"
fi
[ "$(release_materials_resolve_git_source "$TMP/git-source" "$fixture_git_sha" fixture)" = "$fixture_git_sha" ] \
  || fail "clean source worktree did not resolve to its selected commit"
if (release_materials_resolve_git_source "$TMP/git-source" \
  0000000000000000000000000000000000000000 fixture >/dev/null 2>&1); then
  fail "source resolver accepted a commit that differs from the selected commit"
fi
printf 'untracked source\n' > "$TMP/git-source/untracked.go"
if (release_materials_resolve_git_source "$TMP/git-source" "" fixture >/dev/null 2>&1); then
  fail "source resolver accepted a dirty source worktree"
fi
printf 'LICENSE.generated\n' > "$TMP/git-source/.git/info/exclude"
printf 'ignored material\n' > "$TMP/git-source/LICENSE.generated"
if (
  release_materials_init "$TMP/ignored-material/stage" "$TMP/ignored-material/work" fixture
  release_materials_copy_licenses "$TMP/git-source" project >/dev/null 2>&1
); then
  fail "license collection accepted material absent from the selected commit"
fi
printf 'nested license manifest\n' \
  > "$TMP/material-hash/share/licenses/hash-test/LICENSES/MATERIALS.sha256"
printf 'generated inventory\n' \
  > "$TMP/material-hash/share/sources/hash-test/MATERIALS.sha256"
release_materials_hash_tree "$TMP/material-hash" hash-test "$TMP/material-hash-actual"
grep -Fq 'share/licenses/hash-test/LICENSES/MATERIALS.sha256' "$TMP/material-hash-actual" \
  || fail "license file named MATERIALS.sha256 was omitted from the material inventory"
if grep -Fq 'share/sources/hash-test/MATERIALS.sha256' "$TMP/material-hash-actual"; then
  fail "generated material inventory included itself"
fi

material_root="$TMP/material-validation"
material_unit=validation
mkdir -p "$material_root/bin"
printf 'payload\n' > "$material_root/bin/tool"
mkdir -p "$material_root/share/licenses/$material_unit/project" \
  "$material_root/share/sources/$material_unit" "$TMP/material-validation-work"
printf 'fixture license\n' > "$material_root/share/licenses/$material_unit/project/LICENSE"
{
  printf 'payload\tname\tversion\tsource\tintegrity\tlicense_directory\n'
  printf 'bin/tool\tfixture\tv1.0.0\thttps://example.invalid/source.tar.gz\tsha256:fixture\tshare/licenses/%s/project\n' \
    "$material_unit"
} > "$material_root/share/sources/$material_unit/SOURCES.tsv"
printf 'payload\trecord\tname\tversion_or_value\tchecksum\n' \
  > "$material_root/share/sources/$material_unit/GO-BUILD-INFO.tsv"
printf 'module\tversion\tchecksum\n' \
  > "$material_root/share/sources/$material_unit/GO-MODULES.tsv"
release_materials_hash_tree "$material_root" "$material_unit" \
  "$material_root/share/sources/$material_unit/MATERIALS.sha256"
find "$material_root/share" -type d -exec chmod 0755 {} +
find "$material_root/share" -type f -exec chmod 0644 {} +
(
  WORK="$TMP/material-validation-work"
  release_materials_validate "$material_root" "$material_unit"
)

cp -a "$material_root" "$TMP/material-unsafe-license-path"
{
  printf 'payload\tname\tversion\tsource\tintegrity\tlicense_directory\n'
  printf 'bin/tool\tfixture\tv1.0.0\thttps://example.invalid/source.tar.gz\tsha256:fixture\t../../../etc\n'
} > "$TMP/material-unsafe-license-path/share/sources/$material_unit/SOURCES.tsv"
release_materials_hash_tree "$TMP/material-unsafe-license-path" "$material_unit" \
  "$TMP/material-unsafe-license-path/share/sources/$material_unit/MATERIALS.sha256"
chmod 0644 "$TMP/material-unsafe-license-path/share/sources/$material_unit/MATERIALS.sha256"
if (
  WORK="$TMP/material-validation-work"
  release_materials_validate "$TMP/material-unsafe-license-path" "$material_unit" \
    >/dev/null 2>&1
); then
  fail "release material validator accepted a license directory outside its unit"
fi

cp -a "$material_root" "$TMP/material-invalid-record"
{
  printf 'payload\tname\tversion\tsource\tintegrity\tlicense_directory\n'
  printf 'bin/tool\tfixture\tv1.0.0\thttps://example.invalid/source.tar.gz\tsha256:fixture\n'
} > "$TMP/material-invalid-record/share/sources/$material_unit/SOURCES.tsv"
release_materials_hash_tree "$TMP/material-invalid-record" "$material_unit" \
  "$TMP/material-invalid-record/share/sources/$material_unit/MATERIALS.sha256"
chmod 0644 "$TMP/material-invalid-record/share/sources/$material_unit/MATERIALS.sha256"
if (
  WORK="$TMP/material-validation-work"
  release_materials_validate "$TMP/material-invalid-record" "$material_unit" \
    >/dev/null 2>&1
); then
  fail "release material validator accepted a SOURCES.tsv row with fewer than six fields"
fi

cp -a "$material_root" "$TMP/material-unsafe-parent-mode"
chmod 0777 "$TMP/material-unsafe-parent-mode/share"
if (
  WORK="$TMP/material-validation-work"
  release_materials_validate "$TMP/material-unsafe-parent-mode" "$material_unit" \
    >/dev/null 2>&1
); then
  fail "release material validator accepted an unsafe parent directory mode"
fi

cp -a "$material_root" "$TMP/material-empty-license"
rm "$TMP/material-empty-license/share/licenses/$material_unit/project/LICENSE"
release_materials_hash_tree "$TMP/material-empty-license" "$material_unit" \
  "$TMP/material-empty-license/share/sources/$material_unit/MATERIALS.sha256"
if (
  WORK="$TMP/material-validation-work"
  release_materials_validate "$TMP/material-empty-license" "$material_unit" >/dev/null 2>&1
); then
  fail "release material validator accepted an empty license directory"
fi
if (
  release_materials_require_source "$material_root" "$material_unit" bin/tool fixture v2.0.0 \
    >/dev/null 2>&1
); then
  fail "release material validator accepted a different release version"
fi
if (
  release_materials_require_go "$material_root" "$material_unit" bin/tool >/dev/null 2>&1
); then
  fail "release material validator accepted missing Go build records"
fi
cp -a "$material_root" "$TMP/material-missing-payload"
rm "$TMP/material-missing-payload/bin/tool"
if (
  WORK="$TMP/material-validation-work"
  release_materials_validate "$TMP/material-missing-payload" "$material_unit" >/dev/null 2>&1
); then
  fail "release material validator accepted a record for an unshipped payload"
fi

bash "$ROOT/scripts/test-preview-line.sh"
bash "$ROOT/scripts/test-delete-preview.sh"

mkdir -p "$TMP/source-bin"
cat > "$TMP/source-bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "${1:-}" = api ] || exit 2
[ "${2:-}" = "repos/$GITHUB_REPOSITORY/git/ref/heads/${FAKE_SOURCE_REF:?}" ] || exit 2
printf '%s\n' "${FAKE_SOURCE_SHA:?}"
EOF
chmod +x "$TMP/source-bin/gh"
for request in \
  'runtime runtime-v1.2.3 release/v1.2.x' \
  'vmlinux vmlinux-v2.3.4 release/v2.3.x'
do
  read -r unit tag source_ref <<< "$request"
  PATH="$TMP/source-bin:$PATH" GITHUB_REPOSITORY=kuasar-sandbox/guest-runtime \
    FAKE_SOURCE_REF="$source_ref" \
    FAKE_SOURCE_SHA=1111111111111111111111111111111111111111 \
    bash "$ROOT/scripts/validate-release-source.sh" "$source_ref" \
      1111111111111111111111111111111111111111 "$tag" "$unit" >/dev/null
done
if PATH="$TMP/source-bin:$PATH" GITHUB_REPOSITORY=kuasar-sandbox/guest-runtime \
  FAKE_SOURCE_REF=release/v1.2.x \
  FAKE_SOURCE_SHA=1111111111111111111111111111111111111111 \
  bash "$ROOT/scripts/validate-release-source.sh" release/v1.2.x \
    1111111111111111111111111111111111111111 runtime-v1.3.0 runtime >/dev/null 2>&1; then
  fail "release source validator accepted a tag from another version line"
fi
bash -n "$ROOT/scripts/delete-preview.sh" "$ROOT/scripts/validate-release-source.sh"

WORKFLOW="$ROOT/.github/workflows/release-runtime.yml"
grep -Fqx 'run-name: Release ${{ inputs.version }} @${{ inputs.source_sha }} [accelerator=${{ inputs.accelerator_version }},sandboxer=${{ inputs.sandboxer_version }}]' \
  "$WORKFLOW" || fail "runtime release identity does not pin source and dependencies"
grep -Fqx 'run-name: Release ${{ inputs.version }} @${{ inputs.source_sha }}' \
  "$ROOT/.github/workflows/release-vmlinux.yml" \
  || fail "vmlinux release identity does not pin source_sha"
sed -n '/- name: Publish runtime release/,/run: |/p' "$WORKFLOW" \
  | grep -Fq 'RELEASE_DEPENDENCIES: accelerator=${{ needs.preflight.outputs.accelerator_version }},sandboxer=${{ needs.preflight.outputs.sandboxer_version }}' \
  || fail "runtime Preview publish step does not receive dependency binding"
grep -Fq 'kuasar-preview-binding' "$ROOT/scripts/publish-release.sh" \
  || fail "Preview publisher does not record its build binding"
for input in accelerator_version sandboxer_version; do
  grep -Fq "      $input:" "$WORKFLOW" \
    || fail "runtime release workflow is missing required $input input"
  [ "$(grep -Fc "ref: \${{ needs.preflight.outputs.$input }}" "$WORKFLOW")" -eq 2 ] \
    || fail "runtime release workflow does not pin both $input checkouts"
done
if grep -Fq 'connector_version' "$WORKFLOW" \
  || grep -Fq 'src/connector' "$WORKFLOW"; then
  fail "runtime release workflow retains connector outside its payload/build closure"
fi
grep -Fq 'repositories: accelerator,guest-runtime,kuasar-sandbox,sandboxer' \
  "$WORKFLOW" \
  || fail "runtime source token repository set does not match the minimal closure"
grep -Fq "repos/kuasar-sandbox/\$repository/releases/tags/\$version" "$WORKFLOW" \
  || fail "runtime release workflow does not verify dependency releases"
for workflow in release-runtime.yml release-vmlinux.yml delete-preview.yml; do
  [ "$(grep -Fc 'group: component-mutation-${{ github.repository }}-${{ inputs.version }}' \
    "$ROOT/.github/workflows/$workflow")" -eq 1 ] \
    || fail "$workflow does not hold exactly one full-workflow mutation lock"
done
grep -Fq 'kuasar-release-source' "$ROOT/scripts/publish-release.sh" \
  || fail "publisher does not record Stable source provenance"
grep -Fq 'reconcile_main_latest' "$ROOT/scripts/publish-release.sh" \
  || fail "publisher does not reconcile component main Latest by source commit"
RECONCILE_WORKFLOW="$ROOT/.github/workflows/reconcile-latest.yml"
grep -Fq 'group: component-latest-reconciliation-${{ github.repository }}' \
  "$RECONCILE_WORKFLOW" \
  || fail "Latest reconciliation is not serialized across component versions"
grep -Fq 'workflow_run:' "$RECONCILE_WORKFLOW" \
  || fail "Latest reconciliation is not triggered after release completion"
grep -Fq 'schedule:' "$RECONCILE_WORKFLOW" \
  || fail "Latest reconciliation has no automatic recovery schedule"
grep -Fq 'publish-release.sh reconcile' "$RECONCILE_WORKFLOW" \
  || fail "Latest reconciliation does not use the idempotent entrypoint"
if grep -R -Fq 'queue: max' "$ROOT/.github/workflows"; then
  fail "workflows use the unsupported concurrency queue key"
fi
grep -Fq 'PYTHONPYCACHEPREFIX="$(abspath $(BUILD_DIR)/python-cache)"' \
  "$ROOT/Makefile" \
  || fail "make test writes Python bytecode outside the ignored build tree"

[ "$(git -C "$ROOT" ls-files -s -- test/e2e/run_all.sh | awk '{print $1}')" = 100755 ] \
  || fail "test/e2e/run_all.sh is not executable in the Git index"

fixture_root="$TMP/guest-runtime"
mkdir -p "$TMP/src" "$TMP/sandboxer/bin/x86_64" "$TMP/accelerator" \
  "$fixture_root/scripts" "$fixture_root/bin/x86_64" \
  "$fixture_root/native-deps/bin/x86_64" \
  "$fixture_root/native-deps/build/src/e2b-infra" \
  "$fixture_root/native-deps/build/x86_64/src/erofs-utils" \
  "$fixture_root/native-deps/build/src/linux/LICENSES/preferred"
install -m 0644 "$ROOT/LICENSE" "$fixture_root/LICENSE"
install -m 0644 "$ROOT/.gitignore" "$fixture_root/.gitignore"
install -m 0644 "$ROOT/native-deps/.gitignore" "$fixture_root/native-deps/.gitignore"
install -m 0644 "$ROOT/native-deps/Makefile" "$fixture_root/native-deps/Makefile"
install -m 0755 "$ROOT/scripts/release.sh" "$fixture_root/scripts/release.sh"
install -m 0755 "$ROOT/scripts/release-materials.sh" \
  "$fixture_root/scripts/release-materials.sh"
printf '/bin/\n/build/\n' > "$TMP/sandboxer/.gitignore"
printf '/bin/\n/build/\n' > "$TMP/accelerator/.gitignore"
printf 'package main\nfunc main() {}\n' > "$TMP/src/main.go"
GO111MODULE=off go build -o "$TMP/tool" "$TMP/src/main.go"
install -m 0755 "$TMP/tool" "$fixture_root/bin/x86_64/flatten-ctl"
install -m 0755 "$TMP/tool" "$TMP/sandboxer/bin/x86_64/sandbox-init"
install -m 0755 "$TMP/tool" "$fixture_root/native-deps/bin/x86_64/envd"
printf 'runtime bundle\n' > "$fixture_root/bin/x86_64/sandbox-runtime.bundle"
printf '#!/bin/sh\nexit 0\n' > "$fixture_root/native-deps/bin/x86_64/mkfs.erofs"
printf 'kernel\n' > "$fixture_root/native-deps/bin/x86_64/vmlinux"
chmod +x "$fixture_root/native-deps/bin/x86_64/mkfs.erofs"
printf 'fixture sandboxer license\n' > "$TMP/sandboxer/LICENSE"
printf 'fixture accelerator license\n' > "$TMP/accelerator/LICENSE"
printf 'fixture envd license\n' \
  > "$fixture_root/native-deps/build/src/e2b-infra/LICENSE"
printf 'fixture erofs authors\n' \
  > "$fixture_root/native-deps/build/x86_64/src/erofs-utils/AUTHORS"
printf 'fixture erofs license\n' \
  > "$fixture_root/native-deps/build/x86_64/src/erofs-utils/COPYING"
printf 'fixture Linux license\n' \
  > "$fixture_root/native-deps/build/src/linux/COPYING"
printf 'fixture GPL-2.0 text\n' \
  > "$fixture_root/native-deps/build/src/linux/LICENSES/preferred/GPL-2.0"
sandboxer_sha="$(init_fixture_repo "$TMP/sandboxer" LICENSE .gitignore)"
accelerator_sha="$(init_fixture_repo "$TMP/accelerator" LICENSE .gitignore)"
init_fixture_repo "$fixture_root" LICENSE .gitignore native-deps/.gitignore \
  native-deps/Makefile scripts/release.sh scripts/release-materials.sh >/dev/null

mkdir "$TMP/release-build-bin"
cat > "$TMP/release-build-bin/make" <<'EOF'
#!/usr/bin/env bash
# Synthetic payloads test the packaging contract; real native builds are
# separately exercised by the official release assembly validation.
set -euo pipefail
while [ "$#" -gt 0 ] && [ "$1" != -C ]; do shift; done
[ "${1:-}" = -C ] && [ "$#" -ge 2 ]
root="$2"
case "$root" in
  */build/guest-runtime/native-deps)
    [ ! -e "$root/bin/x86_64/envd" ]
    mkdir -p "$root/bin/x86_64" "$root/build/src/e2b-infra" \
      "$root/build/x86_64/src/erofs-utils" "$root/build/src/linux/LICENSES/preferred"
    install -m 0755 "$RELEASE_TEST_TOOL" "$root/bin/x86_64/envd"
    printf '#!/bin/sh\nexit 0\n' > "$root/bin/x86_64/mkfs.erofs"
    chmod 0755 "$root/bin/x86_64/mkfs.erofs"
    printf 'fresh kernel\n' > "$root/bin/x86_64/vmlinux"
    printf 'fixture envd license\n' > "$root/build/src/e2b-infra/LICENSE"
    printf 'fixture erofs authors\n' > "$root/build/x86_64/src/erofs-utils/AUTHORS"
    printf 'fixture erofs license\n' > "$root/build/x86_64/src/erofs-utils/COPYING"
    printf 'fixture Linux license\n' > "$root/build/src/linux/COPYING"
    printf 'fixture GPL-2.0 text\n' > "$root/build/src/linux/LICENSES/preferred/GPL-2.0"
    ;;
  */build/guest-runtime)
    [ ! -e "$root/bin/x86_64/sandbox-runtime.bundle" ]
    mkdir -p "$root/bin/x86_64" "$root/../sandboxer/bin/x86_64"
    install -m 0755 "$RELEASE_TEST_TOOL" "$root/bin/x86_64/flatten-ctl"
    install -m 0755 "$RELEASE_TEST_TOOL" "$root/../sandboxer/bin/x86_64/sandbox-init"
    printf 'fresh runtime bundle\n' > "$root/bin/x86_64/sandbox-runtime.bundle"
    ;;
  *) echo "release build escaped its fresh workspace: $root" >&2; exit 1 ;;
esac
EOF
chmod 0755 "$TMP/release-build-bin/make"
common_env=(
  PATH="$TMP/release-build-bin:$PATH"
  RELEASE_TEST_TOOL="$TMP/tool"
  RELEASE_SANDBOXER_SOURCE_SHA="$sandboxer_sha"
  RELEASE_SANDBOXER_VERSION=v0.1.3
  RELEASE_ACCELERATOR_SOURCE_SHA="$accelerator_sha"
  RELEASE_ACCELERATOR_VERSION=v0.1.3
  EROFS_TARBALL=https://codeload.github.com/erofs/erofs-utils/tar.gz/refs/tags/v1.9.1#erofs-utils-v1.9.1.tar.gz
  EROFS_TARBALL_SHA256=a9ef5ab67c4b8d2d3e9ed71f39cd008bda653142a720d8a395a36f1110d0c432
  ENVD_TARBALL=https://codeload.github.com/e2b-dev/infra/tar.gz/refs/tags/2026.22#e2b-infra-2026.22.tar.gz
  ENVD_TARBALL_SHA256=9e1e81f2963fda1805466c337cd0a33638182a15a66295fd19bba6b9c454d92c
  LINUX_TARBALL=https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.169.tar.gz
  LINUX_TARBALL_SHA256=ab28b4ca2a2eca38b3da9aa33b231288168c3560bbc866359045f1c8f4d48d94
  SOURCE_DATE_EPOCH=1700000000
)

env "${common_env[@]}" "$fixture_root/scripts/release.sh" package \
  runtime runtime-v1.2.3-preview.20260804 x86_64 "$TMP/runtime-bundle"
env "${common_env[@]}" "$fixture_root/scripts/release.sh" package \
  vmlinux vmlinux-v2.3.4 x86_64 "$TMP/vmlinux-bundle"
tar -xOzf "$TMP/runtime-bundle/assets/sandbox-runtime-x86_64-v1.2.3-preview.20260804.tar.gz" \
  ./bin/sandbox-runtime.bundle | grep -Fxq 'fresh runtime bundle' \
  || fail "runtime packaging reused an old local output"
tar -xOzf "$TMP/vmlinux-bundle/assets/vmlinux-x86_64-v2.3.4.tar.gz" \
  ./bin/vmlinux | grep -Fxq 'fresh kernel' \
  || fail "kernel packaging reused an old local output"
grep -Fxq 'runtime bundle' "$fixture_root/bin/x86_64/sandbox-runtime.bundle" \
  || fail "release packaging changed the caller's runtime output"
grep -Fxq kernel "$fixture_root/native-deps/bin/x86_64/vmlinux" \
  || fail "release packaging changed the caller's kernel output"
"$fixture_root/scripts/release.sh" validate \
  runtime runtime-v1.2.3-preview.20260804 x86_64 "$TMP/runtime-bundle"
"$fixture_root/scripts/release.sh" validate \
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
go_toolchain="$(go version | awk '{print $3}')"
for path in ./bin/sandbox-runtime.bundle ./bin/flatten-ctl ./bin/mkfs.erofs \
  ./share/licenses/runtime/project/LICENSE \
  ./share/licenses/runtime/sandboxer/LICENSE \
  ./share/licenses/runtime/accelerator/LICENSE \
  ./share/licenses/runtime/envd/LICENSE \
  ./share/licenses/runtime/erofs-utils/COPYING \
  ./share/licenses/runtime/go-toolchain/"$go_toolchain"/LICENSE \
  ./share/sources/runtime/SOURCES.tsv \
  ./share/sources/runtime/GO-BUILD-INFO.tsv \
  ./share/sources/runtime/GO-MODULES.tsv \
  ./share/sources/runtime/MATERIALS.sha256; do
  tar -tzf "$runtime_archive" | grep -Fx "$path" >/dev/null \
    || fail "runtime archive is missing $path"
done
tar -xOf "$runtime_archive" ./share/sources/runtime/SOURCES.tsv \
  | grep -Fq $'\tGo toolchain\t'"$go_toolchain"$'\t' \
  || fail "runtime archive does not associate its Go toolchain with license material"
vmlinux_archive="$TMP/vmlinux-bundle/assets/vmlinux-x86_64-v2.3.4.tar.gz"
for path in ./bin/vmlinux \
  ./share/licenses/vmlinux/project/LICENSE \
  ./share/licenses/vmlinux/linux/COPYING \
  ./share/licenses/vmlinux/linux/LICENSES/preferred/GPL-2.0 \
  ./share/sources/vmlinux/SOURCES.tsv \
  ./share/sources/vmlinux/GO-BUILD-INFO.tsv \
  ./share/sources/vmlinux/GO-MODULES.tsv \
  ./share/sources/vmlinux/MATERIALS.sha256; do
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

env "${common_env[@]}" "$fixture_root/scripts/release.sh" package \
  runtime runtime-v1.2.3-preview.20260804 x86_64 "$TMP/runtime-reproducible"
env "${common_env[@]}" "$fixture_root/scripts/release.sh" package \
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
if "$fixture_root/scripts/release.sh" validate runtime runtime-v1.2.3-preview.20260804 \
  x86_64 "$TMP/tampered" >/dev/null 2>&1; then
  fail "validator accepted a tampered runtime archive"
fi
cp -a "$TMP/vmlinux-bundle" "$TMP/extra"
touch "$TMP/extra/assets/release.json"
if "$fixture_root/scripts/release.sh" validate vmlinux vmlinux-v2.3.4 \
  x86_64 "$TMP/extra" >/dev/null 2>&1; then
  fail "validator accepted an extra asset"
fi
if env "${common_env[@]}" "$fixture_root/scripts/release.sh" package \
  runtime v1.2.3 x86_64 "$TMP/invalid-version" >/dev/null 2>&1; then
  fail "packager accepted a runtime version without the runtime prefix"
fi
if env "${common_env[@]}" "$fixture_root/scripts/release.sh" package \
  vmlinux vmlinux-v1.2.3 aarch64 "$TMP/invalid-arch" >/dev/null 2>&1; then
  fail "packager accepted an unvalidated release architecture"
fi
if env "${common_env[@]}" \
  ENVD_TARBALL=https://example.invalid/envd.tar.gz \
  "$fixture_root/scripts/release.sh" package runtime \
    runtime-v1.2.3 x86_64 "$TMP/nondefault-envd" >/dev/null 2>&1; then
  fail "packager accepted a non-default Envd source input"
fi
if env "${common_env[@]}" \
  RELEASE_ENVD_SOURCE_DIR="$fixture_root/native-deps/build/src/e2b-infra" \
  "$fixture_root/scripts/release.sh" package runtime \
    runtime-v1.2.3 x86_64 "$TMP/overridden-envd-source" >/dev/null 2>&1; then
  fail "packager accepted an unbound Envd source directory override"
fi

echo "test-release: PASS"
