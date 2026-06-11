package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfig_FromEnvAndFile(t *testing.T) {
	cfgDir := t.TempDir()
	cfgYAML := []byte(`
workspaces:
  - id: 11111111-1111-1111-1111-111111111111
    provider: claude
    agentName: Lambda
    runtimeImage: ghcr.io/chrissnell/multica-runtime-claude:v0.3.0-mk1
    pvcSize: 5Gi
    storageClass: ""
imagePullSecret: ghcr-pull
`)
	if err := os.WriteFile(filepath.Join(cfgDir, "runtime.yaml"), cfgYAML, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MULTICA_SERVER_URL", "http://multica-backend.multica.svc:8080")
	t.Setenv("MULTICA_TOKEN", "tk")
	t.Setenv("POD_NAMESPACE", "multica")
	t.Setenv("CONTROLLER_CONFIG_DIR", cfgDir)

	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got.ServerBaseURL != "http://multica-backend.multica.svc:8080" {
		t.Errorf("ServerBaseURL = %q", got.ServerBaseURL)
	}
	if got.Token != "tk" {
		t.Errorf("Token mismatch")
	}
	if got.Namespace != "multica" {
		t.Errorf("Namespace = %q", got.Namespace)
	}
	if len(got.Workspaces) != 1 || got.Workspaces[0].Provider != "claude" {
		t.Errorf("Workspaces parsed wrong: %+v", got.Workspaces)
	}
	if got.PollInterval != 3*time.Second {
		t.Errorf("PollInterval default = %v", got.PollInterval)
	}
}

func TestLoadConfig_RepoCacheDefaults(t *testing.T) {
	cfgDir := t.TempDir()
	cfgYAML := []byte(`
workspaces:
  - id: 11111111-1111-1111-1111-111111111111
    provider: claude
    runtimeImage: ghcr.io/x/multica-runtime-claude:dev
repoCache:
  enabled: true
`)
	if err := os.WriteFile(filepath.Join(cfgDir, "runtime.yaml"), cfgYAML, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MULTICA_SERVER_URL", "http://x")
	t.Setenv("MULTICA_TOKEN", "tk")
	t.Setenv("POD_NAMESPACE", "multica")
	t.Setenv("CONTROLLER_CONFIG_DIR", cfgDir)

	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !got.RepoCache.Enabled {
		t.Errorf("RepoCache.Enabled = false")
	}
	if got.RepoCache.PVCName != "multica-repocache-repos" {
		t.Errorf("RepoCache.PVCName default = %q", got.RepoCache.PVCName)
	}
	if got.RepoCache.MountPath != "/repos" {
		t.Errorf("RepoCache.MountPath default = %q", got.RepoCache.MountPath)
	}
}

func TestLoadConfig_RepoCacheDisabledLeavesFieldsZero(t *testing.T) {
	cfgDir := t.TempDir()
	cfgYAML := []byte(`
workspaces:
  - id: 11111111-1111-1111-1111-111111111111
    provider: claude
    runtimeImage: ghcr.io/x/multica-runtime-claude:dev
`)
	if err := os.WriteFile(filepath.Join(cfgDir, "runtime.yaml"), cfgYAML, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MULTICA_SERVER_URL", "http://x")
	t.Setenv("MULTICA_TOKEN", "tk")
	t.Setenv("POD_NAMESPACE", "multica")
	t.Setenv("CONTROLLER_CONFIG_DIR", cfgDir)

	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.RepoCache.Enabled {
		t.Errorf("RepoCache.Enabled should be false")
	}
	if got.RepoCache.PVCName != "" {
		t.Errorf("RepoCache.PVCName should be empty when disabled")
	}
}

func TestLoadConfig_WorkerExtraEnv(t *testing.T) {
	cfgDir := t.TempDir()
	cfgYAML := []byte(`
workspaces:
  - id: 11111111-1111-1111-1111-111111111111
    provider: claude
    runtimeImage: ghcr.io/x/multica-runtime-claude:dev
workerExtraEnv:
  - name: CLOUDFLARE_API_TOKEN
    secretName: multica-cloudflare
    secretKey: api-token
  - name: AWS_ACCESS_KEY_ID
    secretName: multica-cloudflare
    secretKey: access-key-id
`)
	if err := os.WriteFile(filepath.Join(cfgDir, "runtime.yaml"), cfgYAML, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MULTICA_SERVER_URL", "http://x")
	t.Setenv("MULTICA_TOKEN", "tk")
	t.Setenv("POD_NAMESPACE", "multica")
	t.Setenv("CONTROLLER_CONFIG_DIR", cfgDir)

	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(got.WorkerExtraEnv) != 2 {
		t.Fatalf("WorkerExtraEnv len = %d, want 2", len(got.WorkerExtraEnv))
	}
	if got.WorkerExtraEnv[0].Name != "CLOUDFLARE_API_TOKEN" ||
		got.WorkerExtraEnv[0].SecretName != "multica-cloudflare" ||
		got.WorkerExtraEnv[0].SecretKey != "api-token" {
		t.Errorf("WorkerExtraEnv[0] = %+v", got.WorkerExtraEnv[0])
	}
}

func TestLoadConfig_WorkerExtraEnvRejectsIncomplete(t *testing.T) {
	cfgDir := t.TempDir()
	cfgYAML := []byte(`
workspaces:
  - id: 11111111-1111-1111-1111-111111111111
    provider: claude
    runtimeImage: ghcr.io/x/multica-runtime-claude:dev
workerExtraEnv:
  - name: CLOUDFLARE_API_TOKEN
    secretName: multica-cloudflare
`)
	if err := os.WriteFile(filepath.Join(cfgDir, "runtime.yaml"), cfgYAML, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MULTICA_SERVER_URL", "http://x")
	t.Setenv("MULTICA_TOKEN", "tk")
	t.Setenv("POD_NAMESPACE", "multica")
	t.Setenv("CONTROLLER_CONFIG_DIR", cfgDir)

	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error for incomplete workerExtraEnv entry, got nil")
	}
}
