package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNotLeader is returned when a refresh was requested on a broker pod
// that isn't currently the lease holder. HTTP callers and the background
// loop treat this as "serve cached and try again later" — refresh is a
// leader-only action.
var ErrNotLeader = errors.New("not the leader; refresh skipped")

// refreshOpTimeout bounds a single rotation (load + OAuth call + store) once it
// has been detached from the caller's request context. It's a backstop that
// guarantees refreshMu is released even if the OAuth client overruns its own
// timeout + retry budget; it is deliberately generous so it never cuts a
// legitimate retry sequence short.
const refreshOpTimeout = 3 * time.Minute

// LeaderGate is the subset of LeaderState that Refresher needs. Pulling it
// out lets tests pass a stub without spinning up an actual elector.
type LeaderGate interface {
	IsLeader() bool
}

// Refresher composes the Secret store, leader gate, and OAuth client.
type Refresher struct {
	store      *SecretStore
	leader     LeaderGate
	oauth      *OAuthClient
	refreshPad time.Duration

	// refreshMu single-flights the actual OAuth refresh. Each refresh rotates
	// the refresh_token server-side, so two refreshes racing on the same stored
	// refresh_token both call Anthropic, both persist (last-write-wins), and
	// every access_token handed out from a losing race is left paired with a
	// refresh_token that's already been superseded — clients then see
	// intermittent 401s and the next refresh can fail with invalid_grant. The
	// holder owns load+refresh+store for one rotation; concurrent callers
	// TryLock, fail, and serve the current cached token rather than queueing.
	refreshMu sync.Mutex

	// lastSeenMu protects lastSeenRefreshToken. The value is the refresh_token
	// the broker most recently observed in (or wrote to) the source Secret.
	// When the value loaded from the Secret differs from this — and we didn't
	// just write it ourselves via a successful refresh — an operator has
	// reseeded the Secret (e.g. after recovering from a revoked-token
	// permanent failure). In that case we MUST bypass the "still fresh"
	// optimization and exchange the new refresh_token immediately, so we
	// (a) prove it works and (b) bring the access_token into sync with the
	// new lineage. Detected reseeds bump reseedDetectedTotal so an operator
	// can confirm the broker noticed.
	lastSeenMu           sync.Mutex
	lastSeenRefreshToken string
}

func NewRefresher(store *SecretStore, leader LeaderGate, oauth *OAuthClient, refreshPad time.Duration) *Refresher {
	return &Refresher{store: store, leader: leader, oauth: oauth, refreshPad: refreshPad}
}

// observeAndCheckReseed records the refresh_token we just loaded and reports
// whether it differs from the last token we observed/wrote. An initial
// observation (lastSeenRefreshToken == "") is not a reseed; only a change
// between two non-empty observations is. The seen-token is always updated to
// the latest observation, so a single reseed fires once and subsequent loads
// look normal.
func (r *Refresher) observeAndCheckReseed(loaded string) bool {
	r.lastSeenMu.Lock()
	defer r.lastSeenMu.Unlock()
	reseed := r.lastSeenRefreshToken != "" && loaded != "" && loaded != r.lastSeenRefreshToken
	r.lastSeenRefreshToken = loaded
	return reseed
}

// RefreshIfNeeded loads the current state and, if we're the leader and the
// access_token is within RefreshPad of expiry (OR the operator reseeded the
// Secret with a fresh refresh_token), calls Anthropic and persists the rotated
// state. Returns (refreshed, current_state, err).
//
// Errors:
//   - ErrNotLeader: serve cached if non-expired; the next leader will refresh.
//   - *PermanentError (oauth_client): operator must intervene; cached preserved.
//   - *TransientError: caller serves cached; next tick retries.
func (r *Refresher) RefreshIfNeeded(ctx context.Context) (bool, *TokenState, error) {
	state, err := r.store.Load(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("load: %w", err)
	}

	reseeded := r.observeAndCheckReseed(state.RefreshToken)
	if reseeded {
		reseedDetectedTotal.Inc()
	}

	// Still fresh enough AND we didn't just spot a reseed — leadership doesn't
	// matter for this branch. A reseed must force a refresh even when the
	// cached access_token is still within pad, otherwise the broker would sit
	// on a perfectly valid new refresh_token without validating it until the
	// access_token next expires — defeating the point of reseeding immediately.
	if !reseeded && !state.ExpiresAt.IsZero() && time.Until(state.ExpiresAt) > r.refreshPad {
		return false, state, nil
	}

	if !r.leader.IsLeader() {
		return false, state, ErrNotLeader
	}

	// Single-flight the rotation, but never queue. If another goroutine is
	// already refreshing, serve the current (still within-pad, non-expired)
	// token and let that in-flight refresh update the cache. Blocking here
	// would pile /access_token requests up behind a slow or failing upstream
	// call — the moment we can least afford added latency.
	if !r.refreshMu.TryLock() {
		return false, state, nil
	}
	defer r.refreshMu.Unlock()

	// Detach the rotation from the caller's context. This is driven from an
	// HTTP request handler, and a single client disconnecting must not cancel
	// a refresh that the rest of the burst (and the background loop) is relying
	// on. Keep request values, drop the cancellation, and bound it independently.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshOpTimeout)
	defer cancel()

	// Double-check under the lock: a sibling may have refreshed and stored a
	// fresh token just before we acquired it. Adopt that instead of spending
	// another rotation that would invalidate it. We feed observeAndCheckReseed
	// the new value too so our internal tracker doesn't decide the sibling's
	// rotation looks like an operator reseed on the next tick.
	state, err = r.store.Load(rctx)
	if err != nil {
		return false, nil, fmt.Errorf("reload under refresh lock: %w", err)
	}
	r.observeAndCheckReseed(state.RefreshToken)
	// Honor the outer reseed signal: if the rotation was triggered by an
	// operator reseed (not by access_token freshness), the inner freshness
	// short-circuit must NOT fire. Otherwise the broker reads the new
	// refresh_token, sees the access_token still fresh, and returns without
	// exchanging — defeating the whole point of detecting the reseed.
	if !reseeded && !state.ExpiresAt.IsZero() && time.Until(state.ExpiresAt) > r.refreshPad {
		return false, state, nil
	}

	res, err := r.oauth.Refresh(rctx, state.RefreshToken)
	if err != nil {
		// Preserve cached state. Classification preserved via errors.As.
		return false, state, err
	}

	newState := &TokenState{
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(res.ExpiresIn) * time.Second),
	}
	if newState.RefreshToken == "" {
		// Anthropic occasionally omits the rotated value when re-using the
		// existing refresh_token is acceptable. Keep the old one.
		newState.RefreshToken = state.RefreshToken
	}
	if err := r.store.Store(rctx, newState); err != nil {
		return false, state, fmt.Errorf("persist new state: %w", err)
	}
	// Record what we just wrote, so the next Load doesn't read our own rotation
	// as a reseed.
	r.observeAndCheckReseed(newState.RefreshToken)
	return true, newState, nil
}
