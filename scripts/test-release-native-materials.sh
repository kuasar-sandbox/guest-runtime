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

# Keep fixture package ownership/digests separate from mutable input bytes.
package_input="$test_root/package-input.a"
printf 'original package payload\n' > "$package_input"
deb_digest="$(md5sum "$package_input" | awk '{print $1}')"
rpm_digest="$(sha256sum "$package_input" | awk '{print $1}')"
package_mutation=none
dpkg-query() {
  case "$1" in
    -S) printf 'fixture:amd64: %s\n' "$package_input"; return ;;
    --control-show) [ "$2" = fixture:amd64 ] && [ "$3" = md5sums ] || return 1 ;;
    *) return 1 ;;
  esac
  case "$package_mutation" in
    unavailable) return 1 ;;
    missing) printf '%s  other-file\n' "$deb_digest" ;;
    duplicate) printf '%s  %s\n' "$deb_digest" "${package_input#/}" "$deb_digest" "${package_input#/}" ;;
    *) printf '%s  %s\n' "$deb_digest" "${package_input#/}" ;;
  esac
}
rpm() {
  [ "$1" = -qf ] && [ "$2" = --dump ] && [ "$3" = "$package_input" ] || return 1
  case "$package_mutation" in
    unavailable) return 1 ;;
    missing) printf '/other-file 1 0 %s 0100644 root root 0 0 0 X\n' "$rpm_digest" ;;
    duplicate) printf '%s 1 0 %s 0100644 root root 0 0 0 X\n' "$package_input" "$rpm_digest" "$package_input" "$rpm_digest" ;;
    *) printf '%s 1 0 %s 0100644 root root 0 0 0 X\n' "$package_input" "$rpm_digest" ;;
  esac
}
for package_format in deb rpm; do
  release_native_verify_package_file "$package_input" "$package_format" fixture:amd64
  for package_mutation in missing duplicate unavailable; do
    if (release_native_verify_package_file "$package_input" "$package_format" fixture:amd64 \
        > "$test_root/package-rejection.log" 2>&1); then
      fail "accepted $package_format $package_mutation package metadata"
    fi
    grep -Eq 'file digest|file digests' "$test_root/package-rejection.log" \
      || fail "package metadata was rejected for an unrelated reason"
  done
  package_mutation=none
done
printf 'locally replaced package payload\n' > "$package_input"
for package_format in deb rpm; do
  if (release_native_verify_package_file "$package_input" "$package_format" fixture:amd64 \
      > "$test_root/package-rejection.log" 2>&1); then
    fail "accepted altered $package_format package payload"
  fi
  grep -Fq 'content differs from installed metadata' "$test_root/package-rejection.log" \
    || fail "altered package payload was rejected for an unrelated reason"
done
# The production collector must verify bytes before relying on source/license
# ownership. This still-owned Debian input has been replaced since installation.
if (release_native_system_input "$package_input" bin/fixture \
    > "$test_root/package-rejection.log" 2>&1); then
  fail "native material collection accepted a replaced Debian input"
fi
grep -Fq 'content differs from installed metadata' "$test_root/package-rejection.log" \
  || fail "material collection did not verify the installed input bytes"
unset -f dpkg-query rpm
printf 'test-native-materials: installed package byte verification PASS\n'


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
