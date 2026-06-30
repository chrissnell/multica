package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Broker wraps the runtime state served to clients: the most recently loaded
// TokenState (cached for fast reads from /access_token), a ready flag flipped
// true once we've successfully loaded state at least once, and the Refresher
// + store used for synchronous refresh-on-demand.
type Broker struct {
	refresher *Refresher
	store     *SecretStore
	logger    *slog.Logger

	mu     sync.RWMutex
	cached *TokenState

	usageMu sync.RWMutex
	usage   *UsageSnapshot

	ready atomic.Bool

	// livenessStaleGrace is how far past ExpiresAt the cached token may drift
	// before /healthz starts failing and asks kubelet to restart us. Failing
	// liveness on stale tokens is the post-2026-06-29 fix: previously the
	// broker silently held a long-expired token while /healthz returned 200,
	// so kubelet had no way to know we were no longer doing our job.
	livenessStaleGrace time.Duration
}

func NewBroker(refresher *Refresher, store *SecretStore, logger *slog.Logger) *Broker {
	return &Broker{
		refresher: refresher,
		store:     store,
		logger:    logger,
		// 10 min: longer than several refresh ticks (30s default), short enough
		// that an operator paging on liveness flaps gets a useful signal within
		// one alert window. Tighter risks flapping on a single failed tick.
		livenessStaleGrace: 10 * time.Minute,
	}
}

// Reload loads state from the Secret into the cache without touching the
// refresh leader gate. Called once at startup so the broker can serve
// /access_token immediately even before leader election settles.
//
// Also mirrors the current access_token into the dedicated access-token
// Secret that worker Job pods read via secretKeyRef. If the mirror Secret
// is the only consumer (the apiKeyHelper HTTP path is deprecated), missing
// it at controller-dispatch time would cause worker pods to fail to start.
func (b *Broker) Reload(ctx context.Context) error {
	state, err := b.store.Load(ctx)
	if err != nil {
		return err
	}
	b.setCached(state)
	b.ready.Store(true)
	if err := b.store.MirrorAccessToken(ctx, state.AccessToken); err != nil {
		// Non-fatal — log but stay up. /access_token still serves; operators
		// can investigate via the broker logs. If the controller can't bind
		// the env var, worker pods will fail until this is resolved.
		b.logger.Warn("mirror access_token on Reload failed", "error", err)
	}
	return nil
}

// RunRefreshLoop ticks at `interval`. Each tick calls RefreshIfNeeded; the
// leader gate makes non-leader pods silently no-op until they win the lease.
func (b *Broker) RunRefreshLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.tickRefresh(ctx)
		}
	}
}

func (b *Broker) tickRefresh(ctx context.Context) {
	start := time.Now()
	refreshed, state, err := b.refresher.RefreshIfNeeded(ctx)
	refreshDuration.Observe(time.Since(start).Seconds())

	switch {
	case err == nil && refreshed:
		refreshTotal.WithLabelValues(outcomeOk).Inc()
		// Any successful rotation clears the "stuck on permanent failure" signal.
		// The streak gauge is what an operator paging on a revoked refresh_token
		// alerts on; if it doesn't fall back to 0 on the very next success, the
		// alert won't auto-resolve when they reseed the Secret.
		permanentFailureStreak.Set(0)
		b.setCached(state)
		if err := b.store.MirrorAccessToken(ctx, state.AccessToken); err != nil {
			b.logger.Warn("mirror access_token after refresh failed", "error", err)
		}
		b.logger.Info("access_token refreshed",
			"result", "success",
			"trigger", "scheduled",
			"expires_at", state.ExpiresAt,
			"valid_for", time.Until(state.ExpiresAt).Truncate(time.Second).String())
	case err == nil && !refreshed:
		refreshTotal.WithLabelValues(outcomeSkipped).Inc()
		// No rotation happened, but RefreshIfNeeded handed back the state it
		// just read from the Secret. Adopt it so the in-memory cache always
		// tracks the Secret — otherwise a pod that never performs a refresh
		// itself (a non-leader replica, or a waiter that lost the single-flight
		// race) keeps serving whatever token it loaded at startup even after
		// the leader has rotated the Secret out from under it.
		if state != nil {
			b.setCached(state)
			if !state.ExpiresAt.IsZero() {
				b.logger.Debug("refresh skipped; token still fresh",
					"expires_at", state.ExpiresAt,
					"expires_in", time.Until(state.ExpiresAt).Truncate(time.Second).String())
			}
		}
	case errors.Is(err, ErrNotLeader):
		refreshTotal.WithLabelValues(outcomeSkipped).Inc()
		refreshFailures.WithLabelValues("not_leader").Inc()
		b.logger.Debug("refresh skipped; this pod is not the leader")
	default:
		refreshTotal.WithLabelValues(outcomeError).Inc()
		var perm *PermanentError
		var transient *TransientError
		switch {
		case errors.As(err, &perm):
			refreshFailures.WithLabelValues("permanent").Inc()
			// Track consecutive permanents only — every other outcome (success,
			// skipped, transient, not_leader) resets the streak. A non-zero
			// streak gauge is the smoking gun for a revoked refresh_token: the
			// only fix is for an operator to reseed the source Secret with a
			// fresh token from a `claude /login`.
			permanentFailureStreak.Inc()
			lastPermanentFailureAt.Set(float64(time.Now().Unix()))
			b.logger.Error("access_token refresh failed",
				"result", "failed", "trigger", "scheduled", "kind", "permanent", "error", err)
		case errors.As(err, &transient):
			refreshFailures.WithLabelValues("transient").Inc()
			// Intentionally do NOT reset permanentFailureStreak — a transient
			// outage on top of a revoked refresh_token must not hide the
			// permanent signal. Only a successful rotation clears the streak.
			b.logger.Warn("access_token refresh failed",
				"result", "failed", "trigger", "scheduled", "kind", "transient", "error", err)
		default:
			refreshFailures.WithLabelValues("other").Inc()
			b.logger.Warn("access_token refresh failed",
				"result", "failed", "trigger", "scheduled", "kind", "other", "error", err)
		}
	}
}

func (b *Broker) setCached(state *TokenState) {
	b.mu.Lock()
	// Monotonic on expiry: never replace the cached token with one that expires
	// earlier. The freshest token is the only one guaranteed still valid after a
	// rotation, so a stale read racing a just-completed refresh (concurrent
	// ticks, a single-flight waiter) must not clobber the newer token.
	if b.cached != nil && !state.ExpiresAt.IsZero() && !b.cached.ExpiresAt.IsZero() &&
		state.ExpiresAt.Before(b.cached.ExpiresAt) {
		b.mu.Unlock()
		return
	}
	b.cached = state
	b.mu.Unlock()
	if !state.ExpiresAt.IsZero() {
		accessTokenExpiresAt.Set(float64(state.ExpiresAt.Unix()))
	}
}

func (b *Broker) cachedSnapshot() *TokenState {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.cached == nil {
		return nil
	}
	cp := *b.cached
	return &cp
}

// ----------- HTTP handlers ----------------------------------------------

// NewAdminMux returns the cluster-reachable handlers: /access_token + probes.
// /refresh is NOT here — see NewOpsMux below for the loopback-only listener.
func NewAdminMux(broker *Broker) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", broker.healthHandler)
	mux.HandleFunc("/readyz", broker.readyHandler)
	mux.HandleFunc("/access_token", broker.accessTokenHandler)
	mux.HandleFunc("/usage", broker.usageHandler)
	return mux
}

// NewOpsMux is the loopback-only handler set. Bound to 127.0.0.1 by main,
// so pod-network traffic can't reach it.
func NewOpsMux(broker *Broker) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/refresh", broker.refreshHandler)
	return mux
}

// healthHandler is the liveness probe. It fails 503 only on signals that a
// restart could plausibly fix:
//   - We were once ready but the cached token has been expired longer than
//     livenessStaleGrace, meaning the refresh loop has clearly stalled and a
//     fresh process (fresh leader bid, fresh kube client, fresh in-mem state)
//     is the cheapest recovery.
//
// We deliberately do NOT fail on "never been ready" — that's startup; the
// startupProbe / readiness probe handle it. Liveness failing during startup
// would crash-loop a brand-new pod that was about to come up.
func (b *Broker) healthHandler(w http.ResponseWriter, r *http.Request) {
	state := b.cachedSnapshot()
	if state != nil && !state.ExpiresAt.IsZero() {
		if expiredFor := time.Since(state.ExpiresAt); expiredFor > b.livenessStaleGrace {
			b.logger.Error("liveness failing: cached token expired beyond grace",
				"expired_at", state.ExpiresAt,
				"expired_for", expiredFor.Truncate(time.Second).String(),
				"grace", b.livenessStaleGrace.String(),
				"leader", b.refresher.leader.IsLeader())
			http.Error(w, "cached token expired beyond grace; restart requested", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (b *Broker) readyHandler(w http.ResponseWriter, r *http.Request) {
	if !b.ready.Load() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

// accessTokenHandler returns the cached bearer token. If the cached token is
// within RefreshPad of expiry, it triggers a synchronous refresh first so
// callers always get a non-expiring token (subject to the leader gate and
// network conditions). If the cached token is already past expiry and no
// fresh one is available, it returns 503.
func (b *Broker) accessTokenHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Best-effort sync refresh. Errors are logged but don't fail the request
	// if we still have a non-expired cached token.
	b.tickRefresh(r.Context())

	state := b.cachedSnapshot()
	if state == nil || state.AccessToken == "" {
		accessTokenRequestsTotal.WithLabelValues(outcomeError).Inc()
		b.logger.Error("access_token requested but no token is cached", "leader", b.refresher.leader.IsLeader())
		http.Error(w, "no token available", http.StatusServiceUnavailable)
		return
	}
	if !state.ExpiresAt.IsZero() && time.Now().After(state.ExpiresAt) {
		// Expired and no refresh available — serve as stale so callers see the
		// 503 they need to surface to operators.
		accessTokenRequestsTotal.WithLabelValues(outcomeStale).Inc()
		b.logger.Error("refusing to serve expired access_token; refresh is not keeping up",
			"expired_at", state.ExpiresAt, "expired_for", time.Since(state.ExpiresAt).Truncate(time.Second).String(),
			"leader", b.refresher.leader.IsLeader())
		http.Error(w, "cached token expired and refresh unavailable", http.StatusServiceUnavailable)
		return
	}
	// A token this close to expiry means the refresh path failed (or this pod
	// isn't the leader): callers that hold it for a multi-minute request will
	// see a mid-flight 401. Surface it — this is the signal that turns an
	// "intermittent 401" report into a diagnosable event.
	if !state.ExpiresAt.IsZero() {
		if remaining := time.Until(state.ExpiresAt); remaining < time.Minute {
			b.logger.Warn("serving access_token that is about to expire",
				"remaining", remaining.Truncate(time.Second).String(), "expires_at", state.ExpiresAt,
				"leader", b.refresher.leader.IsLeader())
		}
	}
	if !state.ExpiresAt.IsZero() {
		b.logger.Debug("serving access_token",
			"expires_at", state.ExpiresAt,
			"expires_in", time.Until(state.ExpiresAt).Truncate(time.Second).String())
	}
	accessTokenRequestsTotal.WithLabelValues(outcomeOk).Inc()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, state.AccessToken)
}

// refreshHandler forces a refresh regardless of expiry. Loopback-only —
// reachable only via kubectl exec on the broker pod. Operator endpoint.
func (b *Broker) refreshHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Bypass the "still fresh" early-return by zeroing the cached expiry
	// briefly via a forced load+refresh path: just call the OAuth client
	// directly through the refresher's components.
	state, err := b.store.Load(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !b.refresher.leader.IsLeader() {
		b.logger.Info("manual access_token refresh rejected; this pod is not the leader")
		http.Error(w, "not the leader; refresh routed to leader pod required", http.StatusServiceUnavailable)
		return
	}
	b.logger.Info("manual access_token refresh requested")
	res, err := b.refresher.oauth.Refresh(r.Context(), state.RefreshToken)
	if err != nil {
		var perm *PermanentError
		status := http.StatusBadGateway
		kind := "transient"
		if errors.As(err, &perm) {
			status = http.StatusBadRequest
			kind = "permanent"
		}
		b.logger.Error("access_token refresh failed",
			"result", "failed", "trigger", "manual", "kind", kind, "error", err)
		http.Error(w, err.Error(), status)
		return
	}
	newState := &TokenState{
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(res.ExpiresIn) * time.Second),
	}
	if newState.RefreshToken == "" {
		newState.RefreshToken = state.RefreshToken
	}
	if err := b.store.Store(r.Context(), newState); err != nil {
		http.Error(w, "persist: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Manual /refresh bypasses RefreshIfNeeded, which is where the reseed
	// tracker normally updates. Without this, a subsequent scheduled tick
	// would Load the new refresh_token, see it differ from the stale tracker
	// value, and counter-productively force *another* rotation.
	b.refresher.observeAndCheckReseed(newState.RefreshToken)
	b.setCached(newState)
	if err := b.store.MirrorAccessToken(r.Context(), newState.AccessToken); err != nil {
		b.logger.Warn("mirror access_token after manual refresh failed", "error", err)
	}
	refreshTotal.WithLabelValues(outcomeOk).Inc()
	permanentFailureStreak.Set(0)
	b.logger.Info("access_token refreshed",
		"result", "success",
		"trigger", "manual",
		"expires_at", newState.ExpiresAt,
		"valid_for", time.Until(newState.ExpiresAt).Truncate(time.Second).String())
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "refreshed; expires_at=%s\n", newState.ExpiresAt.Format(time.RFC3339))
}
