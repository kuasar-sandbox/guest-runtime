[English](README.md) | [简体中文](README_zh.md)

<a id="build--原生依赖构建工作流"></a>
<a id="native-deps"></a>
# native-deps — 原生依赖构建与维护

native-deps 目录的构建工作流:从上游源码构建 kuasar-sandbox 平台运行期消费、但各 Go 仓
不链接的三类默认原生产物——`mkfs.erofs`/`fsck.erofs`(erofs-utils)、`vmlinux`(guest 内核)、
`envd`(e2b guest agent)。工具链(autotools/kbuild/Go)与 Go 仓不同、上游发布节奏独立,
因此内聚在 `guest-runtime/native-deps` 目录下独立构建。另提供可选的 `versitygw`
网关目标,不包含在默认 build 中。

公共流水线:按 URL pin 的上游 tarball(可选 SHA256 校验)→ 共享缓存与
解压 → 本地补丁(vmlinux,`git am`)→ 构建 → `bin/<arch>/`。
本文覆盖构建目标、patch 开发循环、交叉编译与缓存/清理约定;产物本身的设计契约不在
本文:内核配置体系见 [Guest 内核规范](../docs/vmlinux_zh.md)。patched
`cloud-hypervisor` 是 `sandbox-ctl` 的 VMM 运行件,构建与 patch 契约见
`sandboxer/docs/cloud-hypervisor.md`。

平台级聚合由项目主仓编排:
`make -C kuasar-sandbox build` 首先驱动本目录 `make build`,再把
产物按 `kuasar-sandbox/release/bin-inputs.manifest` 收集进
`kuasar-sandbox/bin/<arch>/`,供 e2e、demo 与本地集成复用。

## 1. 概述

<a id="产物与消费方"></a>
### 1.1 产物与版本 pin

| 产物 | 上游(pin) | 本仓输入 | 消费方 |
|---|---|---|---|
| `mkfs.erofs` `fsck.erofs` | erofs-utils v1.9.1 | — | `accelerator`(展平)、`guest-runtime`(打 guest erofs)、源码树诊断 / accelerator 测试 |
| `vmlinux` | linux 6.1.169(LTS,cdn.kernel.org) | `deps/linux-patches/` + `deps/vmlinux/*.config` | `sandboxer`/`sandbox-ctl`(guest 内核) |
| `envd` | e2b-dev/infra 2026.22(源码 tarball) | — | `guest-runtime`(注入 `sandbox-runtime.bundle`) |
| `versitygw`(可选) | versity/versitygw v1.5.0 | — | 按需用于本地/单节点 S3-compatible 文件存储集成 |

pin 落在 Makefile 变量(`EROFS_TARBALL` / `LINUX_TARBALL` / `ENVD_TARBALL` 及可选
`VERSITYGW_TARBALL`,支持 `url#filename` 与本地路径两种形式),配套的
`*_TARBALL_SHA256` 为空时跳过校验。当前 EROFS、Linux、Envd 默认输入配置了 hash;
可选网关的 hash 按部署验证策略补充。升级版本 = 改变量 + 重验 patch 应用。

这些覆盖项用于开发构建。Runtime 与 vmlinux 发行打包器只接受仓库 pin 且带已配置
摘要的 EROFS、Linux 和 Envd 输入(发行工作流可以从精确的公共镜像取得相同 Linux
字节)。发布其他原生输入时,必须同时更新仓库 pin、摘要与发行来源记录;否则打包会
失败,不会为不同输入生成错误的来源记录。

发行工作流在上传前把已完成归档的 SHA-256 记录为 build job output。发布者通过
`RELEASE_ARCHIVE_SHA256` 接收这一独立值,在任何 Tag/Release 写入前核对;不能用
下载后从 bundle 重新计算的值代替。即使重算 bundle 自身的校验和,全部载荷与材料
仍须匹配该次已完成构建。本地打包和独立验证不要求这个发布输入。该记录不证明
编译器来源,也不构成对不可信候选代码的隔离。

发行打包从所选 Git commit 的全新 checkout 出发,在本次临时兄弟工作区中调用现有组件
Makefile。Runtime 打包重建其最小 accelerator/sandboxer 依赖闭包、Envd、EROFS
工具和 Runtime image;Kernel 打包独立重建采用所选 patch 与 config 的 Linux。
仅复用通过 checksum 验证的下载 tarball。已有二进制、解压源码树和开发中的工作
保持不变。内部依赖记录只有在本地 Git Tag 指向所选 commit 时才保留该发行版本;
否则记录 `git:<commit>`,目标正式 Tag 尚不存在时也适用。
Runtime 验证 `flatten-ctl` 必须为本 module 的 Linux/amd64
`github.com/kuasar-sandbox/guest-runtime/cmd/flatten-ctl` 主入口,且 Go VCS 元数据
为干净状态并匹配所选项目 commit。该发行构建不使用缺少 Git 元数据的源码归档。

Runtime 与 Kernel 构建命令使用私有 home/缓存,不继承调用者的云/发布凭据或构建
flag 覆盖。可以保留无凭据的 HTTPS module/network 路由及配置的 checksum mirror。
`GOSUMDB`、`GOTOOLCHAIN` 仍为显式输入,默认分别是 `sum.golang.org`、`local`;
Runtime 验证要求启用 checksum database。Kernel 打包不获取 Go 分发材料。
Runtime 先解析现有的 checksum-pin Envd 源码,再分别在 guest-runtime、sandboxer、
Envd 的 module 目录记录编译器选择。如果显式自动选择产生不同的编译器版本,则
分别验证每个实际分发;不会把父目录的 Go 上下文当成所有 guest 二进制的证据。

发行打包记录全新构建上下文实际选定的 Go 编译器,在构建前后将其分发输入与匹配的
`golang.org/toolchain` 归档逐项比较;归档由配置的 checksum database 认证。这覆盖
编译器、标准库源码及该分发中的其他文件。完整 Go 安装中额外的非构建 `api`、
`doc`、`misc`、`test` 文件不在认证范围,也不作为发行许可来源;核对时处理标准的
`go.mod`/`_go.mod` 安装转换。Go 许可/NOTICE 正文来自已验证归档,包括编译器和
标准库内嵌依赖的材料,保留各自相对路径。独立验证还会
重新核对其字节、来源 URL 和 module h1。版本字符串或重算 bundle 校验和不能替代
来源核对。验证要求启用 checksum database 并取得匹配的归档/缓存;即使采用
`GOTOOLCHAIN=local`,也可能获取核验材料,但不切换构建编译器或静默启用工具链
自动选择。这些检查以可信构建主机为前提,不证明已失陷主机可信。

Kernel 发行构建固定 `KBUILD_BUILD_USER=kuasar`、`KBUILD_BUILD_HOST=release`
和 `KBUILD_BUILD_VERSION=1`,并根据 `SOURCE_DATE_EPOCH` 生成 UTC 格式的
`KBUILD_BUILD_TIMESTAMP`。epoch 默认值为 `0`,也用于归档时间戳。这样内核版本
元数据不会包含构建账号、主机名或构建时的时钟值。开发构建的 Kbuild 默认行为
不变;完整二进制一致仍要求相同的源码、配置和编译器输入。

验证器拒绝非 root 的归档数字属主,并核对 Envd、EROFS 和 Linux 的精确来源 URL
与摘要。发布时把选定的 `SOURCE_SHA` 传入验证器;Runtime 发布还必须提供精确的
accelerator/sandboxer `RELEASE_DEPENDENCIES` 绑定。重新生成校验和不能让不同的
项目 commit、依赖版本或原生来源通过该发布请求。本地源码打包仍可使用尚无 Tag
的依赖 commit,但不会把这些记录声称为已存在的发行版本。
项目、Envd、EROFS、Linux 和 Envd shared module 的必需来源记录还必须指向各自
的许可目录;即使另一个目录的材料有效,也不能用它替代当前来源的材料。

Runtime 打包还读取本次新构建的 `mkfs.erofs` 链接映射。对实际链入的系统静态库
或启动对象,记录文件摘要和已安装的 Debian 源包或 RPM 源包身份,并收集对应的
版权、许可和 NOTICE 文件,包括其引用的公共许可正文。这既覆盖 erofs-utils,
也覆盖 libc、libuuid、编译器运行库及启动对象;仅列包名或 SPDX 标识不能替代正文。
采集前,每个已安装输入都必须匹配可信构建主机 Debian 或 RPM 数据库中的文件
摘要。文件记录缺失、歧义或内容变更都会导致打包失败,仅有包归属不足以证明来源。
这检查已安装文件的完整性,不证明已失陷主机或包数据库可信。
收集的版权、许可和 NOTICE 正文字节也必须匹配其已安装包的文件摘要,所属源包
必须匹配链接输入。引用的 Debian common-license 正文按其自身所属包核验。
Multi-Arch 共同所有者必须全部认同文件字节与要求的源包身份;许可记录缺失、
内容变化或归属冲突时拒绝收集。

项目 CI 模板从 pin 且通过 checksum 验证的 util-linux 源码构建静态 libuuid。
provisioner 按构建身份保留 `SOURCES.tsv`、`MATERIALS.sha256` 和许可目录,
并随库文件复制到每个准备的 slot。打包同时验证材料清单与实际库摘要。
来源缺失、材料被改动或无法归属的原生输入会被拒绝;应更新可信模板,不能编造
来源记录。这些材料服务于发行检视,不构成法律认证。Kernel 源码选择仍独立于 Runtime。

`librocksdb`(`accelerator` 的 CGO 链接依赖)在该仓内构建,不在此处。

<a id="组成"></a>
### 1.2 构建源码组织

| 路径 | 角色 |
|---|---|
| `deps/build-{erofs,vmlinux,envd}.sh` | 源码构建脚本(vmlinux 为 STAGE 多阶段 dispatcher) |
| `deps/build-versitygw.sh` | 可选网关构建 |
| `deps/common.sh` | tarball 下载 / 缓存 / 解压共享逻辑 |
| `deps/linux-patches/` | guest 内核补丁(arch-neutral) |
| `deps/vmlinux/*.config` | 内核 defconfig 片段(common + per-arch) |


<a id="12-目录布局"></a>
### 1.3 目录布局

```
bin/
├── x86_64/                  x86_64 产物: mkfs.erofs fsck.erofs vmlinux envd
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
│   └── src/erofs-utils/     erofs-utils 源树(autotools in-tree,per-arch)
└── aarch64/                 (同上)
```

软链接策略:host = target 的原生构建为公共二进制入口生成 `bin/<name> → <arch>/<name>`;
交叉构建不更新 host 入口软链,避免指向 host 不可执行的产物。
可选网关仅在构建对应目标后才有输出。

<a id="13-幂等与缓存"></a>
### 1.4 幂等与缓存

- **产物复用**:erofs / envd 目标文件已存在时复用产物,需要强制重建时删除输出。
  内核输出还受 tracked 输入依赖控制:构建脚本、common/arch 配置或 patch 变化会重新
  求值配置并使用 Kbuild 增量重建,并非只检查产物存在就无条件跳过。
- **tarball 缓存**:`build/tarball/` 按文件名缓存,命中即不再下载;解压以
  `.extracted` marker 幂等。
- **`make clean`**:删除 `bin/` 与目标构建输出,**保留** tarball
  缓存与 `build/src/*` 源树。内核源树可能携带尚未 format 回 `deps/` 的 patch
  开发 WIP,不能静默丢弃。

<a id="构建"></a>
## 2. 构建目标

```bash
make build      # =all: vmlinux + erofs + envd
make erofs      # mkfs.erofs + fsck.erofs
make vmlinux    # guest 内核
make envd       # e2b guest agent
make versitygw  # 可选网关,不包含在 build 中
make clean      # 见 §1.4
make help       # 列举目标
```

构建耗时取决于主机、工具链及缓存状态,以上命令不承诺固定耗时。

### 2.1 erofs(`make erofs`)

`deps/build-erofs.sh`:解压 erofs-utils 到 `build/<arch>/src/erofs-utils/`(该 autotools
路径不支持 out-of-source 构建,per-arch 各一棵)→ `autoreconf` + `configure`(关闭
压缩 / fuse / 网络特性)→ 只编 `lib` + `mkfs` + `fsck` 三个子目录 → 产出
`bin/<arch>/{mkfs.erofs,fsck.erofs}`。

- 跳过 `mount`/`dump`/`fuse` 子目录:平台不消费这些工具,同时避开 v1.9.1 的 mount.erofs
  在 `--disable-multithreading` 下的 pthread 链接问题。
- configure 期硬依赖 libuuid(无 `--without-uuid` 出口);交叉编译需 multi-arch 的
  `uuid-dev:<arch>`,脚本前置探测并打印 apt 安装指引(§4.2)。
- host 构建依赖:`autoconf automake libtool pkg-config make gcc g++`。

### 2.2 vmlinux(`make vmlinux`)

目标需要重建时依次进入 `deps/build-vmlinux.sh` 的 fetch、patches-apply、build
阶段(§3)。应用 `deps/linux-patches/*.patch` 后,把
`deps/vmlinux/sandbox-common.config` 与 `sandbox-<arch>.config` 拼接成
`arch/<kbuild_arch>/configs/sandbox_defconfig`,`make sandbox_defconfig` +
`make olddefconfig`(解析依赖闭包)→ `make -j$(nproc) <target>` → 拷出
`bin/<arch>/vmlinux`。

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

`deps/build-envd.sh`:e2b-dev/infra 源码 tarball 解压到跨架构共享的
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

### 4.2 工具链准备(x86_64 host → aarch64 为例,反向对称)

```bash
# C 工具链(erofs-utils configure/链接 + kbuild CROSS_COMPILE)
apt install gcc-aarch64-linux-gnu g++-aarch64-linux-gnu

# erofs-utils 的 multi-arch libuuid(configure 期硬依赖)
sudo dpkg --add-architecture arm64
sudo apt update
sudo apt install libuuid1:arm64 uuid-dev:arm64
```

envd 是 `CGO_ENABLED=0` 的纯 Go 构建,GOARCH 即完成交叉,无须以上 C 包。各脚本对
缺失的工具链做前置探测(`${CROSS_PREFIX}gcc`、`uuid-dev:<arch>`),
报错并给出安装指引,而不是在构建中途产生大量链接错误。

## 5. WSL2 注意

内核源码树有大量小文件操作。WSL2 的 `/mnt/<drive>/`(DrvFs)可能增加文件系统访问
开销,这里不承诺固定的变慢倍数。

- **vmlinux**:Makefile 检查内核版本是否含 microsoft 标记且 cwd 是否在 `/mnt/` 下;
  此时若存在 `$HOME/linux-build/src` 目录,默认把 `LINUX_BUILD_SRC` /
  `LINUX_BUILD_OUT` 放到 `$HOME/linux-build/` 下,以便使用 Linux-native 文件系统。

<a id="文档"></a>
## 6. See Also

- [Guest 内核规范](../docs/vmlinux_zh.md) —— guest 内核配置体系、架构差异与关键决策。
- `kuasar-sandbox/docs/release.md` —— runtime、vmlinux 独立版本与平台聚合发布;
  本目录运行期必需产物经 `kuasar-sandbox/release/bin-inputs.manifest` 收集进共享 `bin/`。
