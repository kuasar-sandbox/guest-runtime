# build — 构建、依赖、跨架构

平台所有产物的构建工作流。覆盖:Go 二进制、CGO 二进制(cache-ctl)、第三方
构建(rocksdb / erofs / cloud-hypervisor / vmlinux)、guest rootfs 镜像
(sandbox-runtime.erofs)、跨架构交叉编译、release 打包。

## 1. 概述

### 1.1 产物清单

| 二进制 | 类型 | CGO | 依赖 |
|---|---|---|---|
| manifest-ctl | host CLI | 否 | — |
| flatten-ctl | host CLI | 否 | mkfs.erofs(运行时) |
| store-ctl | host daemon | 否 | — |
| sandbox-ctl | host daemon | 否 | cloud-hypervisor + vmlinux + sandbox-runtime.erofs(运行时) |
| node-ctl | host daemon | 否 | — |
| cache-ctl | host daemon | **是** | librocksdb.a + libstdc++(静态链接)|
| sandbox-init | guest PID 1 | 否 | — |
| sandbox-runtime.erofs | guest rootfs | n/a | sandbox-init + mkfs.erofs(构建时)|
| cloud-hypervisor | host VMM | n/a | rust + ch-patches |
| vmlinux | guest kernel | n/a | linux 6.1.169 + sandbox-{common,arch}.config |
| mkfs.erofs | host 工具 | n/a | erofs-utils 1.9.1 |
| fsck.erofs | host 工具 | n/a | erofs-utils 1.9.1(build-runtime-e2b.sh 解包) |
| envd | guest agent(e2b) | 否 | e2b-dev/infra(发布 tarball) |

### 1.2 目录布局

```
bin/
├── x86_64/                 # x86_64 二进制
│   ├── manifest-ctl, flatten-ctl, mkfs.erofs, fsck.erofs
│   ├── store-ctl, cache-ctl, sandbox-ctl, node-ctl
│   ├── sandbox-init, sandbox-runtime.erofs
│   └── cloud-hypervisor, vmlinux, envd
├── aarch64/                # aarch64 二进制(同上)
└── <name>                  # 软链接 → <host-arch>/<name>(仅原生构建生成)

build/
├── tarball/                # 跨架构共享的源码 tarball 缓存
├── src/{rocksdb,linux,cloud-hypervisor,e2b-infra}/   # 跨架构共享的源码树
├── x86_64/                 # x86_64 中间产物
│   ├── rocksdb/            (out-of-source build)
│   ├── linux/              (kbuild output)
│   ├── cloud-hypervisor/   (cargo target)
│   └── src/erofs-utils/    (in-tree build,per-arch 拷贝)
├── aarch64/                # aarch64 中间产物
└── dist/                   # 发布 tarball(make release 输出)
```

**软链接策略**:`make build`(host = target)完成后,在 `bin/` 下生成
`bin/<x> -> <arch>/<x>` 软链接。这样测试脚本默认 `BIN=$REPO_ROOT/bin` 始终
指向当前架构的二进制。跨架构构建**不更新软链接**(避免覆盖 host 当前指向)。

### 1.3 设计要点

- **per-binary 目标**:每个二进制都有独立 Make target,支持精细化重建
  (`make sandbox-ctl` 只重建一个)
- **Single-entry aggregator**:`make build` 一次产出全部 6 个 Go 二进制 +
  sandbox-runtime.erofs
- **opt-in 重型依赖**:在**主项目（umbrella）Makefile** 的 `make build` 里,
  `cloud-hypervisor` 和 `vmlinux` 不随 Go 二进制一起构建,分别 `make cloud-hypervisor`
  / `make vmlinux` 触发,因为冷构建分别需要 ~10 min / ~5-10 min,且不是每次开发
  都需要重建。注意 **`sandbox-deps` 仓自身**的 `make build`(=all)语义不同:
  它一次构建全部四件原生依赖 `cloud-hypervisor + vmlinux + erofs + envd`
  (CH / vmlinux 为多分钟冷构建)
- **CGO 静态链接**:cache-ctl 静态链接 librocksdb + libstdc++,binary 可以
  ship 到没装这些库的 host;glibc 仍动态
- **跨 arch 自动检测**:`TARGET_ARCH != HOST_ARCH` 自动启用交叉编译,设置
  `CROSS_PREFIX` / `--target` / `ARCH=` / `CROSS_COMPILE=` 等

## 2. 命令矩阵

```
make build             # 6 个 Go 二进制 + sandbox-runtime.erofs(单入口)
make manifest-ctl      # ↑ 单个二进制
make flatten-ctl
make store-ctl
make sandbox-ctl
make node-ctl
make cache-ctl         # CGO,静态链接 rocksdb
make sandbox-init      # guest PID 1,会被 sandbox-runtime 依赖
make sandbox-runtime   # guest rootfs erofs 镜像

make deps              # rocksdb + erofs(同步默认依赖)
make deps-rocksdb
make deps-erofs

make cloud-hypervisor  # patched CH 二进制(~10 min cold,opt-in)
make vmlinux           # guest 内核(~5-10 min,opt-in)
make envd              # e2b guest agent(e2b-dev/infra tarball,subminute)

make ch-fetch          # 拉源码 + git tag(一次性)
make ch-patches-apply  # 应用 deps/ch-patches/*.patch
make ch-patches-format # 反向提取 commits 到 deps/ch-patches/
make ch-build          # 仅 cargo build,不应用 patch
make ch-patches        # ch-patches-apply 的别名

make linux-fetch          # 拉 linux 源码 + git tag linux-patches-base(一次性)
make linux-patches-apply  # git am deps/linux-patches/*.patch
make linux-patches-format # 反向提取 commits 到 deps/linux-patches/
make linux-build          # 仅 defconfig + make,不应用 patch
make linux-patches        # linux-patches-apply 的别名
                          # (make vmlinux = linux-patches-apply + linux-build)

make test              # 所有 Go 单元测试(CGO 启用)
make test-e2e          # 所有 e2e + perf + dedup 报告(完整套件)
make test-e2e-{manifest,cache,cluster,sandbox-cold,sandbox-cold-manifest,
                sandbox-proto,node-ctl,density,obs}

make bench             # Go 微基准(cache/freq + cache/ec + cache/rocks)
make perf              # 系统级 perf(cache + sandbox + density)
make perf-{cache,sandbox,density}

make dedup-report      # 跨 image dedup 比较(test/scripts/dedup_report.sh)

make release VERSION=v0.1   # 当前 TARGET_ARCH 的 release tarball
make release-publish VERSION=v0.1  # 上传 release 到 GitHub
make release-clean

make proto             # 重新生成 gRPC stubs(需 protoc + protoc-gen-go(-grpc))
make lint              # go vet
make clean             # 删除 bin/ + build/<arch>/{rocksdb,cloud-hypervisor,sandbox-runtime,linux},
                        # 保留 tarball + src/(可能含 WIP git)
make help              # 列举所有 target
```

## 3. 跨架构构建

### 3.1 TARGET_ARCH 变量

| 取值 | 等价别名 | GOARCH | RUST_TARGET | KERNEL_ARCH |
|---|---|---|---|---|
| `x86_64`(默认 host=x86_64) | `amd64` | `amd64` | `x86_64-unknown-linux-gnu` | `x86_64` |
| `aarch64`(默认 host=aarch64) | `arm64` | `arm64` | `aarch64-unknown-linux-gnu` | `arm64` |

未设置时默认取 `uname -m`。命令行覆盖:

```bash
make TARGET_ARCH=x86_64 build
make TARGET_ARCH=aarch64 build
```

`HOST_ARCH != TARGET_ARCH` 时自动启用交叉编译。

### 3.2 工具链依赖

**原生构建**(host = target):无额外依赖,使用系统 `gcc` / `g++` /
`cargo` / `make`。

**x86_64 host → aarch64 交叉**(Debian/Ubuntu):

```bash
apt install gcc-aarch64-linux-gnu g++-aarch64-linux-gnu
rustup target add aarch64-unknown-linux-gnu

# erofs-utils + grocksdb (cgo) need target-arch dev libs (multi-arch)
sudo dpkg --add-architecture arm64
sudo apt update
sudo apt install libuuid1:arm64 uuid-dev:arm64 \
                 libzstd-dev:arm64 liblz4-dev:arm64 \
                 zlib1g-dev:arm64 libsnappy-dev:arm64
```

**aarch64 host → x86_64 交叉**(Ubuntu arm64):

```bash
apt install gcc-x86-64-linux-gnu g++-x86-64-linux-gnu
rustup target add x86_64-unknown-linux-gnu

sudo dpkg --add-architecture amd64
sudo apt update
sudo apt install libuuid1:amd64 uuid-dev:amd64 \
                 libzstd-dev:amd64 liblz4-dev:amd64 \
                 zlib1g-dev:amd64 libsnappy-dev:amd64
```

`uuid-dev` 让 `mkfs.erofs` configure 通过(EROFS 镜像 UUID);`libzstd/lz4/zlib/snappy`
让 `cache-ctl` 链接通过(grocksdb 的 cgo LDFLAGS 硬编码了这些 `-l<x>`,即使
我们的 `librocksdb.a` 是无压缩版,链接器仍要 resolve 这些 .so 文件)。

cache-ctl Makefile target 内置一段 dpkg 检测:跨编译时若缺这些 multi-arch
dev 包,会打印一条 apt 命令而不是输出一墙 `cannot find -l<lib>` 链接错误。

**通用依赖**(两架构同):

```bash
# Go 1.24+
# Linux kernel build deps
apt install bc bison flex libelf-dev libssl-dev pkg-config
# erofs-utils build deps
apt install autoconf automake libtool
# rocksdb build deps
apt install cmake
# Docker(e2e 测试)
# /dev/kvm(沙箱 e2e)
```

### 3.3 交叉构建命令

```bash
# x86_64 host 上交叉构建 aarch64
make TARGET_ARCH=aarch64 build
make TARGET_ARCH=aarch64 cache-ctl
make TARGET_ARCH=aarch64 cloud-hypervisor
make TARGET_ARCH=aarch64 vmlinux
```

产物到 `bin/aarch64/`,软链接保持指向 host 架构。运行 aarch64 二进制需要
aarch64 host(交叉构建产物在 x86_64 host 上不能直接执行)。

## 4. 第三方依赖

### 4.1 rocksdb(`make deps-rocksdb`)

构建无压缩版 librocksdb.a 静态库:

- 上游:`facebook/rocksdb v9.7.4`
- 输出:`build/<arch>/rocksdb/lib/librocksdb.a` + `include/`
- 用途:cache-ctl 静态链接(L1 LSM tree + BlobDB)
- 编译选项关闭 snappy / lz4 / zstd / bz2 / zlib(链接器仍要这些 .so 因为
  grocksdb 的 cgo LDFLAGS 硬编码,见 §3.2)

### 4.2 erofs-utils(`make erofs`)

构建 mkfs.erofs + fsck.erofs:

- 上游:`erofs/erofs-utils v1.9.1`
- 输出:`bin/<arch>/mkfs.erofs`、`bin/<arch>/fsck.erofs`
- 用途:
  - mkfs.erofs —— flatten-ctl 运行时展平镜像;build 时打包 sandbox-runtime.erofs
  - fsck.erofs —— orchestrator-ctl `build-runtime`(`fsck.erofs --extract`)解包基础
    runtime erofs 再注入 envd(组 sandbox-runtime-e2b.erofs)
- 只编 `lib`+`mkfs`+`fsck` 子目录(跳过 mount/dump/fuse;mount.erofs 在禁多线程时有 pthread 链接 bug)

erofs-utils 不支持 out-of-source 构建(autotools),源树拷贝到
`build/<arch>/src/erofs-utils/` 各自构建。

### 4.3 cloud-hypervisor(`make cloud-hypervisor`)

应用 ch-patches + cargo build:

- 上游:`cloud-hypervisor/cloud-hypervisor v51.1`
- 输出:`bin/<arch>/cloud-hypervisor`(已 patched)
- 时间:冷构建 ~10 min,热(cargo cache)~秒级
- 路径:`--remap-path-prefix` 剥离构建机绝对路径(panic 消息 + DWARF)——源码树相对
  (`./vmm/src/…`)、registry deps 映射到 `/cargo`,不泄漏 `$CH_SRC` / `$CARGO_HOME`
- patch 范围:外部托管 memfd backing + skip user-managed zone snapshot/restore +
  in-process uffd handler(详见 [`cloud-hypervisor.md`](cloud-hypervisor.md))

**Patch 开发流**:

```bash
make ch-fetch                   # 一次性: 拉源码 + git tag ch-patches-base
cd build/src/cloud-hypervisor   # 改源码 + git commit
make ch-patches-format          # 提取 commits 到 deps/ch-patches/*.patch
make cloud-hypervisor           # 应用 + 重 build,验证可重复
```

`patches-apply` 会做 sanity 检查:目标 git tree 必须有 `ch-patches-base` tag;
HEAD == base 时应用所有 patches;HEAD 已经 = base + patches.len() 个 commits 且
subject 完全匹配时视为已应用,跳过(幂等);任何其他状态:报错并指引"先
`make ch-patches-format` 保 WIP,然后 `git reset --hard ch-patches-base`,
再重跑"——避免静默覆盖未保存的开发中改动。

**WSL2 注意**:CH 源树 ~50K 文件、cargo target ~2 GiB。WSL2 在 `/mnt/<drive>/`
(DrvFs)上每小文件 5-10× I/O 开销。建议覆盖:

```bash
CLOUD_HYPERVISOR_SRC=~/ch-build/src \
CLOUD_HYPERVISOR_BUILD_OUT=~/ch-build/out \
make cloud-hypervisor
```

### 4.4 vmlinux(`make vmlinux`)

`make vmlinux` = `linux-patches-apply` + `linux-build`,应用 linux-patches + 编译:

- 上游:`linux 6.1.169`(LTS)
- patch 范围:`deps/linux-patches/*.patch` —— 平台对 guest 内核的定制补丁,
  当前一条:virtio_balloon 在不可行 host target 下收敛到可持续大小而非活锁
  (详见 [`sandbox-kernel.md`](sandbox-kernel.md) §5.6);arch-neutral,两 arch 共用
- defconfig 来源:`deps/vmlinux/sandbox-common.config` + `sandbox-<arch>.config`
- 输出:`bin/<arch>/vmlinux`(x86_64 是 ELF,aarch64 是 PE Image)
- 时间:首次 ~5-10 min(`-j$(nproc)`)

**Patch 开发流**(与 §4.3 的 ch-patches 流对称):

```bash
make linux-fetch            # 一次性: 拉源码 + git tag linux-patches-base
cd build/src/linux          # 改源码 + git commit
make linux-patches-format   # 提取 commits 到 deps/linux-patches/*.patch
make vmlinux                # 应用 + 重 build,验证可重复
```

`patches-apply` 的幂等 / sanity 检查语义与 ch-patches 相同(tree 须有
`linux-patches-base` tag;HEAD==base 时全部应用;已应用则跳过;其他状态报错
并指引先 `make linux-patches-format` 保 WIP 再 reset),避免静默覆盖开发中改动。

详细配置体系与补丁决策见 [`sandbox-kernel.md`](sandbox-kernel.md)。

### 4.5 envd(`make envd`)

构建 e2b guest agent,供 sandbox-orchestrator `make sandbox-runtime-e2b` 注入到
`sandbox-runtime-e2b.erofs` 的 `/opt/sandbox-runtime/bin/envd`:

- 上游:`e2b-dev/infra` 发布 tarball(默认 tag `2026.22`;`ENVD_TARBALL` 覆盖)
- 输出:`bin/<arch>/envd`
- 构建:`go build packages/envd`,`CGO_ENABLED=0`,GOARCH 选目标(无需交叉工具链)
- 用途:e2b profile guest 内的数据面 agent(端口 49983)

源码 tarball 缓存到 `build/tarball/e2b-infra-<tag>.tar.gz`,extract 到跨架构共享的
`build/src/e2b-infra/`(Go 以 GOARCH 选目标)。与 cloud-hypervisor/vmlinux 不同,envd 无 patch 流。

**工具链注意**:envd 的 `go.mod` 钉了较新的 Go(如 `go 1.26.3`),`GOTOOLCHAIN=auto`
会按需下载该工具链。**该下载要求开启 GOSUMDB**——Go 拒绝在 `GOSUMDB=off` 下下载并运行
工具链(内网构建常关 GOSUMDB,会让这步失败)。

## 5. CGO 与静态链接

cache-ctl 是唯一的 CGO 二进制。Makefile 维护两组 LDFLAGS:

| 名称 | 内容 | 用途 |
|---|---|---|
| `CGO_LDFLAGS` | `-L... -lrocksdb -lstdc++ -lm -lpthread -ldl` | `make test`:dynamic libstdc++,fast iteration |
| `CGO_LDFLAGS_STATIC` | `-L... -Wl,-Bstatic -lrocksdb -lstdc++ -Wl,-Bdynamic -lm -lpthread -ldl` | `make cache-ctl`:静态拉 librocksdb.a + libstdc++ 进 binary;glibc 仍动态 |

`GOLDFLAGS_STATIC`(`-linkmode=external -extldflags "-static-libstdc++ -static-libgcc"`)
进一步指示 Go linker 把 libgcc 也静态。

cache-ctl binary 因此可以 ship 到没装 librocksdb / libstdc++ 的 host;glibc
版本兼容性由 build host 与 deploy host 的 glibc 版本决定(向前兼容,build 在
旧 glibc 上即可)。

cross 编译时 `CC=$(CROSS_PREFIX)gcc` / `CXX=$(CROSS_PREFIX)g++` 透传给 cgo;
跨工具链 g++ ships 一个匹配的 libstdc++.a,所以静态链接不需要显式 sysroot。

## 6. sandbox-runtime.erofs 构建

`make sandbox-runtime` 流程(详见 [`sandbox-runtime.md`](sandbox-runtime.md)):

1. 准备 `build/<arch>/sandbox-runtime/` 空目录
2. mkdir `sbin proc sys dev overlay/lower overlay/upper sysroot opt/sandbox-runtime`
3. cp `bin/<arch>/sandbox-init` → `sbin/init`,chmod +x
4. 选择 mkfs.erofs:优先 `bin/<HOST_ARCH>/mkfs.erofs`,fallback `bin/<arch>/`
   (仅原生构建可用),最后 system PATH。EROFS 文件格式 endian-neutral,
   任意 host arch 上的 mkfs.erofs 都可生成镜像
5. `mkfs.erofs $(BINDIR)/sandbox-runtime.erofs <staging-dir>`
6. **2 MiB 对齐 padding**:CH virtio-pmem 要求 backing file size 是 2 MiB
   倍数(hugepage 边界)。erofs 自身在 4K 边界结束,Makefile 用 truncate 把
   文件 pad 到下一个 2 MiB。EROFS readers 读 superblock 自描述大小,忽略
   trailing 字节,padding 对 mount 不可见

镜像内的 `/sbin/init` 是 target arch 二进制,所以镜像跨 mount 但**不**跨
arch 跑。

## 7. 测试

### 7.1 单元测试

```bash
make test
# CGO_ENABLED=1, race detector on, 全包
```

CGO 启用是为了让 cache/rocks/ + cache/server/ + cmd/cache-ctl/ 编译。其他包
没有 CGO 依赖,启用 CGO 不影响。

### 7.2 e2e 测试

```bash
make test-e2e          # 全套 e2e + perf + dedup 报告
make test-e2e-manifest      # manifest 路径基础(无 KVM 依赖)
make test-e2e-cache         # cache-ctl 三形态
make test-e2e-cluster       # cluster rolling
make test-e2e-sandbox-cold  # 冷启动(需要 KVM + TAP + vmlinux)
make test-e2e-sandbox-cold-manifest  # blk0 是 manifest://
make test-e2e-sandbox-proto # 双向 launch 协议
make test-e2e-node-ctl      # node-ctl daemon 协议
make test-e2e-density       # 密度场景(需要 KVM + vmlinux)
make test-e2e-obs           # OBS backend(可选,需 OBS_E2E=1 + 凭据)
```

需要 KVM / TAP / vmlinux 的目标会自动声明依赖,缺失时脚本报错并指引
prerequisite 设置(`/dev/kvm` 权限、TAP 创建命令等)。OBS 测试在凭据缺失时
跳过,不报错(`REQUIRE_OBS=1` 强制失败)。

### 7.3 性能 harness

```bash
make bench             # Go 微基准(cache/freq + cache/ec + cache/rocks)
make perf              # 全套系统级 perf
make perf-cache        # cache-ctl 三场景 bench(local + tiered-l1 + tiered-shard-l2)
make perf-sandbox      # 沙箱启动 / 快照 / 恢复 wallclock + uffd 行为
make perf-density      # 密度场景(N 沙箱并发,workload = cycles/pareto/idle)
```

详见 [`perf.md`](perf.md)。

## 8. Release 打包

```bash
# 1. 在 x86_64 host(或 aarch64 host)上分别构建两个架构的 release tarball
make TARGET_ARCH=x86_64 release VERSION=v0.1
make TARGET_ARCH=aarch64 release VERSION=v0.1
#    → build/dist/kuasar-sandbox-v0.1-linux-x86_64.tar.gz
#    → build/dist/kuasar-sandbox-v0.1-linux-aarch64.tar.gz

# 2. 打 git tag 并推送(注意:Makefile 不自动打 tag)
git tag v0.1
git push origin v0.1

# 3. 创建 GitHub Release 并上传两个 tarball
make release-publish VERSION=v0.1
```

`release-publish` 要求:`gh` CLI 已认证;`git tag <VERSION>` 已存在并已推送;
`build/dist/` 下两个架构的 tarball 都存在(只有一个会拒绝发布)。

如果同名 release 已存在,assets 会以 `--clobber` 模式覆盖上传。

### 8.1 Release tarball 结构

```
kuasar-sandbox-v0.1/
├── bin/                    # 扁平,所有二进制(单架构)
├── test/{e2e,perf,scripts}/
├── docs/                   # 设计文档(本目录)
├── README.md
└── VERSION                 # version + arch + commit + build date
```

测试脚本默认 `BIN=$REPO_ROOT/bin`,与扁平 bin/ 自然兼容。

## 9. arm64 native 运行环境准备

```bash
# 1. KVM 设备
ls -l /dev/kvm
#   crw-rw---- 1 root kvm 10, 232 ... /dev/kvm
# 当前用户加入 kvm 组,或以 root 跑测试

# 2. cgroup v2 unified hierarchy
mount | grep cgroup2
#   cgroup2 on /sys/fs/cgroup type cgroup2 ...

# 3. TAP 设备(沙箱 e2e 网络隔离)
sudo ip tuntap add dev sb-tap0 mode tap
sudo ip link set sb-tap0 up

# 4. erofs-utils 系统包(可选,做 fallback)
apt install erofs-utils

# 5. 构建 + 跑 e2e
make build
make cache-ctl
make cloud-hypervisor   # ~10 min
make vmlinux            # ~5-10 min
make test
make test-e2e-sandbox-cold
```

## 10. 已知架构差异

详细架构差异(启动协议、中断控制器、串口、页大小、PCI 拓扑)见
[`sandbox-kernel.md`](sandbox-kernel.md) §架构差异和
[`cloud-hypervisor.md`](cloud-hypervisor.md) §启动协议。

`deps/ch-patches/` 中的三个 patches(memfd 注入、snapshot 跳过 user-managed
内存、external uffd handler)不依赖 arch-specific 代码,在 x86_64 与 aarch64
两个 cargo target 上均能编译。`deps/linux-patches/`(virtio_balloon 收敛,§4.4)
同样 arch-neutral,两 arch 共用同一组、各自 defconfig 编译。运行时验证以
native host 跑 `e2e_sandbox_cold.sh` 为准。

## 11. See Also

- [`flatten.md`](flatten.md) §mkfs.erofs 调用 —— flatten-ctl 与 mkfs.erofs
  的运行时关系
- [`sandbox-kernel.md`](sandbox-kernel.md) —— vmlinux 配置体系详细说明
- [`cloud-hypervisor.md`](cloud-hypervisor.md) —— CH patches 设计与 patch
  开发流
- [`sandbox-runtime.md`](sandbox-runtime.md) —— sandbox-runtime.erofs 镜像
  内容与 sandbox-init 二进制
- [`perf.md`](perf.md) —— 性能 harness 详细使用方式
