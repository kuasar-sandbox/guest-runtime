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
# included), where no dynamic loader exists. Needs target libuuid.a, libgcrypt.a
# and libgpg-error.a plus their headers and pkg-config metadata. A maintained
# patch explicitly selects full SHA-256 through Libgcrypt, without fallback.
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
# One .erofs-recipe v2 stamp covers both outputs, source/patch/recipe bytes,
# flags/tools and actual compiler/link dependencies. EROFS_BUILD_JOBS defaults
# to JOBS, then 2; changing job count does not invalidate the recipe.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${EROFS_TARBALL:=https://codeload.github.com/erofs/erofs-utils/tar.gz/refs/tags/v1.9.1#erofs-utils-v1.9.1.tar.gz}"
: "${EROFS_TARBALL_SHA256=a9ef5ab67c4b8d2d3e9ed71f39cd008bda653142a720d8a395a36f1110d0c432}"
: "${BUILD_DIR:=$(pwd)/build}"
: "${BINDIR:=$(pwd)/bin}"
: "${CROSS_PREFIX:=}"

out_mkfs="$BINDIR/mkfs.erofs"
out_fsck="$BINDIR/fsck.erofs"
recipe_stamp="$BINDIR/.erofs-recipe"
src_dir="$BUILD_DIR/src/erofs-utils"
: "${EROFS_BUILD_JOBS:=${JOBS:-2}}"
[[ "$EROFS_BUILD_JOBS" =~ ^[1-9][0-9]*$ ]] || die "EROFS_BUILD_JOBS must be a positive integer"
cc="${CC:-gcc}"
[ -z "$CROSS_PREFIX" ] || cc="${CROSS_PREFIX}gcc"
read -r -a compiler <<< "$cc"
nm_tool="${NM:-${CROSS_PREFIX}nm}"
: "${PKG_CONFIG:=pkg-config}"
require_cmd sha256sum patch python3 "$PKG_CONFIG" "${compiler[0]}" readelf "$nm_tool"

# Cross builds never silently discover host .pc files.
if [ -n "$CROSS_PREFIX" ]; then
    multiarch="$("${compiler[@]}" -print-multiarch)"
    sysroot="$("${compiler[@]}" -print-sysroot)"
    if [ -z "${PKG_CONFIG_LIBDIR+x}" ]; then
        [ -n "$multiarch" ] || die "cross erofs build requires target PKG_CONFIG_LIBDIR"
        export PKG_CONFIG_LIBDIR="${sysroot%/}/usr/lib/$multiarch/pkgconfig:${sysroot%/}/lib/$multiarch/pkgconfig:${sysroot%/}/usr/share/pkgconfig"
    fi
    export PKG_CONFIG_PATH="${PKG_CONFIG_PATH:-}"
    export PKG_CONFIG_SYSROOT_DIR="${PKG_CONFIG_SYSROOT_DIR:-$sysroot}"
fi
export PKG_CONFIG

# A local archive is an input, not a filename-based download-cache entry.
case "$EROFS_TARBALL" in
    http://*|https://*) tarball="$(resolve_tarball "$EROFS_TARBALL" "$EROFS_TARBALL_SHA256")" ;;
    *) tarball="$(realpath -e "$EROFS_TARBALL")"
       [ -z "$EROFS_TARBALL_SHA256" ] || \
           [ "$(sha256sum < "$tarball" | cut -d ' ' -f1)" = "$EROFS_TARBALL_SHA256" ] \
           || die "sha256 mismatch for $tarball" ;;
esac
# shellcheck disable=SC1091
source "$script_dir/erofs-recipe.sh"
# shellcheck disable=SC2034 # Read by the sourced helper.
recipe="$(erofs_recipe_digest)"
# Retire the previous standalone SHA stamp; it cannot authorize reuse.
rm -f "$BINDIR/.erofs-build-inputs"
if [ -x "$out_mkfs" ] && [ -x "$out_fsck" ] && erofs_recipe_matches; then
    log "already built with matching erofs recipe: $out_mkfs + $out_fsck"
    exit 0
fi
[ ! -e "$src_dir/.git" ] || die "refusing to replace an erofs source Git checkout: $src_dir"
rm -f "$recipe_stamp" "$src_dir/.extracted"
require_cmd autoreconf make tar "${CROSS_PREFIX}ar" "${CROSS_PREFIX}ranlib"

prerequisite_hint="erofs requires target static libgcrypt, libgpg-error and libuuid plus headers and pkg-config metadata.
Debian/Ubuntu: libgcrypt20-dev libgpg-error-dev uuid-dev for the target architecture.
RPM devel packages may omit static archives (including openEuler); provision matching
static builds with their source/relink materials before building this guest tool.
For a cross sysroot, set PKG_CONFIG_LIBDIR and PKG_CONFIG_SYSROOT_DIR to target paths."
"$PKG_CONFIG" --exists libgcrypt gpg-error uuid || die "$prerequisite_hint"
gcrypt_cflags="$("$PKG_CONFIG" --cflags libgcrypt gpg-error)"
gcrypt_libs="$("$PKG_CONFIG" --libs --static libgcrypt gpg-error)"
libuuid_CFLAGS="${libuuid_CFLAGS-$("$PKG_CONFIG" --cflags uuid)}"
libuuid_LIBS="${libuuid_LIBS-$("$PKG_CONFIG" --libs --static uuid)}"
export libuuid_CFLAGS libuuid_LIBS
mkdir -p "$BUILD_DIR"
probe_dir="$(mktemp -d "$BUILD_DIR/erofs-link.XXXXXX")"
trap 'rm -rf "$probe_dir"' EXIT
cat > "$probe_dir/probe.c" <<'EOF'
#include <gcrypt.h>
#include <uuid/uuid.h>
int main(void)
{
    unsigned char digest[32];
    uuid_t uuid;
    if (!gcry_check_version(GCRYPT_VERSION) ||
        gcry_control(GCRYCTL_DISABLE_SECMEM, 0) ||
        gcry_control(GCRYCTL_INITIALIZATION_FINISHED, 0) ||
        gcry_md_test_algo(GCRY_MD_SHA256))
        return 1;
    gcry_md_hash_buffer(GCRY_MD_SHA256, digest, "abc", 3);
    uuid_clear(uuid);
    return !uuid_is_null(uuid);
}
EOF
# Conventional compiler/pkg-config flag splitting; never eval flags.
# shellcheck disable=SC2086
if ! "${compiler[@]}" ${CPPFLAGS:-} ${CFLAGS:-} $gcrypt_cflags $libuuid_CFLAGS \
    -c "$probe_dir/probe.c" -o "$probe_dir/probe.o" > "$probe_dir/link.log" 2>&1 \
    || ! "${compiler[@]}" ${CFLAGS:-} "$probe_dir/probe.o" ${LDFLAGS:-} -static \
    $gcrypt_libs $libuuid_LIBS ${LIBS:-} -o "$probe_dir/probe" >> "$probe_dir/link.log" 2>&1; then
    cat "$probe_dir/link.log" >&2
    die "target static Libgcrypt/Libgpg-error/uuid link check failed. $prerequisite_hint"
fi
extract_tarball "$tarball" "$src_dir" >/dev/null
erofs_apply_patches
log "autoreconf (erofs-utils)"
(cd "$src_dir"; if ! ./autogen.sh >/dev/null 2>&1; then autoreconf -i; fi)

# Cross-compile arguments for autotools configure.
configure_cross_args=()
cxx="${CXX:-g++}"; ar="${AR:-ar}"; strip="${STRIP:-strip}"; ranlib="${RANLIB:-ranlib}"
if [ -n "$CROSS_PREFIX" ]; then
    cxx="${CROSS_PREFIX}g++"; ar="${CROSS_PREFIX}ar"
    strip="${CROSS_PREFIX}strip"; ranlib="${CROSS_PREFIX}ranlib"
fi
configure_env=(
    "CC=$cc" "CXX=$cxx" "AR=$ar" "STRIP=$strip" "RANLIB=$ranlib"
    "CPPFLAGS=${CPPFLAGS:-} $gcrypt_cflags -DEROFS_USE_LIBGCRYPT_SHA256=1"
    "LIBS=$gcrypt_libs ${LIBS:-}"
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

log "configure (Libgcrypt SHA-256, no compression, no fuse)"
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

# Never allow ambient configure overrides to re-enable the OpenSSL backend.
if grep -Eq '^#define HAVE_OPENSSL(_EVP_H)? 1$' "$src_dir/config.h"; then
    die "configure enabled an unexpected OpenSSL backend"
fi

log "make mkfs.erofs + fsck.erofs (mkfs + fsck subdirs; skips mount/dump/fuse)"
# Build lib first (mkfs/fsck depend on liberofs.a), then the two subdirs we ship.
# Avoids mount.erofs (pthread link bug in v1.9.1 when multithreading is disabled)
# and other subdirs we don't need.
#
# LDFLAGS=-all-static at make time (not configure: gcc rejects it in configure
# tests): the libtool link-mode flag for a fully static EXECUTABLE — plain
# -static is consumed by libtool itself (= "prefer .a of libtool libs") and
# never reaches the compiler driver.
make -C "$src_dir/lib"  -j"$EROFS_BUILD_JOBS"
# Verify the compiled SHA path, so a later -U in user flags cannot silently
# select the bundled fallback even though the static prerequisite probe passed.
"$nm_tool" -u "$src_dir/lib/liberofs_la-sha256.o" > "$probe_dir/sha-symbols"
for symbol in gcry_md_hash_buffer gcry_md_open; do
    grep -Eq "[[:space:]]U[[:space:]]+$symbol$" "$probe_dir/sha-symbols" \
        || die "compiled SHA object did not select required Libgcrypt backend ($symbol)"
done
make -C "$src_dir/mkfs" -j"$EROFS_BUILD_JOBS" \
    LDFLAGS="${LDFLAGS:-} -all-static -Wl,-Map,$src_dir/mkfs/mkfs.erofs.map"
make -C "$src_dir/fsck" -j"$EROFS_BUILD_JOBS" LDFLAGS="${LDFLAGS:-} -all-static -Wl,-Map,$src_dir/fsck/fsck.erofs.map"

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
erofs_recipe_write_stamp

log "built $out_mkfs + $out_fsck"
# When cross-compiling, --help on the target binary won't run on the host;
# only run it for native builds.
if [ -z "$CROSS_PREFIX" ]; then
    "$out_mkfs" --help 2>&1 | head -2 || true
    "$out_fsck" --help 2>&1 | head -2 || true
else
    file "$out_mkfs" "$out_fsck" 2>&1 | head -2 || true
fi
