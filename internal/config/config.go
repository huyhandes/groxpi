package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Index configuration
	IndexURL       string
	IndexTTL       time.Duration
	ExtraIndexURLs []string
	ExtraIndexTTLs []time.Duration

	// Cache configuration
	CacheSize int64
	CacheDir  string

	// Storage configuration
	StorageType       string // "local" or "s3"
	S3Endpoint        string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3Region          string
	S3Bucket          string
	S3Prefix          string
	S3ForcePathStyle  bool
	S3UseSSL          bool
	S3PartSize        int64 // Multipart upload part size
	S3MaxConnections  int   // Max concurrent S3 connections

	// Timeout configuration
	DownloadTimeout time.Duration
	ConnectTimeout  time.Duration
	ReadTimeout     time.Duration

	// Server configuration
	Port      string
	LogLevel  string
	LogFormat string // console or json
	LogColor  bool   // enable color for console logs

	// SSL configuration
	DisableSSLVerification bool

	// Response configuration
	BinaryFileMimeType bool
}

func Load() *Config {
	cfg := &Config{
		IndexURL:               getEnv("GROXPI_INDEX_URL", "https://pypi.org/simple/"),
		IndexTTL:               getDurationEnv("GROXPI_INDEX_TTL", 30*time.Minute),
		CacheSize:              getIntEnv("GROXPI_CACHE_SIZE", 5*1024*1024*1024), // 5GB
		CacheDir:               getEnv("GROXPI_CACHE_DIR", ""),
		DownloadTimeout:        getFloatDurationEnv("GROXPI_DOWNLOAD_TIMEOUT", 900*time.Millisecond),
		Port:                   getEnv("PORT", "5000"),
		LogLevel:               getEnv("GROXPI_LOGGING_LEVEL", "INFO"),
		LogFormat:              getEnv("GROXPI_LOG_FORMAT", "console"),
		LogColor:               getBoolEnv("GROXPI_LOG_COLOR", true),
		DisableSSLVerification: getBoolEnv("GROXPI_DISABLE_INDEX_SSL_VERIFICATION", false),
		BinaryFileMimeType:     getBoolEnv("GROXPI_BINARY_FILE_MIME_TYPE", false),

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
		S3PartSize:        getIntEnv("GROXPI_S3_PART_SIZE", 10*1024*1024), // 10MB
		S3MaxConnections:  int(getIntEnv("GROXPI_S3_MAX_CONNECTIONS", 100)),
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
			if ttl, err := strconv.Atoi(ttlStr); err == nil {
				cfg.ExtraIndexTTLs[i] = time.Duration(ttl) * time.Second
			} else {
				cfg.ExtraIndexTTLs[i] = 3 * time.Minute // default
			}
		}
	} else {
		// Default TTL for extra indices
		cfg.ExtraIndexTTLs = make([]time.Duration, len(cfg.ExtraIndexURLs))
		for i := range cfg.ExtraIndexTTLs {
			cfg.ExtraIndexTTLs[i] = 3 * time.Minute
		}
	}

	// Parse timeout configurations
	if connectTimeout := getEnv("GROXPI_CONNECT_TIMEOUT", ""); connectTimeout != "" {
		cfg.ConnectTimeout = getFloatDurationEnv("GROXPI_CONNECT_TIMEOUT", 0)
	} else if cfg.ReadTimeout > 0 {
		cfg.ConnectTimeout = 3100 * time.Millisecond
	}

	if readTimeout := getEnv("GROXPI_READ_TIMEOUT", ""); readTimeout != "" {
		cfg.ReadTimeout = getFloatDurationEnv("GROXPI_READ_TIMEOUT", 0)
	} else if cfg.ConnectTimeout > 0 {
		cfg.ReadTimeout = 20 * time.Second
	}

	// Set default cache dir if not specified
	if cfg.CacheDir == "" {
		cfg.CacheDir = os.TempDir()
	}

	// Validate S3 configuration if S3 storage is selected
	if cfg.StorageType == "s3" {
		// Set S3 endpoint to AWS default if not specified
		if cfg.S3Endpoint == "" {
			cfg.S3Endpoint = "s3.amazonaws.com"
		}

		// Validate required S3 settings
		if cfg.S3Bucket == "" {
			panic("GROXPI_S3_BUCKET must be set when using S3 storage")
		}
		if cfg.S3AccessKeyID == "" || cfg.S3SecretAccessKey == "" {
			panic("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be set when using S3 storage")
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

func getIntEnv(key string, defaultValue int64) int64 {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.ParseInt(value, 10, 64); err == nil {
			return intVal
		}
	}
	return defaultValue
}

func getDurationEnv(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return time.Duration(intVal) * time.Second
		}
	}
	return defaultValue
}

func getFloatDurationEnv(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if floatVal, err := strconv.ParseFloat(value, 64); err == nil {
			return time.Duration(floatVal * float64(time.Second))
		}
	}
	return defaultValue
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
