[English](README.md) | [简体中文](README_zh.md)

<a id="native-deps"></a>
# native-deps — 原生依赖构建与维护

native-deps 默认构建 `vmlinux`（Guest 内核）和 `envd`（E2B Guest agent）；`versitygw` 是可选目标。镜像导出使用链接在 `flatten-ctl` 内的 Go EROFS 库。Runtime bundle 打包使用宿主预安装的 mkfs，独立校验使用 fsck/dump。项目不再维护 erofs-utils 补丁或编译、分发 mkfs。

公共流水线:按 URL pin 的上游 tarball(可选 SHA256 校验)→ 共享缓存与
解压 → 本地补丁(vmlinux,`git am`)→ 构建 → `bin/<arch>/`。
本文覆盖构建目标、patch 开发循环、交叉编译与缓存/清理约定;产物本身的设计契约不在
本文:内核配置体系见 [Guest 内核规范](../docs/vmlinux_zh.md)。patched
`cloud-hypervisor` 是 `sandbox-ctl` 的 VMM 运行件,构建与 patch 契约见
[Cloud Hypervisor](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/cloud-hypervisor_zh.md)。

平台级聚合由项目主仓编排:
`make -C kuasar-sandbox build` 首先驱动本目录 `make build`,再把
产物按 `kuasar-sandbox/release/bin-inputs.manifest` 收集进
`kuasar-sandbox/bin/<arch>/`,供 e2e、demo 与本地集成复用。

## 1. 概述

### 1.1 产物与版本 pin

| 产物 | 上游(pin) | 本仓输入 | 消费方 |
|---|---|---|---|
| `vmlinux` | linux 6.1.169(LTS,cdn.kernel.org) | `deps/linux-patches/` + `deps/vmlinux/*.config` | `sandboxer`/`sandbox-ctl`(guest 内核) |
| `envd` | e2b-dev/runtime 2026.22(源码 tarball) | — | `guest-runtime`(注入 `sandbox-runtime.bundle`) |
| `versitygw`(可选) | versity/versitygw v1.5.0 | — | 按需用于本地/单节点 S3-compatible 文件存储集成 |

pin 落在 Makefile 变量(`LINUX_TARBALL` / `ENVD_TARBALL` 及可选
`VERSITYGW_TARBALL`,支持 `url#filename` 与本地路径两种形式),配套的
`*_TARBALL_SHA256` 为空时跳过校验。当前 Linux、Envd 默认输入配置了 hash;
可选网关的 hash 按部署验证策略补充。升级版本 = 改变量 + 重验 patch 应用。

先使用正常 Makefile 目标构建所选源码,再打包匹配的输出。在仓库根目录执行的
`make release-runtime` 依赖 `sandbox-runtime`,`make release-vmlinux`
只依赖 `vmlinux`。直接调用 `release.sh package` 会复用预构建文件,不重建
原生依赖、不替换源码 checkout、不重置缓存。Runtime 与 Kernel 保持独立构建、
打包及选择。

Runtime 包输入为 `bin/<arch>/{sandbox-runtime.bundle,flatten-ctl}`；bundle
预先由宿主 `mkfs.erofs` 构建，该工具不属于 Runtime 载荷。Kernel 使用
`native-deps/bin/<arch>/vmlinux`。已有 `RELEASE_BIN_DIR`、
`RELEASE_NATIVE_BIN_DIR` 可选择匹配输出目录。
`RELEASE_SANDBOXER_SOURCE_DIR`、`RELEASE_ACCELERATOR_SOURCE_DIR`、
`RELEASE_ENVD_SOURCE_DIR`、
`RELEASE_LINUX_SOURCE_DIR` 选择对应源码树;
`RELEASE_SANDBOX_INIT_BIN`、`RELEASE_ENVD_BIN` 选择与镜像匹配的 init 和 Envd
二进制,用于材料收集与镜像一致性检查。
源码版本、原生链接映射与这些产物应一并保留。原生 pin 变化需要同步更新配方、
摘要与来源记录;路径覆盖不表示可以把不同源码字节标记为配置的版本。

Runtime 源码构建使用 `GOWORK=off` 及最小 Accelerator/Sandboxer 依赖闭包。
内部记录只有在本地 Git Tag 指向所选 commit 时使用发行版本,否则使用
`git:<commit>`；未来目标 Tag 不必已经存在。`flatten-ctl` 必须是所选 amd64 或
arm64 目标上的 Linux `github.com/kuasar-sandbox/guest-runtime/cmd/flatten-ctl`
主入口，Go VCS 元数据为干净状态并匹配所选项目 commit，且包含具有版本和
校验和的外部 go-erofs 构建记录。包中请求的 Runtime 或 Kernel
版本与源码 commit 分别记录。

分别在 guest-runtime、sandboxer、Envd 的 module 目录记录 Go 编译器选择。
若这些目录选择不同的编译器版本,则按各载荷实际版本收集对应安装的 Go 及随附
依赖声明,不以父 module 上下文替代载荷自身的选择。Module 材料使用生效的替换
及匹配的 module 校验和,沿用正常 Go 缓存与路由。现有内部组件及 Envd shared
module 本地替换继续支持;官方包不支持其他无版本的第三方替换。这些材料收集器
不下载或认证编译器分发,独立验证也不要求安装与载荷相同版本的编译器。

材料隔离放在 `share/licenses/runtime`、`share/sources/runtime` 或独立的
`vmlinux` 命名空间。每个单元包含 `SOURCES.tsv`、`GO-BUILD-INFO.tsv`、
`GO-MODULES.tsv` 和 `MATERIALS.sha256`。声明缺失、子目录不可读、遍历不完整
或不同输入发生材料冲突时收集失败。项目、Envd、Linux、Go 依赖及内部/shared
module 记录必须指向各自的材料目录。Kernel 包含所选源码中的 Linux
`COPYING` 和完整 `LICENSES` 树;记录的 COPYING 摘要须与包内文件一致。

验证只接受所选单元的载荷:Runtime 的 `bin/` 下为
`sandbox-runtime.bundle`、`flatten-ctl`；Kernel 只有
`bin/vmlinux`。检查精确路径、重复条目、类型、root 属主、权限、清单、来源记录
相符性及校验和。其他单元的材料和无关目录在解包前被拒绝。Runtime 包不能替换
独立选择的 Kernel,也不能改变无关安装根目录的权限。

Runtime 独立验证使用已安装在 `PATH` 中的可信主机 `fsck.erofs` 和
`dump.erofs`,不下载 EROFS 源码或现场编译读取器。读取器须支持镜像格式、
`fsck.erofs --extract` 及 `dump.erofs --path/--cat`。
验证检查 bundle 对齐和前缀摘要,不展开目录树地验证完整 EROFS 文件系统,只把
init、Envd、flatten-ctl 读入固定私有文件。镜像内属主/权限、Go main
身份/目标及构建记录须一致；init 还须绑定所选 Sandboxer commit。内嵌
flatten-ctl 必须等于外部对应载荷。残留的内嵌或外部 mkfs 载荷会被拒绝。不执行任何归档可执行文件。Kernel 验证
不需要 EROFS 读取器、Runtime 载荷或依赖 checkout。

#### 历史原生工具材料

携带 mkfs 的历史 Runtime 包及对应源码、许可和重新链接材料保持不变，使用其匹配的发行工具验证。当前包不再声明这些原生工具为载荷。旧制品仍可读取；重新构建当前 Runtime 使用 Go 生成器和宿主工具。

#### 发布与验证边界

发布者在 Tag/Release 写入前将选定项目 `SOURCE_SHA` 传入验证器,采用 bundle
的 `release-notes.md` 正文,追加既有来源/Preview 标记。生产者提供的说明不得夹带
这些保留标记。源码选择、构建/发布
权限分离及拒绝替换已发布资产的要求保持不变。独立验证是离线包内检查,不获取
项目/依赖 Git 对象或 Go 模块,不与远端源码树比较许可正文。校验和及 VCS 记录
不能证明任意生产者身份,也不构成对不可信 CI 候选的隔离。这些材料服务于发行
检视,不构成法律认证。

`librocksdb`(`accelerator` 的 CGO 链接依赖)在该仓内构建,不在此处。

### 1.2 构建源码组织

| 路径 | 角色 |
|---|---|
| `deps/build-{vmlinux,envd}.sh` | 源码构建脚本(vmlinux 为 STAGE 多阶段 dispatcher) |
| `deps/build-versitygw.sh` | 可选网关构建 |
| `deps/common.sh` | tarball 下载 / 缓存 / 解压共享逻辑 |
| `deps/linux-patches/` | guest 内核补丁(arch-neutral) |
| `deps/vmlinux/*.config` | 内核 defconfig 片段(common + per-arch) |


### 1.3 目录布局

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

### 1.4 幂等与缓存

Envd 复用已存在的目标文件；内核根据脚本、配置和补丁执行增量 Kbuild。下载保留在 `build/tarball/` 并按预期摘要校验。`make clean` 删除 `bin/` 与目标构建输出，保留 tarball 缓存和架构无关源码树，尤其保留内核补丁开发中的工作。宿主 erofs-utils 由环境管理，不进入本项目的 native cache。

## 2. 构建目标

```bash
make build      # =all: vmlinux + envd
make vmlinux    # guest 内核
make envd       # e2b guest agent
make versitygw  # 可选网关,不包含在 build 中
make clean      # 见 §1.4
make help       # 列举目标
```

构建耗时取决于主机、工具链及缓存状态,以上命令不承诺固定耗时。

Workbench 任务向配方传入正整数 `KUASAR_BUILD_JOBS`. 内核编译使用该预算代替宿主 CPU 数量; Envd 即使设置了 `ENVD_GOFLAGS`, 也通过 `go build -p` 显式传入预算. CPU quota 本身不会改变 `nproc` 报告的 CPU 数量.

### 2.1 宿主 EROFS 工具

从宿主发行版安装 erofs-utils，或通过 `BUILD_MKFS_EROFS` 选择宿主可执行的 mkfs。交叉构建也使用宿主架构的打包器。`make erofs`、补丁版 Plan ABI、私有静态链接配方和相应缓存已移除。独立校验需要支持当前格式及 `--extract`、`--path`、`--cat` 的 fsck/dump；可信 CI 可构建未修改的上游读取工具，但不构建或导出 mkfs。

### 2.2 vmlinux(`make vmlinux`)

目标需要重建时依次进入 `deps/build-vmlinux.sh` 的 fetch、patches-apply、build
阶段(§3)。应用 `deps/linux-patches/*.patch` 后,把
`deps/vmlinux/sandbox-common.config` 与 `sandbox-<arch>.config` 拼接成
`arch/<kbuild_arch>/configs/sandbox_defconfig`,`make sandbox_defconfig` +
`make olddefconfig`(解析依赖闭包)→ `make -j<jobs> <target>` → 拷出
`bin/<arch>/vmlinux`. `jobs` 使用已设置的 `KUASAR_BUILD_JOBS`, 否则取 `nproc`.

- `olddefconfig` 后校验 DAX 必需项及 arch 专属关键项；Kconfig 静默丢弃请求的关键项时
  构建立刻失败。
- Make 依赖包含构建脚本、common/arch 配置片段与 tracked kernel
  patches；输入变化会重新求值配置并使用 Kbuild 增量重建。
- 产物格式:x86_64 为 ELF(kbuild target `vmlinux`),aarch64 为 PE Image
  (target `Image`,取 `arch/arm64/boot/Image`);文件名统一 `vmlinux`。
- 源树 `build/src/linux/` 跨架构共享(kbuild 以 `ARCH=` 选目标,输出进 per-arch `O=` 目录)。
- host 构建依赖:`bc bison flex make tar pkg-config gcc` + libelf 头(libelf-dev /
  elfutils-libelf-devel)+ libssl 头(libssl-dev / openssl-devel);后两者是 host 侧
  kbuild 工具(fixdep、sign-file 等)的依赖,不链入 vmlinux。
- 配置体系语义(两段拼接的契约、关键启用/禁用项)见
  [Guest 内核规范](../docs/vmlinux_zh.md) [构建工作流](../docs/vmlinux_zh.md#2-构建工作流)-[配置体系](../docs/vmlinux_zh.md#3-配置体系)。

### 2.3 envd(`make envd`)

`deps/build-envd.sh`:e2b-dev/runtime 源码 tarball 解压到跨架构共享的
`build/src/e2b-infra/`,对 `packages/envd` 执行 `go build`(`GOWORK=off GOOS=linux
CGO_ENABLED=0 -trimpath -ldflags "-s -w"`)→ `bin/<arch>/envd`。GOARCH 即选目标
架构,交叉无需 C 工具链。

- 产物由 `guest-runtime` 的 `make sandbox-runtime` 注入
  `sandbox-runtime.bundle` 的 `/opt/sandbox-runtime/bin/envd`,作为 e2b profile
  guest 内的数据面 agent(端口 49983)。
- 工具链注意:envd 的 `go.mod` 可能要求较新的 Go(如 `go 1.26.3`),`GOTOOLCHAIN=auto`
  按需下载;该下载要求 GOSUMDB 开启——Go 拒绝在 `GOSUMDB=off` 下下载并运行工具链。
- 换 tag:覆盖 `ENVD_TARBALL`(`url#filename` 形式,filename 决定缓存名)。

### 2.4 可选网关(`make versitygw`)

`deps/build-versitygw.sh` 按所选 Go 架构构建配置的网关源码,不包含在 `build` 中。
它供本地/单节点 `builder.files_storage` 在需要 S3-compatible 网关而非云对象存储时
选择。源码、hash 与源码目录变量分别为 `VERSITYGW_TARBALL`、
`VERSITYGW_TARBALL_SHA256` 和 `VERSITYGW_SRC`。

## 3. patch 开发循环(vmlinux)

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
- `HEAD == base`:`git am` 应用全部 patch;
- `HEAD = base + N` 且 N 等于 patch 数、commit subject 与 patch 文件逐一匹配:
  视为已应用,幂等跳过;
- 其他任何状态:报错并指引先用 `make linux-patches-format` 保存 WIP,确认已保存后
  才 reset 到 base tag,再重跑。

当前补丁 arch-neutral:`deps/linux-patches/` 触及
`drivers/virtio/virtio_balloon.c`,两架构共用同一组、各自 defconfig 编译。

## 4. 交叉编译

### 4.1 TARGET_ARCH

| 取值 | 别名 | GOARCH | KERNEL_ARCH |
|---|---|---|---|
| `x86_64` | `amd64` | `amd64` | `x86_64` |
| `aarch64` | `arm64` | `arm64` | `arm64` |

默认取 `uname -m`;`HOST_ARCH != TARGET_ARCH` 时自动启用交叉编译,`CROSS_PREFIX`
自动推导为 `<target>-linux-gnu-`,支持显式覆盖。产物落 `bin/<target>/`,`bin/` 软链
不更新;交叉产物运行验证需对应的 native host。

```bash
make TARGET_ARCH=aarch64 build
```

### 4.2 交叉工具链准备

Kbuild 需要目标 C 工具链，例如在 x86_64 宿主安装 `gcc-aarch64-linux-gnu`。Envd 和 flatten-ctl 使用 Go 交叉构建。Runtime 打包器始终使用宿主 erofs-utils，不再需要为 EROFS 安装目标架构的静态 Libgcrypt、Libgpg-error 或 libuuid。

## 5. WSL2 注意

内核源码树有大量小文件操作。WSL2 的 `/mnt/<drive>/`(DrvFs)可能增加文件系统访问
开销,这里不承诺固定的变慢倍数。

- **vmlinux**:Makefile 检查内核版本是否含 microsoft 标记且 cwd 是否在 `/mnt/` 下;
  此时若存在 `$HOME/linux-build/src` 目录,默认把 `LINUX_BUILD_SRC` /
  `LINUX_BUILD_OUT` 放到 `$HOME/linux-build/` 下,以便使用 Linux-native 文件系统。

## 6. See Also

- [Guest 内核规范](../docs/vmlinux_zh.md) —— guest 内核配置体系、架构差异与关键决策。
- [Release](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release_zh.md) —— runtime、vmlinux 独立版本与平台聚合发布;
  本目录运行期必需产物经 `kuasar-sandbox/release/bin-inputs.manifest` 收集进共享 `bin/`。
