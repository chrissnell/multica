package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"k8s.io/client-go/kubernetes/fake"
)

// readCounter returns the current value of a prometheus.Counter. We register
// metrics with promauto into the default registry, so tests that touch the
// same metric across runs see cumulative values — take a delta around the
// code under test rather than asserting an absolute value.
func readCounter(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if m.Counter == nil || m.Counter.Value == nil {
		t.Fatal("counter has no value")
	}
	return *m.Counter.Value
}

// errStubBuildFailure is the canned error our injected runOnce returns when
// simulating "rebuild failed" — the path Run hits when newElector itself
// errors, distinct from "elector ran and returned".
var errStubBuildFailure = errors.New("stub: build failed")

// We rely on client-go's leaderelection package being exhaustively tested
// upstream; the broker's contribution is just wiring. These tests confirm
// (a) construction succeeds with a fake clientset and (b) IsLeader starts
// false until OnStartedLeading fires. Full election loop semantics (lease
// acquire/renew/release) are upstream's job.
//
// The post-2026-06-29 resilience work adds re-election: when runOnce returns
// short of ctx cancellation, Run rebuilds and re-enters. Because the fake
// clientset doesn't drive lease lifecycle in a way that makes the real
// elector return promptly, we inject ls.runOnce directly to exercise the
// loop semantics — that's what's actually under test here, not the elector.

func TestNewLeaderState_Wireup(t *testing.T) {
	k := fake.NewSimpleClientset()
	ls, err := NewLeaderState(k, "multica", "lease-name", "pod-A")
	if err != nil {
		t.Fatalf("NewLeaderState: %v", err)
	}
	if ls.IsLeader() {
		t.Error("IsLeader() must be false before election begins")
	}
	if ls.runOnce == nil {
		t.Error("runOnce must be set after construction")
	}
}

func TestLeaderState_CallbacksAreOptional(t *testing.T) {
	k := fake.NewSimpleClientset()
	ls, err := NewLeaderState(k, "ns", "name", "id")
	if err != nil {
		t.Fatalf("NewLeaderState: %v", err)
	}
	// Without callbacks set, simulating an internal transition should not panic.
	ls.leader.Store(true)
	if !ls.IsLeader() {
		t.Error("leader transition not reflected")
	}
}

// TestRun_ReElectsAfterRunOnceReturns guards the fix for the 2026-06-29
// incident: when client-go's elector.Run returns because lease renewal failed
// against a transiently-down kube API, the broker must rebuild and re-enter
// election rather than sit there permanently downgraded. We inject runOnce
// that returns immediately; Run should call it repeatedly and bump
// reelectTotal once per re-entry.
func TestRun_ReElectsAfterRunOnceReturns(t *testing.T) {
	k := fake.NewSimpleClientset()
	ls, err := NewLeaderState(k, "ns", "name", "id")
	if err != nil {
		t.Fatalf("NewLeaderState: %v", err)
	}
	// Make the loop tight so the test runs fast.
	ls.reelectBackoff = 5 * time.Millisecond

	var calls int32
	ls.runOnce = func(ctx context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	}

	startTotal := readCounter(t, reelectTotal)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ls.Run(ctx, nil)
		close(done)
	}()

	// Let the loop iterate enough times to prove it's restarting.
	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}

	if got := atomic.LoadInt32(&calls); got < 3 {
		t.Errorf("runOnce invoked %d times; want at least 3 (post-2026-06-29 behavior is re-election, not one-shot)", got)
	}
	if delta := readCounter(t, reelectTotal) - startTotal; delta < 2 {
		t.Errorf("reelectTotal delta = %v, want >= 2 (re-entry counted per loop iteration past the first)", delta)
	}
}

// TestRun_RunOnceErrorBacksOffAndRetries covers the "rebuild errored" branch:
// runOnce returning a non-nil error must not kill the loop; it sleeps the
// backoff and retries. Without this, a transient construction failure would
// leave the pod stuck in Run permanently with no leader bid.
func TestRun_RunOnceErrorBacksOffAndRetries(t *testing.T) {
	k := fake.NewSimpleClientset()
	ls, err := NewLeaderState(k, "ns", "name", "id")
	if err != nil {
		t.Fatalf("NewLeaderState: %v", err)
	}
	ls.reelectBackoff = 5 * time.Millisecond

	var calls int32
	ls.runOnce = func(ctx context.Context) error {
		atomic.AddInt32(&calls, 1)
		return errStubBuildFailure
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ls.Run(ctx, nil)
		close(done)
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}

	if got := atomic.LoadInt32(&calls); got < 3 {
		t.Errorf("runOnce invoked %d times after returning errors; want at least 3 (errors must NOT kill the loop)", got)
	}
}

// TestRun_ExitsOnCtxCancel confirms the loop does the right thing when asked
// to stop: ctx cancel during the backoff sleep returns immediately, not
// after sleeping out the full reelectBackoff.
func TestRun_ExitsOnCtxCancel(t *testing.T) {
	k := fake.NewSimpleClientset()
	ls, err := NewLeaderState(k, "ns", "name", "id")
	if err != nil {
		t.Fatalf("NewLeaderState: %v", err)
	}
	ls.reelectBackoff = 30 * time.Second // Long enough that we'd notice if it slept it out.
	ls.runOnce = func(ctx context.Context) error {
		// Block on ctx so Run reaches the backoff after ctx cancel.
		<-ctx.Done()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ls.Run(ctx, nil)
		close(done)
	}()

	// Give Run a beat to enter the loop.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit promptly on ctx cancel")
	}
}
