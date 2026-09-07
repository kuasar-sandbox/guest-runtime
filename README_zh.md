[English](README.md) | [简体中文](README_zh.md)

# guest-runtime

Guest 运行时镜像与构建工具仓:负责构建 `sandbox-runtime.bundle`、`flatten-ctl`
以及 guest 侧 native 产物。host 生命周期、快照、恢复和 `sandbox-init` 源码属于
`sandboxer`;内容寻址、manifest/cache/store 与展平公共库属于 `accelerator`。
系统集成、跨仓验证和聚合发布由
[Kuasar Sandbox 项目主仓](https://github.com/kuasar-sandbox/kuasar-sandbox)维护。

## 组成

| 路径 | 角色 |
|---|---|
| `cmd/flatten-ctl` | OCI/目录 → EROFS deterministic image builder;复用 `accelerator/pkg/{flatten,image,remote,tar}` |
| `docs/sandbox-runtime.md` | runtime 镜像布局、guest payload、构建和发布契约 |
| `docs/vmlinux.md` | guest kernel 镜像契约和配置理由 |
| `docs/flatten.md` | `flatten-ctl` CLI、远程拉取缓存、OCI Referrers 行为 |
| `native-deps/` | 构建 `vmlinux`、`mkfs.erofs`、`fsck.erofs`、`envd` |
| `scripts/guest-inspect.py` | 从 Host 读取 Guest 内核内存计数器;要求兼容的非随机化 x86_64 布局 |

`guest-runtime` 把 `../sandboxer/bin/<arch>/sandbox-init` 和 guest payload 打进
同一份 DAX-shared EROFS 镜像。镜像内 `/opt/sandbox-runtime/bin/` 首版包含
`envd`、`flatten-ctl`、`mkfs.erofs`;`vmlinux` 和 `cloud-hypervisor` 不进入
runtime 镜像,分别由本仓的独立 `vmlinux-vX.Y.Z` 版本和 `sandboxer` 发布。

## 构建

```bash
make native-deps                 # vmlinux / erofs tools / envd
make flatten-ctl                 # OCI/dir -> deterministic EROFS builder
make build                       # sandbox-runtime.bundle with envd/flatten-ctl/mkfs.erofs
make sandbox-runtime             # same image target, builds ../sandboxer sandbox-init if needed
make build TARGET_ARCH=aarch64
```

`make sandbox-runtime` 需要**宿主可执行的 `BUILD_MKFS_EROFS`**:先查 Host PATH,
再按 Makefile 的 host/target 条件查已有 native 输出,也可显式指定该变量。宿主
packer 缺失时会明确失败,不能用不能在宿主执行的目标架构工具替代。镜像内的
`GUEST_MKFS_EROFS` 是另一个目标架构输入;它与 envd、flatten-ctl、sandbox-init
缺失时会按需触发对应构建目标,详见[构建指南](docs/sandbox-runtime_zh.md)。

## 产物

| 产物 | 生成入口 |
|---|---|
| `bin/<arch>/flatten-ctl` | `make flatten-ctl` |
| `bin/<arch>/sandbox-runtime.bundle` | `make sandbox-runtime` |
| `native-deps/bin/<arch>/vmlinux` | `make native-deps` |
| `native-deps/bin/<arch>/mkfs.erofs` / `fsck.erofs` | `make native-deps` |
| `native-deps/bin/<arch>/envd` | `make native-deps` |

本仓没有通用的 `guest-runtime-vX.Y.Z` 版本或同名归档,而是维护两条独立版本线:

- `runtime-vX.Y.Z`:发布 `sandbox-runtime-x86_64-vX.Y.Z.tar.gz`,包含
  runtime bundle、`flatten-ctl` 和 `mkfs.erofs`。
- `vmlinux-vX.Y.Z`:发布 `vmlinux-x86_64-vX.Y.Z.tar.gz`,包含稳定入口
  `bin/vmlinux`。

本仓文档与 `test/e2e/` 不进入上述组件包。项目主仓的 platform 聚合版本从所选 runtime tag
收集 runtime 文档与 flatten E2E,从所选 vmlinux tag 收集 kernel 文档,统一放入
platform 包。

两条版本线独立演进,版本号不要求相同。当前组件 Release 在组件构建与打包检查
后发布 Linux x86_64 资产;平台聚合再选择精确版本组合,执行跨组件 BMS 与实际
发布资产的 MicroVM 验证。源码支持其他架构不等于已发布对应预构建资产。`envd` 只随 runtime 镜像内置;
`fsck.erofs` 只作为源码树诊断/测试辅助产物。
项目主仓的每日协调器按上海日期分别触发 `runtime-vX.Y.Z-preview.YYYYMMDD` 和
`vmlinux-vX.Y.Z-preview.YYYYMMDD`;Preview 和维护分支 Stable 不更新 GitHub
Latest。两个单元的主线 Stable 独立构建发布;独立的幂等 Reconcile Latest 工作流按
`main` 源码提交先后协调本仓 Latest,同一提交才比较 SemVer。平台聚合仍按两个精确
Tag 选择,不依赖 Latest。
同版本发布与删除共用完整 workflow mutation group;若 GitHub 合并 pending 请求,项目主仓
协调器会把 cancelled 状态作为未完成操作自动重跑,不会把它当作发布或 GC 已完成。

## 文档

- [docs/sandbox-runtime.md](docs/sandbox-runtime_zh.md) — runtime 镜像打包、发布和消费契约。
- [docs/vmlinux.md](docs/vmlinux_zh.md) — guest kernel 配置、构建和平台 ABI。
- [docs/flatten.md](docs/flatten_zh.md) — `flatten-ctl` 命令和确定性展平。
- [native-deps/docs/build.md](native-deps/docs/build_zh.md) — native-deps 构建工作流。

## License

本仓库的项目原创内容采用 [Apache License 2.0](LICENSE).Linux 内核 patch 的
GPL-2.0-only 边界见 [LICENSE_SCOPE.md](LICENSE_SCOPE_zh.md).
贡献授权说明见 [CONTRIBUTING.md](CONTRIBUTING.md).
