[English](runtime-bundle.md) | [简体中文](runtime-bundle_zh.md)

# Runtime bundle：载荷、身份与兼容

本篇是 `sandbox-runtime.bundle` 及 Runtime 发行单元的交付契约，定义文件系统布局、PMEM 对齐、footer 身份、保留路径和兼容要求。消费选定 Runtime/Kernel 产物见[使用 guest-runtime](using-guest-runtime_zh.md)，生产与验证见[构建](build_zh.md)。

`sandbox-runtime.bundle` 是平台随每个 microVM 挂入的一份只读 Guest runtime
镜像。它包含 本仓构建出的 guest PID 1(`sandbox-init`)和平台在
guest 内需要的辅助工具,由本仓打包、发布,再由 `sandboxer/sandbox-ctl` 在
启动沙箱时作为 virtio-pmem 设备提供给 guest。

本文只定义镜像打包、文件布局、版本发布和消费契约。`sandbox-init` 的启动期
rootfs 组装、vsock 控制面、stdio MUX、exec/attach/quiesce 等 ABI 由
[Guest ABI 文档](guest-abi_zh.md)维护。

## 概述

### 职责边界

| 组件 | 职责 |
|---|---|
| `sandboxer` | `sandbox-ctl` 启动 CH,把 runtime 镜像作为 virtio-pmem 挂入 guest |
| `guest-runtime` | 构建 `sandbox-init`、`flatten-ctl`，并与 `envd` 打包为单一 EROFS 镜像 |
| `guest-runtime/native-deps` | 构建 `vmlinux`、`envd`;可选网关单独按需构建,不是 runtime payload |
| `accelerator` | 提供 `flatten-ctl` 复用的 `pkg/{flatten,image,remote,tar}` 和 manifest/cache/store 能力 |

`sandbox-runtime.bundle` 不包含用户 rootfs、用户依赖、guest kernel 或
cloud-hypervisor。用户 rootfs 来自 `boot.root.base`/`boot.disks[]`;guest
kernel 由 `vmlinux` 包发布;VMM 由 `sandboxer` 发布。

### 系统位置

```
                 build time                                      run time

  guest-runtime/bin/<arch>/sandbox-init ─┐
  guest-runtime/bin/<arch>/flatten-ctl├─► sandbox-runtime.bundle ──► sandbox-ctl
  native-deps/bin/<arch>/envd ────────┘             ▲                     │
                                                 │                     ▼
                                           runtime release          guest /sbin/init
```

这份镜像是节点级共享资产。同一节点上相同版本的 sandbox 通过 virtio-pmem +
DAX 映射同一份 host 文件,避免每个 sandbox 独立复制 runtime 文件页。

### 设计目标

- **单一镜像**:e2b 运行时和 builder 运行时合并为同一个 runtime 镜像。
- **只读且版本固定**:镜像由发布流程产生,运行期不修改。
- **跨实例共享**:通过 virtio-pmem + DAX 共享 host page cache。
- **边界清晰**:PID1、共享 ABI、镜像打包与 guest payload 归属 `guest-runtime`，Host 生命周期归属 `sandboxer`。
- **可独立发布**:runtime bundle 可随 `guest-runtime` 专用版本线单独发布并选择旧版本回滚。

## 镜像布局

镜像根目录包含:

```
/sbin/init                         sandbox-init
/proc/                             空挂载点
/sys/                              空挂载点
/dev/                              空挂载点
/overlay/lower/                    用户 base rootfs 挂载点
/overlay/upper/                    可写 ext4 / overlay upper 挂载点
/sysroot/                          switch-root 目标
/sysdisks/disk-{0..7}/              数据盘挂载点
/sysdisks/disk-{0..7}-lower/        数据盘 lower 挂载点
/sysdisks/disk-{0..7}-upper/        数据盘 upper 挂载点
/opt/sandbox-runtime/bin/envd       e2b guest agent
/opt/sandbox-runtime/bin/flatten-ctl
```

当前 Makefile 为序号 0–7 的数据盘预建 `/sysdisks/` 挂载点。除这些平台路径外,
镜像不提供 `/etc`、`/usr`、共享库或通用发行版环境。
`sandbox-init` 通过 Go syscall 完成早期挂载和 switch-root;用户应用真正看到
的 rootfs 来自用户镜像。`/opt/sandbox-runtime` 在 switch-root 前 bind 到
用户 rootfs 同名路径,应用可以只读访问平台工具,但不应把自己的文件放在这个
保留路径下。

### guest payload

| 文件 | 来源 | 用途 |
|---|---|---|
| `/sbin/init` | `bin/<arch>/sandbox-init` | guest PID 1,负责挂载、握手、应用监督 |
| `/opt/sandbox-runtime/bin/envd` | `native-deps/bin/<arch>/envd` | e2b 数据面 agent |
| `/opt/sandbox-runtime/bin/flatten-ctl` | `bin/<arch>/flatten-ctl` | build sandbox 内拉取/展平 OCI 镜像 |

`fsck.erofs` 是诊断/测试工具,不进入 runtime 镜像。`vmlinux` 不是 runtime
镜像内容,由 `vmlinux-<arch>-vX.Y.Z.tar.gz` 独立发布。

### host bundle

发布文件不是裸 EROFS,而是可直接作为 virtio-pmem backing 的 bundle:

```text
raw EROFS | zero padding | trailing ZIP
```

raw EROFS 保持从 offset 0 开始。尾部 ZIP 只包含一个 size=0 的 marker:

```text
.kuasar.digest.<64-lowercase-hex>
```

`digest:` identity 是 ZIP 之前全部字节（EROFS 与对齐 padding）的 SHA-256。
它不覆盖 ZIP，也不是完整 bundle 文件的 SHA-256。构建器计算此前缀的哈希后再写 marker。
运行和恢复检查 bundle/footer 格式并从 EOF 读取声明的身份，不重新计算前缀哈希。
这是身份比较，不是独立的内容或来源验证；部署时应采用可信、另行验证过的发布产物。
[发布载荷验证器](../scripts/release-runtime-payloads.py) 会重新计算前缀哈希并与 marker 比较。

bundle 最终大小保持 2 MiB 对齐，因此 Cloud Hypervisor 无需 offset 能力即可直接映射；
EROFS 依据自身 superblock 忽略尾部 padding 和 ZIP。

## 运行期消费契约

`sandbox-ctl` 把 `sandbox-runtime.bundle` 作为只读 virtio-pmem 设备传给
cloud-hypervisor。guest kernel 挂载该 pmem 后执行 `/sbin/init`,即 `sandbox-init`。

运行期关键约束:

- runtime 镜像只读,不能承载 per-sandbox 状态。
- Snapshot restore 要求匹配捕获的 runtime 内容身份：digest scheme 与 footer 声明的
  digest 必须一致。配置的文件只要身份相同，basename 可以不同；原 basename 仅用于
  定位唯一允许的同目录回退候选。仅 wire 兼容或发布标签相同并不足够。
  应钉住并保留所需产物的不可变内容；
  参见[恢复时的 runtime 选择契约](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/runtime_zh.md#restore)。
- 冷启动（包括从来源派生的 `run --from`）使用选定的目标 runtime，并在新 C0 中记录其
  身份，不要求来源 E 的旧 runtime 身份；Guest ABI 与 VMM 兼容要求仍然适用。
- `sandbox-init` 与 `sandbox-ctl` wire ABI 必须匹配。升级 runtime 前要同步升级
  `sandboxer` 或验证向后兼容。
- `/opt/sandbox-runtime` 是平台保留路径。用户镜像已有该路径时会被平台 bind mount 遮蔽。

`sandbox-runtime.bundle` 不参与 manifest key、API key 或 access token 派生。
密钥由 orchestrator/placer/provider 和 manifest 配置管理;runtime 镜像只携带执行工具。

## 发布件

runtime 镜像由本仓 `main` 上受信任的 `Runtime Release` workflow 从调度器钉住的
源码分支和精确 SHA 独立发布。组件 `main` 用于主线,`release/vX.Y.x` 用于 runtime
维护线;其版本与平台聚合版本、vmlinux 版本均独立:

| 包 | 内容 | Release |
|---|---|---|
| `sandbox-runtime-<arch>-vX.Y.Z.tar.gz` | runtime 镜像、`sandbox-init`、`flatten-ctl` | `guest-runtime` 仓 `runtime-vX.Y.Z` |

这里 `<arch>` 为所选版本实际声明和发布的 `x86_64` 或 `aarch64`。
不要仅凭源码构建支持推断另一架构资产存在，也不要将两个架构覆盖解包到同一 `bin/`。

runtime 专用包内同时放置:

```
bin/sandbox-runtime.bundle
bin/sandbox-init
bin/flatten-ctl
share/licenses/runtime/...
```

外层压缩包还在 `share/licenses/runtime/` 中携带必需的许可、来源及 Go 构建/依赖材料。
这些属于发布包，不是 runtime EROFS 镜像内的文件。

`sandbox-runtime.bundle` 是当前脚本、默认配置和外部分发共同使用的稳定入口。

聚合发布由项目主仓的 `release-vX.Y.Z` 承载,同时上传 platform 包、各独立
版本的原始组件包，使用 GitHub 原生资产大小和摘要校验。platform 包从所选 runtime tag 聚合本仓
runtime 文档与 `test/e2e/`,从独立选择的 vmlinux tag 取得内核文档对(`docs/vmlinux.md` 及该 tag 中存在的 `docs/vmlinux_zh.md`);组件包本身
不重复携带这些内容。把需要的包解到同一目录后，组件包在 `bin/` 提供产品工具，当前 platform 包在 `guide/` 提供文档、
在 `test/` 提供测试，并在 `workbench/` 提供轻量 Workbench 启动器。历史版本沿用其随包指南规定的布局。双语文档交付与二进制版本选择分别验收。

`vmlinux` 不属于 runtime 版本,由本仓 `vmlinux-vX.Y.Z` 独立版本线发布。
runtime workflow 从同一选定 guest-runtime commit 构建 sandbox-init 与 flatten-ctl。核实 Stable `runtime-vX.Y.Z` 的公开制品后，发布器在同一 commit 创建根 Go module tag `vX.Y.Z`；Preview 与 Kernel 发布不创建根 module tag。runtime 与 vmlinux
版本号独立演进。

`envd` 已内置在 runtime 镜像中,不作为独立 `bin/envd` 发布;
`fsck.erofs` 是源码树诊断/测试辅助工具,不进入通用组件包。

## 相关契约

- [sandbox-init](guest-abi_zh.md) - guest PID 1 ABI 和 host/guest 控制协议。
- [sandbox](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/runtime_zh.md) - runtime 镜像消费、启动与恢复。
- [Native Build](../native-deps/README_zh.md) - 宿主 EROFS 工具要求及 vmlinux、envd 构建。
- [vmlinux](vmlinux_zh.md) - guest kernel 与 runtime 镜像的配合关系。
- [flatten](flatten-cli_zh.md) - build sandbox 中的 flatten-ctl 执行模型。
- [聚合验证](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/test/QUICKSTART_zh.md) - 发布包解压和 E2E 入口。
