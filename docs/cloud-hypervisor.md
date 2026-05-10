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
| `vmm/src/vm_config.rs` | ~10 | `MemoryZoneConfig` 新增 `fd` / `uffd_socket` 字段 |
| `vmm/src/config.rs` | ~10 | `MemoryConfig::parse` 增加两个 key 解析 |
| `vmm/src/memory_manager.rs` | ~355 | fd 注入 + user_managed skip + create_ram_region 内创建 uffd + va_report sendmsg(SCM_RIGHTS) + UFFDIO_REGISTER |
| `vmm/src/seccomp_filters.rs` | ~17 | allowlist `userfaultfd` syscall + `UFFDIO_API` / `UFFDIO_REGISTER` ioctl |

总计 ~395 行 Rust + 3 个 cohesive commits。基于 cloud-hypervisor `v51.1`。

### 1.3 维护策略

- patch 文件位置:`deps/ch-patches/000{1,2,3}-*.patch`
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
  --memory-zone size=8G,shared=on,fd=3,uffd_socket=/run/<sid>/uffd.sock \
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
virtio-vsock   → CID=3,launch protocol + ping/restore/quiesce 通道
virtio-balloon → free_page_reporting=on [+ deflate_on_oom=on]
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
  /run/<sid>/vsock.sock_<port>     guest → host 方向
  /run/<sid>/vsock.sock + "CONNECT <port>\n" 行    host → guest 方向
```

host → guest 方向需要在第一笔写入发 ASCII `CONNECT <port>\n`,CH 据此选择
代理目标 port。详细见 [`sandbox.md`](sandbox.md) §vsock 通道。

## 6. 行为契约总结

平台代码(sandbox-ctl + node-ctl)依赖的 CH 行为(包含 patch):

| 行为 | upstream | patched |
|------|----------|---------|
| 命令行不写 `fd=` | ✓(memfd_create 自管 RAM) | ✓(同 upstream)|
| 命令行写 `fd=` | ✗(parse 错误) | ✓(用 fd mmap,标记 user_managed)|
| 命令行写 `fd=` + `uffd_socket=` | ✗ | ✓(创建 uffd + sendmsg + 等 ack)|
| `/vm.snapshot` 对 user_managed zone | (不适用) | 跳过 dump,memory-ranges 表无此 zone |
| `/vm.restore` 对 user_managed zone | (不适用) | 跳过 fill;mmap 直接 fault 触发 uffd |
| balloon `free_page_reporting=on` | ✓ | ✓ |
| balloon `deflate_on_oom=on` | ✓(v51.1 已就绪) | ✓ |
| `vm.resize` `desired_balloon` | ✓ | ✓(平台动态调用)|
| virtio-mem `vm.resize` | ✓ | ✓ |

`free_page_reporting` 与 `deflate_on_oom` 都是 upstream v51.1 原生支持,
无需新 patch。

## 7. 已知限制

- **patch 不向上游**:风格偏差 + 用例特殊,维护成本由本项目承担
- **CH v52+ 升级窗口**:每次 CH 大版本会有 `vmm/src/memory_manager.rs` 内部
  重构,patch 0001/0002 通常需要小幅 rebase。0003 (uffd ioctl) 改动较大,需要
  多花时间 review
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
