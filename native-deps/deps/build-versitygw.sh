#!/usr/bin/env bash
#
# Build versitygw (Versity S3 Gateway) into $BINDIR. versitygw is an
# S3-compatible gateway over a POSIX filesystem; the orchestrator's build
# pipeline uses an S3 endpoint (builder.files_storage) for COPY build
# contexts, and versitygw is the drop-in S3 endpoint for local testing and
# single-node deployments that have no cloud object store. It is a HOST
# binary (it runs on the node, not in a guest), built CGO-free so GOARCH
# alone handles cross-compilation.
#
# Source comes from the versity/versitygw release tarball (pinned by tag in
# the URL), fetched + extracted via the shared tarball/src cache. The main
# package is ./cmd/versitygw.
#
# Inputs (env):
#   VERSITYGW_TARBALL         URL or local path; supports "url#filename" form.
#                             Default: versity/versitygw v1.5.0 github archive.
#   VERSITYGW_TARBALL_SHA256  Optional expected SHA256. Empty → skip verify.
#   BUILD_DIR                 Per-arch build directory (e.g. build/x86_64).
#   BINDIR                    Per-arch bin directory (e.g. bin/x86_64).
#   VERSITYGW_SRC             Source tree to extract into — arch-neutral and
#                             shared (Go: GOARCH picks the target).
#                             Default: build/src/versitygw.
#   TARBALL_CACHE             Optional shared tarball cache.
#   GO_ARCH                   Go GOARCH for the target (amd64 | arm64).
#
# versitygw's go.mod pins a newer Go toolchain; GOTOOLCHAIN=auto fetches it on
# demand (keep GOSUMDB enabled — Go refuses to download+run a toolchain with
# GOSUMDB=off). Idempotent: if $BINDIR/versitygw exists it exits 0; delete to
# force.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${VERSITYGW_TARBALL:=https://github.com/versity/versitygw/archive/refs/tags/v1.5.0.tar.gz#versitygw-1.5.0.tar.gz}"
: "${VERSITYGW_TARBALL_SHA256:=}"
: "${BUILD_DIR:=$(pwd)/build}"
: "${BINDIR:=$(pwd)/bin}"
: "${VERSITYGW_SRC:=$BUILD_DIR/../src/versitygw}"
: "${GO_ARCH:=$(go env GOARCH 2>/dev/null || echo amd64)}"

out_bin="$BINDIR/versitygw"
if [ -x "$out_bin" ]; then
    log "already built: $out_bin (delete it to force rebuild)"
    exit 0
fi

require_cmd go tar

tarball="$(resolve_tarball "$VERSITYGW_TARBALL" "$VERSITYGW_TARBALL_SHA256")"
extract_tarball "$tarball" "$VERSITYGW_SRC" >/dev/null

main_dir="$VERSITYGW_SRC/cmd/versitygw"
[ -f "$main_dir/main.go" ] || die "$main_dir/main.go not found (upstream layout changed?)"

log "go build versitygw (GOARCH=$GO_ARCH, CGO disabled)"
mkdir -p "$BINDIR"
(
    cd "$VERSITYGW_SRC"
    GOWORK=off GOOS=linux GOARCH="$GO_ARCH" CGO_ENABLED=0 \
        GOFLAGS="${VERSITYGW_GOFLAGS:--mod=mod}" \
        go build -trimpath -ldflags "-s -w" -o "$out_bin" ./cmd/versitygw
)

log "built $out_bin"
file "$out_bin" 2>&1 | head -1 || true
