[English](CONTRIBUTING.md) | [简体中文](CONTRIBUTING_zh.md)

# Contributing to guest-runtime

Use this entry for source changes, component validation and release preparation. Public contribution and shared CI policy are maintained by the project; the component-specific steps below show what to run and what the evidence proves.

## Contribution workflow

Use a public Fork and a topic branch. Contributors can open issues, review changes
and propose PRs without upstream write access. Maintainers review changes, arrange
trusted validation and squash merge; direct pushes to upstream `main` are not the
contribution path.

## Prepare a Fork

Fork the repository to your GitHub account, then configure HTTPS remotes:

```bash
git clone https://github.com/<your-login>/guest-runtime.git
cd guest-runtime
git remote add upstream https://github.com/kuasar-sandbox/guest-runtime.git
git fetch upstream
git switch -c feat/short-description upstream/main
# edit, test, and commit
git push -u origin feat/short-description
```

For a maintenance fix, start from the supported `upstream/release/vMAJOR.MINOR.x`
branch instead and target that branch in the PR. Enable **Allow edits from
maintainers** when appropriate.

To advance your Fork's `main` without overwriting local commits or uncommitted work:

```bash
git fetch upstream
git switch main
git merge --ff-only upstream/main
git push origin main
```

Commit or safely preserve uncommitted work before switching. If `--ff-only`
refuses because histories diverged, inspect the local commits and resolve the
history deliberately; do not reset or force-push the Fork's main branch as a
routine synchronization step. Create each new topic branch from the freshly
fetched upstream target, not from an unrelated feature branch.

## Pull request requirements

A pull request should:

- have a focused scope and a clear description of the problem and solution;
- link the relevant issue when one exists;
- include or update tests and documentation when behavior changes;
- contain no credentials, private keys, tokens, customer data, or unrelated generated files;
- describe validation already performed and any operational or compatibility risk.

Draft pull requests are welcome for early feedback, but they are not merge candidates.

## Guest kernel release inputs

Automatic `vmlinux` version selection and kernel release notes consider only
`native-deps/deps/vmlinux/*.config` and `native-deps/deps/linux-patches/`.
Changes to config comments also count as release input changes.

A change to build scripts, Makefiles, upstream source versions or checksums, or
compiler/toolchain inputs that can change the kernel artifact MUST update a
`native-deps/deps/vmlinux/*.config` file in the same PR. A config comment
explaining why the kernel needs a new release is sufficient when no option needs
to change. Those other build inputs do not independently trigger a kernel release.

This release-intent rule does not narrow CI validation or caching. Candidate
kernel builds and cache detection retain their complete build-input rules,
including relevant script-only and Makefile changes.

## E2E execution requirements

Every `test/e2e/cases/<suite>.<case>.sh` starts with one static
`# e2e-requires: [...]` JSON array below its shebang. The owner format is
`test/e2e/lib/requirements.json` with `{"schema":1}`. Declare the minimum kernel
and service capabilities used by the case and its helpers; use `[]` explicitly
when none are needed. Missing, duplicate or unknown capabilities fail validation.
Normal pinned source selection discovers additions, removals and requirement
changes without a platform case allowlist. Runner labels and placement belong
to the trusted platform configuration, not cases. See the
[capability contract](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/test/README.md#static-execution-requirements)
for supported names, checks and historical-input compatibility.

## Documentation

Follow the [project documentation policy](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/CONTRIBUTING.md#documentation-contributions). Maintain full English/Chinese pairs at `name.md` and `name_zh.md` with reciprocal selectors. Preserve requirements, identifiers, examples, diagrams, facts and links; record source-backed corrections. Existing complete English-only material may remain English-only. A summary or language-detector pass is not a complete translation.

## Licensing of contributions

By submitting a contribution, you agree that it is licensed under the license
that applies to the files being changed. New project files without a different
explicit license declaration are contributed under the
[Apache License 2.0](LICENSE). The repository's non-Apache boundaries are
documented in [LICENSE_SCOPE.md](LICENSE_SCOPE.md).

Only submit work that you have the right to contribute. Preserve applicable
copyright, attribution, NOTICE, and SPDX declarations when modifying
third-party or differently licensed material.

## Review expectations

Be constructive and specific. Authors should resolve review conversations or explain why a requested change is not applicable. Maintainers may close stale, superseded, unsafe, or out-of-scope pull requests with an explanation.

## CI admission and merge evidence

The [central CI contract](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/ci.md) is the authority for trusted admission, workflow/status identifiers, companions, source selection and merge evidence. The default-branch wrapper owns admission; candidate workflows do not acquire control-plane or release credentials. Any contributor may open a public Fork PR; privileged execution is a separate trust decision. External candidates that are not admitted need maintainer review and a trusted upstream candidate preserving authorship and the reviewed source, rather than weaker admission.

Before requesting a normal reviewed squash merge:

1. Make the PR ready, target `main` or a supported maintenance branch, and resolve blocking review conversations.
2. Validate the current exact target-base/reviewed-head integration and required tests; skipped, cancelled, failed or old runs do not establish success.
3. Validate each declared companion independently and recheck its repository, PR, base, head, target ref and integration against the recorded source set.
4. Refresh evidence after base/head/companion changes. Body edits do not trigger execution; use the supported Draft-to-Ready event when necessary. Rerun an old event only for a diagnosed transient failure with unchanged admitted inputs.
5. Immediately before merge recheck the live source set. After a companion merges, remove obsolete declarations and rerun remaining candidates.

Do not manufacture statuses, add manual dispatches to bypass admission, weaken branch protection or retain source-fetch credentials in candidate execution. Auto-merge, merge commits and rebase merges are not used. The central contract supplies exact current checks and full trust-boundary details.

## Component build and validation

Follow [Build](docs/build.md) for exact source modes and production inputs. Run package tests using the selected workspace, and use the [prepared E2E guide](test/e2e/README.md) for prebuilt-product tests. The [kernel validation section](docs/vmlinux.md#validation) distinguishes static artifact checks from real boot/ABI evidence. Patch-tree maintenance and safe cleanup are in [native-deps](native-deps/README.md). Shared workflow names and runner routing remain in the central CI contract.
