# The j4k Forgejo fork

This fork runs `code.j4k.dev`. Its upstream base is Forgejo `v17.0.1`
(`363f3dc30beef902b187af5a5244ff7af44901e6`). The latest release is
`v17.0.1-j4k.1`, published as `ghcr.io/jercik/forgejo:17.0.1-j4k.1-rootless`.

## Branch model

`v17.0/j4k` is rebase-managed: it is the upstream release tag, then any temporary
upstream backports, then one commit per carry. Bumping the base means rebasing those carry commits
onto the new upstream tag and force-pushing the branch with `--force-with-lease`;
never merge or squash-merge upstream history into it, because a squash turns the
whole upstream delta into one fork commit and hides the real merge-base.
Release tags `v<upstream>-j4k.N` are immutable and keep every shipped state
reachable after a rebase.

The upstream `v17.0.0` to `v17.0.1` delta adds no `models/forgejo_migrations`
migrations, and the fork delta adds none. Whether a given bump needs the
cluster's coordinated database and data-volume restore for rollback depends on
the upstream delta it carries, not on the fork patches. See the cluster's
`docs/adr/0010-forgejo-fork-image.md` and `roles/forgejo/README.md` for the
backup gate and pin history.

## Carries

- Token-authenticated PR review conversation resolution through `POST` and
  `DELETE /repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments/{comment}/resolution`.
  Actions job tokens are allowed and checked against the task's own repository
  permission. Drop this carry when the pinned upstream release ships an
  equivalent API; as of `v17.0.1`, upstream still has none.
- Pending reviews remain scoped to the negative synthetic Actions reviewer ID
  (`FindReviewOptions.ReviewerID != 0` instead of `> 0`). The
  `actions-reviewer-isolation` version capability advertises this guarantee.
  The change also affects `dismissPriors` in `services/pull/review.go`, which
  passes the review's `ReviewerID` after loading the reviewer. For a deleted
  reviewer (ghost, `-1`) or an Actions-authored review (`-2`), upstream drops
  the filter and dismisses every undismissed review on the PR, whereas the fork
  dismisses only that reviewer's reviews.
  Drop the carry when upstream provides and tests the same isolation; as of
  `v17.0.1`, upstream still filters on `ReviewerID > 0`.
- The `POST .../actions/runs/{run_id}/rerun-failed-jobs` endpoint and the
  `actions-rerun` capability. The run rerun (`.../runs/{run_id}/rerun`) and job
  rerun (`.../jobs/{job_id}/rerun`) endpoints come from upstream since
  `v17.0.0`; the capability's contract is that all three endpoints exist. The
  `rerun-failed-jobs` endpoint is fork-invented and has no upstream equivalent;
  drop the capability only when all three exist upstream.
- Container-registry write races. A concurrent push of an identical manifest or
  blob hit a unique violation (`UQE_package_version_s`,
  `UQE_package_blob_sha256`) and answered 500, and a lost blob insert panicked
  on a nil blob in `services/packages/container/blob.go`. The manifest and blob
  writes now run in `db.RetryTx`, and cleanup deletes only content that the
  attempt's own insert created, never the winner's file. Drop the carry when
  upstream makes these writes idempotent; as of `v17.0.1` it does not.
- Release creation tolerates the background tag sync. `insertRelease` in
  `services/release/release.go` adopts a tag-only row that the sync wrote
  between the existence check and the insert, instead of answering 500 on
  `UQE_release_n`. A regular release at the tag still yields
  `ErrReleaseAlreadyExist`. Drop the carry when upstream handles the same race;
  as of `v17.0.1` it does not.
- `GET .../actions/jobs/{job_id}/logs` answers 404, not 500, for a task without
  a log file (the placeholder task of a reusable-workflow outer job), because
  `GetActionJobLogs` also maps `os.ErrNotExist`. Drop the carry when upstream
  maps the same error; as of `v17.0.1` it does not.
- Required status check patterns (`status_check_contexts`) are validated and
  the merge gate fails closed. `CompileStatusCheckPattern` in
  `models/git/status_check_pattern.go` rejects a pattern that gobwas/glob cannot
  compile (an unclosed `[`) and one that compiles into a matcher that panics
  when it matches (an unclosed `{`, an empty `{}` or `{,}`, or an empty
  alternative after an unbounded one, as in `{x*,}`). Creating or editing a branch
  protection through the API answers 422 naming the pattern, and the web form
  runs the same check. `MergeRequiredContextsCommitStatus` returns `failure` for
  a stored pattern that fails the check or panics, whereas upstream logs and
  drops an uncompilable pattern, so that check stops gating, and answers 500 on
  a panicking one. Drop the carry when upstream validates the patterns on the
  API and the merge gate fails closed; as of upstream `v17.0.1` and `forgejo`
  main, it does not.

## Release flow

`.github/workflows/build-j4k-image.yml` tests the carried APIs, builds both rootless architectures, checks the stamped version,
and publishes the manifest. Push the release tag separately from the branch,
then pin both cluster image settings to the same OCI index digest. Keep the
source and image on GitHub/GHCR so recovery remains possible while the Forge
is down.

Forgejo's contribution policy prohibits AI-authored upstream contributions.
These agent-authored carries must not be submitted as upstream pull requests.
