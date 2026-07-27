package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	// syncJobTimeout bounds a single L1 back-fill. A back-fill that has not
	// finished by then is abandoned; the next read will queue a fresh one.
	syncJobTimeout = 5 * time.Minute

	// uploadJobTimeout bounds a single best-effort upload of a finished local
	// file to the object store.
	uploadJobTimeout = 5 * time.Minute

	// uploadDrainTimeout bounds how long Close waits for queued uploads before
	// giving up on them.
	uploadDrainTimeout = 30 * time.Second
)

// l1Storage is the L1 tier contract: core storage plus the local-path
// capability that is the whole point of having an L1, plus the prefix delete
// that package eviction needs, plus the snapshot the cache listing reads. All
// of those are things only a tier holding real local files can do.
type l1Storage interface {
	Storage
	ZeroCopyCapable
	PrefixDeleter
	Snapshot() []LRUEntry
}

// uploadJob names a finished local file to copy up to the object store.
type uploadJob struct {
	key         string
	contentType string
}

// TieredStorage caches objects on local disk (L1) in front of an object store
// (L2).
//
// L1 is authoritative for writes: a request is served as soon as the local file
// is complete, and the copy to L2 happens afterwards on a bounded worker pool.
// L2 is a best-effort cache, so an upload failure, a full queue or a race with
// local eviction is logged and dropped — a file missing from both tiers is
// simply refetched from upstream.
//
// Zero-copy is re-exposed from L1, the only tier that genuinely has it (real
// files on disk).
type TieredStorage struct {
	localCache    l1Storage              // L1 cache - fast local storage
	remoteStorage Storage                // L2 cache - persistent object storage
	syncQueue     *WorkerPool[string]    // Keys queued for L1 back-fill from L2
	uploadQueue   *WorkerPool[uploadJob] // Finished local files queued for L2
	sf            singleflight.Group
}

var (
	_ Storage         = (*TieredStorage)(nil)
	_ ZeroCopyCapable = (*TieredStorage)(nil)
	_ PrefixDeleter   = (*TieredStorage)(nil)
)

// TieredConfig holds configuration for tiered storage
type TieredConfig struct {
	// Local cache (L1) configuration
	LocalCacheDir  string
	LocalCacheSize int64
	LocalCacheTTL  time.Duration // TTL for local cache entries (0 = disabled)

	// S3 (L2) configuration
	S3Config *S3Config

	// Background transfer configuration, shared by L1 back-fill and L2 upload
	SyncWorkers   int // Number of workers per pool (default: 5)
	SyncQueueSize int // Queue depth per pool (default: 100)
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

	slog.Info("Tiered storage initialized successfully",
		"local_cache_dir", cfg.LocalCacheDir,
		"local_cache_size_bytes", cfg.LocalCacheSize,
		"local_cache_ttl", cfg.LocalCacheTTL,
		"s3_endpoint", cfg.S3Config.Endpoint,
		"s3_bucket", cfg.S3Config.Bucket,
		"workers", cfg.SyncWorkers,
		"queue_size", cfg.SyncQueueSize)

	return ts, nil
}

// newTieredStorage wires the two tiers and starts the background pools. It
// exists so tests can supply tier doubles without a live object store.
func newTieredStorage(l1 l1Storage, l2 Storage, queueSize, workers int) *TieredStorage {
	ts := &TieredStorage{
		localCache:    l1,
		remoteStorage: l2,
	}

	// The job context is derived from the pool's lifetime rather than from the
	// request that queued it: a back-fill must outlive the read that triggered
	// it.
	ts.syncQueue = NewWorkerPool("tiered-sync", queueSize, workers,
		func(poolCtx context.Context, key string) {
			jobCtx, cancel := context.WithTimeout(poolCtx, syncJobTimeout)
			defer cancel()

			if err := ts.populateLocalCache(jobCtx, key); err != nil {
				slog.Error("Failed to populate L1 cache from L2", "error", err, "key", key)
			}
		})

	// Uploads deliberately ignore the pool context so that Close can drain a
	// job that is already running instead of cancelling it mid-flight.
	ts.uploadQueue = NewWorkerPool("tiered-upload", queueSize, workers,
		func(_ context.Context, job uploadJob) {
			jobCtx, cancel := context.WithTimeout(context.Background(), uploadJobTimeout)
			defer cancel()

			if err := ts.uploadToRemote(jobCtx, job); err != nil {
				slog.Warn("Best-effort upload to L2 failed", "error", err, "key", job.key)
			}
		})

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
		return reader, info, nil
	case !errors.Is(err, ErrNotFound):
		slog.Error("L1 read failed for a reason other than a miss", "error", err, "key", key)
		return nil, nil, fmt.Errorf("L1 read of %q failed: %w", key, err)
	}

	reader, info, err = ts.remoteStorage.Get(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, nil, fmt.Errorf("L2 read of %q failed: %w", key, err)
	}

	// Back-fill L1 for future requests without blocking this one. Best-effort:
	// a full queue drops the key rather than blocking the caller.
	if !ts.syncQueue.Submit(key) {
		slog.Warn("Tiered sync queue is full, skipping L1 population", "key", key)
	}

	return reader, info, nil
}

// Put writes the object to local disk and returns as soon as that write is
// complete; the copy to the object store is queued in the background.
func (ts *TieredStorage) Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	// Singleflight prevents duplicate concurrent puts of the same key.
	result, err, _ := ts.sf.Do("put:"+key, func() (any, error) {
		info, err := ts.localCache.Put(ctx, key, reader, size, contentType)
		if err != nil {
			return nil, fmt.Errorf("failed to write to L1 storage: %w", err)
		}

		if !ts.uploadQueue.Submit(uploadJob{key: key, contentType: contentType}) {
			slog.Warn("Tiered upload queue is full, skipping L2 upload", "key", key)
		}

		return info, nil
	})
	if err != nil {
		return nil, err
	}

	return result.(*ObjectInfo), nil
}

// uploadToRemote copies a finished local file up to the object store. The file
// is a real seekable file, so a small object costs a single put rather than a
// multipart upload. A file that has since been evicted is not an error: the
// object is simply refetched from upstream when it is next wanted.
func (ts *TieredStorage) uploadToRemote(ctx context.Context, job uploadJob) error {
	reader, info, err := ts.localCache.Get(ctx, job.key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			slog.Debug("Local file evicted before upload, dropping", "key", job.key)
			return nil
		}
		return fmt.Errorf("failed to read local file for upload: %w", err)
	}
	defer func() { _ = reader.Close() }()

	if _, err := ts.remoteStorage.Put(ctx, job.key, reader, info.Size, job.contentType); err != nil {
		return fmt.Errorf("failed to upload to L2: %w", err)
	}

	slog.Debug("Uploaded local file to L2", "key", job.key, "size", info.Size)
	return nil
}

// Delete removes an object from both tiers. Only the L1 failure is propagated:
// L2 is best-effort, so a stale object left behind there costs a wasted
// back-fill rather than correctness.
func (ts *TieredStorage) Delete(ctx context.Context, key string) error {
	var l2Err error

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		l2Err = ts.remoteStorage.Delete(ctx, key)
	}()

	l1Err := ts.localCache.Delete(ctx, key)
	wg.Wait()

	if l2Err != nil {
		slog.Warn("Failed to delete from L2 (best-effort)", "error", l2Err, "key", key)
	}
	if l1Err != nil {
		return fmt.Errorf("failed to delete from L1 storage: %w", l1Err)
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

// DeletePrefix forwards package eviction to L1, which is the only tier that
// knows its own contents. Without this, evicting a package in hybrid mode
// cleared the index entry and left every file on disk while reporting success.
//
// L2 objects are deliberately left in place. Deleting them would mean listing
// the object store to discover what matches the prefix, and that listing
// operation was removed from the storage interface on purpose. The cost of
// leaving them is bounded and self-correcting: L2 is a best-effort cache, so a
// stale object there costs one wasted back-fill, and the next upstream fetch
// overwrites it. Freeing object-store space is the object store's lifecycle
// policy's job, not the proxy's.
func (ts *TieredStorage) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	return ts.localCache.DeletePrefix(ctx, prefix)
}

// Snapshot forwards the cache listing to L1, the only tier that knows its own
// contents. Without this, the listing page rendered zero rows in hybrid mode
// even with files cached locally, because the top-level backend is this one.
//
// L2 objects are not reported, for the same reason DeletePrefix leaves them
// alone: naming them would need a listing operation the storage interface
// deliberately does without.
func (ts *TieredStorage) Snapshot() []LRUEntry {
	return ts.localCache.Snapshot()
}

// GetFilePath returns the local file path for zero-copy serving. Only L1 holds
// real files, so an object that is only in L2 reports a miss here and the
// caller falls back to streaming it.
func (ts *TieredStorage) GetFilePath(ctx context.Context, key string) (string, error) {
	return ts.localCache.GetFilePath(ctx, key)
}

// Close drains pending uploads under a timeout, stops the pools, then closes
// both tiers. Both tiers are closed even if the first fails.
func (ts *TieredStorage) Close() error {
	ts.uploadQueue.Drain(uploadDrainTimeout)
	ts.syncQueue.Close()

	if err := errors.Join(ts.localCache.Close(), ts.remoteStorage.Close()); err != nil {
		return fmt.Errorf("failed to close tiered storage: %w", err)
	}

	return nil
}

// populateLocalCache copies an object from L2 to L1
func (ts *TieredStorage) populateLocalCache(ctx context.Context, key string) error {
	// The check is only a shortcut, so a failed check is treated as "unknown"
	// and population goes ahead: Put writes to a temp file and renames, so
	// re-populating an object that turned out to be present is harmless,
	// whereas assuming presence would leave L1 cold.
	exists, err := ts.localCache.Exists(ctx, key)
	if err != nil {
		slog.Warn("L1 existence check failed, populating anyway", "error", err, "key", key)
	} else if exists {
		return nil
	}

	reader, info, err := ts.remoteStorage.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to get from L2 for L1 population: %w", err)
	}
	defer func() { _ = reader.Close() }()

	if _, err := ts.localCache.Put(ctx, key, reader, info.Size, info.ContentType); err != nil {
		return fmt.Errorf("failed to populate L1 cache: %w", err)
	}

	return nil
}
