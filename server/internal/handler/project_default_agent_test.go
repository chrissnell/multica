package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// createDefaultAgentTestAgent seeds a minimal workspace agent and returns its
// ID. Mirrors createWebhookTestAgent; kept local so the default-agent tests
// don't depend on another test file's fixture.
func createDefaultAgentTestAgent(t *testing.T, name string) string {
	t.Helper()
	var agentID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent (
			workspace_id, name, description, runtime_mode, runtime_config,
			runtime_id, visibility, max_concurrent_tasks, owner_id,
			instructions, custom_env, custom_args, mcp_config
		)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'private', 1, $4, '', '{}'::jsonb, '[]'::jsonb, '{}'::jsonb)
		RETURNING id
	`, testWorkspaceID, name, testRuntimeID, testUserID).Scan(&agentID); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, agentID)
	})
	return agentID
}

// A malformed default_agent_id is a clean 400 at the parse boundary, never a 500.
func TestCreateProjectMalformedDefaultAgentReturns400(t *testing.T) {
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/projects?workspace_id="+testWorkspaceID, map[string]any{
		"title":            "malformed default agent",
		"default_agent_id": "not-a-uuid",
	})
	testHandler.CreateProject(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed default_agent_id, got %d: %s", w.Code, w.Body.String())
	}
}

// A well-formed UUID that is not an agent in this workspace is a 400.
func TestCreateProjectUnknownDefaultAgentReturns400(t *testing.T) {
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/projects?workspace_id="+testWorkspaceID, map[string]any{
		"title":            "unknown default agent",
		"default_agent_id": "11111111-1111-1111-1111-111111111111",
	})
	testHandler.CreateProject(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown default_agent_id, got %d: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "valid agent") {
		t.Errorf("expected agent-not-found error, got: %s", body)
	}
}

// A valid agent round-trips through create and read.
func TestCreateProjectWithValidDefaultAgentRoundTrips(t *testing.T) {
	agentID := createDefaultAgentTestAgent(t, "default agent create")

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/projects?workspace_id="+testWorkspaceID, map[string]any{
		"title":            "valid default agent project",
		"default_agent_id": agentID,
	})
	testHandler.CreateProject(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var project ProjectResponse
	if err := json.NewDecoder(w.Body).Decode(&project); err != nil {
		t.Fatalf("decode CreateProject: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, project.ID)
	})
	if project.DefaultAgentID == nil || *project.DefaultAgentID != agentID {
		t.Fatalf("expected default_agent_id %q, got %v", agentID, project.DefaultAgentID)
	}

	// GET echoes it too.
	w = httptest.NewRecorder()
	req = newRequest("GET", "/api/projects/"+project.ID+"?workspace_id="+testWorkspaceID, nil)
	req = withURLParam(req, "id", project.ID)
	testHandler.GetProject(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GetProject: %d %s", w.Code, w.Body.String())
	}
	var fetched ProjectResponse
	if err := json.NewDecoder(w.Body).Decode(&fetched); err != nil {
		t.Fatalf("decode GetProject: %v", err)
	}
	if fetched.DefaultAgentID == nil || *fetched.DefaultAgentID != agentID {
		t.Errorf("GET default_agent_id = %v, want %q", fetched.DefaultAgentID, agentID)
	}
}

// Update distinguishes set / omitted / explicit-null, matching the lead_id
// tri-state contract.
func TestUpdateProjectSetAndClearDefaultAgent(t *testing.T) {
	agentID := createDefaultAgentTestAgent(t, "default agent update")

	// Seed a project with the default already set.
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/projects?workspace_id="+testWorkspaceID, map[string]any{
		"title":            "default agent update project",
		"default_agent_id": agentID,
	})
	testHandler.CreateProject(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed CreateProject: %d %s", w.Code, w.Body.String())
	}
	var project ProjectResponse
	if err := json.NewDecoder(w.Body).Decode(&project); err != nil {
		t.Fatalf("decode CreateProject: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, project.ID)
	})

	// Omitting default_agent_id leaves it unchanged.
	w = httptest.NewRecorder()
	req = newRequest("PUT", "/api/projects/"+project.ID, map[string]any{"title": "renamed"})
	req = withURLParam(req, "id", project.ID)
	testHandler.UpdateProject(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update title: %d %s", w.Code, w.Body.String())
	}
	var afterTitle ProjectResponse
	if err := json.NewDecoder(w.Body).Decode(&afterTitle); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if afterTitle.DefaultAgentID == nil || *afterTitle.DefaultAgentID != agentID {
		t.Fatalf("omitted default_agent_id should be unchanged, got %v", afterTitle.DefaultAgentID)
	}

	// Explicit null clears it.
	w = httptest.NewRecorder()
	req = newRequest("PUT", "/api/projects/"+project.ID, map[string]any{"default_agent_id": nil})
	req = withURLParam(req, "id", project.ID)
	testHandler.UpdateProject(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("clear default_agent_id: %d %s", w.Code, w.Body.String())
	}
	var cleared ProjectResponse
	if err := json.NewDecoder(w.Body).Decode(&cleared); err != nil {
		t.Fatalf("decode clear: %v", err)
	}
	if cleared.DefaultAgentID != nil {
		t.Errorf("expected default_agent_id cleared, got %v", *cleared.DefaultAgentID)
	}
}
