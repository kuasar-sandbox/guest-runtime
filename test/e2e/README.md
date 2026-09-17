# Registry and flattening E2E

[English](README.md) | [简体中文](README_zh.md)

This suite drives real Docker, zot OCI 1.1 Referrers, `flatten-ctl`, EROFS and
`store-ctl`. No remote workload image or private runner cache is required by
the default fixture. The working-set VM matrix remains a separately
owned platform check; this suite does not replace it.

## Run and prerequisites

Run `make test-e2e`, or pass the assembled binary directory and zot explicitly:

```sh
BIN=/path/to/assembled/bin ZOT_BIN=/path/to/zot bash test/e2e/run_all.sh
make test-e2e-scripts
```

The real test requires Linux, root or passwordless sudo, Python 3, curl, Docker
with a usable daemon, GNU timeout, `mkfs.erofs`, `flatten-ctl`, `store-ctl`, and
zot. `run_all.sh` requires all prerequisites and helpers; missing or unusable
ones fail. Direct `e2e_flatten.sh` invocations may explicitly use
`REQUIRE_GUEST_RUNTIME=0` for optional local checks, which print `[SKIP]`.
That mode is not CI acceptance. Platform `make e2e-tools` supplies zot; release
packages do not contain the test registry. Private source dependencies still
need authorized access when building binaries; execution with public release
binaries is distinct from testing candidate source.

## Preserved test intentions

| Intention | Executed assertions |
| --- | --- |
| Registry export and EROFS | Export produces a valid subject digest and nonempty EROFS; JSON info validates runtime configuration. |
| OCI Referrers and idempotence | Supported miss, upload, put, then repeated hits of the same ID; real Referrers API contains that artifact. |
| Owner/key isolation | A second owner initially misses; different keys produce different IDs; each owner subsequently retrieves its own ID. |
| Expiration | A short-lived record is observed LIVE, then becomes a supported miss; the exact expired artifact remains indexed. |
| Authentication | The live registry returns HTTP 401 to anonymous access; CLI failure must be authentication-related; explicit credentials allow upload/put and a subsequent hit. |

The default fixture is a small deterministic two-layer Docker archive generated
locally, including non-root ownership, executable content, symlinks, deletion
whiteouts and an opaque directory. Offline tests validate these fixture bytes
and their merge semantics; they are not a claim that every filesystem semantic
was independently checked in the exported EROFS. `E2E_IMAGE` can select an
already cached caller image; the suite never pulls or deletes that input tag.

## Isolation and cleanup

Each run has private temporary directories and its own `DOCKER_CONFIG`.
Caller credentials, credential helpers and ambient registry credentials are not
used. Only the local registry's fixed test credentials are configured.
Generated tags and child processes belong to that invocation. Process cleanup
is bounded and reaps descendants, including detached grandchildren. INT/TERM
preserve exit codes 130/143. Cleanup also runs after partial startup failures;
unrelated resources are not removed. `E2E_KEEP=1` retains evidence only after
owned services and tags have been stopped and removed.

The platform assembler copies the complete `test/e2e` tree, including Python
helpers. Offline regression checks exercise required-prerequisite failures,
malformed JSON, fixture integrity, caller-credential isolation, copied-package
completeness, normal/error exits, signals and repeated cleanup.
