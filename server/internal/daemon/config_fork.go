// This file holds ProbeAgents, the fork's extraction of the agent-CLI probing
// logic out of LoadConfig. The run-task subcommand (single-task / controller
// mode) needs the same probing without the rest of LoadConfig's side effects
// (daemon ID resolution, GC durations, etc.), so LoadConfig now calls into this
// shared function via a single call-site seam. Keeping the extraction in a
// fork-owned file confines the divergence in config.go to that one call site.

package daemon

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// ProbeAgents detects available agent CLIs on PATH and returns a map keyed by
// provider name. Reused by both the daemon's LoadConfig and the run-task
// subcommand (single-task mode), which needs the same probing logic without
// the rest of LoadConfig's side effects (daemon ID resolution, GC durations,
// etc.).
//
// Returns a non-empty map on success. Returns an error when no agent CLI is
// found at all — the caller has no useful work it can do in that case.
//
// Implementation notes: resolveAgentExecutablePath is the primary lookup, but
// on macOS/Linux a GUI-launched daemon (Electron, Launchpad) does not inherit
// the user's interactive shell PATH — fnm/nvm/volta multishells, the Anthropic
// native installer prefix, and per-user npm prefixes all live in dirs only
// added to PATH by ~/.zshrc / ~/.bashrc. We lazily fall back to the login shell
// to canonicalise paths the daemon process can't see, but only when a bare
// command name actually missed the primary lookup — pinning MULTICA_*_PATH
// still takes the fast path.
func ProbeAgents() (map[string]AgentEntry, error) {
	var (
		shellResolveOnce sync.Once
		shellResolved    map[string]string
	)
	getShellResolved := func() map[string]string {
		shellResolveOnce.Do(func() {
			shellResolved = resolveAgentsViaLoginShell(defaultAgentCommandNames)
		})
		return shellResolved
	}
	probe := func(envVar, defaultCmd, modelEnv string) (AgentEntry, bool) {
		cmd := envOrDefault(envVar, defaultCmd)
		if path, err := resolveAgentExecutablePath(cmd); err == nil {
			return AgentEntry{
				Path:  path,
				Model: strings.TrimSpace(os.Getenv(modelEnv)),
			}, true
		}
		// The shell fallback only rescues bare command names. An operator
		// who pinned MULTICA_*_PATH to an absolute or relative path that
		// doesn't exist should hard-miss, not silently get a different
		// binary.
		if strings.ContainsAny(cmd, "/\\") {
			return AgentEntry{}, false
		}
		if path, ok := getShellResolved()[cmd]; ok {
			return AgentEntry{
				Path:  path,
				Model: strings.TrimSpace(os.Getenv(modelEnv)),
			}, true
		}
		if defaultCmd == "codex" && cmd == defaultCmd {
			// Codex Desktop bundles its CLI inside the macOS app instead of
			// installing it onto PATH.
			for _, p := range codexDesktopAppBundlePaths() {
				if _, err := os.Stat(p); err == nil {
					return AgentEntry{
						Path:  p,
						Model: strings.TrimSpace(os.Getenv(modelEnv)),
					}, true
				}
			}
		}
		return AgentEntry{}, false
	}

	agents := map[string]AgentEntry{}
	if e, ok := probe("MULTICA_CLAUDE_PATH", "claude", "MULTICA_CLAUDE_MODEL"); ok {
		agents["claude"] = e
	}
	if e, ok := probe("MULTICA_CODEX_PATH", "codex", "MULTICA_CODEX_MODEL"); ok {
		agents["codex"] = e
	}
	if e, ok := probe("MULTICA_OPENCODE_PATH", "opencode", "MULTICA_OPENCODE_MODEL"); ok {
		agents["opencode"] = e
	}
	if e, ok := probe("MULTICA_OPENCLAW_PATH", "openclaw", "MULTICA_OPENCLAW_MODEL"); ok {
		agents["openclaw"] = e
	}
	if e, ok := probe("MULTICA_HERMES_PATH", "hermes", "MULTICA_HERMES_MODEL"); ok {
		agents["hermes"] = e
	}
	if e, ok := probe("MULTICA_PI_PATH", "pi", "MULTICA_PI_MODEL"); ok {
		agents["pi"] = e
	}
	if e, ok := probe("MULTICA_CURSOR_PATH", "cursor-agent", "MULTICA_CURSOR_MODEL"); ok {
		agents["cursor"] = e
	}
	if e, ok := probe("MULTICA_COPILOT_PATH", "copilot", "MULTICA_COPILOT_MODEL"); ok {
		agents["copilot"] = e
	}
	if e, ok := probe("MULTICA_KIMI_PATH", "kimi", "MULTICA_KIMI_MODEL"); ok {
		agents["kimi"] = e
	}
	if e, ok := probe("MULTICA_KIRO_PATH", "kiro-cli", "MULTICA_KIRO_MODEL"); ok {
		agents["kiro"] = e
	}
	if e, ok := probe("MULTICA_CODEBUDDY_PATH", "codebuddy", "MULTICA_CODEBUDDY_MODEL"); ok {
		agents["codebuddy"] = e
	}
	// agy 1.0.6 added a `--model` flag (MUL-3125), so Antigravity now takes a
	// model env like every other backend. MULTICA_ANTIGRAVITY_MODEL seeds the
	// daemon-wide default; its value is the exact `agy models` display string
	// (e.g. "Claude Opus 4.6 (Thinking)"), not a provider/model slug.
	if e, ok := probe("MULTICA_ANTIGRAVITY_PATH", "agy", "MULTICA_ANTIGRAVITY_MODEL"); ok {
		agents["antigravity"] = e
	}
	qoderPath := envOrDefault("MULTICA_QODER_PATH", "qodercli")
	if path, err := resolveAgentExecutablePath(qoderPath); err == nil {
		agents["qoder"] = AgentEntry{
			Path:  path,
			Model: strings.TrimSpace(os.Getenv("MULTICA_QODER_MODEL")),
		}
	}
	// ByteDance official TRAE CLI (the `traecli` binary from https://docs.trae.cn/cli),
	// driven over ACP via `traecli acp serve --yolo`. MULTICA_TRAECLI_MODEL seeds
	// the daemon-wide default model (a model id from the user's logged-in traecli
	// catalog).
	if e, ok := probe("MULTICA_TRAECLI_PATH", "traecli", "MULTICA_TRAECLI_MODEL"); ok {
		agents["traecli"] = e
	}
	if len(agents) == 0 {
		return nil, fmt.Errorf("no agent CLI found: install claude, codebuddy, codex, copilot, opencode, openclaw, hermes, pi, cursor-agent, kimi, kiro-cli, agy, qodercli, or traecli and ensure it is on PATH")
	}
	return agents, nil
}
