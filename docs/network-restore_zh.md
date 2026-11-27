[English](network-restore.md) | [简体中文](network-restore_zh.md)

# 快照恢复时的平台网络状态

内存恢复会在目标环境继续应用的本地状态，而目标网络身份或远端状态可能已经改变。Runtime 刷新
sandboxer 提供的单个 virtio-net 接口，包括过期邻居和符合条件的旧 TCP 连接。应用仍负责重连及刷新
外部状态。本页说明哪些工作自动完成、哪些仍由应用负责，以及如何排查这个边界。

## 推荐恢复流程

1. 用选定 Runtime/Kernel 准备冷启动实例，通过 sandboxer 或 orchestrator/connector 部署配置平台网络。
   如需应用刷新处理器，在捕获前按[恢复通知](using-guest-runtime_zh.md#application-restore)注册。
2. 捕获实例，并用目标 Host 网络绑定恢复。无需额外网络重置开关或 Guest 网络工具。不要为下述平台
   拥有的状态另加全局路由清理或接口重建。
3. 恢复后验证真实请求及应用的重连、租约逻辑。平台就绪不等待远端或应用重连。监听 socket 保持可用；
   被选中清理的活动连接可能失败，应用可据此建立新连接。
4. 请求失败时先检查 Guest 网络日志及当前地址、路由，再区分应用重试失败、内核不支持清理或用户
   管理的网络状态。Restore 成功本身不证明所有旧 TCP socket 已被释放。

平台网络恢复使用 Host/Guest 线上契约和内核 Netlink 接口，无需单独的 Host 配置开关或内核
补丁，也不遍历用户网络 namespace。以下给出精确所有权和有界行为。

## 行为

Outer init 在用户初始化之前绑定平台 NIC，即使启动时没有 IP 配置也如此。绑定保存在快照私有内存中，
包括原始 network namespace、接口索引、sysfs 对象身份，以及 init 成功安装的精确默认路由。同一个
接口对象改名后仍可识别；复用旧名称或索引的替代设备不会取得该所有权。如果平台设备已移动、消失或
加入用户 master 设备，网络操作停止且不修改设备，Restore 按既有尽力执行策略继续。

| 操作 | 配置 | 动态邻居 | 旧 TCP |
| --- | --- | --- | --- |
| 冷启动 | 应用所给值 | 不重置 | 不重置 |
| 真正 Restore，新 IP | 替换平台 IP/默认路由 | 清理平台 ARP/NDP | 尝试释放变更前选中的一批连接 |
| 真正 Restore，相同 IP | 应用所给值 | 清理平台 ARP/NDP | 尝试释放变更前选中的一批连接 |
| 真正 Restore，未给或空 IP | 保留 IP/默认路由；应用显式 hostname | 清理平台 ARP/NDP | 尝试释放变更前选中的一批连接 |
| Attach／同 VM 重连 | 保留 | 保留 | 保留 |

提供 IP 时，配置替换会移除平台 NIC 上 IPv4 和 IPv6 **两个地址族**的其他全局地址。已经匹配的地址
和前缀保留，以免删除最后一个 IPv4 地址时触发 Linux 删除依赖路由。IPv6 link-local 地址也保留。
只移除 init 记录的默认路由，不清空整个默认路由表。给出新 IP、gateway 为空时，移除旧的自有默认路由。
新默认路由以独占方式创建，仅在收到成功的内核 ACK 后记录。与无关路由冲突时报告配置错误，不静默
替换。无效 CIDR、gateway 地址族及 MTU 在写配置前拒绝。

平台默认路由使用显式非零 metric：IPv4 为 1，IPv6 为 1024。Linux 删除路由时把 metric 0 当作通配，
所以记录 0 不能构成精确删除键。这些 metric 保留普通单默认路由路径；用户路由优先级仍遵循 Linux 规则。

真实 IP/前缀变更仍会产生 Linux 对直连路由及依赖被删地址路由的正常影响。平台不在身份变更后重建用户路由。

邻居删除覆盖已绑定设备上的 IPv4、IPv6 动态条目，不包括 permanent、NOARP、proxy 条目、bridge FDB
或用户设备上的条目。枚举完整结束后才开始删除。

## TCP 选择与释放

修改地址或路由前，在 outer init 原始 network namespace 中枚举活动的完整 TCP socket。可选状态为
ESTABLISHED、SYN_SENT、SYN_RECV、FIN_WAIT1、FIN_WAIT2、CLOSE_WAIT、LAST_ACK、CLOSING；
排除 LISTEN 和 TIME_WAIT。Socket 必须具有有效 cookie、旧平台本地地址、可用路由元数据，且普通
unicast 路由的直接输出接口为绑定的平台 NIC。显式绑定其他 NIC 的 socket 不选中；未绑定设备也不
自动等于属于平台 NIC。

路由查询使用源/目标地址、端口、协议、UID、mark 及 traffic class。IPv4-mapped IPv6 socket 按 IPv4
查询路由，但释放时保留原 IPv6 身份。内核中 inode 为 0 的完整 socket 可以选中；缺失诊断元数据不按
零处理。Multipath、nexthop 对象及封装路由不属于当前简单平台路径。

范围包括应用直接通过平台 NIC 创建的连接。通过用户创建的虚拟 NIC 或其他 network namespace 的
连接仍由用户负责。实现不递归归因 tunnel 或任意报文重写；仅增加一个用户接口也不会关闭普通平台
连接的清理。

完成配置和邻居处理后，向固定的原始 socket 身份（**包括 cookie**）发送 SOCK_DESTROY，不进行
二次普查、不按相同 tuple 查找替代 socket，也不重试到清空。Cookie 已消失属于正常结果。TCP 的
枚举、路由查询和释放共用一个 50 ms 单调时钟期限；选取到释放之间的配置、邻居处理时间也消耗它。
不等待远端回复、应用 close、重连或解冻后清理。Netlink I/O 非阻塞且可取消。

## 失败与兼容

TCP 打开、枚举、分类、权限、能力不支持及释放错误分别记录。畸形或不完整普查不用于释放。单个
socket 释放失败不会导致无限重试；全局权限/能力错误及期限到达会停止剩余批次。
**即使成功释放的 TCP 数量为零，Restore 也可以成功。** 正常的 memory-report 切换、解冻、内层就绪
及 Restore ACK 继续执行；这些既有步骤自身失败仍会阻止就绪。管理生命周期仍遵守先前已有的 Shutdown。

配置和邻居错误保留 Restore 的尽力执行策略，并与 TCP 错误分开报告。整个网络操作有一秒期限；
50 ms 限制 TCP 的额外工作，不是给每个连接各分配一份预算。调度延迟可能增加观测到的墙钟时间，
配置期限不构成硬实时 SLA。

随项目提供的 x86_64、arm64 内核配置启用 `INET_DIAG`、`INET_TCP_DIAG`、`INET_DIAG_DESTROY`。
网络恢复无需额外 network namespace、veth、BPF、Netfilter 或用户态网络工具。不支持销毁的旧内核
仍继续 Restore，并记录 TCP 操作不支持。快照保留捕获时的 init 和内核；安装其他 Runtime 或 Kernel
不会改变已有快照。要采用其他 Runtime/Kernel 组合，需用该组合冷启动并重新捕获。

| 观测结果 | 解释与下一步 |
| --- | --- |
| 相同 IP 恢复仍失去旧连接 | 真正恢复也会清理选中的旧平台 TCP，即使 IP 未变。检查应用是否重试。 |
| 源实例 `snapshot --resume` 保留连接 | 同 VM attach 保留网络状态，不是真正恢复通知。 |
| Restore 成功但记录 TCP 权限、不支持或超时日志 | 清理为尽力执行。检查捕获的内核和应用重试路径，不从 ACK 推断清理完整完成。 |
| 用户 bridge、tunnel、namespace 或静态邻居仍在 | 不属于平台清理范围，由拥有它们的应用或网络组件维护。 |
| 安装新 Runtime 后旧快照行为没变 | 快照保留捕获的 init/内核。用所选组合冷启动并重新捕获。 |

## 验证

单元测试覆盖完整 Netlink dump/ACK 处理、序号/port 校验、截断及错误、有界期限、地址族替换、精确
默认路由所有权、邻居排除、路由和 socket 身份、mapped socket、已消失 cookie 及非致命释放结果。
Restore handler 测试注入不支持、权限、I/O、超时错误，并验证解冻、就绪及 Restore ACK。Attach
单独测试以排除网络重置。

真实网络测试必须在进程自己拥有的可丢弃 network namespace 中运行，并具备所需权限；必须执行这里
的 Go 实现，不能用另一个报文分类器代替。真实 VM 验证使用生产配置及新构建的 init/内核，用基线/功能
成对测试分别衡量 Guest 网络阶段及 Host 恢复就绪。报告真实样本数和分位数，历史原型耗时不能作为
生产结果。
