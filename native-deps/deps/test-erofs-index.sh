#!/usr/bin/env bash
set -euo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$script_dir/common.sh"
require_cmd python3 gcc make tar autoreconf
: "${BUILD_DIR:?must identify the real candidate build}"
: "${BINDIR:?must identify the real candidate binaries}"
: "${EROFS_TARBALL:?must identify the pinned source archive}"
: "${EROFS_TARBALL_SHA256:?must identify the pinned source archive hash}"
[ -z "${CROSS_PREFIX:-}" ] || die "index tests require native runnable binaries (set TARGET_ARCH to the host)"
candidate="$BUILD_DIR/src/erofs-utils"
[ -f "$candidate/.erofs-recipe" ] && [ -x "$BINDIR/mkfs.erofs" ] \
    && [ -x "$BINDIR/fsck.erofs" ] || die "build the patched candidate before testing"
mkdir -p "$BUILD_DIR/tests"
work="$(mktemp -d "$BUILD_DIR/tests/erofs-index.XXXXXX")"
trap 'rm -rf "$work"' EXIT
mkdir "$work/scratch" "$work/upstream"
tarball="$(resolve_tarball "$EROFS_TARBALL" "$EROFS_TARBALL_SHA256")"
tar -xzf "$tarball" --strip-components=1 -C "$work/upstream"
# Run a pristine-source build with the candidate's exact configure arguments.
# No test-only switch exists in the production build/allocator.
(
    cd "$work/upstream"
    ./autogen.sh
    python3 - "$candidate" <<'PY'
import pathlib, shlex, subprocess, sys
config = pathlib.Path(sys.argv[1]) / 'config.status'
args = shlex.split(subprocess.check_output([str(config), '--config'], text=True))
subprocess.run(['./configure', *args], check=True)
PY
    make -C lib -j2
    make -C mkfs -j2 LDFLAGS=-all-static
    make -C fsck -j2 LDFLAGS=-all-static
) > "$work/upstream-build.log" 2>&1 || { cat "$work/upstream-build.log"; die "pristine source build failed"; }
# Linking through the candidate libtool includes its real backend dependencies.
gcc -g -O1 -Wall -Wno-unused-parameter -DHAVE_CONFIG_H \
    -D_FILE_OFFSET_BITS=64 -I"$candidate" -I"$candidate/include" -I"$candidate/lib" \
    -c "$script_dir/tests/erofs-index-unit.c" -o "$work/unit.o"
"$candidate/libtool" --tag=CC --mode=link gcc -o "$work/unit" "$work/unit.o" \
    "$candidate/lib/liberofs.la" \
    -Wl,--wrap=fallocate64,--wrap=mmap64,--wrap=munmap,--wrap=openat64,--wrap=fstatfs64,--wrap=calloc,--wrap=malloc
"$work/unit" "$work/scratch"
python3 "$script_dir/tests/erofs-index-images.py" \
    "$BINDIR/mkfs.erofs" "$BINDIR/fsck.erofs" \
    "$work/upstream/mkfs/mkfs.erofs" "$work/upstream/fsck/fsck.erofs" "$work/images"
echo 'test-erofs-index: real candidate, pristine compatibility, fault injection PASS'
