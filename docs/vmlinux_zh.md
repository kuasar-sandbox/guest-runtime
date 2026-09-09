[English](vmlinux.md) | [简体中文](vmlinux_zh.md)

# vmlinux — guest 内核镜像

平台为每个 sandbox 提供一份**最小化、固定版本**的 Linux 内核镜像
(`bin/<arch>/vmlinux`).本文档定义这份镜像的构建方式、启用/禁用的功能集合,
以及这些选择对启动、设备、资源控制和 Guest 安全边界的影响.

vmlinux 是平台资产,**不**向租户提供通用内核版本/配置接口。部署需要不同 kernel 时,
可通过 `boot.kernel: file://...` 选择自带 vmlinux,仍由同一内核路径入口传给
Cloud Hypervisor。必须用选定的 sandboxer/runtime/VMM 组合验证自带内核的启动协议、
必需设备、Guest ABI 与快照兼容性;平台不保证任意自带内核的功能或兼容性。

## 1. 概述

### 1.1 设计目标

| 目标 | 实现方式 |
|---|---|
| 镜像小、启动快 | 稀疏平台 defconfig 显式保留所需子系统,由 Kbuild 解析;HZ=100;最小控制台配置 |
| 版本与 ABI 可检查 | 固定 upstream 版本、LOCALVERSION、config fragments 和平台补丁;发布时校验精确资产 |
| 单 VM = 单 app 模型 | 关闭 user/net/uts/ipc/time namespace、in-guest userfaultfd(完整 nested 容器运行时非目标,§3.2) |
| host 控制 guest 内存 | 启用 virtio-balloon(host-driven inflate via vm.resize);virtio-mem 驱动保留作扩展点 |
| 单一 rootfs 路径 | virtio-pmem + DAX + EROFS(只读) + ext4(可写) + overlayfs |
| 沙箱内资源隔离 + 存储挂载 | cgroup v2 freezer + cpu/memory/io/pids 控制器(由 launch.cgroup_control 选择应用侧委派,§5.2);NFS 客户端 v3/v4 + FUSE(s3fs)挂载(§3.1) |

### 1.2 内核版本

- 上游:`linux-6.1.169` (LTS),源 `cdn.kernel.org`
- 编译器:host gcc(原生)/ `${CROSS_PREFIX}gcc`(交叉)
- 优化:`CC_OPTIMIZE_FOR_SIZE=y`(-Os),不为 sandbox 启动场景做 -O2
- LOCALVERSION:`-sandbox`(让 `uname -r` 在所有 sandbox 上一致,辅助调试)

### 1.3 输出

```
bin/x86_64/vmlinux        ELF 内核,PVH 启动协议,按大小优化且不含 debug-info
bin/aarch64/vmlinux       PE 格式 Image,EFI stub + ACPI 启动
```

文件名两 arch 都叫 `vmlinux`,内部格式按 arch 不同。cloud-hypervisor 自动检测
格式选择所支持的启动路径——sandbox-ctl 上层使用相同内核路径接口。应测量实际构建或
发布文件的大小;本文不设普遍镜像大小或启动耗时结果。

本仓通过 `main` 上受信任的 `Vmlinux Release` workflow 从调度器钉住的源码分支
和精确 SHA 独立发布 `vmlinux-vX.Y.Z`,制品名为
`vmlinux-x86_64-vX.Y.Z.tar.gz`。该版本线与 `runtime-vX.Y.Z` 独立,
两者版本号不要求一致;平台聚合版本显式选择各自版本。源码支持 aarch64 不代表已经
发布 aarch64 资产或完成了与 x86_64 等价的运行验证。

## 2. 构建工作流

`make vmlinux` 在输出缺失或受跟踪的内核输入更新时依次执行 fetch、patches-apply 和 build。
显式 `linux-fetch`、`linux-patches-apply`、`linux-patches-format`、`linux-build` 目标通过
`native-deps/deps/build-vmlinux.sh` 的 `STAGE` 选择阶段。从仓库根目录使用
`make -C native-deps <target>`,或进入 native-deps 后使用 `make <target>`:

```
fetch         下载并校验 linux-6.1.169.tar.gz(支持 TARBALL_CACHE 共享缓存),
              解压到 build/src/linux/(跨 arch 共享同一棵源码树),git init +
              提交原始树 + 打 tag linux-patches-base
patches-apply git am deps/linux-patches/*.patch 到该树(平台对 guest 内核的
              定制补丁;当前一条:virtio_balloon 在不可行 host target 下收敛
              到可持续大小而非活锁,见 §5.6)。已应用则幂等跳过;未导出或
              与 patch 不一致的工作不会被覆盖
build         把 sandbox-common.config + sandbox-<arch>.config 拼接成
              arch/<kbuild_arch>/configs/sandbox_defconfig → make
              sandbox_defconfig + make olddefconfig(关键:让 kbuild 解析依赖
              闭包并暴露 silent regression)→ make -j$(nproc) <kbuild_target>
              (x86_64: vmlinux ELF;arm64: Image PE)→ cp 到 bin/<arch>/vmlinux
```

增量构建按输入决定:Native Makefile 跟踪 build/common 脚本、两份所选 fragment、patch
目录及受跟踪的 patch。输入变化后,已有 `bin/<arch>/vmlinux` 不会阻止重新构建。
显式 `linux-build` 每次重新解析配置,Kbuild 编译本身保持增量。删除输出或 clean 后再
`make vmlinux` 可强制重新生成。

**Patch 开发流**:在 native-deps 目录执行 `make linux-fetch`
拉源码并打 `linux-patches-base` tag → 在 `build/src/linux/` 改代码 +
`git commit` → `make linux-patches-format` 导出回 `deps/linux-patches/*.patch`
→ `make vmlinux` 重新应用 + 构建。幂等与 sanity 语义统一见
[Native 构建指南](../native-deps/README_zh.md) §3;补丁 arch-neutral,x86_64 /
arm64 共用同一组。必须先保留尚未导出的工作,不能只为让构建继续而重置它。

### 2.1 host 构建依赖

```
bc bison flex make tar pkg-config gcc
libelf-dev / elfutils-libelf-devel
libssl-dev  / openssl-devel
```

获取源码与 patch 开发还需要 Git 以及 Native 构建指南列出的下载/归档工具。
交叉构建额外需要 `gcc-aarch64-linux-gnu` / `gcc-x86-64-linux-gnu`(取决于方向)。

## 3. 配置体系

defconfig 由 `native-deps/deps/vmlinux/` 中的**两段拼接**:

```
sandbox-common.config           跨 arch 通用(子系统启用、基础数据结构、virtio)
+ sandbox-<arch>.config         arch 专属(启动协议、中断控制器、串口)
= sandbox_defconfig             给 kbuild 的最终输入
```

脚本实际执行 `sandbox_defconfig` 和 `olddefconfig`,没有执行 `allnoconfig` 目标。
应检查包含 Kconfig 默认值与依赖闭包的最终 `.config`,不能假定所有未写出的符号都已关闭。

### 3.1 关键启用项

子系统 / 设备驱动:

```
PCI=y, PCI_MSI=y                CH 通过 virtio-pci 透传所有设备;MSI-X 是
                                现代 virtio-pci 的中断路径,必须启用
NETDEVICES=y                    网络驱动伞;依赖缺失可使请求的 VIRTIO_NET 被静默移除
NET=y, INET=y                   网络与 AF_INET 支持;同时保留 AF_VSOCK/AF_NETLINK 的依赖
VIRTIO=y, VIRTIO_PCI=y          virtio 总线
VIRTIO_BLK=y                    blk0/blk1(基础磁盘 + COW 上层)
VIRTIO_NET=y                    eth0(连到 host TAP)
VIRTIO_PMEM=y                   sandbox-runtime.bundle 通过 virtio-pmem 挂入
VIRTIO_VSOCKETS=y               sandbox-init ↔ sandbox-ctl 控制面
VIRTIO_BALLOON=y                host 内存回收(host 通过 vm.resize 推 inflate;
                                内核编译了 free-page reporting,但 CH balloon 配置不协商启用,见 §5.5)
VIRTIO_MEM=y                    host 主动 unplug 内存块
LIBNVDIMM + ZONE_DEVICE +       virtio-pmem FS DAX 直接映射 host page cache;
FS_DAX                          最终配置在 olddefconfig 后强制校验
EROFS_FS=y                      只读根文件系统
EXT4_FS=y                       overlayfs 写层
OVERLAY_FS=y                    EROFS lower + ext4 upper 合并出 / 视图
TMPFS=y                         /tmp 等运行时挂载点
NETWORK_FILESYSTEMS=y           NFS 伞门;须保留依赖使 NFS_FS 在最终配置中生效
NFS_FS=y + NFS_V3 + NFS_V4(.1/.2) 沙箱挂载 NFS(SUNRPC/LOCKD 自动选入;KEYS=y
                                供 v4 idmap,DNS_RESOLVER=y 供 v4 referral)
FUSE_FS=y                       沙箱经 s3fs 挂载 S3(用户态 FUSE;/dev/fuse 由
                                devtmpfs 自动创建)。mount.nfs / s3fs 等用户态
                                工具在沙箱镜像内,内核只提供能力
```

启动 / 时间 / 调度:

```
SMP=y, NR_CPUS=4                随附内核最多支持 4 个 vCPU。未启用 SMP 时,
                                NR_CPUS 可回到 1,无法提供所请求的多 vCPU 拓扑
X86_X2APIC=y(x86_64)            CH 以 MADT type 9 描述 vCPU;关闭时 guest 会忽略
                                全部表项并退化为单个 fallback boot CPU
HZ_100, NO_HZ_IDLE              低 tick 频率 + idle 时不 tick,密度场景关键
HIGH_RES_TIMERS                 hrtimer 子系统(应用 epoll/timerfd 依赖)
HW_RANDOM=y, RANDOM_TRUST_CPU=y 信任可用的 CPU 熵来源,减少对较慢启动熵收集的依赖;
                                实际耗时须测量
RTC_CLASS=y, RTC_HCTOSYS=y      启动时钟同步
PARAVIRT=y, KVM_GUEST=y         (x86_64) PV 优化:tlb shootdown / spinlock
```

应用隔离使用 PID、mount 和 cgroup namespace:

```
NAMESPACES=y, PID_NS=y          sandbox-init 建立 PID/mount 隔离,使主应用看到自己 PID=1
CGROUPS=y                       应用 cgroup namespace 以真实 /app 为根(§5.2)
# UTS_NS, TIME_NS, IPC_NS,       1-VM = 1-app preset 未启用这些额外嵌套隔离
# USER_NS, NET_NS not set
```

cgroup(v2 freezer 核心 + cpu/memory/io/pids 控制器):

```
CGROUPS=y                       cgroup v2 层级。v2 freezer 属核心(无独立
                                Kconfig):sandbox-init 在 quiesce 前原子冻结
                                应用进程树、restore 环境就绪后解冻,消除
                                resume-vs-env-init 竞态(机制见
                                sandboxer/docs/sandbox-init.md §3.4;详见 §5.2)
MEMCG=y                         memory.{high,max,min,low}
CGROUP_SCHED + FAIR_GROUP_SCHED cpu.weight(+ CFS_BANDWIDTH 给 cpu.max)
BLK_CGROUP + BLK_CGROUP_IOCOST  io.weight(io.cost 模型;+ BLK_DEV_THROTTLING
                                给 io.max)
CGROUP_PIDS=y                   pids.max
                                launch.cgroup_control=true 时,sandbox-init 向空的
                                真实 /app 委派可用控制器,受管理应用在 /app/init 启动,
                                用户子 cgroup 全部留在 /app 内。host cgroup + balloon
                                仍框定整台 VM(§5.2)
# CGROUP_FREEZER(v1 旧冻结器)/ RT_GROUP_SCHED / CPUSETS / CGROUP_DEVICE /
# CGROUP_PERF / CGROUP_BPF / CGROUP_HUGETLB / CGROUP_MISC / NET_CLS /
# NET_PRIO not set —— 未用
```

### 3.2 当前最小配置的禁用项

当前平台 preset 没有启用以下随机化或 hardening 选项.这是发布内核的配置事实和
显式安全取舍,不是模板父层共享或快照恢复成立的前提:

```
# RANDOMIZE_BASE not set         当前 preset 未启用 KASLR
# RANDOMIZE_MEMORY not set       当前 x86_64 preset 未启用内核物理映射随机偏移
# SLAB_FREELIST_RANDOM not set   当前 preset 未启用 freelist 随机化
# SLAB_FREELIST_HARDENED not set 当前 preset 未启用 freelist hardening
# SHUFFLE_PAGE_ALLOCATOR not set 当前 preset 未启用 page allocator shuffle
```

这些随机化和 hardening 能力可以增加 Guest kernel 的纵深防御.生产部署必须按威胁模型评估该 preset;
需要不同 hardening、审计或 Guest 功能时,通过 `boot.kernel: file://...` 使用经过验证的
自带内核.快照复用依赖显式父层关系,不以关闭这些机制换取跨虚机内存去重.

仓库的 `scripts/guest-inspect.py` 当前固定使用未随机化的 `KERNEL_IMAGE_BASE` 和
`PAGE_OFFSET`,因此只兼容同时关闭 `CONFIG_RANDOMIZE_BASE` 与
`CONFIG_RANDOMIZE_MEMORY` 的内核.启用这些选项的自带内核不能继续使用该脚本,
除非先让脚本能够发现运行时重定位.这是调试工具约束,不是快照恢复约束.

当前 preset 还关闭 `CONFIG_SLUB_CPU_PARTIAL`.该选项控制 per-CPU partial slab,
属于分配性能与内存占用的权衡,不作为安全 hardening 能力描述.

不需要的子系统(直接砍 + 减小镜像):

```
# MODULES not set                 平台 kernel 单一 vmlinux,无可加载模块
# SOUND / DRM / FB / INPUT / HID  无显示音频
# USB_SUPPORT / I2C / SPI / GPIO  无外设
# WATCHDOG / THERMAL / CPU_FREQ   host 管;guest 看到 fixed-spec
# SCSI / ATA / NVME / LOOP        块设备只走 virtio-blk
# MD / BCACHE                     软 RAID / 缓存层
# BTRFS / F2FS / XFS / CIFS /    本地只用 EROFS + ext4 + tmpfs + overlayfs;
# VFAT / NTFS / AUTOFS4          网络/对象挂载用 NFS + FUSE(已开,见 §3.1)
# HUGETLBFS                       Guest HugeTLBFS 不属于当前 4 KiB preset
# BRIDGE / VLAN / BONDING / TUN   guest 内不需要二层桥接
# VETH / VXLAN / GENEVE / WLAN
# IP_PNP                          网络由 sandbox-init netlink 配置,
                                  不走内核 cmdline ip=... 的 DHCP/BOOTP 路径
# BPF_JIT not set                 BPF_SYSCALL=y 但当前 Guest workload 不要求 JIT;
                                  需要 JIT 的应用使用自带 kernel
```

平台 ABI 边界(关闭 = guest app 看不到这些功能):

```
# USERFAULTFD not set             userfaultfd() 是 host 能力(sandbox-ctl
                                  在 memfd 上注册);guest 调用返回 ENOSYS
# UTS_NS / TIME_NS / IPC_NS /     1-VM = 1-app 模型不需要 nested 隔离
# USER_NS / NET_NS not set
NUMA not set                      关闭 Guest NUMA 支持;不能据此推导内核只有一个 memory zone
```

调试 / 跟踪重型设施关闭。实际镜像大小变化依赖具体构建,不作为发布保证:

```
# DEBUG_INFO / FTRACE / KASAN / UBSAN / KMSAN / KFENCE / DEBUG_FS
# PROFILING / DEBUG_OBJECTS / DEBUG_KMEMLEAK / PROVE_LOCKING
# RUNTIME_TESTING_MENU / KGDB
DEBUG_INFO_NONE=y                 显式无 debug-info(默认即此,固化避免回归)
LOG_BUF_SHIFT=14                  printk ring 的基础 shift;SMP 部分还由
                                  LOG_CPU_MAX_BUF_SHIFT=12 控制
```

例外是一组轻量诊断探测器,当前启用,在 config 中显式标注为临时诊断项
(全部 print-only,不设任何 `*_PANIC`):

```
DEBUG_KERNEL=y                    诊断项的 Kconfig 伞(hung-task 等依赖)
DETECT_HUNG_TASK=y                D 状态卡死打印(timeout 20 s,含锁/IO 等待栈)
LOCKUP_DETECTOR=y                 CPU 困于内核态探测(+SOFTLOCKUP_DETECTOR)
WQ_WATCHDOG=y                     workqueue 停转探测
PSI=y                             内存 / IO 压力量化
MAGIC_SYSRQ=y                     按需 sysrq-t/-w/-m
```

当前未启用的审计 / 安全框架:

```
# AUDIT not set                   当前发布内核不提供 Guest audit 子系统
# SECURITY not set                LSM 框架(SELinux / AppArmor 等)
# INTEGRITY not set               IMA / EVM
# HARDENED_USERCOPY not set       未启用 userspace 拷贝边界 hardening
# FORTIFY_SOURCE not set          未启用内核 fortify checks
```

## 4. 架构差异

### 4.1 启动协议

| 项 | x86_64 | aarch64 |
|---|---|---|
| 镜像格式 | ELF | PE(EFI) |
| 启动入口 | PVH(`PARAVIRT=y` + `XEN_PVH=y` + `PVH=y`)| EFI stub + ACPI(`EFI_STUB=y` + `ACPI=y`)|
| BIOS / 固件 | 无,CH 直接跳 ELF entry | 无,CH 加载 PE Image,跳 EFI stub entry |
| 启动结构 | zero-page 由 CH 填(memmap、cmdline) | FDT + ACPI 表由 CH 构造 |
| SMP 副 CPU 拉起 | APIC INIT-SIPI | PSCI(`ARM_PSCI=y`),不用 x86 INIT-SIPI |
| 内核镜像 subpath | `vmlinux` | `arch/arm64/boot/Image` |
| make target | `vmlinux` | `Image` |

PVH 让 x86_64 跳过 BIOS/PXE 阶段;arm64 fragment 启用 EFI stub/ACPI。启动耗时必须用
精确内核、VMM、硬件、workload 与计时边界测量;配置本身不能证明 50 ms、80 ms 或
亚百毫秒的启动保证。

### 4.2 中断控制器

| 项 | x86_64 | aarch64 |
|---|---|---|
| 控制器 | APIC + IO-APIC + MSI-X | GICv3 + ITS |
| Kconfig | `PCI_MSI=y`(隐含 APIC) | `ARM_GIC_V3=y` + `ARM_GIC_V3_ITS=y` |
| virtio-pci 中断路径 | MSI-X | ITS |

### 4.3 串口 / 控制台

| 项 | x86_64 | aarch64 |
|---|---|---|
| UART 驱动 | 无(8250 不编入) | PL011 AMBA UART(MMIO) |
| virtio-console(hvc) | `VIRTIO_CONSOLE=y` | `VIRTIO_CONSOLE=y` |
| Kconfig | — | `ARM_AMBA=y` + `SERIAL_AMBA_PL011(_CONSOLE)=y` |

**运行时控制台**:默认 CH 命令使用 `--console tty --serial off`,内核 cmdline 注入
`console=hvc0`——两架构的内核 dmesg 都走 virtio-console(hvc0)。部署还可通过
sandbox-ctl 选择 journald 或文件等控制台目的地,Guest hvc0 接口不变。x86_64 内核
不编任何 UART 驱动;aarch64 编入 PL011(CH 在 arm64 暴露 PL011 设备,留作启动早期
与调试控制台)。应用的 stdin/stdout/stderr 不走任何 console 设备(走 vsock,见
[sandboxer Guest ABI 文档](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init_zh.md) §3.5 / §4.5)。

### 4.4 RTC

x86_64 用 CH 提供的 CMOS RTC(`RTC_DRV_CMOS=y`),aarch64 用 PL031(MMIO,
`RTC_DRV_PL031=y`)——arm64 没有 PC 兼容 CMOS 的概念。

### 4.5 页大小

arm64 内核配置可以使用 4 KiB / 16 KiB / 64 KiB。**随附的平台 preset 显式选择
4 KiB**。Host userfaultfd 作用于 host 内存;当前 host handler 与工件路径使用 4 KiB
单位。不能仅凭内核构建成功推导其他 Guest 页大小的兼容性。16 KiB/64 KiB 自带 Guest
内核不属于此 preset,必须与所选 host、VMM、快照路径一起进行端到端验证:

```
CONFIG_ARM64_4K_PAGES=y           显式锁 4 KiB
# CONFIG_ARM64_16K_PAGES not set
# CONFIG_ARM64_64K_PAGES not set
```

x86_64 页大小固定 4 KiB,无此问题。

### 4.6 PCI 拓扑

| 项 | x86_64 | aarch64 |
|---|---|---|
| host bridge 发现 | MMCONFIG / MCFG ACPI 表 | generic ECAM(ACPI/DT) |
| Kconfig | `PCI_MMCONFIG=y` | `PCI_HOST_GENERIC=y` + `PCI_ECAM=y` |

## 5. 关键决策

### 5.1 为什么默认关 USERFAULTFD

`userfaultfd` 是平台 host 端的能力——sandbox-ctl 在 host 上对 backing memfd
的 chVA 注册 uffd,接管 guest RAM 缺页.当前平台 workload 不需要在 **Guest 内**
调用 `userfaultfd()`,因此最小 preset 不暴露该 API.

需要在 guest 内做用户态 uffd 的应用(罕见——多是数据库自己管 page cache 的
场景)走"自带 vmlinux"路径。

### 5.2 cgroup v2:freezer + cpu/memory/io/pids 控制器

`CONFIG_CGROUPS=y` 提供 cgroup v2 层级,平台使用两类能力。

**freezer(核心,无独立 Kconfig)**:快照 quiesce 前,sandbox-init 必须冻结完整应用子树并确认
`cgroup.events:frozen 1`;restore 的墙钟等环境初始化完成后再解冻。否则 `/vm.resume`
可能先于 Guest 处理 `restore` 而解冻 vCPU,使应用在旧墙钟或尚未重连的 MUX 上抢跑。
机制见 [sandboxer Guest ABI 文档](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init_zh.md) §3.4。v2 freezer(`cgroup.freeze`,Linux ≥5.2)
属于 CONFIG_CGROUPS 核心;CGROUP_FREEZER 是不需要的 v1 旧冻结器。

**资源控制器**:MEMCG、CGROUP_SCHED+FAIR_GROUP_SCHED+CFS_BANDWIDTH、
BLK_CGROUP+BLK_CGROUP_IOCOST+BLK_DEV_THROTTLING、CGROUP_PIDS 提供 Guest
`cpu.weight`、`cpu.max`、`memory.{high,max,min,low}`、`io.weight`、`io.max`、`pids.max`。
启用应用委派后,envd 等 E2B 应用可为 ptys/socats/user 工作负载建立子 cgroup。未编入的
控制器不会提供对应接口文件;仅编入也不够,还须在 `cgroup.subtree_control` 中启用,
后代 cgroup 才能获得接口。

应用 cgroup namespace、`/app` 冻结域、委派和进程/FD 生命周期完整定义在 [Guest ABI](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init_zh.md#321-应用-cgroup-namespace-与-controller-拓扑)中。内核必须提供所要求的控制器与 namespace；开启 `launch.cgroup_control` 时，ABI 要求严格启用并校验 advertised controller，不能把缺失能力静默视为成功。

Guest 控制器是在 **VM 预算内进一步细分**,不替代 host 权威。Host cgroup v2 限制
CH 进程,balloon 控制 Guest 可用内存。Guest 子 cgroup 的 `memory.max=max` 表示没有
额外子组上限,不代表 Guest Capacity 耗尽时不会 OOM,也不保证 host `deflate_on_oom`
先发生。必须把 Guest 应用完成情况与 Guest 内存压力同 host cgroup OOM 计数分开验证。

完整 nested 容器运行时(podman / Docker-in-Docker)仍非支持目标;它们需要本 preset
关闭的 user_ns、net_ns 等额外 namespace(§3.2),工作流也不同于平台的短生命周期和快照模型。

### 5.3 为什么 IP_PNP 关闭

内核 `ip=...` cmdline 路径执行 DHCP/BOOTP 自动配网。沙箱已经通过 vsock launch
协议从 sandbox-ctl 获得显式网络配置,由 sandbox-init 以 raw netlink 应用,因此 preset
关闭重复的内核自动配网路径。启动时间与镜像大小的影响必须按精确构建测量,不能把历史
估算当作普遍节省量。

### 5.4 为什么 NR_CPUS=4

随附内核设置 NR_CPUS=4,使 CPU mask 和 per-CPU 数据结构针对最多 4 个 vCPU。
更大的 NR_CPUS 可能增加小 Guest 不使用的结构与固定内存占用。

这是**内核 preset 限制**,不是把 `resources.capacity.cpu` 限定为 1/2/4 三档的平台 API
规则。当前 sandbox 配置校验 CPU 数量为正,没有这组三值枚举。请求 Capacity 仍须由
选定内核和 VMM 支持;配置被 parser 接受并不证明随附的 4-CPU 内核能运行它。
Capacity 与资源语义见 [sandboxer 生命周期文档](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox_zh.md) §4。

### 5.5 为什么不启用 free_page_reporting,以及 VIRTIO_MEM 的角色

`virtio-balloon free_page_reporting` 使 Guest 在 reclaim 期间持续向 host 报告空闲页。
CH 的 `release_memory_range` 可对映射执行 `madvise(MADV_DONTNEED)`。在平台统一
memfd / 外部 uffd 路径中,失效会经 mmu_notifier 传播到 KVM 映射。平台此前记录的故障
表现包括重复失效/IPI 活动、Guest vsock 饥饿及 host→guest ping 超时,因此平台的 CH
balloon 配置**不协商启用 free_page_reporting**;这不是对所有配置的耗时或故障保证。

必须区分编译与运行协商:内核 fragment 实际包含 CONFIG_VIRTIO_BALLOON=y、
CONFIG_VIRTIO_BALLOON_FREE_PAGE_REPORTING=y、CONFIG_PAGE_REPORTING=y。
编译了能力不代表 host 在运行时 advertise/启用它。

内核提供能力而非节点资源策略。`mem_report` 与 Guest 报告字段由 [Guest ABI](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init_zh.md)定义；target/current、增长收缩、reservation 释放及 fresh-report 条件由 [sandboxer 资源闭环](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox_zh.md#93-balloon)定义；稀疏范围 release 由 [VMM 补丁](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/cloud-hypervisor_zh.md#34-0004--balloon-release-跳过-user-managed-zone-的空洞-run)定义。`/proc/meminfo` 的 `Balloon:` 不是该 Guest ABI 的来源，当前值取自 CH `memory_actual_size`，不能从 Guest MemTotal 反推 Capacity。

`VIRTIO_MEM=y` 保留为 host 主动请求 Guest 内存块 unplug 的扩展点。其较粗、较少
的事件可用于另外设计的内存热插拔/NUMA 场景。当前固定 Capacity Budget 模型拒绝
memory hotplug,不使用 virtio-mem。保留驱动本身不能启用新设计;还须有相应 host
支持并完成端到端验证。

### 5.6 为什么打 virtio_balloon 收敛补丁(`deps/linux-patches/`)

Host BalloonController 根据反馈调整 vm.resize target(§5.5),目标可暂时**不可行**:
例如启动工作集占用较大,Guest 暂时没有足够页可释放。在此 patch 处理的故障场景中,
原始 inflate 路径反复分配失败(`Out of puff`)并重试同一 target,而 deflate_on_oom
又在压力下放掉刚充入的页。相互竞争的 inflate/deflate 可使大小不收敛,持续消耗 CPU
并产生映射失效。

平台 patch 改为**收敛**语义。Inflate 分配失败后,driver 把可持续 balloon 大小减去
安全余量,记录为**粘滞上限**,并主动 deflate 到该上限。后续压力只能继续向下收紧。
这是 Guest 应急保护,不是 Budget 调整,也不会自动恢复 host target。CH
memory_actual_size 显示 target/current 不一致时,host 视该阶段为 unstable:
不继续 shrink、不释放 reservation、不丢弃已配置的 memory.high 软保证。正常 grow
仍可通过降低 balloon target 给 Guest 更多内存。

这是纯 Guest 侧鲁棒性修复,不改变 host/guest 协议。它缓解所识别的收敛故障,不保证
任意 Guest workload 都不会 OOM。记账与后续控制见 §5.5 和 sandbox.md §9.3;
patch 源码位于 `native-deps/deps/linux-patches/`。

## 6. 验证

构建后基本 sanity:

```bash
file bin/x86_64/vmlinux
# bin/x86_64/vmlinux: ELF 64-bit LSB executable, x86-64, ...

file bin/aarch64/vmlinux
# bin/aarch64/vmlinux: PE32+ executable (EFI application) Aarch64, ...

# 运行时验证(在 sandbox 内,通过 sandbox-init 早期日志)
# uname -r 应为 6.1.169-sandbox
# /proc/version 不含 build host hostname / username
# /proc/config.gz 不存在(IKCONFIG 关闭)
```

`release-vmlinux.yml` 的可信入口验证**精确选定的源码分支 SHA** 的构建、native dependency
工具和发布包;该 SHA 可来自 main 或受支持的维护分支。该工作流本身不启动 vmlinux.因此组件 Release 成功不能单独作为 Guest
行为验证.进入聚合版本前还需要保留对应精确源码组合的 平台集成测试结果;
聚合版本还需要使用已发布资产运行完整真实 MicroVM E2E,覆盖启动协议、
必需设备、文件系统、网络、Balloon、Cgroup 和 snapshot/restore 路径。
Stable 与 Preview 聚合均使用 exact-asset 集成测试门禁。

配置 review 使用 `make olddefconfig` diff 检查 silent Kconfig 变化;不要以跨实例
RAM 字节相同比例作为发布门禁.

## 7. 维护

- 升级 kernel 主版本(6.1 → 6.x):review `sandbox-common.config` 的功能、安全和
  调试选项,运行 `make olddefconfig` 看 silent regression;并把
  `deps/linux-patches/*.patch` 在新源码树上 rebase
  (`make linux-fetch` 重打 base → 在 `build/src/linux/` `git rebase` /
  重打补丁 → `make linux-patches-format` 回写),解决 virtio_balloon
  收敛补丁(§5.6)与新版驱动的冲突
- 升级架构 fragment:review CH 对该 arch 的设备模型是否有新依赖(例如
  GICv4 / 新 RTC 驱动)
- 不上游化 defconfig:本配置选择跟通用 server distro 取向偏离明显
  (关闭 userfaultfd、裁剪 namespace、cgroup 只留 freezer + sandbox 必需的
  cpu/memory/io/pids 控制器等),不寻求合并到 upstream

## 8. See Also

- [sandboxer VMM 文档](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/cloud-hypervisor_zh.md) —— 平台 VMM 的启动协议、设备
  模型、patch 范围
- [sandbox-runtime_zh.md](sandbox-runtime_zh.md) —— 内核之上的 runtime 镜像打包与布局;
  [sandboxer Guest ABI 文档](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init_zh.md) 负责 rootfs 组装、应用拉起与 Guest ABI
- [sandboxer 生命周期文档](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox_zh.md) §3.1 定义 `boot.kernel` 引用。自带 kernel 须满足本篇的内核能力、所选 Guest ABI 与 VMM 契约及 §6 的验证要求；[工件不兼容边界](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-artifacts_zh.md#144-incompatibility) 另行说明被拒绝的 snapshot 格式。
- [Native 构建指南](../native-deps/README_zh.md) —— `make vmlinux` 工作流、patch 开发循环、
  交叉编译
- [系统总览](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/kuasar-sandbox_zh.md) §4 —— 模板父层与暂停/恢复的系统语义
