package service

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func newDefaultAgentTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// defaultAgentFixture seeds a workspace with a runtime-backed agent and a
// project whose default_agent_id points at that agent. Returns the workspace
// id, the creator (member) id, the agent id, and the project id.
func defaultAgentFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (workspaceID, userID, agentID, projectID string) {
	t.Helper()
	suffix := time.Now().UnixNano()

	if err := pool.QueryRow(ctx, `
		INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id
	`, "Default Agent Test", fmt.Sprintf("default-agent-svc-%d@multica.ai", suffix)).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, $4) RETURNING id
	`, "Default Agent Test", fmt.Sprintf("default-agent-svc-%d", suffix), "default agent service test", "DAS").Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')
	`, workspaceID, userID); err != nil {
		t.Fatalf("create member: %v", err)
	}
	var runtimeID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (
			workspace_id, daemon_id, name, runtime_mode, provider,
			status, device_info, metadata, last_seen_at, visibility, owner_id
		)
		VALUES ($1, NULL, $2, 'cloud', 'default_agent_test', 'online', 'test runtime', '{}'::jsonb, now(), 'private', $3)
		RETURNING id
	`, workspaceID, "Default Agent Runtime", userID).Scan(&runtimeID); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (
			workspace_id, name, description, runtime_mode, runtime_config,
			runtime_id, visibility, max_concurrent_tasks, owner_id
		)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'private', 1, $4)
		RETURNING id
	`, workspaceID, "Default Agent", runtimeID, userID).Scan(&agentID); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title, status, priority, default_agent_id)
		VALUES ($1, $2, 'planned', 'none', $3)
		RETURNING id
	`, workspaceID, "Default Agent Project", agentID).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}

	t.Cleanup(func() {
		c := context.Background()
		pool.Exec(c, `DELETE FROM agent_task_queue WHERE agent_id = $1`, agentID)
		pool.Exec(c, `DELETE FROM issue WHERE workspace_id = $1`, workspaceID)
		pool.Exec(c, `DELETE FROM project WHERE id = $1`, projectID)
		pool.Exec(c, `DELETE FROM agent WHERE id = $1`, agentID)
		pool.Exec(c, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
		pool.Exec(c, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, workspaceID, userID)
		pool.Exec(c, `DELETE FROM workspace WHERE id = $1`, workspaceID)
		pool.Exec(c, `DELETE FROM "user" WHERE id = $1`, userID)
	})
	return workspaceID, userID, agentID, projectID
}

func newDefaultAgentIssueService(pool *pgxpool.Pool) *IssueService {
	queries := db.New(pool)
	taskSvc := NewTaskService(queries, pool, nil, events.New())
	return NewIssueService(queries, pool, nil, nil, taskSvc)
}

// An unassigned issue created in a project with a default_agent_id is
// auto-assigned to that agent. This is the core GRA-380 behavior.
func TestCreateIssueBackfillsProjectDefaultAgent(t *testing.T) {
	ctx := context.Background()
	pool := newDefaultAgentTestPool(t)
	wsID, userID, agentID, projectID := defaultAgentFixture(t, ctx, pool)
	svc := newDefaultAgentIssueService(pool)

	res, err := svc.Create(ctx, IssueCreateParams{
		WorkspaceID: util.MustParseUUID(wsID),
		Title:       "auto-assign me",
		Status:      "todo",
		Priority:    "none",
		CreatorType: "member",
		CreatorID:   util.MustParseUUID(userID),
		ProjectID:   util.MustParseUUID(projectID),
	}, IssueCreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Issue.AssigneeType.String != "agent" || !res.Issue.AssigneeType.Valid {
		t.Fatalf("expected assignee_type=agent, got %+v", res.Issue.AssigneeType)
	}
	if got := util.UUIDToString(res.Issue.AssigneeID); got != agentID {
		t.Fatalf("expected assignee_id=%s, got %s", agentID, got)
	}
}

// An explicit assignee always wins over the project default.
func TestCreateIssueExplicitAssigneeBeatsProjectDefault(t *testing.T) {
	ctx := context.Background()
	pool := newDefaultAgentTestPool(t)
	wsID, userID, agentID, projectID := defaultAgentFixture(t, ctx, pool)
	svc := newDefaultAgentIssueService(pool)

	res, err := svc.Create(ctx, IssueCreateParams{
		WorkspaceID:  util.MustParseUUID(wsID),
		Title:        "explicitly assigned",
		Status:       "todo",
		Priority:     "none",
		CreatorType:  "member",
		CreatorID:    util.MustParseUUID(userID),
		ProjectID:    util.MustParseUUID(projectID),
		AssigneeType: pgtype.Text{String: "member", Valid: true},
		AssigneeID:   util.MustParseUUID(userID),
	}, IssueCreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Issue.AssigneeType.String != "member" {
		t.Fatalf("explicit member assignee should win, got type %q", res.Issue.AssigneeType.String)
	}
	if got := util.UUIDToString(res.Issue.AssigneeID); got != userID {
		t.Fatalf("expected explicit assignee %s, got %s (default agent was %s)", userID, got, agentID)
	}
}

// A backlog issue is still assigned the default agent, but the enqueue is
// parked (backlog is a pre-assign parking lot) — assignment happens regardless
// of status.
func TestCreateBacklogIssueBackfillsProjectDefaultAgent(t *testing.T) {
	ctx := context.Background()
	pool := newDefaultAgentTestPool(t)
	wsID, userID, agentID, projectID := defaultAgentFixture(t, ctx, pool)
	svc := newDefaultAgentIssueService(pool)

	res, err := svc.Create(ctx, IssueCreateParams{
		WorkspaceID: util.MustParseUUID(wsID),
		Title:       "backlog default",
		Status:      "backlog",
		Priority:    "none",
		CreatorType: "member",
		CreatorID:   util.MustParseUUID(userID),
		ProjectID:   util.MustParseUUID(projectID),
	}, IssueCreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := util.UUIDToString(res.Issue.AssigneeID); got != agentID {
		t.Fatalf("backlog issue should still be assigned the default agent %s, got %s", agentID, got)
	}
}
