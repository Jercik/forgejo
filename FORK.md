# The j4k Forgejo fork

This fork runs `code.j4k.dev`. Its upstream base is Forgejo `v17.0.0`
(`0d791421f85ca13a63b665053d81ea765fbbc571`). The next release is
`v17.0.0-j4k.1`, published as `ghcr.io/jercik/forgejo:17.0.0-j4k.1-rootless`.

## Branch model

`v17.0/j4k` is rebase-managed: it is the upstream release tag, then temporary
backports, then one commit per carry. Bumping the base means rebasing those carry commits
onto the new upstream tag and force-pushing the branch with `--force-with-lease`;
never merge or squash-merge upstream history into it, because a squash turns the
whole upstream delta into one fork commit and hides the real merge-base.
Release tags `v<upstream>-j4k.N` are immutable and keep every shipped state
reachable after a rebase.

## Carries

- Token-authenticated PR review conversation resolution through `POST` and
  `DELETE /repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments/{comment}/resolution`.
  Actions job tokens are allowed and checked against the task's own repository
  permission. Drop this carry when the pinned upstream release ships an
  equivalent API; as of `v17.0.0`, upstream still has none.
- Pending reviews remain scoped to the negative synthetic Actions reviewer ID
  (`FindReviewOptions.ReviewerID != 0` instead of `> 0`). The
  `actions-reviewer-isolation` version capability advertises this guarantee.
  The change also affects `dismissPriors` in `services/pull/review.go`, which
  passes the review's `ReviewerID` after loading the reviewer. For a deleted
  reviewer (ghost, `-1`) or an Actions-authored review (`-2`), upstream drops
  the filter and dismisses every undismissed review on the PR, whereas the fork
  dismisses only that reviewer's reviews.
  Drop the carry when upstream provides and tests the same isolation; as of
  `v17.0.0`, upstream still filters on `ReviewerID > 0`.
- The `POST .../actions/runs/{run_id}/rerun-failed-jobs` endpoint and the
  `actions-rerun` capability. The run rerun (`.../runs/{run_id}/rerun`) and job
  rerun (`.../jobs/{job_id}/rerun`) endpoints come from upstream since
  `v17.0.0`; the capability's contract is that all three endpoints exist. The
  `rerun-failed-jobs` endpoint is fork-invented and has no upstream equivalent;
  drop the capability only when all three exist upstream.

## Temporary upstream backports

Two fixes merged on upstream `v17.0/forgejo` after the `v17.0.0` tag are carried verbatim as the first commits on this branch, because `v17.0.0` as tagged regresses this deployment and `v16.0` reaches end of life on 2026-10-29:

- `git(config): fix configUnsetAll receive.hideRefs` (upstream #14870, commit 77986c1125): `v17.0.0` writes an empty `receive.hideRefs` on every startup, after which pull request merges fail with "cannot lock ref".
- `fix: don't attempt to transfer Actions logs to storage when already transferred` (upstream #14855, commit 37e48731a3): a retried final log upload fails, so a job reports as failed minutes later and jobs that need it stay blocked.

Drop both when rebasing onto `v17.0.1` or any later tag that contains them (check with `git merge-base --is-ancestor <sha> <tag>`). They are the only exception to the branch model above.

The upstream `v16.0.6` to `v17.0.0` delta adds six `models/forgejo_migrations`
migrations: `v17a_add_action_task_step_summary`, `v17a_add_invite_expiry_date`,
`v17a_add_job_namespace`, `v17a_add_package_cleanup_based_on_last_download`,
`v17a_add_package_version_last_downloaded` and
`v17a_add_package_version_total_size`. The fork delta itself adds none. Whether
a given bump needs the cluster's coordinated database and data-volume restore
for rollback depends on the upstream delta it carries, not on the fork patches.
See the cluster's `docs/adr/0010-forgejo-fork-image.md` and
`roles/forgejo/README.md` for the backup gate and pin history.

## Release flow

`.github/workflows/build-j4k-image.yml` tests the carried APIs, builds both rootless architectures, checks the stamped version,
and publishes the manifest. Push the release tag separately from the branch,
then pin both cluster image settings to the same OCI index digest. Keep the
source and image on GitHub/GHCR so recovery remains possible while the Forge
is down.

Forgejo's contribution policy prohibits AI-authored upstream contributions.
These agent-authored carries must not be submitted as upstream pull requests.
