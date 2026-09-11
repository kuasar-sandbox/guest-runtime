#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf "$test_root"' EXIT
fail() { echo "test-native-materials: $*" >&2; exit 1; }
# shellcheck source=scripts/release-materials.sh
source "$ROOT/scripts/release-materials.sh"
# shellcheck source=scripts/release-native-materials.sh
source "$ROOT/scripts/release-native-materials.sh"


catalog="$test_root/catalog"
library="$test_root/libuuid.a"
mkdir -p "$catalog/licenses/libuuid" "$catalog/licenses/Documentation/licenses"
printf 'fixture util-linux copyright\n' > "$catalog/licenses/COPYING"
printf 'fixture libuuid copyright\n' > "$catalog/licenses/libuuid/COPYING"
printf 'fixture referenced license text\n' > "$catalog/licenses/Documentation/licenses/BSD-3-Clause"
printf 'fixture static library\n' > "$library"
{
  printf 'payload\tname\tversion\tsource\tintegrity\tlicense_directory\n'
  printf 'libuuid.a\tutil-linux\tfixture\thttps://example.invalid/util-linux.src.rpm\tsha256:%s;tarball-sha256:fixture;srpm-sha256:fixture\tlicenses\n' \
    "$(sha256sum "$library" | awk '{print $1}')"
} > "$catalog/SOURCES.tsv"
hash_catalog() {
  (cd "$1" && find licenses SOURCES.tsv -type f -print | LC_ALL=C sort \
    | while IFS= read -r file; do sha256sum "$file"; done) > "$1/MATERIALS.sha256"
}
hash_catalog "$catalog"
release_materials_init "$test_root/stage" "$test_root/work" runtime
release_native_source_built_uuid "$library" bin/mkfs.erofs "$catalog"
cmp "$catalog/licenses/libuuid/COPYING" "$test_root/stage/share/licenses/runtime/system/libuuid.a/libuuid/COPYING"
[ "$(stat -c %a "$test_root/stage/share/licenses/runtime/system/libuuid.a/libuuid/COPYING")" = 644 ]
awk -F '\t' '$2 == "system:libuuid.a" && $4 == "https://example.invalid/util-linux.src.rpm" {found=1} END {exit !found}' \
  "$RELEASE_MATERIALS_WORK/sources"
for mutation in payload license unlisted symlink missing traversal identity bad-header; do
  candidate="$test_root/$mutation"
  cp -a "$catalog" "$candidate"
  cp "$library" "$test_root/$mutation.a"
  case "$mutation" in
    payload) printf 'different payload\n' >> "$test_root/$mutation.a" ;;
    license) printf 'different notice\n' >> "$candidate/licenses/libuuid/COPYING" ;;
    unlisted) printf 'unlisted\n' > "$candidate/licenses/UNLISTED" ;;
    symlink) ln -s "$catalog/licenses/COPYING" "$candidate/licenses/LINK" ;;
    missing) rm "$candidate/licenses/libuuid/COPYING" ;;
    traversal) printf '%064d  ../libuuid.a\n' 0 >> "$candidate/MATERIALS.sha256" ;;
    identity) sed -i '2s/libuuid.a/other.a/' "$candidate/SOURCES.tsv"; hash_catalog "$candidate" ;;
    bad-header) sed -i '1s/payload/other/' "$candidate/SOURCES.tsv"; hash_catalog "$candidate" ;;
  esac
  if (release_native_source_built_uuid "$test_root/$mutation.a" bin/mkfs.erofs "$candidate" >/dev/null 2>&1); then
    fail "source-built native material accepted $mutation"
  fi
done
mkdir "$test_root/erofs"
printf 'link map without system archives\n' > "$test_root/erofs/empty.map"
for map in missing.map empty.map; do
  if (release_native_erofs_inputs "$test_root/erofs/$map" "$test_root/erofs" bin/mkfs.erofs >/dev/null 2>&1); then
    fail "native input collection accepted $map"
  fi
done
printf 'test-native-materials: source identity, inventory and link-map rejection PASS\n'

(
mkdir -p "$test_root/first" "$test_root/second"
printf 'first link input\n' > "$test_root/first/same.a"
printf 'second link input\n' > "$test_root/second/same.a"
printf 'LOAD %s\n' "$test_root/first/same.a" "$test_root/second/same.a" \
  > "$test_root/erofs/collision.map"
release_native_system_input() { printf '%s\n' "$1" >> "$test_root/collision-collected"; }
if (release_native_erofs_inputs "$test_root/erofs/collision.map" "$test_root/erofs" bin/mkfs.erofs \
    > "$test_root/collision.log" 2>&1); then
  fail "accepted colliding native material names"
fi
grep -Fq 'distinct native link inputs share a material name' "$test_root/collision.log" \
  || fail "native collision failed for an unrelated reason"
[ "$(wc -l < "$test_root/collision-collected")" -eq 1 ] \
  || fail "second colliding native input reached the material collector"
printf 'test-native-materials: colliding input rejected before overwriting materials PASS\n'
)
