#!/usr/bin/env bash
#
# Build mkfs.erofs + fsck.erofs (no compression, no fuse) into $BINDIR.
# mkfs.erofs sits alongside flatten-ctl (it locates mkfs.erofs via its own binary
# directory when MKFS_EROFS_PATH is unset); fsck.erofs stays in the native
# dependency build output as a source-tree diagnostic/test helper.
#
# Both are TARGET-ARCH binaries (not host tools). Component release packages only
# need mkfs.erofs; cross-compilation uses CROSS_PREFIX for the C toolchain.
#
# STATIC linking is required, not cosmetic: mkfs.erofs rides the builder guest
# runtime (/opt/sandbox-runtime/bin, projected into ANY app rootfs — empty ones
# included), where no dynamic loader exists. Needs libuuid.a (Debian/Ubuntu:
# uuid-dev). mkfs/fsck do no NSS lookups, so a glibc-static binary is safe.
#
# Inputs (env):
#   EROFS_TARBALL         URL or local path; supports "url#filename" form.
#                         Default: erofs/erofs-utils v1.9.1 github archive.
#   EROFS_TARBALL_SHA256  Expected SHA256 (default: pinned v1.9.1 archive).
#   BUILD_DIR             Per-arch build directory (e.g. build/x86_64).
#                         Source is extracted under $BUILD_DIR/src/erofs-utils
#                         (per-arch — autotools doesn't support shared src).
#   BINDIR                Per-arch bin directory (e.g. bin/x86_64).
#   TARBALL_CACHE         Optional shared tarball cache (default $BUILD_DIR/tarball).
#   CROSS_PREFIX          Optional GNU-triple prefix. Empty for native builds.
#
# Idempotent only when source, patches and build recipe match both outputs.
# JOBS limits parallel compilation (default: 2).

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${EROFS_TARBALL:=https://codeload.github.com/erofs/erofs-utils/tar.gz/refs/tags/v1.9.1#erofs-utils-v1.9.1.tar.gz}"
: "${EROFS_TARBALL_SHA256:=a9ef5ab67c4b8d2d3e9ed71f39cd008bda653142a720d8a395a36f1110d0c432}"
: "${BUILD_DIR:=$(pwd)/build}"
: "${BINDIR:=$(pwd)/bin}"
: "${CROSS_PREFIX:=}"
: "${JOBS:=2}"
[[ "$JOBS" =~ ^[1-9][0-9]*$ ]] || die "JOBS must be a positive integer"

require_cmd sha256sum patch
tarball="$(resolve_tarball "$EROFS_TARBALL" "$EROFS_TARBALL_SHA256")"
# shellcheck disable=SC1091
source "$script_dir/erofs-recipe.sh"
recipe="$(erofs_recipe_digest)"
src_dir="$BUILD_DIR/src/erofs-utils"
recipe_stamp="$BINDIR/.erofs-recipe"

out_mkfs="$BINDIR/mkfs.erofs"
out_fsck="$BINDIR/fsck.erofs"
if [ -x "$out_mkfs" ] && [ -x "$out_fsck" ] \
    && [ -f "$recipe_stamp" ] && [ -f "$src_dir/.erofs-recipe" ] \
    && [ "$(cat "$recipe_stamp")" = "$recipe" ] \
    && [ "$(cat "$src_dir/.erofs-recipe")" = "$recipe" ]; then
    log "already built with matching erofs recipe: $out_mkfs + $out_fsck"
    exit 0
fi
rm -f "$recipe_stamp" "$src_dir/.extracted"

require_cmd autoreconf make tar pkg-config
if [ -n "$CROSS_PREFIX" ]; then
    require_cmd "${CROSS_PREFIX}gcc" "${CROSS_PREFIX}g++"
else
    require_cmd g++ gcc
fi

# erofs-utils v1.9.1 has a hard libuuid dependency at configure time
# (autotools PKG_CHECK_MODULES, no --without-uuid escape hatch). For
# cross-compilation, the host's libuuid is wrong-arch — Debian's
# multi-arch must provide a target-arch libuuid + uuid-dev. Pre-flight
# this so the user gets an actionable hint instead of a wall of
# autoconf output 30 seconds into the build.
if [ -n "$CROSS_PREFIX" ]; then
    case "$TARGET_ARCH" in
        x86_64)  dpkg_arch=amd64 ;;
        aarch64) dpkg_arch=arm64 ;;
        *)       dpkg_arch="$TARGET_ARCH" ;;
    esac
    if command -v dpkg-query >/dev/null 2>&1; then
        if ! dpkg-query -W -f '${db:Status-Status}\n' "uuid-dev:$dpkg_arch" 2>/dev/null | grep -q '^installed$'; then
            die "cross-compiling erofs-utils to $TARGET_ARCH requires multi-arch libuuid headers (Debian/Ubuntu).
Install:
  sudo dpkg --add-architecture $dpkg_arch
  sudo apt update
  sudo apt install libuuid1:$dpkg_arch uuid-dev:$dpkg_arch"
        fi
    fi
fi

# autotools requires in-source build (no out-of-source support); use a
# per-arch source tree under $BUILD_DIR/src/erofs-utils so x86_64 and
# aarch64 builds don't collide.
extract_tarball "$tarball" "$src_dir" >/dev/null
erofs_apply_patches

log "autoreconf (erofs-utils)"
(cd "$src_dir" && ./autogen.sh >/dev/null 2>&1 || autoreconf -i)

# Cross-compile arguments for autotools configure.
configure_cross_args=()
configure_env=()
if [ -n "$CROSS_PREFIX" ]; then
    # Strip the trailing dash from CROSS_PREFIX to form --host triple.
    host_triple="${CROSS_PREFIX%-}"
    # `gcc -dumpmachine` already returns a complete triple (e.g.
    # "x86_64-linux-gnu"); use as-is. Fall back to uname-derived
    # triple only when the host gcc is missing.
    build_triple="$(gcc -dumpmachine 2>/dev/null || echo "$(uname -m)-linux-gnu")"
    # configure --host tells autotools to use ${host_triple}-gcc etc;
    # for safety also export CC/CXX/AR/STRIP explicitly.
    configure_cross_args+=(
        --host="$host_triple"
        --build="$build_triple"
    )
    configure_env=(
        "CC=${CROSS_PREFIX}gcc"
        "CXX=${CROSS_PREFIX}g++"
        "AR=${CROSS_PREFIX}ar"
        "STRIP=${CROSS_PREFIX}strip"
        "RANLIB=${CROSS_PREFIX}ranlib"
    )
    log "cross-compile mode: --host=$host_triple --build=$build_triple"
fi

log "configure (no compression, no fuse)"
(cd "$src_dir" && env "${configure_env[@]}" ./configure \
    "${configure_cross_args[@]}" \
    --disable-lz4 \
    --disable-lzma \
    --without-zlib \
    --without-libzstd \
    --without-libdeflate \
    --without-xxhash \
    --without-libcurl \
    --without-openssl \
    --without-libxml2 \
    --without-json-c \
    --without-libnl3 \
    --disable-multithreading)

log "make mkfs.erofs + fsck.erofs (mkfs + fsck subdirs; skips mount/dump/fuse)"
# Build lib first (mkfs/fsck depend on liberofs.a), then the two subdirs we ship.
# Avoids mount.erofs (pthread link bug in v1.9.1 when multithreading is disabled)
# and other subdirs we don't need.
#
# LDFLAGS=-all-static at make time (not configure: gcc rejects it in configure
# tests): the libtool link-mode flag for a fully static EXECUTABLE — plain
# -static is consumed by libtool itself (= "prefer .a of libtool libs") and
# never reaches the compiler driver.
make -C "$src_dir/lib"  -j"$JOBS"
make -C "$src_dir/mkfs" -j"$JOBS" \
    LDFLAGS="-all-static -Wl,-Map,$src_dir/mkfs/mkfs.erofs.map"
make -C "$src_dir/fsck" -j"$JOBS" LDFLAGS="-all-static"

mkdir -p "$BINDIR"
cp "$src_dir/mkfs/mkfs.erofs" "$out_mkfs"
cp "$src_dir/fsck/fsck.erofs" "$out_fsck"
chmod +x "$out_mkfs" "$out_fsck"
printf '%s\n' "$recipe" > "$src_dir/.erofs-recipe"
printf '%s\n' "$recipe" > "$recipe_stamp"

log "built $out_mkfs + $out_fsck"
# When cross-compiling, --help on the target binary won't run on the host;
# only run it for native builds.
if [ -z "$CROSS_PREFIX" ]; then
    "$out_mkfs" --help 2>&1 | head -2 || true
    "$out_fsck" --help 2>&1 | head -2 || true
else
    file "$out_mkfs" "$out_fsck" 2>&1 | head -2 || true
fi
