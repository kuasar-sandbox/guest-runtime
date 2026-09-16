[English](LICENSE_SCOPE.md) | [简体中文](LICENSE_SCOPE_zh.md)

<a id="许可证范围"></a>

# License scope

Unless stated otherwise below or in the affected file, original project material in this repository is covered by the Apache License 2.0 in the repository root.

<a id="linux-内核-patch"></a>

## Linux kernel patch

`native-deps/deps/linux-patches/0001-virtio_balloon-converge-to-a-sustainable-size-under-.patch` modifies Linux 6.1.169 kernel sources and is identified as GPL-2.0-only in this repository. The adjacent `.license` file provides a machine-readable mapping. The full license text is in [LICENSES/GPL-2.0-only.txt](LICENSES/GPL-2.0-only.txt).

Project scripts that fetch and build the kernel, and project-maintained configuration fragments, use the root Apache-2.0 license. Linux sources downloaded during a build and not tracked in this repository retain their upstream licenses.

## EROFS patch

`native-deps/deps/erofs-patches/0001-optional-disk-chunk-indexes.patch` modifies
erofs-utils v1.9.1 and is identified as GPL-2.0-or-later by its adjacent
`.license` file. The GPL version 2 text is already retained in
[LICENSES/GPL-2.0-only.txt](LICENSES/GPL-2.0-only.txt); the patch's “or later”
choice follows the affected upstream mkfs source. Original file-level notices
and the library's `GPL-2.0+ OR Apache-2.0` choice remain intact. New library
files use that same dual-license choice. Build scripts and tests retain their
stated project licenses. Downloaded upstream sources retain their own licenses;
Runtime materials preserve their notices and the applied patch provenance.
