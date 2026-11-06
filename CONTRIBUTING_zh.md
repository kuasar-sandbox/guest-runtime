[English](CONTRIBUTING.md) | [简体中文](CONTRIBUTING_zh.md)

# 为 guest-runtime 做贡献

本篇是源码修改、组件验证与发行准备的入口。公开贡献与共享 CI 政策由项目统一维护；以下组件流程说明应执行什么，以及所得证据能证明什么。

## 贡献流程

使用公开 Fork 和主题分支。贡献者无需上游写权限即可提交 issue、评审和 PR。维护者评审修改、安排受信验证并 squash merge；直接推送上游 `main` 不是贡献路径。

## 准备 Fork

把仓库 Fork 到个人 GitHub 账号，然后配置 HTTPS remote：

```bash
git clone https://github.com/<your-login>/guest-runtime.git
cd guest-runtime
git remote add upstream https://github.com/kuasar-sandbox/guest-runtime.git
git fetch upstream
git switch -c feat/short-description upstream/main
# 编辑、测试并提交
git push -u origin feat/short-description
```

维护修复从受支持的 `upstream/release/vMAJOR.MINOR.x` 开始，并把该分支设为 PR 目标。适用时启用 **Allow edits from maintainers**。

按以下方式推进 Fork 的 `main`，避免覆盖本地提交或未提交工作：

```bash
git fetch upstream
git switch main
git merge --ff-only upstream/main
git push origin main
```

切分支前先提交或安全保存未提交内容。若 `--ff-only` 因历史分叉而拒绝，检查本地提交并有意识地处理历史；日常同步不应 reset 或强推 Fork 的 main。每个新主题分支应从新获取的上游目标创建，不从无关功能分支创建。

## PR 要求

PR 应：

- 范围集中，清楚说明问题与解决方法；
- 有相关 issue 时提供链接；
- 行为变化时同步测试与文档；
- 不包含凭据、私钥、令牌、客户数据或无关生成文件；
- 说明已执行的验证，以及运行或兼容风险。

欢迎通过 Draft PR 提前征求意见，但 Draft 不是可合并候选。

## Guest 内核发行输入

自动 `vmlinux` 版本选择和内核发行说明只考虑 `native-deps/deps/vmlinux/*.config` 与 `native-deps/deps/linux-patches/`；配置注释变化也算发行输入变化。

可能改变内核产物的构建脚本、Makefile、上游源码版本/摘要或编译器/工具链变更，必须在同一 PR 更新一个 `native-deps/deps/vmlinux/*.config`。无需改选项时，可用配置注释说明为何需要新内核发行版；其他构建输入本身不会独立触发内核发行。

此发行意图规则不收窄 CI 验证或缓存。候选内核构建与缓存检测继续使用完整构建输入规则，包括相关纯脚本和 Makefile 变化。

## E2E 执行要求

每个 `test/e2e/cases/<suite>.<case>.sh` 必须在 shebang 下方以一个静态 `# e2e-requires: [...]` JSON 数组声明最低内核与服务能力，包括 helper 使用的能力；无需能力时显式写 `[]`。Owner 格式位于 `test/e2e/lib/requirements.json`，包含 `{"schema":1}`。缺失、重复或未知能力导致校验失败。正常固定源码选择会发现 case 增删和要求变更，无需平台维护 case allowlist。Runner label 与放置由受信平台配置决定，不由 case 决定。支持的名称、校验与历史输入兼容见[能力契约](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/test/README_zh.md#静态执行需求)。

## 文档

遵守[项目文档政策](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/CONTRIBUTING.md#documentation-contributions)。以 `name.md` / `name_zh.md` 维护完整英文/中文配对和互相可达的语言选择器，保留要求、标识符、示例、图表、事实和链接，记录有源码依据的修正。已有完整英文独有材料可以继续保留；摘要或语言检测通过都不等于完整翻译。

## 贡献许可

提交贡献即同意采用所修改文件适用的许可证。无其他明确声明的新项目文件按 [Apache License 2.0](LICENSE) 贡献。

仅提交有权贡献的内容。修改第三方或采用其他许可证的材料时，保留适用的 copyright、attribution、NOTICE 与 SPDX 声明。

仓库内非 Apache 的许可边界见 [LICENSE_SCOPE_zh.md](LICENSE_SCOPE_zh.md)。

## 评审要求

评审应具体且建设性。作者应解决评审对话，或解释建议为何不适用。维护者可以说明原因后关闭长期无进展、被替代、不安全或超出范围的 PR。

## CI 准入与合并证据

[中央 CI 契约](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/ci_zh.md)统一定义受信准入、workflow/status 标识符、companion、源码选择和合并证据。默认分支 wrapper 负责准入；候选 workflow 不取得控制面或发行凭据。任何贡献者均可提交公开 Fork PR，特权执行是独立信任决定。未获准入的外部候选应由维护者评审并准备保留作者与已评审源码的受信上游候选，而不是降低准入要求。

请求正常评审后的 squash merge 前：

1. 使 PR 进入 Ready，目标为 `main` 或受支持维护分支，并解决阻塞评审对话。
2. 验证当前精确 target base/reviewed head 的 integration 和必需测试；跳过、取消、失败或旧运行不构成成功。
3. 每个声明的 companion 独立验证，复核 repository、PR、base、head、target ref 与 integration 匹配记录的源码集合。
4. base/head/companion 变化后重新取得证据。只改正文不触发执行，必要时使用受支持的 Draft→Ready 事件。只有已诊断为瞬时失败且准入输入未变时才重跑旧事件。
5. 合并前再次核对实际源码集合；一个 companion 合并后，删除其过时声明，并对剩余候选重新验证。

不得制造 status、用手动 dispatch 绕过准入、削弱分支保护或在候选执行中保留 source-fetch 凭据。不使用 auto-merge、merge commit 或 rebase merge。精确当前检查和完整信任边界以中央契约为准。

## 组件构建与验证

精确源码模式与生产输入见[构建](docs/build_zh.md)。使用选定 workspace 执行包测试，预构建产品测试按 [Prepared E2E 指南](test/e2e/README_zh.md)执行。[内核验证](docs/vmlinux_zh.md#验证)区分静态产物检查与真实启动/ABI 证据。补丁工作树维护与安全清理见 [native-deps](native-deps/README_zh.md)。共享 workflow 名称与 runner 路由以中央 CI 契约为准。
