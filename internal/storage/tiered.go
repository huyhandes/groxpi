package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/phuslu/log"
	"golang.org/x/sync/singleflight"
)

// syncJobTimeout bounds a single L1 back-fill. A back-fill that has not
// finished by then is abandoned; the next read will queue a fresh one.
const syncJobTimeout = 5 * time.Minute

// l1Storage is the L1 tier contract: core storage plus the local-path
// capability that is the whole point of having an L1.
type l1Storage interface {
	Storage
	ZeroCopyCapable
}

// TieredStorage implements a multi-tier caching system with local (L1) and S3 (L2) storage.
//
// Zero-copy is re-exposed from L1, the only tier that genuinely has it (real
// files on disk).
type TieredStorage struct {
	localCache    l1Storage           // L1 cache - fast local storage
	remoteStorage Storage             // L2 cache - persistent S3 storage
	syncQueue     *WorkerPool[string] // Keys queued for L1 cache population
	sf            singleflight.Group
}

var (
	_ Storage         = (*TieredStorage)(nil)
	_ ZeroCopyCapable = (*TieredStorage)(nil)
)

// newTieredSyncQueue builds the bounded worker pool that back-fills L1 from L2.
//
// The job's context is derived from the pool's own lifetime context, never from
// the request that queued it. Submitting used to hand over a context the
// submitting goroutine cancelled on its way out, so every worker found a dead
// context and the back-fill silently never happened.
func newTieredSyncQueue(storage *TieredStorage, queueSize, workerCount int) *WorkerPool[string] {
	return NewWorkerPool("tiered-sync", queueSize, workerCount,
		func(poolCtx context.Context, key string) {
			jobCtx, cancel := context.WithTimeout(poolCtx, syncJobTimeout)
			defer cancel()

			start := time.Now()
			err := storage.populateLocalCache(jobCtx, key)
			duration := time.Since(start)

			if err != nil {
				log.Error().
					Err(err).
					Str("key", key).
					Dur("duration", duration).
					Msg("Failed to populate L1 cache from L2")
			} else {
				log.Debug().
					Str("key", key).
					Dur("duration", duration).
					Msg("Successfully populated L1 cache from L2")
			}
		})
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

	ts := newTieredStorage(localStorage, s3Storage, cfg.SyncQueueSize, cfg.SyncWorkers)

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

// newTieredStorage wires the two tiers and starts the back-fill pool. It exists
// so tests can supply tier doubles without a live S3.
func newTieredStorage(l1 l1Storage, l2 Storage, queueSize, workers int) *TieredStorage {
	ts := &TieredStorage{
		localCache:    l1,
		remoteStorage: l2,
	}
	ts.syncQueue = newTieredSyncQueue(ts, queueSize, workers)

	return ts
}

// Get retrieves an object from tiered storage (L1 → L2 → ErrNotFound).
//
// Only a genuine L1 miss falls through to L2. Any other L1 failure is returned
// as-is: treating a broken local disk as a cache miss hid real errors and
// quietly turned every read into an L2 round trip.
func (ts *TieredStorage) Get(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error) {
	reader, info, err := ts.localCache.Get(ctx, key)
	switch {
	case err == nil:
		log.Debug().Str("key", key).Msg("✅ Tiered storage: L1 hit (local)")
		return reader, info, nil
	case !errors.Is(err, ErrNotFound):
		log.Error().Err(err).Str("key", key).Msg("L1 read failed for a reason other than a miss")
		return nil, nil, fmt.Errorf("L1 read of %q failed: %w", key, err)
	}

	// L1 miss, try L2 (S3) cache
	log.Debug().Str("key", key).Msg("🔍 Tiered storage: L1 miss, checking L2 (S3)")

	reader, info, err = ts.remoteStorage.Get(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			log.Debug().Str("key", key).Msg("❌ Tiered storage: L1 and L2 miss")
			return nil, nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, nil, fmt.Errorf("L2 read of %q failed: %w", key, err)
	}

	log.Info().Str("key", key).Msg("✅ Tiered storage: L2 hit (S3), populating L1 async")

	// Back-fill L1 for future requests without blocking this one. The job owns
	// its own lifetime, so it survives this request finishing. Back-fill is
	// best-effort: a full queue drops the key rather than blocking the caller.
	if !ts.syncQueue.Submit(key) {
		log.Warn().Str("key", key).Msg("Tiered sync queue is full, skipping L1 population")
	}

	return reader, info, nil
}

// Put stores an object in both L1 and L2 concurrently
func (ts *TieredStorage) Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	// Use singleflight to prevent duplicate concurrent puts
	result, err, _ := ts.sf.Do("put:"+key, func() (any, error) {
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

		// Use MultiWriter to write to both pipes simultaneously
		multiWriter := io.MultiWriter(pw1, pw2)
		_, err := io.Copy(multiWriter, reader)
		if err != nil {
			log.Error().Err(err).Str("key", key).Msg("Failed to read source data")
		}
		// A clean close is what tells each tier the object is complete, so a
		// failed read must close with the error instead: neither tier may commit
		// a truncated object.
		_ = pw1.CloseWithError(err)
		_ = pw2.CloseWithError(err)
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

// Exists checks if an object exists in L1 or L2. Absence in L1 is (false, nil),
// so any error here is a real failure and is propagated rather than being
// papered over with an L2 lookup.
func (ts *TieredStorage) Exists(ctx context.Context, key string) (bool, error) {
	exists, err := ts.localCache.Exists(ctx, key)
	if err != nil {
		return false, fmt.Errorf("L1 existence check of %q failed: %w", key, err)
	}
	if exists {
		return true, nil
	}

	return ts.remoteStorage.Exists(ctx, key)
}

// Stat retrieves object metadata from L1, falling through to L2 only on a
// genuine miss.
func (ts *TieredStorage) Stat(ctx context.Context, key string) (*ObjectInfo, error) {
	info, err := ts.localCache.Stat(ctx, key)
	switch {
	case err == nil:
		return info, nil
	case !errors.Is(err, ErrNotFound):
		return nil, fmt.Errorf("L1 stat of %q failed: %w", key, err)
	}

	return ts.remoteStorage.Stat(ctx, key)
}

// List returns a list of objects from L2 (authoritative source)
func (ts *TieredStorage) List(ctx context.Context, opts ListOptions) ([]*ObjectInfo, error) {
	// Always list from L2 (S3) as it's the authoritative source
	return ts.remoteStorage.List(ctx, opts)
}

// GetFilePath returns the local file path for zero-copy serving. Only L1 holds
// real files, so an object that is only in L2 reports a miss here and the
// caller falls back to streaming it.
func (ts *TieredStorage) GetFilePath(ctx context.Context, key string) (string, error) {
	return ts.localCache.GetFilePath(ctx, key)
}

// Close releases resources from both storage backends. Both tiers are closed
// even if the first fails, and every failure is reported.
func (ts *TieredStorage) Close() error {
	// Stop the back-fill pool before the tiers it writes through
	ts.syncQueue.Close()

	if err := errors.Join(ts.localCache.Close(), ts.remoteStorage.Close()); err != nil {
		return fmt.Errorf("failed to close tiered storage: %w", err)
	}

	log.Info().Msg("Tiered storage closed successfully")
	return nil
}

// populateLocalCache copies an object from L2 to L1
func (ts *TieredStorage) populateLocalCache(ctx context.Context, key string) error {
	// Check if already in L1. The check is only a shortcut, so a failed check is
	// treated as "unknown" and population goes ahead: Put writes to a temp file
	// and renames, so re-populating an object that turned out to be present is
	// harmless, whereas assuming presence would leave L1 cold.
	exists, err := ts.localCache.Exists(ctx, key)
	if err != nil {
		log.Warn().Err(err).Str("key", key).Msg("L1 existence check failed, populating anyway")
	} else if exists {
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
