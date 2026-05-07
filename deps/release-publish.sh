#!/usr/bin/env bash
#
# Create a GitHub release for $VERSION and upload all tarballs found
# under $RELEASE_DIR. Both x86_64 and aarch64 tarballs must be present;
# this script refuses to publish a single-arch release.
#
# Inputs (env):
#   VERSION       Release tag (e.g. v0.1) — must match a git tag that has
#                 been pushed to origin. We do NOT push the tag for the
#                 user; tag-and-push is a separate intentional step.
#   RELEASE_DIR   Directory holding mass-sandbox-$VERSION-linux-*.tar.gz.
#
# Requires `gh` CLI authenticated to the repo's origin. If a release with
# the same tag already exists, assets are uploaded with --clobber so this
# command is rerun-safe.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${VERSION:?VERSION required (e.g. v0.1)}"
: "${RELEASE_DIR:?RELEASE_DIR required}"

require_cmd gh git

# Auth check up front — fail fast with a clear message rather than letting
# `gh release create` emit a generic 401.
if ! gh auth status >/dev/null 2>&1; then
    die "gh not authenticated. Run: gh auth login"
fi

# Tag must exist locally and on origin, otherwise gh release create will
# create a tag pointing at HEAD (often not what you want).
if ! git rev-parse "$VERSION" >/dev/null 2>&1; then
    die "git tag $VERSION not found locally. Create it first:
  git tag $VERSION && git push origin $VERSION"
fi

shopt -s nullglob
tarballs=( "$RELEASE_DIR"/mass-sandbox-"$VERSION"-linux-*.tar.gz )
if [ ${#tarballs[@]} -eq 0 ]; then
    die "no tarballs found at $RELEASE_DIR/mass-sandbox-$VERSION-linux-*.tar.gz
Build them first:
  make TARGET_ARCH=x86_64 release
  make TARGET_ARCH=aarch64 release"
fi

# Refuse single-arch publishes — a public release should ship both.
have_x86=0; have_arm=0
for t in "${tarballs[@]}"; do
    case "$(basename "$t")" in
        *-linux-x86_64.tar.gz)  have_x86=1 ;;
        *-linux-aarch64.tar.gz) have_arm=1 ;;
    esac
done
if [ "$have_x86" -eq 0 ] || [ "$have_arm" -eq 0 ]; then
    die "release must include both architectures. Found: ${tarballs[*]##*/}
Build the missing one:
  make TARGET_ARCH=x86_64 release
  make TARGET_ARCH=aarch64 release"
fi

log "tarballs to publish:"
for t in "${tarballs[@]}"; do
    size="$(du -h "$t" | cut -f1)"
    sha="$(sha256sum "$t" | cut -d' ' -f1)"
    echo "    $(basename "$t") ($size, sha256=$sha)"
done

# Either create a new release or reuse an existing one (asset-only update).
if gh release view "$VERSION" >/dev/null 2>&1; then
    log "release $VERSION exists, uploading assets with --clobber"
    gh release upload "$VERSION" "${tarballs[@]}" --clobber
else
    log "creating release $VERSION"
    notes="Release $VERSION

Architectures: x86_64, aarch64.

See README.md and docs/cross-arch.md inside each tarball for build
prerequisites and end-to-end test instructions."
    gh release create "$VERSION" "${tarballs[@]}" \
        --title "$VERSION" \
        --notes "$notes"
fi

log "done"
gh release view "$VERSION" --json url --jq .url
