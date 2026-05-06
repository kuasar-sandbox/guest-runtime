#!/usr/bin/env bash
#
# Build the sandbox guest kernel (vmlinux) from upstream Linux source.
# Output is the raw ELF kernel at $BINDIR/vmlinux, suitable for loading
# by cloud-hypervisor via PVH boot (CONFIG_PVH=y in sandbox.defconfig).
#
# Inputs (env):
#   LINUX_TARBALL          URL or local path; supports "url#filename" form.
#                          Default: kernel.org linux-6.1.169 (LTS).
#   LINUX_TARBALL_SHA256   Optional expected SHA256.
#   BUILD_DIR              Absolute path to repo's build/ directory.
#   BINDIR                 Absolute path to repo's bin/ directory.
#   DEFCONFIG              Optional override for the sandbox.defconfig path.
#                          Default: <repo>/deps/vmlinux/sandbox.defconfig.
#
# Idempotent: if $BINDIR/vmlinux exists, exit 0. Delete the file (or run
# `make clean-deps && make vmlinux`) to force rebuild.
#
# Network: tarball is downloaded from kernel.org if not already cached
# under build/tarball/. Set http(s)_proxy in the environment if upstream
# is blocked.
#
# Build deps required on the host: bc, bison, flex, gcc, make,
#   libelf-dev (libelf headers), libssl-dev (openssl headers), pkg-config.
# The script aborts with a clear message if any are missing.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${LINUX_TARBALL:=https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.169.tar.gz}"
: "${LINUX_TARBALL_SHA256:=}"
: "${BUILD_DIR:=$(pwd)/build}"
: "${BINDIR:=$(pwd)/bin}"
: "${DEFCONFIG:=$script_dir/vmlinux/sandbox.defconfig}"

# LINUX_BUILD_SRC overrides where to extract the kernel source.
# LINUX_BUILD_OUT overrides where kbuild writes .config and .o files.
# Defaults stay under $BUILD_DIR (so `make clean-deps` covers them).
# WSL2 users on /mnt/<drive>/ should override both to a Linux-native fs:
#   make vmlinux LINUX_BUILD_SRC=$HOME/linux-build/src \
#                LINUX_BUILD_OUT=$HOME/linux-build/out
: "${LINUX_BUILD_SRC:=$BUILD_DIR/src/linux}"
: "${LINUX_BUILD_OUT:=$BUILD_DIR/linux}"

out_bin="$BINDIR/vmlinux"
if [ -x "$out_bin" ]; then
    log "already built: $out_bin (delete it to force rebuild)"
    exit 0
fi

[ -f "$DEFCONFIG" ] || die "sandbox defconfig not found: $DEFCONFIG"

require_cmd bc bison flex gcc make tar pkg-config

# libelf + openssl headers presence — kernel build fails late without them
# and the error is a wall of compiler output; check up-front.
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

# Stage the sandbox defconfig into arch/x86/configs/ where kbuild expects
# named defconfigs. After this, `make sandbox_defconfig` resolves it.
log "staging sandbox.defconfig into arch/x86/configs/"
cp "$DEFCONFIG" "$src_dir/arch/x86/configs/sandbox_defconfig"

# Separate output directory so the source tree stays clean and the
# WSL-friendly override (LINUX_BUILD_OUT=~/linux-build/out) can put
# .o files on a fast filesystem.
out_obj="$LINUX_BUILD_OUT"
log "kernel build output: $out_obj"
mkdir -p "$out_obj"

log "make sandbox_defconfig (allnoconfig + sandbox.defconfig + deps)"
make -C "$src_dir" O="$out_obj" ARCH=x86_64 sandbox_defconfig >/dev/null

# olddefconfig resolves any new options the upstream kernel introduced
# since the defconfig was last reviewed; also exposes silent regressions.
log "make olddefconfig (resolve any new options)"
make -C "$src_dir" O="$out_obj" ARCH=x86_64 olddefconfig >/dev/null

log "make vmlinux -j$(nproc) (this takes ~5-10 minutes on first build)"
make -C "$src_dir" O="$out_obj" ARCH=x86_64 -j"$(nproc)" vmlinux

[ -f "$out_obj/vmlinux" ] || die "build finished but $out_obj/vmlinux missing"

mkdir -p "$BINDIR"
cp "$out_obj/vmlinux" "$out_bin"
chmod +x "$out_bin"

log "built $out_bin ($(du -h "$out_bin" | cut -f1))"
"$out_bin" --version 2>&1 | head -1 || true
file "$out_bin" 2>&1 | head -1 || true
