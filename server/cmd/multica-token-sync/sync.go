package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/client-go/kubernetes"
)

// keychainPayload mirrors Claude Code's expected credentials.json shape.
type keychainPayload struct {
	ClaudeAiOauth oauthBlob `json:"claudeAiOauth"`
}

type oauthBlob struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"` // millis since epoch
	Scopes           []string `json:"scopes"`
	SubscriptionType string   `json:"subscriptionType"`
}

// Scopes the Claude Code CLI uses; stable across rotations.
var defaultScopes = []string{
	"user:profile",
	"user:inference",
	"user:sessions:claude_code",
	"user:mcp_servers",
}

// pushSkew tolerates minor timestamp drift so trivial round-trip
// differences (RFC3339 rounds off sub-second precision, brokers stamp
// expires_at from ExpiresIn + now, etc.) don't cause a churn loop where
// each side keeps "correcting" the other by a fraction of a second. Must
// stay well below the broker's refreshPad so a genuine keychain rotation
// still triggers a push.
const pushSkew = 30 * time.Second

// SyncDirection reports which side of the reconciler won on a given tick.
type SyncDirection int

const (
	DirectionNoop SyncDirection = iota
	DirectionPull               // k8s → keychain (broker-driven rotation)
	DirectionPush               // keychain → k8s (Mac CLI rotated while broker was stuck)
)

func (d SyncDirection) String() string {
	switch d {
	case DirectionPull:
		return "pull"
	case DirectionPush:
		return "push"
	default:
		return "noop"
	}
}

// SyncResult captures the per-sync outcome for the caller / logs.
type SyncResult struct {
	Direction      SyncDirection
	Wrote          bool
	OldFingerprint string // sha256 hex of the prior Keychain blob (empty if absent)
	NewFingerprint string // sha256 hex of the freshly built blob (empty on push — see below)
}

// SyncOnce reconciles broker Secret ⇄ Mac Keychain.
//
// Direction is decided by whichever side holds the fresher OAuth token pair.
// The invariant: only one side refreshes at a time. When the broker is
// healthy it rotates every ~refreshPad-of-expiry and its k8s Secret is
// always ahead of the keychain; sync pulls. When the broker is stuck (a
// revoked or race-invalidated refresh_token), the local Claude Code CLI
// eventually refreshes on its own, the keychain overtakes k8s, and sync
// pushes the fresh pair up — the broker's next tick then observes a
// changed refresh_token (see server/cmd/multica-claude-broker/refresher.go
// observeAndCheckReseed) and exchanges it, restoring normal operation with
// no operator intervention.
func SyncOnce(ctx context.Context, cfg *Config, k kubernetes.Interface, kc Keychain, logger *slog.Logger) (*SyncResult, error) {
	kState, err := ReadBrokerState(ctx, k, cfg.Namespace, cfg.SecretName)
	if err != nil {
		return nil, err
	}

	existing, kcErr := kc.Read(cfg.KeychainService, cfg.KeychainAccount)
	kcOAuth, kcExpires, kcParsed := parseKeychain(existing, kcErr)

	if shouldPush(kcOAuth, kcExpires, kcParsed, kState) {
		return pushToBroker(ctx, cfg, k, kcOAuth, kcExpires, existing, logger)
	}
	return pullToKeychain(ctx, cfg, kc, kState, existing, logger)
}

// parseKeychain tolerates a missing / malformed keychain payload — either
// case falls through to a pull, which will overwrite it with a valid one.
func parseKeychain(existing []byte, readErr error) (oauthBlob, time.Time, bool) {
	if readErr != nil || len(existing) == 0 {
		return oauthBlob{}, time.Time{}, false
	}
	var p keychainPayload
	if err := json.Unmarshal(existing, &p); err != nil {
		return oauthBlob{}, time.Time{}, false
	}
	var exp time.Time
	if p.ClaudeAiOauth.ExpiresAt > 0 {
		exp = time.UnixMilli(p.ClaudeAiOauth.ExpiresAt).UTC()
	}
	return p.ClaudeAiOauth, exp, true
}

// shouldPush reports whether the keychain holds a strictly newer,
// still-valid, distinctly-rotated token pair vs the broker. All four
// conditions must hold — dropping any of them re-introduces a failure
// mode we already hit:
//   - unparseable keychain → we don't know what's there, fall back to pull
//   - keychain tokens empty → nothing to push
//   - refresh_token identical → nothing to reseed; push is a no-op
//   - keychain not meaningfully ahead of k8s → push would just churn
//     within timestamp rounding noise
//   - keychain already expired → refuses to push a token we know is dead
func shouldPush(kcOAuth oauthBlob, kcExpires time.Time, kcParsed bool, kState *BrokerState) bool {
	if !kcParsed {
		return false
	}
	if kcOAuth.AccessToken == "" || kcOAuth.RefreshToken == "" {
		return false
	}
	if kcOAuth.RefreshToken == kState.RefreshToken {
		return false
	}
	if kcExpires.IsZero() || !kcExpires.After(kState.ExpiresAt.Add(pushSkew)) {
		return false
	}
	if time.Now().After(kcExpires) {
		return false
	}
	return true
}

// pullToKeychain: broker is truth. Build the payload the broker's state
// implies and write it if the keychain diverges. This is the steady-state
// path when the broker is healthy.
func pullToKeychain(ctx context.Context, cfg *Config, kc Keychain, state *BrokerState, existing []byte, logger *slog.Logger) (*SyncResult, error) {
	_ = ctx // reserved for future k8s-write reconcile; keychain writes are local
	payload := keychainPayload{ClaudeAiOauth: oauthBlob{
		AccessToken:      state.AccessToken,
		RefreshToken:     state.RefreshToken,
		ExpiresAt:        state.ExpiresAt.UnixMilli(),
		Scopes:           defaultScopes,
		SubscriptionType: "max",
	}}
	newBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	newFP := fingerprint(newBytes)

	result := &SyncResult{Direction: DirectionPull, NewFingerprint: newFP}
	if len(existing) > 0 {
		result.OldFingerprint = fingerprint(existing)
		if result.OldFingerprint == newFP {
			result.Direction = DirectionNoop
			logger.Info("keychain already current", "fingerprint", newFP)
			return result, nil
		}
		logger.Info("keychain out of date, pulling from broker",
			"from", result.OldFingerprint, "to", newFP)
	} else {
		logger.Info("keychain entry missing, creating from broker", "to", newFP)
	}

	if cfg.DryRun {
		logger.Info("dry-run; not writing keychain")
		return result, nil
	}
	if err := kc.Write(cfg.KeychainService, cfg.KeychainAccount, newBytes); err != nil {
		return nil, fmt.Errorf("keychain write: %w", err)
	}
	result.Wrote = true
	logger.Info("keychain updated",
		"service", cfg.KeychainService,
		"account", cfg.KeychainAccount,
		"expires_at", state.ExpiresAt.Format(time.RFC3339))
	return result, nil
}

// pushToBroker: Mac is truth. The Claude Code CLI has refreshed while the
// broker was stuck on an invalid refresh_token (usually because a prior
// concurrent-refresh race burned the shared token or the broker hit a
// permanent OAuth failure). Push the fresh pair up to the broker's source
// Secret; the broker's next tick sees a changed refresh_token, treats it
// as a reseed, and exchanges it — recovering without human intervention.
//
// We deliberately do NOT touch the mirror Secret (multica-claude-broker-
// access-token) — the broker owns that write after a successful reseed
// refresh; racing it here would leave the mirror and state Secrets pointing
// at a token the broker still thinks is stale.
func pushToBroker(ctx context.Context, cfg *Config, k kubernetes.Interface, kcOAuth oauthBlob, kcExpires time.Time, existing []byte, logger *slog.Logger) (*SyncResult, error) {
	result := &SyncResult{Direction: DirectionPush}
	if len(existing) > 0 {
		result.OldFingerprint = fingerprint(existing)
	}

	logger.Info("keychain fresher than broker, pushing up to reseed",
		"keychain_expires_at", kcExpires.Format(time.RFC3339))

	if cfg.DryRun {
		logger.Info("dry-run; not patching broker secret")
		return result, nil
	}

	state := &BrokerState{
		AccessToken:  kcOAuth.AccessToken,
		RefreshToken: kcOAuth.RefreshToken,
		ExpiresAt:    kcExpires,
	}
	if err := WriteBrokerState(ctx, k, cfg.Namespace, cfg.SecretName, state); err != nil {
		return nil, fmt.Errorf("push to broker: %w", err)
	}
	result.Wrote = true
	logger.Info("broker state reseeded from keychain",
		"namespace", cfg.Namespace,
		"secret", cfg.SecretName,
		"expires_at", kcExpires.Format(time.RFC3339))
	return result, nil
}

// SyncLoop runs SyncOnce on cfg.Interval until ctx is cancelled. Transient
// errors are logged but never terminate the loop — a long-running daemon
// shouldn't die because the cluster blipped.
func SyncLoop(ctx context.Context, cfg *Config, k kubernetes.Interface, kc Keychain, logger *slog.Logger) {
	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	if _, err := SyncOnce(ctx, cfg, k, kc, logger); err != nil {
		logger.Error("initial sync failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := SyncOnce(ctx, cfg, k, kc, logger); err != nil {
				logger.Error("sync tick failed", "error", err)
			}
		}
	}
}

func fingerprint(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
