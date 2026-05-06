#!/usr/bin/env bash
#
# Build a static librocksdb.a (no compression, no shared lib) into
# $BUILD_DIR/rocksdb/{lib,include}.
#
# Inputs (env):
#   ROCKSDB_TARBALL         URL or local path; supports "url#filename" form.
#                           Default: facebook/rocksdb v9.7.4 github archive.
#   ROCKSDB_TARBALL_SHA256  Optional expected SHA256. Empty → skip verify.
#   BUILD_DIR               Absolute path to the repo's build/ directory.
#                           Default: ./build (resolved from $PWD).
#
# Idempotent: if the output librocksdb.a already exists it exits 0 without
# rebuilding. Delete build/rocksdb/ (or run `make clean-deps`) to force.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${ROCKSDB_TARBALL:=https://github.com/facebook/rocksdb/archive/refs/tags/v9.7.4.tar.gz#rocksdb-9.7.4.tar.gz}"
: "${ROCKSDB_TARBALL_SHA256:=}"
: "${BUILD_DIR:=$(pwd)/build}"
BUILD_DIR="$(cd "$(dirname "$BUILD_DIR")" 2>/dev/null && pwd)/$(basename "$BUILD_DIR")" || BUILD_DIR="$BUILD_DIR"

prefix="$BUILD_DIR/rocksdb"
out_lib="$prefix/lib/librocksdb.a"
if [ -f "$out_lib" ]; then
    log "already built: $out_lib (delete build/rocksdb/ to force rebuild)"
    exit 0
fi

require_cmd cmake make g++ tar

tarball="$(resolve_tarball "$ROCKSDB_TARBALL" "$ROCKSDB_TARBALL_SHA256")"
src_dir="$BUILD_DIR/src/rocksdb"
extract_tarball "$tarball" "$src_dir" >/dev/null

cmake_build="$src_dir/_build"
rm -rf "$cmake_build"

log "configuring rocksdb (no compression, static, release)"
cmake -S "$src_dir" -B "$cmake_build" \
    -DCMAKE_BUILD_TYPE=Release \
    -DPORTABLE=1 \
    -DROCKSDB_BUILD_SHARED=OFF \
    -DWITH_TESTS=OFF \
    -DWITH_BENCHMARK_TOOLS=OFF \
    -DWITH_CORE_TOOLS=OFF \
    -DWITH_TOOLS=OFF \
    -DWITH_TRACE_TOOLS=OFF \
    -DWITH_GFLAGS=OFF \
    -DWITH_SNAPPY=OFF \
    -DWITH_LZ4=OFF \
    -DWITH_ZSTD=OFF \
    -DWITH_BZ2=OFF \
    -DWITH_ZLIB=OFF \
    -DWITH_JEMALLOC=OFF \
    -DWITH_LIBURING=OFF \
    -DFAIL_ON_WARNINGS=OFF

log "building librocksdb.a (this takes several minutes)"
cmake --build "$cmake_build" -j"$(nproc)" --target rocksdb

mkdir -p "$prefix/lib" "$prefix/include"
cp "$cmake_build/librocksdb.a" "$prefix/lib/"
cp -r "$src_dir/include/rocksdb" "$prefix/include/"

log "built $out_lib ($(du -h "$out_lib" | cut -f1))"
