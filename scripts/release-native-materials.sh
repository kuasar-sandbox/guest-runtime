#!/usr/bin/env bash
# Copyright/source material for system archives actually linked into EROFS.
# Sourced by release.sh; uses its existing material layout and fail().
release_native_copy_file() {
  local source="$1" label="$2" name="$3"
  release_materials_safe_relative "$label/$name" || fail "unsafe native material name"
  [ -s "$source" ] || fail "native license material is missing: $source"
  local destination="$RELEASE_MATERIALS_STAGE/share/licenses/$RELEASE_MATERIALS_UNIT/$label/$name"
  mkdir -p "$(dirname "$destination")"
  install -m 0644 "$source" "$destination"
}

release_native_source_built_uuid() {
  local input="$1" payload="$2" catalog="$3" line file expected name version source integrity licenses
  if [ ! -s "$catalog/SOURCES.tsv" ] || [ ! -s "$catalog/MATERIALS.sha256" ]; then
    fail "source-built libuuid material is missing; update the trusted runner template"
  fi
  if find "$catalog" -type l -print -quit | grep -q .; then
    fail "source-built libuuid material contains a symbolic link"
  fi
  while IFS= read -r line; do
    [[ "$line" =~ ^[0-9a-f]{64}\ \ [A-Za-z0-9._+/-]+$ ]] \
      || fail "invalid libuuid material inventory record"
    file="${line#*  }"
    release_materials_safe_relative "$file" || fail "unsafe libuuid material inventory path"
  done < "$catalog/MATERIALS.sha256"
  (cd "$catalog" && sha256sum --status -c MATERIALS.sha256) \
    || fail "source-built libuuid material checksum mismatch"
  sed 's/^.*  //' "$catalog/MATERIALS.sha256" | LC_ALL=C sort \
    > "$RELEASE_MATERIALS_WORK/native-expected-files"
  (cd "$catalog" && find licenses SOURCES.tsv -type f -print | LC_ALL=C sort) \
    > "$RELEASE_MATERIALS_WORK/native-actual-files"
  cmp -s "$RELEASE_MATERIALS_WORK/native-expected-files" "$RELEASE_MATERIALS_WORK/native-actual-files" \
    || fail "source-built libuuid material inventory is incomplete"
  awk -F '\t' '
    NR == 1 {if ($0 != "payload\tname\tversion\tsource\tintegrity\tlicense_directory") exit 1}
    NR == 2 {if (NF != 6 || $3 == "" || $4 == "") exit 1}
    END {if (NR != 2) exit 1}
  ' "$catalog/SOURCES.tsv" || fail "invalid libuuid source record"
  IFS=$'\t' read -r file name version source integrity licenses \
    < <(sed -n '2p' "$catalog/SOURCES.tsv")
  if [ "$file" != libuuid.a ] || [ "$name" != util-linux ] || [ "$licenses" != licenses ]; then
    fail "invalid source-built libuuid identity"
  fi
  expected="${integrity%%;*}"
  [ "$expected" = "sha256:$(sha256sum "$input" | awk '{print $1}')" ] \
    || fail "source-built libuuid does not match its recorded payload"
  if [ ! -s "$catalog/licenses/COPYING" ] || [ ! -s "$catalog/licenses/libuuid/COPYING" ]; then
    fail "source-built libuuid license text is missing"
  fi
  while IFS= read -r file; do
    release_native_copy_file "$file" system/libuuid.a "${file#"$catalog/licenses/"}"
  done < <(find "$catalog/licenses" -type f -print | LC_ALL=C sort)
  release_materials_record_source "$payload" system:libuuid.a "$version" "$source" "$integrity" system/libuuid.a
}

release_native_system_input() {
  local input="$1" payload="$2" query owner source_name version label copyright common
  local source_id rpm_source sibling file count=0
  input="$(realpath -e "$input")" || fail "native link input is missing"
  label="system/$(basename "$input")"
  if [ "$input" = /usr/lib64/libuuid.a ] && [ -f /usr/lib64/.kuasar-libuuid-build-id ]; then
    local build_id
    build_id="$(cat /usr/lib64/.kuasar-libuuid-build-id)"
    [[ "$build_id" =~ ^[0-9a-f]{64}$ ]] || fail "invalid source-built libuuid identity"
    release_native_source_built_uuid "$input" "$payload" "/usr/share/kuasar-ci/native-libuuid/$build_id"
    return
  fi
  if command -v dpkg-query >/dev/null 2>&1 \
    && query="$(dpkg-query -S "$input" 2>/dev/null)"; then
    owner="${query%%: /*}"
    [[ "$owner" != *$'\n'* && "$owner" != *,* ]] || fail "ambiguous native package owner"
    query="$(dpkg-query -W -f '${source:Package}\t${source:Version}\n' "$owner")"
    IFS=$'\t' read -r source_name version <<< "$query"
    if [ -z "$source_name" ] || [ -z "$version" ]; then
      fail "native source package identity is missing"
    fi
    source_id="deb-source:$source_name@$version"
    copyright="/usr/share/doc/${owner%%:*}/copyright"
    release_native_copy_file "$copyright" "$label" copyright
    # Debian copyright files refer to common license texts outside the package.
    while IFS= read -r common; do
      [ -n "$common" ] || continue
      if [ ! -e "$common" ] && [[ "$common" == *. ]]; then
        common="${common%.}"
      fi
      release_native_copy_file "$common" "$label" "common-licenses/$(basename "$common")"
    done < <(grep -Eo '/usr/share/common-licenses/[A-Za-z0-9.+-]+' "$copyright" | LC_ALL=C sort -u)
  elif command -v rpm >/dev/null 2>&1 \
    && query="$(rpm -qf --qf '%{NAME}\t%{VERSION}-%{RELEASE}\t%{SOURCERPM}\n' "$input" 2>/dev/null)"; then
    IFS=$'\t' read -r owner version rpm_source <<< "$query"
    if [ -z "$rpm_source" ] || [ "$rpm_source" = '(none)' ]; then
      fail "native RPM source identity is missing: $input"
    fi
    source_name="$owner"
    source_id="rpm-source:$rpm_source"
    # Static/devel subpackages may keep notices in a sibling from the SAME SRPM.
    # Capture each status before accepting potentially partial command output.
    local packages siblings files
    packages="$(rpm -qa --qf '%{NAME}.%{ARCH}\t%{SOURCERPM}\n')" \
      || fail "cannot enumerate installed RPM packages for license collection"
    siblings="$(awk -F '\t' -v source="$rpm_source" '$2 == source {print $1}' <<< "$packages")" \
      || fail "cannot select same-source RPM license packages"
    while IFS= read -r sibling; do
      [ -n "$sibling" ] || continue
      files="$(rpm -ql "$sibling")" || fail "cannot enumerate RPM license files: $sibling"
      while IFS= read -r file; do
        case "$(basename "$file")" in
          LICENSE*|COPYING*|NOTICE*|COPYRIGHT*|copyright|AUTHORS*|CREDITS*) ;;
          *) [[ "$file" == /usr/share/licenses/* ]] || continue ;;
        esac
        [ ! -d "$file" ] || continue
        release_native_copy_file "$file" "$label" "${file#/}"
        count=$((count + 1))
      done <<< "$files"
    done <<< "$siblings"
    [ "$count" -gt 0 ] || fail "native RPM license material is missing: $rpm_source"
  else
    fail "native input has no package/source material: $input"
  fi
  release_materials_record_source "$payload" "system:$(basename "$input")" "$version" \
    "$source_id" "sha256:$(sha256sum "$input" | awk '{print $1}');package:$source_name" "$label"
}

release_native_erofs_inputs() {
  local map="$1" erofs_source="$2" payload="$3" input canonical name count=0
  local inventory="$RELEASE_MATERIALS_WORK/erofs-inputs"
  local -A selected_inputs=()
  [ -s "$map" ] || fail "matching EROFS linker map is missing"
  : > "$inventory"
  erofs_source="$(realpath -e "$erofs_source")"
  while IFS= read -r input; do
    [ -n "$input" ] || continue
    if [[ "$input" != /* ]]; then input="$(dirname "$map")/$input"; fi
    canonical="$(realpath -e "$input")" || fail "linker map input no longer exists: $input"
    case "$canonical" in
      "$erofs_source"/*) continue ;; # Covered by exact erofs-utils source material.
    esac
    name="$(basename "$canonical")"
    if [ -n "${selected_inputs[$name]:-}" ] && [ "${selected_inputs[$name]}" != "$canonical" ]; then
      fail "distinct native link inputs share a material name: $name"
    fi
    [ -z "${selected_inputs[$name]:-}" ] || continue
    selected_inputs[$name]="$canonical"
    release_native_system_input "$canonical" "$payload"
    printf '%s\t%s\n' "$name" "$(sha256sum "$canonical" | awk '{print $1}')" >> "$inventory"
    count=$((count + 1))
  done < <(awk '$1 == "LOAD" && $2 ~ /\.(a|o)$/ {print $2}' "$map" | LC_ALL=C sort -u)
  [ "$count" -gt 0 ] || fail "EROFS linker map contains no system inputs"
  for input in libc.a libuuid.a; do
    awk -F '\t' -v name="system:$input" '$2 == name {found=1} END {exit !found}' \
      "$RELEASE_MATERIALS_WORK/sources" || fail "EROFS source material is missing $input"
  done
  local destination="$RELEASE_MATERIALS_STAGE/share/sources/$RELEASE_MATERIALS_UNIT/EROFS-INPUTS.tsv"
  { printf 'input\tsha256\n'; LC_ALL=C sort "$inventory"; } > "$destination"
  chmod 0644 "$destination"
}

release_native_validate_erofs_inventory() {
  local source="$1/share/sources/runtime" expected="$WORK/expected-erofs-inputs"
  local actual="$WORK/actual-erofs-inputs" inventory="$1/share/sources/runtime/EROFS-INPUTS.tsv"
  if [ ! -s "$inventory" ] || [ -L "$inventory" ] || [ "$(stat -c '%a' "$inventory")" != 644 ]; then
    fail "missing or unsafe complete EROFS input inventory"
  fi
  awk -F '\t' '
    NR == 1 { if ($0 != "input\tsha256") exit 1; next }
    NF != 2 || $1 !~ /^[A-Za-z0-9._+-]+[.](a|o)$/ ||
      length($2) != 64 || $2 ~ /[^0-9a-f]/ || seen[$1]++ { exit 1 }
    { rows++; print }
    END { if (rows < 2) exit 1 }
  ' "$inventory" > "$expected" || fail "invalid complete EROFS input inventory"
  LC_ALL=C sort -o "$expected" "$expected"
  awk -F '\t' '
    $2 ~ /^system:/ {
      if ($1 != "bin/mkfs.erofs,bin/sandbox-runtime.bundle:/opt/sandbox-runtime/bin/mkfs.erofs") exit 1
      name=$2; sub(/^system:/, "", name)
      if ($6 != "share/licenses/runtime/system/" name) exit 1
      split($5, fields, ";"); digest=fields[1]; sub(/^sha256:/, "", digest)
      if (fields[1] !~ /^sha256:/ || length(digest) != 64 || digest ~ /[^0-9a-f]/) exit 1
      print name FS digest
    }
  ' "$source/SOURCES.tsv" > "$actual" || fail "invalid EROFS source input identity"
  LC_ALL=C sort -o "$actual" "$actual"
  cmp -s "$expected" "$actual" || fail "EROFS source records omit or alter collected linker inputs"
}
