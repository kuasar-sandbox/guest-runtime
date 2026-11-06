# Guest kernel patches

Linux 6.1.169 uses one architecture-neutral virtio balloon patch. Inflation batches
order-0 page allocation and list enqueue within the upstream 256-PFN request.
See [the guest kernel specification](../../../docs/vmlinux.md#balloon-behavior-under-memory-pressure).

This directory remains an input to the generic `linux-patches-apply` and
`linux-patches-format` stages. Future architecture-neutral patches belong here
as `*.patch` files, with appropriate license metadata. See
[the native build guide](../../README.md#kernel-patch-development).

With an empty patch set, `linux-patches-apply` requires source `HEAD` to equal
`linux-patches-base`. It refuses to reuse a source tree containing removed
patches or unformatted work. Preserve that work and select a fresh source and
build directory before building the unpatched kernel; `make clean` preserves
the existing source tree.
