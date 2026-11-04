[English](LICENSE_SCOPE.md) | [简体中文](LICENSE_SCOPE_zh.md)

<a id="许可证范围"></a>

# License scope

Unless stated otherwise below or in the affected file, original project material in this repository is covered by the Apache License 2.0 in the repository root.

<a id="linux-内核-patch"></a>

## Linux kernel builds

Tracked Linux patches in `native-deps/deps/linux-patches/` are GPL-2.0-only. Each patch has an adjacent SPDX license file.

Project scripts that fetch and build the kernel, and project-maintained configuration fragments, use the root Apache-2.0 license. Linux sources downloaded during a build and not tracked in this repository retain their upstream licenses.

## Go submodule notices

`scripts/release-notices/rootless-containers-proto/COPYING` contains the upstream Apache-2.0 license omitted by the Go submodule archive. Packaging supplements the selected `github.com/rootless-containers/proto/go-proto` dependency with this license and its human attribution. Other modules and versions still require their own license material.
