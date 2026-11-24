[English](README.md) | [简体中文](README_zh.md)

# Native 源码与内核补丁维护

生产目标、输入与打包流程见[构建](../docs/build_zh.md)。本就近指南负责 Native 源码/缓存布局、安全增量清理和内核补丁开发工作树；其中 Make 目标从 `guest-runtime/native-deps/` 运行。

native-deps 默认构建 `vmlinux`（Guest 内核）、`envd`（E2B Guest agent）和私有 `systemd/` 安装；`versitygw` 是可选目标。镜像导出使用链接在 `flatten-ctl` 内的 Go EROFS 库。Runtime bundle 打包使用宿主预安装的 mkfs，独立校验使用 fsck/dump。项目不再维护 erofs-utils 补丁或编译、分发 mkfs。

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
| `deps/build-systemd.sh`、`deps/systemd-lock.json`、`deps/package-systemd.py` | 固定 systemd/D-Bus 原生配方、源码 pin、私有 ELF 闭包和许可清单 |
| `deps/build-versitygw.sh` | 可选网关构建 |
| `deps/common.sh` | tarball 下载 / 缓存 / 解压共享逻辑 |
| `deps/linux-patches/` | guest 内核补丁(arch-neutral) |
| `deps/vmlinux/*.config` | 内核 defconfig 片段(common + per-arch) |

## 目录布局

```
bin/
├── x86_64/                  x86_64 产物: vmlinux envd systemd/
├── aarch64/                 aarch64 产物(同上)
└── <name>                   软链 → <host-arch>/<name>,仅原生构建时生成/更新

build/
├── tarball/                 上游 tarball 缓存(跨架构共享)
├── src/
│   ├── linux/               内核源树(跨架构共享;git 仓,patch 开发 WIP 所在)
│   ├── e2b-infra/           envd 源树(跨架构共享,Go 以 GOARCH 选目标)
│   └── versitygw/           可选网关的共享源码
├── x86_64/
│   ├── linux/               kbuild O= 输出
│   └── systemd/             可重建源码、构建、安装目录与输入记录
└── aarch64/                 (同上)
```

软链接策略:host = target 的原生构建为公共二进制入口生成 `bin/<name> → <arch>/<name>`;
交叉构建不更新 host 入口软链,避免指向 host 不可执行的产物。
可选网关仅在构建对应目标后才有输出。

## 幂等与缓存

Envd 复用已存在的目标文件；内核根据脚本、配置和补丁执行增量 Kbuild。下载保留在 `build/tarball/` 并按预期摘要校验。`make clean` 删除 `bin/` 与目标构建输出，保留 tarball 缓存和架构无关源码树，尤其保留内核补丁开发中的工作。宿主 erofs-utils 由环境管理，不进入本项目的 native cache。

## systemd 私有安装

`make systemd` 构建 systemd **257.13**（commit `70b5d110be7702afc4dbce012f60d49506d513da`）和 D-Bus **1.14.10**。`deps/systemd-lock.json` 固定两个源码归档摘要、启用功能和明确的 dlopen/NSS 依赖。`SYSTEMD_TARBALL`、`DBUS_TARBALL` 可以选择本地归档，但不能绕过固定 SHA256；构建机额外安装的可选库不会隐式开启新的 systemd 功能。

此目标在 x86_64、aarch64 上分别原生构建。交叉装配通过顶层 `SYSTEMD_PAYLOAD` 选择对应原生构建的安装树，不在异架构宿主执行或推断目标 DSO。除通用构建工具外，还需预装：

```text
build-essential meson ninja-build pkg-config python3-jinja2 gperf
libcap-dev libmount-dev libseccomp-dev libblkid-dev libacl1-dev
liblz4-dev libzstd-dev liblzma-dev libpcre2-dev libssl-dev
libexpat1-dev libsystemd-dev busybox-static patchelf xz-utils
curl ca-certificates binutils
```

生产 Workbench 必须在选择前已包含这些依赖。配方不安装宿主包，可在普通 UID、只读容器根、清空 capabilities 和可写构建目录中运行。`KUASAR_BUILD_JOBS` 指定编译 CPU 预算。

输出 `bin/<arch>/systemd/` 对应最终 `/run/sandbox-runtime/systemd` 前缀，包含 manager、executor、shutdown helper、journal、工具、units、generators、系统总线和完整动态依赖。校验不执行目标 ELF；匹配 loader 原样复制，其他动态文件设置私有 PT_INTERP 和库搜索路径。Runtime 不依赖用户 libc 或全局 LD_LIBRARY_PATH。唯一指向前缀外的配置链接是经过审阅的 `/etc/environment` 兼容入口。

输入记录包含配方摘要、编译器/工具版本、构建设置和已安装包身份；仅输入记录与完整安装都匹配时复用已有产物。重建从已验证归档重新解压，使用干净 DESTDIR；`build/<arch>/systemd/src/` 是可丢弃的构建源码，不作为补丁开发工作树，也不影响内核维护中的源树。

`share/sandbox-runtime/systemd-manifest.json` 记录所有目录、文件、链接、摘要、loader、源码 pin、依赖包/源包版本和许可路径。许可材料包含 systemd/D-Bus 上游文本、依赖 copyright 及其引用的公共许可证。发行校验从 EROFS 独立读回完整安装，与清单比较，并将相同许可材料收集到 `share/licenses/runtime/systemd-runtime/`。

`dbus-daemon-launch-helper` 是唯一 setuid 文件，EROFS 中为 root 所有、模式 4755；它先核对调用者真实 UID 等于配置中的 messagebus 账号，再执行特权激活。活动 Runtime 投影必须保留该已审阅权限且不能带 nosuid；其他工具不获得特殊权限位或文件 capabilities。dump.erofs 的 Access 文本会省略特殊权限位，校验器因此读取原始 inode mode。

源码锁包含 `deps/systemd-patches/0001-private-configuration-search-path.patch` 摘要。补丁在共享配置搜索路径中，把编译期私有 vendor 目录插入 `/etc`、`/run` 之后、rootfs vendor 目录之前。仅设置 `--prefix` 不会让 sysusers、tmpfiles、preset、journald 找到这些私有默认配置。配方先验证补丁摘要，再应用到重新解压的 upstream 源码；安装清单记录补丁身份。

`make test` 运行不依赖原生产物的篡改拒绝测试。原生 Docker 宿主还可验证不含 rootfs libc 时的默认配置发现及传统 D-Bus 激活：

```bash
python3 -B deps/test-systemd-config.py \
  --payload "$PWD/bin/$(uname -m)/systemd" \
  --output /tmp/systemd-config-qualification
python3 -B deps/test-systemd-dbus.py \
  --payload "$PWD/bin/$(uname -m)/systemd" \
  --output /tmp/systemd-dbus-qualification
```

每个输出目录必须不存在。测试仅清理自己创建的临时容器/镜像并保留证据。配置测试通过 sysusers 创建 messagebus，应用 tmpfiles 和 preset，保留 rootfs vendor 服务，并验证管理员覆盖；D-Bus 测试检查 helper UID 限制、非 root 激活账号和私有前缀代码映射。PID 1、reexec、shutdown 和 E/S 仍由实际 Guest VM 集成验收；这些专项测试不能替代它们。

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
