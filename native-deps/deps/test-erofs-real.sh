#!/usr/bin/env bash
set -euo pipefail
script_dir="$(cd "$(dirname "$0")" && pwd)"
: "${BUILD_DIR:?}" "${BINDIR:?}" "${EROFS_TARBALL:?}" "${EROFS_TARBALL_SHA256?}"
source "$script_dir/common.sh"
[ -z "${CROSS_PREFIX:-}" ] || die 'run the full recipe/root regression on the native architecture'
mkdir -p "$BUILD_DIR/tests/erofs-pristine-bin"
work="$(mktemp -d "$BUILD_DIR/tests/pristine.XXXXXX")"
trap 'rm -rf "$work"' EXIT
case "$EROFS_TARBALL" in
    http://*|https://*) archive="$(resolve_tarball "$EROFS_TARBALL" "$EROFS_TARBALL_SHA256")" ;;
    *) archive="$(realpath -e "$EROFS_TARBALL")" ;;
esac
extract_tarball "$archive" "$work/src" >/dev/null
(
    cd "$work/src"
    ./autogen.sh
    ./configure --disable-lz4 --disable-lzma --without-zlib --without-libzstd \
        --without-libdeflate --without-xxhash --without-libcurl --without-openssl \
        --without-libxml2 --without-json-c --without-libnl3 --disable-multithreading
    make -j"${EROFS_BUILD_JOBS:-2}" -C lib
    for tool in mkfs fsck dump; do
        make -j"${EROFS_BUILD_JOBS:-2}" -C "$tool" LDFLAGS="${LDFLAGS:-} -all-static"
    done
) > "$BUILD_DIR/tests/pristine-build.log" 2>&1 || { cat "$BUILD_DIR/tests/pristine-build.log"; exit 1; }
for tool in mkfs fsck dump; do
    cp "$work/src/$tool/$tool.erofs" "$BUILD_DIR/tests/erofs-pristine-bin/"
done
python3 "$script_dir/test-erofs-images.py" "$BUILD_DIR/tests/erofs-pristine-bin" "$BINDIR" "$work"
