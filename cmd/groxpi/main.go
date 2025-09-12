package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/logger"
	"github.com/huyhandes/groxpi/internal/server"
	"github.com/phuslu/log"
)

func main() {
	// Load configuration
	cfg := config.Load()

	// Initialize logger
	logger.Init(logger.LogConfig{
		Level:  cfg.LogLevel,
		Format: cfg.LogFormat,
		Color:  cfg.LogColor,
	})

	// Log startup info
	log.Info().
		Str("version", "1.0.0").
		Str("storage_type", cfg.StorageType).
		Str("log_level", cfg.LogLevel).
		Str("log_format", cfg.LogFormat).
		Msg("🚀 Starting groxpi server")

	// Log configuration
	log.Info().
		Str("index_url", cfg.IndexURL).
		Int64("cache_size_bytes", cfg.CacheSize).
		Str("cache_size_human", formatBytes(cfg.CacheSize)).
		Dur("index_ttl", cfg.IndexTTL).
		Str("port", cfg.Port).
		Msg("📋 Configuration loaded")

	// Log storage configuration
	if cfg.StorageType == "s3" {
		log.Info().
			Str("endpoint", cfg.S3Endpoint).
			Str("bucket", cfg.S3Bucket).
			Str("prefix", cfg.S3Prefix).
			Str("region", cfg.S3Region).
			Bool("ssl", cfg.S3UseSSL).
			Msg("☁️  S3 storage configured")
	} else {
		log.Info().
			Str("cache_dir", cfg.CacheDir).
			Msg("💾 Local storage configured")
	}

	// Create server
	srv := server.New(cfg)
	app := srv.App()

	// Start server in goroutine
	go func() {
		log.Info().
			Str("address", ":"+cfg.Port).
			Msg("🌐 HTTP server starting")

		if err := app.Listen(":" + cfg.Port); err != nil {
			log.Fatal().Err(err).Msg("Failed to start server")
		}
	}()

	// Wait for interrupt signal
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	// Graceful shutdown
	log.Warn().Msg("⚠️  Shutdown signal received")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := app.ShutdownWithContext(ctx); err != nil {
		log.Error().Err(err).Msg("Server forced to shutdown")
	}

	log.Info().Msg("✅ Server stopped gracefully")
}

// formatBytes converts bytes to human readable format
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
