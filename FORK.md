# The j4k Forgejo fork

This fork runs `code.j4k.dev`. Its upstream base is Forgejo `v16.0.5`
(`8d24b7d52df327118cd776cf461d34e52e0ec1ce`). The next release is
`v16.0.5-j4k.1`, published as `ghcr.io/jercik/forgejo:16.0.5-j4k.1-rootless`.

The fork carries these changes:

- Token-authenticated PR review conversation resolution through `POST` and
  `DELETE /repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments/{comment}/resolution`.
  Drop this carry when the pinned upstream release ships an equivalent API.
- Pending reviews remain scoped to the negative synthetic Actions reviewer ID.
  The `actions-reviewer-isolation` version capability advertises this guarantee.
  Drop the carry when upstream provides and tests the same isolation.
- Actions run rerun, failed-jobs rerun, and job rerun APIs, advertised by the
  `actions-rerun` capability. Drop each endpoint when upstream ships its
  equivalent, and drop the capability when all three exist upstream. The run
  endpoint originated in upstream PR #13924.
- Goldmark `v1.8.6` prevents a tab followed by a single backtick or tilde inside
  a list or blockquote from blanking the whole rendered document. Upstream
  `v16.0.5` still uses `v1.8.2`. Drop this dependency carry when the pinned
  upstream release includes the fix and passes the regression test.
- Actions maintenance uses an optional `[actions] ADMISSION_LOCK_PATH` and
  advertises `actions-admission-drain-state-v1` when configured. Shared file locks cover
  task assignment; exact paused state in the bound file keeps assignments denied
  after the exclusive owner dies while recovery and reporting remain available.
  Admin runner GET responses expose fresh fetch
  observations and global task receipt counts. See
  [the maintenance contract](docs/actions-admission-drain.md).

The fork adds `v16f_j4k-action-task-final-receipt.go` to retain runner final-report
acceptance. Existing tasks remain uncovered SQL NULL; new assignments become
pending until the runner's terminal report is accepted. The upstream `v16.0.2`
to `v16.0.5` upgrade also adds `v17a_add-action-run-workflow-source-commit.go`.
Deployment rollback
therefore follows the cluster's coordinated database and data-volume restore
procedure. See the cluster's `docs/adr/0010-forgejo-fork-image.md` and
`roles/forgejo/README.md` for the backup gate and pin history.

`.github/workflows/build-j4k-image.yml` tests the carried APIs and markdown
regression, builds both rootless architectures, checks the stamped version,
and publishes the manifest. Release tags are immutable. Push the release tag
separately from the branch, then pin both cluster image settings to the same
OCI index digest. Keep the source and image on GitHub/GHCR so recovery remains
possible while the Forge is down.

Forgejo's contribution policy prohibits AI-authored upstream contributions.
These agent-authored carries must not be submitted as upstream pull requests.
