-- Fork-local migration (9000+ reserved band — see server/migrations/README.md).
--
-- Per-project default agent assignee. When an issue is created in a project
-- that has this set and the caller supplies no assignee, the issue-create
-- pipeline back-fills this agent (assignee_type='agent'), which lets the
-- existing on-assign enqueue fire a run automatically. This is what makes a
-- project whose work requires a specific runtime (e.g. a macOS agent) route
-- new issues to that agent without picking an assignee by hand each time.
--
-- ON DELETE SET NULL: deleting the agent simply clears the project default
-- rather than blocking the delete or leaving a dangling reference.
ALTER TABLE project
    ADD COLUMN default_agent_id UUID REFERENCES agent(id) ON DELETE SET NULL;
