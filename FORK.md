# The j4k Forgejo fork

This fork runs `code.j4k.dev`. Its upstream base is Forgejo `v16.0.6`
(`1aa13944143b3bc4312607239a68da23512d5ce3`). The current release is
`v16.0.6-j4k.1`, published as `ghcr.io/jercik/forgejo:16.0.6-j4k.1-rootless`.

## Branch model

`v16.0/j4k` is rebase-managed: it is the upstream release tag plus one commit
per carry, nothing else. Bumping the base means rebasing those carry commits
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
  equivalent API; as of `v16.0.6`, upstream `v17.0` and the development branch
  still have none.
- Pending reviews remain scoped to the negative synthetic Actions reviewer ID
  (`FindReviewOptions.ReviewerID != 0` instead of `> 0`). The
  `actions-reviewer-isolation` version capability advertises this guarantee.
  Drop the carry when upstream provides and tests the same isolation; as of
  `v16.0.6`, no upstream branch does.
- Actions run rerun, failed-jobs rerun, and job rerun APIs, advertised by the
  `actions-rerun` capability. Upstream `v17.0` and the development branch ship
  `POST .../runs/{run_id}/rerun` and `POST .../jobs/{job_id}/rerun` with the
  same guards, so those two endpoints drop on a `v17.0` base. The
  `rerun-failed-jobs` endpoint is fork-invented and has no upstream equivalent;
  drop the capability only when all three exist upstream. The run endpoint
  originated in upstream PR #13924.
- Goldmark `v1.8.6` prevents a tab followed by a single backtick or tilde inside
  a list or blockquote from blanking the whole rendered document. Upstream
  `v16.0.6` still pins `v1.8.2`; `v17.0` and the development branch pin
  `v1.8.6`, so this carry drops on a `v17.0` base.

The fork delta has no database migrations. Whether a given bump needs the
cluster's coordinated database and data-volume restore for rollback depends on
the upstream delta it carries, not on the fork patches. See the cluster's
`docs/adr/0010-forgejo-fork-image.md` and `roles/forgejo/README.md` for the
backup gate and pin history.

## Release flow

`.github/workflows/build-j4k-image.yml` tests the carried APIs and markdown
regression, builds both rootless architectures, checks the stamped version,
and publishes the manifest. Push the release tag separately from the branch,
then pin both cluster image settings to the same OCI index digest. Keep the
source and image on GitHub/GHCR so recovery remains possible while the Forge
is down.

Forgejo's contribution policy prohibits AI-authored upstream contributions.
These agent-authored carries must not be submitted as upstream pull requests.
