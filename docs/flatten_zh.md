[English](flatten.md) | [简体中文](flatten_zh.md)

# flatten — 容器镜像展平工具

把多层 OCI/Docker 镜像合并(展平)成单个 EROFS 文件系统 payload,再封装成
**tarstream 镜像工件**,供 `manifest-ctl` 或沙箱的只读根 `boot.root.base` 使用。
正常导出的 `.img` 含名为 `image` 的 payload 和摘要 marker,不是可直接交给
`mount -t erofs` 的裸文件系统。平台 reader 负责解封装;直接挂载前须先提取
`image` payload(§2.5、§3.1)。

`flatten-ctl` 强调**确定性**——相同的不可变源、runtime config、工具版本与
构建设置应产出相同的 payload 字节,使后续 chunk dedup、manifest content-key
保持稳定。payload **自带 OCI runtime config**(末尾追加 ZIP),沙箱启动时无需
额外索引文件就能拿到 Entrypoint / Env / WorkingDir 等启动参数。

## 1. 概述

### 1.1 为什么要展平

容器镜像由多个 tar layer 叠加而成,容器运行时通常用 overlayfs / fuse-overlay
等机制合并。沙箱镜像路径选择**离线合并成一个 EROFS payload**:

- Guest 经 vhost-user-blk 使用一个只读 base,无需再组装原容器层栈。可写沙箱
  仍可在 Guest 内用 overlayfs 合并该 base 与 ext4 upper。
- 多个 sandbox 可经 Host 数据/缓存路径共享不可变 backing 数据。实际共享量取决于
  数据路径与工作负载;不能仅凭展平就保证某个内存密度收益。
- EROFS 只读且不可变,与 manifest 的 chunk / content key 寻址一致。

### 1.2 输入与输出

输入,三选一:

- **远程 registry 镜像**——镜像引用如 `nginx:1.27`、`gcr.io/ns/app@sha256:...`;
  `flatten-ctl` 直接拉取并展平(§2.3),拉取凭据经 `FLATTEN_REGISTRY_*` 环境变量注入,
  显式 OCI-layout 缓存可跨进程共享；未设置 `cache.dir` 时层直接流式输入；
- **Docker archive**(`docker save` 产物的 tar 流,stdin 或本地文件);
- **已展平的 rootfs 目录**——位置参数是一个本地目录时,Go 规划器直接就地读取
  该目录出图,不复制 staging 树、不修改源树(§2.1 的目录源小节);仍会读取数据并写输出。

(OCI layout 目录暂不直读,见 §5.5。)

输出:**单个 tarstream 工件**,内含 `image` payload(`EROFS + 末尾 ZIP`)及
平台摘要 marker(§3)。ZIP 存放 OCI runtime config 投影。registry 与 docker-archive
两条层源汇入同一 sink;相同的未压缩层和 runtime config 在相同工具/设置下产出
相同的 EROFS payload。目录源也要求源树与配置不变;mtime 在 Go 规划器中固定为零,不触碰源树。整个工件的复现还须包含权威稀疏图和容器编码条件。

展平本身不签名、不加密、不做内容寻址。可选 `--upload` 调用下一阶段的 manifest
ingest,完成加密/去重,见[manifest_zh.md](https://github.com/kuasar-sandbox/accelerator/blob/main/docs/manifest_zh.md)。
manifest 完整性与加密不能直接当作镜像签名校验流程。

### 1.3 在系统中的位置

```mermaid
flowchart TD
  R["Registry 镜像"] --> F["flatten-ctl: 拉取并合并层"]
  A["docker save 归档"] --> F
  D["Rootfs 目录"] --> E["go-erofs 与 config ZIP"]
  F --> E
  E --> T["tarstream 镜像工件"]
  T --> M["manifest-ctl store: 分块、加密、去重"]
  T --> S["sandbox-ctl: file:// base"]
  M --> B["sandbox-ctl: manifest:// base"]
```

## 2. 命令行接口

CLI 不提供 `flatten-ctl verify` 双次导出比较命令. 确定性通过单元测试和可复现的 Manifest key 检查.

七个子命令:`export`(展平为镜像工件,可选直接入库)、`referer`(OCI Referrers
lookup/put 原子操作)、`info`(检视镜像工件 / `manifest://` 引用)、`cache`
(检视/回收本地拉取缓存)、`config`(输出/校验 flatten 配置)、`tar`(通用 tar 提取/封装)、`mountpoint`(自 bind 造挂载点,
guest 内导出配套)。

| 子命令 | 用途 |
|--------|------|
| `export` | registry 镜像 / docker-archive / rootfs 目录 → 确定性 EROFS 的 **tarstream 镜像工件**(条目 `image`,约定后缀 `.img`);`--upload` 时顺带 ingest 进 store 并打印 manifest key |
| `referer` | `lookup` 查询源镜像是否已有可复用 manifest id;`put` 将宿主上传得到的 manifest id 回写到源 repo 的 OCI Referrers(§2.3) |
| `info` | 读镜像工件(经信封)的 EROFS superblock + 末尾 ZIP 里的 OCI runtime config 并打印 |
| `cache` | `cache info` 看缓存占用、`cache gc` 按 LRU 回收到上限(§2.4) |
| `config` | 输出规范化的 flatten 配置(`--config`/`FLATTEN_CONFIG`,加载即校验),或 `--template` 骨架;`-o <file>` 写文件(默认 stdout) |
| `tar` | 通用 tar 提取(全路径声明洞精确)与单文件封装(§2.5) |
| `mountpoint` | `mountpoint <dir>`:MkdirAll + 自 bind,使 `<dir>` 成为挂载点 → `export --skip-mounts` 自动排除之;guest 内导出自身 rootfs 时作 tmpdir/输出落点(防自吞,Linux only) |

`export` 的输入是**位置参数**。先识别本地目录;目录源拒绝 registry/archive 强制
标志。非目录源默认按下列规则判别:

```text
1. 省略或 "-"                → stdin docker-archive
2. "docker-archive:<path>"   → 本地归档文件(剥前缀)
3. os.Stat 命中本地文件       → 本地 docker-archive(./app.tar、/abs/x.tar)
4. 否则                     → 按 name.ParseReference 解析远程引用
```

歧义(本地文件名恰好形如 `repo:tag`)用 `--registry` / `--archive` 强制。`info` 的位置
参数必填,接受镜像工件路径或 `manifest://<hex>`。位置参数须置于 flags 之后(Go stdlib
flag 在首个非 flag 实参处停止解析)。

registry/archive 展平保留属主与权限位(§4.3),需要 root 或 `CAP_CHOWN`,启动时即
预检。目录导出不 chown 源树,只要求足够的读取权限;导出完整 rootfs 通常仍须 root。
`referer`/`info`/`cache`/`config` 不需要特权;`mountpoint` 执行挂载,需要相应权限。

进度与诊断走 stderr,stdout 承载请求的工件流、manifest key 或 `--print-digest`
的 digest。脚本应分开这些输出模式:当前 `--upload --output -` 会在工件之后追加
manifest key,而 `--upload --print-digest` 会输出多个值。捕获 key 时使用命名
`--output` 或不把工件输出到 stdout。`export`/`cache gc` 经 `--no-progress` 关闭进度。
流式导出按准备阶段打印 `flatten: ...`，上传打印 `upload: ...`（百分比和速率）；
字节型进度按 2 秒节流。完整下载入口 `remote.Pull` 仍保留独立的层计数进度回调。

目录导出在规划或写入进度之前，按打开文件的身份排除 stdout、stderr 对应的普通文件及源内别名，无需通过 procfs 解析路径；管道仍直接流式输出。显式输出路径、scratch 和这些重定向文件共用规划、读取及最终校验的同一选集。

### 2.1 `flatten-ctl export`

```
flatten-ctl export [flags] <ref|path|->

  <ref|path|->            registry 镜像引用、docker-archive 或 rootfs 目录(省略/`-` = stdin)
  --output <path|->       镜像工件输出路径(tarstream,条目 image,约定 .img);
                          `-` = stdout(终端拒写)。--upload 关闭时必填,
                          --upload 开启时可省(产物默认丢弃,只要 manifest key)
  --upload                展平后把 EROFS ingest 进 store,stdout 打印 manifest key
  --manifest-config <p>   manifest 配置 YAML(覆盖 MANIFEST_CONFIG env);--upload 必需
  --config <p>            flatten 配置 YAML(覆盖 FLATTEN_CONFIG env):tmpdir/platform/
                          tls/cache/referer(详见 §2.3)
  --platform <os/arch>    覆盖配置里的拉取 platform(os/arch[/variant])
  --tmpdir <dir>          覆盖配置 tmpdir(自动创建)。guest 内导出自身 rootfs 时
                          必须指向 mountpoint 子命令造出的挂载点(连同 mkfs 临时
                          文件一起被 --skip-mounts 排除,防自吞)
  --insecure              registry 源:允许 plain HTTP;TLS 证书校验由配置中的 tls.* 独立控制
  --no-progress           禁用 stderr 进度输出

  # 远程 registry 源(详见 §2.3)
  --print-digest          stdout 打印解析后的源镜像 digest(repo@sha256:..);与 --output - 互斥
  --registry / --archive  强制把位置参数当 registry 引用 / 本地文件(消歧)
  # rootfs 目录源(位置参数是本地目录时;见下文)
  --skip <rel>            排除 rootfs 内的该路径(节点连同子树,mkfs --exclude-path 语义);可重复
  --skip-mounts           排除严格位于 rootfs 之下的全部挂载点(/proc/self/mountinfo)
  --runtime-config <p>    追加的 runtime config JSON:OCI image config 或已投影的
                          config.json(按顶层键自动识别);不给则空配置
```

典型用法:

```bash
# 远程 registry 镜像 → 单文件镜像工件
flatten-ctl export --output nginx.img nginx:1.27

# 远程镜像直接入库(stdout 即 manifest key);凭据走环境变量
export FLATTEN_REGISTRY_USERNAME=robot FLATTEN_REGISTRY_PASSWORD=…
flatten-ctl export --upload --manifest-config manifest.yaml --config flatten.yaml \
    registry.example.com/team/app@sha256:… > app.key

# docker save 管道 → 单文件输出(省略位置参数 = stdin)
docker save myapp:v1 | flatten-ctl export --output my-app.img

# 本地 docker-archive 文件(位置参数在 flags 之后)
flatten-ctl export --output my-app.img ./my-app.tar

# Referrers 原子流程:lookup 命中则宿主直接复用 manifest id;miss 后由宿主 export+store,
# 再把得到的 manifest id 回写
flatten-ctl referer lookup --json --owner "$OWNER" --config flatten.yaml \
    registry.example.com/team/app:v1
flatten-ctl export --output app.img --config flatten.yaml registry.example.com/team/app:v1
APP_KEY=$(manifest-ctl store --manifest-config manifest.yaml app.img)
flatten-ctl referer put --owner "$OWNER" --manifest-id "$APP_KEY" --config flatten.yaml \
    registry.example.com/team/app@sha256:…

# /tmp 不够大时在 flatten.yaml 设 tmpdir: /var/tmp,再 --config flatten.yaml
flatten-ctl export --output big.img --config flatten.yaml ./big.tar
```

**rootfs 目录源**。位置参数 stat 为目录时走第三条源:Go 规划器**就地读取**
该目录——没有 staging 拷贝,源树永不被修改(时间戳在输出元数据中固定为零),保留属主只需要对树的读权限(导出完整 rootfs 用 root 跑)。被
`--skip` / `--skip-mounts` 排除的路径**节点整体消失**(目录本身也不在镜像里);
调用者必须在导出前停止所有写入活动。共同选集排除自有 scratch、计划、pending 输出、
目标路径及已发现的别名，规划、读取和最终校验始终使用此选集。导出 Guest 根目录时，
可将 scratch 和输出放在被排除的挂载点。
导出 `/` 必须带 `--skip-mounts`(否则会走读 /proc、/sys)。registry 专属 flags(`--platform`/`--print-digest`/
`--registry`/`--archive`)与目录源互斥。

```bash
# 把机器/guest 的根做成镜像;先建立被排除的 scratch/输出挂载点(需挂载权限)
flatten-ctl mountpoint /run/flatten-export
flatten-ctl export --skip-mounts --tmpdir /run/flatten-export \
    --runtime-config config.json --output /run/flatten-export/host.img /

# 从准备好的 rootfs 目录出图,剔除缓存目录;config 直接复用旧镜像里的投影
flatten-ctl info --json old.img | jq .config > rc.json
flatten-ctl export --skip var/cache --runtime-config rc.json -output new.img /srv/rootfs

# --upload 同样可用:目录 → EROFS → store,stdout 打 manifest key
flatten-ctl export --skip-mounts --upload --manifest-config m.yaml /srv/rootfs
```

#### 临时文件与失败处理

默认 Registry 导出只保留一个私有 rootfs 和有界 Plan 元数据，直接向不可 seek 的
stdout 等 writer 输出现有工件，不保存整镜像压缩层、mkfs 私有数据 blob 或 raw
中间文件。scratch 父目录依次取 `--tmpdir`、配置 `tmpdir`、`$TMPDIR` 或 `/tmp`。

| 资源 | 生命周期与失败行为 |
|---|---|
| 私有 rootfs、Plan/layout 和 FD | 由 `PreparedImage` 持有到输出结束；Close 等待工作退出再释放。半应用失败使整个 rootfs 失效，重试从固定输入、新 scratch 开始。 |
| Docker-save 输入 | 可 seek 文件原位建索引；不可 seek 输入只保留一份有界外层 spool，全部层校验后、规划前删除。不再单独解出内层 tar。 |
| 最终工件 P | 仅 stdout 时直接流式输出；上传/复读保留一份必要 P，成功或失败后尝试清理。ingest 成功且 key 写出后，后续清理失败通过 stderr 告警，不将已完成上传判为失败。 |
| 命名 `--output` | 先写同目录 pending 文件；writer Close、最终校验、prepared 资源释放和请求的上传均成功后才原子改名。提交前失败保留已有目标文件；rename 后空暂存目录清理失败通过 stderr 告警，不将已提交文件判为失败。 |
| 显式持久缓存 | 完成态 blob 不随任务删除。取消先等待预取退出，再删除本次 pending；GC 独立执行（§2.4）。 |
| 调用者目录、输入归档、无关文件及 scratch 父目录 | 不由任务资源清理删除。 |

新文件使用 `0666` 并受调用者 umask 约束；替换已有普通文件时保留 UID、GID、rwx 权限位及 POSIX 访问 ACL。无法保留这些属性时，已有文件保持不变。临时文件仍由调用者拥有时先恢复权限和 ACL，再设置目标 UID/GID；执行这一所有权转移时，已有 `CAP_CHOWN` 的调用者无需额外持有 `CAP_FOWNER`。输出 artifact 不保留 setuid/setgid 执行位或其他扩展属性。 原子替换还需要替换目录项的权限：父目录带粘滞位时，调用者必须拥有父目录或目标文件，或持有 `CAP_FOWNER`。检测到缺少该权限时，在镜像准备前报错并保留旧文件；仅能写入文件内容并不足够。目标文件自身若为挂载点，不能被原子替换；内核提供挂载 ID 时，在准备前拒绝该目标。挂载目录中的普通文件仍受支持。内核不提供挂载 ID 时，仍由最后的 rename 检查决定，不回退为原地覆盖。临时 artifact 位于私有目录，目录的所有者访问权限不受最终文件 umask 影响，并保留 setgid 目录对新文件 GID 的继承，包括调用者不属于该组且 umask 导致目录访问权限需要修复的情况；写入、复读和上传使用同一个预留文件描述符；提交前若发现临时对象被替换，则拒绝提交。命名目标必须是普通文件或尚不存在：设备、FIFO、目录及符号链接在准备前被拒绝，提交前再次检查。 Linux 6.1 上的特权导出可在没有 procfs 时恢复暂存目录访问权限。在该内核上，若非特权调用者的 umask 屏蔽目录访问权限，则需要 `/proc` 完成描述符回退；不可用时导出失败并保留已有输出。

无法确认暂存目录项为本次私有对象时，导出失败并保留该目录项，即使它是空目录；清理不得删除归属未经确认的对象。

首次 SIGINT/SIGTERM 请求取消；stdout 持续阻塞时，后续信号按正常信号行为终止进程。强制终止无法执行延迟清理。

取消会关闭拥有的输入 body，等待 applier 和规划/输出工作真正结束再清理。stdout
断管走同一错误返回和清理路径。SIGKILL 无法执行清理；这里不提供全局遗留文件
清扫器。`--upload --output -` 仍在工件后追加 manifest key。

生命周期回归使用实际 Go 生成器，并按调用注入 writer/Close/upload 故障：

```bash
GOFLAGS=-p=2 GOMAXPROCS=2 go test -race ./cmd/flatten-ctl
```

覆盖原子输出、规划预算失败、ingest 前资源释放、用户文件保留、断管及 raw 与
artifact 入口的一致性；无需安装 mkfs。独立 fsck、内核挂载和实际 Guest 内容/元数据
验证沿用[既有 E2E 入口](../test/e2e/README.md)。

### 2.2 `flatten-ctl info` — 检视镜像

```
flatten-ctl info [--json] [--manifest-config <path>] <path|manifest://hex>

  <path|manifest://hex>          本地镜像工件路径,或 manifest://<hex>(位置参数,必填)
  --json                         机器可读 JSON 输出(默认人类可读)
  --manifest-config <path>       manifest 配置 YAML;`manifest://` 输入必需,也可由 MANIFEST_CONFIG 提供
```

先解封本地 tarstream 工件或打开 manifest stream,再读 payload 的 EROFS superblock
取得 image size,从末尾 ZIP 解出 OCI runtime config 并打印。`manifest://` 输入经 cache-ctl/store-ctl 的 fetch 路径按 chunk 粒度只读
superblock 与尾部 ZIP,不取回整个镜像。

人类可读输出示例:

```
$ flatten-ctl info my-app.img
EROFS image size:  1.2 GiB (1287651328 bytes)
Architecture:      amd64
Os:                linux
User:              app
Entrypoint:        ["/usr/bin/foo"]
Cmd:               ["--config","/etc/foo.yaml"]
WorkingDir:        /app
Env (3):
  PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
  HOME=/root
  LANG=C.UTF-8
ExposedPorts:      8080/tcp
Labels:
  com.example.version=1.2.3
```

JSON 输出示例:

```json
{
  "erofs_size": 1287651328,
  "config": {
    "Architecture": "amd64",
    "Os": "linux",
    "User": "app",
    "Env": ["PATH=...", "HOME=/root", "LANG=C.UTF-8"],
    "Entrypoint": ["/usr/bin/foo"],
    "Cmd": ["--config", "/etc/foo.yaml"],
    "WorkingDir": "/app",
    "ExposedPorts": {"8080/tcp": {}},
    "Labels": {"com.example.version": "1.2.3"}
  }
}
```

payload 无 ZIP trailer 时,`config` 为 `null`,人类模式打印
`(no OCI config trailer)`。旧裸 EROFS 文件仍需平台信封才能走当前本地 `info` 路径。

辅助管道:

```bash
# 经平台工件信封读取 config.json 投影
flatten-ctl info --json my-app.img | jq .config

# 或者经 flatten-ctl info --json 进 jq
flatten-ctl info --json my-app.img | jq '.config.Entrypoint'
```

直接使用 ZIP 工具时,先提取原始 `image` payload(§3.1)。

### 2.3 远程拉取、本地缓存与 Referrers 回写

位置参数判定为 registry 引用时(§2 判别规则),`flatten-ctl` 经 [go-containerregistry]
直接拉取、展平,无需先 `docker save`。所有 registry 行为由 **`--config` YAML**
(或 `FLATTEN_CONFIG` env)配置,密钥不入文件:

```yaml
tmpdir: ""                     # 每次运行临时目录的父目录(空 = $TMPDIR 或 /tmp)
platform: linux/amd64          # 空 = 跟随宿主架构(linux/$GOARCH);--platform 覆盖
insecure: false                # 允许走纯 HTTP 的 registry(dev/私有);不影响 TLS 证书校验
pull_jobs: 4                   # 既有 remote.Pull 完整下载入口的并发数
tls:                           # HTTPS 证书校验(registry 与 CDN blob 重定向均生效,见下)
  ca_cert: ""                  # 追加信任的 CA bundle 路径(PEM,可含多证书)
  insecure_skip_verify: false  # 完全关闭证书校验(不安全;优先用 ca_cert)
cache:
  dir: ""                      # OCI-layout 持久缓存根;空 = 直接流式输入
  in_flight_bytes: 256MiB      # pending 压缩字节预算，当前层优先
  required: false             # 超预算层默认 bypass；true 则报错
  max_size: 10GiB              # 上限,超出按 LRU 回收;"0" = 不限,仅手动 cache gc
referer:
  validity: 720h               # 可选;referer put 写入 valid_at 的过期段
```

artifact_type 固定为常量 `application/vnd.kuasar.flatten-manifest.v1`(不可配置,保证跨工具/版本一致)。

**凭据(命名空间环境变量,匿名回落)**:`FLATTEN_REGISTRY_TOKEN`(Bearer,优先)或
`FLATTEN_REGISTRY_USERNAME` + `FLATTEN_REGISTRY_PASSWORD`(Basic);都不设则匿名拉公有
镜像。密钥只走 env(不上 argv、不入配置文件),契合 orchestrator 经 exec env 把租户拉取凭据
下发进构建沙箱的模型([deployment_zh.md](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/deployment_zh.md) §5)。

**TLS(`tls.*`)**:作用于全部 HTTPS 请求——既包括 registry API,也包括层 blob 的 CDN
重定向(拦截式代理会用私有 CA 重签这些证书,系统信任库默认拒绝)。`ca_cert` 把额外的
PEM CA bundle 追加进系统信任库,是信任此类私有根 CA 的正路;`insecure_skip_verify`
整体关闭证书校验,仅作 CA 不可得时的逃生口。与 `insecure` 正交:后者只决定 registry
是否可走纯 HTTP,不影响 TLS 校验。

**本地缓存**：标准 OCI layout。只有显式 `cache.dir` 才暂存压缩候选层。当前层先
取得预算，下一层完整 descriptor 能装入剩余字节预算时才预取；超预算层直接 stream
bypass，`cache.required=true` 则拒绝。EOF 不发布命中：完整压缩 size/digest、解压
DiffID/trailer 和 Close 全部成功才提交；命中仍在应用时重验。重复写入保留已有 inode。
读写 lease 防止 GC 删除活动数据；超过宽限期的遗留 pending 可回收。完成态缓存不随
任务删除；`max_size` 是 LRU 目标，不是即时文件系统配额。

**多架构**:registry 引用常指 manifest index,按 `platform` 选出具体 image 再展平;
`Architecture`/`Os` 仍原样投影进 config.json(§3.2),不做校验。

**确定性提醒**:tag 可变,`:latest` 不可复现;可复现构建请钉 `@sha256:`。`export` 把
解析到的 `repo@sha256:..` 打到 stderr(`--no-progress` 关闭),`--print-digest` 另打到
stdout。digest 固定源字节;完整输出复现还需固定配置、工具链与构建设置(§3.3)。tag 的稳定性取决于当前指向。

#### Referrers 原子操作(`referer lookup` / `referer put`)

展平产出的 manifest id(= `--upload` 入库的 manifest 内容键)可经 **OCI Referrers API**
回写到**源镜像所在 repo**,作为 registry 侧、按 owner 作用域的去重备忘。`flatten-ctl`
只暴露 lookup/put 原子能力;是否跳过 export、何时 upload、writeback 失败是否中止,由
调用方(如 `node-ctl run-builder`)编排。

```
flatten-ctl referer lookup --json --owner <owner-token> [--config <p>] [--platform <p>] [--insecure] <ref>
flatten-ctl referer put --owner <owner-token> --manifest-id <64hex> [--validity <dur>] [--config <p>] [--insecure] <subject>
```

`lookup` 解析源 → 平台镜像 digest `D`,直接探测 OCI 1.1 Referrers API。输出:

```json
{"conversion_semantics":"umoci-go-erofs-no-xattr-v1","supported":true,"subject":"repo/app@sha256:...","hit":true,"manifest_id":"..."}
```

- `supported=false`:registry 不支持 Referrers API,调用方可按策略 fallback 或失败;
- `supported=true, hit=false`:支持但没有 owner 匹配且格式有效、尚未过期的记录,
  调用方继续 `export` 并在宿主侧上传;
- `supported=true, hit=true`:返回的 `manifest_id` 可由宿主校验后直接复用,无需拉取/展平。

Lookup/writeback 使用实际生成器的转换政策。缺失或不匹配的 `conversion` 注解按
miss 处理；原始层缓存和显式旧工件仍可读取。Builder 向 lookup/export/put 传递
`--conversion-semantics`，旧或不兼容 Guest 工具会失败，不能把旧转换标成当前政策。
该版本不进入制品身份，并独立于内部 Plan schema。

`lookup` 严格校验 `valid_at`。缺失或格式错误的时间、未来的 import 时间、已经到期
或早于 import 的 expiry 都作为 miss 跳过;存在多条有效记录时返回 import 时间最新者。

`put` 构造 referrer artifact(OCI image manifest:subject=`D`、artifact type 经 config
media type 承载、注解 `owner`/`id`/`valid_at`)并推回源 repo。`put` 需要源 repo push
权限;失败由调用方决定是否使构建失败。

referrer 注解:

```
vnd.kuasar.flatten-manifest.owner    = <hmac-hex> <referer_desc>
vnd.kuasar.flatten-manifest.id       = <manifest_id>                        # ingest 产出的内容键
vnd.kuasar.flatten-manifest.conversion = umoci-go-erofs-no-xattr-v1
vnd.kuasar.flatten-manifest.valid_at = <import RFC3339>[ <expiry RFC3339>]
```

其中 owner token 通常由宿主计算:`hmac = HMAC-SHA256(key = 客户秘钥 MANIFEST_KEY,
msg = referer.key)`,注解值为 `<hmac-hex> <referer_desc>`。Guest 的 `referer` 命令只接收
`--owner`,lookup/put 操作不需要也不应接收 `MANIFEST_KEY`。

**前提与注意**:OCI 规范要求 referrer 与 subject 同 repo → `referer put` 需对**源
repo 有 push 权限**(面向租户自有 registry;对只读上游写不进会失败)。referrer
artifact 含时间戳,本身每次不同,但展平产物 / manifest id 仍确定。对公开 base 镜像,
referrer(owner token / id / 时间)对能读该 repo 者可见——owner 经 HMAC、id 为不透明内容
键,但"某 owner 在某时刻展平过该镜像"这一事实会暴露。

[go-containerregistry]: https://github.com/google/go-containerregistry

### 2.4 `flatten-ctl cache` — 检视/回收拉取缓存

```
flatten-ctl cache info [--config <p>] [--cache-dir <D>]
flatten-ctl cache gc   [--config <p>] [--cache-dir <D>] [--cache-max-size <S>] [--no-progress]
```

`cache` 子命令面向**持久**缓存(`cache.dir` 显式配置时);默认流式 `export` 不生成整镜像压缩层缓存，无需 gc。`cache info` 打印缓存目录、blob 数、总占用与上限。`cache gc` 持 flock 按 LRU 回收到配置
(或 `--cache-max-size` 覆盖)的上限,`"0"` 清空(grace 期内的除外)。缓存目录默认取
`--config` 的 `cache.dir`,`--cache-dir` 覆盖。常驻场景可由 cron 周期跑 `cache gc`
执行回收(`export` 每次拉取后也会顺带回收)。grace 期与并发写入仍可能使占用
暂时超过目标,这不是即时文件系统配额。

### 2.5 `flatten-ctl tar` — tar 流提取与单文件稀疏流

tar 工具面,两个方向都是**纯 Go、零 tar 二进制依赖**:

```
flatten-ctl tar extract [-f tarfile] [--chown u:g] [--chmod 755] [--dense] [规则...]
flatten-ctl tar stream  [-f tarfile] [--size N] [tar内名[:源]]
```

稀疏语义铁律(全平台一致):**零值字节是数据、空洞是缺失,二者业务含义不同,
正常稀疏路径不互转**——洞只来自权威元数据(文件系统 SEEK_HOLE、tar sparse map),
绝不从内容零扫描推导。`--dense` 是明确请求将已声明洞物化成已分配零的例外。

**extract** 从任意 tar 流取文件,**全路径声明洞精确**:对输入单遍流式
(stdlib 解码,管道零落盘),常规文件致密落盘(数据段里的零保持已分配,不打洞);
**稀疏成员**(PAX sparse 1.0 经 `PAXRecords` 检测,老 GNU 'S' 经 typeflag)的还原
分三档:

- `-f` 文件输入:引擎按**条目序数**在第二个句柄上经 tarstream 重定位该成员,
  取回 stdlib reader 隐藏的洞图(相关 API 限制见 [Go issue 22735](https://github.com/golang/go/issues/22735)),
  只 punch 声明的洞;stdlib 侧 `Next()` Seek 跳过包体,数据不读两遍;
- stdin(单遍,图已被 stdlib 消费,且禁落盘):**硬错误**并引导——绝不静默把
  32 GiB 逻辑稀疏档致密化;
- `--dense`:显式整体关闭稀疏处理,回 stdlib 逻辑字节致密落盘(声明洞落为
  已分配零)。

便捷形态:**无规则 + `-f` 平台工件**(一个 payload + digest marker)自动洞精确解出 payload
(`tar extract -f image.img` 即可);**单条显式文件规则**(`成员[:目标文件]`,目标必须是文件路径,不能是 stdout、目录或归档根)
走 tarstream 视图,stdin 也能洞精确(成员实为目录等情形:`-f` 输入回退通用引擎,
stdin 报错引导)。`..` 成员跳过告警,穿 symlink 写出是硬错误;条目命中多条规则时
最具体者胜。规则 = `tar内路径[:外部路径]`:

| 形式 | 含义 |
|---|---|
| `p` | 内外同名 |
| `in:out` | 把条目 in 写到路径 out |
| `in:-` | 条目内容流向 stdout(至多一条;硬链接成员无数据体,输出为空) |
| `dir/` | 目录规则:dir 及其下全部内容 |
| `dir/:out[/]` | 目录前缀重命名 |
| `dir/:` | 解到当前目录 |
| `:dir/` | 整个归档根映射到 dir/ |

不给规则取全部到当前目录;`--chown/--chmod` 改写每个落盘条目的属主/权限;
`--no-chown` 跳过属主恢复。`--chown`
取 `uid:gid`:数字直用,**名字**则按解包目标根的 `/etc/passwd`/`/etc/group` 解析
(Docker `COPY --chown=name` 同款;CGO 关,os/user 直读文件不经 NSS);`user`(无组)
取该用户主组,纯数字 `1000` 镜像为 `1000:1000`。node-ctl 的 COPY step 即以
`extract --dense --chown` 把上下文 tar 摊进 guest rootfs(见 [按目标执行与发布](https://github.com/kuasar-sandbox/orchestrator/blob/main/docs/node-build_zh.md#5-按目标执行与发布))。

**stream** 把**一个文件**封装为 tarstream(一个稀疏 payload +
`.kuasar.digest.<hex>` marker metadata,`accelerator/pkg/tarstream`)。writer 在写 payload
时同步生成摘要,不二次读取。文件源的洞图来自文件系统元数据
(SEEK_HOLE),稀疏保真;stdin 源**必须给 `--size N`**(tar 头先含 size,这是
格式下界),全程直通、**零落盘**,按致密封装(一次性流没有权威洞元数据,
也不做内容探洞)。`--size` 仅限 stdin 源(文件长度以文件系统为准)。
参数 = `tar内名[:源]`:

| 形式 | 含义 |
|---|---|
| (缺省) 或 `-` | ≡ `-:-`:条目名 "-",内容来自 stdin |
| `path/to/file` | ≡ `file:path/to/file`(以 basename 命名) |
| `:源` | ≡ `-:源` |
| `名:-` | 条目名指定,内容来自 stdin |
| `名:源` | 全显式 |

产物本身是合法 tar:`extract`、GNU tar、`archive/tar` 都能读回;平台
`extract` 把 marker 当作保留元数据而不落盘。空洞只占 map 字节,不占流量。

```bash
# 稀疏快照盘 → tarstream → 异地还原(声明洞全程精确保持;16G 逻辑/100M 数据只传 ~100M)
flatten-ctl tar stream -f snap.tar /var/lib/sandbox/disks/overlay.img
flatten-ctl tar extract -f snap.tar "overlay.img:/restore/overlay.img"

# 管道对管道:生成器 → tarstream → 提取(stdin 源给定长度,全程零落盘)
gen-disk | flatten-ctl tar stream --size $((16<<20)) disk.img:- | flatten-ctl tar extract disk.img:-

# 程序化读写(含洞图精确取回、tar 内随机访问)见 accelerator/pkg/tarstream
```

编程接口:`accelerator/pkg/tarstream` 的 `WriteTo`(吃任意 `sparse.Source`,返回
写入时生成的摘要)/`ReadFrom`/`ReadSeekFrom`(洞图精确往返、tar 内零拷贝随机
访问)/`SourceFrom`(tar 流直接开成 `sparse.Source`,可直通 manifest ingest
等管线消费者)/`SourceAt`(随机访问,完整平台工件通过可选 `Digester` 暴露 marker),
本仓与各下游仓均可 import。

## 3. 镜像格式

### 3.1 字节布局

外层 `.img` 是 tarstream 工件,含一个 `image` payload 和 `.kuasar.digest.<hex>`
marker。以下偏移均相对于**解封后的 payload**:

| payload 偏移 | 内容 |
|---|---|
| `[0, erofs_end)` | 自描述 EROFS 文件系统;偏移 1024 的 superblock 给出 image size。 |
| `[erofs_end, payload EOF)` | STORED 无压缩 ZIP,含 OCI runtime config 投影 `config.json`。 |

两段不改变彼此的格式:

- **EROFS reader**:在 payload 偏移 1024 读 superblock,用 `blocks << blkszbits`
  确定文件系统边界,不把尾部 ZIP 当文件系统内容。平台 vhost-user-blk reader 先
  提供 payload 视图;直接 `mount -t erofs` 必须使用提取后的 payload,不能传外层 tar。
- **ZIP reader**:从 payload EOF 反向扫描 EOCD;ZIP 内部 offset 相对于 ZIP 自身
  起点,支持前缀数据的 reader 会处理 EROFS 前缀。直接 ZIP 工具也应接收提取后的 payload。

`erofs_end = blocks × (1 << blkszbits)`,由 superblock 解析。EROFS 有明确的磁盘
字节序,支持跨 Host 架构构建;Guest 仍需支持所用 EROFS 特性,应用二进制仍需匹配执行架构。

```bash
# 使用直接文件系统/ZIP 工具前,先解封工件
flatten-ctl tar extract -f my-app.img image:my-app.erofs
unzip -p my-app.erofs config.json
```

### 3.2 config.json schema

ZIP 内 `config.json` 是 OCI image config 的运行时相关投影。**只保留启动需要
的字段**,跳过 `created` / `author` / `history` / `rootfs.diff_ids` 等会破坏
跨次确定性的元数据。

字段集合(运行时投影 schema):

```
Architecture     string                 (passthrough,例如 "amd64" / "arm64")
Os               string                 (passthrough,例如 "linux")
User             string,omitempty
Env              []string,omitempty
Entrypoint       []string,omitempty
Cmd              []string,omitempty
WorkingDir       string,omitempty
ExposedPorts     map[string]struct{},omitempty
Volumes          map[string]struct{},omitempty
StopSignal       string,omitempty
Labels           map[string]string,omitempty
Healthcheck      *Healthcheck,omitempty   (Test/Interval/Timeout/StartPeriod/Retries,
                                            Duration 字段是 OCI 原生 nanoseconds int64)
```

**为什么 Architecture / Os 不 omitempty**:即使空值也要写入,让下游观察者
看到"源镜像没填这两项"的问题,而不是默默继承默认值。flatten-ctl 是字节级
转换工具,不验证平台兼容(amd64 vs arm64 决策属于上层调度器)。

未列出的源字段(`OnBuild` / `ArgsEscaped` / `Domainname` / `Hostname` /
`AttachStdin` / `Tty` / `MacAddress` 等)被静默丢弃——不在沙箱启动模型中
使用。

### 3.3 跨次确定性来源

ZIP 部分:

- 单 entry,文件名固定 `config.json`
- 无压缩(stored,内容直接可见)
- 修改时间固定为纪元常量 `1980-01-01 00:00 UTC`,绝不用挂钟
- 确定性 JSON 序列化:map keys 按字典序、slice 顺序保留(Env/Cmd/Entrypoint
  语义需要)、零值字段抑制

EROFS 部分见 §4.3 元数据归一化。

相等的运行时投影产生相同的 JSON 与 ZIP 字节。**仅 config 相同不能推出整镜像
hash 相同**:rootfs 数据/元数据、构建工具/参数,以及工件稀疏图和容器编码也须
一致。比较 payload hash 或 manifest key 时应保存这些输入。

### 3.4 沙箱怎么用 config.json

`sandbox-ctl run` 在启动前经 boot.root.base 的 payload 视图读尾部 ZIP,拿到运行时投影作为
LaunchSpec 的 fallback:`sandbox.yaml` `launch.*` 字段优先,`Env` 取镜像在下、
override 在上的合并,`Volumes` 并入 `mounts`。因此 image config 有 Entrypoint/Cmd
时 `launch.exec` 即可省略。合并规则的权威定义见
[宿主镜像默认配置合并](https://github.com/kuasar-sandbox/sandboxer/blob/main/pkg/sandbox/imageconf.go)。

## 4. 算法

### 4.1 输入与层生命周期

`remote.Stream` 在打开层之前固定平台 manifest、config、descriptor 和对应 DiffID。
每次 `LayerInput.Open` 必须为同一不可变身份返回从零开始的新 body；prepared 操作
拥有它，精确关闭一次并检查 Close 错误。gzip/zstd/plain 由明确媒体类型决定。
tar EOF 不等于流结束：有界尾部读取覆盖完整未压缩 DiffID、压缩 size/digest 和
decoder trailer。错误、超限或取消停止输入，不无限 drain。全部层成功前不输出工件。
`remote.Pull` 保留完整下载、可重开的 Source 合同；旧 `Build(Source, ...)` 仍接受
未压缩、可重开的层。

`PrepareArchive` 校验外层成员、offset/length、结束标志及索引预算，按 manifest
顺序开启层。可 seek 输入原位读取，管道只 spool 一次。`PrepareImage`、
`PrepareArchive`、`PrepareDir` 返回的 `PreparedImage` 持有选定源、layout、计划和 FD。
旧 `Flatten` 借用 reader；`PrepareArchive` 拥有并关闭传入 body。
选定的 config 和层必须是普通成员。既有 Skopeo 旧格式别名
`<hex>/layer.tar -> ../<diff-id>.tar` 仅在目标为选定普通层时接受，只建立索引，
不跟随或提取该链接。其他外层链接和 sparse 成员均拒绝。

### 4.2 OCI 应用与稳定源

Linux 适配调用 **umoci v0.6.0** 层 API，不引入其 CLI、CAS 或 runtime bundle 生成。
保留数值 UID/GID、mode、普通目录和类型替换、implicit parent、硬链接身份及特殊文件。
迟到的 `.wh.<name>`、`.wh..wh..opq` 只影响下层条目，不删除同层新增条目。
权限不足明确失败。受支持的层 xattr 在私有 rootfs 中应用并核对；最终输出政策见 §4.3。
半应用树整体废弃，不原地重放同层模拟回滚。

私有 rootfs 在层应用后停止写入。已有目录须由调用者在准备和输出全程保持稳定；
选集和摘要只能检测变化，不能把并发目录变成原子快照。规划、读取和最终校验共享
同一选集，包括 `Skip`、`SkipMounts` 及全部自有 scratch/spool/layout/输出排除。
读取产生的 atime 和排除区域内部变化不导致失效；纳入选集的元数据、链接、对象及
目录成员变化会导致失效。

### 4.3 布局与元数据政策

`accelerator` 使用固定版本的外部模块 `github.com/kuasar-sandbox/go-erofs`，复用其
目录、inode/NID、硬链接、superblock、块分配和紧凑单设备 chunk 索引。调用方沿共同
选集按确定顺序扫描，以约 1 MiB 缓冲逐 4 KiB 计算完整 SHA-256 去重，并顺序写出
数据来源记录。库只规划并输出结构元数据，不暂存 payload。raw 与 artifact 共用
同一 KERPLAN v1 布局；新旧生成器不要求字节相同，新生成器自身保持确定性。

| 字段 | 处理 |
|---|---|
| 时间与 UUID | 零时间与全零 UUID 固定输出身份，不修改调用者目录。 |
| UID/GID/mode | 保留源的数值属主和权限位；层应用权限不足失败。 |
| inode、遍历、硬链接 | 共同选集的确定性排序和真实共享 inode；内容去重独立进行。 |
| xattr | 最终 EROFS **不输出 xattr、file capability、ACL 或安全标签**，即使它们已成功应用到私有 rootfs。 |
| 压缩与设备 | 不压缩、单设备、4 KiB chunk，无隐式 raw/data-blob fallback。 |

内部 Plan schema/能力、`image.ConversionSemantics` 和制品身份分别管理。Plan 声明
逻辑长度、源身份/路径、有序目标和源 offset、length、SHA-256 及 padding。Reader
拒绝未知能力、溢出、数量/字节超限、缺源/截断、重叠和越界。

库的 `LayerLimits.MetadataBytes` 和 `PlanLimits` 字段是可选准入配额。零值不再
额外限制层元数据、规划、落盘 Plan 或索引；整数/分配表示范围及格式边界检查仍然
生效。CLI 使用这些零值选项，因此增加 Builder 资源不再被这些固定的层/计划配额
抵消，不需要新的 CLI 参数或 Builder API。执行资源仍是外层内存/存储边界；独立
调用方必须约束自己的执行环境。这不保证任意大的输入都能完成。

显式正值保留原有保守计费。兼容字段名 `NativeMemoryBytes` 限制 Go 规划器的受控
分配，分给库和调用方去重/硬链接表，各占一半；扩容计入新旧存储并存。
`PlanBytes` 限制计划文件及结构元数据；`IndexMemoryBytes` 保留选集、reader 和
writer 的份额。原先的一百万来源上限改为可寻址来源切片边界。这些估算不是进程
RSS 硬上限；GC、栈及分配器残留还须计量。解码器、归档、路径、摘要和完成条件
检查仍然生效。

### 4.4 顺序输出与接收条件

规划完成后释放对构建索引的引用，再开始 emit；不强制全局 GC，也不承诺内存立即归还系统。layout 中计划 payload 尚未物化的洞标为
**Data**；`RunAt` 是可重复的纯元数据查询，不消费游标、不扫描零字节推断洞。
每个 payload 块进入 writer 前核验计划摘要。现有 runtime-config 投影、tailzip 和
加密 writer 保持不变。

可选 tarstream 最终校验钩子在 payload 完成后、carrier digest marker、完整尾和
加密终结记录之前执行，空源和全 Hole/Zero 源也执行。Prepared 输出用它重验选集和
Plan/layout 身份。接收方必须同时要求完整工件身份/尾以及 producer/ingester 成功，
才能 Ready 或写回转换结果；仅收到 payload 字节不表示成功。

显式 `WriteRaw` 使用同一 Plan；正常 CLI 不生成 raw 暂存。命名 raw 和 artifact
均使用 pending 文件原子提交。rename 是提交点，发生在输出/文件关闭、源校验及源资源
清理成功之后。后续空暂存目录清理不能撤销提交：CLI 向 stderr 告警；raw 文件 API 在
提供 `Options.CleanupWarning` 时同步调用它（nil 时静默），已提交文件仍返回成功。
`Close` 取消并等待活动工作，然后释放拥有的资源；
调用者提供的任意输出 writer 须自行让阻塞的 Write 返回。

`make flatten-ctl` 构建单个静态 Go 可执行文件，运行时不查找 mkfs，
`MKFS_EROFS_PATH` 不再选择生产生成器。`make build` 使用宿主预安装的 erofs-utils
打包 runtime bundle。项目不再编译 mkfs 或维护其补丁；独立 fsck/dump 校验使用
宿主工具，历史 native 性能对照保留其原始源码版本。
没有隐式旧生成器或高空间 fallback。回退应恢复上一组源码/制品及对应转换语义版本。

## 5. 设计决策

### 5.1 为什么是 EROFS 而非 ext4 / squashfs

- **只读、不可变 base**:EROFS 适合内容寻址 chunk 和可复用 backing 数据。
- **单一 payload**:适合单工件传递、manifest 上传和块设备访问；CLI 从稳定源和
  Plan 顺序输出工件，不需要完整 raw 临时文件。
- **可移植磁盘格式**:可跨 Host 架构构建;应用仍须匹配实际执行架构。
- **squashfs 对比**:平台选择不压缩、chunk 布局的 EROFS 以配合数据/去重路径。
  squashfs 也可共享不可变 backing 数据,这里没有普适的密度劣势证据。
- **ext4 对比**:平台用 ext4 提供可写 upper。可变文件系统跨 sandbox 使用需要
  隔离/COW 策略,并有 fsck 等维护事项。这说明当前 EROFS base / ext4 upper
  的职责划分,不能当成所有方案之间的密度基准结论。

### 5.2 为什么离线合并而非运行时 overlayfs

离线合并省去每个 sandbox 单独组装原始容器层栈的步骤。平台经数据路径提供
一个不可变 base;可写沙箱仍可在 Guest 内挂 overlayfs。此方案并不是把一个
Host EROFS mount 当作多个 Guest 的挂载,本文也没有据此建立 Host mount 数量
与密度上限的定量关系。

代价是源镜像更新时多一次构建。镜像构建可在 CI/CD 流水线中完成一次,供多个
sandbox 复用,摊销该成本。

### 5.3 为什么 config.json 内嵌而不是边车文件

外加 `app.erofs.config.json` 边车文件的方案有两个问题:

- **拷贝 / 上传分裂**:用户必须记住"两个文件一对",`scp` 漏一个就坏
- **manifest:// 无副车机制**:manifest 是单一 reader,边车文件需要单独的
  manifest key + 单独的拉取流程

ZIP-at-end 使单个交付工件里的文件系统 payload 自描述。`manifest-ctl store`
可一次 ingest 完整内容,沙箱从同一 payload 读取启动配置。EROFS 自描述边界与
ZIP 的 EOCD 反向扫描允许两种内部格式共存;外层 tarstream 提供平台工件信封。

### 5.4 为什么投影而不是原样转发

OCI image config 字段繁多,大量与启动无关:`created` / `author` / `history`
是构建元数据,跨实例不同;`rootfs.diff_ids` 是层 hash,展平后不再有意义;
`OnBuild` 是构建指令而不是运行时设置。原样转发会:

- 破坏跨次确定性(timestamps 漂移)
- 让 manifest dedup 无谓波动(每次构建的 history 不同)
- 混淆构建指令与运行时配置的边界

显式投影把"运行时启动需要什么"作为唯一标准,下游可预测。

### 5.5 不实现 / 暂不实现的功能

- **任意 OCI layout 目录直读**:支持 registry 拉取(§2.3),缓存也使用 OCI layout,
  但不把任意该格式目录解码为 OCI 镜像。可先用
  `skopeo copy oci:./dir docker-archive:x.tar` 转换,或用 crane 推到 registry 后拉取。
  直接传普通本地目录会选择 rootfs 导出,不会识别 OCI layout。
- **展平阶段的镜像签名/加密**:加密与内容寻址由
  [manifest 阶段](https://github.com/kuasar-sandbox/accelerator/blob/main/docs/manifest_zh.md)
  负责;这不构成镜像签名校验流程。
- **层级保留**:输出把各层合并为一个 EROFS payload。manifest chunk dedup 可复用
  不变内容,但不保留原始层结构。

## 6. 性能特征

构建工作量通常随输入字节数和文件数增长,主要成本包括下载、层解压、文件系统
条目应用与 Go EROFS 规划。缓存状态、元数据分布、存储、CPU 和工具版本均影响结果。
追加单个无压缩 ZIP entry 相对典型镜像构建较小,但仓库没有建立普适的
1 GiB/3–5 秒或 ZIP 小于 10 ms 保证。

确定性自检:用相同配置/工具链对不可变输入导出两次,按目标比较 payload/工件
hash,或复现 manifest key。单元测试覆盖确定性转换;`verify` 子命令已移除(§2)。

`flatten-ctl info` 不解压或读取整个 EROFS,只经对应 payload reader 读 superblock
(128 字节)、扫描 ZIP 尾部并解析单个 stored entry。manifest 路径可能触发网络/
chunk 拉取,不能保证普适的亚毫秒延迟。

## 7. 判断工作负载是否需要适配

从目标节点原生架构的 digest 固定镜像开始，验证真实入口、用户、可写路径和
依赖，而不只启动 shell。[runtime bundle](sandbox-runtime_zh.md) 提供 Guest
init，[所选 guest kernel](vmlinux_zh.md) 提供内核能力；OCI 镜像携带主机模块
不能补足该内核未提供的能力。

| 工作负载需求 | 判断与验收探针 |
|---|---|
| 普通用户态服务 | 用 `flatten-ctl info --json app.img` 检查 User、Entrypoint/Cmd、Env、WorkingDir；以实际用户检查可执行路径、DNS 和 upper layer 可写路径。显式 launch 覆盖镜像默认值（§3.4）。 |
| 文件 capabilities、SELinux label 或其他 xattr | 此路径不保留 xattr（§4.3）；重建/适配应用或验证其他受支持部署，不能假设 capability 位和 label 保留。必须测试实际特权操作。 |
| 设备、内核模块、TUN/bridge 或嵌套容器网络 | 对照所选 kernel 配置；预设不支持可加载模块且裁剪若干设备/网络栈。export 成功不证明这些能力；定制 kernel 须通过内核指南的 ABI/VMM 与启动/恢复验证。 |
| OCI 元数据驱动行为 | 元数据保留不代表消费者执行；检查镜像默认值合并和显式 mounts/launch，不假设 Docker 网络、Healthcheck 编排或 OnBuild 执行。提供并测试显式构建就绪命令。 |
| 有状态或外连服务 | 暂停/恢复前后验证文件及应用状态，测试外部连接重建。进程快照不等于外部数据库 checkpoint，也不会续租远端 lease。 |

在一次性沙箱内执行代表性请求和应拒绝的操作，暂停/恢复并比对命令及文件结果。
启动失败时先区分镜像文件/权限与 kernel 能力缺失，再调整启动策略。保留原镜像
digest 与日志，通过 owner kill 测试沙箱，引用核查后才退役工件。flatten 成功
不是工作负载兼容性认证。

<a id="7-see-also"></a>

## 8. See Also

- [accelerator/docs/manifest_zh.md](https://github.com/kuasar-sandbox/accelerator/blob/main/docs/manifest_zh.md):
  将镜像 ingest 进内容寻址存储,chunk dedup 跨镜像共享重复内容。
- [宿主镜像默认配置合并](https://github.com/kuasar-sandbox/sandboxer/blob/main/pkg/sandbox/imageconf.go):
  启动时使用内嵌 OCI runtime config。
- [sandboxer/docs/sandbox_zh.md](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox_zh.md) `boot.root.base`:
  选择展平镜像作为只读 base。
- [Native 构建指南](../native-deps/README_zh.md):在仓库根目录用
  宿主安装 erofs-utils；用 `make flatten-ctl` 构建 CLI;
  mkfs 只用于宿主打包和独立对照；CLI 内含 Go 生成器，不查找外部 mkfs。
- [系统总览](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/kuasar-sandbox_zh.md) §2.2 / §3.1:
  展平在系统中的位置和目标。


Image 打包使用 `accelerator/pkg/tailzip` 分离 EROFS payload 与配置后缀，再写入 tarstream envelope。声明的 payload commitment 覆盖 EROFS prefix，后缀使用独立的 metadata commitment。新生成器的 EROFS 布局和摘要可以变化；确定性配置 ZIP 及封装规则保持不变，已有工件按原有声明身份读取。
