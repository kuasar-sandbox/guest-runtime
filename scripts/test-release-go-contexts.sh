#!/usr/bin/env bash
# Unit coverage for matching payload compiler versions to every observed root.
# Archive authentication and real compiler builds are covered separately.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'test-release-go-contexts: %s\n' "$*" >&2; exit 1; }
# shellcheck source=scripts/release-materials.sh
source "$ROOT/scripts/release-materials.sh"

# Replace only the expensive authenticated-archive operation in this unit.
# The actual finish function must still select and check every recorded root.
release_materials_go_toolchain_materials() {
  printf '%s\t%s\n' "$1" "$2" >> "$TMP/calls"
  [ "$2" != /fixture/changed-root ] || return 1
  mkdir -p "$3"
  printf 'unit fixture notice\n' > "$3/LICENSE"
  printf 'https://example.invalid/%s.zip\th1:unit-fixture\n' "$1"
}

for scenario in object array duplicate-roots two-roots two-versions missing-version missing-root changed-root; do
  release_materials_init "$TMP/$scenario/stage" "$TMP/$scenario/work" runtime
  RELEASE_MATERIALS_GO_ENV="$TMP/$scenario/contexts.json"
  printf 'go1.24.0\n' > "$RELEASE_MATERIALS_WORK/go-toolchains"
  printf 'bin/fixture\ttoolchain\tgo\tgo1.24.0\t-\n' > "$RELEASE_MATERIALS_WORK/go-build-info"
  : > "$TMP/calls"
  expected=1
  case "$scenario" in
    object) contexts='{"GOVERSION":"go1.24.0","GOROOT":"/fixture/one"}' ;;
    array) contexts='[{"GOVERSION":"go1.24.0","GOROOT":"/fixture/one"}]' ;;
    duplicate-roots)
      contexts='[{"GOVERSION":"go1.24.0","GOROOT":"/fixture/one"},{"GOVERSION":"go1.24.0","GOROOT":"/fixture/one"}]' ;;
    two-roots)
      contexts='[{"GOVERSION":"go1.24.0","GOROOT":"/fixture/one"},{"GOVERSION":"go1.24.0","GOROOT":"/fixture/two"}]'
      expected=2 ;;
    two-versions)
      contexts='[{"GOVERSION":"go1.24.0","GOROOT":"/fixture/one"},{"GOVERSION":"go1.25.0","GOROOT":"/fixture/two"}]'
      printf 'go1.25.0\n' >> "$RELEASE_MATERIALS_WORK/go-toolchains"
      printf 'bin/other\ttoolchain\tgo\tgo1.25.0\t-\n' >> "$RELEASE_MATERIALS_WORK/go-build-info"
      expected=2 ;;
    missing-version) contexts='[{"GOVERSION":"go1.25.0","GOROOT":"/fixture/one"}]' ;;
    missing-root) contexts='[{"GOVERSION":"go1.24.0"}]' ;;
    changed-root)
      contexts='[{"GOVERSION":"go1.24.0","GOROOT":"/fixture/one"},{"GOVERSION":"go1.24.0","GOROOT":"/fixture/changed-root"}]' ;;
  esac
  printf '%s\n' "$contexts" > "$RELEASE_MATERIALS_GO_ENV"
  case "$scenario" in
    missing-version|missing-root|changed-root)
      if (release_materials_finish > "$TMP/$scenario.log" 2>&1); then
        fail "accepted $scenario"
      fi
      grep -Eq 'no recorded build compiler context|material authentication failed' "$TMP/$scenario.log" \
        || fail "$scenario failed for an unrelated reason" ;;
    *)
      release_materials_finish
      [ "$(wc -l < "$TMP/calls")" -eq "$expected" ] || fail "$scenario did not check every unique compiler root"
      grep -Fqx $'go1.24.0\t/fixture/one' "$TMP/calls" || fail "$scenario selected the wrong compiler root"
      if [ "$scenario" = two-versions ]; then
        grep -Fqx $'go1.25.0\t/fixture/two' "$TMP/calls" || fail "second compiler version was not checked"
      fi ;;
  esac
done
printf 'test-release-go-contexts: eight compiler-context selection cases PASS\n'
