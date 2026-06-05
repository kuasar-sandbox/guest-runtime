# cloud-hypervisor — VMM 与平台 patches

平台用 cloud-hypervisor(CH)作为 microVM 监视器。绝大多数路径跑 upstream
行为,只有快照路径与外部托管内存通过我们维护的 3 个 patch 进入 sandbox-ctl
提供的 memfd + uffd 模型。本文档定义 patch 范围、构建工作流、以及外部托管
内存模式的行为契约。

## 1. 概述

### 1.1 为什么需要 patch

平台对 VMM 有两条非 upstream 的诉求:

1. **外部托管 sandbox RAM**:host 进程(sandbox-ctl)持有 memfd inode,CH 通过
   继承 fd 把同一 inode mmap 到自己地址空间——而不是 CH 自己 `memfd_create`。
   这样 sandbox-ctl 拥有内存所有权,快照时直接通过 SEEK_DATA/HOLE 扫驻留页,
   绕过 CH→file→sandbox-ctl 的中转
2. **外部 uffd handler**:sandbox-ctl 接管 sandbox RAM 缺页,实现按需从快照
   或零页填充。CH 自己创建 uffd(必须绑到 CH mm),通过 SCM_RIGHTS 把 fd 传
   给 sandbox-ctl 的 handler

这两点 upstream CH 都不直接支持。第二条尤其本质——upstream uffd handler 模型
是 CH 调外部 socket,handler 提供数据,但平台需要 handler 接管 fault 投递
路由,upstream 的 listener 模型不够。

### 1.2 patch 范围(总结)

| 文件 | 改动行数 | 内容 |
|------|----------|------|
| `vmm/src/vm_config.rs` | ~19 | `MemoryZoneConfig` 新增 `fd` / `uffd_socket` 字段 |
| `vmm/src/config.rs` | ~9 | `MemoryConfig::parse` 增加两个 key 解析 |
| `vmm/src/memory_manager.rs` | ~338 | fd 注入 + user_managed skip + create_ram_region 内创建 uffd + va_report sendmsg(SCM_RIGHTS) + UFFDIO_REGISTER |
| `vmm/src/seccomp_filters.rs` | ~17 | allowlist `userfaultfd` syscall + `UFFDIO_API` / `UFFDIO_REGISTER` ioctl |
| `virtio-devices/src/balloon.rs` | ~38 | balloon release 对 user-managed zone 的空洞 run 跳过 `PUNCH_HOLE`/`MADV_DONTNEED`(`SEEK_DATA` 探测)|
| `virtio-devices/src/seccomp_filters.rs` | ~8 | balloon 线程 seccomp 放行 `SYS_lseek`(skip-hole 探测所需)|

总计 ~429 行 Rust（约数）+ 4 个 cohesive commits。基于 cloud-hypervisor `v51.1`。

### 1.3 维护策略

- patch 文件位置:`deps/ch-patches/000{1,2,3,4}-*.patch`
- 应用方式:`make ch-patches-apply`(在 `make cloud-hypervisor` 内自动调)
- 跟 upstream rebase:每个 CH 大版本(~3 月)review 一次,几行 conflict
  人工 fix
- **不**尝试上游化:patch 设计选择(SCM_RIGHTS in-process + create_ram_region
  里跑 ioctl)与 upstream 风格偏差明显,本项目维持自有 fork

## 2. 启用条件与命令行

CH `--memory-zone` 增加两个 key:

| key | 类型 | 含义 |
|-----|------|------|
| `fd` | int | 由父进程通过 cmd.ExtraFiles 注入的文件描述符,用作 zone 的 backing |
| `uffd_socket` | path | 普通 UDS 路径,CH 在该 socket 上 sendmsg(va_report; SCM_RIGHTS=uffd_C) |

行为契约:

- **不写 fd**(也不写 uffd_socket):完全等同 upstream 行为(memfd_create 自管 RAM,
  无 uffd)。所有不依赖外部托管的调用者(测试用例、其他工具)无须改动
- **仅写 fd**:CH 把 fd 当 backing,跳过 memfd_create;snapshot 时跳过 dump;
  restore 时不 fill。这条独立成立(无 uffd 仍可外部托管)
- **fd + uffd_socket 同时写**:在 fd 行为基础上叠加,CH 内 `userfaultfd()`
  → `UFFDIO_API` → `UFFDIO_REGISTER MISSING on chVA` → `connect uffd_socket`
  → `sendmsg(va_report; SCM_RIGHTS=uffd_fd)` → 等 ack。ack 之前 vCPU 不允许
  跑

平台 sandbox-ctl 调用形式(冷启动 + 恢复完全相同):

```
cloud-hypervisor \
  --memory-zone size=8G,shared=on,fd=3,uffd_socket=/run/sandbox/<sid>/uffd.sock \
  ...
# fd=3 ← memfd from sandbox-ctl via cmd.ExtraFiles[0]
```

详细命令行(冷启动 / 恢复)见 [`sandbox.md`](sandbox.md) §冷启动 与 §恢复。

## 3. patch 提交结构

### 3.1 0001 — externally allocated memfd-backed memory zone

主题:**memory: support externally allocated memfd-backed memory zone**

改动文件:`vm_config.rs` / `config.rs` / `memory_manager.rs`

要点:

- `MemoryZoneConfig` 新增 `pub fd: Option<i32>`(序列化时 `serde(skip)`,因为
  fd 不能跨 serialize)
- `MemoryConfig::parse` 识别 `fd=N` 形式,调 `File::from_raw_fd(N)`,**不**
  关闭原 fd(由调用方 ExtraFiles 管理生命周期)
- `MemoryManager::new` / `create_ram_region` 看到 `zone.fd.is_some()`:
  - 跳过 `memfd_create`,直接 `mmap(NULL, size, PROT_RW, MAP_SHARED, fd, 0)`
  - 在 `MemoryZone` 上标记 `user_managed: true`
- 多 region per zone(x86_64 zone > 3 GiB 时跨 PCI hole 切两段):
  每 region `try_clone()` 一份 fd,各自 `File` ownership;mmap 对同一 inode
  产生多个 VMA,共享 host page cache

`Transportable::send` 与 `fill_saved_regions` 都遍历 `snapshot_memory_ranges`
表,**这两处不需要单独 patch**——靠 commit 0002 在 `memory_range_table` 处过滤
即可同时影响快照和恢复路径。

### 3.2 0002 — snapshot skip user-managed memory zones

主题:**snapshot: skip user-managed memory zones**

改动文件:`memory_manager.rs`

要点:

- `memory_range_table` 在生成 `snapshot_memory_ranges` 时检测每个 zone 的
  `user_managed` 标志,user_managed=true 的 zone 在表中**不出现**
- 这样 `Transportable::send`(快照 dump 路径)看到的 ranges 不含 user-managed
  zone,自然 `Ok(())` 即返回——CH **不**写 8 GiB memory-ranges 文件
- 同理 restore 路径 `fill_saved_regions` 看到的 ranges 表不含此 zone,
  fill 操作变成空操作——guest 物理地址通过 mmap 直接映到 sandbox-ctl 的 memfd
  inode,内容由 sandbox-ctl 的 uffd handler 按需提供

upstream 已有"file-backed + MAP_SHARED + hardlink"的等价 skip 分支(用于
share-storage live migration 场景)。我们的 user_managed zone 满足"file-backed
+ MAP_SHARED",但 memfd 没有 hardlink(`st_nlink == 0`),所以不能复用现有
分支——添加平行分支识别 `user_managed` 标志。

### 3.3 0003 — external uffd handler via in-process create + SCM_RIGHTS

主题:**memory: external uffd handler via in-process create + SCM_RIGHTS handoff**

改动文件:`memory_manager.rs` / `seccomp_filters.rs`

要点:

- `create_ram_region` 在 mmap 完成后,如果 zone 配了 `uffd_socket`:

  ```
  uffd = userfaultfd(O_CLOEXEC | O_NONBLOCK)
  UFFDIO_API features = UFFD_FEATURE_MISSING_SHMEM
                      | UFFD_FEATURE_EVENT_REMOVE
                      | UFFD_FEATURE_EVENT_UNMAP
                      | UFFD_FEATURE_THREAD_ID
  UFFDIO_REGISTER(uffd, [chVA, +size], MISSING)
  conn = connect(uffd_socket)
  sendmsg(conn, iov=va_report{chVA, size}, cmsg=SCM_RIGHTS([uffd]))
  recvmsg(conn, expect ack)
  // 回到 create_ram_region 主流程,继续 vCPU 启动
  ```

- feature bits **必须严格按内核头位置**:

  ```
  UFFD_FEATURE_MISSING_SHMEM = 1 << 5
  UFFD_FEATURE_EVENT_REMOVE  = 1 << 3
  UFFD_FEATURE_EVENT_UNMAP   = 1 << 6
  UFFD_FEATURE_THREAD_ID     = 1 << 8
  ```

  位置写错会被内核解读成别的 feature。例如 `1 << 1` 是 `EVENT_FORK`,要求
  `CAP_SYS_PTRACE`,导致 UFFDIO_API 返回 EPERM。

- seccomp 放行:`SYS_userfaultfd` syscall + `UFFDIO_API` / `UFFDIO_REGISTER`
  ioctl(filter 在 `seccomp_filters.rs`)

**为什么 uffd 必须 CH 内创建**:Linux 内核把 uffd 上下文绑到 `userfaultfd()`
调用进程的 mm。sandbox-ctl 创建的 uffd 上 `UFFDIO_REGISTER` 只能 register
sandbox-ctl mm 的 VMA,不能 register CH mm 的 chVA。所以 uffd 必须在 CH 内
创建,然后通过 SCM_RIGHTS 把 fd(而非内核内部对象)传给 sandbox-ctl 让它读
事件。fd 表是进程级的,但 uffd ctx 的事件路由按创建者 mm 的 VA 解析,
sandbox-ctl 收到 fd 后读出来的事件 va 就是 CH 视角的 chVA。

### 3.4 0004 — balloon release 跳过 user-managed zone 的空洞 run

主题:**virtio-devices: balloon — skip PUNCH_HOLE/MADV_DONTNEED on already-sparse
user-managed ranges**

改动文件:`virtio-devices/src/balloon.rs` / `virtio-devices/src/seccomp_filters.rs`

**背景**。guest balloon 驱动充气分配的页**从不被 guest 写入**:
`balloon_page_alloc` 不带 `__GFP_ZERO`,平台 guest 内核未启用 `init_on_alloc`,
fill 路径只动 struct page 元数据与 PFN 数组。又因 user-managed zone 从不
prefault(memfd 稀疏,内容由 uffd handler 按需填),balloon 让出的 offset
绝大多数(冷启动时**全部**)在 memfd 上本就是空洞——无 inode 页,CH 与
sandbox-ctl 都无 PTE。

但 CH 的 balloon inflate 处理对每个让出 run 仍调 `release_memory_range`:
`fallocate(PUNCH_HOLE|KEEP_SIZE)` on memfd + `madvise(MADV_DONTNEED)` on chVA。
后者落在 uffd 注册的 chVA 上,内核**无条件**(与该 run 是否驻留无关)合成
一条 `EVENT_REMOVE`,且 `MADV_DONTNEED` **同步阻塞**到外部 handler 消费完
该事件才返回。冷启动充气覆盖整个 `capacity − allocatable` 区间时,这是一场
"对从未存在的页"的空 `EVENT_REMOVE` 风暴:压垮 handler 单 reader,并反压
CH 的 balloon 线程。`EVENT_REMOVE` 的真正来源是 chVA 上的 `MADV_DONTNEED`
(不是 memfd 的 fallocate)——所以**必须同时跳过两者**才能不发事件。

**要点**:

- `release_memory_range` 对**有 file_offset 的 region**(即 user-managed
  fd-backed zone)在动作前,用一次 `lseek(fd, file_off, SEEK_DATA)` 探测目标
  `[file_off, file_off + len)`:
  - `ENXIO`,或返回的下一数据字节偏移 `≥ file_off + len` ⇒ 整段空洞 ⇒
    **跳过 fallocate 与 madvise,直接 `Ok(())`**
  - 段内有数据 ⇒ 维持原逻辑(`PUNCH_HOLE` + `MADV_DONTNEED` 覆盖整 run)
  - `lseek` 其他错误不吞:落回原路径,不掩盖
- 仅作用于 `region.file_offset().is_some()` 的 zone。无 fd 的 upstream 匿名
  zone 无可探测,**行为完全不变**(契约同 §6:不写 `fd=` 等同 upstream)
- 该 zone 的 memfd fd 在 CH 内仅经 mmap + `fallocate`(显式 offset)使用,
  无定位读写,故 `lseek` 移动文件位置无副作用
- **seccomp 放行**:balloon device 线程的 seccomp 仅允许 `fallocate`
  (madvise 来自 virtio 公共集),新增 `SYS_lseek`;否则首次探测即 `SIGSYS`
  杀死 balloon 线程,CH 退出(filter 在 `virtio-devices/src/seccomp_filters.rs`,
  与 §3.3 放行 `userfaultfd` 同理)

**正确性**:跳过只命中真空洞(无可释放物);任何**确需回收**的页必有数据,
永不被跳过。被抑制的 `EVENT_REMOVE` 本只驱动 handler 对 backendVA 的
process-level reclaim,而空洞 offset sandbox-ctl 也从未 fault → 那一步本就
是 no-op。跳过前后 host 内存终态完全一致。

**效果与取舍**:改动前 balloon 充满 `capacity − allocatable` 时,**每个 4K
页**(`pbp` 在 x86-4K 被旁路)都对 uffd VMA 做 `MADV_DONTNEED`,而该调用
**同步阻塞**到外部单 reader handler 消费完 `EVENT_REMOVE` 才返回——
`≈ 充气字节 / 4K` 次串行跨进程往返,正是数十秒收敛(及偶发 boot 软死锁)
的根因。改动后冷启动充气页**实测 ~99% 是从未触碰的空洞**,`lseek(SEEK_DATA)`
探测后跳过 (1)(2):无 madvise → 无同步握手 → balloon 线程以内存速度扫过
→ **收敛近乎瞬时**。剩 ~1% 是 guest 启动期经 vhost-blk / 内核进过 page
cache 又释放、folio 仍驻留 memfd 的页,`lseek` 见数据**不跳过**,照常回收
——有界合法,行为同 upstream。guest 退出时整 zone unmap 产生的大
`EVENT_REMOVE` 与本 patch 无关、不计入充气阶段。x86-4K 下空洞探测退化为每
页一次 `lseek`——纯 in-kernel xarray 走查,无事件 / 无 handler 握手 / 无
共享 inode madvise 争用,本地廉价;把连续 PFN 在 inflate 队列合并成 run
再探测是后续可选优化,不在本 patch 范围。

## 4. 构建工作流

`deps/build-cloud-hypervisor.sh` 是多阶段 dispatcher,STAGE 选择阶段:

| STAGE | 输入 | 输出 | 何时用 |
|-------|------|------|--------|
| `fetch` | tarball | 解压 + git init + tag `ch-patches-base` | 一次性,首次构建前 |
| `patches-apply` | `deps/ch-patches/*.patch` | `git am` 到源树 | `make cloud-hypervisor` 自动调 |
| `patches-format` | 当前源树 commits | 提取到 `deps/ch-patches/*.patch` | patch 开发后回写 |
| `build` | 已 patched 源树 | `bin/<arch>/cloud-hypervisor` | `make cloud-hypervisor` 自动调 |

### 4.1 标准 build

```bash
make cloud-hypervisor
# = STAGE=fetch + STAGE=patches-apply + STAGE=build
# 冷构建 ~10 min,热(cargo cache)~秒级
```

### 4.2 patch 开发流

需要修改 patch 时:

```bash
# 1. 一次性:拉源码 + git tag base
make ch-fetch

# 2. 在源树里改代码 + commit
cd build/src/cloud-hypervisor
# ... 编辑 vmm/src/memory_manager.rs ...
git -c user.name=dev -c user.email=dev@local commit -am "...".

# 3. 把 commits 提取回 deps/ch-patches/*.patch
make ch-patches-format

# 4. 重新应用 + build,验证可重复
make cloud-hypervisor
```

`patches-apply` 会做 sanity 检查:

- 目标 git tree 必须有 `ch-patches-base` tag
- 若 HEAD == base:`git am` 应用所有 patches
- 若 HEAD 已经 = base + patches.len() 个 commits 且 subject 完全匹配:
  视为已应用,跳过(幂等)
- 任何其他状态:报错并指引"先 `make ch-patches-format` 保 WIP,然后
  `git reset --hard ch-patches-base`,再重跑"——避免静默覆盖未保存的开发中改动

### 4.3 交叉编译

跨架构构建走 `RUST_TARGET` + `${CROSS_PREFIX}gcc` 链:

```bash
# x86_64 host 上交叉构建 aarch64 cloud-hypervisor
make TARGET_ARCH=aarch64 cloud-hypervisor
# script 内:
#   --target=aarch64-unknown-linux-gnu
#   CARGO_TARGET_AARCH64_UNKNOWN_LINUX_GNU_LINKER=aarch64-linux-gnu-gcc
```

需要 `rustup target add <target>` 提前安装目标 std 库。

### 4.4 WSL2 注意

CH 源树 ~50K 文件、cargo target ~2 GiB。WSL2 在 `/mnt/<drive>/` (DrvFs)上每
小文件 5-10× I/O 开销。建议覆盖 `CH_SRC` + `CH_BUILD_OUT`:

```bash
CH_SRC=~/ch-build/src CH_BUILD_OUT=~/ch-build/out make cloud-hypervisor
```

把构建工件放到 Linux-native 文件系统(ext4 over WSL2)。

## 5. 启动协议(per-arch)

CH 自适应启动协议,sandbox-ctl 命令行不区分 arch:

| arch | 协议 | kernel 入口 | CH 准备 |
|------|------|-------------|---------|
| x86_64 | PVH | ELF entry,zero page 由 CH 填 | memmap(E820) + cmdline + ACPI 表 |
| aarch64 | EFI stub + ACPI | PE Image start,EFI stub 解析 ACPI | UEFI memmap + ACPI 表 + GICv3 节点 |

### 5.1 设备模型(平台用法)

CH 暴露给 guest 的设备清单(冷启动):

```
virtio-pmem    → sandbox-runtime.erofs (DAX, MAP_SHARED 共享 host page cache)
virtio-blk × 2 → blk0 (base, ro) + blk1 (overlay COW, rw),vhost-user backend
virtio-net     → eth0,host TAP 后端
virtio-console → hvc0,内核 dmesg;--console tty(写到 CH 进程的 stdout = sandbox-ctl
                 给的匿名管道),--serial off(无 8250 UART)。CH 进程的 stdin=/dev/null
                 故 CH 不 raw 化任何宿主终端。应用 stdio 不走此设备(走 vsock MUX)
virtio-vsock   → CID=3。控制面短连接(launch / ping / app_started / app_exited /
                 mem_report / quiesce / restore / attach)+ launch/restore/attach 那条
                 连接握手后升级而成的应用 stdio MUX(详见 sandbox-runtime.md §4)
virtio-balloon → size=0 [+ deflate_on_oom=on];host BalloonController 通过
                 /vm.resize 推 target(见 sandbox.md §9.3);free_page_reporting
                 不启用(广播 mmu_notifier 会饿死 guest vsock kthread)
virtio-mem     → host-driven 主动 unplug(扩展点)
```

恢复路径设备拓扑通过 `--restore source_url=<state.json dir>` 从 snapshot
state 还原,不需要重新指定 `--kernel` / `--vsock`。

详细命令行示例与冷启动/恢复差异见 [`sandbox.md`](sandbox.md) §冷启动数据流
与 §恢复数据流。

### 5.2 vsock hybrid 代理

vsock 在 host 端通过 hybrid 代理映射到 UDS:

```
guest VM (CID=3) → CID=2 (host) → CH 把流量转发到
  /run/sandbox/<sid>/vsock.sock_<port>     guest → host 方向(host 在该 UDS 上 listen)
  /run/sandbox/<sid>/vsock.sock + "CONNECT <port>\n" 行    host → guest 方向(guest 在 port 上 listen)
```

host → guest 方向需要在第一笔写入发 ASCII `CONNECT <port>\n`,CH 回一行
`OK <local_port>\n`(host 须先排空再读后续 payload),之后 CH 把流量代理到 guest
对应 port 的 listener。两个方向的连接对 CH 而言都是普通字节流——`launch` /
`restore` / `attach` 这三种连接在应用层握手后由 sandbox-ctl / sandbox-init 自行
转入帧收发态(stdio MUX),CH 不感知。详细见 [`sandbox.md`](sandbox.md) §5.2 与
[`sandbox-runtime.md`](sandbox-runtime.md) §4.2。

## 6. 行为契约总结

平台代码(sandbox-ctl + node-ctl)依赖的 CH 行为(包含 patch):

| 行为 | upstream | patched |
|------|----------|---------|
| 命令行不写 `fd=` | ✓(memfd_create 自管 RAM) | ✓(同 upstream)|
| 命令行写 `fd=` | ✗(parse 错误) | ✓(用 fd mmap,标记 user_managed)|
| 命令行写 `fd=` + `uffd_socket=` | ✗ | ✓(创建 uffd + sendmsg + 等 ack)|
| `/vm.snapshot` 对 user_managed zone | (不适用) | 跳过 dump,memory-ranges 表无此 zone |
| `/vm.restore` 对 user_managed zone | (不适用) | 跳过 fill;mmap 直接 fault 触发 uffd |
| balloon release 对 user_managed zone 的空洞 run | (不适用) | 跳过 `PUNCH_HOLE`+`madvise`,不合成 `EVENT_REMOVE`;有数据的 run 同 upstream |
| balloon `deflate_on_oom=on` | ✓(v51.1 已就绪) | ✓ |
| `vm.resize` `desired_balloon` | ✓ | ✓(平台周期调用,host BalloonController)|
| virtio-mem `vm.resize` | ✓ | ✓ |

平台**不**使用 `free_page_reporting`——upstream 支持完好,但在统一 memfd /
外部 uffd 模型下,CH `release_memory_range` 对自身 mmap 做
`madvise(MADV_DONTNEED)` 会广播 mmu_notifier 失效到 KVM EPT,持续 IPI
shootdown 饿死 guest vsock kthread。改由 host 端 BalloonController 通过
`/vm.resize` 推 inflate target,事件量被反馈环 `MaxStep` 限速。host 推
inflate 时,release 对 user-managed zone 的空洞 run 由 patch 0004(§3.4)
跳过,因此冷启动充气(覆盖整个稀疏区间)根本不产生该 `madvise` 广播与
`EVENT_REMOVE`;`MaxStep` 仅对运行时回收**已驻留**工作集页时仍然有意义。
`deflate_on_oom` 是 upstream v51.1 原生,无需新 patch。

## 7. 已知限制

- **patch 不向上游**:风格偏差 + 用例特殊,维护成本由本项目承担
- **CH v52+ 升级窗口**:每次 CH 大版本会有 `vmm/src/memory_manager.rs` 内部
  重构,patch 0001/0002 通常需要小幅 rebase。0003 (uffd ioctl) 改动较大,需要
  多花时间 review。0004 仅触 `virtio-devices/src/balloon.rs` 单函数
  (`release_memory_range`),rebase 面最小
- **多 fd-backed zone**:本设计 v1 限定单 zone(整 8 GiB 一段)。多 zone(NUMA
  / virtio-mem 横向扩展)是扩展点,需要在 patch 0003 处对每 zone 各自 sendmsg
  一次,sandbox-ctl 端各自维护 addrMap

## 8. See Also

- [`sandbox.md`](sandbox.md) §冷启动 / §恢复 —— sandbox-ctl 怎么用 patched CH
  跑沙箱;命令行示例
- [`sandbox.md`](sandbox.md) §uffd handler —— sandbox-ctl 接收到 uffd_C 之后
  如何处理 fault 事件
- [`sandbox-kernel.md`](sandbox-kernel.md) —— guest kernel 如何配合 CH 启动
  协议(PVH / EFI stub)
- [`build.md`](build.md) —— `make cloud-hypervisor` 工作流 + patch 开发循环
- `PROPOSAL.md` §6.11 / §10.2 —— VMM + Guest 环境在系统中的位置
