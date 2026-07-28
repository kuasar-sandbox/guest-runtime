# build — 原生依赖构建工作流

native-deps 目录的构建工作流:从上游源码构建 kuasar-sandbox 平台运行期消费、但各 Go 仓
不链接的三类原生产物——`mkfs.erofs`/`fsck.erofs`(erofs-utils)、`vmlinux`(guest 内核)、
`envd`(e2b guest agent)。工具链(autotools/kbuild/Go)与 Go 仓不同、冷构建以分钟计、上游发布节奏独立,因此内聚在
`guest-runtime/native-deps` 目录下独立构建。

所有产物走同一条流水线:按 URL pin 的上游 tarball(可选 SHA256 校验)→ 共享缓存与
解压 → 本地补丁(仅 vmlinux,`git am`)→ 构建 → `bin/<arch>/`。
本文覆盖构建目标、patch 开发循环、交叉编译与缓存/清理约定;产物本身的设计契约不在
本文:内核配置体系见 [`../../docs/vmlinux.md`](../../docs/vmlinux.md)。patched
`cloud-hypervisor` 是 `sandbox-ctl` 的 VMM 运行件,构建与 patch 契约见
`sandboxer/docs/cloud-hypervisor.md`。

平台级聚合由 `orchestrator/release-builder` 编排:
`make -C orchestrator/release-builder build` 首先驱动本目录 `make build`,再把
产物按 `orchestrator/release-builder/scripts/bin-inputs.manifest` 收集进
`orchestrator/release-builder/bin/<arch>/`,供 e2e、demo 与 release packaging
复用。

## 1. 概述

### 1.1 产物与版本 pin

| 产物 | 上游(pin) | 本仓输入 | 消费方 |
|---|---|---|---|
| `mkfs.erofs` `fsck.erofs` | erofs-utils v1.9.1 | — | `accelerator`(展平)、`guest-runtime`(打 guest erofs)、源码树诊断 / accelerator 测试 |
| `vmlinux` | linux 6.1.169(LTS,cdn.kernel.org) | `deps/linux-patches/`(1 个)+ `deps/vmlinux/*.config` | `sandboxer`/`sandbox-ctl`(guest 内核) |
| `envd` | e2b-dev/infra 2026.22(发布 tarball) | — | `guest-runtime`(注入 `sandbox-runtime.bundle`) |

pin 全部落在 Makefile 变量(`EROFS_TARBALL` / `LINUX_TARBALL` / `ENVD_TARBALL`,
支持 `url#filename` 与本地路径两种形式),配套的 `*_TARBALL_SHA256`
为空时跳过校验。升级版本 = 改变量 + 重验 patch 应用。

`librocksdb`(`accelerator` 的 CGO 链接依赖)在该仓内构建,不在此处。

### 1.2 目录布局

```
bin/
├── x86_64/                  x86_64 产物: mkfs.erofs fsck.erofs vmlinux
│                            envd
├── aarch64/                 aarch64 产物(同上)
└── <name>                   软链 → <host-arch>/<name>,仅原生构建时生成/更新

build/
├── tarball/                 上游 tarball 缓存(跨架构共享)
├── src/
│   ├── linux/               内核源树(跨架构共享;git 仓,patch 开发 WIP 所在)
│   └── e2b-infra/           envd 源树(跨架构共享,Go 以 GOARCH 选目标)
├── x86_64/
│   ├── linux/               kbuild O= 输出
│   └── src/erofs-utils/     erofs-utils 源树(autotools 仅支持 in-tree,per-arch)
└── aarch64/                 (同上)
```

软链接策略:host = target 的原生构建在 `bin/` 下生成 `bin/<name> → <arch>/<name>`,
以 `bin/` 为入口的消费方始终拿到当前架构可执行的二进制;交叉构建不更新软链
(避免把入口指向 host 上不可执行的产物)。

### 1.3 幂等与缓存

- **产物级跳过**:`bin/<arch>/` 下产物已存在时,erofs / vmlinux / envd 脚本直接退出
  (删除产物以强制重建)。
- **tarball 缓存**:`build/tarball/` 按文件名缓存,命中即不再下载;解压以
  `.extracted` marker 幂等。
- **`make clean`**:删除 `bin/` 与当前 `TARGET_ARCH` 的中间产物,**保留** tarball
  缓存与 `build/src/*` 源树——后者可能携带未 format 回 `deps/` 的 patch 开发
  WIP(git 仓)。

## 2. 构建目标

```bash
make build               # =all: vmlinux + erofs + envd
make erofs               # mkfs.erofs + fsck.erofs(最快,热缓存亚分钟)
make vmlinux             # guest 内核(冷 ~5-10 min)
make envd                # e2b guest agent(亚分钟)
make clean               # 见 §1.3
make help                # 列举目标
```

### 2.1 erofs(`make erofs`)

`deps/build-erofs.sh`:解压 erofs-utils 到 `build/<arch>/src/erofs-utils/`(autotools
不支持 out-of-source 构建,per-arch 各一棵)→ `autoreconf` + `configure`(关闭全部
压缩 / fuse / 网络特性)→ 只编 `lib` + `mkfs` + `fsck` 三个子目录 → 产出
`bin/<arch>/{mkfs.erofs,fsck.erofs}`。

- 跳过 `mount`/`dump`/`fuse` 子目录:v1.9.1 的 mount.erofs 在
  `--disable-multithreading` 下有 pthread 链接 bug,且平台不消费这些工具。
- configure 期硬依赖 libuuid(无 `--without-uuid` 出口);交叉编译需 multi-arch 的
  `uuid-dev:<arch>`,脚本前置探测并打印 apt 安装指引(§4.2)。
- host 构建依赖:`autoconf automake libtool pkg-config make gcc g++`。

### 2.2 vmlinux(`make vmlinux`)

= `linux-patches-apply` + `linux-build`,二者都进 `deps/build-vmlinux.sh`(STAGE
分阶段,§3):应用 `deps/linux-patches/*.patch` 后,把
`deps/vmlinux/sandbox-common.config` 与 `sandbox-<arch>.config` 拼接成
`arch/<kbuild_arch>/configs/sandbox_defconfig`,`make sandbox_defconfig` +
`make olddefconfig`(解析依赖闭包、暴露 silent regression)→
`make -j$(nproc) <target>` → 拷出 `bin/<arch>/vmlinux`。

- `olddefconfig` 后校验 DAX 必需项及 arch 专属关键项；Kconfig 静默丢弃请求项时
  构建立刻失败。
- `vmlinux` 的 Make 依赖包含构建脚本、common/arch 配置片段与 tracked kernel
  patches；任一输入变化都会重新求值配置并使用 Kbuild 增量重建。
- 产物格式:x86_64 为 ELF(kbuild target `vmlinux`),aarch64 为 PE Image
  (target `Image`,取 `arch/arm64/boot/Image`);文件名统一 `vmlinux`。
- 源树 `build/src/linux/` 跨架构共享(kbuild 以 `ARCH=` 选目标,输出进 per-arch
  `O=` 目录)。
- host 构建依赖:`bc bison flex make tar pkg-config gcc` + libelf 头(libelf-dev /
  elfutils-libelf-devel)+ libssl 头(libssl-dev / openssl-devel);后两者是 host 侧
  kbuild 工具(fixdep、sign-file 等)的依赖,不链入 vmlinux。
- 配置体系语义(两段拼接的契约、关键启用/禁用项)见
  [`../../docs/vmlinux.md`](../../docs/vmlinux.md) §2-§3。

### 2.3 envd(`make envd`)

`deps/build-envd.sh`:e2b-dev/infra 发布 tarball 解压到跨架构共享的
`build/src/e2b-infra/`,对 `packages/envd` 执行 `go build`(`GOWORK=off GOOS=linux
CGO_ENABLED=0 -trimpath -ldflags "-s -w"`)→ `bin/<arch>/envd`。GOARCH 即选目标
架构,交叉无需 C 工具链。

- 产物由 `guest-runtime` 的 `make sandbox-runtime` 注入
  `sandbox-runtime.bundle` 的 `/opt/sandbox-runtime/bin/envd`,作为 e2b profile
  guest 内的数据面 agent(端口 49983)。
- 工具链注意:envd 的 `go.mod` pin 较新的 Go(如 `go 1.26.3`),`GOTOOLCHAIN=auto`
  按需下载;该下载要求 GOSUMDB 开启——Go 拒绝在 `GOSUMDB=off` 下下载并运行工具链。
- 换 tag:覆盖 `ENVD_TARBALL`(`url#filename` 形式,filename 决定缓存名)。

## 3. patch 开发循环(vmlinux)

vmlinux 通过 `deps/build-vmlinux.sh` 的 STAGE 多阶段 dispatcher 维护补丁,
Makefile 暴露 `linux-*` 目标:

| STAGE | make 目标 | 行为 |
|---|---|---|
| `fetch` | `linux-fetch` | 解压 tarball → `git init` + 全量 import commit + 打 base tag(`linux-patches-base`);已有 base tag 则跳过;存在无 tag 的外来 git 树则拒绝(不覆盖 WIP) |
| `patches-apply` | `linux-patches-apply` | `git am deps/linux-patches/*.patch`;幂等与 sanity 语义见下 |
| `patches-format` | `linux-patches-format` | `git format-patch <base>..HEAD` 回写 `deps/linux-patches/`(先清旧 `*.patch`) |
| `build` | `linux-build` | 仅构建,不动 patch |

`linux-patches` 是 `linux-patches-apply` 的别名。开发流:

```bash
make linux-fetch                 # 一次性: 拉源码 + git tag linux-patches-base
cd build/src/linux               # 改源码 + git commit(每个 patch 一个 commit)
make linux-patches-format        # 提取 base..HEAD 回 deps/linux-patches/*.patch
make vmlinux                     # 重新 apply + build,验证可重复
```

`patches-apply` 的 sanity 检查,核心目标是**绝不静默覆盖开发中的改动**:

- 源树必须有 base tag,否则报错(指引先跑 `make {linux,ch}-fetch`);
- `HEAD == base`:`git am` 应用全部 patch;
- `HEAD = base + N` 且 N 等于 patch 数、commit subject 与 patch 文件逐一匹配:
  视为已应用,幂等跳过;
- 其他任何状态:报错并指引"先 `make linux-patches-format` 保存 WIP,再
  `git reset --hard <base-tag>`,后重跑"。

补丁本身 arch-neutral:`deps/linux-patches/` 仅触
`drivers/virtio/virtio_balloon.c`,两架构共用同一组、各自 defconfig 编译。

## 4. 交叉编译

### 4.1 TARGET_ARCH

| 取值 | 别名 | GOARCH | KERNEL_ARCH |
|---|---|---|---|
| `x86_64` | `amd64` | `amd64` | `x86_64` |
| `aarch64` | `arm64` | `arm64` | `arm64` |

默认取 `uname -m`;`HOST_ARCH != TARGET_ARCH` 时自动启用交叉编译,`CROSS_PREFIX`
自动推导为 `<target>-linux-gnu-`(可显式覆盖)。产物落 `bin/<target>/`,`bin/` 软链
不更新;交叉产物在 host 上不可执行,运行验证需 native host。

```bash
make TARGET_ARCH=aarch64 build              # vmlinux + erofs + envd
```

### 4.2 工具链准备(x86_64 host → aarch64 为例,反向对称)

```bash
# C 工具链(erofs-utils configure/链接 + kbuild CROSS_COMPILE)
apt install gcc-aarch64-linux-gnu g++-aarch64-linux-gnu

# erofs-utils 的 multi-arch libuuid(configure 期硬依赖)
sudo dpkg --add-architecture arm64
sudo apt update
sudo apt install libuuid1:arm64 uuid-dev:arm64
```

envd 是 `CGO_ENABLED=0` 的纯 Go 构建,GOARCH 即完成交叉,无须以上任何一项。各脚本对
缺失的工具链做前置探测(`${CROSS_PREFIX}gcc`、`uuid-dev:<arch>`),
报错并给出安装指引,而不是在构建中途吐一墙链接错误。

## 5. WSL2 注意

WSL2 的 `/mnt/<drive>/`(DrvFs)上每个小文件有 5-10 倍 I/O 开销,而内核源树 ~85K
文件,放在 DrvFs 上冷构建慢一个量级:

- **vmlinux**:Makefile 自动探测 WSL2 + DrvFs(内核版本带 microsoft 标记且 cwd 在
  `/mnt/` 下);此时若存在 `$HOME/linux-build/src` 目录,自动把 `LINUX_BUILD_SRC` /
  `LINUX_BUILD_OUT` 重定向到 `$HOME/linux-build/` 下(Linux-native 文件系统)。
## 6. See Also

- [`../../docs/vmlinux.md`](../../docs/vmlinux.md) —— guest 内核配置体系、架构差异与关键
  决策(随发布包)。
- `orchestrator/release-builder/Makefile` + `orchestrator/release-builder/scripts/release.sh`
  —— 平台级聚合构建与发布打包;本目录的运行期必需产物经
  `orchestrator/release-builder/scripts/bin-inputs.manifest` 收集进共享 `bin/`。
