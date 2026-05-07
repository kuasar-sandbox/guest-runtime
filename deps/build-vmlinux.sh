#!/usr/bin/env bash
#
# Build the sandbox guest kernel from upstream Linux source. The output
# file lives at $BINDIR/vmlinux for both architectures, but the on-disk
# format differs:
#
#   x86_64  raw ELF kernel, PVH-bootable by cloud-hypervisor
#   arm64   PE-format Image (UEFI/ACPI), bootable by cloud-hypervisor
#
# sandbox-ctl points cloud-hypervisor at this path without arch-specific
# logic; CH detects the format and applies the right boot protocol.
#
# Inputs (env):
#   LINUX_TARBALL          URL or local path; supports "url#filename" form.
#                          Default: kernel.org linux-6.1.169 (LTS).
#   LINUX_TARBALL_SHA256   Optional expected SHA256.
#   BUILD_DIR              Per-arch build directory (e.g. build/x86_64).
#   BINDIR                 Per-arch bin directory (e.g. bin/x86_64).
#   TARBALL_CACHE          Optional shared tarball cache.
#   KERNEL_ARCH            x86_64 or arm64. Default: derived from uname -m.
#   CROSS_PREFIX           GNU-triple prefix for cross-compile, empty for native.
#   LINUX_BUILD_SRC        Where to extract the kernel source. Default
#                          build/src/linux (shared across architectures —
#                          arch selection is via the make ARCH= flag).
#   LINUX_BUILD_OUT        Where kbuild writes .config and .o files. Default
#                          $BUILD_DIR/linux (per-arch).
#
# Idempotent: if $BINDIR/vmlinux exists, exit 0. Delete the file (or run
# `make clean-deps && make vmlinux`) to force rebuild.
#
# Build deps required on the host: bc, bison, flex, make, libelf headers,
# libssl headers, pkg-config, and a (cross) gcc matching $CROSS_PREFIX.
# arm64 cross-builds also need lz4 (gzip is the default Image compression
# method on aarch64; lz4 is occasionally used).
#
# The script aborts with a clear message if any required tool is missing.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${LINUX_TARBALL:=https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.169.tar.gz}"
: "${LINUX_TARBALL_SHA256:=}"
: "${BUILD_DIR:=$(pwd)/build}"
: "${BINDIR:=$(pwd)/bin}"
: "${KERNEL_ARCH:=$(uname -m)}"
: "${CROSS_PREFIX:=}"

# Map host uname -m to kernel ARCH= names (only used when KERNEL_ARCH is
# defaulted from uname; explicit values pass through).
case "$KERNEL_ARCH" in
    x86_64) kbuild_arch=x86;     kbuild_target=vmlinux;  image_subpath="vmlinux" ;;
    aarch64|arm64)
        kbuild_arch=arm64;       kbuild_target=Image;    image_subpath="arch/arm64/boot/Image"
        KERNEL_ARCH=arm64
        ;;
    *) die "unsupported KERNEL_ARCH=$KERNEL_ARCH (want x86_64 or arm64)" ;;
esac

: "${LINUX_BUILD_SRC:=$BUILD_DIR/src/linux}"
: "${LINUX_BUILD_OUT:=$BUILD_DIR/linux}"

out_bin="$BINDIR/vmlinux"
if [ -x "$out_bin" ]; then
    log "already built: $out_bin (delete it to force rebuild)"
    exit 0
fi

common_frag="$script_dir/vmlinux/sandbox-common.config"
arch_frag="$script_dir/vmlinux/sandbox-$KERNEL_ARCH.config"
[ -f "$common_frag" ] || die "config fragment not found: $common_frag"
[ -f "$arch_frag" ]   || die "config fragment not found: $arch_frag"

require_cmd bc bison flex make tar pkg-config

# Cross compiler check.
if [ -n "$CROSS_PREFIX" ]; then
    require_cmd "${CROSS_PREFIX}gcc"
else
    require_cmd gcc
fi

# libelf + openssl headers presence — kernel build fails late without them
# and the error is a wall of compiler output; check up-front. These are
# host-side build tools (used by scripts/sign-file, fixdep, etc.), not
# linked into vmlinux, so we check the host pkg-config without --host.
if ! pkg-config --exists libelf 2>/dev/null; then
    die "libelf development headers not found (install libelf-dev / elfutils-libelf-devel)"
fi
if ! pkg-config --exists libssl openssl 2>/dev/null; then
    die "libssl development headers not found (install libssl-dev / openssl-devel)"
fi

tarball="$(resolve_tarball "$LINUX_TARBALL" "$LINUX_TARBALL_SHA256")"
src_dir="$LINUX_BUILD_SRC"
log "kernel source: $src_dir"
extract_tarball "$tarball" "$src_dir" >/dev/null

# Stage the merged sandbox defconfig under arch/<kbuild_arch>/configs/.
# We concatenate the common fragment + the arch-specific fragment into a
# single sandbox_defconfig and let kbuild's `make sandbox_defconfig` +
# `make olddefconfig` resolve dependencies and silent regressions.
out_obj="$LINUX_BUILD_OUT"
log "kernel build output: $out_obj"
mkdir -p "$out_obj" "$src_dir/arch/$kbuild_arch/configs"
combined_defconfig="$src_dir/arch/$kbuild_arch/configs/sandbox_defconfig"
cat "$common_frag" "$arch_frag" > "$combined_defconfig"
log "merged defconfig: $combined_defconfig"

make_args=(-C "$src_dir" O="$out_obj" ARCH="$kbuild_arch")
if [ -n "$CROSS_PREFIX" ]; then
    make_args+=(CROSS_COMPILE="$CROSS_PREFIX")
fi

log "make ARCH=$kbuild_arch sandbox_defconfig"
make "${make_args[@]}" sandbox_defconfig >/dev/null

# olddefconfig resolves any new options the upstream kernel introduced
# since the defconfig was last reviewed; also exposes silent regressions.
log "make olddefconfig (resolve any new options)"
make "${make_args[@]}" olddefconfig >/dev/null

log "make $kbuild_target -j$(nproc) (this takes ~5-10 minutes on first build)"
make "${make_args[@]}" -j"$(nproc)" "$kbuild_target"

image_full="$out_obj/$image_subpath"
[ -f "$image_full" ] || die "build finished but kernel image missing at $image_full"

mkdir -p "$BINDIR"
cp "$image_full" "$out_bin"
chmod +x "$out_bin"

log "built $out_bin ($(du -h "$out_bin" | cut -f1))"
file "$out_bin" 2>&1 | head -1 || true
