[English](README.md) | [简体中文](README_zh.md)

<a id="build--native-dependency-build-workflow"></a>
<a id="native-deps"></a>
# native-deps — build and maintenance

The native-deps directory builds upstream sources into three default artifact families consumed at runtime, not linked by the Go repositories: `mkfs.erofs`/`fsck.erofs` (erofs-utils), `vmlinux` (guest kernel) and `envd` (E2B guest agent). Their autotools/Kbuild/Go toolchains and independent upstream schedules are kept together in `guest-runtime/native-deps`. An additional `versitygw` gateway target is opt-in and not part of the default build.

The common pipeline is: pinned upstream tarball URL with optional SHA256 verification → shared cache and extraction → local patches (vmlinux, using `git am`) → build → `bin/<arch>/`.

This document covers targets, patch development, cross-compilation and cache/cleanup rules. Artifact-specific design belongs elsewhere: see [the kernel specification](../docs/vmlinux.md) for kernel configuration, and `sandboxer/docs/cloud-hypervisor.md` for the VMM and its patches.

Project-level aggregation runs `make -C kuasar-sandbox build`, which invokes this directory's `make build`, then collects artifacts according to `kuasar-sandbox/release/bin-inputs.manifest` into `kuasar-sandbox/bin/<arch>/` for E2E, Demo and local integration reuse.

<a id="1-概述"></a>
## 1. Overview

<a id="11-产物与版本-pin"></a>
<a id="artifacts-and-consumers"></a>
<a id="产物与消费方"></a>
### 1.1 Artifacts and pinned versions

| Artifact | Pinned upstream | Repository inputs | Consumers |
| --- | --- | --- | --- |
| `mkfs.erofs` / `fsck.erofs` | erofs-utils v1.9.1 | — | accelerator flattening, guest-runtime image construction, source diagnostics and accelerator tests |
| `vmlinux` | Linux 6.1.169 (LTS, cdn.kernel.org) | `deps/linux-patches/` and `deps/vmlinux/*.config` | sandboxer / sandbox-ctl guest kernel |
| `envd` | e2b-dev/infra 2026.22 source tarball | — | Guest agent in `sandbox-runtime.bundle` |
| `versitygw` (opt-in) | versity/versitygw v1.5.0 | — | Local/single-node S3-compatible file-storage integration where required |

Pins live in Makefile variables (`EROFS_TARBALL`, `LINUX_TARBALL`, `ENVD_TARBALL`, and optional `VERSITYGW_TARBALL`), accepting `url#filename` or a local path. An empty corresponding `*_TARBALL_SHA256` skips verification. The current default EROFS, Linux and Envd inputs have hashes configured; the optional gateway hash must be supplied when required by the deployment's verification policy. Updating a version means updating those variables and revalidating patch application.

Those overrides are development-build inputs. The Runtime and vmlinux release
packagers accept only the repository-pinned EROFS, Linux and Envd inputs with
their configured digests (the release workflow may fetch the same Linux bytes
from its exact public mirror). To publish another native input, update its
repository pin, digest and release source record together; packaging fails
instead of emitting provenance for a different input.

Release packaging builds from clean snapshots of the selected Git commits in a
temporary sibling workspace, using the existing component Makefiles. Runtime
packaging rebuilds its minimal accelerator/sandboxer closure, Envd, EROFS tools,
and the image; kernel packaging independently rebuilds Linux with the selected
patches and configuration. Only checksum-verified download tarballs are reused.
Existing binary outputs, extracted source trees and development work remain
untouched. Internal dependency records retain a release version only when its
local Git tag identifies the selected commit; otherwise they record
`git:<commit>`, including when a target formal tag does not exist yet.

Validation rejects non-root numeric archive ownership and checks the exact
Envd, EROFS and Linux source URLs and digests. Publication passes its selected
`SOURCE_SHA` into validation; Runtime publication also requires the exact
accelerator/sandboxer `RELEASE_DEPENDENCIES` binding. Regenerating checksums does
not permit a different project commit, dependency version or native source to
be published under that request. Local source packaging can still use untagged
dependency commits; those records are not claimed to be existing releases.

Runtime packaging also reads the fresh `mkfs.erofs` linker map. For each
linked system archive or startup object it records the actual file digest and
the installed Debian source package or RPM source-package identity, and copies
the corresponding copyright, license and notice files, including referenced
common license texts. This covers libc, libuuid and compiler runtime/startup
inputs as well as erofs-utils itself; a package name or SPDX label alone does
not replace those files.
Before collection, each installed input must match its file digest in the
trusted build host's Debian or RPM database. Missing, ambiguous or changed
file records fail packaging; package ownership alone is insufficient. This
checks installed file integrity, not a compromised host or package database.

The project CI template builds static libuuid from its pinned, checksum-verified
util-linux source. Its provisioner retains a per-build `SOURCES.tsv`,
`MATERIALS.sha256` and license directory beside the template inputs and copies
them with the library into every prepared slot. Packaging verifies both the
inventory and the actual library digest. Missing, altered or unowned native
inputs are rejected; update the trusted template instead of fabricating source
records. The resulting materials support release review, not a legal
certification. Kernel source selection remains independent of Runtime.

`librocksdb`, accelerator's CGO link dependency, is built in that repository, not here.

<a id="组成"></a>
<a id="layout"></a>
### 1.2 Build-source layout

| Path | Role |
| --- | --- |
| `deps/build-{erofs,vmlinux,envd}.sh` | Source-build scripts; vmlinux uses a multi-stage STAGE dispatcher |
| `deps/build-versitygw.sh` | Optional gateway build |
| `deps/common.sh` | Shared tarball download, cache and extraction helpers |
| `deps/linux-patches/` | Architecture-neutral guest-kernel patches |
| `deps/vmlinux/*.config` | Common and per-architecture kernel configuration fragments |


<a id="12-目录布局"></a>
<a id="12-directory-layout"></a>
### 1.3 Directory layout

```text
bin/
├── x86_64/                  x86_64 artifacts: mkfs.erofs, fsck.erofs, vmlinux, envd
├── aarch64/                 aarch64 artifacts (same names)
└── <name>                   Symlink → <host-arch>/<name>, created/updated only for native builds

build/
├── tarball/                 Upstream tarballs, shared across architectures
├── src/
│   ├── linux/               Shared kernel source/git tree; patch-development work lives here
│   ├── e2b-infra/           Shared Envd sources; Go selects the target via GOARCH
│   └── versitygw/           Shared sources for the optional gateway
├── x86_64/
│   ├── linux/               Kbuild O= output
│   └── src/erofs-utils/     Per-architecture autotools in-tree source/build tree
└── aarch64/                 Corresponding target output
```

Native builds (host = target) create `bin/<name> → <arch>/<name>` for their public binary entries. Cross-builds do not update host entry symlinks, avoiding a link to a binary the host cannot execute. Optional gateway output appears only when that target is built.

<a id="13-幂等与缓存"></a>
<a id="13-idempotency-and-caching"></a>
### 1.4 Idempotency and caching

- **Artifact reuse:** EROFS and Envd outputs are reused when their target files already exist; remove outputs to force those builds. Kernel output is also governed by tracked inputs: changes to its build script, common/architecture config fragments or patches trigger configuration reevaluation and incremental Kbuild rather than unconditional existence-only skipping.
- **Tarball cache:** `build/tarball/` caches by filename; hits avoid downloading. Extraction uses an `.extracted` marker for idempotency.
- **`make clean`:** removes `bin/` and target build output while **retaining** tarball caches and architecture-neutral `build/src/*` source trees. Kernel source trees may contain unexported patch-development work and must not be silently discarded.

<a id="2-构建目标"></a>
<a id="build"></a>
<a id="构建"></a>
## 2. Build targets

```bash
make build      # =all: vmlinux + erofs + envd
make erofs      # mkfs.erofs + fsck.erofs
make vmlinux    # Guest kernel
make envd       # E2B guest agent
make versitygw  # Optional gateway; not part of build
make clean      # See section 1.4
make help       # List targets
```

Build duration depends on the host, toolchain and cache state; these commands do not carry a fixed timing guarantee.

<a id="21-erofsmake-erofs"></a>
### 2.1 EROFS (`make erofs`)

`deps/build-erofs.sh` extracts erofs-utils into `build/<arch>/src/erofs-utils/`, keeping one tree per architecture because this autotools path does not support out-of-source builds. It runs `autoreconf` and `configure` with compression/FUSE/network features disabled, builds only the `lib`, `mkfs` and `fsck` subdirectories, and writes `bin/<arch>/{mkfs.erofs,fsck.erofs}`.

- The `mount`/`dump`/`fuse` subdirectories are skipped. The project does not consume those tools; the source workflow also avoids the v1.9.1 mount.erofs pthread-linking issue under `--disable-multithreading`.
- Configure requires libuuid and has no `--without-uuid` path. Cross-compilation requires multiarch `uuid-dev:<arch>`; the script checks it first and prints apt guidance (§4.2).
- Host build tools: `autoconf automake libtool pkg-config make gcc g++`.

<a id="22-vmlinuxmake-vmlinux"></a>
### 2.2 vmlinux (`make vmlinux`)

The target invokes the fetch, patch-application and build stages of `deps/build-vmlinux.sh` when its output needs rebuilding (§3). After applying `deps/linux-patches/*.patch`, it combines `deps/vmlinux/sandbox-common.config` with `sandbox-<arch>.config` into `arch/<kbuild_arch>/configs/sandbox_defconfig`, runs `make sandbox_defconfig` and `make olddefconfig` to resolve Kconfig dependencies, then `make -j$(nproc) <target>`, and copies out `bin/<arch>/vmlinux`.

- After `olddefconfig`, required DAX and architecture-specific options are checked. A requested critical option silently discarded by Kconfig fails the build.
- Make dependencies include build scripts, common/architecture configuration and tracked kernel patches. Changing these inputs reevaluates configuration and uses incremental Kbuild.
- x86_64 output is ELF (Kbuild target `vmlinux`); aarch64 output is the PE Image from `arch/arm64/boot/Image` (target `Image`). Both installed filenames are `vmlinux`.
- `build/src/linux/` is shared across architectures. Kbuild selects with `ARCH=` and writes to an architecture-specific `O=` directory.
- Host requirements: `bc bison flex make tar pkg-config gcc`, libelf headers (`libelf-dev` / `elfutils-libelf-devel`) and libssl headers (`libssl-dev` / `openssl-devel`). The headers support host Kbuild tools such as fixdep/sign-file; they are not linked into vmlinux.
- The two-fragment configuration contract and important enabled/disabled options are documented in [vmlinux.md](../docs/vmlinux.md), sections 2–3.

<a id="23-envdmake-envd"></a>
### 2.3 Envd (`make envd`)

`deps/build-envd.sh` extracts the e2b-dev/infra source tarball into architecture-neutral `build/src/e2b-infra/` and builds `packages/envd` with `GOWORK=off GOOS=linux CGO_ENABLED=0 -trimpath -ldflags "-s -w"`, producing `bin/<arch>/envd`. GOARCH selects the target without a cross C toolchain.

- `make sandbox-runtime` includes it as `/opt/sandbox-runtime/bin/envd`, the E2B-profile guest data-plane agent on port 49983.
- Envd's `go.mod` may require a newer Go toolchain, such as `go 1.26.3`. `GOTOOLCHAIN=auto` downloads it when necessary; toolchain downloading requires GOSUMDB to be enabled and is rejected with `GOSUMDB=off`.
- Override `ENVD_TARBALL` to change the tag. In `url#filename` form, filename determines the cache name.

### 2.4 Optional gateway (`make versitygw`)

`deps/build-versitygw.sh` builds the configured gateway source for the selected Go architecture. This target is not in `build`. It is available for local/single-node `builder.files_storage` deployments that need an S3-compatible gateway rather than a cloud object store. Its source, hash and source-directory variables are `VERSITYGW_TARBALL`, `VERSITYGW_TARBALL_SHA256` and `VERSITYGW_SRC`.

<a id="3-patch-开发循环vmlinux"></a>
## 3. Kernel patch development

The STAGE dispatcher in `deps/build-vmlinux.sh` manages vmlinux patches through these Make targets:

| STAGE | Make target | Behavior |
| --- | --- | --- |
| `fetch` | `linux-fetch` | Extract tarball, initialize Git, import the source and create `linux-patches-base`. Skip an existing base tag; reject an unrelated Git tree without that tag rather than overwrite work. |
| `patches-apply` | `linux-patches-apply` | Apply `deps/linux-patches/*.patch` using `git am`; idempotency and sanity rules below. |
| `patches-format` | `linux-patches-format` | Export `git format-patch <base>..HEAD` to `deps/linux-patches/`, clearing old `*.patch` files first. |
| `build` | `linux-build` | Build only, without changing patches. |

`linux-patches` aliases `linux-patches-apply`. Run from `guest-runtime/native-deps/`:

```bash
make linux-fetch       # Fetch once and create linux-patches-base.
cd build/src/linux
# Edit sources and git commit; use one commit per patch.
cd ../../..            # Return to native-deps before invoking its Make targets.
make linux-patches-format
make vmlinux           # Reapply/build to verify reproducibility.
```

The patch-application sanity checks must **never silently overwrite work in progress**:

- The source tree must have the base tag; otherwise the command reports an error and points to `make {linux,ch}-fetch`.
- `HEAD == base`: apply all patches with `git am`.
- `HEAD = base + N`, where N equals patch count and commit subjects match patch files one by one: treat patches as already applied and skip idempotently.
- Any other state: stop and instruct the developer to save work with `make linux-patches-format`, then reset to the base tag only after preserving that work, and rerun.

The current kernel patch set is architecture-neutral and touches `drivers/virtio/virtio_balloon.c`; both architectures use the same patches and their own defconfig.

<a id="4-交叉编译"></a>
## 4. Cross-compilation

### 4.1 TARGET_ARCH

| Value | Alias | GOARCH | KERNEL_ARCH |
| --- | --- | --- | --- |
| `x86_64` | `amd64` | `amd64` | `x86_64` |
| `aarch64` | `arm64` | `arm64` | `arm64` |

The default is `uname -m`. When `HOST_ARCH != TARGET_ARCH`, cross-compilation is enabled and `CROSS_PREFIX` is derived as `<target>-linux-gnu-` (explicit overrides are supported). Outputs go to `bin/<target>/`; host `bin/` symlinks are not updated. Running target binaries requires a suitable native host.

```bash
make TARGET_ARCH=aarch64 build
```

<a id="42-工具链准备x86_64-host--aarch64-为例反向对称"></a>
### 4.2 Toolchain preparation (x86_64 host to aarch64)

The reverse direction follows the same model.

```bash
# C toolchain for erofs-utils configure/linking and Kbuild CROSS_COMPILE.
apt install gcc-aarch64-linux-gnu g++-aarch64-linux-gnu

# Multiarch libuuid required by erofs-utils configure.
sudo dpkg --add-architecture arm64
sudo apt update
sudo apt install libuuid1:arm64 uuid-dev:arm64
```

Envd is pure Go with `CGO_ENABLED=0`; GOARCH cross-compiles it without these C packages. Scripts check missing toolchains (`${CROSS_PREFIX}gcc`, `uuid-dev:<arch>`) up front and print installation guidance instead of failing later with a large linker error.

<a id="5-wsl2-注意"></a>
## 5. WSL2 notes

A kernel source tree performs many small-file operations. WSL2 builds on `/mnt/<drive>/` (DrvFs) may incur extra filesystem overhead; no fixed slowdown factor is implied.

For **vmlinux**, the Makefile detects a kernel release containing `microsoft` together with a working directory under `/mnt/`. If `$HOME/linux-build/src` exists, it defaults `LINUX_BUILD_SRC` and `LINUX_BUILD_OUT` under `$HOME/linux-build/`, allowing the build to use a Linux-native filesystem.

<a id="documentation"></a>
<a id="文档"></a>
## 6. See also

- [vmlinux.md](../docs/vmlinux.md): kernel configuration, architecture differences and design decisions.
- `kuasar-sandbox/docs/release.md`: independent runtime/vmlinux versions and aggregate releases. Runtime-required artifacts from this directory are collected through `kuasar-sandbox/release/bin-inputs.manifest` into shared `bin/`.
