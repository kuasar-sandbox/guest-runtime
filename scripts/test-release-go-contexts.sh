#!/usr/bin/env bash
# Unit coverage for recording payload compiler versions and installed notices.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'test-release-go-contexts: %s\n' "$*" >&2; exit 1; }
# shellcheck source=scripts/release-materials.sh
source "$ROOT/scripts/release-materials.sh"

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
