[English](build.md) | [简体中文](build_zh.md)

# Build and deliver Runtime and Kernel artifacts

This is the complete producer workflow for the two independent release units. Choose standalone development, selected-source integration or release reproduction explicitly; use the selected compiler, dependencies and input binaries throughout. [Runtime bundle](runtime-bundle.md) and [Kernel](vmlinux.md) define delivered bytes and compatibility. [Native maintenance](../native-deps/README.md) owns the patch-development working tree and cleanup rules.

Unless a subsection specifies `native-deps/`, run commands from the `guest-runtime/` repository root. Keep the selected source set and target architecture fixed through validation and packaging.

## Choose a source and dependency mode

| Mode | Accelerator | sandbox-init |
| --- | --- | --- |
| Standalone CLI development | Obtainable versions in `go.mod`. | Not needed to build only flatten-ctl. |
| Coordinated source development | Selected guest-runtime + accelerator Go workspace. | Built from this repository with the selected workspace. |
| Runtime release reproduction | Clean checkout matching `ACCELERATOR_VERSION`; the release target generates the selected workspace. | Rebuilt from the same selected guest-runtime commit as flatten-ctl. |

Release targets rebuild both owned Go payloads; ordinary assembly may reuse explicitly selected development binaries.

## Source workspace

The selected-source Go build uses a workspace with the sibling `accelerator` module. Standalone `go.mod` resolves a published version. Both release and development builds compile `sandbox-init` from this repository:

```text
<workspace>/
├── guest-runtime/
└── accelerator/
```

For coordinated development, use compatible `main` revisions from these sibling repositories. To reproduce a released composition, use the exact component tags selected by the corresponding [project aggregate release](https://github.com/kuasar-sandbox/kuasar-sandbox/releases) rather than independently choosing GitHub Latest tags. The complete six-repository workspace is documented in the [project README](https://github.com/kuasar-sandbox/kuasar-sandbox).

Standalone `flatten-ctl` builds resolve the obtainable versions committed in `go.mod`.
Coordinated Preview builds use only the selected `guest-runtime` and `accelerator`
source modules. Generate that closure before building or testing:

```bash
GOWORK="$(python3 ../kuasar-sandbox/ci/integration/go_workspace.py --root .. --owners guest-runtime)" || exit 1
export GOWORK
```

The workspace validates module paths without rewriting `go.mod`/`go.sum`. Runtime
publication builds `sandbox-init` from `cmd/sandbox-init`; Sandboxer and Connector are not
Runtime Go workspace members. Its default output is `bin/<arch>/sandbox-init`.
Envd and versitygw keep their independent Go module builds. The standalone Kernel
target uses native inputs and does not join a Go workspace. `go-erofs` remains a
versioned external dependency.

## Build prerequisites

Run the full Runtime assembly workflow on a Linux host (including WSL2 or a Linux container): the Makefile executes its host `runtime-bundle` helper with `GOOS=linux`. Cross-compilation below changes the payload architecture, not the host operating system.

Local builds use environment-provided Go and inherit its `GOROOT` and `GOTOOLCHAIN` selection. Release orchestration requires a working host `gh` with `api --slurp` support on `PATH`. Guest native products follow their selected source and recipe; host EROFS packing and validation tools are installed by the build environment.

CI selects a verified, published Workbench once in trusted preflight and passes that immutable image selection with the exact framework revision to the build jobs. Runtime and VMLinux use native `ubuntu-24.04` x86_64 and `ubuntu-24.04-arm` aarch64 runners. Compilation, ordinary tests, and packaging run through the shared Workbench action as the runner's ordinary UID, with a read-only root filesystem, dropped capabilities, private `/src`, `/build`, and `/work`, and an explicit CPU budget. Publisher credentials stay on the host. Existing architecture-specific test coverage and separate hardware gates still apply; a native ARM build does not imply ARM KVM coverage. Runtime publication uses the verified Workbench's EROFS readers without compiling host helpers.

Building `sandbox-runtime.bundle` requires a **host-architecture** `mkfs.erofs` executable. Install `erofs-utils` on the build host or set:

```bash
BUILD_MKFS_EROFS=/path/to/host/mkfs.erofs make sandbox-runtime
```

The host packer only assembles the runtime bundle at build time and must match the build host. `flatten-ctl` generates EROFS through the pure Go `github.com/kuasar-sandbox/go-erofs` library; neither the Guest nor the Runtime release package needs an accompanying `mkfs.erofs`.

Other prerequisites and native-source locations are documented in [Native build and maintenance](../native-deps/README.md).

## Build

```bash
GOWORK=off make native-deps                # VMLinux, Envd and native inputs
make flatten-ctl                # OCI/directory -> deterministic EROFS builder
make build                      # assemble sandbox-runtime.bundle
make sandbox-runtime            # assemble with the selected sandbox-init binary
make build TARGET_ARCH=aarch64  # cross-build where documented dependencies support it
```

`make sandbox-runtime` consumes:

- `bin/<arch>/sandbox-init`;
- `native-deps/bin/<arch>/envd`;
- `bin/<arch>/flatten-ctl`;
- `BUILD_MKFS_EROFS` as the host image packer.

Ordinary assembly reuses executable input binaries; it does not refresh existing `sandbox-init` or Envd after source or pin changes. `make build` rebuilds `sandbox-init` and `flatten-ctl` first, while direct `make sandbox-runtime` reuses it if executable. Use the release-specific dependency selection described [below](#reproduce-release-dependencies-exactly) for release reproduction.

When `sandbox-init`, Envd or `flatten-ctl` is absent or not executable, the Makefile invokes its corresponding build target. Host `mkfs.erofs` must be installed on `PATH` or selected by `BUILD_MKFS_EROFS`; otherwise the Runtime build fails explicitly. The project no longer maintains erofs-utils patches or builds and distributes mkfs. Workbench and host environments may install distribution erofs-utils packages for build-time packing and independent validation.

## Outputs

| Output | Build target |
| --- | --- |
| `bin/<arch>/flatten-ctl` | `make flatten-ctl` |
| `bin/<arch>/sandbox-runtime.bundle` | `make sandbox-runtime` |
| `native-deps/bin/<arch>/vmlinux` | `make native-deps` |
| `native-deps/bin/<arch>/envd` | `make native-deps` |

## Assemble the Runtime image

Common entry points:

```bash
make flatten-ctl                 # Build the target-architecture guest tool.
make native-deps                 # Build vmlinux / envd.
make sandbox-runtime             # Produce bin/<arch>/sandbox-runtime.bundle.
make build                       # Build flatten-ctl + sandbox-runtime.
make build TARGET_ARCH=aarch64
```

A **host-executable `mkfs.erofs` must already be available** before `make sandbox-runtime`. Install host erofs-utils or set `BUILD_MKFS_EROFS`. The project does not build mkfs. It only packs the runtime bundle; the target Guest uses the Go generator linked into static `flatten-ctl`, with no `GUEST_MKFS_EROFS` payload.

The target's input and build sequence is:

1. Resolve and check `BUILD_MKFS_EROFS`. Use the explicitly selected tool or the preinstalled executable on host PATH. A missing host tool fails before image assembly.
2. If the selected `SANDBOX_INIT` (default `bin/<arch>/sandbox-init`) is absent, build it with `make sandbox-init`.
3. Resolve target `ENVD` (default `native-deps/bin/<arch>/envd`), building it when missing.
4. Resolve this repository's target `FLATTEN_CTL` (default `bin/<arch>/flatten-ctl`), building it when missing.
5. Verify the selected `flatten-ctl` build record links the external go-erofs module. A binary lacking that versioned, checksummed dependency fails explicitly; run `make flatten-ctl` or select a matching binary. Assemble the staging tree and use host `BUILD_MKFS_EROFS` to create temporary runtime EROFS.
6. The host `runtime-bundle` tool copies EROFS, adds PMEM alignment padding, computes SHA256 and appends the empty-marker ZIP, renaming its completed temporary file to `bin/<arch>/sandbox-runtime.bundle` atomically.

That final rename is not a rollback guarantee for the whole Make target: the Makefile removes the previous bundle before running mkfs. A later failure can leave no bundle. Build in an isolated checkout/output directory, validate the result, then deploy it under a new immutable path for future launches. Retain old artifacts still referenced by running VMs or snapshots; do not overwrite files in use.

See [the native-build workflow](../native-deps/README.md) for host EROFS prerequisites and Envd builds and the [Guest ABI](guest-abi.md) for sandbox-init.

### Architectures

The runtime image is built for the target architecture. Its build-directory filename is always `sandbox-runtime.bundle`. `/sbin/init`, `envd`, `flatten-ctl` inside the EROFS prefix must all be executable files for that same target architecture.

`TARGET_ARCH=amd64` normalizes to `x86_64`; `TARGET_ARCH=arm64` normalizes to `aarch64`. Cross-builds do not update host-architecture symlinks, avoiding host entry points to non-runnable binaries. The host packing tool remains separate from this target payload.

## Build the kernel

`make vmlinux` invokes fetch, patch application and build when the output is missing or tracked kernel inputs are newer. Explicit `linux-fetch`, `linux-patches-apply`, `linux-patches-format` and `linux-build` targets use `native-deps/deps/build-vmlinux.sh`, with the phase selected by `STAGE`: run these native targets with `make -C native-deps <target>` from the repository root, or run `make <target>` inside native-deps.

```text
fetch         Download and verify linux-6.1.169.tar.gz, with optional shared TARBALL_CACHE;
              extract the cross-architecture shared source tree at build/src/linux;
              git init, commit the imported tree and tag linux-patches-base.
patches-apply Apply deps/linux-patches/*.patch with git am. An empty patch set
              requires HEAD == linux-patches-base.
              Already-applied patches are an idempotent no-op; unformatted or
              divergent work is not overwritten.
build         Concatenate sandbox-common.config + sandbox-<arch>.config into
              arch/<kbuild_arch>/configs/sandbox_defconfig; run make sandbox_defconfig
              and make olddefconfig to resolve dependencies, then make -j<jobs>
              <kbuild_target> (x86_64: vmlinux ELF; arm64: Image PE), copying the output
              to bin/<arch>/vmlinux.
```

`jobs` uses the positive integer `KUASAR_BUILD_JOBS` supplied by the task. Without an explicit task budget it defaults to `nproc`; a container CPU quota does not by itself constrain that value.

Synthetic source-import author/committer timestamps and patch-application committer timestamps default to `SOURCE_DATE_EPOCH` (zero when unset). Explicit Git date overrides and patch author dates are preserved. These defaults apply only to the recipe's Git subprocesses, keeping SCM-derived kernel version strings reproducible without changing the kernel configuration or developer commits.

Incrementality is input-aware: the native Makefile tracks the build/common scripts, both selected fragments, the patch directory and tracked patches. An existing output alone does not suppress a rebuild after those inputs change. Every explicit `linux-build` re-resolves configuration; Kbuild compilation remains incremental. Removing the output or cleaning before `make vmlinux` forces regeneration.

**Patch development:** inside native-deps, `make linux-fetch` imports the source and creates `linux-patches-base`; edit `build/src/linux/` and commit; run `make linux-patches-format` to export commits into `deps/linux-patches/*.patch`; then run `make vmlinux` to apply/build. See [the native-build guide](../native-deps/README.md#kernel-patch-development) and [Configuration system](vmlinux.md#configuration-system) for idempotence and sanity rules. Patches are architecture-neutral and shared by x86_64 and arm64. Preserve unformatted work rather than resetting it merely to make a build proceed.

### Host build dependencies

```text
bc bison flex make tar pkg-config gcc
libelf-dev / elfutils-libelf-devel
libssl-dev  / openssl-devel
```

Fetching and patch development also require Git and the download/archive tools listed in the native-build guide. Cross-building additionally requires `gcc-aarch64-linux-gnu` or `gcc-x86-64-linux-gnu`, according to direction.

## Pinned native inputs and package validation

| Artifact | Pinned upstream | Repository inputs | Consumers |
| --- | --- | --- | --- |
| `vmlinux` | Linux 6.1.169 (LTS, cdn.kernel.org) | `deps/linux-patches/` and `deps/vmlinux/*.config` | sandboxer / sandbox-ctl guest kernel |
| `envd` | e2b-dev/runtime 2026.22 source tarball | — | Guest agent in `sandbox-runtime.bundle` |
| `versitygw` (opt-in) | versity/versitygw v1.5.0 | — | Local/single-node S3-compatible file-storage integration where required |

Pins live in Makefile variables (`LINUX_TARBALL`, `ENVD_TARBALL`, and optional `VERSITYGW_TARBALL`), accepting `url#filename` or a local path. An empty corresponding `*_TARBALL_SHA256` skips verification. The current default Linux and Envd inputs have hashes configured; the optional gateway hash must be supplied when required by the deployment's verification policy. Updating a version means updating those variables and revalidating patch application. A changed pin alone does not invalidate existing binaries or extracted sources: Envd extraction reuses its `.extracted` marker, and kernel fetch reuses `linux-patches-base`. Preserve local work and select fresh source/output paths when changing upstream inputs; `make clean` does not reset these source caches.

Use the normal Makefile targets to build the selected sources, then package the
matching outputs. From the repository root, `make release-runtime` prepares the
selected dependencies, rebuilds `flatten-ctl` and invokes `sandbox-runtime` before
packaging, while `make release-vmlinux` depends only on `vmlinux`.
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
together. Native pin changes require updating the recipe, input digests and license collection together; a path override is not permission to label different source
bytes as the configured version.

Runtime Go builds use a workspace containing exactly the selected guest-runtime and accelerator modules. `sandbox-init` and `flatten-ctl` are both built from this module; sandboxer is not a Runtime Go workspace member. `flatten-ctl` must identify the Linux `github.com/kuasar-sandbox/guest-runtime/cmd/flatten-ctl` main package and selected amd64 or arm64 target. The current producer checks its own clean Go VCS metadata. Normal binary build information and the versioned, checksummed external go-erofs dependency remain intact; no separate source records are packaged.

License collection resolves the Go compiler selection for the two guest-runtime binaries together and separately for Envd. When these select different compiler versions,
each payload's actual version and the corresponding installed Go/bundled
dependency notices are collected. The parent module's context does not replace
a payload's own selection. Module materials use the effective replacements and
matching module checksums with the normal Go cache and routing. Selected internal modules use the explicit workspace, while Envd keeps its isolated shared-module replacements; other
unversioned third-party replacements are not supported in official packages.
Compiler distributions are neither downloaded nor authenticated by these
material collectors, and standalone validation does not require the payload's
compiler version.

Licenses and notices are isolated under `share/licenses/runtime` or `share/licenses/vmlinux`. Collection rejects missing notices, unreadable subtrees, partial traversals and collisions between different inputs. Kernel includes Linux `COPYING` and the complete `LICENSES` tree from its selected source. No source tables or historical build/test records are shipped.

Validation accepts only the selected unit's payloads: Runtime has `sandbox-runtime.bundle` and `flatten-ctl` under `bin/`; Kernel has `bin/vmlinux` and its runtime identity file `bin/vmlinux.sha256`. It checks exact paths, duplicate entries, types, root ownership, permissions and payload integrity. Foreign unit materials and unrelated directories are refused before extraction. A Runtime archive cannot replace the independently selected Kernel or alter unrelated deployment-root modes.

Standalone Runtime validation uses trusted host `fsck.erofs` and
`dump.erofs` already installed on `PATH`. It does not download EROFS sources
or compile readers on demand. These readers must support the image format,
`fsck.erofs --extract` and `dump.erofs --path/--cat/--ls`.
Validation checks the bundle's alignment and prefix digest, verifies the
complete EROFS filesystem without extracting its tree, and reads only init,
Envd and flatten-ctl into fixed private files. In-image ownership/modes,
Go main identities and target architectures must match. Before publication, embedded init and Envd must equal the actual selected input binaries; published package consumption does not require source provenance. Embedded flatten-ctl must equal its outer
payload. An obsolete embedded or outer mkfs payload is rejected. No archive executable is run. Kernel validation needs no EROFS
reader, Runtime payload or dependency checkout.

### Historical native-tool materials

Historical Runtime packages containing mkfs retain their original source, license and relinking materials; validate them with their matching release tools. Current packages do not claim these native tools as payloads. Existing artifacts remain readable; current Runtime builds use the Go generator and host tools.

### Publishing and validation boundaries

The publisher checks the current project's build output before Tag/Release writes, uses human release notes, verifies uploaded asset sizes/digests and refuses to replace different existing bytes. Source/Preview markers and independent `SHA256SUMS` are not published. Current source selection and acceptance remain pipeline inputs. Independent package consumption uses native GitHub asset integrity and offline payload checks without fetching historical Git or Actions evidence. Licenses remain part of the archive.

`librocksdb`, accelerator's CGO link dependency, is built in that repository, not here.

## Native target commands

Run the following native targets from `guest-runtime/native-deps/`. The root `make native-deps` delegates to this directory; the host image packer is supplied by the environment.

### Build targets

```bash
make build      # =all: vmlinux + envd
make vmlinux    # Guest kernel
make envd       # E2B guest agent
make versitygw  # Optional gateway; not part of build
make clean      # Remove native output; preserve source trees and tarballs
make help       # List targets
```

`make clean` here removes the selected `LINUX_BUILD_OUT` and the entire native `bin/` directory (all architectures), but preserves `build/src/` and `build/tarball/`. Root-level `make clean` first removes this repository's root `bin/` and `build/`, then delegates to this native cleanup; it does not delete the default native source trees. See [idempotency and caching](../native-deps/README.md#idempotency-and-caching) before resetting any cached source or patch work.

Build duration depends on the host, toolchain and cache state; these commands do not carry a fixed timing guarantee.

Workbench jobs pass the positive integer `KUASAR_BUILD_JOBS` to each recipe. Kernel compilation uses it instead of the host processor count; Envd passes it explicitly to `go build -p` even when `ENVD_GOFLAGS` is set. CPU quota alone does not change the processor count reported by `nproc`.

#### Host EROFS tools

Install erofs-utils from the host distribution, or select a host-executable mkfs through `BUILD_MKFS_EROFS`. Cross builds also use a host-architecture packer. The `make erofs` target, patched Plan ABI, private static-link recipe and associated cache are retired. Independent validation needs fsck/dump supporting the current format and `--extract`, `--path`, `--cat`, `--ls`; trusted CI may build unmodified upstream readers, but does not build or export mkfs.

#### vmlinux (`make vmlinux`)

The target invokes the fetch, patch-application and build stages of `deps/build-vmlinux.sh` when its output needs rebuilding ([Kernel patch development](../native-deps/README.md#kernel-patch-development)). After applying `deps/linux-patches/*.patch`, it combines `deps/vmlinux/sandbox-common.config` with `sandbox-<arch>.config` into `arch/<kbuild_arch>/configs/sandbox_defconfig`, runs `make sandbox_defconfig` and `make olddefconfig` to resolve Kconfig dependencies, then `make -j<jobs> <target>`, and copies out `bin/<arch>/vmlinux`. `jobs` is `KUASAR_BUILD_JOBS` when set, otherwise `nproc`.

- After `olddefconfig`, required DAX and architecture-specific options are checked. A requested critical option silently discarded by Kconfig fails the build.
- Make dependencies include build scripts, common/architecture configuration and tracked kernel patches. Changing these inputs reevaluates configuration and uses incremental Kbuild.
- x86_64 output is ELF (Kbuild target `vmlinux`); aarch64 output is the PE Image from `arch/arm64/boot/Image` (target `Image`). Both installed filenames are `vmlinux`.
- `build/src/linux/` is shared across architectures. Kbuild selects with `ARCH=` and writes to an architecture-specific `O=` directory.
- Host requirements: `bc bison flex make tar pkg-config gcc`, libelf headers (`libelf-dev` / `elfutils-libelf-devel`) and libssl headers (`libssl-dev` / `openssl-devel`). The headers support host Kbuild tools such as fixdep/sign-file; they are not linked into vmlinux.
- The two-fragment configuration contract and important enabled/disabled options are documented in the kernel [configuration system](vmlinux.md#configuration-system) and [architecture differences](vmlinux.md#architecture-differences).

#### Envd (`make envd`)

`deps/build-envd.sh` extracts the e2b-dev/runtime source tarball into architecture-neutral `build/src/e2b-infra/` and builds `packages/envd` with `GOWORK=off GOOS=linux CGO_ENABLED=0 -trimpath -ldflags "-s -w"`, producing `bin/<arch>/envd`. GOARCH selects the target without a cross C toolchain.

- `make sandbox-runtime` includes it as `/opt/sandbox-runtime/bin/envd`, the E2B-profile guest data-plane agent on port 49983.
- Envd's `go.mod` may require a newer Go toolchain, such as `go 1.26.3`. `GOTOOLCHAIN=auto` downloads it when necessary; toolchain downloading requires GOSUMDB to be enabled and is rejected with `GOSUMDB=off`.
- Override `ENVD_TARBALL` to change the tag. In `url#filename` form, filename determines the cache name.

#### Optional gateway (`make versitygw`)

`deps/build-versitygw.sh` builds the configured gateway source for the selected Go architecture. This target is not in `build`. It is available for local/single-node `builder.files_storage` deployments that need an S3-compatible gateway rather than a cloud object store. Its source, hash and source-directory variables are `VERSITYGW_TARBALL`, `VERSITYGW_TARBALL_SHA256` and `VERSITYGW_SRC`.

## Cross-compilation

### TARGET_ARCH

| Value | Alias | GOARCH | KERNEL_ARCH |
| --- | --- | --- | --- |
| `x86_64` | `amd64` | `amd64` | `x86_64` |
| `aarch64` | `arm64` | `arm64` | `arm64` |

The default is `uname -m`. When `HOST_ARCH != TARGET_ARCH`, cross-compilation is enabled and `CROSS_PREFIX` is derived as `<target>-linux-gnu-` (explicit overrides are supported). Outputs go to `bin/<target>/`; host `bin/` symlinks are not updated. Running target binaries requires a suitable native host.

```bash
make TARGET_ARCH=aarch64 build
```

### Cross toolchain preparation

Kbuild needs the target C toolchain, for example `gcc-aarch64-linux-gnu` on an x86_64 host. Envd and flatten-ctl cross-build with Go. Runtime packing always uses host erofs-utils; target static Libgcrypt, Libgpg-error and libuuid are no longer EROFS build prerequisites.

## WSL2 notes

A kernel source tree performs many small-file operations. WSL2 builds on `/mnt/<drive>/` (DrvFs) may incur extra filesystem overhead; no fixed slowdown factor is implied.

For **vmlinux**, the Makefile detects a kernel release containing `microsoft` together with a working directory under `/mnt/`. If `$HOME/linux-build/src` exists, it defaults `LINUX_BUILD_SRC` and `LINUX_BUILD_OUT` under `$HOME/linux-build/`, allowing the build to use a Linux-native filesystem.

## Reproduce release dependencies exactly

`make release-runtime` rebuilds `sandbox-init` and `flatten-ctl` from this selected source. The package contains `bin/sandbox-init`; the validator requires it to equal the embedded `/sbin/init` and identify `github.com/kuasar-sandbox/guest-runtime/cmd/sandbox-init`.

`make release-runtime` also checks `ACCELERATOR_VERSION` against the clean sibling accelerator checkout and generates the two-module workspace before compilation. It rebuilds both Go binaries and uses those same outputs for assembly and packaging, regardless of an inherited `GOWORK=off` or old `FLATTEN_CTL`/`SANDBOX_INIT`. Set `KUASAR_PLATFORM_DIR` if the project checkout is not at `../kuasar-sandbox`. Conflicting source directories or missing tags/modules fail before the build.

## Kernel source and licensing

A VMLinux release must be traceable to its public kernel source version, configuration, project patch set, toolchain, and source commit. Kernel patches and copied kernel material retain their upstream copyright and GPL obligations. Project-original build scripts do not relicense the Linux kernel.

The Runtime bundle may contain software under multiple licenses. Its package and native-dependency inputs, notices, source availability, and redistribution obligations must be reviewed as part of the release contract. Never add an internal-only package, private CA, SSH host key, machine identity, production credential, or untraceable prebuilt binary to the guest image.

The repository-wide license boundaries are described in [`LICENSE_SCOPE.md`](../LICENSE_SCOPE.md). Native build details and source locations are documented in [Native build and maintenance](../native-deps/README.md).

## Release selection and validation boundary

The selected owner source explicitly declares its compiled E2E helper inventory in `release/helper-inputs.json`. The current inventory is empty. Future helpers and their build recipe belong in this owner declaration; aggregate packaging discovers them without a platform filename list.

Runtime and VMLinux component releases are built from selected source refs and exact commits. Preview releases are GitHub prereleases for development and evaluation; mainline Stable releases are coordinated independently for each release unit. The project aggregate release always selects exact tags and does not rely on GitHub Latest, then validates the selected composition through project-level integration tests and released-asset testing.

See the [project release documentation](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release.md) and the [latest Stable aggregate release](https://github.com/kuasar-sandbox/kuasar-sandbox/releases/latest).

## Selected sandbox-init and helper provenance

Runtime publication builds both owned Go executables from its admitted source and verifies their current clean commit, main package and target architecture. It packages `bin/sandbox-init` and verifies byte identity with `/sbin/init` in the Runtime image. After verifying the complete public Stable `runtime-vX.Y.Z` release, the publisher creates root module tag `vX.Y.Z` at the same commit; it rejects conflicts and safely resumes an interrupted publication. Preview and Kernel releases do not create root module tags.