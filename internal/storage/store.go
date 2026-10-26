package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/download"
	"github.com/huyhandes/groxpi/internal/index"
	"github.com/huyhandes/groxpi/internal/telemetry"
)

// Fetcher resolves a package file in the index and opens it upstream. It is the
// one cross-module interface storage consumes; download.Service implements it
// and the tests fake it. Resolve fails with a wrapped download.ErrNotListed for
// a file the index does not list, and with any other error when the index
// itself could not be resolved.
type Fetcher interface {
	Resolve(ctx context.Context, pkg, file string) (download.Target, error)
	Fetch(ctx context.Context, t download.Target) (*download.File, error)
}

// RedirectError is a miss the store will not or could not serve, for a file the
// index lists: the caller should redirect the client to URL. Reason is
// telemetry.RedirectCachingDisabled (redirect mode) or
// telemetry.RedirectFetchFailed (the fetch, or its spool, failed before the
// body started). URL comes from download.Target.RedirectURL, so it never
// carries userinfo; Stripped reports that some was removed. URL is empty when
// the target did not parse. Err is the cause, already redacted.
type RedirectError struct {
	URL      string
	Stripped bool
	Reason   string
	Err      error
}

func newRedirectError(t download.Target, reason string, err error) *RedirectError {
	u, stripped := t.RedirectURL()
	return &RedirectError{URL: u, Stripped: stripped, Reason: reason, Err: err}
}

func (e *RedirectError) Error() string {
	return fmt.Sprintf("redirect upstream (%s): %v", e.Reason, e.Err)
}

func (e *RedirectError) Unwrap() error { return e.Err }

// Object is an opened package file. A completed object is seekable (hand it to
// http.ServeContent); an InFlight one tails a download still being written and
// only reads forward.
type Object struct {
	io.ReadSeekCloser
	Name        string
	Size        int64 // -1 when unknown
	ModTime     time.Time
	ETag        string // quoted, empty when unknown
	ContentType string
	InFlight    bool
}

// Progress is one running download as the admin surface sees it.
type Progress struct {
	Package  string
	File     string
	Size     int64 // declared by the index, -1 if unknown
	Bytes    int64 // written so far
	Started  time.Time
	Requests int // clients coalesced onto this download, leader included
}

// ErrInvalidKey is returned for a package or file name that cannot be a single
// path segment under packages/. It is distinct from ErrNotFound: nothing was
// looked up.
var ErrInvalidKey = errors.New("invalid storage key")

// Key is the storage key of a package file, packages/{pkg}/{file}, with pkg
// PEP 503 normalised so every spelling of a name is one key. An empty file
// names the package's prefix. The normalised pkg and the raw file must each be
// one path segment: not empty, no '/', '\' or NUL, and no leading '.', so a key
// never leaves its package directory.
func Key(pkg, file string) (string, error) {
	return key(index.NormalizeName(pkg), file)
}

// key is Key for an already normalised pkg: it only validates.
func key(pkg, file string) (string, error) {
	if !validSegment(pkg) || (file != "" && !validSegment(file)) {
		return "", fmt.Errorf("%w: package %q, file %q", ErrInvalidKey, pkg, file)
	}
	return "packages/" + pkg + "/" + file, nil
}

func validSegment(s string) bool {
	return s != "" && s[0] != '.' && !strings.ContainsAny(s, "/\\\x00")
}

var copyBufPool = sync.Pool{New: func() any { b := make([]byte, 64*1024); return &b }}

// Store is the read-through store: Open serves a hit from the cache or the
// durable store, and on a miss runs one detached download per key into a spool
// file that every waiting client tails while it grows.
//
// The cache (local disk) is bounded and disposable; the durable store (S3) is
// unbounded and receives every verified download. Either may be nil, not both:
// local mode is cache only, s3 mode durable only, hybrid the cache in front.
type Store struct {
	cache    *LocalStorage // nil in s3 mode
	durable  *S3Storage    // nil in local mode
	fetcher  Fetcher
	spoolDir string     // spool directory when there is no cache
	redirect bool       // a miss is never fetched; the caller redirects upstream
	commit   commitFunc // per mode, chosen once in NewStore

	mu         sync.Mutex
	flights    map[string]*flight
	committing map[*flight]struct{} // hybrid flights unregistered but not yet uploaded
	commits    [commitSlots]uint64  // per commitSlot, bumped at each post-commit unregister, under mu
	slotSeed   maphash.Seed         // commitSlot's hash seed, fixed for the Store's life
	leaders    sync.WaitGroup
}

// NewStore builds a store over a cache, a durable store, or both (at least one
// must be non-nil). spoolDir holds the spool files when there is no cache (pure
// s3); crash leftovers in it are removed by the same rule the cache's scan
// applies. With redirect set the store never fetches: a miss in every tier is a
// RedirectError(caching_disabled), while a durable hit in hybrid mode is still
// promoted.
func NewStore(cache *LocalStorage, durable *S3Storage, fetcher Fetcher, spoolDir string, redirect bool) *Store {
	s := &Store{cache: cache, durable: durable, fetcher: fetcher, spoolDir: spoolDir, redirect: redirect,
		flights: map[string]*flight{}, committing: map[*flight]struct{}{}, slotSeed: maphash.MakeSeed()}
	switch {
	case cache == nil:
		s.commit = s.commitS3
		reapSpool(spoolDir)
	case durable == nil:
		s.commit = s.commitLocal
	default:
		s.commit = s.commitHybrid
	}
	return s
}

// New builds the store the configuration's storage mode asks for: the cache
// (local), the durable store (s3), or the cache in front of it (hybrid). The
// zero mode is local. An unknown mode is an error rather than a silent local
// store, which would run without the durable store the operator asked for. A
// download budget of 0 is redirect mode: misses are never fetched.
func New(cfg *config.Config, fetcher Fetcher) (*Store, error) {
	var (
		cache   *LocalStorage
		durable *S3Storage
		err     error
	)
	switch cfg.StorageType {
	case "", "local":
		cache, err = NewLocalStorage(cfg.CacheDir, cfg.CacheSize, 0)
	case "s3", "hybrid":
		durable, err = NewS3Storage(&S3Config{
			Endpoint:        cfg.S3Endpoint,
			AccessKeyID:     cfg.S3AccessKeyID,
			SecretAccessKey: cfg.S3SecretAccessKey,
			Region:          cfg.S3Region,
			Bucket:          cfg.S3Bucket,
			Prefix:          cfg.S3Prefix,
			UseSSL:          cfg.S3UseSSL,
			ForcePathStyle:  cfg.S3ForcePathStyle,
			EnableHTTP2:     cfg.S3EnableHTTP2,
			ConnectTimeout:  cfg.ConnectTimeout,
		})
		if err == nil && cfg.StorageType == "hybrid" {
			if cache, err = NewLocalStorage(cfg.LocalCacheDir, cfg.LocalCacheSize, cfg.LocalCacheTTL); err != nil {
				_ = durable.Close()
				err = fmt.Errorf("failed to create local cache: %w", err)
			}
		}
	default:
		return nil, fmt.Errorf("unknown storage type %q: want local, s3 or hybrid", cfg.StorageType)
	}
	if err != nil {
		return nil, err
	}

	// Pure-s3 spools get a directory of their own: the cache dir defaults to
	// the shared OS temp dir, and the store reaps stale files in its spool dir.
	spoolDir := filepath.Join(cfg.CacheDir, "spool")
	if cache == nil {
		if err := os.MkdirAll(spoolDir, 0750); err != nil {
			_ = durable.Close()
			return nil, fmt.Errorf("failed to create spool directory: %w", err)
		}
	}
	s := NewStore(cache, durable, fetcher, spoolDir, cfg.DownloadTimeout <= 0)

	d := s.Describe()
	attrs := []any{"mode", d.Mode, "cache_dir", d.CacheDir, "cache_size", d.CacheSize, "redirect", s.redirect}
	if durable != nil {
		attrs = append(attrs, "endpoint", config.RedactURL(cfg.S3Endpoint), "bucket", cfg.S3Bucket, "prefix", cfg.S3Prefix)
	}
	slog.Info("storage configured", attrs...)
	return s, nil
}

// Description is what the store reports about itself: the storage mode and
// the cache behind it. With no cache (s3) the directory is empty and the size 0.
type Description struct {
	Mode      string // local, s3 or hybrid
	CacheDir  string
	CacheSize int64 // bytes, 0 = unbounded or no cache
}

// Describe reports the storage mode and the cache this store actually uses.
func (s *Store) Describe() Description {
	switch {
	case s.cache == nil:
		return Description{Mode: "s3"}
	case s.durable == nil:
		return Description{Mode: "local", CacheDir: s.cache.baseDir, CacheSize: s.cache.maxSize}
	default:
		return Description{Mode: "hybrid", CacheDir: s.cache.baseDir, CacheSize: s.cache.maxSize}
	}
}

// reapSpool removes crash-leftover spool files from dir, which the store owns
// alone (never a shared directory such as the OS temp dir).
func reapSpool(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, tmpPrefix+"*"))
	for _, p := range matches {
		if info, err := os.Stat(p); err == nil {
			removeStaleTemp(p, info.ModTime())
		}
	}
}

// Redirects reports whether the store was built in redirect mode, where a miss
// is never fetched.
func (s *Store) Redirects() bool { return s.redirect }

// Open opens a package file. On a miss it resolves the file once and fetches
// it, or in redirect mode returns a RedirectError(caching_disabled). A miss
// that is not served is one of: a *RedirectError (errors.As), a wrapped
// download.ErrNotListed, or an error resolving the package index. A name Key
// refuses, an empty file included, returns ErrInvalidKey before anything is
// touched.
func (s *Store) Open(ctx context.Context, pkg, file string) (*Object, error) {
	if file == "" {
		return nil, fmt.Errorf("%w: empty file name", ErrInvalidKey)
	}
	pkg = index.NormalizeName(pkg) // once, so the resolve below sees it too
	key, err := key(pkg, file)
	if err != nil {
		return nil, err
	}
	f, seen := s.join(key)
	if f != nil {
		return f.open(ctx)
	}

	// A flight that commits between the lookup and the lock below changes its
	// key's commit slot; the lookup then runs once more instead of fetching again.
	slot := s.commitSlot(key)
	var promote *download.File
	for retried := false; ; retried = true {
		var obj *Object
		obj, promote = s.lookup(ctx, key, file, !retried) // one miss per Open
		if obj != nil {
			return obj, nil
		}
		if promote == nil && s.redirect {
			t, err := s.fetcher.Resolve(ctx, pkg, file)
			if err != nil {
				return nil, err
			}
			return nil, newRedirectError(t, telemetry.RedirectCachingDisabled, fmt.Errorf("%w: %s", ErrNotFound, key))
		}
		s.mu.Lock()
		if retried || s.commits[slot] == seen || s.flights[key] != nil {
			break
		}
		s.mu.Unlock()
		if promote != nil {
			_ = promote.Body.Close()
		}
	}
	f = s.flights[key]
	if f != nil {
		f.ref()
		s.mu.Unlock()
		if promote != nil {
			_ = promote.Body.Close()
		}
		return f.open(ctx)
	}
	f = &flight{key: key, pkg: pkg, file: file, size: -1, started: time.Now(), ready: make(chan struct{}), refs: 2, requests: 1}
	f.cond = sync.NewCond(&f.mu)
	s.flights[key] = f
	telemetry.DownloadsInFlight(ctx, int64(len(s.flights)))
	s.leaders.Add(1)
	s.mu.Unlock()

	// The download fills the cache for every waiter, so it must not die with the
	// client that started it. Store.Close waits for it.
	go s.lead(context.WithoutCancel(ctx), f, promote)
	return f.open(ctx)
}

// join refs the flight for key, if any, and returns the commit count seen in
// key's slot.
func (s *Store) join(key string) (*flight, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.flights[key]
	if f != nil {
		f.ref()
	}
	return f, s.commits[s.commitSlot(key)]
}

// commitSlot is the slot of s.commits that counts the commits of key: a hash
// of the key, so a commit only re-checks lookups of keys sharing its slot.
const commitSlots = 64

func (s *Store) commitSlot(key string) int {
	return int(maphash.String(s.slotSeed, key) % commitSlots)
}

func (s *Store) unregister(f *flight) { s.drop(f, false) }

// unregisterCommitted unregisters a flight whose file is now stored, so an
// Open that missed it in between looks again.
func (s *Store) unregisterCommitted(f *flight) { s.drop(f, true) }

func (s *Store) drop(f *flight, committed bool) {
	s.mu.Lock()
	delete(s.flights, f.key)
	if committed {
		s.commits[s.commitSlot(f.key)]++
	}
	n := len(s.flights)
	s.mu.Unlock()
	telemetry.DownloadsInFlight(context.Background(), int64(n))
}

// lookup finds a stored object. In hybrid mode a cache miss with a durable hit
// returns the durable body as promote, to be spooled into the cache like a
// download. A backend failure other than a miss is logged and treated as a miss.
// Misses are counted only with countMiss, so a retried lookup is not a second.
func (s *Store) lookup(ctx context.Context, key, file string, countMiss bool) (*Object, *download.File) {
	ctx, span := telemetry.Tracer().Start(ctx, "storage.exists")
	defer span.End()

	if s.cache != nil {
		f, info, err := s.cache.Get(ctx, key)
		if err == nil {
			telemetry.CacheHit(ctx, telemetry.LayerLocal)
			return &Object{ReadSeekCloser: f, Name: file, Size: info.Size, ModTime: info.LastModified, ContentType: contentType(file)}, nil
		}
		if countMiss {
			telemetry.CacheMiss(ctx, telemetry.LayerLocal)
		}
		if !errors.Is(err, ErrNotFound) {
			// Safe to log raw: a local filesystem error, no index URL in it.
			slog.ErrorContext(ctx, "Failed to read local cache", "error", err, "key", key)
		}
	}
	if s.durable == nil {
		return nil, nil
	}
	// Detached: a promotion outlives this request, and a hit is closed by the
	// server when the response is done.
	rctx := context.WithoutCancel(ctx)
	// The GET is the existence check: a hit whose body cannot be opened is a
	// miss here, never a 200 that dies after its headers.
	rc, info, err := s.durable.Get(rctx, key)
	if err != nil {
		if countMiss {
			telemetry.CacheMiss(ctx, telemetry.LayerRemote)
		}
		if !errors.Is(err, ErrNotFound) {
			// Safe to log raw: an object-store error; S3 credentials sign requests
			// and are never part of the endpoint URL.
			slog.ErrorContext(ctx, "Failed to read remote cache", "error", err, "key", key)
		}
		return nil, nil
	}
	telemetry.CacheHit(ctx, telemetry.LayerRemote)
	if s.cache == nil {
		// No ETag: a local hit sends none, and S3 hits match it header for header.
		return &Object{
			ReadSeekCloser: &rangedReader{
				open: func(off, end int64) (io.ReadCloser, error) { return s.durable.getRange(rctx, key, off, end) },
				size: info.Size, body: rc,
			},
			Name: file, Size: info.Size, ModTime: info.LastModified, ContentType: contentType(file),
		}, nil
	}
	return nil, &download.File{Target: download.Target{Size: info.Size}, Body: rc, Length: info.Size}
}

// lead runs one flight: fetch (or take the promotion source), spool, then
// commit. It holds one ref on the flight, released last.
func (s *Store) lead(ctx context.Context, f *flight, src *download.File) {
	defer s.leaders.Done()
	defer f.release()

	if src != nil {
		defer func() { _ = src.Body.Close() }()
		sp, written, err := s.spool(ctx, f, src, true)
		if err != nil {
			// Safe to log raw: a local or object-store error, no index URL in it.
			slog.ErrorContext(ctx, "Failed to promote a durable-store hit into the cache", "error", err, "key", f.key)
			return
		}
		s.commit(ctx, f, sp, written, true)
		return
	}

	// The resolution and fetch run on the request's context, so the index
	// resolution hangs off the request as it always has. The span is the
	// flight's: it covers resolution, fetch, spool and commit.
	// The leader resolves once; joiners take its outcome from the flight.
	fetchCtx := ctx
	ctx, span := telemetry.Tracer().Start(ctx, "upstream.fetch")
	defer span.End()
	started := time.Now()
	t, err := s.fetcher.Resolve(fetchCtx, f.pkg, f.file)
	if err != nil {
		s.unregister(f)
		f.finish(err)
		span.RecordError(err)
		return
	}
	var (
		sp      *os.File
		written int64
	)
	src, err = s.fetcher.Fetch(fetchCtx, t)
	if err == nil {
		defer func() { _ = src.Body.Close() }()
		sp, written, err = s.spool(ctx, f, src, false)
	} else {
		s.unregister(f)
		f.finish(newRedirectError(t, telemetry.RedirectFetchFailed, err))
	}
	// The fetch ends when the spool is verified (or failed); the commit and any
	// upload are not upstream time.
	outcome := telemetry.OutcomeOK
	if err != nil {
		outcome = telemetry.OutcomeError
	}
	telemetry.UpstreamFetch(ctx, time.Since(started), outcome)
	if err != nil {
		span.RecordError(err)
		// Debug: the server logs the failure per request. Kept for prefetch
		// flights, which have no request. Safe to log raw: download redacts its
		// errors at the source.
		slog.DebugContext(ctx, "Upstream download failed",
			"error", err, "package", f.pkg, "file", f.file)
		return
	}
	s.commit(ctx, f, sp, written, false)
}

// spool copies src into a spool file while hashing and verifies it. It knows
// nothing of storage modes. Readers tail the spool while it grows, the last
// byte held back until verified. On success it returns the spool, still open
// under the leader's ref, with the flight still registered and unfinished: the
// commit owns both. On any failure it unregisters the flight, fails every
// reader and removes the spool; a spool that cannot be created fails a
// download with a fetch_failed RedirectError (a promotion with ErrStorage).
func (s *Store) spool(ctx context.Context, f *flight, src *download.File, promote bool) (_ *os.File, written int64, err error) {
	ctx, span := telemetry.Tracer().Start(ctx, "storage.put")
	defer span.End()

	dir := s.spoolDir
	if s.cache != nil {
		dir = filepath.Dir(s.cache.buildPath(f.key))
	}
	var sp *os.File
	if err = os.MkdirAll(dir, 0750); err == nil {
		sp, err = os.CreateTemp(dir, tmpPrefix+"*")
	}
	if err != nil {
		err = fmt.Errorf("failed to create spool file: %w", err)
		span.RecordError(err)
		s.unregister(f)
		if promote {
			// The durable store holds the file: a failure, not a miss.
			f.finish(fmt.Errorf("%w: %w", ErrStorage, err))
		} else {
			f.finish(newRedirectError(src.Target, telemetry.RedirectFetchFailed, err))
		}
		return nil, 0, err
	}
	total := src.Size
	if total <= 0 {
		total = src.Length
	}
	f.start(sp, src.Size, total, quoteETag(src.SHA256))

	h := sha256.New()
	bufp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufp)
	for {
		n, rerr := src.Body.Read(*bufp)
		if n > 0 {
			if _, err = sp.Write((*bufp)[:n]); err != nil {
				break
			}
			h.Write((*bufp)[:n])
			written += int64(n)
			f.advance(written)
		}
		if rerr != nil {
			if rerr != io.EOF {
				err = rerr
			}
			break
		}
	}
	if err == nil {
		err = verify(ctx, f.key, src, hex.EncodeToString(h.Sum(nil)), written, total)
	}
	if err != nil {
		span.RecordError(err)
		s.unregister(f)
		f.finish(err)
		_ = os.Remove(sp.Name())
		return nil, 0, err
	}
	return sp, written, nil
}

// commitFunc makes a verified spool permanent; NewStore picks one from the
// tiers present. A commit's failures are logged, not reported: every reader
// already has, or will have, the verified bytes from the spool. The rename
// keeps the inode and sp stays open under the leader's ref, so an upload reads
// the same bytes whatever the cache does to the path meanwhile. promote marks
// a spool filled from the durable store.
type commitFunc func(ctx context.Context, f *flight, sp *os.File, written int64, promote bool)

// commitLocal renames the spool into the cache, unregisters the flight, then
// finishes it. Invariant: rename, unregister, finish, in that order, so the
// request after a reader's EOF is a cache hit rather than a join of a finished
// flight. An evicted flight is not renamed, and one evicted during the rename
// is removed again: DeletePrefix marks before it deletes, so either it sees the
// file or the re-check sees the mark.
func (s *Store) commitLocal(ctx context.Context, f *flight, sp *os.File, written int64, _ bool) {
	if s.evicted(f) {
		_ = os.Remove(sp.Name())
	} else if err := s.cache.commit(ctx, sp.Name(), f.key, written); err != nil {
		// Safe to log raw: a local filesystem error, no index URL in it.
		slog.ErrorContext(ctx, "Failed to commit download to cache", "error", err, "key", f.key)
		_ = os.Remove(sp.Name())
	} else if s.evicted(f) {
		_ = s.cache.remove(f.key, written)
	}
	s.unregisterCommitted(f)
	f.finish(nil)
}

// commitHybrid is commitLocal, then the upload to the durable store unless the
// spool was promoted from there. Invariant: rename, unregister, finish, then
// Put, so readers never wait on the durable store and a cache commit failure
// still uploads.
func (s *Store) commitHybrid(ctx context.Context, f *flight, sp *os.File, written int64, promote bool) {
	if promote {
		s.commitLocal(ctx, f, sp, written, promote)
		return
	}
	// Still evictable between the unregister and the end of the Put.
	s.mu.Lock()
	s.committing[f] = struct{}{}
	s.mu.Unlock()
	s.commitLocal(ctx, f, sp, written, promote)
	s.upload(ctx, f, sp, written)
	s.mu.Lock()
	delete(s.committing, f)
	s.mu.Unlock()
}

// commitS3 finishes the flight, uploads the spool, unregisters, then removes
// the spool. Invariant: finish, Put, unregister, remove, in that order, so a
// request during the upload joins the flight instead of fetching again, and
// the spool goes only once nothing can read it by path.
func (s *Store) commitS3(ctx context.Context, f *flight, sp *os.File, written int64, _ bool) {
	f.finish(nil)
	s.upload(ctx, f, sp, written)
	s.unregisterCommitted(f)
	_ = os.Remove(sp.Name())
}

// upload puts the verified spool into the durable store under its own span,
// unless the flight was evicted. The check and the start of the Put are one
// step under s.mu, so DeletePrefix either stops the Put or waits for it.
func (s *Store) upload(ctx context.Context, f *flight, sp *os.File, written int64) {
	s.mu.Lock()
	if f.evicted {
		s.mu.Unlock()
		return
	}
	f.uploaded = make(chan struct{})
	s.mu.Unlock()
	defer close(f.uploaded)

	ctx, span := telemetry.Tracer().Start(ctx, "storage.upload")
	defer span.End()
	if err := s.durable.Put(ctx, f.key, io.NewSectionReader(sp, 0, written), written, contentType(f.file)); err != nil {
		span.RecordError(err)
		// Safe to log raw: an object-store error; S3 credentials sign requests
		// and are never part of the endpoint URL.
		slog.ErrorContext(ctx, "Failed to upload download to object store", "error", err, "key", f.key)
	}
}

// verify checks the received bytes against what the index declared: the SHA-256
// when it supplied one, and the expected length (declared size, else upstream
// Content-Length) when one is known. With neither, the file is accepted
// unverified and that is logged.
func verify(ctx context.Context, key string, src *download.File, digest string, received, expected int64) error {
	var err error
	switch {
	case src.SHA256 != "" && !strings.EqualFold(src.SHA256, digest):
		err = fmt.Errorf("verification failed: %s: sha256 %s, expected %s", key, digest, src.SHA256)
	case expected >= 0 && received != expected:
		// A declared size was promised to the client as Content-Length.
		err = fmt.Errorf("verification failed: %s: %d bytes, expected %d", key, received, expected)
	case src.SHA256 == "" && expected < 0:
		slog.WarnContext(ctx, "⚠️ Caching unverified file: index supplied neither a hash nor a length",
			"key", key, "size", received)
	}
	if err != nil {
		telemetry.VerificationFailure(ctx)
	}
	return err
}

// InFlight lists the downloads running right now, oldest first.
func (s *Store) InFlight() []Progress {
	s.mu.Lock()
	out := make([]Progress, 0, len(s.flights))
	for _, f := range s.flights {
		f.mu.Lock()
		out = append(out, Progress{Package: f.pkg, File: f.file, Size: f.size, Bytes: f.written, Started: f.started, Requests: f.requests})
		f.mu.Unlock()
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// Snapshot lists every file in the cache, most recently accessed first, for the
// admin listing. With no cache (pure s3) it is empty: the durable store is not
// listed.
func (s *Store) Snapshot() []CacheEntry {
	if s.cache == nil {
		return nil
	}
	return s.cache.Snapshot()
}

// DeletePrefix removes every file under prefix from both the cache and the
// durable store, and reports how many went from the cache (from the durable
// store when there is no cache). A durable failure is an error, not
// best-effort: an object left there is promoted straight back.
//
// It first marks every flight under prefix evicted, so its commit stores
// nothing, and waits for any upload that already started, so the delete runs
// after it. That wait is bounded by the flight's context, which the Put runs
// on. If ctx ends first, DeletePrefix returns its error and deletes nothing:
// it never deletes while a Put may still land. A flight still spooling is
// never waited on.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	var uploads []chan struct{}
	s.mu.Lock()
	mark := func(f *flight) {
		if strings.HasPrefix(f.key, prefix) {
			f.evicted = true
			if f.uploaded != nil {
				uploads = append(uploads, f.uploaded)
			}
		}
	}
	for _, f := range s.flights {
		mark(f)
	}
	for f := range s.committing {
		mark(f)
	}
	s.mu.Unlock()
	for _, c := range uploads {
		select {
		case <-c:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	var (
		deleted    int
		cerr, derr error
	)
	if s.durable != nil {
		deleted, derr = s.durable.DeletePrefix(ctx, prefix)
	}
	if s.cache != nil {
		deleted, cerr = s.cache.DeletePrefix(ctx, prefix)
	}
	return deleted, errors.Join(cerr, derr)
}

// evicted reports whether DeletePrefix has marked f.
func (s *Store) evicted(f *flight) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return f.evicted
}

// Close waits for detached downloads, their uploads included, then closes both
// tiers.
func (s *Store) Close() error {
	s.leaders.Wait()
	var cerr, derr error
	if s.cache != nil {
		cerr = s.cache.Close()
	}
	if s.durable != nil {
		derr = s.durable.Close()
	}
	return errors.Join(cerr, derr)
}

// flight is one running download. refs counts the leader plus every opened
// reader; the spool fd closes when the last one lets go.
type flight struct {
	key, pkg, file string
	started        time.Time
	ready          chan struct{} // closed once the spool exists or the flight failed
	requests       int           // guarded by Store.mu
	evicted        bool          // guarded by Store.mu: DeletePrefix ran; the commit stores nothing
	uploaded       chan struct{} // guarded by Store.mu: made when the Put starts, closed when it ends

	mu      sync.Mutex
	cond    *sync.Cond
	spool   *os.File
	size    int64 // declared by the index, -1 when unknown
	total   int64 // expected length for the last-byte holdback, -1 when unknown
	etag    string
	written int64
	done    bool
	err     error
	refs    int
}

func (f *flight) start(sp *os.File, size, total int64, etag string) {
	f.mu.Lock()
	f.spool, f.size, f.total, f.etag = sp, size, total, etag
	f.mu.Unlock()
	close(f.ready)
}

func (f *flight) advance(n int64) {
	f.mu.Lock()
	f.written = n
	f.mu.Unlock()
	f.cond.Broadcast()
}

func (f *flight) finish(err error) {
	f.mu.Lock()
	f.done, f.err = true, err
	started := f.spool != nil
	f.mu.Unlock()
	f.cond.Broadcast()
	if !started {
		close(f.ready)
	}
}

// ref counts one more request on the flight. Caller holds Store.mu.
func (f *flight) ref() {
	f.requests++
	f.mu.Lock()
	f.refs++
	f.mu.Unlock()
}

func (f *flight) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs--
	if f.refs == 0 && f.spool != nil {
		_ = f.spool.Close()
	}
}

// open waits for the spool and returns a tail reader over it, holding a ref
// taken at join time. Only a flight that failed before its body started (no
// spool) fails here; once a body exists, even a failed one, the reader's first
// Read reports the failure, so a verification failure never becomes a
// redirect to the bytes that failed it.
func (f *flight) open(ctx context.Context) (*Object, error) {
	select {
	case <-f.ready:
	case <-ctx.Done():
		f.release()
		return nil, ctx.Err()
	}
	f.mu.Lock()
	err, started := f.err, f.spool != nil
	obj := &Object{ReadSeekCloser: &tail{f: f}, Name: f.file, Size: f.size, ETag: f.etag, ContentType: contentType(f.file), InFlight: true}
	f.mu.Unlock()
	if !started {
		f.release()
		return nil, err
	}
	return obj, nil
}

// tail reads a spool file as it grows. Until the download is verified, the byte
// at the expected length's end (or, with no known length, the last byte written)
// and everything past it are held back, so a reader never sees a complete body
// that then fails — not even when upstream sends more than it declared.
//
// ponytail: a reader blocked on a stalled upstream is not woken by its own
// client leaving; it waits for the leader to finish or fail. An upstream-fetch
// wait is bounded by GROXPI_DOWNLOAD_IDLE_TIMEOUT, after which the fetch fails.
type tail struct {
	f      *flight
	off    int64
	closed bool
}

func (t *tail) Read(p []byte) (int, error) {
	f := t.f
	f.mu.Lock()
	var avail int64
	for {
		avail = f.written - t.off
		if f.err != nil {
			f.mu.Unlock()
			return 0, f.err
		}
		if f.done {
			if avail <= 0 {
				f.mu.Unlock()
				return 0, io.EOF
			}
			break
		}
		if f.total >= 0 {
			avail = min(avail, f.total-1-t.off)
		} else {
			avail--
		}
		if avail > 0 {
			break
		}
		f.cond.Wait()
	}
	f.mu.Unlock()
	if int64(len(p)) > avail {
		p = p[:avail]
	}
	n, err := f.spool.ReadAt(p, t.off)
	t.off += int64(n)
	if err == io.EOF && n == len(p) {
		err = nil
	}
	return n, err
}

// Seek always fails: a download in flight is only read forward.
func (t *tail) Seek(int64, int) (int64, error) {
	return 0, errors.New("seek on a download in flight")
}

func (t *tail) Close() error {
	if !t.closed {
		t.closed = true
		t.f.release()
	}
	return nil
}

const (
	// firstWindow and maxWindow bound the ranged GETs a seek opens: a small
	// Range request transfers at most firstWindow from the object store, and a
	// long read doubles the window per request up to maxWindow.
	firstWindow = 1 << 20
	maxWindow   = 64 << 20
)

// rangedReader is a lazy io.ReadSeeker over an object-store body. It starts
// holding the whole-object body the lookup opened, at offset 0. Seek only
// records the offset; a Read anywhere else than the open body's position
// closes it and opens a bounded window from there, and a window read to its end
// opens the next, twice as large. A size probe fetches nothing, so
// http.ServeContent handles single and multi-range requests.
type rangedReader struct {
	open      func(off, end int64) (io.ReadCloser, error) // end inclusive
	size, off int64
	body      io.ReadCloser
	bodyOff   int64 // position of body
	window    int64 // size of the next sequential window
}

func (r *rangedReader) Read(p []byte) (int, error) {
	if r.off >= r.size {
		return 0, io.EOF
	}
	if r.body != nil && r.bodyOff != r.off {
		_ = r.Close()
		r.window = 0
	}
	for opened := false; ; {
		if r.body == nil {
			r.window = min(max(2*r.window, firstWindow), maxWindow)
			b, err := r.open(r.off, min(r.off+r.window, r.size)-1)
			if err != nil {
				return 0, err
			}
			r.body, r.bodyOff, opened = b, r.off, true
		}
		n, err := r.body.Read(p)
		r.off += int64(n)
		r.bodyOff = r.off
		if err == io.EOF && r.off < r.size {
			_ = r.Close()
			if n > 0 {
				return n, nil
			}
			if opened {
				return 0, io.ErrUnexpectedEOF // a fresh window with nothing in it
			}
			continue // this body is done; the next window carries on
		}
		return n, err
	}
}

func (r *rangedReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += r.off
	case io.SeekEnd:
		offset += r.size
	}
	if offset < 0 {
		return r.off, errors.New("negative seek on an object-store body")
	}
	r.off = offset
	return offset, nil
}

func (r *rangedReader) Close() error {
	if r.body == nil {
		return nil
	}
	err := r.body.Close()
	r.body = nil
	return err
}

// quoteETag quotes the index's bare hex digest as an entity-tag.
func quoteETag(etag string) string {
	if etag == "" {
		return ""
	}
	return `"` + etag + `"`
}

// contentType derives a content type from the distribution filename so the
// header can be sent before the first byte of the body.
func contentType(fileName string) string {
	name := strings.ToLower(fileName)
	switch {
	case strings.HasSuffix(name, ".whl"), strings.HasSuffix(name, ".zip"), strings.HasSuffix(name, ".egg"):
		return "application/zip"
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		return "application/gzip"
	case strings.HasSuffix(name, ".tar.bz2"):
		return "application/x-bzip2"
	case strings.HasSuffix(name, ".tar.xz"):
		return "application/x-xz"
	case strings.HasSuffix(name, ".tar"):
		return "application/x-tar"
	case strings.HasSuffix(name, ".metadata"):
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
