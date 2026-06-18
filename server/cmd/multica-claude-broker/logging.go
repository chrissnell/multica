package main

import (
	"log/slog"
	"net/http"
	"time"
)

// statusRecorder captures the response status and body size for access logging.
// http.ResponseWriter doesn't expose what the handler wrote back, so we wrap it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// withRequestLogging emits one debug-level access-log line per request: which
// mux handled it, method, path, response status, bytes, and wall-clock
// duration. It short-circuits when debug is disabled so the hot /access_token
// path pays nothing in production (info level); flip BROKER_LOG_LEVEL=debug to
// see exactly who is calling the broker and how often.
func withRequestLogging(logger *slog.Logger, mux string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !logger.Enabled(r.Context(), slog.LevelDebug) {
			h.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		logger.Debug("http request",
			"mux", mux,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration", time.Since(start).Truncate(time.Millisecond).String(),
			"remote", r.RemoteAddr,
		)
	})
}
