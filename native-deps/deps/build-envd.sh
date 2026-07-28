#!/usr/bin/env bash
#
# Build the e2b guest agent (envd) into $BINDIR. envd is injected into
# sandbox-runtime.bundle at /opt/sandbox-runtime/bin/envd (done by
# guest-runtime's `make sandbox-runtime`), where sandbox-init
# auto-bind-mounts it into the guest. It is treated as a TARGET-ARCH binary
# (the guest's arch), built CGO-free so GOARCH alone handles cross-compilation.
#
# Source comes from the e2b-dev/infra release tarball (pinned by tag in the URL),
# fetched + extracted via the shared tarball/src cache like the other deps.
#
# Inputs (env):
#   ENVD_TARBALL          URL or local path; supports "url#filename" form.
#                         Default: e2b-dev/infra 2026.22 github archive.
#   ENVD_TARBALL_SHA256   Optional expected SHA256. Empty → skip verify.
#   BUILD_DIR             Per-arch build directory (e.g. build/x86_64).
#   BINDIR                Per-arch bin directory (e.g. bin/x86_64).
#   ENVD_SRC              Source tree to extract into — arch-neutral and shared
#                         (Go: GOARCH picks the target). Default: build/src/e2b-infra
#                         (i.e. $BUILD_DIR/../src/e2b-infra).
#   TARBALL_CACHE         Optional shared tarball cache (default $BUILD_DIR/tarball).
#   GO_ARCH               Go GOARCH for the target (amd64 | arm64).
#
# envd's go.mod pins a newer Go toolchain (e.g. `go 1.26.3`); with GOTOOLCHAIN=auto
# the Go command fetches it on demand. That toolchain download requires the checksum
# database — keep GOSUMDB enabled (Go refuses to download+run a toolchain when
# GOSUMDB=off). Toolchain + module fetches use the ambient Go environment.
#
# Idempotent: if $BINDIR/envd already exists it exits 0 without rebuilding.
# Delete it to force.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${ENVD_TARBALL:=https://codeload.github.com/e2b-dev/infra/tar.gz/refs/tags/2026.22#e2b-infra-2026.22.tar.gz}"
: "${ENVD_TARBALL_SHA256:=}"
: "${BUILD_DIR:=$(pwd)/build}"
: "${BINDIR:=$(pwd)/bin}"
: "${ENVD_SRC:=$BUILD_DIR/../src/e2b-infra}"
: "${GO_ARCH:=$(go env GOARCH 2>/dev/null || echo amd64)}"

out_bin="$BINDIR/envd"
if [ -x "$out_bin" ]; then
    log "already built: $out_bin (delete it to force rebuild)"
    exit 0
fi

require_cmd go tar

tarball="$(resolve_tarball "$ENVD_TARBALL" "$ENVD_TARBALL_SHA256")"
extract_tarball "$tarball" "$ENVD_SRC" >/dev/null

envd_dir="$ENVD_SRC/packages/envd"
[ -f "$envd_dir/main.go" ] || die "$envd_dir/main.go not found (upstream layout changed?)"

log "go build envd (GOARCH=$GO_ARCH, CGO disabled)"
mkdir -p "$BINDIR"
(
    cd "$envd_dir"
    GOWORK=off GOOS=linux GOARCH="$GO_ARCH" CGO_ENABLED=0 \
        GOFLAGS="${ENVD_GOFLAGS:--mod=mod}" \
        go build -trimpath -ldflags "-s -w" -o "$out_bin" .
)

log "built $out_bin"
file "$out_bin" 2>&1 | head -1 || true
