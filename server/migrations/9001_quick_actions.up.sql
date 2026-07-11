-- Fork-local migration (9000+ reserved band — see server/migrations/README.md).
--
-- Workspace-shared quick actions: reusable comment macros surfaced as buttons
-- on the issue detail sidebar. Clicking one posts its body as a comment on the
-- issue, which can kick off agent work. Shared across the whole workspace
-- (not per-user), so every member sees the same set.
--
-- Originally shipped as 111_quick_actions; renumbered into the fork band to
-- avoid colliding with upstream's low-numbered sequence. IF NOT EXISTS guards
-- keep it a safe no-op on any environment that already applied the old 111
-- (the 9000 shim renames that record so this file is normally skipped outright).
CREATE TABLE IF NOT EXISTS quick_action (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    label TEXT NOT NULL,
    body TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS quick_action_workspace_idx ON quick_action (workspace_id);
