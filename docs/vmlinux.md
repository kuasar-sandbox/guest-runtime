[English](vmlinux.md) | [简体中文](vmlinux_zh.md)

<a id="vmlinux--guest-内核镜像"></a>

# vmlinux — guest kernel image

The platform supplies each sandbox with a **minimal, pinned-version** Linux kernel image (`bin/<arch>/vmlinux`). This document defines how that image is built, which features are enabled or disabled, and how those choices affect boot, devices, resource control and the guest security boundary.

Vmlinux is a platform artifact. Tenants do not receive a general kernel-version/configuration API. A deployment needing a different kernel can select its own vmlinux with `boot.kernel: file://...`; the same kernel-path mechanism passes it to Cloud Hypervisor. The custom kernel's boot protocol, required devices, guest ABI and snapshot compatibility must be validated with the selected sandboxer/runtime/VMM combination. The platform does not guarantee arbitrary custom-kernel functionality or compatibility.

<a id="1-概述"></a>

## 1. Overview

<a id="11-设计目标"></a>

### 1.1 Design goals

| Goal | Implementation |
|---|---|
| Small image and fast boot | Sparse platform defconfig with explicit required subsystems, resolved by Kbuild; HZ=100; a minimal console configuration |
| Inspectable version and ABI | Pin upstream version, LOCALVERSION, configuration fragments and platform patches; validate exact release assets |
| One VM = one application | Disable user/net/uts/ipc/time namespaces and in-guest userfaultfd; a complete nested container runtime is outside the supported target (§3.2) |
| Host-controlled guest memory | Enable virtio-balloon for host-driven inflation through vm.resize; retain the virtio-mem driver as an extension point |
| One rootfs construction path | virtio-pmem + DAX + read-only EROFS + writable ext4 + overlayfs |
| In-sandbox resource isolation and storage mounts | cgroup v2 freezer plus cpu/memory/io/pids controllers; application delegation is selected by launch.cgroup_control (§5.2); NFS v3/v4 client and FUSE (s3fs) mounts (§3.1) |

<a id="12-内核版本"></a>

### 1.2 Kernel version

- Upstream: `linux-6.1.169` (LTS), from `cdn.kernel.org`.
- Compiler: host gcc for native builds, or `${CROSS_PREFIX}gcc` for cross-builds.
- Optimization: `CC_OPTIMIZE_FOR_SIZE=y` (-Os), rather than -O2 for this boot-oriented preset.
- LOCALVERSION: `-sandbox`, making `uname -r` consistent for sandboxes using this pinned image and aiding diagnosis.

<a id="13-输出"></a>

### 1.3 Output

```text
bin/x86_64/vmlinux        ELF kernel, PVH boot protocol; size-optimized, without debug information
bin/aarch64/vmlinux       PE-format Image, EFI stub + ACPI boot
```

Both architectures use the filename `vmlinux`; the internal format differs by architecture. Cloud Hypervisor selects the matching supported boot path from the kernel format, while sandbox-ctl uses the same upper-level kernel-path interface. Measure the actual built/released file size; this document does not specify a universal image-size or boot-time result.

The trusted `Vmlinux Release` workflow on this repository's `main` independently publishes `vmlinux-vX.Y.Z` from the source branch and exact SHA pinned by the dispatcher. Its archive is `vmlinux-x86_64-vX.Y.Z.tar.gz`. This release line is independent of `runtime-vX.Y.Z`: their version numbers need not match, and the platform aggregate explicitly selects each. Source support for aarch64 does not mean an aarch64 release archive or equivalent runtime validation has been published.

<a id="2-构建工作流"></a>

## 2. Build workflow

`make vmlinux` invokes fetch, patch application and build when the output is missing or tracked kernel inputs are newer. Explicit `linux-fetch`, `linux-patches-apply`, `linux-patches-format` and `linux-build` targets use `native-deps/deps/build-vmlinux.sh`, with the phase selected by `STAGE`: run these native targets with `make -C native-deps <target>` from the repository root, or run `make <target>` inside native-deps.

```text
fetch         Download and verify linux-6.1.169.tar.gz, with optional shared TARBALL_CACHE;
              extract the cross-architecture shared source tree at build/src/linux;
              git init, commit the imported tree and tag linux-patches-base.
patches-apply Apply deps/linux-patches/*.patch with git am. The current platform patch
              makes virtio_balloon converge to a sustainable size under an infeasible
              host target rather than livelocking (§5.6). Already-applied patches are
              an idempotent no-op; unformatted or divergent work is not overwritten.
build         Concatenate sandbox-common.config + sandbox-<arch>.config into
              arch/<kbuild_arch>/configs/sandbox_defconfig; run make sandbox_defconfig
              and make olddefconfig to resolve dependencies, then make -j$(nproc)
              <kbuild_target> (x86_64: vmlinux ELF; arm64: Image PE), copying the output
              to bin/<arch>/vmlinux.
```

Incrementality is input-aware: the native Makefile tracks the build/common scripts, both selected fragments, the patch directory and tracked patches. An existing output alone does not suppress a rebuild after those inputs change. Every explicit `linux-build` re-resolves configuration; Kbuild compilation remains incremental. Removing the output or cleaning before `make vmlinux` forces regeneration.

**Patch development:** inside native-deps, `make linux-fetch` imports the source and creates `linux-patches-base`; edit `build/src/linux/` and commit; run `make linux-patches-format` to export commits into `deps/linux-patches/*.patch`; then run `make vmlinux` to apply/build. See [the native-build guide](../native-deps/README.md) §3 for idempotence and sanity rules. Patches are architecture-neutral and shared by x86_64 and arm64. Preserve unformatted work rather than resetting it merely to make a build proceed.

<a id="21-host-构建依赖"></a>

### 2.1 Host build dependencies

```text
bc bison flex make tar pkg-config gcc
libelf-dev / elfutils-libelf-devel
libssl-dev  / openssl-devel
```

Fetching and patch development also require Git and the download/archive tools listed in the native-build guide. Cross-building additionally requires `gcc-aarch64-linux-gnu` or `gcc-x86-64-linux-gnu`, according to direction.

<a id="3-配置体系"></a>

## 3. Configuration system

The defconfig is assembled from **two fragments** in `native-deps/deps/vmlinux/`:

```text
sandbox-common.config          Architecture-neutral subsystems, base structures and virtio
+ sandbox-<arch>.config        Architecture-specific boot, interrupt controller and console
= sandbox_defconfig            Final Kbuild input
```

The script runs `sandbox_defconfig` and `olddefconfig`; it does not run an `allnoconfig` target. Review the resolved `.config`, including Kconfig defaults and dependency closure, rather than assuming every omitted symbol is disabled.

<a id="31-关键启用项"></a>

### 3.1 Key enabled features

Subsystems and device drivers:

```text
PCI=y, PCI_MSI=y                CH exposes virtio-pci devices; MSI-X is the modern
                                virtio-pci interrupt path and must be enabled.
NETDEVICES=y                    Network-driver umbrella; missing dependencies can
                                silently remove requested VIRTIO_NET.
NET=y, INET=y                   Networking and AF_INET support; retain the dependencies
                                needed by AF_VSOCK and AF_NETLINK as well.
VIRTIO=y, VIRTIO_PCI=y          Virtio bus.
VIRTIO_BLK=y                    blk0/blk1: base disk and COW upper layer.
VIRTIO_NET=y                    eth0, connected to the host TAP.
VIRTIO_PMEM=y                   Mount sandbox-runtime.bundle through virtio-pmem.
VIRTIO_VSOCKETS=y               sandbox-init ↔ sandbox-ctl control plane.
VIRTIO_BALLOON=y                Host memory reclamation through vm.resize inflation;
                                compiled free-page reporting is not negotiated by the
                                platform's CH balloon configuration (§5.5).
VIRTIO_MEM=y                    Driver for host-requested memory-block unplugging.
LIBNVDIMM + ZONE_DEVICE +       Filesystem DAX on virtio-pmem maps host page-cache pages;
FS_DAX                          required resolved symbols are checked after olddefconfig.
EROFS_FS=y                      Read-only root filesystem.
EXT4_FS=y                       Writable overlayfs upper layer.
OVERLAY_FS=y                    Merge the EROFS lower and ext4 upper into the / view.
TMPFS=y                         Runtime mounts such as /tmp.
NETWORK_FILESYSTEMS=y           NFS umbrella; preserve dependencies so NFS_FS survives.
NFS_FS=y + NFS_V3 + NFS_V4(.1/.2) Guest NFS mounts; SUNRPC/LOCKD are selected dependencies.
                                KEYS=y supports v4 idmap; DNS_RESOLVER=y supports referrals.
FUSE_FS=y                       FUSE for s3fs and similar guest clients; devtmpfs creates
                                /dev/fuse. The sandbox image supplies mount.nfs/s3fs;
                                the kernel supplies the capability, not those tools.
```

Boot, time and scheduling:

```text
SMP=y, NR_CPUS=4                The bundled kernel supports up to four vCPUs. Without SMP,
                                NR_CPUS can fall back to one, losing the requested multi-vCPU topology.
X86_X2APIC=y (x86_64)           CH describes vCPUs with MADT type 9; without x2APIC support,
                                the guest can retain only its fallback boot CPU.
HZ_100, NO_HZ_IDLE              Low tick frequency; no periodic tick while idle.
HIGH_RES_TIMERS                 hrtimer support used by application epoll/timerfd.
HW_RANDOM=y, RANDOM_TRUST_CPU=y Trust CPU entropy rather than relying only on slower
                                entropy collection during startup; measure actual timing.
RTC_CLASS=y, RTC_HCTOSYS=y      Synchronize the clock at boot.
PARAVIRT=y, KVM_GUEST=y         x86_64 paravirtualized TLB/spinlock optimizations.
```

Namespaces used by application isolation include PID, mount and cgroup namespaces:

```text
NAMESPACES=y, PID_NS=y          sandbox-init creates PID/mount isolation so the primary
                                application sees itself as PID 1.
CGROUPS=y                       Application cgroup namespace rooted at real /app (§5.2).
# UTS_NS, TIME_NS, IPC_NS,       Additional nested isolation is not enabled by this
# USER_NS, NET_NS not set       one-VM/one-application preset.
```

cgroup v2 freezer and cpu/memory/io/pids controllers:

```text
CGROUPS=y                       cgroup v2 hierarchy, including its core freezer;
                                sandbox-init freezes the recursive application subtree
                                before quiesce and thaws after restore initialization.
MEMCG=y                         memory.{high,max,min,low}.
CGROUP_SCHED + FAIR_GROUP_SCHED cpu.weight; CFS_BANDWIDTH supplies cpu.max.
BLK_CGROUP + BLK_CGROUP_IOCOST  io.weight via the io.cost model;
                                BLK_DEV_THROTTLING supplies io.max.
CGROUP_PIDS=y                   pids.max.
                                With launch.cgroup_control=true, sandbox-init delegates
                                available controllers to an empty real /app; managed
                                application processes start in /app/init and user child
                                cgroups stay below /app. Host cgroup/balloon authority
                                still bounds the VM (§5.2).
# CGROUP_FREEZER (legacy v1) / RT_GROUP_SCHED / CPUSETS / CGROUP_DEVICE /
# CGROUP_PERF / CGROUP_BPF / CGROUP_HUGETLB / CGROUP_MISC / NET_CLS /
# NET_PRIO not set              Unused by the preset.
```

<a id="32-当前最小配置的禁用项"></a>

### 3.2 Features disabled by the current minimal configuration

The platform preset does not enable the following randomization or hardening options. This is a fact about the shipped configuration and an explicit security tradeoff, not a prerequisite for template-parent sharing or snapshot restore:

```text
# RANDOMIZE_BASE not set         KASLR is not enabled by the current preset.
# RANDOMIZE_MEMORY not set       No randomized kernel physical-map offset on x86_64.
# SLAB_FREELIST_RANDOM not set   No freelist randomization.
# SLAB_FREELIST_HARDENED not set No freelist hardening.
# SHUFFLE_PAGE_ALLOCATOR not set No page-allocator shuffling.
```

These mechanisms can strengthen guest-kernel defense in depth. Production deployments must evaluate the preset against their threat model. If different hardening, auditing or guest features are required, select a validated custom kernel through `boot.kernel: file://...`. Snapshot reuse depends on explicit parent relationships; disabling these mechanisms is not a way to obtain cross-VM memory deduplication.

The repository's `scripts/guest-inspect.py` currently uses fixed, nonrandomized `KERNEL_IMAGE_BASE` and `PAGE_OFFSET` values. It therefore supports only kernels with both CONFIG_RANDOMIZE_BASE and CONFIG_RANDOMIZE_MEMORY disabled. A custom kernel enabling them cannot use that script unchanged; the script must first learn to discover runtime relocation. This is a debugging-tool constraint, not a snapshot-restore constraint.

The preset also disables CONFIG_SLUB_CPU_PARTIAL. That option controls per-CPU partial slabs and trades allocator performance against memory footprint; it is not described as a security-hardening feature.

Unused subsystems are disabled to reduce image and runtime footprint:

```text
# MODULES not set                 One vmlinux artifact, without loadable modules.
# SOUND / DRM / FB / INPUT / HID  No display/audio/input stack.
# USB_SUPPORT / I2C / SPI / GPIO  No peripheral-device stack.
# WATCHDOG / THERMAL / CPU_FREQ   Host-managed; guest sees its configured capacity.
# SCSI / ATA / NVME / LOOP        Block devices use virtio-blk.
# MD / BCACHE                     No software RAID/cache layer.
# BTRFS / F2FS / XFS / CIFS /     Local filesystems use EROFS, ext4, tmpfs and overlayfs;
# VFAT / NTFS / AUTOFS4           network/object mounts use NFS and FUSE (§3.1).
# HUGETLBFS                       Guest HugeTLBFS is outside the current 4 KiB preset.
# BRIDGE / VLAN / BONDING / TUN   No in-guest L2 bridge stack.
# VETH / VXLAN / GENEVE / WLAN
# IP_PNP                          sandbox-init configures networking through netlink,
                                  not a kernel ip=... DHCP/BOOTP path.
# BPF_JIT not set                 BPF_SYSCALL=y; current guest workloads do not require
                                  JIT. Applications needing JIT require a validated kernel.
```

Platform ABI boundaries: disabled features are unavailable to guest applications:

```text
# USERFAULTFD not set             sandbox-ctl uses userfaultfd on the host backing memfd;
                                  an in-guest userfaultfd() call returns ENOSYS.
# UTS_NS / TIME_NS / IPC_NS /     Additional nested isolation is not enabled.
# USER_NS / NET_NS not set
NUMA not set                      Guest NUMA support is disabled; this alone does not
                                  imply that the kernel has only one memory zone.
```

Heavy debugging and tracing facilities are disabled. Actual binary-size savings depend on the build and are not a release guarantee:

```text
# DEBUG_INFO / FTRACE / KASAN / UBSAN / KMSAN / KFENCE / DEBUG_FS
# PROFILING / DEBUG_OBJECTS / DEBUG_KMEMLEAK / PROVE_LOCKING
# RUNTIME_TESTING_MENU / KGDB
DEBUG_INFO_NONE=y                 Explicitly omit debug information to prevent regression.
LOG_BUF_SHIFT=14                  Base printk ring shift, with the SMP contribution also
                                  controlled by LOG_CPU_MAX_BUF_SHIFT=12.
```

A lightweight diagnostic set remains enabled and is explicitly marked temporary in the configuration. These detectors print diagnostics; the preset does not enable their panic options:

```text
DEBUG_KERNEL=y                    Kconfig umbrella required by diagnostics such as hung tasks.
DETECT_HUNG_TASK=y                Report tasks stuck in D state after 20 seconds, with
                                  lock/I/O wait stacks.
LOCKUP_DETECTOR=y                 Detect CPUs stuck in kernel execution, with SOFTLOCKUP_DETECTOR.
WQ_WATCHDOG=y                     Detect stalled workqueues.
PSI=y                             Quantify memory and I/O pressure.
MAGIC_SYSRQ=y                     On-demand sysrq-t/-w/-m.
```

Auditing and security frameworks currently disabled:

```text
# AUDIT not set                   No guest audit subsystem in the shipped kernel.
# SECURITY not set                No LSM framework such as SELinux/AppArmor.
# INTEGRITY not set               No IMA/EVM.
# HARDENED_USERCOPY not set       No hardened userspace-copy boundary checking.
# FORTIFY_SOURCE not set          No kernel fortify checks.
```

<a id="4-架构差异"></a>

## 4. Architecture differences

<a id="41-启动协议"></a>

### 4.1 Boot protocol

| Item | x86_64 | aarch64 |
|---|---|---|
| Image format | ELF | PE (EFI) |
| Boot entry | PVH (`PARAVIRT=y` + `XEN_PVH=y` + `PVH=y`) | EFI stub + ACPI (`EFI_STUB=y` + `ACPI=y`) |
| BIOS / firmware | None; CH enters the ELF kernel directly | CH loads the PE Image using the configured EFI-stub boot path |
| Boot structures | CH supplies the zero page, memory map and command line | CH constructs FDT and ACPI tables |
| Secondary-CPU startup | APIC INIT-SIPI | PSCI (`ARM_PSCI=y`), rather than x86 INIT-SIPI |
| Kernel-image subpath | `vmlinux` | `arch/arm64/boot/Image` |
| Make target | `vmlinux` | `Image` |

PVH lets x86_64 skip BIOS/PXE; the arm64 fragment enables EFI-stub/ACPI support. Boot latency must be measured with the exact image, VMM, hardware, workload and timing boundary. The configuration alone does not establish a 50 ms, 80 ms or sub-100 ms boot guarantee.

<a id="42-中断控制器"></a>

### 4.2 Interrupt controllers

| Item | x86_64 | aarch64 |
|---|---|---|
| Controller | APIC + IO-APIC + MSI-X | GICv3 + ITS |
| Kconfig | `PCI_MSI=y`, with APIC support | `ARM_GIC_V3=y` + `ARM_GIC_V3_ITS=y` |
| virtio-pci interrupt path | MSI-X | ITS |

<a id="43-串口--控制台"></a>

### 4.3 Serial ports and console

| Item | x86_64 | aarch64 |
|---|---|---|
| UART driver | None; 8250 is not built in | PL011 AMBA UART, MMIO |
| virtio-console (hvc) | `VIRTIO_CONSOLE=y` | `VIRTIO_CONSOLE=y` |
| Kconfig | — | `ARM_AMBA=y` + `SERIAL_AMBA_PL011(_CONSOLE)=y` |

**Runtime console:** the default CH command uses `--console tty --serial off`, with `console=hvc0` supplied in the kernel command line, so kernel dmesg uses virtio-console on both architectures. A deployment may select the sandbox-ctl console destination, including journald or file output, without changing the guest hvc0 interface. The x86_64 preset builds no UART driver; aarch64 includes PL011, which CH exposes there, for early-boot/debug use. Application stdin/stdout/stderr use vsock rather than a console device; see [sandbox-init.md](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init.md) §3.5 / §4.5.

### 4.4 RTC

x86_64 uses CH's CMOS RTC (`RTC_DRV_CMOS=y`); aarch64 uses the MMIO PL031 (`RTC_DRV_PL031=y`). The arm64 device model does not use a PC-compatible CMOS RTC.

<a id="45-页大小"></a>

### 4.5 Page size

Arm64 kernel configurations can use 4 KiB, 16 KiB or 64 KiB pages. The **bundled platform preset explicitly selects 4 KiB**. Host userfaultfd operations act on host memory; the platform's current host handler and artifact paths use 4 KiB units. Do not infer compatibility with a different guest page size solely from a successful kernel build. A 16 KiB/64 KiB custom guest kernel is outside this preset and requires end-to-end validation with the selected host, VMM and snapshot path.

```text
CONFIG_ARM64_4K_PAGES=y           Explicitly pin 4 KiB.
# CONFIG_ARM64_16K_PAGES not set
# CONFIG_ARM64_64K_PAGES not set
```

The x86_64 base page size is 4 KiB and has no equivalent preset choice.

<a id="46-pci-拓扑"></a>

### 4.6 PCI topology

| Item | x86_64 | aarch64 |
|---|---|---|
| Host-bridge discovery | MMCONFIG / MCFG ACPI table | Generic ECAM through ACPI/DT |
| Kconfig | `PCI_MMCONFIG=y` | `PCI_HOST_GENERIC=y` + `PCI_ECAM=y` |

<a id="5-关键决策"></a>

## 5. Key decisions

<a id="51-为什么默认关-userfaultfd"></a>

### 5.1 Why USERFAULTFD is disabled by default

`userfaultfd` is a platform **host-side** facility: sandbox-ctl registers host virtual addresses of the memfd backing guest RAM and handles their faults. Current platform workloads do not need to call `userfaultfd()` **inside the guest**, so the minimal preset does not expose that API.

Applications needing in-guest userfaultfd, such as applications managing their own page cache, must use a validated custom vmlinux.

<a id="52-cgroup-v2freezer--cpumemoryiopids-控制器"></a>

### 5.2 cgroup v2: freezer and cpu/memory/io/pids controllers

`CONFIG_CGROUPS=y` supplies the cgroup v2 hierarchy. The platform uses two aspects of it.

**Freezer, part of the core:** before snapshot quiesce, sandbox-init must freeze the complete application subtree and confirm `cgroup.events:frozen 1`. It thaws only after restore initialization, including wall-clock repair, is ready. Otherwise `/vm.resume` can restart vCPUs before the guest processes `restore`, allowing application code to run with stale time or a disconnected MUX. The freezer mechanism is documented in [sandbox-init.md](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init.md) §3.4. The v2 freezer (`cgroup.freeze`, Linux ≥5.2) is part of CONFIG_CGROUPS; the separate CGROUP_FREEZER option is the legacy v1 freezer and is unnecessary.

**Resource controllers:** MEMCG; CGROUP_SCHED + FAIR_GROUP_SCHED + CFS_BANDWIDTH; BLK_CGROUP + BLK_CGROUP_IOCOST + BLK_DEV_THROTTLING; and CGROUP_PIDS provide guest `cpu.weight`, `cpu.max`, `memory.{high,max,min,low}`, `io.weight`, `io.max` and `pids.max`. An E2B application such as envd can create per-process child groups for ptys/socats/user workloads when application delegation is enabled. If a controller is absent, its interface files are unavailable; compiling it is not enough, because it must also be enabled through `cgroup.subtree_control` before descendants receive its interfaces.

The [Guest ABI](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init.md#321-application-cgroup-namespace-and-controller-topology) completely defines the application cgroup namespace, `/app` freeze domain, delegation and process/FD lifetime. The kernel must provide the required controllers and namespaces. With `launch.cgroup_control` enabled, the ABI requires strict enablement/verification of advertised controllers rather than treating missing capabilities as success.

Guest controllers **subdivide the VM's budget**; they do not replace host authority. Host cgroup v2 limits the CH process and balloon control governs available guest memory. Leaving guest child `memory.max=max` does not impose a separate child maximum, but it also does not prevent guest OOM when guest capacity is exhausted, or guarantee that host `deflate_on_oom` acts first. Validate guest application completion and guest memory pressure separately from host cgroup OOM counters.

A complete nested container runtime, such as podman or Docker-in-Docker, remains outside the supported target. Such workflows need additional namespaces including user_ns and net_ns, disabled by this preset (§3.2), and differ from the platform's short-lived, snapshot-oriented model.

<a id="53-为什么-ip_pnp-关闭"></a>

### 5.3 Why IP_PNP is disabled

The kernel `ip=...` command-line path performs DHCP/BOOTP autoconfiguration. The sandbox already receives explicit networking configuration from sandbox-ctl over the vsock launch protocol, and sandbox-init applies it with raw netlink. The preset therefore omits the redundant kernel autoconfiguration path. Measure any startup-time and binary-size effect on the exact build instead of treating historical estimates as universal savings.

<a id="54-为什么-nr_cpus4"></a>

### 5.4 Why NR_CPUS=4

The bundled kernel sets NR_CPUS=4 so its CPU masks and per-CPU structures target at most four vCPUs. Larger values can increase structures and the fixed memory footprint unused by small guests.

This is a **kernel-preset limit**, not a platform API rule restricting `resources.capacity.cpu` to the set 1/2/4. The current sandbox configuration validates a positive CPU count; it does not impose that three-value enum. Requested capacity still must be supported by the selected kernel and VMM. A configuration accepted by the parser is not proof that the bundled four-CPU kernel can run it. See [sandbox.md](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox.md) §4 for Capacity and resource semantics.

<a id="55-为什么不启用-free_page_reporting以及-virtio_mem-的角色"></a>

### 5.5 Why free_page_reporting is not negotiated, and the role of VIRTIO_MEM

`virtio-balloon free_page_reporting` lets a guest continuously report free pages to the host during reclaim. CH's `release_memory_range` can respond with `madvise(MADV_DONTNEED)` on its mapping. In the platform's unified memfd / external-uffd path, invalidation propagates through mmu_notifier to KVM mappings. The platform's reported failure mode involved repeated invalidation/IPI activity and guest vsock starvation with host-to-guest ping timeouts. This is the rationale for **not negotiating free_page_reporting in the platform CH balloon configuration**; it is not a universal timing or failure guarantee.

The distinction is explicit: the kernel fragment contains CONFIG_VIRTIO_BALLOON=y, CONFIG_VIRTIO_BALLOON_FREE_PAGE_REPORTING=y and CONFIG_PAGE_REPORTING=y. Compiled capability does not mean the host advertises/enables the feature at runtime.

The kernel supplies capabilities, not node resource policy. [Guest ABI](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init.md) owns `mem_report` and its fields; [sandboxer resource control](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox.md#93-balloon) owns target/current, growth/shrink, reservation release and fresh-report conditions; the [VMM patch](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/cloud-hypervisor.md#34-0004--skip-hole-only-runs-during-balloon-release) owns sparse-range release. `/proc/meminfo`’s `Balloon:` is not an input in this Guest ABI: current comes from CH `memory_actual_size`, and Capacity must not be inferred from guest MemTotal.

`VIRTIO_MEM=y` is retained as an extension point for host-driven guest memory-block unplugging. Its coarse, less frequent events can support separately designed memory-hotplug/NUMA scenarios. The platform's current fixed-capacity Budget model rejects memory hotplug and does not use virtio-mem. Retaining the driver is only an extension point; enabling a new hotplug design would also require corresponding host support and end-to-end validation.

<a id="56-为什么打-virtio_balloon-收敛补丁depslinux-patches"></a>

### 5.6 Why the virtio_balloon convergence patch is applied

Host BalloonController updates vm.resize targets using feedback (§5.5), and a requested target can temporarily be **infeasible**: for example, the startup working set still occupies more pages than the guest can release. In the failure scenario addressed by the patch, the stock inflate path repeatedly fails allocation (`Out of puff`) and retries the same target while deflate_on_oom releases recently inflated pages under pressure. Competing inflate/deflate activity can prevent useful convergence while consuming CPU and generating mapping invalidations.

The platform patch instead enforces **convergence**. When inflation cannot allocate pages, the driver records the sustainable balloon size minus a safety margin as a **sticky ceiling** and actively deflates to it. Further pressure can only tighten that ceiling downward. This is emergency guest protection, not a Budget change, and it does not automatically restore the host target. If CH memory_actual_size shows target/current disagreement, host control treats the phase as unstable: it does not continue shrinking, release reservation or discard the configured memory.high soft guarantee. Ordinary growth can still give the guest more memory by lowering the balloon target.

The patch is guest-side robustness work and does not change the host/guest protocol. It reduces the identified convergence failure; it is not a guarantee that arbitrary guest workloads cannot OOM. Accounting and subsequent control are described in §5.5 and sandbox.md §9.3. Patch sources are in [native-deps/deps/linux-patches](../native-deps/deps/linux-patches/).

<a id="6-验证"></a>

## 6. Validation

Basic post-build sanity:

```bash
file bin/x86_64/vmlinux
# bin/x86_64/vmlinux: ELF 64-bit LSB executable, x86-64, ...

file bin/aarch64/vmlinux
# bin/aarch64/vmlinux: PE32+ executable (EFI application) Aarch64, ...

# Runtime checks inside a sandbox, using sandbox-init's early logs:
# uname -r should be 6.1.169-sandbox
# /proc/version must not contain the build host's hostname or username
# /proc/config.gz should be absent because IKCONFIG is disabled
```

The trusted `release-vmlinux.yml` workflow builds and validates the **exact selected source-branch SHA**, which can belong to main or a supported maintenance branch. It exercises native-dependency tooling and package validation, but does not boot vmlinux itself. A successful component Release therefore is not sufficient evidence of guest behavior. Before aggregation, retain platform integration-test results for the matching exact source combination. Published aggregate assets additionally require complete real-MicroVM E2E covering boot protocol, required devices, filesystems, networking, balloon, cgroup and snapshot/restore paths. Stable and Preview aggregates both use the exact-asset integration-test gate.

For configuration review, inspect the resolved olddefconfig diff for silent Kconfig changes. Do not use the proportion of identical RAM bytes across instances as a release gate.

<a id="7-维护"></a>

## 7. Maintenance

- When upgrading the kernel line, such as 6.1 to another 6.x release, review sandbox-common.config features, security choices and diagnostics, and inspect olddefconfig output for silent regressions. Rebase `deps/linux-patches/*.patch` onto the new source: create the new imported baseline with linux-fetch, preserve and rebase/reapply the working commits in build/src/linux, then export with linux-patches-format. Resolve conflicts between the convergence patch (§5.6) and the new virtio_balloon driver.
- When changing an architecture fragment, review whether CH's device model for that architecture adds dependencies such as GICv4 or another RTC driver.
- The project does not seek to upstream this defconfig as a general server-distribution configuration. Its choices intentionally differ: guest userfaultfd is disabled, namespaces are trimmed, and cgroups retain the freezer plus the cpu/memory/io/pids controllers needed by sandbox workloads.

## 8. See also

- [sandboxer/docs/cloud-hypervisor.md](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/cloud-hypervisor.md): VMM boot protocols, device model and patch scope.
- [sandbox-runtime.md](sandbox-runtime.md): packaging and layout of the runtime image above this kernel; [sandboxer/docs/sandbox-init.md](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init.md) owns rootfs assembly, application startup and the guest ABI.
- [sandboxer/docs/sandbox.md](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox.md) §3.1 defines `boot.kernel` references. A custom kernel must satisfy this document's capabilities, the selected Guest ABI and VMM contracts, and the validation in §6; [artifact incompatibility](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-artifacts.md#144-incompatibility) separately describes rejected snapshot formats.
- [Native-build workflow](../native-deps/README.md): make vmlinux, patch development and cross-compilation.
- [Project system overview](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/kuasar-sandbox.md) §4: template parents and stateful pause/resume semantics.
