package storage

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/phuslu/log"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
)

// TieredStorage implements a multi-tier caching system with local (L1) and S3 (L2) storage
type TieredStorage struct {
	localCache    StreamingStorage // L1 cache - fast local storage
	remoteStorage StreamingStorage // L2 cache - persistent S3 storage
	syncQueue     *TieredSyncQueue // Async queue for L1 cache population
	sf            singleflight.Group
}

// TieredSyncRequest represents a pending L1 cache population request
type TieredSyncRequest struct {
	Key      string
	Context  context.Context
	ResultCh chan error
}

// TieredSyncQueue manages async L1 cache population from L2
type TieredSyncQueue struct {
	storage     *TieredStorage
	queue       chan *TieredSyncRequest
	semaphore   *semaphore.Weighted
	workerCount int
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

// NewTieredSyncQueue creates a new tiered sync queue
func NewTieredSyncQueue(storage *TieredStorage, queueSize, workerCount int) *TieredSyncQueue {
	ctx, cancel := context.WithCancel(context.Background())

	tsq := &TieredSyncQueue{
		storage:     storage,
		queue:       make(chan *TieredSyncRequest, queueSize),
		semaphore:   semaphore.NewWeighted(int64(workerCount)),
		workerCount: workerCount,
		ctx:         ctx,
		cancel:      cancel,
	}

	// Start worker goroutines
	for i := 0; i < workerCount; i++ {
		tsq.wg.Add(1)
		go tsq.worker(i)
	}

	log.Info().
		Int("workers", workerCount).
		Int("queue_size", queueSize).
		Msg("Tiered storage sync queue initialized")

	return tsq
}

// worker processes L1 cache population requests
func (tsq *TieredSyncQueue) worker(id int) {
	defer tsq.wg.Done()

	log.Debug().Int("worker_id", id).Msg("Tiered sync worker started")

	for {
		select {
		case <-tsq.ctx.Done():
			log.Debug().Int("worker_id", id).Msg("Tiered sync worker shutting down")
			return
		case req := <-tsq.queue:
			// Acquire semaphore to limit concurrent operations
			if err := tsq.semaphore.Acquire(req.Context, 1); err != nil {
				req.ResultCh <- fmt.Errorf("failed to acquire semaphore: %w", err)
				continue
			}

			// Copy from L2 (S3) to L1 (local)
			start := time.Now()
			err := tsq.storage.populateLocalCache(req.Context, req.Key)
			duration := time.Since(start)

			// Release semaphore
			tsq.semaphore.Release(1)

			// Send result
			select {
			case req.ResultCh <- err:
			case <-req.Context.Done():
				// Context cancelled, don't block
			}

			// Log completion
			if err != nil {
				log.Error().
					Err(err).
					Str("key", req.Key).
					Dur("duration", duration).
					Int("worker_id", id).
					Msg("Failed to populate L1 cache from L2")
			} else {
				log.Debug().
					Str("key", req.Key).
					Dur("duration", duration).
					Int("worker_id", id).
					Msg("Successfully populated L1 cache from L2")
			}
		}
	}
}

// SubmitSync submits an async L1 cache population request
func (tsq *TieredSyncQueue) SubmitSync(ctx context.Context, key string) <-chan error {
	resultCh := make(chan error, 1)

	req := &TieredSyncRequest{
		Key:      key,
		Context:  ctx,
		ResultCh: resultCh,
	}

	select {
	case tsq.queue <- req:
		// Successfully queued
	case <-ctx.Done():
		// Context cancelled
		resultCh <- ctx.Err()
	default:
		// Queue full, skip async sync (not critical)
		log.Warn().Str("key", key).Msg("Tiered sync queue is full, skipping L1 population")
		resultCh <- fmt.Errorf("sync queue is full")
	}

	return resultCh
}

// Close shuts down the tiered sync queue
func (tsq *TieredSyncQueue) Close() error {
	tsq.cancel()

	// Close the queue channel to signal workers to finish current work
	close(tsq.queue)

	// Wait for all workers to finish
	tsq.wg.Wait()

	log.Info().Msg("Tiered sync queue shut down")
	return nil
}

// TieredConfig holds configuration for tiered storage
type TieredConfig struct {
	// Local cache (L1) configuration
	LocalCacheDir  string
	LocalCacheSize int64
	LocalCacheTTL  time.Duration // TTL for local cache entries (0 = disabled)

	// S3 (L2) configuration
	S3Config *S3Config

	// Sync queue configuration
	SyncWorkers   int // Number of workers for L1 population (default: 5)
	SyncQueueSize int // Size of sync queue (default: 100)
}

// NewTieredStorage creates a new tiered storage backend
func NewTieredStorage(cfg *TieredConfig) (*TieredStorage, error) {
	// Set defaults
	if cfg.SyncWorkers == 0 {
		cfg.SyncWorkers = 5
	}
	if cfg.SyncQueueSize == 0 {
		cfg.SyncQueueSize = 100
	}
	if cfg.LocalCacheSize == 0 {
		cfg.LocalCacheSize = 10 * 1024 * 1024 * 1024 // 10GB default
	}

	// Create local storage with LRU eviction (L1 cache)
	localStorage, err := NewLRULocalStorage(cfg.LocalCacheDir, cfg.LocalCacheSize, cfg.LocalCacheTTL)
	if err != nil {
		return nil, fmt.Errorf("failed to create local storage: %w", err)
	}

	// Create S3 storage (L2 cache)
	s3Storage, err := NewS3Storage(cfg.S3Config)
	if err != nil {
		return nil, fmt.Errorf("failed to create S3 storage: %w", err)
	}

	// Create tiered storage
	ts := &TieredStorage{
		localCache:    localStorage,
		remoteStorage: s3Storage,
	}

	// Initialize sync queue
	ts.syncQueue = NewTieredSyncQueue(ts, cfg.SyncQueueSize, cfg.SyncWorkers)

	log.Info().
		Str("local_cache_dir", cfg.LocalCacheDir).
		Int64("local_cache_size_bytes", cfg.LocalCacheSize).
		Int64("local_cache_size_mb", cfg.LocalCacheSize/(1024*1024)).
		Dur("local_cache_ttl", cfg.LocalCacheTTL).
		Str("s3_endpoint", cfg.S3Config.Endpoint).
		Str("s3_bucket", cfg.S3Config.Bucket).
		Int("sync_workers", cfg.SyncWorkers).
		Int("sync_queue_size", cfg.SyncQueueSize).
		Msg("Tiered storage initialized successfully")

	return ts, nil
}

// Get retrieves an object from tiered storage (L1 → L2 → error)
func (ts *TieredStorage) Get(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error) {
	// Try L1 (local) cache first
	reader, info, err := ts.localCache.Get(ctx, key)
	if err == nil {
		log.Debug().Str("key", key).Msg("✅ Tiered storage: L1 hit (local)")
		return reader, info, nil
	}

	// L1 miss, try L2 (S3) cache
	log.Debug().Str("key", key).Msg("🔍 Tiered storage: L1 miss, checking L2 (S3)")

	reader, info, err = ts.remoteStorage.Get(ctx, key)
	if err == nil {
		log.Info().Str("key", key).Msg("✅ Tiered storage: L2 hit (S3), populating L1 async")

		// Asynchronously populate L1 cache for future requests
		// Don't block current request on L1 population
		go func() {
			// Use background context with timeout
			syncCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			// Submit async sync request (non-blocking)
			_ = ts.syncQueue.SubmitSync(syncCtx, key)
		}()

		return reader, info, nil
	}

	// Both L1 and L2 miss
	log.Debug().Str("key", key).Msg("❌ Tiered storage: L1 and L2 miss")
	return nil, nil, fmt.Errorf("object not found in tiered storage: %s", key)
}

// GetRange retrieves a byte range from tiered storage
func (ts *TieredStorage) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, *ObjectInfo, error) {
	// Try L1 (local) cache first
	reader, info, err := ts.localCache.GetRange(ctx, key, offset, length)
	if err == nil {
		log.Debug().Str("key", key).Msg("✅ Tiered storage range: L1 hit (local)")
		return reader, info, nil
	}

	// L1 miss, try L2 (S3) cache
	log.Debug().Str("key", key).Msg("🔍 Tiered storage range: L1 miss, checking L2 (S3)")

	reader, info, err = ts.remoteStorage.GetRange(ctx, key, offset, length)
	if err == nil {
		log.Debug().Str("key", key).Msg("✅ Tiered storage range: L2 hit (S3)")

		// For range requests, we don't populate L1 cache
		// Only full file downloads populate L1 cache
		return reader, info, nil
	}

	// Both L1 and L2 miss
	return nil, nil, fmt.Errorf("object not found in tiered storage: %s", key)
}

// Put stores an object in both L1 and L2 concurrently
func (ts *TieredStorage) Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	// Use singleflight to prevent duplicate concurrent puts
	result, err, _ := ts.sf.Do("put:"+key, func() (interface{}, error) {
		return ts.putInternal(ctx, key, reader, size, contentType)
	})

	if err != nil {
		return nil, err
	}

	return result.(*ObjectInfo), nil
}

// putInternal performs the actual concurrent put to both L1 and L2
func (ts *TieredStorage) putInternal(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	// Create pipes for concurrent writes to both L1 and L2
	pr1, pw1 := io.Pipe()
	pr2, pw2 := io.Pipe()

	var l2Info *ObjectInfo
	var l1Err, l2Err error

	var wg sync.WaitGroup
	wg.Add(3) // Reader + 2 writers

	// Goroutine to read from source and tee to both pipes
	go func() {
		defer wg.Done()
		defer func() {
			_ = pw1.Close()
			_ = pw2.Close()
		}()

		// Use MultiWriter to write to both pipes simultaneously
		multiWriter := io.MultiWriter(pw1, pw2)
		_, err := io.Copy(multiWriter, reader)
		if err != nil {
			log.Error().Err(err).Str("key", key).Msg("Failed to read source data")
		}
	}()

	// Write to L2 (S3) - primary storage
	go func() {
		defer wg.Done()
		l2Info, l2Err = ts.remoteStorage.Put(ctx, key, pr2, size, contentType)
	}()

	// Write to L1 (local) - fast cache
	go func() {
		defer wg.Done()
		_, l1Err = ts.localCache.Put(ctx, key, pr1, size, contentType)
	}()

	// Wait for all operations to complete
	wg.Wait()

	// L2 (S3) is primary - if it fails, the operation fails
	if l2Err != nil {
		log.Error().Err(l2Err).Str("key", key).Msg("Failed to write to L2 (S3)")
		return nil, fmt.Errorf("failed to write to L2 storage: %w", l2Err)
	}

	// L1 failure is non-fatal (just log warning)
	if l1Err != nil {
		log.Warn().Err(l1Err).Str("key", key).Msg("Failed to write to L1 (local), but L2 (S3) succeeded")
	} else {
		log.Debug().Str("key", key).Msg("✅ Successfully wrote to both L1 and L2")
	}

	// Return L2 info as the authoritative source
	return l2Info, nil
}

// PutMultipart uploads a large object using multipart upload
func (ts *TieredStorage) PutMultipart(ctx context.Context, key string, reader io.Reader, size int64, contentType string, partSize int64) (*ObjectInfo, error) {
	// For multipart uploads, only write to L2 (S3) initially
	// L1 cache will be populated on first read
	log.Debug().
		Str("key", key).
		Int64("size", size).
		Int64("part_size", partSize).
		Msg("Tiered storage: Multipart upload to L2 only")

	info, err := ts.remoteStorage.PutMultipart(ctx, key, reader, size, contentType, partSize)
	if err != nil {
		return nil, fmt.Errorf("failed multipart upload to L2: %w", err)
	}

	log.Info().
		Str("key", key).
		Int64("size", size).
		Msg("✅ Multipart upload to L2 succeeded, L1 will be populated on first read")

	return info, nil
}

// Delete removes an object from both L1 and L2
func (ts *TieredStorage) Delete(ctx context.Context, key string) error {
	var l1Err, l2Err error

	// Delete from both caches concurrently
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		l1Err = ts.localCache.Delete(ctx, key)
	}()

	go func() {
		defer wg.Done()
		l2Err = ts.remoteStorage.Delete(ctx, key)
	}()

	wg.Wait()

	// L2 is primary - if it fails, the operation fails
	if l2Err != nil {
		log.Error().Err(l2Err).Str("key", key).Msg("Failed to delete from L2 (S3)")
		return fmt.Errorf("failed to delete from L2 storage: %w", l2Err)
	}

	// L1 failure is non-fatal
	if l1Err != nil {
		log.Warn().Err(l1Err).Str("key", key).Msg("Failed to delete from L1, but L2 succeeded")
	}

	return nil
}

// Exists checks if an object exists in L1 or L2
func (ts *TieredStorage) Exists(ctx context.Context, key string) (bool, error) {
	// Check L1 first (fast)
	exists, err := ts.localCache.Exists(ctx, key)
	if err == nil && exists {
		return true, nil
	}

	// Check L2
	return ts.remoteStorage.Exists(ctx, key)
}

// Stat retrieves object metadata from L1 or L2
func (ts *TieredStorage) Stat(ctx context.Context, key string) (*ObjectInfo, error) {
	// Try L1 first
	info, err := ts.localCache.Stat(ctx, key)
	if err == nil {
		return info, nil
	}

	// Try L2
	return ts.remoteStorage.Stat(ctx, key)
}

// List returns a list of objects from L2 (authoritative source)
func (ts *TieredStorage) List(ctx context.Context, opts ListOptions) ([]*ObjectInfo, error) {
	// Always list from L2 (S3) as it's the authoritative source
	return ts.remoteStorage.List(ctx, opts)
}

// GetPresignedURL generates a presigned URL from L2
func (ts *TieredStorage) GetPresignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	// Always generate presigned URLs from L2 (S3)
	return ts.remoteStorage.GetPresignedURL(ctx, key, expiry)
}

// StreamingPut stores an object with streaming support
func (ts *TieredStorage) StreamingPut(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	// For streaming puts, use the same logic as regular Put
	return ts.Put(ctx, key, reader, size, contentType)
}

// StreamingGet retrieves an object with zero-copy optimizations
func (ts *TieredStorage) StreamingGet(ctx context.Context, key string, writer io.Writer) (*ObjectInfo, error) {
	// Try L1 first (supports zero-copy)
	info, err := ts.localCache.StreamingGet(ctx, key, writer)
	if err == nil {
		log.Debug().Str("key", key).Msg("✅ Tiered streaming get: L1 hit (local, zero-copy)")
		return info, nil
	}

	// L1 miss, try L2
	log.Debug().Str("key", key).Msg("🔍 Tiered streaming get: L1 miss, streaming from L2 (S3)")

	info, err = ts.remoteStorage.StreamingGet(ctx, key, writer)
	if err == nil {
		log.Info().Str("key", key).Msg("✅ Tiered streaming get: L2 hit (S3), populating L1 async")

		// Asynchronously populate L1 cache for future requests
		go func() {
			syncCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			_ = ts.syncQueue.SubmitSync(syncCtx, key)
		}()

		return info, nil
	}

	// Both L1 and L2 miss
	return nil, fmt.Errorf("object not found in tiered storage: %s", key)
}

// GetFilePath returns the local file path for zero-copy operations (L1 only)
func (ts *TieredStorage) GetFilePath(ctx context.Context, key string) (string, error) {
	// Only L1 supports local file paths
	return ts.localCache.GetFilePath(ctx, key)
}

// SupportsZeroCopy indicates if L1 supports zero-copy operations
func (ts *TieredStorage) SupportsZeroCopy() bool {
	return ts.localCache.SupportsZeroCopy()
}

// Close releases resources from both storage backends
func (ts *TieredStorage) Close() error {
	// Close sync queue first
	if ts.syncQueue != nil {
		if err := ts.syncQueue.Close(); err != nil {
			log.Error().Err(err).Msg("Failed to close tiered sync queue")
		}
	}

	// Close both storage backends
	var l1Err, l2Err error

	if ts.localCache != nil {
		l1Err = ts.localCache.Close()
	}

	if ts.remoteStorage != nil {
		l2Err = ts.remoteStorage.Close()
	}

	// Return first error encountered
	if l1Err != nil {
		return l1Err
	}
	if l2Err != nil {
		return l2Err
	}

	log.Info().Msg("Tiered storage closed successfully")
	return nil
}

// populateLocalCache copies an object from L2 to L1
func (ts *TieredStorage) populateLocalCache(ctx context.Context, key string) error {
	// Check if already in L1
	exists, err := ts.localCache.Exists(ctx, key)
	if err == nil && exists {
		log.Debug().Str("key", key).Msg("Object already in L1 cache, skipping population")
		return nil
	}

	// Get from L2
	reader, info, err := ts.remoteStorage.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to get from L2 for L1 population: %w", err)
	}
	defer func() { _ = reader.Close() }()

	// Write to L1
	_, err = ts.localCache.Put(ctx, key, reader, info.Size, info.ContentType)
	if err != nil {
		return fmt.Errorf("failed to populate L1 cache: %w", err)
	}

	log.Info().
		Str("key", key).
		Int64("size", info.Size).
		Msg("✅ Successfully populated L1 cache from L2")

	return nil
}
