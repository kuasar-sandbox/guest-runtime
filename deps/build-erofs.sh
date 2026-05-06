#!/usr/bin/env bash
#
# Build a mkfs.erofs binary (no compression, no fuse) into $BINDIR so it
# sits alongside flatten-ctl — flatten-ctl locates mkfs.erofs via its
# own binary directory when MKFS_EROFS_PATH is unset.
#
# Inputs (env):
#   EROFS_TARBALL         URL or local path; supports "url#filename" form.
#                         Default: erofs/erofs-utils v1.9.1 github archive.
#   EROFS_TARBALL_SHA256  Optional expected SHA256. Empty → skip verify.
#   BUILD_DIR             Absolute path to the repo's build/ directory.
#                         Default: ./build (resolved from $PWD).
#   BINDIR                Absolute path to the repo's bin/ directory.
#                         Default: ./bin.
#
# Idempotent: if $BINDIR/mkfs.erofs already exists it exits 0 without
# rebuilding. Delete the file (or `make clean-deps`) to force.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${EROFS_TARBALL:=https://github.com/erofs/erofs-utils/archive/refs/tags/v1.9.1.tar.gz#erofs-utils-v1.9.1.tar.gz}"
: "${EROFS_TARBALL_SHA256:=}"
: "${BUILD_DIR:=$(pwd)/build}"
: "${BINDIR:=$(pwd)/bin}"

out_bin="$BINDIR/mkfs.erofs"
if [ -x "$out_bin" ]; then
    log "already built: $out_bin (delete it to force rebuild)"
    exit 0
fi

require_cmd autoreconf make g++ gcc tar pkg-config

tarball="$(resolve_tarball "$EROFS_TARBALL" "$EROFS_TARBALL_SHA256")"
src_dir="$BUILD_DIR/src/erofs-utils"
extract_tarball "$tarball" "$src_dir" >/dev/null

log "autoreconf (erofs-utils)"
(cd "$src_dir" && ./autogen.sh >/dev/null 2>&1 || autoreconf -i)

log "configure (no compression, no fuse)"
(cd "$src_dir" && ./configure \
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
"$out_bin" --help 2>&1 | head -3 || true
