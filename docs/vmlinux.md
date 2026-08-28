# vmlinux — guest 内核镜像

平台为每个 sandbox 提供一份**最小化、固定版本**的 Linux 内核镜像
(`bin/<arch>/vmlinux`).本文档定义这份镜像的构建方式、启用/禁用的功能集合,
以及这些选择对启动、设备、资源控制和 Guest 安全边界的影响.

vmlinux 是平台资产,**不**对外暴露内核版本/配置接口给租户。需要不同 kernel 的
用户应用通过 `boot.kernel: file://...` 自带 vmlinux,平台保证 cloud-hypervisor
的命令行兼容,但不对自带 kernel 的功能负责。

## 1. 概述

### 1.1 设计目标

| 目标 | 实现方式 |
|---|---|
| 镜像小、启动快 | allnoconfig 起步;仅启用沙箱必需的子系统;HZ=100;TTY 单端口 |
| 版本与 ABI 可检查 | 固定 upstream 版本、LOCALVERSION、config fragments 和平台补丁;发布时校验精确资产 |
| 单 VM = 单 app 模型 | 关闭 user/net/uts/ipc/time namespace、in-guest userfaultfd(完整 nested 容器运行时非目标,§3.2) |
| host 控制 guest 内存 | 启用 virtio-balloon(host-driven inflate via vm.resize)+ virtio-mem |
| 单一 rootfs 路径 | virtio-pmem + DAX + EROFS(只读) + ext4(可写) + overlayfs |
| 沙箱内资源隔离 + 存储挂载 | cgroup v2 freezer + cpu/memory/io/pids 控制器(envd 经 subtree_control 做 per-进程隔离,§5.2);NFS 客户端 v3/v4 + FUSE(s3fs)挂载(§3.1) |

### 1.2 内核版本

- 上游:`linux-6.1.169` (LTS),源 `cdn.kernel.org`
- 编译器:host gcc(原生)/ `${CROSS_PREFIX}gcc`(交叉)
- 优化:`CC_OPTIMIZE_FOR_SIZE=y`(-Os),不为 sandbox 启动场景做 -O2
- LOCALVERSION:`-sandbox`(让 `uname -r` 在所有 sandbox 上一致,辅助调试)

### 1.3 输出

```
bin/x86_64/vmlinux        ELF 内核,PVH 启动协议,~21 MiB(CC_OPTIMIZE_FOR_SIZE,无 debug-info)
bin/aarch64/vmlinux       PE 格式 Image,EFI stub + ACPI 启动,~14 MiB
```

文件名两 arch 都叫 `vmlinux`,内部格式按 arch 不同。cloud-hypervisor 自动检测
格式选择启动协议——sandbox-ctl 上层路径无 arch 分支。

本仓通过 `Vmlinux Release` workflow 独立发布 `vmlinux-vX.Y.Z`,制品名为
`vmlinux-x86_64-vX.Y.Z.tar.gz`。该版本线与 `runtime-vX.Y.Z` 独立,
两者版本号不要求一致;平台聚合版本显式选择各自版本。

## 2. 构建工作流

`make vmlinux` = `linux-patches-apply` + `linux-build`,二者都触发
`deps/build-vmlinux.sh`(按 `STAGE` 选阶段):

```
fetch         下载并校验 linux-6.1.169.tar.gz(支持 TARBALL_CACHE 共享缓存),
              解压到 build/src/linux/(跨 arch 共享同一棵源码树),git init +
              提交原始树 + 打 tag linux-patches-base
patches-apply git am deps/linux-patches/*.patch 到该树(平台对 guest 内核的
              定制补丁;当前一条:virtio_balloon 在不可行 host target 下收敛
              到可持续大小而非活锁,见 §5.6)。已应用则幂等跳过
build         把 sandbox-common.config + sandbox-<arch>.config 拼接成
              arch/<kbuild_arch>/configs/sandbox_defconfig → make
              sandbox_defconfig + make olddefconfig(关键:让 kbuild 解析依赖
              闭包并暴露 silent regression)→ make -j$(nproc) <kbuild_target>
              (x86_64: vmlinux ELF;arm64: Image PE)→ cp 到 bin/<arch>/vmlinux
```

幂等:`bin/<arch>/vmlinux` 存在则跳过(删了重跑或 `make clean && make vmlinux`
强制重建)。

**Patch 开发流**:`make linux-fetch`
拉源码并打 `linux-patches-base` tag → 在 `build/src/linux/` 改代码 +
`git commit` → `make linux-patches-format` 导出回 `deps/linux-patches/*.patch`
→ `make vmlinux` 重新应用 + 构建。幂等与 sanity 语义统一见
`guest-runtime/native-deps/docs/build.md` §3;补丁 arch-neutral,x86_64 /
arm64 共用同一组。

### 2.1 host 构建依赖

```
bc bison flex make tar pkg-config gcc
libelf-dev / elfutils-libelf-devel
libssl-dev  / openssl-devel
```

交叉构建额外:`gcc-aarch64-linux-gnu` / `gcc-x86-64-linux-gnu`(取决于方向)。

## 3. 配置体系

defconfig 是**两段拼接**:

```
sandbox-common.config           跨 arch 通用(子系统启用、基础数据结构、virtio)
+ sandbox-<arch>.config         arch 专属(启动协议、中断控制器、串口)
= sandbox_defconfig             给 kbuild 的最终输入
```

### 3.1 关键启用项

子系统 / 设备驱动:

```
PCI=y, PCI_MSI=y                CH 通过 virtio-pci 透传所有设备;MSI-X 是
                                现代 virtio-pci 的中断路径,必须启用
NETDEVICES=y                    网络驱动伞,缺失时 VIRTIO_NET 静默 drop
NET=y, INET=y                   socket family(AF_INET/AF_VSOCK/AF_NETLINK)伞
VIRTIO=y, VIRTIO_PCI=y          virtio 总线
VIRTIO_BLK=y                    blk0/blk1(基础磁盘 + COW 上层)
VIRTIO_NET=y                    eth0(连到 host TAP)
VIRTIO_PMEM=y                   sandbox-runtime.bundle 通过 virtio-pmem 挂入
VIRTIO_VSOCKETS=y               sandbox-init ↔ sandbox-ctl 控制面
VIRTIO_BALLOON=y                host 内存回收(host 通过 vm.resize 推 inflate;
                                平台不启用 free_page_reporting,见 §5.5)
VIRTIO_MEM=y                    host 主动 unplug 内存块
LIBNVDIMM + ZONE_DEVICE +       virtio-pmem FS DAX 直接映射 host page cache;
FS_DAX                          最终配置在 olddefconfig 后强制校验
EROFS_FS=y                      只读根文件系统
EXT4_FS=y                       overlayfs 写层
OVERLAY_FS=y                    EROFS lower + ext4 upper 合并出 / 视图
TMPFS=y                         /tmp 等运行时挂载点
NETWORK_FILESYSTEMS=y           NFS 伞门(缺失时 NFS_FS 静默 drop,同 NET/PCI)
NFS_FS=y + NFS_V3 + NFS_V4(.1/.2) 沙箱挂载 NFS(SUNRPC/LOCKD 自动选入;KEYS=y
                                供 v4 idmap,DNS_RESOLVER=y 供 v4 referral)
FUSE_FS=y                       沙箱经 s3fs 挂载 S3(用户态 FUSE;/dev/fuse 由
                                devtmpfs 自动创建)。mount.nfs / s3fs 等用户态
                                工具在沙箱镜像内,内核只提供能力
```

启动 / 时间 / 调度:

```
SMP=y, NR_CPUS=4                沙箱 capacity.cpu ≤ 4(平台 fixed-spec 上限)。
                                CONFIG_SMP 不开 NR_CPUS 静默回到 1,
                                cpu>1 沙箱启动失败
X86_X2APIC=y(x86_64)            CH 以 MADT type 9 描述 vCPU;关闭时 guest 会忽略
                                全部表项并退化为单个 fallback boot CPU
HZ_100, NO_HZ_IDLE              低 tick 频率 + idle 时不 tick,密度场景关键
HIGH_RES_TIMERS                 hrtimer 子系统(应用 epoll/timerfd 依赖)
HW_RANDOM=y, RANDOM_TRUST_CPU=y rdrand 直接信任,跳过 jitterentropy 慢启动
                                (~100ms 节省)
RTC_CLASS=y, RTC_HCTOSYS=y      启动时钟同步
PARAVIRT=y, KVM_GUEST=y         (x86_64) PV 优化:tlb shootdown / spinlock
```

namespace(沙箱内只用 PID + MOUNT):

```
NAMESPACES=y, PID_NS=y          sandbox-init clone(NEWPID|NEWNS) 使用户 app
                                看到自己 PID=1
# UTS_NS, TIME_NS, IPC_NS,       1-VM = 1-app 模型不需要内嵌再切割
# USER_NS, NET_NS not set
```

cgroup(v2 freezer 核心 + cpu/memory/io/pids 控制器):

```
CGROUPS=y                       cgroup v2 层级。v2 freezer 属核心(无独立
                                Kconfig):sandbox-init 在 quiesce 前原子冻结
                                应用进程树、restore 环境就绪后解冻,消除
                                resume-vs-env-init 竞态(机制见
                                sandbox-runtime.md §3.4;详见 §5.2)
MEMCG=y                         memory.{high,max,min,low}
CGROUP_SCHED + FAIR_GROUP_SCHED cpu.weight(+ CFS_BANDWIDTH 给 cpu.max)
BLK_CGROUP + BLK_CGROUP_IOCOST  io.weight(io.cost 模型;+ BLK_DEV_THROTTLING
                                给 io.max)
CGROUP_PIDS=y                   pids.max
                                envd 在 guest 内为每类进程(ptys/socats/user)
                                建子 cgroup 并设上述项;sandbox-init 经 root
                                cgroup.subtree_control 下放控制器(§5.2)。host
                                cgroup v2 限 CH + balloon 仍框定整台 VM
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
# HUGETLBFS                       与 4 KiB-uffd 模型不兼容
# BRIDGE / VLAN / BONDING / TUN   guest 内不需要二层桥接
# VETH / VXLAN / GENEVE / WLAN
# IP_PNP                          网络由 sandbox-init netlink 配置,
                                  非 cmdline ip=...(节省 ~26ms initcall +
                                  ~10 KiB dhcp/bootp 客户端)
# BPF_JIT not set                 BPF_SYSCALL=y 但当前 Guest workload 不要求 JIT;
                                  需要 JIT 的应用使用自带 kernel
```

平台 ABI 边界(关闭 = guest app 看不到这些功能):

```
# USERFAULTFD not set             userfaultfd() 是 host 能力(sandbox-ctl
                                  在 memfd 上注册);guest 调用返回 ENOSYS
# UTS_NS / TIME_NS / IPC_NS /     1-VM = 1-app 模型不需要 nested 隔离
# USER_NS / NET_NS not set
NUMA not set                      沙箱永远单 zone 单 node
```

调试 / 跟踪(重型设施全关,~MiB 级镜像收益):

```
# DEBUG_INFO / FTRACE / KASAN / UBSAN / KMSAN / KFENCE / DEBUG_FS
# PROFILING / DEBUG_OBJECTS / DEBUG_KMEMLEAK / PROVE_LOCKING
# RUNTIME_TESTING_MENU / KGDB
DEBUG_INFO_NONE=y                 显式无 debug-info(默认即此,固化避免回归)
LOG_BUF_SHIFT=14                  16 KiB printk ring,控制最小 Guest 的固定内存预算
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
| SMP 副 CPU 拉起 | APIC INIT-SIPI | PSCI(`ARM_PSCI=y`,无 IPI/SIPI 概念) |
| 内核镜像 subpath | `vmlinux` | `arch/arm64/boot/Image` |
| make target | `vmlinux` | `Image` |

PVH 让 x86_64 跳过 BIOS/PXE 阶段,从 firstboot 到 sandbox-init 仅 ~50 ms 内核
boot 时间;arm64 EFI stub 略慢(~80 ms)但仍亚百毫秒。

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

**运行时控制台**:平台启动 CH 时带 `--console tty --serial off`,内核 cmdline 自动
注入 `console=hvc0`——两架构的内核 dmesg 都走 virtio-console(hvc0)。x86_64 内核
不编任何 UART 驱动;aarch64 编入 PL011(CH 在 arm64 暴露 PL011 设备,留作启动早期
与调试控制台)。应用的 stdin/stdout/stderr 不走任何 console 设备(走 vsock,见
`guest-runtime/docs/sandbox-runtime.md` §3.5 / §4.5)。

### 4.4 RTC

x86_64 用 CH 提供的 CMOS RTC(`RTC_DRV_CMOS=y`),aarch64 用 PL031(MMIO,
`RTC_DRV_PL031=y`)——arm64 没有 PC 兼容 CMOS 的概念。

### 4.5 页大小

aarch64 默认配置在不同发行版可能是 4K / 16K / 64K。**uffd 协议在
`PAGE_SIZE` 上工作**,host (sandbox-ctl) 推理 4 KiB 批次边界——guest 必须
4 KiB 才能匹配:

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

`CONFIG_CGROUPS=y` 提供 cgroup v2 层级,平台用其两类能力。

**freezer(核心,无独立 Kconfig)**:快照前 sandbox-init 要**原子冻结应用进程
树**,restore 环境(墙钟等)就绪后再解冻,否则 `/vm.resume` 先于 guest 处理
`restore` 解冻 vCPU,应用会带着旧墙钟 / 未重连的 MUX 抢跑一段(resume-vs-env-init
竞态;机制见 `guest-runtime/docs/sandbox-runtime.md` §3.4)。v2 freezer
(`cgroup.freeze`,内核 ≥5.2)即 `CONFIG_CGROUPS=y` 自带;`CGROUP_FREEZER` 是
v1 旧冻结器,不需要。

**资源控制器(`MEMCG` / `CGROUP_SCHED`+`FAIR_GROUP_SCHED`+`CFS_BANDWIDTH` /
`BLK_CGROUP`+`BLK_CGROUP_IOCOST`+`BLK_DEV_THROTTLING` / `CGROUP_PIDS`)**:e2b
模型要在 guest 内对每类进程做资源隔离——envd 为 pty / socat / user 进程各建
子 cgroup 并设 `cpu.weight` / `memory.{high,max,min,low}` / `io.weight` /
`pids.max`。控制器不编入则这些接口文件根本不存在(envd 跳过并告警);编入后还须
**经 `cgroup.subtree_control` 下放**才在子 cgroup 出现,而 envd 自身不写
subtree_control——故 **sandbox-init 挂载 cgroup2 后,把 root `cgroup.controllers`
里的控制器写入 root `cgroup.subtree_control`**
(`sandboxer/cmd/sandbox-init/cgroup.go`;root cgroup 豁免
no-internal-process 规则,故 PID 1 留在 root 仍可下放)。

这是**在 VM 预算内再细分**,不替代 host 权威:host cgroup v2 限 CH 进程 + balloon
仍框定整台 VM 的资源上限;guest 内子 cgroup 默认 `memory.max=max`(不设即无限),
仅 envd 显式设限才生效,故默认不会早于 host `deflate_on_oom` 触发 guest 内 OOM。

sandbox-init 另建唯一固定 `/sys/fs/cgroup/app` 作冻结域(envd 的 ptys/socats/user
是其 root 同级 cgroup)。完整的 nested 容器运行时(podman / docker-in-docker)仍非
支持目标——它们还需 `user_ns` / `net_ns`(平台关闭,§3.2),且其工作流跟"短生命
周期 + 快照恢复"模式正交。

### 5.3 为什么 IP_PNP 关闭

kernel 自身的 `ip=...` cmdline 自动配网走 dhcp/bootp 协议,在 sandbox 模型下
是反模式:网络配置已经由 sandbox-ctl 通过 vsock launch 协议显式下发给
sandbox-init,sandbox-init 用 raw netlink 配置。`ip_auto_config` initcall
~26 ms 净开销 + ~10 KiB dhcp/bootp 客户端代码,关掉直接收益。

### 5.4 为什么 NR_CPUS=4

平台 fixed-spec 把 capacity.cpu 限到 1/2/4 三档(详见 `sandboxer/docs/sandbox.md`
§4 资源模型)。
NR_CPUS=4 让 guest 内核数据结构(per-cpu / cpumask)按 4 核维度分配——
更大的 NR_CPUS 会增加最小 Guest 不使用的 per-CPU 数据结构和固定内存预算.

### 5.5 为什么不启用 free_page_reporting,以及 VIRTIO_MEM 的角色

`virtio-balloon free_page_reporting` 让 guest 把每轮 page reclaim 中的空闲页号
持续推到 host;host 端 CH 的 `release_memory_range` 在收到这些页号时,对自身
mmap 做 `madvise(MADV_DONTNEED)` 释放进程 PTE。在平台的统一 memfd / 外部 uffd
模型下,这条 madvise 会广播 mmu_notifier 失效到 KVM EPT,持续的 IPI shootdown
会饿死 guest vsock kthread(timer 中断丢失,host→guest ping 在十数秒内全部
timeout)。因此平台**关闭 free_page_reporting**——内核侧 `VIRTIO_BALLOON=y`
启用模块,但 CH 命令行不开 FPR feature。

替代路径:guest `sandbox-init` 在 cold launch barrier 后立即推送一份
`mem_report`,之后默认每 5 s 推送 `MemAvailable` 及诊断字段。host 仅从
Cloud Hypervisor `vm.info` 取 balloon target 与 `memory_actual_size`,不从 guest
`MemTotal` 反推 Capacity,也不要求 guest 上报 balloon current。sandbox-local
Budget 控制器必要时以 `PUT /api/v1/vm.resize` 改变 target;shrink 每份 fresh
report 最多 inflate 64 MiB,并等 `memory_actual_size` 收敛后才允许下一步。CH 在
inflate 处理路径里 `fallocate(PUNCH_HOLE) + madvise(DONTNEED)`。详见
`sandboxer/docs/sandbox.md` §9.3。

当前内核虽启用 `CONFIG_VIRTIO_BALLOON=y`,但真实 guest 的
`/proc/meminfo` 不导出 `Balloon:` 字段。该字段不是 guest ABI,也不是
Budget 控制的可选数据源;current 始终以 CH 的
`memory_actual_size` 为准。

`VIRTIO_MEM=y` 保留作扩展点:virtio-mem 是 host 主动 → guest unplug 路径,
事件粒度大、批量少,适合 NUMA / 横向扩 zone 等场景。平台当前不依赖
virtio-mem 路径,保留驱动使该扩展无需重打 vmlinux。

### 5.6 为什么打 virtio_balloon 收敛补丁(`deps/linux-patches/`)

host BalloonController 按反馈推 `vm.resize` target(§5.5),目标值可能一时
**不可行**——例如启动突发期工作集还很大,host 却把 target 设得超过 guest
当下能腾出的页。stock virtio_balloon 在这种情况下会**活锁**:inflate 路径分配
不到页(日志 "Out of puff")→ 不缩 target、下一轮继续猛冲;同时 `deflate_on_oom`
在内存压力下又把刚充进去的气放掉。两股力来回拉扯,balloon 大小在零和满之间
震荡、永不收敛,既没真正回收内存,又持续烧 CPU 与 mmu_notifier 流量。

平台补丁改为**收敛**语义:当 inflate 遇到分配失败,driver 把可持续
balloon 大小减去安全余量后记为**粘滞上限**,并主动 deflate 到该上限;
后续压力事件只会继续向下收紧。它是保护 guest 应用免于 OOM 的应急机制,
不是 Budget 调整,也不会自动恢复 host target。host 通过 CH
`memory_actual_size` 看到 target/current 不一致后,把该阶段视为 unstable:不继续
shrink,不释放 reservation,并保留已设置的 `memory.high` 软保证。正常 grow
仍可通过降低 balloon target 给 guest 更多内存。这是纯 guest 侧鲁棒性
修复,不改 host↔guest 协议;安全记账与后续控制见 §5.5 和
`sandboxer/docs/sandbox.md` §9.3。

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

`release-vmlinux.yml` 当前验证精确 `main` 源码的构建、native dependency 脚本和
发布包,但该工作流本身不启动 vmlinux.因此组件 Release 成功不能单独作为 Guest
行为验证.进入聚合版本前还需要保留对应精确源码组合的 platform BMS 结果;
Stable 聚合版本还需要使用已发布资产运行完整真实 MicroVM E2E,覆盖启动协议、
必需设备、文件系统、网络、Balloon、Cgroup 和 snapshot/restore 路径.

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

- `sandboxer/docs/cloud-hypervisor.md` —— 平台 VMM 的启动协议、设备
  模型、patch 范围
- `guest-runtime/docs/sandbox-runtime.md` —— 内核之上 sandbox-init 完成
  rootfs 组装与应用拉起
- `sandboxer/docs/sandbox.md` §3.1(`boot.kernel`)/ §14.2(平台 ABI 边界)——
  沙箱配置如何引用 vmlinux,以及自带 kernel 的接入方式
- `guest-runtime/native-deps/docs/build.md` —— `make vmlinux` 工作流、patch 开发循环、
  交叉编译
- `kuasar-sandbox/docs/kuasar-sandbox.md` §4 —— 模板父层与暂停/恢复的系统语义
