[English](sandbox-runtime.md) | [简体中文](sandbox-runtime_zh.md)

# sandbox-runtime — guest runtime image

`sandbox-runtime.bundle` is the read-only Guest runtime image attached to each MicroVM. It contains the guest PID 1 (`sandbox-init`) built by `sandboxer` and the platform's guest-side helper tools. This repository packages and releases it; `sandboxer/sandbox-ctl` supplies it to the guest as a virtio-pmem device when starting a sandbox.

This document defines image packaging, filesystem layout, versioning and consumption. The startup rootfs assembly, vsock control plane, stdio MUX and exec/attach/quiesce ABI of `sandbox-init` are maintained in [the sandboxer guest ABI specification](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init.md).

## 1. Overview

### 1.1 Ownership boundaries

| Component | Responsibility |
| --- | --- |
| `sandboxer` | Build `sandbox-init`; `sandbox-ctl` starts CH and attaches the runtime image as guest virtio-pmem. |
| `guest-runtime` | Package `sandbox-init`, `envd`, `flatten-ctl` and `mkfs.erofs` into one EROFS image. |
| `guest-runtime/native-deps` | Build `mkfs.erofs`, `fsck.erofs`, `vmlinux` and `envd`; the optional gateway is a separate opt-in target, not runtime payload. |
| `accelerator` | Supply the reusable `pkg/{flatten,image,remote,tar}` packages used by `flatten-ctl`, and Manifest/Cache/Store capabilities. |

`sandbox-runtime.bundle` does not contain the user rootfs, user dependencies, guest kernel or Cloud Hypervisor. The user rootfs comes from `boot.root.base`/`boot.disks[]`. The kernel is published in the vmlinux package; the VMM is published by sandboxer.

### 1.2 Place in the system

```text
                 build time                                      run time

  sandboxer/bin/<arch>/sandbox-init ─┐
  guest-runtime/bin/<arch>/flatten-ctl├─► sandbox-runtime.bundle ──► sandbox-ctl
  native-deps/bin/<arch>/envd ────────┤          ▲                     │
  native-deps/bin/<arch>/mkfs.erofs ──┘          │                     │ virtio-pmem+DAX
                                                 │                     ▼
                                           runtime release          guest /sbin/init
```

The image is a node-shared artifact. Sandboxes using the same version on a node map the same host file through virtio-pmem and DAX instead of copying runtime file pages independently.

### 1.3 Design goals

- **One image:** the E2B and builder runtimes share one runtime image.
- **Read-only and versioned:** the release process produces the image; runtime execution does not modify it.
- **Cross-instance sharing:** virtio-pmem and DAX share the host page cache.
- **Clear ownership:** sandboxer owns the PID 1 protocol; guest-runtime owns image packaging and guest payload.
- **Independent releases:** the runtime bundle can be published through the repository's dedicated release line and selected independently for rollback.

## 2. Image layout

The image root contains:

```text
/sbin/init                         sandbox-init
/proc/                             Empty mountpoint
/sys/                              Empty mountpoint
/dev/                              Empty mountpoint
/overlay/lower/                    User base-rootfs mountpoint
/overlay/upper/                    Writable ext4 / overlay-upper mountpoint
/sysroot/                          switch-root target
/sysdisks/disk-{0..7}/              Data-disk mountpoints
/sysdisks/disk-{0..7}-lower/        Data-disk lower mountpoints
/sysdisks/disk-{0..7}-upper/        Data-disk upper mountpoints
/opt/sandbox-runtime/bin/envd       E2B guest agent
/opt/sandbox-runtime/bin/flatten-ctl
/opt/sandbox-runtime/bin/mkfs.erofs
```

The `/sysdisks/` mountpoints are precreated by the current Makefile for data-disk ordinals 0–7. Apart from these platform paths, the image supplies no `/etc`, `/usr`, shared libraries or general distribution environment.

`sandbox-init` performs early mounting and switch-root through Go syscalls. The rootfs actually seen by the user application comes from its image. Before switch-root, `/opt/sandbox-runtime` is bind-mounted at the same path in the user rootfs. Applications can read the platform tools but should not place their own files under that reserved path.

### 2.1 Guest payload

| File | Source | Purpose |
| --- | --- | --- |
| `/sbin/init` | `../sandboxer/bin/<arch>/sandbox-init` | Guest PID 1: mounts, handshakes and application supervision. |
| `/opt/sandbox-runtime/bin/envd` | `native-deps/bin/<arch>/envd` | E2B data-plane agent. |
| `/opt/sandbox-runtime/bin/flatten-ctl` | `bin/<arch>/flatten-ctl` | Pull/flatten OCI images inside the build sandbox. |
| `/opt/sandbox-runtime/bin/mkfs.erofs` | `native-deps/bin/<arch>/mkfs.erofs` | Generate EROFS base images inside the build sandbox. |

`fsck.erofs` is a diagnostic/test tool and is not included in the runtime image. `vmlinux` is not runtime-image content; it is released independently as `vmlinux-x86_64-vX.Y.Z.tar.gz`.

### 2.2 Host bundle

The delivered file is not bare EROFS, but a bundle directly usable as virtio-pmem backing:

```text
raw EROFS | zero padding | trailing ZIP
```

Raw EROFS starts at offset 0. The trailing ZIP contains only one zero-size marker:

```text
.kuasar.digest.<64-lowercase-hex>
```

The `digest:` identity covers every byte before the ZIP, including EROFS and alignment padding. The builder computes carrier identity while copying EROFS, then writes the marker. Run/restore reads the marker at EOF without rescanning EROFS. Final bundle size is aligned to 2 MiB, so Cloud Hypervisor can map it directly without offset support; EROFS uses its own superblock to ignore trailing padding and ZIP.

## 3. Build

Common entry points:

```bash
make flatten-ctl                 # Build the target-architecture guest tool.
make native-deps                 # Build mkfs.erofs / fsck.erofs / vmlinux / envd.
make sandbox-runtime             # Produce bin/<arch>/sandbox-runtime.bundle.
make build                       # Build flatten-ctl + sandbox-runtime.
make build TARGET_ARCH=aarch64
```

A **host-executable `mkfs.erofs` must already be available** before `make sandbox-runtime`. Install erofs-utils, build a host copy with `make -C native-deps erofs TARGET_ARCH=$(uname -m)`, or set `BUILD_MKFS_EROFS` to an existing host executable. This is distinct from `GUEST_MKFS_EROFS`, the target-architecture binary embedded in the image; cross-builds must not execute the guest binary on the host.

The target's input and build sequence is:

1. Resolve and check `BUILD_MKFS_EROFS`. Lookup tries host PATH, the native target output when host and target match, then the native-deps host entry. A missing host tool fails before image assembly.
2. If the selected `SANDBOX_INIT` (default `../sandboxer/bin/<arch>/sandbox-init`) is absent, delegate to `make -C ../sandboxer sandbox-init`.
3. Resolve target `ENVD` (default `native-deps/bin/<arch>/envd`), building it when missing.
4. Resolve this repository's target `FLATTEN_CTL` (default `bin/<arch>/flatten-ctl`), building it when missing.
5. Resolve target `GUEST_MKFS_EROFS` (default `native-deps/bin/<arch>/mkfs.erofs`), building it when missing. Assemble the staging tree and use the separate host `BUILD_MKFS_EROFS` to generate temporary raw EROFS.
6. The host `runtime-bundle` tool copies EROFS, adds PMEM alignment padding, computes SHA256 and appends the empty-marker ZIP, publishing `bin/<arch>/sandbox-runtime.bundle` atomically.

See [the native-build workflow](../native-deps/README.md) for mkfs.erofs/Envd builds and the [sandboxer guest ABI](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init.md) for sandbox-init.

### 3.1 Architectures

The runtime image is built for the target architecture. Its build-directory filename is always `sandbox-runtime.bundle`. `/sbin/init`, `envd`, `flatten-ctl` and `mkfs.erofs` inside the EROFS prefix must all be executable files for that same target architecture.

`TARGET_ARCH=amd64` normalizes to `x86_64`; `TARGET_ARCH=arm64` normalizes to `aarch64`. Cross-builds do not update host-architecture symlinks, avoiding host entry points to non-runnable binaries. The host packing tool remains separate from this target payload.

## 4. Runtime consumption contract

`sandbox-ctl` supplies `sandbox-runtime.bundle` to Cloud Hypervisor as a read-only virtio-pmem device. The guest kernel mounts the pmem image and executes `/sbin/init`, which is sandbox-init.

Key constraints:

- The runtime image is read-only and cannot contain per-sandbox state.
- Restoring a snapshot requires a compatible runtime image. Production should pin it by digest or release version.
- The sandbox-init and sandbox-ctl wire ABIs must match. Before upgrading runtime, upgrade sandboxer accordingly or validate backward compatibility.
- `/opt/sandbox-runtime` is reserved. A preexisting directory at that path in the user image is obscured by the platform bind mount.

`sandbox-runtime.bundle` does not participate in deriving manifest keys, API keys or access tokens. Those keys are managed by orchestrator/placer/providers and Manifest configuration; the runtime image carries execution tools only.

## 5. Release artifacts

The trusted `Runtime Release` workflow on this repository's main branch independently publishes the runtime image from the source branch and exact SHA pinned by the coordinator. Component `main` is used for the development line; `release/vX.Y.x` is used for runtime maintenance. Runtime, aggregate and vmlinux versions are independent:

| Package | Contents | Release |
| --- | --- | --- |
| `sandbox-runtime-x86_64-vX.Y.Z.tar.gz` | Runtime image, `flatten-ctl` and `mkfs.erofs` | `runtime-vX.Y.Z` in guest-runtime |

The runtime package contains:

```text
bin/sandbox-runtime.bundle
bin/flatten-ctl
bin/mkfs.erofs
```

`sandbox-runtime.bundle` is the stable filename shared by scripts, default configuration and external distribution.

The project repository's `release-vX.Y.Z` aggregate release uploads the platform package, original independently versioned component packages and aggregate `SHA256SUMS`. The platform package takes runtime documentation and `test/e2e/` from the selected runtime tag and the kernel documentation pair (`docs/vmlinux.md` and, when present at that selected tag, `docs/vmlinux_zh.md`) from the independently selected kernel tag; component packages do not duplicate those documents. Extracting the required packages into one directory produces shared `bin/`, `docs/`, `test/` and `deploy/` layouts. Bilingual documentation delivery is validated separately from binary release selection.

`vmlinux` is not part of the runtime version; it uses this repository's independent `vmlinux-vX.Y.Z` line. The runtime workflow explicitly selects a published sandboxer tag to build the image. Runtime and vmlinux version numbers evolve independently.

Envd is already embedded in the image and is not published separately as `bin/envd`. `fsck.erofs` is a source-tree diagnostic/test helper, not part of the general component packages.

## 6. Reliability and upgrades

### 6.1 Node upgrades

A node can keep multiple runtime images, for example:

```text
/opt/sandbox/runtime/v0.1.0/sandbox-runtime.bundle
/opt/sandbox/runtime/v0.2.0/sandbox-runtime.bundle
```

New sandboxes use the new version; running sandboxes retain their original pmem file. Before deleting an old image, ensure no running VM or snapshot awaiting restore depends on it.

### 6.2 Host reboot

Sandboxes do not automatically resume after a host reboot. On node startup, the runtime image must still exist in the deployment directory for subsequent sandbox creation.

### 6.3 Rollback

Point configuration back to an old runtime file and restart node-ctl, or have the scheduling layer stop placing new sandboxes on the node. Already running sandboxes are unaffected by the new configuration.

## 7. Troubleshooting

| Symptom | Check |
| --- | --- |
| Guest cannot start `/sbin/init` | Confirm the runtime-image architecture matches vmlinux/CH. |
| Build sandbox cannot find `flatten-ctl` | Inspect `/opt/sandbox-runtime/bin/flatten-ctl` in the image. |
| Build sandbox cannot generate EROFS | Check guest `/opt/sandbox-runtime/bin/mkfs.erofs` and its permissions. |
| Host cannot pack a cross-architecture image | Check host `BUILD_MKFS_EROFS` separately from target `GUEST_MKFS_EROFS`. |
| Behavior changes after restore | Compare the snapshot's runtime digest with restore configuration. |
| Scripts cannot find runtime after package extraction | Confirm the runtime archive was extracted and `bin/sandbox-runtime.bundle` exists. |

## 8. See also

- [sandbox-init](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox-init.md): guest PID 1 ABI and host/guest protocol.
- [sandbox](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/sandbox.md): runtime-image consumption, startup and restore.
- [Native-build workflow](../native-deps/README.md): mkfs.erofs, vmlinux and Envd builds.
- [vmlinux](vmlinux.md): guest kernel and runtime-image relationship.
- [flatten](flatten.md): flatten-ctl execution in a build sandbox.
- [Aggregate validation](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/test/QUICKSTART.md): package extraction and E2E entry points.
