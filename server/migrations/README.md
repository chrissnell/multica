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
  merge. A `9000+` file sorts after every upstream file whose number stays below
  `900` — i.e. all of upstream's current and any foreseeable range (upstream is
  ~160 and climbs one at a time) — so the fork band runs last and can safely
  depend on any upstream table.

  **Bound (be honest about the edge):** ordering is a byte-wise string sort, not
  numeric, so the "runs last" guarantee is not literally unbounded. If upstream
  ever reaches the 3-digit `900`–`999` range, those files sort *after* `9001…`
  (compare `9001_` vs `900_`: at the 4th byte `'1'` < `'_'`). Widening the band
  to `90000+` does **not** fix this — `90000_` still sorts before `900_` for the
  same reason. The only unbounded fix is a non-digit prefix (e.g. `fork_0001_…`),
  since any letter sorts after any digit. We deliberately keep the simpler `9000+`
  scheme because upstream reaching migration `900` is hundreds of migrations away;
  revisit (switch to a letter prefix) if that horizon ever gets close.

The band is anchored by `9000_reserve_fork_migration_band`, which also performs
a one-time fixup: the fork's first local migration originally shipped as
`111_quick_actions` (colliding with upstream's own `111_*`) and is
`9001_quick_actions` now. On a cluster that already applied the old `111`, the
shim renames its `schema_migrations` record to `9001_quick_actions` so the
renumbered file is treated as already applied and its `CREATE TABLE` is not
re-run. On a fresh database the rename matches nothing and is a no-op.
