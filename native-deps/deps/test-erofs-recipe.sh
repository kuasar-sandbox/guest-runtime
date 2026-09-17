#!/usr/bin/env bash
# Executable regressions for root dispatch, relocation and consumed inputs.
set -euo pipefail
# Fixture makes must not inherit caller-specific BINDIR/BUILD_DIR overrides.
unset MAKEFLAGS MFLAGS MAKELEVEL
original_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${BUILD_DIR:?}" "${BINDIR:?}" "${EROFS_TARBALL:?}"
: "${EROFS_TARBALL_SHA256?}"
source "$original_dir/common.sh"
mkdir -p "$BUILD_DIR/tests"
work="$(mktemp -d "$BUILD_DIR/tests/erofs-recipe.XXXXXX")"
trap 'rm -rf "$work"' EXIT
candidate_bin="$BINDIR"
baseline_bin="$BUILD_DIR/tests/erofs-pristine-bin"
[ -x "$baseline_bin/mkfs.erofs" ] && [ -x "$baseline_bin/fsck.erofs" ] \
    || die 'run test-erofs-real.sh first to build the pristine baseline'
archive="$(resolve_tarball "$EROFS_TARBALL" "$EROFS_TARBALL_SHA256")"
fixture="$work/first"
mkdir -p "$fixture/native-deps/deps" "$fixture/native-deps/bin/$TARGET_ARCH" "$fixture/native-deps/build/tarball"
cp "$original_dir/../../Makefile" "$fixture/Makefile"
cp "$original_dir/../Makefile" "$fixture/native-deps/Makefile"
cp "$original_dir/"{build-erofs.sh,common.sh,erofs-recipe.sh} "$fixture/native-deps/deps/"
cp -a "$original_dir/erofs-patches" "$fixture/native-deps/deps/"
cp "$candidate_bin/"{mkfs.erofs,fsck.erofs,.erofs-recipe} "$fixture/native-deps/bin/$TARGET_ARCH/"
cp "$archive" "$fixture/native-deps/build/tarball/source.tar.gz"
export CROSS_PREFIX='' PKG_CONFIG="${PKG_CONFIG:-pkg-config}"
select_fixture() {
    fixture="$1"
    script_dir="$fixture/native-deps/deps"
    export BUILD_DIR="$fixture/native-deps/build/$TARGET_ARCH" BINDIR="$fixture/native-deps/bin/$TARGET_ARCH"
    export TARBALL_CACHE="$fixture/native-deps/build/tarball"
    export EROFS_TARBALL="$TARBALL_CACHE/source.tar.gz"
    # shellcheck disable=SC2034 # Read by the sourced recipe helper.
    tarball="$EROFS_TARBALL"
    src_dir="$BUILD_DIR/src/erofs-utils"
    recipe_stamp="$BINDIR/.erofs-recipe"
    source "$script_dir/erofs-recipe.sh"
}
select_fixture "$fixture"
recipe="$(erofs_recipe_digest)"
erofs_recipe_matches || die 'copied outputs failed portable stamp validation'
bash "$script_dir/build-erofs.sh" > "$work/hit.log" 2>&1
grep -Fq 'already built with matching erofs recipe' "$work/hit.log"
# Cache exports need the single output stamp, not an extracted build tree.
[ ! -e "$src_dir" ] || die 'cache-hit check unexpectedly reconstructed sources'
cp -a "$fixture" "$work/relocated"
select_fixture "$work/relocated"
[ "$(erofs_recipe_digest)" = "$recipe" ] || die 'equal relocated recipe changed identity'
bash "$script_dir/build-erofs.sh" > "$work/relocated.log" 2>&1
grep -Fq 'already built with matching erofs recipe' "$work/relocated.log"
make -C "$fixture" build GO=true SANDBOX_INIT="$(type -P true)" ENVD="$(type -P true)" \
    FLATTEN_CTL="$(type -P true)" BUILD_MKFS_EROFS="$(type -P true)" \
    > "$work/root-hit.log" 2>&1 || { cat "$work/root-hit.log"; die "root cache fixture failed"; }
grep -Fq 'already built with matching erofs recipe' "$work/root-hit.log"
echo 'test-erofs-recipe: relocated cache and ordinary root build reuse PASS'

# For cheap invalidation tests stop before compilation. This fixture header is
# deliberately synthesized AFTER installing the stubs; it is not a build claim.
mkdir -p "$work/tools"
cat > "$work/tools/autoreconf" <<'STUB'
#!/usr/bin/env bash
echo 'test deliberately stops a required rebuild at autoreconf' >&2
exit 89
STUB
chmod +x "$work/tools/autoreconf"
cp "$work/tools/autoreconf" "$work/tools/aclocal"
export PATH="$work/tools:$PATH"
recipe="$(erofs_recipe_digest)"
{ printf 'erofs-recipe-v2\t%s\n' "$recipe"; tail -n +2 "$recipe_stamp"; } > "$work/baseline-stamp"
expect_rebuild() {
    local label="$1" status=0
    shift
    cp "$work/baseline-stamp" "$recipe_stamp"
    "$@" bash "$script_dir/build-erofs.sh" > "$work/$label.log" 2>&1 || status=$?
    if [ "$status" -ne 89 ]; then
        [ "$status" -ne 0 ] && grep -Eq 'target static Libgcrypt/Libgpg-error/uuid link check failed|erofs requires target static' "$work/$label.log" \
            || { cat "$work/$label.log"; die "changed $label was hidden or failed unexpectedly ($status)"; }
    fi
    [ ! -e "$recipe_stamp" ] || die 'failed rebuild retained a valid stamp'
}
for file in Makefile native-deps/Makefile native-deps/deps/erofs-recipe.sh \
    native-deps/deps/erofs-patches/series \
    native-deps/deps/erofs-patches/0002-explicit-libgcrypt-sha256.patch \
    native-deps/deps/erofs-patches/0002-explicit-libgcrypt-sha256.patch.license; do
    cp "$fixture/$file" "$work/saved-input"
    printf '\n# changed recipe input\n' >> "$fixture/$file"
    expect_rebuild "file-${file##*/}" env
    cp "$work/saved-input" "$fixture/$file"
done
for setting in CFLAGS=-O0 CXXFLAGS=-O0 CPPFLAGS=-DRECIPE_TEST LDFLAGS=-s \
    LIBS=-l_review_nonexistent_library PKG_CONFIG_SYSROOT_DIR=/missing-recipe-sysroot \
    PKG_CONFIG_PATH=/missing-recipe-pc libuuid_CFLAGS=-DRECIPE_TEST libuuid_LIBS=-lmissing \
    MAX_BLOCK_SIZE=8192; do
    expect_rebuild "env-${setting%%=*}" env "$setting"
done
# A compiler executable changed in place and changed pkg-config bytes must
# invalidate even when the command/path strings have not changed.
cat > "$work/tools/recipe-cc" <<'STUB'
#!/usr/bin/env bash
exec gcc "$@"
STUB
chmod +x "$work/tools/recipe-cc"
first="$(CC="$work/tools/recipe-cc" erofs_recipe_digest)"
printf '\n# updated compiler wrapper\n' >> "$work/tools/recipe-cc"
[ "$(CC="$work/tools/recipe-cc" erofs_recipe_digest)" != "$first" ] || die 'compiler bytes were not tracked'
mkdir "$work/pkgconfig"
cp "$(pkg-config --variable=pcfiledir uuid)/uuid.pc" "$work/pkgconfig/uuid.pc"
first="$(PKG_CONFIG_LIBDIR="$work/pkgconfig" erofs_recipe_digest)"
printf '\n# updated dependency metadata\n' >> "$work/pkgconfig/uuid.pc"
[ "$(PKG_CONFIG_LIBDIR="$work/pkgconfig" erofs_recipe_digest)" != "$first" ] || die 'pkg-config bytes were not tracked'
# Explicitly empty source checksums keep the documented local-source override.
EROFS_TARBALL_SHA256='' expect_rebuild empty-sha env

first="$(CFLAGS="-DRECIPE_SOURCE=$work/first" erofs_recipe_digest)"
[ "$(CFLAGS="-DRECIPE_SOURCE=$work/relocated" erofs_recipe_digest)" != "$first" ] \
    || die 'meaningful absolute compiler flag was erased from the recipe'

echo 'test-erofs-recipe: helper/Makefiles/series/patch/license/flags/tool/pkg-config invalidation PASS'

# Ordinary root build must enter the inner recipe even with baseline executables.
cp "$baseline_bin/mkfs.erofs" "$BINDIR/mkfs.erofs"
cp "$baseline_bin/fsck.erofs" "$BINDIR/fsck.erofs"
status=0
make -C "$fixture" build GO=true SANDBOX_INIT="$(type -P true)" ENVD="$(type -P true)" \
    FLATTEN_CTL="$(type -P true)" BUILD_MKFS_EROFS="$(type -P true)" \
    > "$work/root-stale.log" 2>&1 || status=$?
[ "$status" -ne 0 ] && grep -Fq 'test deliberately stops a required rebuild' "$work/root-stale.log" \
    || die 'root build accepted baseline executables without recipe validation'
make -C "$fixture" build GO=true SANDBOX_INIT="$(type -P true)" ENVD="$(type -P true)" \
    FLATTEN_CTL="$(type -P true)" BUILD_MKFS_EROFS="$(type -P true)" GUEST_MKFS_EROFS="$(type -P true)" \
    > "$work/root-custom.log" 2>&1
! grep -Fq 'bash deps/build-erofs.sh' "$work/root-custom.log" || die 'explicit custom guest tool was rebuilt'
echo 'test-erofs-recipe: root stale pristine executable rejection and custom override PASS'

# Build against private real inputs, then mutate those exact consumed files.
# No system library/header is modified. Compiler work is limited to two jobs.
export PATH="${PATH#"$work/tools:"}"
select_fixture "$work/first"
mkdir -p "$fixture/inputs"
printf '#define RECIPE_TEST_INPUT 1\n' > "$fixture/inputs/consumed.h"
printf 'int recipe_test_input(void) { return 1; }\n' > "$fixture/inputs/consumed.c"
gcc -c "$fixture/inputs/consumed.c" -o "$fixture/inputs/consumed.o"
ar cr "$fixture/inputs/libconsumed.a" "$fixture/inputs/consumed.o"
export CPPFLAGS="${CPPFLAGS:-} -include $fixture/inputs/consumed.h"
export LIBS="${LIBS:-} $fixture/inputs/libconsumed.a"
EROFS_BUILD_JOBS=2 bash "$script_dir/build-erofs.sh" > "$work/consumed-build.log" 2>&1 \
    || { cat "$work/consumed-build.log"; die 'private dependency build failed'; }
cp "$recipe_stamp" "$work/consumed-stamp"
grep -Fq $'workspace\tinputs/consumed.h' "$recipe_stamp" || die 'consumed header missing from stamp'
grep -Fq $'workspace\tinputs/libconsumed.a' "$recipe_stamp" || die 'consumed archive missing from stamp'
for input in consumed.h libconsumed.a; do
    cp "$fixture/inputs/$input" "$work/saved-input"
    case "$input" in
      *.h) printf '#error changed consumed input\n' > "$fixture/inputs/$input" ;;
      *.a) printf 'invalid archive and invalid linker script @@@\n' > "$fixture/inputs/$input" ;;
    esac
    status=0
    bash "$script_dir/build-erofs.sh" > "$work/consumed-$input.log" 2>&1 || status=$?
    [ "$status" -ne 0 ] && [ ! -e "$recipe_stamp" ] \
        && grep -Fq 'target static Libgcrypt/Libgpg-error/uuid link check failed' "$work/consumed-$input.log" \
        || { cat "$work/consumed-$input.log"; die "cached success hid changed consumed $input"; }
    cp "$work/saved-input" "$fixture/inputs/$input"
    cp "$work/consumed-stamp" "$recipe_stamp"
done
status=0
make -C "$fixture/native-deps" erofs LIBS=-l_review_nonexistent_library \
    > "$work/real-libs.log" 2>&1 || status=$?
[ "$status" -ne 0 ] && [ ! -e "$recipe_stamp" ] && grep -Fq 'target static Libgcrypt/Libgpg-error/uuid link check failed' "$work/real-libs.log" \
    || { cat "$work/real-libs.log"; die 'make cached success hid nonexistent LIBS'; }
echo 'test-erofs-recipe: real consumed header/archive mutations and make LIBS failure PASS'

# A caller cannot silently undo the backend define with a later CFLAGS -U.
status=0
CFLAGS="${CFLAGS:-} -UEROFS_USE_LIBGCRYPT_SHA256" bash "$script_dir/build-erofs.sh" > "$work/no-backend.log" 2>&1 || status=$?
[ "$status" -ne 0 ] && [ ! -e "$recipe_stamp" ] \
    && grep -Fq 'compiled SHA object did not select required Libgcrypt backend' "$work/no-backend.log" \
    || { cat "$work/no-backend.log"; die 'accepted a compiled SHA fallback'; }
echo 'test-erofs-recipe: real compiled backend fallback rejection PASS'
