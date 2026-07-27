package storage

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/phuslu/log"
	"golang.org/x/sync/singleflight"
)

// S3Config holds S3 storage configuration
type S3Config struct {
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	Region          string
	Bucket          string
	Prefix          string
	UseSSL          bool
	ForcePathStyle  bool

	// Performance tuning
	ConnectTimeout time.Duration
	RequestTimeout time.Duration
	EnableHTTP2    bool // Enable HTTP/2 for better multiplexing (default: true)
	TransferAccel  bool // Enable S3 Transfer Acceleration (default: false)
}

// s3MaxConns bounds the single HTTP connection pool shared by every S3 operation.
const s3MaxConns = 100

// newS3Transport builds the one HTTP transport all S3 operations share.
func newS3Transport(cfg *S3Config) *http.Transport {
	return &http.Transport{
		MaxIdleConns:          s3MaxConns,
		MaxIdleConnsPerHost:   s3MaxConns,
		MaxConnsPerHost:       s3MaxConns,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true, // S3 handles compression
		ResponseHeaderTimeout: cfg.RequestTimeout,
		TLSHandshakeTimeout:   cfg.ConnectTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     cfg.EnableHTTP2,
		DialContext: (&net.Dialer{
			Timeout:   cfg.ConnectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
}

// S3Storage implements Storage for S3-compatible backends.
//
// Its objects live across the network rather than on the local filesystem, so it
// deliberately does not implement ZeroCopyCapable: there is no local path to
// serve.
type S3Storage struct {
	client    *minio.Client
	transport *http.Transport
	bucket    string
	prefix    string

	// statSF deduplicates concurrent metadata lookups
	statSF singleflight.Group
}

var _ Storage = (*S3Storage)(nil)

// NewS3Storage creates a new S3 storage backend
func NewS3Storage(cfg *S3Config) (*S3Storage, error) {
	// Set defaults
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 5 * time.Minute
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}

	// Normalize endpoint URL - remove protocol if present
	endpoint := cfg.Endpoint
	if after, ok := strings.CutPrefix(endpoint, "https://"); ok {
		endpoint = after
		cfg.UseSSL = true
	} else if after, ok := strings.CutPrefix(endpoint, "http://"); ok {
		endpoint = after
		cfg.UseSSL = false
	}

	log.Debug().
		Str("original_endpoint", cfg.Endpoint).
		Str("normalized_endpoint", endpoint).
		Str("bucket", cfg.Bucket).
		Str("region", cfg.Region).
		Bool("ssl", cfg.UseSSL).
		Msg("Creating S3 storage backend")

	transport := newS3Transport(cfg)

	// Handle S3 Transfer Acceleration
	s3Endpoint := endpoint
	if cfg.TransferAccel {
		// Use transfer acceleration endpoint if enabled
		if !strings.Contains(endpoint, "amazonaws.com") {
			log.Warn().Msg("Transfer acceleration only works with AWS S3, ignoring setting")
		} else {
			// Replace s3.region.amazonaws.com with s3-accelerate.amazonaws.com
			parts := strings.Split(endpoint, ".")
			if len(parts) >= 3 && parts[0] == "s3" {
				s3Endpoint = "s3-accelerate.amazonaws.com"
				log.Info().Str("endpoint", s3Endpoint).Msg("Using S3 Transfer Acceleration")
			}
		}
	}

	opts := &minio.Options{
		Creds:     credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:    cfg.UseSSL,
		Region:    cfg.Region,
		Transport: transport,
	}

	// Enable path-style addressing for MinIO
	if cfg.ForcePathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}

	client, err := minio.New(s3Endpoint, opts)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create S3 client")
		return nil, fmt.Errorf("failed to create S3 client: %w", err)
	}

	// Ensure bucket exists
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ConnectTimeout)
	defer cancel()

	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		log.Error().Err(err).Str("bucket", cfg.Bucket).Msg("Failed to check bucket existence")
		return nil, fmt.Errorf("failed to check bucket existence: %w", err)
	}
	if !exists {
		log.Error().Str("bucket", cfg.Bucket).Msg("Bucket does not exist")
		return nil, fmt.Errorf("bucket %s does not exist", cfg.Bucket)
	}

	// Create S3 storage instance
	storage := &S3Storage{
		client:    client,
		transport: transport,
		bucket:    cfg.Bucket,
		prefix:    strings.TrimSuffix(cfg.Prefix, "/"),
	}

	log.Info().
		Str("endpoint", cfg.Endpoint).
		Str("bucket", cfg.Bucket).
		Str("prefix", cfg.Prefix).
		Bool("http2_enabled", cfg.EnableHTTP2).
		Bool("transfer_accel", cfg.TransferAccel).
		Msg("S3 storage backend initialized successfully with performance optimizations")

	return storage, nil
}

// buildKey constructs the full S3 key with prefix
func (s *S3Storage) buildKey(key string) string {
	if s.prefix == "" {
		return key
	}
	return fmt.Sprintf("%s/%s", s.prefix, key)
}

// isNotFoundResponse reports whether err is MinIO's way of saying the object is
// simply absent, as opposed to any other failure (denied, throttled, network).
// Only this case may be reported as a miss.
func isNotFoundResponse(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound
}

// s3Error wraps a MinIO failure for key, folding a genuine absence into the
// shared ErrNotFound sentinel so callers can branch with errors.Is.
func s3Error(err error, key string) error {
	if isNotFoundResponse(err) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return fmt.Errorf("s3 operation on %s failed: %w", key, err)
}

// Get retrieves an object from S3.
//
// Reads are deliberately not deduplicated: an S3 reader can only be consumed
// once, so concurrent callers each need their own. Singleflight is applied to
// the metadata operations, where the result is shareable.
func (s *S3Storage) Get(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error) {
	fullKey := s.buildKey(key)

	log.Debug().Str("key", key).Str("full_key", fullKey).Msg("Getting object from S3")

	object, err := s.client.GetObject(ctx, s.bucket, fullKey, minio.GetObjectOptions{})
	if err != nil {
		log.Error().Err(err).Str("key", key).Msg("Failed to get object")
		return nil, nil, s3Error(err, key)
	}

	// Stat resolves the response headers without consuming the body, so the
	// metadata below is complete before the caller reads a single byte.
	stat, err := object.Stat()
	if err != nil {
		_ = object.Close()
		return nil, nil, s3Error(err, key)
	}

	info := &ObjectInfo{
		Key:          key,
		Size:         stat.Size,
		LastModified: stat.LastModified,
		ETag:         stat.ETag,
		ContentType:  stat.ContentType,
	}

	return object, info, nil
}

// Put stores an object in S3
func (s *S3Storage) Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	fullKey := s.buildKey(key)

	log.Debug().
		Str("key", key).
		Int64("size", size).
		Str("content_type", contentType).
		Msg("Storing object in S3")

	opts := minio.PutObjectOptions{
		ContentType: contentType,
	}

	// reader is handed to minio-go as-is: PutObject already buffers a sized
	// reader itself, so staging the body here would only add a copy.
	start := time.Now()
	uploadInfo, err := s.client.PutObject(ctx, s.bucket, fullKey, reader, size, opts)
	if err != nil {
		log.Error().Err(err).Str("key", key).Msg("Failed to put object")
		return nil, fmt.Errorf("failed to put object %s: %w", key, err)
	}

	duration := time.Since(start)
	log.Info().
		Str("key", key).
		Int64("size", uploadInfo.Size).
		Str("etag", uploadInfo.ETag).
		Dur("duration", duration).
		Float64("speed_mbps", float64(uploadInfo.Size)/duration.Seconds()/(1024*1024)).
		Msg("Object stored successfully")

	return &ObjectInfo{
		Key:         key,
		Size:        uploadInfo.Size,
		ETag:        uploadInfo.ETag,
		ContentType: contentType,
	}, nil
}

// Delete removes an object from S3
func (s *S3Storage) Delete(ctx context.Context, key string) error {
	fullKey := s.buildKey(key)

	log.Debug().Str("key", key).Msg("Deleting object from S3")

	err := s.client.RemoveObject(ctx, s.bucket, fullKey, minio.RemoveObjectOptions{})
	if err != nil {
		log.Error().Err(err).Str("key", key).Msg("Failed to delete object")
		return fmt.Errorf("failed to delete object %s: %w", key, err)
	}

	log.Debug().Str("key", key).Msg("Object deleted successfully")
	return nil
}

// Exists checks if an object exists in S3 with singleflight deduplication
func (s *S3Storage) Exists(ctx context.Context, key string) (bool, error) {
	// Use singleflight to deduplicate concurrent stat requests
	result, err, _ := s.statSF.Do("exists:"+key, func() (any, error) {
		return s.existsInternal(ctx, key)
	})

	if err != nil {
		return false, err
	}

	return result.(bool), nil
}

// existsInternal performs the actual S3 Exists operation
func (s *S3Storage) existsInternal(ctx context.Context, key string) (bool, error) {
	fullKey := s.buildKey(key)

	_, err := s.client.StatObject(ctx, s.bucket, fullKey, minio.StatObjectOptions{})
	if err != nil {
		// Absence is the answer, not a failure. Anything else is a failure and
		// must not be reported as "does not exist".
		if isNotFoundResponse(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check object existence %s: %w", key, err)
	}

	return true, nil
}

// Stat retrieves object metadata without downloading content with singleflight deduplication
func (s *S3Storage) Stat(ctx context.Context, key string) (*ObjectInfo, error) {
	// Use singleflight to deduplicate concurrent stat requests
	result, err, _ := s.statSF.Do("stat:"+key, func() (any, error) {
		return s.statInternal(ctx, key)
	})

	if err != nil {
		return nil, err
	}

	return result.(*ObjectInfo), nil
}

// statInternal performs the actual S3 Stat operation
func (s *S3Storage) statInternal(ctx context.Context, key string) (*ObjectInfo, error) {
	fullKey := s.buildKey(key)

	stat, err := s.client.StatObject(ctx, s.bucket, fullKey, minio.StatObjectOptions{})
	if err != nil {
		return nil, s3Error(err, key)
	}

	return &ObjectInfo{
		Key:          key,
		Size:         stat.Size,
		LastModified: stat.LastModified,
		ETag:         stat.ETag,
		ContentType:  stat.ContentType,
	}, nil
}

// Close releases any resources held by the storage backend
func (s *S3Storage) Close() error {
	s.transport.CloseIdleConnections()
	return nil
}
