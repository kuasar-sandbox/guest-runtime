[English](using-guest-runtime.md) | [简体中文](using-guest-runtime_zh.md)

# 使用选定的 Guest Runtime 与 Kernel

取得并验证匹配聚合发行版，或明确选定兼容 Runtime/Kernel/sandboxer 组合后，使用本流程检查安装产物、制备应用镜像、选择直接启动或 systemd，并准备应用的捕获与恢复。完整平台安装、应用模板及集群调度见[项目快速开始](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/quickstart_zh.md)。

推荐顺序是：验证发布制品、展平应用、用内建空白可写 upper 冷启动、验证真实业务操作，再捕获可复用状态。
修改 Runtime、Kernel 或启动策略后，需要重新冷启动并捕获。

## 选择两个独立发行单元

本仓不发布通用的 `guest-runtime-vX.Y.Z` Release,而是维护两条独立版本线:

- **`runtime-vX.Y.Z`** - 发布 `sandbox-runtime-<arch>-vX.Y.Z.tar.gz`,包含 Runtime bundle、静态 `sandbox-init` 与 `flatten-ctl` 及其许可和源码材料;
- **`vmlinux-vX.Y.Z`** - 发布 `vmlinux-<arch>-vX.Y.Z.tar.gz`,在稳定路径 `bin/vmlinux` 包含 guest kernel，并在 `bin/vmlinux.sha256` 携带原始内核文件的 SHA-256。

两者版本号可以独立演进。项目聚合 Release 精确选择一个 Runtime Tag 和一个 VMLinux Tag,不假定版本号相同。

GitHub 组件资产在组件 build/package 检查后从所选 source ref 和精确 commit 发布，架构集合由各个所选 Release 声明。这里 `<arch>` 为该版本实际发布的 `x86_64` 或 `aarch64`。项目聚合 Release 随后选择精确 Runtime、VMLinux 和其他组件 Tag,并对该组合执行跨组件集成测试及已发布资产的 MicroVM 验证。Source Makefile 支持其他 `TARGET_ARCH` 不等于已为该架构发布预构建 artifact。

## 检查已安装产物

前置条件：Linux、Bash、`file`、`sha256sum`、已验证并解包的 Runtime/Kernel 归档；启动 VM 时还需匹配 sandboxer。`PREFIX` 为部署根目录，不能把不同架构覆盖到同一 `bin/`。新 Kernel 包包含身份 sidecar；没有 sidecar 的历史包按所选发行版规定验证。

```bash
set -euo pipefail
PREFIX=/path/to/validated-release
test -s "$PREFIX/bin/sandbox-runtime.bundle"
test -x "$PREFIX/bin/sandbox-init"
test -x "$PREFIX/bin/flatten-ctl"
test -s "$PREFIX/bin/vmlinux"
file "$PREFIX/bin/sandbox-init" "$PREFIX/bin/flatten-ctl" "$PREFIX/bin/vmlinux"
(cd "$PREFIX/bin" && sha256sum --check vmlinux.sha256)
"$PREFIX/bin/sandbox-init" --version
"$PREFIX/bin/flatten-ctl" --version
```

预期显示选定目标架构，并得到 `vmlinux: OK`。这验证已安装原始内核身份，不是签名、Guest 启动或快照兼容证明。Runtime bundle 的身份/布局和所选 sandbox-init ABI 仍须分别满足，见 [Runtime bundle](runtime-bundle_zh.md)与 [Kernel](vmlinux_zh.md)。

## 制备并检查应用镜像

选定不可变 registry digest，或使用 [flatten-ctl](flatten-cli_zh.md) 中的 Docker archive/目录流程。Registry 导出为保留层属主，需要 root 或 CAP_CHOWN。使用新的私有目录和足够 scratch 空间；生产凭据按文档放入受保护的配置/环境，不放入命令历史。

```bash
WORK=$(mktemp -d)
IMAGE=registry.example.com/team/app@sha256:REPLACE_WITH_COMPLETE_DIGEST
"$PREFIX/bin/flatten-ctl" export --output "$WORK/app.img" "$IMAGE"
"$PREFIX/bin/flatten-ctl" info --json "$WORK/app.img"
```

把 registry 占位符换成可访问的完整 digest；需要鉴权、TLS 或缓存时传入选定 `--config`。成功应产生完整工件，并能读取镜像/运行配置报告。`.img` 是 tarstream 载体，不应直接作为裸 EROFS mount。导出失败时依据 stderr 和文档的自有资源清理规则诊断后再复用。镜像仍被引用时保留 `WORK`；消费者释放后，只清理这个私有目录。

## 将产物绑定给 sandboxer

把已安装内核以 `file://` ref 配入 `boot.kernel`，Runtime bundle 配入 `boot.runtime`，应用工件配入 `boot.root.base`。使用所选 sandboxer 的完整配置和启动流程，其生命周期 API 负责网络、资源与恢复准入。Runtime 是共享只读 PMEM 镜像，应用镜像提供用户 rootfs；YAML 被接受本身不能证明 kernel/VMM/snapshot 组合兼容。

下例为能在无 NIC 环境运行的应用绑定三个冷启动工件，并通过 sandboxer 内建初始化器创建空的 1 GiB
ext4 upper。宿主须满足所选 sandboxer 的 Linux/KVM/VMM 要求，且 sandbox-ctl 能找到
`cloud-hypervisor`。此最小 shell 示例使用不含空格的绝对路径。这里不配置网络；需要时按
sandboxer/connector 流程添加。

```bash
cat > "$WORK/sandbox.yaml" <<EOF
resources:
  capacity: {cpu: 2, memory: 2GiB}
  allocatable: {cpu: 2, memory: 2GiB}
boot:
  kernel: file://$PREFIX/bin/vmlinux
  runtime: file://$PREFIX/bin/sandbox-runtime.bundle
  root:
    base: file://$WORK/app.img
    overlay:
      diff_template: mkfs://ext4?size=1GiB
EOF
"$PREFIX/bin/sandbox-ctl" config --config "$WORK/sandbox.yaml" --check strict
"$PREFIX/bin/sandbox-ctl" run --config "$WORK/sandbox.yaml" \
  --sandbox-id guest-example --tty=false
```

先通过严格配置验证，再由前台 run 报告启动结果或具体错误。省略 `launch.exec` 时，overlay 模式采用
镜像 Entrypoint/Cmd，无需另填 startup resource override。保持工作负载运行，在第二个终端用镜像中
确实存在的命令检查，例如：

```bash
"$PREFIX/bin/sandbox-ctl" exec --sandbox-id guest-example -- /bin/sh -c 'id; pwd'
```

对于新建普通 upper，内建初始化器替代 Host 手工格式化。其确定性格式不包含 ext4 journal；工作负载
需要其他文件系统特性时，使用外部准备的模板，并将选定容量和格式作为工作负载定义的一部分，见
[文件系统源与磁盘容量](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/configuration_zh.md#disk-capacity)。
同一个可写设备不能同时使用该初始化器和不可变 COW base；上例独立的只读 EROFS 根盘可以与它配合。

Runtime/Kernel 身份错误、无效文件系统源、权限不足或 VMM 不兼容都须在复用前解决。结束这个非 TTY
示例时，在持有 `run` 的终端按 Ctrl+C 并等待退出。捕获、独立恢复、磁盘保留与清理按
[sandboxer 使用指南](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/runtime_zh.md#standalone-workflow)操作。
消费者仍在引用时，保留应用、Runtime 和 Kernel 文件。

<a id="application-startup"></a>

## 选择应用启动方式

普通应用入口保持默认的 direct 模式即可。Runtime 已提供 Guest PID 1、挂载、初始化和进程监督，
应用无需自行复制 sandbox-init 或编写平台服务。

应用需要 systemd unit 时，选择 `launch.systemd: runtime`，使用 Runtime Bundle 内匹配的私有安装：

```yaml
launch:
  systemd: runtime
  # overlay 模式省略 exec/args，即继续使用镜像 Entrypoint/Cmd。
```

启动前将这个片段合入冷启动配置。应用 unit 放在应用镜像内，或在制备阶段安装。Runtime 自动创建
保留的 `sandbox-init.service`、executor 连接和启动屏障；不要另写替代平台 unit，也不要通过将
`launch.exec` 设为 `/sbin/init` 来启用 systemd。通过 `sysinit.target` 排序的标准服务会等待
executor 附着并发出 READY。这不等待应用健康，也不代表所有用户服务均已启动。

| 策略 | 适用场景 |
| --- | --- |
| `off` | 直接运行应用；未继承源策略时为默认值。 |
| `runtime` | 应用需要 systemd，并使用 Runtime 的私有 manager、loader、依赖库和工具。 |
| `provided` | 应用镜像有意提供合格的 systemd 安装。 |
| `auto` | 优先使用镜像中的合格安装，仅在不存在候选时回退到 Runtime；已有不兼容候选会使启动失败。 |

当前资格检查支持 amd64、arm64 的动态链接 glibc manager，不接受静态或 musl manager。这是
manager 的资格要求，不要求所有工作负载二进制都使用 glibc。完整选择、cgroup、PATH 和关闭规则见
[Guest ABI](guest-abi_zh.md#systemd-environments)。Runtime 模式下，可从 Host 检查 manager 和应用 unit：

```bash
"$PREFIX/bin/sandbox-ctl" exec --sandbox-id guest-example -- \
  /run/sandbox-runtime/systemd/bin/systemctl is-active sandbox-init.service
"$PREFIX/bin/sandbox-ctl" exec --sandbox-id guest-example -- \
  /run/sandbox-runtime/systemd/bin/systemctl status YOUR_APPLICATION.service
```

将应用 unit 名换为镜像中安装的名称。平台 READY 不是应用就绪检查，仍需执行代表性业务请求。
平台 service READY 先于 Launch Init，因此 Init 可以调用 `systemctl`。停止环境时使用 Runtime owner
的关闭流程；直接给 systemd 发送 SIGTERM 请求的是重新执行，不是正常关机。

<a id="application-restore"></a>

## 准备应用的捕获与恢复

先区分新的冷启动与继续已保存内存：

| 操作 | 应用行为 |
| --- | --- |
| `run --config` 或对 E 执行 `run --from` | 应用生效的 files/mounts 和 Init，再启动主进程与 plugin。 |
| 对 S 执行 `run --restore` | 继续捕获的进程、manager、namespace 和内存；不重跑 Init，不替换 launch 策略。 |
| 对源实例执行 `snapshot --resume` | 捕获后继续同一个 VM，不触发真正恢复通知。 |

捕获已经冻结用户环境并同步受管根盘/数据盘 ext4 文件系统。Guest 全局 `sync`、手工清缓存循环和
外部 `fsfreeze` 脚本不是平台捕获屏障的前提。应用仍负责与外部数据库、租约及远程服务的一致性；
[quiesce 契约](guest-abi_zh.md#quiesce)没有增加通用应用快照前钩子。受管 `syncfs` 错误会使捕获
失败，并在该代 Guest 中持续记录；后续一次 `sync` 成功不会清除它。

### 异步应用通知

已经实现信号处理器的应用或 plugin，可以在捕获前订阅：

```yaml
launch:
  restore_signal: SIGUSR1
```

这个片段为已有主进程定义增加订阅。捕获前应安装并验证处理器；未处理的 SIGUSR1 可能终止进程。
每个 plugin 都有独立可选的 `restore_signal`。仅真正内存恢复会向仍存活且已订阅的主 PID 发通知。
信号是异步的，不等待处理器完成刷新；应用负责重试、幂等以及重连后的就绪状态。

### 同步 systemd 恢复钩子

systemd 环境下，在捕获前安装组件自己负责的 oneshot unit。例如在应用镜像中创建
`/etc/systemd/system/application-after-restore.service`：

```ini
[Unit]
Description=Refresh application state after sandbox restore
PartOf=sandbox-restore.target
Before=sandbox-restore.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/libexec/application-after-restore
TimeoutStartSec=5s
[Install]
RequiredBy=sandbox-restore.target
```

由应用提供该可执行文件，并返回实际执行结果。制备运行环境时、捕获之前启用 unit：

```bash
"$PREFIX/bin/sandbox-ctl" exec --sandbox-id guest-example -- \
  /run/sandbox-runtime/systemd/bin/systemctl enable application-after-restore.service
```

Runtime 提供 `sandbox-restore.target`，在每次真正恢复时重新启动；该 target 不接入冷启动。
`RequiredBy` 配合上述排序会使 helper 失败传递给 target，并阻止成功的恢复 ACK。改为 `WantedBy`
时仍等待，但不传播 helper 失败。整个 job 必须在既有 10 秒私有 RPC 预算内完成，包含排队时间；
这里的 5 秒只是示例 unit 超时。Host `timeouts.restore` 可以施加更短上限。钩子执行时用户环境已解冻、
主 Host MUX 仍分离，因此不能依赖本轮 Host exec/MUX 才能完成。完整语义见
[恢复通知](guest-abi_zh.md#restore-notifications)。

### 网络状态与验证

真正恢复时，Runtime 应用当前平台 NIC 配置、清理其动态 ARP/NDP 条目，并有界尝试释放选中的旧 TCP
连接。普通源实例续跑或 attach 不重置这些网络状态。通过 Host 网络 provider 配置目标，不要为这些
工作另加 Guest 全局路由清理、重建 NIC 或 Host 轮询循环。应用仍须重连并刷新外部状态；用户创建的
NIC、其他 network namespace 和归属不明确的路由连接不在平台清理范围内，见[网络恢复](network-restore_zh.md)。

发布可复用模板前，验证代表性业务请求、写入并读取持久文件、捕获并恢复到独立实例，再次核对请求、
文件及预期通知。也要继续源实例，确认那里没有触发真正恢复钩子。平台 ACK 成功不证明外部服务已就绪。
已有快照保留捕获时的 Runtime 和 Kernel；采用新功能需要重新捕获。

## 保留、升级与恢复选定产物

### 节点升级

节点可同时保留多份 runtime 镜像,例如:

```
/opt/sandbox/runtime/v0.1.0/sandbox-runtime.bundle
/opt/sandbox/runtime/v0.2.0/sandbox-runtime.bundle
```

新沙箱使用新版本;运行中沙箱继续持有启动时的 pmem 文件。删除旧版本前必须
确认没有运行中 VM 或待恢复 snapshot 依赖它。

ABI 兼容并不意味着新 runtime 可恢复旧内存快照。这里 S 指内存快照工件，E 指
它引用的冷沙箱工件，后者记录 runtime 身份。恢复会把 runtime 的
**digest scheme 与 Bundle footer 声明的 digest** 与 E 比较，而不比较 basename；
规则见 [sandboxer 恢复选择](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/runtime_zh.md)。

例如 S1 使用 `/runtime/v1/sandbox-runtime.bundle` 捕获，即使 v2 wire ABI
兼容，也不意味着能使用 `/runtime/v2/sandbox-runtime.bundle`。如果内容身份不同，
应显式把 `boot.runtime` 绑定到匹配 S1 所引用 E 的保留文件。匹配的 v1 文件可以
使用新的名称或路径；给不同内容的 runtime 改名并不会使其兼容。上面的分版本
目录支持显式绑定，恢复不会搜索兄弟版本目录。

若需要自动回退，应在捕获 v1 时使用 basename `sandbox-runtime-v1.bundle`，
把它与 `sandbox-runtime-v2.bundle` 不可变地保留在同一目录，并让默认配置指向 v2。
恢复先检查配置的 v2 文件；若它不存在、不可读、格式错误或身份不匹配，则且仅再尝试
`<default-directory>/sandbox-runtime-v1.bundle`。原 basename 仅用于这个回退候选
定位：使用第一个匹配的候选，不扫描目录。若两个候选均未通过，格式、I/O 和权限
错误不会被报告为身份不兼容。无效引用配置与取消不会触发回退。

只要运行中 VM、保留的模板、快照或父链仍依赖旧 runtime，就保留其不可变字节，
包括备份和回滚候选。运行中 VM 引用的路径应保持稳定；依赖自动回退时保留捕获时
的 basename，否则显式绑定匹配的保留文件。内存恢复不读取外部 kernel 文件或其
身份 sidecar，但仍要求兼容的架构、VMM 和设备输入。应保留冷启动及回滚需要的
kernel 文件和 sidecar。退役前由部署方盘点引用，并在合格目标节点验证恢复；
只保留最新 ABI 兼容 runtime 不足以保障恢复。

若要从冷 E 工件采用新的 kernel/runtime 内容，应使用目标配置冷启动。
Sandboxer 基于目标身份派生新的 C0，同时保留源的有效磁盘图。这不是恢复 S
保存的内存；内存恢复仍保留 S 原始的 E/C0 身份。

### 整机重启

独立的 `sandbox-ctl` 不提供调度器，也不在 Host 重启后自动重建 VM。保留 Runtime、冷启动输入及
所需可移植快照。Orchestrator 管理的节点按平台持久节点和恢复流程处理；其所有权与放置决策独立于
本组件的制品要求。本地内存 checkpoint holder 不能替代已发布、可恢复的制品。

### 回滚

准备回滚时先暂停新放置；这能控制影响，但不等于已选回旧 runtime。对于 orchestrator
管理的节点，把 conductor 配置的 `sandbox.boot.runtime` 指向保留的兼容文件，
重启 conductor 服务（`node-ctl conductor serve`），验证一次新的冷启动后再恢复准入。
独立使用 `sandbox-ctl` 时，在下次启动的配置中选择 `boot.runtime`。保留既有 runner
单元及其引用文件：修改默认值不会迁移运行中的 VM，也不覆盖上文的快照身份规则。

## 排错

| 现象 | 检查项 |
|---|---|
| guest 无法启动 `/sbin/init` | 确认 runtime 镜像 arch 与 vmlinux/CH 目标 arch 一致 |
| systemd 模式在应用启动前失败 | 检查 `launch.systemd`、所选 manager 资格、保留路径冲突及必要 cgroup controller；`auto` 不掩盖镜像 manager 不兼容。 |
| Restore 成功但外部请求失败 | 检查应用重连、租约逻辑及网络日志。TCP 清理有界且为尽力执行，不保证外部会话可用。 |
| 恢复钩子未运行 | 确认是真正内存恢复、捕获了订阅或启用的 unit，并核对捕获的 Runtime 版本；源实例续跑和冷启动不通知。 |
| 捕获报告受管 `syncfs` 错误 | 检查存储及应用 I/O 错误；错误在该代 Guest 中持续保留。先诊断原因，再准备新的冷启动实例。 |
| build sandbox 找不到 `flatten-ctl` | 检查 `/opt/sandbox-runtime/bin/flatten-ctl` 是否进入镜像 |
| build sandbox 无法生成 EROFS | 检查 guest `flatten-ctl`、源读取权限和规划预算 |
| host 无法打包交叉架构镜像 | 检查 `BUILD_MKFS_EROFS` 是否为宿主架构工具 |
| restore 后行为异常 | 检查 snapshot 使用的 runtime digest 与 restore 配置是否匹配 |
| 解包后脚本找不到 runtime | 确认已解压 runtime archive,且 `bin/sandbox-runtime.bundle` 存在 |

## 后续入口

- [Runtime 交付契约](runtime-bundle_zh.md)
- [内核能力与验证](vmlinux_zh.md)
- [完整 flatten CLI](flatten-cli_zh.md)
- [网络恢复及其限制](network-restore_zh.md)
- [构建与发行复现](build_zh.md)
