#!/usr/bin/env bash
# Exercise the actual packager's owned-directory trap with Go-style read-only caches.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d)"
trap 'chmod -R u+w "$test_root"; rm -rf "$test_root"' EXIT
fail() { printf 'test-release-cleanup: %s\n' "$*" >&2; exit 1; }
printf 'unrelated fixture\n' > "$test_root/unrelated"
for expected in 0 42; do
  actual=0
  TMPDIR="$test_root" bash -c '
    set -euo pipefail
    source <(sed -n "/^WORK=/p; /^trap /p" "$1")
    printf "%s\n" "$WORK"
    mkdir -p "$WORK/go-mod/module"
    printf "owned fixture\n" > "$WORK/go-mod/module/LICENSE"
    chmod 0444 "$WORK/go-mod/module/LICENSE"
    chmod 0500 "$WORK/go-mod/module"
    exit "$2"
  ' _ "$ROOT/scripts/release.sh" "$expected" > "$test_root/work-$expected" || actual=$?
  [ "$actual" -eq "$expected" ] || fail "cleanup changed exit status $expected to $actual"
  owned="$(<"$test_root/work-$expected")"
  [[ "$owned" == "$test_root/"?* ]] || fail "unexpected work directory"
  [ ! -e "$owned" ] || fail "read-only module cache survived exit $expected"
  cmp "$test_root/unrelated" <(printf 'unrelated fixture\n') \
    || fail "cleanup changed an unrelated object"
done
printf 'test-release-cleanup: success/failure read-only cache cleanup and unrelated preservation PASS\n'
