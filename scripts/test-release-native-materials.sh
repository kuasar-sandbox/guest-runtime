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

# C5 was withdrawn: ordinary complete inventories have no added row/byte quota.
# Keep the existing structural, permission and source-table consistency checks.
(
WORK="$test_root/inventory-work"
inventory_root="$test_root/inventory"
source_root="$inventory_root/share/sources/runtime"
mkdir -p "$WORK" "$source_root"
awk 'BEGIN {
  print "input\tsha256"
  for (i=0; i<4096; i++) {
    name=sprintf("lib%0200d.a", i)
    printf "%s\t%064d\n", name, i
  }
}' > "$source_root/EROFS-INPUTS.tsv"
chmod 0644 "$source_root/EROFS-INPUTS.tsv"
awk -F '\t' 'NR > 1 {
  printf "bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs\tsystem:%s\tfixture\tfixture\tsha256:%s\tshare/licenses/runtime/system/%s\n", $1, $2, $1
}' "$source_root/EROFS-INPUTS.tsv" > "$source_root/SOURCES.tsv"
[ "$(stat -c %s "$source_root/EROFS-INPUTS.tsv")" -gt 1048576 ]
release_native_validate_erofs_inventory "$inventory_root"
for mutation in duplicate malformed mismatch mode; do
  candidate="$test_root/inventory-$mutation"
  cp -a "$inventory_root" "$candidate"
  case "$mutation" in
    duplicate) sed -n '2p' "$source_root/EROFS-INPUTS.tsv" >> "$candidate/share/sources/runtime/EROFS-INPUTS.tsv" ;;
    malformed) sed -i '2s/[0-9]$/x/' "$candidate/share/sources/runtime/EROFS-INPUTS.tsv" ;;
    mismatch) sed -i '1d' "$candidate/share/sources/runtime/SOURCES.tsv" ;;
    mode) chmod 0600 "$candidate/share/sources/runtime/EROFS-INPUTS.tsv" ;;
  esac
  if (release_native_validate_erofs_inventory "$candidate" > "$test_root/inventory-$mutation.log" 2>&1); then
    fail "complete native inventory accepted $mutation"
  fi
done
printf 'test-native-materials: no withdrawn inventory quota; structure and consistency retained PASS\n'
)

# Legacy cache material can retain a linker map and notices without the EROFS
# objects. External archives remain mandatory and fully inventoried.
(
  release_materials_init "$test_root/legacy-stage" "$test_root/legacy-work" runtime
  mkdir -p "$test_root/legacy-source/mkfs" "$test_root/legacy-inputs"
  for name in libc.a libuuid.a; do printf 'fixture archive\n' > "$test_root/legacy-inputs/$name"; done
  {
    printf 'LOAD mkfs_erofs-main.o\nLOAD ../lib/.libs/liberofs.a\n'
    printf 'LOAD %s\n' "$test_root/legacy-inputs/libc.a" "$test_root/legacy-inputs/libuuid.a"
  } > "$test_root/legacy-source/mkfs/mkfs.erofs.map"
  release_native_system_input() {
    release_materials_record_source "$2" "system:${1##*/}" fixture fixture \
      "sha256:$(sha256sum "$1" | awk '{print $1}')" "system/${1##*/}"
  }
  release_native_erofs_inputs "$test_root/legacy-source/mkfs/mkfs.erofs.map" \
    "$test_root/legacy-source" bin/mkfs.erofs
  [ "$(wc -l < "$RELEASE_MATERIALS_STAGE/share/sources/runtime/EROFS-INPUTS.tsv")" -eq 3 ]
  rm "$test_root/legacy-inputs/libuuid.a"
  if (release_native_erofs_inputs "$test_root/legacy-source/mkfs/mkfs.erofs.map" \
      "$test_root/legacy-source" bin/mkfs.erofs > "$test_root/legacy-missing.log" 2>&1); then
    fail 'legacy map accepted a missing external archive'
  fi
  grep -Fq 'linker map input no longer exists:' "$test_root/legacy-missing.log"
  echo 'test-native-materials: legacy missing intermediates accepted; missing external input rejected PASS'
)

# Validate trusted-source structure as well as archive/source byte equality.
(
  source_root="$test_root/patch-source"
  mkdir -p "$source_root/native-deps/deps"
  cp -a "$ROOT/native-deps/deps/erofs-patches" "$source_root/native-deps/deps/"
  git -C "$source_root" init -q
  git -C "$source_root" add .
  git -C "$source_root" -c user.name=Fixture -c user.email=fixture@example.invalid commit -qm fixture
  ROOT="$source_root" WORK="$test_root/patch-work"
  mkdir "$WORK"
  selected="$(git -C "$ROOT" rev-parse HEAD)"
  directory="$ROOT/native-deps/deps/erofs-patches"
  release_native_validate_erofs_patches "$directory" "$selected"
  # Worktree edits never become the reference, even if the index hides them.
  git -C "$ROOT" update-index --assume-unchanged native-deps/deps/erofs-patches/series
  printf '# altered worktree\n' >> "$directory/series"
  if (release_native_validate_erofs_patches "$directory" "$selected" > "$WORK/altered.log" 2>&1); then
    fail 'patch validation trusted edited worktree bytes'
  fi
  grep -Fq 'EROFS patch material differs from selected source:' "$WORK/altered.log"
  git -C "$ROOT" update-index --no-assume-unchanged native-deps/deps/erofs-patches/series
  git -C "$ROOT" show "$selected:native-deps/deps/erofs-patches/series" > "$directory/series"
  for mutation in duplicate traversal unlisted sidecar hidden nested symlink; do
    fixture="$test_root/patch-source-$mutation"
    cp -a "$ROOT" "$fixture"
    material="$fixture/native-deps/deps/erofs-patches"
    case "$mutation" in
      duplicate) cat "$directory/series" >> "$material/series" ;;
      traversal) printf '../outside.patch\n' > "$material/series" ;;
      unlisted) printf 'unlisted\n' > "$material/unlisted.patch" ;;
      sidecar) rm "$material/0002-explicit-libgcrypt-sha256.patch.license" ;;
      hidden) printf 'hidden\n' > "$material/.hidden" ;;
      nested) mkdir "$material/nested"; printf 'nested\n' > "$material/nested/file" ;;
      symlink) ln -s README.md "$material/LINK" ;;
    esac
    git -C "$fixture" add -A
    git -C "$fixture" -c user.name=Fixture -c user.email=fixture@example.invalid commit -qm "$mutation"
    sha="$(git -C "$fixture" rev-parse HEAD)"
    if (ROOT="$fixture" release_native_validate_erofs_patches "$material" "$sha" > "$WORK/$mutation.log" 2>&1); then
      fail "selected patch source accepted $mutation"
    fi
  done
  if (release_native_validate_erofs_patches "$directory" 0000000000000000000000000000000000000000 > "$WORK/absent.log" 2>&1); then
    fail 'unavailable selected source was accepted'
  fi
  grep -Fq 'selected source commit is unavailable' "$WORK/absent.log"
  echo 'test-native-materials: selected Git bytes, safe patch structure and unavailable-source rejection PASS'
)

(
root="$test_root/gcrypt-inventory"
mkdir -p "$root/lib" "$root/notices" "$root/erofs"
for input in libc.a libuuid.a libgcrypt.a libgpg-error.a crtbeginT.o; do
  printf 'fixture %s\n' "$input" > "$root/lib/$input"
  printf 'LOAD %s/lib/%s\n' "$root" "$input"
done > "$root/erofs/mkfs.map"
printf 'fixture Libgcrypt/Libgpg-error copyright and license\n' > "$root/notices/LICENSE"
dpkg-query() { return 1; }
rpm() {
  case "$1" in
    -qf) printf 'fixture-devel\t1.10-test\tfixture-source-1.10-test.src.rpm\n' ;;
    -qa) printf 'fixture-devel.x86_64\tfixture-source-1.10-test.src.rpm\n' ;;
    -ql) printf '%s/notices/LICENSE\n' "$root" ;;
    *) return 1 ;;
  esac
}
release_materials_init "$root/stage" "$root/work" runtime
payload=bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs
release_native_erofs_inputs "$root/erofs/mkfs.map" "$root/erofs" "$payload"
for input in libgcrypt.a libgpg-error.a; do
  cmp "$root/notices/LICENSE" "$root/stage/share/licenses/runtime/system/$input/${root#/}/notices/LICENSE"
  awk -F '\t' -v name="system:$input" '$2 == name && $4 == "rpm-source:fixture-source-1.10-test.src.rpm" {found=1} END {exit !found}' \
    "$RELEASE_MATERIALS_WORK/sources" || fail "missing Libgcrypt/Libgpg-error source record: $input"
  grep -q "^$input" "$root/stage/share/sources/runtime/EROFS-INPUTS.tsv" || fail "Libgcrypt/Libgpg-error archive omitted from inventory"
done
rm "$root/notices/LICENSE"
if (release_native_erofs_inputs "$root/erofs/mkfs.map" "$root/erofs" "$payload" > "$root/missing.log" 2>&1); then
  fail 'accepted missing linked-library license text'
fi
grep -qF 'native license material is missing' "$root/missing.log" || fail 'missing notice failed for unrelated reason'
printf 'test-native-materials: Libgcrypt/Libgpg-error link inputs, identities and notice texts PASS\n'
)

# The installed Ubuntu package route remains independent of source-built catalogs.
(
  root="$test_root/ubuntu-packages"
  mkdir -p "$root/lib"
  for name in libgcrypt.a libgpg-error.a; do printf 'fixture archive\n' > "$root/lib/$name"; done
  dpkg-query() {
    case "$1" in
      -S) printf 'fixture-dev:amd64: %s\n' "$2" ;;
      -W) printf 'fixture-source\t1.2-3ubuntu1\n' ;;
      *) return 1 ;;
    esac
  }
  rpm() { fail 'Ubuntu package inputs reached RPM fallback'; }
  release_native_source_built_crypto() { fail 'Ubuntu package input reached source-built provider'; }
  release_native_copy_file() {
    [ "$1" = /usr/share/doc/fixture-dev/copyright ] || fail 'wrong Ubuntu notice lookup'
    printf '%s\n' "$2" >> "$root/notices-collected"
  }
  grep() { return 1; } # Fixture copyright has no common-license references.
  release_materials_init "$root/stage" "$root/work" runtime
  for name in libgcrypt.a libgpg-error.a; do
    release_native_system_input "$root/lib/$name" bin/mkfs.erofs
    awk -F '\t' -v name="system:$name" '$2 == name && $3 == "1.2-3ubuntu1" &&
      $4 == "deb-source:fixture-source@1.2-3ubuntu1" && $5 ~ /;package:fixture-source$/ {found=1}
      END {exit !found}' "$RELEASE_MATERIALS_WORK/sources" || fail 'Ubuntu package identity changed'
  done
  [ "$(wc -l < "$root/notices-collected")" -eq 2 ]
  [ ! -e "$root/stage/share/sources/runtime/native-crypto" ]
  printf 'test-native-materials: package-only Ubuntu crypto collection PASS\n'
)
