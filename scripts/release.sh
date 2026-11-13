#!/usr/bin/env bash

set -euo pipefail
umask 022

TOOL_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="${RELEASE_SOURCE_ROOT:-$TOOL_ROOT}"
[ -d "$ROOT" ] && [ ! -L "$ROOT" ] || { echo "release: invalid selected source root" >&2; exit 1; }
ROOT="$(cd "$ROOT" && pwd -P)"
WORK="$(mktemp -d)"
trap 'chmod -R u+w "$WORK"; rm -rf "$WORK"' EXIT
# shellcheck source=scripts/release-materials.sh
source "${TOOL_ROOT:-$ROOT}/scripts/release-materials.sh"

fail() {
  echo "release: $*" >&2
  exit 1
}

validate_kind() {
  case "$1" in runtime|vmlinux) ;; *) fail "release kind must be runtime or vmlinux" ;; esac
}

validate_version() {
  local kind="$1" version="$2"
  validate_kind "$kind"
  [[ "$version" =~ ^${kind}-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-preview\.[0-9]{8}(\.[1-9][0-9]*)?)?$ ]] \
    || fail "$kind version must match $kind-vX.Y.Z or $kind-vX.Y.Z-preview.YYYYMMDD[.N]"
}

normalize_arch() {
  case "$1" in
    amd64|x86_64) printf 'x86_64\n' ;;
    arm64|aarch64) printf 'aarch64\n' ;;
    *) fail "unsupported release architecture: $1" ;;
  esac
}

archive_name() {
  local kind="$1" version="$2" arch payload
  validate_version "$kind" "$version"
  arch="$(normalize_arch "$3")"
  payload="${version#${kind}-}"
  case "$kind" in
    runtime) printf 'sandbox-runtime-%s-%s.tar.gz\n' "$arch" "$payload" ;;
    vmlinux) printf 'vmlinux-%s-%s.tar.gz\n' "$arch" "$payload" ;;
  esac
}

copy_file() {
  local source="$1" destination="$2"
  [ -f "$ROOT/$source" ] || fail "missing release input: $ROOT/$source"
  mkdir -p "$(dirname "$STAGE/$destination")"
  install -m 0644 "$ROOT/$source" "$STAGE/$destination"
}

copy_external_file() {
  local source="$1" destination="$2"
  [ -f "$source" ] || fail "missing release input: $source"
  mkdir -p "$(dirname "$STAGE/$destination")"
  install -m 0644 "$source" "$STAGE/$destination"
}

copy_executable() {
  local source="$1" destination="$2"
  [ -x "$source" ] || fail "missing executable release input: $source"
  mkdir -p "$(dirname "$STAGE/$destination")"
  install -m 0755 "$source" "$STAGE/$destination"
}

copy_root_script() {
  local source="$1" destination="$2"
  [ -f "$ROOT/$source" ] || fail "missing script release input: $ROOT/$source"
  mkdir -p "$(dirname "$STAGE/$destination")"
  install -m 0755 "$ROOT/$source" "$STAGE/$destination"
}

# Inspect headers without executing target payloads on the build host.
check_target_binary() {
  local file="$1" machine
  case "$2" in
    x86_64) machine='Advanced Micro Devices X86-64' ;;
    aarch64) machine='AArch64' ;;
    *) fail "invalid target: $2" ;;
  esac
  LC_ALL=C readelf -h "$file" | awk -F: -v machine="$machine" '
    { gsub(/^[ \t]+|[ \t]+$/, "", $1); gsub(/^[ \t]+|[ \t]+$/, "", $2) }
    $1 == "Class" { class++; if ($2 != "ELF64") bad=1 }
    $1 == "Data" { data++; if ($2 != "2\047s complement, little endian") bad=1 }
    $1 == "Machine" { arch++; if ($2 != machine) bad=1 }
    END { exit bad || class != 1 || data != 1 || arch != 1 }
  ' || fail "${3:-payload} has the wrong ELF target ($2): $file"
}

validate_kernel_image() {
  if [ "$2" = x86_64 ]; then
    check_target_binary "$1" "$2"
    return
  fi
  [ "$2" = aarch64 ] || fail "invalid kernel target: $2"
  # Linux arm64 boot protocol: uncompressed Image, not an ELF executable.
  # https://docs.kernel.org/arch/arm64/booting.html
  python3 - "$1" <<'PY'
import struct
import sys
with open(sys.argv[1], "rb") as image:
    header = image.read(64)
if len(header) != 64 or header[56:60] != b"ARM\x64":
    raise SystemExit("release: expected an uncompressed AArch64 kernel Image")
size, flags = struct.unpack_from("<QQ", header, 16)
if size < 64 or flags & 1 or any(header[32:56]):
    raise SystemExit("release: invalid little-endian AArch64 kernel Image header")
PY
}

check_go_binary() {
  local file="$1" info
  local target_arch="$2" go_arch
  case "$target_arch" in x86_64) go_arch=amd64 ;; aarch64) go_arch=arm64 ;; *) fail "invalid target: $target_arch" ;; esac
  info="$(go version -m "$file" 2>/dev/null)" || fail "Go build info is missing from $file"
  awk -F '\t' -v expected_arch="$go_arch" '
    $2 == "build" && $3 ~ /^GOOS=/ { os++; if ($3 != "GOOS=linux") bad=1 }
    $2 == "build" && $3 ~ /^GOARCH=/ { arch++; if ($3 != "GOARCH=" expected_arch) bad=1 }
    END { exit bad || os != 1 || arch != 1 }
  ' <<< "$info" || fail "Go release payload must target linux/$go_arch: $file"
  awk -F '\t' '
    $2 == "path" { paths++; if ($3 != "github.com/kuasar-sandbox/guest-runtime/cmd/flatten-ctl") bad=1 }
    $2 == "mod" { modules++; if ($3 != "github.com/kuasar-sandbox/guest-runtime") bad=1 }
    END { exit bad || paths != 1 || modules != 1 }
  ' <<< "$info" || fail "flatten-ctl must use the selected guest-runtime module and main package"
  awk -F '\t' '
    $2 == "dep" && $3 == "github.com/kuasar-sandbox/go-erofs" && $4 ~ /^v/ && $5 ~ /^h1:/ { found++ }
    END { exit found != 1 }
  ' <<< "$info" || fail "flatten-ctl lacks the linked Go EROFS generator"
  check_target_binary "$file" "$target_arch"
}

record_release_go_contexts() {
  local build_root="$1" envd_root="$2" context directory info
  for context in runtime envd; do
    case "$context" in
      runtime) directory="$build_root" ;;
      envd) directory="$envd_root/packages/envd" ;;
    esac
    info="$WORK/go-context-$context.json"
    if [ "$context" = envd ]; then
      GOWORK=off go -C "$directory" env -json GOROOT GOVERSION GOHOSTOS GOHOSTARCH > "$info"
    else
      go -C "$directory" env -json GOROOT GOVERSION GOHOSTOS GOHOSTARCH > "$info"
    fi
  done
  RELEASE_MATERIALS_GO_ENV="$WORK/go-build-toolchains.json"
  jq -s . "$WORK/go-context-runtime.json" "$WORK/go-context-envd.json" \
    > "$RELEASE_MATERIALS_GO_ENV"
}

validate_archive_paths() {
  local archive="$1" kind="$2" listing="$WORK/listing"
  tar -tzf "$archive" > "$listing"
  awk '
    /^\// { exit 1 }
    { path=$0; sub(/^\.\//, "", path); if (path ~ /(^|\/)\.\.($|\/)/) exit 1 }
  ' "$listing" || fail "$archive contains an unsafe path"
  if grep -E '(^|/)release\.json$|(^|/)release/[^/]+\.json$' "$listing" >/dev/null; then
    fail "$archive contains release metadata JSON"
  fi
  awk -v unit="$kind" '
    {
      path=$0; sub(/^\.\//, "", path)
      directory=(path == "" || path ~ /\/$/); sub(/\/$/, "", path)
      if (path ~ /(^|\/)\.\.?($|\/)|\/\// || path !~ /^[A-Za-z0-9._+@:\/~=-]*$/ || seen[path]++) exit 1
      material=(path ~ ("^share/licenses/" unit "/"))
      if (directory) {
        if (path == "" || path == "bin" || path == "share" ||
            path == "share/licenses" ||
            path == "share/licenses/" unit || material) next
        exit 1
      }
      if (material) next
      if (unit == "runtime" && path ~ /^bin\/(sandbox-runtime[.]bundle|sandbox-init|flatten-ctl)$/) next
      if (unit == "vmlinux" && (path == "bin/vmlinux" || path == "bin/vmlinux.sha256")) next
      exit 1
    }
  ' "$listing" || fail "$archive contains an entry outside the exact $kind release layout"
  tar --numeric-owner -tvzf "$archive" | awk '
    $2 != "0/0" { exit 1 }
    $1 ~ /^d/ { if ($1 != "drwxr-xr-x") exit 1; next }
    $1 !~ /^-/ { exit 1 }
    {
      path=$6; sub(/^\.\//, "", path)
      expected=(path ~ /^bin\/(sandbox-init|flatten-ctl)$/ ? "-rwxr-xr-x" : "-rw-r--r--")
      if ($1 != expected) exit 1
    }
  ' || fail "$archive contains an unsafe type, mode or ownership"
}


prepare_runtime_verifier() {
  # Use trusted host readers; never run a binary supplied by the archive.
  RUNTIME_FSCK="$(command -v fsck.erofs)" \
    || fail "Runtime validation requires fsck.erofs on PATH"
  RUNTIME_DUMP="$(command -v dump.erofs)" \
    || fail "Runtime validation requires dump.erofs on PATH"
}

validate_runtime_payloads() {
  local extract="$1" target_arch="$2" go_arch payloads="$WORK/runtime-payloads" binary package module info
  case "$target_arch" in x86_64) go_arch=amd64 ;; aarch64) go_arch=arm64 ;; *) fail "invalid target: $target_arch" ;; esac
  prepare_runtime_verifier
  python3 "${TOOL_ROOT:-$ROOT}/scripts/release-runtime-payloads.py" --without-mkfs \
    "$extract/bin/sandbox-runtime.bundle" "$RUNTIME_FSCK" "$RUNTIME_DUMP" "$payloads" \
    || fail "Runtime embedded payload verification failed"
  cmp -s "$payloads/flatten-ctl" "$extract/bin/flatten-ctl" \
    || fail "embedded flatten-ctl differs from the verified outer payload"
  cmp -s "$payloads/init" "$extract/bin/sandbox-init" \
    || fail "embedded sandbox-init differs from the verified outer payload"
  for binary in init envd flatten-ctl; do
    check_target_binary "$payloads/$binary" "$target_arch" "embedded $binary"
  done
  for binary in init envd; do
    case "$binary" in
      init) module=github.com/kuasar-sandbox/guest-runtime; package="$module/cmd/sandbox-init" ;;
      envd) module=github.com/e2b-dev/infra/packages/envd; package="$module" ;;
    esac
    info="$(go version -m "$payloads/$binary" 2>/dev/null)" || fail "embedded Go build info is missing"
    awk -F '\t' -v module="$module" -v package="$package" -v expected_arch="$go_arch" '
      $2 == "path" { paths++; if ($3 != package) bad=1 }
      $2 == "mod" { modules++; if ($3 != module) bad=1 }
      $2 == "build" && $3 ~ /^GOOS=/ { os++; if ($3 != "GOOS=linux") bad=1 }
      $2 == "build" && $3 ~ /^GOARCH=/ { arch++; if ($3 != "GOARCH=" expected_arch) bad=1 }
      END { exit bad || paths != 1 || modules != 1 || os != 1 || arch != 1 }
    ' <<< "$info" || fail "embedded $binary has the wrong Go main identity or target"
  done
  release_materials_validate_go "$extract" runtime bin/sandbox-runtime.bundle:/sbin/init "$payloads/init"
  release_materials_validate_go "$extract" runtime bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd "$payloads/envd"
}

validate_bundle() {
  [ "$#" -eq 4 ] || fail "usage: release.sh validate <runtime|vmlinux> <version> <arch> <bundle-dir>"
  local kind="$1" version="$2" arch archive bundle="$4"
  arch="$(normalize_arch "$3")"
  archive="$(archive_name "$kind" "$version" "$arch")"
  [ -s "$bundle/release-notes.md" ] || fail "release-notes.md is missing"
  [ -d "$bundle/assets" ] || fail "assets directory is missing"

  local expected="$WORK/expected-assets" actual="$WORK/actual-assets"
  printf '%s\n' "$archive" | LC_ALL=C sort > "$expected"
  find "$bundle/assets" -mindepth 1 -maxdepth 1 -printf '%f\n' | LC_ALL=C sort > "$actual"
  cmp -s "$expected" "$actual" \
    || { diff -u "$expected" "$actual" >&2 || true; fail "bundle contains an unexpected asset set"; }
  [ -s "$bundle/assets/$archive" ] && [ ! -L "$bundle/assets/$archive" ] \
    || fail "release archive must be a nonempty regular file"


  validate_archive_paths "$bundle/assets/$archive" "$kind"
  local extract="$WORK/extract"
  rm -rf "$extract"
  mkdir -p "$extract"
  tar -xzf "$bundle/assets/$archive" -C "$extract"
  release_materials_validate "$extract" "$kind"
  case "$kind" in
    runtime)
      release_materials_require_license "$extract" runtime accelerator
      release_materials_require_license "$extract" runtime envd
      release_materials_validate_go "$extract" runtime bin/flatten-ctl
      [ -f "$extract/bin/sandbox-runtime.bundle" ] \
        || fail "$archive is missing bin/sandbox-runtime.bundle"
      [ -x "$extract/bin/sandbox-init" ] || fail "$archive is missing bin/sandbox-init"
      release_materials_validate_go "$extract" runtime bin/sandbox-init
      [ -x "$extract/bin/flatten-ctl" ] || fail "$archive is missing bin/flatten-ctl"
      check_go_binary "$extract/bin/flatten-ctl" "$arch"
      validate_runtime_payloads "$extract" "$arch"
      ;;
    vmlinux)
      [ -f "$extract/bin/vmlinux" ] || fail "$archive is missing bin/vmlinux"
      validate_kernel_image "$extract/bin/vmlinux" "$arch"
      # Historical kernel packages have no sidecar. New packages always emit
      # it, and a present record must exactly describe the shipped raw kernel.
      # Compare with trusted output rather than letting archive text name files
      # for sha256sum --check to open.
      if [ -f "$extract/bin/vmlinux.sha256" ]; then
        (cd "$extract/bin" && sha256sum -- vmlinux) > "$WORK/vmlinux.sha256" \
          || fail "cannot checksum packaged bin/vmlinux"
        cmp -s "$WORK/vmlinux.sha256" "$extract/bin/vmlinux.sha256" \
          || fail "bin/vmlinux.sha256 does not match the packaged kernel"
      fi
      [ -s "$extract/share/licenses/vmlinux/linux/COPYING" ] \
        || fail "Linux COPYING is missing"
      ;;
  esac
}

package_release() {
  [ "$#" -eq 4 ] || fail "usage: release.sh package <runtime|vmlinux> <version> <arch> <output-dir>"
  local kind="$1" version="$2" arch archive output="$4" epoch bin_dir native_bin_dir project_sha value
  arch="$(normalize_arch "$3")"
  archive="$(archive_name "$kind" "$version" "$arch")"
  if [ -z "$output" ] || [ "$output" = / ] || [ "$output" = . ]; then
    fail "unsafe output directory: $output"
  fi
  [ ! -e "$output" ] || fail "output already exists: $output"
  epoch="${SOURCE_DATE_EPOCH:-0}"
  [[ "$epoch" =~ ^[0-9]+$ ]] || fail "SOURCE_DATE_EPOCH must be an integer"

  STAGE="$WORK/stage"
  rm -rf "$STAGE"
  mkdir -p "$STAGE"
  project_sha="$(release_materials_resolve_git_source "$ROOT" "${SOURCE_SHA:-}" guest-runtime)"
  bin_dir="${RELEASE_BIN_DIR:-$ROOT/bin/$arch}"
  native_bin_dir="${RELEASE_NATIVE_BIN_DIR:-$ROOT/native-deps/bin/$arch}"
  release_materials_init "$STAGE" "$WORK/materials" "$kind"
  release_materials_copy_licenses "$ROOT" project
  case "$kind" in
    runtime)
      local accelerator_source envd_source
      local sandbox_init_bin envd_bin accelerator_version
      local accelerator_sha
      accelerator_source="${RELEASE_ACCELERATOR_SOURCE_DIR:-$ROOT/../accelerator}"
      envd_source="${RELEASE_ENVD_SOURCE_DIR:-${ENVD_SRC:-$ROOT/native-deps/build/src/e2b-infra}}"
      sandbox_init_bin="${RELEASE_SANDBOX_INIT_BIN:-$bin_dir/sandbox-init}"
      envd_bin="${RELEASE_ENVD_BIN:-$native_bin_dir/envd}"
      accelerator_version="${RELEASE_ACCELERATOR_VERSION:-${ACCELERATOR_VERSION:-}}"
      for value in "$accelerator_version"; do
        [[ "$value" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-preview\.[0-9]{8}(\.[1-9][0-9]*)?)?$ ]] \
          || fail "runtime internal dependency versions must identify selected component releases"
      done
      accelerator_sha="$(release_materials_resolve_git_source "$accelerator_source" \
        "${RELEASE_ACCELERATOR_SOURCE_SHA:-}" accelerator)"
      record_release_go_contexts "$ROOT" "$envd_source"
      copy_external_file "$bin_dir/sandbox-runtime.bundle" bin/sandbox-runtime.bundle
      copy_executable "$sandbox_init_bin" bin/sandbox-init
      release_materials_require_go_revision "$STAGE/bin/sandbox-init" "$project_sha"
      copy_executable "$bin_dir/flatten-ctl" bin/flatten-ctl
      check_go_binary "$STAGE/bin/flatten-ctl" "$arch"
      release_materials_require_go_revision "$STAGE/bin/flatten-ctl" "$project_sha"
      accelerator_version="$(release_materials_git_version "$accelerator_source" "$accelerator_version" "$accelerator_sha")"
      release_materials_copy_licenses "$accelerator_source" accelerator
      release_materials_copy_licenses "$envd_source" envd
      release_materials_copy_licenses "$envd_source" \
        go/github.com/e2b-dev/infra/packages/shared@v0.0.0
      release_materials_record_source 'bin/sandbox-runtime.bundle,bin/sandbox-init,bin/flatten-ctl' guest-runtime "$version" \
        "https://github.com/kuasar-sandbox/guest-runtime/commit/$project_sha" \
        "git:$project_sha" project
      release_materials_record_source 'bin/sandbox-runtime.bundle,bin/flatten-ctl' accelerator "$accelerator_version" \
        "https://github.com/kuasar-sandbox/accelerator/commit/$accelerator_sha" \
        "git:$accelerator_sha" accelerator
      release_materials_record_source 'bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd' envd 2026.22 \
        'https://github.com/e2b-dev/runtime/archive/refs/tags/2026.22.tar.gz' \
        'sha256:8f074b23dcb2c9db48f8e674a8ab8082b54a16088fb29b66376178feeb7fe040' envd
      release_materials_record_source 'bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd' \
        github.com/e2b-dev/infra/packages/shared 2026.22 \
        'https://github.com/e2b-dev/runtime/archive/refs/tags/2026.22.tar.gz' \
        'sha256:8f074b23dcb2c9db48f8e674a8ab8082b54a16088fb29b66376178feeb7fe040' \
        go/github.com/e2b-dev/infra/packages/shared@v0.0.0
      release_materials_add_go_binary "$STAGE/bin/flatten-ctl" bin/flatten-ctl
      release_materials_add_go_binary "$STAGE/bin/sandbox-init" bin/sandbox-init
      release_materials_add_go_binary "$STAGE/bin/sandbox-init" bin/sandbox-runtime.bundle:/sbin/init
      release_materials_add_go_binary "$envd_bin" bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd
      ;;
    vmlinux)
      local linux_source linux_license_sha
      copy_external_file "$native_bin_dir/vmlinux" bin/vmlinux
      validate_kernel_image "$STAGE/bin/vmlinux" "$arch"
      # Derive identity from the final staged bytes, never a possibly stale
      # checksum beside the build input. The basename matches sandboxer #297.
      (cd "$STAGE/bin" && sha256sum -- vmlinux > vmlinux.sha256) \
        || fail "cannot generate bin/vmlinux.sha256"
      linux_source="${RELEASE_LINUX_SOURCE_DIR:-${LINUX_BUILD_SRC:-$ROOT/native-deps/build/src/linux}}"
      release_materials_copy_licenses "$linux_source" linux
      linux_license_sha="$(sha256sum "$linux_source/COPYING" | awk '{print $1}')"
      release_materials_record_source bin/vmlinux linux 6.1.169 \
        'https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.169.tar.gz' \
        'sha256:ab28b4ca2a2eca38b3da9aa33b231288168c3560bbc866359045f1c8f4d48d94' linux
      release_materials_record_source bin/vmlinux guest-runtime-kernel-inputs "$version" \
        "https://github.com/kuasar-sandbox/guest-runtime/tree/$project_sha/native-deps/deps" \
        "git:$project_sha;linux-copying-sha256:$linux_license_sha" project
      ;;
  esac
  release_materials_finish

  mkdir -p "$output/assets"
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" \
    --pax-option=delete=atime,delete=ctime -czf "$output/assets/$archive" -C "$STAGE" .
  cat > "$output/release-notes.md" <<EOF
$kind $version for Linux.

Download the archive for your architecture, verify its native GitHub asset size and digest, then extract it into a Kuasar Sandbox deployment root. Documentation and E2E suites from this exact tag are collected by the aggregate platform release.
EOF
  SOURCE_SHA="$project_sha" validate_bundle "$kind" "$version" "$arch" "$output"
  if [ "$kind" = runtime ]; then
    cmp -s "$WORK/runtime-payloads/init" "$sandbox_init_bin" \
      || fail "embedded sandbox-init differs from the selected build binary"
    cmp -s "$WORK/runtime-payloads/envd" "$envd_bin" \
      || fail "embedded envd differs from the selected build binary"
  fi
  echo "==> prepared $output for $version"
}

command -v go >/dev/null || fail "go is required"
case "${1:-}" in
  archive-name) shift; [ "$#" -eq 3 ] || fail "usage: release.sh archive-name <runtime|vmlinux> <version> <arch>"; archive_name "$@" ;;
  package) shift; package_release "$@" ;;
  validate) shift; validate_bundle "$@" ;;
  *) fail "usage: release.sh <archive-name|package|validate> ..." ;;
esac
