[English](README.md) | [简体中文](README_zh.md)

# guest-runtime

`guest-runtime` builds the **guest kernel, runtime image, and image-building tools** used by [Kuasar Sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox).

The repository owns the guest environment and its build inputs, provenance, and release artifacts. Host-side MicroVM lifecycle, snapshot, restore, and `sandbox-init` source belong to [`sandboxer`](https://github.com/kuasar-sandbox/sandboxer). Shared Manifest, cache/store, OCI retrieval, and image-flattening libraries belong to [`accelerator`](https://github.com/kuasar-sandbox/accelerator).

## Responsibilities

- build the guest VMLinux image from a documented kernel source, configuration, and patch set;
- build native guest dependencies such as EROFS tools and Envd;
- build `flatten-ctl`, the OCI/directory-to-EROFS image builder;
- assemble `sandbox-runtime.bundle`, including `sandbox-init` from `sandboxer` and the guest payloads maintained here;
- validate the produced guest/runtime filesystem structure;
- publish and document two independently versioned release units: Runtime and VMLinux.

The repository is one component repository even though it publishes two release-unit version lines.

## Repository layout

| Path | Purpose |
| --- | --- |
| `cmd/flatten-ctl` | OCI or directory to deterministic EROFS image builder, using shared `accelerator` packages |
| `native-deps/` | VMLinux, EROFS tools, Envd, and other native guest build inputs |
| `docs/sandbox-runtime.md` | Runtime image layout, guest payload, build, and release contract |
| `docs/vmlinux.md` | Guest kernel source/configuration contract and platform ABI |
| `docs/flatten.md` | `flatten-ctl`, remote image retrieval, cache, and OCI Referrers behavior |
| `scripts/guest-inspect.py` | Read guest kernel memory counters from the host using a compatible nonrandomized x86_64 layout |

The runtime image places its initial guest tools under `/opt/sandbox-runtime/bin/`. VMLinux and the Cloud Hypervisor binary are not embedded in the runtime bundle: VMLinux is published as its own release unit, and Cloud Hypervisor is built and published by `sandboxer`.

## Source workspace

The source build uses sibling repositories. `go.mod` resolves `accelerator` through `../accelerator`, and the Runtime image consumes or builds `sandbox-init` through `../sandboxer`:

```text
<workspace>/
├── guest-runtime/
├── accelerator/
└── sandboxer/
```

For coordinated development, use compatible `main` revisions from these sibling repositories. To reproduce a released composition, use the exact component tags selected by the corresponding [project aggregate release](https://github.com/kuasar-sandbox/kuasar-sandbox/releases) rather than independently choosing GitHub Latest tags. The complete six-repository workspace is documented in the [project README](https://github.com/kuasar-sandbox/kuasar-sandbox).

Go-only `flatten-ctl` builds require a sibling `accelerator` checkout. Building the
Runtime from source also requires `sandboxer` for `sandbox-init`, plus the
host/target native tools below. Use `GOWORK=off` for this minimal source closure:
the guest `sandbox-init` target does not import Connector, while a shared Go
workspace can load unrelated host-side Sandboxer dependencies. The full host
Sandboxer build still needs Connector; Runtime does not add it as a release input.
The standalone
Kernel target uses its own Native build inputs; it does not require starting the
platform. Internal `require` versions identify target formal component releases,
with Daily Preview suffixes removed. Those tags may not exist yet: local `replace`
directives select the sibling sources, including when `GOWORK=off`. Record their
actual SHAs when reporting a build or test result.

## Build prerequisites

Builds use environment-provided Go and inherit its `GOROOT` and `GOTOOLCHAIN` selection. Release automation requires a working `gh` with `api --slurp` support on `PATH`; the project does not install, replace, or authenticate these environment tools against fixed binary digests. EROFS tools remain built from the project-selected sources, patches, and recipes.

Building `sandbox-runtime.bundle` requires a **host-architecture** `mkfs.erofs` executable. Install `erofs-utils` on the build host or set:

```bash
BUILD_MKFS_EROFS=/path/to/host/mkfs.erofs make sandbox-runtime
```

The host packer is separate from `native-deps/bin/<target-arch>/mkfs.erofs`, which is the target-architecture static binary copied into the guest Runtime image. This distinction is required for cross-builds: an aarch64 guest binary cannot package an image on an x86_64 host.

Other prerequisites and native-source locations are documented in [Native build and maintenance](native-deps/README.md).

## Build

```bash
GOWORK=off make native-deps                # VMLinux, target EROFS tools, Envd, and native inputs
GOWORK=off make flatten-ctl                # OCI/directory -> deterministic EROFS builder
GOWORK=off make build                      # assemble sandbox-runtime.bundle
GOWORK=off make sandbox-runtime            # build the runtime image, building sandbox-init if needed
GOWORK=off make build TARGET_ARCH=aarch64  # cross-build where documented dependencies support it
```

`make sandbox-runtime` consumes:

- `../sandboxer/bin/<arch>/sandbox-init`;
- `native-deps/bin/<arch>/envd`;
- `native-deps/bin/<arch>/mkfs.erofs` as the guest payload;
- `bin/<arch>/flatten-ctl`;
- `BUILD_MKFS_EROFS` as the host image packer.

When `sandbox-init`, Envd or `flatten-ctl` is absent, the Makefile invokes its corresponding build target. Every ordinary Runtime build also checks the EROFS recipe before consuming the repository-managed target-architecture guest `mkfs.erofs`, even if that executable already exists. An explicit `GUEST_MKFS_EROFS` override is used as supplied and must be executable. The host `mkfs.erofs` is different: it must already be available on `PATH`, at a documented native-deps host path, or through `BUILD_MKFS_EROFS`; otherwise the Runtime build fails explicitly. A clean public build must use documented public source and download locations and must not depend on a developer's private package mirror or cache.

## Outputs

| Output | Build target |
| --- | --- |
| `bin/<arch>/flatten-ctl` | `make flatten-ctl` |
| `bin/<arch>/sandbox-runtime.bundle` | `make sandbox-runtime` |
| `native-deps/bin/<arch>/vmlinux` | `make native-deps` |
| `native-deps/bin/<arch>/mkfs.erofs` / `fsck.erofs` | `make native-deps` |
| `native-deps/bin/<arch>/envd` | `make native-deps` |

## Two release units

This repository does not publish a generic `guest-runtime-vX.Y.Z` release. It maintains two independent version lines:

- **`runtime-vX.Y.Z`** — publishes `sandbox-runtime-x86_64-vX.Y.Z.tar.gz`, containing the runtime bundle, `flatten-ctl`, and the EROFS creation tool selected by the release contract;
- **`vmlinux-vX.Y.Z`** — publishes `vmlinux-x86_64-vX.Y.Z.tar.gz`, containing the guest kernel at the stable `bin/vmlinux` path.

The two version numbers may advance independently. The project aggregate release selects an exact Runtime tag and an exact VMLinux tag; it does not assume that their version numbers match.

Current GitHub component assets are published for Linux x86_64 from selected source refs and exact commits after their component build and packaging checks. The project aggregate release later selects exact Runtime, VMLinux, and other component tags and performs cross-component integration tests plus released-asset MicroVM validation for that composition. Source Makefiles may support another `TARGET_ARCH`, but source-build support does not by itself mean a prebuilt artifact is published for that architecture.

Runtime publication embeds `sandbox-init` from the selected sandboxer Release. It verifies the Release tag/source, asset sizes and GitHub digests, and `SHA256SUMS` before installing the binary; the selected sandboxer source remains available for provenance and license validation. Rebuilding the same commit can change Go build metadata or workspace-derived defaults, so a source rebuild does not establish byte identity with the published dependency.

## Kernel source and licensing

A VMLinux release must be traceable to its public kernel source version, configuration, project patch set, toolchain, and source commit. Kernel patches and copied kernel material retain their upstream copyright and GPL obligations. Project-original build scripts do not relicense the Linux kernel.

The Runtime bundle may contain software under multiple licenses. Its package and native-dependency inputs, notices, source availability, and redistribution obligations must be reviewed as part of the release contract. Never add an internal-only package, private CA, SSH host key, machine identity, production credential, or untraceable prebuilt binary to the guest image.

The repository-wide license boundaries are described in [`LICENSE_SCOPE.md`](LICENSE_SCOPE.md). Native build details and source locations are documented in [Native build and maintenance](native-deps/README.md).

## Integration boundaries

- `sandboxer` owns `sandbox-init` source and the host lifecycle; this repository packages the built guest binary into the Runtime image;
- `accelerator` owns the shared EROFS/OCI flattening libraries; this repository publishes the `flatten-ctl` binary in the Runtime release unit;
- `orchestrator` consumes the produced Runtime and VMLinux artifacts through the complete platform;
- `kuasar-sandbox/kuasar-sandbox` selects exact release units, runs cross-component validation, and publishes aggregate releases.

## Release model

Runtime and VMLinux component releases are built from selected source refs and exact commits. Preview releases are GitHub prereleases for development and evaluation; mainline Stable releases are coordinated independently for each release unit. The project aggregate release always selects exact tags and does not rely on GitHub Latest, then validates the selected composition through project-level integration tests and released-asset testing.

See the [project release documentation](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release.md) and the [latest Stable aggregate release](https://github.com/kuasar-sandbox/kuasar-sandbox/releases/latest).

## Documentation

Detailed design and reference documents have complete English/Chinese pairs. English uses the default filename; the language selector opens the full Chinese version:

- [`docs/sandbox-runtime.md`](docs/sandbox-runtime.md) — Runtime image layout, guest payload, build, release, and consumption contract;
- [`docs/vmlinux.md`](docs/vmlinux.md) — guest kernel configuration, build, platform ABI, and source relationship;
- [`docs/flatten.md`](docs/flatten.md) — `flatten-ctl`, deterministic flattening, remote retrieval, caching, and OCI Referrers;
- [Native build and maintenance](native-deps/README.md) — native-dependency source and build workflow.

Keep maintained pairs synchronized according to the [documentation policy](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/CONTRIBUTING.md#documentation-contributions).

## Contributing and security

Read the repository-specific [contribution guide](CONTRIBUTING.md) and the [organization contribution guide](https://github.com/kuasar-sandbox/.github/blob/main/CONTRIBUTING.md). Changes to the guest ABI, runtime contents, kernel configuration, source provenance, or release artifacts require the corresponding validation and any necessary companion pull requests.

Do not report vulnerabilities or disclose private package sources, credentials, signing material, or customer data in public issues. Use the [Kuasar Sandbox Security Policy](https://github.com/kuasar-sandbox/kuasar-sandbox/security/policy) and GitHub private vulnerability reporting.

## License

Original project content is licensed under the [Apache License 2.0](LICENSE). Linux kernel patches retain the GPL-2.0-only boundary documented in [`LICENSE_SCOPE.md`](LICENSE_SCOPE.md). Runtime packages, EROFS tools, Envd, Buildroot/distribution inputs, and other third-party materials retain their own licenses, notices, source, and redistribution obligations.
