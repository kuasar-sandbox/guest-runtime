#!/usr/bin/env bash
#
# Build a static librocksdb.a (no compression, no shared lib) into
# $BUILD_DIR/rocksdb/{lib,include}.
#
# Inputs (env):
#   ROCKSDB_TARBALL         URL or local path; supports "url#filename" form.
#                           Default: facebook/rocksdb v9.7.4 github archive.
#   ROCKSDB_TARBALL_SHA256  Optional expected SHA256. Empty → skip verify.
#   BUILD_DIR               Absolute path to the per-arch build directory
#                           (e.g. build/x86_64). librocksdb.a lands at
#                           $BUILD_DIR/rocksdb/lib/librocksdb.a.
#   TARBALL_CACHE           Optional shared tarball cache (default $BUILD_DIR/tarball).
#                           Multi-arch Makefile sets this to build/tarball.
#   TARGET_ARCH             Optional, used to pick CMake's SYSTEM_PROCESSOR
#                           when cross-compiling.
#   CROSS_PREFIX            Optional GNU-triple prefix (e.g. "aarch64-linux-gnu-").
#                           Empty for native builds.
#
# Idempotent: if the output librocksdb.a already exists it exits 0 without
# rebuilding. Delete $BUILD_DIR/rocksdb/ to force.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${ROCKSDB_TARBALL:=https://github.com/facebook/rocksdb/archive/refs/tags/v9.7.4.tar.gz#rocksdb-9.7.4.tar.gz}"
: "${ROCKSDB_TARBALL_SHA256:=}"
: "${BUILD_DIR:=$(pwd)/build}"
: "${TARGET_ARCH:=$(uname -m)}"
: "${CROSS_PREFIX:=}"

prefix="$BUILD_DIR/rocksdb"
out_lib="$prefix/lib/librocksdb.a"
if [ -f "$out_lib" ]; then
    log "already built: $out_lib (delete $prefix/ to force rebuild)"
    exit 0
fi

require_cmd cmake make tar
if [ -n "$CROSS_PREFIX" ]; then
    require_cmd "${CROSS_PREFIX}gcc" "${CROSS_PREFIX}g++"
else
    require_cmd g++
fi

tarball="$(resolve_tarball "$ROCKSDB_TARBALL" "$ROCKSDB_TARBALL_SHA256")"
# rocksdb source is arch-neutral; share across architectures by extracting
# under build/src/rocksdb (sibling of per-arch build/$ARCH/). The cmake
# build directory is per-arch under $prefix so object files don't collide.
shared_src_root="$(dirname "$BUILD_DIR")"   # = build/
src_dir="$shared_src_root/src/rocksdb"
extract_tarball "$tarball" "$src_dir" >/dev/null

# Out-of-source build: object files under $prefix/build, install into
# $prefix/{lib,include}. Keeps source tree clean and per-arch builds
# isolated.
cmake_build="$prefix/build"
rm -rf "$cmake_build"
mkdir -p "$cmake_build"

# When cross-compiling, hand cmake the target compilers + system processor
# so it generates the right link line. SYSTEM_NAME=Linux (we always target
# Linux); SYSTEM_PROCESSOR matches uname -m on the target.
cmake_cross_args=()
if [ -n "$CROSS_PREFIX" ]; then
    cmake_cross_args+=(
        -DCMAKE_SYSTEM_NAME=Linux
        -DCMAKE_SYSTEM_PROCESSOR="$TARGET_ARCH"
        -DCMAKE_C_COMPILER="${CROSS_PREFIX}gcc"
        -DCMAKE_CXX_COMPILER="${CROSS_PREFIX}g++"
        -DCMAKE_AR="$(command -v "${CROSS_PREFIX}ar" || echo "${CROSS_PREFIX}ar")"
        -DCMAKE_RANLIB="$(command -v "${CROSS_PREFIX}ranlib" || echo "${CROSS_PREFIX}ranlib")"
    )
    log "cross-compile mode: TARGET_ARCH=$TARGET_ARCH CROSS_PREFIX=$CROSS_PREFIX"
fi

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
    -DFAIL_ON_WARNINGS=OFF \
    "${cmake_cross_args[@]}"

log "building librocksdb.a (this takes several minutes)"
cmake --build "$cmake_build" -j"$(nproc)" --target rocksdb

mkdir -p "$prefix/lib" "$prefix/include"
cp "$cmake_build/librocksdb.a" "$prefix/lib/"
cp -r "$src_dir/include/rocksdb" "$prefix/include/"

log "built $out_lib ($(du -h "$out_lib" | cut -f1))"
