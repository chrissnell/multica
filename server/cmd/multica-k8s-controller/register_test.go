package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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

// TestHeartbeatLoop_RespondsToPendingModelList pins the regression fix for
// "No Models Available" in the agent-create dropdown on k8s deployments.
// The controller used to discard the heartbeat ack, so server-side
// model_list requests sat unanswered until the 30s UI timeout. The loop
// must (a) examine the ack and (b) POST a `completed` result containing
// the static catalog for the runtime's provider.
//
// We use a `gemini` runtime here rather than `claude` to make the test
// hermetic: claude discovery shells out to `claude --help` for thinking-
// level probing, which on a dev host that has the CLI installed slows the
// test down. Gemini has no shellout (`geminiStaticModels()` returns
// directly). The controller code path is provider-agnostic.
func TestHeartbeatLoop_RespondsToPendingModelList(t *testing.T) {
	var (
		mu              sync.Mutex
		modelReports    []map[string]any
		heartbeatHits   int
		alreadyAnswered bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/daemon/heartbeat":
			mu.Lock()
			heartbeatHits++
			// Advertise a pending model_list once; further ticks ack empty
			// so the test doesn't race on dozens of in-flight reports.
			ack := map[string]any{}
			if !alreadyAnswered {
				ack["pending_model_list"] = map[string]any{"id": "req-abc"}
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(ack)
		case "/api/daemon/runtimes/rt-X/models/req-abc/result":
			body, _ := io.ReadAll(r.Body)
			var payload map[string]any
			_ = json.Unmarshal(body, &payload)
			mu.Lock()
			modelReports = append(modelReports, payload)
			alreadyAnswered = true
			mu.Unlock()
			_, _ = io.WriteString(w, "{}")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cli := daemon.NewClient(srv.URL)
	cli.SetToken("tk")

	ctx, cancel := context.WithCancel(context.Background())
	runtimes := []Registered{{RuntimeID: "rt-X", Provider: "gemini"}}

	done := make(chan struct{})
	go func() {
		RunHeartbeatLoop(ctx, cli, runtimes, 10*time.Millisecond)
		close(done)
	}()

	// Poll for the report rather than sleeping a fixed window — the
	// async-goroutine timing in the loop makes a fixed sleep flaky on
	// loaded CI runners.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(modelReports)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if heartbeatHits == 0 {
		t.Fatalf("no heartbeats sent")
	}
	if len(modelReports) == 0 {
		t.Fatalf("controller never reported a model_list result")
	}
	first := modelReports[0]
	if got, want := first["status"], "completed"; got != want {
		t.Fatalf("status: got %v want %v (payload=%+v)", got, want, first)
	}
	models, ok := first["models"].([]any)
	if !ok || len(models) == 0 {
		t.Fatalf("expected non-empty models list, got %#v", first["models"])
	}
}

func TestHeartbeatLoop_SendsForEveryRuntime(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/daemon/heartbeat" {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			calls = append(calls, string(body))
			mu.Unlock()
		}
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()

	cli := daemon.NewClient(srv.URL)
	cli.SetToken("tk")

	ctx, cancel := context.WithCancel(context.Background())
	runtimes := []Registered{
		{RuntimeID: "rt-A"}, {RuntimeID: "rt-B"},
	}

	done := make(chan struct{})
	go func() {
		RunHeartbeatLoop(ctx, cli, runtimes, 10*time.Millisecond)
		close(done)
	}()

	// Give it time for 3 ticks across both runtimes
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n < 4 { // at least 2 ticks × 2 runtimes
		t.Fatalf("expected ≥4 heartbeat calls, got %d", n)
	}
}
