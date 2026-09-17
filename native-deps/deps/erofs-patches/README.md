[English](README.md) | [简体中文](README_zh.md)

# EROFS SHA-256 patch

`series` applies `0002-explicit-libgcrypt-sha256.patch` to the original erofs-utils v1.9.1 archive, SHA-256 `a9ef5ab67c4b8d2d3e9ed71f39cd008bda653142a720d8a395a36f1110d0c432`. This standalone patch changes only `lib/sha256.h` and `lib/sha256.c`. It does not include the optional disk index patch.

The build explicitly defines `EROFS_USE_LIBGCRYPT_SHA256` and disables OpenSSL and multithreading. Both SHA entry points initialize Libgcrypt on first use. The single-threaded tools disable secure memory because image chunks are not secrets. Initialization or streaming-context allocation failure aborts before a digest can be used. One-shot hashing uses `gcry_md_hash_buffer`; streaming uses the SHA-256 context, copies all 32 bytes, and closes it. There is no fallback, linker wrapping, digest truncation, layout change, or CPU-specific build flag.

The adjacent `.license` maps the patch to the original file licenses: `lib/sha256.h` is `GPL-2.0-or-later OR Apache-2.0`; `lib/sha256.c` is `Unlicense`. Upstream SPDX and copyright text remain intact. Other EROFS files retain their own licenses, including the GPL-2.0-marked hashmap objects actually linked into the tools. See [license scope](../../../LICENSE_SCOPE.md).

[Libgcrypt](https://www.gnupg.org/software/libgcrypt/index.html) and Libgpg-error libraries use LGPL-2.1-or-later; their source distributions contain additional file-specific licenses and notices. Collect the actual target packages' copyright, LICENSE/COPYING/NOTICE material and source versions, rather than assigning one generic license to all their files. The native release collector records every external link input's bytes and package/source identity in `EROFS-INPUTS.tsv` and `SOURCES.tsv` and rejects missing notices. Patch bytes and sidecars must match the selected commit's local Git objects.

For source rebuilding or relinking, retain the selected guest recipe/helper, ordered patch directory, original pinned archive, both linker maps, EROFS object/archive inputs, and the exact target library source/build configuration and notices. The [native build guide](../../README.md) describes package and sysroot inputs. Rebuild with `make -C native-deps erofs` using the recorded target compiler, flags and static libraries. Release builders must supply matching source/relink materials through the existing materials contract; passing synthetic packaging tests does not establish that those materials have been provisioned.
