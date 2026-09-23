#!/usr/bin/env bash
# Real Docker -> zot OCI 1.1 -> flatten-ctl/EROFS -> store-ctl -> referrers.
# The default workload is assembled locally; no workload image is pulled.
set -euo pipefail

LIB="${E2E_LIB:?E2E_LIB is required}/guest-runtime"
source "$LIB/common.sh"
for helper in fixture.py assertions.py process.py; do
    [ -r "$LIB/$helper" ] || die "incomplete prepared E2E helpers: missing $helper"
done

FLATTEN_CTL="${BIN:?BIN is required}/flatten-ctl"
STORE_CTL="$BIN/store-ctl"
TEST_BIN="${TEST_BIN:-${E2E_WORKSPACE:?E2E_WORKSPACE is required}/helpers}"
ZOT_BIN="$TEST_BIN/zot"
MKFS_EROFS_PATH="$BIN/mkfs.erofs"
export MKFS_EROFS_PATH
# Public test data, never caller credentials.
MANIFEST_KEY_A="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
MANIFEST_KEY_B="fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
OWNER_A="owner-a e2e-owner"
OWNER_B="owner-b e2e-owner"
AUTH_USER="e2euser"
AUTH_PASS="e2epass"

log preflight
[ "$(uname -s)" = Linux ] || missing "E2E requires Linux"
for tool in python3 curl docker timeout "$FLATTEN_CTL" "$STORE_CTL" "$ZOT_BIN" "$MKFS_EROFS_PATH"; do
    have "$tool" || missing "prerequisite not found: $tool"
done
# The real layer extractor must preserve non-root ownership. Pass only the
# suite's explicit knobs across sudo, without the caller's credential env.
require_root

init_work
echo "  work dir: $WORK"
run docker info >/dev/null 2>&1 || missing "docker daemon is not usable"
for tool in "$FLATTEN_CTL" "$STORE_CTL" "$ZOT_BIN"; do
    run "$tool" --help >"$WORK/preflight.log" 2>&1 || missing "prerequisite is not runnable: $tool"
done
run "$MKFS_EROFS_PATH" --version >"$WORK/mkfs-version.log" 2>&1 || missing "mkfs.erofs is not runnable: $MKFS_EROFS_PATH"

capture() { # label command...; retain output and fail before dependent assertions
    local label="$1"
    shift
    if ! run "$@" >"$WORK/$label.out" 2>"$WORK/$label.err"; then
        cat "$WORK/$label.err" >&2
        die "$label failed (output in $WORK/$label.out)"
    fi
}

wait_service() { # supervisor-pid port label [HTTP status]
    local pid="$1" port="$2" label="$3" expected="${4:-}" status deadline=$((SECONDS + 30))
    while [ "$SECONDS" -lt "$deadline" ] && kill -0 "$pid" 2>/dev/null; do
        if [ -n "$expected" ]; then
            status="$(curl -q -sS --max-time 1 -o /dev/null -w '%{http_code}' \
                "http://127.0.0.1:$port/v2/" 2>/dev/null || true)"
            [ "$status" != "$expected" ] || return 0
        elif (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
            return 0
        fi
        sleep 0.1
    done
    echo "e2e: $label failed readiness on 127.0.0.1:$port" >&2
    stop_owned "$pid"
    return 1
}

start_zot() { # root output-port-variable [htpasswd]; bounded retry on fresh port
    local root="$1" output_var="$2" htpasswd="${3:-}" attempt port dir zot_pid expected=200
    [ -z "$htpasswd" ] || expected=401
    for attempt in 1 2; do
        port="$(free_port)"
        dir="$root/attempt-$attempt"
        mkdir -p "$dir"
        python3 - "$dir" "$port" "$htpasswd" "${ZOT_LOG_LEVEL:-warn}" <<'PY'
import json, pathlib, sys
directory, port, htpasswd, level = sys.argv[1:]
config = {"storage": {"rootDirectory": directory + "/data", "dedupe": False, "gc": False},
          "http": {"address": "127.0.0.1", "port": port, "compat": ["docker2s2"]},
          "log": {"level": level, "output": directory + "/zot.log"}}
if htpasswd:
    config["http"]["auth"] = {"htpasswd": {"path": htpasswd}}
pathlib.Path(directory, "zot-config.json").write_text(json.dumps(config))
PY
        start_owned zot_pid 0 "$ZOT_BIN" serve "$dir/zot-config.json" >"$dir/zot.stdout" 2>&1
        if wait_service "$zot_pid" "$port" zot "$expected"; then
            printf -v "$output_var" '%s' "$port"
            return 0
        fi
        cat "$dir/zot.stdout" >&2
        [ ! -f "$dir/zot.log" ] || cat "$dir/zot.log" >&2
    done
    die "zot failed both startup attempts"
}

own_tag() {
    if run docker image inspect "$1" >/dev/null 2>&1; then
        die "refusing to replace an existing Docker tag: $1"
    fi
    # Register before load/tag so an interrupt or partial failure is cleaned.
    DOCKER_TAGS+=("$1")
}

seed() {
    own_tag "$1"
    capture tag docker tag "$E2E_IMAGE" "$1"
    capture push docker push "$1"
}

write_remote() { # file cache-directory
    python3 - "$1" "$2" "$TMPDIR" <<'PY'
import json, pathlib, sys
pathlib.Path(sys.argv[1]).write_text(json.dumps({
    "insecure": True, "pull_jobs": 4, "tmpdir": sys.argv[3],
    "cache": {"dir": sys.argv[2], "max_size": "2GiB"}, "referer": {"validity": "24h"}}))
PY
}

lookup() { # label owner config ref expected [manifest-id]
    local label="$1" owner="$2" config="$3" ref="$4" expected="$5" fields=() id_args=()
    [ "$#" -lt 6 ] || id_args=(--id "$6")
    capture "$label" "$FLATTEN_CTL" referer lookup --json --owner "$owner" --config "$config" "$ref"
    check_json lookup "$WORK/$label.out" --subject "${ref%:*}@$SUBJECT_DIGEST" \
        --expect "$expected" "${id_args[@]}" >"$WORK/$label.fields"
    mapfile -t fields <"$WORK/$label.fields"
    LOOKUP_HIT="${fields[0]}"
}

put() { # label owner config subject manifest-id [extra flags]
    local label="$1" owner="$2" config="$3" subject="$4" manifest_id="$5"
    shift 5
    capture "$label" "$FLATTEN_CTL" referer put --json --owner "$owner" --config "$config" \
        --manifest-id "$manifest_id" "$@" "$subject"
    check_json put "$WORK/$label.out" --subject "$subject" --id "$manifest_id"
}

upload() { # label key config ref output-id-variable
    local label="$1" key="$2" config="$3" ref="$4" output_var="$5" manifest_id
    MANIFEST_KEY="$key" capture "$label" "$FLATTEN_CTL" export --upload --no-progress \
        --manifest-config "$WORK/manifest.yaml" --config "$config" "$ref"
    manifest_id="$(<"$WORK/$label.out")"
    [[ "$manifest_id" =~ ^[0-9a-f]{64}$ ]] || die "$label returned an invalid manifest ID"
    printf -v "$output_var" '%s' "$manifest_id"
}

fetch_referrers() { # label port repository digest
    capture "$1" curl -q -fsS --max-time 10 \
        "http://127.0.0.1:$2/v2/$3/referrers/$4"
}

# The random tag is owned by this invocation; config and layer bytes are fixed.
RUN_ID="${WORK##*.}"
RUN_ID="${RUN_ID,,}"
REPOSITORY="e2e-$RUN_ID/app"
FIXTURE_ARCH=""
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
    run docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || missing "E2E_IMAGE must already be cached: $E2E_IMAGE (no automatic pull)"
fi

log "start store-ctl and anonymous zot"
STORE_PORT="$(free_port)"
mkdir -p "$WORK/store"
python3 - "$WORK" "$STORE_PORT" <<'PY'
import json, pathlib, sys
work, port = sys.argv[1:]
configs = {
    "store.yaml": {"listen": "127.0.0.1:" + port, "backend": "fs",
                   "fs": {"root": work + "/store", "verify_content_key": True}},
    "manifest.yaml": {"manifest": {"key": ""},
                      "store": {"endpoint": "127.0.0.1:" + port, "pool": 4, "timeout": "30s"},
                      "cache": {"endpoint": ""},
                      "chunker": {"mode": "cdc", "cdc": {"min": "128KiB", "avg": "512KiB", "max": "1MiB"}},
                      "crypto": {"chunk": "aes", "manifest": "aes"}},
}
for name, config in configs.items():
    pathlib.Path(work, name).write_text(json.dumps(config))
PY
capture store-init "$STORE_CTL" init --config "$WORK/store.yaml" --generation G1
start_owned STORE_PID 0 "$STORE_CTL" serve --config "$WORK/store.yaml" >"$WORK/store-serve.log" 2>&1
wait_service "$STORE_PID" "$STORE_PORT" store-ctl || { cat "$WORK/store-serve.log" >&2; die "store startup failed"; }
start_zot "$WORK/zot-anon" ZOT_PORT
write_remote "$WORK/remote.yaml" "$WORK/cache-anon"
REF="127.0.0.1:$ZOT_PORT/$REPOSITORY:v1"
seed "$REF"

log "TEST 1: registry export and real EROFS info"
capture t1-export "$FLATTEN_CTL" export --output "$WORK/out.erofs" --print-digest \
    --config "$WORK/remote.yaml" --no-progress "$REF"
SUBJECT_REF="$(<"$WORK/t1-export.out")"
SUBJECT_DIGEST="${SUBJECT_REF##*@}"
if ! [[ "$SUBJECT_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] ||
    [ "$SUBJECT_REF" != "${REF%:*}@$SUBJECT_DIGEST" ]; then
    die "export returned an invalid subject digest"
fi
[ -s "$WORK/out.erofs" ] || die "EROFS artifact is missing or empty"
capture t1-info "$FLATTEN_CTL" info --json "$WORK/out.erofs"
info_args=()
[ -z "$FIXTURE_ARCH" ] || info_args=(--fixture-arch "$FIXTURE_ARCH")
check_json info "$WORK/t1-info.out" "${info_args[@]}"
ok "export and info read real EROFS and the image runtime configuration"

log "TEST 2: OCI 1.1 lookup, upload, put and idempotent reuse"
lookup t2-miss "$OWNER_A" "$WORK/remote.yaml" "$REF" miss
upload t2-upload "$MANIFEST_KEY_A" "$WORK/remote.yaml" "$REF" ID1
put t2-put "$OWNER_A" "$WORK/remote.yaml" "$SUBJECT_REF" "$ID1"
lookup t2-hit "$OWNER_A" "$WORK/remote.yaml" "$REF" hit "$ID1"
lookup t2-reuse "$OWNER_A" "$WORK/remote.yaml" "$REF" hit "$ID1"
fetch_referrers t2-referrers "$ZOT_PORT" "$REPOSITORY" "$SUBJECT_DIGEST"
check_json referrers "$WORK/t2-referrers.out" --owner "$OWNER_A" --id "$ID1" >"$WORK/t2-record"
ok "supported miss -> put -> repeated hit with the same ID; real OCI artifact indexed"

log "TEST 3: owner and key isolation"
write_remote "$WORK/remote-b.yaml" "$WORK/cache-b"
lookup t3-miss "$OWNER_B" "$WORK/remote-b.yaml" "$REF" miss
upload t3-upload "$MANIFEST_KEY_B" "$WORK/remote-b.yaml" "$REF" ID3
[ "$ID3" != "$ID1" ] || die "different key produced the same manifest ID"
put t3-put "$OWNER_B" "$WORK/remote-b.yaml" "$SUBJECT_REF" "$ID3"
lookup t3-owner-b "$OWNER_B" "$WORK/remote-b.yaml" "$REF" hit "$ID3"
lookup t3-owner-a "$OWNER_A" "$WORK/remote.yaml" "$REF" hit "$ID1"
ok "each owner reuses its own ID; different keys produce different IDs"

log "TEST 4: observe LIVE then EXPIRED while registry remains supported"
EXPIRED_REPOSITORY="e2e-$RUN_ID/expired"
EXPIRED_REF="127.0.0.1:$ZOT_PORT/$EXPIRED_REPOSITORY:v1"
EXPIRED_SUBJECT="${EXPIRED_REF%:*}@$SUBJECT_DIGEST"
EXPIRY_TTL_SECONDS=10
seed "$EXPIRED_REF"
lookup t4-initial "$OWNER_A" "$WORK/remote.yaml" "$EXPIRED_REF" miss
T4_LIVE=0
for attempt in 1 2 3; do
    put "t4-put-$attempt" "$OWNER_A" "$WORK/remote.yaml" "$EXPIRED_SUBJECT" "$ID1" \
        --validity "${EXPIRY_TTL_SECONDS}s"
    lookup "t4-live-$attempt" "$OWNER_A" "$WORK/remote.yaml" "$EXPIRED_REF" any
    if [ "$LOOKUP_HIT" = hit ]; then
        check_json lookup "$WORK/t4-live-$attempt.out" --subject "$EXPIRED_SUBJECT" --expect hit --id "$ID1" >/dev/null
        T4_LIVE=1
        break
    fi
done
[ "$T4_LIVE" = 1 ] || die "could not observe a LIVE expiring referrer"
fetch_referrers t4-index-live "$ZOT_PORT" "$EXPIRED_REPOSITORY" "$SUBJECT_DIGEST"
check_json referrers "$WORK/t4-index-live.out" --owner "$OWNER_A" --id "$ID1" --expiring >"$WORK/t4-record"
T4_RECORD="$(<"$WORK/t4-record")"
deadline=$((SECONDS + EXPIRY_TTL_SECONDS + 10))
while :; do
    lookup t4-expiry "$OWNER_A" "$WORK/remote.yaml" "$EXPIRED_REF" any
    [ "$LOOKUP_HIT" != miss ] || break
    [ "$SECONDS" -lt "$deadline" ] || die "expired referrer was still returned"
    sleep 0.2
done
fetch_referrers t4-index-expired "$ZOT_PORT" "$EXPIRED_REPOSITORY" "$SUBJECT_DIGEST"
check_json referrers "$WORK/t4-index-expired.out" --owner "$OWNER_A" --id "$ID1" \
    --record "$T4_RECORD" --expired >/dev/null
ok "LIVE record became a supported miss; the same expired artifact remains indexed"

log "TEST 5: auth denial, FLATTEN_REGISTRY_* credentials, authenticated post-put hit"
cat >"$WORK/htpasswd" <<'EOF'
e2euser:$2y$05$/Jvk/Gj8hT1jwrfwYfy89OeTyXpVOpkH3Bpy3UrFx0XnTG5rmy6eq
EOF
start_zot "$WORK/zot-auth" AUTH_PORT "$WORK/htpasswd"
AUTH_REF="127.0.0.1:$AUTH_PORT/$REPOSITORY:v1"
AUTH_SUBJECT="${AUTH_REF%:*}@$SUBJECT_DIGEST"
# Write only our private push config. Never login/logout or read caller auth.
write_docker_auth "$WORK/docker-auth/config.json" "127.0.0.1:$AUTH_PORT" "$AUTH_USER" "$AUTH_PASS"
DOCKER_CONFIG="$WORK/docker-auth" seed "$AUTH_REF"
write_remote "$WORK/remote-auth.yaml" "$WORK/cache-auth"
capture t5-http-denied curl -q -sS --max-time 10 -o "$WORK/t5-http-body" -w '%{http_code}' \
    "http://127.0.0.1:$AUTH_PORT/v2/$REPOSITORY/manifests/v1"
[ "$(<"$WORK/t5-http-denied.out")" = 401 ] || die "auth registry did not return HTTP 401"
if run "$FLATTEN_CTL" export --output "$WORK/auth-noauth.erofs" --config "$WORK/remote-auth.yaml" \
    --no-progress "$AUTH_REF" >"$WORK/t5-noauth.out" 2>"$WORK/t5-noauth.err"; then
    die "anonymous export unexpectedly succeeded"
else
    status=$?
    [ "$status" = 1 ] || die "anonymous export exited with unexpected status $status"
    check_json auth-denied "$WORK/t5-noauth.err"
fi
ok "anonymous export failed with an authentication denial from the live registry"

authed() { FLATTEN_REGISTRY_USERNAME="$AUTH_USER" FLATTEN_REGISTRY_PASSWORD="$AUTH_PASS" "$@"; }
authed lookup t5-miss "$OWNER_A" "$WORK/remote-auth.yaml" "$AUTH_REF" miss
authed upload t5-upload "$MANIFEST_KEY_A" "$WORK/remote-auth.yaml" "$AUTH_REF" ID4
authed put t5-put "$OWNER_A" "$WORK/remote-auth.yaml" "$AUTH_SUBJECT" "$ID4"
authed lookup t5-hit "$OWNER_A" "$WORK/remote-auth.yaml" "$AUTH_REF" hit "$ID4"
ok "credentialed lookup, export/upload, put and post-put hit succeeded"

log "ALL E2E ASSERTIONS PASSED (cleanup follows)"
