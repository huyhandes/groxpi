package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
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
		cfg := mustLoad(t)

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

		cfg := mustLoad(t)

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
	})

	t.Run("extra indices configuration", func(t *testing.T) {
		_ = os.Setenv("GROXPI_EXTRA_INDEX_URLS", "https://extra1.example.com,https://extra2.example.com")
		_ = os.Setenv("GROXPI_EXTRA_INDEX_TTLS", "120,240")

		cfg := mustLoad(t)

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

		cfg := mustLoad(t)

		if cfg.ConnectTimeout != 5*time.Second {
			t.Errorf("Expected ConnectTimeout to be 5s, got %v", cfg.ConnectTimeout)
		}

		if cfg.ReadTimeout != 30*time.Second {
			t.Errorf("Expected ReadTimeout to be 30s, got %v", cfg.ReadTimeout)
		}
	})
}

// TestLoadS3WithoutStaticCredentials pins that S3 and hybrid storage load with
// no static access keys configured. The loader used to abort here, which made
// instance, task and web-identity roles unreachable.
func TestLoadS3WithoutStaticCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("GROXPI_S3_BUCKET", "groxpi-cache")

	for _, storageType := range []string{"s3", "hybrid"} {
		t.Run(storageType, func(t *testing.T) {
			t.Setenv("GROXPI_STORAGE_TYPE", storageType)

			cfg := mustLoad(t)

			if cfg.S3AccessKeyID != "" || cfg.S3SecretAccessKey != "" {
				t.Fatalf("expected no static credentials, got %q/%q", cfg.S3AccessKeyID, cfg.S3SecretAccessKey)
			}
			if cfg.S3Bucket != "groxpi-cache" {
				t.Errorf("expected bucket to survive, got %q", cfg.S3Bucket)
			}
		})
	}
}

// TestLoadS3LeavesEmptyEndpointEmpty pins that no endpoint means no endpoint:
// the AWS SDK then resolves the regional one itself. Forcing s3.amazonaws.com
// sent every request to us-east-1.
func TestLoadS3LeavesEmptyEndpointEmpty(t *testing.T) {
	t.Setenv("GROXPI_S3_BUCKET", "groxpi-cache")
	t.Setenv("GROXPI_S3_ENDPOINT", "")

	for _, storageType := range []string{"s3", "hybrid"} {
		t.Run(storageType, func(t *testing.T) {
			t.Setenv("GROXPI_STORAGE_TYPE", storageType)
			if got := mustLoad(t).S3Endpoint; got != "" {
				t.Fatalf("S3Endpoint = %q, want empty", got)
			}
		})
	}
}

// TestLoadS3RequiresBucket pins that the bucket requirement remains.
func TestLoadS3RequiresBucket(t *testing.T) {
	t.Setenv("GROXPI_STORAGE_TYPE", "s3")
	t.Setenv("GROXPI_S3_BUCKET", "")

	cfg, err := Load()
	if err == nil || !strings.Contains(err.Error(), "GROXPI_S3_BUCKET") {
		t.Fatalf("Load() = %v, %v; want an error naming GROXPI_S3_BUCKET", cfg, err)
	}
}

// mustLoad is Load for a test whose environment is valid.
func mustLoad(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestResolutionOrder(t *testing.T) {
	cfg := &Config{
		IndexURL:       "https://pypi.org/simple/",
		IndexTTL:       30 * time.Minute,
		ExtraIndexURLs: []string{"https://private.example.com/simple/", "https://other.example.com/simple/"},
		ExtraIndexTTLs: []time.Duration{time.Minute}, // second one omitted on purpose
	}

	got := cfg.ResolutionOrder()

	want := []Index{
		{URL: "https://private.example.com/simple/", TTL: time.Minute},
		{URL: "https://other.example.com/simple/", TTL: defaultExtraIndexTTL},
		{URL: "https://pypi.org/simple/", TTL: 30 * time.Minute},
	}
	if len(got) != len(want) {
		t.Fatalf("Expected %d indexes, got %d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Index[%d]: expected %+v, got %+v", i, want[i], got[i])
		}
	}
}

func TestResolutionOrder_PrimaryOnly(t *testing.T) {
	cfg := &Config{IndexURL: "https://pypi.org/simple/", IndexTTL: time.Hour}
	got := cfg.ResolutionOrder()
	if len(got) != 1 || got[0].URL != cfg.IndexURL {
		t.Fatalf("Expected the primary index alone, got %+v", got)
	}
}

// captureLogs collects everything logged through the default logger while fn
// runs, so a test can assert a misconfiguration was announced rather than
// swallowed.
func captureLogs(t *testing.T, fn func()) string {
	t.Helper()

	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)

	fn()

	return buf.String()
}

// TestDurationEnvAcceptsUnits pins that a duration written the Go way is
// honoured. "300s" used to fail the bare-integer parse and silently fall back to
// the default, which is how a 300 second download budget became 900ms.
func TestDurationEnvAcceptsUnits(t *testing.T) {
	cases := map[string]time.Duration{
		"300s":  300 * time.Second,
		"5m":    5 * time.Minute,
		"1h30m": 90 * time.Minute,
		"300":   300 * time.Second, // proxpi's bare-seconds spelling still works
		"2.5":   2500 * time.Millisecond,
	}

	for value, want := range cases {
		t.Run(value, func(t *testing.T) {
			t.Setenv("GROXPI_DOWNLOAD_TIMEOUT", value)
			if got := mustLoad(t).DownloadTimeout; got != want {
				t.Fatalf("GROXPI_DOWNLOAD_TIMEOUT=%s gave %v, want %v", value, got, want)
			}
		})
	}
}

// TestExtraIndexTTLs pins that GROXPI_EXTRA_INDEX_TTLS is positional: an empty
// or unparseable entry falls back to the default in its own slot instead of
// shifting later TTLs onto the wrong index, and each entry is read like any
// other duration (Go units, or bare seconds).
func TestExtraIndexTTLs(t *testing.T) {
	d := defaultExtraIndexTTL
	cases := map[string][]time.Duration{
		"60,,120":     {60 * time.Second, d, 120 * time.Second},
		"5m, 90s":     {5 * time.Minute, 90 * time.Second},
		"30,later,1h": {30 * time.Second, d, time.Hour},
		"-5,2.5":      {d, 2500 * time.Millisecond},
		",":           {d, d},
	}
	for value, want := range cases {
		t.Run(value, func(t *testing.T) {
			t.Setenv("GROXPI_EXTRA_INDEX_TTLS", value)
			if got := mustLoad(t).ExtraIndexTTLs; !slices.Equal(got, want) {
				t.Fatalf("GROXPI_EXTRA_INDEX_TTLS=%q gave %v, want %v", value, got, want)
			}
		})
	}
}

// TestDownloadIdleTimeout pins the stall budget for an upstream file body: 60s
// by default, and a value at or below zero means the default rather than "never
// time out", since a body that never times out hangs every request tailing it.
func TestDownloadIdleTimeout(t *testing.T) {
	cases := map[string]time.Duration{
		"":    60 * time.Second,
		"0":   60 * time.Second,
		"-5":  60 * time.Second,
		"2.5": 2500 * time.Millisecond,
		"90s": 90 * time.Second,
	}
	for value, want := range cases {
		t.Run("value="+value, func(t *testing.T) {
			t.Setenv("GROXPI_DOWNLOAD_IDLE_TIMEOUT", value)
			if got := mustLoad(t).DownloadIdleTimeout; got != want {
				t.Fatalf("GROXPI_DOWNLOAD_IDLE_TIMEOUT=%q gave %v, want %v", value, got, want)
			}
		})
	}
}

// TestMalformedEnvIsLoud pins that a value we cannot use is announced. Load
// cannot fail - a mistyped tuning knob must not stop the proxy - so the warning
// is the only thing standing between an operator and a setting that was never
// applied.
func TestMalformedEnvIsLoud(t *testing.T) {
	cases := []struct {
		key, value string
		read       func(*Config) any
		want       any
	}{
		{"GROXPI_INDEX_CACHE_SIZE", "512MB", func(c *Config) any { return c.IndexCacheSize }, int64(256 * 1024 * 1024)},
		{"GROXPI_CACHE_SIZE", "-1", func(c *Config) any { return c.CacheSize }, int64(5 * 1024 * 1024 * 1024)},
		{"GROXPI_DOWNLOAD_TIMEOUT", "later", func(c *Config) any { return c.DownloadTimeout }, 900 * time.Millisecond},
		// Bare seconds that do not fit a time.Duration are malformed, not a
		// wrapped or saturated budget.
		{"GROXPI_DOWNLOAD_TIMEOUT", "NaN", func(c *Config) any { return c.DownloadTimeout }, 900 * time.Millisecond},
		{"GROXPI_DOWNLOAD_TIMEOUT", "Inf", func(c *Config) any { return c.DownloadTimeout }, 900 * time.Millisecond},
		{"GROXPI_DOWNLOAD_TIMEOUT", "-Inf", func(c *Config) any { return c.DownloadTimeout }, 900 * time.Millisecond},
		{"GROXPI_DOWNLOAD_TIMEOUT", "1e300", func(c *Config) any { return c.DownloadTimeout }, 900 * time.Millisecond},
		// The boundary itself: these seconds times 1e9 round to 2^63, one past
		// the largest time.Duration.
		{"GROXPI_DOWNLOAD_TIMEOUT", "9223372036.854775807", func(c *Config) any { return c.DownloadTimeout }, 900 * time.Millisecond},
		{"GROXPI_INDEX_TTL", "-5m", func(c *Config) any { return c.IndexTTL }, 30 * time.Minute},
	}

	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)

			var cfg *Config
			logs := captureLogs(t, func() { cfg = mustLoad(t) })

			if got := tc.read(cfg); got != tc.want {
				t.Errorf("expected the default %v, got %v", tc.want, got)
			}
			if !strings.Contains(logs, tc.key) {
				t.Errorf("malformed %s was ignored silently; logs were:\n%s", tc.key, logs)
			}
		})
	}
}

// TestLoadRejectsUnknownStorageType pins that a mistyped storage type stops the
// proxy at startup, the same way a missing bucket does. It used to fall through
// to local storage silently, so an operator who wrote "minio" ran without their
// durable store and never knew.
func TestLoadRejectsUnknownStorageType(t *testing.T) {
	t.Setenv("GROXPI_STORAGE_TYPE", "minio")

	cfg, err := Load()
	if err == nil || !strings.Contains(err.Error(), "GROXPI_STORAGE_TYPE") {
		t.Fatalf("Load() = %v, %v; want an error naming GROXPI_STORAGE_TYPE", cfg, err)
	}
}

// TestLoadStorageTypeIsCaseInsensitive pins that the storage type is trimmed
// and lowercased before it is checked.
func TestLoadStorageTypeIsCaseInsensitive(t *testing.T) {
	t.Setenv("GROXPI_S3_BUCKET", "groxpi-cache")

	for value, want := range map[string]string{"S3": "s3", " Hybrid ": "hybrid", "LOCAL": "local"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("GROXPI_STORAGE_TYPE", value)
			if got := mustLoad(t).StorageType; got != want {
				t.Fatalf("GROXPI_STORAGE_TYPE=%q gave %q, want %q", value, got, want)
			}
		})
	}
}

// TestBoolEnvSpellings pins the accepted boolean spellings. Anything else keeps
// the default and is announced: "disable" used to read as true, which turned
// SSL on for an operator trying to turn it off.
func TestBoolEnvSpellings(t *testing.T) {
	const key = "GROXPI_TEST_BOOL"
	cases := []struct {
		value      string
		defaultVal bool
		want       bool
		warns      bool
	}{
		{"1", false, true, false},
		{"true", false, true, false},
		{"TRUE", false, true, false},
		{"yes", false, true, false},
		{"on", false, true, false},
		{"0", true, false, false},
		{"false", true, false, false},
		{"False", true, false, false},
		{"no", true, false, false},
		{"off", true, false, false},
		{"", true, true, false},
		{"disable", true, true, true},
		{"enabled", false, false, true},
	}

	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(key, tc.value)

			var got bool
			logs := captureLogs(t, func() { got = getBoolEnv(key, tc.defaultVal) })

			if got != tc.want {
				t.Errorf("%s=%q (default %v) gave %v, want %v", key, tc.value, tc.defaultVal, got, tc.want)
			}
			if warned := strings.Contains(logs, key); warned != tc.warns {
				t.Errorf("%s=%q: warned=%v, want %v; logs were:\n%s", key, tc.value, warned, tc.warns, logs)
			}
		})
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		raw, want string
	}{
		{"https://pypi.org/simple/", "https://pypi.org/simple/"},
		{"https://user:s3cr3t@private.example.com/simple/", "https://redacted@private.example.com/simple/"},
		{"https://token@private.example.com/simple/", "https://redacted@private.example.com/simple/"},
		{"://nonsense", "[unparseable-url]"},
	}
	for _, tt := range tests {
		if got := RedactURL(tt.raw); got != tt.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
	idx := Index{URL: "https://user:s3cr3t@private.example.com/simple/"}
	if got := idx.Redacted(); got != "https://redacted@private.example.com/simple/" {
		t.Errorf("Index.Redacted() = %q", got)
	}
}

func TestRedactErrorText(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"no url", errors.New("boom"), "boom"},
		{"plain url", errors.New("HTTP 404 from https://pypi.org/simple/x/"), "HTTP 404 from https://pypi.org/simple/x/"},
		{"password", errors.New("HTTP 404 from https://user:s3cr3t@h.example/x"), "HTTP 404 from https://redacted@h.example/x"},
		{"token as username", errors.New(`Get "https://tok3n@h.example/x": EOF`), `Get "https://redacted@h.example/x": EOF`},
		{"two urls", errors.New("a https://u:p@one/x b http://t@two/y"), "a https://redacted@one/x b http://redacted@two/y"},
		{"at sign in path", errors.New("https://h.example/a@b"), "https://h.example/a@b"},
		{"wrapped", fmt.Errorf("outer: %w", errors.New("https://u:s3cr3t@h/x")), "outer: https://redacted@h/x"},
	}
	for _, tt := range tests {
		if got := RedactErrorText(tt.err); got != tt.want {
			t.Errorf("%s: RedactErrorText = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestRedactURLError(t *testing.T) {
	if RedactURLError(nil) != nil {
		t.Error("RedactURLError(nil) should stay nil")
	}
	plain := errors.New("boom")
	if !errors.Is(RedactURLError(plain), plain) || RedactURLError(plain).Error() != "boom" {
		t.Error("an error without a *url.Error should pass through untouched")
	}

	for _, raw := range []string{"https://deploy:s3cr3t@h.example/x", "https://s3cr3t@h.example/x"} {
		// Redact before wrapping, as the callers do: fmt.Errorf renders its text
		// when it is created, so rewriting the *url.Error afterwards cannot reach it.
		uerr := &url.Error{Op: "Get", URL: raw, Err: errors.New("EOF")}
		got := fmt.Errorf("wrapped: %w", RedactURLError(uerr))
		if strings.Contains(got.Error(), "s3cr3t") {
			t.Errorf("secret survived: %v", got)
		}
		var found *url.Error
		if !errors.As(got, &found) || found.URL != "https://redacted@h.example/x" {
			t.Errorf("unwrapping should find the redacted *url.Error, got %v", found)
		}
	}

	// An unparseable URL is dropped whole: there is no telling where its
	// credentials end.
	perr := &url.Error{Op: "parse", URL: "https://u:s3cr3t@h.example:bad port/x", Err: errors.New(`invalid port ":bad port" after host`)}
	got := RedactURLError(perr)
	if strings.Contains(got.Error(), "s3cr3t") || !strings.Contains(got.Error(), "[unparseable-url]") {
		t.Errorf("unparseable URL not dropped: %v", got)
	}
}
