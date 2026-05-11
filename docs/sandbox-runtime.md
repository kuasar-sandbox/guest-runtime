# sandbox-runtime — guest 运行时

guest 内 PID 1 二进制 `sandbox-init` 与承载它的根文件系统镜像
`sandbox-runtime.erofs`。负责沙箱启动期 rootfs 组装、应用拉起、生命周期监督、
以及与 host sandbox-ctl 之间的控制面通信。

`sandbox-runtime` 是节点级共享资产——所有 sandbox 通过 virtio-pmem + DAX
直接映射 host 上同一份 erofs 文件,获得无运行时拷贝、跨 sandbox 共享 host
page cache 的密度收益。

## 1. 概述

### 1.1 设计目标

| 目标 | 实现方式 |
|---|---|
| 启动快 | 单一静态 Go 二进制 PID=1,无 systemd / dracut / busybox 链路 |
| 跨实例可去重 | 启动期内存页内容确定;sandbox-init 自身镜像版本固化 |
| 跨 sandbox 共享 | virtio-pmem + DAX 让 host page cache 一份 RAM 跨 N 个 sandbox |
| 一 VM = 一 app | sandbox-init clone(NEWPID|NEWNS) 让用户 app 看到自己 PID=1 |
| 生命周期可寻址 | exit / SIGTERM / quiesce / restore 都通过 vsock 通知 host |

### 1.2 系统中的位置

```
   HOST                                                      GUEST VM
   ────                                                      ────────

   sandbox-ctl ── spawn CH ──►  cloud-hypervisor             kernel boot
                                       │                          │
                                       │ virtio-pmem / DAX        │  pivot to /
   sandbox-runtime.erofs  ◄────────────┤  (host shared file)      │  /sbin/init = sandbox-init
                                       │                          │    phase 1: mount + overlayfs
                                       │                          │    phase 2: vsock launch handshake
                                       │                          │    phase 3: supervisor loop
                                       │                          │
                                       ├──────── launch ─────────►│    fork/exec user app
                                       ├──────── ping ───────────►│      - new PID / Mount ns
                                       │◄──────── app_started ────┤      - app sees itself PID 1
                                       │                          │
   /run/<sid>/vsock.sock  ◄────────────┤◄──────── app_exited ─────┤    user app exits
                                       │                          │    reboot()
                                       │◄──────── CH exits ───────┤
```

### 1.3 不做的事

- **无容器运行时**:平台直接管理 sandbox 生命周期,不引入 runc / crun / podman
- **无 systemd / OpenRC**:进程监督由 sandbox-init 自己写的 supervisor 完成
- **无 busybox / util-linux**:所有功能(mount / mkdir / chdir / pivot_root /
  reboot)通过 Go syscall 完成,镜像只装一个 sandbox-init
- **无 /etc / /usr**:guest rootfs 由用户镜像(blk0 base)提供;sandbox-runtime
  只提供 /sbin/init 和挂载点

## 2. sandbox-runtime.erofs 镜像结构

```
/sbin/init                sandbox-init 静态 Go 二进制,~10-15 MiB
/proc/                    空挂载点
/sys/                     空挂载点
/dev/                     空挂载点
/mnt/lower/               空挂载点(blk0 EROFS 挂入点)
/mnt/upper/               空挂载点(blk1 ext4 挂入点)
/mnt/newroot/             空挂载点(overlayfs 合并目标 + pivot_root 目标)
```

**没有其他文件**——无 /etc、/usr、/var、/lib、共享库等。所有额外功能由
sandbox-init 通过 Go syscall 实现。

镜像小(~15 MiB)+ DAX 直接映射 host page cache,N 个 sandbox 共享同一份内存
工作集(实际 ~10 MiB 驻留)。EROFS 文件格式 endian-neutral,任意 host arch 上
的 mkfs.erofs 都可生成镜像;镜像内的 `/sbin/init` 是 target arch 二进制。

构建方式见 [`build.md`](build.md) `make sandbox-runtime` target。

## 3. sandbox-init 三阶段

### 3.1 阶段 1:早期挂载 + rootfs 组装

```
1. mount -t proc      proc       /proc
2. mount -t sysfs     sysfs      /sys
3. mount -t devtmpfs  devtmpfs   /dev   (kernel CONFIG_DEVTMPFS_MOUNT=y 时跳过)
4. wait for /dev/vda 和 /dev/vdb 出现(轮询 stat,timeout 10s)
5. mount -t erofs -o ro /dev/vda /mnt/lower
6. mount -t ext4         /dev/vdb /mnt/upper
7. mkdir /mnt/upper/upperdir /mnt/upper/workdir 若不存在
8. mount -t overlay overlay -o lowerdir=/mnt/lower,
                                 upperdir=/mnt/upper/upperdir,
                                 workdir=/mnt/upper/workdir  /mnt/newroot
9. pivot_root + chroot + exec /proc/self/exe → 进入阶段 2
```

全部通过 `golang.org/x/sys/unix.Mount` + `unix.Pivot_root` 等 syscall 完成,
不依赖任何外部二进制。

`/dev/vda`(blk0 base)是用户应用的镜像 erofs(只读);`/dev/vdb`(blk1 overlay
ext4)是写层。overlay 合并后 `/mnt/newroot` 是 guest rootfs 的最终视图,
`pivot_root` 之后这套视图变成新的 `/`。

### 3.2 阶段 2:从 host 拿 launch 配置 + 网络配置 + 应用拉起

```
1. socket(AF_VSOCK, SOCK_STREAM) → bind+listen :5000
   反向通道(host→guest)早于 hello 开门,vsock 不依赖网络配置;
   listener 接收 ping/restore/quiesce 等(协议见 §4)
   accept-and-dispatch loop 在独立 goroutine 跑,跨阶段 3 持续存在

2. socket(AF_VSOCK, SOCK_STREAM) → connect(CID=2 host, port=5000)
   重试策略:第一次立即发起,失败后 100µs 起指数退避(×2,上限 10ms),deadline 5s
3. write {"type":"hello","phase":"ready"}
4. read launch message → LaunchSpec(exec/args/env/workdir/restart, network)
5. close(vsock_fd)              ← 单次握手,launch 收完即断

6. 若 LaunchSpec.Network 非空,applyNetwork:
   - Sethostname(network.hostname)
   - 通过 raw netlink RTM_NEWLINK 把 interface 拉 UP
   - RTM_NEWADDR 配置 IP/CIDR
   - 若 gateway 非空,RTM_NEWROUTE 装默认路由
   失败 fast-fail —— 一次性沙箱模型下"网络配置失败"必须立刻 die,不让用户
   app 在没有网络的环境里悄悄跑

7. 通过 exec.Cmd 拉起子进程:
   SysProcAttr.Cloneflags = CLONE_NEWPID | CLONE_NEWNS
   argv: [/proc/self/exe, "exec-child", workdir, exec, args...]
   env:  LaunchSpec.Env(默认补 PATH)

8. 子进程在新 ns 内做:
   - mount -t proc proc /proc       (反映新 PID ns)
   - chdir(workdir)
   - syscall.Exec(exec, args...)

9. 父进程(sandbox-init pid=1)向 host 发短连接 app_started{pid} → 等 ack → close
   父进程进入阶段 3 supervisor;**仅持有一个 idle vsock listener,无 inflight 数据连接**
```

**bind+listen 必须在 hello 之前完成**:host 在 `launch` 写完后立即起 ping
ticker,如果 listener 没起会落空。vsock 不依赖 IP 配置,phase 1 完成 pivot
后立即 bind+listen 是合理的。

**applyNetwork 不是 listener 的前置依赖**——applyNetwork 只对应用层网络服务
有意义;vsock 控制面与网络配置正交。

**fast-fail 网络**:在一次性沙箱模型下,网络配置失败后让应用悄悄跑反而是
反模式——上层调度器期望"沙箱起不来 = 重新调度",而不是"沙箱起来了但 IP 错
了"。

### 3.3 阶段 3:supervisor

```
loop:
  signal.Notify(sigchld, sigterm, sigint)
  select:
    sigchld:
      pid, status = waitpid(-1, WNOHANG)
      if pid == app_pid:
        switch sandbox.restart:
          never:        do app_exit_then_reboot(status)
          on-failure:   if status != 0: refork app else app_exit_then_reboot(status)
          always:       refork app
      else:
        # 孤儿被 reparent 到 PID 1 (sandbox-init),收割之
        // do nothing

    sigterm/sigint:
      send SIGTERM to app_pid
      wait up to 10s for app to exit (status assembled from waitpid result)
      do app_exit_then_reboot(status)

app_exit_then_reboot(status):
  short-conn dial host vsock:5000 → write app_exited{code} → wait ack(timeout) → close
  do reboot()                       # ack 拿不到也照常 reboot
```

`reboot()` 调 `unix.Reboot(LINUX_REBOOT_CMD_RESTART)` → kernel 触发 ACPI
shutdown → CH 看到 vCPU 关 → CH 进程退出。

**反向 listener 在独立 goroutine 中持续运行**,与 supervisor signal loop
并行;处理 host 下发的 `ping` / `restore` / `quiesce` 短连接。listener 整个
沙箱生命周期(冷启动 + snapshot/restore + 退出)持续存在,唯一退出点是进程
reboot。

**mem_report 上报 goroutine**:与 supervisor 并行的第二个常驻 goroutine,默认
每 5 s 读一次 `/proc/meminfo` 的 `MemAvailable:` 和 `MemTotal:`,短连接发
`mem_report` 给 host(协议见 §4.3)。host 端 BalloonController 据此把 balloon
target 锚定在合理水位(详见 sandbox.md §9.3);失败仅记 stderr,不影响 supervisor。

### 3.4 quiesce 处理

quiesce 是 host `/vm.pause` 之前的最后一次清理机会,目的是把跨实例 snapshot
的内存与磁盘状态推向"确定性",让分块去重率从 50-70% 升至 >90%
(PROPOSAL §4)。

listener 单线程顺序处理保证 host 看到的语义就是"quiesced 一回来即可继续
/vm.pause"。

**必做项(v1)**:

```
1. sync(2)                                       // ~ms,把 ext4 upperdir 全部 dirty 落地
2. open("/proc/sys/vm/drop_caches", O_WRONLY) → write("3\n")
                                                  // 同时丢 page cache + dentry/inode cache
3. WriteMessage(quiesced)                        // µs
4. close(conn)                                   // µs
```

**为什么必须做这两步**:

- **sync 在前**:`drop_caches` 只丢 clean,先 sync 把 dirty 转 clean,disk
  dump 与 memory dump 看到的是一致状态
- **drop_caches=3**:page cache 是确定性 snapshot 的核心污染源。同一应用不同
  启动序的 page cache 内容按访问顺序、prefetch 时序差异化堆积,跨实例
  ~90% 不同;drop 后再访问按需 demand-fault,每实例 restore 后 page cache 初值
  统一为空,直接对应 PROPOSAL §4 量化的"确定性 50→90% dedup"差距来源
- sandbox-init 是 PID 1 root,write 无权限障碍

**扩展项(v2,协议预留扩展位)**:

| 动作 | 说明 |
|---|---|
| 应用层 quiesce hook(信号通知 user app)| sandbox.yaml `quiesce.signal`(默认空=skip);收到 quiesce 时向 app_pid 发信号,等固定窗口(默认 100 ms)。应用自行决定如何响应:flush 内部缓存、drain transient sockets、关闭 outbound TCP。默认不发——SIGUSR1 等信号没注册 handler 时默认会终止应用,需用户显式声明才安全 |
| `/tmp` tmpfs 重置 | 阶段 1 加 `mount -t tmpfs tmpfs /tmp`;quiesce 时 `umount2(MNT_DETACH)` + remount。MNT_DETACH 处理 user app 持有 /tmp fd 的情况 |
| outbound 连接关闭 | 应用层 fd,sandbox-init 无主动关闭权,依赖 hook |

**不做项**:

- **不**关闭 vsock listener(后续 `restore` 依赖它)
- **不**调用 user app 终止——quiesce 不是 sigterm
- **不**重置 RNG / 熵池——熵池重新播种是 restore 路径的事
- **不**清理 /var/log 等运行时日志——应用职责

**错误处理**:

- sync / drop_caches 任一失败 → stderr 记录,继续后续步骤
- 任一步骤失败都**不**中止 quiesce:agent 尽力清理,质量不到位反映在
  dedup 率指标上,**不**阻塞 snapshot
- quiesced 若写不出去(连接已断)→ host 端 deadline 内拿不到响应,host 视为
  协议失败放弃此次 snapshot,sandbox 继续运行

**与 /vm.pause 的契约窗口**:quiesced 写出与 host 调 `/vm.pause` 之间存在
数十 µs 窗口,理论上 user app 在该窗口内可再分配 page cache 或写 dirty。量级
远低于启动期堆积,对 dedup 率影响可忽略。绝对消除该窗口可在 quiesce handler
末尾 `kill -STOP app_pid`、resume 时 `kill -CONT`,本设计不引入(工程收益
不抵复杂度)。

## 4. vsock 控制面协议

### 4.1 通道与寻址

vsock 端口固定 `5000`,**两个方向都复用同一端口号**,身份按方向区分:

```
                      ┌────────────────────────────┐                    ┌─────────────────────────┐
                      │ sandbox-ctl (host)         │                    │ sandbox-init (guest)    │
                      │                            │                    │                         │
  guest → host  ────► │ listen UDS                 │ ◄──── CH proxy ─── │ AF_VSOCK dial CID=2:5000│
                      │ /run/<sid>/vsock.sock_5000 │                    │                         │
                      │                            │                    │                         │
  host → guest  ────► │ dial UDS                   │ ───── CH proxy ──► │ AF_VSOCK listen :5000   │
                      │ /run/<sid>/vsock.sock      │ + "CONNECT 5000\n" │                         │
                      └────────────────────────────┘                    └─────────────────────────┘
```

- **guest → host**:guest 调 `connect(SockaddrVM{CID=2, Port=5000})`,host 在
  `<vsock-base>_5000` UDS 上 accept
- **host → guest**:host 调 `connect(<vsock-base>)`,**第一笔写入**为 ASCII
  `CONNECT 5000\n`(CH hybrid vsock 协议头),CH 把后续字节代理到 guest
  port 5000 listener
- 两个方向独立寻址,**互不干扰**——同一时刻 host→guest ping 与 guest→host
  app_started 可并行,各用一条新连接

### 4.2 wire format

`[4 字节 little-endian uint32 长度] [JSON payload]`

JSON 可读、调试友好;消息量极少,无需 protobuf 工具链。

### 4.3 消息目录

请求/响应严格 1:1,在同一短连接上完成,不混用:

| 类型 | 方向 | 配对响应 | 用途 |
|---|---|---|---|
| `hello` | guest → host | `launch` | 冷启动握手:guest 报告 phase=ready,host 回 LaunchSpec |
| `launch` | host → guest | (无,握手收尾) | 见上 |
| `app_started` | guest → host | `ack` | guest 已 fork/exec 用户进程,携带 guest pid |
| `app_exited` | guest → host | `ack` | guest 用户进程退出,携带退出码;guest 收到 ack 后再做 reboot |
| `ping` | host → guest | `pong` | 健康探测,host 计 RTT、超时、失败数 |
| `restore` | host → guest | `restored` | 快照恢复 vCPU 起跑后 host 主动通知 guest;guest 回 ack 即视为 ready |
| `quiesce` | host → guest | `quiesced` | 快照前要求 guest 完成清理动作并排空数据连接 |
| `mem_report` | guest → host | `mem_report_ack` | guest 周期(默认 5 s)上报 `/proc/meminfo` 的 MemAvailable/MemTotal,喂给 host 端 BalloonController(§sandbox §9.3) |
| `error` | 任意 | (终止) | 任一端拒绝/出错的兜底响应 |

字段集合(`Message` 结构体在 `pkg/sandbox/proto`):

```json
{
  "type":     "<one of above>",
  "phase":    "ready",                 // hello: optional hint
  "launch":   { ... LaunchSpec ... },  // launch
  "pid":      4711,                    // app_started
  "code":     0,                       // app_exited
  "id":       42,                      // ping/pong: 单调递增,host 分配
  "t_send_ns":1715000000000000000,     // ping: host 单调时钟 ns;guest 原样回填到 pong
  "epoch":    3,                       // restore: 第 N 次 restore;每次 +1
  "mem_avail_bytes": 4294967296,       // mem_report: /proc/meminfo MemAvailable (bytes)
  "mem_total_bytes": 8589934592,       // mem_report: /proc/meminfo MemTotal (bytes)
  "msg":      "<reason>"               // error: 人类可读理由
}
```

`MaxMessageBytes` = 64 KiB。

### 4.4 时序

冷启动主时序:

```
   sandbox-ctl                                       sandbox-init  (guest)
   ───────────                                       ─────────────────────
   listen <base>_5000        ◄── guest→host UDS ready
   spawn CH                                          kernel boot
                                                     phase 1: mount + pivot
                                                     AF_VSOCK bind+listen :5000
                             ┌── conn1: hello / launch ──┐
                        ◄────│ dial CID=2:5000           │
                             │ → "hello"                 │
                        ────►│ ← "launch" + LaunchSpec   │
                             │ close (both sides)        │
                             └───────────────────────────┘
   ping ticker (1 Hz) start  ◄── starts once "launch" write completes
                                                     applyNetwork
                                                     fork/exec user app
                             ┌── conn2: app_started ─────┐
                        ◄────│ dial CID=2:5000           │
                             │ → "app_started" pid=N     │
                        ────►│ ← "ack"                   │
                             │ close                     │
                             └───────────────────────────┘
                             ┌── conn3..k: ping / pong ──┐
                        ────►│ host→guest "ping" id=k    │
                             │ ← "pong" id=k             │
                             │ close                     │
                             └───────────────────────────┘
                             ...
                                                     user app exits
                             ┌── conn_last: app_exited ──┐
                        ◄────│ → "app_exited" code=C     │
                        ────►│ ← "ack"                   │
                             │ close                     │
                             └───────────────────────────┘
                                                     reboot()
```

快照前后:

```
   snapshot                                          restore
   ────────                                          ───────
   sandbox-ctl                 sandbox-init           sandbox-ctl                 sandbox-init
   ───────────                 ────────────           ───────────                 ────────────
   ping ticker stop                                   /vm.resume OK
   "quiesce" ─────────────►    drain data conns       "restore" epoch=N ──────►   (listener up; recv + handle)
                               run pre-snap cleanup            ◄── "restored" ─── ack ready
              ◄── "quiesced" ──(listener kept)        ping ticker (re)start
   /vm.pause                                                                      (listener unchanged)
   /vm.snapshot
```

**listener 跨快照不关闭**——若 quiesce 把 listener 关掉,host 之后下发的
`restore` 就无人 accept,guest agent 不可达。quiesce 仅要求 guest 排空数据
短连接(实际由短连接纪律保证窗口期内本就为空)并完成 pre-snap 清理动作;
listener fd 在 snapshot/restore 间保持 bind+listen idle 状态。

### 4.5 ping 健康探测

**目的**:用 host→guest 探针检测 guest agent 存活与响应延迟。**不**作为应用
层心跳,不主动 kill VM,只产指标。

**生命周期**(全部内置,不暴露成配置):

| 事件 | ping ticker 状态 |
|---|---|
| sandbox-ctl 完成 `launch` 写入 | start |
| sandbox-ctl 收到 `restored` 响应 | start |
| sandbox-ctl 完成 `quiesce` 写入 | stop |
| CH 进程退出 | stop |

**默认参数**(可由 sandbox.yaml `health.ping:` 覆盖,**仅 interval 与
timeout**):

| 参数 | 默认 | 含义 |
|---|---|---|
| `interval` | 1 s | 两次 ping 起始时刻间隔(非 RTT 后再 sleep) |
| `timeout` | 200 ms | 单次 dial+write+read 总预算;到点视为失败 |

**指标**(sandbox-ctl 暴露,统计窗口 = 沙箱生命周期):

- `ping_attempts_total` — 发起次数
- `ping_success_total` — 收到 pong 数
- `ping_timeout_total` — 在 timeout 内未收到 pong
- `ping_dial_error_total` — vsock dial 失败(CH 未起、CONNECT 行被拒、guest
  未 listen)
- `ping_rtt_ms_{p50,p95,p99,max}` — pong 到达时 `now - t_send_ns` 计算

### 4.6 短连接纪律

每次请求一条新 vsock 连接,完成响应即双向 close。不做长连接、不做多路复用、
不做 keepalive。原则:

1. **快照干净**:`/vm.pause` 时 guest 内若仍有任何 inflight AF_VSOCK *数据*
   连接,寄存在 vCPU/devices state 内的 socket 状态会进 snapshot,restore 时
   既无对端也无 host UDS 与之配对,只能强行清理。短连接 + ping ticker 在
   quiesce 处主动停摆,保证 quiesce 写入瞬间 guest 内只有一个 listener idle
   socket,无 inflight 数据连接
2. **listener 保留**:bind+listen 的 idle vsock socket 没有连接表也没有缓冲
   数据,跟随 snapshot 过去再 restore,recv 队列与 accept 队列都为空,语义干净;
   这是 quiesce 不关闭 listener 的前提
3. **错误隔离**:任一连接的协议错误只影响本次请求,不污染下一笔
4. **顺序不依赖**:`app_started` / `app_exited` / `ping` / `restore` 互相独立,
   guest 端 listener 单线程依次 accept-and-dispatch,避免请求交错

### 4.7 失败语义

**guest 端**(sandbox-init):

- listener accept 错误 → 记录 stderr 日志,不退出 init(避免单次连接异常杀
  整个沙箱)
- 收到未知 type 或字段不合规 → 回 `error{msg}` → close;ping 缺 id 直接拒绝
- `app_exited` 必须在 reboot 前发出;若 host 未在 timeout 内 ack,guest 仍
  照常 reboot——通知尽力而为

**host 端**(sandbox-ctl):

- 任意 host→guest 请求失败计入对应 `*_error_total`,不立刻 kill VM;由更
  上层 health checker(本文档不覆盖)按指标决策
- `restore` 在 deadline 内未收到 `restored` → restore 失败回退:sandbox-ctl
  调 CH `/vm.shutdown` 终结此次恢复并向 restore 调用方返回错误,**不**让
  guest agent 不可达的沙箱继续承载请求

| 消息 | 单次 deadline | 备注 |
|---|---|---|
| `hello/launch` | 复用 dial 重试预算 5 s | 冷启动早期 host listener 可能短暂未起,既有指数退避保留 |
| `app_started` | 200 ms | 健康路径 µs 级,deadline 仅作 host 协程泄漏兜底 |
| `app_exited` | 200 ms | ack 拿不到也照常 reboot,通知尽力而为 |
| `ping` | 200 ms | 1 s interval 下足够裕度;到点计入 `ping_timeout_total` |
| `quiesce` | 5 s | guest 要 drop caches / 清 /tmp / 关闭 outbound 连接,数十 ms 起步,留足头部 |
| `restore` | 5 s | kernel vsock 层在此期间 hold 住连接请求等 vCPU 跑起来 accept |
| `mem_report` | 200 ms | guest 每 5 s 一次,host 失败仅记日志、controller 在下一 tick 用旧 hint |

## 5. 应用契约

### 5.1 launch 配置(LaunchSpec)

来源:`boot.root.base` 末尾 ZIP 内嵌 `config.json`(OCI image runtime config)
⊕ sandbox.yaml `launch:` 节(yaml override 优先,Env merge),host sandbox-ctl
合并后通过 launch 协议下发。

字段:

```json
{
  "exec":    "/usr/bin/foo",
  "args":    ["arg1", "arg2"],
  "env":     {"PATH": "...", "HOME": "/root"},
  "workdir": "/",
  "restart": "never",                    // never | on-failure | always
  "network": {
    "interface": "eth0",
    "ip":        "169.254.1.1/31",
    "gateway":   "",
    "hostname":  "my-sandbox"
  }
}
```

### 5.2 用户应用看到的环境

- **PID 1**:用户应用本身(由于 CLONE_NEWPID,看自己 PID = 1)
- **mount namespace**:私有挂载 ns,但起始视图与 sandbox-init 相同
  (overlayfs 合并的 / + 自挂的 /proc)
- **网络**:eth0(virtio-net,host TAP 后端),IP 已由 sandbox-init 配好
- **/dev**:`devtmpfs`(/dev/null、/dev/random、/dev/urandom 等)
- **vsock**:无,guest 应用不应直接用 vsock(平台保留 CID=3 + port 5000)
- **balloon / mem hotplug**:透明,应用不可见

### 5.3 退出语义

- `restart: never`:应用退出 → sandbox-init 通知 host(`app_exited`)→ reboot
  → CH 退 → sandbox-ctl 退,sandbox 销毁。退出码透传到 sandbox-ctl
- `restart: on-failure`:exit code != 0 时 fork-restart;== 0 时同 never
- `restart: always`:不管 exit code,fork-restart

Restart 是同进程内 fork,**不**重新走整个 sandbox 启动序——快照恢复期被
restored 的 sandbox-init 仍在原 supervisor 循环内。

### 5.4 信号处理

- sandbox-ctl 通过 vsock 发 `quiesce`(snapshot 前)/ `restore`(restore 后)
  / `ping`,**不**直接给 user app 发信号
- 来自 host 的 SIGTERM 通过 cloud-hypervisor 传到 sandbox-init,
  sandbox-init 转发给 user app(给 10s 优雅退出窗口)
- `quiesce.signal`(扩展)发给 user app 做应用层清理

## 6. 扩展点

| 扩展 | 引入条件 | 影响章节 |
|---|---|---|
| 应用 quiesce hook | 跨实例去重率超过 PROPOSAL §4 量化的"非确定性 50-70%" 上限的用例 | §3.4 quiesce 扩展项表 |
| 自带 vmlinux | 用户需要 cgroup-in-guest / nested userfaultfd / 别的 kernel 特性 | sandbox-ctl `boot.kernel: file://...` |
| 自带 sandbox-runtime | 用户应用对 PID 1 / supervisor 有特殊要求(罕见) | 平台不阻止,但失去 DAX 共享收益 |

## 7. See Also

- [`sandbox.md`](sandbox.md) §launch / §快照 / §恢复 —— host 侧 sandbox-ctl
  对应行为;vsock UDS 起在 host 哪
- [`sandbox-kernel.md`](sandbox-kernel.md) —— guest kernel 启用的 namespace
  / 文件系统 / 网络功能为何如此
- [`cloud-hypervisor.md`](cloud-hypervisor.md) §vsock hybrid 代理 —— vsock
  在 host 侧映射到 UDS 的 CONNECT 行格式
- [`build.md`](build.md) —— `make sandbox-runtime` 构建流程
- `PROPOSAL.md` §4 "确定性 Guest 配置" —— quiesce 必做项的目标依据
