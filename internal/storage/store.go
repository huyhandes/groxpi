package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/huyhandes/groxpi/internal/download"
	"github.com/huyhandes/groxpi/internal/telemetry"
)

// Fetcher opens a package file upstream. It is the one cross-module interface
// storage consumes; download.Service implements it and the tests fake it.
type Fetcher interface {
	Fetch(ctx context.Context, pkg, file string) (*download.File, error)
}

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

// Key is the storage key of a package file. An empty file names the package's
// prefix.
func Key(pkg, file string) string { return "packages/" + pkg + "/" + file }

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
	spoolDir string // spool directory when there is no cache

	mu      sync.Mutex
	flights map[string]*flight
	leaders sync.WaitGroup
}

// NewStore builds a store over a cache, a durable store, or both (at least one
// must be non-nil). spoolDir holds the spool files when there is no cache (pure
// s3); crash leftovers in it are removed by the same rule the cache's scan
// applies.
func NewStore(cache *LocalStorage, durable *S3Storage, fetcher Fetcher, spoolDir string) *Store {
	s := &Store{cache: cache, durable: durable, fetcher: fetcher, spoolDir: spoolDir, flights: map[string]*flight{}}
	if cache == nil {
		reapSpool(spoolDir)
	}
	return s
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

// Open opens a package file. On a miss it fetches upstream when fetch is set,
// and otherwise returns ErrNotFound (redirect mode).
func (s *Store) Open(ctx context.Context, pkg, file string, fetch bool) (*Object, error) {
	key := Key(pkg, file)
	if f := s.join(key); f != nil {
		return f.open(ctx)
	}

	obj, promote := s.lookup(ctx, key, file)
	if obj != nil {
		return obj, nil
	}
	if promote == nil && !fetch {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}

	// ponytail: a leader committing between lookup and this lock is missed and
	// the file fetched twice; re-check the backend here if that ever shows up.
	s.mu.Lock()
	f := s.flights[key]
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

func (s *Store) join(key string) *flight {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.flights[key]
	if f != nil {
		f.ref()
	}
	return f
}

func (s *Store) unregister(f *flight) {
	s.mu.Lock()
	delete(s.flights, f.key)
	n := len(s.flights)
	s.mu.Unlock()
	telemetry.DownloadsInFlight(context.Background(), int64(n))
}

// lookup finds a stored object. In hybrid mode a cache miss with a durable hit
// returns the durable body as promote, to be spooled into the cache like a
// download. A backend failure other than a miss is logged and treated as a miss.
func (s *Store) lookup(ctx context.Context, key, file string) (*Object, *download.File) {
	ctx, span := telemetry.Tracer().Start(ctx, "storage.exists")
	defer span.End()

	if s.cache != nil {
		rc, info, err := s.cache.Get(ctx, key)
		if err == nil {
			telemetry.CacheHit(ctx, telemetry.LayerLocal)
			return &Object{ReadSeekCloser: rc.(*os.File), Name: file, Size: info.Size, ModTime: info.LastModified, ContentType: contentType(file)}, nil
		}
		telemetry.CacheMiss(ctx, telemetry.LayerLocal)
		if !errors.Is(err, ErrNotFound) {
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
		s.remoteMiss(ctx, key, err)
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

func (s *Store) remoteMiss(ctx context.Context, key string, err error) {
	telemetry.CacheMiss(ctx, telemetry.LayerRemote)
	if !errors.Is(err, ErrNotFound) {
		slog.ErrorContext(ctx, "Failed to read remote cache", "error", err, "key", key)
	}
}

// lead runs one flight: fetch (or take the promotion source), spool, verify,
// commit. It holds one ref on the flight, released last.
func (s *Store) lead(ctx context.Context, f *flight, src *download.File) {
	defer s.leaders.Done()
	defer f.release()

	if src != nil {
		defer func() { _ = src.Body.Close() }()
		if err := s.fill(ctx, f, src, true); err != nil {
			slog.ErrorContext(ctx, "Failed to promote a durable-store hit into the cache", "error", err, "key", f.key)
		}
		return
	}

	// The fetch itself runs on the request's context, so the index resolution
	// inside it hangs off the request as it always has; the span covers it.
	fetchCtx := ctx
	ctx, span := telemetry.Tracer().Start(ctx, "upstream.fetch")
	defer span.End()
	started := time.Now()
	src, err := s.fetcher.Fetch(fetchCtx, f.pkg, f.file)
	if err == nil {
		defer func() { _ = src.Body.Close() }()
		err = s.fill(ctx, f, src, false)
	} else {
		s.unregister(f)
		f.finish(err)
	}
	outcome := telemetry.OutcomeOK
	if err != nil {
		outcome = telemetry.OutcomeError
		span.RecordError(err)
		slog.ErrorContext(ctx, "Upstream download failed",
			"error", err, "package", f.pkg, "file", f.file)
	}
	telemetry.UpstreamFetch(ctx, time.Since(started), outcome)
}

// fill copies src into a spool file while hashing, then commits it (rename into
// the cache, upload to the durable store, or both) or, on any failure, fails
// every reader and removes the spool.
func (s *Store) fill(ctx context.Context, f *flight, src *download.File, promote bool) (err error) {
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
		s.unregister(f)
		f.finish(fmt.Errorf("failed to create spool file: %w", err))
		return err
	}
	total := src.Size
	if total <= 0 {
		total = src.Length
	}
	f.start(sp, src.Size, total, quoteETag(src.SHA256))

	h := sha256.New()
	bufp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufp)
	var written int64
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
		return err
	}

	// Commit failures are logged, not reported: every reader already has, or
	// will have, the verified bytes from the spool. The rename keeps the inode,
	// and sp stays open under the leader's ref, so the upload below reads the
	// same bytes whatever the cache does to the path meanwhile.
	if s.cache != nil {
		if cerr := s.cache.commit(ctx, sp.Name(), f.key, written); cerr != nil {
			slog.ErrorContext(ctx, "Failed to commit download to cache", "error", cerr, "key", f.key)
			_ = os.Remove(sp.Name())
		}
		// Unregister before finishing, so the request after EOF is a cache hit.
		s.unregister(f)
		f.finish(nil)
	} else {
		f.finish(nil)
	}
	// A promotion came from the durable store; it is not sent back there.
	if s.durable != nil && !promote {
		if uerr := s.durable.Put(ctx, f.key, io.NewSectionReader(sp, 0, written), written, contentType(f.file)); uerr != nil {
			slog.ErrorContext(ctx, "Failed to upload download to object store", "error", uerr, "key", f.key)
		}
	}
	if s.cache == nil {
		s.unregister(f)
		_ = os.Remove(sp.Name())
	}
	return nil
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
func (s *Store) DeletePrefix(ctx context.Context, prefix string) (int, error) {
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
// client leaving; it waits for the leader to finish or fail.
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
