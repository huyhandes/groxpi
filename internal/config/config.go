package config

import (
	"errors"
	"log/slog"
	"math"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Version is groxpi's version: the startup log, the home page and every
// upstream User-Agent carry it. It is a constant, not read from build info.
const Version = "1.0.0"

// UserAgent is the User-Agent header on every upstream request.
const UserAgent = "groxpi/" + Version

// defaultExtraIndexTTL is the TTL an extra index gets when its position in
// GROXPI_EXTRA_INDEX_TTLS is missing or unparseable.
const defaultExtraIndexTTL = 3 * time.Minute

// DefaultDownloadIdleTimeout is GROXPI_DOWNLOAD_IDLE_TIMEOUT when unset or at
// or below zero.
const DefaultDownloadIdleTimeout = 60 * time.Second

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
// It lives here rather than beside an HTTP client because index, download and
// storage all import this package, so it is the one place every module that
// names a URL can reach.
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

// credentialedURLPattern matches the user-info component of an absolute URL: a
// scheme, then everything up to the "@" that is still inside the authority. The
// character class cannot cross a "/", so a path containing "@" is not matched.
var credentialedURLPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/?#\s"'@]+@`)

// RedactErrorText renders an error for a log line, an HTTP body or the admin
// page with any URL credentials stripped out of the whole message.
//
// Redacting the URL attribute beside the error is not enough: a package file's
// URL is resolved against its index's base URL, which carries that index's
// user-info, and both net/http's *url.Error and the downloader's own "HTTP 404
// from <url>" print that URL into the error string itself. So the message is
// rewritten rather than the field next to it.
func RedactErrorText(err error) string {
	if err == nil {
		return ""
	}
	return credentialedURLPattern.ReplaceAllString(err.Error(), "${1}redacted@")
}

// RedactURLError strips credentials out of the URL net/http embeds in its
// transport errors: *url.Error prints the URL it failed on, user-info and all, so
// returning one unredacted leaks a private index's password into any log or
// response that records the error.
func RedactURLError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		uerr.URL = RedactURL(uerr.URL)
	}
	return err
}

// ResolutionOrder returns the indexes to consult for a package: the extra
// indexes first in configured order, then the primary last. The first index that
// has the package wins and its file list is used whole — see
// docs/adr/0001-extras-first-index-resolution.md.
func (c *Config) ResolutionOrder() []Index {
	indexes := make([]Index, 0, len(c.ExtraIndexURLs)+1)
	for i, extra := range c.ExtraIndexURLs {
		ttl := defaultExtraIndexTTL
		if i < len(c.ExtraIndexTTLs) {
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

	// Hybrid storage configuration: the local cache in front of S3
	LocalCacheSize int64         // Size limit for local cache (hybrid mode only)
	LocalCacheDir  string        // Directory for local cache (hybrid mode only)
	LocalCacheTTL  time.Duration // TTL for local cache entries (0 = disabled)

	// S3 Performance Configuration
	S3EnableHTTP2 bool // Enable HTTP/2 for better multiplexing

	// Timeout configuration
	DownloadTimeout     time.Duration
	DownloadIdleTimeout time.Duration // longest gap between body bytes of an upstream file
	ConnectTimeout      time.Duration
	ReadTimeout         time.Duration

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

// Load reads the configuration from the environment. A malformed optional value
// falls back to its default with a warning; a setting the proxy cannot run
// without (storage type, bucket) is an error.
func Load() (*Config, error) {
	cfg := &Config{
		IndexURL:               getEnv("GROXPI_INDEX_URL", "https://pypi.org/simple/"),
		IndexTTL:               getDurationEnv("GROXPI_INDEX_TTL", 30*time.Minute),
		CacheSize:              getIntEnv("GROXPI_CACHE_SIZE", 5*1024*1024*1024),    // 5GB
		IndexCacheSize:         getIntEnv("GROXPI_INDEX_CACHE_SIZE", 256*1024*1024), // 256MB, provisional
		CacheDir:               getEnv("GROXPI_CACHE_DIR", ""),
		DownloadTimeout:        getDurationEnv("GROXPI_DOWNLOAD_TIMEOUT", 900*time.Millisecond),
		DownloadIdleTimeout:    getDurationEnv("GROXPI_DOWNLOAD_IDLE_TIMEOUT", DefaultDownloadIdleTimeout),
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
		StorageType:       strings.ToLower(strings.TrimSpace(getEnv("GROXPI_STORAGE_TYPE", "local"))),
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

		// Hybrid storage configuration: the local cache in front of S3
		LocalCacheSize: getIntEnv("GROXPI_LOCAL_CACHE_SIZE", 10*1024*1024*1024), // 10GB default
		LocalCacheDir:  getEnv("GROXPI_LOCAL_CACHE_DIR", ""),
		LocalCacheTTL:  getDurationEnv("GROXPI_LOCAL_CACHE_TTL", 0), // 0 = disabled
	}

	// Parse extra index URLs. Like the TTLs below the list is positional: an
	// empty item is skipped, but it still owns its TTL slot (dropped after the
	// TTLs are parsed), so "a,,b" with "60,,120" gives b 120.
	var emptyURLSlots []int
	if extraURLs := getEnv("GROXPI_EXTRA_INDEX_URLS", ""); extraURLs != "" {
		for i, item := range strings.Split(extraURLs, ",") {
			if item = strings.TrimSpace(item); item == "" {
				slog.Warn("Ignoring empty GROXPI_EXTRA_INDEX_URLS item; its GROXPI_EXTRA_INDEX_TTLS slot is skipped too",
					"variable", "GROXPI_EXTRA_INDEX_URLS", "value", extraURLs, "position", i)
				emptyURLSlots = append(emptyURLSlots, i)
				continue
			}
			cfg.ExtraIndexURLs = append(cfg.ExtraIndexURLs, item)
		}
	}

	// Parse extra index TTLs. Split without dropping empty entries: the list is
	// positional, so "60,,120" must leave the second index on the default rather
	// than hand it the third index's TTL.
	if extraTTLs := getEnv("GROXPI_EXTRA_INDEX_TTLS", ""); extraTTLs != "" {
		ttlStrs := strings.Split(extraTTLs, ",")
		cfg.ExtraIndexTTLs = make([]time.Duration, len(ttlStrs))
		for i, ttlStr := range ttlStrs {
			cfg.ExtraIndexTTLs[i] = defaultExtraIndexTTL
			if ttlStr = strings.TrimSpace(ttlStr); ttlStr != "" {
				cfg.ExtraIndexTTLs[i] = parseDuration("GROXPI_EXTRA_INDEX_TTLS", ttlStr, defaultExtraIndexTTL)
			}
		}
		// Drop the slots of skipped URLs, last first so earlier positions hold.
		for _, slot := range slices.Backward(emptyURLSlots) {
			if slot < len(cfg.ExtraIndexTTLs) {
				cfg.ExtraIndexTTLs = slices.Delete(cfg.ExtraIndexTTLs, slot, slot+1)
			}
		}
	} else {
		// Default TTL for extra indices
		cfg.ExtraIndexTTLs = make([]time.Duration, len(cfg.ExtraIndexURLs))
		for i := range cfg.ExtraIndexTTLs {
			cfg.ExtraIndexTTLs[i] = defaultExtraIndexTTL
		}
	}

	// A stalled body with no idle bound hangs every request tailing it, so zero
	// is the default, not "never".
	if cfg.DownloadIdleTimeout <= 0 {
		cfg.DownloadIdleTimeout = DefaultDownloadIdleTimeout
	}

	// Set default cache dir if not specified
	if cfg.CacheDir == "" {
		cfg.CacheDir = os.TempDir()
	}

	// Set default local cache dir for hybrid mode
	if cfg.LocalCacheDir == "" {
		cfg.LocalCacheDir = cfg.CacheDir
	}

	// An unknown storage type is fatal, like a missing bucket: falling back to
	// local would run without the durable store the operator asked for.
	switch cfg.StorageType {
	case "local", "s3", "hybrid":
	default:
		return nil, errors.New(`GROXPI_STORAGE_TYPE must be "local", "s3" or "hybrid", got "` + cfg.StorageType + `"`)
	}

	// Validate S3 configuration if S3 or hybrid storage is selected
	if cfg.StorageType == "s3" || cfg.StorageType == "hybrid" {
		// Only the bucket is required. Credentials are deliberately not
		// checked: with none configured the AWS default chain takes over, which
		// is how instance, task and web-identity roles work.
		if cfg.S3Bucket == "" {
			return nil, errors.New("GROXPI_S3_BUCKET must be set when using S3 or hybrid storage")
		}
	}

	return cfg, nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// invalidEnv reports a setting that could not be used and names the value taken
// instead. Load fails only on a setting the proxy cannot run without - a
// mistyped cache size must not stop the proxy from starting - so the
// substitution has to be visible in the log, or an operator who wrote "512MB"
// never learns their tuning was ignored.
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

	return parseDuration(key, value, defaultValue)
}

// parseDuration reads a Go duration ("5m", "90s") or, as proxpi did, a bare
// number of seconds. A negative or unparseable value is announced and replaced
// by defaultValue.
func parseDuration(key, value string, defaultValue time.Duration) time.Duration {
	d, err := time.ParseDuration(value)
	if err != nil {
		seconds, floatErr := strconv.ParseFloat(value, 64)
		nanos := seconds * float64(time.Second)
		// NaN, ±Inf and anything past ~292 years do not fit a time.Duration;
		// converting them would wrap or be implementation-defined.
		// float64(math.MaxInt64) is exactly 2^63, the first value out of range.
		if floatErr != nil || math.IsNaN(nanos) || math.Abs(nanos) >= float64(math.MaxInt64) {
			invalidEnv(key, value, defaultValue)
			return defaultValue
		}
		d = time.Duration(nanos)
	}
	if d < 0 {
		invalidEnv(key, value, defaultValue)
		return defaultValue
	}
	return d
}

// getBoolEnv reads a boolean from a fixed set of spellings. Anything else keeps
// the default and is announced: reading every unknown value as true turned
// GROXPI_S3_USE_SSL=disable into SSL on.
func getBoolEnv(key string, defaultValue bool) bool {
	value := os.Getenv(key)
	switch strings.ToLower(value) {
	case "":
		return defaultValue
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		invalidEnv(key, value, defaultValue)
		return defaultValue
	}
}
