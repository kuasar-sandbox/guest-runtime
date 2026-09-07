[English](README.md) | [简体中文](README_zh.md)

# native-deps

Native dependency builds for [Kuasar Sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox): build upstream sources, including local patches, into native artifacts that the Go repositories consume at runtime rather than link. Their autotools/Kbuild/Go toolchains and upstream release schedules differ from the consuming Go repositories, so they are built independently in `guest-runtime/native-deps`.

<a id="产物与消费方"></a>
## Artifacts and consumers

| Artifact | Source | Consumer |
| --- | --- | --- |
| `mkfs.erofs` | erofs-utils v1.9.1 | `accelerator` flattening and `guest-runtime` runtime-image construction |
| `fsck.erofs` | erofs-utils v1.9.1 | Source-tree diagnostics and accelerator EROFS-content assertions |
| `vmlinux` | Linux 6.1.169 + `deps/linux-patches` + `deps/vmlinux/*.config` | `sandboxer` / `sandbox-ctl` guest kernel |
| `envd` | e2b-dev/infra source tarball at tag `2026.22` | Guest agent included in `sandbox-runtime.bundle` |
| `versitygw` (opt-in) | versity/versitygw v1.5.0 | Local/single-node S3-compatible file-storage gateway where required; not part of `make build` |

Patched `cloud-hypervisor`, the VMM used by `sandbox-ctl`, is built in `sandboxer/native-deps`. `librocksdb`, accelerator's CGO link dependency, is built in the accelerator repository, not here. The pinned versions above describe the current Makefile inputs, not an independent version policy.

<a id="组成"></a>
## Layout

| Path | Role |
| --- | --- |
| `deps/build-{erofs,vmlinux,envd}.sh` | Source-build scripts; vmlinux uses a multi-stage STAGE dispatcher |
| `deps/build-versitygw.sh` | Optional gateway build |
| `deps/common.sh` | Shared tarball download, cache and extraction helpers |
| `deps/linux-patches/` | Architecture-neutral guest-kernel patches |
| `deps/vmlinux/*.config` | Common and per-architecture kernel configuration fragments |

<a id="构建"></a>
## Build

```bash
make build                     # =all: vmlinux + erofs + envd
make erofs                     # mkfs.erofs + fsck.erofs
make vmlinux                   # Guest kernel
make envd                      # E2B guest agent; override the tag with ENVD_TARBALL
make versitygw                 # Optional; not included in build
make build TARGET_ARCH=aarch64 # Cross-build; CROSS_PREFIX is derived automatically

# Kernel patch development (edit and commit in the subshell, then return here).
make linux-fetch && (cd build/src/linux && <edit+commit>) && make linux-patches-format
```

<a id="文档"></a>
## Documentation

- [docs/build.md](docs/build.md): build targets/stages, patch development, cross-compilation and WSL2.
- [../docs/vmlinux.md](../docs/vmlinux.md): kernel configuration and platform ABI boundaries.
