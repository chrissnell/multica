package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// LeaderState wraps client-go's leaderelection.LeaderElector with a small
// public surface: Run, IsLeader, and optional callbacks. The chart pins
// replicas: 1 + strategy: Recreate so usually only one process bids for the
// lease — but the lease eliminates correctness bugs (clock skew, stale
// RenewTime, lost RELEASE during partitions) that hand-rolled coordination
// routinely gets wrong, and survives accidental scale-up.
//
// Run restarts election whenever the elector returns short of ctx cancellation
// — e.g. after a kube API blip causes lease renewal to fail. Without this loop
// a single transient failure permanently downgrades the pod to a Ready-but-
// non-functional state with no token refreshing happening. See the 2026-06-29
// incident: lost leadership at 08:12Z and never re-bid until pod was killed.
type LeaderState struct {
	// runOnce runs exactly one election round and blocks until either the
	// elector loses leadership or ctx is cancelled. Returning here is the
	// signal that Run's outer loop should rebuild and re-enter election.
	// Injectable so tests can drive failure modes (elector returns immediately)
	// that the fake clientset doesn't produce on its own.
	runOnce func(ctx context.Context) error

	leader atomic.Bool

	mu               sync.RWMutex
	OnStartedLeading func()
	OnStoppedLeading func()

	// reelectBackoff is the floor between consecutive runOnce invocations
	// inside Run's restart loop. Without it, a kube API server that's hard-down
	// would spin us at full CPU. With the default LeaderElectionConfig (Retry
	// 4s) the elector itself paces acquire attempts, so this just guards
	// against a degenerate immediate-return failure mode.
	reelectBackoff time.Duration
}

// NewLeaderState configures an elector against a Lease named `name` in
// namespace `ns`, with this pod's identity. Durations follow the
// kubernetes-author defaults for control-plane components (Lease 30s,
// renew 20s, retry 4s).
//
// The elector is rebuilt on every Run-loop iteration. client-go's
// LeaderElector is single-shot — once it returns (either we lost the lease or
// initial acquire failed), its internal observedRecord goes stale and reusing
// it after a renewal failure has tripped people up. Building a fresh one each
// time costs a small allocation and guarantees we start from a clean state.
func NewLeaderState(k kubernetes.Interface, ns, name, identity string) (*LeaderState, error) {
	ls := &LeaderState{
		reelectBackoff: 2 * time.Second,
	}
	newElector := func() (*leaderelection.LeaderElector, error) {
		lock := &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Name: name, Namespace: ns},
			Client:     k.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
		}
		return leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock:            lock,
			LeaseDuration:   30 * time.Second,
			RenewDeadline:   20 * time.Second,
			RetryPeriod:     4 * time.Second,
			ReleaseOnCancel: true, // SIGTERM → tidy handoff
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(context.Context) {
					ls.leader.Store(true)
					ls.mu.RLock()
					cb := ls.OnStartedLeading
					ls.mu.RUnlock()
					if cb != nil {
						cb()
					}
				},
				OnStoppedLeading: func() {
					ls.leader.Store(false)
					ls.mu.RLock()
					cb := ls.OnStoppedLeading
					ls.mu.RUnlock()
					if cb != nil {
						cb()
					}
				},
			},
			Name: "multica-claude-broker",
		})
	}
	// Surface configuration errors at construction so callers don't discover
	// them on the first Run-loop iteration.
	if _, err := newElector(); err != nil {
		return nil, fmt.Errorf("build elector: %w", err)
	}
	ls.runOnce = func(ctx context.Context) error {
		elector, err := newElector()
		if err != nil {
			return err
		}
		elector.Run(ctx)
		return nil
	}
	return ls, nil
}

// Run drives the leader election loop until ctx is cancelled. Each iteration
// rebuilds the client-go elector and calls its Run, which blocks until either
// we lose the lease or initial acquisition gives up. On return we sleep
// briefly and re-enter election — this is the difference between "a single
// kube API blip permanently downgrades us to a useless ready pod" and
// "transient failures heal themselves on the next acquire cycle".
//
// Optional `logger` is informational; nil is fine.
func (l *LeaderState) Run(ctx context.Context, logger *slog.Logger) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := l.runOnce(ctx); err != nil {
			// runOnce only errors on rebuild failure — pure construction, so
			// the only realistic cause is memory pressure or programmer error.
			// Pause then retry; nothing else is reasonable.
			if logger != nil {
				logger.Error("election round failed to start", "error", err)
			}
			if !sleepCtx(ctx, l.reelectBackoff) {
				return
			}
			continue
		}
		// runOnce returned. If ctx is dead, exit cleanly; otherwise re-elect.
		// Every such return — initial-acquire abandoned, renewal failed, lease
		// lost — is a recovery event. Counts so an operator can alert on
		// "broker reelected too many times in 1h", which usually correlates
		// with kube-API instability.
		if ctx.Err() != nil {
			return
		}
		reelectTotal.Inc()
		if logger != nil {
			logger.Warn("elector returned without ctx cancellation; re-entering election",
				"backoff", l.reelectBackoff.String())
		}
		if !sleepCtx(ctx, l.reelectBackoff) {
			return
		}
	}
}

// sleepCtx blocks for d or until ctx is cancelled. Returns true if the sleep
// completed normally, false if ctx was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// IsLeader is safe to call from any goroutine.
func (l *LeaderState) IsLeader() bool { return l.leader.Load() }
