#!/usr/bin/env bash
# Unit coverage for recording payload compiler versions and installed notices.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'test-release-go-contexts: %s\n' "$*" >&2; exit 1; }
# shellcheck source=scripts/release-materials.sh
source "$ROOT/scripts/release-materials.sh"

# Resolve compiler distributions from the actual release binaries. Component
# source directories can select a different Go patch release when an already
# published dependency was built independently.
mkdir -p "$TMP/bin" "$TMP/fake-tools"
printf 'fixture\n' > "$TMP/bin/one"
printf 'fixture\n' > "$TMP/bin/two"
cat > "$TMP/fake-tools/go" <<'EOF'
#!/bin/sh
if [ "$1" = version ] && [ "$2" = -m ]; then
  case "$3" in
    */one) printf '%s: go1.24.0\n' "$3" ;;
    */two) printf '%s: go1.25.0 X:fixture\n' "$3" ;;
    *) exit 71 ;;
  esac
elif [ "$1" = env ] && [ "$2" = -json ]; then
  case "${GOTOOLCHAIN-}" in
    go1.24.0) version=go1.24.0; root="$FIXTURE_ROOT/one" ;;
    go1.25.0) version=go1.25.0; root="$FIXTURE_ROOT/two" ;;
    *) exit 72 ;;
  esac
  printf '{"GOROOT":"%s","GOVERSION":"%s","GOHOSTOS":"linux","GOHOSTARCH":"amd64"}\n' \
    "$root" "$version"
else
  exit 73
fi
EOF
chmod +x "$TMP/fake-tools/go"
release_materials_init "$TMP/record/stage" "$TMP/record/work" runtime
PATH="$TMP/fake-tools:$PATH" FIXTURE_ROOT="$TMP/roots" \
  release_materials_record_go_contexts "$TMP/record/contexts.json" "$TMP/bin/one" "$TMP/bin/two"
[ "$(jq -r 'map(.GOVERSION) | sort | join(" ")' "$TMP/record/contexts.json")" = 'go1.24.0 go1.25.0' ] \
  || fail "actual payload compiler contexts were not recorded"

# Real local notice trees; no compiler download or authentication fixture.
for selected in one two; do
  mkdir -p "$TMP/roots/$selected/src/vendor/example"
  printf 'fixture Go license\n' > "$TMP/roots/$selected/LICENSE"
  printf 'fixture vendor notice\n' > "$TMP/roots/$selected/src/vendor/example/NOTICE"
done

for scenario in object array duplicate-roots two-roots two-versions missing-version missing-root missing-notices; do
  release_materials_init "$TMP/$scenario/stage" "$TMP/$scenario/work" runtime
  RELEASE_MATERIALS_GO_ENV="$TMP/$scenario/contexts.json"
  printf 'go1.24.0\n' > "$RELEASE_MATERIALS_WORK/go-toolchains"
  printf 'bin/fixture\ttoolchain\tgo\tgo1.24.0\t-\n' > "$RELEASE_MATERIALS_WORK/go-build-info"
  expected=1
  case "$scenario" in
    object) contexts='{"GOVERSION":"go1.24.0","GOROOT":"/fixture/one"}' ;;
    array) contexts='[{"GOVERSION":"go1.24.0","GOROOT":"/fixture/one"}]' ;;
    duplicate-roots)
      contexts='[{"GOVERSION":"go1.24.0","GOROOT":"/fixture/one"},{"GOVERSION":"go1.24.0","GOROOT":"/fixture/one"}]' ;;
    two-roots)
      contexts='[{"GOVERSION":"go1.24.0","GOROOT":"/fixture/one"},{"GOVERSION":"go1.24.0","GOROOT":"/fixture/two"}]'
      expected=1 ;;
    two-versions)
      contexts='[{"GOVERSION":"go1.24.0","GOROOT":"/fixture/one"},{"GOVERSION":"go1.25.0","GOROOT":"/fixture/two"}]'
      printf 'go1.25.0\n' >> "$RELEASE_MATERIALS_WORK/go-toolchains"
      printf 'bin/other\ttoolchain\tgo\tgo1.25.0\t-\n' >> "$RELEASE_MATERIALS_WORK/go-build-info"
      expected=2 ;;
    missing-version) contexts='[{"GOVERSION":"go1.25.0","GOROOT":"/fixture/one"}]' ;;
    missing-root) contexts='[{"GOVERSION":"go1.24.0"}]' ;;
    missing-notices)
      contexts='[{"GOVERSION":"go1.24.0","GOROOT":"/fixture/one"},{"GOVERSION":"go1.24.0","GOROOT":"/fixture/missing-notices"}]' ;;
  esac
  contexts="${contexts//\/fixture\//$TMP/roots/}"
  printf '%s\n' "$contexts" > "$RELEASE_MATERIALS_GO_ENV"
  case "$scenario" in
    missing-version|missing-root|missing-notices)
      if (release_materials_finish > "$TMP/$scenario.log" 2>&1); then
        fail "accepted $scenario"
      fi
      grep -Eq 'no recorded build compiler context|selected Go distribution has no LICENSE' "$TMP/$scenario.log" \
        || fail "$scenario failed for an unrelated reason" ;;
    *)
      release_materials_finish
      [ "$(awk -F '\t' '$2 == "Go toolchain" { n++ } END { print n }' "$TMP/$scenario/stage/share/sources/runtime/SOURCES.tsv")" -eq "$expected" ] \
        || fail "$scenario recorded the wrong number of compiler versions"
      license_root="$TMP/$scenario/stage/share/licenses/runtime/go-toolchain"
      cmp -s "$TMP/roots/one/LICENSE" "$license_root/go1.24.0/LICENSE" || fail "selected Go license missing"
      cmp -s "$TMP/roots/one/src/vendor/example/NOTICE" "$license_root/go1.24.0/src/vendor/example/NOTICE" || fail "nested Go notice missing"
      if [ "$scenario" = two-versions ]; then
        cmp -s "$TMP/roots/two/LICENSE" "$license_root/go1.25.0/LICENSE" || fail "second compiler's license missing"
      fi ;;
  esac
done
printf 'test-release-go-contexts: eight compiler-context selection cases PASS\n'
