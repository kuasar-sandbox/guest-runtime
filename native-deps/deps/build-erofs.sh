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
# included), where no dynamic loader exists. Needs target libuuid.a, libssl.a
# and libcrypto.a (Debian/Ubuntu: uuid-dev, libssl-dev). SHA-256 uses the
# existing upstream OpenSSL EVP backend, including its built-in provider.
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
# Reuse requires both binaries and a matching .erofs-build-inputs stamp.
# The stamp covers this recipe, source pin, flags, tools and selected static
# link inputs, not the entire host filesystem. Delete either binary to force.
# EROFS_BUILD_JOBS optionally bounds make parallelism (default: nproc).

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${EROFS_TARBALL:=https://codeload.github.com/erofs/erofs-utils/tar.gz/refs/tags/v1.9.1#erofs-utils-v1.9.1.tar.gz}"
: "${EROFS_TARBALL_SHA256:=}"
: "${BUILD_DIR:=$(pwd)/build}"
: "${BINDIR:=$(pwd)/bin}"
: "${CROSS_PREFIX:=}"

out_mkfs="$BINDIR/mkfs.erofs"
out_fsck="$BINDIR/fsck.erofs"
stamp="$BINDIR/.erofs-build-inputs"
src_dir="$BUILD_DIR/src/erofs-utils"
cc="${CROSS_PREFIX}gcc"
: "${PKG_CONFIG:=pkg-config}"
require_cmd autoreconf make tar sha256sum "$PKG_CONFIG" "$cc" "${CROSS_PREFIX}g++" \
    "${CROSS_PREFIX}ar" "${CROSS_PREFIX}ranlib" readelf

# Never let cross configure discover host .pc files by default. Explicit
# pkg-config paths/sysroots remain available for non-Debian toolchains.
if [ -n "$CROSS_PREFIX" ]; then
    multiarch="$("$cc" -print-multiarch)"
    sysroot="$("$cc" -print-sysroot)"
    if [ -z "${PKG_CONFIG_LIBDIR+x}" ]; then
        [ -n "$multiarch" ] || die "cross erofs build requires target PKG_CONFIG_LIBDIR"
        export PKG_CONFIG_LIBDIR="${sysroot%/}/usr/lib/$multiarch/pkgconfig:${sysroot%/}/lib/$multiarch/pkgconfig:${sysroot%/}/usr/share/pkgconfig"
    fi
    export PKG_CONFIG_PATH="${PKG_CONFIG_PATH:-}"
    export PKG_CONFIG_SYSROOT_DIR="${PKG_CONFIG_SYSROOT_DIR:-$sysroot}"
fi
export PKG_CONFIG

prerequisite_hint="erofs requires target static libcrypto, libssl and libuuid plus headers and pkg-config metadata.
Debian/Ubuntu: install libssl-dev and uuid-dev for the target architecture
(e.g. libssl-dev:arm64 uuid-dev:arm64 with gcc-aarch64-linux-gnu).
RPM: openssl-devel (openssl-static if split), libuuid-devel and static libc/uuid.
For a cross sysroot, set PKG_CONFIG_LIBDIR and PKG_CONFIG_SYSROOT_DIR to target paths."
"$PKG_CONFIG" --exists openssl uuid || die "$prerequisite_hint"
# Upstream uses the openssl module (libssl + libcrypto), not just libcrypto.
# Supply private static dependencies too, for distributions where these include
# -ldl/-pthread. No configure auto-detection or non-OpenSSL fallback is allowed.
openssl_CFLAGS="$("$PKG_CONFIG" --cflags openssl)"
openssl_LIBS="$("$PKG_CONFIG" --libs --static openssl)"
libuuid_CFLAGS="$("$PKG_CONFIG" --cflags uuid)"
libuuid_LIBS="$("$PKG_CONFIG" --libs --static uuid)"
export openssl_CFLAGS openssl_LIBS libuuid_CFLAGS libuuid_LIBS

mkdir -p "$BUILD_DIR"
probe_dir="$(mktemp -d "$BUILD_DIR/erofs-link.XXXXXX")"
trap 'rm -rf "$probe_dir"' EXIT
cat > "$probe_dir/probe.c" <<'EOF'
#include <openssl/evp.h>
#include <openssl/ssl.h>
#include <uuid/uuid.h>
int main(void)
{
    unsigned char digest[EVP_MAX_MD_SIZE];
    unsigned int size;
    uuid_t uuid;
    SSL_CTX *ssl = SSL_CTX_new(TLS_method());
    int ok = EVP_Digest("abc", 3, digest, &size, EVP_sha256(), 0);
    uuid_clear(uuid);
    SSL_CTX_free(ssl);
    return !(ok && size == 32 && uuid_is_null(uuid));
}
EOF
# Intentional shell word splitting for conventional compiler/pkg-config flags;
# never eval them. The target linker is the architecture/static-archive check.
# shellcheck disable=SC2086
if ! "$cc" ${CPPFLAGS:-} ${CFLAGS:-} $openssl_CFLAGS $libuuid_CFLAGS \
    -c "$probe_dir/probe.c" -o "$probe_dir/probe.o" > "$probe_dir/link.log" 2>&1 \
    || ! "$cc" ${CFLAGS:-} "$probe_dir/probe.o" ${LDFLAGS:-} -static -Wl,-Map,"$probe_dir/probe.map" \
    $openssl_LIBS $libuuid_LIBS ${LIBS:-} -o "$probe_dir/probe" > "$probe_dir/link.log" 2>&1; then
    cat "$probe_dir/link.log" >&2
    die "target static OpenSSL/uuid link check failed. $prerequisite_hint"
fi

# Pinned URLs need no download on a warm binary cache. Local or unpinned
# sources are hashed directly; a same-name local replacement cannot hide behind
# common.sh's filename-based download cache.
tarball=""
case "$EROFS_TARBALL" in
    http://*|https://*) source_digest="$EROFS_TARBALL_SHA256" ;;
    *) tarball="$(realpath -e "$EROFS_TARBALL")"
       source_digest="$(sha256sum "$tarball" | awk '{print $1}')"
       [ -z "$EROFS_TARBALL_SHA256" ] || [ "$source_digest" = "$EROFS_TARBALL_SHA256" ] \
           || die "sha256 mismatch for $tarball" ;;
esac
if [ -z "$source_digest" ]; then
    tarball="$(resolve_tarball "$EROFS_TARBALL" "$EROFS_TARBALL_SHA256")"
    source_digest="$(sha256sum "$tarball" | awk '{print $1}')"
fi
{
    printf '%s\n' "$EROFS_TARBALL" "$source_digest" "$CROSS_PREFIX"
    for name in CFLAGS CPPFLAGS CXXFLAGS LDFLAGS LIBS SOURCE_DATE_EPOCH \
        PKG_CONFIG PKG_CONFIG_PATH PKG_CONFIG_LIBDIR PKG_CONFIG_SYSROOT_DIR; do
        printf '%s=%s\n' "$name" "${!name-}"
    done
    (cd "$script_dir/.." && sha256sum Makefile deps/build-erofs.sh deps/common.sh)
    for tool in "$cc" "${CROSS_PREFIX}ar" "${CROSS_PREFIX}ranlib" autoreconf make; do
        "$tool" --version
        sha256sum "$(command -v "$tool")"
    done
    "$cc" -dumpmachine
    "$PKG_CONFIG" --modversion openssl uuid
    printf '%s\n' "$openssl_CFLAGS" "$openssl_LIBS" "$libuuid_CFLAGS" "$libuuid_LIBS"
    # Bounded by this one link, including libc and compiler startup objects.
    while IFS= read -r input; do
        [[ "$input" == "$probe_dir/"* ]] || sha256sum "$input"
    done < <(
        awk '$1 == "LOAD" && $2 ~ /\.(a|o)$/ {print $2}' "$probe_dir/probe.map" | LC_ALL=C sort -u
    )
} > "$probe_dir/inputs"
input_hash="$(sha256sum "$probe_dir/inputs" | awk '{print $1}')"
if [ -x "$out_mkfs" ] && [ -x "$out_fsck" ] && [ -f "$stamp" ] \
    && [ "$(cat "$stamp")" = "$input_hash" ]; then
    log "already built: $out_mkfs + $out_fsck (inputs unchanged)"
    exit 0
fi

[ -n "$tarball" ] || tarball="$(resolve_tarball "$EROFS_TARBALL" "$EROFS_TARBALL_SHA256")"
# This per-arch tree is generated build output. A recipe/source/flag change
# must not reuse old configure results, objects or the extraction marker.
[ ! -e "$src_dir/.git" ] || die "refusing to replace an EROFS source checkout: $src_dir"
rm -f "$stamp" "$src_dir/.extracted"
extract_tarball "$tarball" "$src_dir" >/dev/null

log "autoreconf (erofs-utils)"
(cd "$src_dir"; if ! ./autogen.sh >/dev/null 2>&1; then autoreconf -i; fi)

# Cross-compile arguments for autotools configure.
configure_cross_args=()
configure_env=(
    "CC=$cc" "CXX=${CROSS_PREFIX}g++" "AR=${CROSS_PREFIX}ar"
    "STRIP=${CROSS_PREFIX}strip" "RANLIB=${CROSS_PREFIX}ranlib"
)
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
    log "cross-compile mode: --host=$host_triple --build=$build_triple"
fi

log "configure (OpenSSL SHA-256, no compression, no fuse)"
(cd "$src_dir" && env "${configure_env[@]}" ./configure \
    "${configure_cross_args[@]}" \
    --disable-lz4 \
    --disable-lzma \
    --without-zlib \
    --without-libzstd \
    --without-libdeflate \
    --without-xxhash \
    --without-libcurl \
    --with-openssl \
    --without-libxml2 \
    --without-json-c \
    --without-libnl3 \
    --disable-multithreading)

# v1.9.1 can miss headers despite --with-openssl. Check both switches used by
# lib/sha256.h, so this build can never silently select the bundled fallback.
for define in HAVE_OPENSSL HAVE_OPENSSL_EVP_H; do
    grep -qx "#define $define 1" "$src_dir/config.h" \
        || die "configure did not enable the required OpenSSL SHA-256 backend ($define)"
done

log "make mkfs.erofs + fsck.erofs (mkfs + fsck subdirs; skips mount/dump/fuse)"
# Build lib first (mkfs/fsck depend on liberofs.a), then the two subdirs we ship.
# Avoids mount.erofs (pthread link bug in v1.9.1 when multithreading is disabled)
# and other subdirs we don't need.
#
# LDFLAGS=-all-static at make time (not configure: gcc rejects it in configure
# tests): the libtool link-mode flag for a fully static EXECUTABLE — plain
# -static is consumed by libtool itself (= "prefer .a of libtool libs") and
# never reaches the compiler driver.
make -C "$src_dir/lib"  -j"${EROFS_BUILD_JOBS:-$(nproc)}"
make -C "$src_dir/mkfs" -j"${EROFS_BUILD_JOBS:-$(nproc)}" \
    LDFLAGS="${LDFLAGS:-} -all-static -Wl,-Map,$src_dir/mkfs/mkfs.erofs.map"
make -C "$src_dir/fsck" -j"${EROFS_BUILD_JOBS:-$(nproc)}" LDFLAGS="${LDFLAGS:-} -all-static"

for binary in "$src_dir/mkfs/mkfs.erofs" "$src_dir/fsck/fsck.erofs"; do
    readelf -lW "$binary" > "$probe_dir/elf-programs"
    readelf -dW "$binary" > "$probe_dir/elf-dynamic"
    if grep -q INTERP "$probe_dir/elf-programs" \
        || grep -q NEEDED "$probe_dir/elf-dynamic"; then
        die "erofs output must be fully static: $binary"
    fi
done
mkdir -p "$BINDIR"
cp "$src_dir/mkfs/mkfs.erofs" "$out_mkfs"
cp "$src_dir/fsck/fsck.erofs" "$out_fsck"
chmod +x "$out_mkfs" "$out_fsck"
printf '%s\n' "$input_hash" > "$stamp"

log "built $out_mkfs + $out_fsck"
# When cross-compiling, --help on the target binary won't run on the host;
# only run it for native builds.
if [ -z "$CROSS_PREFIX" ]; then
    "$out_mkfs" --help 2>&1 | head -2 || true
    "$out_fsck" --help 2>&1 | head -2 || true
else
    file "$out_mkfs" "$out_fsck" 2>&1 | head -2 || true
fi
