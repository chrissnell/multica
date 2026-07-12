package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func brokerSecret(access, refresh string, exp time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "broker", Namespace: "multica"},
		Data: map[string][]byte{
			"access_token":  []byte(access),
			"refresh_token": []byte(refresh),
			"expires_at":    []byte(exp.UTC().Format(time.RFC3339)),
		},
	}
}

// seedKeychainWithOAuth writes a payload that mirrors what pullToKeychain
// would produce, so tests can start from a "keychain already populated"
// state without going through SyncOnce first.
func seedKeychainWithOAuth(t *testing.T, kc *stubKeychain, service, account, access, refresh string, exp time.Time) {
	t.Helper()
	payload := keychainPayload{ClaudeAiOauth: oauthBlob{
		AccessToken:      access,
		RefreshToken:     refresh,
		ExpiresAt:        exp.UnixMilli(),
		Scopes:           defaultScopes,
		SubscriptionType: "max",
	}}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal seed payload: %v", err)
	}
	if err := kc.Write(service, account, raw); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
}

// getSecretTokens is a k8s-fake introspection helper — pulls the three
// broker keys back out so tests can assert push actually landed the write.
func getSecretTokens(t *testing.T, k *fake.Clientset, ns, name string) (access, refresh, expires string) {
	t.Helper()
	sec, err := k.CoreV1().Secrets(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret %s/%s: %v", ns, name, err)
	}
	// WriteBrokerState uses JSON-merge patch against `.data` (base64
	// values), which the fake clientset applies directly to `.Data`, so a
	// plain Get here recovers the pushed bytes without walking stringData.
	if v, ok := sec.Data["access_token"]; ok {
		access = string(v)
	}
	if v, ok := sec.Data["refresh_token"]; ok {
		refresh = string(v)
	}
	if v, ok := sec.Data["expires_at"]; ok {
		expires = string(v)
	}
	return
}

func TestSync_WritesKeychainWhenMissing(t *testing.T) {
	exp := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	k := fake.NewSimpleClientset(brokerSecret("ACCESS", "REFRESH", exp))
	kc := &stubKeychain{data: map[string][]byte{}}
	cfg := &Config{Namespace: "multica", SecretName: "broker", KeychainService: "claude", KeychainAccount: "u"}
	res, err := SyncOnce(context.Background(), cfg, k, kc, discardLogger())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if res.Direction != DirectionPull {
		t.Errorf("Direction = %v, want pull", res.Direction)
	}
	if !res.Wrote {
		t.Error("expected write on first sync")
	}

	raw, err := kc.Read("claude", "u")
	if err != nil {
		t.Fatalf("keychain read: %v", err)
	}
	var got struct {
		ClaudeAiOauth struct {
			AccessToken      string   `json:"accessToken"`
			RefreshToken     string   `json:"refreshToken"`
			ExpiresAt        int64    `json:"expiresAt"`
			Scopes           []string `json:"scopes"`
			SubscriptionType string   `json:"subscriptionType"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("payload not valid JSON: %v", err)
	}
	if got.ClaudeAiOauth.AccessToken != "ACCESS" || got.ClaudeAiOauth.RefreshToken != "REFRESH" {
		t.Errorf("payload tokens wrong: %+v", got.ClaudeAiOauth)
	}
	if got.ClaudeAiOauth.SubscriptionType != "max" {
		t.Errorf("subscriptionType = %q", got.ClaudeAiOauth.SubscriptionType)
	}
	if len(got.ClaudeAiOauth.Scopes) != 4 {
		t.Errorf("scopes = %v", got.ClaudeAiOauth.Scopes)
	}
	if got.ClaudeAiOauth.ExpiresAt != exp.UnixMilli() {
		t.Errorf("ExpiresAt = %d want %d", got.ClaudeAiOauth.ExpiresAt, exp.UnixMilli())
	}
}

func TestSync_SkipsWhenUnchanged(t *testing.T) {
	exp := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	k := fake.NewSimpleClientset(brokerSecret("A", "R", exp))
	kc := &stubKeychain{data: map[string][]byte{}}
	cfg := &Config{Namespace: "multica", SecretName: "broker", KeychainService: "claude", KeychainAccount: "u"}

	r1, err := SyncOnce(context.Background(), cfg, k, kc, discardLogger())
	if err != nil {
		t.Fatalf("SyncOnce#1: %v", err)
	}
	if !r1.Wrote {
		t.Error("expected write on first sync")
	}

	r2, err := SyncOnce(context.Background(), cfg, k, kc, discardLogger())
	if err != nil {
		t.Fatalf("SyncOnce#2: %v", err)
	}
	if r2.Wrote {
		t.Error("expected no-op on second sync with unchanged broker state")
	}
	if r2.Direction != DirectionNoop {
		t.Errorf("Direction = %v, want noop", r2.Direction)
	}
	if r2.OldFingerprint == "" || r2.OldFingerprint != r2.NewFingerprint {
		t.Errorf("fingerprints should match on no-op: old=%s new=%s", r2.OldFingerprint, r2.NewFingerprint)
	}
}

func TestSync_RewritesWhenTokenChanges(t *testing.T) {
	exp := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	kc := &stubKeychain{data: map[string][]byte{}}
	cfg := &Config{Namespace: "multica", SecretName: "broker", KeychainService: "claude", KeychainAccount: "u"}

	// First broker state.
	k1 := fake.NewSimpleClientset(brokerSecret("A1", "R1", exp))
	if _, err := SyncOnce(context.Background(), cfg, k1, kc, discardLogger()); err != nil {
		t.Fatalf("SyncOnce#1: %v", err)
	}
	// Broker rotates → new access/refresh tokens (same expires_at, so no
	// mistaken push — only pull direction is legal here).
	k2 := fake.NewSimpleClientset(brokerSecret("A2", "R2", exp))
	r, err := SyncOnce(context.Background(), cfg, k2, kc, discardLogger())
	if err != nil {
		t.Fatalf("SyncOnce#2: %v", err)
	}
	if r.Direction != DirectionPull {
		t.Errorf("Direction = %v, want pull", r.Direction)
	}
	if !r.Wrote {
		t.Error("expected re-write after broker rotation")
	}
	if r.OldFingerprint == r.NewFingerprint {
		t.Errorf("fingerprints should differ after rotation: %s", r.NewFingerprint)
	}
}

func TestSync_DryRunDoesNotWrite(t *testing.T) {
	exp := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	k := fake.NewSimpleClientset(brokerSecret("A", "R", exp))
	kc := &stubKeychain{data: map[string][]byte{}}
	cfg := &Config{
		Namespace: "multica", SecretName: "broker",
		KeychainService: "claude", KeychainAccount: "u",
		DryRun: true,
	}
	res, err := SyncOnce(context.Background(), cfg, k, kc, discardLogger())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if res.Wrote {
		t.Error("dry-run must not write")
	}
	if _, err := kc.Read("claude", "u"); err == nil {
		t.Error("keychain should still be empty after dry-run")
	}
}

func TestSync_BrokerErrorPropagates(t *testing.T) {
	k := fake.NewSimpleClientset() // no secret
	kc := &stubKeychain{data: map[string][]byte{}}
	cfg := &Config{Namespace: "multica", SecretName: "broker", KeychainService: "claude", KeychainAccount: "u"}
	if _, err := SyncOnce(context.Background(), cfg, k, kc, discardLogger()); err == nil {
		t.Error("expected error when broker secret missing")
	}
}

// TestSync_PushesWhenKeychainFresher: the archetypal recovery scenario.
// Broker is stuck on a stale refresh_token; Claude Code CLI on this Mac
// has since refreshed. Sync must push the keychain's fresh pair up so the
// broker's reseed-detector fires on its next tick.
func TestSync_PushesWhenKeychainFresher(t *testing.T) {
	old := time.Now().Add(30 * time.Minute)
	fresh := time.Now().Add(2 * time.Hour) // well past pushSkew
	k := fake.NewSimpleClientset(brokerSecret("STALE_A", "STALE_R", old))
	kc := &stubKeychain{data: map[string][]byte{}}
	seedKeychainWithOAuth(t, kc, "claude", "u", "FRESH_A", "FRESH_R", fresh)
	cfg := &Config{Namespace: "multica", SecretName: "broker", KeychainService: "claude", KeychainAccount: "u"}

	res, err := SyncOnce(context.Background(), cfg, k, kc, discardLogger())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if res.Direction != DirectionPush {
		t.Fatalf("Direction = %v, want push", res.Direction)
	}
	if !res.Wrote {
		t.Fatal("expected push to write to broker secret")
	}
	access, refresh, expires := getSecretTokens(t, k, "multica", "broker")
	if access != "FRESH_A" || refresh != "FRESH_R" {
		t.Errorf("broker secret tokens = %q / %q, want FRESH_A / FRESH_R", access, refresh)
	}
	// Broker's RFC3339 format drops sub-second; check it parses back within pushSkew.
	got, err := time.Parse(time.RFC3339, expires)
	if err != nil {
		t.Fatalf("parse pushed expires_at %q: %v", expires, err)
	}
	if diff := got.Sub(fresh); diff > pushSkew || diff < -pushSkew {
		t.Errorf("pushed expires_at %v differs from keychain %v by %v (>%v)", got, fresh, diff, pushSkew)
	}
}

// TestSync_DoesNotPushWhenExpired: refuses to push a token we already
// know is dead. Otherwise a permanently-broken keychain would replace the
// broker's (also broken) state with a demonstrably-invalid one, and there
// would be no path back to health.
func TestSync_DoesNotPushWhenExpired(t *testing.T) {
	kExpired := time.Now().Add(-1 * time.Hour)
	kFuture := time.Now().Add(1 * time.Hour)
	k := fake.NewSimpleClientset(brokerSecret("STALE_A", "STALE_R", kFuture))
	kc := &stubKeychain{data: map[string][]byte{}}
	seedKeychainWithOAuth(t, kc, "claude", "u", "DEAD_A", "DEAD_R", kExpired)
	cfg := &Config{Namespace: "multica", SecretName: "broker", KeychainService: "claude", KeychainAccount: "u"}

	res, err := SyncOnce(context.Background(), cfg, k, kc, discardLogger())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if res.Direction == DirectionPush {
		t.Fatal("must not push an expired keychain token")
	}
	// This will pull broker → keychain instead. Verify the broker's tokens
	// still stand and the keychain got overwritten with them.
	access, refresh, _ := getSecretTokens(t, k, "multica", "broker")
	if access != "STALE_A" || refresh != "STALE_R" {
		t.Errorf("broker tokens = %q / %q, want unchanged STALE_A / STALE_R", access, refresh)
	}
}

// TestSync_DoesNotPushWhenSameRefreshToken: nothing to reseed if
// refresh_token matches. A push here would be a no-op at best and could
// churn the broker's reseed-detector at worst.
func TestSync_DoesNotPushWhenSameRefreshToken(t *testing.T) {
	old := time.Now().Add(30 * time.Minute)
	fresh := time.Now().Add(2 * time.Hour)
	k := fake.NewSimpleClientset(brokerSecret("A_OLD", "SAME_R", old))
	kc := &stubKeychain{data: map[string][]byte{}}
	// Same refresh_token but a fresher expires_at (mirroring a broker
	// rotation where the refresh_token was reused, not replaced).
	seedKeychainWithOAuth(t, kc, "claude", "u", "A_NEW", "SAME_R", fresh)
	cfg := &Config{Namespace: "multica", SecretName: "broker", KeychainService: "claude", KeychainAccount: "u"}

	res, err := SyncOnce(context.Background(), cfg, k, kc, discardLogger())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if res.Direction == DirectionPush {
		t.Fatal("must not push when refresh_token unchanged")
	}
}

// TestSync_DoesNotPushWithinSkew: a keychain that's only fractionally
// ahead of the broker (RFC3339 rounding, clock skew) must not flip
// direction — otherwise two peers that agree on the tokens can enter a
// ping-pong loop where each keeps "correcting" the other by <1 sec.
func TestSync_DoesNotPushWithinSkew(t *testing.T) {
	base := time.Now().Add(1 * time.Hour)
	within := base.Add(pushSkew / 2) // < pushSkew ahead
	k := fake.NewSimpleClientset(brokerSecret("A_OLD", "R_OLD", base))
	kc := &stubKeychain{data: map[string][]byte{}}
	seedKeychainWithOAuth(t, kc, "claude", "u", "A_NEW", "R_NEW", within)
	cfg := &Config{Namespace: "multica", SecretName: "broker", KeychainService: "claude", KeychainAccount: "u"}

	res, err := SyncOnce(context.Background(), cfg, k, kc, discardLogger())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if res.Direction == DirectionPush {
		t.Fatal("must not push when within pushSkew of broker's expires_at")
	}
}

// TestSync_DryRunDoesNotPush: dry-run must be an assertion tool, not a
// state-mutating one — even in the push direction.
func TestSync_DryRunDoesNotPush(t *testing.T) {
	old := time.Now().Add(30 * time.Minute)
	fresh := time.Now().Add(2 * time.Hour)
	k := fake.NewSimpleClientset(brokerSecret("STALE_A", "STALE_R", old))
	kc := &stubKeychain{data: map[string][]byte{}}
	seedKeychainWithOAuth(t, kc, "claude", "u", "FRESH_A", "FRESH_R", fresh)
	cfg := &Config{
		Namespace: "multica", SecretName: "broker",
		KeychainService: "claude", KeychainAccount: "u",
		DryRun: true,
	}

	res, err := SyncOnce(context.Background(), cfg, k, kc, discardLogger())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if res.Direction != DirectionPush {
		t.Errorf("Direction = %v, want push (even in dry-run the decision stands)", res.Direction)
	}
	if res.Wrote {
		t.Error("dry-run must not write to broker secret")
	}
	access, refresh, _ := getSecretTokens(t, k, "multica", "broker")
	if access != "STALE_A" || refresh != "STALE_R" {
		t.Errorf("broker secret was mutated in dry-run: %q / %q", access, refresh)
	}
}
