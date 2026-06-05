# sandbox-runtime

microVM 沙箱生命周期引擎，是 [kuasar-sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox)
的运行时核心：冷启动、快照、恢复，以及块设备（vhost-user-blk）与按需内存（uffd）的
host 侧编排。

## 组成

| 路径 | 角色 |
|---|---|
| `cmd/sandbox-ctl` | host 控制平面：`run` / `snapshot` / `exec` / `config` / `info`（恢复 = `run --restore`，无独立 `restore` 子命令） |
| `cmd/sandbox-init` | guest PID 1（打进 `sandbox-runtime.erofs`）；三阶段 init + vsock 控制面 |
| `pkg/sandbox` | 控制平面库：config/cgroup/ch/lifecycle/uffd/memory/snapshot/restore/mux/proto/tap |
| `pkg/vhost` | vhost-user-blk 后端（经 accelerator 的 `manifest/fetch` 取 chunk） |
| `pkg/resource` | **导出面**：节点资源控制协议（wire + `Client`）；`sandbox-sentinel` import 它实现控制器 |

## 跨仓依赖（薄）

| 依赖 | 用途 | 解析 |
|---|---|---|
| `sandbox-accelerator/pkg/manifest` (+`cache`/`store` client) | 快照 ingest/fetch、vhost 块读 | `replace => ../sandbox-accelerator` |
| `sandbox-builder/pkg/image` | 读取展平镜像内嵌的 RuntimeConfig | `replace => ../sandbox-builder` |
| `sandbox-vswitch/pkg/tapfd` | tapfd 交接消费侧（`RecvFdsWithNetns`） | `replace => ../sandbox-vswitch` |

以上均为 **纯 Go、无 CGO** 的导入面 —— `sandbox-runtime` 整体 `CGO_ENABLED=0` 构建，
不引入 rocksdb / eBPF 等重依赖（Go module-graph pruning 保证 vswitch 的 eBPF 依赖不进闭包）。

运行期还需 **sandbox-deps** 产出的原生件：`vmlinux`（guest 内核）、`cloud-hypervisor`（VMM）、
`mkfs.erofs`（打 guest 镜像）。

## 构建

```bash
make sandbox-ctl sandbox-init   # 两个纯 Go 二进制
make sandbox-runtime            # 把 sandbox-init 打成 sandbox-runtime.erofs（需 mkfs.erofs）
make vet test
```

## 私网 / 离线构建

`go.mod` 用 `replace` 指向兄弟仓相对路径，clone 全组织为兄弟目录后即可离线 `go build`；
第三方依赖走内网 `GOPROXY`。组织级 `go.work` 见 [kuasar-sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox)。

设计细节见 `docs/sandbox.md`（host 控制平面）、`docs/sandbox-runtime.md`（guest 运行时）。
