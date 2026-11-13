[English](README.md) | [简体中文](README_zh.md)

# guest-runtime

首次运行跨组件沙箱或 Demo，推荐从项目[快速开始](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/quickstart_zh.md)
使用匹配聚合版本的 [workbench](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/workbench/README_zh.md)。
本组件仍可独立构建和部署，workbench 不是生产运行的强制依赖。预构建架构以所选
发布版的实际资产为准；源码支持某架构不意味着所有历史版本都提供该架构制品。

`guest-runtime` 构建 [Kuasar Sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox) 使用的 **guest kernel、Runtime image 和 image build tool**。

本仓负责 guest 环境及其构建输入、provenance 与 Release artifact。Guest PID 1 源码与共享 ABI 包由本仓维护。Host 侧 MicroVM 生命周期、snapshot 和 restore 属于 [`sandboxer`](https://github.com/kuasar-sandbox/sandboxer)。共享 Manifest、cache/store、OCI retrieval 和 image-flattening library 属于 [`accelerator`](https://github.com/kuasar-sandbox/accelerator)。

## 职责

- 从有文档记录的 kernel source、config 和 patch set 构建 guest VMLinux image;
- 构建 Envd 等原生 guest 依赖，并使用环境安装的宿主 EROFS 工具打包与校验;
- 构建 OCI/directory 到 EROFS 的 image builder `flatten-ctl`;
- 组装 `sandbox-runtime.bundle`,其中包括本仓构建的 `sandbox-init` 和本仓维护的 guest payload;
- 验证生成的 guest/Runtime 文件系统结构;
- 发布并记录两种独立版本的发行单元:Runtime 和 VMLinux。

本仓只对应一个组件,但发布两条发行单元版本线。

## 仓库布局

| 路径 | 用途 |
| --- | --- |
| `cmd/sandbox-init` | Guest PID 1：运行环境准备、进程监督与控制协议 |
| `pkg/guestabi` | Host 与 Guest 共用的控制、MUX 和转发协议包 |
| `cmd/flatten-ctl` | 使用共享 `accelerator` package 将 OCI 或目录构建为 deterministic EROFS image |
| `native-deps/` | Guest VMLinux/Envd 源码输入、补丁树和 Native 维护 |
| `scripts/guest-inspect.py` | 在兼容的 nonrandomized x86_64 布局中从 Host 读取 guest kernel memory counter |

Runtime image 将初始 guest tool 放在 `/opt/sandbox-runtime/bin/`。VMLinux 与 Cloud Hypervisor binary 不嵌入 Runtime bundle:VMLinux 作为独立发行单元发布,Cloud Hypervisor 由 `sandboxer` 构建和发布。

## 按流程或交付契约阅读

| 读者任务 | 指南 |
| --- | --- |
| 消费选定发行版、制备应用并绑定产物 | [使用 guest-runtime](docs/using-guest-runtime_zh.md)：验证、冷启动示例、升级与恢复 |
| 查看 Runtime 字节布局、payload、身份和兼容 | [Runtime bundle](docs/runtime-bundle_zh.md) |
| 核对 Guest 内核能力、架构及精确交付身份 | [VMLinux](docs/vmlinux_zh.md) |
| 用发行 CLI 导出、检查、缓存或转换镜像 | [flatten-ctl](docs/flatten-cli_zh.md) |
| 构建或复现选定 Runtime/Kernel 发行版 | [构建](docs/build_zh.md)：源码模式、依赖选择、Native 目标和包验证 |

[共享镜像制备实现](https://github.com/kuasar-sandbox/accelerator/blob/main/docs/image-preparation_zh.md)归 accelerator。内核补丁开发与安全增量源码/缓存操作就近保留在 [native-deps](native-deps/README_zh.md)。[贡献指南](CONTRIBUTING_zh.md)说明源码修改、发行意图与验证。每篇目标指南均有完整英文版。

## 集成边界

- 本仓构建 `sandbox-init` 并维护 [Guest ABI](docs/guest-abi_zh.md)；`sandboxer` 负责 Host 生命周期；
- `accelerator` 负责共享 EROFS/OCI flattening library;本仓在 Runtime 发行单元发布 `flatten-ctl` binary;
- `orchestrator` 通过完整平台使用生成的 Runtime 和 VMLinux artifact;
- `kuasar-sandbox/kuasar-sandbox` 选择精确发行单元,运行跨组件验证并发布聚合 Release。

## 发行选择

同一仓库的 Runtime 与 VMLinux 是独立版本的发行单元。采用经验证聚合版选定的精确 Tag，并使用所声明架构的包。[使用 guest-runtime](docs/using-guest-runtime_zh.md)说明选择与验证，[构建](docs/build_zh.md)说明选定源码复现。共享发行线、Preview、清理与 Latest 规则由[项目发行契约](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release_zh.md)统一定义。

## 贡献与安全

阅读[仓库贡献指南](CONTRIBUTING.md)和[组织贡献指南](https://github.com/kuasar-sandbox/.github/blob/main/CONTRIBUTING.md)。Guest ABI、Runtime content、kernel config、source provenance 或 Release artifact 的修改必须完成相应验证和必要的 companion PR。

不要在公开 Issue 报告漏洞,也不要公开 private package source、credential、signing material 或 customer data。按 [Kuasar Sandbox 安全策略](https://github.com/kuasar-sandbox/kuasar-sandbox/security/policy)使用 GitHub private vulnerability reporting。

## License

项目原创内容采用 [Apache License 2.0](LICENSE)。内核构建输入的许可证边界见 [`LICENSE_SCOPE_zh.md`](LICENSE_SCOPE_zh.md)。Runtime package、EROFS tool、Envd、Buildroot/distribution input 和其他第三方 material 保留各自 license、notice、source 与 redistribution obligation。
