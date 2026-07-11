# Migrations

Plain `.sql` files applied in filename order by `server/cmd/migrate` (see
`server/internal/migrations`). Each migration is a pair:

```
NNN_descriptive_name.up.sql
NNN_descriptive_name.down.sql   # always provide both directions
```

Applied versions are tracked in the `schema_migrations` table, keyed by the
filename stem (e.g. `105_issue_metadata`). Ordering is a **lexicographic string
sort of the full filename**, not numeric — which is why every number is
zero-padded to three digits (`005`, `060`, `110`).

## Fork-local migrations live at 9000+

This is a long-lived fork that tracks an upstream whose migration sequence keeps
climbing (currently 160+). To keep our own migrations from ever colliding with a
number upstream will later reach, **all fork-local migrations use a reserved
`9000+` band.** Never add a fork migration in the low-numbered range that
upstream owns.

- New fork migration → pick the next free number at `9000+`
  (`9002_…`, `9003_…`).
- Upstream migrations keep their original low numbers; we take them verbatim on
  merge. A `9000+` file always sorts after every 3-digit upstream file, so it
  runs last and can safely depend on any upstream table.

The band is anchored by `9000_reserve_fork_migration_band`, which also performs
a one-time fixup: the fork's first local migration originally shipped as
`111_quick_actions` (colliding with upstream's own `111_*`) and is
`9001_quick_actions` now. On a cluster that already applied the old `111`, the
shim renames its `schema_migrations` record to `9001_quick_actions` so the
renumbered file is treated as already applied and its `CREATE TABLE` is not
re-run. On a fresh database the rename matches nothing and is a no-op.
