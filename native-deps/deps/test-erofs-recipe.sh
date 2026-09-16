#!/usr/bin/env bash
# Exercise the real build entry point with old executable files present. A
# deliberately failing autoconf stub keeps this cache regression inexpensive.
set -euo pipefail
original_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${BUILD_DIR:?}"
: "${EROFS_TARBALL:?}"
: "${EROFS_TARBALL_SHA256:?}"
source "$original_dir/common.sh"
mkdir -p "$BUILD_DIR/tests"
work="$(mktemp -d "$BUILD_DIR/tests/erofs-recipe.XXXXXX")"
trap 'rm -rf "$work"' EXIT
export BUILD_DIR="$work/build" BINDIR="$work/bin" CROSS_PREFIX=''
mkdir -p "$work/deps" "$BINDIR" "$work/tools" "$BUILD_DIR/src/erofs-utils"
cp "$original_dir/"{build-erofs.sh,common.sh,erofs-recipe.sh} "$work/deps/"
cp -a "$original_dir/erofs-patches" "$work/deps/"
script_dir="$work/deps"
src_dir="$BUILD_DIR/src/erofs-utils"
source "$script_dir/erofs-recipe.sh"
tarball="$(resolve_tarball "$EROFS_TARBALL" "$EROFS_TARBALL_SHA256")"
erofs_recipe_digest > "$BINDIR/.erofs-recipe"
cp "$BINDIR/.erofs-recipe" "$src_dir/.erofs-recipe"
touch "$src_dir/.extracted"
cp "$(type -P true)" "$BINDIR/mkfs.erofs"
cp "$(type -P true)" "$BINDIR/fsck.erofs"
bash "$script_dir/build-erofs.sh" > "$work/hit.log" 2>&1
grep -Fq 'already built with matching erofs recipe' "$work/hit.log"
cat > "$work/tools/autoreconf" <<'EOF'
#!/usr/bin/env bash
echo 'test deliberately stops a required rebuild at autoreconf' >&2
exit 89
EOF
chmod +x "$work/tools/autoreconf"
cp "$work/tools/autoreconf" "$work/tools/autoconf"
export PATH="$work/tools:$PATH"
printf '\n# cache regression: patch bytes changed\n' >> "$script_dir/erofs-patches/0001-optional-disk-chunk-indexes.patch"
status=0
bash "$script_dir/build-erofs.sh" > "$work/miss.log" 2>&1 || status=$?
[ "$status" -eq 89 ] || { cat "$work/miss.log"; die "changed patch was hidden by old executables"; }
[ ! -e "$BINDIR/.erofs-recipe" ] || die "failed rebuild left a valid recipe stamp"
grep -Fq 'EROFS_INDEX_STORAGE' "$src_dir/lib/blobchunk.c"
grep -Fq 'applying erofs patch:' "$work/miss.log"
# Make itself must call the recipe check even when mkfs exists. This dependency
# is phony intentionally; timestamps and an old .extracted marker are insufficient.
make -n -C "$original_dir/.." erofs BINDIR="$BINDIR" BUILD_DIR="$BUILD_DIR" \
    > "$work/make.log"
grep -Fq 'bash deps/build-erofs.sh' "$work/make.log"
echo 'test-erofs-recipe: matching reuse, changed patch rejection with stale binaries, explicit application PASS'
