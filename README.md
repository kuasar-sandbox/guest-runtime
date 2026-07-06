# guest-runtime

Guest 运行时镜像与构建工具仓:负责构建 `sandbox-runtime.erofs`、`flatten-ctl`
以及 guest 侧 native 产物。host 生命周期、快照、恢复和 `sandbox-init` 源码属于
`sandboxer`;内容寻址、manifest/cache/store 与展平公共库属于 `accelerator`。

## 组成

| 路径 | 角色 |
|---|---|
| `cmd/flatten-ctl` | OCI/目录 → EROFS deterministic image builder;复用 `accelerator/pkg/{flatten,image,remote,tar}` |
| `docs/sandbox-runtime.md` | runtime 镜像布局、guest payload、构建和发布契约 |
| `docs/vmlinux.md` | guest kernel 镜像契约和配置理由 |
| `docs/flatten.md` | `flatten-ctl` CLI、远程拉取缓存、OCI Referrers 行为 |
| `native-deps/` | 构建 `vmlinux`、`mkfs.erofs`、`fsck.erofs`、`envd` |
| `scripts/guest-inspect.py` | 检查 guest/runtime 镜像的辅助脚本 |

`guest-runtime` 把 `../sandboxer/bin/<arch>/sandbox-init` 和 guest payload 打进
同一份 DAX-shared EROFS 镜像。镜像内 `/opt/sandbox-runtime/bin/` 首版包含
`envd`、`flatten-ctl`、`mkfs.erofs`;`vmlinux` 和 `cloud-hypervisor` 不进入
runtime 镜像,分别由 `guest-runtime` 专用包和 `sandboxer` 发布。

## 构建

```bash
make native-deps                 # vmlinux / erofs tools / envd
make flatten-ctl                 # OCI/dir -> deterministic EROFS builder
make build                       # sandbox-runtime.erofs with envd/flatten-ctl/mkfs.erofs
make sandbox-runtime             # same image target, builds ../sandboxer sandbox-init if needed
make build TARGET_ARCH=aarch64
```

`make sandbox-runtime` 需要 `mkfs.erofs`;查找顺序为 `PATH`、`bin/<arch>/`、
`native-deps/bin/<arch>/`。它还会消费 `native-deps/bin/<arch>/envd` 和本仓
`bin/<arch>/flatten-ctl`;缺失时按需触发对应构建目标。

## 产物

| 产物 | 生成入口 |
|---|---|
| `bin/<arch>/flatten-ctl` | `make flatten-ctl` |
| `bin/<arch>/sandbox-runtime.erofs` | `make sandbox-runtime` |
| `native-deps/bin/<arch>/vmlinux` | `make native-deps` |
| `native-deps/bin/<arch>/mkfs.erofs` / `fsck.erofs` | `make native-deps` |
| `native-deps/bin/<arch>/envd` | `make native-deps` |

跨仓发布由 `orchestrator/release-builder` 编排:

- `guest-runtime-<version>-linux-<arch>.tar.gz`:包含 `flatten-ctl`、erofs tools、
  `envd`、本仓文档和 flatten e2e。
- `sandbox-runtime-<arch>-<version>.tar.gz`:runtime 镜像专用包,包含版本化
  `.bundle` 文件和 `bin/sandbox-runtime.erofs` 兼容别名。
- `vmlinux-<arch>-<version>.tar.gz`:guest kernel 专用包,包含版本化 kernel 文件
  和 `bin/vmlinux` 兼容别名。

## 文档

- [docs/sandbox-runtime.md](docs/sandbox-runtime.md) — runtime 镜像打包、发布和消费契约。
- [docs/vmlinux.md](docs/vmlinux.md) — guest kernel 配置、构建和平台 ABI。
- [docs/flatten.md](docs/flatten.md) — `flatten-ctl` 命令和确定性展平。
- [native-deps/docs/build.md](native-deps/docs/build.md) — native-deps 构建工作流。
