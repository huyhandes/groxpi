package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
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
	MaxConnections int   // Max concurrent connections
	ConnectTimeout time.Duration
	RequestTimeout time.Duration
}

// Buffer pools for zero-copy optimizations
var s3BufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 64*1024) // 64KB buffers for S3 streaming
		return &buf
	},
}

// S3Storage implements Storage interface for S3-compatible backends
type S3Storage struct {
	client    *minio.Client
	bucket    string
	prefix    string
	partSize  int64
	transport *http.Transport

	// Singleflight groups for deduplicating concurrent operations
	statSF singleflight.Group // For Stat/Exists operations
	listSF singleflight.Group // For List operations
}

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
	if strings.HasPrefix(endpoint, "https://") {
		endpoint = strings.TrimPrefix(endpoint, "https://")
		cfg.UseSSL = true
	} else if strings.HasPrefix(endpoint, "http://") {
		endpoint = strings.TrimPrefix(endpoint, "http://")
		cfg.UseSSL = false
	}

	log.Debug().
		Str("original_endpoint", cfg.Endpoint).
		Str("normalized_endpoint", endpoint).
		Str("bucket", cfg.Bucket).
		Str("region", cfg.Region).
		Bool("ssl", cfg.UseSSL).
		Msg("Creating S3 storage backend")

	// Configure HTTP transport for performance with extended timeouts
	transport := &http.Transport{
		MaxIdleConns:          cfg.MaxConnections,
		MaxIdleConnsPerHost:   cfg.MaxConnections,
		MaxConnsPerHost:       cfg.MaxConnections,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,               // S3 already handles compression
		ResponseHeaderTimeout: cfg.RequestTimeout, // 5 minutes for response headers
		TLSHandshakeTimeout:   cfg.ConnectTimeout, // TLS handshake timeout
		ExpectContinueTimeout: 1 * time.Second,    // 100-continue timeout
		DialContext: (&net.Dialer{
			Timeout:   cfg.ConnectTimeout, // Connection establishment timeout
			KeepAlive: 30 * time.Second,   // Keep-alive timeout
		}).DialContext,
	}

	// Initialize MinIO client
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

	client, err := minio.New(endpoint, opts)
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

	log.Info().
		Str("endpoint", cfg.Endpoint).
		Str("bucket", cfg.Bucket).
		Str("prefix", cfg.Prefix).
		Msg("S3 storage backend initialized successfully")

	return &S3Storage{
		client:    client,
		bucket:    cfg.Bucket,
		prefix:    strings.TrimSuffix(cfg.Prefix, "/"),
		partSize:  cfg.PartSize,
		transport: transport,
	}, nil
}

// buildKey constructs the full S3 key with prefix
func (s *S3Storage) buildKey(key string) string {
	if s.prefix == "" {
		return key
	}
	return fmt.Sprintf("%s/%s", s.prefix, key)
}

// Get retrieves an object from S3 with singleflight deduplication
func (s *S3Storage) Get(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error) {
	// For S3, we cannot safely share readers between goroutines since each reader
	// can only be read once. Instead of using singleflight for Get operations,
	// we'll get fresh readers for each request. Singleflight is still useful for
	// metadata operations like Stat and Exists.
	return s.getInternal(ctx, key)
}

// getInternal performs the actual S3 Get operation
func (s *S3Storage) getInternal(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error) {
	fullKey := s.buildKey(key)

	log.Debug().Str("key", key).Str("full_key", fullKey).Msg("Getting object from S3")

	// Get object
	object, err := s.client.GetObject(ctx, s.bucket, fullKey, minio.GetObjectOptions{})
	if err != nil {
		log.Error().Err(err).Str("key", key).Msg("Failed to get object")
		return nil, nil, fmt.Errorf("failed to get object %s: %w", key, err)
	}

	// Get object info
	stat, err := object.Stat()
	if err != nil {
		_ = object.Close()
		return nil, nil, fmt.Errorf("failed to stat object %s: %w", key, err)
	}

	info := &ObjectInfo{
		Key:          key,
		Size:         stat.Size,
		LastModified: stat.LastModified,
		ETag:         stat.ETag,
		ContentType:  stat.ContentType,
		Metadata:     stat.UserMetadata,
	}

	return object, info, nil
}

// GetRange retrieves a byte range from an object with zero-copy optimization
func (s *S3Storage) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, *ObjectInfo, error) {
	fullKey := s.buildKey(key)

	log.Debug().
		Str("key", key).
		Str("full_key", fullKey).
		Int64("offset", offset).
		Int64("length", length).
		Msg("Getting object range from S3")

	opts := minio.GetObjectOptions{}
	if offset >= 0 && length > 0 {
		// Set the range header for partial content
		_ = opts.SetRange(offset, offset+length-1)
		log.Debug().
			Int64("range_start", offset).
			Int64("range_end", offset+length-1).
			Msg("Setting range header for S3 request")
	}

	object, err := s.client.GetObject(ctx, s.bucket, fullKey, opts)
	if err != nil {
		log.Error().Err(err).Str("key", key).Msg("Failed to get object range from S3")
		return nil, nil, fmt.Errorf("failed to get object range %s: %w", key, err)
	}

	// For range requests, we need to get object info without consuming the reader
	// First get the full object info using a separate Stat call
	fullObjectInfo, err := s.Stat(ctx, key)
	if err != nil {
		_ = object.Close()
		log.Error().Err(err).Str("key", key).Msg("Failed to get object info for range request")
		return nil, nil, fmt.Errorf("failed to get object info for range %s: %w", key, err)
	}

	// Create object info for the range request
	info := &ObjectInfo{
		Key:          key,
		Size:         length, // Size is the requested range length
		LastModified: fullObjectInfo.LastModified,
		ETag:         fullObjectInfo.ETag,
		ContentType:  fullObjectInfo.ContentType,
		Metadata:     fullObjectInfo.Metadata,
	}

	log.Debug().
		Str("key", key).
		Int64("requested_length", length).
		Int64("object_size", fullObjectInfo.Size).
		Msg("S3 range request prepared")

	// For small ranges, use buffer pool to potentially reduce allocations
	if length > 0 && length <= 64*1024 {
		bufPtr := s3BufferPool.Get().(*[]byte)
		buf := *bufPtr

		// Create a buffered reader that returns buffer to pool when closed
		bufferedReader := &s3BufferedReader{
			Reader: object,
			buffer: buf,
			bufPtr: bufPtr,
			pool:   &s3BufferPool,
		}

		return bufferedReader, info, nil
	}

	return object, info, nil
}

// s3BufferedReader wraps an io.ReadCloser with a buffer pool for zero-copy optimization
type s3BufferedReader struct {
	io.Reader
	buffer []byte
	bufPtr *[]byte // pointer to buffer for proper pool management
	pool   *sync.Pool
}

// Close returns the buffer to the pool and closes the underlying reader
func (r *s3BufferedReader) Close() error {
	if r.pool != nil && r.bufPtr != nil {
		r.pool.Put(r.bufPtr)
		r.bufPtr = nil
		r.buffer = nil
		r.pool = nil
	}

	if closer, ok := r.Reader.(io.ReadCloser); ok {
		return closer.Close()
	}
	return nil
}

// Put stores an object in S3 with zero-copy optimization
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

	// Use multipart for large files
	if size > s.partSize {
		opts.PartSize = uint64(s.partSize)
	}

	// For small files, use buffer pool for potential zero-copy optimization
	actualReader := reader
	if size > 0 && size <= 64*1024 {
		bufPtr := s3BufferPool.Get().(*[]byte)
		defer s3BufferPool.Put(bufPtr)
		buf := *bufPtr

		if size <= int64(len(buf)) {
			// Read into pooled buffer for zero-copy optimization
			n, err := io.ReadFull(reader, buf[:size])
			if err != nil && err != io.ErrUnexpectedEOF {
				return nil, fmt.Errorf("failed to read data: %w", err)
			}
			actualReader = bytes.NewReader(buf[:n])
		}
	}

	start := time.Now()
	uploadInfo, err := s.client.PutObject(ctx, s.bucket, fullKey, actualReader, size, opts)
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

// PutMultipart uploads a large object using multipart upload with custom part size
func (s *S3Storage) PutMultipart(ctx context.Context, key string, reader io.Reader, size int64, contentType string, partSize int64) (*ObjectInfo, error) {
	fullKey := s.buildKey(key)

	if partSize == 0 {
		partSize = s.partSize
	}

	opts := minio.PutObjectOptions{
		ContentType: contentType,
		PartSize:    uint64(partSize),
	}

	uploadInfo, err := s.client.PutObject(ctx, s.bucket, fullKey, reader, size, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to put multipart object %s: %w", key, err)
	}

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
	result, err, _ := s.statSF.Do("exists:"+key, func() (interface{}, error) {
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
		errResponse := minio.ToErrorResponse(err)
		if errResponse.Code == "NoSuchKey" {
			return false, nil
		}
		return false, fmt.Errorf("failed to check object existence %s: %w", key, err)
	}

	return true, nil
}

// Stat retrieves object metadata without downloading content with singleflight deduplication
func (s *S3Storage) Stat(ctx context.Context, key string) (*ObjectInfo, error) {
	// Use singleflight to deduplicate concurrent stat requests
	result, err, _ := s.statSF.Do("stat:"+key, func() (interface{}, error) {
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
		return nil, fmt.Errorf("failed to stat object %s: %w", key, err)
	}

	return &ObjectInfo{
		Key:          key,
		Size:         stat.Size,
		LastModified: stat.LastModified,
		ETag:         stat.ETag,
		ContentType:  stat.ContentType,
		Metadata:     stat.UserMetadata,
	}, nil
}

// List returns a list of objects matching the options with singleflight deduplication
func (s *S3Storage) List(ctx context.Context, opts ListOptions) ([]*ObjectInfo, error) {
	// Create cache key from list options
	listKey := fmt.Sprintf("list:%s:%d:%s", opts.Prefix, opts.MaxKeys, opts.StartAfter)

	// Use singleflight to deduplicate concurrent list requests
	result, err, _ := s.listSF.Do(listKey, func() (interface{}, error) {
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
	for object := range s.client.ListObjects(ctx, s.bucket, listOpts) {
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

// GetPresignedURL generates a presigned URL for direct download
func (s *S3Storage) GetPresignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	fullKey := s.buildKey(key)

	// Generate presigned URL
	url, err := s.client.PresignedGetObject(ctx, s.bucket, fullKey, expiry, nil)
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL for %s: %w", key, err)
	}

	return url.String(), nil
}

// StreamingPut stores an object with streaming support and concurrent reads
func (s *S3Storage) StreamingPut(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	fullKey := s.buildKey(key)

	log.Debug().
		Str("key", key).
		Str("full_key", fullKey).
		Int64("size", size).
		Str("content_type", contentType).
		Msg("Streaming put to S3")

	// Use multipart upload for better streaming performance
	if size > s.partSize {
		return s.streamingMultipartPut(ctx, fullKey, reader, size, contentType)
	}

	// For smaller objects, use regular put with buffer optimization
	opts := minio.PutObjectOptions{
		ContentType: contentType,
	}

	// Use pooled buffer for streaming
	bufPtr := s3BufferPool.Get().(*[]byte)
	bufReader := &bufferedReader{
		reader: reader,
		buffer: *bufPtr,
		bufPtr: bufPtr,
		pool:   &s3BufferPool,
	}
	defer func() {
		if err := bufReader.Close(); err != nil {
			// Log error but continue
			_ = err
		}
	}()

	start := time.Now()
	info, err := s.client.PutObject(ctx, s.bucket, fullKey, bufReader, size, opts)
	duration := time.Since(start)

	if err != nil {
		log.Error().
			Err(err).
			Str("key", key).
			Int64("size", size).
			Dur("duration", duration).
			Msg("Failed to put object to S3")
		return nil, fmt.Errorf("failed to put object %s: %w", key, err)
	}

	log.Info().
		Str("key", key).
		Str("etag", info.ETag).
		Int64("size", info.Size).
		Dur("duration", duration).
		Float64("speed_mbps", float64(info.Size)/duration.Seconds()/(1024*1024)).
		Msg("Successfully put object to S3")

	return &ObjectInfo{
		Key:         key,
		Size:        info.Size,
		ETag:        info.ETag,
		ContentType: contentType,
	}, nil
}

// streamingMultipartPut uses multipart upload for large objects
func (s *S3Storage) streamingMultipartPut(ctx context.Context, fullKey string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	opts := minio.PutObjectOptions{
		ContentType: contentType,
		PartSize:    uint64(s.partSize),
	}

	start := time.Now()
	info, err := s.client.PutObject(ctx, s.bucket, fullKey, reader, size, opts)
	duration := time.Since(start)

	if err != nil {
		log.Error().
			Err(err).
			Str("full_key", fullKey).
			Int64("size", size).
			Dur("duration", duration).
			Msg("Failed multipart upload to S3")
		return nil, fmt.Errorf("failed multipart upload: %w", err)
	}

	log.Info().
		Str("full_key", fullKey).
		Str("etag", info.ETag).
		Int64("size", info.Size).
		Dur("duration", duration).
		Float64("speed_mbps", float64(info.Size)/duration.Seconds()/(1024*1024)).
		Msg("Successfully completed multipart upload to S3")

	return &ObjectInfo{
		Size:        info.Size,
		ETag:        info.ETag,
		ContentType: contentType,
	}, nil
}

// StreamingGet retrieves an object with streaming optimizations
func (s *S3Storage) StreamingGet(ctx context.Context, key string, writer io.Writer) (*ObjectInfo, error) {
	fullKey := s.buildKey(key)

	log.Debug().Str("key", key).Str("full_key", fullKey).Msg("Streaming get from S3")

	// Get object info first for metadata
	objInfo, err := s.client.StatObject(ctx, s.bucket, fullKey, minio.StatObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to stat object %s: %w", key, err)
	}

	// Get object stream
	object, err := s.client.GetObject(ctx, s.bucket, fullKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get object %s: %w", key, err)
	}
	defer func() {
		if err := object.Close(); err != nil {
			// Log error but continue
			_ = err
		}
	}()

	// Use pooled buffer for optimized streaming
	copyBufPtr := s3BufferPool.Get().(*[]byte)
	defer s3BufferPool.Put(copyBufPtr)
	copyBuf := *copyBufPtr

	start := time.Now()
	written, err := io.CopyBuffer(writer, object, copyBuf)
	duration := time.Since(start)

	if err != nil {
		log.Error().
			Err(err).
			Str("key", key).
			Int64("bytes_written", written).
			Dur("duration", duration).
			Msg("Failed to stream from S3")
		return nil, fmt.Errorf("failed to stream object %s: %w", key, err)
	}

	log.Debug().
		Str("key", key).
		Int64("bytes_streamed", written).
		Dur("duration", duration).
		Float64("speed_mbps", float64(written)/duration.Seconds()/(1024*1024)).
		Msg("Successfully streamed from S3")

	return &ObjectInfo{
		Key:          key,
		Size:         objInfo.Size,
		LastModified: objInfo.LastModified,
		ETag:         objInfo.ETag,
		ContentType:  objInfo.ContentType,
	}, nil
}

// GetFilePath returns empty path as S3 doesn't support local file paths
func (s *S3Storage) GetFilePath(ctx context.Context, key string) (string, error) {
	return "", fmt.Errorf("S3 storage doesn't support local file paths")
}

// SupportsZeroCopy indicates if the backend supports zero-copy operations
func (s *S3Storage) SupportsZeroCopy() bool {
	return false // S3 requires network transfer, no zero-copy possible
}

// bufferedReader wraps a reader with pooled buffer for streaming optimization
type bufferedReader struct {
	reader io.Reader
	buffer []byte
	bufPtr *[]byte // pointer to buffer for proper pool management
	pool   *sync.Pool
	closed bool
}

func (br *bufferedReader) Read(p []byte) (n int, err error) {
	return br.reader.Read(p)
}

func (br *bufferedReader) Close() error {
	if !br.closed && br.bufPtr != nil {
		br.pool.Put(br.bufPtr)
		br.bufPtr = nil
		br.buffer = nil
		br.closed = true
	}

	if closer, ok := br.reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// Close releases any resources held by the storage backend
func (s *S3Storage) Close() error {
	// Close idle connections
	if s.transport != nil {
		s.transport.CloseIdleConnections()
	}
	return nil
}
