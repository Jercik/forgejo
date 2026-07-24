# The j4k Forgejo Fork

This is a personal fork of [Forgejo](https://codeberg.org/forgejo/forgejo). It exists to add features the j4k infrastructure needs that upstream doesn't ship — currently a REST API for resolving pull-request review conversations, which upstream exposes only through the web UI's session-authenticated route. Images built from this fork run the production Forge at `code.j4k.dev`.

Unlike the sync-rules-managed repos, this `AGENTS.md` is hand-maintained — edit it directly.

## Intent: track upstream, carry a minimal delta

Stay current with upstream Forgejo — its releases, fixes, and improvements — and layer personally needed features on top. The delta is a deliberate carry, not a divergence:

- Rebase the delta onto each upstream release of the pinned line: branch `vX.Y/j4k` sits on upstream tag `vX.Y.Z`, and release tags are `vX.Y.Z-j4k.N` — `N` increments when the delta changes on an unchanged upstream base, and resets to 1 on a new base.
- Keep every carried commit small, integration-tested, and independently droppable.
- Drop a patch the moment upstream ships an equivalent feature — the pin history in the cluster repo records the exit condition for each one.

**No upstream PRs.** Forgejo's `CONTRIBUTING.md` bans AI-authored contributions, and this delta is agent-authored. Never open a pull request against upstream from this code; upstreaming a feature requires the user to hand-author it from scratch.

## Why GitHub, not Codeberg or code.j4k.dev

The fork's host must provide two things: CI that builds multi-arch (amd64 + arm64) container images, and hosting that stays reachable while `code.j4k.dev` is down.

- **Not code.j4k.dev — self-hosting deadlock.** This fork's image *is* the Forgejo instance at `code.j4k.dev`. The cluster's deploy stops Forgejo before pulling the new image, so a pull from its own registry would fail mid-upgrade — and if the instance ever broke, the source and image needed to repair it would be unreachable. The fork must live outside the system it patches.
- **Not Codeberg.** Upstream's home, but its request-access Woodpecker CI has no native arm64 runners for container builds, and its fair-use package registry isn't positioned to serve production image pulls.
- **GitHub.** Free Actions runners for both architectures (`ubuntu-latest`, `ubuntu-24.04-arm`) plus public image hosting on `ghcr.io` — the whole build-and-host pipeline with zero standing infrastructure to maintain.

The full decision record — including the rejected alternative of driving upstream's web resolve route with a bot session — is ADR-0010 (`docs/adr/0010-forgejo-fork-image.md`) in the cluster repo.

## Repository shape

- `origin` — `git@github.com:Jercik/forgejo.git` (the fork; pushes and release tags go here).
- `upstream` — `https://codeberg.org/forgejo/forgejo.git` (fetch releases and tags from here).
- Branch `v16.0/j4k` — upstream tag `v16.0.2` plus the carried delta:
  - `feat(api)` — `POST`/`DELETE /repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments/{comment}/resolution`, with integration tests in `tests/integration/api_pull_review_resolve_test.go`.
  - `chore(swagger)` — regenerated `templates/swagger/v1_json.tmpl` for the new endpoints.
  - `ci` — `.github/workflows/build-j4k-image.yml`.
  - `docs` — this file and `CLAUDE.md`, carried like any other delta commit.

## Release pipeline

Pushing a `v*-j4k.*` tag runs `.github/workflows/build-j4k-image.yml`: the carried integration tests, a per-arch rootless image build pushed by digest to `ghcr.io/jercik/forgejo`, a stamped-version gate, and a manifest merge that publishes `ghcr.io/jercik/forgejo:<version>-rootless`. The pinnable digest lands in the merge job's step summary.

The consumer is the cluster repo (`~/Developer/j4k/cluster`): `roles/forgejo/defaults/main.yml` pins the image by digest, and the role README records pin history. Image changes deploy backup-gated, because Forgejo's database migrations are forward-only.

Cutting a release:

1. `git fetch upstream --tags`. For a patch bump within the line, rebase the carried delta onto the new tag and reuse the branch — `git rebase --onto v16.0.2 v16.0.1 v16.0/j4k`, then `git push --force-with-lease origin v16.0/j4k` (a plain push is rejected as non-fast-forward after a rebase). A new minor line gets a fresh `vX.Y/j4k` branch instead. If the rebase conflicts on `templates/swagger/v1_json.tmpl`, don't hand-merge the generated JSON — resolve by re-running `make generate-swagger` on the new base (the BSD-sed gotcha below applies).
2. Run the integration tests: `TAGS="sqlite sqlite_unlock_notify" make 'test-sqlite#TestAPIPullReviewResolve'`.
3. Create the signed tag `vX.Y.Z-j4k.N`; push the branch first, then the tag in a **separate** push. When branch and tag were pushed together, GitHub delivered only the branch push event and the tag-triggered workflow never fired (observed on this repo; GitHub documents push-event suppression only for >3 tags, so don't count on a combined push).
4. Take the digest from the workflow's step summary, then update the cluster pin following the role README's Upgrading procedure — re-diff the carried delta's `models/migrations` + `models/forgejo_migrations` for changes (that diff decides whether pin-revert remains a valid rollback), deploy backup-gated — and append the pin-history entry, including the exit condition for any newly carried patch.

## Adding a feature

Every new carried feature is its own `feat` commit with an integration test; if it touches the API, regenerate swagger in a separate `chore(swagger)` commit. Extend the CI test job in `.github/workflows/build-j4k-image.yml` to run the new test — the job only runs what it names, currently `test-sqlite#TestAPIPullReviewResolve`. Then cut a release with `N` incremented.

## Gotchas

- **BSD sed eats the swagger newline.** On macOS, `make generate-swagger` silently drops the trailing newline from `templates/swagger/v1_json.tmpl` (the Makefile's `$a\` sed idiom no-ops under BSD sed). Restore it before committing, or the next `make swagger-check` or regeneration on Linux reports a one-byte diff on a file that looks identical.
- **The stamped version comes from `git describe`, not the build arg.** The image derives `FORGEJO_VERSION` from the checkout's tags — hence `fetch-depth: 0` in CI; the `RELEASE_VERSION` build arg alone cannot produce `X.Y.Z-j4k.N+gitea-…`. The workflow's version gate rejects a wrongly stamped image before the manifest is tagged.
