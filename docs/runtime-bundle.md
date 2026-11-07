[English](runtime-bundle.md) | [简体中文](runtime-bundle_zh.md)

# Runtime bundle: payload, identity and compatibility

This is the delivery contract for `sandbox-runtime.bundle` and its Runtime release unit. It owns filesystem layout, PMEM alignment, footer identity, reserved paths and compatibility requirements. Follow [Using guest-runtime](using-guest-runtime.md) to consume a selected pair of Runtime/Kernel artifacts, or [Build](build.md) to produce and validate them.

`sandbox-runtime.bundle` is the read-only Guest runtime image attached to each MicroVM. It contains the guest PID 1 (`sandbox-init`) built by `sandboxer` and the platform's guest-side helper tools. This repository packages and releases it; `sandboxer/sandbox-ctl` supplies it to the guest as a virtio-pmem device when starting a sandbox.

This document defines image packaging, filesystem layout, versioning and consumption. The startup rootfs assembly, vsock control plane, stdio MUX and exec/attach/quiesce ABI of `sandbox-init` are maintained in [the sandboxer guest ABI specification](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/guest-abi.md).

## Overview

### Ownership boundaries

| Component | Responsibility |
| --- | --- |
| `sandboxer` | Build `sandbox-init`; `sandbox-ctl` starts CH and attaches the runtime image as guest virtio-pmem. |
| `guest-runtime` | Package `sandbox-init`, `envd`, `flatten-ctl` into one EROFS image. |
| `guest-runtime/native-deps` | Build `vmlinux` and `envd`; the optional gateway is a separate opt-in target, not runtime payload. |
| `accelerator` | Supply the reusable `pkg/{flatten,image,remote,tar}` packages used by `flatten-ctl`, and Manifest/Cache/Store capabilities. |

`sandbox-runtime.bundle` does not contain the user rootfs, user dependencies, guest kernel or Cloud Hypervisor. The user rootfs comes from `boot.root.base`/`boot.disks[]`. The kernel is published in the vmlinux package; the VMM is published by sandboxer.

### Place in the system

```text
                 build time                                      run time

  sandboxer/bin/<arch>/sandbox-init ─┐
  guest-runtime/bin/<arch>/flatten-ctl├─► sandbox-runtime.bundle ──► sandbox-ctl
  native-deps/bin/<arch>/envd ────────┘             ▲                     │
                                                 │                     ▼
                                           runtime release          guest /sbin/init
```

The image is a node-shared artifact. Sandboxes using the same version on a node map the same host file through virtio-pmem and DAX instead of copying runtime file pages independently.

### Design goals

- **One image:** the E2B and builder runtimes share one runtime image.
- **Read-only and versioned:** the release process produces the image; runtime execution does not modify it.
- **Cross-instance sharing:** virtio-pmem and DAX share the host page cache.
- **Clear ownership:** sandboxer owns the PID 1 protocol; guest-runtime owns image packaging and guest payload.
- **Independent releases:** the runtime bundle can be published through the repository's dedicated release line and selected independently for rollback.

## Image layout

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
```

The `/sysdisks/` mountpoints are precreated by the current Makefile for data-disk ordinals 0–7. Apart from these platform paths, the image supplies no `/etc`, `/usr`, shared libraries or general distribution environment.

`sandbox-init` performs early mounting and switch-root through Go syscalls. The rootfs actually seen by the user application comes from its image. Before switch-root, `/opt/sandbox-runtime` is bind-mounted at the same path in the user rootfs. Applications can read the platform tools but should not place their own files under that reserved path.

### Guest payload

| File | Source | Purpose |
| --- | --- | --- |
| `/sbin/init` | `../sandboxer/bin/<arch>/sandbox-init` | Guest PID 1: mounts, handshakes and application supervision. |
| `/opt/sandbox-runtime/bin/envd` | `native-deps/bin/<arch>/envd` | E2B data-plane agent. |
| `/opt/sandbox-runtime/bin/flatten-ctl` | `bin/<arch>/flatten-ctl` | Pull/flatten OCI images inside the build sandbox. |

`fsck.erofs` is a diagnostic/test tool and is not included in the runtime image. `vmlinux` is not runtime-image content; it is released independently as `vmlinux-<arch>-vX.Y.Z.tar.gz`.

### Host bundle

The delivered file is not bare EROFS, but a bundle directly usable as virtio-pmem backing:

```text
raw EROFS | zero padding | trailing ZIP
```

Raw EROFS starts at offset 0. The trailing ZIP contains only one zero-size marker:

```text
.kuasar.digest.<64-lowercase-hex>
```

The `digest:` identity is SHA-256 of every byte before the ZIP, including EROFS
and alignment padding. It excludes the ZIP and is not the SHA-256 of the complete
bundle file. The builder hashes that prefix before writing the marker. Run/restore
checks the bundle/footer format and reads the declared identity at EOF without
rehashing the prefix. This is an identity comparison, not independent content or
origin verification; deploy trusted, separately verified release artifacts. The
[release payload validator](../scripts/release-runtime-payloads.py) does recompute
the prefix hash and compare it with the marker.

Final bundle size is aligned to 2 MiB, so Cloud Hypervisor can map it directly
without offset support; EROFS uses its own superblock to ignore trailing padding
and ZIP.

## Runtime consumption contract

`sandbox-ctl` supplies `sandbox-runtime.bundle` to Cloud Hypervisor as a read-only virtio-pmem device. The guest kernel mounts the pmem image and executes `/sbin/init`, which is sandbox-init.

Key constraints:

- The runtime image is read-only and cannot contain per-sandbox state.
- Snapshot restore requires the captured runtime content identity: digest scheme
  and footer-declared digest must match. A configured file with the same identity
  may have a different basename; the original basename only identifies the one
  permitted sibling fallback. Wire compatibility or a release label alone is
  insufficient. Pin and retain the immutable required artifact; see the
  [restore runtime selection contract](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/runtime.md#restore).
- Cold startup, including source-derived `run --from`, uses the selected target
  runtime and records its identity in a new C0. It does not require the source E's
  old runtime identity; Guest ABI and VMM compatibility still apply.
- The sandbox-init and sandbox-ctl wire ABIs must match. Before upgrading runtime, upgrade sandboxer accordingly or validate backward compatibility.
- `/opt/sandbox-runtime` is reserved. A preexisting directory at that path in the user image is obscured by the platform bind mount.

`sandbox-runtime.bundle` does not participate in deriving manifest keys, API keys or access tokens. Those keys are managed by orchestrator/placer/providers and Manifest configuration; the runtime image carries execution tools only.

## Release artifacts

The trusted `Runtime Release` workflow on this repository's main branch independently publishes the runtime image from the source branch and exact SHA pinned by the coordinator. Component `main` is used for the development line; `release/vX.Y.x` is used for runtime maintenance. Runtime, aggregate and vmlinux versions are independent:

| Package | Contents | Release |
| --- | --- | --- |
| `sandbox-runtime-<arch>-vX.Y.Z.tar.gz` | Runtime image, `flatten-ctl` | `runtime-vX.Y.Z` in guest-runtime |

Here `<arch>` is `x86_64` or `aarch64` when declared and published by the selected release.
Do not infer another architecture's assets from source-build support or overwrite two architectures
into the same extracted `bin/` directory.

The runtime package contains:

```text
bin/sandbox-runtime.bundle
bin/flatten-ctl
share/licenses/runtime/...
```

The outer archive also carries required license, source and Go build/dependency
materials under `share/licenses/runtime/`. These are release-package material,
not files in the runtime EROFS image.

`sandbox-runtime.bundle` is the stable filename shared by scripts, default configuration and external distribution.

The project repository's `release-vX.Y.Z` aggregate release uploads the platform package, original independently versioned component packages, verified using native GitHub asset sizes and digests. The platform package takes runtime documentation and `test/e2e/` from the selected runtime tag and the kernel documentation pair (`docs/vmlinux.md` and, when present at that selected tag, `docs/vmlinux_zh.md`) from the independently selected kernel tag; component packages do not duplicate those documents. Extracting the required packages into one directory produces shared `bin/`, `docs/`, `test/` and `deploy/` layouts. Bilingual documentation delivery is validated separately from binary release selection.

`vmlinux` is not part of the runtime version; it uses this repository's independent `vmlinux-vX.Y.Z` line. The runtime workflow explicitly selects a published sandboxer tag to build the image. Runtime and vmlinux version numbers evolve independently.

Envd is already embedded in the image and is not published separately as `bin/envd`. `fsck.erofs` is a host diagnostic/test tool, not part of the general component packages.

## Related contracts

- [sandbox-init](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/guest-abi.md): guest PID 1 ABI and host/guest protocol.
- [sandbox](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/runtime.md): runtime-image consumption, startup and restore.
- [Native-build workflow](../native-deps/README.md): host EROFS prerequisites, vmlinux and Envd builds.
- [vmlinux](vmlinux.md): guest kernel and runtime-image relationship.
- [flatten](flatten-cli.md): flatten-ctl execution in a build sandbox.
- [Aggregate validation](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/test/QUICKSTART.md): package extraction and E2E entry points.
