-- No-op: the band reservation carries no schema of its own. The quick_action
-- table is owned by 9001_quick_actions and dropped by its own down migration.
-- Rolling back does not resurrect the legacy 111_quick_actions record — a full
-- rollback removes the fork feature entirely, which is the intended end state.
SELECT 1;
