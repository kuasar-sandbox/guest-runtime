[English](build.md) | [简体中文](build_zh.md)

# 构建与交付 Runtime、Kernel 产物

本篇给出两个独立发行单元的完整生产流程。明确选择独立开发、选定源码集成或发行复现，并始终使用匹配的编译器、依赖和输入二进制。[Runtime bundle](runtime-bundle_zh.md)与[Kernel](vmlinux_zh.md)定义交付字节和兼容；[Native 维护](../native-deps/README_zh.md)管理补丁开发工作树及清理规则。

除小节明确要求在 `native-deps/` 执行外，命令均从 `guest-runtime/` 仓库根运行。验证与打包期间保持所选源码集合和目标架构固定。

## 选择源码与依赖模式

| 模式 | Accelerator | sandbox-init |
| --- | --- | --- |
| 独立 CLI 开发 | `go.mod` 中可取得的版本。 | 仅构建 flatten-ctl 时不需要。 |
| 协同源码开发 | 选定 guest-runtime + accelerator Go workspace。 | 普通装配目标可以使用本地相邻源码构建。 |
| Runtime 发行复现 | 与 `ACCELERATOR_VERSION` 匹配的干净 checkout，由发行目标生成 workspace。 | 装配前验证精确 `SANDBOXER_VERSION` 发布二进制，并准备匹配源码声明。 |

本地 fallback 构建不能证明与发布依赖字节相同；发行依赖验证失败时，不能用旧开发二进制替代。

## 源码工作区

选定源码的 Go 构建通过 workspace 使用兄弟仓 `accelerator`;独立 `go.mod` 解析已发布版本。发布流程从选定 sandboxer 发布包安装 `sandbox-init`;本地开发也可以从兄弟仓 sandboxer 构建它:

```text
<workspace>/
├── guest-runtime/
├── accelerator/
└── sandboxer/
```

协调开发使用这些兄弟仓相互兼容的 `main` revision。复现已发布组合时,使用对应[项目聚合 Release](https://github.com/kuasar-sandbox/kuasar-sandbox/releases)精确选择的组件 Tag,不要分别选择各仓 GitHub Latest。完整六仓工作区见[项目 README](https://github.com/kuasar-sandbox/kuasar-sandbox)。

独立 `flatten-ctl` 构建解析 `go.mod` 中真实可获取的版本。协调 Preview 构建只使用选定的 `guest-runtime` 与 `accelerator` 源码模块；构建和测试前生成该闭包：

```bash
GOWORK="$(python3 ../kuasar-sandbox/ci/integration/go_workspace.py --root .. --owners guest-runtime)" || exit 1
export GOWORK
```

工作区验证 module path，不改写 `go.mod`/`go.sum`。Runtime 发布从指定 sandboxer Release 消费 `sandbox-init` 二进制；Sandboxer、Connector 不加入 Runtime Go 工作区。组装前将该程序安装到 `../sandboxer/bin/<arch>/sandbox-init`，或显式设置 `SANDBOX_INIT`。Envd、versitygw 保持独立 Go 模块构建；Kernel 使用 Native 输入，不加入 Go workspace。`go-erofs` 继续作为版本化外部依赖。

## 构建前置

本地构建使用环境提供的 Go, 继承 `GOROOT` 与 `GOTOOLCHAIN` 选择. Release 编排要求宿主 `PATH` 中已有支持 `api --slurp` 的 `gh`. Guest 原生产物使用选定源码与 recipe，宿主 EROFS 打包和验证工具由构建环境安装。

CI 在受信 preflight 中一次选择已发布并验证的 Workbench, 将不可变镜像身份与精确 framework revision 传给构建 Job. Runtime 与 VMLinux 分别使用原生 `ubuntu-24.04` x86_64 和 `ubuntu-24.04-arm` aarch64 Runner. 编译, 普通测试和打包通过共享 Workbench action 执行, 使用 Runner 的 ordinary UID, 只读根文件系统, 移除 capabilities, 私有 `/src`, `/build`, `/work` 和显式 CPU 预算. 发布凭据留在宿主. 保留已有按架构声明的测试范围及独立硬件门禁; 原生 ARM 构建不代表 ARM KVM 覆盖. Runtime 发布直接使用已验证 Workbench 的 EROFS reader, 不在宿主编译 helper.

构建 `sandbox-runtime.bundle` 需要**宿主架构**的 `mkfs.erofs` executable。在构建 Host 安装 `erofs-utils`,或设置:

```bash
BUILD_MKFS_EROFS=/path/to/host/mkfs.erofs make sandbox-runtime
```

Host packer 仅在构建时组装 runtime bundle，必须匹配宿主架构。`flatten-ctl` 内部使用纯 Go 的 `github.com/kuasar-sandbox/go-erofs` 库生成 EROFS；Guest 和 Runtime 发行包不再携带 `mkfs.erofs`。

其他前置和 Native source 位置见 [Native 构建与维护](../native-deps/README_zh.md)。

## 构建

```bash
GOWORK=off make native-deps                # VMLinux、Envd 及 Native 输入
make flatten-ctl                # OCI/directory -> deterministic EROFS builder
make build                      # 组装 sandbox-runtime.bundle
make sandbox-runtime            # 使用选定的 sandbox-init 二进制组装 Runtime
make build TARGET_ARCH=aarch64  # 在依赖支持的范围内交叉构建
```

`make sandbox-runtime` 使用:

- `../sandboxer/bin/<arch>/sandbox-init`;
- `native-deps/bin/<arch>/envd`;
- `bin/<arch>/flatten-ctl`;
- 作为 Host image packer 的 `BUILD_MKFS_EROFS`。

缺少 `sandbox-init`、Envd 或 `flatten-ctl` 时，Makefile 调用对应构建目标。宿主 `mkfs.erofs` 必须已在 `PATH` 中，或由 `BUILD_MKFS_EROFS` 指定；缺少时 Runtime 构建明确失败。项目不再维护 erofs-utils 补丁、编译或分发 mkfs。Workbench 或宿主可安装发行版的 erofs-utils，用于构建时打包和独立校验。

## 输出

| 输出 | Build target |
| --- | --- |
| `bin/<arch>/flatten-ctl` | `make flatten-ctl` |
| `bin/<arch>/sandbox-runtime.bundle` | `make sandbox-runtime` |
| `native-deps/bin/<arch>/vmlinux` | `make native-deps` |
| `native-deps/bin/<arch>/envd` | `make native-deps` |

## 装配 Runtime 镜像

常用入口:

```bash
make flatten-ctl                 # 构建目标架构 guest 内工具
make native-deps                 # 构建 vmlinux / envd
make sandbox-runtime             # 生成 bin/<arch>/sandbox-runtime.bundle
make build                       # 构建 flatten-ctl + sandbox-runtime
make build TARGET_ARCH=aarch64
```

执行 `make sandbox-runtime` 前必须已有**宿主可执行的 `mkfs.erofs`**。可安装宿主 erofs-utils，或通过 `BUILD_MKFS_EROFS` 指定宿主工具。项目不编译 mkfs；该工具仅打包 runtime bundle；目标 Guest 使用静态 `flatten-ctl` 内部的 Go 生成器，不再安装 `GUEST_MKFS_EROFS`。

目标的输入和构建顺序:

1. 解析并检查 `BUILD_MKFS_EROFS`:使用显式指定的工具或宿主 PATH 中预安装的工具。缺少宿主工具在组装镜像前即失败。
2. 若所选 `SANDBOX_INIT`(默认 `../sandboxer/bin/<arch>/sandbox-init`)不存在,
   委托 `make -C ../sandboxer sandbox-init`。
3. 使用目标架构 `ENVD`(默认 `native-deps/bin/<arch>/envd`),缺失时触发构建。
4. 使用本仓目标架构 `FLATTEN_CTL`(默认 `bin/<arch>/flatten-ctl`),缺失时触发构建。
5. 检查所选 `flatten-ctl` 的构建记录中有外部 go-erofs 模块；旧增量产物会明确失败，须先执行 `make flatten-ctl` 或选择匹配的二进制。组装 staging，使用宿主 `BUILD_MKFS_EROFS` 生成临时 runtime EROFS。
6. host `runtime-bundle` 工具复制 EROFS、补齐 PMEM 对齐、计算 SHA256,
   追加空 marker ZIP,原子发布为 `bin/<arch>/sandbox-runtime.bundle`。

宿主 EROFS 工具要求和 `envd` 构建流程见[Native Build 文档](../native-deps/README_zh.md)。
`sandbox-init` 实现与 ABI 见[sandboxer Guest ABI](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/guest-abi_zh.md)。

### 架构

runtime 镜像按 target arch 构建。构建目录文件名统一为 `sandbox-runtime.bundle`,
其 EROFS prefix 内的 `/sbin/init`、`envd`、`flatten-ctl`
必须都是同一 target arch 的可执行文件。

`TARGET_ARCH=amd64` 归一化为 `x86_64`;`TARGET_ARCH=arm64` 归一化为 `aarch64`。
交叉构建不更新 host 架构软链,避免把通用入口指向 host 不可执行的二进制。
宿主打包工具始终与这些目标架构 payload 分离。

## 构建内核

`make vmlinux` 在输出缺失或受跟踪的内核输入更新时依次执行 fetch、patches-apply 和 build。
显式 `linux-fetch`、`linux-patches-apply`、`linux-patches-format`、`linux-build` 目标通过
`native-deps/deps/build-vmlinux.sh` 的 `STAGE` 选择阶段。从仓库根目录使用
`make -C native-deps <target>`,或进入 native-deps 后使用 `make <target>`:

```
fetch         下载并校验 linux-6.1.169.tar.gz(支持 TARBALL_CACHE 共享缓存),
              解压到 build/src/linux/(跨 arch 共享同一棵源码树),git init +
              提交原始树 + 打 tag linux-patches-base
patches-apply git am deps/linux-patches/*.patch 到该树。
              空补丁集要求 HEAD == linux-patches-base。已应用的补丁幂等跳过;
              未导出或与 patch 不一致的工作不会被覆盖
build         把 sandbox-common.config + sandbox-<arch>.config 拼接成
              arch/<kbuild_arch>/configs/sandbox_defconfig → make
              sandbox_defconfig + make olddefconfig(关键:让 kbuild 解析依赖
              闭包并暴露 silent regression)→ make -j<jobs> <kbuild_target>
              (x86_64: vmlinux ELF;arm64: Image PE)→ cp 到 bin/<arch>/vmlinux
```

`jobs` 使用任务传入的正整数 `KUASAR_BUILD_JOBS`. 未设置任务预算时默认取 `nproc`; 容器 CPU quota 本身不会限制该值.

合成源码 import 的 author/committer 时间及应用补丁时的 committer 时间默认取 `SOURCE_DATE_EPOCH` (未设置时为零). 保留显式 Git 日期参数和补丁 author 日期. 这些默认值只作用于配方的 Git 子进程, 使 SCM 派生的内核版本字符串可复现, 不改变内核配置或开发者提交.

增量构建按输入决定:Native Makefile 跟踪 build/common 脚本、两份所选 fragment、patch
目录及受跟踪的 patch。输入变化后,已有 `bin/<arch>/vmlinux` 不会阻止重新构建。
显式 `linux-build` 每次重新解析配置,Kbuild 编译本身保持增量。删除输出或 clean 后再
`make vmlinux` 可强制重新生成。

**Patch 开发流**:在 native-deps 目录执行 `make linux-fetch`
拉源码并打 `linux-patches-base` tag → 在 `build/src/linux/` 改代码 +
`git commit` → `make linux-patches-format` 导出回 `deps/linux-patches/*.patch`
→ `make vmlinux` 重新应用 + 构建。幂等与 sanity 语义统一见
[Native 构建指南](../native-deps/README_zh.md) [配置体系](vmlinux_zh.md#配置体系);补丁 arch-neutral,x86_64 /
arm64 共用同一组。必须先保留尚未导出的工作,不能只为让构建继续而重置它。

### host 构建依赖

```
bc bison flex make tar pkg-config gcc
libelf-dev / elfutils-libelf-devel
libssl-dev  / openssl-devel
```

获取源码与 patch 开发还需要 Git 以及 Native 构建指南列出的下载/归档工具。
交叉构建额外需要 `gcc-aarch64-linux-gnu` / `gcc-x86-64-linux-gnu`(取决于方向)。

## 固定 Native 输入与包验证

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
输入摘要与许可证收集；路径覆盖不表示可以把不同源码字节标记为配置的版本。

Runtime Go 构建的 workspace 只包含本轮选定的 guest-runtime 和 accelerator 模块。`sandbox-init` 来自指定 sandboxer 发布二进制；sandboxer 不加入 Runtime Go workspace。`flatten-ctl` 必须标识 Linux `github.com/kuasar-sandbox/guest-runtime/cmd/flatten-ctl` 主入口及所选 amd64 或 arm64 目标。当次生产者检查自身干净的 Go VCS 元数据。正常二进制构建信息和具有版本、校验和的外部 go-erofs 依赖保留，不打包独立来源记录。

许可证收集分别解析 guest-runtime、所选 sandbox-init 二进制和 Envd 的 Go 编译器选择。
若这些目录选择不同的编译器版本,则按各载荷实际版本收集对应安装的 Go 及随附
依赖声明,不以父 module 上下文替代载荷自身的选择。Module 材料使用生效的替换
及匹配的 module 校验和,沿用正常 Go 缓存与路由。选定内部组件使用显式 workspace；Envd 保持隔离的 shared module 本地替换；官方包不支持其他无版本的第三方替换。这些材料收集器
不下载或认证编译器分发,独立验证也不要求安装与载荷相同版本的编译器。

许可证和声明分别放在 `share/licenses/runtime` 或 `share/licenses/vmlinux`。声明缺失、目录不可读、遍历不完整或不同输入冲突时收集失败。Kernel 保留所选 Linux 源码的 `COPYING` 和完整 `LICENSES` 树。不发布来源表或历史构建、测试记录。

验证只接受所选单元的载荷：Runtime 的 `bin/` 下为 `sandbox-runtime.bundle` 和 `flatten-ctl`；Kernel 为 `bin/vmlinux` 及运行身份文件 `bin/vmlinux.sha256`。验证精确路径、重复条目、类型、root 属主、权限和载荷完整性。其他单元的材料和无关目录在解包前被拒绝。Runtime 包不能替换独立选择的 Kernel，也不能改变无关安装根目录的权限。

Runtime 独立验证使用已安装在 `PATH` 中的可信主机 `fsck.erofs` 和
`dump.erofs`,不下载 EROFS 源码或现场编译读取器。读取器须支持镜像格式、
`fsck.erofs --extract` 及 `dump.erofs --path/--cat`。
验证检查 bundle 对齐和前缀摘要,不展开目录树地验证完整 EROFS 文件系统,只把
init、Envd、flatten-ctl 读入固定私有文件。镜像内属主/权限、Go main
身份和目标架构须一致。发布前，内嵌 init 与 Envd 必须分别等于本次选定的输入二进制；消费发布包不要求来源证明。内嵌
flatten-ctl 必须等于外部对应载荷。残留的内嵌或外部 mkfs 载荷会被拒绝。不执行任何归档可执行文件。Kernel 验证
不需要 EROFS 读取器、Runtime 载荷或依赖 checkout。

### 历史原生工具材料

携带 mkfs 的历史 Runtime 包及对应源码、许可和重新链接材料保持不变，使用其匹配的发行工具验证。当前包不再声明这些原生工具为载荷。旧制品仍可读取；重新构建当前 Runtime 使用 Go 生成器和宿主工具。

### 发布与验证边界

发布者在 Tag/Release 写入前检查本次项目构建输出，使用人类说明，验证上传资产大小和摘要，并拒绝覆盖已有不同字节。不发布来源/Preview 标记及独立 `SHA256SUMS`；源码选择和当次验收仅作为流水线输入。消费者使用 GitHub 原生资产完整性校验和离线载荷检查，不获取历史 Git 或 Actions 证明。许可证保留在包内。

`librocksdb`(`accelerator` 的 CGO 链接依赖)在该仓内构建,不在此处。

## Native 目标命令

以下 Native 目标从 `guest-runtime/native-deps/` 执行。仓库根目录的 `make native-deps` 委派到该目录，宿主镜像打包器由环境提供。

### 构建目标

```bash
make build      # =all: vmlinux + envd
make vmlinux    # guest 内核
make envd       # e2b guest agent
make versitygw  # 可选网关,不包含在 build 中
make clean      # 见 Native 源码幂等与缓存
make help       # 列举目标
```

构建耗时取决于主机、工具链及缓存状态,以上命令不承诺固定耗时。

Workbench 任务向配方传入正整数 `KUASAR_BUILD_JOBS`. 内核编译使用该预算代替宿主 CPU 数量; Envd 即使设置了 `ENVD_GOFLAGS`, 也通过 `go build -p` 显式传入预算. CPU quota 本身不会改变 `nproc` 报告的 CPU 数量.

#### 宿主 EROFS 工具

从宿主发行版安装 erofs-utils，或通过 `BUILD_MKFS_EROFS` 选择宿主可执行的 mkfs。交叉构建也使用宿主架构的打包器。`make erofs`、补丁版 Plan ABI、私有静态链接配方和相应缓存已移除。独立校验需要支持当前格式及 `--extract`、`--path`、`--cat` 的 fsck/dump；可信 CI 可构建未修改的上游读取工具，但不构建或导出 mkfs。

#### vmlinux(`make vmlinux`)

目标需要重建时依次进入 `deps/build-vmlinux.sh` 的 fetch、patches-apply、build
阶段([patch 开发循环(vmlinux)](../native-deps/README_zh.md#patch-开发循环vmlinux))。应用 `deps/linux-patches/*.patch` 后,把
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
  [Guest 内核规范](vmlinux_zh.md) [构建工作流](#构建内核)-[配置体系](vmlinux_zh.md#配置体系)。

#### envd(`make envd`)

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

#### 可选网关(`make versitygw`)

`deps/build-versitygw.sh` 按所选 Go 架构构建配置的网关源码,不包含在 `build` 中。
它供本地/单节点 `builder.files_storage` 在需要 S3-compatible 网关而非云对象存储时
选择。源码、hash 与源码目录变量分别为 `VERSITYGW_TARBALL`、
`VERSITYGW_TARBALL_SHA256` 和 `VERSITYGW_SRC`。

## 交叉编译

### TARGET_ARCH

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

### 交叉工具链准备

Kbuild 需要目标 C 工具链，例如在 x86_64 宿主安装 `gcc-aarch64-linux-gnu`。Envd 和 flatten-ctl 使用 Go 交叉构建。Runtime 打包器始终使用宿主 erofs-utils，不再需要为 EROFS 安装目标架构的静态 Libgcrypt、Libgpg-error 或 libuuid。

## WSL2 注意

内核源码树有大量小文件操作。WSL2 的 `/mnt/<drive>/`(DrvFs)可能增加文件系统访问
开销,这里不承诺固定的变慢倍数。

- **vmlinux**:Makefile 检查内核版本是否含 microsoft 标记且 cwd 是否在 `/mnt/` 下;
  此时若存在 `$HOME/linux-build/src` 目录,默认把 `LINUX_BUILD_SRC` /
  `LINUX_BUILD_OUT` 放到 `$HOME/linux-build/` 下,以便使用 Linux-native 文件系统。

## 精确复现发行依赖

`make release-runtime SANDBOXER_VERSION=<tag>` 同样通过已登录的 GitHub CLI 下载并校验该精确 sandboxer Release。装配与打包使用构建目录内同一个已安装二进制及编译器许可证；即使本地存在旧开发二进制，Release 缺失或校验失败也会在装配前停止。
本地 sandboxer 源码必须位于该精确 tag，以提供对应许可证；缺少 tag 或 HEAD 不符会在下载前失败。

`make release-runtime` 同时核对 `ACCELERATOR_VERSION` 与干净的相邻 accelerator checkout，并在编译前生成两个模块的 workspace。它重新编译 `flatten-ctl`，装配和打包使用同一输出，不受继承的 `GOWORK=off` 或旧 `FLATTEN_CTL` 影响。项目 checkout 不在 `../kuasar-sandbox` 时设置 `KUASAR_PLATFORM_DIR`。源码目录冲突或缺 Tag、缺模块会在构建前停止。

## Kernel source 与许可证

VMLinux Release 必须可追溯到公开 kernel source version、config、项目 patch set、toolchain 和 source commit。Kernel patch 与复制的 kernel material 保留上游 copyright 和 GPL 义务。项目原创 build script 不会重许可 Linux kernel。

Runtime bundle 可包含采用多种许可证的软件。Package/Native dependency input、notice、source availability 和 redistribution obligation 必须作为 Release contract 的一部分检视。不得向 guest image 添加 internal-only package、private CA、SSH host key、machine identity、production credential 或来源不可追溯的预构建 binary。

仓库级许可证边界见 [`LICENSE_SCOPE_zh.md`](../LICENSE_SCOPE_zh.md)。Native build 细节和 source location 见 [Native 构建与维护](../native-deps/README_zh.md)。

## 发行选择与验证边界

Runtime 与 VMLinux 组件 Release 从所选 source ref 和精确 commit 构建。Preview Release 是供开发和评估使用的 GitHub prerelease;mainline Stable 分别对每个发行单元协调。项目聚合 Release 始终选择精确 Tag,不依赖 GitHub Latest,随后通过项目级集成测试和已发布资产测试验证所选组合。

版本关系见[项目 Release 文档](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release_zh.md),可用的 Stable 聚合版本由 [GitHub Latest Release](https://github.com/kuasar-sandbox/kuasar-sandbox/releases/latest) 渠道解析。

## 选定 sandbox-init 与 helper 的来源

所选 owner 源码在 `release/helper-inputs.json` 显式声明编译型 E2E helper 清单。当前清单为空。后续 helper 及其构建配方由此 owner 声明维护，聚合打包自动发现，无需平台文件名清单。

Runtime 发布内嵌所选 sandboxer Release 中的 `sandbox-init`。安装前从所选 Tag 的完整 Release 包集合固定本机架构资产 ID，并校验 GitHub 原生大小与摘要。忽略历史正文 marker、校验和附件及来源旁车，只下载本机架构。校验归档路径、文件类型、所有权、权限与 ELF 架构，并按实际二进制中读取的编译器版本复制 Go 法律声明。重新编译同一提交可能改变 Go 构建元数据或工作区派生的默认值，因此源码重编译不能证明与已发布依赖逐字节一致。
