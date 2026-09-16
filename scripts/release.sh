#!/usr/bin/env bash

set -euo pipefail
umask 022

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'chmod -R u+w "$WORK"; rm -rf "$WORK"' EXIT
# shellcheck source=scripts/release-materials.sh
source "$ROOT/scripts/release-materials.sh"
# shellcheck source=scripts/release-native-materials.sh
source "$ROOT/scripts/release-native-materials.sh"

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
  local file="$1" info
  info="$(go version -m "$file" 2>/dev/null)" || fail "Go build info is missing from $file"
  awk -F '\t' '
    $2 == "build" && $3 ~ /^GOOS=/ { os++; if ($3 != "GOOS=linux") bad=1 }
    $2 == "build" && $3 ~ /^GOARCH=/ { arch++; if ($3 != "GOARCH=amd64") bad=1 }
    END { exit bad || os != 1 || arch != 1 }
  ' <<< "$info" || fail "Go release payload must target linux/amd64: $file"
  awk -F '\t' '
    $2 == "path" { paths++; if ($3 != "github.com/kuasar-sandbox/guest-runtime/cmd/flatten-ctl") bad=1 }
    $2 == "mod" { modules++; if ($3 != "github.com/kuasar-sandbox/guest-runtime") bad=1 }
    END { exit bad || paths != 1 || modules != 1 }
  ' <<< "$info" || fail "flatten-ctl must use the selected guest-runtime module and main package"
}

record_release_go_contexts() {
  local build_root="$1" sandboxer_root="$2" envd_root="$3" context directory info
  for context in runtime sandboxer envd; do
    case "$context" in
      runtime) directory="$build_root" ;;
      sandboxer) directory="$sandboxer_root" ;;
      envd) directory="$envd_root/packages/envd" ;;
    esac
    info="$WORK/go-context-$context.json"
    GOWORK=off go -C "$directory" env -json GOROOT GOVERSION GOHOSTOS GOHOSTARCH > "$info"
  done
  RELEASE_MATERIALS_GO_ENV="$WORK/go-build-toolchains.json"
  jq -s . "$WORK/go-context-runtime.json" "$WORK/go-context-sandboxer.json" "$WORK/go-context-envd.json" \
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
      material=(path ~ ("^share/(licenses|sources)/" unit "/"))
      if (directory) {
        if (path == "" || path == "bin" || path == "share" ||
            path == "share/licenses" || path == "share/sources" ||
            path == "share/licenses/" unit || path == "share/sources/" unit || material) next
        exit 1
      }
      if (material) next
      if (unit == "runtime" && path ~ /^bin\/(sandbox-runtime[.]bundle|flatten-ctl|mkfs[.]erofs)$/) next
      if (unit == "vmlinux" && path == "bin/vmlinux") next
      exit 1
    }
  ' "$listing" || fail "$archive contains an entry outside the exact $kind release layout"
  tar --numeric-owner -tvzf "$archive" | awk '
    $2 != "0/0" { exit 1 }
    $1 ~ /^d/ { if ($1 != "drwxr-xr-x") exit 1; next }
    $1 !~ /^-/ { exit 1 }
    {
      path=$6; sub(/^\.\//, "", path)
      expected=(path ~ /^bin\/(flatten-ctl|mkfs[.]erofs)$/ ? "-rwxr-xr-x" : "-rw-r--r--")
      if ($1 != expected) exit 1
    }
  ' || fail "$archive contains an unsafe type, mode or ownership"
}

validate_source_inventory() {
  local table="$1/share/sources/$2/SOURCES.tsv" unit="$2"
  awk -F '\t' -v unit="$unit" '
    NR == 1 { if ($0 != "payload\tname\tversion\tsource\tintegrity\tlicense_directory") exit 1; next }
    NF != 6 || seen[$1 FS $2]++ { exit 1 }
    unit == "runtime" {
      if ($2 == "guest-runtime" || $2 == "accelerator") {
        if ($1 != "bin/sandbox-runtime.bundle,bin/flatten-ctl") exit 1
        next
      }
      if ($2 == "sandboxer") {
        if ($1 != "bin/sandbox-runtime.bundle:/sbin/init") exit 1
        next
      }
      if ($2 == "envd" || $2 == "github.com/e2b-dev/infra/packages/shared") {
        if ($1 != "bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd") exit 1
        next
      }
      if ($2 == "erofs-utils" || $2 == "guest-runtime-erofs-patches") {
        if ($1 != "bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs") exit 1
        next
      }
      if ($2 == "Go toolchain") {
        if ($1 != "bin/flatten-ctl" && $1 != "bin/sandbox-runtime.bundle:/sbin/init" &&
            $1 != "bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd") exit 1
        next
      }
    }
    unit == "vmlinux" && ($2 == "linux" || $2 == "guest-runtime-kernel-inputs") {
      if ($1 != "bin/vmlinux") exit 1
      next
    }
    $2 ~ /^system:/ {
      name=substr($2, 8)
      if (unit == "runtime") {
        if ($1 != "bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs" ||
            name !~ /^[A-Za-z0-9._+-]+[.](a|o)$/) exit 1
      } else {
        exit 1
      }
      if ($6 != "share/licenses/" unit "/system/" name || $3 !~ /^[A-Za-z0-9.+:~_-]+$/) exit 1
      # The existing runner source-build catalog records tarball/SRPM inputs
      # instead of a binary package owner; preserve that collection format.
      if (name == "libuuid.a" && split($5, fields, ";") == 3) {
        if ($4 !~ /^https:\/\/[^[:space:]]+[.]src[.]rpm$/ ||
            fields[1] !~ /^sha256:/ || fields[2] !~ /^tarball-sha256:/ || fields[3] !~ /^srpm-sha256:/) exit 1
        for (i=1; i<=3; i++) {
          digest=fields[i]; sub(/^[^:]+:/, "", digest)
          if (length(digest) != 64 || digest ~ /[^0-9a-f]/) exit 1
        }
        next
      }
      if (split($5, fields, ";") != 2 || fields[1] !~ /^sha256:/ || fields[2] !~ /^package:/) exit 1
      digest=substr(fields[1], 8); package=substr(fields[2], 9)
      if (length(digest) != 64 || digest ~ /[^0-9a-f]/ || package !~ /^[A-Za-z0-9][A-Za-z0-9.+_-]*$/) exit 1
      if ($4 ~ /^deb-source:/) {
        if (package !~ /^[a-z0-9][a-z0-9+.-]*$/ || $4 != "deb-source:" package "@" $3) exit 1
      } else if ($4 ~ /^rpm-source:[A-Za-z0-9][A-Za-z0-9.+:~_-]*[.](no)?src[.]rpm$/) {
        suffix="-" $3 ".src.rpm"; alternate="-" $3 ".nosrc.rpm"
        if (substr($4, length($4)-length(suffix)+1) != suffix &&
            substr($4, length($4)-length(alternate)+1) != alternate) exit 1
      } else exit 1
      next
    }
    { exit 1 }
  ' "$table" || fail "unrecognized or inconsistent source inventory record"
}

prepare_runtime_verifier() {
  # Use trusted host readers; never run a binary supplied by the archive.
  RUNTIME_FSCK="$(command -v fsck.erofs)" \
    || fail "Runtime validation requires fsck.erofs on PATH"
  RUNTIME_DUMP="$(command -v dump.erofs)" \
    || fail "Runtime validation requires dump.erofs on PATH"
}

validate_runtime_payloads() {
  local extract="$1" payloads="$WORK/runtime-payloads" sandboxer_sha binary package module info
  prepare_runtime_verifier
  python3 "$ROOT/scripts/release-runtime-payloads.py" \
    "$extract/bin/sandbox-runtime.bundle" "$RUNTIME_FSCK" "$RUNTIME_DUMP" "$payloads" \
    || fail "Runtime embedded payload verification failed"
  cmp -s "$payloads/mkfs.erofs" "$extract/bin/mkfs.erofs" \
    || fail "embedded mkfs.erofs differs from the shipped host payload"
  cmp -s "$payloads/flatten-ctl" "$extract/bin/flatten-ctl" \
    || fail "embedded flatten-ctl differs from the verified outer payload"
  sandboxer_sha="$(awk -F '\t' '$2 == "sandboxer" { sub(/^git:/, "", $5); print $5 }' \
    "$extract/share/sources/runtime/SOURCES.tsv")"
  release_materials_require_go_revision "$payloads/init" "$sandboxer_sha"
  for binary in init envd; do
    case "$binary" in
      init) module=github.com/kuasar-sandbox/sandboxer; package="$module/cmd/sandbox-init" ;;
      envd) module=github.com/e2b-dev/infra/packages/envd; package="$module" ;;
    esac
    info="$(go version -m "$payloads/$binary" 2>/dev/null)" || fail "embedded Go build info is missing"
    awk -F '\t' -v module="$module" -v package="$package" '
      $2 == "path" { paths++; if ($3 != package) bad=1 }
      $2 == "mod" { modules++; if ($3 != module) bad=1 }
      $2 == "build" && $3 ~ /^GOOS=/ { os++; if ($3 != "GOOS=linux") bad=1 }
      $2 == "build" && $3 ~ /^GOARCH=/ { arch++; if ($3 != "GOARCH=amd64") bad=1 }
      END { exit bad || paths != 1 || modules != 1 || os != 1 || arch != 1 }
    ' <<< "$info" || fail "embedded $binary has the wrong Go main identity or target"
  done
  (
    local validation="$WORK/runtime-records" payload binary
    release_materials_init "$validation/stage" "$validation/materials" runtime
    for binary in init envd; do
      case "$binary" in
        init) payload=bin/sandbox-runtime.bundle:/sbin/init ;;
        envd) payload=bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd ;;
      esac
      release_materials_add_go_binary "$payloads/$binary" "$payload"
    done
    LC_ALL=C sort -u "$validation/materials/go-build-info" > "$validation/expected"
    awk -F '\t' 'NR > 1 && $1 ~ /^bin\/sandbox-runtime[.]bundle:/ { print }' \
      "$extract/share/sources/runtime/GO-BUILD-INFO.tsv" | LC_ALL=C sort -u > "$validation/actual"
    cmp -s "$validation/expected" "$validation/actual" \
      || fail "Go build records differ from embedded Runtime payloads"
  ) || return 1
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
  local selected_sha="${SOURCE_SHA:-}" selected_url="" selected_integrity=""
  if [ "$kind" = runtime ]; then
    check_go_binary "$extract/bin/flatten-ctl"
    if [ -z "$selected_sha" ]; then
      selected_sha="$(go version -m "$extract/bin/flatten-ctl" | \
        awk -F '\t' '$2 == "build" && $3 ~ /^vcs.revision=/ {print substr($3, 14)}')"
    fi
    [[ "$selected_sha" =~ ^[0-9a-f]{40}$ ]] || fail "flatten-ctl must bind its full selected source commit"
    release_materials_require_go_revision "$extract/bin/flatten-ctl" "$selected_sha"
  elif [ -z "$selected_sha" ]; then
    selected_sha="$(awk -F '\t' '$1 == "bin/vmlinux" && $2 == "guest-runtime-kernel-inputs" {
      split($5, fields, ";"); sub(/^git:/, "", fields[1]); print fields[1]
    }' "$extract/share/sources/vmlinux/SOURCES.tsv")"
  fi
  validate_source_inventory "$extract" "$kind"
  release_materials_validate "$extract" "$kind"
  if [ -n "$selected_sha" ]; then
    [[ "$selected_sha" =~ ^[0-9a-f]{40}$ ]] || fail "SOURCE_SHA must be a full lowercase commit"
    selected_url="https://github.com/kuasar-sandbox/guest-runtime/commit/$selected_sha"
    selected_integrity="git:$selected_sha"
  fi
  case "$kind" in
    runtime)
      release_materials_require_source "$extract" "$kind" 'bin/sandbox-runtime.bundle,bin/flatten-ctl' 'guest-runtime' "$version" "$selected_url" "$selected_integrity"
      release_materials_require_source "$extract" "$kind" 'bin/sandbox-runtime.bundle:/sbin/init' sandboxer ""
      release_materials_require_source "$extract" "$kind" 'bin/sandbox-runtime.bundle,bin/flatten-ctl' accelerator ""
      release_materials_require_source "$extract" "$kind" 'bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd' 'envd' "2026.22" \
        'https://github.com/e2b-dev/runtime/archive/refs/tags/2026.22.tar.gz' \
        'sha256:8f074b23dcb2c9db48f8e674a8ab8082b54a16088fb29b66376178feeb7fe040'
      release_materials_require_source "$extract" "$kind" 'bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd' 'github.com/e2b-dev/infra/packages/shared' "2026.22" \
        'https://github.com/e2b-dev/runtime/archive/refs/tags/2026.22.tar.gz' \
        'sha256:8f074b23dcb2c9db48f8e674a8ab8082b54a16088fb29b66376178feeb7fe040'
      release_materials_require_source "$extract" "$kind" 'bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs' 'erofs-utils' "v1.9.1" \
        'https://github.com/erofs/erofs-utils/archive/refs/tags/v1.9.1.tar.gz' \
        'sha256:a9ef5ab67c4b8d2d3e9ed71f39cd008bda653142a720d8a395a36f1110d0c432'
      release_materials_require_source "$extract" "$kind" \
        'bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs' \
        guest-runtime-erofs-patches "$version" \
        "https://github.com/kuasar-sandbox/guest-runtime/tree/$selected_sha/native-deps/deps/erofs-patches" \
        "git:$selected_sha"
      [ -s "$extract/share/sources/runtime/erofs-patches/series" ] \
        || fail "Runtime source material is missing the erofs patch series"
      local erofs_patch
      while IFS= read -r erofs_patch || [ -n "$erofs_patch" ]; do
        case "$erofs_patch" in ''|'#'*) continue ;; esac
        [[ "$erofs_patch" =~ ^[A-Za-z0-9._-]+\.patch$ ]] \
          && [ -s "$extract/share/sources/runtime/erofs-patches/$erofs_patch" ] \
          || fail "Runtime source material is missing an erofs patch from its series"
      done < "$extract/share/sources/runtime/erofs-patches/series"
      release_materials_require_go_key "$extract" "$kind" 'bin/flatten-ctl'
      release_native_validate_erofs_inventory "$extract"
      release_materials_require_source "$extract" "$kind" \
        'bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs' 'system:libc.a' ""
      release_materials_require_source "$extract" "$kind" \
        'bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs' 'system:libuuid.a' ""
      release_materials_require_go_key "$extract" "$kind" 'bin/sandbox-runtime.bundle:/sbin/init'
      release_materials_require_go_key "$extract" "$kind" 'bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd'
      [ -f "$extract/bin/sandbox-runtime.bundle" ] \
        || fail "$archive is missing bin/sandbox-runtime.bundle"
      [ -x "$extract/bin/flatten-ctl" ] || fail "$archive is missing bin/flatten-ctl"
      [ -x "$extract/bin/mkfs.erofs" ] || fail "$archive is missing bin/mkfs.erofs"
      check_go_binary "$extract/bin/flatten-ctl"
      validate_runtime_payloads "$extract"
      ;;
    vmlinux)
      [ -f "$extract/bin/vmlinux" ] || fail "$archive is missing bin/vmlinux"
      local linux_license_sha
      linux_license_sha="$(sha256sum "$extract/share/licenses/vmlinux/linux/COPYING" | awk '{print $1}')"
      release_materials_require_source "$extract" "$kind" 'bin/vmlinux' 'linux' "6.1.169" \
        'https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.169.tar.gz' \
        'sha256:ab28b4ca2a2eca38b3da9aa33b231288168c3560bbc866359045f1c8f4d48d94'
      if [ -n "$selected_sha" ]; then
        selected_url="https://github.com/kuasar-sandbox/guest-runtime/tree/$selected_sha/native-deps/deps"
        selected_integrity="git:$selected_sha;linux-copying-sha256:$linux_license_sha"
      fi
      release_materials_require_source "$extract" "$kind" 'bin/vmlinux' 'guest-runtime-kernel-inputs' "$version" "$selected_url" "$selected_integrity"
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
      local sandboxer_source accelerator_source envd_source erofs_source
      local sandbox_init_bin envd_bin sandboxer_version accelerator_version
      local sandboxer_sha accelerator_sha patch_file
      sandboxer_source="${RELEASE_SANDBOXER_SOURCE_DIR:-$ROOT/../sandboxer}"
      accelerator_source="${RELEASE_ACCELERATOR_SOURCE_DIR:-$ROOT/../accelerator}"
      envd_source="${RELEASE_ENVD_SOURCE_DIR:-${ENVD_SRC:-$ROOT/native-deps/build/src/e2b-infra}}"
      erofs_source="${RELEASE_EROFS_SOURCE_DIR:-$ROOT/native-deps/build/$arch/src/erofs-utils}"
      sandbox_init_bin="${RELEASE_SANDBOX_INIT_BIN:-$sandboxer_source/bin/$arch/sandbox-init}"
      envd_bin="${RELEASE_ENVD_BIN:-$native_bin_dir/envd}"
      sandboxer_version="${RELEASE_SANDBOXER_VERSION:-${SANDBOXER_VERSION:-}}"
      accelerator_version="${RELEASE_ACCELERATOR_VERSION:-${ACCELERATOR_VERSION:-}}"
      for value in "$sandboxer_version" "$accelerator_version"; do
        [[ "$value" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-preview\.[0-9]{8}(\.[1-9][0-9]*)?)?$ ]] \
          || fail "runtime internal dependency versions must identify selected component releases"
      done
      sandboxer_sha="$(release_materials_resolve_git_source "$sandboxer_source" \
        "${RELEASE_SANDBOXER_SOURCE_SHA:-}" sandboxer)"
      accelerator_sha="$(release_materials_resolve_git_source "$accelerator_source" \
        "${RELEASE_ACCELERATOR_SOURCE_SHA:-}" accelerator)"
      record_release_go_contexts "$ROOT" "$sandboxer_source" "$envd_source"
      copy_external_file "$bin_dir/sandbox-runtime.bundle" bin/sandbox-runtime.bundle
      copy_executable "$bin_dir/flatten-ctl" bin/flatten-ctl
      copy_executable "$native_bin_dir/mkfs.erofs" bin/mkfs.erofs
      check_go_binary "$STAGE/bin/flatten-ctl"
      release_materials_require_go_revision "$STAGE/bin/flatten-ctl" "$project_sha"
      accelerator_version="$(release_materials_git_version "$accelerator_source" "$accelerator_version" "$accelerator_sha")"
      sandboxer_version="$(release_materials_git_version "$sandboxer_source" "$sandboxer_version" "$sandboxer_sha")"
      release_materials_copy_licenses "$sandboxer_source" sandboxer
      release_materials_copy_licenses "$accelerator_source" accelerator
      release_materials_copy_licenses "$envd_source" envd
      release_materials_copy_licenses "$envd_source" \
        go/github.com/e2b-dev/infra/packages/shared@v0.0.0
      release_materials_copy_licenses "$erofs_source" erofs-utils
      release_native_erofs_inputs "$erofs_source/mkfs/mkfs.erofs.map" "$erofs_source" \
        'bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs'
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
        'https://github.com/e2b-dev/runtime/archive/refs/tags/2026.22.tar.gz' \
        'sha256:8f074b23dcb2c9db48f8e674a8ab8082b54a16088fb29b66376178feeb7fe040' envd
      release_materials_record_source 'bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd' \
        github.com/e2b-dev/infra/packages/shared 2026.22 \
        'https://github.com/e2b-dev/runtime/archive/refs/tags/2026.22.tar.gz' \
        'sha256:8f074b23dcb2c9db48f8e674a8ab8082b54a16088fb29b66376178feeb7fe040' \
        go/github.com/e2b-dev/infra/packages/shared@v0.0.0
      release_materials_record_source 'bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs' erofs-utils v1.9.1 \
        'https://github.com/erofs/erofs-utils/archive/refs/tags/v1.9.1.tar.gz' \
        'sha256:a9ef5ab67c4b8d2d3e9ed71f39cd008bda653142a720d8a395a36f1110d0c432' erofs-utils
      for patch_file in "$ROOT"/native-deps/deps/erofs-patches/*; do
        copy_file "${patch_file#"$ROOT/"}" "share/sources/runtime/erofs-patches/${patch_file##*/}"
      done
      release_materials_record_source 'bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs' \
        guest-runtime-erofs-patches "$version" \
        "https://github.com/kuasar-sandbox/guest-runtime/tree/$project_sha/native-deps/deps/erofs-patches" \
        "git:$project_sha" erofs-utils
      release_materials_add_go_binary "$STAGE/bin/flatten-ctl" bin/flatten-ctl
      release_materials_add_go_binary "$sandbox_init_bin" bin/sandbox-runtime.bundle:/sbin/init
      release_materials_add_go_binary "$envd_bin" bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd
      ;;
    vmlinux)
      local linux_source linux_license_sha
      copy_external_file "$native_bin_dir/vmlinux" bin/vmlinux
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
  (cd "$output/assets" && sha256sum "$archive" > SHA256SUMS)
  cat > "$output/release-notes.md" <<EOF
$kind $version for Linux $arch.

Extract the archive into a Kuasar Sandbox deployment root and verify it with \`SHA256SUMS\`. Documentation and E2E suites from this exact tag are collected by the aggregate platform release.
EOF
  SOURCE_SHA="$project_sha" validate_bundle "$kind" "$version" "$arch" "$output"
  echo "==> prepared $output for $version"
}

command -v go >/dev/null || fail "go is required"
case "${1:-}" in
  archive-name) shift; [ "$#" -eq 3 ] || fail "usage: release.sh archive-name <runtime|vmlinux> <version> <arch>"; archive_name "$@" ;;
  package) shift; package_release "$@" ;;
  validate) shift; validate_bundle "$@" ;;
  *) fail "usage: release.sh <archive-name|package|validate> ..." ;;
esac
