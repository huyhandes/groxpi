// This is an internal test package so that TestHealthCheck can exercise the
// real healthCheck: package main cannot be imported from an external test.
package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/logger"
	"github.com/huyhandes/groxpi/internal/server"
	"github.com/huyhandes/groxpi/internal/telemetry"
)

// TestStartupWithoutOTLPEndpoint is the regression guard for observability
// becoming a startup dependency: with no collector configured, telemetry setup
// installs nothing, the logger initialises, and the server serves a request.
func TestStartupWithoutOTLPEndpoint(t *testing.T) {
	for _, key := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_SERVICE_NAME"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	t.Setenv("GROXPI_CACHE_DIR", t.TempDir())
	t.Setenv("GROXPI_LOGGING_LEVEL", "ERROR")

	cfg := loadConfig(t)
	if cfg.OTLPEndpoint != "" {
		t.Fatalf("expected no OTLP endpoint, got %q", cfg.OTLPEndpoint)
	}

	shutdownTelemetry, err := telemetry.Setup(context.Background(), cfg.OTLPEndpoint != "", cfg.ServiceName)
	if err != nil {
		t.Fatalf("telemetry setup must not fail without a collector: %v", err)
	}
	t.Cleanup(func() { _ = shutdownTelemetry(context.Background()) })

	logger.Init(logger.LogConfig{Level: cfg.LogLevel, Format: cfg.LogFormat, Color: cfg.LogColor})

	srv, err := server.NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: srv.Router()}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	resp, err := http.Get("http://" + listener.Addr().String() + "/health")
	if err != nil {
		t.Fatalf("server did not serve a request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /health = %d, want 200", resp.StatusCode)
	}
}

// TestMainExitsOnInvalidConfig pins that bad configuration stops the process
// with a non-zero exit, both when serving and when probing. main calls
// os.Exit, so each case re-runs this test binary as the child process.
func TestMainExitsOnInvalidConfig(t *testing.T) {
	if os.Getenv("GROXPI_TEST_RUN_MAIN") == "1" {
		os.Args = append([]string{"groxpi"}, strings.Fields(os.Getenv("GROXPI_TEST_ARGS"))...)
		main()
		return
	}

	for _, tc := range []struct {
		name, args, wantStderr string
	}{
		{"serve", "", "Invalid configuration"},
		{"health check", "--health-check", "health check failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The deadline turns a main that starts serving instead of exiting into a failure, not a hang.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMainExitsOnInvalidConfig$")
			cmd.Env = append(os.Environ(),
				"GROXPI_TEST_RUN_MAIN=1",
				"GROXPI_TEST_ARGS="+tc.args,
				"GROXPI_STORAGE_TYPE=bogus",
				"GROXPI_CACHE_DIR="+t.TempDir(),
			)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			if ctx.Err() != nil {
				t.Fatalf("main did not exit on invalid configuration (stderr: %s)", stderr.String())
			}

			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
				t.Fatalf("want a non-zero exit, got %v (stderr: %s)", err, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr %q does not mention %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}

// loadConfig is config.Load for a test whose environment is valid.
func loadConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// TestHealthCheck covers the three states the container probe can observe: a
// serving server, a degraded one, and nothing listening.
func TestHealthCheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()

	if err := healthCheck(ok.URL); err != nil {
		t.Errorf("healthCheck against a 200 server: %v", err)
	}

	degraded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer degraded.Close()

	if err := healthCheck(degraded.URL); err == nil {
		t.Error("healthCheck against a 503 server: want error, got nil")
	}

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	if err := healthCheck(deadURL); err == nil {
		t.Error("healthCheck with nothing listening: want error, got nil")
	}
}
