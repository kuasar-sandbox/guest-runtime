# sandbox-kernel — guest 内核定制

平台为每个 sandbox 提供一份**最小化、确定性、跨实例一致**的 Linux 内核镜像
(`bin/<arch>/vmlinux`)。本文档定义这份镜像的构建方式、启用/禁用的功能集合,
以及为什么这些选择对沙箱模型(高密度、亚秒启动、跨实例去重)是必要的。

vmlinux 是平台资产,**不**对外暴露内核版本/配置接口给租户。需要不同 kernel 的
用户应用通过 `boot.kernel: file://...` 自带 vmlinux,平台保证 cloud-hypervisor
的命令行兼容,但不对自带 kernel 的功能负责。

## 1. 概述

### 1.1 设计目标

| 目标 | 实现方式 |
|---|---|
| 镜像小、启动快 | allnoconfig 起步;仅启用沙箱必需的子系统;HZ=100;TTY 单端口 |
| 跨实例可去重 | 关闭所有运行时随机化(KASLR、SLAB freelist、页面 shuffle);固定 LOCALVERSION |
| 单 VM = 单 app 模型 | 关闭 cgroups、user/net/uts/ipc/time namespace、in-guest userfaultfd |
| host 控制 guest 内存 | 启用 virtio-balloon(host-driven inflate via vm.resize)+ virtio-mem |
| 单一 rootfs 路径 | virtio-pmem + DAX + EROFS(只读) + ext4(可写) + overlayfs |

### 1.2 内核版本

- 上游:`linux-6.1.169` (LTS),源 `cdn.kernel.org`
- 编译器:host gcc(原生)/ `${CROSS_PREFIX}gcc`(交叉)
- 优化:`CC_OPTIMIZE_FOR_SIZE=y`(-Os),不为 sandbox 启动场景做 -O2
- LOCALVERSION:`-sandbox`(让 `uname -r` 在所有 sandbox 上一致,辅助调试)

### 1.3 输出

```
bin/x86_64/vmlinux        ELF 内核,PVH 启动协议,~6 MiB
bin/aarch64/vmlinux       PE 格式 Image,EFI stub + ACPI 启动,~14 MiB
```

文件名两 arch 都叫 `vmlinux`,内部格式按 arch 不同。cloud-hypervisor 自动检测
格式选择启动协议——sandbox-ctl 上层路径无 arch 分支。

## 2. 构建工作流

`make vmlinux` 触发 `deps/build-vmlinux.sh`:

```
1. 下载并校验 linux-6.1.169.tar.gz(支持 TARBALL_CACHE 共享缓存)
2. 解压到 build/src/linux/(跨 arch 共享同一棵源码树)
3. 把 sandbox-common.config + sandbox-<arch>.config 拼接成
   arch/<kbuild_arch>/configs/sandbox_defconfig
4. make sandbox_defconfig + make olddefconfig(关键:让 kbuild 解析依赖
   闭包并暴露 silent regression)
5. make -j$(nproc) <kbuild_target>
   x86_64: vmlinux         (ELF)
   arm64:  Image           (PE)
6. cp 到 bin/<arch>/vmlinux
```

幂等:`bin/<arch>/vmlinux` 存在则跳过(删了重跑或 `make clean-deps && make vmlinux`
强制重建)。

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
VIRTIO_PMEM=y                   sandbox-runtime.erofs 通过 virtio-pmem 挂入
VIRTIO_VSOCKETS=y               sandbox-init ↔ sandbox-ctl 控制面
VIRTIO_BALLOON=y                host 内存回收(host 通过 vm.resize 推 inflate;
                                平台不启用 free_page_reporting,见 §5.5)
VIRTIO_MEM=y                    host 主动 unplug 内存块
LIBNVDIMM + DAX                 virtio-pmem DAX 直接映射 host page cache
EROFS_FS=y                      只读根文件系统
EXT4_FS=y                       overlayfs 写层
OVERLAY_FS=y                    EROFS lower + ext4 upper 合并出 / 视图
TMPFS=y                         /tmp 等运行时挂载点
```

启动 / 时间 / 调度:

```
SMP=y, NR_CPUS=4                沙箱 capacity.cpu ≤ 4(平台 fixed-spec 上限)。
                                CONFIG_SMP 不开 NR_CPUS 静默回到 1,
                                cpu>1 沙箱启动失败
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

### 3.2 关键禁用项(收敛闭口)

去重 / 确定性敏感(任一启用都会让跨实例 RAM 内容差异化):

```
# RANDOMIZE_BASE not set         KASLR(放置内核 + 模块虚地址)
# RANDOMIZE_MEMORY not set       (x86_64) 内核物理映射随机偏移
# SLAB_FREELIST_RANDOM not set   slab freelist 随机顺序
# SLAB_FREELIST_HARDENED not set 同上 + 安全混淆
# SHUFFLE_PAGE_ALLOCATOR not set 内存初始化时 buddy 列表 shuffle
# SLUB_CPU_PARTIAL not set       per-cpu partial slab(随访问历史变化)
```

不需要的子系统(直接砍 + 减小镜像):

```
# MODULES not set                 平台 kernel 单一 vmlinux,无可加载模块
# SOUND / DRM / FB / INPUT / HID  无显示音频
# USB_SUPPORT / I2C / SPI / GPIO  无外设
# WATCHDOG / THERMAL / CPU_FREQ   host 管;guest 看到 fixed-spec
# SCSI / ATA / NVME / LOOP        块设备只走 virtio-blk
# MD / BCACHE                     软 RAID / 缓存层
# BTRFS / F2FS / XFS / NFS / FUSE 只用 EROFS + ext4 + tmpfs + overlayfs
# CIFS / VFAT / NTFS / AUTOFS4
# HUGETLBFS                       与 4 KiB-uffd 模型不兼容
# BRIDGE / VLAN / BONDING / TUN   guest 内不需要二层桥接
# VETH / VXLAN / GENEVE / WLAN
# IP_PNP                          网络由 sandbox-init netlink 配置,
                                  非 cmdline ip=...(节省 ~26ms initcall +
                                  ~10 KiB dhcp/bootp 客户端)
# BPF_JIT not set                 BPF_SYSCALL=y 但 JIT 关(JIT 输出与
                                  地址相关,跨实例 page cache 差异化)
```

平台契约边界(关闭 = guest app 看不到这些功能):

```
# CGROUPS not set                 guest 不切子 cgroup;host cgroup v2 限 CH
                                  即足够。systemd-style 资源管理 / nested
                                  容器运行时不支持
# USERFAULTFD not set             userfaultfd() 是 host 能力(sandbox-ctl
                                  在 memfd 上注册);guest 调用返回 ENOSYS
# UTS_NS / TIME_NS / IPC_NS /     1-VM = 1-app 模型不需要 nested 隔离
# USER_NS / NET_NS not set
NUMA not set                      沙箱永远单 zone 单 node
```

调试 / 跟踪(全部关掉,~MiB 级镜像收益):

```
# DEBUG_KERNEL / DEBUG_INFO / FTRACE / KASAN / UBSAN / KMSAN / KFENCE
# DEBUG_FS / MAGIC_SYSRQ / PROFILING / DEBUG_OBJECTS / DEBUG_KMEMLEAK
# PROVE_LOCKING / RUNTIME_TESTING_MENU / KGDB
DEBUG_INFO_NONE=y                 显式无 debug-info(默认即此,固化避免回归)
LOG_BUF_SHIFT=14                  16 KiB printk ring(默认 17=128 KiB,
                                  默认值 ~50% 的内核启动期 RSS 会变成
                                  跨实例不同的 printk 内容)
```

安全 / 强化(单 VM = 单 app 模型不需要 host kernel 级强化):

```
# AUDIT not set                   audit 子系统 ~MiB 镜像 + 跨实例不可去重
                                  的事件流
# SECURITY not set                LSM 框架(SELinux / AppArmor 等)
# INTEGRITY not set               IMA / EVM
# HARDENED_USERCOPY not set       userspace 拷贝边界检查
# FORTIFY_SOURCE not set          libc 强化(guest userspace 不依赖)
# LOCKUP_DETECTOR / SOFTLOCKUP    定时探测线程,跨实例 RNG seed 差异化
# DETECT_HUNG_TASK
```

## 4. 架构差异

### 4.1 启动协议

| 项 | x86_64 | aarch64 |
|---|---|---|
| 镜像格式 | ELF | PE(EFI) |
| 启动入口 | PVH(`PARAVIRT=y` + `XEN_PVH=y` + `PVH=y`)| EFI stub + ACPI(`EFI_STUB=y` + `ACPI=y`)|
| BIOS / 固件 | 无,CH 直接跳 ELF entry | 无,CH 加载 PE Image,跳 EFI stub entry |
| 启动结构 | zero-page 由 CH 填(memmap、cmdline) | FDT + ACPI 表由 CH 构造 |
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
| 早期控制台 | 8250(I/O port 0x3f8)/ virtio-console | PL011 AMBA UART(MMIO)/ virtio-console |
| Kconfig | (8250 内嵌核心) | `ARM_AMBA=y` + `SERIAL_AMBA_PL011=y` |

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
的 chVA 注册 uffd,接管所有 guest RAM 缺页。**guest 内**调用 `userfaultfd()`
对沙箱模型没有作用,反而增加内核 ~10 KiB 代码 + 几个跨实例非确定性的内部状态
(per-uffd ctx hash 等)。

需要在 guest 内做用户态 uffd 的应用(罕见——多是数据库自己管 page cache 的
场景)走"自带 vmlinux"路径。

### 5.2 为什么默认关 cgroups

平台模型是"一 VM 一 app",host cgroup v2 限制 CH 进程已经把资源边界划定。
guest 内再切 cgroup 没有意义,反而:

- 多一层资源记账,跨实例 systemd-cgroup 路径差异化
- guest 内 OOM 可能在 cgroup 边界先触发,触发链路与 host 期待的
  `deflate_on_oom` 不一致

systemd-style 服务管理或 nested 容器(podman / docker-in-docker)在沙箱模型下
不是支持目标——它们的工作流跟"短生命周期 + 快照恢复"模式正交。

### 5.3 为什么 IP_PNP 关闭

kernel 自身的 `ip=...` cmdline 自动配网走 dhcp/bootp 协议,在 sandbox 模型下
是反模式:网络配置已经由 sandbox-ctl 通过 vsock launch 协议显式下发给
sandbox-init,sandbox-init 用 raw netlink 配置。`ip_auto_config` initcall
~26 ms 净开销 + ~10 KiB dhcp/bootp 客户端代码,关掉直接收益。

### 5.4 为什么 NR_CPUS=4

平台 fixed-spec 把 capacity.cpu 限到 1/2/4 三档(详见 sandbox.md §资源模型)。
NR_CPUS=4 让 guest 内核数据结构(per-cpu / cpumask)按 4 核维度分配——
NR_CPUS=8/16 多余的 per-cpu 字段会让跨实例 RAM 多出一些低利用率脏页。

### 5.5 为什么不启用 free_page_reporting,以及 VIRTIO_MEM 的角色

`virtio-balloon free_page_reporting` 让 guest 把每轮 page reclaim 中的空闲页号
持续推到 host;host 端 CH 的 `release_memory_range` 在收到这些页号时,对自身
mmap 做 `madvise(MADV_DONTNEED)` 释放进程 PTE。在平台的统一 memfd / 外部 uffd
模型下,这条 madvise 会广播 mmu_notifier 失效到 KVM EPT,持续的 IPI shootdown
会饿死 guest vsock kthread(timer 中断丢失,host→guest ping 在十数秒内全部
timeout)。因此平台**关闭 free_page_reporting**——内核侧 `VIRTIO_BALLOON=y`
启用模块,但 CH 命令行不开 FPR feature。

替代路径:host 端 BalloonController 周期(默认 5 s)从 guest 拉取 mem_report
(MemAvailable/MemTotal,详见 sandbox-runtime.md §4.3),按反馈策略推
`PUT /api/v1/vm.resize` 改变 balloon target;guest balloon 驱动按 target inflate,
CH 在 inflate 处理路径里 `fallocate(PUNCH_HOLE) + madvise(DONTNEED)`。事件量
被反馈环 `MaxStep`(默认 ≤ 256 MiB/tick)限速,不会形成 IPI 风暴。

`VIRTIO_MEM=y` 仍保留作扩展点:virtio-mem 是 host 主动 → guest unplug 路径,
事件粒度大、批量少,适合 NUMA / 横向扩 zone 等场景。当前 v1 不依赖
virtio-mem 路径,但保留驱动让未来扩展无需重打 vmlinux。

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

跨实例 RAM 去重率(PROPOSAL §4):同 vmlinux + 同 sandbox-runtime + 同应用,
冷启动到 settled 的 RAM 内容跨实例 hash 相同区段应 > 90%。低于 50% 通常
是新启用的随机化(KASLR / SLAB 等)漏网,通过比对 `make olddefconfig`
diff 排查。

## 7. 维护

- 升级 kernel 主版本(6.1 → 6.x):review `sandbox-common.config` 是否有
  新引入的随机化/调试选项需要禁用;运行 `make olddefconfig` 看 silent
  regression
- 升级架构 fragment:review CH 对该 arch 的设备模型是否有新依赖(例如
  GICv4 / 新 RTC 驱动)
- 不上游化 defconfig:本配置选择跟通用 server distro 取向偏离明显
  (关闭 cgroups / userfaultfd / namespace 等),不寻求合并到 upstream

## 8. See Also

- [`cloud-hypervisor.md`](cloud-hypervisor.md) —— 平台 VMM 的启动协议、设备
  模型、patch 范围
- [`sandbox-runtime.md`](sandbox-runtime.md) —— 内核之上 sandbox-init 完成
  rootfs 组装与应用拉起
- [`sandbox.md`](sandbox.md) §boot.kernel —— 沙箱配置如何引用 vmlinux,
  以及自带 kernel 的接入方式
- [`build.md`](build.md) —— `make vmlinux` 工作流、交叉编译矩阵
- `PROPOSAL.md` §4 "确定性 Guest 配置" —— 跨实例 RAM 去重率目标的来源
