#!/usr/bin/env bash
#
# Pack the current TARGET_ARCH's binaries + test scripts + docs into
# $RELEASE_DIR/mass-sandbox-$VERSION-linux-$TARGET_ARCH.tar.gz.
#
# Inputs (env, all required):
#   VERSION       Release tag (e.g. v0.1)
#   TARGET_ARCH   x86_64 or aarch64
#   BINDIR        Per-arch bin dir (e.g. abs path of bin/x86_64)
#   RELEASE_DIR   Where to write the tarball (e.g. abs path of build/dist)
#
# The tarball layout is intentionally flat under bin/: e2e/perf scripts
# default to BIN=$REPO_ROOT/bin (the symlink farm), and a release tarball
# extracted on a fresh host satisfies that lookup naturally — no per-arch
# subdirs inside the release.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${VERSION:?VERSION required (e.g. v0.1)}"
: "${TARGET_ARCH:?TARGET_ARCH required}"
: "${BINDIR:?BINDIR required}"
: "${RELEASE_DIR:?RELEASE_DIR required}"

require_cmd tar

name="mass-sandbox-${VERSION}-linux-${TARGET_ARCH}"
stage="$RELEASE_DIR/$name"
tarball="$RELEASE_DIR/$name.tar.gz"

# Required binaries — refuse to pack a partial release.
required_bins=(
    manifest-ctl flatten-ctl mkfs.erofs
    store-ctl cache-ctl
    sandbox-ctl sandbox-init sandbox-runtime.erofs
    cloud-hypervisor vmlinux
)

log "checking required binaries under $BINDIR"
missing=()
for b in "${required_bins[@]}"; do
    if [ ! -e "$BINDIR/$b" ]; then
        missing+=("$b")
    fi
done
if [ ${#missing[@]} -gt 0 ]; then
    die "missing binaries in $BINDIR: ${missing[*]}
Build them first:
  make TARGET_ARCH=$TARGET_ARCH build build-cgo cloud-hypervisor vmlinux"
fi

log "staging $stage"
rm -rf "$stage"
mkdir -p "$stage/bin" "$stage/test" "$stage/docs"

# Binaries — copy (not symlink) so the tarball is self-contained.
for b in "${required_bins[@]}"; do
    cp -p "$BINDIR/$b" "$stage/bin/$b"
done

# Test scripts: e2e + perf + scripts.
for d in e2e perf scripts; do
    if [ -d "$repo_root/test/$d" ]; then
        cp -r "$repo_root/test/$d" "$stage/test/$d"
    fi
done

# Docs — README at root, cross-arch guide for build/run reference.
cp "$repo_root/README.md" "$stage/README.md"
[ -f "$repo_root/docs/cross-arch.md" ] && cp "$repo_root/docs/cross-arch.md" "$stage/docs/cross-arch.md"

# VERSION file — release identity for support / bug reports. Includes
# git commit + build date so a binary can be traced back to source state.
git_sha="$(cd "$repo_root" && git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
git_dirty=""
if [ "$git_sha" != "unknown" ]; then
    if ! (cd "$repo_root" && git diff --quiet HEAD 2>/dev/null); then
        git_dirty="-dirty"
    fi
fi
build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
cat > "$stage/VERSION" <<EOF
version:  $VERSION
arch:     $TARGET_ARCH
commit:   $git_sha$git_dirty
built:    $build_date
EOF
log "VERSION:"
sed 's/^/    /' "$stage/VERSION"

# Pack — deterministic ordering for reproducible-ish tarballs.
log "packing $tarball"
mkdir -p "$RELEASE_DIR"
rm -f "$tarball"
tar --sort=name --owner=0 --group=0 --numeric-owner \
    -czf "$tarball" -C "$RELEASE_DIR" "$name"
rm -rf "$stage"

size="$(du -h "$tarball" | cut -f1)"
sha256="$(sha256sum "$tarball" | cut -d' ' -f1)"
log "built $tarball ($size)"
log "sha256 $sha256"
