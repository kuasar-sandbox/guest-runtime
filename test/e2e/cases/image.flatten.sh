#!/usr/bin/env bash
# OCI image/config/ownership -> real EROFS correctness using prepared products.
set -euo pipefail

LIB="${E2E_LIB:?E2E_LIB is required}/guest-runtime"
source "$LIB/common.sh"
for helper in fixture.py assertions.py process.py; do
    [ -r "$LIB/$helper" ] || die "incomplete prepared E2E helpers: missing $helper"
done

FLATTEN_CTL="${BIN:?BIN is required}/flatten-ctl"
ZOT_BIN="${ZOT_BIN:-${E2E_WORKSPACE:?E2E_WORKSPACE is required}/helpers/zot}"
MKFS_EROFS_PATH="$BIN/mkfs.erofs"
FSCK_EROFS="${FSCK_EROFS:-fsck.erofs}"
export MKFS_EROFS_PATH

log preflight
[ "$(uname -s)" = Linux ] || die "E2E requires Linux"
for tool in python3 curl docker timeout readlink stat "$FLATTEN_CTL" "$ZOT_BIN" "$MKFS_EROFS_PATH" "$FSCK_EROFS"; do
    have "$tool" || die "prerequisite not found: $tool"
done
require_root

init_work
echo "  work dir: $WORK"
run docker info >/dev/null 2>&1 || die "docker daemon is not usable"
for tool in "$FLATTEN_CTL" "$ZOT_BIN"; do
    run "$tool" --help >"$WORK/preflight.log" 2>&1 || die "prerequisite is not runnable: $tool"
done
run "$MKFS_EROFS_PATH" --version >"$WORK/mkfs-version.log" 2>&1 || die "mkfs.erofs is not runnable: $MKFS_EROFS_PATH"
FSCK_HELP="$($FSCK_EROFS --help 2>&1 || true)"
grep -Fq -- '--extract' <<<"$FSCK_HELP" || die "fsck.erofs does not support --extract"

capture() {
    local label="$1"
    shift
    if ! run "$@" >"$WORK/$label.out" 2>"$WORK/$label.err"; then
        cat "$WORK/$label.err" >&2
        die "$label failed (output in $WORK/$label.out)"
    fi
}

wait_zot() {
    local pid="$1" port="$2" deadline=$((SECONDS + 30)) status
    while [ "$SECONDS" -lt "$deadline" ] && kill -0 "$pid" 2>/dev/null; do
        status="$(curl -q -sS --max-time 1 -o /dev/null -w '%{http_code}' \
            "http://127.0.0.1:$port/v2/" 2>/dev/null || true)"
        [ "$status" != 200 ] || return 0
        sleep 0.1
    done
    stop_owned "$pid" || true
    echo "e2e: zot failed readiness on 127.0.0.1:$port" >&2
    return 1
}

start_zot() {
    local root="$1" output_var="$2" port dir zot_pid attempt
    for attempt in 1 2; do
        port="$(free_port)"
        dir="$root/attempt-$attempt"
        mkdir -p "$dir"
        python3 - "$dir" "$port" "${ZOT_LOG_LEVEL:-warn}" <<'PY'
import json, pathlib, sys
directory, port, level = sys.argv[1:]
pathlib.Path(directory, "zot-config.json").write_text(json.dumps({
    "storage": {"rootDirectory": directory + "/data", "dedupe": False, "gc": False},
    "http": {"address": "127.0.0.1", "port": port, "compat": ["docker2s2"]},
    "log": {"level": level, "output": directory + "/zot.log"},
}))
PY
        start_owned zot_pid 0 "$ZOT_BIN" serve "$dir/zot-config.json" >"$dir/zot.stdout" 2>&1
        if wait_zot "$zot_pid" "$port"; then
            printf -v "$output_var" '%s' "$port"
            return 0
        fi
    done
    die "zot failed both startup attempts"
}

own_tag() {
    run docker image inspect "$1" >/dev/null 2>&1 && die "refusing to replace an existing Docker tag: $1"
    DOCKER_TAGS+=("$1")
}

seed() {
    own_tag "$1"
    capture tag docker tag "$E2E_IMAGE" "$1"
    capture push docker push "$1"
}

write_remote() {
    python3 - "$1" "$2" "$TMPDIR" <<'PY'
import json, pathlib, sys
pathlib.Path(sys.argv[1]).write_text(json.dumps({
    "insecure": True,
    "pull_jobs": 4,
    "tmpdir": sys.argv[3],
    "cache": {"dir": sys.argv[2], "max_size": "2GiB"},
    "referer": {"validity": "24h"},
}))
PY
}

fixture_arch() {
    local image="$1" value
    capture architecture docker image inspect "$image" --format '{{.Architecture}}'
    value="$(<"$WORK/architecture.out")"
    case "$value" in
        amd64|x86_64) printf '%s' amd64 ;;
        arm64|aarch64) printf '%s' arm64 ;;
        *) die "unsupported prepared image architecture: $value" ;;
    esac
}

RUN_ID="${WORK##*.}"
RUN_ID="${RUN_ID,,}"
if [ -z "${E2E_IMAGE:-}" ]; then
    [ "${KUASAR_ARTIFACT_E2E:-0}" != 1 ] || die "prepared E2E_IMAGE is required"
    capture architecture docker info --format '{{.Architecture}}'
    case "$(<"$WORK/architecture.out")" in
        amd64|x86_64) FIXTURE_ARCH=amd64 ;;
        arm64|aarch64) FIXTURE_ARCH=arm64 ;;
        *) die "unsupported Docker server architecture" ;;
    esac
    E2E_IMAGE="guest-runtime-e2e:$RUN_ID"
    own_tag "$E2E_IMAGE"
    capture fixture python3 "$LIB/fixture.py" "$WORK/fixture.tar" \
        --tag "$E2E_IMAGE" --architecture "$FIXTURE_ARCH"
    capture load docker load --input "$WORK/fixture.tar"
else
    run docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || \
        die "E2E_IMAGE must already be cached: $E2E_IMAGE (no automatic pull)"
    FIXTURE_ARCH="$(fixture_arch "$E2E_IMAGE")"
fi

log "image.flatten: real registry image -> EROFS"
start_zot "$WORK/zot" ZOT_PORT
REPOSITORY="e2e-$RUN_ID/flatten"
REF="127.0.0.1:$ZOT_PORT/$REPOSITORY:v1"
write_remote "$WORK/remote.yaml" "$WORK/cache"
seed "$REF"

capture export "$FLATTEN_CTL" export --output "$WORK/out.erofs" --print-digest \
    --config "$WORK/remote.yaml" --no-progress "$REF"
SUBJECT_REF="$(<"$WORK/export.out")"
SUBJECT_DIGEST="${SUBJECT_REF##*@}"
[[ "$SUBJECT_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] || die "export returned an invalid subject digest"
[ "$SUBJECT_REF" = "${REF%:*}@$SUBJECT_DIGEST" ] || die "export resolved an unexpected subject"
[ -s "$WORK/out.erofs" ] || die "EROFS artifact is missing or empty"

capture info "$FLATTEN_CTL" info --json "$WORK/out.erofs"
check_json info "$WORK/info.out" --fixture-arch "$FIXTURE_ARCH"
ok "image.flatten preserved the real image runtime configuration in a valid EROFS artifact"

# The export is a tarstream bundle. Extract the actual EROFS payload and inspect
# its resulting filesystem so this case proves layer/whiteout/symlink/ownership
# semantics rather than only container metadata.
capture payload "$FLATTEN_CTL" tar extract -f "$WORK/out.erofs" --dense --no-chown \
    "image:$WORK/rootfs.erofs"
[ -s "$WORK/rootfs.erofs" ] || die "flattened EROFS payload is missing or empty"
mkdir -p "$WORK/rootfs"
capture fsck "$FSCK_EROFS" --extract="$WORK/rootfs" "$WORK/rootfs.erofs"

ROOTFS="$WORK/rootfs"
[ "$(cat "$ROOTFS/home/e2e/message")" = "upper layer" ] || die "upper layer replacement content was not preserved"
[ ! -e "$ROOTFS/home/e2e/remove-me" ] || die "whiteouted lower-layer file is still present"
[ ! -e "$ROOTFS/home/e2e/opaque/old" ] || die "opaque whiteout did not hide lower-layer file"
[ ! -e "$ROOTFS/home/e2e/opaque/nested" ] || die "opaque whiteout did not hide lower-layer subtree"
[ "$(cat "$ROOTFS/home/e2e/opaque/new")" = "visible upper file" ] || die "upper file in opaque directory was not preserved"
[ -L "$ROOTFS/home/e2e/current" ] || die "fixture symlink was not preserved"
[ "$(readlink "$ROOTFS/home/e2e/current")" = "message" ] || die "fixture symlink target changed"
[ "$(stat -c '%u:%g' "$ROOTFS/home/e2e")" = "10001:10002" ] || die "fixture directory ownership changed"
[ "$(stat -c '%u:%g' "$ROOTFS/home/e2e/message")" = "10001:10002" ] || die "fixture file ownership changed"
[ "$(stat -c '%u:%g' "$ROOTFS/home/e2e/current")" = "10001:10002" ] || die "fixture symlink ownership changed"
[ -x "$ROOTFS/usr/bin/fixture" ] || die "lower-layer executable mode was not preserved"
[ "$("$ROOTFS/usr/bin/fixture")" = "guest-runtime fixture" ] || die "lower-layer executable content changed"
ok "image.flatten preserved merged filesystem contents, whiteouts, symlink and non-root ownership"

log "image.flatten: PASS"
