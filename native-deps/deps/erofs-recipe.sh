# Sourced by build-erofs.sh after resolving the verified source archive.
# Keep the recipe beside the outputs AND extracted sources: an old executable
# or .extracted marker alone must never hide changed patches or build flags.
erofs_recipe_digest() {
    {
        sha256sum "$tarball" "$script_dir/build-erofs.sh" \
            "$script_dir/common.sh" "$script_dir/erofs-recipe.sh"
        (cd "$script_dir/erofs-patches" && sha256sum series *.patch)
        printf '%s\n' "$EROFS_TARBALL" "$EROFS_TARBALL_SHA256" \
            "$CROSS_PREFIX" "${TARGET_ARCH:-}" \
            "${CC:-}" "${CXX:-}" "${AR:-}" "${RANLIB:-}" "${STRIP:-}" \
            "${CFLAGS:-}" "${CPPFLAGS:-}" "${LDFLAGS:-}" \
            "${PKG_CONFIG:-}" "${PKG_CONFIG_PATH:-}" "${PKG_CONFIG_LIBDIR:-}"
    } | sha256sum | cut -d ' ' -f1
}

erofs_apply_patches() {
    local patch_name
    while IFS= read -r patch_name || [ -n "$patch_name" ]; do
        case "$patch_name" in
            ''|'#'*) continue ;;
        esac
        [[ "$patch_name" =~ ^[A-Za-z0-9._-]+\.patch$ ]] \
            || die "invalid erofs patch name in series: $patch_name"
        log "applying erofs patch: $patch_name"
        patch --batch --fuzz=0 -d "$src_dir" -p1 \
            < "$script_dir/erofs-patches/$patch_name"
    done < "$script_dir/erofs-patches/series"
}
