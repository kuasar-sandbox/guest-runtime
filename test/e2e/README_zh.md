[English](README.md) | [简体中文](README_zh.md)

# Registry 与镜像展平 E2E

Guest-runtime 产品 E2E 统一由平台提供的 prepared-workspace runner 执行。组件仓库提供两个相互独立的 `image` 用例以及 guest-runtime 专属 helper，不再提供另一套产品 E2E runner。

执行模型保持简单：

```text
预构建产品 -> prepare -> <suite>.<case>.sh -> run
```

当前用例：

- `image.flatten.sh`：从 OCI 镜像配置/属主到真实 flatten/EROFS 输出的正确性。
- `image.registry.sh`：真实 registry、OCI 1.1 Referrers、Store、owner/key 隔离、过期与认证契约。

## 运行与前置条件

使用 Kuasar 平台测试包或精确 integration prepared workspace 中的 runner 和 cases。例如：

```sh
RUNNER=/path/to/platform/test/e2e/e2e
RELEASE_DIR=/path/to/prebuilt/platform-release
WORK=/tmp/kuasar-e2e

"$RUNNER" prepare --release-dir "$RELEASE_DIR" --workdir "$WORK"
"$RUNNER" run --workdir "$WORK" \
  --include image.flatten.sh \
  --include image.registry.sh
```

产品用例只消费 prepared `BIN`、`E2E_WORKSPACE`、`E2E_LIB`、prepared fixture 和 prepared test helper。执行阶段不编译产品或 helper、不查找相邻源码仓库、不自动拉取兜底工作负载镜像，也不回退到源码路径。

已选择的用例要求 Linux、root、Python 3、curl、可用 Docker daemon、GNU timeout、`mkfs.erofs`、支持 `--extract` 的 `fsck.erofs`、`flatten-ctl`、`store-ctl` 和 prepared zot helper。缺少或不可用的前置条件会使已选择用例失败，不能以成功 skip 代替。架构和宿主机能力属于执行条件，不是 suite。

Source/helper 回归保留在产品 E2E 之外，可通过以下命令运行：

```sh
make test-e2e-scripts
```

## 保留的测试意图

| 用例 | 实际执行的断言 |
| --- | --- |
| `image.flatten.sh` | 导出有效 subject digest 和非空 EROFS；JSON info 保留 prepared 镜像的 Architecture、OS、User、WorkingDir、Entrypoint、Env；从真实 flatten 产物提取出的文件系统继续保留层合并结果、whiteout、符号链接、可执行位和非 root 属主。 |
| `image.registry.sh` | 支持的 miss、upload/put 与幂等命中；真实 OCI Referrers 记录；owner/key 隔离；有限有效期的过期行为；匿名认证拒绝与显式认证成功。 |

Prepared fixture 是小型确定性双层 Docker 归档，包含非 root 属主、可执行内容、符号链接、删除 whiteout 和 opaque 目录。Artifact E2E 在真实 flatten 后的 EROFS 产物上验证这些语义；source/helper 回归单独验证 fixture 和断言 helper，不把产品 E2E 重新变成源码执行。

## 隔离与清理

每个用例使用私有临时状态和独立 `DOCKER_CONFIG`，不读取调用者凭据、credential helper 或环境中的 registry 凭据。生成的 tag 和子进程都归本次调用所有。进程清理有界并回收后代，包括脱离会话的孙进程。INT/TERM 分别保留退出码 130/143；部分启动失败也会清理，不删除其他任务资源。`E2E_KEEP=1` 仅在自有服务和 tag 已停止、移除后保留证据。

共享 runner 路由、provenance 校验和架构 lane 由平台 CI 契约维护。本文只负责 guest-runtime 用例意图和前置条件。
