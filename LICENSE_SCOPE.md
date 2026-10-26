[English](LICENSE_SCOPE.md) | [简体中文](LICENSE_SCOPE_zh.md)

<a id="许可证范围"></a>

# License scope

Unless stated otherwise below or in the affected file, original project material in this repository is covered by the Apache License 2.0 in the repository root.

<a id="linux-内核-patch"></a>

## Linux kernel builds

Project scripts that fetch and build the kernel, and project-maintained configuration fragments, use the root Apache-2.0 license. Linux sources downloaded during a build and not tracked in this repository retain their upstream licenses.

## Go submodule notices

`scripts/release-notices/rootless-containers-proto/COPYING` is the unmodified Apache-2.0 license from upstream commit `4cd87ebadd670f849a518618bc9862e703f9baa5`. That commit's `go-proto` module archive omits the repository-root license. Packaging supplements only `github.com/rootless-containers/proto/go-proto@v0.0.0-20230421021042-4cd87ebadd67` with this pinned notice, verifies its digest and retains the module's ordinary source/checksum inventory. The adjacent `NOTICE` records provenance; this does not exempt other modules or versions from license collection.
