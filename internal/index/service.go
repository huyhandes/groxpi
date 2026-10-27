// Package index answers one question: what files does a package have. It owns
// the upstream client, the bounded entry cache, extras-first resolution and the
// singleflight that deduplicates upstream index fetches. Everything else in
// groxpi asks it through Resolve; the server renders the answer.
package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/singleflight"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/telemetry"
)

// JSONContentType is the PEP 691 media type of every index body served as JSON.
const JSONContentType = "application/vnd.pypi.simple.v1+json"

// upstream is the one-method view of client that resolution needs. It is an
// interface because the tests have a second implementation (fakeUpstream).
type upstream interface {
	GetPackageFiles(ctx context.Context, index config.Index, packageName string) ([]FileInfo, error)
}

// Service is the index module. One instance serves every configured index.
type Service struct {
	cache    *indexCache
	upstream upstream
	sf       singleflight.Group

	// indexes is the resolution order: extra indexes first, primary last. Each
	// carries its own TTL.
	indexes []config.Index
	// missTTL is how long "not found on every index" is remembered: the shortest
	// TTL in the resolution order, so a package newly published to any index is
	// hidden no longer than that index's own entries would be stale.
	missTTL time.Duration

	// The root index is proxied rather than parsed, so it needs an HTTP client of
	// its own instead of the typed index client.
	rootURL    string
	rootClient *http.Client

	// transport is the one upstream transport, shared by the index client, the
	// root client and download (through Transport).
	transport *http.Transport
}

// New builds the index module from configuration. Expired entries are swept at
// the shortest positive TTL in the resolution order: an entry outlives its TTL
// by at most one interval, and nothing scans on the read path.
func New(cfg *config.Config) *Service {
	transport := newTransport(cfg)
	return newService(cfg, newIndexCache(cfg.IndexCacheSize, sweepInterval(cfg)), newClient(cfg, transport), transport)
}

// sweepInterval is the shortest positive TTL across every configured index, or
// 0 (no sweeper) when none is positive: a TTL of 0 caches nothing to sweep.
func sweepInterval(cfg *config.Config) time.Duration {
	var every time.Duration
	for _, index := range cfg.ResolutionOrder() {
		if index.TTL > 0 && (every == 0 || index.TTL < every) {
			every = index.TTL
		}
	}
	return every
}

func newService(cfg *config.Config, cache *indexCache, up upstream, transport *http.Transport) *Service {
	indexes := cfg.ResolutionOrder()
	missTTL := indexes[0].TTL
	for _, index := range indexes[1:] {
		missTTL = min(missTTL, index.TTL)
	}
	return &Service{
		cache:    cache,
		upstream: up,
		indexes:  indexes,
		missTTL:  missTTL,
		rootURL:  strings.TrimSuffix(cfg.IndexURL, "/") + "/",
		// ponytail: one fixed timeout for the whole proxied response. The root list
		// is large but a single upstream GET; a byte-rate budget can come later if a
		// slow index actually shows up.
		rootClient: &http.Client{Transport: transport, Timeout: 60 * time.Second},
		transport:  transport,
	}
}

// Transport is the upstream transport index built from configuration. download
// sends file requests through it, so TLS settings and connection pooling are
// the same for every upstream call.
func (s *Service) Transport() http.RoundTripper { return s.transport }

// Close stops the cache sweeper.
func (s *Service) Close() { s.cache.close() }

// Invalidate drops a package's cached entry, a cached miss included, so the
// next request goes upstream. Any spelling of the name drops the one normalised
// entry.
func (s *Service) Invalidate(packageName string) {
	s.cache.invalidate(NormalizeName(packageName))
}

// Resolve returns the cache entry for a package — parsed file list and
// serialised bodies, produced together — using the index cache and
// deduplicating concurrent upstream fetches. It is the only path to the upstream
// package index. A miss on every configured index is ErrNotFound, and is itself
// cached for the shortest index TTL. The name is normalised first, so every
// spelling shares one entry and one upstream query.
func (s *Service) Resolve(ctx context.Context, packageName string) (*Entry, error) {
	packageName = NormalizeName(packageName)
	ctx, span := telemetry.Tracer().Start(ctx, "index.resolve")
	defer span.End()

	if entry, found := s.cache.get(packageName); found {
		telemetry.CacheHit(ctx, telemetry.LayerIndex)
		if entry.missing {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, packageName)
		}
		return entry, nil
	}
	telemetry.CacheMiss(ctx, telemetry.LayerIndex)

	result, err := s.shared(ctx, "package-files:"+packageName, func() (any, error) {
		// The fetch fills the cache for every waiter, so it must not die with
		// whichever client happened to trigger it.
		ctx := context.WithoutCancel(ctx)
		index, files, err := s.queryIndexes(ctx, packageName)
		if errors.Is(err, ErrNotFound) {
			// Every index answered and none has the package. Remembering that is
			// what stops a CI fleet retrying a typo from querying every upstream
			// on every request. Errors are not remembered: an unreachable index
			// says nothing about the package.
			s.cache.set(packageName, newMissEntry(), s.missTTL)
			return nil, err
		}
		// A failure is logged here, once per lookup: every waiter may have left
		// by now, and the ones still waiting share this one failure.
		fail := func(err error) (any, error) {
			slog.ErrorContext(ctx, "Failed to fetch package files",
				"error", config.RedactErrorText(err), "package", packageName)
			return nil, err
		}
		if err != nil {
			return fail(err)
		}
		body, err := encodePackageFiles(packageName, files)
		if err != nil {
			return fail(fmt.Errorf("failed to encode index for package %q: %w", packageName, err))
		}
		entry := newPackageEntry(files, body)
		// Never cache an empty index: a transient upstream fault would otherwise
		// poison this package for the whole TTL. The TTL is the answering index's
		// own, so a fast-moving private index is not held stale by PyPI's.
		if len(files) > 0 {
			s.cache.set(packageName, entry, index.TTL)
		}
		return entry, nil
	})
	if err != nil {
		return nil, err
	}

	entry, ok := result.(*Entry)
	if !ok {
		return nil, fmt.Errorf("unexpected index result type %T for package %q", result, packageName)
	}
	return entry, nil
}

// shared runs fn once per key for every concurrent caller, and lets each caller
// stop waiting when its own context ends. fn must run detached from ctx: it
// carries on for the callers still waiting, and fills the cache for later ones.
// fn runs on its own goroutine, where a panic would kill the process, so a panic
// comes back as an error instead.
func (s *Service) shared(ctx context.Context, key string, fn func() (any, error)) (any, error) {
	guarded := func() (v any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = panicError(ctx, rec)
			}
		}()
		return fn()
	}
	select {
	case r := <-s.sf.DoChan(key, guarded):
		return r.Val, r.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// errPanic marks a lookup that panicked, e.g. a parser bug fed upstream data.
var errPanic = errors.New("index lookup panicked")

// panicError logs a recovered panic with its stack and turns it into an error,
// so the caller answers a 5xx instead of the process dying with it.
func panicError(ctx context.Context, rec any) error {
	err := fmt.Errorf("%w: %v", errPanic, rec)
	slog.ErrorContext(ctx, "Index lookup panicked",
		"error", config.RedactErrorText(err), "stack", string(debug.Stack()))
	return err
}

// resolutionResult classifies one index's answer for the resolution metric. A
// cancellation is not an error: it means a higher-priority index answered first
// and this query was abandoned on purpose.
func resolutionResult(err error) string {
	switch {
	case err == nil:
		return telemetry.ResultHit
	case errors.Is(err, ErrNotFound):
		return telemetry.ResultMiss
	case errors.Is(err, context.Canceled):
		return telemetry.ResultCancelled
	default:
		return telemetry.ResultError
	}
}

// indexAnswer is one index's reply to one package query.
type indexAnswer struct {
	files []FileInfo
	err   error
}

// queryIndexes asks the configured indexes for a package and returns the answer
// of the highest-priority index that has it — extra indexes before the primary,
// in configured order. The file list is returned whole: results from different
// indexes are never merged, unioned or deduplicated, which is what makes
// dependency confusion impossible rather than something to detect. See
// docs/adr/0001-extras-first-index-resolution.md.
//
// The extras are queried concurrently. The primary is not: it is only asked once
// every extra has missed, because a speculative query tells the public index
// which internal package names exist even when its answer is thrown away.
func (s *Service) queryIndexes(ctx context.Context, packageName string) (config.Index, []FileInfo, error) {
	if len(s.indexes) == 0 {
		return config.Index{}, nil, fmt.Errorf("%w: %s (no index configured)", ErrNotFound, packageName)
	}
	extras, primary := s.indexes[:len(s.indexes)-1], s.indexes[len(s.indexes)-1]

	if len(extras) > 0 {
		index, files, err := s.queryConcurrently(ctx, extras, packageName)
		// Only a miss on every extra falls through to the primary. A success is the
		// answer, and a failure aborts: if the private index is unreachable, serving
		// the public index's files under an internal name is exactly the outcome the
		// ordering exists to prevent.
		if !errors.Is(err, ErrNotFound) {
			return index, files, err
		}
	}
	return s.queryConcurrently(ctx, []config.Index{primary}, packageName)
}

// queryConcurrently queries indexes in parallel and returns the answer of the
// first one in slice order that has the package. Selection waits for each index in
// turn, so arrival order cannot decide: a fast index never answers for a name a
// slower higher-priority index also has. Once an index has answered, the
// lower-priority queries still in flight are cancelled.
func (s *Service) queryConcurrently(ctx context.Context, indexes []config.Index, packageName string) (config.Index, []FileInfo, error) {
	answers := make([]indexAnswer, len(indexes))
	done := make([]chan struct{}, len(indexes))
	cancels := make([]context.CancelFunc, len(indexes))

	for i, index := range indexes {
		queryCtx, cancel := context.WithCancel(ctx)
		done[i], cancels[i] = make(chan struct{}), cancel
		go func(i int, index config.Index) {
			defer close(done[i])

			// One span and one counted outcome per index consulted, so a slow or
			// unreachable private index is visible without reading the code. The
			// identity is the redacted URL: a raw index URL must never reach a span
			// attribute or a metric label.
			redacted := index.Redacted()
			queryCtx, span := telemetry.Tracer().Start(queryCtx, "index.query",
				trace.WithAttributes(attribute.String(telemetry.AttrIndex, redacted)))
			defer span.End()

			// The upstream answer is parsed here, on a goroutine of its own:
			// a panic must become this index's error, not kill the process.
			// It runs before span.End, so a panicking query is recorded as an
			// error too.
			result := telemetry.ResultError
			defer func() {
				if rec := recover(); rec != nil {
					answers[i] = indexAnswer{err: panicError(ctx, rec)}
				}
				span.SetAttributes(attribute.String(telemetry.AttrIndexResult, result))
				telemetry.IndexResolution(queryCtx, redacted, result)
			}()

			files, err := s.upstream.GetPackageFiles(queryCtx, index, packageName)
			result = resolutionResult(err)

			// Written before the channel closes and read after: no other
			// synchronisation is needed.
			answers[i] = indexAnswer{files: files, err: err}
		}(i, index)
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()

	// ctx carries no cancellation (Resolve detaches it so one client cannot
	// abort a shared resolution), so each wait ends only when its query does;
	// the HTTP client's timeout bounds that.
	for i, index := range indexes {
		<-done[i]

		switch answer := answers[i]; {
		case answer.err == nil:
			slog.DebugContext(ctx, "Package resolved from index",
				"package", packageName,
				"index", index.Redacted(),
				"files", len(answer.files))
			return index, answer.files, nil
		case errors.Is(answer.err, ErrNotFound):
			slog.DebugContext(ctx, "Package not on index, trying the next",
				"package", packageName,
				"index", index.Redacted())
		default:
			return config.Index{}, nil, fmt.Errorf("index %s failed for package %q: %w",
				index.Redacted(), packageName, answer.err)
		}
	}

	return config.Index{}, nil, fmt.Errorf("%w: %s", ErrNotFound, packageName)
}

// maxRootIndexBytes caps the proxied root index. See the read in ProxyRoot.
// Documented in docs/api-endpoints.md, since a client meets it as a 502.
const maxRootIndexBytes int64 = 256 << 20

// RootResponse is one upstream root-index response, held only as long as it
// takes to answer the burst of clients that shared its fetch.
type RootResponse struct {
	Status          int
	ContentType     string
	ContentEncoding string
	Body            []byte
}

// ProxyRoot fetches the upstream root index verbatim: the client's accepted
// content type and encoding are forwarded and the body is copied back
// undecoded. Nothing is cached — the full project list is tens of megabytes and
// is served through, not stored — but concurrent callers asking for the same
// representation still share one upstream fetch.
func (s *Service) ProxyRoot(ctx context.Context, accept, acceptEncoding string) (*RootResponse, error) {
	result, err := s.shared(ctx, "root:"+accept+"\x00"+acceptEncoding, func() (any, error) {
		// The fetch serves every waiter, so it must not die with whichever client
		// happened to trigger it; the HTTP client's own timeout bounds it.
		ctx := context.WithoutCancel(ctx)
		root, err := s.fetchRoot(ctx, accept, acceptEncoding)
		if err != nil {
			// Logged here, once per fetch, as for a package lookup.
			slog.ErrorContext(ctx, "Failed to proxy root index", "error", config.RedactErrorText(err))
		}
		return root, err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to proxy root index: %w", err)
	}

	root, ok := result.(*RootResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected root index result type %T", result)
	}
	return root, nil
}

// fetchRoot is ProxyRoot's one upstream GET, run once for every waiter.
func (s *Service) fetchRoot(ctx context.Context, accept, acceptEncoding string) (*RootResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.rootURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", config.UserAgent)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	// Setting this explicitly also stops net/http decompressing the body, which
	// is what makes the pass-through byte-level.
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}

	resp, err := s.rootClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	// The whole response is held in memory to be handed to every waiter, so an
	// upstream that answers this route with an endless body would otherwise be
	// an unauthenticated way to exhaust the process's memory. The ceiling is far
	// above PyPI's own project list; exceeding it fails loudly rather than
	// serving a truncated index.
	body, err := readCapped(resp.Body, maxRootIndexBytes)
	if err != nil {
		return nil, err
	}
	return &RootResponse{
		Status:          resp.StatusCode,
		ContentType:     resp.Header.Get("Content-Type"),
		ContentEncoding: resp.Header.Get("Content-Encoding"),
		Body:            body,
	}, nil
}

// The wire shapes below are the PEP 691 JSON this proxy serves for a package.

type wireMeta struct {
	APIVersion string `json:"api-version"`
}

type wireFile struct {
	Filename string `json:"filename"`
	// PEP 691: hashes MUST be present, even when empty.
	Hashes         map[string]string `json:"hashes"`
	RequiresPython string            `json:"requires-python,omitempty"`
	URL            string            `json:"url"`
	// PEP 691: yanked is true or the non-empty reason string; omitted when
	// the file is not yanked.
	Yanked any `json:"yanked,omitempty"`
	// PEP 714: JSON servers emit core-metadata only ("should not emit the old
	// keys in JSON"); the HTML page carries both attributes.
	CoreMetadata any `json:"core-metadata,omitempty"`
}

type wireFiles struct {
	Files []wireFile `json:"files"`
	Meta  wireMeta   `json:"meta"`
	Name  string     `json:"name"`
}

// encodePackageFiles marshals a package's file list, rewriting every URL to
// point at this proxy.
func encodePackageFiles(packageName string, files []FileInfo) ([]byte, error) {
	body := wireFiles{
		Files: make([]wireFile, 0, len(files)),
		Meta:  wireMeta{APIVersion: "1.0"},
		Name:  packageName,
	}
	for _, file := range files {
		wf := wireFile{
			Filename:       file.Name,
			Hashes:         file.Hashes,
			RequiresPython: file.RequiresPython,
			URL:            proxyFileURL(packageName, file.Name),
		}
		if wf.Hashes == nil {
			wf.Hashes = map[string]string{}
		}
		if file.IsYanked() {
			wf.Yanked = true
			if reason := file.GetYankedReason(); reason != "" {
				wf.Yanked = reason
			}
		}
		if hashes, ok := file.Metadata(); ok {
			wf.CoreMetadata = true
			if len(hashes) > 0 {
				wf.CoreMetadata = hashes
			}
		}
		body.Files = append(body.Files, wf)
	}
	return json.Marshal(body)
}

// proxyFileURL is the path this proxy serves a package file at, each segment
// percent-encoded so a name with "#" or "?" is not cut short by the client.
func proxyFileURL(packageName, fileName string) string {
	return "/simple/" + url.PathEscape(packageName) + "/" + url.PathEscape(fileName)
}
