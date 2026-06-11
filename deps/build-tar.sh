#!/usr/bin/env bash
#
# Build GNU tar into $BINDIR — statically (musl) for native builds.
#
# flatten-ctl's `tar` subcommand and pkg/tar drive GNU tar as their
# engine: sparse encoding (PAX 1.0), --transform renames and the
# extraction hardening all come from it, and host tars are unreliable
# (busybox has no sparse support, bsdtar different flags). The binary
# ships in the release tarball next to flatten-ctl, which finds it via
# its own binary directory when TAR_PATH is unset.
#
# Native builds require musl-gcc (apt install musl-tools): glibc's
# libc.a exports internal symbols (__mktime_internal among others) that
# collide with the gnulib replacements GNU tar always compiles, so a
# glibc -static link cannot work. Cross builds (CROSS_PREFIX set) use
# the GNU-triple glibc toolchain and link dynamically, like the other
# deps artifacts — Debian ships no musl cross toolchain.
#
# ACL/SELinux/xattr support is configured out: the platform's image
# pipeline drops xattrs at mkfs.erofs level anyway (-x-1), and leaving
# them out keeps the closure at plain libc.
#
# Inputs (env):
#   TAR_TARBALL         URL or local path; supports "url#filename" form.
#                       Default: GNU tar 1.35 from ftp.gnu.org.
#   TAR_TARBALL_SHA256  Optional expected SHA256. Empty → skip verify.
#   BUILD_DIR           Per-arch build directory (e.g. build/x86_64).
#   BINDIR              Per-arch bin directory (e.g. bin/x86_64).
#   TARBALL_CACHE       Optional shared tarball cache.
#   CROSS_PREFIX        Optional GNU-triple prefix. Empty for native.
#
# Idempotent: if $BINDIR/tar already exists it exits 0 without
# rebuilding. Delete it to force.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${TAR_TARBALL:=https://ftp.gnu.org/gnu/tar/tar-1.35.tar.gz#tar-1.35.tar.gz}"
: "${TAR_TARBALL_SHA256:=}"
: "${BUILD_DIR:=$(pwd)/build}"
: "${BINDIR:=$(pwd)/bin}"
: "${CROSS_PREFIX:=}"

out_tar="$BINDIR/tar"
if [ -x "$out_tar" ]; then
    log "already built: $out_tar (delete to force rebuild)"
    exit 0
fi

require_cmd make
if [ -n "$CROSS_PREFIX" ]; then
    require_cmd "${CROSS_PREFIX}gcc"
else
    command -v musl-gcc >/dev/null 2>&1 \
        || die "musl-gcc not found (apt install musl-tools) — needed for the static GNU tar build"
fi

tarball="$(resolve_tarball "$TAR_TARBALL" "$TAR_TARBALL_SHA256")"
src_dir="$BUILD_DIR/src/gnu-tar"
rm -rf "$src_dir" # always configure from a clean tree
extract_tarball "$tarball" "$src_dir" >/dev/null

configure_args=(--disable-nls --disable-acl --without-posix-acls --without-selinux --without-xattrs)
configure_env=()
ldflags="-static"
if [ -n "$CROSS_PREFIX" ]; then
    host_triple="${CROSS_PREFIX%-}"
    build_triple="$(gcc -dumpmachine 2>/dev/null || echo "$(uname -m)-linux-gnu")"
    configure_args+=(--host="$host_triple" --build="$build_triple")
    configure_env=("CC=${CROSS_PREFIX}gcc" "AR=${CROSS_PREFIX}ar" "STRIP=${CROSS_PREFIX}strip" "RANLIB=${CROSS_PREFIX}ranlib")
    ldflags=""
    log "cross-compile mode: --host=$host_triple --build=$build_triple (dynamic link)"
else
    configure_env=("CC=musl-gcc")
fi

log "configure (GNU tar${ldflags:+, static via musl})"
# FORCE_UNSAFE_CONFIGURE lets configure proceed under uid 0 (it
# refuses to run as root otherwise).
(cd "$src_dir" && env "${configure_env[@]}" FORCE_UNSAFE_CONFIGURE=1 \
    ./configure "${configure_args[@]}" LDFLAGS="$ldflags" >/dev/null)

log "make (GNU tar)"
make -C "$src_dir" -j"$(nproc)" >/dev/null

mkdir -p "$BINDIR"
install -m 0755 "$src_dir/src/tar" "$out_tar"
if [ -n "$CROSS_PREFIX" ]; then
    "${CROSS_PREFIX}strip" "$out_tar" || true
    log "built: $out_tar (cross: not runnable on this host)"
else
    strip "$out_tar" || true
    log "built: $out_tar ($("$out_tar" --version | head -1))"
fi
