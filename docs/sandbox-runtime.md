# sandbox-runtime — guest 运行时

guest 内 PID 1 二进制 `sandbox-init` 与承载它的根文件系统镜像
`sandbox-runtime.erofs`。负责沙箱启动期 rootfs 组装、应用拉起、生命周期监督、
应用 stdio/console 转发、以及与 host sandbox-ctl 之间的控制面通信。

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
| 一 VM = 一 app | sandbox-init clone(NEWPID\|NEWNS) 让用户 app 看到自己 PID=1 |
| 生命周期可寻址 | exit / SIGTERM / quiesce / restore 都通过 vsock 通知 host |
| app I/O 干净 | 应用 stdin/stdout/stderr(或一个伪终端)走 vsock 转发,与内核 dmesg 隔离 |

### 1.2 系统中的位置

```
   HOST                                                      GUEST VM
   ────                                                      ────────

   sandbox-ctl ── spawn CH ──►  cloud-hypervisor             kernel boot
                                       │                          │
                                       │ virtio-pmem / DAX        │  mount root, exec /sbin/init
   sandbox-runtime.erofs  ◄────────────┤  (host shared file)      │  = sandbox-init
                                       │                          │    phase 1: mount + overlayfs + chroot
        kernel dmesg  ◄── --console ───┤ ◄── hvc0 (virtio-con) ── │    phase 2: vsock launch handshake
        (host captures to stderr/file) │                          │              + app stdio wiring
                                       │                          │    phase 3: supervisor loop
                                       │                          │
                                       ├──── conn: launch ───────►│    fork/exec user app
                                       │     (conn → MUX) ◄══════►│      app stdin/out/err ↔ MUX
                                       ├──── conn: ping ─────────►│      app sees itself PID 1
                                       │◄─── conn: app_started ───┤
                                       │                          │
   /run/<sid>/vsock.sock  ◄────────────┤◄─── conn: app_exited ────┤    user app exits
                                       │                          │    reboot()
                                       │◄─── CH exits ────────────┤
```

### 1.3 不做的事

- **无容器运行时**:平台直接管理 sandbox 生命周期,不引入 runc / crun / podman
- **无 systemd / OpenRC**:进程监督由 sandbox-init 自己写的 supervisor 完成
- **无 busybox / util-linux**:所有功能(mount / mkdir / chdir / chroot /
  reboot / openpty)通过 Go syscall 完成,镜像只装一个 sandbox-init
- **无 /etc / /usr**:guest rootfs 由用户镜像(blk0 base)提供;sandbox-runtime
  只提供 /sbin/init 和挂载点
- **不复用控制面短连接做 stdio**:管理操作各用一条短连接(§4.3);只有 launch /
  restore / attach 这三种操作的连接在握手后升级为长连接 MUX(§4.5)

## 2. sandbox-runtime.erofs 镜像结构

```
/sbin/init                sandbox-init 静态 Go 二进制,~10-15 MiB
/proc/                    空挂载点
/sys/                     空挂载点
/dev/                     空挂载点
/mnt/lower/               空挂载点(blk0 EROFS 挂入点)
/mnt/upper/               空挂载点(blk1 ext4 挂入点)
/mnt/newroot/             空挂载点(overlayfs 合并目标 + chroot 目标)
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
3. mount -t devtmpfs  devtmpfs   /dev   (kernel CONFIG_DEVTMPFS_MOUNT=y 时 EBUSY,跳过)
4. wait for /dev/vda 和 /dev/vdb 出现(轮询 stat,timeout 10s)
5. mount -t erofs -o ro /dev/vda /mnt/lower
6. mount -t ext4         /dev/vdb /mnt/upper
7. mkdir /mnt/upper/upperdir /mnt/upper/workdir 若不存在
8. mount -t overlay overlay -o lowerdir=/mnt/lower,
                                 upperdir=/mnt/upper/upperdir,
                                 workdir=/mnt/upper/workdir  /mnt/newroot
9. MS_MOVE /proc /sys /dev 到 newroot 下;chdir(newroot) → MS_MOVE . / → chroot(.) → 进入阶段 2
10. AF_VSOCK bind+listen :5000   ← 反向通道(host→guest)早于阶段 2 的 hello 开门
```

全部通过 `golang.org/x/sys/unix.Mount` / `unix.Chroot` 等 syscall 完成,
不依赖任何外部二进制。

`/dev/vda`(blk0 base)是用户应用的镜像 erofs(只读);`/dev/vdb`(blk1 overlay
ext4)是写层。overlay 合并后 `/mnt/newroot` 是 guest rootfs 的最终视图,
`chroot` 之后这套视图变成新的 `/`。承载 sandbox-init 自身的 `sandbox-runtime.erofs`
由内核经 virtio-pmem 挂在 `/`(`root=/dev/pmem0 ... dax=always`),阶段 1 把它
让位给 overlay。

**bind+listen 必须在阶段 2 的 hello 之前完成**:host 在 `launch` 写完后立即起
ping ticker,如果 listener 没起会落空。vsock 不依赖 IP 配置,阶段 1 完成 chroot
后立即 bind+listen 是合理的。

### 3.2 阶段 2:launch 握手 + stdio 接线 + 应用拉起

阶段 2 在**同一条 vsock 连接**上完成 launch 握手,握手收尾后该连接**不关闭**——
它升级成 MUX,承载应用的 stdin/stdout/stderr(或一个伪终端)直到沙箱结束(协议
见 §4.5)。

```
1. socket(AF_VSOCK, SOCK_STREAM) → connect(CID=2 host, port=5000)
   重试:第一次立即发起,失败后 100µs 起指数退避(×2,上限 10ms),deadline 5s

2. write  hello{phase:"ready"}
3. read   launch{spec}                  ← LaunchSpec: exec/args/env/workdir/restart,
                                           network, stdio{tty? / which channels}
4. 若 spec.Network 非空,applyNetwork:
     - Sethostname(network.hostname)
     - raw netlink RTM_NEWLINK(interface UP) / RTM_NEWADDR(IP/CIDR) / RTM_NEWROUTE(gateway)
   失败 fast-fail —— 一次性沙箱模型下"网络配置失败"必须立刻 die
5. 按 spec.stdio 准备应用的 stdio fd(§3.5):
     - tty 模式:openpty(); 记下 master fd 与 slave fd; 初始 winsize 来自 spec
     - pipe 模式:为每个声明的通道(stdin/stdout/stderr)建一对 pipe / socketpair
       未声明 stdin → 应用的 fd 0 接 /dev/null
6. write  launch_ack{stdio: 实际启用的 channel 集合}
                                         ← 此后这条连接进入 MUX 帧收发态,不再关闭
7. read   ack                            ← host 确认进入 MUX 态

8. 通过 exec.Cmd 拉起子进程:
     SysProcAttr.Cloneflags = CLONE_NEWPID | CLONE_NEWNS
     tty 模式: Setctty + setsid + slave 作为 fd 0/1/2;关闭 master 副本于子进程
     pipe 模式: 各 pipe/socketpair 的 child 端作为 fd 0/1/2
     argv: [/proc/self/exe, "exec-child", workdir, exec, args...]
     env:  spec.Env(默认补 PATH)

9. 子进程在新 ns 内:mount -t proc proc /proc → chdir(workdir) → syscall.Exec(exec, args...)

10. 父进程(sandbox-init pid=1):
     - 启动 stdio 桥接 goroutine:app 端 fd ↔ MUX 流(§3.5)
     - 短连接 dial host:5000 发 app_started{pid} → 等 ack → close
     - 进入阶段 3 supervisor
```

**applyNetwork 不是 listener 的前置依赖**——applyNetwork 只对应用层网络服务
有意义;vsock 控制面与网络配置正交。

**fast-fail 网络**:一次性沙箱模型下,网络配置失败后让应用悄悄跑反而是反模式——
上层调度器期望"沙箱起不来 = 重新调度",而不是"沙箱起来了但 IP 错了"。

### 3.3 阶段 3:supervisor

```
loop:
  signal.Notify(sigchld, sigterm, sigint)
  select:
    sigchld:
      pid, status = waitpid(-1, WNOHANG)
      if pid == app_pid:
        switch sandbox.restart:        // v1: 三种策略都 = 通知 + reboot;in-place refork 是 v2
          *: app_exit_then_reboot(status)
      else:                            // 孤儿被 reparent 到 PID 1,收割之
        // do nothing
    sigterm/sigint:
      send SIGTERM to app_pid
      wait up to 10s for app to exit (status assembled from waitpid result)
      app_exit_then_reboot(status)

app_exit_then_reboot(status):
  收尾 MUX:应用 fd 已关 → 各 stdout/stderr/pty 流发 EOF → 等 host 排空(有界,带超时)
  short-conn dial host:5000 → write app_exited{code, term_signal} → wait ack(timeout) → close
  reboot(LINUX_REBOOT_CMD_POWER_OFF)   # ack 拿不到也照常 reboot;POWER_OFF → CH 干净退 0
```

`reboot(POWER_OFF)`(而非 `RESTART`):一次性沙箱模型下应用退出即沙箱结束,
CH 应随之干净退出。`RESTART` 会触发 CH 的"原地重启"流程,试图重连 vhost-user-blk
后端——而后端只接受一次连接(sandbox = 单 VM 生命周期),重连失败 CH 非零退出。

**反向 listener 在独立 goroutine 中持续运行**,与 supervisor signal loop 并行;
处理 host 下发的 `ping` / `restore` / `quiesce` / `attach` 短连接(§4.3、§4.4)。
listener 整个沙箱生命周期(冷启动 + snapshot/restore + MUX 重连 + 退出)持续存在,
唯一退出点是进程 reboot。

**mem_report 上报 goroutine**:与 supervisor 并行的第二个常驻 goroutine,默认
每 5 s 读一次 `/proc/meminfo` 的 `MemAvailable:` 和 `MemTotal:`,短连接发
`mem_report` 给 host(协议见 §4.4)。host 端 BalloonController 据此把 balloon
target 锚定在合理水位(详见 [`sandbox.md`](sandbox.md) §9.3);失败仅记 stderr。

### 3.4 quiesce 处理

quiesce 是 host `/vm.pause` 之前的最后一次清理机会,目标两件事:

1. 把跨实例 snapshot 的内存与磁盘状态推向"确定性",让分块去重率从 50-70% 升至
   >90%(PROPOSAL §4)。
2. **让 MUX 连接在快照前彻底关闭**——快照绝不能捕获一条半开/握手中途的 MUX 连接
   (restore 出来后无对端,成为悬挂状态;§4.6)。

`attach`/`quiesce` 等短连接由 listener 单线程顺序处理;host 看到的语义是
"`quiesced` 一回来即可继续 `/vm.pause`"。

**quiesce 流程**:

```
1. [prep] sync(2)                                  // ~ms,把 ext4 upperdir 全部 dirty 落地
2. [prep] open("/proc/sys/vm/drop_caches", O_WRONLY) → write("3\n")
                                                    // 同时丢 page cache + dentry/inode cache
3. 停止读应用的 stdout/stderr pipe(或 pty master) // 应用很快在下一次 write 阻塞,残留有界
   (停读是 MUX 关闭的前置动作,不提前做,缩短应用阻塞窗口)
4. 在 MUX 连接上发起优雅关闭握手(§4.6):MUX_CLOSE → 收 MUX_CLOSE_ACK → close(MUX);
   连接已断则降级硬丢
5. WriteMessage(quiesced) 于 quiesce 短连接
6. close(quiesce 短连接)
```

**为什么 prep 必须做这两步**:

- **sync 在前**:`drop_caches` 只丢 clean,先 sync 把 dirty 转 clean,disk dump
  与 memory dump 看到的是一致状态
- **drop_caches=3**:page cache 是确定性 snapshot 的核心污染源;同一应用不同启动
  序的 page cache 内容按访问顺序、prefetch 时序差异化堆积,跨实例 ~90% 不同;drop
  后每实例 restore 后 page cache 初值统一为空,直接对应 PROPOSAL §4 量化的"确定性
  50→90% dedup"差距来源。sandbox-init 是 PID 1 root,write 无权限障碍

**扩展项(v2,协议预留扩展位)**:

| 动作 | 说明 |
|---|---|
| 应用层 quiesce hook(信号通知 user app)| sandbox.yaml `quiesce.signal`(默认空=skip);收到 quiesce 时向 app_pid 发信号,等固定窗口(默认 100 ms)。默认不发——SIGUSR1 等信号没注册 handler 时默认终止应用,需用户显式声明才安全 |
| `/tmp` tmpfs 重置 | 阶段 1 加 `mount -t tmpfs tmpfs /tmp`;quiesce 时 `umount2(MNT_DETACH)` + remount |
| outbound 连接关闭 | 应用层 fd,sandbox-init 无主动关闭权,依赖 hook |

**不做项**:

- **不**关闭 vsock listener(后续 `restore`/`attach` 依赖它)
- **不**调用 user app 终止——quiesce 不是 sigterm;应用是被"停读其输出"自然反压
  而阻塞,不是被信号
- **不**重置 RNG / 熵池——熵池重新播种是 restore 路径的事
- **不**清理 /var/log 等运行时日志——应用职责

**错误处理**:

- prep 的 sync / drop_caches 任一失败 → stderr 记录,继续后续步骤(best-effort,
  质量不到位反映在 dedup 率指标上,**不**阻塞快照)
- **MUX_CLOSE 握手与 `quiesced` 不是 best-effort**:`quiesced` 写出意味着"MUX
  已关、应用已阻塞、guest 处于干净态"。若 MUX_CLOSE 握手因连接已断而走不通 →
  按硬丢处理(对端也看到了断链),仍可发 `quiesced`;若 `quiesced` 写不出去(host
  侧不可达)→ host 在 deadline 内拿不到响应 → host 视为协议失败、放弃此次 snapshot,
  sandbox 继续运行(详见 §4.9 与 [`sandbox.md`](sandbox.md) §6.2)

### 3.5 应用 stdio / console 接线

应用看到的 stdin/stdout/stderr(或一个伪终端)由 sandbox-init 在 guest 内创建,
经 MUX 流(§4.5)与 host 双向桥接。**不**让应用继承 sandbox-init 的 fd(那是
`/dev/hvc0` = 内核 dmesg 控制台),应用输出因此不被内核刷屏污染,host 侧也不混入
内核日志。

**两种模式,互斥**(由 launch 协议的 `stdio` 字段决定,见 §4.4 / §5.1):

| 模式 | guest 内 | 应用看到 | MUX 流 |
|---|---|---|---|
| **tty** | `openpty()`;子进程 `setsid()` + `TIOCSCTTY(slave)` + slave→fd 0/1/2;初始 `TIOCSWINSZ` 来自 spec | 一个真伪终端:`isatty()`=true、有 job control、收 SIGWINCH | 1 条:pty 流(双向,master ↔ host)+ control 流(`SET_WINSIZE` 等) |
| **pipe** | 为声明的通道各建一对 pipe/socketpair;child 端→fd 0/1/2;未声明 stdin → fd 0 接 /dev/null | 普通管道:`isatty()`=false | 按声明:stdin(host→guest)/ stdout / stderr(guest→host)+ control 流 |

sandbox-init 持有 pty master 端 / 各 pipe 的 sandbox-init 端,起桥接 goroutine
在它与 MUX 流之间双向拷贝;收到 control 流上的 `SET_WINSIZE` → 对 pty master 做
`TIOCSWINSZ`(内核自动给应用进程组发 SIGWINCH)。

**反压**:当 host 不消费某条流(终端被 Ctrl-S、`--stdout-to` 的盘满、或 MUX 暂时
不存在),该流的接收窗口耗尽 → sandbox-init 那侧停止排空 → 内核 pipe / pty 缓冲
填满 → 应用在 `write` 上阻塞。不丢字节、不做环形 buffer(详见 §4.5)。

**内核 dmesg**:走 `/dev/hvc0`(virtio-console),与应用 stdio 是完全独立的一条道;
host 侧由 CH 把它写到 sandbox-ctl 给 CH 的 stdout(一根匿名管道),sandbox-ctl
按 `--console` 标志决定丢弃 / 写 stderr / 写文件(详见 [`sandbox.md`](sandbox.md)
§2.2 / §5.2)。`--serial off`——没有 8250 UART。

## 4. vsock 控制面 + console MUX 协议

### 4.1 两类连接

```
  (1) management short-conn  — one new conn per management op; request/response; close
        │  ops:  hello/launch · app_started · app_exited · ping · mem_report ·
        │        quiesce · restore · attach                          (§4.3 / §4.4)
        │  wire: [4B LE len][JSON]    ·    no multiplexing    ·    no keepalive

  (2) MUX long-conn          — at most one; carries the app's stdin/stdout/stderr (or a pty)
        │  born from the launch / restore / attach conn, which stays open after
        │  the *_ack and switches to framed mode                     (§4.5 / §4.6)
        │  wire: [stream:u8][type:u8][len:u16 BE][payload]   ·   per-stream flow control
```

管理连接不做帧复用——每条连接就一次请求 + 一次响应。MUX 是唯一带帧多路复用与
流控的连接,只承载应用的 stdin/stdout/stderr 或伪终端,**不**替代任何管理操作:
即使 MUX 开着,`quiesce` / `app_exited` 等仍各起各的短连接、并行发生。

### 4.2 通道与寻址

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

- **guest → host**:guest `connect(SockaddrVM{CID=2, Port=5000})`,host 在
  `<vsock-base>_5000` UDS 上 accept。承载:`hello/launch` 握手(其连接升级 MUX)、
  `app_started` / `app_exited` / `mem_report`
- **host → guest**:host `connect(<vsock-base>)`,**第一笔写入**为 ASCII
  `CONNECT 5000\n`(CH hybrid vsock 协议头;CH 回一行 `OK <port>\n`,host 须先排空
  再读后续 payload),CH 把其余字节代理到 guest port 5000 listener。承载:`ping` /
  `quiesce` / `restore`(其连接升级 MUX)/ `attach`(其连接升级 MUX)
- 两个方向独立寻址,互不干扰——同一时刻 host→guest `ping` 与 guest→host
  `app_started` 可并行,各用一条新连接

### 4.3 管理操作集

请求/响应严格 1:1,在同一条短连接上完成;"ACK 后" 一栏标明该连接是关闭还是升级为 MUX:

| 操作 | 拨号 | 序列 | ACK 后 | 用途 |
|---|---|---|---|---|
| **冷启动** | guest→host | `hello` → `launch{spec}` → `launch_ack{stdio}` → `ack` | **升级 MUX** | guest 报 ready,host 回 LaunchSpec;guest 准备好 app stdio 后回 launch_ack,该连接成为 MUX |
| **应用启动通知** | guest→host | `app_started{pid}` → `ack` | 关 | guest 已 fork/exec 用户进程 |
| **应用退出通知** | guest→host | `app_exited{code, term_signal}` → `ack` | 关 | 用户进程退出;guest 收 ack 后再 reboot;host 用作自身退出码 |
| **健康探测** | host→guest | `ping{id, t_send_ns}` → `pong{id, t_send_ns}` | 关 | host 计 RTT / 超时 / 失败数(§4.8) |
| **mem 报告** | guest→host | `mem_report{mem_avail, mem_total}` → `mem_report_ack` | 关 | guest 周期上报 `/proc/meminfo`,喂 host BalloonController |
| **快照前** | host→guest | `quiesce` → `quiesced` | 关 | guest 跑 prep + 关闭 MUX(§3.4),`quiesced` ⇒ 可安全 `/vm.pause` |
| **恢复后** | host→guest | `restore{epoch, wallclock_ns}` → `restore_ack{stdio, app_state}` | **升级 MUX** | 快照恢复 vCPU 起跑后 host 通知 guest;`restore` 携带 host 发送前一刻的墙钟 `wallclock_ns`,guest 收到后先 `clock_settime` 把 `CLOCK_REALTIME` 跳到该值(CH 把快照里的旧钟原样载回,不纠正则落后整个静置区间;单调钟不受影响),再回 `restore_ack`——它是 ATTACH_ACK 的超集(含 channel 集合 + 应用状态)外加"恢复完成"信号(host 据此判定 restore 完成);该连接成为新 MUX |
| **MUX 重连** | host→guest | `attach{epoch}` → `attach_ack{stdio, app_state}` | **升级 MUX** | 可靠性兜底:MUX 因 vsock 异常断了,host 拨新连接重建;guest 收到先把旧 MUX 优雅关闭(已断则硬丢)再回 ack,该连接成为新 MUX(§4.6) |
| `error` | 任意 | (终止) | 关 | 任一端拒绝/出错的兜底响应,`msg` 人类可读 |

`ATTACH` 仅用于 sandbox-ctl 自身的可靠性兜底(同一进程在 MUX 连接坏掉后重建转发),
**不**用于另一个进程接管会话。

### 4.4 管理消息 wire format 与字段

`[4 字节 little-endian uint32 长度] [JSON payload]`,`MaxMessageBytes` = 64 KiB。
JSON 可读、调试友好;消息量极少,无需 protobuf 工具链。

```json
{
  "type":     "<one of §4.3>",
  "phase":    "ready",                 // hello: optional hint
  "launch":   { ... LaunchSpec ... },  // launch (含 stdio 节,见 §5.1)
  "stdio":    { ... },                 // launch_ack / restore_ack / attach_ack: 实际启用的 channel 集合
  "app_state":"running",               // restore_ack / attach_ack: running | exited{code,term_signal}
  "pid":      4711,                    // app_started
  "code":     0, "term_signal": 0,     // app_exited
  "id":       42,                      // ping/pong: 单调递增,host 分配
  "t_send_ns":1715000000000000000,     // ping: host 单调时钟 ns;guest 原样回填到 pong
  "epoch":    3,                       // restore / attach: 第 N 次;每次 +1,用于去重 in-flight
  "wallclock_ns":1715000000000000000,  // restore: host 墙钟,guest 落 CLOCK_REALTIME(attach 不带)
  "mem_avail_bytes": 4294967296,       // mem_report
  "mem_total_bytes": 8589934592,       // mem_report
  "msg":      "<reason>"               // error
}
```

字段集合的权威定义在 `pkg/sandbox/proto`(stdlib-only,guest sandbox-init 直接 import)。

### 4.5 MUX 子协议

**升级**:`launch` / `restore` / `attach` 三种操作,在双方交换完 `*_ack` 之后,
这条 vsock 连接**不关闭**——后续字节进入 MUX 帧收发态。任一时刻系统内至多一条
MUX 连接(冷启动时由 launch 那条而生;快照恢复后由 restore 那条而生;MUX 因故断了
由 attach 那条重建)。

**帧格式**:

```
 0       1       2               4                       4+len
 ┌───────┬───────┬───────────────┬───────────────────────┐
 │stream │ type  │  len  (u16 BE)│   payload (len bytes)  │
 │ (u8)  │ (u8)  │               │                       │
 └───────┴───────┴───────────────┴───────────────────────┘

 stream:  0 = CONTROL   1 = STDIN   2 = STDOUT   3 = STDERR   4 = PTY
 type (数据流 1..4):  DATA   EOF   RESET
 type (CONTROL 0):    WINDOW_UPDATE   SET_WINSIZE   MUX_CLOSE   MUX_CLOSE_ACK
 len:  payload 长度;DATA 帧 ≤ 16–32 KiB(多路之间公平,也是天然读块大小)
```

**stream 集合 = pty 模式 XOR pipe 模式**(在 `launch_ack` / `restore_ack` /
`attach_ack` 的 `stdio` 字段里声明):

- **pty 模式**:CONTROL(0)+ PTY(4)。终端没有独立 stderr——应用的 stdout+stderr
  都到这个伪终端,合并在 PTY 流上
- **pipe 模式**:CONTROL(0)+ 按声明的 STDIN(1)/STDOUT(2)/STDERR(3)。未声明的
  stream 上出现任何帧 = 协议违规(见下文状态机)

**方向约定**(违反即协议违规):

| stream | host→guest | guest→host |
|---|---|---|
| STDIN(1) | `DATA`(应用输入)、`EOF`(host stdin 关) | `RESET`(拒收) |
| STDOUT(2) / STDERR(3) | `RESET`(拒收) | `DATA`、`EOF`(应用退出/关闭其 fd) |
| PTY(4) | `DATA`(键盘字节,逐字节透传) | `DATA`、`EOF`(应用退出) |
| CONTROL(0) | `WINDOW_UPDATE`(对 guest→host 流补信用)、`SET_WINSIZE`、`MUX_CLOSE_ACK` | `WINDOW_UPDATE`(对 host→guest 流补信用)、`MUX_CLOSE` |

**流控**:每条数据流一个**接收窗口**(= 该方向接收 buffer 容量,例如 64 KiB)。
发送方维护每流剩余信用,DATA 每发 N 字节扣 N;信用为 0 的流跳过、去发别的流。
接收方排空一部分就发 `WINDOW_UPDATE{stream, delta}` 补回。**无连接级窗口**(流就
那么几条、量也小);DATA 帧大小上限保证 muxer 在多条流之间轮转公平。这样一条流的
sink 卡住只卡它自己,CONTROL 帧(`SET_WINSIZE` 等)永远能流。

**detached = 应用阻塞**:MUX 连接不存在的那段时间(quiesce 窗口、或连接刚断尚未
attach),没有对端给信用 → sandbox-init 那侧停止排空 → 内核 pipe / pty 缓冲填满 →
应用在 `write` 阻塞。**不丢字节、不做环形 buffer、不做"丢了 N 字节"标记** —— detached
只发生在受控短窗口,阻塞是可接受的、也是最简单自洽的。残留字节(per-stream buffer
里没冲完的 + 内核 pipe 里的)有上限(buffer 容量 + 一个 pipe 大小),随内存快照
被捕获,restore 后 attach 时排空、应用解除阻塞。

**per-stream 状态机**:

- 未在协商集合里的 stream 上收到任何帧 = **协议违规** → 拆掉整条 MUX 连接(这是
  bug,要响)
- 某方向发过 `EOF` 后,在该方向再发 `DATA` = 协议违规 → 拆连接
- 收到 `RESET` → 该流标 CLOSED,本地 fd 关 / 给应用 SIGPIPE
- 对端不要你 offer 的某条流(资源级,非协议违规)→ 不开它 / 回 `RESET` 该流,连接继续
- `EOF` 是单向半关:STDIN 上 host→guest 的 `EOF` 关应用 stdin 的写端;STDOUT/STDERR
  / PTY 上 guest→host 的 `EOF` 表示应用关了对应 fd / 退出

**SET_WINSIZE**:host 侧 SIGWINCH 时 host 在 CONTROL 流上发 `SET_WINSIZE{cols,rows}`
→ guest 对 pty master 做 `TIOCSWINSZ` → 内核给应用进程组发 SIGWINCH。仅 pty 模式有意义。
进入 MUX 态(含 restore/attach 后)host 先发一次初始 winsize。

### 4.6 MUX 优雅关闭握手

MUX 连接的**有序关闭**是一个两端同步的小协议(相当于应用层的 FIN / FIN-ACK),
之后接标准 socket orderly close。**永远 guest 发起、host 响应、guest 收到响应才
真正 close;发起到收响应之间到达的帧照常处理。**

```
  guest (sandbox-init)                                     host (sandbox-ctl)
    │
    │ ── MUX_CLOSE (CONTROL frame) ────────────────────►    (guest sends no more data frames after this)
    │ ◄── may still receive WINDOW_UPDATE / leftover STDIN DATA   (guest processes these normally)
    │ ◄── MUX_CLOSE_ACK (CONTROL frame) ──────────────      host: flush pending → ACK → no more MUX frames
    │     on ACK → close(MUX)                                host: read → EOF → close(MUX)
    ▼
    back to:  listener up  ·  app session alive (app blocked on write)  ·  no MUX
```

host 对 MUX 关闭的全部职责:收到 `MUX_CLOSE` → 把要发的发完 → 回 `MUX_CLOSE_ACK`
→ read 到 EOF → close。不需要知道为什么关、什么时候关。

**两个触发点**(同一握手):

1. **quiesce 流程**(§3.4 第 4 步)——guest 在 quiesce 流程靠后一步关闭 MUX。
2. **ATTACH 流程**——host 拨新连接发 `attach`,guest 在回 `attach_ack` 之前先把
   **旧 MUX 连接**优雅关闭。兜底:旧 MUX 大概率正因 vsock 断了才触发重连,这时对它
   发 `MUX_CLOSE` 直接报错 → guest **降级为硬丢弃**旧连接,不阻塞 attach。统一逻辑:
   收到 `attach` → 尝试优雅关旧 MUX、失败即硬丢 → 回 `attach_ack` 于新连接 → per-stream
   window 重新协商、winsize 重发、恢复读 app pipe、残留回放 → 续传。

**MUX 因 vsock 异常突然断**(非 quiesce、非 attach 主动关):两端各自看到错误,无优雅
握手;guest 回到"等 host 来 attach 重建"的状态(listener 一直在),host 检测到后拨
新连接发 `attach`。

### 4.7 时序

每段图里 `─►` 是普通管理短连接的请求/响应,`══►` 是 MUX 帧流动;一条连接做完
管理握手后转为 MUX 用 "(this conn ⇒ MUX)" 标注。

**冷启动**:

```
  sandbox-ctl                                              sandbox-init (guest)
  ───────────                                              ────────────────────
  listen <base>_5000
  spawn CH ─────────────────────────────────────────────►  kernel boot → mount + chroot
                                                           AF_VSOCK bind+listen :5000
                       ◄── hello{ready} ──────────────────  dial CID=2:5000      [conn A]
  ── launch{spec, stdio} ───────────────────────────────►  applyNetwork; openpty / pipes
                       ◄── launch_ack{stdio} ─────────────  prepare app stdio fds
  ── ack ───────────────────────────────────────────────►  (conn A ⇒ MUX)
  send initial SET_WINSIZE  ════════════════════════════►  (tty mode);  fork/exec user app
  ping ticker (1 Hz) start                                 app fd 0/1/2 = pty slave / pipes; MUX bridge up
                       ◄── app_started{pid} ── [conn B] ──  ;  reply ack; close conn B
  ── ping ──► ◄── pong ──  [conn C..k, repeats]            ║  MUX: STDIN/STDOUT/STDERR (or PTY) + WINDOW_UPDATE flow
                       ◄── app_exited{code,sig} [conn L] ─  user app exits → MUX flushed (EOF)
  ── ack ───────────────────────────────────────────────►  reboot(POWER_OFF) → CH exits 0
```

**MUX 重连(ATTACH)**:

```
  sandbox-ctl                                              sandbox-init
  ───────────                                              ────────────
  MUX read/write error                                     MUX read/write error  (old conn dead on both ends)
  dial CID=2:5000 ── attach{epoch} ─────────────────────►  try graceful MUX_CLOSE on old conn (err → hard-drop)
                       ◄── attach_ack{stdio, app_state} ──  reply on this new conn
  send SET_WINSIZE  ════════════════════════════════════►  (this conn ⇒ MUX);  resume reading app pipes; replay residual
  resume normal MUX flow                                   resume normal MUX flow
```

**quiesce → snapshot**:

```
  sandbox-ctl                                              sandbox-init
  ───────────                                              ────────────
  ping ticker stop
  dial CID=2:5000 ── quiesce ───────────────────────────►  prep: sync ; echo 3 > drop_caches
                                                           stop reading app stdout/stderr (pty master)
                       ◄══ MUX: MUX_CLOSE ════════════════  on the (separate) MUX conn: send MUX_CLOSE
  ══ MUX: MUX_CLOSE_ACK ════════════════════════════════►  recv ACK → close(MUX);  host: read → EOF → close(MUX)
                       ◄── quiesced ─────────────────────  reply on the quiesce conn; close it
  ✓ MUX closed + quiesced received  →  /vm.pause  /vm.snapshot
  snapshot state:  listener up · app session alive (app blocked) · no MUX · no active mgmt conn
```

**restore**:

```
  sandbox-ctl                                              sandbox-init
  ───────────                                              ────────────
  /vm.resume OK
  dial CID=2:5000 ── restore{epoch} ────────────────────►  sees "session already exists"
                       ◄── restore_ack{stdio, app_state} ─  reply on this conn
  send SET_WINSIZE  ════════════════════════════════════►  (this conn ⇒ MUX);  resume reading app pipes; replay residual
  ping ticker (re)start                                    (listener unchanged across the snapshot)
```

**listener 跨快照不关闭**——若 quiesce 把 listener 关掉,host 之后下发的 `restore`
/ `attach` 就无人 accept,guest agent 不可达。listener fd 在 snapshot/restore 间保持
bind+listen idle 状态(idle vsock socket 没有连接表也没有缓冲数据,跟随快照过去再
restore 语义干净)。

### 4.8 ping 健康探测

**目的**:用 host→guest 探针检测 guest agent 存活与响应延迟。**不**作为应用层
心跳,不主动 kill VM,只产指标。

| 事件 | ping ticker 状态 |
|---|---|
| sandbox-ctl 完成 `launch` 写入 | start |
| sandbox-ctl 收到 `restore_ack` 响应 | start |
| sandbox-ctl 完成 `quiesce` 写入 | stop |
| CH 进程退出 | stop |

**默认参数**(可由 sandbox.yaml `health.ping:` 覆盖,**仅 interval 与 timeout**):

| 参数 | 默认 | 含义 |
|---|---|---|
| `interval` | 1 s | 两次 ping 起始时刻间隔 |
| `timeout` | 200 ms | 单次 dial+write+read 总预算;到点视为失败 |

**指标**(sandbox-ctl 暴露,统计窗口 = 沙箱生命周期):`ping_attempts_total` /
`ping_success_total` / `ping_timeout_total` / `ping_dial_error_total` /
`ping_rtt_ms_{p50,p95,p99,max}`(pong 到达时 `now - t_send_ns` 计算)。

### 4.9 失败语义

**guest 端**(sandbox-init):

- listener accept 错误 → 记录 stderr,不退出 init(避免单次连接异常杀整个沙箱)
- 收到未知 type / 字段不合规 → 回 `error{msg}` → close;ping 缺 id 直接拒绝
- MUX 上协议违规(协商外 stream、EOF 后又 DATA、坏帧)→ 拆 MUX 连接;此后等 host
  来 `attach` 重建
- `app_exited` 必须在 reboot 前发出;host 未在 timeout 内 ack → guest 仍照常 reboot

**host 端**(sandbox-ctl):

- 任意 host→guest 短连接失败计入对应 `*_error_total`,不立刻 kill VM;由更上层
  health checker(本文档不覆盖)按指标决策
- `restore` 在 deadline 内未收到 `restore_ack` → restore 失败回退:sandbox-ctl 调
  CH `/vm.shutdown` 终结此次恢复并向调用方返回错误
- MUX 连接读/写出错 → host 拨新连接发 `attach` 重建;`attach` 也失败 → 计入指标,
  应用 stdio 转发中断(应用因反压阻塞),由上层决策
- `quiesce` 在 deadline 内未收到 `quiesced` → host 视为协议失败,**放弃此次 snapshot**
  (绝不带半状态/半开 MUX 快照),sandbox 继续运行

| 消息 | 单次 deadline | 备注 |
|---|---|---|
| `hello/launch/launch_ack/ack` | 复用 dial 重试预算 5 s | 冷启动早期 host listener 可能短暂未起,既有指数退避保留;此连接随后转 MUX,deadline 只覆盖握手段 |
| `app_started` | 200 ms | 健康路径 µs 级,deadline 仅作 host 协程泄漏兜底 |
| `app_exited` | 200 ms | ack 拿不到也照常 reboot |
| `ping` | 200 ms | 1 s interval 下足够裕度;到点计入 `ping_timeout_total` |
| `quiesce` | 8 s | guest 要 drop caches + 停读 app pipe + MUX_CLOSE 一来回;留足头部 |
| `restore` / `attach` | 5 s | kernel vsock 层在此期间 hold 住连接请求等 vCPU 跑起来 accept;此连接随后转 MUX |
| `mem_report` | 200 ms | guest 每 5 s 一次,host 失败仅记日志、controller 在下一 tick 用旧 hint |

## 5. 应用契约

### 5.1 launch 配置(LaunchSpec)

来源:`boot.root.base` 末尾 ZIP 内嵌 `config.json`(OCI image runtime config)⊕
sandbox.yaml `launch:` 节(yaml override 优先,Env merge),host sandbox-ctl 合并后
通过 launch 协议下发。`stdio` 节由 host 侧 `sandbox-ctl run` 的 `--tty` / `--stdin`
/ `--stdout` / `--stderr`(及它们的 `-from`/`-to`)解析决定(详见
[`sandbox.md`](sandbox.md) §2.2)。

```json
{
  "exec":    "/usr/bin/foo",
  "args":    ["arg1", "arg2"],
  "env":     {"PATH": "...", "HOME": "/root"},
  "workdir": "/",
  "restart": "never",                    // never | on-failure | always
  "network": { "interface": "eth0", "ip": "169.254.1.1/31", "gateway": "", "hostname": "my-sandbox" },
  "stdio":   {
    "tty":     true,                     // true: 给应用一个伪终端(pty 模式);false: pipe 模式
    "winsize": {"cols": 80, "rows": 24}, // tty 模式的初始窗口大小
    "stdin":   false,                    // pipe 模式:是否开 stdin 通道(否则 app fd 0 = /dev/null)
    "stdout":  true,                     // pipe 模式:是否开 stdout 通道
    "stderr":  true                      // pipe 模式:是否开 stderr 通道
  }
}
```

### 5.2 用户应用看到的环境

- **PID 1**:用户应用本身(CLONE_NEWPID,看自己 PID = 1)
- **mount namespace**:私有挂载 ns,起始视图与 sandbox-init 相同(overlayfs 合并的
  / + 自挂的 /proc)
- **网络**:eth0(virtio-net,host TAP 后端),IP 已由 sandbox-init 配好
- **/dev**:`devtmpfs`(/dev/null、/dev/random、/dev/urandom 等)
- **stdin/stdout/stderr**:
  - tty 模式:fd 0/1/2 是同一个伪终端的从端,`isatty()`=true,有控制终端与 job
    control,窗口变化收 SIGWINCH;stdout 与 stderr 在该终端上合并
  - pipe 模式:fd 0/1/2 是普通管道(`isatty()`=false);未声明 stdin 时 fd 0 = /dev/null
  - **不**是 `/dev/console` / `/dev/hvc0`——应用输出不混入内核 dmesg,反之亦然
- **vsock**:无,guest 应用不应直接用 vsock(平台保留 CID=3 + port 5000)
- **balloon / mem hotplug**:透明,应用不可见

### 5.3 退出语义

- `restart: never`:应用退出 → sandbox-init 收尾 MUX、发 `app_exited{code,term_signal}`
  → reboot → CH 退 → sandbox-ctl 退,sandbox 销毁。退出码(或致死信号)透传到
  sandbox-ctl 的进程退出码
- `restart: on-failure` / `always`:v1 三种策略行为相同(通知 + reboot);同进程内
  in-place refork 是 v2

Restart(v2)是同进程内 fork,**不**重新走整个 sandbox 启动序;快照恢复期被
restore 的 sandbox-init 仍在原 supervisor 循环内。

### 5.4 信号处理

- sandbox-ctl 通过 vsock 发 `quiesce`(snapshot 前)/ `restore` / `attach` / `ping`,
  **不**直接给 user app 发信号
- 来自 host 的 SIGTERM 通过 cloud-hypervisor 传到 sandbox-init,sandbox-init 转发给
  user app(给 10 s 优雅退出窗口)
- tty 模式下,host 终端在 raw 态时键盘 `^C`(0x03)作为字节经 MUX PTY 流送到 guest
  伪终端,由 guest 的行规程转成 SIGINT 发给应用——这是想要的;杀沙箱另走 SIGTERM
  或转义序列(详见 [`sandbox.md`](sandbox.md) §2.2)
- `quiesce.signal`(v2 扩展)发给 user app 做应用层清理

## 6. 扩展点

| 扩展 | 引入条件 | 影响章节 |
|---|---|---|
| 应用 quiesce hook | 跨实例去重率超过 PROPOSAL §4 量化的"非确定性 50-70%" 上限的用例 | §3.4 quiesce 扩展项表 |
| in-place app restart | `restart: on-failure/always` 真正同进程 refork(不 reboot) | §3.3 / §5.3 |
| 应用 stderr 旁路 | 需要 host 侧 stdout 与 stderr 分流(终端模式天然无此区分,pipe 模式可加一条 vsock 旁路) | §3.5 / §4.5 |
| 自带 vmlinux | 用户需要 cgroup-in-guest / nested userfaultfd / 别的 kernel 特性 | sandbox-ctl `boot.kernel: file://...` |
| 自带 sandbox-runtime | 用户应用对 PID 1 / supervisor 有特殊要求(罕见) | 平台不阻止,但失去 DAX 共享收益 |

## 7. See Also

- [`sandbox.md`](sandbox.md) §2.2(`run` 的 `--tty` / `--console` / stdio 标志)、
  §5.2(CH 冷启动命令行)、§6.2 / §6.3(snapshot 时序 / ctl.sock 协议)、§7(恢复)
- [`sandbox-kernel.md`](sandbox-kernel.md) —— guest kernel 启用的 namespace /
  文件系统 / virtio-console / 网络功能为何如此
- [`cloud-hypervisor.md`](cloud-hypervisor.md) §vsock hybrid 代理 —— vsock 在 host
  侧映射到 UDS 的 CONNECT 行格式;`--console` / `--serial` 的用法
- [`build.md`](build.md) —— `make sandbox-runtime` 构建流程
- `PROPOSAL.md` §4 "确定性 Guest 配置" —— quiesce prep 必做项的目标依据
