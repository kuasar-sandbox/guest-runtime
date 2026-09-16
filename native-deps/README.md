[English](README.md) | [简体中文](README_zh.md)

<a id="native-deps"></a>
# native-deps — build and maintenance

The native-deps directory builds upstream sources into three default artifact families consumed at runtime, not linked by the Go repositories: `mkfs.erofs`/`fsck.erofs` (erofs-utils), `vmlinux` (guest kernel) and `envd` (E2B guest agent). Their autotools/Kbuild/Go toolchains and independent upstream schedules are kept together in `guest-runtime/native-deps`. An additional `versitygw` gateway target is opt-in and not part of the default build.

The common pipeline is: pinned upstream tarball URL with optional SHA256 verification → shared cache and extraction → local patches (vmlinux, using `git am`) → build → `bin/<arch>/`.

This document covers targets, patch development, cross-compilation and cache/cleanup rules. Artifact-specific design belongs elsewhere: see [the kernel specification](../docs/vmlinux.md) for kernel configuration, and [Cloud Hypervisor](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/cloud-hypervisor.md) for the VMM and its patches.

Project-level aggregation runs `make -C kuasar-sandbox build`, which invokes this directory's `make build`, then collects artifacts according to `kuasar-sandbox/release/bin-inputs.manifest` into `kuasar-sandbox/bin/<arch>/` for E2E, Demo and local integration reuse.

## 1. Overview

### 1.1 Artifacts and pinned versions

| Artifact | Pinned upstream | Repository inputs | Consumers |
| --- | --- | --- | --- |
| `mkfs.erofs` / `fsck.erofs` | erofs-utils v1.9.1 | — | accelerator flattening, guest-runtime image construction, source diagnostics and accelerator tests |
| `vmlinux` | Linux 6.1.169 (LTS, cdn.kernel.org) | `deps/linux-patches/` and `deps/vmlinux/*.config` | sandboxer / sandbox-ctl guest kernel |
| `envd` | e2b-dev/runtime 2026.22 source tarball | — | Guest agent in `sandbox-runtime.bundle` |
| `versitygw` (opt-in) | versity/versitygw v1.5.0 | — | Local/single-node S3-compatible file-storage integration where required |

Pins live in Makefile variables (`EROFS_TARBALL`, `LINUX_TARBALL`, `ENVD_TARBALL`, and optional `VERSITYGW_TARBALL`), accepting `url#filename` or a local path. An empty corresponding `*_TARBALL_SHA256` skips verification. The current default EROFS, Linux and Envd inputs have hashes configured; the optional gateway hash must be supplied when required by the deployment's verification policy. Updating a version means updating those variables and revalidating patch application.

Use the normal Makefile targets to build the selected sources, then package the
matching outputs. From the repository root, `make release-runtime` depends on
`sandbox-runtime`, while `make release-vmlinux` depends only on `vmlinux`.
Calling `release.sh package` directly reuses prebuilt files; it does not rebuild
native dependencies, replace source checkouts or reset caches. Runtime and
Kernel remain independently built, packaged and selected.

Runtime inputs are `bin/<arch>/{sandbox-runtime.bundle,flatten-ctl}` and
`native-deps/bin/<arch>/mkfs.erofs`; Kernel uses
`native-deps/bin/<arch>/vmlinux`. Existing `RELEASE_BIN_DIR` and
`RELEASE_NATIVE_BIN_DIR` overrides select matching output directories.
`RELEASE_SANDBOXER_SOURCE_DIR`, `RELEASE_ACCELERATOR_SOURCE_DIR`,
`RELEASE_ENVD_SOURCE_DIR`, `RELEASE_EROFS_SOURCE_DIR` and
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
Linux/amd64 `github.com/kuasar-sandbox/guest-runtime/cmd/flatten-ctl` main
package, with clean Go VCS metadata matching the selected project commit.
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
EROFS, Linux, internal/shared-module and system records must reference their
own material directories. Kernel includes Linux `COPYING` and the complete
`LICENSES` tree from its selected source; the recorded COPYING digest must
agree with the bundled file.

Validation accepts only the selected unit's payloads: Runtime has
`sandbox-runtime.bundle`, `flatten-ctl` and `mkfs.erofs` under `bin/`;
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
Envd, mkfs and flatten-ctl into fixed private files. In-image ownership/modes,
Go main identities/targets and build records must match; init must also bind
the selected Sandboxer commit. Embedded mkfs and flatten-ctl must equal their
outer payloads. No archive executable is run. Kernel validation needs no EROFS
reader, Runtime payload or dependency checkout.

The matching `mkfs.erofs` link map supplies every actual external archive and
startup object to `EROFS-INPUTS.tsv`. The inventory retains their names and
file digests; source rows and per-input `system/<input>` notice directories
must cover exactly that set, including libc, libuuid, OpenSSL (libcrypto/libssl) and GCC/CRT inputs.
Packaging records installed Debian/RPM source-package identities and copies
their copyright, license and NOTICE files, including referenced common-license
texts. Package names or SPDX labels alone do not replace the texts. RPM package
and same-source sibling file listings must complete successfully; partial output
is not complete coverage. Installed package ownership provides attribution,
not dpkg/RPM file-digest or co-owner authentication.

The existing project CI template builds static libuuid from pinned,
checksum-verified util-linux source. Its provisioner retains `SOURCES.tsv`,
`MATERIALS.sha256` and a license directory beside the template inputs and
copies them with the library into each prepared slot. Packaging checks this
source-built library's inventory and digest. Missing, altered or unowned inputs
need the actual matching materials, not fabricated source records.

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
| `deps/build-{erofs,vmlinux,envd}.sh` | Source-build scripts; vmlinux uses a multi-stage STAGE dispatcher |
| `deps/build-versitygw.sh` | Optional gateway build |
| `deps/common.sh` | Shared tarball download, cache and extraction helpers |
| `deps/linux-patches/` | Architecture-neutral guest-kernel patches |
| `deps/vmlinux/*.config` | Common and per-architecture kernel configuration fragments |


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

### 1.4 Idempotency and caching

- **Artifact reuse:** Envd outputs are reused when their target files already exist. EROFS checks both executables and `bin/<arch>/.erofs-build-inputs` on every invocation: its recipe/Makefile/common helper, source pin or local tarball bytes, compiler/build tools, flags, pkg-config selection and the static link probe's selected archives/startup objects determine reuse. Unchanged inputs reuse the binaries; changed inputs re-extract the generated per-architecture EROFS tree and rebuild. A Git checkout there is refused, preserving patch development. Removing either binary also forces a rebuild. Build parallelism does not invalidate output. Kernel output is also governed by tracked inputs: changes to its build script, common/architecture config fragments or patches trigger configuration reevaluation and incremental Kbuild rather than unconditional existence-only skipping.
- **Tarball cache:** `build/tarball/` caches by filename; hits avoid downloading. Extraction uses an `.extracted` marker for idempotency.
- **`make clean`:** removes `bin/` and target build output while **retaining** tarball caches and architecture-neutral `build/src/*` source trees. Kernel source trees may contain unexported patch-development work and must not be silently discarded.

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

### 2.1 EROFS (`make erofs`)

`deps/build-erofs.sh` extracts erofs-utils into `build/<arch>/src/erofs-utils/`, keeping one tree per architecture because this autotools path does not support out-of-source builds. It runs `autoreconf` and `configure` with compression/FUSE/network features disabled, builds only the `lib`, `mkfs` and `fsck` subdirectories, and writes `bin/<arch>/{mkfs.erofs,fsck.erofs}`.

- The Runtime tool build skips the `mount`/`dump`/`fuse` subdirectories; it also avoids the v1.9.1 mount.erofs pthread-linking issue under `--disable-multithreading`. Standalone bundle validation separately requires a trusted host `dump.erofs` installation (§1.1).
- `--with-openssl` explicitly selects v1.9.1's existing EVP SHA-256 backend. Full SHA-256 deduplication, chunk sizes, uncompressed layout and disabled multithreading stay unchanged. The build verifies both upstream backend defines and refuses unavailable headers/libraries rather than selecting another backend.
- The target compiler must statically link OpenSSL (`libcrypto.a` and `libssl.a`, required by upstream configure) and `libuuid.a`. Debian/Ubuntu: `libssl-dev uuid-dev`; openEuler: `openssl-devel` supplies both OpenSSL archives, with the existing source-built static uuid. Other RPM distributions may split `openssl-static`. The preflight checks actual target linking, including private pkg-config dependencies, and prints dependency guidance on failure.
- Both output ELFs must have no `INTERP` or `NEEDED` entries. SHA-256 uses OpenSSL's built-in provider and needs no guest library, provider module or configuration file. Validate a new distribution/toolchain with a small chunked-image comparison in an empty root. OpenSSL can link unused loader/NSS code that produces glibc static-link warnings; this is not evidence that the SHA-256 path needs those services.
- OpenSSL is supplied by the target distribution, not vendored or independently upgraded here. Release provenance uses the actual link map, package/source identity and matching installed copyright/license texts (§1.1), including OpenSSL 3's Apache-2.0 notices. Missing license material fails packaging.
- `EROFS_BUILD_JOBS=2 make -j2 erofs` bounds each native sub-build for a small machine; the default remains `nproc`.
- Host build tools: `autoconf automake libtool pkg-config make gcc g++ binutils` (including `readelf`).

### 2.2 vmlinux (`make vmlinux`)

The target invokes the fetch, patch-application and build stages of `deps/build-vmlinux.sh` when its output needs rebuilding (§3). After applying `deps/linux-patches/*.patch`, it combines `deps/vmlinux/sandbox-common.config` with `sandbox-<arch>.config` into `arch/<kbuild_arch>/configs/sandbox_defconfig`, runs `make sandbox_defconfig` and `make olddefconfig` to resolve Kconfig dependencies, then `make -j$(nproc) <target>`, and copies out `bin/<arch>/vmlinux`.

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
- `HEAD == base`: apply all patches with `git am`.
- `HEAD = base + N`, where N equals patch count and commit subjects match patch files one by one: treat patches as already applied and skip idempotently.
- Any other state: stop and instruct the developer to save work with `make linux-patches-format`, then reset to the base tag only after preserving that work, and rerun.

The current kernel patch set is architecture-neutral and touches `drivers/virtio/virtio_balloon.c`; both architectures use the same patches and their own defconfig.

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

### 4.2 Toolchain preparation (x86_64 host to aarch64)

The reverse direction follows the same model.

```bash
# C toolchain for erofs-utils configure/linking and Kbuild CROSS_COMPILE.
apt install gcc-aarch64-linux-gnu g++-aarch64-linux-gnu

# Target static OpenSSL and libuuid, including pkg-config metadata.
sudo dpkg --add-architecture arm64
sudo apt update
sudo apt install libssl-dev:arm64 uuid-dev:arm64
```

Envd is pure Go with `CGO_ENABLED=0`; GOARCH cross-compiles it without these C packages. The script uses the target compiler for a static OpenSSL/uuid link probe before configuring. Cross builds default `PKG_CONFIG_LIBDIR` to the compiler's multiarch directories inside its sysroot, excluding host library directories. Set `PKG_CONFIG_LIBDIR`, `PKG_CONFIG_SYSROOT_DIR` and, if needed, `PKG_CONFIG_PATH` explicitly for other target sysroots. These overrides and resolved libraries are build/cache inputs; a host `.pc` file cannot make an incompatible target archive link successfully.

## 5. WSL2 notes

A kernel source tree performs many small-file operations. WSL2 builds on `/mnt/<drive>/` (DrvFs) may incur extra filesystem overhead; no fixed slowdown factor is implied.

For **vmlinux**, the Makefile detects a kernel release containing `microsoft` together with a working directory under `/mnt/`. If `$HOME/linux-build/src` exists, it defaults `LINUX_BUILD_SRC` and `LINUX_BUILD_OUT` under `$HOME/linux-build/`, allowing the build to use a Linux-native filesystem.

## 6. See also

- [vmlinux.md](../docs/vmlinux.md): kernel configuration, architecture differences and design decisions.
- [Release](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release.md): independent runtime/vmlinux versions and aggregate releases. Runtime-required artifacts from this directory are collected through `kuasar-sandbox/release/bin-inputs.manifest` into shared `bin/`.
