[English](README.md) | [简体中文](README_zh.md)

# Registry and flattening E2E

Guest-runtime product E2E is consumed through the platform-owned prepared-workspace runner. The component contributes two independent `image` cases and guest-runtime-specific helpers; it does not provide a separate product-E2E runner.

The execution model is:

```text
prebuilt products -> prepare -> <suite>.<case>.sh -> run
```

The cases are:

- `image.flatten.sh`: OCI image configuration and ownership through real flatten/EROFS output.
- `image.registry.sh`: live registry, OCI 1.1 Referrers, Store, owner/key isolation, expiry and authentication.

## Run and prerequisites

Use the runner and cases from a Kuasar platform test bundle or exact integration prepared workspace. For example:

```sh
RUNNER=/path/to/platform/test/e2e/e2e
RELEASE_DIR=/path/to/prebuilt/platform-release
WORK=/tmp/kuasar-e2e

"$RUNNER" prepare --release-dir "$RELEASE_DIR" --workdir "$WORK"
"$RUNNER" run --workdir "$WORK" \
  --include image.flatten.sh \
  --include image.registry.sh
```

The product cases consume only prepared `BIN`, `E2E_WORKSPACE`, `E2E_LIB`, prepared fixture inputs and prepared test helpers. They do not build products or helpers, discover sibling source repositories, pull fallback workload images, or fall back to source during execution.

The selected cases require Linux, root, Python 3, curl, a usable Docker daemon, GNU timeout, `mkfs.erofs`, `flatten-ctl`, `store-ctl`, and the prepared zot helper. Missing or unusable selected prerequisites fail the case; they are not reported as a successful skip. Architecture and host capabilities are execution conditions, not suites.

Source/helper regressions remain outside product E2E and can be run with:

```sh
make test-e2e-scripts
```

## Preserved test intentions

| Case | Executed assertions |
| --- | --- |
| `image.flatten.sh` | Export produces a valid subject digest and nonempty EROFS; JSON info preserves Architecture, OS, User, WorkingDir, Entrypoint and Env from the prepared image; ownership and layer semantics remain valid. |
| `image.registry.sh` | Supported miss, upload/put and idempotent hits; live OCI Referrers records; owner/key isolation; finite-lifetime expiry behavior; anonymous authentication denial and explicit-auth success. |

The prepared fixture is a small deterministic two-layer Docker archive, including non-root ownership, executable content, symlinks, deletion whiteouts and an opaque directory. Source/helper regressions validate those fixture bytes and merge semantics; artifact E2E consumes the prepared fixture instead of regenerating it as a hidden fallback.

## Isolation and cleanup

Each case uses private temporary state and its own `DOCKER_CONFIG`. Caller credentials, credential helpers and ambient registry credentials are not used. Generated tags and child processes belong to the invocation. Process cleanup is bounded and reaps descendants, including detached grandchildren. INT/TERM preserve exit codes 130/143. Cleanup also runs after partial startup failures; unrelated resources are not removed. `E2E_KEEP=1` retains evidence only after owned services and tags have been stopped and removed.

Shared runner routing, provenance validation and architecture lanes are maintained by the platform CI contract. This guide owns only guest-runtime case intent and prerequisites.
