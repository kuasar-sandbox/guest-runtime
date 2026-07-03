#!/usr/bin/env bash
#
# Multi-stage dispatcher for the sandbox guest kernel: upstream Linux
# source built with the patches under deps/linux-patches/.  Structure
# mirrors deps/build-cloud-hypervisor.sh exactly (same patch dev loop).
#
# Stages are selected via the STAGE env var (default: build):
#
#   STAGE=fetch          extract tarball + git init + tag linux-patches-base
#   STAGE=patches-apply  git am deps/linux-patches/*.patch onto the tree
#   STAGE=patches-format git format-patch base..HEAD -> deps/linux-patches/
#   STAGE=build          merge sandbox defconfig + make -> $BINDIR/vmlinux
#
# Each stage is idempotent in the safe sense:
#   - fetch skips if the base tag already exists; refuses if a foreign
#     git tree is present without that tag (won't overwrite WIP).
#   - patches-apply requires HEAD == base, OR detects the patches are
#     already applied (subject compare) and no-ops; otherwise refuses.
#   - patches-format clears stale *.patch (keeps the curated
#     0000-cover-letter.txt) before regenerating from commits.
#   - build skips if $BINDIR/vmlinux already exists; kbuild is otherwise
#     incremental.
#
# The output file lives at $BINDIR/vmlinux for both architectures, but
# the on-disk format differs:
#   x86_64  raw ELF kernel, PVH-bootable by cloud-hypervisor
#   arm64   PE-format Image (UEFI/ACPI), bootable by cloud-hypervisor
# sandbox-ctl points CH at this path with no arch-specific logic.
#
# Inputs (env, all optional):
#   LINUX_TARBALL          URL or local path; supports "url#filename" form.
#                          Default: kernel.org linux-6.1.169 (LTS).
#   LINUX_TARBALL_SHA256   Optional expected SHA256.
#   BUILD_DIR              Per-arch build directory (e.g. build/x86_64).
#   BINDIR                 Per-arch bin directory (e.g. bin/x86_64).
#   TARBALL_CACHE          Optional shared tarball cache.
#   KERNEL_ARCH            x86_64 or arm64. Default: derived from uname -m.
#   CROSS_PREFIX           GNU-triple prefix for cross-compile, empty for native.
#   LINUX_BUILD_SRC        Source tree. Default build/src/linux. WSL2 users
#                          on /mnt/<drive>/ set this to a Linux-native path
#                          (the Makefile auto-redirects to ~/linux-build/src).
#   LINUX_BUILD_OUT        kbuild O= dir. Default $BUILD_DIR/linux.
#   LINUX_PATCHES_DIR      Patch files. Default $(pwd)/deps/linux-patches.
#   LINUX_BASE_TAG         git tag for the import baseline.
#                          Default: linux-patches-base.
#
# Build deps (build stage): bc, bison, flex, make, libelf headers,
# libssl headers, pkg-config, and a (cross) gcc matching $CROSS_PREFIX.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${STAGE:=build}"
: "${LINUX_TARBALL:=https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.169.tar.gz}"
: "${LINUX_TARBALL_SHA256:=}"
: "${BUILD_DIR:=$(pwd)/build}"
: "${BINDIR:=$(pwd)/bin}"
: "${KERNEL_ARCH:=$(uname -m)}"
: "${CROSS_PREFIX:=}"
: "${LINUX_BUILD_SRC:=$BUILD_DIR/src/linux}"
: "${LINUX_BUILD_OUT:=$BUILD_DIR/linux}"
: "${LINUX_PATCHES_DIR:=$(pwd)/deps/linux-patches}"
: "${LINUX_BASE_TAG:=linux-patches-base}"

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

src_dir="$LINUX_BUILD_SRC"
out_obj="$LINUX_BUILD_OUT"
out_bin="$BINDIR/vmlinux"

do_fetch() {
    if [ -d "$src_dir/.git" ]; then
        if git -C "$src_dir" rev-parse --verify "$LINUX_BASE_TAG" >/dev/null 2>&1; then
            log "$LINUX_BASE_TAG tag exists at $src_dir, skipping fetch"
            return 0
        fi
        die "git tree exists at $src_dir but no $LINUX_BASE_TAG tag — refusing to overwrite. Either tag manually (git -C $src_dir tag $LINUX_BASE_TAG <commit>) or rm -rf $src_dir to start fresh."
    fi
    require_cmd tar git
    tarball="$(resolve_tarball "$LINUX_TARBALL" "$LINUX_TARBALL_SHA256")"
    log "kernel source: $src_dir"
    extract_tarball "$tarball" "$src_dir" >/dev/null
    log "git init + tag $LINUX_BASE_TAG at $src_dir"
    git -C "$src_dir" init -q
    printf '*.o\n*.ko\n*.cmd\n.tmp_versions/\n' >> "$src_dir/.git/info/exclude"
    git -C "$src_dir" -c user.name=deps -c user.email=deps@local add -A
    git -C "$src_dir" -c user.name=deps -c user.email=deps@local \
        commit -q -m "import $(basename "$tarball")"
    git -C "$src_dir" tag "$LINUX_BASE_TAG"
}

do_patches_apply() {
    require_cmd git
    [ -d "$src_dir/.git" ] || die "no git tree at $src_dir; run 'make linux-fetch' first"
    git -C "$src_dir" rev-parse --verify "$LINUX_BASE_TAG" >/dev/null 2>&1 \
        || die "$LINUX_BASE_TAG tag missing at $src_dir"

    shopt -s nullglob
    local patches=( "$LINUX_PATCHES_DIR"/*.patch )

    local base head
    base=$(git -C "$src_dir" rev-parse "$LINUX_BASE_TAG")
    head=$(git -C "$src_dir" rev-parse HEAD)

    if [ "${#patches[@]}" -eq 0 ]; then
        if [ "$base" != "$head" ]; then
            log "no patches in $LINUX_PATCHES_DIR but src tree has commits beyond $LINUX_BASE_TAG — assuming WIP, leaving as-is"
        else
            log "no patches in $LINUX_PATCHES_DIR — nothing to apply"
        fi
        return 0
    fi

    if [ "$base" = "$head" ]; then
        log "applying ${#patches[@]} patch(es) from $LINUX_PATCHES_DIR"
        git -C "$src_dir" -c user.name=deps -c user.email=deps@local am "${patches[@]}"
        return 0
    fi

    # HEAD differs from base: either the patches are already applied
    # (steady state after a prior apply — re-running must be a no-op,
    # not a failure) or there's WIP we must not clobber.  Distinguish by
    # comparing commit subjects on base..HEAD against the patch files.
    local applied_count
    applied_count=$(git -C "$src_dir" rev-list --count "$LINUX_BASE_TAG..HEAD")
    if [ "$applied_count" -ne "${#patches[@]}" ]; then
        die "HEAD ($head) is $applied_count commit(s) past $LINUX_BASE_TAG but $LINUX_PATCHES_DIR has ${#patches[@]} patch(es). Likely WIP not yet formatted.
Save WIP and reset:
  make linux-patches-format
  git -C $src_dir reset --hard $LINUX_BASE_TAG
Then re-run."
    fi

    local applied_subjects patch_subjects
    applied_subjects=$(git -C "$src_dir" log --reverse --format=%s "$LINUX_BASE_TAG..HEAD")
    # git format-patch emits "Subject: [PATCH N/M] <text>" and folds long
    # subjects across lines (RFC 2822 continuation: leading whitespace).
    # Extract Subject, strip the bracketed prefix, and unfold continuations
    # back into one line so the comparison matches `git log --format=%s`.
    patch_subjects=$(for p in "${patches[@]}"; do
        awk '
            /^Subject: / {
                sub(/^Subject: /, "")
                sub(/^\[PATCH[^]]*\] /, "")
                subj = $0
                capturing = 1
                next
            }
            capturing && /^[ \t]/ {
                sub(/^[ \t]+/, " ")
                subj = subj $0
                next
            }
            capturing {
                print subj
                capturing = 0
                exit
            }
            END { if (capturing) print subj }
        ' "$p"
    done)

    if [ "$applied_subjects" = "$patch_subjects" ]; then
        log "all ${#patches[@]} patch(es) already applied at HEAD — skipping git am"
        return 0
    fi

    die "HEAD ($head) is $applied_count commit(s) past $LINUX_BASE_TAG but commit subjects don't match $LINUX_PATCHES_DIR.
Probable cause: WIP commits or hand-edits diverged from the tracked patch files.
Save WIP and reset:
  make linux-patches-format
  git -C $src_dir reset --hard $LINUX_BASE_TAG
Then re-run."
}

do_patches_format() {
    require_cmd git
    [ -d "$src_dir/.git" ] || die "no git tree at $src_dir"
    git -C "$src_dir" rev-parse --verify "$LINUX_BASE_TAG" >/dev/null 2>&1 \
        || die "$LINUX_BASE_TAG tag missing at $src_dir"

    mkdir -p "$LINUX_PATCHES_DIR"
    # Only the machine-generated commit patches are regenerated; the
    # curated upstream cover letter (0000-cover-letter.txt) is preserved.
    rm -f "$LINUX_PATCHES_DIR"/*.patch

    local base head
    base=$(git -C "$src_dir" rev-parse "$LINUX_BASE_TAG")
    head=$(git -C "$src_dir" rev-parse HEAD)
    if [ "$base" = "$head" ]; then
        log "HEAD == $LINUX_BASE_TAG; no commits to extract (cleared *.patch)"
        return 0
    fi
    git -C "$src_dir" format-patch "$LINUX_BASE_TAG..HEAD" -o "$LINUX_PATCHES_DIR" >/dev/null
    local n
    n=$(find "$LINUX_PATCHES_DIR" -maxdepth 1 -name '*.patch' | wc -l)
    log "extracted $n patch(es) to $LINUX_PATCHES_DIR"
}

do_build() {
    if [ -x "$out_bin" ]; then
        log "already built: $out_bin (delete it to force rebuild)"
        exit 0
    fi

    local common_frag arch_frag
    common_frag="$script_dir/vmlinux/sandbox-common.config"
    arch_frag="$script_dir/vmlinux/sandbox-$KERNEL_ARCH.config"
    [ -f "$common_frag" ] || die "config fragment not found: $common_frag"
    [ -f "$arch_frag" ]   || die "config fragment not found: $arch_frag"
    [ -d "$src_dir" ]     || die "no source at $src_dir; run 'make linux-fetch' first"

    require_cmd bc bison flex make pkg-config
    if [ -n "$CROSS_PREFIX" ]; then
        require_cmd "${CROSS_PREFIX}gcc"
    else
        require_cmd gcc
    fi
    # libelf + openssl headers are host build tools (scripts/sign-file,
    # fixdep, ...), not linked into vmlinux; check host pkg-config.
    if ! pkg-config --exists libelf 2>/dev/null; then
        die "libelf development headers not found (install libelf-dev / elfutils-libelf-devel)"
    fi
    if ! pkg-config --exists libssl openssl 2>/dev/null; then
        die "libssl development headers not found (install libssl-dev / openssl-devel)"
    fi

    log "kernel source:       $src_dir"
    log "kernel build output: $out_obj"
    mkdir -p "$out_obj" "$src_dir/arch/$kbuild_arch/configs"
    local combined_defconfig="$src_dir/arch/$kbuild_arch/configs/sandbox_defconfig"
    cat "$common_frag" "$arch_frag" > "$combined_defconfig"
    log "merged defconfig: $combined_defconfig"

    local make_args=(-C "$src_dir" O="$out_obj" ARCH="$kbuild_arch")
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

    local image_full="$out_obj/$image_subpath"
    [ -f "$image_full" ] || die "build finished but kernel image missing at $image_full"

    mkdir -p "$BINDIR"
    cp "$image_full" "$out_bin"
    chmod +x "$out_bin"

    log "built $out_bin ($(du -h "$out_bin" | cut -f1))"
    file "$out_bin" 2>&1 | head -1 || true
}

case "$STAGE" in
    fetch)          do_fetch ;;
    patches-apply)  do_patches_apply ;;
    patches-format) do_patches_format ;;
    build)          do_build ;;
    *) die "unknown STAGE='$STAGE' (want fetch|patches-apply|patches-format|build)" ;;
esac
