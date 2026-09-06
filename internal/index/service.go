// Package index answers one question: what files does a package have. It owns
// the upstream client, the bounded entry cache, extras-first resolution and the
// singleflight that deduplicates upstream index fetches. Everything else in
// groxpi asks it through Resolve or through the routes it registers.
package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

// upstream is the one-method view of Client that resolution needs. It is an
// interface because the tests have a second implementation.
type upstream interface {
	GetPackageFiles(ctx context.Context, index config.Index, packageName string) ([]FileInfo, error)
}

// Service is the index module. One instance serves every configured index.
type Service struct {
	cache    *IndexCache
	upstream upstream
	sf       singleflight.Group

	// indexes is the resolution order: extra indexes first, primary last. Each
	// carries its own TTL.
	indexes []config.Index

	// The root index is proxied rather than parsed, so it needs an HTTP client of
	// its own instead of the typed index client.
	rootURL    string
	rootClient *http.Client
}

// New builds the index module from configuration. Expired entries are swept at
// the index TTL cadence: an entry outlives its TTL by at most one interval, and
// nothing scans on the read path.
func New(cfg *config.Config) *Service {
	return newService(cfg, NewIndexCache(cfg.IndexCacheSize, cfg.IndexTTL), NewClient(cfg))
}

func newService(cfg *config.Config, cache *IndexCache, up upstream) *Service {
	return &Service{
		cache:    cache,
		upstream: up,
		indexes:  cfg.ResolutionOrder(),
		rootURL:  strings.TrimSuffix(cfg.IndexURL, "/") + "/",
		// ponytail: one fixed timeout for the whole proxied response. The root list
		// is large but a single upstream GET; a byte-rate budget can come later if a
		// slow index actually shows up.
		rootClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// Close stops the cache sweeper.
func (s *Service) Close() { s.cache.Close() }

// Invalidate drops a package's cached entry so the next request goes upstream.
func (s *Service) Invalidate(packageName string) { s.cache.InvalidatePackage(packageName) }

// Resolve returns the cache entry for a package — parsed file list and
// serialised bodies, produced together — using the index cache and
// deduplicating concurrent upstream fetches. It is the only path to the upstream
// package index. A miss on every configured index is ErrNotFound.
func (s *Service) Resolve(ctx context.Context, packageName string) (*Entry, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "index.resolve")
	defer span.End()

	if entry, found := s.cache.GetPackage(packageName); found {
		telemetry.CacheHit(ctx, telemetry.LayerIndex)
		return entry, nil
	}
	telemetry.CacheMiss(ctx, telemetry.LayerIndex)

	result, err, _ := s.sf.Do("package-files:"+packageName, func() (any, error) {
		// The fetch fills the cache for every waiter, so it must not die with
		// whichever client happened to trigger it.
		index, files, err := s.queryIndexes(context.WithoutCancel(ctx), packageName)
		if err != nil {
			return nil, err
		}
		body, err := encodePackageFiles(packageName, files)
		if err != nil {
			return nil, fmt.Errorf("failed to encode index for package %q: %w", packageName, err)
		}
		entry := NewPackageEntry(files, body)
		// Never cache an empty index: a transient upstream fault would otherwise
		// poison this package for the whole TTL. The TTL is the answering index's
		// own, so a fast-moving private index is not held stale by PyPI's.
		if len(files) > 0 {
			s.cache.SetPackage(packageName, entry, index.TTL)
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

			files, err := s.upstream.GetPackageFiles(queryCtx, index, packageName)
			result := resolutionResult(err)
			span.SetAttributes(attribute.String(telemetry.AttrIndexResult, result))
			telemetry.IndexResolution(queryCtx, redacted, result)

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

	for i, index := range indexes {
		select {
		case <-done[i]:
		case <-ctx.Done():
			return config.Index{}, nil, ctx.Err()
		}

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

// maxRootIndexBytes caps the proxied root index. See the read in proxyRoot.
// Documented in docs/api-endpoints.md, since a client meets it as a 502.
const maxRootIndexBytes int64 = 256 << 20

// rootResponse is one upstream root-index response, held only as long as it
// takes to answer the burst of clients that shared its fetch.
type rootResponse struct {
	status          int
	contentType     string
	contentEncoding string
	body            []byte
}

// proxyRoot fetches the upstream root index verbatim: the client's accepted
// content type and encoding are forwarded and the body is copied back
// undecoded. Nothing is cached — the full project list is tens of megabytes and
// is served through, not stored — but concurrent callers asking for the same
// representation still share one upstream fetch.
func (s *Service) proxyRoot(ctx context.Context, accept, acceptEncoding string) (*rootResponse, error) {
	result, err, _ := s.sf.Do("root:"+accept+"\x00"+acceptEncoding, func() (any, error) {
		// The fetch serves every waiter, so it must not die with whichever client
		// happened to trigger it; the HTTP client's own timeout bounds it.
		req, err := http.NewRequestWithContext(context.WithoutCancel(ctx), http.MethodGet, s.rootURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "groxpi/1.0.0")
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
		// serving a truncated index. Reading one byte past the cap is what tells
		// the two apart: a body of exactly the cap is served, the first byte over
		// is an error.
		//
		// ponytail: the overflow branch is untested — reaching it needs a
		// 256 MiB response body, which is not a test worth running.
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxRootIndexBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(body)) > maxRootIndexBytes {
			return nil, fmt.Errorf("root index exceeds the %d byte limit", maxRootIndexBytes)
		}
		return &rootResponse{
			status:          resp.StatusCode,
			contentType:     resp.Header.Get("Content-Type"),
			contentEncoding: resp.Header.Get("Content-Encoding"),
			body:            body,
		}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to proxy root index: %w", err)
	}

	root, ok := result.(*rootResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected root index result type %T", result)
	}
	return root, nil
}

// The wire shapes below are the typed replacement for the maps the index
// responses used to be built from. Fields are declared in the key order
// encoding/json emitted for those maps (alphabetical), so response bytes are
// unchanged.

type wireMeta struct {
	APIVersion string `json:"api-version"`
}

type wireFile struct {
	Filename       string            `json:"filename"`
	Hashes         map[string]string `json:"hashes,omitempty"`
	RequiresPython string            `json:"requires-python,omitempty"`
	URL            string            `json:"url"`
	Yanked         bool              `json:"yanked,omitempty"`
	YankedReason   string            `json:"yanked-reason,omitempty"`
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
			URL:            ProxyFileURL(packageName, file.Name),
		}
		if file.IsYanked() {
			wf.Yanked = true
			wf.YankedReason = file.GetYankedReason()
		}
		body.Files = append(body.Files, wf)
	}
	return json.Marshal(body)
}

// ProxyFileURL is the path this proxy serves a package file at.
func ProxyFileURL(packageName, fileName string) string {
	return "/simple/" + packageName + "/" + fileName
}
