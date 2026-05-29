package daemon

import (
	"io"
	"log/slog"
	"testing"
)

func TestNewSingleTaskRunner_BuildsWithoutRegistration(t *testing.T) {
	t.Setenv("MULTICA_TOKEN", "mul_single_task_test")

	cfg := Config{
		ServerBaseURL:  "http://example.invalid",
		WorkspacesRoot: t.TempDir(),
		HealthPort:     0, // OS-picked; constructor binds it.
	}

	r, err := NewSingleTaskRunner(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewSingleTaskRunner: %v", err)
	}
	defer r.Close()

	if r.client.Token() != "mul_single_task_test" {
		t.Fatalf("token not loaded from env, got %q", r.client.Token())
	}
	if r.HealthPort() == 0 {
		t.Fatalf("expected health port to be bound, got 0")
	}
}

func TestNewSingleTaskRunner_SeedsRuntimeIndex(t *testing.T) {
	t.Setenv("MULTICA_TOKEN", "tk")
	cfg := Config{ServerBaseURL: "http://example.invalid", WorkspacesRoot: t.TempDir()}
	r, err := NewSingleTaskRunner(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	r.SeedRuntime("rt-1", "claude")
	r.mu.Lock()
	got := r.runtimeIndex["rt-1"].Provider
	r.mu.Unlock()
	if got != "claude" {
		t.Fatalf("runtimeIndex not seeded, got provider %q", got)
	}
}
