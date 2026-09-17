[English](README.md) | [简体中文](README_zh.md)

# guest-runtime

`guest-runtime` 构建 [Kuasar Sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox) 使用的 **guest kernel、Runtime image 和 image build tool**。

本仓负责 guest 环境及其构建输入、provenance 与 Release artifact。Host 侧 MicroVM 生命周期、snapshot、restore 和 `sandbox-init` 源码属于 [`sandboxer`](https://github.com/kuasar-sandbox/sandboxer)。共享 Manifest、cache/store、OCI retrieval 和 image-flattening library 属于 [`accelerator`](https://github.com/kuasar-sandbox/accelerator)。

## 职责

- 从有文档记录的 kernel source、config 和 patch set 构建 guest VMLinux image;
- 构建 EROFS tool 和 Envd 等 Native guest 依赖;
- 构建 OCI/directory 到 EROFS 的 image builder `flatten-ctl`;
- 组装 `sandbox-runtime.bundle`,其中包括 `sandboxer` 提供的 `sandbox-init` 和本仓维护的 guest payload;
- 验证生成的 guest/Runtime 文件系统结构;
- 发布并记录两种独立版本的发行单元:Runtime 和 VMLinux。

本仓只对应一个组件,但发布两条发行单元版本线。

## 仓库布局

| 路径 | 用途 |
| --- | --- |
| `cmd/flatten-ctl` | 使用共享 `accelerator` package 将 OCI 或目录构建为 deterministic EROFS image |
| `native-deps/` | VMLinux、EROFS tool、Envd 和其他 Native guest 构建输入 |
| `docs/sandbox-runtime.md` | Runtime image 布局、guest payload、build 和 Release contract |
| `docs/vmlinux.md` | Guest kernel source/config contract 与 platform ABI |
| `docs/flatten.md` | `flatten-ctl`、remote image retrieval、cache 和 OCI Referrers 行为 |
| `scripts/guest-inspect.py` | 在兼容的 nonrandomized x86_64 布局中从 Host 读取 guest kernel memory counter |

Runtime image 将初始 guest tool 放在 `/opt/sandbox-runtime/bin/`。VMLinux 与 Cloud Hypervisor binary 不嵌入 Runtime bundle:VMLinux 作为独立发行单元发布,Cloud Hypervisor 由 `sandboxer` 构建和发布。

## 源码工作区

源码构建使用兄弟仓。`go.mod` 通过 `../accelerator` 解析 `accelerator`,Runtime image 通过 `../sandboxer` 使用或构建 `sandbox-init`:

```text
<workspace>/
├── guest-runtime/
├── accelerator/
└── sandboxer/
```

协调开发使用这些兄弟仓相互兼容的 `main` revision。复现已发布组合时,使用对应[项目聚合 Release](https://github.com/kuasar-sandbox/kuasar-sandbox/releases)精确选择的组件 Tag,不要分别选择各仓 GitHub Latest。完整六仓工作区见[项目 README](https://github.com/kuasar-sandbox/kuasar-sandbox)。

Go-only `flatten-ctl` 构建需要兄弟 `accelerator`。从源码构建 Runtime 还需要
`sandboxer` 提供 `sandbox-init`,以及下述 host/target Native tool。此最小源码闭包
使用 `GOWORK=off`:guest `sandbox-init` target 不导入 Connector,但共享 Go workspace
可能加载无关的 Host 侧 Sandboxer 依赖。完整 Host Sandboxer 构建仍需要 Connector;
Runtime 不将它新增为发行输入。独立 Kernel target 使用自己的 Native 构建输入,
不要求启动平台。内部 `require` 版本标识目标正式组件 Release,Daily Preview 后缀已
去除。这些 Tag 可以尚不存在:即使设置 `GOWORK=off`,本地 `replace` 仍选择兄弟仓
源码。报告 build/test 结果时记录实际 SHA。

## 构建前置

构建 `sandbox-runtime.bundle` 需要**宿主架构**的 `mkfs.erofs` executable。在构建 Host 安装 `erofs-utils`,或设置:

```bash
BUILD_MKFS_EROFS=/path/to/host/mkfs.erofs make sandbox-runtime
```

Host packer 与作为目标架构 static binary 复制进 guest Runtime image 的 `native-deps/bin/<target-arch>/mkfs.erofs` 不同。Cross-build 必须保持这一区分:aarch64 guest binary 不能在 x86_64 Host 上打包 image。

其他前置和 Native source 位置见 [Native 构建与维护](native-deps/README_zh.md)。

## 构建

```bash
GOWORK=off make native-deps                # VMLinux、目标 EROFS tools、Envd 及 Native 输入
GOWORK=off make flatten-ctl                # OCI/directory -> deterministic EROFS builder
GOWORK=off make build                      # 组装 sandbox-runtime.bundle
GOWORK=off make sandbox-runtime            # 构建 Runtime,按需构建 sandbox-init
GOWORK=off make build TARGET_ARCH=aarch64  # 在依赖支持的范围内交叉构建
```

`make sandbox-runtime` 使用:

- `../sandboxer/bin/<arch>/sandbox-init`;
- `native-deps/bin/<arch>/envd`;
- 作为 guest payload 的 `native-deps/bin/<arch>/mkfs.erofs`;
- `bin/<arch>/flatten-ctl`;
- 作为 Host image packer 的 `BUILD_MKFS_EROFS`。

缺少 `sandbox-init`、Envd 或 `flatten-ctl` 时，Makefile 会调用对应构建 target。每次普通 Runtime 构建在使用仓库管理的目标架构 guest `mkfs.erofs` 前也会检查 EROFS 配方，即使可执行文件已经存在。明确指定 `GUEST_MKFS_EROFS` 时直接使用该覆盖值，文件必须可执行。Host `mkfs.erofs` 不同:它必须已在 `PATH`、有文档记录的 native-deps Host 路径中,或由 `BUILD_MKFS_EROFS` 指定;否则 Runtime 构建明确失败。干净的公开构建必须使用有文档记录的公开 source/download location,不得依赖开发者私有 package mirror 或 cache。

## 输出

| 输出 | Build target |
| --- | --- |
| `bin/<arch>/flatten-ctl` | `make flatten-ctl` |
| `bin/<arch>/sandbox-runtime.bundle` | `make sandbox-runtime` |
| `native-deps/bin/<arch>/vmlinux` | `make native-deps` |
| `native-deps/bin/<arch>/mkfs.erofs` / `fsck.erofs` | `make native-deps` |
| `native-deps/bin/<arch>/envd` | `make native-deps` |

## 两个发行单元

本仓不发布通用的 `guest-runtime-vX.Y.Z` Release,而是维护两条独立版本线:

- **`runtime-vX.Y.Z`** - 发布 `sandbox-runtime-x86_64-vX.Y.Z.tar.gz`,包含 Runtime bundle、`flatten-ctl` 和 Release contract 选定的 EROFS creation tool;
- **`vmlinux-vX.Y.Z`** - 发布 `vmlinux-x86_64-vX.Y.Z.tar.gz`,在稳定路径 `bin/vmlinux` 包含 guest kernel。

两者版本号可以独立演进。项目聚合 Release 精确选择一个 Runtime Tag 和一个 VMLinux Tag,不假定版本号相同。

当前 GitHub 组件资产在组件 build/package 检查后从所选 source ref 和精确 commit 发布 Linux x86_64。项目聚合 Release 随后选择精确 Runtime、VMLinux 和其他组件 Tag,并对该组合执行跨组件集成测试及已发布资产的 MicroVM 验证。Source Makefile 支持其他 `TARGET_ARCH` 不等于已为该架构发布预构建 artifact。

Runtime 发布内嵌所选 sandboxer Release 中的 `sandbox-init`。安装该二进制前会校验 Release 的 Tag/源码、资产大小、GitHub 摘要及 `SHA256SUMS`；所选 sandboxer 源码仍用于来源和许可证校验。重新编译同一提交可能改变 Go 构建元数据或工作区派生的默认值，因此源码重编译不能证明与已发布依赖逐字节一致。

## Kernel source 与许可证

VMLinux Release 必须可追溯到公开 kernel source version、config、项目 patch set、toolchain 和 source commit。Kernel patch 与复制的 kernel material 保留上游 copyright 和 GPL 义务。项目原创 build script 不会重许可 Linux kernel。

Runtime bundle 可包含采用多种许可证的软件。Package/Native dependency input、notice、source availability 和 redistribution obligation 必须作为 Release contract 的一部分检视。不得向 guest image 添加 internal-only package、private CA、SSH host key、machine identity、production credential 或来源不可追溯的预构建 binary。

仓库级许可证边界见 [`LICENSE_SCOPE_zh.md`](LICENSE_SCOPE_zh.md)。Native build 细节和 source location 见 [Native 构建与维护](native-deps/README_zh.md)。

## 集成边界

- `sandboxer` 负责 `sandbox-init` 源码和 Host 生命周期;本仓将构建后的 guest binary 打进 Runtime image;
- `accelerator` 负责共享 EROFS/OCI flattening library;本仓在 Runtime 发行单元发布 `flatten-ctl` binary;
- `orchestrator` 通过完整平台使用生成的 Runtime 和 VMLinux artifact;
- `kuasar-sandbox/kuasar-sandbox` 选择精确发行单元,运行跨组件验证并发布聚合 Release。

## Release 模型

Runtime 与 VMLinux 组件 Release 从所选 source ref 和精确 commit 构建。Preview Release 是供开发和评估使用的 GitHub prerelease;mainline Stable 分别对每个发行单元协调。项目聚合 Release 始终选择精确 Tag,不依赖 GitHub Latest,随后通过项目级集成测试和已发布资产测试验证所选组合。

版本关系见[项目 Release 文档](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release_zh.md),可用的 Stable 聚合版本由 [GitHub Latest Release](https://github.com/kuasar-sandbox/kuasar-sandbox/releases/latest) 渠道解析。

## 文档

详细设计与参考文档均有完整英中配对。英文使用默认文件名,语言选择器打开完整中文版本:

- [`docs/sandbox-runtime_zh.md`](docs/sandbox-runtime_zh.md) - Runtime image 布局、guest payload、build、Release 和 consumption contract;
- [`docs/vmlinux_zh.md`](docs/vmlinux_zh.md) - guest kernel config、build、platform ABI 和 source relationship;
- [`docs/flatten_zh.md`](docs/flatten_zh.md) - `flatten-ctl`、deterministic flattening、remote retrieval、cache 和 OCI Referrers;
- [Native 构建与维护](native-deps/README_zh.md) - Native dependency source 与 build workflow。

按[文档策略](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/CONTRIBUTING_zh.md#文档贡献)同步维护成对文档。

## 贡献与安全

阅读[仓库贡献指南](CONTRIBUTING.md)和[组织贡献指南](https://github.com/kuasar-sandbox/.github/blob/main/CONTRIBUTING.md)。Guest ABI、Runtime content、kernel config、source provenance 或 Release artifact 的修改必须完成相应验证和必要的 companion PR。

不要在公开 Issue 报告漏洞,也不要公开 private package source、credential、signing material 或 customer data。按 [Kuasar Sandbox 安全策略](https://github.com/kuasar-sandbox/kuasar-sandbox/security/policy)使用 GitHub private vulnerability reporting。

## License

项目原创内容采用 [Apache License 2.0](LICENSE)。Linux kernel patch 保留 [`LICENSE_SCOPE_zh.md`](LICENSE_SCOPE_zh.md)记录的 GPL-2.0-only 边界。Runtime package、EROFS tool、Envd、Buildroot/distribution input 和其他第三方 material 保留各自 license、notice、source 与 redistribution obligation。
