# CLAUDE.md

Guidance for Claude Code when working in this repository. Keep this file short and authoritative: rules here should be hard to infer from code or easy to get wrong.

## Conventions

The source of truth for code naming, i18n glossary, and Chinese product voice is:

- `apps/docs/content/docs/developers/conventions.mdx`
- `apps/docs/content/docs/developers/conventions.zh.mdx`

Read it before editing translations in `packages/views/locales/`, naming routes/packages/files/DB columns/types, or writing Chinese UI/docs copy. Do not rely on `packages/views/locales/glossary.md`; it is only a redirect stub.

## Project Shape

Multica is an AI-native task management platform for small teams, with agents as first-class assignees that can own issues, comment, and change status.

- `server/`: Go backend, Chi router, sqlc, gorilla/websocket.
- `apps/web/`: Next.js App Router.
- `apps/desktop/`: Electron desktop app.
- `apps/mobile/`: Expo / React Native iOS app. Read `apps/mobile/CLAUDE.md` before touching it.
- `packages/core/`: headless business logic, API client, React Query hooks, Zustand stores.
- `packages/ui/`: atomic UI components only.
- `packages/views/`: shared business pages/components for web and desktop.
- `packages/tsconfig/`: shared TypeScript config.

Shared packages export raw `.ts` / `.tsx` and are compiled by consuming apps. Dependency direction is `views -> core + ui`; `core` and `ui` must stay independent.

## State Rules

Keep server state and client state separate.

- TanStack Query owns server state: issues, users, workspaces, inbox, agents, members, and anything fetched from the API.
- Zustand owns client/view state: filters, drafts, modals, tab layout, and navigation history. Current workspace identity is route-driven; platform stores/singletons may mirror slug/id only for headers, persistence namespaces, and reconnects.
- Shared Zustand stores live in `packages/core/`, never in `packages/views/` or app directories.
- React Context is for platform plumbing only, such as `WorkspaceIdProvider` and `NavigationProvider`.
- Only auth/workspace stores may call `api.*` directly. Other server interaction belongs in queries/mutations.
- Workspace-scoped query keys must include `wsId`.
- Optimistic updates only when ALL hold: outcome locally predictable, user stays on the same screen (no navigation), failure is rare, rollback is trivial. Canonical: status/assignee/toggle field patches — patch determinate caches, roll back on failure, invalidate uncertain projections on settle.
- Flows that navigate or confirm (create, delete, leave) must await the server before navigating or cleaning up; never optimistically remove an entity from cache.
- Chat/message send uses the pending-message pattern: render immediately with a visible pending state and retry on failure, not silent optimism.
- WebSocket events invalidate or patch Query cache for server data. They must never mirror server payload data into Zustand; clearing client-owned pointers (active session, selection, current workspace) is allowed only with a single responder and a self-initiated guard when this client can cause the event.
- Persist durable preferences/drafts/layout. Do not persist server data or ephemeral UI state.
- Zustand selectors must return stable references. Do not return freshly allocated objects/arrays from selectors without shallow comparison.
- Hooks that need workspace context should accept `wsId`; do not call `useWorkspaceId()` internally unless the hook is guaranteed to run under the provider.

## Package Boundaries

These are hard constraints:

- `packages/core/`: no `react-dom`, `localStorage` (use `StorageAdapter`), `process.env`, or UI libraries.
- `packages/ui/`: no `@multica/core` imports and no business logic.
- `packages/views/`: no `next/*`, no `react-router-dom`, no stores. Use `NavigationAdapter`, `useNavigation()`, and `<AppLink>`.
- `apps/web/platform/`: only place for Next.js navigation/platform APIs.
- `apps/desktop/src/renderer/src/platform/`: only place for `react-router-dom` navigation wiring.
- Every workspace under `apps/` and `packages/` must declare directly imported external packages in its own `package.json`.
- Shared dependencies use `catalog:` from `pnpm-workspace.yaml`; `apps/mobile/` pins Expo/React Native related versions directly.

## Sharing Rules

Web and desktop share business logic, hooks, stores, components, and views through `packages/core/`, `packages/ui/`, and `packages/views/`.

If the same logic exists in both web and desktop, extract it unless it depends on platform APIs:

1. Next.js, Electron, or router APIs stay in the app/platform layer.
2. Headless logic belongs in `packages/core/`.
3. Shared UI or business views belong in `packages/views/`.
4. Shared primitives belong in `packages/ui/`.

Mobile is independent. It may import types and pure functions from `@multica/core`, with `import type` for types, but owns its UI, state, hooks, providers, i18n, React version, build pipeline, and release cadence.

## Commands

Use the repo scripts as the source of truth. Common commands:

```bash
make dev              # auto-setup and start the app
make start            # start backend + frontend
make stop             # stop app processes for this checkout
make server           # run Go server only
make daemon           # run local daemon
make test             # Go tests
make sqlc             # regenerate sqlc code after SQL changes
pnpm install
pnpm dev:web
pnpm dev:desktop
pnpm build
pnpm typecheck
pnpm lint
pnpm test             # TS/Vitest tests through Turborepo
pnpm exec playwright test
pnpm ui:add badge     # shadcn/Base UI component into packages/ui
```

Worktrees share one PostgreSQL container and get isolated DB names/ports via `.env.worktree`. `make dev` auto-detects this. For manual setup use `make worktree-env`, `make setup-worktree`, and `make start-worktree`. `pnpm dev:desktop` additionally self-isolates per worktree (its own renderer port + app name) automatically, independent of `.env.worktree`.

CI runs Node 22, Go 1.26.1, and a `pgvector/pgvector:pg17` PostgreSQL service.

## Database and Migration Rules

These are hard requirements for every new or modified database design and production migration:

- Do not add database foreign keys (`FOREIGN KEY` / `REFERENCES`), cascading deletes, or cascading updates. Resolve relationships, validation, and dependent cleanup explicitly in application code. Use an application transaction when cleanup and the parent operation must commit or roll back atomically.
- Every index created by a migration must use `CREATE INDEX CONCURRENTLY` or `CREATE UNIQUE INDEX CONCURRENTLY`, including indexes on newly created tables. PostgreSQL rejects concurrent index creation inside a transaction or a multi-command string, so keep each concurrent index build in its own single-statement migration file. The repository migration runner executes migration files outside an explicit transaction to support this.

## Coding Rules

- TypeScript strict mode is enabled; keep types explicit.
- Go follows standard conventions: `gofmt`, `go vet`, checked errors.
- Code comments must be English.
- Prefer existing patterns/components over new parallel abstractions.
- Avoid broad refactors unless required by the task.
- For internal, non-boundary code, do not add compatibility layers, fallback paths, dual writes, legacy adapters, or temporary shims unless explicitly requested.
- API boundaries are different: installed desktop clients can talk to newer backends, so response parsing must follow the API compatibility rules below.
- If a flow or API is being replaced and the product is not live, prefer removing the old path instead of preserving both.
- New global pre-workspace routes must be a single word (`/login`, `/inbox`) or `/{noun}/{verb}` (`/workspaces/new`). Do not add hyphenated root routes like `/new-workspace`.
- Reserved slugs live in `server/internal/handler/reserved_slugs.json`. Edit it, run `pnpm generate:reserved-slugs`, and commit the generated `packages/core/paths/reserved-slugs.ts`.
- When changing CLI commands/flags, API fields, or product behavior documented by built-in skills under `server/internal/service/builtin_skills/*`, update the relevant `SKILL.md` and `references/*-source-map.md` in the same PR.
- **Fork-local DB migrations live in a reserved `9000+` band**, never in the low-numbered range upstream owns and keeps climbing through. Add new fork migrations as `9002_…`, `9003_…`. See `server/migrations/README.md` for the rationale and the one-time `111 → 9001` fixup.
- **Merging upstream (`multica-ai/multica`) is a scheduled, merge-not-rebase, never-squash routine.** Before running or reviewing an upstream merge, read `docs/upstream-tracking.md` (remote convention, tracking decision, staged conflict resolution, UI-retention rule). The `upstream-tracking` workflow opens the merge PRs; `scripts/upstream-merge.sh` drives it locally.

## API Compatibility

Frontend code must survive backend response drift, especially in installed desktop builds.

- Parse API JSON with `parseWithFallback` in `packages/core/api/schema.ts` and a zod schema. Do not cast network JSON to `T`.
- Endpoint responses consumed by UI logic must pass through a schema before returning.
- Downstream UI should optional-chain and default fields defensively.
- Prefer explicit boolean checks (`=== true`) over truthy/falsy checks on server fields.
- Do not pin critical affordances to one backend boolean; combine signals when possible.
- Server-driven enum switches need a `default` branch.
- When adding or changing an endpoint, add/update the schema and include a malformed-response test.

## Backend UUID Rules

In `server/internal/handler/`, always know where a UUID came from before using it in write queries.

- Resource path params that may be UUIDs or human-readable IDs must be resolved through loaders such as `loadIssueForUser`, `loadSkillForUser`, `loadAgentForUser`, or `requireDaemonRuntimeAccess`; subsequent writes use the resolved `entity.ID`.
- Pure UUID inputs from request boundaries use `parseUUIDOrBadRequest(w, s, fieldName)` and return immediately on `ok=false`.
- Trusted UUID round-trips from sqlc results or test fixtures use `parseUUID(s)`, which panics on invalid input.
- Outside handlers, `util.ParseUUID(s) (pgtype.UUID, error)` is the safe variant; always check the error.

## Web/Desktop Features

When adding a shared page or feature for web and desktop:

1. Put the page/component in `packages/views/<domain>/`.
2. Add platform wiring in both `apps/web/app/` and the desktop router, unless the desktop flow is a transition overlay.
3. Use `useNavigation().push()` or `<AppLink>` in shared code.
4. Use shared guards/providers such as `DashboardGuard` from `packages/views/layout/`.
5. Keep platform-only UI in the app or inject it through props/slots.
6. Hooks that need workspace context should accept `wsId`.

CSS for web/desktop is shared from `packages/ui/styles/`. Use semantic tokens such as `bg-background` and `text-muted-foreground`; avoid hardcoded Tailwind colors and duplicated base styles.

## Desktop Rules

Desktop routing has three categories:

- Session routes: workspace-scoped tab destinations such as `/:slug/issues`.
- Transition flows: pre-workspace one-shot actions such as create workspace or accept invite. These are `WindowOverlay` state, not routes.
- Error/stale states: stale workspace tabs should auto-heal by dropping stale tab groups, not render desktop error pages.

More desktop constraints:

- New pre-workspace desktop flows register a `WindowOverlay` type in `stores/window-overlay-store.ts`; do not add them to `routes.tsx`.
- `setCurrentWorkspace(slug, uuid)` from `@multica/core/platform` mirrors the active route for headers, storage namespaces, and reconnects; workspace route layouts own setting it.
- Code that leaves workspace context must call `setCurrentWorkspace(null, null)` explicitly.
- Workspace delete must await the server before navigation/cleanup. Workspace leave currently clears/navigates before mutation only to avoid the `member:removed` realtime race; treat that as known debt, not a reusable pattern.
- Cross-workspace navigation must go through the navigation adapter so it can call `switchWorkspace(slug, targetPath)`.
- Full-window desktop views outside the dashboard shell must mount `<DragStrip />` from `@multica/views/platform` as the first flex child. Interactive controls in the top 48px need `WebkitAppRegion: "no-drag"`.

## Mobile Rules

Read `apps/mobile/CLAUDE.md` before touching `apps/mobile/`. It contains the mandatory pre-flight process, import limits, parity rules, tech stack, UI rules, data helpers, realtime strategy, and mobile release flow.

Root-level reminders:

- Mobile shares only `@multica/core` types and pure functions.
- Mobile must match web/desktop product semantics: counts, permissions, enums/transitions, and data identity.
- Mobile may differ in UI/interaction when the phone context requires it.

## UI Rules

- Prefer shadcn/Base UI components over custom implementations. Add them with `pnpm ui:add <component>` from the repo root.
- Use design tokens and semantic classes; avoid hardcoded colors.
- Do not introduce extra local state unless the design requires it.
- Handle overflow, long text, scrolling, alignment, and spacing deliberately.
- If a component is identical between web and desktop, it belongs in a shared package.

## Testing

Tests follow the code:

| What is tested | Location |
| --- | --- |
| Shared business logic, stores, queries, hooks | `packages/core/*.test.ts` |
| Shared UI components, pages, forms, modals | `packages/views/*.test.tsx` |
| Platform wiring such as cookies, redirects, search params | `apps/web/*.test.tsx` or `apps/desktop/` |
| End-to-end flows | `e2e/*.spec.ts` |
| Backend | `server/` Go tests |

Rules:

- Never test shared component behavior in an app test file.
- `packages/views/` tests must not mock `next/*` or `react-router-dom`.
- Mock `@multica/core` stores with the Zustand callable-store shape (`selectorFn` plus `getState`).
- Mock `@multica/core/api` for API calls.
- E2E tests should use `TestApiClient` for setup/teardown.
- Prefer writing the failing test in the correct package before implementation when the change is behavioral.

## Verification

For code changes, run the narrowest useful checks while iterating, then run broader verification when risk justifies it or when asked.

Useful checks:

```bash
pnpm typecheck
pnpm test
make test
pnpm exec playwright test
make check
```

Do not claim verification passed unless you ran it. If you skip checks because the change is docs-only or the user asked not to run them, say so.

## Commits and Releases

- Commits should be atomic and use conventional prefixes: `feat(scope)`, `fix(scope)`, `refactor(scope)`, `docs`, `test(scope)`, `chore(scope)`.
- A production deployment requires a CLI release tag on `main`: create `v0.x.x`, push it, and let `release.yml` publish binaries and the Homebrew tap.
- Bump patch by default unless the user specifies a version.

## Domain Reminders

1. Create a tag on the `main` branch: `git tag v0.x.x`
2. Push the tag: `git push origin v0.x.x`
3. GitHub Actions automatically triggers `release.yml`: runs Go tests → GoReleaser builds multi-platform binaries → publishes to GitHub Releases + Homebrew tap

By default, bump the patch version each release (e.g. `v0.1.12` → `v0.1.13`), unless the user specifies a specific version.

## Self-hosted image tags (Harbor)

The CLI release flow above is for the GitHub `v0.x.x` semver line. It is **not** the production deploy tag for the self-hosted Kubernetes stack. Those images (`registry.chrissnell.com/multica/multica-{backend,web,postgres,controller,claude-broker,repocache,runtime-base,runtime-claude}`) all share a single tag of the form `vX.Y.Z-mkN` and are built/pushed manually from a developer workstation.

The contention problem we kept hitting: with no in-repo source of truth for `mkN`, two parallel feature branches would both pick the next number, both push to Harbor under the same tag, and stomp on each other. Whoever deployed second silently overwrote the first.

**Source of truth:** [`packaging/image-tag`](packaging/image-tag). One line, the current Harbor tag. Every image moves together — there is no per-subsystem tag.

### Workflow — pushing a new release

Every step matters. Skipping the PR step or the rollout check has burned us before.

**1. Branch + bump.** Bump commits never go directly on `main` — they go through a PR to the fork (`chrissnell/multica`).

```bash
git checkout main && git pull --ff-only origin main
make print-next-image-tag                   # peek at N+1 without writing
git checkout -b chore/bump-images-mk<N+1>
make bump-images                            # writes vX.Y.Z-mk(N+1) to packaging/image-tag
git add packaging/image-tag
git commit -m "chore(images): bump to $(cat packaging/image-tag)"
git push -u origin HEAD
gh pr create --repo chrissnell/multica --base main --head $(git branch --show-current) \
  --title "chore(images): bump to $(cat packaging/image-tag)" \
  --body "Bumps packaging/image-tag for the next Harbor rollout."
```

Wait for the PR to merge before continuing. The build itself can run from any branch — only the bump's claim on the `mk` number requires the merge.

**2. Build + push.** Skip `postgres` (intentionally pinned to `v0.4.0-mk3` in `deploy/farm-talos/values.yaml` to avoid bouncing the stateful pod for a no-op rebuild). Skip `runtime` unless toolchain pins (`packaging/rust-version`, `packaging/claude-code-version`, etc.) actually changed — runtime has its own lifecycle and a rebuild may pick up unrelated WIP pins.

```bash
./packaging/scripts/build-images.sh backend web controller claude-broker repocache \
  > scratch/build-$(cat packaging/image-tag).log 2>&1 &
# When the runtime toolchain pins moved, additionally:
./packaging/scripts/build-images.sh runtime
```

Cross-build from Apple Silicon to `linux/amd64` runs ~20–30 min for the five platform images. Verify every line in the log: each image should end with a successful `docker push`. If one fails, rebuild only that image (`./packaging/scripts/build-images.sh <name>`), don't restart the batch.

**3. Update `deploy/farm-talos/values.yaml`.** Two edits:

- Top-level `image.tag:` → the new tag.
- Any per-subsystem `image.tag:` override (`daemon.image.tag`, `controller.image.tag`, `claudeBroker.image.tag`, `repocache.image.tag`) that now equals the new top-level tag is redundant — delete the override line and the comment that justified the drift. Leave `image.tags.postgres: v0.4.0-mk3` alone, it's load-bearing.

**4. helm upgrade + watch rollout.** Confirm the kubectl context first; this cluster only.

```bash
kubectl config current-context              # MUST be admin@farm-talos; abort otherwise
helm upgrade --install multica packaging/helm/multica/ -n multica -f deploy/farm-talos/values.yaml
for d in multica-backend multica-web multica-controller multica-claude-broker multica-repocache; do
  kubectl -n multica rollout status deploy/$d --timeout=5m
done
```

A rollout timeout usually means `ImagePullBackOff` — `kubectl -n multica describe pod <name>` to confirm. Check the tag exists in Harbor and the `registry-credentials` pull secret is healthy.

**5. Tag the release on GitHub.** Once rollout is green:

```bash
git checkout main && git pull --ff-only origin main
git tag $(cat packaging/image-tag)
git push origin $(cat packaging/image-tag)
```

`make print-image-tag` shows the current pin. `make print-next-image-tag` shows what `bump-images` would write without writing it.

### Deconflicting parallel branches

The bump commit IS the lock. If two branches both run `make bump-images` against the same base (say both produce `v0.4.0-mk6`):

- **Whoever merges first** wins. Their bump commit lands on `main`, their build at `v0.4.0-mk6` is canonical.
- **The loser rebases**, runs `make bump-images` again (now produces `v0.4.0-mk7`), re-runs `build-images.sh`, and updates their PR. The mk6 images they previously pushed are dead — replace them or leave them in Harbor's history; do not deploy them.
- **Never edit `packaging/image-tag` by hand to "fix" a conflict.** Re-run the bump script — it's the only thing that guarantees the suffix matches the actual increment.

### Sandbox / WIP builds (no contention)

When testing on a branch without spending an mk number, pass an explicit tag — the pin file is left untouched and Harbor's mk-series is unaffected:

```bash
./packaging/scripts/build-images.sh --tag wip-$(whoami)-$(git rev-parse --short HEAD)
# helm upgrade --set image.tag=wip-…  (in a non-prod cluster)
```

The `wip-*` namespace is purely a convention — anything that isn't `vX.Y.Z-mkN` is sandbox by definition.

### When to bump the base version (e.g. `v0.4.0` → `v0.5.0`)

The base is bumped when the CLI semver line bumps (see *CLI Release* above). Use `./packaging/scripts/bump-image-tag.sh --base v0.5.0` to reset the suffix to `-mk1` against the new base. Most days you do not touch the base — every bump is an mk.

## Multi-tenancy

All queries filter by `workspace_id`. Membership checks gate access. `X-Workspace-ID` header routes requests to the correct workspace.

## Agent Assignees

Assignees are polymorphic — can be a member or an agent. `assignee_type` + `assignee_id` on issues. Agents render with distinct styling (purple background, robot icon).

## Agent PR Policy

PRs from automated sessions may carry the `auto-merge` label as a hint that
the work is ready to ship, but merging is always a human action — open the
PR, confirm the diff, click Merge. CI is the only automated gate.

CODEOWNERS reserves `packaging/image-tag` and `deploy/farm-talos/values.yaml`
for the `multica-release-bot` GitHub App. Agent PRs that touch those files
will fail the protection check until the release bot's automation
(`make release` → `image-release.yml`) modifies them. This is intentional —
it prevents two parallel agent PRs from racing on the same `mkN` suffix.
