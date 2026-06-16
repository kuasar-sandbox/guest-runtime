# sandbox-deps

[kuasar-sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox) 平台的原生依赖
构建:从上游源码(含本地 patch)构建 Go 仓不链接、但运行期消费的原生件。工具链
(autotools/kbuild/cargo/Go)与 Go 仓不同、冷构建以分钟计、上游发布节奏独立,
故单独成仓。

## 产物与消费方

| 产物 | 来源 | 消费方 |
|---|---|---|
| `mkfs.erofs` | erofs-utils v1.9.1 | `sandbox-accelerator`(展平)、`sandbox-runtime`(打 guest 镜像) |
| `fsck.erofs` | erofs-utils v1.9.1 | `sandbox-orchestrator`(`fsck.erofs --extract` 解包 base runtime 注入 envd) |
| `vmlinux` | Linux 6.1.169 + `deps/linux-patches` + `deps/vmlinux/*.config` | `sandbox-runtime`(guest 内核) |
| `cloud-hypervisor` | CH v51.1 + `deps/ch-patches`(memfd 注入 / snapshot skip / 外部 uffd / balloon 跳洞) | `sandbox-runtime`(VMM) |
| `envd` | e2b-dev/infra 发布 tarball(tag `2026.22`) | `sandbox-orchestrator`(注入 `sandbox-runtime-e2b.erofs` 的 guest agent) |

`librocksdb`(`sandbox-accelerator` 的 CGO 链接依赖)在该仓内构建,不在此处。

## 组成

| 路径 | 角色 |
|---|---|
| `deps/build-{erofs,vmlinux,cloud-hypervisor,envd}.sh` | 构建脚本(vmlinux / CH 为 STAGE 多阶段 dispatcher) |
| `deps/common.sh` | tarball 下载 / 缓存 / 解压共享逻辑 |
| `deps/ch-patches/` | cloud-hypervisor 补丁(4 个,`git am` 应用) |
| `deps/linux-patches/` | guest 内核补丁(1 个,arch-neutral) |
| `deps/vmlinux/*.config` | 内核 defconfig 片段(common + per-arch) |

## 构建

```bash
make build                      # =all: cloud-hypervisor + vmlinux + erofs + envd
make erofs                      # mkfs.erofs + fsck.erofs(最快)
make vmlinux                    # guest 内核(冷构建 ~5-10 min)
make cloud-hypervisor           # 打 patch + cargo build(冷 ~5-10 min,热秒级)
make envd                       # e2b guest agent(ENVD_TARBALL 覆盖 tag)
make build TARGET_ARCH=aarch64  # 交叉编译(CROSS_PREFIX 自动推导)

# CH / 内核 patch 开发循环(linux-* 目标同构)
make ch-fetch && (cd build/src/cloud-hypervisor && <edit+commit>) && make ch-patches-format
```

## 文档

- [docs/build.md](docs/build.md) —— 构建工作流:目标 / 阶段 / patch 开发循环 /
  交叉编译 / WSL2。
- [docs/cloud-hypervisor.md](docs/cloud-hypervisor.md) —— VMM patch 范围与外部
  托管内存行为契约(随发布包)。
- [docs/sandbox-kernel.md](docs/sandbox-kernel.md) —— guest 内核配置体系与平台
  ABI 边界(随发布包)。
