# sandbox-deps

[kuasar-sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox) 平台的原生依赖构建：从源码（含本地 patch）
构建 Go 仓不链接、但运行期消费的原生件。不同工具链（C/Rust/kbuild）、构建慢、发布节奏独立，
故单独成仓。

## 产物与消费方

| 产物 | 来源 | 消费方 |
|---|---|---|
| `mkfs.erofs` | erofs-utils v1.9.1 | `sandbox-builder`（展平）、`sandbox-runtime`（打 guest 镜像） |
| `fsck.erofs` | erofs-utils v1.9.1 | `sandbox-orchestrator`（`fsck.erofs --extract` 解包 base runtime 注入 envd） |
| `vmlinux` | Linux 6.1.169 + `deps/linux-patches` + `deps/vmlinux/*.config` | `sandbox-runtime`（boot guest） |
| `cloud-hypervisor` | CH v51.1 + `deps/ch-patches`（memfd 注入 / uffd handler / snapshot skip / balloon） | `sandbox-runtime`（VMM） |
| `envd` | e2b-dev/infra（发布 tarball，默认 tag `2026.22`） | `sandbox-orchestrator`（注入 `sandbox-runtime-e2b.erofs` 的 guest agent） |

> `librocksdb`（`sandbox-accelerator` 的 CGO 链接依赖）在该仓内构建，不在此处。

## 构建

```bash
make build                      # =all：cloud-hypervisor + vmlinux + erofs + envd（全部四件）
make erofs                      # mkfs.erofs + fsck.erofs（较快）
make vmlinux                    # 内核（~5-10 min 冷启；需 bc/bison/flex/libelf-dev/libssl-dev）
make cloud-hypervisor           # 打 patch + cargo build（~minutes 冷启，~200 crates）
make envd                       # e2b guest agent（e2b-dev/infra tarball；ENVD_TARBALL 覆盖 tag）
make TARGET_ARCH=aarch64 ...    # 交叉编译（配合 CROSS_PREFIX）

# CH / 内核 patch 开发循环
make ch-fetch && (cd build/src/cloud-hypervisor && <edit+commit>) && make ch-patches-format
```

各产物有 `fetch` / `patches-apply` / `patches-format` / `build` 子阶段，幂等性在脚本内。

## 内容

- `deps/build-{erofs,vmlinux,cloud-hypervisor,envd}.sh` + `common.sh` — 构建脚本
- `deps/ch-patches/`、`deps/linux-patches/` — 源码 patch（`git am` 应用）
- `deps/vmlinux/*.config` — 内核 defconfig 片段（common + per-arch）

平台 ABI 边界与定制要点见 `docs/sandbox-kernel.md`、`docs/cloud-hypervisor.md`；
跨架构与 release 打包见 `docs/build.md`。
