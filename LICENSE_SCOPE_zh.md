[English](LICENSE_SCOPE.md) | [简体中文](#许可证范围)

# 许可证范围

除下述文件外,本仓库中未另行声明许可证的项目原创内容适用根目录的 Apache License 2.0.

## Linux 内核构建

`native-deps/deps/linux-patches/` 中受跟踪的 Linux 补丁使用 GPL-2.0-only，每个补丁均附有 SPDX 许可证文件。

用于获取和构建内核的项目脚本及项目维护的配置片段适用根目录的 Apache-2.0.构建时下载且未跟踪在本仓库中的 Linux 源码继续适用其上游许可证.

## Go 子模块声明

`scripts/release-notices/rootless-containers-proto/COPYING` 是 Go 子模块归档遗漏的上游 Apache-2.0 许可证。打包为选定的 `github.com/rootless-containers/proto/go-proto` 依赖补充许可证和人类可读的归属说明；其他模块和版本仍须提供各自的许可证材料。

## Runtime 原生载荷

Runtime Bundle 还包含 Envd、私有 systemd/D-Bus 安装及其选定 loader 和依赖库。这些内容保留上游
许可证，根目录 Apache-2.0 声明不对其重新授权。Runtime 归档在 `share/licenses/runtime/` 下携带
收集的材料，其中包括 `share/licenses/runtime/systemd-runtime/`。原生安装清单及包校验将该清单与
交付载荷对应起来。详见 [Native 维护](native-deps/README_zh.md#systemd-私有安装)和
[Runtime 交付](docs/runtime-bundle_zh.md#发布件)。
