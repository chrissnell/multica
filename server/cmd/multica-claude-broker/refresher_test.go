package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type stubLeader struct{ leader bool }

func (s *stubLeader) IsLeader() bool { return s.leader }

func makeRefresher(t *testing.T, initial *TokenState, isLeader bool, srvURL string) *Refresher {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data: map[string][]byte{
			"access_token":  []byte(initial.AccessToken),
			"refresh_token": []byte(initial.RefreshToken),
			"expires_at":    []byte(initial.ExpiresAt.Format(time.RFC3339)),
		},
	}
	k := fake.NewSimpleClientset(sec)
	store := NewSecretStore(k, "ns", "s", "ns-access-token")
	oauth := newClientForTest(srvURL, "client-id-x", "oauth-2025-04-20")
	return NewRefresher(store, &stubLeader{leader: isLeader}, oauth, 5*time.Minute)
}

func TestRefreshIfNeeded_StillFresh_NoRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Anthropic must not be called when token is fresh")
	}))
	defer srv.Close()
	state := &TokenState{
		AccessToken:  "A",
		RefreshToken: "R",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	}
	r := makeRefresher(t, state, true, srv.URL)
	refreshed, _, err := r.RefreshIfNeeded(context.Background())
	if err != nil {
		t.Fatalf("RefreshIfNeeded: %v", err)
	}
	if refreshed {
		t.Errorf("expected no refresh for fresh token")
	}
}

func TestRefreshIfNeeded_ExpiringButNotLeader_ReturnsCached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("non-leader must not call Anthropic")
	}))
	defer srv.Close()
	state := &TokenState{
		AccessToken:  "A",
		RefreshToken: "R",
		ExpiresAt:    time.Now().Add(1 * time.Minute), // within refresh pad
	}
	r := makeRefresher(t, state, false, srv.URL)
	refreshed, cached, err := r.RefreshIfNeeded(context.Background())
	if !errors.Is(err, ErrNotLeader) {
		t.Errorf("expected ErrNotLeader, got %v", err)
	}
	if refreshed {
		t.Errorf("non-leader refreshed")
	}
	if cached == nil || cached.AccessToken != "A" {
		t.Errorf("non-leader didn't return cached state: %+v", cached)
	}
}

func TestRefreshIfNeeded_LeaderRefreshes(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "ACCESS_NEW",
			"refresh_token": "REFRESH_NEW",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()
	state := &TokenState{
		AccessToken:  "ACCESS_OLD",
		RefreshToken: "REFRESH_OLD",
		ExpiresAt:    time.Now().Add(2 * time.Minute), // within refresh pad
	}
	r := makeRefresher(t, state, true, srv.URL)
	refreshed, newState, err := r.RefreshIfNeeded(context.Background())
	if err != nil {
		t.Fatalf("RefreshIfNeeded: %v", err)
	}
	if !refreshed {
		t.Errorf("expected refresh")
	}
	if newState.AccessToken != "ACCESS_NEW" || newState.RefreshToken != "REFRESH_NEW" {
		t.Errorf("unexpected new state: %+v", newState)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("server calls = %d, want 1", atomic.LoadInt32(&calls))
	}
}

func TestRefreshIfNeeded_LeaderRefresh_EmptyRotatedTokenKeepsOld(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "ACCESS_NEW",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()
	state := &TokenState{
		AccessToken:  "ACCESS_OLD",
		RefreshToken: "REFRESH_OLD",
		ExpiresAt:    time.Now().Add(1 * time.Minute),
	}
	r := makeRefresher(t, state, true, srv.URL)
	_, newState, err := r.RefreshIfNeeded(context.Background())
	if err != nil {
		t.Fatalf("RefreshIfNeeded: %v", err)
	}
	if newState.RefreshToken != "REFRESH_OLD" {
		t.Errorf("expected refresh_token to be preserved when server omits it; got %q", newState.RefreshToken)
	}
}

// TestRefreshIfNeeded_ConcurrentRefreshSingleFlights guards the fix for the
// intermittent-401 bug: a burst of /access_token requests landing inside the
// refresh window must rotate the refresh_token exactly once, not once per
// caller. Each refresh rotates the token server-side, so N concurrent
// rotations would invalidate the access_tokens handed out by the losing races.
func TestRefreshIfNeeded_ConcurrentRefreshSingleFlights(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// Hold briefly so the other goroutines genuinely contend for the lock
		// (TryLock-fail and serve cached) while this one is mid-rotation.
		time.Sleep(50 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "ACCESS_NEW",
			"refresh_token": "REFRESH_NEW",
			"expires_in":    3600, // comfortably past the 5m pad → recheck short-circuits
		})
	}))
	defer srv.Close()
	state := &TokenState{
		AccessToken:  "ACCESS_OLD",
		RefreshToken: "REFRESH_OLD",
		ExpiresAt:    time.Now().Add(1 * time.Minute), // within refresh pad
	}
	r := makeRefresher(t, state, true, srv.URL)

	const n = 16
	var wg sync.WaitGroup
	var refreshedCount int32
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			refreshed, _, err := r.RefreshIfNeeded(context.Background())
			if err != nil {
				t.Errorf("RefreshIfNeeded: %v", err)
				return
			}
			if refreshed {
				atomic.AddInt32(&refreshedCount, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream Anthropic calls = %d, want exactly 1 (single-flight)", got)
	}
	if got := atomic.LoadInt32(&refreshedCount); got != 1 {
		t.Errorf("callers that rotated = %d, want exactly 1", got)
	}
}

// TestRefreshIfNeeded_OperatorReseed_ForcesRefresh is the post-2026-06-29
// recovery-path test: after the operator reseeds the source Secret with a
// fresh refresh_token (the standard fix for a revoked-token permanent
// failure), the very next tick must rotate the token immediately instead of
// waiting for the cached access_token to expire — even though the access_token
// is still fresh — so we both (a) prove the new refresh_token works and (b)
// bring the access_token into sync with the new lineage. The detection bumps
// reseedDetectedTotal so an operator can confirm the broker noticed.
func TestRefreshIfNeeded_OperatorReseed_ForcesRefresh(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "ACCESS_AFTER_RESEED",
			"refresh_token": "REFRESH_AFTER_RESEED",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	// access_token is well within freshness pad — normally we'd skip refresh.
	state := &TokenState{
		AccessToken:  "ACCESS_OLD",
		RefreshToken: "REFRESH_OLD",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	}
	r := makeRefresher(t, state, true, srv.URL)

	// First call establishes the baseline lastSeen value. Token is fresh, no refresh.
	refreshed, _, err := r.RefreshIfNeeded(context.Background())
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if refreshed {
		t.Fatalf("baseline: should not refresh (token is fresh)")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("baseline: server called %d times, want 0", atomic.LoadInt32(&calls))
	}

	// Operator reseeds the Secret with a different refresh_token. Access token
	// is *still fresh* in the Secret — what changed is just the refresh_token.
	reseed := &TokenState{
		AccessToken:  "ACCESS_OLD",
		RefreshToken: "REFRESH_RESEEDED",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	}
	if err := r.store.Store(context.Background(), reseed); err != nil {
		t.Fatalf("simulate reseed: %v", err)
	}

	startReseed := readCounter(t, reseedDetectedTotal)

	// Second tick: must detect the reseed and force a rotation despite freshness.
	refreshed, newState, err := r.RefreshIfNeeded(context.Background())
	if err != nil {
		t.Fatalf("post-reseed: %v", err)
	}
	if !refreshed {
		t.Errorf("post-reseed: must refresh; reseed should bypass freshness check")
	}
	if newState.AccessToken != "ACCESS_AFTER_RESEED" || newState.RefreshToken != "REFRESH_AFTER_RESEED" {
		t.Errorf("post-reseed state wrong: %+v", newState)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("post-reseed: server called %d times, want 1", atomic.LoadInt32(&calls))
	}
	if delta := readCounter(t, reseedDetectedTotal) - startReseed; delta < 1 {
		t.Errorf("reseedDetectedTotal delta = %v, want >= 1", delta)
	}
}

// TestRefreshIfNeeded_OurOwnRotationIsNotReseed guards against a self-trigger
// loop: after we successfully rotate the refresh_token ourselves, the next
// tick must NOT see the rotated value as an operator reseed. Without the
// post-Store observation, the broker would force a refresh on every tick
// after every rotation forever.
func TestRefreshIfNeeded_OurOwnRotationIsNotReseed(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "ACCESS_NEW",
			"refresh_token": "REFRESH_NEW",
			"expires_in":    3600, // well past pad, so post-rotation it's fresh
		})
	}))
	defer srv.Close()

	state := &TokenState{
		AccessToken:  "ACCESS_OLD",
		RefreshToken: "REFRESH_OLD",
		ExpiresAt:    time.Now().Add(1 * time.Minute), // within pad → first call refreshes
	}
	r := makeRefresher(t, state, true, srv.URL)

	if _, _, err := r.RefreshIfNeeded(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("first refresh: server calls = %d, want 1", got)
	}

	// Second tick: token is now fresh AND our internal tracker should already
	// know about REFRESH_NEW (we stored it). No refresh, no reseed detection.
	startReseed := readCounter(t, reseedDetectedTotal)
	refreshed, _, err := r.RefreshIfNeeded(context.Background())
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if refreshed {
		t.Errorf("second tick refreshed — our own rotation was misread as an operator reseed")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("second tick re-called upstream: total = %d, want 1", got)
	}
	if delta := readCounter(t, reseedDetectedTotal) - startReseed; delta != 0 {
		t.Errorf("reseedDetectedTotal delta = %v, want 0 (our rotation must not look like a reseed)", delta)
	}
}

func TestRefreshIfNeeded_LeaderPermanentError_PreservesCached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	state := &TokenState{
		AccessToken:  "ACCESS_OLD",
		RefreshToken: "REFRESH_OLD",
		ExpiresAt:    time.Now().Add(1 * time.Minute),
	}
	r := makeRefresher(t, state, true, srv.URL)
	refreshed, cached, err := r.RefreshIfNeeded(context.Background())
	var perm *PermanentError
	if !errors.As(err, &perm) {
		t.Errorf("expected PermanentError, got %T: %v", err, err)
	}
	if refreshed {
		t.Errorf("must not mark refreshed on permanent error")
	}
	if cached == nil || cached.AccessToken != "ACCESS_OLD" {
		t.Errorf("cached state not preserved: %+v", cached)
	}
}
