#!/usr/bin/env bash

set -euo pipefail
umask 022

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
# shellcheck source=scripts/release-materials.sh
source "$ROOT/scripts/release-materials.sh"

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
  [[ "$version" =~ ^${kind}-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-preview\.[0-9]{8})?$ ]] \
    || fail "$kind version must match $kind-vX.Y.Z or $kind-vX.Y.Z-preview.YYYYMMDD"
}

normalize_arch() {
  case "$1" in
    amd64|x86_64) printf 'x86_64\n' ;;
    *) fail "unsupported release architecture: $1; current release target is x86_64" ;;
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

check_go_binary() {
  go version -m "$1" >/dev/null 2>&1 || fail "Go build info is missing from $1"
}

native_make_default() {
  local name="$1" value
  value="$(awk -v name="$name" '$1 == name && $2 == "?=" { print $3; exit }' \
    "$ROOT/native-deps/Makefile")"
  [ -n "$value" ] || fail "native dependency pin is missing from native-deps/Makefile: $name"
  printf '%s\n' "$value"
}

require_native_pin() {
  if [ "$#" -lt 3 ] || [ "$#" -gt 4 ]; then
    fail "require_native_pin requires name, repository value, selected value and optional mirror"
  fi
  local name="$1" repository_value="$2" selected_value="$3" mirror="${4:-}"
  local actual selected
  actual="$(native_make_default "$name")"
  [ "$actual" = "$repository_value" ] \
    || fail "$name no longer matches the source record used by release packaging"
  if [[ -v $name ]]; then
    selected="${!name}"
    if [ "$selected" != "$selected_value" ] \
      && { [ -z "$mirror" ] || [ "$selected" != "$mirror" ]; }; then
      fail "$name override is not supported by the pinned release material contract"
    fi
  fi
}

reject_release_path_overrides() {
  local name
  for name in "$@"; do
    if [[ -v $name ]]; then
      fail "$name override is not supported by the pinned release material contract"
    fi
  done
}

require_selected_value() {
  local name="$1" expected="$2" selected
  if [[ -v $name ]]; then
    selected="${!name}"
    [ "$selected" = "$expected" ] \
      || fail "$name override is not supported by the pinned release material contract"
  fi
}

validate_native_release_inputs() {
  local kind="$1" arch="$2"
  case "$kind" in
    runtime)
      reject_release_path_overrides RELEASE_BIN_DIR RELEASE_NATIVE_BIN_DIR \
        RELEASE_ENVD_SOURCE_DIR RELEASE_EROFS_SOURCE_DIR \
        RELEASE_SANDBOX_INIT_BIN RELEASE_ENVD_BIN
      require_selected_value SANDBOXER_DIR ../sandboxer
      require_selected_value SANDBOX_INIT "../sandboxer/bin/$arch/sandbox-init"
      require_selected_value ENVD "native-deps/bin/$arch/envd"
      require_selected_value FLATTEN_CTL "bin/$arch/flatten-ctl"
      require_selected_value GUEST_MKFS_EROFS "native-deps/bin/$arch/mkfs.erofs"
      require_native_pin EROFS_TARBALL \
        'https://codeload.github.com/erofs/erofs-utils/tar.gz/refs/tags/v1.9.1\#erofs-utils-v1.9.1.tar.gz' \
        'https://codeload.github.com/erofs/erofs-utils/tar.gz/refs/tags/v1.9.1#erofs-utils-v1.9.1.tar.gz'
      require_native_pin EROFS_TARBALL_SHA256 \
        a9ef5ab67c4b8d2d3e9ed71f39cd008bda653142a720d8a395a36f1110d0c432 \
        a9ef5ab67c4b8d2d3e9ed71f39cd008bda653142a720d8a395a36f1110d0c432
      require_native_pin ENVD_TARBALL \
        'https://codeload.github.com/e2b-dev/infra/tar.gz/refs/tags/2026.22\#e2b-infra-2026.22.tar.gz' \
        'https://codeload.github.com/e2b-dev/infra/tar.gz/refs/tags/2026.22#e2b-infra-2026.22.tar.gz'
      require_native_pin ENVD_TARBALL_SHA256 \
        9e1e81f2963fda1805466c337cd0a33638182a15a66295fd19bba6b9c454d92c \
        9e1e81f2963fda1805466c337cd0a33638182a15a66295fd19bba6b9c454d92c
      ;;
    vmlinux)
      reject_release_path_overrides RELEASE_NATIVE_BIN_DIR RELEASE_LINUX_SOURCE_DIR
      require_native_pin LINUX_TARBALL \
        'https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.169.tar.gz' \
        'https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.169.tar.gz' \
        'https://mirrors.tuna.tsinghua.edu.cn/kernel/v6.x/linux-6.1.169.tar.gz'
      require_native_pin LINUX_TARBALL_SHA256 \
        ab28b4ca2a2eca38b3da9aa33b231288168c3560bbc866359045f1c8f4d48d94 \
        ab28b4ca2a2eca38b3da9aa33b231288168c3560bbc866359045f1c8f4d48d94
      ;;
  esac
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
    { path=$0; sub(/^\.\//, "", path) }
    path != "" && path !~ /\/$/ && path !~ /^bin\// && path !~ ("^share/(licenses|sources)/" unit "/") { exit 1 }
  ' "$listing" || fail "$archive contains a file outside the $kind release layout"
  tar -tvzf "$archive" | awk '$1 !~ /^[-d]/ { exit 1 }' \
    || fail "$archive contains a non-regular, non-directory entry"
}

validate_bundle() {
  [ "$#" -eq 4 ] || fail "usage: release.sh validate <runtime|vmlinux> <version> <arch> <bundle-dir>"
  local kind="$1" version="$2" arch archive bundle="$4"
  arch="$(normalize_arch "$3")"
  archive="$(archive_name "$kind" "$version" "$arch")"
  [ -s "$bundle/release-notes.md" ] || fail "release-notes.md is missing"
  [ -d "$bundle/assets" ] || fail "assets directory is missing"

  local expected="$WORK/expected-assets" actual="$WORK/actual-assets"
  printf '%s\n' "$archive" SHA256SUMS | LC_ALL=C sort > "$expected"
  find "$bundle/assets" -mindepth 1 -maxdepth 1 -type f -printf '%f\n' | LC_ALL=C sort > "$actual"
  cmp -s "$expected" "$actual" \
    || { diff -u "$expected" "$actual" >&2 || true; fail "bundle contains an unexpected asset set"; }
  [ "$(grep -cve '^[[:space:]]*$' "$bundle/assets/SHA256SUMS")" -eq 1 ] \
    || fail "SHA256SUMS must contain exactly one entry"
  local digest listed extra
  read -r digest listed extra < "$bundle/assets/SHA256SUMS"
  listed="${listed#\*}"
  if ! [[ "$digest" =~ ^[0-9a-f]{64}$ ]] \
    || [ "$listed" != "$archive" ] || [ -n "${extra:-}" ]; then
    fail "SHA256SUMS does not describe the expected archive"
  fi
  (cd "$bundle/assets" && sha256sum --quiet -c SHA256SUMS) \
    || fail "SHA256SUMS validation failed"

  validate_archive_paths "$bundle/assets/$archive" "$kind"
  local extract="$WORK/extract"
  rm -rf "$extract"
  mkdir -p "$extract"
  tar -xzf "$bundle/assets/$archive" -C "$extract"
  release_materials_validate "$extract" "$kind"
  case "$kind" in
    runtime)
      [ -f "$extract/bin/sandbox-runtime.bundle" ] \
        || fail "$archive is missing bin/sandbox-runtime.bundle"
      [ -x "$extract/bin/flatten-ctl" ] || fail "$archive is missing bin/flatten-ctl"
      [ -x "$extract/bin/mkfs.erofs" ] || fail "$archive is missing bin/mkfs.erofs"
      check_go_binary "$extract/bin/flatten-ctl"
      ;;
    vmlinux)
      [ -f "$extract/bin/vmlinux" ] || fail "$archive is missing bin/vmlinux"
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
  validate_native_release_inputs "$kind" "$arch"

  STAGE="$WORK/stage"
  rm -rf "$STAGE"
  mkdir -p "$STAGE"
  bin_dir="$ROOT/bin/$arch"
  native_bin_dir="$ROOT/native-deps/bin/$arch"
  project_sha="$(release_materials_resolve_git_source "$ROOT" "" guest-runtime)"
  release_materials_init "$STAGE" "$WORK/materials" "$kind"
  release_materials_copy_licenses "$ROOT" project
  case "$kind" in
    runtime)
      local sandboxer_source accelerator_source envd_source erofs_source
      local sandbox_init_bin envd_bin sandboxer_version accelerator_version
      local sandboxer_sha accelerator_sha
      copy_external_file "$bin_dir/sandbox-runtime.bundle" bin/sandbox-runtime.bundle
      copy_executable "$bin_dir/flatten-ctl" bin/flatten-ctl
      copy_executable "$native_bin_dir/mkfs.erofs" bin/mkfs.erofs
      check_go_binary "$STAGE/bin/flatten-ctl"
      sandboxer_source="${RELEASE_SANDBOXER_SOURCE_DIR:-$ROOT/../sandboxer}"
      accelerator_source="${RELEASE_ACCELERATOR_SOURCE_DIR:-$ROOT/../accelerator}"
      envd_source="$ROOT/native-deps/build/src/e2b-infra"
      erofs_source="$ROOT/native-deps/build/$arch/src/erofs-utils"
      sandbox_init_bin="$sandboxer_source/bin/$arch/sandbox-init"
      envd_bin="$native_bin_dir/envd"
      sandboxer_version="${RELEASE_SANDBOXER_VERSION:-${SANDBOXER_VERSION:-}}"
      accelerator_version="${RELEASE_ACCELERATOR_VERSION:-${ACCELERATOR_VERSION:-}}"
      for value in "$sandboxer_version" "$accelerator_version"; do
        [[ "$value" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-preview\.[0-9]{8})?$ ]] \
          || fail "runtime internal dependency versions must identify selected component releases"
      done
      sandboxer_sha="$(release_materials_resolve_git_source "$sandboxer_source" \
        "${RELEASE_SANDBOXER_SOURCE_SHA:-}" sandboxer)"
      accelerator_sha="$(release_materials_resolve_git_source "$accelerator_source" \
        "${RELEASE_ACCELERATOR_SOURCE_SHA:-}" accelerator)"
      release_materials_copy_licenses "$sandboxer_source" sandboxer
      release_materials_copy_licenses "$accelerator_source" accelerator
      release_materials_copy_licenses "$envd_source" envd
      release_materials_copy_licenses "$erofs_source" erofs-utils
      release_materials_record_source 'bin/sandbox-runtime.bundle,bin/flatten-ctl' guest-runtime "$version" \
        "https://github.com/kuasar-sandbox/guest-runtime/commit/$project_sha" \
        "git:$project_sha" project
      release_materials_record_source 'bin/sandbox-runtime.bundle:/sbin/init' sandboxer "$sandboxer_version" \
        "https://github.com/kuasar-sandbox/sandboxer/commit/$sandboxer_sha" \
        "git:$sandboxer_sha" sandboxer
      release_materials_record_source 'bin/sandbox-runtime.bundle,bin/flatten-ctl' accelerator "$accelerator_version" \
        "https://github.com/kuasar-sandbox/accelerator/commit/$accelerator_sha" \
        "git:$accelerator_sha" accelerator
      release_materials_record_source 'bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd' envd 2026.22 \
        'https://github.com/e2b-dev/infra/archive/refs/tags/2026.22.tar.gz' \
        'sha256:9e1e81f2963fda1805466c337cd0a33638182a15a66295fd19bba6b9c454d92c' envd
      release_materials_record_source 'bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs' erofs-utils v1.9.1 \
        'https://github.com/erofs/erofs-utils/archive/refs/tags/v1.9.1.tar.gz' \
        'sha256:a9ef5ab67c4b8d2d3e9ed71f39cd008bda653142a720d8a395a36f1110d0c432' erofs-utils
      release_materials_add_go_binary "$STAGE/bin/flatten-ctl" bin/flatten-ctl
      release_materials_add_go_binary "$sandbox_init_bin" bin/sandbox-runtime.bundle:/sbin/init
      release_materials_add_go_binary "$envd_bin" bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd
      ;;
    vmlinux)
      local linux_source linux_license_sha
      copy_external_file "$native_bin_dir/vmlinux" bin/vmlinux
      linux_source="$ROOT/native-deps/build/src/linux"
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
  (cd "$output/assets" && sha256sum "$archive" > SHA256SUMS)
  cat > "$output/release-notes.md" <<EOF
$kind $version for Linux $arch.

Extract the archive into a Kuasar Sandbox deployment root and verify it with \`SHA256SUMS\`. Documentation and E2E suites from this exact tag are collected by the aggregate platform release.
EOF
  validate_bundle "$kind" "$version" "$arch" "$output"
  echo "==> prepared $output for $version"
}

command -v go >/dev/null || fail "go is required"
case "${1:-}" in
  archive-name) shift; [ "$#" -eq 3 ] || fail "usage: release.sh archive-name <runtime|vmlinux> <version> <arch>"; archive_name "$@" ;;
  package) shift; package_release "$@" ;;
  validate) shift; validate_bundle "$@" ;;
  *) fail "usage: release.sh <archive-name|package|validate> ..." ;;
esac
