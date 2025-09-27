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
	"golang.org/x/sync/semaphore"
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

	// Async write configuration
	AsyncWrites    bool // Enable async writes for non-blocking operations (default: true)
	AsyncWorkers   int  // Number of async write workers (default: 10)
	AsyncQueueSize int  // Size of async write queue (default: 1000)
}

// Adaptive buffer pools for different file sizes to optimize memory usage
var (
	// Small files (< 16KB) - 4KB buffers
	s3SmallBufferPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 4*1024) // 4KB buffers
			return &buf
		},
	}

	// Medium files (16KB - 256KB) - 16KB buffers
	s3MediumBufferPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 16*1024) // 16KB buffers
			return &buf
		},
	}

	// Large files (256KB - 4MB) - 64KB buffers
	s3LargeBufferPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 64*1024) // 64KB buffers
			return &buf
		},
	}

	// Huge files (> 4MB) - 256KB buffers
	s3HugeBufferPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 256*1024) // 256KB buffers
			return &buf
		},
	}
)

// getOptimalBufferPool returns the appropriate buffer pool based on file size
func getOptimalBufferPool(size int64) *sync.Pool {
	switch {
	case size < 16*1024: // < 16KB
		return &s3SmallBufferPool
	case size < 256*1024: // < 256KB
		return &s3MediumBufferPool
	case size < 4*1024*1024: // < 4MB
		return &s3LargeBufferPool
	default: // >= 4MB
		return &s3HugeBufferPool
	}
}

// getBufferSizeForPool returns the buffer size for a given pool
func getBufferSizeForPool(pool *sync.Pool) int {
	switch pool {
	case &s3SmallBufferPool:
		return 4 * 1024
	case &s3MediumBufferPool:
		return 16 * 1024
	case &s3LargeBufferPool:
		return 64 * 1024
	case &s3HugeBufferPool:
		return 256 * 1024
	default:
		return 64 * 1024 // fallback
	}
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
		transport := &http.Transport{
			MaxIdleConns:          maxConns,
			MaxIdleConnsPerHost:   maxConns,
			MaxConnsPerHost:       maxConns,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    true, // S3 handles compression
			ResponseHeaderTimeout: cfg.RequestTimeout,
			TLSHandshakeTimeout:   cfg.ConnectTimeout,
			ExpectContinueTimeout: 1 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   cfg.ConnectTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}

		// Enable HTTP/2 if configured
		if cfg.EnableHTTP2 {
			// Enable HTTP/2 support
			http2Transport := &http.Transport{
				MaxIdleConns:          maxConns,
				MaxIdleConnsPerHost:   maxConns,
				MaxConnsPerHost:       maxConns,
				IdleConnTimeout:       90 * time.Second,
				DisableCompression:    true,
				ResponseHeaderTimeout: cfg.RequestTimeout,
				TLSHandshakeTimeout:   cfg.ConnectTimeout,
				ExpectContinueTimeout: 1 * time.Second,
				ForceAttemptHTTP2:     true, // Force HTTP/2
				DialContext: (&net.Dialer{
					Timeout:   cfg.ConnectTimeout,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			}
			return http2Transport
		}

		return transport
	}

	return &S3ConnectionPool{
		readTransport:  baseTransport(cfg.ReadPoolSize),
		writeTransport: baseTransport(cfg.WritePoolSize),
		metaTransport:  baseTransport(cfg.MetaPoolSize),
	}
}

// GetReadTransport returns the transport optimized for GET operations
func (pool *S3ConnectionPool) GetReadTransport() *http.Transport {
	return pool.readTransport
}

// GetWriteTransport returns the transport optimized for PUT operations
func (pool *S3ConnectionPool) GetWriteTransport() *http.Transport {
	return pool.writeTransport
}

// GetMetaTransport returns the transport optimized for metadata operations
func (pool *S3ConnectionPool) GetMetaTransport() *http.Transport {
	return pool.metaTransport
}

// Close closes all idle connections in the pools
func (pool *S3ConnectionPool) Close() {
	pool.readTransport.CloseIdleConnections()
	pool.writeTransport.CloseIdleConnections()
	pool.metaTransport.CloseIdleConnections()
}

// AsyncWriteRequest represents a pending write operation
type AsyncWriteRequest struct {
	Key         string
	Reader      io.Reader
	Size        int64
	ContentType string
	ResultCh    chan AsyncWriteResult
	Context     context.Context
}

// AsyncWriteResult contains the result of an async write operation
type AsyncWriteResult struct {
	Info  *ObjectInfo
	Error error
}

// AsyncWriteQueue manages non-blocking S3 write operations
type AsyncWriteQueue struct {
	storage     *S3Storage
	queue       chan *AsyncWriteRequest
	semaphore   *semaphore.Weighted
	workerCount int
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

// NewAsyncWriteQueue creates a new async write queue
func NewAsyncWriteQueue(storage *S3Storage, queueSize, workerCount int) *AsyncWriteQueue {
	ctx, cancel := context.WithCancel(context.Background())

	awq := &AsyncWriteQueue{
		storage:     storage,
		queue:       make(chan *AsyncWriteRequest, queueSize),
		semaphore:   semaphore.NewWeighted(int64(workerCount)),
		workerCount: workerCount,
		ctx:         ctx,
		cancel:      cancel,
	}

	// Start worker goroutines
	for i := 0; i < workerCount; i++ {
		awq.wg.Add(1)
		go awq.worker(i)
	}

	log.Info().
		Int("workers", workerCount).
		Int("queue_size", queueSize).
		Msg("S3 async write queue initialized")

	return awq
}

// worker processes async write requests
func (awq *AsyncWriteQueue) worker(id int) {
	defer awq.wg.Done()

	log.Debug().Int("worker_id", id).Msg("S3 async write worker started")

	for {
		select {
		case <-awq.ctx.Done():
			log.Debug().Int("worker_id", id).Msg("S3 async write worker shutting down")
			return
		case req := <-awq.queue:
			// Acquire semaphore to limit concurrent operations
			if err := awq.semaphore.Acquire(req.Context, 1); err != nil {
				req.ResultCh <- AsyncWriteResult{Error: fmt.Errorf("failed to acquire semaphore: %w", err)}
				continue
			}

			// Perform the write operation
			start := time.Now()
			info, err := awq.storage.putInternal(req.Context, req.Key, req.Reader, req.Size, req.ContentType)
			duration := time.Since(start)

			// Release semaphore
			awq.semaphore.Release(1)

			// Send result
			result := AsyncWriteResult{Info: info, Error: err}
			select {
			case req.ResultCh <- result:
			case <-req.Context.Done():
				// Context cancelled, don't block
			}

			// Log async write completion
			if err != nil {
				log.Error().
					Err(err).
					Str("key", req.Key).
					Int64("size", req.Size).
					Dur("duration", duration).
					Int("worker_id", id).
					Msg("Async S3 write failed")
			} else {
				log.Debug().
					Str("key", req.Key).
					Int64("size", req.Size).
					Dur("duration", duration).
					Int("worker_id", id).
					Msg("Async S3 write completed")
			}
		}
	}
}

// SubmitWrite submits an async write request
func (awq *AsyncWriteQueue) SubmitWrite(ctx context.Context, key string, reader io.Reader, size int64, contentType string) <-chan AsyncWriteResult {
	resultCh := make(chan AsyncWriteResult, 1)

	req := &AsyncWriteRequest{
		Key:         key,
		Reader:      reader,
		Size:        size,
		ContentType: contentType,
		ResultCh:    resultCh,
		Context:     ctx,
	}

	select {
	case awq.queue <- req:
		// Successfully queued
	case <-ctx.Done():
		// Context cancelled
		resultCh <- AsyncWriteResult{Error: ctx.Err()}
	default:
		// Queue full, return error
		resultCh <- AsyncWriteResult{Error: fmt.Errorf("async write queue is full")}
	}

	return resultCh
}

// Close shuts down the async write queue
func (awq *AsyncWriteQueue) Close() error {
	awq.cancel()

	// Close the queue channel to signal workers to finish current work
	close(awq.queue)

	// Wait for all workers to finish
	awq.wg.Wait()

	log.Info().Msg("S3 async write queue shut down")
	return nil
}

// S3Storage implements Storage interface for S3-compatible backends
type S3Storage struct {
	readClient  *minio.Client // Client optimized for GET operations
	writeClient *minio.Client // Client optimized for PUT operations
	metaClient  *minio.Client // Client optimized for metadata operations
	bucket      string
	prefix      string
	partSize    int64
	connPool    *S3ConnectionPool

	// Async write queue for non-blocking operations
	asyncQueue  *AsyncWriteQueue
	asyncWrites bool

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

	// Set async write defaults
	if cfg.AsyncWorkers == 0 {
		cfg.AsyncWorkers = 10
	}
	if cfg.AsyncQueueSize == 0 {
		cfg.AsyncQueueSize = 1000
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
	readClient, err := createClient(connPool.GetReadTransport(), "read")
	if err != nil {
		return nil, err
	}

	writeClient, err := createClient(connPool.GetWriteTransport(), "write")
	if err != nil {
		return nil, err
	}

	metaClient, err := createClient(connPool.GetMetaTransport(), "metadata")
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
		asyncWrites: cfg.AsyncWrites,
	}

	// Initialize async write queue if enabled
	if cfg.AsyncWrites {
		storage.asyncQueue = NewAsyncWriteQueue(storage, cfg.AsyncQueueSize, cfg.AsyncWorkers)
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
		Bool("async_writes", cfg.AsyncWrites).
		Int("async_workers", cfg.AsyncWorkers).
		Int("async_queue_size", cfg.AsyncQueueSize).
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

// calculateOptimalPartSize calculates the optimal part size for multipart uploads based on file size
func (s *S3Storage) calculateOptimalPartSize(fileSize int64) int64 {
	// AWS S3 limits: min 5MB, max 5GB per part, max 10,000 parts total
	const (
		minPartSize = int64(5 * 1024 * 1024)        // 5MB minimum
		maxPartSize = int64(5 * 1024 * 1024 * 1024) // 5GB maximum (but we'll use smaller)
		maxParts    = 10000
	)

	// For very large files, calculate part size to stay under 10,000 parts
	calculatedPartSize := fileSize / maxParts
	if calculatedPartSize < minPartSize {
		calculatedPartSize = minPartSize
	}

	// Use larger parts for better throughput, but not too large
	// Scale part size based on file size:
	// - Files < 100MB: use 10MB parts (default)
	// - Files 100MB-1GB: use 32MB parts
	// - Files 1GB-10GB: use 64MB parts
	// - Files > 10GB: use 128MB parts
	var optimalPartSize int64

	switch {
	case fileSize < int64(100*1024*1024): // < 100MB
		optimalPartSize = int64(10 * 1024 * 1024) // 10MB
	case fileSize < int64(1*1024*1024*1024): // < 1GB
		optimalPartSize = int64(32 * 1024 * 1024) // 32MB
	case fileSize < int64(10*1024*1024*1024): // < 10GB
		optimalPartSize = int64(64 * 1024 * 1024) // 64MB
	default: // > 10GB
		optimalPartSize = int64(128 * 1024 * 1024) // 128MB
	}

	// Use the larger of calculated and optimal part size
	if calculatedPartSize > optimalPartSize {
		optimalPartSize = calculatedPartSize
	}

	// Cap at reasonable maximum for memory efficiency
	maxReasonablePartSize := int64(256 * 1024 * 1024) // 256MB
	if optimalPartSize > maxReasonablePartSize {
		optimalPartSize = maxReasonablePartSize
	}

	log.Debug().
		Int64("file_size", fileSize).
		Int64("calculated_part_size", calculatedPartSize).
		Int64("optimal_part_size", optimalPartSize).
		Msg("Calculated optimal multipart size")

	return optimalPartSize
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

	// Get object using read-optimized client
	object, err := s.readClient.GetObject(ctx, s.bucket, fullKey, minio.GetObjectOptions{})
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

	object, err := s.readClient.GetObject(ctx, s.bucket, fullKey, opts)
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

	// For small ranges, use appropriate buffer pool to reduce allocations
	if length > 0 && length <= 256*1024 {
		pool := getOptimalBufferPool(length)
		bufPtr := pool.Get().(*[]byte)
		buf := *bufPtr

		// Create a buffered reader that returns buffer to pool when closed
		bufferedReader := &s3BufferedReader{
			Reader: object,
			buffer: buf,
			bufPtr: bufPtr,
			pool:   pool,
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

// putInternal performs the actual S3 Put operation (used by both sync and async paths)
func (s *S3Storage) putInternal(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
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
		partSize := s.calculateOptimalPartSize(size)
		opts.PartSize = uint64(partSize)
		log.Debug().
			Int64("file_size", size).
			Int64("part_size", partSize).
			Msg("Using optimized multipart upload")
	}

	// For small files, use appropriate buffer pool for zero-copy optimization
	actualReader := reader
	if size > 0 && size <= 256*1024 {
		pool := getOptimalBufferPool(size)
		bufPtr := pool.Get().(*[]byte)
		defer pool.Put(bufPtr)
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
	uploadInfo, err := s.writeClient.PutObject(ctx, s.bucket, fullKey, actualReader, size, opts)
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

// Put stores an object in S3 with automatic sync/async selection based on configuration
func (s *S3Storage) Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	// For small files and async writes enabled, use async queue
	if s.asyncWrites && s.asyncQueue != nil && size <= 256*1024 { // <= 256KB
		// Read all data into memory for async processing
		pool := getOptimalBufferPool(size)
		bufPtr := pool.Get().(*[]byte)
		defer pool.Put(bufPtr)
		buf := *bufPtr

		// Ensure buffer is large enough
		if size > int64(len(buf)) {
			// Fall back to sync operation for oversized files
			return s.putInternal(ctx, key, reader, size, contentType)
		}

		// Read data into buffer
		data := buf[:size]
		n, err := io.ReadFull(reader, data)
		if err != nil && err != io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("failed to read data for async upload: %w", err)
		}
		data = data[:n]

		// Submit async write
		resultCh := s.asyncQueue.SubmitWrite(ctx, key, bytes.NewReader(data), int64(n), contentType)

		// Wait for result
		select {
		case result := <-resultCh:
			return result.Info, result.Error
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Use synchronous operation for large files or when async is disabled
	return s.putInternal(ctx, key, reader, size, contentType)
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

	uploadInfo, err := s.writeClient.PutObject(ctx, s.bucket, fullKey, reader, size, opts)
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

	_, err := s.metaClient.StatObject(ctx, s.bucket, fullKey, minio.StatObjectOptions{})
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

	stat, err := s.metaClient.StatObject(ctx, s.bucket, fullKey, minio.StatObjectOptions{})
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

// GetPresignedURL generates a presigned URL for direct download
func (s *S3Storage) GetPresignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	fullKey := s.buildKey(key)

	// Generate presigned URL using read client
	url, err := s.readClient.PresignedGetObject(ctx, s.bucket, fullKey, expiry, nil)
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
		optimalPartSize := s.calculateOptimalPartSize(size)
		return s.streamingMultipartPut(ctx, fullKey, reader, size, contentType, optimalPartSize)
	}

	// For smaller objects, use regular put with buffer optimization
	opts := minio.PutObjectOptions{
		ContentType: contentType,
	}

	// Use appropriately sized pooled buffer for streaming
	pool := getOptimalBufferPool(size)
	bufPtr := pool.Get().(*[]byte)
	bufReader := &bufferedReader{
		reader: reader,
		buffer: *bufPtr,
		bufPtr: bufPtr,
		pool:   pool,
	}
	defer func() {
		if err := bufReader.Close(); err != nil {
			// Log error but continue
			_ = err
		}
	}()

	start := time.Now()
	info, err := s.writeClient.PutObject(ctx, s.bucket, fullKey, bufReader, size, opts)
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
func (s *S3Storage) streamingMultipartPut(ctx context.Context, fullKey string, reader io.Reader, size int64, contentType string, partSize int64) (*ObjectInfo, error) {
	opts := minio.PutObjectOptions{
		ContentType: contentType,
		PartSize:    uint64(partSize),
	}

	log.Debug().
		Str("full_key", fullKey).
		Int64("size", size).
		Int64("part_size", partSize).
		Msg("Starting multipart upload with optimal part size")

	start := time.Now()
	info, err := s.writeClient.PutObject(ctx, s.bucket, fullKey, reader, size, opts)
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

	// Get object info first for metadata using metadata client
	objInfo, err := s.metaClient.StatObject(ctx, s.bucket, fullKey, minio.StatObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to stat object %s: %w", key, err)
	}

	// Get object stream using read client
	object, err := s.readClient.GetObject(ctx, s.bucket, fullKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get object %s: %w", key, err)
	}
	defer func() {
		if err := object.Close(); err != nil {
			// Log error but continue
			_ = err
		}
	}()

	// Use appropriately sized pooled buffer for optimized streaming
	pool := getOptimalBufferPool(objInfo.Size)
	copyBufPtr := pool.Get().(*[]byte)
	defer pool.Put(copyBufPtr)
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
	// Close async write queue first to ensure all pending writes complete
	if s.asyncQueue != nil {
		if err := s.asyncQueue.Close(); err != nil {
			log.Error().Err(err).Msg("Failed to close async write queue")
		}
	}

	// Close all connection pools
	if s.connPool != nil {
		s.connPool.Close()
	}
	return nil
}
