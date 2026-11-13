[English](using-guest-runtime.md) | [简体中文](using-guest-runtime_zh.md)

# Use a selected guest Runtime and Kernel

Use this workflow after obtaining and validating a matching aggregate release, or a deliberately selected compatible Runtime/Kernel/sandboxer combination. It checks installed products, prepares an application image and explains version retention, restore and troubleshooting. Full platform installation and scheduling remain in the [project quickstart](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/quickstart.md).

## Select the two independent release units

This repository does not publish a generic `guest-runtime-vX.Y.Z` release. It maintains two independent version lines:

- **`runtime-vX.Y.Z`** — publishes `sandbox-runtime-<arch>-vX.Y.Z.tar.gz`, containing the runtime bundle, static `sandbox-init` and `flatten-ctl`, and their license/source materials;
- **`vmlinux-vX.Y.Z`** — publishes `vmlinux-<arch>-vX.Y.Z.tar.gz`, containing the guest kernel at the stable `bin/vmlinux` path and its raw-file SHA-256 at `bin/vmlinux.sha256`.

The two version numbers may advance independently. The project aggregate release selects an exact Runtime tag and an exact VMLinux tag; it does not assume that their version numbers match.

GitHub component assets are published for the architectures declared by each selected release, from exact source refs/commits after component build and packaging checks. Here `<arch>` is `x86_64` or `aarch64` when that release publishes it. The project aggregate release later selects exact Runtime, VMLinux, and other component tags and performs cross-component integration tests plus released-asset MicroVM validation for that composition. Source Makefiles may support another `TARGET_ARCH`, but source-build support does not by itself mean a prebuilt artifact is published for that architecture.

## Check the installed product set

Prerequisites: Linux, Bash, `file`, `sha256sum`, the validated extracted Runtime and Kernel archives, and matching sandboxer when starting a VM. `PREFIX` is their deployment root. Do not extract different architectures over the same `bin/`. New kernel packages include the identity sidecar; historical packages without it follow the selected release's documented validation path.

```bash
set -euo pipefail
PREFIX=/path/to/validated-release
test -s "$PREFIX/bin/sandbox-runtime.bundle"
test -x "$PREFIX/bin/sandbox-init"
test -x "$PREFIX/bin/flatten-ctl"
test -s "$PREFIX/bin/vmlinux"
file "$PREFIX/bin/sandbox-init" "$PREFIX/bin/flatten-ctl" "$PREFIX/bin/vmlinux"
(cd "$PREFIX/bin" && sha256sum --check vmlinux.sha256)
```

Expect the selected target architecture and `vmlinux: OK`. This checks the installed raw-kernel identity, not a signature, guest boot or snapshot compatibility. Runtime bundle identity/layout and the selected sandbox-init ABI remain separate requirements; see [Runtime bundle](runtime-bundle.md) and [Kernel](vmlinux.md).

## Prepare and inspect an application image

Select an immutable registry digest, or use the Docker archive and directory workflows in [flatten-ctl](flatten-cli.md). Registry export needs root or CAP_CHOWN to preserve layer ownership. Use a fresh private directory and a host with enough scratch space; keep production credentials in the documented protected configuration/environment, not in the command history.

```bash
WORK=$(mktemp -d)
IMAGE=registry.example.com/team/app@sha256:REPLACE_WITH_COMPLETE_DIGEST
"$PREFIX/bin/flatten-ctl" export --output "$WORK/app.img" "$IMAGE"
"$PREFIX/bin/flatten-ctl" info --json "$WORK/app.img"
```

Replace the registry placeholder with an accessible digest and supply a selected `--config` when authentication, TLS or cache settings are required. Expect a complete artifact and a readable image/runtime-config report. `.img` is a tarstream carrier: do not pass it directly to mount as raw EROFS. Before reuse, a failed export must be diagnosed using stderr and the documented owned-resource cleanup. Retain `WORK` while its image is referenced; remove only this private directory after consumers release it.

## Bind the artifacts to sandboxer

Supply the installed kernel as `boot.kernel`, the runtime bundle as `boot.runtime`, and the application artifact as `boot.root.base` using `file://` references. Use the selected sandboxer's full configuration and launch workflow; its lifecycle API owns networking, resources and restore admission. The runtime is a shared read-only PMEM image; the application image supplies the user rootfs. An accepted YAML file alone does not prove the kernel, VMM and snapshot combination is compatible.

For an application image whose command can run without a NIC, the following cold-start configuration binds the three artifacts. `UPPER_TEMPLATE` must already be a compatible empty ext4 upper template from the selected deployment workflow. The host must satisfy the selected sandboxer's Linux/KVM/VMM requirements. Use absolute paths without spaces in this minimal shell example. Networking is intentionally omitted; adding it requires the sandboxer/connector workflow.

```bash
UPPER_TEMPLATE=/absolute/path/to/validated-empty-upper.ext4
test -s "$UPPER_TEMPLATE"
cat > "$WORK/sandbox.yaml" <<EOF
resources:
  capacity: {cpu: 2, memory: 2GiB}
  allocatable: {cpu: 2, memory: 2GiB}
boot:
  kernel: file://$PREFIX/bin/vmlinux
  runtime: file://$PREFIX/bin/sandbox-runtime.bundle
  root:
    base: file://$WORK/app.img
    overlay:
      diff_template: file://$UPPER_TEMPLATE
EOF
"$PREFIX/bin/sandbox-ctl" config --config "$WORK/sandbox.yaml" --check strict
"$PREFIX/bin/sandbox-ctl" run --config "$WORK/sandbox.yaml" --sandbox-id guest-example
```

Strict configuration validation should succeed before launch; the foreground run then reports the sandbox startup or a specific error. With `launch.exec` omitted, overlay mode uses the image Entrypoint/Cmd. No startup-resource override is required. Runtime/kernel identity errors, a missing/invalid ext4 upper template, insufficient privileges or incompatible VMM inputs must be resolved rather than bypassed. For inspection, shutdown and retained-state cleanup follow [sandboxer usage](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/runtime.md#standalone-workflow). Do not delete the application, Runtime, kernel or backing template while they remain referenced.

## Retain, upgrade and recover selected artifacts

### Node upgrades

A node can keep multiple runtime images, for example:

```text
/opt/sandbox/runtime/v0.1.0/sandbox-runtime.bundle
/opt/sandbox/runtime/v0.2.0/sandbox-runtime.bundle
```

New sandboxes use the new version; running sandboxes retain their original pmem file. Before deleting an old image, ensure no running VM or snapshot awaiting restore depends on it.

ABI compatibility alone does not make a new runtime eligible for an old memory
snapshot. Here S denotes the memory snapshot artifact and E its referenced cold
sandbox artifact, which records the runtime identity. Restore compares the runtime's
**digest scheme and declared Bundle footer digest**, not its basename, with E;
see [sandboxer restore selection](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/runtime.md#restore).

For example, S1 captured with `/runtime/v1/sandbox-runtime.bundle` cannot use
`/runtime/v2/sandbox-runtime.bundle` merely because v2 has a compatible wire ABI.
If the content identity differs, explicitly bind `boot.runtime` to a retained file
matching S1's E. A matching v1 file may have a new name or path; renaming a different
runtime does not make it compatible. The directory layout above supports explicit
binding; restore does not search sibling version directories.

For automatic fallback, capture v1 with basename `sandbox-runtime-v1.bundle`, keep
that immutable file alongside `sandbox-runtime-v2.bundle`, and point the default
at v2. Restore checks the configured v2 file first. If that check fails because the file is absent, unreadable, malformed or has a
different identity, it tries exactly `<default-directory>/sandbox-runtime-v1.bundle`.
The original basename is only this fallback lookup hint: the first matching
candidate is used, with no directory scan. If neither candidate passes, format,
I/O and permission errors are not reported as identity incompatibility. Invalid
reference configuration and cancellation do not trigger fallback.

Retain immutable runtime bytes while any live VM, retained template, snapshot or
parent chain needs them, including backups and rollback candidates. Keep paths
stable while live VMs reference them; preserve the captured basename when relying
on automatic fallback, or explicitly bind a matching retained file. Memory restore
does not read an external kernel file or its identity sidecar, but still requires
compatible architecture/VMM/device inputs. Retain kernel files and sidecars needed
for cold starts and rollback. Before retirement, the deployment owner must inventory
references and test restore on an eligible destination; retaining only the latest
ABI-compatible runtime is insufficient.

To adopt new kernel/runtime contents from a cold E artifact instead, start cold
using the target configuration. Sandboxer derives a new C0 from the target identities
while preserving the effective source disk graph. This is not a resume of S's saved
memory; memory restore retains S's original E/C0 identity.

### Host reboot

Sandboxes do not automatically resume after a host reboot. On node startup, the runtime image must still exist in the deployment directory for subsequent sandbox creation.

### Rollback

Pause new placements while preparing rollback; this contains impact but does not select an old runtime. On an orchestrator-managed node, set the conductor configuration's `sandbox.boot.runtime` to the retained compatible file and restart the conductor service (`node-ctl conductor serve`), then verify a new cold start before reopening admission. For standalone `sandbox-ctl`, select `boot.runtime` in the configuration for the next launch. Preserve existing runner units and referenced files: changing the default does not migrate running VMs or override the snapshot identity rules above.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Guest cannot start `/sbin/init` | Confirm the runtime-image architecture matches vmlinux/CH. |
| Build sandbox cannot find `flatten-ctl` | Inspect `/opt/sandbox-runtime/bin/flatten-ctl` in the image. |
| Build sandbox cannot generate EROFS | Check guest `flatten-ctl`, source read permissions and planner budgets. |
| Host cannot pack a cross-architecture image | Check that `BUILD_MKFS_EROFS` is executable on the build host. |
| Behavior changes after restore | Compare the snapshot's runtime digest with restore configuration. |
| Scripts cannot find runtime after package extraction | Confirm the runtime archive was extracted and `bin/sandbox-runtime.bundle` exists. |

## Next steps

- [Runtime delivery contract](runtime-bundle.md)
- [Kernel capabilities and verification](vmlinux.md)
- [Complete flatten CLI](flatten-cli.md)
- [Build and release reproduction](build.md)
