# flatten-ctl e2e — registry pull → flatten → referrer

End-to-end test driving the real `flatten-ctl` binary against a real OCI 1.1
registry ([zot](https://github.com/project-zot/zot)). It validates the remote
path the unit tests can't: pulling from a registry over HTTP, flattening,
ingesting into a store, writing the manifest referrer back via zot's real
Referrers API, and skipping re-export on a second run. (The unit tests use an
in-memory ggcr registry that only does the tag-schema referrers fallback.)

## Run

    make test-e2e-flatten      # also runs as part of `make test-e2e` + the umbrella gate

This builds `flatten-ctl` + the sibling `store-ctl`, prefers a `zot` already on
`PATH` (fetching one into `bin/` only as a fallback), and runs
`test/e2e/e2e_flatten.sh`. Any missing requirement yields a clean `[SKIP]`.

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

Any missing requirement yields a clean `[SKIP]` (exit 0), not a failure.

- `docker`, with the seed image cached (default `python:3.12-alpine`),
- `mkfs.erofs` on `PATH` or via `MKFS_EROFS_PATH`,
- `curl`, and network access to download zot (honours the ambient proxy env),
- the sibling `../accelerator` checkout (builds `store-ctl`),
- `htpasswd` for the basic-auth sub-test (that sub-test is skipped if absent).

## Knobs

- `E2E_IMAGE=<repo:tag>` — cached image to seed (default `python:3.12-alpine`).
- `ZOT_VERSION=<tag>` — zot release to download (Makefile; default pinned).
- `E2E_KEEP=1` — leave the work dir and processes up for debugging.
