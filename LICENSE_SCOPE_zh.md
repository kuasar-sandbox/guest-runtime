[English](LICENSE_SCOPE.md) | [简体中文](LICENSE_SCOPE_zh.md)

# 许可证范围

除下述文件外,本仓库中未另行声明许可证的项目原创内容适用根目录的 Apache License 2.0.

## Linux 内核 patch

`native-deps/deps/linux-patches/0001-virtio_balloon-converge-to-a-sustainable-size-under-.patch` 是针对 Linux 6.1.169 内核源码的修改,在本仓库中按 GPL-2.0-only 标识.相邻的 `.license` 文件提供机器可读映射,许可证全文见 [`LICENSES/GPL-2.0-only.txt`](LICENSES/GPL-2.0-only.txt).

用于获取和构建内核的项目脚本及项目维护的配置片段适用根目录的 Apache-2.0.构建时下载且未跟踪在本仓库中的 Linux 源码继续适用其上游许可证.

## Go 子模块声明

`scripts/release-notices/rootless-containers-proto/COPYING` 是上游提交 `4cd87ebadd670f849a518618bc9862e703f9baa5` 中未经修改的 Apache-2.0 许可证。该提交的 `go-proto` 模块归档遗漏了仓库根目录许可证。打包仅为 `github.com/rootless-containers/proto/go-proto@v0.0.0-20230421021042-4cd87ebadd67` 补充这份固定声明，校验其摘要，并保留模块原有的来源和校验和清单。相邻 `NOTICE` 记录来源；其他模块或版本仍必须收集许可证材料。
