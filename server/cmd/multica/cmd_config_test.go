package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

func newConfigTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config"}
	cmd.Flags().String("profile", "", "")
	return cmd
}

func TestRunConfigSetPersistsSupportedKeysInProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := newConfigTestCmd()
	_ = cmd.Flags().Set("profile", "dev")

	stderr := captureStderr(t)
	defer stderr.restore()
	if err := runConfigSet(cmd, []string{"server_url", "http://127.0.0.1:8080"}); err != nil {
		t.Fatalf("runConfigSet server_url: %v", err)
	}
	if err := runConfigSet(cmd, []string{"app_url", "http://127.0.0.1:3000"}); err != nil {
		t.Fatalf("runConfigSet app_url: %v", err)
	}
	if err := runConfigSet(cmd, []string{"workspace_id", "ws-123"}); err != nil {
		t.Fatalf("runConfigSet workspace_id: %v", err)
	}
	_ = stderr.read()

	cfg, err := cli.LoadCLIConfigForProfile("dev")
	if err != nil {
		t.Fatalf("LoadCLIConfigForProfile: %v", err)
	}
	if cfg.ServerURL != "http://127.0.0.1:8080" || cfg.AppURL != "http://127.0.0.1:3000" || cfg.WorkspaceID != "ws-123" {
		t.Fatalf("config = %#v, want persisted supported keys", cfg)
	}
}

func TestRunConfigShowIncludesProfileAndDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := newConfigTestCmd()
	_ = cmd.Flags().Set("profile", "empty")

	out, err := captureStdout(t, func() error { return runConfigShow(cmd, nil) })
	if err != nil {
		t.Fatalf("runConfigShow: %v", err)
	}
	for _, want := range []string{
		"Profile:      empty",
		"server_url:              (not set)",
		"app_url:                 (not set)",
		"workspace_id:            (not set)",
		"cf_access_client_id:     (not set)",
		"cf_access_client_secret: (not set)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("runConfigShow output missing %q:\n%s", want, out)
		}
	}
}

// TestRunConfigShowMasksCFAccessSecret guards against the secret ever being
// echoed in full: `multica config` output frequently ends up in bug reports
// and support pastes, and the CF Access service-token secret is a live
// bearer credential — treat it the way we would any other and confirm the
// mask never contains the middle of the value.
func TestRunConfigShowMasksCFAccessSecret(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	seed := cli.CLIConfig{
		CFAccessClientID:     "abcd1234.access",
		CFAccessClientSecret: "supersecret-value-with-enough-length",
	}
	if err := cli.SaveCLIConfig(seed); err != nil {
		t.Fatalf("save seed: %v", err)
	}
	// SaveCLIConfig writes CF defaults into the package globals as a side
	// effect via later Load calls. Reset them so unrelated tests in this
	// package binary aren't affected by leftover state.
	t.Cleanup(func() { cli.SetCFAccessDefaults("", "") })

	cmd := newConfigTestCmd()
	out, err := captureStdout(t, func() error { return runConfigShow(cmd, nil) })
	if err != nil {
		t.Fatalf("runConfigShow: %v", err)
	}
	if !strings.Contains(out, "cf_access_client_id:     abcd1234.access") {
		t.Fatalf("cf_access_client_id not shown verbatim:\n%s", out)
	}
	if strings.Contains(out, "supersecret-value-with-enough-length") {
		t.Fatalf("plaintext secret leaked in `config show` output:\n%s", out)
	}
	if !strings.Contains(out, "supe") || !strings.Contains(out, "ngth") {
		t.Fatalf("masked secret missing head/tail hint:\n%s", out)
	}
}

// TestRunConfigSetRejectsHalfPair covers the fix for the silent
// half-pair-persistence footgun: setHeaders drops a half-pair at request
// time, so persisting one via `config set` would silently regress into the
// original CF Access "server unreachable" symptom. Fail loud at the write.
func TestRunConfigSetRejectsHalfPair(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(func() { cli.SetCFAccessDefaults("", "") })

	cmd := newConfigTestCmd()
	stderr := captureStderr(t)
	defer stderr.restore()

	if err := runConfigSet(cmd, []string{"cf_access_client_id", "abcd1234.access"}); err == nil {
		t.Fatalf("runConfigSet cf_access_client_id alone: want error, got nil")
	} else if !strings.Contains(err.Error(), "both be set") {
		t.Fatalf("runConfigSet cf_access_client_id alone: want both-be-set error, got %v", err)
	}

	// Setting both halves in sequence should succeed on the second call.
	if err := runConfigSet(cmd, []string{"cf_access_client_secret", "the-secret-value-goes-here"}); err == nil {
		t.Fatalf("runConfigSet cf_access_client_secret alone: want error, got nil")
	}

	// Empty the guard by seeding both fields on disk before flipping one
	// back to empty — that flip must also be rejected.
	seed := cli.CLIConfig{CFAccessClientID: "abcd1234.access", CFAccessClientSecret: "shhh-this-is-secret-enough"}
	if err := cli.SaveCLIConfig(seed); err != nil {
		t.Fatalf("save seed: %v", err)
	}
	if err := runConfigSet(cmd, []string{"cf_access_client_id", ""}); err == nil {
		t.Fatalf("runConfigSet clearing only cf_access_client_id: want error, got nil")
	}
	_ = stderr.read()
}

// TestRunConfigSetHidesSecretEcho — never print the secret back on stderr
// after a `config set`. Users who ran with `set -x` would otherwise leak it
// into shell history.
func TestRunConfigSetHidesSecretEcho(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(func() { cli.SetCFAccessDefaults("", "") })

	// Seed both halves so setting the secret alone doesn't trip the pair
	// guard — this test is about the echo, not the validation.
	if err := cli.SaveCLIConfig(cli.CLIConfig{CFAccessClientID: "abcd1234.access", CFAccessClientSecret: "old-secret-value-abcdef"}); err != nil {
		t.Fatalf("save seed: %v", err)
	}

	cmd := newConfigTestCmd()
	stderr := captureStderr(t)
	defer stderr.restore()

	if err := runConfigSet(cmd, []string{"cf_access_client_secret", "new-secret-value-abcdef"}); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}
	out := stderr.read()
	if strings.Contains(out, "new-secret-value-abcdef") {
		t.Fatalf("secret echoed to stderr on `config set`:\n%s", out)
	}
	if !strings.Contains(out, "value hidden") {
		t.Fatalf("expected 'value hidden' hint, got:\n%s", out)
	}
}

func TestRunConfigSetRejectsUnknownKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := newConfigTestCmd()
	err := runConfigSet(cmd, []string{"token", "secret"})
	if err == nil || !strings.Contains(err.Error(), "unknown config key") {
		t.Fatalf("runConfigSet error = %v, want unknown key", err)
	}
}
