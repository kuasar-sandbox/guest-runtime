[English](README.md) | [简体中文](README_zh.md)

<a id="native-deps"></a>
# native-deps — build and maintenance

The default native-deps artifacts are `vmlinux` (Guest kernel) and `envd` (E2B Guest agent); `versitygw` is opt-in. Image export uses the Go EROFS library linked into `flatten-ctl`. Runtime bundle packing uses a preinstalled host mkfs, and independent validation uses fsck/dump. The project no longer maintains erofs-utils patches or builds and distributes mkfs.

The common pipeline is: pinned upstream tarball URL with optional SHA256 verification → shared cache and extraction → local patches (vmlinux, using `git am`) → build → `bin/<arch>/`.

This document covers targets, patch development, cross-compilation and cache/cleanup rules. Artifact-specific design belongs elsewhere: see [the kernel specification](../docs/vmlinux.md) for kernel configuration, and [Cloud Hypervisor](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/cloud-hypervisor.md) for the VMM and its patches.

Project-level aggregation runs `make -C kuasar-sandbox build`, which invokes this directory's `make build`, then collects artifacts according to `kuasar-sandbox/release/bin-inputs.manifest` into `kuasar-sandbox/bin/<arch>/` for E2E, Demo and local integration reuse.

## 1. Overview

### 1.1 Artifacts and pinned versions

| Artifact | Pinned upstream | Repository inputs | Consumers |
| --- | --- | --- | --- |
| `vmlinux` | Linux 6.1.169 (LTS, cdn.kernel.org) | `deps/linux-patches/` and `deps/vmlinux/*.config` | sandboxer / sandbox-ctl guest kernel |
| `envd` | e2b-dev/runtime 2026.22 source tarball | — | Guest agent in `sandbox-runtime.bundle` |
| `versitygw` (opt-in) | versity/versitygw v1.5.0 | — | Local/single-node S3-compatible file-storage integration where required |

Pins live in Makefile variables (`LINUX_TARBALL`, `ENVD_TARBALL`, and optional `VERSITYGW_TARBALL`), accepting `url#filename` or a local path. An empty corresponding `*_TARBALL_SHA256` skips verification. The current default Linux and Envd inputs have hashes configured; the optional gateway hash must be supplied when required by the deployment's verification policy. Updating a version means updating those variables and revalidating patch application.

Use the normal Makefile targets to build the selected sources, then package the
matching outputs. From the repository root, `make release-runtime` depends on
`sandbox-runtime`, while `make release-vmlinux` depends only on `vmlinux`.
Calling `release.sh package` directly reuses prebuilt files; it does not rebuild
native dependencies, replace source checkouts or reset caches. Runtime and
Kernel remain independently built, packaged and selected.

Runtime package inputs are `bin/<arch>/{sandbox-runtime.bundle,flatten-ctl}`;
the bundle is built earlier with a host `mkfs.erofs`, which is not a Runtime
payload. Kernel uses
`native-deps/bin/<arch>/vmlinux`. Existing `RELEASE_BIN_DIR` and
`RELEASE_NATIVE_BIN_DIR` overrides select matching output directories.
`RELEASE_SANDBOXER_SOURCE_DIR`, `RELEASE_ACCELERATOR_SOURCE_DIR`,
`RELEASE_ENVD_SOURCE_DIR` and
`RELEASE_LINUX_SOURCE_DIR` select the corresponding source trees;
`RELEASE_SANDBOX_INIT_BIN` and `RELEASE_ENVD_BIN` select the matching init
and Envd binaries for material collection and image consistency checks. Keep source versions, native link maps and these outputs
together. Native pin changes require updating the recipe, digests and source
records together; a path override is not permission to label different source
bytes as the configured version.

The Runtime source build uses `GOWORK=off` and the minimal
Accelerator/Sandboxer dependency closure. Internal records use a release version
only when its local Git tag identifies the selected commit, otherwise
`git:<commit>`; future target tags need not exist. `flatten-ctl` must be the
Linux `github.com/kuasar-sandbox/guest-runtime/cmd/flatten-ctl` main package
for the selected amd64 or arm64 target, with clean Go VCS metadata matching the
selected project commit and a versioned, checksummed external go-erofs build record.
The package records its requested Runtime or Kernel version separately from
source commits.

Go compiler selection is recorded separately in the guest-runtime, sandboxer
and Envd module directories. When these select different compiler versions,
each payload's actual version and the corresponding installed Go/bundled
dependency notices are collected. The parent module's context does not replace
a payload's own selection. Module materials use the effective replacements and
matching module checksums with the normal Go cache and routing. Existing
internal and Envd shared-module local replacements remain supported; other
unversioned third-party replacements are not supported in official packages.
Compiler distributions are neither downloaded nor authenticated by these
material collectors, and standalone validation does not require the payload's
compiler version.

Materials are isolated under `share/licenses/runtime` and
`share/sources/runtime`, or the separate `vmlinux` namespaces. Each unit
carries `SOURCES.tsv`, `GO-BUILD-INFO.tsv`, `GO-MODULES.tsv` and
`MATERIALS.sha256`. Collection rejects missing notices, unreadable subtrees,
partial traversals and collisions between different inputs. Project, Envd,
Linux, Go dependency and internal/shared-module records must reference their
own material directories. Kernel includes Linux `COPYING` and the complete
`LICENSES` tree from its selected source; the recorded COPYING digest must
agree with the bundled file.

Validation accepts only the selected unit's payloads: Runtime has
`sandbox-runtime.bundle` and `flatten-ctl` under `bin/`;
Kernel has only `bin/vmlinux`. It checks exact paths, duplicate entries,
types, root ownership, modes, inventory, source-record consistency and
checksums. Foreign unit materials and unrelated directories are refused before
extraction. A Runtime archive cannot replace the independently selected Kernel
or alter unrelated deployment-root modes.

Standalone Runtime validation uses trusted host `fsck.erofs` and
`dump.erofs` already installed on `PATH`. It does not download EROFS sources
or compile readers on demand. These readers must support the image format,
`fsck.erofs --extract` and `dump.erofs --path/--cat`.
Validation checks the bundle's alignment and prefix digest, verifies the
complete EROFS filesystem without extracting its tree, and reads only init,
Envd and flatten-ctl into fixed private files. In-image ownership/modes,
Go main identities/targets and build records must match; init must also bind
the selected Sandboxer commit. Embedded flatten-ctl must equal its outer
payload. An obsolete embedded or outer mkfs payload is rejected. No archive executable is run. Kernel validation needs no EROFS
reader, Runtime payload or dependency checkout.

#### Historical native-tool materials

Historical Runtime packages containing mkfs retain their original source, license and relinking materials; validate them with their matching release tools. Current packages do not claim these native tools as payloads. Existing artifacts remain readable; current Runtime builds use the Go generator and host tools.

#### Publishing and validation boundaries

The publisher supplies the selected project `SOURCE_SHA` to validation before
Tag/Release writes, uses the bundle's `release-notes.md` body and appends the
existing source/Preview markers. Producer-supplied notes may not contain those
reserved markers. Source selection, build/publish permission
separation and refusal to replace published assets remain required. Independent
validation is an offline bundle check: it does not fetch project/dependency Git
objects or Go modules, or compare notices with remote source trees. Checksums
and VCS records do not attest an arbitrary producer or isolate untrusted CI
candidates. These materials support release review, not legal certification.

`librocksdb`, accelerator's CGO link dependency, is built in that repository, not here.

### 1.2 Build-source layout

| Path | Role |
| --- | --- |
| `deps/build-{vmlinux,envd}.sh` | Source-build scripts; vmlinux uses a multi-stage STAGE dispatcher |
| `deps/build-versitygw.sh` | Optional gateway build |
| `deps/common.sh` | Shared tarball download, cache and extraction helpers |
| `deps/linux-patches/` | Architecture-neutral guest-kernel patches |
| `deps/vmlinux/*.config` | Common and per-architecture kernel configuration fragments |


### 1.3 Directory layout

```text
bin/
├── x86_64/                  x86_64 artifacts: vmlinux, envd
├── aarch64/                 aarch64 artifacts (same names)
└── <name>                   Symlink → <host-arch>/<name>, created/updated only for native builds

build/
├── tarball/                 Upstream tarballs, shared across architectures
├── src/
│   ├── linux/               Shared kernel source/git tree; patch-development work lives here
│   ├── e2b-infra/           Shared Envd sources; Go selects the target via GOARCH
│   └── versitygw/           Shared sources for the optional gateway
├── x86_64/
│   └── linux/               Kbuild O= output
└── aarch64/                 Corresponding target output
```

Native builds (host = target) create `bin/<name> → <arch>/<name>` for their public binary entries. Cross-builds do not update host entry symlinks, avoiding a link to a binary the host cannot execute. Optional gateway output appears only when that target is built.

### 1.4 Idempotency and caching

Envd reuses existing target files; the kernel uses scripts, configuration and patches for incremental Kbuild. Downloads remain in `build/tarball/` and are verified against expected digests. `make clean` removes `bin/` and target build output while preserving tarball caches and architecture-neutral source trees, including kernel patch-development work. Host erofs-utils is managed by the environment and is outside this project's native cache.

## 2. Build targets

```bash
make build      # =all: vmlinux + envd
make vmlinux    # Guest kernel
make envd       # E2B guest agent
make versitygw  # Optional gateway; not part of build
make clean      # See section 1.4
make help       # List targets
```

Build duration depends on the host, toolchain and cache state; these commands do not carry a fixed timing guarantee.

Workbench jobs pass the positive integer `KUASAR_BUILD_JOBS` to each recipe. Kernel compilation uses it instead of the host processor count; Envd passes it explicitly to `go build -p` even when `ENVD_GOFLAGS` is set. CPU quota alone does not change the processor count reported by `nproc`.

### 2.1 Host EROFS tools

Install erofs-utils from the host distribution, or select a host-executable mkfs through `BUILD_MKFS_EROFS`. Cross builds also use a host-architecture packer. The `make erofs` target, patched Plan ABI, private static-link recipe and associated cache are retired. Independent validation needs fsck/dump supporting the current format and `--extract`, `--path`, `--cat`; trusted CI may build unmodified upstream readers, but does not build or export mkfs.

### 2.2 vmlinux (`make vmlinux`)

The target invokes the fetch, patch-application and build stages of `deps/build-vmlinux.sh` when its output needs rebuilding (§3). After applying `deps/linux-patches/*.patch`, it combines `deps/vmlinux/sandbox-common.config` with `sandbox-<arch>.config` into `arch/<kbuild_arch>/configs/sandbox_defconfig`, runs `make sandbox_defconfig` and `make olddefconfig` to resolve Kconfig dependencies, then `make -j<jobs> <target>`, and copies out `bin/<arch>/vmlinux`. `jobs` is `KUASAR_BUILD_JOBS` when set, otherwise `nproc`.

- After `olddefconfig`, required DAX and architecture-specific options are checked. A requested critical option silently discarded by Kconfig fails the build.
- Make dependencies include build scripts, common/architecture configuration and tracked kernel patches. Changing these inputs reevaluates configuration and uses incremental Kbuild.
- x86_64 output is ELF (Kbuild target `vmlinux`); aarch64 output is the PE Image from `arch/arm64/boot/Image` (target `Image`). Both installed filenames are `vmlinux`.
- `build/src/linux/` is shared across architectures. Kbuild selects with `ARCH=` and writes to an architecture-specific `O=` directory.
- Host requirements: `bc bison flex make tar pkg-config gcc`, libelf headers (`libelf-dev` / `elfutils-libelf-devel`) and libssl headers (`libssl-dev` / `openssl-devel`). The headers support host Kbuild tools such as fixdep/sign-file; they are not linked into vmlinux.
- The two-fragment configuration contract and important enabled/disabled options are documented in [vmlinux.md](../docs/vmlinux.md), sections 2–3.

### 2.3 Envd (`make envd`)

`deps/build-envd.sh` extracts the e2b-dev/runtime source tarball into architecture-neutral `build/src/e2b-infra/` and builds `packages/envd` with `GOWORK=off GOOS=linux CGO_ENABLED=0 -trimpath -ldflags "-s -w"`, producing `bin/<arch>/envd`. GOARCH selects the target without a cross C toolchain.

- `make sandbox-runtime` includes it as `/opt/sandbox-runtime/bin/envd`, the E2B-profile guest data-plane agent on port 49983.
- Envd's `go.mod` may require a newer Go toolchain, such as `go 1.26.3`. `GOTOOLCHAIN=auto` downloads it when necessary; toolchain downloading requires GOSUMDB to be enabled and is rejected with `GOSUMDB=off`.
- Override `ENVD_TARBALL` to change the tag. In `url#filename` form, filename determines the cache name.

### 2.4 Optional gateway (`make versitygw`)

`deps/build-versitygw.sh` builds the configured gateway source for the selected Go architecture. This target is not in `build`. It is available for local/single-node `builder.files_storage` deployments that need an S3-compatible gateway rather than a cloud object store. Its source, hash and source-directory variables are `VERSITYGW_TARBALL`, `VERSITYGW_TARBALL_SHA256` and `VERSITYGW_SRC`.

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
- `HEAD == base`: apply all patches with `git am`, or succeed without changes when the patch set is empty.
- `HEAD = base + N`, where N equals patch count and commit subjects match patch files one by one: treat patches as already applied and skip idempotently.
- Any other state: stop and instruct the developer to save work with `make linux-patches-format`, then reset to the base tag only after preserving that work, and rerun.

No kernel patches are currently shipped. The patch pipeline remains architecture-neutral; both architectures use the same patch directory and their own defconfig.

An empty patch set with `HEAD != base` is rejected: the source may still contain a removed patch or unformatted work. Preserve that work and select fresh `LINUX_BUILD_SRC` and `LINUX_BUILD_OUT` paths for an unpatched build. `make clean` preserves the source tree and therefore does not remove previously applied patches. The explicit `linux-build` target remains available for building a development tree without applying the tracked patch set.

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

### 4.2 Cross toolchain preparation

Kbuild needs the target C toolchain, for example `gcc-aarch64-linux-gnu` on an x86_64 host. Envd and flatten-ctl cross-build with Go. Runtime packing always uses host erofs-utils; target static Libgcrypt, Libgpg-error and libuuid are no longer EROFS build prerequisites.

## 5. WSL2 notes

A kernel source tree performs many small-file operations. WSL2 builds on `/mnt/<drive>/` (DrvFs) may incur extra filesystem overhead; no fixed slowdown factor is implied.

For **vmlinux**, the Makefile detects a kernel release containing `microsoft` together with a working directory under `/mnt/`. If `$HOME/linux-build/src` exists, it defaults `LINUX_BUILD_SRC` and `LINUX_BUILD_OUT` under `$HOME/linux-build/`, allowing the build to use a Linux-native filesystem.

## 6. See also

- [vmlinux.md](../docs/vmlinux.md): kernel configuration, architecture differences and design decisions.
- [Release](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release.md): independent runtime/vmlinux versions and aggregate releases. Runtime-required artifacts from this directory are collected through `kuasar-sandbox/release/bin-inputs.manifest` into shared `bin/`.
