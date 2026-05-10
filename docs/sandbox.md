# sandbox — 沙箱运行时控制工具

`sandbox-ctl` 是平台沙箱的 host 端控制平面,管理一个 microVM 的完整生命周期
(冷启动、快照、恢复)。每个沙箱由一组 `sandbox-ctl + cloud-hypervisor` 双
进程承载,sandbox-ctl 是 CH 的父进程,负责 vhost-user-blk backend、uffd
handler、cgroup/balloon 联动、与 node-ctl 的资源协议对话。

进程模型类 `runc run`:一个 `sandbox-ctl run` 命令就是一个 sandbox 的完整
生命周期。退出 = sandbox 销毁。

## 1. 概述

### 1.1 系统中的位置

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ HOST                                                                          │
│                                                                               │
│  ┌─── 全局共享 ──────────────────────────────────────────────────────────┐   │
│  │  /opt/sandbox/vmlinux              kernel 镜像,各 sandbox 各自加载   │   │
│  │  /opt/sandbox/sandbox-runtime.erofs Guest PID 1 镜像,DAX 共享       │   │
│  │  cache-ctl daemon                  跨 sandbox 共享的 chunk 缓存     │   │
│  │  store-ctl daemon                  内容寻址存储后端                  │   │
│  │  node-ctl  daemon                  沙箱资源控制器(可选)            │   │
│  └────────────────────────────────────────────────────────────────────────┘   │
│                                                                               │
│  ┌─── per-sandbox(每个 sandbox 一组进程) ────────────────────────────┐    │
│  │                                                                       │    │
│  │  ┌─────────────────────────────────────┐                             │    │
│  │  │ sandbox-ctl 进程                    │                             │    │
│  │  │                                     │                             │    │
│  │  │  control plane (lifecycle)          │                             │    │
│  │  │  vhost-user-blk backend × 2         │                             │    │
│  │  │  uffd handler                       │                             │    │
│  │  │  va_report UDS server               │                             │    │
│  │  │  resource executor (→ node-ctl)     │                             │    │
│  │  │                                     │                             │    │
│  │  │  link: pkg/fetch → cache-ctl RPC    │                             │    │
│  │  │  link: pkg/ingest(快照上传时使用)   │                             │    │
│  │  └─────┬───────────┬───────────┬───────┘                             │    │
│  │        │           │           │                                      │    │
│  │        │ vhost-user│ vhost-user│ CH API UDS                           │    │
│  │        │ blk0      │ blk1      │                                      │    │
│  │        ▼           ▼           ▼                                      │    │
│  │  ┌──────────────────────────────────────┐                            │    │
│  │  │ cloud-hypervisor (patched)           │                            │    │
│  │  │   KVM + virtio devices              │                            │    │
│  │  │   pmem → sandbox-runtime.erofs      │                            │    │
│  │  │   blk0/blk1 → sandbox-ctl backend   │                            │    │
│  │  └──────────────────────────────────────┘                            │    │
│  │                                                                       │    │
│  │  /run/<sid>/                                                          │    │
│  │    ch.sock   blk0.sock   blk1.sock   uffd.sock   ctl.sock            │    │
│  │    vsock.sock + vsock.sock_5000                                       │    │
│  │    blk1.diff(本地 ext4 COW 上层文件)                                │    │
│  │  /var/lib/sandbox/<sid>/snap/  [snapshot/restore 中转目录]           │    │
│  └────────────────────────────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────────────────────────┘
```

sandbox-ctl 是 CH 的父进程。CH 退出 → sandbox-ctl 收 SIGCHLD → 优雅
cleanup → 自身退出。退出码 = CH 退出码(经映射)。

### 1.2 设计原则

1. **统一内存所有权**:sandbox-ctl 拥有 memfd inode,CH 通过继承 fd 映射同一
   inode。冷启动 + 恢复用同一份内存代码路径(详见 §冷启动数据流);snapshot
   时 sandbox-ctl 直接 SEEK_DATA/HOLE 扫驻留页,不经 CH→file→sandbox-ctl 的
   中转
2. **单 uffd 模型**:CH 进程内创建 uffd(必须绑到 CH mm),通过 SCM_RIGHTS 把
   fd 传给 sandbox-ctl 的 handler;sandbox-ctl 自己 mmap 的 backendVA 不注册
   uffd,kernel 走默认 shmem 缺页路径。这从根本上避免跨 mm folio-creation
   race
3. **patch 影响面最小**:CH 改动 ~395 行,集中在 4 个文件;仅 `--memory-zone fd=`
   出现时激活;不写 fd= 的所有调用者行为完全等同 upstream
4. **资源可寻址**:任何运行所需文件(vmlinux 除外)都能选 `file://` 或
   `manifest://`,运行时无差别看待;snapshot 同样可上传至 manifest 存储
5. **三态资源控制**:是否启用 cgroup 限制、是否启用动态控制由 sandbox.yaml
   字段是否存在决定,无独立 mode 开关(详见 §资源模型)
6. **per-sandbox 一进程**:类 `runc run` 语义,不是 daemon。故障域、资源
   记账、生命周期对齐自然清晰

### 1.3 故障域

| 故障 | 直接影响 | 自愈 / 处理 |
|------|---------|------------|
| CH 崩溃 | sandbox-ctl 收 SIGCHLD | 整 sandbox 销毁 |
| sandbox-ctl 崩溃 | CH 失去父进程 + uffd handler 没了 | vCPU 卡 fault → 上层 supervisor SIGKILL CH |
| node-ctl 不可达(动态控制模式) | 长连断 | 退避重连 5×;持续 60s 失败切到无控制器降级模式 |
| 单沙箱 OOM | guest 内进程被 kill;deflate_on_oom 释放 balloon | 非平台级故障 |
| 资源开销 | sandbox-ctl Go runtime ~10-15 MiB;CH 自身 ~13 MiB | blk1.diff 是 sparse 文件,实际 = 已写 sectors |

## 2. 命令行接口

### 2.1 子命令总览

| 子命令 | 用途 |
|--------|------|
| `run` | 启动一个 sandbox 跑到退出。冷启动 = 不带 `--restore`;恢复 = 带 `--restore=<...>`,与冷启动共用同一进程模型与 stdio 接线 |
| `snapshot` | 暂停一个运行中的 sandbox 并 dump 到 snapshot |

### 2.2 `sandbox-ctl run`

```
sandbox-ctl run [flags]

  # 配置 + 身份
  --config <path>         sandbox.yaml 路径(SANDBOX_CONFIG env;flag 优先)
  --manifest-config <path>  manifest 配置 YAML(MANIFEST_CONFIG env);仅 manifest://
                          资源(blk0 base、--restore manifest://、--upload)需要
  --sandbox-id <sid>      覆盖 yaml 里的 sandbox.id

  # 执行环境
  --ch-binary <path>      cloud-hypervisor 二进制路径。不显式指定时按以下顺序查找:
                          1. SANDBOX_CH_PATH env(若非空)
                          2. <sandbox-ctl 自身可执行文件目录>/cloud-hypervisor
                          3. exec.LookPath("cloud-hypervisor")(走 PATH)
  --run-dir <dir>         host runtime state 目录(SANDBOX_RUN_DIR env;默认 /run)。
                          sandbox-ctl 在 <run-dir>/<sid>/ 下创建 ch.sock /
                          blk{0,1}.sock / vsock.sock / uffd.sock / ctl.sock 与
                          blk1.diff 默认存放位置

  # 资源覆盖(运维临时调整)
  --cgroup-path <path>    覆盖 control.cgroup_path
  --controller <path>     覆盖 control.controller;`--controller=disable` 强制清空,
                          即使配置文件给了也不连

  # 恢复模式
  --restore <ref>         恢复模式:从 snapshot bundle (<sid>.snapshot 文件 / 或
                          manifest://<hex>)读 snapshot.cfg + 内存内容启动。
                          <ref> = 本地文件路径或 manifest://<hex>。不带此 flag
                          = 冷启动模式。restore 模式下 sandbox.yaml 字段语义
                          见 §11.0

  # stdio 模型(冷启动 + 恢复模式都生效)
  --stdin                 默认关闭。`--stdin` 或 `--stdin=true` 让沙箱继承
                          sandbox-ctl 进程的 stdin
  --stdout                默认开启,继承 sandbox-ctl stdout。`--stdout=false` 关闭
                          (重定向到 /dev/null)
  --stderr                默认开启,继承 sandbox-ctl stderr。`--stderr=false` 关闭
  --stdin-from <file>     把 <file> 当作沙箱的 stdin(隐含 `--stdin=true`)
  --stdout-to <file>      把沙箱 stdout 写到 <file>(隐含 `--stdout=true`)
  --stderr-to <file>      把沙箱 stderr 写到 <file>(隐含 `--stderr=true`)
  --tty                   创建一对 pty,slave 同时给 CH 的 stdin/stdout/stderr,
                          master 桥接到 sandbox-ctl 当前 controlling tty
                          (适用于交互式调试;终端 Ctrl-C / Ctrl-D 直达 guest)
```

**行为**:阻塞前台运行,直到 CH 退出或收到 SIGTERM/SIGINT。退出码 = CH exit
code(0 = 正常 reboot,非 0 = panic/crash)。

**stdio 决策**:

| 流 | 默认 | `--tty` | `--xxx=false` | `--xxx-{from,to}=F` |
|---|---|---|---|---|
| stdin | /dev/null | pty slave | /dev/null | open(F, O_RDONLY) |
| stdout | os.Stdout | pty slave | /dev/null | open(F, O_WRONLY\|O_CREATE\|O_APPEND, 0644) |
| stderr | os.Stderr | pty slave | /dev/null | 同 stdout |

互斥规则:

- `--tty` 与 `--stdin` / `--stdout` / `--stderr` / `--stdin-from` / `--stdout-to`
  / `--stderr-to` 中任一显式赋值互斥(`--tty` 包打全部三流)
- `--stdin=false` 与 `--stdin-from` 互斥
- `--stdout=false` 与 `--stdout-to` 互斥
- `--stderr=false` 与 `--stderr-to` 互斥

**guest console 行为**:sandbox-ctl 自身的日志写 `os.Stderr`(单行
`[sandbox-ctl ...]` 前缀)。CH 子进程通过 `cmd.Stdout/Stderr` 继承 sandbox-ctl
的 stdio(按上面 stdio 决策表选择来源);CH 启动时带 `--console tty --serial null`,
所以 guest 通过 `/dev/console` 或 hvc0 的写入会随 CH 的 stdio 一并进入选定目的地。

guest 内核启动消息 + 任何写到 hvc0 的内容是否真的出现在选定目的地,取决于
用户 sandbox.yaml `boot.cmdline` 是否写了 `console=hvc0`(平台不自动注入)。
静默场景(密度部署、stdout 容量受限)省略 `console=` 即可;此时 guest 没有
active console 设备,sandbox-ctl 的 stdio 上只剩下自身日志。

**恢复模式**:`--restore=<file_path|manifest://hex>` 让 sandbox-ctl 走恢复路径
(详见 §7)。`<ref>` 形式:

- 本地 file path — 从 disk 读 ZIP + memfd 写 SparseSnapshotSource
- `manifest://<hex>` — 经 cache-ctl + store-ctl 拉 chunk,memfd 写
  ManifestSnapshotSource;snapshot.cfg 内 base_ref / overlay.base 若是 manifest://
  也按 chunk 粒度 lazy fetch

迁移说明:**`sandbox-ctl restore` 子命令已删除**。原 `sandbox-ctl restore --snapshot=<x> --config=y.yaml`
的语义对应于 `sandbox-ctl run --restore=<x> --config=y.yaml`,行为完全等价
(stdio / cgroup / network 接线统一)。

### 2.3 `sandbox-ctl snapshot`

```
sandbox-ctl snapshot [flags]

  --sandbox-id <sid>    必填,目标 sandbox
  --output <out_dir>    本地输出目录,产出 <sid>.snapshot + <sha256>.overlay 两个文件
  --upload              上传至 manifest 存储:overlay 流式 ingest 拿 manifest key,
                        <sid>.snapshot 内嵌 snapshot.cfg 的 overlay.base 写为
                        manifest://<key>;stdout 输出 snapshot manifest key
  --resume              默认 false(快照后销毁沙箱);`--resume` / `--resume=true`
                        保留沙箱继续运行
  --run-dir <dir>       与 run 一致;SANDBOX_RUN_DIR env;默认 /run。snapshot 通过
                        <run-dir>/<sid>/ctl.sock 联系运行中的 sandbox-ctl run 进程
  --timeout <sec>       等 snapshot_done 的超时,默认 60s
```

**`--output` 与 `--upload` 严格互斥**——必须二选一,两者都给或都不给均报错
退出。两者代表"持久化目的地"的两种形态(本地 vs manifest 存储),混用会让
snapshot.cfg 的 overlay.base 引用与实际数据位置脱节。

**销毁路径**(`--resume=false` 默认):snapshot 完成后,sandbox-ctl run 进程
通过 CH `/vm.shutdown` 优雅关机(ACPI shutdown → guest sandbox-init 收到事件
→ reboot syscall),等 CH 退出后 sandbox-ctl run 进程自身也退出。`--resume=true`
则跳过 shutdown,调 `/vm.resume` 让沙箱继续运行,sandbox-ctl run 进程不退出。

## 3. 配置

### 3.1 sandbox.yaml

```yaml
# 资源规格
resources:
  capacity:                    # vCPU/内存数(guest 看到的"声明规格")
    cpu: 2
    memory: 8GiB
  allocatable:                 # ≤ capacity,默认 = capacity
    cpu: 0.1                   # ≤ capacity.cpu;无 cgroup 模式必须 == capacity.cpu
    memory: 128MiB             # balloon 初始膨胀 = capacity.memory − 此值
    deflate_on_oom: true       # CH --balloon 是否带 deflate_on_oom

  control:                     # 部署模式驱动
    cgroup_path: ""            # 空 = 无 cgroup 模式;非空 = 必须已存在的 cgroup 绝对路径
    controller: ""             # 空 = 无 cgroup / 静态 cgroup 模式;非空 = 动态控制模式(UDS 路径)

  overhead:                    # 仅 cgroup_path 已设时允许;默认 32 MiB
    memory: 32MiB              # memory.max = capacity.memory + 此值
  watermark_high:              # 仅 cgroup_path 已设时允许;默认 allocatable.memory × 0.875
    memory: 128MiB             # cgroup memory.high 初始值
  startup_burst:               # 仅 controller 已设时允许;默认 = allocatable.memory
    memory: 256MiB             # 启动期 allocatable_now;约束 floor ≤ 此 ≤ capacity

# 网络
network:
  tap: tap0                    # 预创建的 TAP 名,sandbox-ctl 不创建只 attach
  interface: eth0              # guest 内网卡名,默认 eth0
  ip: 169.254.1.1/31           # CIDR(IPv4 或 IPv6);空则不配 IP
  gateway: ""                  # 默认路由下一跳;空则不配默认路由
  hostname: my-sandbox         # guest hostname(sethostname)

# Guest 启动 + rootfs
boot:
  kernel: file:///opt/sandbox/vmlinux              # 仅冷启动需要
  runtime: file:///opt/sandbox/sandbox-runtime.erofs  # 仅 file://
  cmdline: "console=hvc0"
  root:
    base: file:///container-snapshot.erofs    # 自动挂为 disk0(vhost-user-blk ro)
                                              # flattened image,尾部附加的 zip 内含
                                              # config.json 定义默认容器启动配置
    overlay:                                  # 自动挂为 disk1(vhost-user-blk rw)
      base: file:///container-snapshot.ext4   # 可选,快照恢复时常用 manifest://
      diff: file:///run/<sid>/blk1.diff       # 始终 file://;不存在则创建空 sparse
      size: 10GiB                             # 可选,默认 10GiB(若 overlay.base 较大则取较大者)

# 容器应用启动配置(覆盖 boot.root.base 内嵌的 config.json 默认值)
launch:
  exec: /usr/bin/foo
  args: ["arg1", "arg2"]
  env:
    PATH: /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
    HOME: /root
  workdir: /
  restart: never              # never | on-failure | always
```

清单配置(manifest/store/crypto/cache 节)单独存在,不放入 sandbox.yaml——它
是节点级配置,所有 CLI 共享(sandbox-ctl 通过 `--manifest-config` 或
`MANIFEST_CONFIG` 环境变量引用,详见 [`manifest.md`](manifest.md))。

**绝对路径要求**:sandbox.yaml 中所有 `file://` URL 必须是绝对路径
(`file:///abs/path/to/file`)。配置校验阶段拒绝 `file://relative/path`
——避免不同启动目录(systemd unit / shell pwd / orchestrator workdir)解释路径
不一致。restore 模式下 `boot.runtime` / `boot.root.base` / `boot.root.overlay.diff`
若提供也仍要求绝对路径(只是允许整字段不写,由 snapshot.cfg 复制,详见 §11.0)。

**自动注入的 kernel cmdline**(用户不写、不可改):

```
init=/sbin/init root=/dev/pmem0 ro rootfstype=erofs dax=always
```

锁定 `sandbox-runtime.erofs` 通过 virtio-pmem DAX 挂为 `/`、由 `/sbin/init`
(sandbox-init 二进制)接管。`boot.cmdline` 的内容追加在后,用户控制 `console=`、
`quiet` 等。

**`console=hvc0` 的语义**:平台**不**自动注入。CH 启动时已经带
`--console tty --serial null`(把 hvc0 接到 CH 进程 stdio,8250/virtio-serial
关掉),`console=hvc0` 是 guest kernel 端"是否绑定 hvc0 作为 active console"
的开关。用户写了:kernel 启动消息 + 任何对 `/dev/console` / hvc0 的写入都通过
CH stdio 回传到 sandbox-ctl 选定的目的地(详见 §2.2 stdio 决策)。用户没写:
guest 没有 active console,sandbox-ctl stdio 上只有 sandbox-ctl 自身日志,
适合密度部署 / stdout 容量受限场景。

**网络配置不进 cmdline**:IP/Gateway/Hostname/Interface 通过 vsock launch 协议
下发给 sandbox-init;phase 2 由 sandbox-init 用 raw netlink 配置(IFF_UP +
RTM_NEWADDR + 可选 RTM_NEWROUTE)。kernel 已删 `CONFIG_IP_PNP*`,不再支持
`ip=...` cmdline 形式。

**`launch.*` 不进 cmdline**:容器启动配置(exec/args/env/workdir/restart)通过
vsock 在运行时下发,见 [`sandbox-runtime.md`](sandbox-runtime.md) §阶段 2。

### 3.2 file:// vs manifest:// truth table

| 字段 | `file://` | `manifest://` | 备注 |
|------|-----------|---------------|------|
| `boot.kernel` | ✓ | ✗ | vmlinux 体积小且节点级共享,manifest 化没有收益 |
| `boot.runtime` | ✓ | ✗ | sandbox-runtime.erofs 节点级共享,DAX 直接用 host 文件 |
| `boot.root.base` | ✓ | ✓ | 跨 sandbox 复用率高,manifest 化收益最大 |
| `boot.root.overlay.base` | ✓ | ✓ | 快照恢复时常用 manifest:// |
| `boot.root.overlay.diff` | ✓ (only) | ✗ | 运行时 dirty 数据,本地 sparse 文件 |
| `run --restore=<ref>` | ✓ | ✓ | `<sid>.snapshot` 文件路径 / manifest://<key> |

### 3.3 flattened image 内嵌 config.json

`boot.root.base` 指向的 erofs 文件结构借鉴 `<sid>.snapshot`:

```
偏移 [0, erofs_end)              erofs 文件系统
偏移 [erofs_end, EOF)             ZIP archive
                                    / config.json     OCI image runtime config
                                    / ...
```

ZIP 中央目录在文件末尾,erofs 内核驱动从文件起点读到 erofs superblock 标记的
尺寸,自然忽略后缀;ZIP 解析器从尾部倒推。同一个文件:guest 把 erofs 部分
挂为 / 的 lower 层,sandbox-ctl 在启动前读 ZIP 取 config.json 作为 launch
默认值,与 sandbox.yaml `launch.*` 合并(yaml 优先)。

`launch.exec` 不再必填——若 image config 有 Entrypoint/Cmd 即可省略。

### 3.4 snapshot.cfg(snapshot 内嵌)

`snapshot.cfg` 是 `<sid>.snapshot` 末尾 ZIP 内的一个 entry,记录 snapshot 时
**guest 那一侧不可重生的契约**——capacity、runtime / image base 内容指针、
overlay 数据指针。**host 侧可重选的字段一概不存**(network、launch、cgroup、
controller、cmdline 等都由 restore 调用者通过 sandbox.yaml 重新提供)。

**字段 schema**(YAML):

```yaml
# 资源规格仅留 capacity
resources:
  capacity:
    cpu: 2
    memory: 8GiB

# Guest 启动 + rootfs
boot:
  runtime_ref: file://sandbox-runtime.erofs@sha256:<digest>
                 # boot.runtime 仅支持 file://(见 §3.2 truth table),
                 # snapshot.cfg 保存的 runtime_ref 也只会是 file:// 形式
  root:
    base_ref:    file://container-image.erofs@sha256:<digest>
                 # 或 manifest://<key>(原引用是 manifest:// 时原样保留)
    overlay:
      base:      file://<sha256>.overlay
                 # 或 manifest://<key>(--upload 模式)
```

**字段说明**:

- `resources.capacity.{cpu,memory}`:guest 看到的物理规格。restore 时
  sandbox.yaml 若提供必须严格相等,不一致拒绝启动(详见 §11.0、§13)
- `runtime_ref`:始终 `file://<basename>@sha256:<digest>` 形式
  (`boot.runtime` 本身只支持 file://)。restore 时 basename 用于在
  `<sid>.snapshot` 同目录定位文件,digest 用于内容校验(防止换了一个不同
  版本的 erofs)
- `base_ref`(file:// 类):同 runtime_ref,`file://<basename>@sha256:<digest>`
- `base_ref`(manifest:// 类):原样保留 manifest key(`manifest://<key>`),
  无需 digest(manifest key 已是 content-addressable)
- `overlay.base`(file:// 类):指向 snapshot 输出目录中那个 `<sha256>.overlay`
  文件,basename 自带 sha256 摘要,无需额外 `@sha256:` 后缀
- `overlay.base`(manifest:// 类):上传后的 overlay manifest key

**故意不存的字段**:

| 字段 | 为什么不存 |
|---|---|
| `sandbox.id` | 由 host 传入(`run --sandbox-id` 或 yaml) |
| `network.{tap,ip,gateway,hostname,interface}` | host-localized,restore 时由 sandbox.yaml 提供 |
| `launch.{exec,args,env,workdir,restart}` | 应用启动配置在 guest 内存里已经反映为运行中进程,restore 后不再走 launch 协议 |
| `control.{cgroup_path,controller}` | host-localized 资源策略 |
| `overhead` / `watermark_high` / `startup_burst` / `allocatable` | 同上,host 资源策略 |
| `boot.kernel` / `boot.cmdline` | restore 不重新 boot,kernel 在 snapshot 内存中 |
| `boot.root.overlay.diff` | host 本地写层路径,restore 时新建一个 |
| `boot.root.overlay.size` | 不影响内容,沿用 capacity 默认 / yaml 提供 |

**版本字段**:不引入显式 schema version。snapshot 是 ephemeral 资产
(host 重启即丢,跨主机复制只在调度场景),hard cut over;旧版 snapshot 解析
失败时报清晰错误"snapshot.cfg not found in <file>; produced by old sandbox-ctl?"。

restore 时 host sandbox.yaml 与 snapshot.cfg 的字段语义合并规则详见 §11.0。

## 4. 资源模型

### 4.1 三种部署模式

是否启用 cgroup 限制、是否启用动态控制由 `control` 字段是否存在驱动:

| 模式 | 字段配置 | 适用场景 |
|---|---------|---------|
| **无 cgroup** | `control.cgroup_path` 未设 | 开发环境、调试、单租户独占节点 |
| **静态 cgroup** | `control.cgroup_path` 已设,`control.controller` 未设 | 生产但无超分需求;cgroup 隔离即可 |
| **动态控制** | `control.cgroup_path` 已设,`control.controller` 已设 | 生产高密度、多租户共节点 |

**能力对比**:

| 能力 | 无 cgroup | 静态 cgroup | 动态控制 |
|------|----------|------------|----------|
| cgroup PSI 反压 | 无 | ✓ memory.high | ✓(随 allocatable 动态) |
| balloon + deflate_on_oom | ✓ | ✓ | ✓ |
| CPU 公平共享 | ✗(allocatable.cpu == capacity.cpu) | ✓ cpu.weight | ✓ |
| 跨沙箱仲裁 | ✗ | ✗ | ✓ |
| 防惊群 | ✗ | ✗ | ✓ 节点限速 + emergency_pool |
| 创建期资源管控 | ✗ | ✗ | ✓ |
| 超分密度提升 | ✗ | 有限(allocatable 必须保守) | ✓ allocatable 可贴近真实工作集 |

切换模式通过加 / 减 sandbox.yaml 字段。命令行覆盖在配置变更不便时给运维一个
临时调整入口(`--cgroup-path` / `--controller`)。

### 4.2 资源量

每沙箱在每个维度(memory / cpu)上有三个量:

| 量 | 内存 | CPU | 含义 |
|----|------|-----|------|
| `capacity` | resources.capacity.memory | resources.capacity.cpu(整数 vCPU) | guest 看到的"声明规格";cgroup 上界 |
| `floor` | resources.allocatable.memory | resources.allocatable.cpu | 最小保留(K8s request 类比) |
| `allocatable_now` | balloon 与 cgroup memory.high 实时反映 | (CPU 维度无 allocatable_now) | 内存独有的运行时 budget,在 [floor, capacity] 浮动;**仅动态控制模式存在** |

CPU 通过 cpu.max + cpu.weight 静态表达;无 cgroup / 静态 cgroup 模式内存也
不浮动。详见 §6 / §7。

### 4.3 内存与 CPU 的不对称性

| 维度 | 内存 | CPU |
|------|------|-----|
| 物理强制机制 | balloon | cgroup cpu.max |
| 反压机制 | cgroup memory.high(PSI) | cgroup cpu.max(throttle) |
| 灾难性后果 | OOM kill | 仅减速,无 kill |
| 调整生效延迟 | balloon 数 ms-数十 ms | cpu.max 立即(下一 period) |
| 释放路径 | madvise / free_page_reporting,异步 | 无需释放;period 边界自动 |
| 安全网 | deflate_on_oom | 不需要 |
| 是否有 burst 状态机 | 是 | 否 |

CPU 维度本质比内存简单——没有不可逆失败、调整即时、释放廉价。本设计利用
内核 cgroup v2 的 cpu.weight 公平共享语义,**让 CPU 控制完全静态化**,只有
内存有 burst/recover 状态机。

## 5. 冷启动数据流

### 5.1 时序

```
T0   sandbox-ctl run --config sandbox.yaml 启动
T1   解析 yaml(可选 --sandbox-id 覆盖)→ 构造完整 SandboxConfig
T2   验证 TAP 存在,准备 /run/<sid>/ 目录
T3   准备 blk1.diff 文件(若不存在则 truncate 到声明大小,稀疏文件)
T4   动态控制模式:dial controller, send Admit, 收 grant 后继续(详见 §10)
T5   cgroup setup:写 cgroup limits + 把自身 PID 加入 cgroup.procs
     (后续 fork 的 CH 自然在同 cgroup)
T6   memory 准备(统一模型,冷启动 + 恢复同):
     T6a memfd_create("sandbox-<sid>-ram", MFD_CLOEXEC | MFD_ALLOW_SEALING)
     T6b ftruncate(memfd, ramSize)
     T6c F_ADD_SEALS: F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_SEAL
     T6d backendVA = mmap(NULL, ramSize, RW, MAP_SHARED, memfd, 0)
     T6e madvise(backendVA, ramSize, MADV_NOHUGEPAGE)
         (uffd 在 4 KiB 粒度操作,THP 把多页折成 2 MiB 大页会破坏 fault 路由)
     T6f addrMap = NewAddressMap(ramSize)(此时尚未 register VMA)
         pageStates 切片初始全 Absent,ramSize/4KiB 个元素
         snapshotReader = ZeroSource(冷启动 sentinel)
T7   构造 BlockReader for blk0(file 或 manifest)
T8   起 blk0 backend goroutine:listen /run/<sid>/blk0.sock
T9   构造 BlockBaseReader for blk1.base(可能为空)
T10  起 blk1 backend goroutine:listen /run/<sid>/blk1.sock
     blk1 backend 内部对 blk1.diff 做 SEEK_DATA 扫描重建 dirty bitmap
T11  读 boot.root.base 末尾 ZIP 拿 ImageConfig(缺 ZIP 软失败返回空)
     合并 ImageConfig 与 sandbox.yaml `launch:` → LaunchSpec
T12  起 launch server goroutine:listen /run/<sid>/vsock.sock_5000
T13  起 va_report UDS server: listen /run/<sid>/uffd.sock
     OnReady callback 内将 adopt uffd_C(从 SCM_RIGHTS)+ 起 epoll/worker
T14  起 ctl.sock UDS server: listen /run/<sid>/ctl.sock(snapshot 请求入口)
T15  构造 CH 命令行(详见 §5.2):
     `--memory-zone size=<ramSize>,shared=on,fd=3,uffd_socket=/run/<sid>/uffd.sock`
     `--console tty --serial null`(guest hvc0 接 CH 自己的 stdio,8250 关掉)
     cmd.ExtraFiles = [memfd] 让 fd=3 在 CH 进程中可见
     cmd.Stdin/Stdout/Stderr 按 §2.2 stdio 决策接线
T16  fork+exec cloud-hypervisor (patched),捕获 stdout/stderr
T17  CH (patched) 启动:
     T17a 解析 --memory-zone fd=3 → 跳过 memfd_create,mmap 同一 inode → chVA
     T17b userfaultfd() → uffd_C(绑到 CH 的 mm)
          UFFDIO_API features = MISSING_SHMEM | EVENT_REMOVE | EVENT_UNMAP | THREAD_ID
          UFFDIO_REGISTER(uffd_C, [chVA, +ramSize], MISSING)
     T17c connect uffd_socket → sendmsg(va_report; SCM_RIGHTS=uffd_C) → 等 ack
     T17d memory_range_table 检测 user_managed → snapshot_memory_ranges 不含此 zone
     T17e 配 virtio-pmem(sandbox-runtime.erofs DAX)、virtio-vsock(cid=3)、
          virtio-net、virtio-balloon
     T17f vhost-user-blk 握手:SET_OWNER → SET_FEATURES → SET_MEM_TABLE [memfd fd]
          backend fstat 比对 inode = sandbox-ctl 启动时记下的 memfd inode
          → 复用 backendVA → 不再 mmap
     T17g boot vCPU(此时 sandbox-ctl 端 ack 已回,handler 已就绪)
T18  va_report server 收到 sendmsg:
     T18a recvmsg → 解出 uffd_C fd 和 va_report 内容
     T18b addrMap.RegisterVMA(ProcessCH, chVA, ramSize)
     T18c epoll_create1 → add uffd_C → 起 reader + N worker
     T18d 回 ack 给 CH
T19  Guest 内 kernel 启动 → mount /dev/pmem0 → exec /sbin/init = sandbox-init
     sandbox-init 三阶段(详见 sandbox-runtime.md §3)
T20  vCPU 跑过程中:
     · vCPU 首次访问页 → uffd_C MISSING fault → handler 走 Absent → ZEROPAGE
     · backend 访问 backendVA → kernel 默认 shmem 缺页:folio 已存在(handler 装的)→
       直接装 sandbox-ctl mm PTE,无 uffd 事件
     · guest balloon free_page_reporting → CH 在 memfd 上 fallocate(PUNCH_HOLE)
       + 在 chVA 上 madvise(DONTNEED) → uffd_C 投 EVENT_REMOVE → handler push
       到 removeQ → flusher batch+merge 后 madvise(DONTNEED, backendVA)
T21  user app 退出 → sandbox-init reboot → CH vCPU shutdown → CH 进程退出
T22  sandbox-ctl cmd.Wait() 返回
T23  cleanup:停 backend / 关 uffd / munmap / unlink sockets / 移除 cgroup
T24  sandbox-ctl 退出,exit code = CH exit code
```

**关于冷启动 uffd 开销**:每个首次访问页要走一次 uffd 往返,单次 ~5-10 µs。
典型 sandbox 工作集 ~50-200 MiB → 12K-50K 次 ZEROPAGE,累加 60-500 ms。
这部分**摊在 vCPU 运行过程**而不是 pre-boot,对端到端冷启动 P50 几乎无影响。

### 5.2 CH 命令行(冷启动)

```
cloud-hypervisor \
  --api-socket  /run/<sid>/ch.sock \
  --kernel      /opt/sandbox/vmlinux \
  --pmem        file=/opt/sandbox/sandbox-runtime.erofs,discard_writes=on,iommu=off \
  --memory-zone size=8G,shared=on,fd=3,uffd_socket=/run/<sid>/uffd.sock \
  --balloon     size=4G,free_page_reporting=on \
  --disk        vhost_user=on,socket=/run/<sid>/blk0.sock,readonly=on \
  --disk        vhost_user=on,socket=/run/<sid>/blk1.sock \
  --vsock       cid=3,socket=/run/<sid>/vsock.sock \
  --net         tap=tap0,mac=<auto>,iommu=off \
  --console     tty \
  --serial      null \
  --cmdline     "init=/sbin/init root=/dev/pmem0 ro rootfstype=erofs dax=always
                 console=hvc0"
```

要点:
- `--memory-zone size=8G` 是 capacity;balloon 在启动时自动膨胀到 4G(=
  capacity − allocatable)
- `--memory-zone fd=3,uffd_socket=...`:patched CH 跳过 memfd_create,直接用
  sandbox-ctl 传入的 fd 当 backing;在 create_ram_region 内自己创建 uffd,
  通过 uffd_socket 发 va_report + SCM_RIGHTS,等 sandbox-ctl ack 后才允许
  vCPU 跑(详见 [`cloud-hypervisor.md`](cloud-hypervisor.md))
- `--pmem discard_writes=on` 让 guest 写 pmem 不影响 host 文件
- blk0 readonly=on 在 vhost-user 协议层告知 guest 这是只读盘
- `--vsock cid=3,socket=...`:CH 创建 virtio-vsock 设备,guest CID=3,
  通过 hybrid 代理把 guest port 5000 流量映射到 host UDS
- `--console tty`:guest 通过 `/dev/console`(或 hvc0,见 cmdline)写出的内容
  走 CH 进程自身 stdio,sandbox-ctl 通过继承 stdio(`cmd.Stdout/Stderr`)按
  §2.2 stdio 决策表传到选定目的地。`--serial null` 关掉 8250 / virtio-serial
  的兜底路径,避免双 console 输出
- cmdline 中的 `console=hvc0` 是**用户可选**——平台不自动注入。不写则 guest
  kernel 不绑定 hvc0,sandbox-ctl stdio 上就只有自身日志

## 6. snapshot 数据流

### 6.1 输出文件命名与字节布局

**输出目录布局**(`--output <out_dir>` 模式):

```
<out_dir>/
├── <sid>.snapshot          # 内存稀疏拷贝 + 末尾 ZIP
└── <sha256>.overlay        # ext4 sparse 文件,sha256 = SHA256(整字节流, hole=0)
```

`<sid>` = sandbox id(`sandbox-ctl run --sandbox-id` 设的或 yaml 里的);
`<sha256>` = `<sha256>.overlay` 文件**整字节流**(物理读取顺序,sparse hole
读到 0)的 SHA256,full hex(64 字符)。

**`<sid>.snapshot` 字节布局**(物理稀疏 + 末尾 ZIP):

```
逻辑偏移 [0, ramSize)        memfd 内容(SEEK_DATA/HOLE 稀疏化:零页是文件空洞,
                              非零页占物理 4 KiB)
逻辑偏移 [ramSize, EOF)      标准 ZIP archive
                                / config.json     CH 设备拓扑 + memory layout
                                / state.json      vCPU 寄存器、virtio queue、IRQ
                                / snapshot.cfg    §3.4 schema(替代旧 sandbox.cfg)
```

`ramSize` 是 zone 的逻辑大小;ZIP append 在该逻辑偏移之后。依赖 ZIP 中央目录
(End of Central Directory)在文件末尾的特性:

- ZIP 解析器调 `archive/zip.NewReader(ReaderAt, size)`,从末尾扫 EOCD,定位
  中央目录,再定位每个 entry 的 local header
- ZIP 内部 offset 都相对 ZIP 起点,NewReader 不需要知道 ZIP 起点——从末尾
  倒推
- 因此 prefix 长度(`ramSize`)不影响 ZIP 解析;memory 区域 + ZIP 共存于一个
  文件

**关键**:`<sid>.snapshot` 是**稀疏文件**——`stat.Size() = ramSize + zipSize`,
但 `st_blocks * 512`(物理占用)= 驻留页数 × 4 KiB + ZIP 字节。一个 8 GiB
sandbox 实际驻留 200 MiB → 文件物理 ~200 MiB。`tar`、`cp --sparse=auto`、
`pkg/ingest` 都尊重稀疏(后者把空洞编码进 manifest.HoleExtent)。

**单 zone 假设**:v1 限定单 memory zone。多 zone 扩展时格式扩展见 §14。

**`<sha256>.overlay` 写入路径**(hash-then-copy):

1. snapshot 完成 srv1.Quiesce() 后,blk1.diff 内容稳定
2. 第一道扫:stream-hash blk1.diff(`io.Copy(sha256.New(), blk1.diff)`)→ digest
3. 第二道扫:用最终文件名 `<out_dir>/<digest>.overlay` 一次 sparse copy
   (`SEEK_DATA/HOLE` 驱动,空洞保留)

不产生临时文件 + rename;输出目录从开始到结束只见最终命名。同一沙箱多次
snapshot 在 overlay 内容不变时**自动写到同名文件**(覆盖,等价于无 op,
天然内容寻址)。

代价是对 blk1.diff 的两次读;但 quiesce 后 blk1.diff 通常驻 page cache(沙箱
刚跑过的写层),第二道读 ≈ 内存读,可忽略。

### 6.2 snapshot 时序

时序原则:
- **overlay 在 memory 之前完整处理**(包括可能的 ingest)→ snapshot.cfg 拿到
  overlay.base 引用一次写入 ZIP,不需要事后回填重写
- **config.json + state.json + snapshot.cfg 全程在内存暂存**,只在最末把 ZIP
  一次性 append
- pause 窗口 = quiesce + CH dump + overlay export + memory dump + zip append。
  overlay + memory 写都是 SEEK_DATA/HOLE 驱动的 sparse copy,稀疏 sandbox
  8 GiB → 驻留 200 MiB → ~100 ms

```
T0  sandbox-ctl snapshot --sandbox-id <sid> [--output <out_dir>] [--upload]
T1  通过 <run-dir>/<sid>/ctl.sock 联系目标 sandbox-ctl run 进程
T2  目标进程串行:
    T2a 通过 vsock 短连接发 quiesce 给 sandbox-init,等 quiesced 响应
        (sandbox-init 完成 sync + drop_caches)
    T2b CH /vm.pause:vCPU 暂停,virtio 设备 quiesce
    T2c srv0.Quiesce() + srv1.Quiesce()(vhost-user-blk backend 排空 inflight)
T3  CH /vm.snapshot { destination_url=file://<run-dir>/<sid>/snap-stage/ }
    patched CH 在该目录写(memory-ranges 自动跳过):
      config.json   - VM 配置(devices, memory layout, ...)
      state.json    - vCPU 寄存器、virtio queue 状态、IRQ 等
    sandbox-ctl 把这两个文件读进内存作为 ZIP 内容暂存,不再落盘
T4  overlay 处理:
    T4a 第一道扫:stream-hash blk1.diff → digest(SHA256 整字节流,hole=0)
    T4b 若 --output:用 <out_dir>/<digest>.overlay 为目标 sparse copy
        (SEEK_DATA/HOLE 驱动);overlay_ref = file://<digest>.overlay
        若 --upload:跳过 hash + 写文件,直接走 pkg/ingest 流式喂 manifest store
        → overlay_manifest_key;overlay_ref = manifest://<overlay_manifest_key>
T5  生成最终 snapshot.cfg(在内存中,§3.4 schema):
    resources.capacity:        从 sandbox 当前 SandboxConfig
    boot.runtime_ref:           file://<basename>@sha256:<digest>(file 模式 host
                                启动时已扫过)或 manifest://<key>(原引用)
    boot.root.base_ref:         同上规则
    boot.root.overlay.base:     T4b 的 overlay_ref
T6  生成 <sid>.snapshot 内容:
    [memory 段]  ramSize 字节,SEEK_DATA/HOLE 驱动的稀疏数据
    [ZIP 段]     从 T3/T5 内存副本一次性写出 config.json / state.json /
                  snapshot.cfg(三个 entries)
    若 --upload:走流式构造,直接 io.Reader 喂 ingest,不落盘;
                  stdout 输出 snapshot_manifest_key
    若 --output:写到 <out_dir>/<sid>.snapshot(稀疏文件 + ZIP 尾)
T7  srv0.Resume() + srv1.Resume()
T8  resume_after=true:CH /vm.resume,沙箱继续运行
    resume_after=false(默认):CH /vm.shutdown,等 CH 退出 → sandbox-ctl run
                  进程也退出
T9  ctl.sock 回 snapshot_done
T10 sandbox-ctl snapshot(发起方进程)收到 done:
    若 --upload:stdout 输出 snapshot_manifest_key
    否则:本地产物在 <out_dir>/(<sid>.snapshot + <sha256>.overlay)
```

**关键差异 vs 一般 VMM 快照**:
- patched CH **不写** memory-ranges
- sandbox-ctl 持有 memfd 自己 sparse 拷贝,**不经 CH→file→sandbox-ctl 的中转**
- ZIP 内 snapshot.cfg 在 T5 一次写入即终态,**没有"事后回填重写 ZIP"步骤**
- 8 GiB sandbox / 200 MiB 驻留:物理 I/O ~200 MiB(过去 ~24 GiB),延迟
  ~1 s 量级
- overlay 文件名内嵌 SHA256(content-addressable),同沙箱多次 snapshot 内容
  不变时自动同名

### 6.3 ctl.sock 协议

`<run-dir>/<sid>/ctl.sock` 是 sandbox-ctl run 进程在 §5 启动时建立的 UDS,
snapshot 子命令通过它触发快照。请求 / 响应都是 JSON,长度前缀(4 字节 LE
uint32)+ payload。

请求(snapshot 子命令 → sandbox-ctl run):

| 字段 | 类型 | 说明 |
|---|---|---|
| `type` | string | `"snapshot_request"` |
| `out_dir` | string | `--output` 目录;snapshot 在该目录写 `<sid>.snapshot` + `<sha256>.overlay`;与 `upload` 互斥 |
| `upload` | bool | true 时走流式 ingest 到 manifest store;与 `out_dir` 互斥 |
| `resume_after` | bool | 默认 false(零值即销毁);CLI 默认与之一致。`--resume` 触发 true |

响应:

```json
{
  "type": "snapshot_done",
  "memory_size":         8589934592,
  "memory_resident":     209715200,
  "wallclock_pause_ms":  12,
  "wallclock_dump_ms":   840,
  "overlay_ref":         "file://<sha256>.overlay" 或 "manifest://<key>",
  "snapshot_key":        "<hex>"   // 仅 upload 模式
}
```

或 `{"type": "error", "msg": "<reason>"}`。run 进程在收到请求时若处于不可
snapshot 状态(CH 已退出 / 配置不一致 / out_dir+upload 都给或都没给)直接回
error。

## 7. 恢复数据流(`run --restore=`)

恢复模式与冷启动共用同一进程入口(`sandbox-ctl run`),只是带 `--restore=<ref>`
flag。复用冷启动 §5.1 的 T6-T18 整段(memfd 准备 → backend 起 → CH 启动 →
va_report → uffd_C 就绪);区别:

1. 配置来源是 `<sid>.snapshot` 末尾 ZIP 内嵌的 config.json / state.json /
   snapshot.cfg,与 host sandbox.yaml 按 §11.0 规则合并
2. snapshotReader 是 `SparseSnapshotSource`(file:// 时)或 `ManifestSnapshotSource`
   (manifest:// 时),不是 `ZeroSource`
3. CH 命令行带 `--restore source_url=...`,不带 `--kernel` / `--vsock`
4. config.json 内捕获了原 run 的 paths(uffd_socket / blk0.sock / blk1.sock /
   vsock.sock),restore 前必须**重写为本次 run 的 paths**(基于 host
   `<run-dir>`)

```
T0  sandbox-ctl run --restore <ref> --config sandbox.yaml [--run-dir <dir>] ...
T1  解析 host sandbox.yaml(本地化字段:network、overlay.diff、cgroup/控制器等)
T2  打开 <ref>:
    file path: os.Open + Stat → ReaderAt
    manifest://: 通过 store + cache 客户端取 manifest → 解封 → fetch.Fetcher 包成 ReaderAt
T3  archive/zip.NewReader(ReaderAt, totalSize) → 解出 config.json / state.json /
    snapshot.cfg
T4  ApplyRestoreOverrides(host sandbox.yaml, snapshot.cfg, snapshotPath):
    - 验证 capacity 一致(host 提供时)
    - 验证 boot.runtime / boot.root.base 协议 + basename + digest 匹配(host 提供时)
    - 未提供时从 snapshot.cfg 复制(file 模式解析为 .snapshot 同目录文件)
    - 详见 §11.0 字段语义表 + §13 校验矩阵
T5  从 snapshot.cfg 拿 capacity 推 ramSize;从 state.json 解出 balloon 状态推
    allocatable_at_snapshot(详见 §11.1)
T6  动态控制模式:Admit{floor, allocatable_at_snapshot} → grant
T7  cgroup setup + TAP 验证 + blk1.diff(全新)准备
T8  state.json 直接写到 <run-dir>/<sid>/snap-state/
    config.json 经路径重写后写入(uffd_socket / blk0/1.sock / vsock.sock 都改为
    本次 <run-dir>/<sid>/ 下的对应名)
T9  memory 准备:同冷启动 §5.1 T6,**唯一差别** snapshotReader = SparseSnapshotSource
    或 ManifestSnapshotSource
T10 blk0 + blk1 backend 起;launch server **不起**(restore 不再 launch);
    va_report UDS server 起,OnReady 内 adopt CH 送来的 uffd_C
T11 spawn cloud-hypervisor (patched):
      --api-socket <run-dir>/<sid>/ch.sock
      --memory-zone size=<ramSize>,shared=on,fd=3,uffd_socket=<run-dir>/<sid>/uffd.sock
      --restore source_url=<run-dir>/<sid>/snap-state/
      --console tty --serial null
    cmd.Stdin/Stdout/Stderr 按 §2.2 stdio 决策接线
T12 CH (patched) 启动:同冷启动 T17a-T17c(创建 uffd_C,sendmsg va_report);
    fill_saved_regions 看到 user_managed zone 不在 ranges 表,自然空操作
    CH 进入 paused 状态
T13 sandbox-ctl 在 va_report 收到 sendmsg 后:
    addrMap.RegisterVMA(ProcessCH, chVA),起 epoll(uffd_C)+ worker pool,回 ack
T14 sandbox-ctl 调 PUT /api/v1/vm.resume → vCPU 从 snapshot 时刻继续
    首访 RAM → fault → handler Absent 分支 → snapshotReader.ReadAt → UFFDIO_COPY
T15 vsock 短连接发 restore{epoch=N} 给 sandbox-init(listener 跨快照保留),
     等 restored 响应作为 guest agent ready 信号(单次 deadline 5 s);
     收到 restored 后(re)start ping ticker;
     deadline 到点未收到 → restore 失败回退:CH /vm.shutdown 并向调用方报错
T16 vCPU 跑,fault 流转见 §8 uffd handler;balloon EVENT_REMOVE 同冷启动
T17 user app 退出 / 接收外部信号 → 退出流程同冷启动
```

CH patched 在 create_ram_region 严格按以下顺序确保 va_report ordering:

```
1. addr = mmap(NULL, ramSize, ..., fd=memfd_fd, 0)
2. uffd_C = userfaultfd();UFFDIO_API;UFFDIO_REGISTER(uffd_C, [addr, +ramSize], MISSING)
3. sendmsg(va_report{addr, ramSize}; SCM_RIGHTS=uffd_C fd) → wait_ack
4. return addr
```

为什么必须 register 在 send 之前:第 2 步之后,任何对 [addr, +ramSize) 的访问
都会触发 fault → 路由到 uffd_C → sandbox-ctl handler。但 handler 此刻还没拿到
fd 也没在 addrMap 里 register chVA,会丢事件。

ack 之前 sandbox-ctl 必须完成:recvmsg → uffd_C fd → addrMap.RegisterVMA →
epoll_create1 + add uffd_C + 起 worker pool,然后才 ack。

## 8. uffd handler

冷启动 + 恢复**统一存在**。架构核心是**单 uffd 模型**:CH 进程内创建 uffd
注册 chVA,通过 SCM_RIGHTS 把 fd 传给 sandbox-ctl 的 handler;sandbox-ctl 自己
mmap 得到的 backendVA **不**单独注册 uffd,kernel 直接走 shmem 缺页路径。

### 8.1 单 uffd 为何足够

典型 cold-start / restore 顺序是 vCPU 先触碰 RAM(kernel boot 扫内存、
sandbox-init 起来、vCPU 走 guest kernel boot)→ chVA fault → uffd_C fire →
handler 通过 UFFDIO_COPY/ZEROPAGE **同时**把 folio 装进 shmem inode + chVA
装 PTE。之后 vhost backend 第一次 memcpy 到 backendVA[N] 时:

```
kernel 缺页处理:
  - sandbox-ctl mm 中 backendVA[N] 没 PTE
  - 该 VMA 没注册 uffd → 走默认 shmem fault path
  - shmem inode 在 offset N 已有 folio(handler 装的) → 直接装 sandbox-ctl mm 的 PTE
  - 不投 uffd 事件,无 race,无需 handler 介入
```

backendVA 上**完全不注册 uffd**就根除了 cross-mm folio-creation race。

### 8.2 SnapshotReader 接口

两模式只在 source 实现上不同。单次 ReadAt 同时返回分类(zero / data)和
source-aligned run length:

```
type SnapshotReader interface {
    // ReadAt 填 buf 中 memfdOffset 起的若干字节并返回 run 分类。
    //   n     页对齐的字节数(PageSize ≤ n ≤ len(buf),EOF 时 0)
    //   zero  true → 该段全零;handler 装零页,不读 buf
    //         false → buf[:n] 是真实数据;handler COPY
    //   err   io.EOF 表示已超出 source 范围
    //
    // source 根据自身布局自由选 n:
    //   - file-backed sparse source 在 IsZero 翻转处截断
    //   - manifest-backed source 在 chunk 边界截断
    ReadAt(buf []byte, memfdOffset uint64) (n int, zero bool, err error)
}

冷启动:    ZeroSource{}
              ReadAt → 永远 (len(buf), true, nil)

file:// 恢复:  SparseSnapshotSource{fd, baseOff, ramSize, holeMap}
              holeMap 启动时一次 SEEK_DATA/HOLE 扫得,每页一 bit
              ReadAt 用 bits.TrailingZeros64 word-level 扫 run,然后 pread 数据段

manifest:// 恢复: ManifestSnapshotSource{fetcher, ctx}
              data run cap 在 chunk 末尾(一次 ReadAt 至多一次 chunk fetch + 解密)
              zero run 跨 hole 和 IsZero chunk 合并(全部走 UFFDIO_ZEROPAGE 不读 store)
```

handler 主逻辑一份代码,模式差异隐藏在 source 实现里。

### 8.3 PageState 状态机

```
type PageState uint8
const (
    StateAbsent   = 0   // folio 还没 handler 装过
    StateLoaded   = 1   // folio 已在 memfd inode + chVA PTE 已装
    StateReleased = 2   // balloon EVENT_REMOVE 后,fault 时直接 ZEROPAGE
)
```

PageState 用 `[]uint8`,size = ramSize / pageSize。每 4 GiB RAM = 1 MiB 状态表。

转移:

| 当前 | 事件 | 处理 | 新态 |
|------|------|------|------|
| Absent | uffd_C PAGEFAULT | OverrideMap 命中 → buf 装入 → UFFDIO_COPY;否则 source.ReadAt → ZERO 段 ZEROPAGE / 数据段 COPY | Loaded |
| Released | uffd_C PAGEFAULT | UFFDIO_ZEROPAGE | Loaded |
| Loaded | uffd_C PAGEFAULT | 正常路径不应出现;UFFDIO_WAKE 兜底 | Loaded |
| 任意 | EVENT_REMOVE on uffd_C | pageStates[range] = Released(同步,fault 路径要看);push 到 removeQ;flusher 异步 batch+merge → madvise(DONTNEED, backendVA);OverrideMap.Drop(pageIdx) | Released |

### 8.4 worker pool

- N worker goroutines,N = `runtime.NumCPU()`,最少 2
- 每个 goroutine LockOSThread(避免 Go runtime 把 worker 移动到别的 OS 线程)
- 一个 reader goroutine **epoll uffd_C**;事件按 `memfd_offset` 哈希分发到
  worker queue
- 哈希用 **memfd offset**(同 page 的事件路由到同 worker 避免状态表争抢)
- worker 收到事件,先检查 OverrideMap,命中走 per-page COPY 路径;未命中走
  source 批量路径

**5.10 内核兼容**:仅依赖 4.11+ 引入的 `UFFD_FEATURE_MISSING_SHMEM` /
`EVENT_REMOVE` / `EVENT_UNMAP` / `THREAD_ID`。**不**用 `MINOR_SHMEM`(5.13+)
/ `UFFDIO_CONTINUE`(5.13+)。

### 8.5 EVENT_REMOVE 处理

CH 在 balloon free_page_reporting 路径里,对每段 guest 上报为空闲的页同时做
两件事:

1. 在 memfd 上 `fallocate(FALLOC_FL_PUNCH_HOLE | KEEP_SIZE)` —— 释放 inode 页
2. 在 chVA 上 `madvise(DONTNEED)` —— 清自己进程的 PTE

其中 (1) 触发 kernel 通过 `unmap_mapping_range` 向所有 mapping 该 inode 的
VMA 投放 PTE 失效,在 chVA 上则合成一条 `EVENT_REMOVE` 投递到 uffd_C 的事件
队列。

handler 收到 `EVENT_REMOVE` 后做 **process-level reclaim**:对 sandbox-ctl
自己的 backendVA mmap 做 `madvise(DONTNEED)`,把进程级 PTE/RSS 份额也清掉。

**双端职责划分**:

| 端 | 动作 | 释放对象 | 触发时机 |
|---|---|---|---|
| CH(balloon 处理函数) | `fallocate(PUNCH_HOLE)` on memfd + `madvise(DONTNEED)` on chVA | inode 页 + CH 自己的 PTE | guest free_page_reporting 上报 |
| sandbox-ctl handler | `madvise(DONTNEED)` on backendVA | sandbox-ctl 自己的 PTE/RSS | 收到 `EVENT_REMOVE` |

**事件量级与异步化**:cold-start 前 ~1s,balloon inflate + free_page_reporting
把 ~7-8 GiB 空闲页一次性还给 host,产生数千条 `EVENT_REMOVE` 事件。reader
goroutine 同步处理 madvise 会被 syscall 时间挤占,page-fault 派发饿死。
所以 EVENT_REMOVE 路径异步化:reader 只更新 pageStates(同步,fault 路径要
看)+ push 到 removeQ;flusher goroutine batch + merge contiguous ranges +
sort by offset → 几十次 madvise 完成数千事件。

**关键不变量**:CH 的 fallocate(PUNCH_HOLE) 与 sandbox-ctl 的
madvise(DONTNEED) 是**互补**的,不是 redundant。前者管 file pages,后者管
process PTE,缺任一边都泄漏。

## 9. cgroup 与 balloon 联动

### 9.1 cgroup 内存设置(静态 cgroup / 动态控制模式)

```
memory.max       ← capacity_bytes + overhead.memory       # 不变
memory.high      ← watermark_high.memory                  # 静态 cgroup 模式恒为该值;
                                                              动态控制模式随 allocatable_now × ratio 变化
memory.swap.max  ← 0                                      # 禁 swap
```

**关键决策点**:

- `memory.max = capacity + overhead`,**不等于 allocatable**。把 memory.max
  设成 allocatable 时,guest 合法使用到 allocatable 上限会让 cgroup OOM kill
  CH 进程,等同于平台主动终结沙箱
- `memory.high < memory.max` 留出反压窗口:guest 内存接近 high 时内核给 CH
  进程内存分配加 PSI 延迟,但不 kill
- 禁用 swap:超分语义下 swap 会让 OOM 决策路径模糊

### 9.2 cgroup CPU 设置

cgroup_path 已设的情况下,sandbox-ctl 在该 cgroup 内写两项,启动后**全程
不变**:

```
cpu.max     ← capacity.cpu × period " " period               # period = 100ms
cpu.weight  ← clamp(round(allocatable.cpu × 100), 1, 10000)
```

**意义**:
- `cpu.max` 是 cgroup 的硬上限。设成 capacity 意味着**无 CPU 竞争时,沙箱可以
  跑满 capacity 核**——这是平台对应用的承诺
- `cpu.weight` 是公平共享权重,仅在多个 cgroup 同时争用 CPU 时生效。
  `allocatable.cpu × 100` 让 1 核 allocatable 对应权重 100(等于内核默认),
  0.1 核 → 10,2 核 → 200

**结合后语义**:
- 节点 CPU 充裕(总用量 < 物理核数):每沙箱可达 cpu.max(= capacity)
- 节点 CPU 紧张:内核 CFS 按 cpu.weight 比例分配,在 admission 保证
  `Σ allocatable.cpu ≤ physical_cpu` 的前提下,每沙箱至少等于 allocatable.cpu

这套静态模型实现了"无竞争时给 capacity / 有竞争时给 floor",**完全不需要
运行时调整 cpu.max,也不需要 CPU 维度的 burst/recover 状态机或 RPC**。

### 9.3 balloon 配置

CH 命令行(三种模式都用,跟 cgroup 解耦):

```
--balloon size=<initial>,free_page_reporting=on[,deflate_on_oom=on]
```

- `size = capacity − allocatable_now`(无 cgroup / 静态 cgroup 模式 = capacity −
  allocatable.memory;动态控制模式启动时 = capacity − startup_burst.memory)
- `free_page_reporting=on` 持续回收 guest 内 free 页
- `deflate_on_oom=on` 由 `allocatable.deflate_on_oom` 决定

balloon target 的运行期变化(仅动态控制模式)通过 CH HTTP API
`PUT /api/v1/vm.resize`(payload `{"desired_balloon": <bytes>}`)发起。

### 9.4 deflate_on_oom 安全网

deflate_on_oom 触发链路:

1. Guest 应用申请内存,guest 内 RAM 用尽
2. Guest 内核 OOM killer 即将触发,balloon driver 拦截
3. Balloon driver 从 balloon 池释放页给 guest 进程
4. CH 在 host 上 RSS 增长,但 < memory.max(静态 cgroup / 动态控制模式)

定位:**双重失败的最后防御**。动态控制模式正常路径下:
- 第一道:cgroup memory.high PSI 给 CH 进程内存 alloc 加延迟
- 第二道:sandbox-ctl 检测 high 事件 → 控制器 grant → balloon deflate

只有当反馈环路追不上 guest 增长时,deflate_on_oom 才生效,代价是 guest 内
进程被杀。无 cgroup / 静态 cgroup 模式没有反馈环路,deflate_on_oom 是唯一
防线,所以默认开启。

## 10. 与 node-ctl 的资源协议

详细协议规范见 [`node.md`](node.md) §协议;本节描述 sandbox-ctl 侧的执行器
行为。

### 10.1 沙箱状态机

动态控制模式下沙箱经过 7 个阶段;静态 cgroup 模式只走 admitted → creating →
startup → settled,不进入 burst / recover;restoring 仅在快照恢复路径出现:

| 阶段 | 触发 | allocatable_now 内存策略(动态控制模式) | sandbox-ctl 行为 |
|------|------|----------------------------------|-------------------|
| **admitted** | 收到 Admit grant | reservation 占用预算,沙箱未启动 | 验证 cgroup_path,准备 socket |
| **creating** | 开始创建 CH 等 | reservation 持有 | 拉起 CH、handshake |
| **startup** | CH 已启动,等 launch hello | startup_burst.memory | 等 launch protocol hello |
| **restoring** | CH /vm.restore + /vm.resume 完成,等 restore-ack | allocatable_at_snapshot 或降级值 | 调用 SendRestore 等 ack |
| **settled** | hello 收到 / SendRestore 返回 nil | 渐缩到 floor + 工作集余量 | 周期上报 RSS;发 Settled |
| **burst** | 检测到压力 | 申请扩展,可达 capacity | resize-balloon、改 memory.high |
| **recover** | 压力消退 + 冷却 | 不主动收回,等被动回缩 | 继续上报 |

**settled 触发是事件驱动,不依赖定时器**:

- **冷启动 startup → settled**:由 sandbox-init phase 2 拨号 launch server
  发 `hello` 消息触发——sandbox-init 在 mount/network 等平台初始化完成、即将
  拿到 launch spec 启动用户进程的时刻
- **恢复 restoring → settled**:由 `SendRestore(epoch)` 返回 nil 触发
  (host→guest 反向 channel,sandbox-init 立即 reply restored)

### 10.2 各阶段的 cgroup 与 balloon

动态控制模式下(静态 cgroup 模式全程对应 settled 列):

| 阶段 | memory.max | memory.high | balloon target | cpu.max / cpu.weight |
|------|-----------|-------------|----------------|---------------------|
| admitted | capacity+overhead | startup_burst × ratio | (未启动 CH) | 静态(永不变) |
| creating | 同上 | 同上 | (CH 启动中) | 同上 |
| startup | 同上 | 同上 | capacity − startup_burst | 同上 |
| restoring | 同上 | allocatable_at_snapshot × ratio | capacity − allocatable_at_snapshot | 同上 |
| settled | 同上(永不变) | allocatable_now × ratio | capacity − allocatable_now | 同上 |
| burst | 同上 | allocatable_now × ratio(allocatable_now ↑) | capacity − allocatable_now ↓ | 同上 |
| recover | 同上 | 同上 | 同上 | 同上 |

`ratio = watermark_high.memory / allocatable.memory`,默认 0.875。memory.max、
cpu.max、cpu.weight 全程不变。memory.high 与 balloon target 随 allocatable_now
同步变化。

### 10.3 压力信号(动态控制模式)

sandbox-ctl 内部周期 100 ms 读取:

| 文件 | 信号 | 用途 |
|------|------|------|
| `memory.events.local` | high 计数差(本周期新增) | 主信号:有新 high 事件 → 申请扩展 |
| `memory.events.local` | oom 计数差 | 紧急信号:发 urgency=high 申请 |
| `memory.current` | RSS 数值与短周期斜率(过去 1s) | 预测信号:即将触 high 时提前申请 |
| `memory.pressure` | PSI memory.some.avg10 | 诊断,不入决策 |

不采纳:guest balloon STATS_VQ(5 s 周期太粗);uffd fault rate(信号扭曲);
guest 内进程级压力(跨 host/guest 边界,接口复杂)。

### 10.4 长连维持与降级

**长连维持**:

- 沙箱进入 startup 后,连接保持活跃;sandbox-ctl 在此连接上发后续 RPC、收
  ReclaimRequest/UpdateConfig 推送
- 30s 周期 Heartbeat;控制器 90s(3 个周期)未收到 → 视为掉线

**断连降级**:

- 连接断开,sandbox-ctl 退避重试(1s, 2s, 5s, 10s, 10s, ...)
- 持续 60s 重连失败 → 切到无控制器模式继续:保持当前 allocatable 不变,接管
  cgroup memory.high 设置,不再申请 burst
- 此时即使有新压力,只能靠 cgroup PSI 反压 + deflate_on_oom 兜底
- 重连成功后自动恢复联动

## 11. 恢复时的资源衔接

恢复路径与冷启动有两层差别:**(a)** sandbox.yaml 里大量字段在 restore 模式下
有特殊语义(与 snapshot.cfg 合并、覆盖、断言式校验);**(b)** guest 在快照里
**已有 in-memory 状态**(driver 缓冲、应用堆、page cache),`/vm.resume` 之后
cloud-hypervisor 按快照 page 索引把这些页 fault 回 guest 物理地址,host
allocatable 初值必须够大才能避免 PSI 节流 / sensor 反复 burst。

§11.0 解决 (a),§11.1-§11.3 解决 (b)。

### 11.0 restore 模式下 sandbox.yaml 字段语义

`run --restore=<ref>` 时,sandbox.yaml 与内嵌 snapshot.cfg 按下表合并。
"提供时"指 yaml 里该字段非零值,"未提供"指空值或字段缺失。

| sandbox.yaml 字段 | 提供时 | 未提供时 |
|---|---|---|
| `resources.capacity.{cpu,memory}` | 与 snapshot.cfg 严格相等才允许;不一致拒绝启动(error: "capacity mismatch") | 直接用 snapshot.cfg.resources.capacity |
| `resources.allocatable.*` | 与冷启动语义相同(host 资源策略) | 沿用冷启动默认(等于 capacity) |
| `network.tap` | 必须;沙箱挂到该 TAP | error: missing(restore 不能没网络配置) |
| `network.{ip,gateway,hostname,interface}` | 用作本次恢复的网络配置 | 跳过 IP / hostname 配置,沙箱起来后自行处理 |
| `boot.kernel` | 静默忽略(restore 不 boot) | 同 |
| `boot.runtime`(仅 file://) | basename + sha256 digest 与 snapshot.cfg.runtime_ref 全部匹配才允许;否则拒绝 | 用 snapshot.cfg.runtime_ref:basename 解析为 `<sid>.snapshot` 同目录文件 |
| `boot.root.base`(file://) | 协议 + basename + digest 与 snapshot.cfg.base_ref 一致才允许 | 用 snapshot.cfg.base_ref:basename 解析为 `<sid>.snapshot` 同目录文件 |
| `boot.root.base`(manifest://) | manifest key 与 snapshot.cfg.base_ref 一致才允许 | 用 snapshot.cfg.base_ref 原值 |
| `boot.root.overlay.base` | **静默忽略** | 用 snapshot.cfg.overlay.base |
| `boot.root.overlay.diff` | 必须 file:// 绝对路径;沙箱写层 | error: missing |
| `boot.root.overlay.size` | 与冷启动一致(默认 10 GiB) | 默认 10 GiB |
| `boot.cmdline` | 静默忽略(restore 不 boot) | 同 |
| `launch.*` | 静默忽略(应用在 guest 内存里) | 同 |
| `control.cgroup_path` / `control.controller` | 用作本次恢复的资源策略 | 同冷启动默认 |
| `overhead` / `watermark_high` / `startup_burst` | 同 control 规则 | 同冷启动默认 |
| 其他 | 静默忽略 | — |

**为什么 capacity 必须严格相等(而不是 max)**:guest 内存中已经按当时
capacity 决定了页面布局、kernel 内部数据结构(NR_CPUS / per-cpu data /
zone watermarks)。变了 capacity 等于换了一套硬件假设,行为未定义。

**为什么 file:// runtime / base 要 digest 校验**:跨主机移动 snapshot 时,
目标 host 上同名 sandbox-runtime.erofs / container-image.erofs 可能是不同
版本。digest 不匹配的恢复不安全(guest 期望 inode 数据与文件实际数据不一致,
EROFS 挂载或运行时读会读到诡异数据)。

**boot.root.overlay.base 为什么忽略 yaml**:overlay 数据是 sandbox 自己的写
状态,与镜像 base 等价物,只能从 snapshot 内部走。让 yaml 强制提供没意义,
反而引入"不一致"风险面。

**绝对路径要求**:restore 模式下 yaml 提供的 file:// URL 仍要求绝对路径
(允许的是"整字段不写",不是"写相对路径")。

### 11.1 唯一来源:CH 自己的 balloon 状态

`sandbox-ctl` 在运行期通过 `applyAllocatable()` 维护 `balloon.target =
capacity − allocatable_now`,因此 balloon target/current 已经把 allocatable_now
的语义编码进去了。CH 在 `/vm.snapshot` 时把 balloon 设备状态(含 `num_pages`
= host 想拿走的页数 / target、`actual` = guest 已交还的页数 / current)写入
bundle 内的 `state.json`,**无须再额外保存**。

恢复时 sandbox-ctl 从 bundle 解出:

| 字段 | 来源 | 含义 |
|---|---|---|
| `capacity` | `snapshot.cfg` 的 `resources.capacity.memory` | 快照时的 memory-zone 容量 |
| `balloon.target` | `state.json` `snapshots["device-manager"].snapshots["__balloon"]` 内的 `config.num_pages` × 4 KiB | host 当时想保留多少页(对应 capacity − allocatable_now) |
| `balloon.current` | 同上 `config.actual` × 4 KiB | guest balloon 驱动当时已实际交还的页数 |

派生:

```
allocatable_at_snapshot = capacity − min(balloon.target, balloon.current)
```

取 `min` 是为了在 balloon 还没收敛时拿到更宽裕的 allocatable:
- **inflating**(host 想拿更多,guest 还没让出,target > current):取 current
  → guest 此刻仍持有较大有效内存,fault 重放时给它这个量,平滑过渡
- **deflating**(host 想还给 guest,guest 还没扩张,target < current):取
  target → host 已经计划放出,sensor/heartbeat 会让 guest 后续扩到该值

恢复后,sandbox-ctl 通过 `/vm.resize` 把 balloon target 重新设回该 allocatable
对应的值,使 host 与 guest 视图一致。

### 11.2 无 cgroup / 静态 cgroup 模式(无控制器)的恢复决策

```
initial_alloc = max(yaml.allocatable.memory, allocatable_at_snapshot)
```

`max` 的语义:从动态控制模式拍下的快照可能 allocatable_at_snapshot >
yaml.allocatable(运行期 burst 过),恢复到静态部署时若直接用 yaml,guest
的工作集会被压回 yaml,触发 PSI 节流。`max` 让恢复保留 burst 后的工作集
大小;若运营策略要求严格遵守 yaml,可在 `sandbox.yaml` 里把 capacity 也调小
到 yaml.allocatable,那时 allocatable_at_snapshot ≤ yaml(由 capacity 自然
约束)。

### 11.3 动态控制模式(有控制器)的恢复决策

`sandbox-ctl` 在 `Admit` 中携带:

| 字段 | 值 | 角色 |
|---|---|---|
| `floor_memory_bytes` | `yaml.allocatable.memory` | 控制器降级时的回退下限 |
| `allocatable_at_snapshot` | 上一节派生值 | 控制器优先尝试授予的 QoS 目标 |

控制器复用 admit 逻辑——把 `allocatable_at_snapshot` 当 requestedInitial、
`floor` 当 fallback——**恢复路径没有新分支**:

| 余量情况 | granted_initial_alloc |
|---|---|
| 可分配余量 ≥ allocatable_at_snapshot | = allocatable_at_snapshot(完美还原) |
| floor ≤ 可分配余量 < allocatable_at_snapshot | = floor(降级) |
| 可分配余量 < floor | status=rejected,上层调度器换节点 |

降级路径下,sensor 在 restoring 阶段已激活,若 fault 重放压满 cgroup memory.high
就立即 RequestBudget(urgency=normal),逐步把 allocatable 拉回 burst 状态;
预算不允许时,沙箱以 floor 长期运行,直到节点预算释放或被调度走。

## 12. vhost-user-blk backend

### 12.1 协议子集

实现以下 vhost-user 消息(其他不实现,收到回 not_supported):

| Message | 用途 |
|---------|------|
| GET/SET_FEATURES | feature negotiation |
| GET/SET_PROTOCOL_FEATURES | protocol-level extensions |
| SET_OWNER / RESET_OWNER | ownership 锁 |
| SET_MEM_TABLE | guest physical memory layout(关键,带 fd) |
| SET_VRING_NUM/ADDR/BASE/KICK/CALL/ENABLE | virtq 编程 |
| GET_QUEUE_NUM | virtq 数量 |
| GET_CONFIG / SET_CONFIG | virtio-blk 配置区 |

### 12.2 SET_MEM_TABLE 行为契约

由于统一内存所有权模型,sandbox-ctl 永远先于 CH mmap memfd 并 register uffd,
backend 收到 SET_MEM_TABLE 时**永远**走"复用 backendVA"路径:

```
对每个 region:
  fstat(fd).Ino 与启动时记录的 sandbox-ctl-mmap'd memfd inode 比对
    匹配 → 注册 (gpa, hva = backendVA + mmap_offset, size) 进 GPA→HVA 表
            不再 mmap,不再 register uffd
    不匹配 → 拒绝(协议错误,违反统一模型 invariant)
关闭收到的 fd
回 ack
```

只有一种正确路径,没有冷启动 / 恢复分支。

### 12.3 BlockReader 接口契约

```
BlockReader:
  ReadAt(buf []byte, offset int64) → (int, error)
  Size() int64
  Close() error
```

实现:
- **FileReader**:本地 `pread(2)`;Size 来自 stat
- **ManifestReader**:从 store-ctl + cache-ctl 拿 manifest blob → 解码 + 解封
  keys → `fetch.Fetcher` → ReadAt 时零填充 hole 区段;Size 来自 manifest
  元数据。manifest hole 在块设备语义下等价于零页,Fetcher 内部直接 memset
  不再走 store

blk0 backend:写请求(IN/DISCARD/FLUSH)拒绝,返回 IO_ERR;读请求经
BlockReader 拿数据,写到 guest buffer (HVA)。

manifest:// 路径需要 sandbox-ctl 持有 store + cache 客户端 + chunk + key-table
加密器 + customer key —— 整套从清单配置在 sandbox 启动时一次性建立,
blk0 / overlay base 共享同一套客户端连接池,sandbox 退出时统一释放。

### 12.4 blk1 base+diff COW

blk1 是可写盘,基础语义为"上层 ext4 sparse 文件 + 可选 base 层":

```
读路径:
  for each 4K block in [off, off+len(buf)):
    if dirtyBitmap[blk]:    pread(diffFile, slice, blk*4K)
    elif baseReader != nil: baseReader.ReadAt(slice, blk*4K)
    else:                   memset(slice, 0)

写路径:
  pwrite(diffFile, buf, off);
  for each 4K block: dirtyBitmap[blk] |= 1
```

启动时通过 SEEK_DATA/HOLE 扫 blk1.diff 重建 dirtyBitmap。

DISCARD 路径(无 base layer 时正确):`fallocate(PUNCH_HOLE)` + bitmap 清掉;
读 ReadAt 看到 bitmap 干净 → memset(0)。**带 base layer 时**需要扩展为三态
`{ clean, dirty, discard }`(详见扩展点)。

### 12.5 Quiesce / Resume

snapshot 路径要求 `/vm.pause` 之后内存内容稳定,但 backend worker 可能正在
`processChain` 写 backendVA(virtio IN 完成阶段把 disk 数据 memcpy 到 GPA)。
若不同步会导致快照内存与 vCPU 看到的状态不一致。

- `Server.Quiesce()`:返回时,该 server 没有任何 worker 在 `processChain`
  中,且后续不会有新一轮 `processChain` 进入(直到 `Resume()`)
- `Server.Resume()`:解除 Quiesce,worker 在下一次 KICK 时正常处理
- 两者**只用于 snapshot 路径**,主流程 cold-start 不调

调用顺序见 §6.2 T2c。

## 13. 配置校验矩阵

### 13.1 冷启动校验(`ValidateCold`)

冷启动模式下 `ValidateCold` 必须强制以下规则,违反任一即报错并退出:

| 规则 | 错误消息 |
|------|---------|
| 所有 file:// URL 必须绝对路径(filepath.IsAbs) | "<field> file:// must be absolute" |
| `cgroup_path` 为空时,`controller` 必须为空 | "controller requires cgroup_path" |
| `cgroup_path` 为空时,`overhead` / `watermark_high` / `startup_burst` 必须未设 | "<field> requires cgroup_path" |
| `cgroup_path` 为空时,`allocatable.cpu == capacity.cpu` | "fractional cpu requires cgroup_path" |
| `cgroup_path` 已设时,该路径必须存在(系统调用检查) | "cgroup_path <p> does not exist" |
| `cgroup_path` 已设但 `controller` 为空时,`startup_burst` 必须未设 | "startup_burst requires controller" |
| `controller` 已设时,`startup_burst.memory` 满足 `floor ≤ ≤ capacity` | "startup_burst.memory out of [floor, capacity]" |
| `allocatable.cpu ≤ capacity.cpu` 且均 > 0 | "allocatable.cpu must be in (0, capacity.cpu]" |
| `allocatable.memory ≤ capacity.memory` | "allocatable.memory must be ≤ capacity.memory" |
| `overhead.memory ≥ 0` | "overhead.memory must be non-negative" |
| `watermark_high.memory > 0` 且 `≤ allocatable.memory`(静态 cgroup / 动态控制模式启动初值) | "watermark_high.memory out of (0, allocatable.memory]" |

`allocatable.memory == capacity.memory` 时若 `deflate_on_oom` 显式写 true
(默认值)不报错,记 warn 日志:"deflate_on_oom set but balloon not configured"。

### 13.2 stdio flag 互斥(`run --restore` / 冷启动同)

| 规则 | 错误消息 |
|------|---------|
| `--tty` 与 `--stdin` / `--stdout` / `--stderr` / `--stdin-from` / `--stdout-to` / `--stderr-to` 任一显式赋值互斥 | "--tty conflicts with explicit stdio flags" |
| `--stdin=false` 与 `--stdin-from` 互斥 | "--stdin=false conflicts with --stdin-from" |
| `--stdout=false` 与 `--stdout-to` 互斥 | "--stdout=false conflicts with --stdout-to" |
| `--stderr=false` 与 `--stderr-to` 互斥 | "--stderr=false conflicts with --stderr-to" |

### 13.3 snapshot 子命令校验

| 规则 | 错误消息 |
|------|---------|
| `--output` 与 `--upload` 必须二选一,不能都给或都不给 | "--output and --upload are mutually exclusive; one is required" |

### 13.4 restore 模式校验(`ValidateRestore` + `ApplyRestoreOverrides`)

`run --restore=<ref>` 触发。`ValidateCold` 的 file:// 绝对路径 + cgroup
gating 规则仍生效;额外:

| 规则 | 错误消息 |
|------|---------|
| sandbox.yaml 提供 `boot.runtime` 时,协议必须与 snapshot.cfg.runtime_ref 一致 | "boot.runtime scheme mismatch with snapshot.cfg" |
| sandbox.yaml 提供 `boot.root.base` 时,协议必须与 snapshot.cfg.base_ref 一致 | "boot.root.base scheme mismatch with snapshot.cfg" |
| sandbox.yaml 提供 file:// runtime / base 时,basename(filepath.Base)必须与 snapshot.cfg ref 中 basename 一致 | "<field> basename mismatch with snapshot.cfg" |
| sandbox.yaml 提供 file:// runtime / base 时,文件 SHA256 digest 必须与 snapshot.cfg ref 中 @sha256:<digest> 一致 | "<field> digest mismatch with snapshot.cfg" |
| sandbox.yaml 提供 manifest:// runtime / base 时,manifest key 必须与 snapshot.cfg ref 一致 | "<field> manifest key mismatch with snapshot.cfg" |
| sandbox.yaml 提供 `resources.capacity.{cpu,memory}` 时,与 snapshot.cfg 严格相等 | "capacity mismatch with snapshot.cfg" |
| sandbox.yaml 必须提供 `boot.root.overlay.diff`(file:// 绝对路径) | "boot.root.overlay.diff is required in restore mode" |
| sandbox.yaml 必须提供 `network.tap` | "network.tap is required in restore mode" |

## 14. 已知限制与扩展点

按根因分三类。**§14.1** 是固有架构约束(本系统不做);**§14.2** 是平台 ABI
边界(用户应用要更多功能须自带 kernel);**§14.3** 是预留的扩展点。

### 14.1 固有架构约束

- **THP for sandbox 内存**:uffd 在 4 KiB 粒度操作,THP 把多页折成 2 MiB 大页
  会破坏 fault 路由。`MADV_NOHUGEPAGE` 在 mmap 之后立即下
- **confidential VM**(SEV-SNP / TDX):需要 `guest_memfd` 而非普通 memfd,
  跟 uffd 协议不兼容
- **跨节点 live migration**:page server / source-on-demand 协议是另一个产品
  级特性。本系统是单节点 snapshot + restore
- **多 memory zone**:NUMA / virtio-mem 用例;本设计是固定 spec、单 zone
- **restore 后再 snapshot**:uffd-backed 内存做 snapshot 的状态收敛复杂度高;
  本设计 snapshot/restore 是单向的
- **多 sandbox 共享 sandbox-ctl**:per-sandbox 一进程是有意的架构选择(故障
  域、资源记账)

### 14.2 平台 ABI 边界(用户应用须自带 kernel)

guest kernel 是平台提供的最小镜像,以下功能默认不开。需要的用户 app 自带
vmlinux 通过 `boot.kernel: file://...` 提供:

- **in-guest cgroups**:host 通过 cgroup v2 限制 CH 进程;guest 不能再切子层
- **in-guest USER / NET / UTS / IPC / TIME 命名空间**:相关 `CONFIG_*_NS`
  未启用,`unshare(CLONE_NEW*)` 返回 EINVAL
- **in-guest userfaultfd 系统调用**:`CONFIG_USERFAULTFD` 未启用
- **NR_CPUS 上限 4**

详见 [`sandbox-kernel.md`](sandbox-kernel.md) §平台 ABI 边界。

### 14.3 扩展点(用例驱动)

| 扩展点 | 引入条件 | 影响章节 |
|---|---|---|
| **DISCARD with overlay.base layer** | 引入 manifest:// base + 本地 diff 部署形态 | 三态 stateMap(clean/dirty/discard,2 bits/block) |
| **memory hotplug / virtio-mem** | 弹性扩缩用例 | uffd 动态 register、状态表扩容 |
| **incremental snapshot** | 频繁 snapshot 同一 sandbox 的用例 | KVM_GET_DIRTY_LOG 接入 + log-mode CH 协调 |
| **应用 quiesce hook** | 跨实例去重率超过 PROPOSAL §4 量化的"非确定性 50-70%"上限的用例 | sandbox-runtime quiesce 扩展项表 |

## 15. See Also

- [`sandbox-runtime.md`](sandbox-runtime.md) —— guest 内 sandbox-init 三阶段
  与 vsock 控制面协议
- [`cloud-hypervisor.md`](cloud-hypervisor.md) —— CH patches、命令行选项、
  外部托管内存契约
- [`sandbox-kernel.md`](sandbox-kernel.md) —— guest kernel defconfig、平台
  ABI 边界
- [`node.md`](node.md) —— 资源控制协议规范、节点级仲裁、admission、reclaimer
- [`manifest.md`](manifest.md) —— manifest:// 资源拉取通道(blk0 base、
  snapshot)
- [`cache.md`](cache.md) —— sandbox-ctl 通过 cache-ctl 客户端做 chunk-level
  请求
- [`flatten.md`](flatten.md) —— 构建 boot.root.base 的 EROFS 镜像
- [`build.md`](build.md) —— sandbox-ctl + sandbox-init + cloud-hypervisor +
  vmlinux 的整体构建流程
- [`perf.md`](perf.md) —— 沙箱性能基线与密度调优
- `PROPOSAL.md` §4 / §6.9-6.11 / §10.2 —— 沙箱在系统中的位置与目标
