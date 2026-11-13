#!/usr/bin/env bash

set -euo pipefail

# This suite compiles private synthetic modules; product source gates select
# their own verified workspace outside these standalone packaging fixtures.
export GOWORK=off

# Archive fixtures target x86_64 even when this gate runs on an ARM host.
# Validators run natively; payload fixtures are inspected, never executed.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'chmod -R u+w "$TMP"; rm -rf "$TMP"' EXIT

fail() {
  echo "test-release: $*" >&2
  exit 1
}

# shellcheck source=scripts/release-materials.sh
source "$ROOT/scripts/release-materials.sh"

bash "$ROOT/scripts/test-release-materials.sh"
bash "$ROOT/scripts/test-release-go-notices.sh"
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-selected-go-materials.py"
# Cross-repository integration is opt-in; the CI source wrapper supplies its
# selected platform explicitly. Standalone packaging needs no sibling checkout.
if [[ -n "${KUASAR_PLATFORM_DIR:-${KUASAR_PLATFORM_ROOT:-}}" ]]; then
  PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-workspace.py"
else
  echo 'test-release: workspace integration not selected (set KUASAR_PLATFORM_DIR to run)'
fi
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-go-environment.py"
bash "$ROOT/scripts/test-release-license-traversal.sh"
bash "$ROOT/scripts/test-release-cleanup.sh"
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-archive-layout.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-kernel-checksum.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-runtime-payloads.py" --prepare "$TMP/runtime-readers"
bash "$ROOT/scripts/test-release-go-contexts.sh"
bash "$ROOT/native-deps/deps/test-common.sh"
python3 -B "$ROOT/scripts/test-static-crypto.py"

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

mkdir -p "$TMP/git-source"
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
# Package validation checks actual paths, permissions, notices and payloads.
material_root="$TMP/material-validation"
material_unit=validation
mkdir -p "$material_root/share/licenses/$material_unit/project" "$TMP/material-validation-work"
printf 'fixture license\n' > "$material_root/share/licenses/$material_unit/project/LICENSE"
find "$material_root/share" -type d -exec chmod 0755 {} +
find "$material_root/share" -type f -exec chmod 0644 {} +
WORK="$TMP/material-validation-work" release_materials_validate "$material_root" "$material_unit"
for mutation in parent-mode file-mode link fifo empty missing unsafe-path source-record; do
  altered="$TMP/material-$mutation"
  cp -a "$material_root" "$altered"
  license="$altered/share/licenses/$material_unit/project/LICENSE"
  case "$mutation" in
    parent-mode) chmod 0777 "$altered/share" ;;
    file-mode) chmod 0666 "$license" ;;
    link) rm "$license"; ln -s /etc/passwd "$license" ;;
    fifo) rm "$license"; mkfifo "$license" ;;
    empty) : > "$license" ;;
    missing) rm "$license" ;;
    unsafe-path) mv "$license" "${license} unsafe" ;;
    source-record) mkdir -p "$altered/share/sources/$material_unit"; printf 'retired\n' > "$altered/share/sources/$material_unit/SOURCES.tsv" ;;
  esac
  if (WORK="$TMP/material-validation-work" release_materials_validate "$altered" "$material_unit" >/dev/null 2>&1); then
    fail "license validator accepted $mutation"
  fi
done
if (WORK="$TMP/material-validation-work" release_materials_validate_go "$material_root" "$material_unit" bin/missing >/dev/null 2>&1); then
  fail "validator accepted missing Go binary"
fi

bash "$ROOT/scripts/test-preview-line.sh"
bash "$ROOT/scripts/test-delete-preview.sh"

# The component passes every input unchanged to the fixed trusted helper.
mkdir -p "$TMP/source-entry/trusted/platform/release"
cat > "$TMP/source-entry/trusted/platform/release/producer-inputs.py" <<'PYENTRY'
import os, sys
from pathlib import Path
unit = os.environ['ENTRY_UNIT']
prefix = unit + '-' if unit in ('runtime', 'vmlinux') else ''
assert sys.argv[1:] == ['admit', unit, 'release/v1.2.x', '1' * 40,
                       prefix + 'v1.2.3', os.environ['GITHUB_REPOSITORY'], 'producer-source.tar']
if os.environ.get('ENTRY_REJECT'): raise SystemExit(23)
Path('producer-source.tar').write_text('admitted fixture')
PYENTRY
(
  cd "$TMP/source-entry"
  export GITHUB_REPOSITORY=kuasar-sandbox/guest-runtime ENTRY_UNIT=runtime
  bash "$ROOT/scripts/validate-release-source.sh" release/v1.2.x 1111111111111111111111111111111111111111 runtime-v1.2.3 runtime
  [ -f producer-source.tar ] || fail "source admission was not preserved"
  rm producer-source.tar
  status=0
  ENTRY_REJECT=1 bash "$ROOT/scripts/validate-release-source.sh" release/v1.2.x 1111111111111111111111111111111111111111 runtime-v1.2.3 runtime || status=$?
  [ "$status" -eq 23 ] || fail "source admission failure was not preserved"
  [ ! -e producer-source.tar ] || fail "rejected admission produced an artifact"
)
(
  cd "$TMP/source-entry"
  export GITHUB_REPOSITORY=kuasar-sandbox/guest-runtime ENTRY_UNIT=vmlinux
  bash "$ROOT/scripts/validate-release-source.sh" release/v1.2.x 1111111111111111111111111111111111111111 vmlinux-v1.2.3 vmlinux
  [ -f producer-source.tar ] || fail "source admission was not preserved"
  rm producer-source.tar
  status=0
  ENTRY_REJECT=1 bash "$ROOT/scripts/validate-release-source.sh" release/v1.2.x 1111111111111111111111111111111111111111 vmlinux-v1.2.3 vmlinux || status=$?
  [ "$status" -eq 23 ] || fail "source admission failure was not preserved"
  [ ! -e producer-source.tar ] || fail "rejected admission produced an artifact"
)
rm "$TMP/source-entry/trusted/platform/release/producer-inputs.py"
if (cd "$TMP/source-entry"; bash "$ROOT/scripts/validate-release-source.sh" main 1111111111111111111111111111111111111111 v1.2.3 connector) >/dev/null 2>&1; then
  fail "source validator accepted a missing trusted helper"
fi
bash -n "$ROOT/scripts/delete-preview.sh" "$ROOT/scripts/validate-release-source.sh"

WORKFLOW="$ROOT/.github/workflows/release-runtime.yml"
grep -Fqx "run-name: Release \${{ inputs.version }} @\${{ inputs.source_sha }} [accelerator=\${{ inputs.accelerator_version }}\${{ inputs.sandboxer_version && format(',sandboxer={0}', inputs.sandboxer_version) || '' }}]" \
  "$WORKFLOW" || fail "runtime release identity does not pin source and dependencies"
grep -Fqx 'run-name: Release ${{ inputs.version }} @${{ inputs.source_sha }}' \
  "$ROOT/.github/workflows/release-vmlinux.yml" \
  || fail "vmlinux release identity does not pin source_sha"
for input in accelerator_version; do
  grep -Fq "      $input:" "$WORKFLOW" \
    || fail "runtime release workflow is missing required $input input"
  grep -Fq -- "--dependency ${input%_version} \"\${{ needs.preflight.outputs.$input }}\" \"\${{ needs.preflight.outputs.${input%_version}_sha }}\"" "$WORKFLOW" \
    || fail "release restore does not bind $input tag and SHA to trusted preflight outputs"
  if grep -Fq "ref: \${{ needs.preflight.outputs.${input%_version}_sha }}" "$WORKFLOW"; then
    fail "release workflow refetches a dependency by bare SHA"
  fi
done
# Validate the named native build step rather than YAML single-line formatting.
python3 "$ROOT/scripts/ci-test-workflows.py" "${KUASAR_PLATFORM_DIR:-${KUASAR_PLATFORM_ROOT:-}}" \
  || fail "release workflow contracts do not preserve native inputs and trust boundaries"
if grep -Fq 'connector_version' "$WORKFLOW" \
  || grep -Fq 'src/connector' "$WORKFLOW"; then
  fail "runtime release workflow retains connector outside its payload/build closure"
fi
if grep -Fq 'create-github-app-token' "$WORKFLOW"; then
  fail "public artifact builds must not receive the App key"
fi
grep -Fq "repos/kuasar-sandbox/\$repository/releases/tags/\$version" "$WORKFLOW" \
  || fail "runtime release workflow does not verify dependency releases"
for workflow in release-runtime.yml release-vmlinux.yml delete-preview.yml; do
  [ "$(grep -Fc 'group: component-mutation-${{ github.repository }}-${{ inputs.version }}' \
    "$ROOT/.github/workflows/$workflow")" -eq 1 ] \
    || fail "$workflow does not hold exactly one full-workflow mutation lock"
done
[ ! -e "$ROOT/.github/workflows/reconcile-latest.yml" ] || fail "retired Latest writer remains"
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-publisher.py"
grep -Fq 'PYTHONPYCACHEPREFIX="$(abspath $(BUILD_DIR)/python-cache)"' \
  "$ROOT/Makefile" \
  || fail "make test writes Python bytecode outside the ignored build tree"

for case_path in test/e2e/cases/image.flatten.sh test/e2e/cases/image.registry.sh; do
  [ "$(git -C "$ROOT" ls-files -s -- "$case_path" | awk '{print $1}')" = 100755 ] \
    || fail "$case_path is not executable in the Git index"
done
for retired in test/e2e/run_all.sh test/e2e/e2e_flatten.sh test/e2e/common.sh; do
  if git -C "$ROOT" ls-files --error-unmatch -- "$retired" >/dev/null 2>&1; then
    fail "$retired is retired and must not remain in the Git index"
  fi
done

fixture_root="$TMP/guest-runtime"
mkdir -p "$TMP/src" "$TMP/accelerator" \
  "$fixture_root/scripts" "$fixture_root/bin/x86_64" \
  "$fixture_root/native-deps/bin/x86_64" \
  "$fixture_root/native-deps/build/src/e2b-infra" \
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
install -m 0644 "$ROOT/scripts/release-publisher.py" "$fixture_root/scripts/release-publisher.py"
install -m 0755 "$ROOT/scripts/validate-preview-line.sh" "$fixture_root/scripts/validate-preview-line.sh"
mkdir "$fixture_root/scripts/testdata"
install -m 0644 "$ROOT/scripts/testdata/linux-COPYING" "$fixture_root/scripts/testdata/linux-COPYING"
install -m 0755 "$ROOT/scripts/release-materials.sh" \
  "$fixture_root/scripts/release-materials.sh"
mkdir -p "$fixture_root/native-deps/deps"
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
install -m 0644 "$ROOT/scripts/static-crypto-catalog.py" "$fixture_root/scripts/static-crypto-catalog.py"
mkdir -p "$fixture_root/cmd/flatten-ctl" "$fixture_root/cmd/other-tool"
# A private offline module fixture supplies a real external dependency record.
# It never enters the caller's module cache or any published runtime.
export GOMODCACHE="$TMP/generator-module-cache" GOPROXY="file://$TMP/generator-proxy"
export GOSUMDB=off
export GOENV=off GOPRIVATE='' GONOPROXY=none GONOSUMDB=none GOINSECURE='' GOAUTH=off
python3 - "$TMP/generator-proxy" <<'PY'
import json
from pathlib import Path
import sys
import zipfile
module, version = "github.com/kuasar-sandbox/go-erofs", "v0.0.0-test"
proxy = Path(sys.argv[1]) / module / "@v"
proxy.mkdir(parents=True)
files = {"go.mod": "module " + module + "\n\ngo 1.24\n",
         "fixture.go": "package erofs\nfunc Fixture() {}\n",
         "LICENSE": "fixture generator license\n"}
(proxy / (version + ".mod")).write_text(files["go.mod"])
(proxy / (version + ".info")).write_text(json.dumps({"Version": version, "Time": "2026-01-01T00:00:00Z"}))
with zipfile.ZipFile(proxy / (version + ".zip"), "w") as archive:
    for name, content in files.items():
        archive.writestr(module + "@" + version + "/" + name, content)
PY
printf 'module github.com/kuasar-sandbox/guest-runtime\n\ngo 1.24\nrequire github.com/kuasar-sandbox/go-erofs v0.0.0-test\n' > "$fixture_root/go.mod"
printf 'package main\nimport "github.com/kuasar-sandbox/go-erofs"\nfunc main() { erofs.Fixture() }\n' > "$fixture_root/cmd/flatten-ctl/main.go"
(cd "$fixture_root" && GOWORK=off go mod tidy)
printf 'package main\nfunc main() {}\n' > "$fixture_root/cmd/other-tool/main.go"
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
install -m 0755 "$TMP/tool" "$fixture_root/bin/x86_64/sandbox-init"
install -m 0755 "$TMP/tool" "$fixture_root/native-deps/bin/x86_64/envd"
printf 'runtime bundle\n' > "$fixture_root/bin/x86_64/sandbox-runtime.bundle"
# Native fixture headers are inspected; their programs are never executed.
install -m 0755 "$TMP/tool" "$fixture_root/native-deps/bin/x86_64/mkfs.erofs"
install -m 0644 "$TMP/tool" "$fixture_root/native-deps/bin/x86_64/vmlinux"
printf 'fixture accelerator license\n' > "$TMP/accelerator/LICENSE"
for dependency in accelerator; do
  mkdir "$TMP/$dependency/LICENSES"
  printf 'fixture dependency attribution\n' > "$TMP/$dependency/NOTICE"
  printf 'fixture nested dependency notice\n' > "$TMP/$dependency/LICENSES/NOTICE.txt"
done
printf 'fixture envd license\n' \
  > "$fixture_root/native-deps/build/src/e2b-infra/LICENSE"
printf 'fixture Linux license\n' \
  > "$fixture_root/native-deps/build/src/linux/COPYING"
printf 'fixture GPL-2.0 text\n' \
  > "$fixture_root/native-deps/build/src/linux/LICENSES/preferred/GPL-2.0"
mkdir -p "$fixture_root/cmd/sandbox-init"
printf 'package main\nfunc main() {}\n' > "$fixture_root/cmd/sandbox-init/main.go"
accelerator_sha="$(init_fixture_repo "$TMP/accelerator" LICENSE LICENSES NOTICE .gitignore)"
git -C "$TMP/accelerator" tag v0.1.3 "$accelerator_sha"
mkdir -p "$fixture_root/native-deps/deps/vmlinux"
printf 'CONFIG_LOCALVERSION="-kuasar"\nCONFIG_LOCALVERSION_AUTO=y\n' \
  > "$fixture_root/native-deps/deps/vmlinux/sandbox-common.config"
init_fixture_repo "$fixture_root" LICENSE LICENSES NOTICE .gitignore native-deps/.gitignore \
  native-deps/deps/vmlinux/sandbox-common.config \
  native-deps/Makefile native-deps/deps/common.sh scripts/release.sh scripts/release-materials.sh \
  scripts/static-crypto-catalog.py scripts/publish-release.sh scripts/release-publisher.py \
  scripts/release-runtime-payloads.py scripts/test-release-runtime-payloads.py \
  scripts/validate-preview-line.sh scripts/testdata/linux-COPYING go.mod go.sum cmd >/dev/null
project_sha="$(git -C "$fixture_root" rev-parse HEAD)"
mkdir "$TMP/release-build-bin" "$TMP/system-inputs"
printf 'fixture libc archive\n' > "$TMP/system-inputs/libc.a"
printf 'fixture libuuid archive\n' > "$TMP/system-inputs/libuuid.a"
for input in libgcrypt.a libgpg-error.a; do
  printf 'fixture crypto archive\n' > "$TMP/system-inputs/$input"
done
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
(cd "$fixture_root" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=true \
  -o bin/x86_64/flatten-ctl ./cmd/flatten-ctl)
(cd "$fixture_root" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=true \
  -o bin/x86_64/sandbox-init ./cmd/sandbox-init)
mkdir -p "$fixture_root/native-deps/build/src/e2b-infra/packages"
cp -a "$TMP/src" "$fixture_root/native-deps/build/src/e2b-infra/packages/envd"
cp -a "$TMP/shared" "$fixture_root/native-deps/build/src/e2b-infra/packages/shared"
tree="$fixture_root/build/runtime-fixture"
mkdir -p "$tree/sbin" "$tree/opt/sandbox-runtime/bin"
install -m 0755 "$fixture_root/bin/x86_64/sandbox-init" "$tree/sbin/init"
install -m 0755 "$fixture_root/native-deps/bin/x86_64/envd" "$tree/opt/sandbox-runtime/bin/envd"
install -m 0755 "$fixture_root/bin/x86_64/flatten-ctl" "$tree/opt/sandbox-runtime/bin/flatten-ctl"
mkfs.erofs --all-root -T0 -U 00000000-0000-0000-0000-000000000000 \
  "$fixture_root/build/runtime.erofs" "$tree" >/dev/null
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-runtime-payloads.py" --pack \
  "$fixture_root/build/runtime.erofs" "$fixture_root/bin/x86_64/sandbox-runtime.bundle"
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
  RELEASE_ACCELERATOR_SOURCE_SHA="$accelerator_sha"
  RELEASE_ACCELERATOR_VERSION=v0.1.3
  ENVD_TARBALL=https://codeload.github.com/e2b-dev/runtime/tar.gz/refs/tags/2026.22#e2b-runtime-2026.22.tar.gz
  ENVD_TARBALL_SHA256=8f074b23dcb2c9db48f8e674a8ab8082b54a16088fb29b66376178feeb7fe040
  LINUX_TARBALL=https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.169.tar.gz
  LINUX_TARBALL_SHA256=ab28b4ca2a2eca38b3da9aa33b231288168c3560bbc866359045f1c8f4d48d94
  SOURCE_DATE_EPOCH=1700000000
)

env "${common_env[@]}" RELEASE_SOURCE_ROOT="$fixture_root" "$fixture_root/scripts/release.sh" package \
  runtime runtime-v1.2.3-preview.20260804 x86_64 "$TMP/runtime-bundle"
env "${common_env[@]}" RELEASE_SOURCE_ROOT="$fixture_root" "$fixture_root/scripts/release.sh" package \
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
# A current publication may bind its own binaries to its selected commit;
# standalone validation above does not require or query historical sources.
if SOURCE_SHA=0000000000000000000000000000000000000000 "$fixture_root/scripts/release.sh" validate \
  runtime runtime-v1.2.3-preview.20260804 x86_64 "$TMP/runtime-bundle" > "$TMP/publication-source.log" 2>&1; then
  fail "publication accepted an owner binary built at another commit"
fi
grep -Fq 'Go payload must be built from the clean selected commit' "$TMP/publication-source.log" \
  || fail "publication source mismatch failed for an unrelated reason"

"$fixture_root/scripts/release.sh" validate \
  vmlinux vmlinux-v2.3.4 x86_64 "$TMP/vmlinux-bundle"
RELEASE_ACCELERATOR_SOURCE_DIR="$TMP/absent-accelerator" \
  "$fixture_root/scripts/release.sh" validate \
  vmlinux vmlinux-v2.3.4 x86_64 "$TMP/vmlinux-bundle"

RELEASE_KIND=runtime RELEASE_DEPENDENCIES=accelerator=v0.1.3 bash "$ROOT/scripts/test-publisher.sh" \
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

# Keep the same ELF/module/architecture metadata while changing the selected
# input bytes. Package must compare its embedded binaries with these inputs.
for input in init envd; do
  if [ "$input" = init ]; then
    source_input="$fixture_root/bin/x86_64/sandbox-init"
    override=RELEASE_SANDBOX_INIT_BIN
    expected='embedded sandbox-init differs from the verified outer payload'
  else
    source_input="$fixture_root/native-deps/bin/x86_64/envd"
    override=RELEASE_ENVD_BIN
    expected='embedded envd differs from the selected build binary'
  fi
  cp "$source_input" "$TMP/changed-$input"
  printf 'different selected bytes\n' >> "$TMP/changed-$input"
  if env "${common_env[@]}" "$override=$TMP/changed-$input" RELEASE_SOURCE_ROOT="$fixture_root" \
    "$ROOT/scripts/release.sh" package runtime runtime-v1.2.3-preview.20260804 x86_64 \
    "$TMP/changed-input-$input" > "$TMP/changed-input-$input.log" 2>&1; then
    fail "packager accepted different selected $input bytes"
  fi
  grep -Fq "$expected" "$TMP/changed-input-$input.log" \
    || { tail -n 10 "$TMP/changed-input-$input.log"; fail "selected $input bytes failed for an unrelated reason"; }
done

runtime_archive="$TMP/runtime-bundle/assets/sandbox-runtime-x86_64-v1.2.3-preview.20260804.tar.gz"
# The generator is linked into flatten-ctl; retired native inputs are not shipped.
if tar -tzf "$runtime_archive" | grep -E '^./bin/mkfs[.]erofs$|^./share/sources/runtime/(EROFS-INPUTS[.]tsv|erofs-patches/)'; then
  fail "runtime package retained a native generator payload or its payload inventory"
fi

go_toolchain="$(go version | awk '{print $3}')"
for path in ./bin/sandbox-runtime.bundle ./bin/sandbox-init ./bin/flatten-ctl \
  ./share/licenses/runtime/project/LICENSE \
  ./share/licenses/runtime/accelerator/LICENSE \
  ./share/licenses/runtime/envd/LICENSE \
  ./share/licenses/runtime/go-toolchain/"$go_toolchain"/LICENSE; do
  tar -tzf "$runtime_archive" | grep -Fx "$path" >/dev/null \
    || fail "runtime archive is missing $path"
done
vmlinux_archive="$TMP/vmlinux-bundle/assets/vmlinux-x86_64-v2.3.4.tar.gz"
for path in ./bin/vmlinux ./bin/vmlinux.sha256 \
  ./share/licenses/vmlinux/project/LICENSE \
  ./share/licenses/vmlinux/linux/COPYING \
  ./share/licenses/vmlinux/linux/LICENSES/preferred/GPL-2.0; do
  tar -tzf "$vmlinux_archive" | grep -Fx "$path" >/dev/null \
    || fail "vmlinux archive is missing $path"
done
candidate="$TMP/linux-copying-bytes"
cp -a "$TMP/vmlinux-bundle" "$candidate"
mkdir "$candidate/root"
tar --same-permissions -xzf "$vmlinux_archive" -C "$candidate/root"
copying="$candidate/root/share/licenses/vmlinux/linux/COPYING"
: > "$copying"

tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1700000000 \
  -czf "$candidate/assets/$(basename "$vmlinux_archive")" -C "$candidate/root" .
if "$fixture_root/scripts/release.sh" validate vmlinux vmlinux-v2.3.4 x86_64 "$candidate" > "$candidate/result.log" 2>&1; then
  fail "validator accepted empty Linux COPYING"
fi
grep -Fq 'license material is empty' "$candidate/result.log" \
  || fail "Linux COPYING mutation failed for an unrelated reason"

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

env "${common_env[@]}" RELEASE_SOURCE_ROOT="$fixture_root" "$ROOT/scripts/release.sh" package \
  runtime runtime-v1.2.3-preview.20260804 x86_64 "$TMP/runtime-reproducible"
env "${common_env[@]}" RELEASE_SOURCE_ROOT="$fixture_root" "$ROOT/scripts/release.sh" package \
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
if env "${common_env[@]}" RELEASE_SOURCE_ROOT="$fixture_root" "$fixture_root/scripts/release.sh" package \
  runtime v1.2.3 x86_64 "$TMP/invalid-version" >/dev/null 2>&1; then
  fail "packager accepted a runtime version without the runtime prefix"
fi
if env "${common_env[@]}" RELEASE_NATIVE_BIN_DIR="$fixture_root/native-deps/bin/x86_64" \
  RELEASE_SOURCE_ROOT="$fixture_root" "$fixture_root/scripts/release.sh" package vmlinux vmlinux-v1.2.3 aarch64 "$TMP/invalid-arch" >/dev/null 2>&1; then
  fail "packager accepted x86_64 payloads as aarch64"
fi
env "${common_env[@]}" \
  RELEASE_BIN_DIR="$fixture_root/bin/x86_64" \
  RELEASE_NATIVE_BIN_DIR="$fixture_root/native-deps/bin/x86_64" \
  RELEASE_ENVD_SOURCE_DIR="$fixture_root/native-deps/build/src/e2b-infra" \
  RELEASE_SOURCE_ROOT="$fixture_root" "$fixture_root/scripts/release.sh" package runtime \
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

    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1700000000 \
      -czf "$candidate/assets/$candidate_archive" -C "$candidate/root" .
    if "$fixture_root/scripts/release.sh" validate "$kind" "$version" x86_64 "$candidate" > "$candidate/result.log" 2>&1; then
      fail "validator accepted $kind extra layout entry $extra_path with regenerated checksums"
    fi
    grep -Fq 'outside the exact' "$candidate/result.log" \
      || fail "extra layout entry failed for an unrelated reason"
  done

done
git clone --quiet --no-local "$fixture_root" "$TMP/target-source"
for target in darwin/amd64 linux/arm64; do
  (cd "$TMP/target-source" && GOWORK=off CGO_ENABLED=0 GOOS="${target%/*}" GOARCH="${target#*/}" \
    go build -buildvcs=true -o "$TMP/target-${target//\//-}" ./cmd/flatten-ctl)
done
(cd "$TMP/target-source" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -buildvcs=true -o "$TMP/other-main" ./cmd/other-tool)
(cd "$TMP/target-source" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -buildvcs=false -o "$TMP/unstamped" ./cmd/flatten-ctl)
printf '// fixture dirty source\n' >> "$TMP/target-source/cmd/flatten-ctl/main.go"
(cd "$TMP/target-source" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
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
  if "$fixture_root/scripts/release.sh" validate runtime runtime-v1.2.3-preview.20260804 \
    x86_64 "$candidate" > "$candidate/result.log" 2>&1; then
    fail "validator accepted $mutation flatten-ctl with regenerated checksums"
  fi
  case "$mutation" in
    wrong-*) expected='must target linux/amd64' ;;
    other-*) expected='must use the selected guest-runtime module and main package' ;;
    unstamped) expected='embedded flatten-ctl differs' ;;
    dirty-source) expected='embedded flatten-ctl differs' ;;
  esac
  grep -Fq "$expected" "$candidate/result.log" || fail "$mutation failed for an unrelated reason"
done

for kind in runtime vmlinux; do
  if [ "$kind" = runtime ]; then
    version=runtime-v1.2.3-preview.20260804
    labels=(project accelerator envd)
  else
    version=vmlinux-v2.3.4
    labels=(project linux)
  fi
  archive_name="$("$fixture_root/scripts/release.sh" archive-name "$kind" "$version" x86_64)"
  for label in "${labels[@]}"; do
    candidate="$TMP/$kind-missing-$label"
    cp -a "$TMP/$kind-bundle" "$candidate"
    mkdir "$candidate/root"
    tar --same-permissions -xzf "$candidate/assets/$archive_name" -C "$candidate/root"
    rm -rf "$candidate/root/share/licenses/$kind/$label"
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1700000000 \
      -czf "$candidate/assets/$archive_name" -C "$candidate/root" .
    if "$fixture_root/scripts/release.sh" validate "$kind" "$version" x86_64 "$candidate" > "$candidate/result.log" 2>&1; then
      fail "validator accepted missing $kind/$label licenses"
    fi
    grep -Eq 'license material is missing|Linux COPYING is missing' "$candidate/result.log" || fail 'license absence failed for an unrelated reason'
  done
  if tar -tzf "$TMP/$kind-bundle/assets/$archive_name" | grep -E '(^|/)sources/|SOURCES[.]tsv|GO-BUILD-INFO[.]tsv|GO-MODULES[.]tsv|MATERIALS[.]sha256' >/dev/null; then
    fail "new $kind package contains source records"
  fi
done

for kind in runtime vmlinux; do
  if [ "$kind" = runtime ]; then
    version=runtime-v1.2.3-preview.20260804
    names=(envd)
  else
    version=vmlinux-v2.3.4
    names=(linux)
  fi
  archive_name="$("$fixture_root/scripts/release.sh" archive-name "$kind" "$version" x86_64)"
  for mutation in owner setuid setgid writable-directory writable-payload; do
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
      fi
      tar --sort=name --owner="$owner" --group=0 --numeric-owner --mtime=@1700000000 \
        -czf "$candidate/assets/$archive_name" -C "$candidate/root" .
      if "$fixture_root/scripts/release.sh" validate "$kind" "$version" x86_64 "$candidate" >/dev/null 2>&1; then
        fail "validator accepted $kind $name with changed $mutation and regenerated checksums"
      fi
    done
  done
done

# Standalone validation reads package bytes and notices, with no Git history,
# checkout, downloads, or rebuild of the embedded released sandbox-init.
mkdir -p "$TMP/standalone-tools" "$TMP/standalone-bin"
cp -a "$fixture_root/scripts" "$TMP/standalone-tools/scripts"
for command in git curl wget cargo make gcc; do
  printf '#!/bin/sh\nexit 97\n' > "$TMP/standalone-bin/$command"
  chmod 0755 "$TMP/standalone-bin/$command"
done
for kind in runtime vmlinux; do
  if [ "$kind" = runtime ]; then version=runtime-v1.2.3-preview.20260804; else version=vmlinux-v2.3.4; fi
  env PATH="$TMP/standalone-bin:$PATH" GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
    "$TMP/standalone-tools/scripts/release.sh" validate "$kind" "$version" x86_64 "$TMP/$kind-bundle"
done
echo "test-release: standalone validation without checkout/download/build PASS"
echo "test-release: PASS"
