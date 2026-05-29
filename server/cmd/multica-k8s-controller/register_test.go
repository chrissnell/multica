package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon"
)

func TestRegisterAll_MapsBackRuntimeIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/register" || r.Method != "POST" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		// echo a single runtime back with a server-assigned ID
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runtimes": []map[string]any{
				{"id": "rt-server-1", "name": "Lambda", "provider": "claude", "status": "online"},
			},
		})
	}))
	defer srv.Close()

	cfg := &Config{
		ServerBaseURL:  srv.URL,
		Token:          "tk",
		DaemonIDPrefix: "k8s-controller",
		DeviceName:     "multica-cluster",
		Workspaces: []WorkspaceConfig{
			{ID: "ws-1", Provider: "claude", AgentName: "Lambda", RuntimeImage: "img:v"},
		},
	}
	cli := daemon.NewClient(cfg.ServerBaseURL)
	cli.SetToken(cfg.Token)

	got, err := RegisterAll(context.Background(), cli, cfg)
	if err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("expected 1 registered runtime, got %d", len(got))
	}
	if got[0].RuntimeID != "rt-server-1" {
		t.Errorf("runtime id mismatch: %+v", got[0])
	}
	if got[0].WorkspaceID != "ws-1" {
		t.Errorf("workspace mapping lost: %+v", got[0])
	}
}
