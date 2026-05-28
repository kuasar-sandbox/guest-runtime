#!/usr/bin/env bash
#
# Build a mkfs.erofs binary (no compression, no fuse) into $BINDIR so it
# sits alongside flatten-ctl — flatten-ctl locates mkfs.erofs via its
# own binary directory when MKFS_EROFS_PATH is unset.
#
# mkfs.erofs is treated as a TARGET-ARCH binary (not a host tool): it
# ships in the release tarball next to flatten-ctl for the target it
# runs on. Cross-compilation uses CROSS_PREFIX for the C toolchain.
#
# Inputs (env):
#   EROFS_TARBALL         URL or local path; supports "url#filename" form.
#                         Default: erofs/erofs-utils v1.9.1 github archive.
#   EROFS_TARBALL_SHA256  Optional expected SHA256. Empty → skip verify.
#   BUILD_DIR             Per-arch build directory (e.g. build/x86_64).
#                         Source is extracted under $BUILD_DIR/src/erofs-utils
#                         (per-arch — autotools doesn't support shared src).
#   BINDIR                Per-arch bin directory (e.g. bin/x86_64).
#   TARBALL_CACHE         Optional shared tarball cache (default $BUILD_DIR/tarball).
#   CROSS_PREFIX          Optional GNU-triple prefix. Empty for native builds.
#
# Idempotent: if $BINDIR/mkfs.erofs already exists it exits 0 without
# rebuilding. Delete it to force.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${EROFS_TARBALL:=https://github.com/erofs/erofs-utils/archive/refs/tags/v1.9.1.tar.gz#erofs-utils-v1.9.1.tar.gz}"
: "${EROFS_TARBALL_SHA256:=}"
: "${BUILD_DIR:=$(pwd)/build}"
: "${BINDIR:=$(pwd)/bin}"
: "${CROSS_PREFIX:=}"

out_bin="$BINDIR/mkfs.erofs"
if [ -x "$out_bin" ]; then
    log "already built: $out_bin (delete it to force rebuild)"
    exit 0
fi

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

tarball="$(resolve_tarball "$EROFS_TARBALL" "$EROFS_TARBALL_SHA256")"
# autotools requires in-source build (no out-of-source support); use a
# per-arch source tree under $BUILD_DIR/src/erofs-utils so x86_64 and
# aarch64 builds don't collide.
src_dir="$BUILD_DIR/src/erofs-utils"
extract_tarball "$tarball" "$src_dir" >/dev/null

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

log "make mkfs.erofs (only mkfs subdir; skips mount/fsck/dump/fuse)"
# Build lib first (mkfs depends on liberofs.a), then mkfs only.
# Avoids mount.erofs (pthread link bug in v1.9.1 when multithreading
# is disabled) and other subdirs we don't need.
make -C "$src_dir/lib"  -j"$(nproc)"
make -C "$src_dir/mkfs" -j"$(nproc)"

mkdir -p "$BINDIR"
cp "$src_dir/mkfs/mkfs.erofs" "$out_bin"
chmod +x "$out_bin"

log "built $out_bin"
# When cross-compiling, --help on the target binary won't run on the host;
# only run it for native builds.
if [ -z "$CROSS_PREFIX" ]; then
    "$out_bin" --help 2>&1 | head -3 || true
else
    file "$out_bin" 2>&1 | head -1 || true
fi
