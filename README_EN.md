# Kuasar Sandbox Guest Runtime

[简体中文](README.md) · [Kuasar Sandbox project](https://github.com/kuasar-sandbox/kuasar-sandbox)

`guest-runtime` builds and publishes the guest environment used by Kuasar Sandbox MicroVMs. It owns the guest kernel, runtime image, guest initialization/control software, image-construction inputs, and the source and license traceability needed to reproduce those artifacts.

This is one component repository with two independently versioned release units:

- **Runtime** — the guest userspace/runtime image and related metadata;
- **VMLINUX** — the guest Linux kernel artifact and its source/configuration/patch provenance.

The component can be consumed by the complete Kuasar Sandbox platform or built and released according to its own artifact contracts. The project repository owns system-level documentation, cross-component BMS/E2E, demos, and aggregate release selection.

## What the guest environment provides

The guest environment is intentionally smaller than a general-purpose VM distribution while still supporting Agent, Serverless, and reinforcement-learning workloads.

Depending on the selected profile and build, it provides:

- a Linux guest kernel configured for the MicroVM device and control model;
- a root filesystem/runtime image suitable for block-device-backed boot;
- a dedicated guest initialization process as PID 1;
- application process supervision and lifecycle signaling;
- host–guest control and execution channels;
- storage, network, balloon, vsock, and other drivers required by the selected platform release;
- optional guest services used by the E2B-compatible profile;
- image-building tools that transform declared inputs into release artifacts.

The exact package list, kernel configuration, guest services, and profile behavior are defined by the source commit and release unit, not by a generic promise that every Linux package or service is present.

## Runtime and VMLinux release units

### Runtime

Runtime tags use names such as:

```text
runtime-v0.1.2
runtime-v0.1.3-preview.20260904
```

A Runtime release should identify:

- the source commit;
- declared package/rootfs inputs;
- guest initialization/control version;
- filesystem-image build inputs;
- toolchain or container image used by the build;
- included license and notice material;
- checksums and validation results.

### VMLinux

VMLINUX tags use names such as:

```text
vmlinux-v0.1.1
vmlinux-v0.1.2-preview.20260902
```

A VMLinux release should identify:

- upstream Linux source version and source location;
- project kernel configuration;
- applied patches and their provenance;
- compiler/toolchain inputs;
- resulting kernel artifact and checksum;
- GPL source-availability and notice obligations.

Runtime and VMLinux versions can advance independently. An aggregate Kuasar Sandbox release selects one exact version of each.

## Component boundaries

| Component | Guest-runtime interaction |
| --- | --- |
| [`sandboxer`](https://github.com/kuasar-sandbox/sandboxer) | Boots the selected VMLinux and Runtime, coordinates guest initialization/control, and consumes the supported guest-device contract |
| [`orchestrator`](https://github.com/kuasar-sandbox/orchestrator) | Selects the guest profile/artifacts through node and template configuration and exposes the higher-level lifecycle |
| [`accelerator`](https://github.com/kuasar-sandbox/accelerator) | Pulls and prepares OCI/rootfs inputs and can store or distribute Runtime/image artifacts through configured data paths |
| [`connector`](https://github.com/kuasar-sandbox/connector) | Provides the host-side sandbox network; the Runtime supplies the compatible guest network stack and drivers |
| [`kuasar-sandbox`](https://github.com/kuasar-sandbox/kuasar-sandbox) | Selects exact Runtime, VMLinux, and component versions and validates the complete platform |

## Build from public sources

A public build must not depend on organization-only repositories, internal package mirrors, private registries, unpublished patches, or a maintainer's undeclared cache.

Use the repository `Makefile`, current Chinese README, and `docs/` tree as the authoritative source for the selected commit. A clean build environment generally needs:

- the documented container engine and/or host toolchain;
- public Linux kernel sources and compiler prerequisites;
- the declared Runtime/rootfs package or build-system sources;
- EROFS and other image-construction tools used by the selected path;
- sufficient disk, memory, and build time;
- network access to the public sources declared by the build.

The repository should expose separate, repeatable entry points for Runtime and VMLinux. Build logs must clearly identify when an input came from a local cache; a missing public input must fail rather than silently switching to an internal mirror.

## Validate artifacts

Validation should cover more than successful compilation.

### Runtime validation

- image/rootfs structure and expected filesystem type;
- required guest binaries, libraries, configuration, and init entry point;
- absence of private keys, SSH host keys, internal CAs, machine identities, package-manager credentials, and internal repository configuration;
- package/source/license inventory;
- boot and guest-control checks in a real MicroVM;
- checksum and release-manifest consistency.

### VMLinux validation

- source, config, patch, and toolchain traceability;
- required MicroVM drivers and kernel options;
- absence of unintended debug or private signing material;
- boot with the selected Runtime and sandboxer release;
- checksum and release-manifest consistency;
- GPL source availability corresponding to the distributed binary.

Cross-component boot and lifecycle validation runs through the project BMS using an exact source set.

## Supply-chain and fork-CI boundaries

Kernel and Runtime builds execute large, privileged, and supply-chain-sensitive workloads. External fork code must not automatically receive:

- package-registry, source-mirror, signing, or release credentials;
- organization-private repository access;
- trusted builder caches shared with Stable releases;
- write access to release tags or assets;
- control-plane or long-lived infrastructure credentials.

Candidate builds should use isolated builders, commit- and toolchain-bound caches, immutable input references, and explicit approval before running on privileged or release-adjacent infrastructure.

## Licensing and source obligations

The repository contains material under more than one license scope.

- Project-owned build scripts and tools are generally covered by the repository's Apache License 2.0 terms.
- The Linux kernel and kernel-derived material remain subject to GPL and upstream copyright obligations.
- Kernel patches retain their source and applicable licensing context.
- Runtime packages, libraries, guest services, EROFS tools, build-system inputs, firmware, and other third-party components retain their own licenses and redistribution terms.
- Binary releases may require notices, copyright inventories, corresponding source, source offers, or other material in addition to the repository root license.

Do not label an entire Runtime or VMLinux archive Apache-2.0 merely because project-owned scripts use that license. Consult the repository's license-scope, source-provenance, and release-notice documentation.

## Documentation

- [Project English overview](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/README_EN.md)
- [English Quick Start](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/quickstart_en.md)
- [Project architecture](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/kuasar-sandbox.md)
- [English release overview](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/releases_en.md)
- [Repository documentation](docs/)

Detailed kernel and Runtime design documents may remain Chinese during the initial source-publication phase. Public build inputs, source provenance, license obligations, artifact contracts, and security boundaries must retain an accurate English entry point.

## Releases

- [Guest Runtime releases](https://github.com/kuasar-sandbox/guest-runtime/releases)
- [Aggregate Kuasar Sandbox releases](https://github.com/kuasar-sandbox/kuasar-sandbox/releases)

Use the Runtime and VMLinux versions selected by one aggregate release. Do not infer compatibility only from similar version numbers and do not replace an existing stable tag or asset in place.

## Contributing

Read the project [English contribution guide](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/CONTRIBUTING_EN.md). Keep Runtime, Kernel, and build-system changes independently reviewable where possible. Document every new source, package, patch, binary, license, cache, and build prerequisite. Cross-component guest-device or lifecycle changes may require linked companion pull requests and exact-source-set BMS validation.

## Security

Do not report vulnerabilities in public issues. Use the project [English Security Policy](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/SECURITY_EN.md) and GitHub private vulnerability reporting.

Security-sensitive reports can include malicious image construction, guest-init/control bypass, unsafe kernel configuration or patches, embedded credentials, compromised public inputs, cache poisoning, release-builder privilege escalation, or license/source-material substitution. Remove credentials, internal endpoints, customer data, and unredacted build logs from public material.

## License

See [LICENSE](LICENSE) and the repository's license-scope/source-provenance documentation. Each distributed file and binary remains governed by its applicable license and source-availability obligations.