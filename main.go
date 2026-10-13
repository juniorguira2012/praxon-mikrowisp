// mikrowisp-proxy is a lightweight HTTP proxy that fronts a Mikrowisp ISP
// management installation. It forwards every API call to the real
// Mikrowisp upstream and, for two specific endpoints (GetInvoices and
// GetClientsDetails), can filter rows out of the response based on
// per-customer "extra" fields configured by the operator in a TOML file.
//
// Why this exists
// ---------------
// Mikrowisp lets the operator define arbitrary custom fields per
// customer in their panel: SSN, plan_contratado, tipo_cliente, etc.
// Some operators want to keep specific customers out of their
// downstream billing pipeline (Axentra ERP) for legitimate business
// reasons documented elsewhere. This proxy is the place where that
// filtering happens, BEFORE the data ever reaches the downstream system.
//
// Running the proxy is the operator's choice and the operator's
// responsibility under DR tax law. Axentra ships the tool; the
// operator decides whether and how to use it.
//
// Running it
// ----------
//
//	mikrowisp-proxy -config /etc/mikrowisp-proxy/config.toml
//
// or via Docker (see Dockerfile + README).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/axentra/mikrowisp-proxy/internal/config"
	"github.com/axentra/mikrowisp-proxy/internal/proxy"
)

func main() {
	configPath := flag.String("config", "/etc/mikrowisp-proxy/config.toml", "path to TOML config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(1)
	}

	logger := buildLogger(cfg.Logging.Level, cfg.Logging.JSON)
	slog.SetDefault(logger)

	logger.Info("starting mikrowisp-proxy",
		slog.String("bind", cfg.Server.Bind),
		slog.String("upstream", cfg.Upstream.BaseURL),
		slog.Int("rules", len(cfg.Filter.Exclude)),
		slog.Int("cache_ttl_seconds", cfg.Filter.Cache.TTLSeconds),
	)
	if cfg.Upstream.InsecureSkipVerify {
		logger.Warn("upstream TLS verification is DISABLED — only safe on private networks")
	}
	if len(cfg.Filter.Exclude) == 0 {
		logger.Info("no exclusion rules configured; proxy is operating in pass-through mode")
	}

	upstream := proxy.NewUpstream(
		cfg.Upstream.BaseURL,
		cfg.UpstreamTimeout(),
		cfg.Upstream.InsecureSkipVerify,
	)
	filter := proxy.NewFilter(
		cfg.Filter.Exclude,
		upstream,
		cfg.CacheTTL(),
		cfg.Filter.Cache.MaxEntries,
	)
	handler := proxy.NewServer(upstream, filter, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Single-line OK response. Liveness only — does not probe upstream.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	// Catch-all: every other path goes through the proxy. Mikrowisp's
	// API surface is /api/v1/* but we don't enforce the prefix so any
	// future Mikrowisp endpoint additions (or proxy probes) just work.
	mux.Handle("/", handler)

	srv := &http.Server{
		Addr:         cfg.Server.Bind,
		Handler:      mux,
		ReadTimeout:  cfg.ReadTimeout(),
		WriteTimeout: cfg.WriteTimeout(),
	}

	// Run the server in a goroutine so we can listen for shutdown signals
	// in the main one. SIGTERM is what Docker sends on `docker stop`.
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", slog.String("addr", cfg.Server.Bind))
		errCh <- srv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("server failed", slog.String("err", err.Error()))
			os.Exit(1)
		}
	case sig := <-sigCh:
		logger.Info("shutdown signal received", slog.String("signal", sig.String()))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", slog.String("err", err.Error()))
			os.Exit(1)
		}
		logger.Info("shutdown complete")
	}
}

// buildLogger maps the config's level/json choices into a configured
// slog.Logger. Centralized here so the level parsing rules are testable
// and don't drift from one log call site to another.
func buildLogger(level string, jsonFmt bool) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if jsonFmt {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
