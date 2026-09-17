# Registry 与镜像展平 E2E

[English](README.md) | [简体中文](README_zh.md)

本套件驱动真实 Docker、zot OCI 1.1 Referrers、`flatten-ctl`、EROFS 和
`store-ctl`。默认夹具不依赖远端工作负载镜像或私有 runner 缓存。Working-set VM
矩阵仍由平台单独负责;本套件不能替代该检查。

## 运行与前置条件

运行 `make test-e2e`,或显式传入组装后的二进制目录及 zot:

```sh
BIN=/path/to/assembled/bin ZOT_BIN=/path/to/zot bash test/e2e/run_all.sh
make test-e2e-scripts
```

真实测试要求 Linux、root 或免密码 sudo、Python 3、curl、可用的 Docker daemon、
GNU timeout、`mkfs.erofs`、`flatten-ctl`、`store-ctl` 和 zot。`run_all.sh` 强制
要求全部前置条件和辅助脚本;缺失或不可运行均失败。直接运行 `e2e_flatten.sh` 时,
可显式用 `REQUIRE_GUEST_RUNTIME=0` 执行可选本地检查,并明确输出 `[SKIP]`;
该模式不是 CI 验收。平台 `make e2e-tools` 提供 zot;发行包不包含测试 registry。
构建二进制所需的私有源码依赖仍须授权访问;执行公开发行二进制与验证候选源码是不同证据。

## 保留的测试意图

| 意图 | 实际执行的断言 |
| --- | --- |
| Registry 导出与 EROFS | 导出有效 subject digest 和非空 EROFS,用 JSON info 检查运行配置。 |
| OCI Referrers 与幂等 | 支持的 miss、upload、put 后重复命中同一 ID;真实 Referrers API 中存在对应 artifact。 |
| Owner/key 隔离 | 第二个 owner 初次 miss;不同 key 产生不同 ID;两个 owner 随后各自读取原 ID。 |
| 过期过滤 | 短有效期记录先被观测为 LIVE,随后成为支持的 miss;同一过期 artifact 仍在 registry 索引中。 |
| 认证 | 正常运行的 registry 对匿名请求返回 HTTP 401;CLI 失败必须与认证相关;显式凭据能够 upload/put 并随后命中。 |

默认夹具是本地生成的小型确定性双层 Docker 归档,包含非 root 属主、可执行内容、
符号链接、删除 whiteout 和 opaque 目录。离线测试检查夹具字节及层合并语义,
不据此声称已经逐项验证导出 EROFS 的全部文件系统语义。`E2E_IMAGE` 可选择调用者
已缓存的镜像;套件不自动拉取,也不删除该输入 tag。

## 隔离与清理

每次运行使用私有临时目录和独立 `DOCKER_CONFIG`,不读取调用者凭据、credential
helper 或环境中的 registry 凭据。仅配置本地测试 registry 的固定测试凭据。
生成的 tag 和子进程均归本次调用所有。进程清理有界并回收后代,包括已脱离会话的孙进程。
INT/TERM 分别保留退出码 130/143。部分启动失败也执行清理,不会删除其他任务资源。
`E2E_KEEP=1` 只保留证据;本次服务和 tag 仍必须先停止并移除。

平台组装器复制完整 `test/e2e` 目录,包括 Python 辅助脚本。离线回归覆盖前置条件
缺失、非法 JSON、夹具完整性、调用者凭据隔离、组装包完整性、正常/失败退出、信号与重复清理。
