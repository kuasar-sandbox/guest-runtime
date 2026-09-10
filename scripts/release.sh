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

stage_release_source() {
  local source="$1" sha="$2" destination="$3"
  [ ! -e "$destination" ] || fail "fresh release checkout already exists"
  mkdir -p "$destination"
  local -a git_env=(env -i PATH="$PATH" GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null)
  "${git_env[@]}" git -C "$destination" init --quiet --template=
  "${git_env[@]}" git -C "$destination" fetch --quiet --depth=1 "$source" "$sha"
  "${git_env[@]}" git -C "$destination" -c advice.detachedHead=false checkout --quiet --detach "$sha"
}

prepare_release_build_environment() {
  local proxy="${GOPROXY:-https://proxy.golang.org,direct}" route variable value
  local sumdb="${GOSUMDB:-sum.golang.org}" sumdb_identity sumdb_url sumdb_extra
  local toolchain="${GOTOOLCHAIN:-local}"
  local -a routes
  IFS=',|' read -r -a routes <<< "$proxy"
  for route in "${routes[@]}"; do
    case "$route" in direct|off) continue ;; esac
    [[ "$route" == https://?* && "$route" != *[@?#[:space:]]* ]] \
      || fail "release Go proxy routing must use credential-free HTTPS"
  done
  [[ "$sumdb" != *$'\n'* && "$sumdb" != *$'\r'* ]] \
    || fail "release checksum database routing must be a single line"
  read -r sumdb_identity sumdb_url sumdb_extra <<< "$sumdb"
  [[ "$sumdb_identity" =~ ^[A-Za-z0-9._+/:=-]+$ && -z "$sumdb_extra" ]] \
    || fail "invalid release checksum database identity"
  if [ -n "$sumdb_url" ]; then
    [[ "$sumdb_url" == https://?* && "$sumdb_url" != *[@?#[:space:]]* ]] \
      || fail "release checksum database routing must use credential-free HTTPS"
  fi
  [[ "$toolchain" =~ ^(local|auto|path|go[0-9]+\.[0-9]+(\.[0-9]+|beta[0-9]+|rc[0-9]+)?(\+(auto|path))?)$ ]] \
    || fail "invalid release Go toolchain selection"
  mkdir -p "$WORK/go-home" "$WORK/go-cache" "$WORK/go-mod"
  chmod 0700 "$WORK/go-home" "$WORK/go-cache" "$WORK/go-mod"
  RELEASE_BUILD_ENV=(env -i PATH="$PATH" HOME="$WORK/go-home" LANG=C
    GOWORK=off GOENV=off GOFLAGS=-mod=readonly GOPROXY="$proxy" GOSUMDB="$sumdb" GOTOOLCHAIN="$toolchain"
    GOCACHE="$WORK/go-cache" GOMODCACHE="$WORK/go-mod"
    GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null)
  for variable in HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY http_proxy https_proxy all_proxy no_proxy \
    SSL_CERT_FILE SSL_CERT_DIR; do
    value="${!variable:-}"
    [ -n "$value" ] || continue
    case "$variable" in
      HTTP_PROXY|HTTPS_PROXY|ALL_PROXY|http_proxy|https_proxy|all_proxy)
        [[ "$value" != *[@?#[:space:]]* ]] || fail "release build cannot pass an authenticated proxy"
        ;;
    esac
    RELEASE_BUILD_ENV+=("$variable=$value")
  done
  for variable in EROFS_TARBALL EROFS_TARBALL_SHA256 ENVD_TARBALL ENVD_TARBALL_SHA256 LINUX_TARBALL LINUX_TARBALL_SHA256; do
    if [[ -v $variable ]]; then RELEASE_BUILD_ENV+=("$variable=${!variable}"); fi
  done
}

release_runtime_native_cflags() {
  printf '%s\n' "-O2 -g -ffile-prefix-map=$1=/usr/src/kuasar"
}

release_linux_copying_sha() {
  # COPYING from the checksum-pinned Linux 6.1.169 source, independently of
  # downloaded release metadata. Update and verify together with the native pin.
  printf '%s\n' fb5a425bd3b3cd6071a3a9aff9909a859e7c1158d54d32e07658398cd67eb6a0
}

verify_release_go_contexts() {
  local build_root="$1" sandboxer_root="$2" envd_root="$3" context directory info
  for context in runtime sandboxer envd; do
    case "$context" in
      runtime) directory="$build_root" ;;
      sandboxer) directory="$sandboxer_root" ;;
      envd) directory="$envd_root/packages/envd" ;;
    esac
    info="$WORK/go-context-$context.json"
    "${RELEASE_BUILD_ENV[@]}" go -C "$directory" env -json GOROOT GOVERSION GOHOSTOS GOHOSTARCH > "$info"
    RELEASE_MATERIALS_WORK="$WORK/go-before-$context" GOMODCACHE="$WORK/go-mod" \
      release_materials_verify_build_go "$info"
  done
  RELEASE_MATERIALS_GO_ENV="$WORK/go-build-toolchains.json"
  jq -s . "$WORK/go-context-runtime.json" "$WORK/go-context-sandboxer.json" "$WORK/go-context-envd.json" \
    > "$RELEASE_MATERIALS_GO_ENV"
}

stage_release_envd_source() {
  local build_root="$1" destination="$2" tarball_dir="$3" spec checksum
  spec="${ENVD_TARBALL:-$(native_make_default ENVD_TARBALL)}"
  spec="${spec//\\#/\#}"
  checksum="${ENVD_TARBALL_SHA256:-$(native_make_default ENVD_TARBALL_SHA256)}"
  # Reuse the recipe's authenticated tarball/extraction path in this fresh tree.
  # Resolve envd's module before selecting its compiler, not in the parent module.
  "${RELEASE_BUILD_ENV[@]}" bash -s -- "$build_root/native-deps/deps/common.sh" \
    "$destination" "$tarball_dir" "$spec" "$checksum" <<'ENVD_SOURCE'
set -euo pipefail
# shellcheck source=native-deps/deps/common.sh
source "$1"
TARBALL_CACHE="$3"
tarball="$(resolve_tarball "$4" "$5")"
extract_tarball "$tarball" "$2"
ENVD_SOURCE
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

requested_dependency_version() {
  local name="$1" binding="${RELEASE_DEPENDENCIES:-}" entry value result=""
  local -a entries
  [ -n "$binding" ] || return 0 # Standalone source packaging can use untagged commits.
  IFS=, read -r -a entries <<< "$binding"
  [ "${#entries[@]}" -eq 2 ] || fail "Runtime release must bind accelerator and sandboxer"
  for entry in "${entries[@]}"; do
    case "${entry%%=*}" in accelerator|sandboxer) ;; *) fail "unexpected Runtime dependency" ;; esac
    value="${entry#*=}"
    [[ "$value" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-preview\.[0-9]{8})?$ ]] \
      || fail "invalid Runtime dependency version"
    if [ "${entry%%=*}" = "$name" ]; then
      [ -z "$result" ] || fail "duplicate Runtime dependency: $name"
      result="$value"
    fi
  done
  [ -n "$result" ] || fail "missing Runtime dependency: $name"
  printf '%s\n' "$result"
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
  release_materials_require_git_licenses "$extract" "$kind" "$ROOT" "$selected_sha" project
  release_materials_validate "$extract" "$kind"
  if [ -n "$selected_sha" ]; then
    [[ "$selected_sha" =~ ^[0-9a-f]{40}$ ]] || fail "SOURCE_SHA must be a full lowercase commit"
    selected_url="https://github.com/kuasar-sandbox/guest-runtime/commit/$selected_sha"
    selected_integrity="git:$selected_sha"
  fi
  case "$kind" in
    runtime)
      local expected_sandboxer expected_accelerator
      expected_sandboxer="$(requested_dependency_version sandboxer)" || fail "invalid sandboxer release binding"
      expected_accelerator="$(requested_dependency_version accelerator)" || fail "invalid accelerator release binding"
      release_materials_require_source "$extract" "$kind" 'bin/sandbox-runtime.bundle,bin/flatten-ctl' 'guest-runtime' "$version" "$selected_url" "$selected_integrity"
      release_materials_require_source "$extract" "$kind" 'bin/sandbox-runtime.bundle:/sbin/init' 'sandboxer' "$expected_sandboxer"
      release_materials_require_source "$extract" "$kind" 'bin/sandbox-runtime.bundle,bin/flatten-ctl' 'accelerator' "$expected_accelerator"
      release_materials_require_source "$extract" "$kind" 'bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd' 'envd' "2026.22" \
        'https://github.com/e2b-dev/infra/archive/refs/tags/2026.22.tar.gz' \
        'sha256:9e1e81f2963fda1805466c337cd0a33638182a15a66295fd19bba6b9c454d92c'
      release_materials_require_source "$extract" "$kind" 'bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd' 'github.com/e2b-dev/infra/packages/shared' "2026.22" \
        'https://github.com/e2b-dev/infra/archive/refs/tags/2026.22.tar.gz' \
        'sha256:9e1e81f2963fda1805466c337cd0a33638182a15a66295fd19bba6b9c454d92c'
      release_materials_require_source "$extract" "$kind" 'bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs' 'erofs-utils' "v1.9.1" \
        'https://github.com/erofs/erofs-utils/archive/refs/tags/v1.9.1.tar.gz' \
        'sha256:a9ef5ab67c4b8d2d3e9ed71f39cd008bda653142a720d8a395a36f1110d0c432'
      release_materials_require_go "$extract" "$kind" 'bin/flatten-ctl'
      release_materials_require_source "$extract" "$kind" \
        'bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs' 'system:libc.a' ""
      release_materials_require_source "$extract" "$kind" \
        'bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs' 'system:libuuid.a' ""
      release_materials_require_go "$extract" "$kind" 'bin/sandbox-runtime.bundle:/sbin/init'
      release_materials_require_go "$extract" "$kind" 'bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd'
      [ -f "$extract/bin/sandbox-runtime.bundle" ] \
        || fail "$archive is missing bin/sandbox-runtime.bundle"
      [ -x "$extract/bin/flatten-ctl" ] || fail "$archive is missing bin/flatten-ctl"
      [ -x "$extract/bin/mkfs.erofs" ] || fail "$archive is missing bin/mkfs.erofs"
      check_go_binary "$extract/bin/flatten-ctl"
      ;;
    vmlinux)
      [ -f "$extract/bin/vmlinux" ] || fail "$archive is missing bin/vmlinux"
      [ "$(sha256sum "$extract/share/licenses/vmlinux/linux/COPYING" | awk '{print $1}')" = "$(release_linux_copying_sha)" ] \
        || fail "Linux COPYING differs from the pinned source"
      release_materials_require_source "$extract" "$kind" 'bin/vmlinux' 'linux' "6.1.169" \
        'https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.169.tar.gz' \
        'sha256:ab28b4ca2a2eca38b3da9aa33b231288168c3560bbc866359045f1c8f4d48d94'
      if [ -n "$selected_sha" ]; then
        selected_url="https://github.com/kuasar-sandbox/guest-runtime/tree/$selected_sha/native-deps/deps"
        selected_integrity="git:$selected_sha;linux-copying-sha256:$(release_linux_copying_sha)"
      fi
      release_materials_require_source "$extract" "$kind" 'bin/vmlinux' 'guest-runtime-kernel-inputs' "$version" "$selected_url" "$selected_integrity"
      ;;
  esac
}

package_release() {
  [ "$#" -eq 4 ] || fail "usage: release.sh package <runtime|vmlinux> <version> <arch> <output-dir>"
  local kind="$1" version="$2" arch archive output="$4" epoch bin_dir native_bin_dir project_sha value
  local build_workspace="$WORK/build" build_root tarball_dir
  arch="$(normalize_arch "$3")"
  archive="$(archive_name "$kind" "$version" "$arch")"
  if [ -z "$output" ] || [ "$output" = / ] || [ "$output" = . ]; then
    fail "unsafe output directory: $output"
  fi
  [ ! -e "$output" ] || fail "output already exists: $output"
  epoch="${SOURCE_DATE_EPOCH:-0}"
  [[ "$epoch" =~ ^[0-9]+$ ]] || fail "SOURCE_DATE_EPOCH must be an integer"
  validate_native_release_inputs "$kind" "$arch"
  prepare_release_build_environment

  STAGE="$WORK/stage"
  rm -rf "$STAGE"
  mkdir -p "$STAGE"
  project_sha="$(release_materials_resolve_git_source "$ROOT" "${SOURCE_SHA:-}" guest-runtime)"
  build_root="$build_workspace/guest-runtime"
  stage_release_source "$ROOT" "$project_sha" "$build_root"
  bin_dir="$build_root/bin/$arch"
  native_bin_dir="$build_root/native-deps/bin/$arch"
  tarball_dir="$ROOT/native-deps/build/tarball"
  release_materials_init "$STAGE" "$WORK/materials" "$kind"
  release_materials_copy_licenses "$ROOT" project
  case "$kind" in
    runtime)
      local sandboxer_source accelerator_source envd_source erofs_source
      local sandbox_init_bin envd_bin sandboxer_version accelerator_version
      local sandboxer_sha accelerator_sha
      sandboxer_source="${RELEASE_SANDBOXER_SOURCE_DIR:-$ROOT/../sandboxer}"
      accelerator_source="${RELEASE_ACCELERATOR_SOURCE_DIR:-$ROOT/../accelerator}"
      envd_source="$build_root/native-deps/build/src/e2b-infra"
      erofs_source="$build_root/native-deps/build/$arch/src/erofs-utils"
      sandbox_init_bin="$build_workspace/sandboxer/bin/$arch/sandbox-init"
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
      stage_release_source "$sandboxer_source" "$sandboxer_sha" "$build_workspace/sandboxer"
      stage_release_source "$accelerator_source" "$accelerator_sha" "$build_workspace/accelerator"
      stage_release_envd_source "$build_root" "$envd_source" "$tarball_dir"
      verify_release_go_contexts "$build_root" "$build_workspace/sandboxer" "$envd_source"
      # Fresh source/build roots prevent local output and extraction caches from
      # changing the payload while the material record still names pinned inputs.
      "${RELEASE_BUILD_ENV[@]}" \
        CFLAGS="$(release_runtime_native_cflags "$build_workspace")" \
        make --no-print-directory -C "$build_root/native-deps" \
        TARGET_ARCH="$arch" TARBALL_DIR="$tarball_dir" \
        ENVD_SRC="$envd_source" erofs envd
      "${RELEASE_BUILD_ENV[@]}" \
        make --no-print-directory -C "$build_root" \
        TARGET_ARCH="$arch" BUILD_MKFS_EROFS="$native_bin_dir/mkfs.erofs" \
        flatten-ctl sandbox-init sandbox-runtime
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
        'https://github.com/e2b-dev/infra/archive/refs/tags/2026.22.tar.gz' \
        'sha256:9e1e81f2963fda1805466c337cd0a33638182a15a66295fd19bba6b9c454d92c' envd
      release_materials_record_source 'bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd' \
        github.com/e2b-dev/infra/packages/shared 2026.22 \
        'https://github.com/e2b-dev/infra/archive/refs/tags/2026.22.tar.gz' \
        'sha256:9e1e81f2963fda1805466c337cd0a33638182a15a66295fd19bba6b9c454d92c' \
        go/github.com/e2b-dev/infra/packages/shared@v0.0.0
      release_materials_record_source 'bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs' erofs-utils v1.9.1 \
        'https://github.com/erofs/erofs-utils/archive/refs/tags/v1.9.1.tar.gz' \
        'sha256:a9ef5ab67c4b8d2d3e9ed71f39cd008bda653142a720d8a395a36f1110d0c432' erofs-utils
      release_materials_add_go_binary "$STAGE/bin/flatten-ctl" bin/flatten-ctl
      release_materials_add_go_binary "$sandbox_init_bin" bin/sandbox-runtime.bundle:/sbin/init
      release_materials_add_go_binary "$envd_bin" bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/envd
      ;;
    vmlinux)
      local linux_source linux_license_sha
      # Release metadata must not disclose the build account/host or depend on
      # the wall clock. Development builds retain the normal Kbuild defaults.
      "${RELEASE_BUILD_ENV[@]}" env \
        KBUILD_BUILD_USER=kuasar KBUILD_BUILD_HOST=release KBUILD_BUILD_VERSION=1 \
        KBUILD_BUILD_TIMESTAMP="$(LC_ALL=C date -u -d "@$epoch" '+%a %b %e %T UTC %Y')" \
        make --no-print-directory -C "$build_root/native-deps" \
        TARGET_ARCH="$arch" TARBALL_DIR="$tarball_dir" \
        LINUX_BUILD_SRC="$build_root/native-deps/build/src/linux" \
        LINUX_BUILD_OUT="$build_root/native-deps/build/$arch/linux" vmlinux
      copy_external_file "$native_bin_dir/vmlinux" bin/vmlinux
      linux_source="$build_root/native-deps/build/src/linux"
      release_materials_copy_licenses "$linux_source" linux
      linux_license_sha="$(sha256sum "$linux_source/COPYING" | awk '{print $1}')"
      [ "$linux_license_sha" = "$(release_linux_copying_sha)" ] \
        || fail "Linux COPYING differs from the pinned source"
      release_materials_record_source bin/vmlinux linux 6.1.169 \
        'https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.169.tar.gz' \
        'sha256:ab28b4ca2a2eca38b3da9aa33b231288168c3560bbc866359045f1c8f4d48d94' linux
      release_materials_record_source bin/vmlinux guest-runtime-kernel-inputs "$version" \
        "https://github.com/kuasar-sandbox/guest-runtime/tree/$project_sha/native-deps/deps" \
        "git:$project_sha;linux-copying-sha256:$linux_license_sha" project
      ;;
  esac
  GOMODCACHE="$WORK/go-mod" release_materials_finish

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
