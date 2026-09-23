#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${BIN:?BIN must point to the prepared platform binary directory}"
: "${ZOT_BIN:?ZOT_BIN must point to the prepared registry helper}"
: "${E2E_IMAGE:?E2E_IMAGE must identify the prepared guest-runtime fixture}"
for binary in flatten-ctl store-ctl mkfs.erofs; do
    [ -x "$BIN/$binary" ] || { echo "missing prepared product: $BIN/$binary" >&2; exit 1; }
done
for helper in common.sh fixture.py assertions.py process.py; do
    [ -r "$SCRIPT_DIR/$helper" ] || { echo "missing prepared guest-runtime helper: $helper" >&2; exit 1; }
done

# Transitional owner entry while #172 still selects component run_all.sh.
# The focused cases use the final E2E_LIB/<owner> contract. Build that library
# view only from immutable prepared files; do not compile or discover source.
CASE_LIB="$(mktemp -d "${TMPDIR:-/tmp}/guest-runtime-e2e-lib.XXXXXX")"
cleanup() { rm -rf "$CASE_LIB"; }
trap cleanup EXIT INT TERM
mkdir -p "$CASE_LIB/guest-runtime"
for helper in common.sh fixture.py assertions.py process.py; do
    install -m 0644 "$SCRIPT_DIR/$helper" "$CASE_LIB/guest-runtime/$helper"
done

privileged=()
if [ "$EUID" -ne 0 ]; then privileged=(sudo -n); fi
for case in image.flatten.sh image.registry.sh; do
    echo "==> guest-runtime/$case"
    "${privileged[@]}" env \
        PATH="$PATH" \
        BIN="$BIN" \
        E2E_LIB="$CASE_LIB" \
        TEST_BIN="$(dirname "$ZOT_BIN")" \
        E2E_IMAGE="$E2E_IMAGE" \
        KUASAR_ARTIFACT_E2E="${KUASAR_ARTIFACT_E2E:-1}" \
        REQUIRE_GUEST_RUNTIME=1 \
        TMPDIR="${TMPDIR:-/tmp}" \
        bash "$SCRIPT_DIR/cases/$case"
done
