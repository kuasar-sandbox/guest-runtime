#!/usr/bin/env bash

set -euo pipefail

REPOSITORY=kuasar-sandbox/guest-runtime

fail() {
  echo "release: $*" >&2
  exit 1
}

validate_version() {
  local name="$1"
  local version="$2"
  case "$name" in
    runtime)
      [[ "$version" =~ ^runtime-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] \
        || fail "runtime version must match runtime-vX.Y.Z without leading zeroes"
      ;;
    vmlinux)
      [[ "$version" =~ ^vmlinux-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] \
        || fail "vmlinux version must match vmlinux-vX.Y.Z without leading zeroes"
      ;;
    *) fail "release name must be runtime or vmlinux" ;;
  esac
}

normalize_arch() {
  case "$1" in
    amd64) echo x86_64 ;;
    arm64) echo aarch64 ;;
    x86_64|aarch64) echo "$1" ;;
    *) fail "unsupported architecture: $1" ;;
  esac
}

read_revisions() {
  local manifest="$1"
  [ -f "$manifest" ] || fail "revision manifest not found: $manifest"
  IFS= read -r header < "$manifest" || fail "revision manifest is empty"
  [ "$header" = $'repository\trequested_ref\tresolved_sha\trole' ] \
    || fail "unexpected revision manifest header"

  local rows="$WORK/revisions.ndjson"
  : > "$rows"
  local line=1 repository requested_ref resolved_sha role extra
  while IFS=$'\t' read -r repository requested_ref resolved_sha role extra; do
    line=$((line + 1))
    if [ -z "$repository" ] || [ -z "$requested_ref" ] || [ -z "$resolved_sha" ] \
      || [ -z "$role" ] || [ -n "${extra:-}" ]; then
      fail "malformed revision manifest row $line"
    fi
    [[ "$repository" =~ ^kuasar-sandbox/[A-Za-z0-9_.-]+$ ]] \
      || fail "invalid repository at row $line"
    [[ "$resolved_sha" =~ ^[0-9a-f]{40}$ ]] \
      || fail "invalid commit at row $line"
    jq -cn \
      --arg repository "$repository" \
      --arg requested_ref "$requested_ref" \
      --arg commit "$resolved_sha" \
      --arg role "$role" \
      '{repository: $repository, requestedRef: $requested_ref, commit: $commit, role: $role}' \
      >> "$rows"
  done < <(sed -n '2,$p' "$manifest")

  [ -s "$rows" ] || fail "revision manifest has no entries"
  [ "$(jq -s --arg repository "$REPOSITORY" '[.[] | select(.repository == $repository)] | length' "$rows")" -eq 1 ] \
    || fail "revision manifest must contain exactly one $REPOSITORY entry"
  jq -s '.' "$rows" > "$WORK/revisions.json"
}

copy_file() {
  local source="$1"
  local destination="$2"
  [ -f "$ROOT/$source" ] || fail "missing release input: $ROOT/$source"
  mkdir -p "$(dirname "$STAGE/$destination")"
  install -m 0644 "$ROOT/$source" "$STAGE/$destination"
}

copy_executable() {
  local source="$1"
  local destination="$2"
  [ -x "$source" ] || fail "missing executable release input: $source"
  mkdir -p "$(dirname "$STAGE/$destination")"
  install -m 0755 "$source" "$STAGE/$destination"
}

copy_external_file() {
  local source="$1"
  local destination="$2"
  [ -f "$source" ] || fail "missing release input: $source"
  mkdir -p "$(dirname "$STAGE/$destination")"
  install -m 0644 "$source" "$STAGE/$destination"
}

copy_root_script() {
  local source="$1"
  local destination="$2"
  [ -f "$ROOT/$source" ] || fail "missing script release input: $ROOT/$source"
  mkdir -p "$(dirname "$STAGE/$destination")"
  install -m 0755 "$ROOT/$source" "$STAGE/$destination"
}

package_release() {
  [ "$#" -eq 5 ] || fail "usage: release.sh package <runtime|vmlinux> <version> <arch> <revisions.tsv> <output-dir>"
  local name="$1"
  local version="$2"
  local arch
  arch="$(normalize_arch "$3")"
  local revisions="$4"
  local output="$5"

  validate_version "$name" "$version"
  if [ -z "$output" ] || [ "$output" = / ] || [ "$output" = . ]; then
    fail "unsafe output directory: $output"
  fi
  [ ! -e "$output" ] || fail "output already exists: $output"
  read_revisions "$revisions"

  local commit
  commit="$(jq -er --arg repository "$REPOSITORY" '.[] | select(.repository == $repository) | .commit' "$WORK/revisions.json")"
  local epoch="${SOURCE_DATE_EPOCH:-0}"
  [[ "$epoch" =~ ^[0-9]+$ ]] || fail "SOURCE_DATE_EPOCH must be an integer"
  local created_at
  created_at="$(date -u -d "@$epoch" +%Y-%m-%dT%H:%M:%SZ)"
  local archive
  case "$name" in
    runtime) archive="sandbox-runtime-$arch-$version.tar.gz" ;;
    vmlinux) archive="vmlinux-$arch-$version.tar.gz" ;;
  esac

  STAGE="$WORK/stage"
  mkdir -p "$STAGE/bin" "$STAGE/release"
  local bin_dir="${RELEASE_BIN_DIR:-$ROOT/bin/$arch}"
  local native_bin_dir="${RELEASE_NATIVE_BIN_DIR:-$ROOT/native-deps/bin/$arch}"
  case "$name" in
    runtime)
      [ "$(jq --arg repository kuasar-sandbox/sandboxer '
        [.[] | select(.repository == $repository and (.requestedRef | test("^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$")))] | length
      ' "$WORK/revisions.json")" -eq 1 ] \
        || fail "runtime revision manifest must identify one versioned sandboxer source"
      copy_external_file "$bin_dir/sandbox-runtime.bundle" bin/sandbox-runtime.bundle
      copy_external_file "$bin_dir/sandbox-runtime.bundle" "bin/sandbox-runtime-$arch-$version.bundle"
      copy_executable "$bin_dir/flatten-ctl" bin/flatten-ctl
      copy_executable "$native_bin_dir/mkfs.erofs" bin/mkfs.erofs
      copy_file README.md docs/guest-runtime.md
      copy_file docs/sandbox-runtime.md docs/sandbox-runtime.md
      copy_file docs/flatten.md docs/flatten.md
      copy_root_script test/e2e/e2e_flatten.sh test/e2e/e2e_flatten.sh
      copy_file test/e2e/README.md test/e2e/README.md
      ;;
    vmlinux)
      copy_external_file "$native_bin_dir/vmlinux" bin/vmlinux
      copy_external_file "$native_bin_dir/vmlinux" "bin/vmlinux-$arch-$version"
      copy_file docs/vmlinux.md docs/vmlinux.md
      ;;
  esac

  jq -n \
    --arg name "$name" \
    --arg version "$version" \
    --arg architecture "$arch" \
    --arg archive "$archive" \
    --arg repository "$REPOSITORY" \
    --arg commit "$commit" \
    --arg created_at "$created_at" \
    --slurpfile sources "$WORK/revisions.json" '
      {
        schemaVersion: 1,
        name: $name,
        version: $version,
        architecture: $architecture,
        archive: $archive,
        repository: $repository,
        commit: $commit,
        createdAt: $created_at,
        sources: $sources[0]
      }
    ' > "$STAGE/release/$name.json"

  mkdir -p "$output/assets"
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" \
    --pax-option=delete=atime,delete=ctime -czf "$output/assets/$archive" -C "$STAGE" .
  (cd "$output/assets" && sha256sum "$archive" > SHA256SUMS)

  local artifacts="$WORK/artifacts.ndjson"
  for filename in "$archive" SHA256SUMS; do
    jq -cn \
      --arg name "$filename" \
      --arg sha256 "$(sha256sum "$output/assets/$filename" | awk '{print $1}')" \
      --argjson size "$(stat -c '%s' "$output/assets/$filename")" \
      '{name: $name, sha256: $sha256, size: $size}' >> "$artifacts"
  done

  : "${RELEASE_WORKFLOW_REPOSITORY:=local}"
  : "${RELEASE_WORKFLOW_RUN_ID:=0}"
  : "${RELEASE_WORKFLOW_RUN_ATTEMPT:=0}"
  : "${RELEASE_WORKFLOW_RUN_URL:=local}"
  jq -n \
    --slurpfile package "$STAGE/release/$name.json" \
    --slurpfile artifacts "$artifacts" \
    --arg workflow_repository "$RELEASE_WORKFLOW_REPOSITORY" \
    --argjson workflow_run_id "$RELEASE_WORKFLOW_RUN_ID" \
    --argjson workflow_run_attempt "$RELEASE_WORKFLOW_RUN_ATTEMPT" \
    --arg workflow_run_url "$RELEASE_WORKFLOW_RUN_URL" '
      $package[0] + {
        kind: "component",
        tag: $package[0].version,
        validation: {
          workflowRepository: $workflow_repository,
          workflowRunId: $workflow_run_id,
          workflowRunAttempt: $workflow_run_attempt,
          workflowRunUrl: $workflow_run_url
        },
        artifacts: $artifacts
      }
    ' > "$output/release.json"
  cat > "$output/release-notes.md" <<EOF
$name $version for Linux $arch.

The release archive is merge-safe: extract it directly into a Kuasar Sandbox deployment directory. Verify it with \`SHA256SUMS\`. The attached \`release.json\` records the exact source revision and artifact digest.
EOF
  validate_bundle "$output"
  echo "==> prepared $output for $version"
}

validate_bundle() {
  [ "$#" -eq 1 ] || fail "usage: release.sh validate <bundle-dir>"
  local bundle="$1"
  local manifest="$bundle/release.json"
  [ -f "$manifest" ] || fail "release.json is missing"
  [ -s "$bundle/release-notes.md" ] || fail "release-notes.md is missing"
  [ -d "$bundle/assets" ] || fail "assets directory is missing"

  jq -e --arg repository "$REPOSITORY" '
      .schemaVersion == 1
      and .kind == "component"
      and (.name == "runtime" or .name == "vmlinux")
      and .repository == $repository
      and (
        (.name == "runtime" and (.version | test("^runtime-v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$")))
        or (.name == "vmlinux" and (.version | test("^vmlinux-v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$")))
      )
      and .tag == .version
      and (.architecture == "x86_64" or .architecture == "aarch64")
      and (.commit | test("^[0-9a-f]{40}$"))
      and (
        (.name == "runtime" and .archive == ("sandbox-runtime-" + .architecture + "-" + .version + ".tar.gz"))
        or (.name == "vmlinux" and .archive == ("vmlinux-" + .architecture + "-" + .version + ".tar.gz"))
      )
      and (.sources | type == "array" and length >= 1)
      and (.commit as $commit | [.sources[] | select(.repository == $repository and .commit == $commit)] | length == 1)
      and (
        .name != "runtime"
        or ([.sources[] | select(
          .repository == "kuasar-sandbox/sandboxer"
          and (.requestedRef | test("^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$"))
        )] | length == 1)
      )
      and (.artifacts | length == 2)
      and ([.artifacts[].name] | unique | length == 2)
      and ([.artifacts[].sha256] | all(test("^[0-9a-f]{64}$")))
      and ([.artifacts[].size] | all(type == "number" and . >= 0))
    ' "$manifest" >/dev/null || fail "release.json failed schema validation"

  local archive name
  archive="$(jq -er '.archive' "$manifest")"
  name="$(jq -er '.name' "$manifest")"
  local expected="$WORK/expected-names"
  local actual="$WORK/actual-names"
  printf '%s\n' "$archive" SHA256SUMS | LC_ALL=C sort > "$expected"
  find "$bundle/assets" -mindepth 1 -maxdepth 1 -type f -printf '%f\n' | LC_ALL=C sort > "$actual"
  cmp -s "$expected" "$actual" || fail "bundle contains an unexpected asset set"

  local artifact_name sha size actual_sha actual_size count=0
  while IFS=$'\t' read -r artifact_name sha size; do
    [[ "$artifact_name" =~ ^[A-Za-z0-9._-]+$ ]] || fail "unsafe artifact name: $artifact_name"
    [ -f "$bundle/assets/$artifact_name" ] || fail "missing artifact: $artifact_name"
    actual_sha="$(sha256sum "$bundle/assets/$artifact_name" | awk '{print $1}')"
    actual_size="$(stat -c '%s' "$bundle/assets/$artifact_name")"
    [ "$actual_sha" = "$sha" ] || fail "checksum mismatch: $artifact_name"
    [ "$actual_size" = "$size" ] || fail "size mismatch: $artifact_name"
    count=$((count + 1))
  done < <(jq -r '.artifacts[] | [.name, .sha256, (.size | tostring)] | @tsv' "$manifest")
  [ "$count" -eq 2 ] || fail "release.json must describe two artifacts"
  (cd "$bundle/assets" && sha256sum --quiet -c SHA256SUMS) \
    || fail "SHA256SUMS validation failed"

  local metadata
  metadata="$(tar -xOzf "$bundle/assets/$archive" "./release/$name.json" 2>/dev/null)" \
    || fail "$archive does not contain release/$name.json"
  jq -e --slurpfile releases "$manifest" '
    $releases[0] as $release
    | .name == $release.name
    and .version == $release.version
    and .architecture == $release.architecture
    and .archive == $release.archive
    and .repository == $release.repository
    and .commit == $release.commit
    and .sources == $release.sources
  ' <<< "$metadata" >/dev/null || fail "archive metadata does not match release.json"
}

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

command -v jq >/dev/null || fail "jq is required"

case "${1:-}" in
  package)
    shift
    package_release "$@"
    ;;
  validate)
    shift
    validate_bundle "$@"
    ;;
  *)
    fail "usage: release.sh <package <runtime|vmlinux> <version> <arch> <revisions.tsv> <output-dir>|validate <bundle-dir>>"
    ;;
esac
