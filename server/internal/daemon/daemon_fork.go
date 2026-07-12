// This file holds fork-local daemon behaviour that upstream does not have.
// Keeping it out of daemon.go confines the fork's divergence there to narrow
// call-site seams, so routine upstream merges stop re-conflicting in the hot
// daemon functions. The upstream-owned entry points call into the symbols
// defined here:
//
//   - resolveAuth()                  -> applyForkTokenAuth()  (headless MULTICA_TOKEN)
//   - workspaceCoAuthoredByEnabled() -> coAuthoredByEnabledFromSettings()
//   - handleModelList()              -> BuildModelListPayload()
//
// coAuthoredByEnabledFromSettings, fetchCoAuthoredByEnabled and
// BuildModelListPayload are additive and are also called from controller-mode
// code (health_fork.go, the k8s controller).

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// applyForkTokenAuth honors the headless MULTICA_TOKEN env (consistent with the
// CLI's resolveToken), letting daemons run in containers with only an env var
// and no on-disk config file. Returns true when it authenticated so resolveAuth
// can short-circuit; false falls through to the config-file path.
func (d *Daemon) applyForkTokenAuth() bool {
	envTok := strings.TrimSpace(os.Getenv("MULTICA_TOKEN"))
	if envTok == "" {
		return false
	}
	d.client.SetToken(envTok)
	d.logger.Info("authenticated via MULTICA_TOKEN env")
	return true
}

// coAuthoredByEnabledFromSettings resolves the Co-authored-by gate from a raw
// workspace settings payload. It is the single decision point shared by the
// daemon-mode gate (which reads synced settings) and the controller-mode gate
// (which fetches settings live), so both honor the identical contract: the
// trailer is opt-in and defaults to OFF. It is installed only when the
// workspace explicitly sets `co_authored_by_enabled`=true; the GitHub master
// switch (`github_enabled`=false) still forces it off, and an absent or
// malformed payload resolves to off so attribution can never reappear by
// accident.
func coAuthoredByEnabledFromSettings(settings json.RawMessage) bool {
	if len(settings) == 0 {
		return false // default: disabled
	}
	var s struct {
		GitHubEnabled       *bool `json:"github_enabled"`
		CoAuthoredByEnabled *bool `json:"co_authored_by_enabled"`
	}
	if err := json.Unmarshal(settings, &s); err != nil {
		return false // default: disabled when payload is malformed
	}
	if s.GitHubEnabled != nil && !*s.GitHubEnabled {
		return false
	}
	if s.CoAuthoredByEnabled == nil {
		return false // default: disabled
	}
	return *s.CoAuthoredByEnabled
}

// fetchCoAuthoredByEnabled resolves the Co-authored-by gate for controller-mode
// workers, which run as stateless single-task pods with no synced
// workspaceState to read. It fetches the workspace settings live from the
// server at checkout time and applies the shared resolver.
//
// Reading live is the only way a stateless worker can honor a freshly-flipped
// `co_authored_by_enabled` toggle — there is no settings sync loop in
// controller mode. On a fetch error the gate resolves to false: a worker that
// cannot confirm the setting must not append attribution it can't justify.
func (d *Daemon) fetchCoAuthoredByEnabled(ctx context.Context, workspaceID string) bool {
	resp, err := d.client.GetWorkspaceRepos(ctx, workspaceID)
	if err != nil || resp == nil {
		d.logger.Warn("repo checkout: co-authored-by settings fetch failed; defaulting off",
			"workspace_id", workspaceID, "error", err)
		return false
	}
	return coAuthoredByEnabledFromSettings(resp.Settings)
}

// BuildModelListPayload runs model discovery for the given provider and
// returns the wire payload to send back via ReportModelListResult. Shared
// by the host daemon's heartbeat handler and the k8s controller's
// heartbeat handler so the response shape stays consistent across both
// runtime hosts.
//
// `executablePath` selects the agent CLI to probe; pass "" to let
// agent.ListModels use the provider's default binary name on PATH. On
// hosts without the CLI installed (e.g. the in-cluster controller),
// `claude` falls back to the static catalog and thinking-level probing
// silently no-ops — the static catalog already carries every model the
// daemon advertises.
//
// Returns a `failed` payload (never an error) when discovery fails so
// callers can forward the response to the server unconditionally.
func BuildModelListPayload(ctx context.Context, provider, executablePath string) map[string]any {
	models, err := agent.ListModels(ctx, provider, executablePath)
	if err != nil {
		return map[string]any{
			"status": "failed",
			"error":  err.Error(),
		}
	}

	// Wire format matches handler.ModelEntry. Use a struct (not
	// map[string]string) so the Default bool and the per-model
	// Thinking catalog round-trip — without it the UI loses its
	// "default" badge on the advertised pick and the thinking-level
	// picker for claude/codex (MUL-2339).
	type thinkingLevelWire struct {
		Value       string `json:"value"`
		Label       string `json:"label"`
		Description string `json:"description,omitempty"`
	}
	type modelThinkingWire struct {
		SupportedLevels []thinkingLevelWire `json:"supported_levels"`
		DefaultLevel    string              `json:"default_level,omitempty"`
	}
	type modelWire struct {
		ID       string             `json:"id"`
		Label    string             `json:"label"`
		Provider string             `json:"provider,omitempty"`
		Default  bool               `json:"default,omitempty"`
		Thinking *modelThinkingWire `json:"thinking,omitempty"`
	}
	wire := make([]modelWire, 0, len(models))
	for _, m := range models {
		entry := modelWire{
			ID:       m.ID,
			Label:    m.Label,
			Provider: m.Provider,
			Default:  m.Default,
		}
		if m.Thinking != nil {
			levels := make([]thinkingLevelWire, 0, len(m.Thinking.SupportedLevels))
			for _, lvl := range m.Thinking.SupportedLevels {
				levels = append(levels, thinkingLevelWire{
					Value:       lvl.Value,
					Label:       lvl.Label,
					Description: lvl.Description,
				})
			}
			entry.Thinking = &modelThinkingWire{
				SupportedLevels: levels,
				DefaultLevel:    m.Thinking.DefaultLevel,
			}
		}
		wire = append(wire, entry)
	}
	return map[string]any{
		"status":    "completed",
		"models":    wire,
		"supported": agent.ModelSelectionSupported(provider),
	}
}
