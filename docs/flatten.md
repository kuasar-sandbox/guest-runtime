# flatten — 容器镜像展平工具

把多层 OCI/Docker 镜像合并(展平)成单个 EROFS 文件系统镜像,作为
`manifest-ctl` 的输入,或直接作为沙箱的只读根(`boot.root.base`)挂载。

`flatten-ctl` 强调**确定性**——同一镜像每次展平出字节级相同的输出,
保证后续 chunk dedup、manifest content-key 跨次稳定。输出文件**自带 OCI
runtime config**(末尾追加 ZIP),沙箱启动时无需额外索引文件就能拿到
Entrypoint / Env / WorkingDir 等启动参数。

## 1. 概述

### 1.1 为什么要展平

容器镜像由若干 tar layer 叠加而成,运行时需要 overlayfs / fuse-overlay 等
文件系统驱动把它们合并成单一根目录。沙箱场景里我们换成更轻的方案:**离线
合并到一个 EROFS 镜像**——

- guest 用 vhost-user-blk 直接把它当 read-only 根挂上,无需 overlayfs;
- host 多个 sandbox 共享同一个 EROFS 文件的 page cache,密度成本明显降低;
- EROFS 是只读 + 不可变,与内容寻址 (manifest 的 chunk + content key) 天然契合。

### 1.2 输入与输出

输入,三选一:

- **远程 registry 镜像**——镜像引用如 `nginx:1.27`、`gcr.io/ns/app@sha256:...`;
  `flatten-ctl` 直接拉取并展平(§2.4),拉取凭据经 `FLATTEN_REGISTRY_*` 环境变量注入,
  层 blob 落入可跨进程共享的本地 OCI-layout 缓存;
- **Docker archive**(`docker save` 产物的 tar 流,stdin 或本地文件);
- **已展平的 rootfs 目录**——位置参数是一个本地目录时,mkfs.erofs 直接就地读取
  该目录出图,零拷贝、不改源树(§2.1 的目录源小节)。

(OCI layout 目录暂不直读,见 §5.5。)

输出:**单文件**,字节布局 `EROFS 镜像 + 末尾 ZIP archive`(详见 §3 镜像
格式)。ZIP 内含 OCI runtime config 投影,沙箱启动时直接读取。**registry 与
docker-archive 两条层源汇入同一展平 sink,同一镜像内容产出逐字节相同的 EROFS;
目录源同样确定性(同一棵树两次导出字节相同,mtime 由 `-T0 --ignore-mtime` 在
mkfs 层归一,不触碰源树)。**

不做的事:不签名,不加密,不分块——加密/去重由 `manifest.md` 描述的下一阶
段处理。

### 1.3 在系统中的位置

```
   ┌─ registry image ─┐
   │  repo@sha256:..  │──┐    ┌─ flatten-ctl ───────────┐     ┌─ manifest-ctl store ──────┐
   └──────────────────┘  ├───►│  pull / layer iter      │────►│  chunk + crypto + dedup   │
   ┌─ docker save ────┐  │    │  merge + whiteout       │     │  → store-ctl gRPC Put     │
   │  layered tar     │──┘    │  mtime → 0              │     └───────────────────────────┘
   └──────────────────┘       │  mkfs.erofs             │
                              │  + append config zip    │     ┌─ sandbox-ctl run ─────────┐
                              └─────────────────────────┘     │  boot.root.base =         │
                                                              │    file:// | manifest://  │
                                                              └───────────────────────────┘
```

## 2. 命令行接口

七个子命令:`export`(展平为镜像工件,可选直接入库)、`referer`(OCI Referrers
lookup/put 原子操作)、`info`(检视镜像工件 / `manifest://` 引用)、`cache`
(检视/回收本地拉取缓存)、`config`(输出/校验 flatten 配置)、`tar`(通用 tar 提取/封装)、`mountpoint`(自 bind 造挂载点,
guest 内导出配套)。

| 子命令 | 用途 |
|--------|------|
| `export` | registry 镜像 / docker-archive / rootfs 目录 → 确定性 EROFS 的 **tarstream 镜像工件**(条目 `image`,约定后缀 `.img`);`--upload` 时顺带 ingest 进 store 并打印 manifest key |
| `referer` | `lookup` 查询源镜像是否已有可复用 manifest id;`put` 将宿主上传得到的 manifest id 回写到源 repo 的 OCI Referrers(§2.4) |
| `info` | 读镜像工件(经信封)的 EROFS superblock + 末尾 ZIP 里的 OCI runtime config 并打印 |
| `cache` | `cache info` 看缓存占用、`cache gc` 按 LRU 回收到上限(§2.5) |
| `config` | 输出规范化的 flatten 配置(`--config`/`FLATTEN_CONFIG`,加载即校验),或 `--template` 骨架;`-o <file>` 写文件(默认 stdout) |
| `tar` | 通用 tar 提取(全路径声明洞精确)与单文件封装(§2.6) |
| `mountpoint` | `mountpoint <dir>`:MkdirAll + 自 bind,使 `<dir>` 成为挂载点 → `export --skip-mounts` 自动排除之;guest 内导出自身 rootfs 时作 tmpdir/输出落点(防自吞,Linux only) |

`export` 的输入是**位置参数**(匿名),按下列优先级自动判别 registry / 本地:

```
1. "-"                       → stdin docker-archive
2. "docker-archive:<path>"   → 本地文件(剥前缀)
3. os.Stat 命中常规文件       → 本地 docker-archive  (./app.tar、/abs/x.tar 自然命中)
4. 否则按 name.ParseReference → 远程 registry 引用
```

歧义(本地文件名恰好形如 `repo:tag`)用 `--registry` / `--archive` 强制。`info` 的位置
参数必填,接受镜像工件路径或 `manifest://<hex>`。位置参数须置于 flags 之后(Go stdlib
flag 在首个非 flag 实参处停止解析)。

展平保留镜像内文件的属主与权限位(§4.3),因此 `export` 需要 root 或
`CAP_CHOWN`;启动时即预检,避免昂贵的拉取+解包后才在首层 chown 上失败。
`referer`/`info`/`cache`/`config` 不需要特权。

进度与诊断一律走 stderr,stdout 只承载交付物(镜像工件流 / manifest key /
`--print-digest` 的 digest);`export`/`cache gc` 经 `--no-progress` 关闭。
拉取按层打 `pull: <done>/<total> layers`(只计缓存未命中的层),展平按阶段打
`flatten: ...`,`--upload` 上传打 `upload: ...`(百分比+速率);字节型进度 2s 节流。

### 2.1 `flatten-ctl export`

```
flatten-ctl export [flags] <ref|path|->

  <ref|path|->            registry 镜像引用,或 docker-archive(省略/`-` = stdin)
  --output <path|->       镜像工件输出路径(tarstream,条目 image,约定 .img);
                          `-` = stdout(终端拒写)。--upload 关闭时必填,
                          --upload 开启时可省(产物默认丢弃,只要 manifest key)
  --upload                展平后把 EROFS ingest 进 store,stdout 打印 manifest key
  --manifest-config <p>   manifest 配置 YAML(覆盖 MANIFEST_CONFIG env);--upload 必需
  --config <p>            flatten 配置 YAML(覆盖 FLATTEN_CONFIG env):tmpdir/platform/
                          tls/cache/referer(详见 §2.4)
  --platform <os/arch>    覆盖配置里的拉取 platform(os/arch[/variant])
  --tmpdir <dir>          覆盖配置 tmpdir(自动创建)。guest 内导出自身 rootfs 时
                          必须指向 mountpoint 子命令造出的挂载点(连同 mkfs 临时
                          文件一起被 --skip-mounts 排除,防自吞)
  --insecure              registry 源:允许 plain-HTTP / 跳过 TLS 校验(覆盖配置)
  --no-progress           禁用 stderr 进度输出

  # 远程 registry 源(详见 §2.4)
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

**rootfs 目录源**。位置参数 stat 为目录时走第三条源:mkfs.erofs **就地读取**
该目录——没有 staging 拷贝,源树永不被修改(时间戳归一靠 `-T0 --ignore-mtime`
在镜像层完成),保留属主只需要对树的读权限(导出完整 rootfs 用 root 跑)。被
`--skip` / `--skip-mounts` 排除的路径**节点整体消失**(目录本身也不在镜像里);
输出文件落在 rootfs 内时自动排除自身。导出 `/` 必须带 `--skip-mounts`(否则会
走读 /proc、/sys)。registry 专属 flags(`--platform`/`--print-digest`/
`--registry`/`--archive`)与目录源互斥。

```bash
# 把一台机器/一个 guest 的根做成沙箱镜像(挂载点全部剔除)
flatten-ctl export --skip-mounts --runtime-config config.json -output host.img /

# 从准备好的 rootfs 目录出图,剔除缓存目录;config 直接复用旧镜像里的投影
flatten-ctl info --json old.img | jq .config > rc.json
flatten-ctl export --skip var/cache --runtime-config rc.json -output new.img /srv/rootfs

# --upload 同样可用:目录 → EROFS → store,stdout 打 manifest key
flatten-ctl export --skip-mounts --upload --manifest-config m.yaml /srv/rootfs
```

### 2.2 (已移除)

`flatten-ctl verify` 已移除:确定性由单元测试与 manifest key 的可复现性背书,
双跑比对不再提供增量价值。(§ 编号保留占位,避免跨仓引用重排。)

### 2.3 `flatten-ctl info` — 检视镜像

```
flatten-ctl info [--json] [--manifest-config <path>] <path|manifest://hex>

  <path|manifest://hex>          EROFS 文件路径,或 manifest://<hex>(位置参数,必填)
  --json                         机器可读 JSON 输出(默认人类可读)
  --manifest-config <path>       manifest 配置 YAML;`manifest://` 输入必需
```

读 EROFS superblock 拿 image size,从末尾 ZIP 解出 OCI runtime config 并
打印。`manifest://` 输入经 cache-ctl/store-ctl 的 fetch 路径按 chunk 粒度只读
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

无 ZIP trailer 时(老版本输出)`config` 为 `null`,人类模式打印
`(no OCI config trailer)` 提示。

辅助管道:

```bash
# 用 unzip 直接拎 config.json(ZIP 是标准格式,任何 ZIP 工具可读)
flatten-ctl info --json my-app.img | jq .config

# 或者经 flatten-ctl info --json 进 jq
flatten-ctl info --json my-app.img | jq '.config.Entrypoint'
```

### 2.4 远程拉取、本地缓存与 Referrers 回写

位置参数判定为 registry 引用时(§2 判别规则),`flatten-ctl` 经 [go-containerregistry]
直接拉取、展平,无需先 `docker save`。所有 registry 行为由 **`--config` YAML**
(或 `FLATTEN_CONFIG` env)配置,密钥不入文件:

```yaml
tmpdir: ""                     # 每次运行临时目录的父目录(空 = $TMPDIR 或 /tmp)
platform: linux/amd64          # 空 = 跟随宿主架构(linux/$GOARCH);--platform 覆盖
insecure: false                # 允许走纯 HTTP 的 registry(dev/私有);不影响 TLS 证书校验
pull_jobs: 4                   # 并发下载层数(下载并行、apply 串行)
tls:                           # HTTPS 证书校验(registry 与 CDN blob 重定向均生效,见下)
  ca_cert: ""                  # 追加信任的 CA bundle 路径(PEM,可含多证书)
  insecure_skip_verify: false  # 完全关闭证书校验(不安全;优先用 ca_cert)
cache:
  dir: ""                      # OCI-layout 持久缓存根;空(默认)= 临时缓存(tmpdir 下,跑完清理)
  max_size: 10GiB              # 上限,超出按 LRU 回收;"0" = 不限,仅手动 cache gc
referer:
  validity: 720h               # 可选;referer put 写入 valid_at 的过期段
```

artifact_type 固定为常量 `application/vnd.kuasar.flatten-manifest.v1`(不可配置,保证跨工具/版本一致)。

**凭据(命名空间环境变量,匿名回落)**:`FLATTEN_REGISTRY_TOKEN`(Bearer,优先)或
`FLATTEN_REGISTRY_USERNAME` + `FLATTEN_REGISTRY_PASSWORD`(Basic);都不设则匿名拉公有
镜像。密钥只走 env(不上 argv、不入配置文件),契合 orchestrator 经 exec env 把租户拉取凭据
下发进构建沙箱的模型(`kuasar-sandbox/docs/deployment.md` §5)。

**TLS(`tls.*`)**:作用于全部 HTTPS 请求——既包括 registry API,也包括层 blob 的 CDN
重定向(拦截式代理会用私有 CA 重签这些证书,系统信任库默认拒绝)。`ca_cert` 把额外的
PEM CA bundle 追加进系统信任库,是信任此类私有根 CA 的正路;`insecure_skip_verify`
整体关闭证书校验,仅作 CA 不可得时的逃生口。与 `insecure` 正交:后者只决定 registry
是否可走纯 HTTP,不影响 TLS 校验。

**本地缓存**:标准 **OCI image layout** 目录(`oci-layout` + `index.json` +
`blobs/<algo>/<hex>`),`crane`/`skopeo` 可直接检视。缓存命中判定 = `blobs/` 下该 digest
是否存在(内容寻址,多个 `flatten-ctl` 进程共享同一目录天然安全);blob 边下边校 digest、
原子落盘(temp→rename)。超出 `max_size` 时持 flock 按 LRU(mtime)回收到低水位,并以
grace 期保护近期写入的 blob 不被并发拉取误删。`cache.dir` 指向共享路径即跨任务去重,指向
每任务独立路径即隔离——由调用方按需配置。

**多架构**:registry 引用常指 manifest index,按 `platform` 选出具体 image 再展平;
`Architecture`/`Os` 仍原样投影进 config.json(§3.2),不做校验。

**确定性提醒**:tag 可变,`:latest` 不可复现;可复现构建请钉 `@sha256:`。`export` 把
解析到的 `repo@sha256:..` 打到 stderr(`--no-progress` 关闭),`--print-digest` 另打到
stdout。复现性以 digest 为准:对 tag 只与其当前指向一样稳,对 digest 永远稳定。

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
{"supported":true,"subject":"repo/app@sha256:...","hit":true,"manifest_id":"..."}
```

- `supported=false`:registry 不支持 Referrers API,调用方可按策略 fallback 或失败;
- `supported=true, hit=false`:支持但没有 owner 匹配且格式有效、尚未过期的记录,
  调用方继续 `export` 并在宿主侧上传;
- `supported=true, hit=true`:返回的 `manifest_id` 可由宿主校验后直接复用,无需拉取/展平。

`lookup` 严格校验 `valid_at`。缺失或格式错误的时间、未来的 import 时间、已经到期
或早于 import 的 expiry 都作为 miss 跳过;存在多条有效记录时返回 import 时间最新者。

`put` 构造 referrer artifact(OCI image manifest:subject=`D`、artifact type 经 config
media type 承载、注解 `owner`/`id`/`valid_at`)并推回源 repo。`put` 需要源 repo push
权限;失败由调用方决定是否使构建失败。

referrer 注解:

```
vnd.kuasar.flatten-manifest.owner    = <hmac-hex> <referer_desc>
vnd.kuasar.flatten-manifest.id       = <manifest_id>                        # ingest 产出的内容键
vnd.kuasar.flatten-manifest.valid_at = <import RFC3339>[ <expiry RFC3339>]
```

其中 owner token 通常由宿主计算:`hmac = HMAC-SHA256(key = 客户秘钥 MANIFEST_KEY,
msg = referer.key)`,注解值为 `<hmac-hex> <referer_desc>`。guest 命令只接收
`--owner`,不需要也不应接收 `MANIFEST_KEY`。

**前提与注意**:OCI 规范要求 referrer 与 subject 同 repo → `referer put` 需对**源
repo 有 push 权限**(面向租户自有 registry;对只读上游写不进会失败)。referrer
artifact 含时间戳,本身每次不同,但展平产物 / manifest id 仍确定。对公开 base 镜像,
referrer(owner token / id / 时间)对能读该 repo 者可见——owner 经 HMAC、id 为不透明内容
键,但"某 owner 在某时刻展平过该镜像"这一事实会暴露。

[go-containerregistry]: https://github.com/google/go-containerregistry

### 2.5 `flatten-ctl cache` — 检视/回收拉取缓存

```
flatten-ctl cache info [--config <p>] [--cache-dir <D>]
flatten-ctl cache gc   [--config <p>] [--cache-dir <D>] [--cache-max-size <S>] [--no-progress]
```

`cache` 子命令面向**持久**缓存(`cache.dir` 显式配置时);默认临时缓存随 `export` 跑完即清,
无需 gc。`cache info` 打印缓存目录、blob 数、总占用与上限。`cache gc` 持 flock 按 LRU 回收到配置
(或 `--cache-max-size` 覆盖)的上限,`"0"` 清空(grace 期内的除外)。缓存目录默认取
`--config` 的 `cache.dir`,`--cache-dir` 覆盖。常驻场景可由 cron 周期跑 `cache gc`
强约束上限(`export` 每次拉取后也会顺带回收)。

### 2.6 `flatten-ctl tar` — tar 流提取与单文件稀疏流

tar 工具面,两个方向都是**纯 Go、零 tar 二进制依赖**:

```
flatten-ctl tar extract [-f tarfile] [--chown u:g] [--chmod 755] [--dense] [规则...]
flatten-ctl tar stream  [-f tarfile] [--size N] [tar内名[:源]]
```

稀疏语义铁律(全平台一致):**零值字节是数据、空洞是缺失,二者业务含义不同,
永不互转**——洞只来自权威元数据(文件系统 SEEK_HOLE、tar sparse map),
绝不从内容零扫描推导。

**extract** 从任意 tar 流取文件,**全路径声明洞精确**:对输入单遍流式
(stdlib 解码,管道零落盘),常规文件致密落盘(数据段里的零保持已分配,不打洞);
**稀疏成员**(PAX sparse 1.0 经 `PAXRecords` 检测,老 GNU 'S' 经 typeflag)的还原
分三档:

- `-f` 文件输入:引擎按**条目序数**在第二个句柄上经 tarstream 重定位该成员,
  取回 stdlib reader 隐藏的洞图(Go 1.26 仍未导出,golang.org/issue/22735),
  只 punch 声明的洞;stdlib 侧 `Next()` Seek 跳过包体,数据不读两遍;
- stdin(单遍,图已被 stdlib 消费,且禁落盘):**硬错误**并引导——绝不静默把
  32 GiB 逻辑稀疏档致密化;
- `--dense`:显式整体关闭稀疏处理,回 stdlib 逻辑字节致密落盘(声明洞落为
  已分配零)。

便捷形态:**无规则 + `-f` 平台工件**(一个 payload + digest marker)自动洞精确解出 payload
(`tar extract -f image.img` 即可);**单条显式文件规则**(`成员[:目标文件]`)
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

不给规则取全部到当前目录;`--chown/--chmod` 改写每个落盘条目的属主/权限。`--chown`
取 `uid:gid`:数字直用,**名字**则按解包目标根的 `/etc/passwd`/`/etc/group` 解析
(Docker `COPY --chown=name` 同款;CGO 关,os/user 直读文件不经 NSS);`user`(无组)
取该用户主组,纯数字 `1000` 镜像为 `1000:1000`。node-ctl 的 COPY step 即以
`extract --dense --chown` 把上下文 tar 摊进 guest rootfs(见 node.md §12)。

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

```
偏移 [0,         erofs_end)      EROFS 文件系统(self-describing,
                                  superblock 在偏移 1024 标明 image size)
偏移 [erofs_end, EOF)             ZIP archive(STORED 模式无压缩)
                                    / config.json     OCI runtime config 投影
```

两段共存于一个文件,**互不干扰**:

- **EROFS 读路径**(kernel `mount -t erofs` / vhost-user-blk backend):
  从偏移 0 读 superblock,superblock 自带 `blocks << blkszbits` 的 image
  size,kernel 不读 size 之后的字节,trailing ZIP 自然不可见
- **ZIP 读路径**(任意标准 ZIP 工具,如 `unzip`):从文件**末尾**扫
  End-of-Central-Directory(EOCD),内部 offset 都是相对 ZIP 起点,EOCD
  扫描容忍前缀任意字节,EROFS 段的存在不影响 ZIP 解析

`erofs_end = blocks × (1 << blkszbits)`,从 superblock 解析。EROFS 镜像
endian-neutral,跨 host arch 可挂载。

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

两个运行时投影字段相等 → JSON 字节相等 → ZIP entry 字节相等 → 整文件
sha256 相等。

### 3.4 沙箱怎么用 config.json

`sandbox-ctl run` 在启动前从 boot.root.base 文件末尾解 ZIP,拿到运行时投影作为
LaunchSpec 的 fallback:`sandbox.yaml` `launch.*` 字段优先,`Env` 取镜像在下、
override 在上的合并,`Volumes` 并入 `mounts`。因此 image config 有 Entrypoint/Cmd
时 `launch.exec` 即可省略。合并规则的权威定义见
`sandboxer/docs/sandbox.md` §3.3。

## 4. 算法

### 4.1 layer 迭代

展平引擎以 `Source` 抽象输入:Source 给出底→顶有序的层(每层一条**未压缩** tar 流)
与原始 OCI image config JSON,两个实现共享同一个确定性 sink(`Build`):

- **docker-archive**:输入 tar 内含 `manifest.json`(层顺序 + image config 路径)与
  各层 tarball;先解到临时目录,按 `manifest.json` 顺序逐层打开(gzip 层透明解压);
- **registry**(`pkg/remote`):层 blob 经本地 OCI-layout 缓存,按媒体类型解压
  (gzip/zstd)成同样的未压缩 tar 流(§2.4)。

`Build` 把每层 tar entry 流式应用到磁盘上的临时 staging rootfs:上层 entry 覆盖下层
同路径 entry;每个 entry 落盘后按 tar 头 chown+chmod 保留属主与权限位(含
setuid/setgid/sticky;先 chown 后 chmod,因为 chown 会清掉 setuid/setgid)——
这一步要求 root/CAP_CHOWN(§2)。image config 按 §3.2 投影,最后追加进 ZIP。

### 4.2 whiteout 处理

OCI 镜像规范用特殊文件名表达"删除":

- `.wh.<name>` —— 删除同目录下的 `<name>`(file 或 dir);
- `.wh..wh..opq` —— 把所在目录标记为 opaque(其下所有底层条目都不可见)。

`flatten-ctl` 在合并时识别这两类 marker,把对应条目从 staging rootfs 中移除,然
后**不**把 marker 本身写进输出——最终 EROFS 只看到合并后的可见文件。

### 4.3 元数据归一化(确定性的关键)

为保证字节级可重复,以下字段在写入 EROFS 之前被强制归一化:

| 字段 | 处理 |
|---|---|
| mtime / atime | 归零(epoch 0);EROFS 侧另以 `-T0` 固定全部时间戳 |
| uid / gid / mode | 保留原值(应用语义敏感:`/tmp` 须 1777、`/home/<user>` 须 user 属主)——按层 tar 头 chown+chmod,需 root/CAP_CHOWN(§2) |
| inode 编号 | 由 `mkfs.erofs` 按确定顺序分配 |
| 文件遍历顺序 | 字典序(`mkfs.erofs` 内部) |
| hardlink | 在 staging 上重建为真实硬链接(同 inode);`-Ededupe` 另对重复数据块去重 |
| 扩展属性(xattr) | 不写入(`-x-1` 禁用;层 tar 中的 xattr 不应用) |

EROFS 自身的格式版本由 `mkfs.erofs` 决定(我们用 erofs-utils 1.9.x);项目
固化此版本,确保跨节点构建结果一致。

### 4.4 mkfs.erofs 调用

`Build` 对 staging rootfs 执行 `mkfs.erofs`,flag 集固定:

```
mkfs.erofs -Ededupe --chunksize=4096 -T0 -b4096 -x-1 \
    -U 00000000-0000-0000-0000-000000000000 --quiet <output> <staging-dir>
```

- `-Ededupe --chunksize=4096` —— 镜像内数据块去重 + chunk 化布局,缩小元数据体积、
  稳定数据块偏移,最大化下游 CDC 跨镜像去重;
- 不压缩 —— 原始字节才能被 CDC 分块跨镜像去重;
- `-T0` / 固定 UUID / `-x-1` —— 固定时间戳、固定 UUID、禁 xattr,消除非确定性来源。

完成后追加 ZIP:以 append 方式打开输出文件,在 EROFS 段之后写一条 STORED
(无压缩)模式的 `config.json` entry(§3.3 的确定性约束)。

`mkfs.erofs` 由 `guest-runtime/native-deps` 构建产出(`make -C ../guest-runtime/native-deps erofs`);本仓
`make build` 只构建 flatten-ctl。运行期定位优先级:`MKFS_EROFS_PATH` 环境变量 >
flatten-ctl 同目录 > `PATH`。

EROFS 格式 endian-neutral,所以 host arch 与 target arch 无关——任何
`mkfs.erofs` 都能产出可被任何 arch guest 挂载的镜像。

## 5. 设计决策

### 5.1 为什么是 EROFS 而非 ext4 / squashfs

- **EROFS 是只读** —— 与内容寻址 (chunk + content key) 天然兼容,加上
  不可变性反过来支持 host 跨 sandbox 共享。
- **EROFS 单文件** —— 输出是流式 / 单文件,适合 manifest 上传与挂载。
- **EROFS endian-neutral** —— 跨 host arch 构建跨 host arch 挂载,密度
  规划不受架构耦合。
- **squashfs** 跨 sandbox 共享要走 page cache 命中,密度退化。
- **ext4** 是可写的;每个 sandbox 要独立 mount + COW 才能复用,密度比 EROFS
  + overlayfs 组合差,而且需要 fsck 等运维。

### 5.2 为什么离线合并而非运行时 overlayfs

运行时 overlayfs 需要每 sandbox mount 一次,host 内核 mount 表线性增长,
密度上限受 mount 数量约束;EROFS 单 mount 跨多 sandbox 共享绕开这个瓶颈。

代价是镜像构建一次,所以更新流程多一步——但镜像构建本来就是 CI/CD 流水
线,这一步成本可摊销。

### 5.3 为什么 config.json 内嵌而不是边车文件

外加 `app.erofs.config.json` 边车文件的方案有两个问题:

- **拷贝 / 上传分裂**:用户必须记住"两个文件一对",`scp` 漏一个就坏
- **manifest:// 无副车机制**:manifest 是单一 reader,边车文件需要单独的
  manifest key + 单独的拉取流程

ZIP-at-end 让单一文件自描述,`manifest-ctl store` 一次喂入即完整,
`sandbox-ctl run` 一次解析即拿到全部启动配置。EROFS 与 ZIP 双格式天然不
冲突(superblock 自描述 size + ZIP 从尾部扫 EOCD),无需引入新协议层。

### 5.4 为什么投影而不是原样转发

OCI image config 字段繁多,大量与启动无关:`created` / `author` / `history`
是构建元数据,跨实例不同;`rootfs.diff_ids` 是层 hash,展平后不再有意义;
`OnBuild` 是构建指令而不是运行时设置。原样转发会:

- 破坏跨次确定性(timestamps 漂移)
- 让 manifest dedup 无谓波动(每次构建的 history 不同)
- 误导沙箱(应用看到 OnBuild 可能误用)

显式投影把"运行时启动需要什么"作为唯一标准,下游可预测。

### 5.5 不实现 / 暂不实现的功能

- **OCI layout 目录直读** —— `export` 现已支持直接从 registry 拉取(§2.4),且本地
  拉取缓存本身就是标准 OCI layout 目录;但"把任意 OCI layout 目录当输入展平"仍未
  直接支持。需要时上游可用 `skopeo copy oci:./dir docker-archive:x.tar` 转换,或
  `crane push` 到 registry 再拉
- **镜像签名/加密** —— manifest 层做(`manifest.md`)
- **层级保留** —— flatten-ctl 输出是合并后的单一 EROFS,层信息丢失。需要
  分层保留的场景(例如增量推送)由 manifest 层 chunk dedup 取代

## 6. 性能特征

构建时间:与镜像大小近似线性,主要成本在 layer tar 解压 + mkfs.erofs。
1 GiB 镜像 ~3-5 秒(SSD)。ZIP append 步骤 < 10 ms(单 entry,无压缩)。

确定性自检:同镜像两次展平产出 sha256 一致,由单元测试 + manifest key 可复现性背书(`verify` 子命令已移除,见 §2.2)。

`flatten-ctl info` 不解压 EROFS,仅读 superblock(128 字节)+ ZIP EOCD 扫描
+ 单 entry 解压,亚毫秒级。

## 7. See Also

- `accelerator/docs/manifest.md` —— 展平后的镜像经 manifest-ctl 入内容
  寻址存储;chunk dedup 跨镜像共享 layer-level 重复内容
- `sandboxer/docs/sandbox.md` §3.3 flattened image 内嵌 config.json ——
  沙箱启动时如何使用 ZIP trailer 中的 OCI runtime config
- `sandboxer/docs/sandbox.md` §boot.root.base —— 用展平镜像作为 sandbox
  的只读根
- `guest-runtime/native-deps/docs/build.md` —— 构建 mkfs.erofs(`make -C
  guest-runtime/native-deps erofs`);本仓 `make build` 只构建 flatten-ctl,运行期
  经同目录 / `PATH` 定位 mkfs.erofs
- `kuasar-sandbox/docs/kuasar-sandbox.md` §2.2 / §3.1 —— 展平在系统中的位置与目标
