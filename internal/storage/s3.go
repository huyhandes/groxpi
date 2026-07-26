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
	PartSize       int64 // Multipart upload part size (default: 10MB)
	MaxConnections int   // Max concurrent connections (legacy - use specific pools below)
	ConnectTimeout time.Duration
	RequestTimeout time.Duration

	// Connection pool configuration
	ReadPoolSize  int  // Max connections for GET operations (default: 50)
	WritePoolSize int  // Max connections for PUT operations (default: 30)
	MetaPoolSize  int  // Max connections for HEAD/STAT operations (default: 20)
	EnableHTTP2   bool // Enable HTTP/2 for better multiplexing (default: true)
	TransferAccel bool // Enable S3 Transfer Acceleration (default: false)
}

// S3ConnectionPool manages HTTP connections for different types of S3 operations
type S3ConnectionPool struct {
	readTransport  *http.Transport // For GET operations
	writeTransport *http.Transport // For PUT operations
	metaTransport  *http.Transport // For HEAD/STAT operations
}

// NewS3ConnectionPool creates optimized HTTP transports for different operation types
func NewS3ConnectionPool(cfg *S3Config) *S3ConnectionPool {
	// Set defaults
	if cfg.ReadPoolSize == 0 {
		cfg.ReadPoolSize = 50
	}
	if cfg.WritePoolSize == 0 {
		cfg.WritePoolSize = 30
	}
	if cfg.MetaPoolSize == 0 {
		cfg.MetaPoolSize = 20
	}

	baseTransport := func(maxConns int) *http.Transport {
		return &http.Transport{
			MaxIdleConns:          maxConns,
			MaxIdleConnsPerHost:   maxConns,
			MaxConnsPerHost:       maxConns,
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

	return &S3ConnectionPool{
		readTransport:  baseTransport(cfg.ReadPoolSize),
		writeTransport: baseTransport(cfg.WritePoolSize),
		metaTransport:  baseTransport(cfg.MetaPoolSize),
	}
}

// Close closes all idle connections in the pools
func (pool *S3ConnectionPool) Close() {
	pool.readTransport.CloseIdleConnections()
	pool.writeTransport.CloseIdleConnections()
	pool.metaTransport.CloseIdleConnections()
}

// S3Storage implements Storage for S3-compatible backends.
//
// Its objects live across the network rather than on the local filesystem, so it
// deliberately does not implement ZeroCopyCapable: there is no local path to
// serve.
type S3Storage struct {
	readClient  *minio.Client // Client optimized for GET operations
	writeClient *minio.Client // Client optimized for PUT operations
	metaClient  *minio.Client // Client optimized for metadata operations
	bucket      string
	prefix      string
	partSize    int64
	connPool    *S3ConnectionPool

	// Singleflight groups for deduplicating concurrent operations
	statSF singleflight.Group // For Stat/Exists operations
	listSF singleflight.Group // For List operations
}

var _ Storage = (*S3Storage)(nil)

// NewS3Storage creates a new S3 storage backend
func NewS3Storage(cfg *S3Config) (*S3Storage, error) {
	// Set defaults
	if cfg.PartSize == 0 {
		cfg.PartSize = 10 * 1024 * 1024 // 10MB default
	}
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = 100
	}
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

	// Create connection pool for different operation types
	connPool := NewS3ConnectionPool(cfg)

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

	// Helper function to create MinIO client with specific transport
	createClient := func(transport *http.Transport, clientType string) (*minio.Client, error) {
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
			log.Error().Err(err).Str("client_type", clientType).Msg("Failed to create S3 client")
			return nil, fmt.Errorf("failed to create S3 %s client: %w", clientType, err)
		}

		return client, nil
	}

	// Create specialized clients for different operations
	readClient, err := createClient(connPool.readTransport, "read")
	if err != nil {
		return nil, err
	}

	writeClient, err := createClient(connPool.writeTransport, "write")
	if err != nil {
		return nil, err
	}

	metaClient, err := createClient(connPool.metaTransport, "metadata")
	if err != nil {
		return nil, err
	}

	// Ensure bucket exists using metadata client
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ConnectTimeout)
	defer cancel()

	exists, err := metaClient.BucketExists(ctx, cfg.Bucket)
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
		readClient:  readClient,
		writeClient: writeClient,
		metaClient:  metaClient,
		bucket:      cfg.Bucket,
		prefix:      strings.TrimSuffix(cfg.Prefix, "/"),
		partSize:    cfg.PartSize,
		connPool:    connPool,
	}

	log.Info().
		Str("endpoint", cfg.Endpoint).
		Str("bucket", cfg.Bucket).
		Str("prefix", cfg.Prefix).
		Int("read_pool_size", cfg.ReadPoolSize).
		Int("write_pool_size", cfg.WritePoolSize).
		Int("meta_pool_size", cfg.MetaPoolSize).
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

// calculateOptimalPartSize picks a multipart part size for fileSize: bigger
// parts for bigger files, then raised if necessary to stay under S3's limit of
// 10,000 parts. Every part size below is already well above S3's 5MB minimum,
// and the part-count floor only overtakes the table above roughly 1.28 TB, so in
// practice the size band decides.
func calculateOptimalPartSize(fileSize int64) int64 {
	const maxParts = 10000

	var partSize int64
	switch {
	case fileSize < 100*1024*1024: // < 100MB
		partSize = 10 * 1024 * 1024 // 10MB
	case fileSize < 1024*1024*1024: // < 1GB
		partSize = 32 * 1024 * 1024 // 32MB
	case fileSize < 10*1024*1024*1024: // < 10GB
		partSize = 64 * 1024 * 1024 // 64MB
	default: // >= 10GB
		partSize = 128 * 1024 * 1024 // 128MB
	}

	return max(partSize, fileSize/maxParts)
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

	// Get object using read-optimized client
	object, err := s.readClient.GetObject(ctx, s.bucket, fullKey, minio.GetObjectOptions{})
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

	// Use optimized multipart for large files
	if size > s.partSize {
		partSize := calculateOptimalPartSize(size)
		opts.PartSize = uint64(partSize)
		log.Debug().
			Int64("file_size", size).
			Int64("part_size", partSize).
			Msg("Using optimized multipart upload")
	}

	// reader is handed to minio-go as-is: PutObject already buffers a sized
	// reader itself, so staging the body here would only add a copy.
	start := time.Now()
	uploadInfo, err := s.writeClient.PutObject(ctx, s.bucket, fullKey, reader, size, opts)
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

	err := s.writeClient.RemoveObject(ctx, s.bucket, fullKey, minio.RemoveObjectOptions{})
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

	_, err := s.metaClient.StatObject(ctx, s.bucket, fullKey, minio.StatObjectOptions{})
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

	stat, err := s.metaClient.StatObject(ctx, s.bucket, fullKey, minio.StatObjectOptions{})
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

// List returns a list of objects matching the options with singleflight deduplication
func (s *S3Storage) List(ctx context.Context, opts ListOptions) ([]*ObjectInfo, error) {
	// Create cache key from list options
	listKey := fmt.Sprintf("list:%s:%d:%s", opts.Prefix, opts.MaxKeys, opts.StartAfter)

	// Use singleflight to deduplicate concurrent list requests
	result, err, _ := s.listSF.Do(listKey, func() (any, error) {
		return s.listInternal(ctx, opts)
	})

	if err != nil {
		return nil, err
	}

	return result.([]*ObjectInfo), nil
}

// listInternal performs the actual S3 List operation
func (s *S3Storage) listInternal(ctx context.Context, opts ListOptions) ([]*ObjectInfo, error) {
	prefix := s.buildKey(opts.Prefix)

	listOpts := minio.ListObjectsOptions{
		Prefix:     prefix,
		Recursive:  false,
		MaxKeys:    opts.MaxKeys,
		StartAfter: opts.StartAfter,
	}

	var objects []*ObjectInfo
	for object := range s.metaClient.ListObjects(ctx, s.bucket, listOpts) {
		if object.Err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", object.Err)
		}

		// Strip prefix from key
		key := strings.TrimPrefix(object.Key, s.prefix+"/")

		objects = append(objects, &ObjectInfo{
			Key:          key,
			Size:         object.Size,
			LastModified: object.LastModified,
			ETag:         object.ETag,
			ContentType:  object.ContentType,
		})
	}

	return objects, nil
}

// Close releases any resources held by the storage backend
func (s *S3Storage) Close() error {
	// Close all connection pools
	if s.connPool != nil {
		s.connPool.Close()
	}
	return nil
}
