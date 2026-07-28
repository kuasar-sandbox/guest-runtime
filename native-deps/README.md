# native-deps

[kuasar-sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox) 平台的原生依赖
构建:从上游源码(含本地 patch)构建 Go 仓不链接、但运行期消费的原生件。工具链
(autotools/kbuild/Go)与 Go 仓不同、冷构建以分钟计、上游发布节奏独立,
因此内聚在 `guest-runtime/native-deps` 目录下独立构建。

## 产物与消费方

| 产物 | 来源 | 消费方 |
|---|---|---|
| `mkfs.erofs` | erofs-utils v1.9.1 | `accelerator`(展平)、`guest-runtime`(打 guest runtime 镜像) |
| `fsck.erofs` | erofs-utils v1.9.1 | 源码树诊断与 accelerator EROFS 内容断言测试 |
| `vmlinux` | Linux 6.1.169 + `deps/linux-patches` + `deps/vmlinux/*.config` | `sandboxer`/`sandbox-ctl`(guest 内核) |
| `envd` | e2b-dev/infra 发布 tarball(tag `2026.22`) | `guest-runtime`(注入 `sandbox-runtime.bundle` 的 guest agent) |

patched `cloud-hypervisor` 是 `sandbox-ctl` 的 VMM 运行件,由
`sandboxer/native-deps` 构建;`librocksdb`(`accelerator` 的 CGO 链接依赖)在
accelerator 仓内构建,不在此处。

## 组成

| 路径 | 角色 |
|---|---|
| `deps/build-{erofs,vmlinux,envd}.sh` | 源码构建脚本(vmlinux 为 STAGE 多阶段 dispatcher) |
| `deps/common.sh` | tarball 下载 / 缓存 / 解压共享逻辑 |
| `deps/linux-patches/` | guest 内核补丁(1 个,arch-neutral) |
| `deps/vmlinux/*.config` | 内核 defconfig 片段(common + per-arch) |

## 构建

```bash
make build                      # =all: vmlinux + erofs + envd
make erofs                      # mkfs.erofs + fsck.erofs(最快)
make vmlinux                    # guest 内核(冷构建 ~5-10 min)
make envd                       # e2b guest agent(ENVD_TARBALL 覆盖 tag)
make build TARGET_ARCH=aarch64  # 交叉编译(CROSS_PREFIX 自动推导)

# 内核 patch 开发循环
make linux-fetch && (cd build/src/linux && <edit+commit>) && make linux-patches-format
```

## 文档

- [docs/build.md](docs/build.md) —— 构建工作流:目标 / 阶段 / patch 开发循环 /
  交叉编译 / WSL2。
- [../docs/vmlinux.md](../docs/vmlinux.md) —— guest 内核配置体系与平台
  ABI 边界(随发布包)。
