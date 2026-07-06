# guest-runtime

Guest runtime image and native dependency build repo for kuasar-sandbox.

This repo builds the guest-side artifacts consumed by the sandbox engine at
runtime. Host lifecycle code remains in `sandboxer`; reusable content/image
libraries remain in `accelerator`.

| Path | Role |
|---|---|
| `Makefile` | Builds one `sandbox-runtime.erofs` from `../sandboxer/bin/<arch>/sandbox-init` plus guest payload. |
| `cmd/flatten-ctl` | OCI/目录 → EROFS deterministic image builder; the CLI imports `accelerator/pkg/{flatten,image,remote,tar}`. |
| `docs/sandbox-runtime.md` | Guest runtime image layout, payload projection, and packaging contract. |
| `docs/vmlinux.md` | Guest kernel image contract and config rationale. |
| `docs/flatten.md` | `flatten-ctl` CLI, remote pull cache, and OCI Referrers behavior. |
| `native-deps/` | Builds `vmlinux`, `mkfs.erofs`, `fsck.erofs`, and `envd`. |
| `scripts/guest-inspect.py` | Helper for inspecting guest/runtime images. |

`sandbox-init` source and host lifecycle code live in
[`sandboxer`](https://github.com/kuasar-sandbox/sandboxer). `guest-runtime`
packages the built `sandbox-init` into the DAX-shared EROFS image and injects
the guest payload used by e2b/build flows under `/opt/sandbox-runtime/bin/`:
`envd`, `flatten-ctl`, and `mkfs.erofs`.

## Build

```bash
make native-deps                 # vmlinux / erofs tools / envd
make flatten-ctl                 # OCI/dir -> deterministic EROFS builder
make build                       # sandbox-runtime.erofs with envd/flatten-ctl/mkfs.erofs
make sandbox-runtime             # same image target, builds ../sandboxer sandbox-init if needed
make build TARGET_ARCH=aarch64
```

`make sandbox-runtime` needs `mkfs.erofs`, found from `PATH`, `bin/<arch>/`,
or `native-deps/bin/<arch>/`; it also consumes `native-deps/bin/<arch>/envd`
and this repo's `bin/<arch>/flatten-ctl`, building those targets on demand.

## Artifacts

| Artifact | Producer |
|---|---|
| `bin/<arch>/flatten-ctl` | `make flatten-ctl` |
| `bin/<arch>/sandbox-runtime.erofs` | `make sandbox-runtime` |
| `native-deps/bin/<arch>/vmlinux` | `make native-deps` |
| `native-deps/bin/<arch>/mkfs.erofs` / `fsck.erofs` | `make native-deps` |
| `native-deps/bin/<arch>/envd` | `make native-deps` |

The cross-repo release build is driven from `orchestrator/release-builder`.
