[English](README.md) | [简体中文](README_zh.md)

# 本地 erofs-utils 补丁

基线为上游 erofs-utils **v1.9.1**，源码归档 SHA-256：
`a9ef5ab67c4b8d2d3e9ed71f39cd008bda653142a720d8a395a36f1110d0c432`。
按 `series` 顺序执行 `patch --batch --fuzz=0 -p1`；`build-erofs.sh`
在重新解压后显式完成这一步。这些是下游补丁，不表示上游已支持或接纳。

`0001-optional-disk-chunk-indexes.patch` 实现
[guest-runtime#64](https://github.com/kuasar-sandbox/guest-runtime/issues/64)
的 chunk 索引部分。它保留 2026 年 9 月性能实验的文件支持分配思路，改为单一
生产选择变量、局部且可检查的哈希增长、更大的稳定记录分段、全部 inode 引用
数组所有者及能力检查。它不包含实验的缓冲读取/SEEK_DATA 修改或全局 hashmap
API 修改。未改动的通用 hashmap 仍是桶增长及链顺序的参考实现。
SHA-256 后端选择由另一个独立修改处理。

相邻 `.license` 沿用上游 mkfs 源码，将组合补丁标识为 GPL-2.0-or-later。
修改的上游文件保留其原始 SPDX 许可证和版权声明。新增 `lib/index.c` 与
`lib/liberofs_index.h` 使用上游库的 `GPL-2.0+ OR Apache-2.0` 选择。
上游 `COPYING`、`LICENSES/` 和 `AUTHORS` 仍是第三方归属材料来源。
Runtime 源码清单保留此目录，并绑定 guest-runtime 精确提交，同时保留未改变
的上游归档身份。

`make -C native-deps test` 构建并运行真实候选、原始上游参考、带系统调用包装
的分配测试及有界镜像输入。故障注入只链接到测试程序；生产没有注入环境变量
或堆回退。当前本地验证使用 Linux x86_64/ext4；其他磁盘文件系统只有在所需
操作成功时才会被接受，没有 ext4 白名单。磁盘模式要求 Linux `O_TMPFILE`、
`fallocate` 空间预留及 `MAP_SHARED` 映射，不使用具名文件回退。
普通内存模式不要求这些文件系统能力。
