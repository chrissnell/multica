package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestBroker(t *testing.T, initial *TokenState, isLeader bool, anthropicSrv string) (*Broker, *fake.Clientset) {
	b, k, _ := newTestBrokerWithLog(t, initial, isLeader, anthropicSrv)
	return b, k
}

// newTestBrokerWithLog is newTestBroker plus a captured log buffer, so tests
// can assert on the broker's structured output (e.g. refresh result lines).
func newTestBrokerWithLog(t *testing.T, initial *TokenState, isLeader bool, anthropicSrv string) (*Broker, *fake.Clientset, *bytes.Buffer) {
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
	oauth := newClientForTest(anthropicSrv, "client-id-x", "oauth-2025-04-20")
	refresher := NewRefresher(store, &stubLeader{leader: isLeader}, oauth, 5*time.Minute)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	b := NewBroker(refresher, store, logger)
	if err := b.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return b, k, &buf
}

func TestAdminMux_AccessTokenFresh_NoRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called for fresh token")
	}))
	defer srv.Close()
	state := &TokenState{AccessToken: "A", RefreshToken: "R", ExpiresAt: time.Now().Add(1 * time.Hour)}
	b, _ := newTestBroker(t, state, true, srv.URL)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/access_token", nil)
	NewAdminMux(b).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body)
	}
	if w.Body.String() != "A" {
		t.Errorf("body = %q, want A", w.Body.String())
	}
}

func TestAdminMux_AccessTokenStale_RefreshesAndServes(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "ROTATED",
			"refresh_token": "R2",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()
	state := &TokenState{AccessToken: "OLD", RefreshToken: "R", ExpiresAt: time.Now().Add(1 * time.Minute)}
	b, _ := newTestBroker(t, state, true, srv.URL)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/access_token", nil)
	NewAdminMux(b).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if w.Body.String() != "ROTATED" {
		t.Errorf("expected rotated token in body, got %q", w.Body.String())
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("upstream calls = %d, want 1", atomic.LoadInt32(&calls))
	}
}

func TestTickRefresh_LogsSuccessResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "ROTATED", "refresh_token": "R2", "expires_in": 3600,
		})
	}))
	defer srv.Close()
	state := &TokenState{AccessToken: "OLD", RefreshToken: "R", ExpiresAt: time.Now().Add(1 * time.Minute)}
	b, _, buf := newTestBrokerWithLog(t, state, true, srv.URL)

	w := httptest.NewRecorder()
	NewAdminMux(b).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/access_token", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	out := buf.String()
	for _, want := range []string{`msg="access_token refreshed"`, "result=success", "trigger=scheduled", "valid_for="} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q; got: %s", want, out)
		}
	}
}

func TestTickRefresh_LogsFailureResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest) // 4xx → permanent
	}))
	defer srv.Close()
	state := &TokenState{AccessToken: "OLD", RefreshToken: "R", ExpiresAt: time.Now().Add(1 * time.Minute)}
	b, _, buf := newTestBrokerWithLog(t, state, true, srv.URL)

	w := httptest.NewRecorder()
	NewAdminMux(b).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/access_token", nil))

	out := buf.String()
	for _, want := range []string{`msg="access_token refresh failed"`, "result=failed", "kind=permanent"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q; got: %s", want, out)
		}
	}
}

func TestOpsMux_RefreshLogsSuccessResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "FORCED", "refresh_token": "R2", "expires_in": 3600,
		})
	}))
	defer srv.Close()
	state := &TokenState{AccessToken: "FRESH", RefreshToken: "R", ExpiresAt: time.Now().Add(1 * time.Hour)}
	b, _, buf := newTestBrokerWithLog(t, state, true, srv.URL)

	w := httptest.NewRecorder()
	NewOpsMux(b).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/refresh", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body)
	}
	out := buf.String()
	for _, want := range []string{`msg="access_token refreshed"`, "result=success", "trigger=manual"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q; got: %s", want, out)
		}
	}
}

func TestAdminMux_AccessTokenExpiredAndNotLeader_503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("non-leader must not call upstream")
	}))
	defer srv.Close()
	state := &TokenState{AccessToken: "EXPIRED", RefreshToken: "R", ExpiresAt: time.Now().Add(-1 * time.Minute)}
	b, _ := newTestBroker(t, state, false, srv.URL)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/access_token", nil)
	NewAdminMux(b).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body=%s", w.Code, w.Body)
	}
}

func TestAdminMux_Readyz(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	state := &TokenState{AccessToken: "A", RefreshToken: "R", ExpiresAt: time.Now().Add(1 * time.Hour)}
	b, _ := newTestBroker(t, state, true, srv.URL)

	w := httptest.NewRecorder()
	NewAdminMux(b).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("ready status = %d", w.Code)
	}
}

func TestAdminMux_DoesNotExposeRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called via admin mux /refresh")
	}))
	defer srv.Close()
	state := &TokenState{AccessToken: "A", RefreshToken: "R", ExpiresAt: time.Now().Add(1 * time.Hour)}
	b, _ := newTestBroker(t, state, true, srv.URL)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/refresh", nil)
	NewAdminMux(b).ServeHTTP(w, r)
	// ServeMux returns 404 for unregistered paths.
	if w.Code != http.StatusNotFound {
		t.Errorf("/refresh on admin mux must 404 (loopback-only), got %d", w.Code)
	}
}

func TestOpsMux_RefreshForcesRefresh(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "FORCED",
			"refresh_token": "R2",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()
	// Even though state is fresh, /refresh forces a call.
	state := &TokenState{AccessToken: "FRESH", RefreshToken: "R", ExpiresAt: time.Now().Add(1 * time.Hour)}
	b, _ := newTestBroker(t, state, true, srv.URL)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/refresh", nil)
	NewOpsMux(b).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("upstream calls = %d, want 1", atomic.LoadInt32(&calls))
	}
	if !strings.Contains(w.Body.String(), "refreshed") {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestOpsMux_RefreshOnNonLeader_503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	state := &TokenState{AccessToken: "A", RefreshToken: "R", ExpiresAt: time.Now().Add(1 * time.Hour)}
	b, _ := newTestBroker(t, state, false, srv.URL)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/refresh", nil)
	NewOpsMux(b).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("non-leader /refresh = %d, want 503", w.Code)
	}
}

func TestOpsMux_GetRefreshIsMethodNotAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	state := &TokenState{AccessToken: "A", RefreshToken: "R", ExpiresAt: time.Now().Add(1 * time.Hour)}
	b, _ := newTestBroker(t, state, true, srv.URL)

	w := httptest.NewRecorder()
	NewOpsMux(b).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/refresh", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /refresh = %d, want 405", w.Code)
	}
}

// TestAdminMux_Healthz_FreshTokenOK covers the happy path: a non-expired
// cached token means /healthz returns 200, regardless of leader status.
func TestAdminMux_Healthz_FreshTokenOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	state := &TokenState{AccessToken: "A", RefreshToken: "R", ExpiresAt: time.Now().Add(1 * time.Hour)}
	b, _ := newTestBroker(t, state, true, srv.URL)

	w := httptest.NewRecorder()
	NewAdminMux(b).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("healthz with fresh token = %d, want 200", w.Code)
	}
}

// TestAdminMux_Healthz_ExpiredWithinGrace covers the recently-expired window:
// a few seconds past ExpiresAt is *not* a liveness failure because one missed
// tick is normal under load. Only sustained staleness past the grace fails.
func TestAdminMux_Healthz_ExpiredWithinGrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	state := &TokenState{AccessToken: "A", RefreshToken: "R", ExpiresAt: time.Now().Add(-30 * time.Second)}
	b, _ := newTestBroker(t, state, true, srv.URL)
	b.livenessStaleGrace = 5 * time.Minute

	w := httptest.NewRecorder()
	NewAdminMux(b).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("healthz within grace = %d, want 200 (one stale tick is not a restart signal)", w.Code)
	}
}

// TestAdminMux_Healthz_ExpiredBeyondGrace is the 2026-06-29 regression guard:
// when the cached token has been expired longer than livenessStaleGrace, the
// refresh loop has clearly stopped working — /healthz must return 503 so
// kubelet restarts us. Previously this returned 200 and the broker stayed up
// silently failing for hours.
func TestAdminMux_Healthz_ExpiredBeyondGrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	state := &TokenState{AccessToken: "STALE", RefreshToken: "R", ExpiresAt: time.Now().Add(-20 * time.Minute)}
	// Non-leader because that's the common stuck case: lost leadership, never
	// re-bid, never refreshed again.
	b, _ := newTestBroker(t, state, false, srv.URL)
	b.livenessStaleGrace = 10 * time.Minute

	w := httptest.NewRecorder()
	NewAdminMux(b).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("healthz with token expired beyond grace = %d, want 503", w.Code)
	}
}

// TestAdminMux_Healthz_NoCacheStillOK confirms liveness doesn't fail during
// startup: a brand-new pod whose Reload hasn't run yet must not crash-loop
// before it gets a chance to come up. (Readiness handles "not ready yet".)
func TestAdminMux_Healthz_NoCacheStillOK(t *testing.T) {
	// Manually wire a broker that hasn't been Reloaded.
	k := fake.NewSimpleClientset()
	store := NewSecretStore(k, "ns", "s", "ns-access-token")
	oauth := newClientForTest("http://unused", "client-id-x", "oauth-2025-04-20")
	refresher := NewRefresher(store, &stubLeader{leader: false}, oauth, 5*time.Minute)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	b := NewBroker(refresher, store, logger)

	w := httptest.NewRecorder()
	NewAdminMux(b).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("healthz before first Reload = %d, want 200 (startup, not liveness, owns this case)", w.Code)
	}
}

// readGauge mirrors readCounter for a prometheus.Gauge.
func readGauge(t *testing.T, g interface {
	Write(*dto.Metric) error
}) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("read gauge: %v", err)
	}
	if m.Gauge == nil || m.Gauge.Value == nil {
		t.Fatal("gauge has no value")
	}
	return *m.Gauge.Value
}

// TestTickRefresh_PermanentFailureIncrementsStreak guards the post-2026-06-29
// alerting hook: consecutive permanents (revoked refresh_token, invalid_grant)
// must increment a gauge that operators can alert on, and the
// last_permanent_failure_at timestamp must advance. A transient outage in the
// middle must NOT reset the streak, because that would hide the permanent
// signal behind any network blip.
func TestTickRefresh_PermanentFailureIncrementsStreak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest) // 4xx → permanent
	}))
	defer srv.Close()
	state := &TokenState{AccessToken: "OLD", RefreshToken: "R", ExpiresAt: time.Now().Add(1 * time.Minute)}
	b, _ := newTestBroker(t, state, true, srv.URL)

	permanentFailureStreak.Set(0)
	// Zero out the timestamp gauge: other tests in the suite may have left a
	// recent value, and our two ticks below land within the same Unix second
	// so a delta check wouldn't reliably register movement.
	lastPermanentFailureAt.Set(0)

	b.tickRefresh(context.Background())
	if got := readGauge(t, permanentFailureStreak); got != 1 {
		t.Errorf("streak after 1 permanent = %v, want 1", got)
	}
	b.tickRefresh(context.Background())
	if got := readGauge(t, permanentFailureStreak); got != 2 {
		t.Errorf("streak after 2 permanents = %v, want 2", got)
	}
	if endTs := readGauge(t, lastPermanentFailureAt); endTs <= 0 {
		t.Errorf("lastPermanentFailureAt not set after permanent failure: %v", endTs)
	}
}

// TestTickRefresh_SuccessClearsPermanentStreak proves that as soon as the
// operator reseeds a valid refresh_token, the very next successful rotation
// drops the streak back to 0 so the alert auto-resolves.
func TestTickRefresh_SuccessClearsPermanentStreak(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "ROTATED", "refresh_token": "R2", "expires_in": 3600,
		})
	}))
	defer srv.Close()
	state := &TokenState{AccessToken: "OLD", RefreshToken: "R", ExpiresAt: time.Now().Add(1 * time.Minute)}
	b, _ := newTestBroker(t, state, true, srv.URL)

	permanentFailureStreak.Set(0)
	b.tickRefresh(context.Background())
	b.tickRefresh(context.Background())
	if got := readGauge(t, permanentFailureStreak); got != 2 {
		t.Fatalf("precondition: want streak=2, got %v", got)
	}

	// "Operator reseeded a working refresh_token."
	fail.Store(false)
	b.tickRefresh(context.Background())
	if got := readGauge(t, permanentFailureStreak); got != 0 {
		t.Errorf("streak after success = %v, want 0 (alert must auto-resolve)", got)
	}
}

// TestTickRefresh_TransientDoesNotResetStreak guards the deliberate decision
// to NOT reset the streak on transient errors. A transient outage stacked on
// top of a revoked refresh_token must not hide the permanent signal — only a
// real success proves the refresh_token is good.
func TestTickRefresh_TransientDoesNotResetStreak(t *testing.T) {
	var mode atomic.Int32 // 0 = permanent (400), 1 = transient (502)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
		} else {
			http.Error(w, "bad gateway", http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	state := &TokenState{AccessToken: "OLD", RefreshToken: "R", ExpiresAt: time.Now().Add(1 * time.Minute)}
	b, _ := newTestBroker(t, state, true, srv.URL)

	permanentFailureStreak.Set(0)
	b.tickRefresh(context.Background()) // permanent
	if got := readGauge(t, permanentFailureStreak); got != 1 {
		t.Fatalf("precondition: want streak=1, got %v", got)
	}
	mode.Store(1)
	b.tickRefresh(context.Background()) // transient
	if got := readGauge(t, permanentFailureStreak); got != 1 {
		t.Errorf("streak after transient = %v, want 1 (transient must not reset; only success does)", got)
	}
}

func TestMetricsMux_HasMetricsEndpoint(t *testing.T) {
	w := httptest.NewRecorder()
	NewMetricsMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Errorf("metrics status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "multica_claude_broker_constants_info") {
		t.Errorf("metrics missing constants_info gauge; body excerpt: %.200s", w.Body.String())
	}
}
