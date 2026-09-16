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

bash "$ROOT/scripts/test-release-materials.sh"
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-prepare-sandbox-init.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-go-environment.py"
bash "$ROOT/scripts/test-release-license-traversal.sh"
bash "$ROOT/scripts/test-release-cleanup.sh"
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-archive-layout.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-source-inventory.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-runtime-payloads.py" --prepare "$TMP/runtime-readers"
bash "$ROOT/scripts/test-release-go-contexts.sh"
bash "$ROOT/native-deps/deps/test-common.sh"
bash "$ROOT/scripts/test-release-native-materials.sh"
bash "$ROOT/scripts/test-release-rpm-enumeration.sh"

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
    || fail "runtime release workflow does not pin preflight and build $input checkouts"
done
grep -Fq 'run: make -C src/guest-runtime erofs envd' "$WORKFLOW" \
  || fail "runtime release must retain normal native sources and link records"
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
mkdir "$fixture_root/LICENSES"
printf 'fixture nested project notice\n' > "$fixture_root/LICENSES/NOTICE.txt"
printf 'fixture project attribution\n' > "$fixture_root/NOTICE"
install -m 0644 "$ROOT/.gitignore" "$fixture_root/.gitignore"
install -m 0644 "$ROOT/native-deps/.gitignore" "$fixture_root/native-deps/.gitignore"
install -m 0644 "$ROOT/native-deps/Makefile" "$fixture_root/native-deps/Makefile"
install -m 0755 "$ROOT/scripts/release.sh" "$fixture_root/scripts/release.sh"
install -m 0644 "$ROOT/scripts/release-runtime-payloads.py" "$fixture_root/scripts/release-runtime-payloads.py"
install -m 0644 "$ROOT/scripts/test-release-runtime-payloads.py" "$fixture_root/scripts/test-release-runtime-payloads.py"
install -m 0755 "$ROOT/scripts/publish-release.sh" "$fixture_root/scripts/publish-release.sh"
install -m 0755 "$ROOT/scripts/validate-preview-line.sh" "$fixture_root/scripts/validate-preview-line.sh"
mkdir "$fixture_root/scripts/testdata"
install -m 0644 "$ROOT/scripts/testdata/linux-COPYING" "$fixture_root/scripts/testdata/linux-COPYING"
install -m 0755 "$ROOT/scripts/release-materials.sh" \
  "$fixture_root/scripts/release-materials.sh"
mkdir -p "$fixture_root/native-deps/deps"
cp -a "$ROOT/native-deps/deps/erofs-patches" "$fixture_root/native-deps/deps/"
cat > "$fixture_root/native-deps/deps/common.sh" <<'EOF'
# Synthetic envd source fixture, not a native source-authentication result.
resolve_tarball() {
  [ "$1" = 'https://codeload.github.com/e2b-dev/runtime/tar.gz/refs/tags/2026.22#e2b-runtime-2026.22.tar.gz' ]
  [ "$2" = 8f074b23dcb2c9db48f8e674a8ab8082b54a16088fb29b66376178feeb7fe040 ]
  printf 'fixture-envd-source\n'
}
extract_tarball() {
  [ "$1" = fixture-envd-source ]
  [ ! -e "$2" ]
  mkdir -p "$2/packages/envd" "$2/packages/shared"
  printf 'module github.com/e2b-dev/infra/packages/shared\n\ngo 1.24\n' > "$2/packages/shared/go.mod"
  printf 'package shared\nfunc Fixture() {}\n' > "$2/packages/shared/shared.go"
  printf 'module github.com/e2b-dev/infra/packages/envd\n\ngo 1.24\nrequire github.com/e2b-dev/infra/packages/shared v0.0.0\nreplace github.com/e2b-dev/infra/packages/shared => ../shared\n' > "$2/packages/envd/go.mod"
  printf 'package main\nimport "github.com/e2b-dev/infra/packages/shared"\nfunc main() { shared.Fixture() }\n' > "$2/packages/envd/main.go"
  printf 'fixture envd license\n' > "$2/LICENSE"
}
EOF
install -m 0755 "$ROOT/scripts/release-native-materials.sh" \
  "$fixture_root/scripts/release-native-materials.sh"
mkdir -p "$fixture_root/cmd/flatten-ctl" "$fixture_root/cmd/other-tool"
printf 'module github.com/kuasar-sandbox/guest-runtime\n\ngo 1.24\n' > "$fixture_root/go.mod"
printf 'package main\nfunc main() {}\n' > "$fixture_root/cmd/flatten-ctl/main.go"
printf 'package main\nfunc main() {}\n' > "$fixture_root/cmd/other-tool/main.go"
printf '/bin/\n/build/\n' > "$TMP/sandboxer/.gitignore"
printf '/bin/\n/build/\n' > "$TMP/accelerator/.gitignore"
mkdir "$TMP/shared"
printf 'module github.com/e2b-dev/infra/packages/shared\n\ngo 1.24\n' > "$TMP/shared/go.mod"
printf 'package shared\nfunc Fixture() {}\n' > "$TMP/shared/shared.go"
printf 'module github.com/e2b-dev/infra/packages/envd\n\ngo 1.24\nrequire github.com/e2b-dev/infra/packages/shared v0.0.0\nreplace github.com/e2b-dev/infra/packages/shared => ../shared\n' \
  > "$TMP/src/go.mod"
printf 'package main\nimport "github.com/e2b-dev/infra/packages/shared"\nfunc main() { shared.Fixture() }\n' \
  > "$TMP/src/main.go"
(cd "$TMP/src" && GOWORK=off GOOS=linux GOARCH=amd64 GOAMD64=v1 CGO_ENABLED=0 GOEXPERIMENT='' \
  go build -trimpath -ldflags "-s -w" -o "$TMP/tool" .)
install -m 0755 "$TMP/tool" "$fixture_root/bin/x86_64/flatten-ctl"
install -m 0755 "$TMP/tool" "$TMP/sandboxer/bin/x86_64/sandbox-init"
install -m 0755 "$TMP/tool" "$fixture_root/native-deps/bin/x86_64/envd"
printf 'runtime bundle\n' > "$fixture_root/bin/x86_64/sandbox-runtime.bundle"
printf '#!/bin/sh\nexit 0\n' > "$fixture_root/native-deps/bin/x86_64/mkfs.erofs"
printf 'kernel\n' > "$fixture_root/native-deps/bin/x86_64/vmlinux"
chmod +x "$fixture_root/native-deps/bin/x86_64/mkfs.erofs"
printf 'fixture sandboxer license\n' > "$TMP/sandboxer/LICENSE"
printf 'fixture accelerator license\n' > "$TMP/accelerator/LICENSE"
for dependency in accelerator sandboxer; do
  mkdir "$TMP/$dependency/LICENSES"
  printf 'fixture dependency attribution\n' > "$TMP/$dependency/NOTICE"
  printf 'fixture nested dependency notice\n' > "$TMP/$dependency/LICENSES/NOTICE.txt"
done
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
mkdir -p "$TMP/sandboxer/cmd/sandbox-init"
printf 'module github.com/kuasar-sandbox/sandboxer\n\ngo 1.24\n' > "$TMP/sandboxer/go.mod"
printf 'package main\nfunc main() {}\n' > "$TMP/sandboxer/cmd/sandbox-init/main.go"
sandboxer_sha="$(init_fixture_repo "$TMP/sandboxer" LICENSE LICENSES NOTICE .gitignore go.mod cmd)"
accelerator_sha="$(init_fixture_repo "$TMP/accelerator" LICENSE LICENSES NOTICE .gitignore)"
git -C "$TMP/accelerator" tag v0.1.3 "$accelerator_sha"
git -C "$TMP/sandboxer" tag v0.1.3 "$sandboxer_sha"
mkdir -p "$fixture_root/native-deps/deps/vmlinux"
printf 'CONFIG_LOCALVERSION="-kuasar"\nCONFIG_LOCALVERSION_AUTO=y\n' \
  > "$fixture_root/native-deps/deps/vmlinux/sandbox-common.config"
init_fixture_repo "$fixture_root" LICENSE LICENSES NOTICE .gitignore native-deps/.gitignore \
  native-deps/deps/erofs-patches \
  native-deps/deps/vmlinux/sandbox-common.config \
  native-deps/Makefile native-deps/deps/common.sh scripts/release.sh scripts/release-materials.sh \
  scripts/release-native-materials.sh scripts/publish-release.sh \
  scripts/release-runtime-payloads.py scripts/test-release-runtime-payloads.py \
  scripts/validate-preview-line.sh scripts/testdata/linux-COPYING go.mod cmd >/dev/null
project_sha="$(git -C "$fixture_root" rev-parse HEAD)"
mkdir "$TMP/release-build-bin" "$TMP/system-inputs"
printf 'fixture libc archive\n' > "$TMP/system-inputs/libc.a"
printf 'fixture libuuid archive\n' > "$TMP/system-inputs/libuuid.a"
printf 'fixture GCC runtime archive\n' > "$TMP/system-inputs/libgcc.a"
printf 'fixture startup object\n' > "$TMP/system-inputs/crtbeginT.o"
printf 'fixture native system license\n' > "$TMP/system-inputs/LICENSE"
cat > "$TMP/release-build-bin/dpkg-query" <<'EOF'
#!/bin/sh
# These synthetic files are deliberately outside the real package database.
exit 1
EOF
cat > "$TMP/release-build-bin/rpm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  -qf)
    if [ "$2" = --dump ]; then
      printf '%s 1 0 %s 0100644 root root 0 0 0 X\n' "$3" "$(sha256sum "$3" | awk '{print $1}')"
    elif [ "$3" = '%{SOURCERPM}\n' ]; then
      printf 'fixture-system-1.0-1.src.rpm\n'
    else
      printf 'fixture-system\t1.0-1\tfixture-system-1.0-1.src.rpm\n'
    fi
    ;;
  -qa) printf 'fixture-system.x86_64\tfixture-system-1.0-1.src.rpm\n' ;;
  -ql) printf '%s/LICENSE\n' "$RELEASE_TEST_SYSTEM_INPUTS" ;;
  *) exit 1 ;;
esac
EOF
chmod 0755 "$TMP/release-build-bin/dpkg-query" "$TMP/release-build-bin/rpm"
# Build matching synthetic payloads once before exercising package/validate.
(cd "$fixture_root" && GOWORK=off CGO_ENABLED=0 go build -trimpath -buildvcs=true \
  -o bin/x86_64/flatten-ctl ./cmd/flatten-ctl)
(cd "$TMP/sandboxer" && GOWORK=off CGO_ENABLED=0 go build -trimpath -buildvcs=true \
  -o bin/x86_64/sandbox-init ./cmd/sandbox-init)
mkdir -p "$fixture_root/native-deps/build/src/e2b-infra/packages"
cp -a "$TMP/src" "$fixture_root/native-deps/build/src/e2b-infra/packages/envd"
cp -a "$TMP/shared" "$fixture_root/native-deps/build/src/e2b-infra/packages/shared"
tree="$fixture_root/build/runtime-fixture"
mkdir -p "$tree/sbin" "$tree/opt/sandbox-runtime/bin"
install -m 0755 "$TMP/sandboxer/bin/x86_64/sandbox-init" "$tree/sbin/init"
install -m 0755 "$fixture_root/native-deps/bin/x86_64/envd" "$tree/opt/sandbox-runtime/bin/envd"
install -m 0755 "$fixture_root/native-deps/bin/x86_64/mkfs.erofs" "$tree/opt/sandbox-runtime/bin/mkfs.erofs"
install -m 0755 "$fixture_root/bin/x86_64/flatten-ctl" "$tree/opt/sandbox-runtime/bin/flatten-ctl"
mkfs.erofs --all-root -T0 -U 00000000-0000-0000-0000-000000000000 \
  "$fixture_root/build/runtime.erofs" "$tree" >/dev/null
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-runtime-payloads.py" --pack \
  "$fixture_root/build/runtime.erofs" "$fixture_root/bin/x86_64/sandbox-runtime.bundle"
mkdir -p "$fixture_root/native-deps/build/x86_64/src/erofs-utils/mkfs"
for input in libc.a libuuid.a libgcc.a crtbeginT.o; do
  printf 'LOAD %s/%s\n' "$TMP/system-inputs" "$input"
done > "$fixture_root/native-deps/build/x86_64/src/erofs-utils/mkfs/mkfs.erofs.map"
install -m 0644 "$ROOT/scripts/testdata/linux-COPYING" "$fixture_root/native-deps/build/src/linux/COPYING"
cat > "$TMP/release-build-bin/make" <<'EOF'
#!/bin/sh
echo 'packaging must not rebuild prebuilt payloads' >&2
exit 1
EOF
chmod 0755 "$TMP/release-build-bin/make"
common_env=(
  PATH="$TMP/release-build-bin:$PATH"
  RELEASE_TEST_TOOL="$TMP/tool"
  RELEASE_TEST_PROJECT_SHA="$project_sha"
  RELEASE_TEST_SYSTEM_INPUTS="$TMP/system-inputs"
  RELEASE_SANDBOXER_SOURCE_SHA="$sandboxer_sha"
  RELEASE_SANDBOXER_VERSION=v0.1.3
  RELEASE_ACCELERATOR_SOURCE_SHA="$accelerator_sha"
  RELEASE_ACCELERATOR_VERSION=v0.1.3
  EROFS_TARBALL=https://codeload.github.com/erofs/erofs-utils/tar.gz/refs/tags/v1.9.1#erofs-utils-v1.9.1.tar.gz
  EROFS_TARBALL_SHA256=a9ef5ab67c4b8d2d3e9ed71f39cd008bda653142a720d8a395a36f1110d0c432
  ENVD_TARBALL=https://codeload.github.com/e2b-dev/runtime/tar.gz/refs/tags/2026.22#e2b-runtime-2026.22.tar.gz
  ENVD_TARBALL_SHA256=8f074b23dcb2c9db48f8e674a8ab8082b54a16088fb29b66376178feeb7fe040
  LINUX_TARBALL=https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.169.tar.gz
  LINUX_TARBALL_SHA256=ab28b4ca2a2eca38b3da9aa33b231288168c3560bbc866359045f1c8f4d48d94
  SOURCE_DATE_EPOCH=1700000000
)

env "${common_env[@]}" "$fixture_root/scripts/release.sh" package \
  runtime runtime-v1.2.3-preview.20260804 x86_64 "$TMP/runtime-bundle"
env "${common_env[@]}" "$fixture_root/scripts/release.sh" package \
  vmlinux vmlinux-v2.3.4 x86_64 "$TMP/vmlinux-bundle"
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-runtime-binding.py" \
  "$fixture_root" "$TMP/runtime-bundle" "$TMP/runtime-readers/runtime-verifier"
tar -xOzf "$TMP/runtime-bundle/assets/sandbox-runtime-x86_64-v1.2.3-preview.20260804.tar.gz" \
  ./bin/sandbox-runtime.bundle | cmp "$fixture_root/bin/x86_64/sandbox-runtime.bundle" -
tar -xOzf "$TMP/vmlinux-bundle/assets/vmlinux-x86_64-v2.3.4.tar.gz" \
  ./bin/vmlinux | cmp "$fixture_root/native-deps/bin/x86_64/vmlinux" -
grep -Fxq 'CONFIG_LOCALVERSION_AUTO=y' "$fixture_root/native-deps/deps/vmlinux/sandbox-common.config" \
  || fail "release packaging changed the caller's Kernel config"
"$fixture_root/scripts/release.sh" validate \
  runtime runtime-v1.2.3-preview.20260804 x86_64 "$TMP/runtime-bundle"
"$fixture_root/scripts/release.sh" validate \
  vmlinux vmlinux-v2.3.4 x86_64 "$TMP/vmlinux-bundle"
RELEASE_ACCELERATOR_SOURCE_DIR="$TMP/absent-accelerator" \
RELEASE_SANDBOXER_SOURCE_DIR="$TMP/absent-sandboxer" \
  "$fixture_root/scripts/release.sh" validate \
  vmlinux vmlinux-v2.3.4 x86_64 "$TMP/vmlinux-bundle"

RELEASE_KIND=runtime RELEASE_DEPENDENCIES=accelerator=v0.1.3,sandboxer=v0.1.3 bash "$ROOT/scripts/test-publisher.sh" \
  "$fixture_root/scripts/publish-release.sh" "$TMP/runtime-bundle" \
  kuasar-sandbox/guest-runtime runtime-v1.2.3-preview.20260804 \
  "$project_sha" main
RELEASE_KIND=vmlinux bash "$ROOT/scripts/test-publisher.sh" \
  "$fixture_root/scripts/publish-release.sh" "$TMP/vmlinux-bundle" \
  kuasar-sandbox/guest-runtime vmlinux-v2.3.4 \
  "$project_sha" main
RELEASE_KIND=vmlinux bash "$ROOT/scripts/test-publisher.sh" \
  "$fixture_root/scripts/publish-release.sh" "$TMP/vmlinux-bundle" \
  kuasar-sandbox/guest-runtime vmlinux-v2.3.4 \
  "$project_sha" release/v2.3.x

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
  ./share/sources/runtime/EROFS-INPUTS.tsv \
  ./share/sources/runtime/erofs-patches/series \
  ./share/sources/runtime/erofs-patches/0001-optional-disk-chunk-indexes.patch \
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
candidate="$TMP/linux-copying-bytes"
cp -a "$TMP/vmlinux-bundle" "$candidate"
mkdir "$candidate/root"
tar --same-permissions -xzf "$vmlinux_archive" -C "$candidate/root"
copying="$candidate/root/share/licenses/vmlinux/linux/COPYING"
printf 'unselected notice bytes\n' >> "$copying"
release_materials_hash_tree "$candidate/root" vmlinux \
  "$candidate/root/share/sources/vmlinux/MATERIALS.sha256"
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1700000000 \
  -czf "$candidate/assets/$(basename "$vmlinux_archive")" -C "$candidate/root" .
(cd "$candidate/assets" && sha256sum "$(basename "$vmlinux_archive")" > SHA256SUMS)
if "$fixture_root/scripts/release.sh" validate vmlinux vmlinux-v2.3.4 x86_64 "$candidate" > "$candidate/result.log" 2>&1; then
  fail "validator accepted changed Linux COPYING with regenerated checksums"
fi
grep -Fq 'missing or inconsistent source record for guest-runtime-kernel-inputs' "$candidate/result.log" \
  || fail "Linux COPYING mutation failed for an unrelated reason"

for input in libc.a libuuid.a libgcc.a crtbeginT.o; do
  candidate="$TMP/omitted-native-$input"
  cp -a "$TMP/runtime-bundle" "$candidate"
  mkdir "$candidate/root"
  tar --same-permissions -xzf "$runtime_archive" -C "$candidate/root"
  table="$candidate/root/share/sources/runtime/SOURCES.tsv"
  awk -F '\t' -v name="system:$input" '$2 != name' "$table" > "$candidate/changed-sources"
  install -m 0644 "$candidate/changed-sources" "$table"
  mv "$candidate/root/share/licenses/runtime/system/$input" "$candidate/removed-notices"
  release_materials_hash_tree "$candidate/root" runtime "$candidate/root/share/sources/runtime/MATERIALS.sha256"
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1700000000 \
    -czf "$candidate/assets/$(basename "$runtime_archive")" -C "$candidate/root" .
  (cd "$candidate/assets" && sha256sum "$(basename "$runtime_archive")" > SHA256SUMS)
  if "$fixture_root/scripts/release.sh" validate runtime runtime-v1.2.3-preview.20260804 x86_64 "$candidate" > "$candidate/result.log" 2>&1; then
    fail "validator accepted omitted native input and notices: $input"
  fi
  grep -Fq 'EROFS source records omit or alter collected linker inputs' "$candidate/result.log" \
    || fail "omitted native input failed for an unrelated reason"
done
for input in libgcc.a crtbeginT.o; do
  candidate="$TMP/redirected-native-$input"
  cp -a "$TMP/runtime-bundle" "$candidate"
  mkdir "$candidate/root"
  tar --same-permissions -xzf "$runtime_archive" -C "$candidate/root"
  table="$candidate/root/share/sources/runtime/SOURCES.tsv"
  awk -F '\t' -v OFS='\t' -v name="system:$input" \
    '$2 == name {$6="share/licenses/runtime/system/libc.a"} {print}' "$table" > "$candidate/changed-sources"
  install -m 0644 "$candidate/changed-sources" "$table"
  mv "$candidate/root/share/licenses/runtime/system/$input" "$candidate/removed-notices"
  release_materials_hash_tree "$candidate/root" runtime "$candidate/root/share/sources/runtime/MATERIALS.sha256"
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1700000000 \
    -czf "$candidate/assets/$(basename "$runtime_archive")" -C "$candidate/root" .
  (cd "$candidate/assets" && sha256sum "$(basename "$runtime_archive")" > SHA256SUMS)
  if "$fixture_root/scripts/release.sh" validate runtime runtime-v1.2.3-preview.20260804 x86_64 "$candidate" > "$candidate/result.log" 2>&1; then
    fail "validator accepted another input's notices for $input"
  fi
  grep -Fq 'unrecognized or inconsistent source inventory record' "$candidate/result.log" \
    || { sed -n '1,$p' "$candidate/result.log" >&2; fail "redirected native notices failed for an unrelated reason"; }
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
env "${common_env[@]}" \
  RELEASE_BIN_DIR="$fixture_root/bin/x86_64" \
  RELEASE_NATIVE_BIN_DIR="$fixture_root/native-deps/bin/x86_64" \
  RELEASE_ENVD_SOURCE_DIR="$fixture_root/native-deps/build/src/e2b-infra" \
  "$fixture_root/scripts/release.sh" package runtime \
    runtime-v1.2.3 x86_64 "$TMP/overridden-envd-source"

for kind in runtime vmlinux; do
  if [ "$kind" = runtime ]; then version=runtime-v1.2.3-preview.20260804; else version=vmlinux-v2.3.4; fi
  for extra_path in root/ etc/ bin/extra/ share/licenses/foreign/ bin/vmlinux bin/sandbox-runtime.bundle bin/other-tool; do
    if { [ "$kind" = vmlinux ] && [ "$extra_path" = bin/vmlinux ]; } \
      || { [ "$kind" = runtime ] && [ "$extra_path" = bin/sandbox-runtime.bundle ]; }; then continue; fi
    candidate="$(mktemp -d "$TMP/extra-layout-$kind.XXXXXX")"
    cp -a "$TMP/$kind-bundle/." "$candidate"
    mkdir "$candidate/root"
    candidate_archive="$("$fixture_root/scripts/release.sh" archive-name "$kind" "$version" x86_64)"
    tar --same-permissions -xzf "$candidate/assets/$candidate_archive" -C "$candidate/root"
    case "$extra_path" in
      */) mkdir -p "$candidate/root/$extra_path" ;;
      *) printf 'foreign payload must not be installed\n' > "$candidate/root/$extra_path" ;;
    esac
    release_materials_hash_tree "$candidate/root" "$kind" \
      "$candidate/root/share/sources/$kind/MATERIALS.sha256"
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1700000000 \
      -czf "$candidate/assets/$candidate_archive" -C "$candidate/root" .
    (cd "$candidate/assets" && sha256sum "$candidate_archive" > SHA256SUMS)
    if "$fixture_root/scripts/release.sh" validate "$kind" "$version" x86_64 "$candidate" > "$candidate/result.log" 2>&1; then
      fail "validator accepted $kind extra layout entry $extra_path with regenerated checksums"
    fi
    grep -Fq 'outside the exact' "$candidate/result.log" \
      || fail "extra layout entry failed for an unrelated reason"
  done
  if SOURCE_SHA=0000000000000000000000000000000000000000 \
    "$fixture_root/scripts/release.sh" validate "$kind" "$version" x86_64 "$TMP/$kind-bundle" >/dev/null 2>&1; then
    fail "validator accepted $kind from another selected source commit"
  fi
  SOURCE_SHA="$project_sha" "$fixture_root/scripts/release.sh" validate "$kind" "$version" x86_64 "$TMP/$kind-bundle"
done
for dependency in accelerator sandboxer; do
  for mutation in source integrity; do
    candidate="$TMP/dependency-$dependency-$mutation"
    cp -a "$TMP/runtime-bundle" "$candidate"
    mkdir "$candidate/root"
    candidate_archive="$(basename "$runtime_archive")"
    tar --same-permissions -xzf "$runtime_archive" -C "$candidate/root"
    case "$mutation" in
      source|integrity)
        column=4; [ "$mutation" != integrity ] || column=5
        awk -F '\t' -v OFS='\t' -v dependency="$dependency" -v column="$column" \
          '$2 == dependency { $column = (column == 4 ? "https://example.invalid/unselected-source" : "git:0000000000000000000000000000000000000000") } {print}' \
          "$candidate/root/share/sources/runtime/SOURCES.tsv" > "$candidate/changed-sources"
        install -m 0644 "$candidate/changed-sources" "$candidate/root/share/sources/runtime/SOURCES.tsv"
        ;;
    esac
    release_materials_hash_tree "$candidate/root" runtime \
      "$candidate/root/share/sources/runtime/MATERIALS.sha256"
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1700000000 \
      -czf "$candidate/assets/$candidate_archive" -C "$candidate/root" .
    (cd "$candidate/assets" && sha256sum "$candidate_archive" > SHA256SUMS)
    if "$fixture_root/scripts/release.sh" validate runtime runtime-v1.2.3-preview.20260804 \
      x86_64 "$candidate" > "$candidate/result.log" 2>&1; then
      fail "validator accepted $dependency $mutation with regenerated checksums"
    fi
    expected='source record'
    grep -Fq "$expected" "$candidate/result.log" \
      || fail "$dependency $mutation failed for an unrelated reason"
  done
done

git clone --quiet --no-local "$fixture_root" "$TMP/target-source"
for target in darwin/amd64 linux/arm64; do
  (cd "$TMP/target-source" && GOWORK=off CGO_ENABLED=0 GOOS="${target%/*}" GOARCH="${target#*/}" \
    go build -buildvcs=true -o "$TMP/target-${target//\//-}" ./cmd/flatten-ctl)
done
(cd "$TMP/target-source" && GOWORK=off CGO_ENABLED=0 \
  go build -buildvcs=true -o "$TMP/other-main" ./cmd/other-tool)
(cd "$TMP/target-source" && GOWORK=off CGO_ENABLED=0 \
  go build -buildvcs=false -o "$TMP/unstamped" ./cmd/flatten-ctl)
printf '// fixture dirty source\n' >> "$TMP/target-source/cmd/flatten-ctl/main.go"
(cd "$TMP/target-source" && GOWORK=off CGO_ENABLED=0 \
  go build -buildvcs=true -o "$TMP/dirty-source" ./cmd/flatten-ctl)
for mutation in wrong-os wrong-arch other-main other-module unstamped dirty-source; do
  candidate="$TMP/flatten-$mutation"
  cp -a "$TMP/runtime-bundle" "$candidate"
  mkdir "$candidate/root"
  tar --same-permissions -xzf "$runtime_archive" -C "$candidate/root"
  payload="$TMP/$mutation"
  case "$mutation" in
    wrong-os) payload="$TMP/target-darwin-amd64" ;;
    wrong-arch) payload="$TMP/target-linux-arm64" ;;
    other-module) payload="$TMP/tool" ;;
  esac
  install -m 0755 "$payload" "$candidate/root/bin/flatten-ctl"
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1700000000 \
    -czf "$candidate/assets/$(basename "$runtime_archive")" -C "$candidate/root" .
  (cd "$candidate/assets" && sha256sum "$(basename "$runtime_archive")" > SHA256SUMS)
  if "$fixture_root/scripts/release.sh" validate runtime runtime-v1.2.3-preview.20260804 \
    x86_64 "$candidate" > "$candidate/result.log" 2>&1; then
    fail "validator accepted $mutation flatten-ctl with regenerated checksums"
  fi
  case "$mutation" in
    wrong-*) expected='must target linux/amd64' ;;
    other-*) expected='must use the selected guest-runtime module and main package' ;;
    unstamped) expected='must bind its full selected source commit' ;;
    dirty-source) expected='must be built from the clean selected commit' ;;
  esac
  grep -Fq "$expected" "$candidate/result.log" || fail "$mutation failed for an unrelated reason"
done

for kind in runtime vmlinux; do
  if [ "$kind" = runtime ]; then
    version=runtime-v1.2.3-preview.20260804
    payload=bin/flatten-ctl
  else
    version=vmlinux-v2.3.4
    payload=bin/vmlinux
  fi
  archive_name="$("$fixture_root/scripts/release.sh" archive-name "$kind" "$version" x86_64)"
  for mutation in unknown-source uninventoried-system; do
    candidate="$TMP/$kind-$mutation"
    cp -a "$TMP/$kind-bundle" "$candidate"
    mkdir "$candidate/root"
    tar --same-permissions -xzf "$candidate/assets/$archive_name" -C "$candidate/root"
    inventory="$candidate/root/share/sources/$kind/SOURCES.tsv"
    if [ "$mutation" = unknown-source ]; then
      printf '%s\tfabricated-source\t1.2.3\thttps://example.invalid/source\tsha256:%064d\tshare/licenses/%s/project\n' \
        "$payload" 0 "$kind" >> "$inventory"
    else
      label="share/licenses/$kind/system/uninventoried.a"
      install -d -m 0755 "$candidate/root/$label"
      install -m 0644 "$candidate/root/share/licenses/$kind/project/LICENSE" "$candidate/root/$label/LICENSE"
      native_payload=bin/vmlinux
      if [ "$kind" = runtime ]; then
        native_payload=bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs
      fi
      printf '%s\tsystem:uninventoried.a\t1.2.3\tdeb-source:fixture@1.2.3\tsha256:%064d;package:fixture\t%s\n' \
        "$native_payload" 0 "$label" >> "$inventory"
    fi
    release_materials_hash_tree "$candidate/root" "$kind" "$candidate/root/share/sources/$kind/MATERIALS.sha256"
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1700000000 \
      -czf "$candidate/assets/$archive_name" -C "$candidate/root" .
    (cd "$candidate/assets" && sha256sum "$archive_name" > SHA256SUMS)
    if "$fixture_root/scripts/release.sh" validate "$kind" "$version" x86_64 "$candidate" \
      > "$candidate/result.log" 2>&1; then
      fail "validator accepted $kind $mutation with regenerated checksums"
    fi
    expected="unrecognized or inconsistent source inventory record"
    if [ "$kind:$mutation" = runtime:uninventoried-system ]; then
      expected="EROFS source records omit or alter collected linker inputs"
    fi
    grep -Fq "$expected" "$candidate/result.log" \
      || { sed -n '1,$p' "$candidate/result.log" >&2; fail "$kind $mutation failed for an unrelated reason"; }
  done
done

for kind in runtime vmlinux; do
  if [ "$kind" = runtime ]; then
    version=runtime-v1.2.3-preview.20260804
    names=(guest-runtime sandboxer accelerator envd erofs-utils guest-runtime-erofs-patches github.com/e2b-dev/infra/packages/shared system:libc.a system:libuuid.a)
  else
    version=vmlinux-v2.3.4
    names=(guest-runtime-kernel-inputs linux)
  fi
  archive_name="$("$fixture_root/scripts/release.sh" archive-name "$kind" "$version" x86_64)"
  for name in "${names[@]}"; do
    candidate="$TMP/$kind-license-${name//\//-}"
    cp -a "$TMP/$kind-bundle" "$candidate"
    mkdir "$candidate/root"
    tar --same-permissions -xzf "$candidate/assets/$archive_name" -C "$candidate/root"
    inventory="$candidate/root/share/sources/$kind/SOURCES.tsv"
    label="share/licenses/$kind/project"
    case "$name" in
      guest-runtime) label="share/licenses/runtime/envd" ;;
      guest-runtime-kernel-inputs) label="share/licenses/vmlinux/linux" ;;
    esac
    awk -F '\t' -v OFS='\t' -v name="$name" -v label="$label" \
      '$2 == name {$6=label} {print}' "$inventory" > "$candidate/changed.tsv"
    install -m 0644 "$candidate/changed.tsv" "$inventory"
    release_materials_hash_tree "$candidate/root" "$kind" \
      "$candidate/root/share/sources/$kind/MATERIALS.sha256"
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1700000000 \
      -czf "$candidate/assets/$archive_name" -C "$candidate/root" .
    (cd "$candidate/assets" && sha256sum "$archive_name" > SHA256SUMS)
    if "$fixture_root/scripts/release.sh" validate "$kind" "$version" x86_64 "$candidate" \
      > "$candidate/result.log" 2>&1; then
      fail "validator accepted $name bound to another license directory"
    fi
    case "$name" in
      system:*) expected="unrecognized or inconsistent source inventory record" ;;
      github.com/e2b-dev/infra/packages/shared|erofs-utils|guest-runtime-erofs-patches) expected="missing or inconsistent source record for $name" ;;
      *) expected="unclaimed release license material" ;;
    esac
    grep -Fq "$expected" "$candidate/result.log" \
      || { sed -n '1,$p' "$candidate/result.log" >&2; fail "$name license binding failed for an unrelated reason"; }
  done
done

for kind in runtime vmlinux; do
  if [ "$kind" = runtime ]; then
    version=runtime-v1.2.3-preview.20260804
    names=(envd erofs-utils)
  else
    version=vmlinux-v2.3.4
    names=(linux)
  fi
  archive_name="$("$fixture_root/scripts/release.sh" archive-name "$kind" "$version" x86_64)"
  for mutation in owner source integrity setuid setgid writable-directory writable-payload; do
    for name in "${names[@]}"; do
      candidate="$TMP/$kind-$mutation-$name"
      cp -a "$TMP/$kind-bundle" "$candidate"
      mkdir "$candidate/root"
      tar --same-permissions -xzf "$candidate/assets/$archive_name" -C "$candidate/root"
      owner=0
      if [ "$mutation" = owner ]; then
        owner=1234
      elif [[ "$mutation" == setuid || "$mutation" == setgid || "$mutation" == writable-* ]]; then
        payload=bin/vmlinux
        [ "$kind" != runtime ] || payload=bin/flatten-ctl
        case "$mutation" in
          setuid) chmod 4755 "$candidate/root/$payload" ;;
          setgid) chmod 2755 "$candidate/root/$payload" ;;
          writable-directory) chmod 0777 "$candidate/root/bin" ;;
          writable-payload) chmod 0777 "$candidate/root/$payload" ;;
        esac
      else
        column=4
        [ "$mutation" != integrity ] || column=5
        inventory="$candidate/root/share/sources/$kind/SOURCES.tsv"
        awk -F '\t' -v OFS='\t' -v name="$name" -v column="$column" \
          '$2 == name {$column="https://example.invalid/not-the-selected-source"} {print}' \
          "$inventory" > "$candidate/changed.tsv"
        install -m 0644 "$candidate/changed.tsv" "$inventory"
        release_materials_hash_tree "$candidate/root" "$kind" \
          "$candidate/root/share/sources/$kind/MATERIALS.sha256"
      fi
      tar --sort=name --owner="$owner" --group=0 --numeric-owner --mtime=@1700000000 \
        -czf "$candidate/assets/$archive_name" -C "$candidate/root" .
      (cd "$candidate/assets" && sha256sum "$archive_name" > SHA256SUMS)
      if "$fixture_root/scripts/release.sh" validate "$kind" "$version" x86_64 "$candidate" >/dev/null 2>&1; then
        fail "validator accepted $kind $name with changed $mutation and regenerated checksums"
      fi
    done
  done
done

# Standalone validation must not need source checkouts, module downloads or a build.
mkdir -p "$TMP/standalone-tools" "$TMP/standalone-bin"
cp -a "$fixture_root/scripts" "$TMP/standalone-tools/scripts"
for command in git curl wget cargo make gcc; do
  printf '#!/bin/sh\nexit 97\n' > "$TMP/standalone-bin/$command"
  chmod 0755 "$TMP/standalone-bin/$command"
done
env PATH="$TMP/standalone-bin:$PATH" GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
  SOURCE_SHA="$project_sha" "$TMP/standalone-tools/scripts/release.sh" validate \
  runtime runtime-v1.2.3-preview.20260804 x86_64 "$TMP/runtime-bundle"
env PATH="$TMP/standalone-bin:$PATH" GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
  SOURCE_SHA="$project_sha" "$TMP/standalone-tools/scripts/release.sh" validate \
  vmlinux vmlinux-v2.3.4 x86_64 "$TMP/vmlinux-bundle"
echo "test-release: standalone validation without checkouts/downloads/build PASS"

echo "test-release: PASS"
