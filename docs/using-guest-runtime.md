[English](using-guest-runtime.md) | [简体中文](using-guest-runtime_zh.md)

# Use a selected guest Runtime and Kernel

Use this workflow after obtaining and validating a matching aggregate release, or a deliberately selected compatible Runtime/Kernel/sandboxer combination. It checks installed products, prepares an application image, selects direct or systemd startup, and prepares the application for capture and restore. To start a sandbox with the complete platform, use the `docker pull` and `docker run` workflow in the [project Quick Start](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/quickstart.md).

The recommended sequence is: verify the released artifacts, flatten the application,
cold-start it with a built-in empty writable upper, validate its real operation,
and only then capture reusable state. Changing the runtime, kernel or startup
policy requires a new cold start and a new capture.

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
"$PREFIX/bin/sandbox-init" --version
"$PREFIX/bin/flatten-ctl" --version
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

For an application image whose command can run without a NIC, the following
cold-start configuration binds the three artifacts and creates an empty 1 GiB
ext4 upper through sandboxer's built-in initializer. The host must satisfy the
selected sandboxer's Linux/KVM/VMM requirements, with `cloud-hypervisor`
available to sandbox-ctl. Use absolute paths without spaces in this minimal shell
example. Networking is omitted; add it through the sandboxer/connector workflow.

```bash
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
      diff_template: mkfs://ext4?size=1GiB
EOF
"$PREFIX/bin/sandbox-ctl" config --config "$WORK/sandbox.yaml" --check strict
"$PREFIX/bin/sandbox-ctl" run --config "$WORK/sandbox.yaml" \
  --sandbox-id guest-example --tty=false
```

Strict configuration validation should succeed before launch; the foreground run
then reports startup or a specific error. With `launch.exec` omitted, overlay mode
uses the image Entrypoint/Cmd. No startup-resource override is required. Keep the
workload alive while inspecting it from a second terminal, using a command that
exists in that image:

```bash
"$PREFIX/bin/sandbox-ctl" exec --sandbox-id guest-example -- /bin/sh -c 'id; pwd'
```

The built-in initializer replaces manual host formatting for a fresh ordinary
upper. Its deterministic profile has no ext4 journal. Use an externally prepared
template when the workload requires other filesystem features, and retain the
selected size and profile as part of the workload definition; see
[filesystem sources and disk capacity](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/configuration.md#disk-capacity).
A writable initializer cannot be combined with an immutable COW base for that
same device; the separate read-only EROFS root above is supported.

Runtime/kernel identity errors, invalid filesystem sources, insufficient
privileges or incompatible VMM inputs must be resolved before reuse. Stop this
non-TTY example with Ctrl+C in its owning `run` terminal and wait for exit.
For capture, independent restore, retained disks and cleanup, follow
[sandboxer usage](https://github.com/kuasar-sandbox/sandboxer/blob/main/docs/runtime.md#standalone-workflow).
Keep application, Runtime and kernel files while consumers still reference them.

<a id="application-startup"></a>

## Choose the application's startup mode

For an ordinary application entrypoint, keep direct mode, the default. The runtime
already supplies Guest PID 1, mounts, initialization and process supervision.
Applications do not need a copied sandbox-init or a hand-written platform service.

When the application needs systemd units, choose `launch.systemd: runtime` to use
the matching private installation in the Runtime Bundle:

```yaml
launch:
  systemd: runtime
  # Omit exec/args to retain the image's Entrypoint/Cmd in overlay mode.
```

Merge this into the cold-start configuration before launching. Keep application
units in the application image or install them during its preparation. The
runtime creates its reserved `sandbox-init.service`, executor wiring and startup
barrier; do not create a replacement platform unit or set `launch.exec` to
`/sbin/init` to enable systemd. Standard services ordered through `sysinit.target`
wait until the executor has attached and sent READY. This does not wait for
application health or for every user service to start.

| Policy | Choose it when |
| --- | --- |
| `off` | The application runs directly; this is the default without an inherited source policy. |
| `runtime` | The application needs systemd and should use the Runtime's private manager, loader, libraries and tools. |
| `provided` | The application image deliberately supplies a qualified systemd installation. |
| `auto` | Prefer the image's qualified installation and fall back to Runtime only if no candidate exists. An incompatible existing candidate fails launch. |

Qualification currently covers dynamically linked glibc managers on amd64 and
arm64; static or musl managers are not accepted. This is a manager qualification
rule, not a requirement that every workload binary use glibc. The full selection,
cgroup, PATH and shutdown rules are in [Guest ABI](guest-abi.md#systemd-environments).
For Runtime mode, inspect the manager and an application unit from the host:

```bash
"$PREFIX/bin/sandbox-ctl" exec --sandbox-id guest-example -- \
  /run/sandbox-runtime/systemd/bin/systemctl is-active sandbox-init.service
"$PREFIX/bin/sandbox-ctl" exec --sandbox-id guest-example -- \
  /run/sandbox-runtime/systemd/bin/systemctl status YOUR_APPLICATION.service
```

Replace the application unit name with one installed in the image. Platform READY
is not its readiness check: also run the application's representative request.
Launch Init may use `systemctl` because platform service READY precedes Init.
Use the runtime owner's shutdown workflow for stopping the environment; sending
SIGTERM directly to systemd requests reexecution, not normal poweroff.

<a id="application-restore"></a>

## Prepare the application for capture and restore

First distinguish a fresh cold start from resuming saved memory:

| Operation | Application behavior |
| --- | --- |
| `run --config` or `run --from` E | Apply the effective files/mounts and Init, then start the main process and plugins. |
| `run --restore` S | Continue captured processes, manager, namespaces and memory. Do not replay Init or replace launch policy. |
| `snapshot --resume` of the source | Continue the same VM after capture; do not run true-restore notifications. |

Capture already freezes the user environment and syncs its managed root/data ext4
filesystems. Guest-wide `sync`, manual cache-drop loops and external `fsfreeze`
scripts are not prerequisites for the platform capture barrier. Applications
still own consistency with external databases, leases and remote services;
the [quiesce contract](guest-abi.md#quiesce) does not add a general application
pre-snapshot hook. A managed `syncfs` error fails capture and stays recorded for
that Guest generation; a later successful `sync` does not clear it.

### Asynchronous application notification

An application or plugin that already handles a signal can subscribe before
capture:

```yaml
launch:
  restore_signal: SIGUSR1
```

This fragment adds a subscription to the existing main process definition.
Install and test the handler before capture; an unhandled SIGUSR1 can terminate
the process. Each plugin has its own optional `restore_signal`. Only a true
memory restore sends the notification, to the still-live subscribed main PID.
Signal delivery is asynchronous and does not wait for the handler's refresh work.
The application owns retries, idempotency and readiness after reconnecting.

### A synchronous systemd restore hook

For active systemd environments, install a component-owned oneshot unit before
capture. For example, place this at
`/etc/systemd/system/application-after-restore.service` in the application image:

```ini
[Unit]
Description=Refresh application state after sandbox restore
PartOf=sandbox-restore.target
Before=sandbox-restore.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/libexec/application-after-restore
TimeoutStartSec=5s
[Install]
RequiredBy=sandbox-restore.target
```

Supply that executable with the application and make it return its real result.
Enable the unit while preparing the live environment, before capture:

```bash
"$PREFIX/bin/sandbox-ctl" exec --sandbox-id guest-example -- \
  /run/sandbox-runtime/systemd/bin/systemctl enable application-after-restore.service
```

The Runtime supplies `sandbox-restore.target` and restarts it on each true restore;
the target is not linked into cold boot. `RequiredBy` with the ordering above makes
a helper failure fail the target and prevents successful restore ACK. A
`WantedBy` dependency still waits but does not propagate helper failure. Keep the
whole job within the existing 10-second private RPC budget, including queue time;
5 seconds here is an example unit timeout. Host `timeouts.restore` can impose a
shorter bound. A hook runs after thaw while the main Host MUX is detached, so it
must finish without depending on Host exec/MUX being available during restore. Full notification semantics
are in [Restore notifications](guest-abi.md#restore-notifications).

### Network state and verification

On true restore, the runtime applies the current platform NIC configuration,
clears its dynamic ARP/NDP entries and makes a bounded attempt to release selected
old TCP connections. Ordinary source resume or attach does not reset that network
state. Configure the destination through the Host network provider; avoid adding
a blanket Guest route flush, NIC recreation or Host polling loop for platform
network recovery.
Applications still need to reconnect and renew external state. User-created NICs,
other network namespaces and ambiguous routed connections remain outside the
platform cleanup; see [Network restore](network-restore.md).

Before publishing a reusable template, test a representative request, write and
read a persistent file, capture and restore into an independent instance, and
verify the request, file and intended notification again. Also resume the source
and confirm that true-restore hooks did not run there. A successful platform ACK
does not prove external service readiness. Existing snapshots keep their captured
Runtime and kernel; create a fresh capture to adopt these features.

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

Standalone `sandbox-ctl` does not provide a scheduler or automatically recreate
VMs after a host reboot. Retain the runtime, cold-start inputs and any required
portable snapshots. On orchestrator-managed nodes, use the platform's persistent
node and recovery workflow; its ownership and placement decisions are separate
from this component's artifact requirements. Local in-memory checkpoint holders
are not a substitute for published, recoverable artifacts.

### Rollback

Pause new placements while preparing rollback; this contains impact but does not select an old runtime. On an orchestrator-managed node, set the conductor configuration's `sandbox.boot.runtime` to the retained compatible file and restart the conductor service (`node-ctl conductor serve`), then verify a new cold start before reopening admission. For standalone `sandbox-ctl`, select `boot.runtime` in the configuration for the next launch. Preserve existing runner units and referenced files: changing the default does not migrate running VMs or override the snapshot identity rules above.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Guest cannot start `/sbin/init` | Confirm the runtime-image architecture matches vmlinux/CH. |
| systemd mode fails before application launch | Check `launch.systemd`, the selected manager's qualification, reserved-path conflicts and required cgroup controllers; `auto` does not hide an incompatible image manager. |
| Restore succeeds but an external request fails | Check the application's reconnect/lease logic and network logs. TCP cleanup is bounded and best effort, not an external-session guarantee. |
| Restore hook does not run | Confirm a true memory restore, a captured subscription or enabled unit, and the captured Runtime version. Source resume and cold starts do not notify. |
| Capture reports managed `syncfs` failure | Inspect storage and application I/O errors; the failure is sticky for that Guest generation. Diagnose the error before preparing a new cold instance. |
| Build sandbox cannot find `flatten-ctl` | Inspect `/opt/sandbox-runtime/bin/flatten-ctl` in the image. |
| Build sandbox cannot generate EROFS | Check guest `flatten-ctl`, source read permissions and planner budgets. |
| Host cannot pack a cross-architecture image | Check that `BUILD_MKFS_EROFS` is executable on the build host. |
| Behavior changes after restore | Compare the snapshot's runtime digest with restore configuration. |
| Scripts cannot find runtime after package extraction | Confirm the runtime archive was extracted and `bin/sandbox-runtime.bundle` exists. |

## Next steps

- [Runtime delivery contract](runtime-bundle.md)
- [Kernel capabilities and verification](vmlinux.md)
- [Complete flatten CLI](flatten-cli.md)
- [Network restore and its limits](network-restore.md)
- [Build and release reproduction](build.md)
