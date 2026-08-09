#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${BIN:?BIN must point to the assembled platform binary directory}"
: "${ZOT_BIN:?ZOT_BIN must point to the platform-provided registry}"

echo
echo "========================================="
echo "  guest-runtime/e2e_flatten.sh"
echo "========================================="
REQUIRE_GUEST_RUNTIME=1 \
FLATTEN_CTL="${FLATTEN_CTL:-$BIN/flatten-ctl}" \
STORE_CTL="${STORE_CTL:-$BIN/store-ctl}" \
MKFS_EROFS_PATH="${MKFS_EROFS_PATH:-$BIN/mkfs.erofs}" \
ZOT_BIN="$ZOT_BIN" \
    bash "$SCRIPT_DIR/e2e_flatten.sh"
