[English](using-guest-runtime.md) | [简体中文](using-guest-runtime_zh.md)

# 使用选定的 Guest Runtime 与 Kernel

取得并验证匹配聚合发行版，或明确选定兼容 Runtime/Kernel/sandboxer 组合后，使用本流程检查安装产物、制备应用镜像，并处理版本保留、恢复和排障。完整平台安装与调度见[项目快速开始](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/quickstart_zh.md)。

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

下例为能在无 NIC 环境运行的应用绑定三个冷启动工件。`UPPER_TEMPLATE` 必须已由选定部署流程准备为兼容的空 ext4 upper 模板，并通过所选部署流程验证。宿主须满足所选 sandboxer 的 Linux/KVM/VMM 要求。此最小 shell 示例使用不含空格的绝对路径。这里不配置网络；需要网络时按 sandboxer/connector 流程添加。

```bash
UPPER_TEMPLATE=/absolute/path/to/validated-empty-upper.ext4
test -s "$UPPER_TEMPLATE"
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
      diff_template: file://$UPPER_TEMPLATE
EOF
"$PREFIX/bin/sandbox-ctl" config --config "$WORK/sandbox.yaml" --check strict
"$PREFIX/bin/sandbox-ctl" run --config "$WORK/sandbox.yaml" --sandbox-id guest-example
```

应先通过严格配置验证，再由前台 run 报告启动结果或具体错误。省略 `launch.exec` 时，overlay 模式采用镜像 Entrypoint/Cmd；无需另填 startup resource override。身份不匹配、upper 模板缺失/无效、权限不足或 VMM 不兼容都应修复后重试，不能绕过。检查、关闭及保留状态清理按 [sandboxer 使用指南](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/runtime_zh.md#standalone-workflow)。应用镜像、Runtime、内核与 backing 模板仍被引用时不得删除。

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

整机重启后沙箱不自动恢复。节点重新启动时需要 runtime 镜像文件仍在部署目录,
供后续新沙箱创建使用。

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
| build sandbox 找不到 `flatten-ctl` | 检查 `/opt/sandbox-runtime/bin/flatten-ctl` 是否进入镜像 |
| build sandbox 无法生成 EROFS | 检查 guest `flatten-ctl`、源读取权限和规划预算 |
| host 无法打包交叉架构镜像 | 检查 `BUILD_MKFS_EROFS` 是否为宿主架构工具 |
| restore 后行为异常 | 检查 snapshot 使用的 runtime digest 与 restore 配置是否匹配 |
| 解包后脚本找不到 runtime | 确认已解压 runtime archive,且 `bin/sandbox-runtime.bundle` 存在 |

## 后续入口

- [Runtime 交付契约](runtime-bundle_zh.md)
- [内核能力与验证](vmlinux_zh.md)
- [完整 flatten CLI](flatten-cli_zh.md)
- [构建与发行复现](build_zh.md)
