package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWithRequestLogging_LogsAtDebug(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := withRequestLogging(logger, "admin", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hi"))
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/access_token", nil))

	if w.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418 (middleware must pass through)", w.Code)
	}
	out := buf.String()
	for _, want := range []string{"http request", "mux=admin", "method=GET", "path=/access_token", "status=418", "bytes=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q; got: %s", want, out)
		}
	}
}

func TestWithRequestLogging_SilentBelowDebug(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := withRequestLogging(logger, "ops", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/refresh", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log output at info level, got: %s", buf.String())
	}
}
