#!/usr/bin/env bash
# Offline recipe regression: stub the toolchain, exercise the real Make target.
set -euo pipefail
script_dir="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fail() { echo "test-erofs: $*" >&2; exit 1; }
real_make="$(command -v make)"
mkdir -p "$work/native/deps" "$work/tools" "$work/source" "$work/libs"
cp "$script_dir"/{build-erofs.sh,common.sh} "$work/native/deps/"
cp "$script_dir/../Makefile" "$work/native/"
export TEST_EROFS_WORK="$work"
for lib in crypto ssl uuid; do printf '%s v1\n' "$lib" > "$work/libs/lib$lib.a"; done
cat > "$work/tools/gcc" <<'TOOL'
#!/usr/bin/env bash
set -eu
case "$1" in
    --version) echo 'fixture cc 1'; exit ;;
    -dumpmachine|-print-multiarch) echo "${TEST_CC_TARGET:-x86_64}-linux-gnu"; exit ;;
    -print-sysroot) echo /target; exit ;;
esac
[ "${TEST_LINK_FAIL:-0}" = 0 ] || exit 1
while [ "$#" -gt 0 ]; do
    case "$1" in
        -o) printf object > "$2"; shift ;;
        -Wl,-Map,*) printf 'LOAD %s\n' "$TEST_EROFS_WORK"/libs/*.a > "${1#-Wl,-Map,}" ;;
    esac
    shift
done
TOOL
for tool in g++ ar ranlib autoreconf aarch64-linux-gnu-gcc aarch64-linux-gnu-g++ aarch64-linux-gnu-ar aarch64-linux-gnu-ranlib; do
    ln -s gcc "$work/tools/$tool"
done
cat > "$work/tools/pkg-config" <<'TOOL'
#!/usr/bin/env bash
set -eu
printf '%s|%s|%s|%s\n' "$*" "${PKG_CONFIG_LIBDIR-}" "${PKG_CONFIG_PATH-}" "${PKG_CONFIG_SYSROOT_DIR-}" >> "$TEST_EROFS_WORK/pkg.log"
case "$1" in
    --exists) exit "${TEST_PKG_FAIL:-0}" ;;
    --modversion) echo 1 ;;
    --cflags) echo "-I$TEST_EROFS_WORK/include" ;;
    --libs) [ "$2" = --static ]; echo "-L$TEST_EROFS_WORK/libs -lssl -lcrypto -luuid -pthread" ;;
    *) exit 1 ;;
esac
TOOL
cat > "$work/tools/make" <<'TOOL'
#!/usr/bin/env bash
set -eu
if [ "$1" = --version ]; then echo 'fixture make 1'; exit; fi
[ "$1" = -C ]
case "$2" in
    */mkfs|*/fsck)
        printf '#!/bin/sh\nexit 0\n' > "$2/$(basename "$2").erofs"
        chmod +x "$2/$(basename "$2").erofs"
        ;;
esac
TOOL
cat > "$work/tools/readelf" <<'TOOL'
#!/usr/bin/env bash
if [ "${TEST_DYNAMIC:-0}" = 1 ]; then echo INTERP; fi
exit 0
TOOL
cat > "$work/source/autogen.sh" <<'TOOL'
#!/bin/sh
exit 0
TOOL
cat > "$work/source/configure" <<'TOOL'
#!/usr/bin/env bash
set -eu
[[ " $* " == *' --with-openssl '* ]]
[[ " $* " == *' --disable-multithreading '* ]]
[[ " $* " != *' --without-openssl '* ]]
[[ "$openssl_LIBS" == *-pthread* ]]
[ "$CC" = "${CROSS_PREFIX}gcc" ]
echo configure >> "$TEST_EROFS_WORK/configured"
printf '#define HAVE_OPENSSL 1\n' > config.h
if [ "${TEST_NO_EVP:-0}" = 0 ]; then echo '#define HAVE_OPENSSL_EVP_H 1' >> config.h; fi
mkdir lib mkfs fsck
TOOL
chmod +x "$work/tools/"* "$work/source/"*
tar -czf "$work/source.tgz" -C "$work" source
export PATH="$work/tools:$PATH" EROFS_BUILD_JOBS=2
run() {
    "$real_make" -j2 -C "$work/native" erofs EROFS_TARBALL="$work/source.tgz" \
        EROFS_TARBALL_SHA256= "$@" > "$work/run.log" 2>&1 || { cat "$work/run.log" >&2; return 1; }
}
count() { wc -l < "$work/configured"; }
run
[ "$(count)" = 1 ] || fail 'cold build'
run
[ "$(count)" = 1 ] || fail 'unchanged inputs rebuilt'
EROFS_BUILD_JOBS=1 run
[ "$(count)" = 1 ] || fail 'job count invalidated binaries'
printf unrelated > "$work/native/README.md"
run
[ "$(count)" = 1 ] || fail 'unrelated documentation invalidated binaries'
for mutation in recipe common makefile flags library tarball missing-binary; do
    touch "$work/native/build/x86_64/src/erofs-utils/stale-object"
    before="$(count)"
    case "$mutation" in
        recipe) echo '# recipe change' >> "$work/native/deps/build-erofs.sh" ;;
        common) echo '# helper change' >> "$work/native/deps/common.sh" ;;
        makefile) echo '# make recipe change' >> "$work/native/Makefile" ;;
        flags) export CFLAGS=-O1 ;;
        library) echo changed >> "$work/libs/libcrypto.a" ;;
        tarball) echo extra > "$work/source/extra"; tar -czf "$work/source.tgz" -C "$work" source ;;
        missing-binary) rm "$work/native/bin/x86_64/fsck.erofs" ;;
    esac
    run
    [ "$(count)" -eq "$((before + 1))" ] || fail "$mutation did not rebuild"
    [ ! -e "$work/native/build/x86_64/src/erofs-utils/stale-object" ] || fail "$mutation reused stale extraction"
    run
    [ "$(count)" -eq "$((before + 1))" ] || fail "$mutation repeated build"
done
for failure in pkg link backend dynamic; do
    case "$failure" in
        pkg) export TEST_PKG_FAIL=1; expected='erofs requires target static' ;;
        link) export TEST_LINK_FAIL=1; expected='target static OpenSSL/uuid link check failed' ;;
        backend) export TEST_NO_EVP=1; expected='did not enable the required OpenSSL SHA-256 backend'; rm "$work/native/bin/x86_64/fsck.erofs" ;;
        dynamic) export TEST_DYNAMIC=1; expected='erofs output must be fully static' ;;
    esac
    if run > /dev/null 2>&1; then fail "accepted $failure"; fi
    grep -qF "$expected" "$work/run.log" || fail "$failure lacked actionable error"
    unset TEST_PKG_FAIL TEST_LINK_FAIL TEST_NO_EVP TEST_DYNAMIC
done
# Missing cross metadata must fail before configure, never use host defaults.
if TEST_CC_TARGET=aarch64 TEST_PKG_FAIL=1 run TARGET_ARCH=aarch64 > /dev/null 2>&1; then
    fail 'cross pkg-config failure accepted'
fi
grep -Fq '/target/usr/lib/aarch64-linux-gnu/pkgconfig:/target/lib/aarch64-linux-gnu/pkgconfig:/target/usr/share/pkgconfig||/target' "$work/pkg.log" \
    || fail 'cross pkg-config did not isolate target search paths'
TEST_CC_TARGET=aarch64 PKG_CONFIG_LIBDIR=/custom/pc PKG_CONFIG_SYSROOT_DIR=/custom run TARGET_ARCH=aarch64
grep -Fq '|/custom/pc||/custom' "$work/pkg.log" || fail 'cross overrides lost'
printf 'test-erofs: explicit static backend, input reuse/invalidation and cross metadata PASS\n'
