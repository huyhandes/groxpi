package config

import (
	"os"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	// Save original environment
	originalEnv := make(map[string]string)
	envVars := []string{
		"GROXPI_INDEX_URL",
		"GROXPI_INDEX_TTL",
		"GROXPI_CACHE_SIZE",
		"GROXPI_CACHE_DIR",
		"GROXPI_DOWNLOAD_TIMEOUT",
		"PORT",
		"GROXPI_LOGGING_LEVEL",
		"GROXPI_DISABLE_INDEX_SSL_VERIFICATION",
		"GROXPI_BINARY_FILE_MIME_TYPE",
		"GROXPI_EXTRA_INDEX_URLS",
		"GROXPI_EXTRA_INDEX_TTLS",
		"GROXPI_CONNECT_TIMEOUT",
		"GROXPI_READ_TIMEOUT",
	}

	for _, env := range envVars {
		originalEnv[env] = os.Getenv(env)
		_ = os.Unsetenv(env)
	}

	// Restore environment after test
	defer func() {
		for _, env := range envVars {
			if val, ok := originalEnv[env]; ok && val != "" {
				_ = os.Setenv(env, val)
			} else {
				_ = os.Unsetenv(env)
			}
		}
	}()

	t.Run("default values", func(t *testing.T) {
		cfg := Load()

		if cfg.IndexURL != "https://pypi.org/simple/" {
			t.Errorf("Expected default IndexURL to be 'https://pypi.org/simple/', got %s", cfg.IndexURL)
		}

		if cfg.IndexTTL != 30*time.Minute {
			t.Errorf("Expected default IndexTTL to be 30m, got %v", cfg.IndexTTL)
		}

		if cfg.CacheSize != 5*1024*1024*1024 {
			t.Errorf("Expected default CacheSize to be 5GB, got %d", cfg.CacheSize)
		}

		if cfg.DownloadTimeout != 900*time.Millisecond {
			t.Errorf("Expected default DownloadTimeout to be 900ms, got %v", cfg.DownloadTimeout)
		}

		if cfg.Port != "5000" {
			t.Errorf("Expected default Port to be '5000', got %s", cfg.Port)
		}

		if cfg.LogLevel != "INFO" {
			t.Errorf("Expected default LogLevel to be 'INFO', got %s", cfg.LogLevel)
		}

		if cfg.DisableSSLVerification != false {
			t.Errorf("Expected default DisableSSLVerification to be false, got %v", cfg.DisableSSLVerification)
		}

		if cfg.BinaryFileMimeType != false {
			t.Errorf("Expected default BinaryFileMimeType to be false, got %v", cfg.BinaryFileMimeType)
		}
	})

	t.Run("custom environment variables", func(t *testing.T) {
		_ = os.Setenv("GROXPI_INDEX_URL", "https://test.pypi.org/simple/")
		_ = os.Setenv("GROXPI_INDEX_TTL", "600")
		_ = os.Setenv("GROXPI_CACHE_SIZE", "1073741824") // 1GB
		_ = os.Setenv("GROXPI_CACHE_DIR", "/custom/cache")
		_ = os.Setenv("GROXPI_DOWNLOAD_TIMEOUT", "2.5")
		_ = os.Setenv("PORT", "8080")
		_ = os.Setenv("GROXPI_LOGGING_LEVEL", "DEBUG")
		_ = os.Setenv("GROXPI_DISABLE_INDEX_SSL_VERIFICATION", "1")
		_ = os.Setenv("GROXPI_BINARY_FILE_MIME_TYPE", "1")

		cfg := Load()

		if cfg.IndexURL != "https://test.pypi.org/simple/" {
			t.Errorf("Expected IndexURL to be 'https://test.pypi.org/simple/', got %s", cfg.IndexURL)
		}

		if cfg.IndexTTL != 600*time.Second {
			t.Errorf("Expected IndexTTL to be 600s, got %v", cfg.IndexTTL)
		}

		if cfg.CacheSize != 1073741824 {
			t.Errorf("Expected CacheSize to be 1GB, got %d", cfg.CacheSize)
		}

		if cfg.CacheDir != "/custom/cache" {
			t.Errorf("Expected CacheDir to be '/custom/cache', got %s", cfg.CacheDir)
		}

		if cfg.DownloadTimeout != 2500*time.Millisecond {
			t.Errorf("Expected DownloadTimeout to be 2.5s, got %v", cfg.DownloadTimeout)
		}

		if cfg.Port != "8080" {
			t.Errorf("Expected Port to be '8080', got %s", cfg.Port)
		}

		if cfg.LogLevel != "DEBUG" {
			t.Errorf("Expected LogLevel to be 'DEBUG', got %s", cfg.LogLevel)
		}

		if cfg.DisableSSLVerification != true {
			t.Errorf("Expected DisableSSLVerification to be true, got %v", cfg.DisableSSLVerification)
		}

		if cfg.BinaryFileMimeType != true {
			t.Errorf("Expected BinaryFileMimeType to be true, got %v", cfg.BinaryFileMimeType)
		}
	})

	t.Run("extra indices configuration", func(t *testing.T) {
		_ = os.Setenv("GROXPI_EXTRA_INDEX_URLS", "https://extra1.example.com,https://extra2.example.com")
		_ = os.Setenv("GROXPI_EXTRA_INDEX_TTLS", "120,240")

		cfg := Load()

		expectedURLs := []string{"https://extra1.example.com", "https://extra2.example.com"}
		if len(cfg.ExtraIndexURLs) != len(expectedURLs) {
			t.Errorf("Expected %d extra index URLs, got %d", len(expectedURLs), len(cfg.ExtraIndexURLs))
		}

		for i, url := range expectedURLs {
			if i >= len(cfg.ExtraIndexURLs) || cfg.ExtraIndexURLs[i] != url {
				t.Errorf("Expected extra index URL[%d] to be %s, got %s", i, url, cfg.ExtraIndexURLs[i])
			}
		}

		expectedTTLs := []time.Duration{120 * time.Second, 240 * time.Second}
		if len(cfg.ExtraIndexTTLs) != len(expectedTTLs) {
			t.Errorf("Expected %d extra index TTLs, got %d", len(expectedTTLs), len(cfg.ExtraIndexTTLs))
		}

		for i, ttl := range expectedTTLs {
			if i >= len(cfg.ExtraIndexTTLs) || cfg.ExtraIndexTTLs[i] != ttl {
				t.Errorf("Expected extra index TTL[%d] to be %v, got %v", i, ttl, cfg.ExtraIndexTTLs[i])
			}
		}
	})

	t.Run("timeout configuration", func(t *testing.T) {
		_ = os.Setenv("GROXPI_CONNECT_TIMEOUT", "5.0")
		_ = os.Setenv("GROXPI_READ_TIMEOUT", "30.0")

		cfg := Load()

		if cfg.ConnectTimeout != 5*time.Second {
			t.Errorf("Expected ConnectTimeout to be 5s, got %v", cfg.ConnectTimeout)
		}

		if cfg.ReadTimeout != 30*time.Second {
			t.Errorf("Expected ReadTimeout to be 30s, got %v", cfg.ReadTimeout)
		}
	})
}

// GetEnv is not exported, skip these tests

// GetIntEnv is not exported, skip these tests

// GetBoolEnv is not exported, skip these tests

// SplitAndTrim is not exported, skip these tests
