# shellcheck shell=bash
# shellcheck disable=SC2154 # Build context is supplied by build-erofs.sh.
# Sourced by build-erofs.sh. One versioned stamp lives beside BOTH outputs.
# Recipe identities use logical names, never sha256sum's absolute filenames.
erofs_recipe_file() {
    local digest
    digest="$(sha256sum < "$2")" || return
    printf '%s\t%s\n' "$1" "${digest%% *}"
}

erofs_recipe_value() {
    # Compiler flags and sysroot paths can change output semantics. Preserve
    # their values; only source locators and file labels are location-neutral.
    printf '%s\t%s\0' "$1" "$2"
}

erofs_recipe_tool() {
    local label="$1" command="$2" word resolved index=0
    local -a words
    read -r -a words <<< "$command"
    erofs_recipe_value "tool/$label" "$command"
    for word in "${words[@]}"; do
        if resolved="$(type -P -- "$word")" && [ -f "$resolved" ]; then
            erofs_recipe_file "tool/$label/$index" "$resolved" || return
        fi
        index=$((index + 1))
    done
}

erofs_recipe_digest() (
    set -euo pipefail
    local file name tool command program output variable site config_sites
    local -a compiler pkgconfig packages=(uuid libgcrypt gpg-error)
    {
        printf 'erofs-recipe-v2\n'
        erofs_recipe_file source/archive "$tarball"
        for file in ../../Makefile ../Makefile build-erofs.sh common.sh erofs-recipe.sh; do
            erofs_recipe_file "recipe/$file" "$script_dir/$file"
        done
        while IFS= read -r -d '' file; do
            name="${file##*/}"
            [[ "$name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] \
                && [ -f "$file" ] && [ ! -L "$file" ] \
                || die "unsafe erofs patch material: $name"
            erofs_recipe_file "patches/$name" "$file"
        done < <(find "$script_dir/erofs-patches" -mindepth 1 -maxdepth 1 -print0 | LC_ALL=C sort -z)
        # EROFS_TARBALL is a locator, not source identity: the verified bytes and
        # expected digest above/below bind URL and local-archive use alike.
        for variable in EROFS_TARBALL_SHA256 CROSS_PREFIX TARGET_ARCH \
            CC CXX CPP AR RANLIB STRIP LD NM AS OBJDUMP OBJCOPY READELF \
            CFLAGS CXXFLAGS CPPFLAGS LDFLAGS LIBS ARFLAGS \
            CPATH C_INCLUDE_PATH CPLUS_INCLUDE_PATH LIBRARY_PATH COMPILER_PATH GCC_EXEC_PREFIX \
            CONFIG_SITE CONFIG_SHELL MAX_BLOCK_SIZE SOURCE_DATE_EPOCH LANG LC_ALL \
            AUTOCONF AUTOHEADER AUTOMAKE ACLOCAL ACLOCAL_PATH AUTOM4TE M4 LIBTOOLIZE \
            libuuid_CFLAGS libuuid_LIBS; do
            erofs_recipe_value "env/$variable" "${!variable-unset}"
        done
        # Include pkg-config controls and explicit autoconf cache overrides.
        while IFS= read -r variable; do
            case "$variable" in PKG_CONFIG*|*_cv_*)
                erofs_recipe_value "env/$variable" "${!variable}" ;; esac
        done < <(compgen -e | LC_ALL=C sort)
        for tool in gcc g++ ar ranlib strip ld nm as objdump objcopy readelf \
            make patch tar bash sh autoreconf autoconf autoheader automake aclocal autom4te m4 libtoolize; do
            command="$tool"
            case "$tool" in
                gcc) command="${CROSS_PREFIX:+${CROSS_PREFIX}gcc}"; command="${command:-${CC:-gcc}}" ;;
                g++) command="${CROSS_PREFIX:+${CROSS_PREFIX}g++}"; command="${command:-${CXX:-g++}}" ;;
                nm) command="${NM:-${CROSS_PREFIX}nm}" ;;
                ar|ranlib|strip) variable="${tool^^}"
                    command="${CROSS_PREFIX:+${CROSS_PREFIX}$tool}"; command="${command:-${!variable:-$tool}}" ;;
                *) variable="${tool^^}"; command="${!variable:-$tool}" ;;
            esac
            erofs_recipe_tool "$tool" "$command"
        done
        read -r -a compiler <<< "${CC:-gcc}"
        [ -z "$CROSS_PREFIX" ] || compiler=("${CROSS_PREFIX}gcc")
        for program in cc1 collect2 lto1 ld as; do
            output="$("${compiler[@]}" -print-prog-name="$program")"
            erofs_recipe_tool "compiler/$program" "$output"
        done
        command="${PKG_CONFIG:-pkg-config}"
        if [ -z "${PKG_CONFIG:-}" ] && [ -n "$CROSS_PREFIX" ] \
            && type -P "${CROSS_PREFIX}pkg-config" >/dev/null; then
            command="${CROSS_PREFIX}pkg-config"
        fi
        erofs_recipe_tool pkg-config "$command"
        read -r -a pkgconfig <<< "$command"
        # Keep the dependency list aligned with the explicit static preflight.
        for name in "${packages[@]}"; do
            for program in --modversion --cflags --libs --static; do
                output="$("${pkgconfig[@]}" "$program" --libs "$name" 2>&1)" || output="unavailable:$output"
                erofs_recipe_value "pkg/$name/$program" "$output"
            done
            output="$("${pkgconfig[@]}" --variable=pcfiledir "$name" 2>/dev/null)" || output=''
            [ ! -f "$output/$name.pc" ] || erofs_recipe_file "pkg/$name.pc" "$output/$name.pc"
        done
        config_sites="${CONFIG_SITE:-/usr/local/share/config.site /usr/local/etc/config.site}"
        # Autoconf splits its site list and uses defaults for unset OR empty.
        for site in $config_sites; do
            [ ! -f "$site" ] || erofs_recipe_file config.site "$site"
        done
    } | sha256sum | cut -d ' ' -f1
)

# The remaining stamp records are the outputs plus actual external compiler
# dependencies from automake depfiles and both linker maps. No source-tree stamp
# or generated tree is needed for reuse after a native cache restore.
erofs_recipe_stamp() {
    python3 - "$1" "$recipe_stamp" "$recipe" "$BINDIR" "$src_dir" "$script_dir/../.." <<'PY'
import hashlib, os, pathlib, shlex, sys
mode, stamp, recipe, bindir, source, root = sys.argv[1:]
bindir, source, root = (pathlib.Path(p).resolve() for p in (bindir, source, root))

def digest(path):
    with path.open('rb') as data:
        return hashlib.file_digest(data, 'sha256').hexdigest()

def record(path):
    path = pathlib.Path(os.path.abspath(path))
    if path.is_relative_to(bindir):
        scope, name = 'output', str(path.relative_to(bindir))
    elif path.is_relative_to(root):
        scope, name = 'workspace', str(path.relative_to(root))
    else:
        scope, name = 'external', str(path)
    if '\n' in name or '\t' in name:
        raise ValueError('unsupported dependency filename')
    return '\t'.join((digest(path), scope, name))

try:
    if mode == 'check':
        lines = pathlib.Path(stamp).read_text().splitlines()
        if not lines or lines[0] != 'erofs-recipe-v2\t' + recipe:
            sys.exit(1)
        outputs = set()
        for line in lines[1:]:
            expected, scope, name = line.split('\t')
            if scope == 'external' and name.startswith('/'):
                path = pathlib.Path(name)
            elif scope in ('output', 'workspace') and not pathlib.Path(name).is_absolute() and '..' not in pathlib.Path(name).parts:
                path = (bindir if scope == 'output' else root) / name
                if scope == 'output':
                    outputs.add(name)
            else:
                sys.exit(1)
            if digest(path) != expected:
                sys.exit(1)
        sys.exit(0 if outputs == {'mkfs.erofs', 'fsck.erofs'} else 1)
    paths = {bindir / 'mkfs.erofs', bindir / 'fsck.erofs'}
    for directory in ('lib', 'mkfs', 'fsck'):
        depfiles = list((source / directory / '.deps').glob('*.P*'))
        if not depfiles:
            raise ValueError('missing compiler dependency files: ' + directory)
        for depfile in depfiles:
            text = depfile.read_text().replace('\\\n', '')
            # Automake emits an ordinary dependency rule followed by phony rules.
            if text.lstrip().startswith('#') or not text.strip():
                continue  # configure creates dummy files for disabled objects
            dependencies = text.split('\n', 1)[0].split(':', 1)[1]
            for name in shlex.split(dependencies.replace('$$', '$')):
                path = pathlib.Path(os.path.abspath(source / directory / name))
                if not path.is_relative_to(source):
                    paths.add(path)
    for tool in ('mkfs', 'fsck'):
        linkmap = source / tool / (tool + '.erofs.map')
        for line in linkmap.read_text().splitlines():
            if line.startswith('LOAD '):
                path = pathlib.Path(os.path.abspath(source / tool / line[5:].strip()))
                if not path.is_relative_to(source):
                    paths.add(path)
    # Atomic promotion: a failed build/dependency collection leaves no valid stamp.
    temporary = pathlib.Path(stamp + '.tmp')
    temporary.write_text('erofs-recipe-v2\t' + recipe + '\n' + '\n'.join(sorted(map(record, paths))) + '\n')
    temporary.replace(stamp)
except (OSError, ValueError, IndexError) as error:
    if mode != 'check':
        print('erofs recipe: ' + str(error), file=sys.stderr)
    sys.exit(1)
PY
}

erofs_recipe_matches() { erofs_recipe_stamp check; }
erofs_recipe_write_stamp() { erofs_recipe_stamp write; }

erofs_apply_patches() {
    local patch_name
    local -A seen=()
    while IFS= read -r patch_name || [ -n "$patch_name" ]; do
        case "$patch_name" in ''|'#'*) continue ;; esac
        [[ "$patch_name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*\.patch$ ]] \
            && [ -z "${seen[$patch_name]:-}" ] \
            && [ -s "$script_dir/erofs-patches/$patch_name.license" ] \
            || die "invalid or duplicate erofs patch name or missing license: $patch_name"
        seen[$patch_name]=1
        log "applying erofs patch: $patch_name"
        patch --batch --fuzz=0 -d "$src_dir" -p1 < "$script_dir/erofs-patches/$patch_name"
    done < "$script_dir/erofs-patches/series"
}
