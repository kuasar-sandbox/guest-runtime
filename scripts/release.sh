#!/usr/bin/env bash

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

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

copy_root_executable() {
  local source="$1" destination="$2"
  [ -x "$ROOT/$source" ] || fail "missing executable release input: $ROOT/$source"
  mkdir -p "$(dirname "$STAGE/$destination")"
  install -m 0755 "$ROOT/$source" "$STAGE/$destination"
}

check_go_binary() {
  go version -m "$1" >/dev/null 2>&1 || fail "Go build info is missing from $1"
}

validate_archive_paths() {
  local archive="$1" listing="$WORK/listing"
  tar -tzf "$archive" > "$listing"
  awk '
    /^\// { exit 1 }
    { path=$0; sub(/^\.\//, "", path); if (path ~ /(^|\/)\.\.($|\/)/) exit 1 }
  ' "$listing" || fail "$archive contains an unsafe path"
  if grep -E '(^|/)release\.json$|(^|/)release/[^/]+\.json$' "$listing" >/dev/null; then
    fail "$archive contains release metadata JSON"
  fi
  awk '
    { path=$0; sub(/^\.\//, "", path) }
    path != "" && path !~ /\/$/ && path !~ /^(bin|docs|test)\// { exit 1 }
  ' "$listing" || fail "$archive contains a file outside bin/, docs/, or test/"
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

  validate_archive_paths "$bundle/assets/$archive"
  local extract="$WORK/extract"
  rm -rf "$extract"
  mkdir -p "$extract"
  tar -xzf "$bundle/assets/$archive" -C "$extract"
  case "$kind" in
    runtime)
      [ -f "$extract/bin/sandbox-runtime.bundle" ] \
        || fail "$archive is missing bin/sandbox-runtime.bundle"
      [ -x "$extract/bin/flatten-ctl" ] || fail "$archive is missing bin/flatten-ctl"
      [ -x "$extract/bin/mkfs.erofs" ] || fail "$archive is missing bin/mkfs.erofs"
      check_go_binary "$extract/bin/flatten-ctl"
      local file
      for file in docs/guest-runtime.md docs/sandbox-runtime.md docs/flatten.md \
        test/e2e/e2e_flatten.sh test/e2e/README.md; do
        [ -f "$extract/$file" ] || fail "$archive is missing $file"
      done
      ;;
    vmlinux)
      [ -f "$extract/bin/vmlinux" ] || fail "$archive is missing bin/vmlinux"
      [ -f "$extract/docs/vmlinux.md" ] || fail "$archive is missing docs/vmlinux.md"
      ;;
  esac
}

package_release() {
  [ "$#" -eq 4 ] || fail "usage: release.sh package <runtime|vmlinux> <version> <arch> <output-dir>"
  local kind="$1" version="$2" arch archive output="$4" epoch bin_dir native_bin_dir
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
  bin_dir="${RELEASE_BIN_DIR:-$ROOT/bin/$arch}"
  native_bin_dir="${RELEASE_NATIVE_BIN_DIR:-$ROOT/native-deps/bin/$arch}"
  case "$kind" in
    runtime)
      copy_external_file "$bin_dir/sandbox-runtime.bundle" bin/sandbox-runtime.bundle
      copy_executable "$bin_dir/flatten-ctl" bin/flatten-ctl
      copy_executable "$native_bin_dir/mkfs.erofs" bin/mkfs.erofs
      check_go_binary "$STAGE/bin/flatten-ctl"
      copy_file README.md docs/guest-runtime.md
      copy_file docs/sandbox-runtime.md docs/sandbox-runtime.md
      copy_file docs/flatten.md docs/flatten.md
      copy_root_executable test/e2e/e2e_flatten.sh test/e2e/e2e_flatten.sh
      copy_file test/e2e/README.md test/e2e/README.md
      ;;
    vmlinux)
      copy_external_file "$native_bin_dir/vmlinux" bin/vmlinux
      copy_file docs/vmlinux.md docs/vmlinux.md
      ;;
  esac

  mkdir -p "$output/assets"
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" \
    --pax-option=delete=atime,delete=ctime -czf "$output/assets/$archive" -C "$STAGE" .
  (cd "$output/assets" && sha256sum "$archive" > SHA256SUMS)
  cat > "$output/release-notes.md" <<EOF
$kind $version for Linux $arch.

Extract the archive into a Kuasar Sandbox deployment root and verify it with \`SHA256SUMS\`. GitHub provides the source archives for this tag automatically.
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
