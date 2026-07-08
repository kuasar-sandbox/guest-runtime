# flatten-ctl e2e — registry pull → flatten → referrer

End-to-end test driving the real `flatten-ctl` binary against a real OCI 1.1
registry ([zot](https://github.com/project-zot/zot)). It validates the remote
path the unit tests can't: pulling from a registry over HTTP, flattening,
ingesting into a store, writing the manifest referrer back via zot's real
Referrers API, and skipping re-export on a second run. (The unit tests use an
in-memory ggcr registry that only does the tag-schema referrers fallback.)

## Run

    make test-e2e              # also runs as part of the release-builder umbrella gate

This builds `flatten-ctl` + the sibling `store-ctl` and runs
`test/e2e/e2e_flatten.sh` against `ZOT_BIN` or a `zot` already on `PATH`.
When run from the source-tree release-builder umbrella, `make e2e-tools`
downloads zot into `build/e2e-tools/` and passes `ZOT_BIN`; release packages do
not ship zot. Under `make test-e2e`, missing requirements fail via
`REQUIRE_GUEST_RUNTIME=1`; direct ad-hoc script runs may still skip soft
prerequisites for local convenience.

## What it checks

1. Starts `store-ctl` (fs backend) and `zot` on ephemeral localhost ports.
2. Seeds a locally-cached docker image into zot via `docker push`.
3. Runs flatten-ctl and asserts:
   - pull + flatten + `info` produce a valid EROFS;
   - `--upload --with-referer` prints a manifest id and writes a referrer;
   - a second run reuses the same id and skips re-export (idempotent), proving
     the round-trip through zot's real Referrers API;
   - a different customer key does not reuse the referrer (owner isolation);
   - a basic-auth registry rejects anonymous pulls and accepts credentialed
     ones (`FLATTEN_REGISTRY_*`).

## Requirements

- `docker`, with the seed image cached (default `python:3.12-alpine`),
- `mkfs.erofs` on `PATH` or via `MKFS_EROFS_PATH`,
- `curl`,
- `zot` on `PATH` or via `ZOT_BIN`,
- the sibling `../accelerator` checkout (builds `store-ctl`).

## Knobs

- `E2E_IMAGE=<repo:tag>` — cached image to seed (default `python:3.12-alpine`).
- `ZOT_BIN=<path>` — override the zot executable.
- `E2E_KEEP=1` — leave the work dir and processes up for debugging.
