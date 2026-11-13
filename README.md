[English](README.md) | [简体中文](README_zh.md)

# guest-runtime

For the first cross-component sandbox or Demo, follow the project
[Quick Start](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/quickstart.md)
with the matching aggregate release and
[workbench](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/workbench/README.md).
This component can still be built/deployed independently; workbench is not a
production runtime dependency. Published architectures are specific to the
selected release; source support does not imply all historical assets exist.

`guest-runtime` builds the **guest kernel, runtime image, and image-building tools** used by [Kuasar Sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox).

The repository owns the guest environment and its build inputs, provenance, and release artifacts. Guest PID 1 source and its shared ABI packages belong here. Host-side MicroVM lifecycle, snapshot and restore belong to [`sandboxer`](https://github.com/kuasar-sandbox/sandboxer). Shared Manifest, cache/store, OCI retrieval, and image-flattening libraries belong to [`accelerator`](https://github.com/kuasar-sandbox/accelerator).

## Responsibilities

- build the guest VMLinux image from a documented kernel source, configuration, and patch set;
- build native guest dependencies such as Envd and use environment-installed host EROFS tools for packaging and validation;
- build `flatten-ctl`, the OCI/directory-to-EROFS image builder;
- assemble `sandbox-runtime.bundle`, including the locally built `sandbox-init` and the guest payloads maintained here;
- validate the produced guest/runtime filesystem structure;
- publish and document two independently versioned release units: Runtime and VMLinux.

The repository is one component repository even though it publishes two release-unit version lines.

## Repository layout

| Path | Purpose |
| --- | --- |
| `cmd/sandbox-init` | Guest PID 1: runtime setup, supervision and control protocol |
| `pkg/guestabi` | Shared control, MUX and forward protocol packages consumed by Host and Guest |
| `cmd/flatten-ctl` | OCI or directory to deterministic EROFS image builder, using shared `accelerator` packages |
| `native-deps/` | Guest VMLinux/Envd source inputs, patch trees and native maintenance |
| `scripts/guest-inspect.py` | Read guest kernel memory counters from the host using a compatible nonrandomized x86_64 layout |

The runtime image places its initial guest tools under `/opt/sandbox-runtime/bin/`. VMLinux and the Cloud Hypervisor binary are not embedded in the runtime bundle: VMLinux is published as its own release unit, and Cloud Hypervisor is built and published by `sandboxer`.

## Choose a workflow or delivery contract

| Reader task | Guide |
| --- | --- |
| Consume a selected release, prepare an application and bind its artifacts | [Using guest-runtime](docs/using-guest-runtime.md), including verification, cold-start example, upgrades and recovery |
| Inspect the Runtime byte layout, payload, identity and compatibility | [Runtime bundle](docs/runtime-bundle.md) |
| Check supported guest-kernel capabilities, architectures and exact delivery identity | [VMLinux](docs/vmlinux.md) |
| Export, inspect, cache or convert an image with the published CLI | [flatten-ctl](docs/flatten-cli.md) |
| Build or reproduce a selected Runtime/Kernel release | [Build](docs/build.md), including source modes, dependency selection, native targets and package validation |

The [shared image-preparation implementation](https://github.com/kuasar-sandbox/accelerator/blob/main/docs/image-preparation.md) belongs to accelerator. Kernel patch development and safe incremental source/cache handling stay next to their inputs in [native-deps](native-deps/README.md). [Contributing](CONTRIBUTING.md) covers source changes, release intent and validation. Each target guide has a full Chinese edition.

## Integration boundaries

- `guest-runtime` builds `sandbox-init` and maintains its [Guest ABI](docs/guest-abi.md); `sandboxer` owns the host lifecycle;
- `accelerator` owns the shared EROFS/OCI flattening libraries; this repository publishes the `flatten-ctl` binary in the Runtime release unit;
- `orchestrator` consumes the produced Runtime and VMLinux artifacts through the complete platform;
- `kuasar-sandbox/kuasar-sandbox` selects exact release units, runs cross-component validation, and publishes aggregate releases.

## Release selection

Runtime and VMLinux are independently versioned release units from this one repository. Use the exact tags selected by a validated aggregate release, with each package built for its declared architecture. [Using guest-runtime](docs/using-guest-runtime.md) explains selection and verification; [Build](docs/build.md) explains selected-source reproduction. Shared release-line, Preview, cleanup and Latest rules are defined once in the [project release contract](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release.md).

## Contributing and security

Read the repository-specific [contribution guide](CONTRIBUTING.md) and the [organization contribution guide](https://github.com/kuasar-sandbox/.github/blob/main/CONTRIBUTING.md). Changes to the guest ABI, runtime contents, kernel configuration, source provenance, or release artifacts require the corresponding validation and any necessary companion pull requests.

Do not report vulnerabilities or disclose private package sources, credentials, signing material, or customer data in public issues. Use the [Kuasar Sandbox Security Policy](https://github.com/kuasar-sandbox/kuasar-sandbox/security/policy) and GitHub private vulnerability reporting.

## License

Original project content is licensed under the [Apache License 2.0](LICENSE). Kernel build inputs retain the licensing boundary documented in [`LICENSE_SCOPE.md`](LICENSE_SCOPE.md). Runtime packages, EROFS tools, Envd, Buildroot/distribution inputs, and other third-party materials retain their own licenses, notices, source, and redistribution obligations.
