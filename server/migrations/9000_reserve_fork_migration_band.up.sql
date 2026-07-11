-- Fork migration band reservation + one-time legacy fixup.
--
-- Convention: fork-local migrations live at 9000+ so they never collide with
-- upstream's low-numbered, ever-climbing sequence (see server/migrations/README.md).
-- This is the first migration in that band; it exists to reserve it and to
-- carry the record fixup below.
--
-- Fixup: the fork's first local migration originally shipped as
-- 111_quick_actions and has already been applied in live clusters. Renumbering
-- it to 9001_quick_actions would otherwise make the runner treat 9001 as
-- never-applied and re-run its CREATE TABLE, which would fail on the existing
-- table. Renaming the recorded version here keeps the applied state intact:
--
--   * Fresh database  — no 111 row exists, the UPDATE matches nothing (no-op),
--     then 9001_quick_actions runs normally and creates the table.
--   * Deployed cluster — the 111 row is renamed to 9001, so the runner sees
--     9001_quick_actions as already applied and skips it; the table is untouched.
--
-- The NOT EXISTS guard keeps this safe if both rows somehow coexist (avoids a
-- primary-key clash on the rename).
UPDATE schema_migrations
SET version = '9001_quick_actions'
WHERE version = '111_quick_actions'
  AND NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '9001_quick_actions');
