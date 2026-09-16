[English](README.md) | [简体中文](README_zh.md)

# Local erofs-utils patches

Base: upstream erofs-utils **v1.9.1**, archive SHA-256
`a9ef5ab67c4b8d2d3e9ed71f39cd008bda653142a720d8a395a36f1110d0c432`.
Apply `series` in order with `patch --batch --fuzz=0 -p1`; `build-erofs.sh`
does this explicitly after fresh extraction. These are downstream patches,
not a claim of upstream support or acceptance.

`0001-optional-disk-chunk-indexes.patch` implements the chunk-index part of
[guest-runtime#64](https://github.com/kuasar-sandbox/guest-runtime/issues/64).
It retains the file-backed allocator concept from the September 2026 performance
experiment, with one production selector, checked local hash growth, larger
stable record segments, all inode reference-array owners, and capability checks.
It excludes that experiment's buffered-read/SEEK_DATA changes and global hashmap
API changes. The unchanged generic hashmap remains the reference for bucket
growth and chain order. SHA-256 backend selection is a separate change.

The patch's adjacent `.license` file identifies the combined patch as
GPL-2.0-or-later, following the upstream mkfs source. Modified upstream files
retain their original SPDX licenses and copyright
notices. New `lib/index.c` and `lib/liberofs_index.h` use the upstream library's
`GPL-2.0+ OR Apache-2.0` choice. Upstream `COPYING`, `LICENSES/`, and `AUTHORS`
remain the source of third-party attribution. Runtime source inventories retain
this directory and bind it to the exact guest-runtime commit alongside the
unchanged upstream archive identity.

`make -C native-deps test` builds and runs the real candidate, a pristine
reference, syscall-wrapped allocation tests and bounded image fixtures. Fault
injection is linked only into the test executable. Production has no injection
environment variable or heap fallback. Current local validation is Linux x86_64
on ext4; other disk filesystems are accepted only if the required operations
succeed, without an ext4 allowlist. Disk mode requires Linux `O_TMPFILE`,
`fallocate` reservation and `MAP_SHARED` mappings. There is no named-file
fallback. Normal memory mode does not require these filesystem capabilities.
