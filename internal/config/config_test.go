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
		os.Unsetenv(env)
	}

	// Restore environment after test
	defer func() {
		for _, env := range envVars {
			if val, ok := originalEnv[env]; ok && val != "" {
				os.Setenv(env, val)
			} else {
				os.Unsetenv(env)
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
		os.Setenv("GROXPI_INDEX_URL", "https://test.pypi.org/simple/")
		os.Setenv("GROXPI_INDEX_TTL", "600")
		os.Setenv("GROXPI_CACHE_SIZE", "1073741824") // 1GB
		os.Setenv("GROXPI_CACHE_DIR", "/custom/cache")
		os.Setenv("GROXPI_DOWNLOAD_TIMEOUT", "2.5")
		os.Setenv("PORT", "8080")
		os.Setenv("GROXPI_LOGGING_LEVEL", "DEBUG")
		os.Setenv("GROXPI_DISABLE_INDEX_SSL_VERIFICATION", "1")
		os.Setenv("GROXPI_BINARY_FILE_MIME_TYPE", "1")

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
		os.Setenv("GROXPI_EXTRA_INDEX_URLS", "https://extra1.example.com,https://extra2.example.com")
		os.Setenv("GROXPI_EXTRA_INDEX_TTLS", "120,240")

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
		os.Setenv("GROXPI_CONNECT_TIMEOUT", "5.0")
		os.Setenv("GROXPI_READ_TIMEOUT", "30.0")

		cfg := Load()

		if cfg.ConnectTimeout != 5*time.Second {
			t.Errorf("Expected ConnectTimeout to be 5s, got %v", cfg.ConnectTimeout)
		}

		if cfg.ReadTimeout != 30*time.Second {
			t.Errorf("Expected ReadTimeout to be 30s, got %v", cfg.ReadTimeout)
		}
	})
}

func TestGetEnv(t *testing.T) {
	t.Run("existing environment variable", func(t *testing.T) {
		os.Setenv("TEST_VAR", "test_value")
		defer os.Unsetenv("TEST_VAR")

		result := getEnv("TEST_VAR", "default")
		if result != "test_value" {
			t.Errorf("Expected 'test_value', got %s", result)
		}
	})

	t.Run("non-existing environment variable", func(t *testing.T) {
		result := getEnv("NON_EXISTING_VAR", "default_value")
		if result != "default_value" {
			t.Errorf("Expected 'default_value', got %s", result)
		}
	})
}

func TestGetIntEnv(t *testing.T) {
	t.Run("valid integer", func(t *testing.T) {
		os.Setenv("TEST_INT", "42")
		defer os.Unsetenv("TEST_INT")

		result := getIntEnv("TEST_INT", 100)
		if result != 42 {
			t.Errorf("Expected 42, got %d", result)
		}
	})

	t.Run("invalid integer", func(t *testing.T) {
		os.Setenv("TEST_INT", "not_a_number")
		defer os.Unsetenv("TEST_INT")

		result := getIntEnv("TEST_INT", 100)
		if result != 100 {
			t.Errorf("Expected default value 100, got %d", result)
		}
	})

	t.Run("non-existing variable", func(t *testing.T) {
		result := getIntEnv("NON_EXISTING_INT", 200)
		if result != 200 {
			t.Errorf("Expected default value 200, got %d", result)
		}
	})
}

func TestGetBoolEnv(t *testing.T) {
	testCases := []struct {
		name     string
		value    string
		expected bool
	}{
		{"empty string", "", false},
		{"zero", "0", false},
		{"no", "no", false},
		{"off", "off", false},
		{"false", "false", false},
		{"FALSE", "FALSE", false},
		{"one", "1", true},
		{"yes", "yes", true},
		{"true", "true", true},
		{"TRUE", "TRUE", true},
		{"anything else", "random", true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			os.Setenv("TEST_BOOL", tc.value)
			defer os.Unsetenv("TEST_BOOL")

			result := getBoolEnv("TEST_BOOL", false)
			if result != tc.expected {
				t.Errorf("For value '%s', expected %v, got %v", tc.value, tc.expected, result)
			}
		})
	}
}

func TestSplitAndTrim(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		sep      string
		expected []string
	}{
		{"empty string", "", ",", []string{}},
		{"single item", "item1", ",", []string{"item1"}},
		{"multiple items", "item1,item2,item3", ",", []string{"item1", "item2", "item3"}},
		{"with spaces", " item1 , item2 , item3 ", ",", []string{"item1", "item2", "item3"}},
		{"with empty items", "item1,,item3", ",", []string{"item1", "item3"}},
		{"different separator", "item1;item2;item3", ";", []string{"item1", "item2", "item3"}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := splitAndTrim(tc.input, tc.sep)

			if len(result) != len(tc.expected) {
				t.Errorf("Expected %d items, got %d", len(tc.expected), len(result))
				return
			}

			for i, expected := range tc.expected {
				if result[i] != expected {
					t.Errorf("Expected item[%d] to be '%s', got '%s'", i, expected, result[i])
				}
			}
		})
	}
}
