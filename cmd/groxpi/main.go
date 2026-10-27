package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/logger"
	"github.com/huyhandes/groxpi/internal/server"
	"github.com/huyhandes/groxpi/internal/telemetry"
)

// healthCheck GETs baseURL's health endpoint and returns an error unless the
// server answers 200. The 2s timeout keeps the probe inside the Dockerfile's
// HEALTHCHECK --timeout=3s rather than hanging on an unresponsive server.
func healthCheck(baseURL string) error {
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(baseURL + "/health")
	if err != nil {
		return fmt.Errorf("health request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned status %d", resp.StatusCode)
	}
	return nil
}

func main() {
	// The health-check probe runs before anything else is constructed: no
	// telemetry, no logger, no server, no storage backend.
	cfg, cfgErr := config.Load()
	if len(os.Args) > 1 && os.Args[1] == "--health-check" {
		err := cfgErr
		if err == nil {
			err = healthCheck("http://127.0.0.1:" + cfg.Port)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "health check failed:", err)
			os.Exit(1)
		}
		return
	}

	// Bad configuration stops the process before anything is constructed. The
	// logger is not configured yet, so this goes to slog's default (stderr).
	if cfgErr != nil {
		logger.Fatal("Invalid configuration", "error", cfgErr)
	}

	// Install the OpenTelemetry providers before the logger, so that the log
	// bridge picks up a real provider when one is configured. With no endpoint
	// configured this installs nothing and cannot fail.
	shutdownTelemetry, err := telemetry.Setup(context.Background(), cfg.OTLPEndpoint != "", cfg.ServiceName)
	if err != nil {
		// A misconfigured exporter must not stop the proxy from serving.
		slog.Warn("Telemetry disabled: failed to set up OpenTelemetry", "error", err)
		shutdownTelemetry = func(context.Context) error { return nil }
	}

	// Initialize logger
	logger.Init(logger.LogConfig{
		Level:  cfg.LogLevel,
		Format: cfg.LogFormat,
		Color:  cfg.LogColor,
	})

	// fatal is logger.Fatal with the telemetry flushed first, under a short
	// deadline: os.Exit skips every defer, so without it the record saying why
	// the process died never reaches the collector.
	fatal := func(msg string, args ...any) {
		slog.Log(context.Background(), logger.LevelFatal, msg, args...)
		flushCtx, cancelFlush := context.WithTimeout(context.Background(), 2*time.Second)
		_ = shutdownTelemetry(flushCtx)
		cancelFlush()
		os.Exit(1)
	}

	// Log startup info
	slog.Info("🚀 Starting groxpi server",
		"version", config.Version,
		"log_level", cfg.LogLevel,
		"log_format", cfg.LogFormat,
		"otlp_endpoint", config.RedactURL(cfg.OTLPEndpoint))

	// Log configuration
	slog.Info("📋 Configuration loaded",
		"index_url", config.RedactURL(cfg.IndexURL),
		"index_ttl", cfg.IndexTTL,
		"port", cfg.Port)

	// The storage module logs its own "storage configured" line: it alone
	// knows which cache the mode really uses.
	// Create server
	srv, err := server.NewServer(cfg)
	if err != nil {
		fatal("Failed to construct server", "error", err)
	}
	router := srv.Router()

	// Create HTTP server
	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,  // slowloris guard; bodies are streamed so no ReadTimeout
		IdleTimeout:       120 * time.Second, // reap idle keep-alive connections
	}

	// Start server in goroutine
	go func() {
		slog.Info("🌐 HTTP server starting", "address", ":"+cfg.Port)

		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("Failed to start server", "error", err)
		}
	}()

	// Wait for interrupt signal
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	// Graceful shutdown
	slog.Warn("⚠️  Shutdown signal received")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	shutdown(ctx, httpServer, srv)

	// The telemetry flush gets its own budget: a slow drain consumes ctx, and a
	// flush on an expired context would drop the shutdown-path spans and logs.
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFlush()
	if err := shutdownTelemetry(flushCtx); err != nil {
		slog.Error("Failed to flush telemetry", "error", err)
	}

	slog.Info("✅ Server stopped gracefully")
}

// shutdown drains in-flight requests, then releases the server's resources.
// Both steps share one budget: ctx is the whole grace period, not a per-step
// allowance, so a slow request drain leaves less time for the backend rather
// than pushing the total past the container's stop grace.
func shutdown(ctx context.Context, httpServer *http.Server, srv *server.Server) {
	if err := httpServer.Shutdown(ctx); err != nil {
		slog.Error("Server forced to shutdown", "error", err)
	}

	if err := srv.CloseContext(ctx); err != nil {
		slog.Error("Failed to close storage backend", "error", err)
	}
}
