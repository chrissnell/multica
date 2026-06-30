package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var version = "dev"

func main() {
	cfg, cfgErr := LoadConfig()
	logger := newLogger(cfg)
	if cfgErr != nil {
		logger.Error("load config", "error", cfgErr)
		os.Exit(1)
	}
	if err := run(logger, cfg); err != nil {
		logger.Error("broker exited with error", "error", err)
		os.Exit(1)
	}
}

// newLogger builds the process logger from config. A nil cfg (LoadConfig
// failed before we could read the log knobs) falls back to info/text so the
// failure itself is still reported. Debug level also turns on source
// locations, since that's exactly when file:line is worth the extra width.
func newLogger(cfg *Config) *slog.Logger {
	level := slog.LevelInfo
	format := "text"
	if cfg != nil {
		level = cfg.LogLevel
		format = cfg.LogFormat
	}
	opts := &slog.HandlerOptions{Level: level, AddSource: level <= slog.LevelDebug}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

func run(logger *slog.Logger, cfg *Config) error {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}
	k, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("k8s client: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	identity := mustHostnameOrRandom()
	logger.Info("broker starting",
		"version", version,
		"claude_version", Constants.ClaudeVersion,
		"extracted_at", Constants.ExtractedAt,
		"identity", identity,
		"namespace", cfg.Namespace,
		"secret", cfg.SecretName,
		"access_token_secret", cfg.AccessTokenSecret,
		"lease", cfg.LeaseName,
		"refresh_pad", cfg.RefreshPad.String(),
		"refresh_interval", cfg.RefreshInterval.String(),
		"usage_interval", cfg.UsageInterval.String(),
		"log_level", cfg.LogLevel.String(),
		"log_format", cfg.LogFormat,
	)

	store := NewSecretStore(k, cfg.Namespace, cfg.SecretName, cfg.AccessTokenSecret)
	leader, err := NewLeaderState(k, cfg.Namespace, cfg.LeaseName, identity)
	if err != nil {
		return err
	}
	leader.OnStartedLeading = func() {
		logger.Info("became leader")
		leaderStateGauge.Set(1)
	}
	leader.OnStoppedLeading = func() {
		logger.Warn("lost leadership")
		leaderStateGauge.Set(0)
	}
	oauth := DefaultOAuthClient()
	oauth.Logger = logger
	refresher := NewRefresher(store, leader, oauth, cfg.RefreshPad)
	broker := NewBroker(refresher, store, logger)

	// Bootstrap order is load-bearing (see plan-review concern #2):
	//   1. Load cached state into the broker BEFORE leader election starts.
	//      Reload is leader-independent; it lets /access_token serve cached
	//      tokens immediately. If the Secret is missing, fail-closed and exit.
	if err := broker.Reload(ctx); err != nil {
		return fmt.Errorf("initial reload: %w (has the bootstrap procedure been run?)", err)
	}
	//   2. Start leader election in a goroutine. The elector calls
	//      OnStartedLeading once we win the lease. leader.Run loops on the
	//      elector — a transient kube-API failure that returns it doesn't
	//      permanently downgrade us, we re-enter election after a short pause.
	go leader.Run(ctx, logger)
	//   3. Start the refresh ticker. RefreshIfNeeded gates on IsLeader(), so
	//      every tick before election settles is a silent no-op. Once we
	//      become leader, the next tick refreshes if the cached token is
	//      within RefreshPad of expiry.
	go broker.RunRefreshLoop(ctx, cfg.RefreshInterval)
	//   4. Start the plan-usage poller. Leader-gated like refresh, so only
	//      one replica polls Anthropic's rate-limited usage endpoint. It
	//      serves the cached snapshot from the admin mux's /usage handler.
	usagePoller := NewUsagePoller(broker, leader, DefaultUsageClient(), cfg.UsageInterval)
	go usagePoller.Run(ctx)

	adminSrv := &http.Server{
		Addr:              cfg.AdminAddr,
		Handler:           withRequestLogging(logger, "admin", NewAdminMux(broker)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	opsSrv := &http.Server{
		Addr:              cfg.OpsAddr,
		Handler:           withRequestLogging(logger, "ops", NewOpsMux(broker)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	metricsSrv := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           NewMetricsMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := adminSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("admin server exited", "error", err)
		}
	}()
	go func() {
		if err := opsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("ops server exited", "error", err)
		}
	}()
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server exited", "error", err)
		}
	}()
	logger.Info("broker up", "admin", cfg.AdminAddr, "ops", cfg.OpsAddr, "metrics", cfg.MetricsAddr)

	<-ctx.Done()
	logger.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = adminSrv.Shutdown(sctx)
	_ = opsSrv.Shutdown(sctx)
	_ = metricsSrv.Shutdown(sctx)
	return nil
}

func mustHostnameOrRandom() string {
	if h, _ := os.Hostname(); h != "" {
		return h
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "broker-" + hex.EncodeToString(b[:])
}
