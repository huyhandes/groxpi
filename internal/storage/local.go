package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/huyhandes/groxpi/internal/telemetry"
)

const (
	// touchInterval throttles the atime bump on a hit: recency at hour
	// resolution is plenty for eviction and keeps hot files from paying a
	// syscall per serve.
	touchInterval = time.Hour

	// ponytail: hardcoded 90% low watermark, make it a setting when an operator
	// needs a different eviction hysteresis.
	lowWatermarkPercent = 90

	tmpPrefix = ".tmp-"
)

// CacheEntry is one cached file as the listing shows it. The filesystem is the
// index: CreatedAt is the file's mtime (its Last-Modified), Accessed its atime.
type CacheEntry struct {
	Key       string
	Size      int64
	CreatedAt time.Time
	Accessed  time.Time
}

// LocalStorage stores objects as plain files under a base directory and evicts
// them by atime. The filesystem is the LRU index: a hit bumps the file's atime
// (explicitly, so a noatime mount still works), one counter tracks total size,
// and crossing maxSize walks the tree deleting oldest-accessed files down to the
// low watermark. The same walk applies the TTL, measured from when the file was
// written (its mtime); with a TTL set, a background ticker also runs the walk so
// an idle cache still expires. Recency lives on disk, so it survives a restart.
type LocalStorage struct {
	baseDir string
	maxSize int64         // 0 = unlimited
	ttl     time.Duration // 0 = disabled; age since the file was written

	size   atomic.Int64
	walkMu sync.Mutex // one eviction walk at a time

	stopSweep context.CancelFunc // nil when no TTL sweeper runs
	sweeper   sync.WaitGroup
}

// maxTTLSweepInterval caps how long the cache can go without checking for
// expired files, however long the TTL is.
const maxTTLSweepInterval = time.Minute

// ttlSweepInterval is half the TTL, so a file outlives its TTL by at most 50%,
// capped so a long TTL does not leave stale files on disk for hours.
func ttlSweepInterval(ttl time.Duration) time.Duration {
	return max(min(ttl/2, maxTTLSweepInterval), time.Millisecond)
}

// localError wraps a filesystem failure during op on key, folding a genuine
// absence into the shared ErrNotFound sentinel so callers can branch with
// errors.Is. It is the local counterpart of s3Error.
func localError(err error, key, op string) error {
	if os.IsNotExist(err) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return fmt.Errorf("failed to %s %s: %w", op, key, err)
}

// NewLocalStorage creates a local filesystem storage backend bounded by
// maxSize bytes (0 = unlimited) whose files expire ttl after they were written
// (0 = never). A startup walk seeds the size counter and applies both limits.
func NewLocalStorage(baseDir string, maxSize int64, ttl time.Duration) (*LocalStorage, error) {
	// Ensure base directory exists
	if err := os.MkdirAll(baseDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}

	l := &LocalStorage{baseDir: baseDir, maxSize: maxSize, ttl: ttl}
	if err := l.evict(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to scan cache directory: %w", err)
	}
	if ttl > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		l.stopSweep = cancel
		l.sweeper.Go(func() { l.sweep(ctx, ttlSweepInterval(ttl)) })
	}
	return l, nil
}

// sweep runs the eviction walk every interval until ctx is cancelled, so the
// TTL applies even when nothing is written.
func (l *LocalStorage) sweep(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !l.walkMu.TryLock() {
				continue // a size-driven walk is already running
			}
			if err := l.evict(ctx); err != nil {
				slog.Error("Local cache TTL sweep failed", "error", err)
			}
			l.walkMu.Unlock()
		}
	}
}

// removeStaleTemp removes a temp file whose mtime is older than touchInterval:
// a write in progress keeps its mtime fresh, so an old one is a crash leftover.
func removeStaleTemp(path string, mtime time.Time) {
	if time.Since(mtime) > touchInterval {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Error("Failed to remove stale temp file", "path", path, "error", err)
		}
	}
}

// scan walks the cache directory and reports every committed file. Temp files
// are not reported; one whose mtime is older than touchInterval is a crash
// leftover (a write in progress keeps its mtime fresh) and is removed. Files
// vanishing mid-walk are not errors.
func (l *LocalStorage) scan() ([]CacheEntry, error) {
	var entries []CacheEntry
	err := filepath.WalkDir(l.baseDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			var info fs.FileInfo
			if info, err = d.Info(); err == nil && strings.HasPrefix(d.Name(), tmpPrefix) {
				removeStaleTemp(path, info.ModTime())
			} else if err == nil {
				rel, _ := filepath.Rel(l.baseDir, path)
				entries = append(entries, CacheEntry{
					Key:       filepath.ToSlash(rel),
					Size:      info.Size(),
					CreatedAt: info.ModTime(),
					Accessed:  atime(info),
				})
			}
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	})
	return entries, err
}

// evict walks the cache once: files written longer than the TTL ago go, and,
// if the total is over maxSize, oldest-accessed files go until it is at the low
// watermark. The size counter is then reset to what is left on disk.
// Caller must hold walkMu, or be the constructor.
func (l *LocalStorage) evict(ctx context.Context) error {
	now := time.Now()
	entries, err := l.scan()
	if err != nil {
		return err
	}

	var total int64
	for _, e := range entries {
		total += e.Size
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Accessed.Before(entries[j].Accessed) })
	target := l.maxSize * lowWatermarkPercent / 100

	// Oldest access first, so shrinking takes from that end. The TTL is by
	// write time, which the access order says nothing about, so with a TTL on
	// every file is checked.
	shrink := l.maxSize > 0 && total > l.maxSize
	var evicted int64
	for _, e := range entries {
		shrink = shrink && total > target
		if !shrink && (l.ttl <= 0 || now.Sub(e.CreatedAt) <= l.ttl) {
			if l.ttl <= 0 {
				break
			}
			continue
		}
		if err := l.remove(e.Key, e.Size); err != nil {
			slog.Error("Failed to evict cached file", "key", e.Key, "error", err)
			continue
		}
		total -= e.Size
		evicted++
	}
	// ponytail: a Put racing the walk can be counted twice or not at all; the
	// next walk's resync corrects it.
	l.size.Store(total)

	if evicted > 0 {
		telemetry.CacheEviction(ctx, telemetry.LayerLocal, evicted)
		slog.Info("Evicted files from local cache", "evicted_count", evicted, "size_bytes", l.size.Load(), "max_size_bytes", l.maxSize)
	}
	telemetry.CacheOccupancy(ctx, telemetry.LayerLocal, l.size.Load())
	return nil
}

// remove unlinks key and takes its size off the counter.
func (l *LocalStorage) remove(key string, size int64) error {
	if err := os.Remove(l.buildPath(key)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	l.size.Add(-size)
	return nil
}

// maybeEvict runs an eviction walk when the size counter crosses maxSize.
//
// ponytail: runs synchronously on the Put that crosses the limit; move to a
// background goroutine if that one Put's latency ever matters.
func (l *LocalStorage) maybeEvict(ctx context.Context) {
	due := func() bool {
		return l.maxSize > 0 && l.size.Load() > l.maxSize
	}
	telemetry.CacheOccupancy(ctx, telemetry.LayerLocal, l.size.Load())
	if !due() || !l.walkMu.TryLock() {
		return // under limits, or a walk is already running
	}
	defer l.walkMu.Unlock()
	if !due() {
		return // a walk finished between the check and the lock
	}
	if err := l.evict(ctx); err != nil {
		slog.Error("Local cache eviction walk failed", "error", err)
	}
}

// touch marks path as recently used by setting its atime, at most once per
// touchInterval. mtime is preserved: it is the file's Last-Modified.
func touch(path string, info fs.FileInfo) {
	now := time.Now()
	if now.Sub(atime(info)) < touchInterval {
		return
	}
	if err := os.Chtimes(path, now, info.ModTime()); err != nil {
		slog.Debug("Failed to bump atime", "path", path, "error", err)
	}
}

// Snapshot lists every cached file, most recently accessed first, by walking
// the cache directory on demand. The admin listing pages the result.
//
// ponytail: the walk itself is whole-tree; stream it when a listing of
// hundreds of thousands of files gets slow.
func (l *LocalStorage) Snapshot() []CacheEntry {
	entries, err := l.scan()
	if err != nil {
		slog.Error("Failed to list local cache", "error", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Accessed.After(entries[j].Accessed) })
	return entries
}

// DeletePrefix removes every cached file whose key starts with prefix and
// reports how many were removed. A prefix matching nothing is not an error.
func (l *LocalStorage) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	entries, err := l.scan()
	if err != nil {
		return 0, err
	}
	var errs []error
	deleted := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Key, prefix) {
			continue
		}
		if err := l.remove(e.Key, e.Size); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete %q: %w", e.Key, err))
			continue
		}
		deleted++
	}
	telemetry.CacheEviction(ctx, telemetry.LayerLocal, int64(deleted))
	telemetry.CacheOccupancy(ctx, telemetry.LayerLocal, l.size.Load())
	return deleted, errors.Join(errs...)
}

// buildPath constructs the full filesystem path
func (l *LocalStorage) buildPath(key string) string {
	// Join cleans ".." segments, so a key can only ever name a path under baseDir.
	return filepath.Join(l.baseDir, filepath.Clean("/"+key))
}

// Get retrieves an object from local filesystem
func (l *LocalStorage) Get(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error) {
	path := l.buildPath(key)

	file, err := os.Open(path) // #nosec G304 -- path is confined to baseDir by buildPath
	if err != nil {
		return nil, nil, localError(err, key, "open")
	}

	stat, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("failed to stat file: %w", err)
	}
	touch(path, stat)

	return file, &ObjectInfo{Size: stat.Size(), LastModified: stat.ModTime()}, nil
}

// commit renames a finished temp file (in key's directory) into place and
// accounts for its size. It is the whole of a local write once the spool's
// bytes are on disk.
func (l *LocalStorage) commit(ctx context.Context, tmpPath, key string, written int64) error {
	path := l.buildPath(key)
	// An overwrite replaces the old file's bytes rather than adding to them.
	var replaced int64
	if old, err := os.Stat(path); err == nil {
		replaced = old.Size()
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to move file: %w", err)
	}
	l.size.Add(written - replaced)
	l.maybeEvict(ctx)
	return nil
}

// Close stops the TTL sweeper, if one runs. Calling it twice is safe.
func (l *LocalStorage) Close() error {
	if l.stopSweep != nil {
		l.stopSweep()
		l.sweeper.Wait()
	}
	return nil
}
