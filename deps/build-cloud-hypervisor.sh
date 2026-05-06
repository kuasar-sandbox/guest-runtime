#!/usr/bin/env bash
#
# Multi-stage dispatcher for cloud-hypervisor source-with-patches build.
# Stages are selected via the STAGE env var (default: build):
#
#   STAGE=fetch          extract tarball + git init + tag ch-patches-base
#   STAGE=patches-apply  git am deps/ch-patches/*.patch onto the source tree
#   STAGE=patches-format git format-patch ch-patches-base..HEAD → deps/ch-patches/
#   STAGE=build          cargo build --release --bin cloud-hypervisor → BINDIR
#
# Each stage is idempotent in the safe sense:
#   - fetch skips if the ch-patches-base tag already exists; refuses if a
#     foreign git tree is present without that tag (won't overwrite WIP).
#   - patches-apply requires HEAD == ch-patches-base; otherwise refuses
#     and tells the user to format-extract WIP first, then reset.
#   - patches-format clears stale .patch files before regenerating.
#   - build relies on cargo's incremental compile.
#
# Inputs (env, all optional):
#   CLOUD_HYPERVISOR_TARBALL         URL or local path; supports "url#filename" form.
#                                     Default: cloud-hypervisor v51.1 github archive.
#   CLOUD_HYPERVISOR_TARBALL_SHA256  Optional expected SHA256.
#   BUILD_DIR                         Repo's build/ root.
#   BINDIR                            Repo's bin/.
#   CH_SRC                            Source tree location. Default: $BUILD_DIR/src/cloud-hypervisor.
#   CH_BUILD_OUT                      cargo target dir. Default: $BUILD_DIR/cloud-hypervisor.
#   PATCHES_DIR                       Patch files location. Default: $(pwd)/deps/ch-patches.
#   CH_BASE_TAG                       git tag name for the import baseline. Default: ch-patches-base.
#
# WSL2 users on /mnt/<drive>/ should override CH_SRC + CH_BUILD_OUT to a
# Linux-native filesystem (e.g. ~/ch-build/{src,out}) — DrvFs adds 5-10×
# I/O overhead per small file, and a kernel-grade Rust workspace has
# ~50k files plus a 2 GB cargo target.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${STAGE:=build}"
: "${CLOUD_HYPERVISOR_TARBALL:=https://github.com/cloud-hypervisor/cloud-hypervisor/archive/refs/tags/v51.1.tar.gz#cloud-hypervisor-51.1.tar.gz}"
: "${CLOUD_HYPERVISOR_TARBALL_SHA256:=}"
: "${BUILD_DIR:=$(pwd)/build}"
: "${BINDIR:=$(pwd)/bin}"
: "${CH_SRC:=$BUILD_DIR/src/cloud-hypervisor}"
: "${CH_BUILD_OUT:=$BUILD_DIR/cloud-hypervisor}"
: "${PATCHES_DIR:=$(pwd)/deps/ch-patches}"
: "${CH_BASE_TAG:=ch-patches-base}"

do_fetch() {
    if [ -d "$CH_SRC/.git" ]; then
        if git -C "$CH_SRC" rev-parse --verify "$CH_BASE_TAG" >/dev/null 2>&1; then
            log "$CH_BASE_TAG tag exists at $CH_SRC, skipping fetch"
            return 0
        fi
        die "git tree exists at $CH_SRC but no $CH_BASE_TAG tag — refusing to overwrite. Either tag manually (git -C $CH_SRC tag $CH_BASE_TAG <commit>) or rm -rf $CH_SRC to start fresh."
    fi
    require_cmd tar git
    tarball="$(resolve_tarball "$CLOUD_HYPERVISOR_TARBALL" "$CLOUD_HYPERVISOR_TARBALL_SHA256")"
    extract_tarball "$tarball" "$CH_SRC" >/dev/null
    log "git init + tag $CH_BASE_TAG at $CH_SRC"
    git -C "$CH_SRC" init -q
    git -C "$CH_SRC" -c user.name=deps -c user.email=deps@local add -A
    git -C "$CH_SRC" -c user.name=deps -c user.email=deps@local commit -q -m "import $tarball"
    git -C "$CH_SRC" tag "$CH_BASE_TAG"
}

do_patches_apply() {
    require_cmd git
    [ -d "$CH_SRC/.git" ] || die "no git tree at $CH_SRC; run 'make ch-fetch' first"
    git -C "$CH_SRC" rev-parse --verify "$CH_BASE_TAG" >/dev/null 2>&1 \
        || die "$CH_BASE_TAG tag missing at $CH_SRC"

    local base head
    base=$(git -C "$CH_SRC" rev-parse "$CH_BASE_TAG")
    head=$(git -C "$CH_SRC" rev-parse HEAD)
    if [ "$base" != "$head" ]; then
        die "HEAD ($head) differs from $CH_BASE_TAG ($base). Run 'make ch-patches-format' to save WIP, then 'git -C $CH_SRC reset --hard $CH_BASE_TAG', then re-run."
    fi

    shopt -s nullglob
    local patches=( "$PATCHES_DIR"/*.patch )
    if [ "${#patches[@]}" -eq 0 ]; then
        log "no patches in $PATCHES_DIR — nothing to apply"
        return 0
    fi
    log "applying ${#patches[@]} patch(es) from $PATCHES_DIR"
    git -C "$CH_SRC" -c user.name=deps -c user.email=deps@local am "${patches[@]}"
}

do_patches_format() {
    require_cmd git
    [ -d "$CH_SRC/.git" ] || die "no git tree at $CH_SRC"
    git -C "$CH_SRC" rev-parse --verify "$CH_BASE_TAG" >/dev/null 2>&1 \
        || die "$CH_BASE_TAG tag missing at $CH_SRC"

    mkdir -p "$PATCHES_DIR"
    rm -f "$PATCHES_DIR"/*.patch

    local base head
    base=$(git -C "$CH_SRC" rev-parse "$CH_BASE_TAG")
    head=$(git -C "$CH_SRC" rev-parse HEAD)
    if [ "$base" = "$head" ]; then
        log "HEAD == $CH_BASE_TAG; no commits to extract (cleared $PATCHES_DIR)"
        return 0
    fi
    git -C "$CH_SRC" format-patch "$CH_BASE_TAG..HEAD" -o "$PATCHES_DIR" >/dev/null
    local n
    n=$(find "$PATCHES_DIR" -maxdepth 1 -name '*.patch' | wc -l)
    log "extracted $n patch(es) to $PATCHES_DIR"
}

do_build() {
    require_cmd cargo rustc
    [ -f "$CH_SRC/Cargo.toml" ] || die "no source at $CH_SRC; run 'make ch-fetch' first"
    mkdir -p "$CH_BUILD_OUT"
    log "kernel source: $CH_SRC"
    log "cargo target:  $CH_BUILD_OUT"
    log "cargo build --release --bin cloud-hypervisor (cache hot ≈ seconds; cold ≈ 5-10 min)"
    CARGO_TARGET_DIR="$CH_BUILD_OUT" cargo build --release --locked \
        --manifest-path "$CH_SRC/Cargo.toml" --bin cloud-hypervisor
    mkdir -p "$BINDIR"
    cp "$CH_BUILD_OUT/release/cloud-hypervisor" "$BINDIR/cloud-hypervisor"
    chmod +x "$BINDIR/cloud-hypervisor"
    log "built $BINDIR/cloud-hypervisor ($(du -h "$BINDIR/cloud-hypervisor" | cut -f1))"
    "$BINDIR/cloud-hypervisor" --version 2>&1 | head -1 || true
}

case "$STAGE" in
    fetch)          do_fetch ;;
    patches-apply)  do_patches_apply ;;
    patches-format) do_patches_format ;;
    build)          do_build ;;
    *) die "unknown STAGE='$STAGE' (want fetch|patches-apply|patches-format|build)" ;;
esac
