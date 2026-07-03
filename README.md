# guest-runtime

Guest runtime image and native dependency build repo for kuasar-sandbox.

This repo owns artifacts that are consumed by the sandbox engine at runtime but
are not themselves the host-side sandbox lifecycle implementation:

| Path | Role |
|---|---|
| `Makefile` | Builds one `sandbox-runtime.erofs` from `../sandboxer/bin/<arch>/sandbox-init` plus guest payload. |
| `docs/sandbox-runtime.md` | Guest runtime image and `sandbox-init` ABI/design. |
| `native-deps/` | Builds `vmlinux`, `cloud-hypervisor`, `mkfs.erofs`, `fsck.erofs`, and `envd`. |
| `scripts/guest-inspect.py` | Helper for inspecting guest/runtime images. |

`sandbox-init` source and host lifecycle code live in
[`sandboxer`](https://github.com/kuasar-sandbox/sandboxer). `guest-runtime`
packages the built `sandbox-init` into the DAX-shared EROFS image and injects
the guest payload used by e2b/build flows under `/opt/sandbox-runtime/bin/`:
`envd`, `flatten-ctl`, and `mkfs.erofs`.

## Build

```bash
make native-deps                 # vmlinux / cloud-hypervisor / erofs tools / envd
make build                       # sandbox-runtime.erofs with envd/flatten-ctl/mkfs.erofs
make sandbox-runtime             # same image target, builds ../sandboxer sandbox-init if needed
make build TARGET_ARCH=aarch64
```

`make sandbox-runtime` needs `mkfs.erofs`, found from `PATH`, `bin/<arch>/`,
or `native-deps/bin/<arch>/`; it also consumes `native-deps/bin/<arch>/envd`
and `../accelerator/bin/<arch>/flatten-ctl`, building those targets on demand
when their sibling repos are available.

## Artifacts

| Artifact | Producer |
|---|---|
| `bin/<arch>/sandbox-runtime.erofs` | `make sandbox-runtime` |
| `native-deps/bin/<arch>/vmlinux` | `make native-deps` |
| `native-deps/bin/<arch>/cloud-hypervisor` | `make native-deps` |
| `native-deps/bin/<arch>/mkfs.erofs` / `fsck.erofs` | `make native-deps` |
| `native-deps/bin/<arch>/envd` | `make native-deps` |

The cross-repo release build is driven from `orchestrator/release-builder`.
