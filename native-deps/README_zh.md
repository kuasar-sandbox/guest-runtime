[English](README.md) | [简体中文](README_zh.md)

# Native 源码与内核补丁维护

生产目标、输入与打包流程见[构建](../docs/build_zh.md)。本就近指南负责 Native 源码/缓存布局、安全增量清理和内核补丁开发工作树；其中 Make 目标从 `guest-runtime/native-deps/` 运行。

native-deps 默认构建 `vmlinux`（Guest 内核）和 `envd`（E2B Guest agent）；`versitygw` 是可选目标。镜像导出使用链接在 `flatten-ctl` 内的 Go EROFS 库。Runtime bundle 打包使用宿主预安装的 mkfs，独立校验使用 fsck/dump。项目不再维护 erofs-utils 补丁或编译、分发 mkfs。

公共流水线:按 URL pin 的上游 tarball(可选 SHA256 校验)→ 共享缓存与
解压 → 本地补丁(vmlinux,`git am`)→ 构建 → `bin/<arch>/`。
本文覆盖 patch 开发循环与源码/缓存清理；生产目标、交叉编译与打包统一见[构建](../docs/build_zh.md)。产物本身的设计契约不在本文：内核配置体系见 [Guest 内核规范](../docs/vmlinux_zh.md)。patched
`cloud-hypervisor` 是 `sandbox-ctl` 的 VMM 运行件,构建与 patch 契约见
[Cloud Hypervisor](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/vmm_zh.md)。

平台级聚合由项目主仓编排:
`make -C kuasar-sandbox build` 首先驱动本目录 `make build`,再把
产物按 `kuasar-sandbox/release/inputs/bin-inputs.yaml` 收集进
`kuasar-sandbox/bin/<arch>/`,供 e2e、demo 与本地集成复用。

## 构建源码组织

| 路径 | 角色 |
|---|---|
| `deps/build-{vmlinux,envd}.sh` | 源码构建脚本(vmlinux 为 STAGE 多阶段 dispatcher) |
| `deps/build-versitygw.sh` | 可选网关构建 |
| `deps/common.sh` | tarball 下载 / 缓存 / 解压共享逻辑 |
| `deps/linux-patches/` | guest 内核补丁(arch-neutral) |
| `deps/vmlinux/*.config` | 内核 defconfig 片段(common + per-arch) |

## 目录布局

```
bin/
├── x86_64/                  x86_64 产物: vmlinux envd
├── aarch64/                 aarch64 产物(同上)
└── <name>                   软链 → <host-arch>/<name>,仅原生构建时生成/更新

build/
├── tarball/                 上游 tarball 缓存(跨架构共享)
├── src/
│   ├── linux/               内核源树(跨架构共享;git 仓,patch 开发 WIP 所在)
│   ├── e2b-infra/           envd 源树(跨架构共享,Go 以 GOARCH 选目标)
│   └── versitygw/           可选网关的共享源码
├── x86_64/
│   └── linux/               kbuild O= 输出
└── aarch64/                 (同上)
```

软链接策略:host = target 的原生构建为公共二进制入口生成 `bin/<name> → <arch>/<name>`;
交叉构建不更新 host 入口软链,避免指向 host 不可执行的产物。
可选网关仅在构建对应目标后才有输出。

## 幂等与缓存

Envd 复用已存在的目标文件；内核根据脚本、配置和补丁执行增量 Kbuild。下载保留在 `build/tarball/` 并按预期摘要校验。`make clean` 删除 `bin/` 与目标构建输出，保留 tarball 缓存和架构无关源码树，尤其保留内核补丁开发中的工作。宿主 erofs-utils 由环境管理，不进入本项目的 native cache。

## patch 开发循环(vmlinux)

vmlinux 通过 `deps/build-vmlinux.sh` 的 STAGE dispatcher 维护补丁,
Makefile 暴露 `linux-*` 目标:

| STAGE | make 目标 | 行为 |
|---|---|---|
| `fetch` | `linux-fetch` | 解压 tarball → `git init` + 全量 import commit + 打 base tag(`linux-patches-base`);已有 base tag 则跳过;存在无 tag 的外来 git 树则拒绝,不覆盖 WIP |
| `patches-apply` | `linux-patches-apply` | `git am deps/linux-patches/*.patch`;幂等与 sanity 语义见下 |
| `patches-format` | `linux-patches-format` | `git format-patch <base>..HEAD` 回写 `deps/linux-patches/`,先清旧 `*.patch` |
| `build` | `linux-build` | 仅构建,不动 patch |

`linux-patches` 是 `linux-patches-apply` 的别名。从 `guest-runtime/native-deps/` 执行:

```bash
make linux-fetch       # 一次性: 拉源码 + git tag linux-patches-base
cd build/src/linux
# 改源码 + git commit,每个 patch 一个 commit
cd ../../..            # 回到 native-deps 再执行它的 Make 目标
make linux-patches-format
make vmlinux           # 重新 apply + build,验证可重复
```

`patches-apply` 的 sanity 检查,核心目标是**绝不静默覆盖开发中的改动**:

- 源树必须有 base tag,否则报错并指引先跑 `make {linux,ch}-fetch`;
- `HEAD == base`:`git am` 应用全部 patch;空补丁集则直接成功,不改源码;
- `HEAD = base + N` 且 N 等于 patch 数、commit subject 与 patch 文件逐一匹配:
  视为已应用,幂等跳过;
- 其他任何状态:报错并指引先用 `make linux-patches-format` 保存 WIP,确认已保存后
  才 reset 到 base tag,再重跑。

两个架构均应用 [内核 balloon 行为](../docs/vmlinux_zh.md#内存压力下的-balloon-行为) 所述的 balloon order-0 批处理补丁。补丁管线仍然 arch-neutral:两架构共用同一补丁目录,
各自 defconfig 编译。

空补丁集且 `HEAD != base` 时拒绝继续:源码可能仍带有已移除的补丁或未导出的
工作。先保存这些工作,再为无补丁构建选择新的 `LINUX_BUILD_SRC` 和
`LINUX_BUILD_OUT` 路径。`make clean` 保留源码树,因此不会移除之前应用的补丁。
显式 `linux-build` 目标仍可用于直接构建开发中的源码树,不应用受跟踪的补丁集。

## 相关契约

- [Guest 内核规范](../docs/vmlinux_zh.md) —— guest 内核配置体系、架构差异与关键决策。
- [Release](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release_zh.md) —— runtime、vmlinux 独立版本与平台聚合发布;
  本目录运行期必需产物经 `kuasar-sandbox/release/inputs/bin-inputs.yaml` 收集进共享 `bin/`。
