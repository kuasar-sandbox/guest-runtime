[English](LICENSE_SCOPE.md) | [简体中文](LICENSE_SCOPE_zh.md)

# 许可证范围

除下述文件外,本仓库中未另行声明许可证的项目原创内容适用根目录的 Apache License 2.0.

## Linux 内核 patch

`native-deps/deps/linux-patches/0001-virtio_balloon-converge-to-a-sustainable-size-under-.patch` 是针对 Linux 6.1.169 内核源码的修改,在本仓库中按 GPL-2.0-only 标识.相邻的 `.license` 文件提供机器可读映射,许可证全文见 [`LICENSES/GPL-2.0-only.txt`](LICENSES/GPL-2.0-only.txt).

用于获取和构建内核的项目脚本及项目维护的配置片段适用根目录的 Apache-2.0.构建时下载且未跟踪在本仓库中的 Linux 源码继续适用其上游许可证.

## EROFS 补丁

`native-deps/deps/erofs-patches/0001-optional-disk-chunk-indexes.patch` 修改
erofs-utils v1.9.1，相邻 `.license` 文件标识为 GPL-2.0-or-later。GPL 第 2 版
全文已保存在 [LICENSES/GPL-2.0-only.txt](LICENSES/GPL-2.0-only.txt)；补丁的
“或更高版本”选择沿用被修改的上游 mkfs 源码。原有文件级声明和库的
`GPL-2.0+ OR Apache-2.0` 选择保持不变，新增库文件使用同一双许可证选择。
构建脚本及测试保留其声明的项目许可证。下载的上游源码保留各自许可证；
Runtime 材料保留上游声明及实际应用补丁的来源。
