#!/usr/bin/env bash
# Shared only by this suite and its focused script regression tests.

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PIDS=()
DOCKER_TAGS=()
WORK=""

log() { printf '\n=== %s ===\n' "$*"; }
ok() { printf '  [ ok ] %s\n' "$*"; }
die() { printf '[FAIL] %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }
missing() { die "$*"; }
require_root() { [ "$(id -u)" -eq 0 ] || die "root is required"; }

start_owned() { # output-pid-variable timeout-seconds command...
    local output_var="$1" limit="$2" parent_pid="$BASHPID"
    shift 2
    python3 "$E2E_DIR/process.py" --parent "$parent_pid" --timeout "$limit" -- "$@" <&0 &
    PIDS+=("$!")
    printf -v "$output_var" '%s' "$!"
}

forget_pid() {
    local target="$1" pid kept=()
    for pid in "${PIDS[@]}"; do
        [ "$pid" = "$target" ] || kept+=("$pid")
    done
    PIDS=("${kept[@]}")
}

run() {
    local command_pid status=0
    start_owned command_pid 180 "$@"
    wait "$command_pid" || status=$?
    forget_pid "$command_pid"
    return "$status"
}

owned_alive() {
    local status
    status=$(cat -- "/proc/$1/status" 2>/dev/null) || return 1
    if [[ "$status" =~ (^|$'\n')PPid:[[:blank:]]+([0-9]+)($|$'\n') ]]; then
        if [ "${BASH_REMATCH[2]}" = "$BASHPID" ]; then return 0; else return 1; fi
    fi
    return 1
}

stop_owned() {
    local pid="$1" attempt
    if owned_alive "$pid"; then kill -TERM "$pid" 2>/dev/null || true; fi
    for ((attempt=0; attempt<60; attempt++)); do
        owned_alive "$pid" || break
        sleep 0.1
    done
    if owned_alive "$pid"; then
        printf 'e2e: supervisor %s exceeded cleanup deadline\n' "$pid" >&2
        kill -KILL "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
        forget_pid "$pid"
        return 1
    fi
    wait "$pid" 2>/dev/null || true
    forget_pid "$pid"
}

cleanup() {
    local status=$? pid tag cleanup_failed=0
    trap - EXIT
    trap '' INT TERM
    for pid in "${PIDS[@]}"; do
        if owned_alive "$pid"; then kill -TERM "$pid" 2>/dev/null || true; fi
    done
    for pid in "${PIDS[@]}"; do stop_owned "$pid" || cleanup_failed=1; done
    for tag in "${DOCKER_TAGS[@]}"; do
        if ! timeout --kill-after=1s 10s docker image rm "$tag" >"$WORK/remove.log" 2>&1; then
            if ! timeout --kill-after=1s 10s docker info >/dev/null 2>&1 ||
                timeout --kill-after=1s 10s docker image inspect "$tag" >/dev/null 2>&1; then
                printf 'e2e: could not remove owned tag %s\n' "$tag" >&2
                cat "$WORK/remove.log" >&2
                cleanup_failed=1
            fi
        fi
    done
    if [ -n "$WORK" ]; then
        if [ "${E2E_KEEP:-0}" = 1 ]; then
            printf 'E2E_KEEP=1: evidence retained at %s after service/tag cleanup\n' "$WORK"
        else
            rm -rf -- "$WORK" || cleanup_failed=1
        fi
    fi
    [ "$status" -ne 0 ] || status="$cleanup_failed"
    exit "$status"
}

init_work() {
    umask 077
    WORK="$(mktemp -d "${TMPDIR:-/tmp}/guest-runtime-e2e.XXXXXXXX")"
    trap cleanup EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    export DOCKER_CONFIG="$WORK/docker" TMPDIR="$WORK/tmp"
    mkdir -p "$DOCKER_CONFIG" "$TMPDIR" "$WORK/docker-auth"
    printf '{"auths":{}}\n' >"$DOCKER_CONFIG/config.json"
    unset DOCKER_AUTH_CONFIG DOCKER_CONTEXT
    unset FLATTEN_REGISTRY_TOKEN FLATTEN_REGISTRY_USERNAME FLATTEN_REGISTRY_PASSWORD
    export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
}

free_port() {
    python3 - <<'PY'
import socket
with socket.socket() as sock:
    sock.bind(("127.0.0.1", 0))
    print(sock.getsockname()[1])
PY
}

check_json() { run python3 "$E2E_DIR/assertions.py" "$@"; }

write_docker_auth() { # owned config path registry username password (test values)
    python3 - "$@" <<'PY'
import base64, json, pathlib, sys
path, registry, username, password = sys.argv[1:]
auth = base64.b64encode((username + ":" + password).encode()).decode()
pathlib.Path(path).write_text(json.dumps({"auths": {registry: {"auth": auth}}}))
PY
}
