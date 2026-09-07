[English](sandbox-runtime.md) | [简体中文](sandbox-runtime_zh.md)

# sandbox-runtime — guest runtime 镜像

`sandbox-runtime.bundle` 是平台随每个 microVM 挂入的一份只读 Guest runtime
镜像。它包含 `sandboxer` 构建出的 guest PID 1(`sandbox-init`)和平台在
guest 内需要的辅助工具,由本仓打包、发布,再由 `sandboxer/sandbox-ctl` 在
启动沙箱时作为 virtio-pmem 设备提供给 guest。

本文只定义镜像打包、文件布局、版本发布和消费契约。`sandbox-init` 的启动期
rootfs 组装、vsock 控制面、stdio MUX、exec/attach/quiesce 等 ABI 由
[sandboxer Guest ABI 文档](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init_zh.md)维护。

## 1. 概述

### 1.1 职责边界

| 组件 | 职责 |
|---|---|
| `sandboxer` | 构建 `sandbox-init`;`sandbox-ctl` 启动 CH,把 runtime 镜像作为 virtio-pmem 挂入 guest |
| `guest-runtime` | 打包 `sandbox-init`、`envd`、`flatten-ctl`、`mkfs.erofs` 为单一 EROFS 镜像 |
| `guest-runtime/native-deps` | 构建 `mkfs.erofs`、`fsck.erofs`、`vmlinux`、`envd`;可选网关单独按需构建,不是 runtime payload |
| `accelerator` | 提供 `flatten-ctl` 复用的 `pkg/{flatten,image,remote,tar}` 和 manifest/cache/store 能力 |

`sandbox-runtime.bundle` 不包含用户 rootfs、用户依赖、guest kernel 或
cloud-hypervisor。用户 rootfs 来自 `boot.root.base`/`boot.disks[]`;guest
kernel 由 `vmlinux` 包发布;VMM 由 `sandboxer` 发布。

### 1.2 系统位置

```
                 build time                                      run time

  sandboxer/bin/<arch>/sandbox-init ─┐
  guest-runtime/bin/<arch>/flatten-ctl├─► sandbox-runtime.bundle ──► sandbox-ctl
  native-deps/bin/<arch>/envd ────────┤          ▲                     │
  native-deps/bin/<arch>/mkfs.erofs ──┘          │                     │ virtio-pmem+DAX
                                                 │                     ▼
                                           runtime release          guest /sbin/init
```

这份镜像是节点级共享资产。同一节点上相同版本的 sandbox 通过 virtio-pmem +
DAX 映射同一份 host 文件,避免每个 sandbox 独立复制 runtime 文件页。

### 1.3 设计目标

- **单一镜像**:e2b 运行时和 builder 运行时合并为同一个 runtime 镜像。
- **只读且版本固定**:镜像由发布流程产生,运行期不修改。
- **跨实例共享**:通过 virtio-pmem + DAX 共享 host page cache。
- **边界清晰**:PID1 协议在 `sandboxer`,镜像打包和 guest payload 在 `guest-runtime`。
- **可独立发布**:runtime bundle 可随 `guest-runtime` 专用版本线单独发布并选择旧版本回滚。

## 2. 镜像布局

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
/opt/sandbox-runtime/bin/mkfs.erofs
```

当前 Makefile 为序号 0–7 的数据盘预建 `/sysdisks/` 挂载点。除这些平台路径外,
镜像不提供 `/etc`、`/usr`、共享库或通用发行版环境。
`sandbox-init` 通过 Go syscall 完成早期挂载和 switch-root;用户应用真正看到
的 rootfs 来自用户镜像。`/opt/sandbox-runtime` 在 switch-root 前 bind 到
用户 rootfs 同名路径,应用可以只读访问平台工具,但不应把自己的文件放在这个
保留路径下。

### 2.1 guest payload

| 文件 | 来源 | 用途 |
|---|---|---|
| `/sbin/init` | `../sandboxer/bin/<arch>/sandbox-init` | guest PID 1,负责挂载、握手、应用监督 |
| `/opt/sandbox-runtime/bin/envd` | `native-deps/bin/<arch>/envd` | e2b 数据面 agent |
| `/opt/sandbox-runtime/bin/flatten-ctl` | `bin/<arch>/flatten-ctl` | build sandbox 内拉取/展平 OCI 镜像 |
| `/opt/sandbox-runtime/bin/mkfs.erofs` | `native-deps/bin/<arch>/mkfs.erofs` | build sandbox 内生成 EROFS base 镜像 |

`fsck.erofs` 是诊断/测试工具,不进入 runtime 镜像。`vmlinux` 不是 runtime
镜像内容,由 `vmlinux-x86_64-vX.Y.Z.tar.gz` 独立发布。

### 2.2 host bundle

发布文件不是裸 EROFS,而是可直接作为 virtio-pmem backing 的 bundle:

```text
raw EROFS | zero padding | trailing ZIP
```

raw EROFS 保持从 offset 0 开始。尾部 ZIP 只包含一个 size=0 的 marker:

```text
.kuasar.digest.<64-lowercase-hex>
```

`digest:` identity 覆盖 ZIP 之前的全部字节,即 EROFS 和对齐 padding。构建器复制 EROFS 的
同时计算 carrier identity,再写 marker;运行和恢复只从 EOF 读取 marker,不重新扫描
EROFS。bundle 最终大小保持 2 MiB 对齐,因此 Cloud Hypervisor 无需 offset
能力即可继续直接映射,EROFS 依据自身 superblock 忽略尾部 padding 和 ZIP。

## 3. 构建

常用入口:

```bash
make flatten-ctl                 # 构建目标架构 guest 内工具
make native-deps                 # 构建 mkfs.erofs / fsck.erofs / vmlinux / envd
make sandbox-runtime             # 生成 bin/<arch>/sandbox-runtime.bundle
make build                       # 构建 flatten-ctl + sandbox-runtime
make build TARGET_ARCH=aarch64
```

执行 `make sandbox-runtime` 前必须已有**宿主可执行的 `mkfs.erofs`**。可安装 erofs-utils,
先执行 `make -C native-deps erofs TARGET_ARCH=$(uname -m)` 构建宿主版本,或设置
`BUILD_MKFS_EROFS` 为已有宿主工具。它与被打入镜像的目标架构 `GUEST_MKFS_EROFS`
不同;交叉构建不能在宿主直接执行 guest 工具。

目标的输入和构建顺序:

1. 解析并检查 `BUILD_MKFS_EROFS`:先查 host PATH,host/target 相同时再查目标输出,
   最后查 native-deps 的 host 入口。缺少宿主工具在组装镜像前即失败。
2. 若所选 `SANDBOX_INIT`(默认 `../sandboxer/bin/<arch>/sandbox-init`)不存在,
   委托 `make -C ../sandboxer sandbox-init`。
3. 使用目标架构 `ENVD`(默认 `native-deps/bin/<arch>/envd`),缺失时触发构建。
4. 使用本仓目标架构 `FLATTEN_CTL`(默认 `bin/<arch>/flatten-ctl`),缺失时触发构建。
5. 使用目标架构 `GUEST_MKFS_EROFS`(默认 `native-deps/bin/<arch>/mkfs.erofs`),缺失时
   触发 erofs 构建。组装 staging 后,使用另行解析的宿主 `BUILD_MKFS_EROFS` 生成临时 raw EROFS。
6. host `runtime-bundle` 工具复制 EROFS、补齐 PMEM 对齐、计算 SHA256,
   追加空 marker ZIP,原子发布为 `bin/<arch>/sandbox-runtime.bundle`。

`mkfs.erofs` 和 `envd` 构建流程见[Native Build 文档](../native-deps/docs/build_zh.md)。
`sandbox-init` 实现与 ABI 见[sandboxer Guest ABI](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init_zh.md)。

### 3.1 架构

runtime 镜像按 target arch 构建。构建目录文件名统一为 `sandbox-runtime.bundle`,
其 EROFS prefix 内的 `/sbin/init`、`envd`、`flatten-ctl`、`mkfs.erofs`
必须都是同一 target arch 的可执行文件。

`TARGET_ARCH=amd64` 归一化为 `x86_64`;`TARGET_ARCH=arm64` 归一化为 `aarch64`。
交叉构建不更新 host 架构软链,避免把通用入口指向 host 不可执行的二进制。
宿主打包工具始终与这些目标架构 payload 分离。

## 4. 运行期消费契约

`sandbox-ctl` 把 `sandbox-runtime.bundle` 作为只读 virtio-pmem 设备传给
cloud-hypervisor。guest kernel 挂载该 pmem 后执行 `/sbin/init`,即 `sandbox-init`。

运行期关键约束:

- runtime 镜像只读,不能承载 per-sandbox 状态。
- 同一 snapshot restore 必须使用兼容 runtime 镜像;生产应按 digest 或发布版本钉住。
- `sandbox-init` 与 `sandbox-ctl` wire ABI 必须匹配。升级 runtime 前要同步升级
  `sandboxer` 或验证向后兼容。
- `/opt/sandbox-runtime` 是平台保留路径。用户镜像已有该路径时会被平台 bind mount 遮蔽。

`sandbox-runtime.bundle` 不参与 manifest key、API key 或 access token 派生。
密钥由 orchestrator/placer/provider 和 manifest 配置管理;runtime 镜像只携带执行工具。

## 5. 发布件

runtime 镜像由本仓 `main` 上受信任的 `Runtime Release` workflow 从调度器钉住的
源码分支和精确 SHA 独立发布。组件 `main` 用于主线,`release/vX.Y.x` 用于 runtime
维护线;其版本与平台聚合版本、vmlinux 版本均独立:

| 包 | 内容 | Release |
|---|---|---|
| `sandbox-runtime-x86_64-vX.Y.Z.tar.gz` | runtime 镜像、`flatten-ctl`、`mkfs.erofs` | `guest-runtime` 仓 `runtime-vX.Y.Z` |

runtime 专用包内同时放置:

```
bin/sandbox-runtime.bundle
bin/flatten-ctl
bin/mkfs.erofs
```

`sandbox-runtime.bundle` 是当前脚本、默认配置和外部分发共同使用的稳定入口。

聚合发布由项目主仓的 `release-vX.Y.Z` 承载,同时上传 platform 包、各独立
版本的原始组件包和聚合 `SHA256SUMS`。platform 包从所选 runtime tag 聚合本仓
runtime 文档与 `test/e2e/`,从独立选择的 vmlinux tag 取得内核文档对(`docs/vmlinux.md` 及该 tag 中存在的 `docs/vmlinux_zh.md`);组件包本身
不重复携带这些内容。用户把需要的包解到同一目录即可得到共享 `bin/`、`docs/`、
`test/`、`deploy/` 布局。双语文档交付与二进制版本选择分别验收。

`vmlinux` 不属于 runtime 版本,由本仓 `vmlinux-vX.Y.Z` 独立版本线发布。
runtime workflow 显式选择已发布 sandboxer tag 构建镜像;runtime 与 vmlinux
版本号独立演进。

`envd` 已内置在 runtime 镜像中,不作为独立 `bin/envd` 发布;
`fsck.erofs` 是源码树诊断/测试辅助工具,不进入通用组件包。

## 6. 可靠性与升级

### 6.1 节点升级

节点可同时保留多份 runtime 镜像,例如:

```
/opt/sandbox/runtime/v0.1.0/sandbox-runtime.bundle
/opt/sandbox/runtime/v0.2.0/sandbox-runtime.bundle
```

新沙箱使用新版本;运行中沙箱继续持有启动时的 pmem 文件。删除旧版本前必须
确认没有运行中 VM 或待恢复 snapshot 依赖它。

### 6.2 整机重启

整机重启后沙箱不自动恢复。节点重新启动时需要 runtime 镜像文件仍在部署目录,
供后续新沙箱创建使用。

### 6.3 回滚

回滚 = 配置重新指向旧 runtime 文件 + 重启 `node-ctl` 或让调度层停止向该节点
放置新沙箱。已运行沙箱不受新配置影响。

## 7. 排错

| 现象 | 检查项 |
|---|---|
| guest 无法启动 `/sbin/init` | 确认 runtime 镜像 arch 与 vmlinux/CH 目标 arch 一致 |
| build sandbox 找不到 `flatten-ctl` | 检查 `/opt/sandbox-runtime/bin/flatten-ctl` 是否进入镜像 |
| build sandbox 无法生成 EROFS | 检查 guest `/opt/sandbox-runtime/bin/mkfs.erofs` 和权限 |
| host 无法打包交叉架构镜像 | 分别检查 host `BUILD_MKFS_EROFS` 与 target `GUEST_MKFS_EROFS` |
| restore 后行为异常 | 检查 snapshot 使用的 runtime digest 与 restore 配置是否匹配 |
| 解包后脚本找不到 runtime | 确认已解压 runtime archive,且 `bin/sandbox-runtime.bundle` 存在 |

## 8. See Also

- [sandbox-init](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init_zh.md) - guest PID 1 ABI 和 host/guest 控制协议。
- [sandbox](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox_zh.md) - runtime 镜像消费、启动与恢复。
- [Native Build](../native-deps/docs/build_zh.md) - mkfs.erofs、vmlinux、envd 构建。
- [vmlinux](vmlinux_zh.md) - guest kernel 与 runtime 镜像的配合关系。
- [flatten](flatten_zh.md) - build sandbox 中的 flatten-ctl 执行模型。
- [聚合验证](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/test/QUICKSTART_zh.md) - 发布包解压和 E2E 入口。
