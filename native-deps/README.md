[English](README.md) | [简体中文](README_zh.md)

# Native source and kernel patch maintenance

The production target, input and packaging workflow is in [Build](../docs/build.md). This local guide owns native source/cache layout, safe incremental cleanup and the kernel patch-development working tree. Run its Make targets from `guest-runtime/native-deps/`.

The default native-deps artifacts are `vmlinux` (Guest kernel), `envd` (E2B Guest agent), and the private `systemd/` installation; `versitygw` is opt-in. Image export uses the Go EROFS library linked into `flatten-ctl`. Runtime bundle packing uses a preinstalled host mkfs, and independent validation uses fsck/dump. The project no longer maintains erofs-utils patches or builds and distributes mkfs.

The common pipeline is: pinned upstream tarball URL with optional SHA256 verification → shared cache and extraction → local patches (vmlinux, using `git am`) → build → `bin/<arch>/`.

This document covers patch development and source/cache cleanup. Production targets, cross-compilation and packaging are defined in [Build](../docs/build.md). Artifact-specific design belongs elsewhere: see [the kernel specification](../docs/vmlinux.md) for kernel configuration, and [Cloud Hypervisor](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/vmm.md) for the VMM and its patches.

Project-level aggregation runs `make -C kuasar-sandbox build`, which invokes this directory's `make build`, then collects artifacts according to `kuasar-sandbox/release/inputs/bin-inputs.yaml` into `kuasar-sandbox/bin/<arch>/` for E2E, Demo and local integration reuse.

## Build-source layout

| Path | Role |
| --- | --- |
| `deps/build-{vmlinux,envd}.sh` | Source-build scripts; vmlinux uses a multi-stage STAGE dispatcher |
| `deps/build-systemd.sh`, `deps/systemd-lock.json`, `deps/package-systemd.py` | Fixed native systemd/D-Bus recipe, source pins, private ELF closure and notice inventory |
| `deps/build-versitygw.sh` | Optional gateway build |
| `deps/common.sh` | Shared tarball download, cache and extraction helpers |
| `deps/linux-patches/` | Architecture-neutral guest-kernel patches |
| `deps/vmlinux/*.config` | Common and per-architecture kernel configuration fragments |

## Directory layout

```text
bin/
├── x86_64/                  x86_64 artifacts: vmlinux, envd, systemd/
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
│   └── systemd/             Disposable source/build/stage directories and input receipt
└── aarch64/                 Corresponding target output
```

Native builds (host = target) create `bin/<name> → <arch>/<name>` for their public binary entries. Cross-builds do not update host entry symlinks, avoiding a link to a binary the host cannot execute. Optional gateway output appears only when that target is built.

## Idempotency and caching

Envd reuses existing target files; the kernel uses scripts, configuration and patches for incremental Kbuild. Downloads remain in `build/tarball/` and are verified against expected digests. `make clean` removes `bin/` and target build output while preserving tarball caches and architecture-neutral source trees, including kernel patch-development work. Host erofs-utils is managed by the environment and is outside this project's native cache.

## Private systemd installation

`make systemd` builds systemd **257.13**, commit `70b5d110be7702afc4dbce012f60d49506d513da`, and D-Bus **1.14.10**. `deps/systemd-lock.json` fixes both archive digests, enabled features and explicit dlopen/NSS dependencies. Source archive overrides (`SYSTEMD_TARBALL`, `DBUS_TARBALL`) may select local copies, but cannot bypass the locked SHA256. Optional builder packages do not silently enable additional systemd features.

This target runs natively on x86_64 and aarch64. Cross-assembly uses an installation produced by a native builder through the top-level `SYSTEMD_PAYLOAD` override; it does not execute or infer target DSOs on a different architecture. The native builder requires these packages in addition to the common build tools:

```text
build-essential meson ninja-build pkg-config python3-jinja2 gperf
libcap-dev libmount-dev libseccomp-dev libblkid-dev libacl1-dev
liblz4-dev libzstd-dev liblzma-dev libpcre2-dev libssl-dev
libexpat1-dev libsystemd-dev busybox-static patchelf xz-utils
curl ca-certificates binutils
```

The production Workbench must contain those dependencies before selection. The recipe installs no host packages and works under an ordinary UID with a read-only container root, dropped capabilities and a writable build directory. `KUASAR_BUILD_JOBS` supplies the explicit compiler budget.

The output is `bin/<arch>/systemd/`, representing the final `/run/sandbox-runtime/systemd` prefix. It contains manager, executor, shutdown helper, journals, tools, units, generators, system bus and a complete dynamic dependency tree. Every target ELF is checked without executing it. The matching loader is copied unchanged; ordinary dynamic files receive the private PT_INTERP and library search paths. Runtime does not depend on the user's libc or a global `LD_LIBRARY_PATH`. The only external configuration symlink is the reviewed `/etc/environment` compatibility entry.

The input receipt records recipe hashes, compiler/tool versions, build settings and installed package identities. Existing output is reused only when that receipt and the complete installation validate. Rebuilds re-extract the verified source archives and use a clean DESTDIR; `build/<arch>/systemd/src/` is disposable and is not a patch-development worktree. Removing it does not affect the kernel's maintained source tree.

`share/sandbox-runtime/systemd-manifest.json` records every directory, file, symlink, file digest, ELF loader, source pin, dependency package/source version and notice path. Notices include upstream systemd/D-Bus material, dependency copyrights and referenced common license texts. Release validation independently checks the installed EROFS tree against that manifest and copies the same notices to `share/licenses/runtime/systemd-runtime/`.

`dbus-daemon-launch-helper` is the sole setuid file, mode 4755 and root-owned after EROFS packing. It checks the caller's real UID against the configured messagebus account before privileged activation. The active Runtime projection must retain that reviewed permission and must not be nosuid; no other tool gains privilege bits or file capabilities. The verifier reads raw EROFS inode mode because dump.erofs's formatted Access field omits special permission bits.

The source lock includes the hash of `deps/systemd-patches/0001-private-configuration-search-path.patch`. It inserts the compiled private vendor directory after `/etc` and `/run`, before rootfs vendor directories, in systemd's shared configuration search paths. Without this patch, setting `--prefix` alone leaves sysusers, tmpfiles, preset and journald defaults undiscovered. Patches are verified before applying to fresh upstream sources, and their identities remain in the installation manifest.

Run dependency-independent tamper tests with `make test`. A native Docker host can additionally qualify default configuration discovery and traditional D-Bus activation without a rootfs libc:

```bash
python3 -B deps/test-systemd-config.py \
  --payload "$PWD/bin/$(uname -m)/systemd" \
  --output /tmp/systemd-config-qualification
python3 -B deps/test-systemd-dbus.py \
  --payload "$PWD/bin/$(uname -m)/systemd" \
  --output /tmp/systemd-dbus-qualification
```

Each output directory must be new. The tests remove only their own temporary container/image and retain evidence. Configuration checks create messagebus through sysusers, apply tmpfiles and presets, preserve rootfs vendor services and verify administrator overrides. D-Bus checks cover the helper UID guard, activation under a non-root account and private-prefix code mappings. Full PID 1, reexec, shutdown and E/S behavior are qualified by the Guest VM integration tests; these focused tests do not replace them.

## Kernel patch development

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

Both architectures apply the order-0 balloon batching patch described in [kernel balloon behavior](../docs/vmlinux.md#balloon-behavior-under-memory-pressure). The patch pipeline remains architecture-neutral; both architectures use the same patch directory and their own defconfig.

An empty patch set with `HEAD != base` is rejected: the source may still contain a removed patch or unformatted work. Preserve that work and select fresh `LINUX_BUILD_SRC` and `LINUX_BUILD_OUT` paths for an unpatched build. `make clean` preserves the source tree and therefore does not remove previously applied patches. The explicit `linux-build` target remains available for building a development tree without applying the tracked patch set.

## Related contracts

- [vmlinux.md](../docs/vmlinux.md): kernel configuration, architecture differences and design decisions.
- [Release](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release.md): independent runtime/vmlinux versions and aggregate releases. Runtime-required artifacts from this directory are collected through `kuasar-sandbox/release/inputs/bin-inputs.yaml` into shared `bin/`.
