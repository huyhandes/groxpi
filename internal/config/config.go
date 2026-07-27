package config

import (
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultExtraIndexTTL is the TTL an extra index gets when its position in
// GROXPI_EXTRA_INDEX_TTLS is missing or unparseable.
const defaultExtraIndexTTL = 3 * time.Minute

// Index is one configured upstream index: where to fetch from, and how long its
// answers stay cached. The TTL is per index so a fast-moving private index can be
// refreshed more often than PyPI.
type Index struct {
	URL string
	TTL time.Duration
}

// Redacted renders the index URL with any credentials removed. Every log field,
// error message and response body that names an index goes through this; the raw
// URL is never formatted directly, because a URL reaches a log through wrapped
// errors that never passed a logging call.
func (i Index) Redacted() string { return RedactURL(i.URL) }

// RedactURL replaces a URL's user-info with a fixed placeholder, leaving URLs
// without credentials untouched. An unparseable URL is dropped whole: we cannot
// tell where its credentials end.
//
// It lives here rather than beside the HTTP client because internal/pypi already
// imports this package, so a helper both sides can reach has to sit on the side
// without the dependency.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparseable-url]"
	}
	if u.User == nil {
		return raw
	}
	u.User = url.User("redacted")
	return u.String()
}

// ResolutionOrder returns the indexes to consult for a package: the extra
// indexes first in configured order, then the primary last. The first index that
// has the package wins and its file list is used whole — see
// docs/adr/0001-extras-first-index-resolution.md.
func (c *Config) ResolutionOrder() []Index {
	indexes := make([]Index, 0, len(c.ExtraIndexURLs)+1)
	for i, extra := range c.ExtraIndexURLs {
		ttl := defaultExtraIndexTTL
		if i < len(c.ExtraIndexTTLs) && c.ExtraIndexTTLs[i] > 0 {
			ttl = c.ExtraIndexTTLs[i]
		}
		indexes = append(indexes, Index{URL: extra, TTL: ttl})
	}
	return append(indexes, Index{URL: c.IndexURL, TTL: c.IndexTTL})
}

type Config struct {
	// Index configuration
	IndexURL       string
	IndexTTL       time.Duration
	ExtraIndexURLs []string
	ExtraIndexTTLs []time.Duration

	// Cache configuration
	CacheSize int64
	CacheDir  string
	// IndexCacheSize bounds the in-memory index cache across every stored
	// representation (parsed list, JSON body, gzipped body). The 256 MB default
	// is provisional: it cannot be tuned honestly until cache metrics exist.
	IndexCacheSize int64

	// Storage configuration
	StorageType       string // "local", "s3", or "hybrid"
	S3Endpoint        string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3Region          string
	S3Bucket          string
	S3Prefix          string
	S3ForcePathStyle  bool
	S3UseSSL          bool

	// Hybrid/Tiered storage configuration
	LocalCacheSize      int64         // Size limit for local L1 cache (hybrid mode only)
	LocalCacheDir       string        // Directory for local L1 cache (hybrid mode only)
	LocalCacheTTL       time.Duration // TTL for local L1 cache entries (0 = disabled)
	TieredSyncWorkers   int           // Number of workers for L1 population (default: 5)
	TieredSyncQueueSize int           // Size of tiered sync queue (default: 100)

	// S3 Performance Configuration
	S3EnableHTTP2 bool // Enable HTTP/2 for better multiplexing

	// Timeout configuration
	DownloadTimeout time.Duration
	ConnectTimeout  time.Duration
	ReadTimeout     time.Duration

	// Server configuration
	Port      string
	LogLevel  string
	LogFormat string // console or json
	LogColor  bool   // enable color for console logs

	// Observability configuration. Read from the standard OpenTelemetry
	// environment variables so a collector is configured the same way here as in
	// the rest of an operator's fleet. An empty endpoint leaves all three signals
	// inert: no providers, no connection attempt, no startup dependency.
	OTLPEndpoint string
	ServiceName  string

	// SSL configuration
	DisableSSLVerification bool

	// Administrative interface. The surface is off unless credentials are
	// configured: with none set the routes do not exist. AdminEnabled is the
	// operator asserting they want it, which makes missing credentials a startup
	// failure rather than an open panel.
	//
	// Basic authentication sends the credentials in cleartext, so a deployment
	// needs a TLS-terminating proxy in front.
	AdminEnabled  bool
	AdminUsername string
	AdminPassword string
}

// AdminConfigured reports whether the administrative surface should be mounted.
// Credentials alone are the switch; AdminEnabled only escalates their absence
// into a startup failure.
func (c *Config) AdminConfigured() bool {
	return c.AdminUsername != "" && c.AdminPassword != ""
}

func Load() *Config {
	cfg := &Config{
		IndexURL:               getEnv("GROXPI_INDEX_URL", "https://pypi.org/simple/"),
		IndexTTL:               getDurationEnv("GROXPI_INDEX_TTL", 30*time.Minute),
		CacheSize:              getIntEnv("GROXPI_CACHE_SIZE", 5*1024*1024*1024),    // 5GB
		IndexCacheSize:         getIntEnv("GROXPI_INDEX_CACHE_SIZE", 256*1024*1024), // 256MB, provisional
		CacheDir:               getEnv("GROXPI_CACHE_DIR", ""),
		DownloadTimeout:        getDurationEnv("GROXPI_DOWNLOAD_TIMEOUT", 900*time.Millisecond),
		Port:                   getEnv("PORT", "5000"),
		LogLevel:               getEnv("GROXPI_LOGGING_LEVEL", "INFO"),
		LogFormat:              getEnv("GROXPI_LOG_FORMAT", "console"),
		LogColor:               getBoolEnv("GROXPI_LOG_COLOR", true),
		OTLPEndpoint:           getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		ServiceName:            getEnv("OTEL_SERVICE_NAME", "groxpi"),
		DisableSSLVerification: getBoolEnv("GROXPI_DISABLE_INDEX_SSL_VERIFICATION", false),
		AdminEnabled:           getBoolEnv("GROXPI_ADMIN_ENABLED", false),
		AdminUsername:          getEnv("GROXPI_ADMIN_USERNAME", ""),
		AdminPassword:          getEnv("GROXPI_ADMIN_PASSWORD", ""),
		ConnectTimeout:         getDurationEnv("GROXPI_CONNECT_TIMEOUT", 0),
		ReadTimeout:            getDurationEnv("GROXPI_READ_TIMEOUT", 0),

		// Storage configuration
		StorageType:       getEnv("GROXPI_STORAGE_TYPE", "local"),
		S3Endpoint:        getEnv("AWS_ENDPOINT_URL", ""),
		S3AccessKeyID:     getEnv("AWS_ACCESS_KEY_ID", ""),
		S3SecretAccessKey: getEnv("AWS_SECRET_ACCESS_KEY", ""),
		S3Region:          getEnv("AWS_REGION", "us-east-1"),
		S3Bucket:          getEnv("GROXPI_S3_BUCKET", ""),
		S3Prefix:          getEnv("GROXPI_S3_PREFIX", "groxpi"),
		S3ForcePathStyle:  getBoolEnv("GROXPI_S3_FORCE_PATH_STYLE", false),
		S3UseSSL:          getBoolEnv("GROXPI_S3_USE_SSL", true),

		// S3 Performance Configuration
		S3EnableHTTP2: getBoolEnv("GROXPI_S3_ENABLE_HTTP2", true),

		// Hybrid/Tiered storage configuration
		LocalCacheSize:      getIntEnv("GROXPI_LOCAL_CACHE_SIZE", 10*1024*1024*1024), // 10GB default
		LocalCacheDir:       getEnv("GROXPI_LOCAL_CACHE_DIR", ""),
		LocalCacheTTL:       getDurationEnv("GROXPI_LOCAL_CACHE_TTL", 0), // 0 = disabled
		TieredSyncWorkers:   int(getIntEnv("GROXPI_TIERED_SYNC_WORKERS", 5)),
		TieredSyncQueueSize: int(getIntEnv("GROXPI_TIERED_SYNC_QUEUE_SIZE", 100)),
	}

	// Parse extra index URLs
	if extraURLs := getEnv("GROXPI_EXTRA_INDEX_URLS", ""); extraURLs != "" {
		cfg.ExtraIndexURLs = splitAndTrim(extraURLs, ",")
	}

	// Parse extra index TTLs
	if extraTTLs := getEnv("GROXPI_EXTRA_INDEX_TTLS", ""); extraTTLs != "" {
		ttlStrs := splitAndTrim(extraTTLs, ",")
		cfg.ExtraIndexTTLs = make([]time.Duration, len(ttlStrs))
		for i, ttlStr := range ttlStrs {
			ttl, err := strconv.Atoi(ttlStr)
			if err != nil || ttl < 0 {
				invalidEnv("GROXPI_EXTRA_INDEX_TTLS", ttlStr, defaultExtraIndexTTL)
				cfg.ExtraIndexTTLs[i] = defaultExtraIndexTTL
				continue
			}
			cfg.ExtraIndexTTLs[i] = time.Duration(ttl) * time.Second
		}
	} else {
		// Default TTL for extra indices
		cfg.ExtraIndexTTLs = make([]time.Duration, len(cfg.ExtraIndexURLs))
		for i := range cfg.ExtraIndexTTLs {
			cfg.ExtraIndexTTLs[i] = defaultExtraIndexTTL
		}
	}

	// Set default cache dir if not specified
	if cfg.CacheDir == "" {
		cfg.CacheDir = os.TempDir()
	}

	// Set default local cache dir for hybrid mode
	if cfg.LocalCacheDir == "" {
		cfg.LocalCacheDir = cfg.CacheDir
	}

	// Validate S3 configuration if S3 or hybrid storage is selected
	if cfg.StorageType == "s3" || cfg.StorageType == "hybrid" {
		// Set S3 endpoint to AWS default if not specified
		if cfg.S3Endpoint == "" {
			cfg.S3Endpoint = "s3.amazonaws.com"
		}

		// Only the bucket is required. Credentials are deliberately not
		// checked: with none configured the AWS default chain takes over, which
		// is how instance, task and web-identity roles work.
		if cfg.S3Bucket == "" {
			panic("GROXPI_S3_BUCKET must be set when using S3 or hybrid storage")
		}
	}

	return cfg
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// invalidEnv reports a setting that could not be used and names the value taken
// instead. Load has no error return - a mistyped cache size must not stop the
// proxy from starting - so the substitution has to be visible in the log, or an
// operator who wrote "512MB" never learns their tuning was ignored.
func invalidEnv(key, value string, fallback any) {
	slog.Warn("Ignoring malformed environment variable, using default instead",
		"variable", key,
		"value", value,
		"default", fallback)
}

func getIntEnv(key string, defaultValue int64) int64 {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}

	intVal, err := strconv.ParseInt(value, 10, 64)
	if err != nil || intVal < 0 {
		invalidEnv(key, value, defaultValue)
		return defaultValue
	}
	return intVal
}

// getDurationEnv reads a duration written either with a unit ("300s", "5m") or
// as a bare number of seconds ("300", "2.5"). Both spellings have to work: the
// bare-seconds form is proxpi's, and the unit form is what anyone reading Go
// durations elsewhere in the configuration will reach for.
func getDurationEnv(key string, defaultValue time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}

	d, err := time.ParseDuration(value)
	if err != nil {
		seconds, floatErr := strconv.ParseFloat(value, 64)
		if floatErr != nil {
			invalidEnv(key, value, defaultValue)
			return defaultValue
		}
		d = time.Duration(seconds * float64(time.Second))
	}
	if d < 0 {
		invalidEnv(key, value, defaultValue)
		return defaultValue
	}
	return d
}

func getBoolEnv(key string, defaultValue bool) bool {
	value := strings.ToLower(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	return value != "0" && value != "no" && value != "off" && value != "false"
}

func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
