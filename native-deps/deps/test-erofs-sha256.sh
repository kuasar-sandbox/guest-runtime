#!/usr/bin/env bash
set -euo pipefail
script_dir="$(cd "$(dirname "$0")" && pwd)"
: "${BUILD_DIR:?}"
source "$script_dir/common.sh"
src="$BUILD_DIR/src/erofs-utils"
mkdir -p "$BUILD_DIR"
work="$(mktemp -d "$BUILD_DIR/sha-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT
# Native caches retain verified outputs/materials, not a complete source tree.
# Keep the vector test self-contained on such hits without replacing the cached
# tools or inflating the production cache with test-only compilation inputs.
if [ ! -f "$src/lib/sha256.c" ] || [ ! -f "$src/config.h" ]; then
    if ! BUILD_DIR="$work/native" BINDIR="$work/bin" \
        EROFS_BUILD_JOBS="${EROFS_BUILD_JOBS:-2}" \
        bash "$script_dir/build-erofs.sh" > "$work/source-build.log" 2>&1; then
        cat "$work/source-build.log" >&2
        die 'cannot prepare verified SHA test sources after a native cache hit'
    fi
    src="$work/native/src/erofs-utils"
fi
read -r -a compiler <<< "${CC:-gcc}"
[ -z "${CROSS_PREFIX:-}" ] || compiler=("${CROSS_PREFIX}gcc")
read -r -a cppflags <<< "${CPPFLAGS:-}"
read -r -a cflags <<< "${CFLAGS:-}"
read -r -a ldflags <<< "${LDFLAGS:-}"
read -r -a gcrypt_cflags <<< "$("${PKG_CONFIG:-pkg-config}" --cflags libgcrypt gpg-error)"
read -r -a gcrypt_libs <<< "$("${PKG_CONFIG:-pkg-config}" --libs --static libgcrypt gpg-error)"
flags=("${cppflags[@]}" "${cflags[@]}" "${gcrypt_cflags[@]}" -DHAVE_CONFIG_H
    -DEROFS_USE_LIBGCRYPT_SHA256=1 -I"$src" -I"$src/include" -I"$src/lib")
"${compiler[@]}" "${flags[@]}" -c "$src/lib/sha256.c" -o "$work/sha.o"
"${compiler[@]}" "${flags[@]}" -c "$script_dir/test-erofs-sha256.c" -o "$work/test.o"
"${compiler[@]}" "${ldflags[@]}" -static "$work/test.o" "$work/sha.o" "${gcrypt_libs[@]}" -o "$work/test"
# The cross runner is a test-only command, not part of the build/backend API.
read -r -a runner <<< "${EROFS_TEST_RUNNER:-}"
"${runner[@]}" "$work/test" > "$work/digests"
python3 - "$work/digests" <<'PY'
import hashlib, pathlib, sys
sizes = (0, 1, 55, 56, 63, 64, 65, 4095, 4096, 4097, 1048576)
data = bytes((i * 17 + i // 13) & 255 for i in range(max(sizes)))
expected = [hashlib.sha256(b'abc').hexdigest()] + [hashlib.sha256(data[:n]).hexdigest() for n in sizes]
assert pathlib.Path(sys.argv[1]).read_text().splitlines() == expected
PY
"${compiler[@]}" "${flags[@]}" -Dgcry_check_version=test_gcry_check_version \
    -Dgcry_md_open=test_gcry_md_open -c "$src/lib/sha256.c" -o "$work/fault.o"
"${compiler[@]}" "${ldflags[@]}" -static "$work/test.o" "$work/fault.o" "${gcrypt_libs[@]}" -o "$work/fault"
ulimit -c 0
for failure in INIT OPEN; do
    status=0
    env "TEST_SHA_${failure}_FAIL=1" "${runner[@]}" "$work/fault" > "$work/fault.log" 2>&1 || status=$?
    [ "$status" -eq 134 ] || die "SHA $failure failure did not abort ($status)"
    grep -Fq 'erofs: Libgcrypt SHA-256' "$work/fault.log"
done
printf 'test-erofs-sha256: one-shot/streaming SHA256 vectors, boundaries, closed contexts and fatal initialization/allocation failures PASS\n'
