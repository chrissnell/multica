# Upstream tracking playbook

This fork (`chrissnell/multica`) carries a large, long-lived set of changes on
top of upstream `multica-ai/multica` — the full Kubernetes agent runtime and a
handful of UI features. To stop that delta from silently rotting into another
640-commit backlog, we track upstream on a schedule and merge it in small,
routine batches.

Read this before running or reviewing an upstream merge.

## The `upstream` remote

By convention, `upstream` points at the canonical repo:

```bash
git remote add upstream https://github.com/multica-ai/multica.git   # first time
git remote set-url upstream https://github.com/multica-ai/multica.git  # idempotent
git fetch upstream main
```

`origin` stays our fork. `scripts/upstream-merge.sh` sets this remote up for you.

## What we track: upstream `main` (not release tags)

**Decision: we track upstream `main`.**

Rationale — freshness vs. stability:

- Our fork already runs behind two independent gates before anything reaches
  production: the merge PR's full CI (`.github/workflows/ci.yml`) and a
  human review + manual merge. Upstream `main` being occasionally rough is
  caught there, so we don't need release tags as an extra stability buffer.
- Upstream tags cut infrequently and can lag `main` by hundreds of commits.
  Tracking tags would recreate exactly the backlog problem this system exists
  to prevent.
- Small, frequent merges off `main` keep each conflict surface tiny and keep
  our fork-owned files (K8S runtime, daemon/controller, fork UI) continuously
  reconciled against upstream's direction instead of in one big-bang catch-up.

To change this (e.g. pin to tags during an upstream-instability window), set
`UPSTREAM_REF` in `.github/workflows/upstream-tracking.yml` and in
`scripts/upstream-merge.sh` invocations.

## The recurring workflow

> **Do the one-time base graft first.** Until the squashed catch-up base is
> repaired (see *One-time repair* below), every run computes ~660 commits behind
> and opens a giant conflict draft. Land the graft on `main`, then the biweekly
> cadence is meaningful and the first run should be `up_to_date` or a small clean
> merge.

`.github/workflows/upstream-tracking.yml` runs biweekly (08:00 UTC on the 1st
and 15th) and on-demand via **Run workflow**. Each run:

1. Fetches `upstream/main` and computes how far behind we are.
2. If nothing new — exits quietly.
3. Otherwise creates `merge/upstream-<YYYYMMDD>` and runs `git merge upstream/main`.
4. **Clean merge** → pushes the branch and opens a normal PR labeled
   `upstream-merge`. This PR's CI runs the full suite (frontend, backend,
   installer, and the `packaging` helm-render job).
5. **Conflicts** → pushes the branch with the conflict markers committed and
   opens a **draft** PR labeled `upstream-merge` + `needs-manual-merge`, listing
   the conflicted files and pointing back here.

Merging is always a human action — CI is the only automated gate.

### CI auto-run on merge PRs (`UPSTREAM_PAT`)

GitHub deliberately does **not** trigger `pull_request` / `push` workflows for
refs created with the default `GITHUB_TOKEN` (recursion prevention). So a merge
PR opened by the workflow under the default token shows **no checks**. To make
`ci.yml` run automatically, add a repo secret **`UPSTREAM_PAT`** — a fine-grained
PAT with `contents: write` + `pull-requests: write`; the workflow uses it for the
branch push and PR creation when present and falls back to `GITHUB_TOKEN`
otherwise. Without the PAT, kick CI manually on the PR: push an empty commit
(`git commit --allow-empty -m "ci: trigger"`) or close and reopen it.

The heavy lifting lives in `scripts/upstream-merge.sh`, which is runnable
locally to drive or debug a merge without a workflow round-trip:

```bash
scripts/upstream-merge.sh --check-only   # just report how far behind we are
scripts/upstream-merge.sh                # create merge/upstream-<date> and merge
```

## Merge rules

### Merge, never rebase

Always `git merge upstream/main`. Never rebase our fork onto upstream — rebasing
199+ fork commits over 600+ upstream commits replays every fork commit against a
moving base and multiplies conflicts. A merge resolves the combined delta once.

### Never squash an upstream merge

This is the load-bearing rule for the whole system. An upstream merge **must**
land on `main` as a real merge commit that keeps `upstream/main` as a second
parent. If you squash it, git loses all record that upstream's history is
incorporated: the merge-base snaps back to the last real merge and the *next*
`git merge upstream/main` re-litigates every upstream commit from scratch —
recreating the backlog this system exists to kill.

Concretely, when merging an `upstream-merge` PR, use **Create a merge commit**
(or fast-forward a real merge commit onto `main`). Do **not** use **Squash and
merge**.

### Staged conflict resolution

Resolve in this order — it isolates the risky Go work from mechanical churn:

1. **`go.mod` / `go.sum`** — union both dependency sets (we add
   `k8s.io/{api,apimachinery,client-go}` and friends), then `cd server && go mod tidy`.
2. **Pure UI / locale JSON** — take upstream's file, then re-apply our deltas per
   the UI-retention rule below. New upstream locales must gain our fork feature
   keys or locale-parity checks fail.
3. **Daemon / agent Go files** — hand-merge carefully; this is where correctness
   risk lives (`server/pkg/agent/claude.go`, `server/internal/daemon/*`,
   `daemon/execenv/runtime_config.go`, `handler/daemon.go`,
   `daemon/repocache/cache.go`). Preserve our controller/single-task hooks and
   OAuth-token passthrough on top of upstream's refactors.

Fork-local DB migrations live in the reserved `9000+` band and never collide
with upstream's low-numbered range — take upstream migrations verbatim on merge.
See `server/migrations/README.md`.

### UI-retention rule

**Keep every fork UI change unless upstream shipped the identical feature.** For
each fork feature, check whether upstream independently implemented it; if yes,
take upstream's and drop ours, otherwise preserve ours. Fork UI features to
check on each merge:

- Active Issues sidebar view
- Workspace-shared quick actions on the issue sidebar
- Scroll-to-bottom pill for long timelines
- Copy-to-clipboard button on blockquotes in the issue timeline
- Per-action duration + time-since-completion in the execution-log transcript
- Claude plan-limits (session + weekly) in the left toolbar

### Verify before marking a PR ready

The merge PR's CI covers this, but when resolving locally run:

```bash
cd server && go test ./...          # backend
pnpm exec turbo build typecheck lint test --filter='!@multica/docs' --filter='!@multica/mobile'
for t in packaging/scripts/tests/test-*.sh; do bash "$t"; done   # helm-render + packaging
```

## One-time repair: grafting a squashed base

If an upstream catch-up merge was ever squash-merged (as the initial catch-up,
PR #90 / commit `b00bb79d`, was), `main` carries the merged *content* but not the
upstream *ancestry* — so the tooling reports the fork as hundreds of commits
behind and the next merge would conflict on all of them. Repair it once with a
zero-content `-s ours` graft that records the real upstream parent:

```bash
git checkout main && git pull --ff-only origin main
# <upstream-parent> is the upstream commit the squashed merge incorporated.
# For PR #90 that is 05d929858237eeed3edac4df7329397181bf9c34, recoverable as
# the second parent of the PR's pre-squash merge commit (9e2871a4).
git merge -s ours <upstream-parent> \
  -m "chore(upstream): record upstream ancestry for the squashed catch-up merge

Zero-content -s ours graft. main already contains the merged tree; this only
restores upstream/main as an ancestor so future git merge upstream/main is a
small routine merge instead of re-litigating the whole catch-up."
# Land it PRESERVING the merge commit (real merge / direct push) — NOT a squash,
# or the graft is lost and you are back where you started.
git push origin main
```

Verify: `scripts/upstream-merge.sh --check-only` should drop from hundreds of
commits behind to only the handful upstream has added since the catch-up.
