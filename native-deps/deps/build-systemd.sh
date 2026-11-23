#!/usr/bin/env bash
# Build the matching native systemd/D-Bus installation at its final Guest prefix.
# Both supported architectures build natively; cross assembly can select an
# already verified SYSTEMD_PAYLOAD. This script never installs host packages.
set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
source "$script_dir/common.sh"
repo_root="$(cd "$script_dir/../.." && pwd)"
lock="$script_dir/systemd-lock.json"
: "${TARGET_ARCH:=$(uname -m)}"
case "$TARGET_ARCH" in amd64) TARGET_ARCH=x86_64;; arm64) TARGET_ARCH=aarch64;; esac
case "$TARGET_ARCH" in x86_64|aarch64) ;; *) die "unsupported systemd TARGET_ARCH=$TARGET_ARCH";; esac
[ "$(uname -m)" = "$TARGET_ARCH" ] || die "systemd requires a native $TARGET_ARCH builder; use a verified SYSTEMD_PAYLOAD for cross assembly"
: "${BUILD_DIR:=$(pwd)/build/$TARGET_ARCH}"
: "${BINDIR:=$(pwd)/bin/$TARGET_ARCH}"
: "${KUASAR_BUILD_JOBS:=$(nproc)}"
[[ "$KUASAR_BUILD_JOBS" =~ ^[1-9][0-9]*$ ]] || die "KUASAR_BUILD_JOBS must be a positive integer"
require_cmd python3 meson ninja pkg-config gcc make patch patchelf readelf ldconfig dpkg-query tar sha256sum
python3 -c 'import jinja2' || die "systemd requires Python Jinja2"
pkg-config --exists libcap mount libseccomp blkid libacl liblz4 libzstd liblzma libpcre2-8 openssl expat libsystemd \
  || die "missing systemd development libraries; see native-deps/README.md"
[ -x /bin/busybox ] || die "systemd mount helpers require the native busybox-static package"

mapfile -t inputs < <(python3 - "$lock" <<'PY'
import json, re, sys
p=json.load(open(sys.argv[1]))
for key in ('systemd','dbus'):
    if not re.fullmatch('[0-9a-f]{64}',p[key]['sha256']):
        raise SystemExit('systemd source SHA256 must be pinned')
print(p['prefix'])
for key in ('systemd','dbus'):
    for field in ('version','url','sha256'):
        print(p[key][field])
PY
)
[ "${#inputs[@]}" -eq 7 ] || die "invalid systemd source lock"
prefix="${inputs[0]}"
systemd_version="${inputs[1]}"; systemd_spec="${SYSTEMD_TARBALL:-${inputs[2]}}"; systemd_sha="${inputs[3]}"
dbus_version="${inputs[4]}"; dbus_spec="${DBUS_TARBALL:-${inputs[5]}}"; dbus_sha="${inputs[6]}"

mkdir -p "$BUILD_DIR" "$BINDIR"
build_root="$BUILD_DIR/systemd"
mkdir -p "$build_root"
receipt="$build_root/inputs.json"
python3 "$script_dir/package-systemd.py" inputs --lock "$lock" --output "$receipt"
out="$BINDIR/systemd"
if [ -d "$out" ] && python3 "$repo_root/scripts/release-systemd-payload.py" verify "$out" \
    --arch "$TARGET_ARCH" --lock "$lock" --inputs "$receipt" >/dev/null; then
    log "systemd payload inputs and complete installation verified: $out"
    exit 0
fi

systemd_tarball="$(resolve_tarball "$systemd_spec" "$systemd_sha")"
dbus_tarball="$(resolve_tarball "$dbus_spec" "$dbus_sha")"
# These are disposable build sources, not patch-development worktrees. A
# rebuild always re-extracts verified archives so edited cache contents cannot
# be mislabeled with the locked upstream identity.
systemd_src="$build_root/src/systemd-$systemd_sha"
dbus_src="$build_root/src/dbus-$dbus_sha"
rm -rf "$systemd_src" "$dbus_src"
extract_tarball "$systemd_tarball" "$systemd_src" >/dev/null
extract_tarball "$dbus_tarball" "$dbus_src" >/dev/null
while IFS=$'\t' read -r patch_file patch_sha; do
    printf '%s  %s\n' "$patch_sha" "$script_dir/$patch_file" | sha256sum -c -
    patch --batch --forward -d "$systemd_src" -p1 < "$script_dir/$patch_file"
done < <(python3 - "$lock" <<'PY'
import json, sys
for patch in json.load(open(sys.argv[1]))['systemd']['patches']:
    print(patch['file'] + '\t' + patch['sha256'])
PY
)
[ "$(cat "$systemd_src/meson.version")" = "$systemd_version" ] || die "systemd archive version differs from lock"

systemd_build="$build_root/systemd-build"
dbus_build="$build_root/dbus-build"
stage="$build_root/stage"
# Reconfiguration uses a clean DESTDIR: removed helpers/units must not survive a
# recipe change. Build directories likewise never retain auto-detected features.
rm -rf "$stage" "$systemd_build" "$dbus_build"
mkdir -p "$stage" "$dbus_build"
mapfile -t meson_options < <(python3 - "$lock" <<'PY'
import json,sys
for option in json.load(open(sys.argv[1]))['meson_options']:
    print('-D'+option)
PY
)
log "building systemd $systemd_version for $TARGET_ARCH ($KUASAR_BUILD_JOBS jobs)"
meson setup "$systemd_build" "$systemd_src" --prefix="$prefix" --libdir=lib --libexecdir=lib \
  --sysconfdir=/etc --localstatedir=/var "${meson_options[@]}" \
  "-Dmount-path=$prefix/bin/mount" "-Dumount-path=$prefix/bin/umount" \
  "-Ddbuspolicydir=$prefix/share/dbus-1/system.d" \
  "-Ddbussystemservicedir=$prefix/share/dbus-1/system-services" \
  "-Ddbussessionservicedir=$prefix/share/dbus-1/services" \
  "-Ddbus-interfaces-dir=$prefix/share/dbus-1/interfaces"
ninja -C "$systemd_build" -j "$KUASAR_BUILD_JOBS"
DESTDIR="$stage" meson install -C "$systemd_build" --no-rebuild

log "building D-Bus $dbus_version for $TARGET_ARCH ($KUASAR_BUILD_JOBS jobs)"
(
  cd "$dbus_build"
  "$dbus_src/configure" --prefix="$prefix" --libdir="$prefix/lib" \
    --libexecdir="$prefix/lib/dbus-1.0" --sysconfdir=/etc --localstatedir=/var --runstatedir=/run \
    --with-dbus-user=messagebus --with-system-socket=/run/dbus/system_bus_socket \
    --with-system-pid-file=/run/dbus/pid --with-systemdsystemunitdir="$prefix/lib/systemd/system" \
    --disable-doxygen-docs --disable-xml-docs --disable-tests --disable-selinux \
    --disable-apparmor --disable-libaudit --disable-x11-autolaunch --enable-systemd
  make -j "$KUASAR_BUILD_JOBS"
  make DESTDIR="$stage" install
)

candidate="$(mktemp -d "$BINDIR/.systemd.XXXXXX")"
trap 'rm -rf "$candidate"' EXIT
python3 "$script_dir/package-systemd.py" assemble --stage "$stage" --output "$candidate" \
  --arch "$TARGET_ARCH" --lock "$lock" --inputs "$receipt" \
  --systemd-source "$systemd_src" --dbus-source "$dbus_src"
python3 "$repo_root/scripts/release-systemd-payload.py" verify "$candidate" \
  --arch "$TARGET_ARCH" --lock "$lock" --inputs "$receipt"
rm -rf "$out.previous"
if [ -e "$out" ]; then mv "$out" "$out.previous"; fi
mv "$candidate" "$out"
rm -rf "$out.previous"
trap - EXIT
log "built and verified $out"
