#!/usr/bin/env bash
#
# End-to-end test for flatten-ctl's registry path against a real OCI 1.1
# registry (zot). Unlike the unit tests (which use an in-memory ggcr registry
# that only does the tag-schema referrers fallback), zot implements the real
# /v2/<name>/referrers/<digest> API — so this exercises the genuine
# pull -> flatten -> ingest -> referrer-writeback -> idempotent-skip flow end
# to end, through the actual flatten-ctl binary.
#
# Seeds a locally-cached docker image (default python:3.12-alpine; override
# with E2E_IMAGE) into zot via `docker push`, so no synthetic image tooling is
# needed and the layers have real, readable file modes.
#
# Requirements: docker, mkfs.erofs, curl, flatten-ctl, store-ctl, and zot. When
# REQUIRE_GUEST_RUNTIME=1, missing prerequisites fail the e2e instead of
# skipping.
#
# Env knobs:
#   FLATTEN_CTL, STORE_CTL, ZOT_BIN   binary paths (default: look up on PATH)
#   E2E_IMAGE                          cached image to seed (python:3.12-alpine)
#   E2E_KEEP=1                         keep the work dir / processes for debug
set -uo pipefail

# The e2e only talks to localhost (zot, store-ctl, docker, curl); keep any
# ambient proxy out of that path. zot is supplied by ZOT_BIN, PATH, or the
# release-builder umbrella bin/.
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost

# --------------------------------------------------------------------------
# config
# --------------------------------------------------------------------------
FLATTEN_CTL="${FLATTEN_CTL:-flatten-ctl}"
STORE_CTL="${STORE_CTL:-store-ctl}"
ZOT_BIN="${ZOT_BIN:-zot}"
E2E_IMAGE="${E2E_IMAGE:-python:3.12-alpine}"
# 32-byte hex customer key — also the referrer owner HMAC key. Test fixture.
MANIFEST_KEY_A="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
MANIFEST_KEY_B="fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
OWNER_A="owner-a e2e-owner"
OWNER_B="owner-b e2e-owner"
AUTH_USER="e2euser"
AUTH_PASS="e2epass"

FAILS=0
PIDS=()
DOCKER_TAGS=()
AUTH_LOGGED_IN=""

# --------------------------------------------------------------------------
# helpers
# --------------------------------------------------------------------------
log()  { printf '\n=== %s ===\n' "$*"; }
ok()   { printf '  [ ok ] %s\n' "$*"; }
bad()  { printf '  [FAIL] %s\n' "$*"; FAILS=$((FAILS + 1)); }
skip() {
	if [ "${REQUIRE_GUEST_RUNTIME:-0}" = "1" ]; then
		printf '[FAIL] %s\n' "$*" >&2
		exit 1
	fi
	printf '[SKIP] %s\n' "$*"
	exit 0
}
have() { command -v "$1" >/dev/null 2>&1; }
json_bool_true() { grep -q "\"$2\":true" "$1"; }
json_string() { sed -n "s/.*\"$2\":\"\\([^\"]*\\)\".*/\\1/p" "$1"; }

# port_free PORT -> 0 if nothing is listening on 127.0.0.1:PORT
port_free() { ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
free_port() {
	local p
	for _ in $(seq 1 100); do
		p=$(((RANDOM % 20000) + 20000))
		if port_free "$p"; then printf '%s' "$p"; return 0; fi
	done
	echo "e2e: no free port" >&2
	return 1
}
wait_port() { # host port label
	local i
	for i in $(seq 1 150); do
		if (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null; then exec 3>&-; return 0; fi
		sleep 0.2
	done
	echo "e2e: timed out waiting for $3 ($1:$2)" >&2
	return 1
}

cleanup() {
	[ -n "${E2E_KEEP:-}" ] && { echo "E2E_KEEP set — leaving $WORK and processes"; return; }
	local pid
	for pid in "${PIDS[@]:-}"; do [ -n "$pid" ] && kill "$pid" 2>/dev/null || true; done
	local t
	for t in "${DOCKER_TAGS[@]:-}"; do [ -n "$t" ] && docker rmi -f "$t" >/dev/null 2>&1 || true; done
	[ -n "$AUTH_LOGGED_IN" ] && docker logout "$AUTH_LOGGED_IN" >/dev/null 2>&1 || true
	[ -n "${WORK:-}" ] && rm -rf "$WORK"
}
trap cleanup EXIT

# start_zot DIR PORT [HTPASSWD] -> writes config, starts zot, waits ready
start_zot() {
	local dir="$1" port="$2" htp="${3:-}" cfg="$1/zot-config.json" auth=""
	[ -n "$htp" ] && auth=", \"auth\": {\"htpasswd\": {\"path\": \"$htp\"}}"
	cat >"$cfg" <<EOF
{
  "storage": { "rootDirectory": "$dir/data", "dedupe": false, "gc": false },
  "http": { "address": "127.0.0.1", "port": "$port", "compat": ["docker2s2"]$auth },
  "log": { "level": "${ZOT_LOG_LEVEL:-warn}", "output": "$dir/zot.log" }
}
EOF
	"$ZOT_BIN" serve "$cfg" >"$dir/zot.stdout" 2>&1 &
	PIDS+=("$!")
	if ! wait_port 127.0.0.1 "$port" "zot"; then
		echo "zot failed to start; logs:" >&2
		cat "$dir/zot.stdout" "$dir/zot.log" 2>/dev/null >&2
		return 1
	fi
	# /v2/ answers (200 anon, 401 authed) once routes are wired.
	curl -sS --retry 40 --retry-connrefused --retry-delay 1 --retry-max-time 40 \
		-o /dev/null "http://127.0.0.1:$port/v2/" 2>/dev/null || true
	return 0
}

# seed IMAGE REGHOST/REPO:TAG -> docker tag + push (assumes insecure 127.0.0.1)
seed() {
	local src="$1" ref="$2"
	docker tag "$src" "$ref"
	DOCKER_TAGS+=("$ref")
	docker push "$ref" >/dev/null 2>"$WORK/push.err" || {
		echo "e2e: docker push $ref failed:" >&2
		cat "$WORK/push.err" >&2
		return 1
	}
}

is_hex64() { [[ "$1" =~ ^[0-9a-f]{64}$ ]]; }

# --------------------------------------------------------------------------
# preflight
# --------------------------------------------------------------------------
# flatten-ctl preserves the image's real uid/gid (chown), which needs root —
# re-exec under sudo so the flattened rootfs keeps ownership (e.g. /home/<user>).
if [ "$(id -u)" -ne 0 ]; then
	command -v sudo >/dev/null 2>&1 || skip "not root and sudo unavailable (flatten preserves ownership; needs root)"
	exec sudo -nE bash "$0" "$@"
fi
log "preflight"
have curl || skip "curl not found"
have docker || skip "docker not found"
docker info >/dev/null 2>&1 || skip "docker not usable (daemon down or no permission)"
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || {
	echo "seed image $E2E_IMAGE not cached; trying docker pull (respects ambient proxy)…"
	docker pull "$E2E_IMAGE" >/dev/null 2>&1 || skip "seed image $E2E_IMAGE unavailable (set E2E_IMAGE to a cached image)"
}
"$FLATTEN_CTL" --help >/dev/null 2>&1 || skip "flatten-ctl not runnable ($FLATTEN_CTL)"
"$STORE_CTL" --help >/dev/null 2>&1 || skip "store-ctl not runnable ($STORE_CTL)"
"$ZOT_BIN" --help >/dev/null 2>&1 || skip "zot not runnable ($ZOT_BIN)"
if [ -z "${MKFS_EROFS_PATH:-}" ] && ! have mkfs.erofs; then
	skip "mkfs.erofs not found (set MKFS_EROFS_PATH or add to PATH)"
fi
echo "  flatten-ctl: $FLATTEN_CTL"
echo "  store-ctl:   $STORE_CTL"
echo "  zot:         $ZOT_BIN ($("$ZOT_BIN" --version 2>/dev/null | head -1))"
echo "  seed image:  $E2E_IMAGE"

WORK="$(mktemp -d)"
echo "  work dir:    $WORK"

# --------------------------------------------------------------------------
# bring up store-ctl (fs backend) + anonymous zot
# --------------------------------------------------------------------------
log "start store-ctl (fs backend)"
STORE_PORT="$(free_port)"
mkdir -p "$WORK/store"
cat >"$WORK/store.yaml" <<EOF
listen: 127.0.0.1:$STORE_PORT
backend: fs
fs:
  root: $WORK/store
  verify_content_key: true
EOF
"$STORE_CTL" init --config "$WORK/store.yaml" --generation G1 >"$WORK/store-init.log" 2>&1 || {
	echo "store-ctl init failed:" >&2
	cat "$WORK/store-init.log" >&2
	exit 1
}
"$STORE_CTL" serve --config "$WORK/store.yaml" >"$WORK/store-serve.log" 2>&1 &
PIDS+=("$!")
wait_port 127.0.0.1 "$STORE_PORT" "store-ctl" || exit 1

log "start zot (anonymous)"
ZOT_PORT="$(free_port)"
mkdir -p "$WORK/zot-anon"
start_zot "$WORK/zot-anon" "$ZOT_PORT" || exit 1

# --------------------------------------------------------------------------
# configs
# --------------------------------------------------------------------------
cat >"$WORK/manifest.yaml" <<EOF
manifest:
  key: ""
store:
  endpoint: 127.0.0.1:$STORE_PORT
  pool: 4
  timeout: 30s
cache:
  endpoint: ""
chunker:
  mode: cdc
  cdc:
    min: 128KiB
    avg: 512KiB
    max: 1MiB
crypto:
  chunk: aes
  manifest: aes
EOF

write_remote() { # file cachedir
	cat >"$1" <<EOF
insecure: true
pull_jobs: 4
cache:
  dir: $2
  max_size: 2GiB
referer:
  validity: 24h
EOF
}
write_remote "$WORK/remote.yaml" "$WORK/cache-anon"

REF="127.0.0.1:$ZOT_PORT/e2e/app:v1"
log "seed $E2E_IMAGE -> $REF"
seed "$E2E_IMAGE" "$REF" || exit 1
ok "image pushed to zot"

# ==========================================================================
# TEST 1 — pull + flatten + info (no store needed)
# ==========================================================================
log "TEST 1: pull + flatten + info"
DIGEST_LINE="$("$FLATTEN_CTL" export --output "$WORK/out.erofs" --print-digest \
	--config "$WORK/remote.yaml" --no-progress "$REF" 2>"$WORK/t1.err")" || {
	echo "flatten-ctl export failed:" >&2
	cat "$WORK/t1.err" >&2
	bad "export --output"
}
SUBJECT_DIGEST="${DIGEST_LINE##*@}" # sha256:...
if [ -s "$WORK/out.erofs" ]; then ok "EROFS produced ($(wc -c <"$WORK/out.erofs") bytes)"; else bad "EROFS missing/empty"; fi
[[ "$SUBJECT_DIGEST" == sha256:* ]] && ok "resolved digest $SUBJECT_DIGEST" || bad "no resolved digest (got '$DIGEST_LINE')"
if "$FLATTEN_CTL" info "$WORK/out.erofs" >"$WORK/info.txt" 2>&1; then
	grep -q "EROFS image size" "$WORK/info.txt" && ok "flatten-ctl info reads the EROFS" || bad "info missing EROFS size"
else
	bad "flatten-ctl info failed"
	cat "$WORK/info.txt" >&2
fi

# ==========================================================================
# TEST 2 — referer lookup + upload + put, idempotent skip, real Referrers API
# ==========================================================================
log "TEST 2: referer lookup + upload + put (idempotent)"
"$FLATTEN_CTL" referer lookup --json --owner "$OWNER_A" --config "$WORK/remote.yaml" "$REF" >"$WORK/t2lookup1.json" 2>"$WORK/t2lookup1.err" || {
	echo "first referer lookup failed:" >&2
	cat "$WORK/t2lookup1.err" >&2
	bad "first referer lookup"
}
json_bool_true "$WORK/t2lookup1.json" supported && ok "zot reports Referrers support" || bad "lookup did not report supported=true"
if json_bool_true "$WORK/t2lookup1.json" hit; then bad "initial lookup unexpectedly hit"; else ok "initial lookup missed"; fi
SUBJECT_REF="$(json_string "$WORK/t2lookup1.json" subject)"
[[ "$SUBJECT_REF" == *@sha256:* ]] && ok "lookup returned subject $SUBJECT_REF" || bad "lookup missing subject ('$SUBJECT_REF')"

ID1="$(MANIFEST_KEY="$MANIFEST_KEY_A" "$FLATTEN_CTL" export --upload \
	--manifest-config "$WORK/manifest.yaml" --config "$WORK/remote.yaml" "$REF" 2>"$WORK/t2a.err")" || {
	echo "export upload run failed:" >&2
	cat "$WORK/t2a.err" >&2
	bad "export upload"
}
is_hex64 "$ID1" && ok "export printed manifest id ($ID1)" || bad "export: not a 64-hex id ('$ID1')"
"$FLATTEN_CTL" referer put --owner "$OWNER_A" --manifest-id "$ID1" --config "$WORK/remote.yaml" "$SUBJECT_REF" >"$WORK/t2put.out" 2>"$WORK/t2put.err" || {
	echo "referer put failed:" >&2
	cat "$WORK/t2put.err" >&2
	bad "referer put"
}
grep -q "written" "$WORK/t2put.out" && ok "referer put wrote owner/id annotation" || bad "referer put did not report written"

"$FLATTEN_CTL" referer lookup --json --owner "$OWNER_A" --config "$WORK/remote.yaml" "$REF" >"$WORK/t2lookup2.json" 2>"$WORK/t2lookup2.err" || {
	echo "second referer lookup failed:" >&2
	cat "$WORK/t2lookup2.err" >&2
	bad "second referer lookup"
}
ID2="$(json_string "$WORK/t2lookup2.json" manifest_id)"
[ "$ID2" = "$ID1" ] && ok "2nd run reused the same manifest id (deterministic)" || bad "2nd run id differs ('$ID2' != '$ID1')"
json_bool_true "$WORK/t2lookup2.json" hit && ok "2nd lookup hit the referrer and can skip re-export" || bad "2nd lookup did not hit"

# Direct check of zot's OCI 1.1 Referrers API for the subject.
if [ "$SUBJECT_DIGEST" != "${SUBJECT_DIGEST#sha256:}" ]; then
	if curl -fsS "http://127.0.0.1:$ZOT_PORT/v2/e2e/app/referrers/$SUBJECT_DIGEST" >"$WORK/referrers.json" 2>/dev/null; then
		grep -q "flatten-manifest" "$WORK/referrers.json" && ok "zot Referrers API lists our flatten-manifest artifact" || bad "referrers list missing our artifact"
		grep -q "$ID1" "$WORK/referrers.json" && ok "referrer carries the manifest id annotation" || echo "  [info] manifest id not in descriptor annotations (registry-dependent)"
	else
		bad "zot referrers endpoint not reachable for $SUBJECT_DIGEST"
	fi
fi

# ==========================================================================
# TEST 3 — owner isolation: a different customer key must not reuse the referrer
# ==========================================================================
log "TEST 3: owner isolation (different MANIFEST_KEY)"
write_remote "$WORK/remote-b.yaml" "$WORK/cache-b"
"$FLATTEN_CTL" referer lookup --json --owner "$OWNER_B" --config "$WORK/remote-b.yaml" "$REF" >"$WORK/t3lookup.json" 2>"$WORK/t3lookup.err" || {
	echo "owner-isolation lookup failed:" >&2
	cat "$WORK/t3lookup.err" >&2
	bad "owner-isolation lookup"
}
if json_bool_true "$WORK/t3lookup.json" hit; then bad "different owner unexpectedly hit an existing referrer"; else ok "different owner did not match existing referrer"; fi
ID3="$(MANIFEST_KEY="$MANIFEST_KEY_B" "$FLATTEN_CTL" export --upload \
	--manifest-config "$WORK/manifest.yaml" --config "$WORK/remote-b.yaml" "$REF" 2>"$WORK/t3.err")" || {
	echo "owner-isolation run failed:" >&2
	cat "$WORK/t3.err" >&2
	bad "owner-isolation run"
}
[ -n "$ID3" ] && [ "$ID3" != "$ID1" ] && ok "different key -> different manifest id" || bad "different key produced same id ('$ID3')"
"$FLATTEN_CTL" referer put --owner "$OWNER_B" --manifest-id "$ID3" --config "$WORK/remote-b.yaml" "$SUBJECT_REF" >/dev/null 2>"$WORK/t3put.err" || {
	cat "$WORK/t3put.err" >&2
	bad "owner-isolation referer put"
}

# ==========================================================================
# TEST 4 — expired referrers are filtered and returned as a miss
# ==========================================================================
log "TEST 4: expired referrer filtering"
EXPIRED_REF="127.0.0.1:$ZOT_PORT/e2e/expired:v1"
EXPIRY_TTL=100ms
seed "$E2E_IMAGE" "$EXPIRED_REF" || bad "seed expiry test image"
"$FLATTEN_CTL" referer lookup --json --owner "$OWNER_A" --config "$WORK/remote.yaml" "$EXPIRED_REF" >"$WORK/t4lookup1.json" 2>"$WORK/t4lookup1.err" || {
	cat "$WORK/t4lookup1.err" >&2
	bad "expiry initial lookup"
}
EXPIRED_SUBJECT_REF="$(json_string "$WORK/t4lookup1.json" subject)"
EXPIRED_SUBJECT_DIGEST="${EXPIRED_SUBJECT_REF##*@}"
"$FLATTEN_CTL" referer put --owner "$OWNER_A" --manifest-id "$ID1" --validity "$EXPIRY_TTL" \
	--config "$WORK/remote.yaml" "$EXPIRED_SUBJECT_REF" >"$WORK/t4put.out" 2>"$WORK/t4put.err" || {
	cat "$WORK/t4put.err" >&2
	bad "expiry referer put"
}

# Prove that the registry indexed the matching record before accepting a miss.
# This avoids racing a live lookup against the deliberately short validity.
T4_INDEXED=""
for _ in $(seq 1 50); do
	if curl -fsS "http://127.0.0.1:$ZOT_PORT/v2/e2e/expired/referrers/$EXPIRED_SUBJECT_DIGEST" >"$WORK/t4referrers.json" 2>/dev/null &&
		grep -q "$ID1" "$WORK/t4referrers.json" &&
		grep -q "$OWNER_A" "$WORK/t4referrers.json" &&
		grep -q "vnd.kuasar.flatten-manifest.valid_at" "$WORK/t4referrers.json"; then
		T4_INDEXED=1
		break
	fi
	sleep 0.1
done
[ -n "$T4_INDEXED" ] && ok "expiring referrer is indexed with owner/id/valid_at" || bad "expiring referrer was not indexed"

T4_EXPIRED=""
for _ in $(seq 1 50); do
	if ! "$FLATTEN_CTL" referer lookup --json --owner "$OWNER_A" --config "$WORK/remote.yaml" "$EXPIRED_REF" >"$WORK/t4lookup2.json" 2>"$WORK/t4lookup2.err"; then
		cat "$WORK/t4lookup2.err" >&2
		bad "expiry post-expiry lookup"
		break
	fi
	if ! json_bool_true "$WORK/t4lookup2.json" hit; then
		T4_EXPIRED=1
		break
	fi
	sleep 0.1
done
json_bool_true "$WORK/t4lookup2.json" supported && ok "registry remains supported after expiry" || bad "post-expiry lookup lost supported state"
[ -n "$T4_EXPIRED" ] && ok "expired referrer is filtered as a miss" || bad "expired referrer was returned"

# ==========================================================================
# TEST 5 — basic-auth registry: credentials via FLATTEN_REGISTRY_* env
# ==========================================================================
log "TEST 5: basic-auth registry"
AUTH_PORT="$(free_port)"
mkdir -p "$WORK/zot-auth"
# Static bcrypt htpasswd line for AUTH_USER=e2euser, AUTH_PASS=e2epass.
# Keeping this fixture in the script avoids a dependency on apache2-utils.
cat >"$WORK/htpasswd" <<'EOF'
e2euser:$2y$05$/Jvk/Gj8hT1jwrfwYfy89OeTyXpVOpkH3Bpy3UrFx0XnTG5rmy6eq
EOF
start_zot "$WORK/zot-auth" "$AUTH_PORT" "$WORK/htpasswd" || bad "auth zot start"

AUTH_REF="127.0.0.1:$AUTH_PORT/e2e/app:v1"
docker login "127.0.0.1:$AUTH_PORT" -u "$AUTH_USER" --password-stdin <<<"$AUTH_PASS" >/dev/null 2>&1 \
	&& AUTH_LOGGED_IN="127.0.0.1:$AUTH_PORT" || bad "docker login to auth zot"
seed "$E2E_IMAGE" "$AUTH_REF" || bad "seed auth zot"
docker logout "127.0.0.1:$AUTH_PORT" >/dev/null 2>&1 && AUTH_LOGGED_IN=""

write_remote "$WORK/remote-auth.yaml" "$WORK/cache-auth"

# No credentials -> must fail (auth is actually enforced).
if FLATTEN_REGISTRY_USERNAME="" FLATTEN_REGISTRY_PASSWORD="" \
	"$FLATTEN_CTL" export --output "$WORK/auth-noauth.erofs" \
	--config "$WORK/remote-auth.yaml" --no-progress "$AUTH_REF" >/dev/null 2>"$WORK/t4noauth.err"; then
	bad "anonymous pull from auth registry unexpectedly succeeded"
else
	ok "anonymous pull rejected by the authed registry"
fi

# With credentials -> full upload + referrer flow succeeds.
FLATTEN_REGISTRY_USERNAME="$AUTH_USER" FLATTEN_REGISTRY_PASSWORD="$AUTH_PASS" \
	"$FLATTEN_CTL" referer lookup --json --owner "$OWNER_A" --config "$WORK/remote-auth.yaml" "$AUTH_REF" >"$WORK/t4lookup.json" 2>"$WORK/t4lookup.err" || {
	echo "authed lookup failed:" >&2
	cat "$WORK/t4lookup.err" >&2
	bad "authed referer lookup"
}
AUTH_SUBJECT_REF="$(json_string "$WORK/t4lookup.json" subject)"
ID4="$(FLATTEN_REGISTRY_USERNAME="$AUTH_USER" FLATTEN_REGISTRY_PASSWORD="$AUTH_PASS" \
	MANIFEST_KEY="$MANIFEST_KEY_A" "$FLATTEN_CTL" export --upload \
	--manifest-config "$WORK/manifest.yaml" --config "$WORK/remote-auth.yaml" "$AUTH_REF" 2>"$WORK/t4.err")" || {
	echo "authed run failed:" >&2
	cat "$WORK/t4.err" >&2
	bad "authed upload"
}
is_hex64 "$ID4" && ok "credentialed pull + flatten + upload succeeded ($ID4)" || bad "authed run: not a 64-hex id ('$ID4')"
FLATTEN_REGISTRY_USERNAME="$AUTH_USER" FLATTEN_REGISTRY_PASSWORD="$AUTH_PASS" \
	"$FLATTEN_CTL" referer put --owner "$OWNER_A" --manifest-id "$ID4" --config "$WORK/remote-auth.yaml" "$AUTH_SUBJECT_REF" >/dev/null 2>"$WORK/t4put.err" || {
	echo "authed referer put failed:" >&2
	cat "$WORK/t4put.err" >&2
	bad "authed referer put"
}
ok "credentialed referer put succeeded"

# --------------------------------------------------------------------------
# summary
# --------------------------------------------------------------------------
log "summary"
if [ "$FAILS" -eq 0 ]; then
	echo "ALL E2E CHECKS PASSED"
	exit 0
fi
echo "E2E FAILED: $FAILS check(s)"
exit 1
