// Package download serves one package file: cached from storage when it is
// there, otherwise fetched upstream once, streamed to the client and cached on
// the way through. It owns the miss pipeline, the singleflight that makes
// concurrent requests for one file a single upstream download, and the registry
// that shows which downloads are running.
package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/index"
	"github.com/huyhandes/groxpi/internal/storage"
	"github.com/huyhandes/groxpi/internal/telemetry"
)

// ErrCachingDisabled reports that GROXPI_DOWNLOAD_TIMEOUT is 0, so every
// download is a redirect and nothing can be warmed into the cache.
var ErrCachingDisabled = errors.New("caching is disabled (download timeout is 0)")

// ErrNotListed reports that the index does not list the requested file.
var ErrNotListed = errors.New("file not listed by the index")

// Resolver is what this module needs from the index module. It is an interface
// because the tests have a second implementation.
type Resolver interface {
	Resolve(ctx context.Context, packageName string) (*index.Entry, error)
}

// ServeAction is the outcome of the miss pipeline: what the transport has to
// do to satisfy the request.
type ServeAction int

const (
	// ActionNotFound means the file is neither cached nor known to the index.
	ActionNotFound ServeAction = iota
	// ActionFromStorage means the object is cached in the storage backend.
	ActionFromStorage
	// ActionStreamAndCache means the file must be fetched upstream, streamed to
	// the client and cached on the way through.
	ActionStreamAndCache
	// ActionRedirect means the client should be sent to the upstream URL.
	ActionRedirect
)

// ServePlan is the decision Plan reached for one package file. It is a value:
// nothing has been written to the client when it is returned.
type ServePlan struct {
	Action      ServeAction
	PackageName string
	FileName    string
	StorageKey  string
	URL         string        // ActionStreamAndCache / ActionRedirect
	ContentType string        // derived from the filename
	ETag        string        // from the index hashes, empty if unknown
	SHA256      string        // from the index hashes, empty if unknown
	Size        int64         // from the PyPI index, -1 if unknown
	Timeout     time.Duration // ActionStreamAndCache: time-to-first-byte budget
}

// Service is the download module.
type Service struct {
	storage         storage.Storage
	resolver        Resolver
	downloader      *teeDownloader
	sf              singleflight.Group
	inflight        inflight
	downloadTimeout time.Duration
}

// New builds the download module over a storage backend and an index resolver.
func New(cfg *config.Config, st storage.Storage, resolver Resolver) *Service {
	// The configured budget is the time-to-first-byte budget; see newTeeDownloader.
	streamTimeout := cfg.DownloadTimeout
	if streamTimeout <= 0 {
		streamTimeout = 5 * time.Minute
	}
	return &Service{
		storage:         st,
		resolver:        resolver,
		downloader:      newTeeDownloader(st, &http.Client{Timeout: streamTimeout}),
		downloadTimeout: cfg.DownloadTimeout,
	}
}

// StorageKey is where a package file lives in storage. The package prefix is
// what makes evicting a package a prefix delete rather than a second index.
func StorageKey(packageName, fileName string) string {
	return "packages/" + packageName + "/" + fileName
}

// Plan decides how a package file should be served. It performs no client I/O.
// A nil error with ActionNotFound means the index does not list the file; a
// non-nil error means the package index itself could not be resolved.
func (s *Service) Plan(ctx context.Context, packageName, fileName string) (ServePlan, error) {
	plan := ServePlan{
		Action:      ActionNotFound,
		PackageName: packageName,
		FileName:    fileName,
		StorageKey:  StorageKey(packageName, fileName),
		Size:        -1,
	}

	existsCtx, existsSpan := telemetry.Tracer().Start(ctx, "storage.exists")
	exists, err := s.storage.Exists(existsCtx, plan.StorageKey)
	existsSpan.End()
	if err != nil {
		// A backend hiccup must not fail the request: fall through to upstream.
		slog.ErrorContext(ctx, "Failed to check storage", "error", err, "key", plan.StorageKey)
	} else if exists {
		slog.DebugContext(ctx, "✅ Serving from storage cache", "package", packageName, "file", fileName)
		plan.Action = ActionFromStorage
		return plan, nil
	}

	entry, err := s.resolver.Resolve(ctx, packageName)
	if err != nil {
		return plan, fmt.Errorf("failed to resolve index for package %q: %w", packageName, err)
	}

	info, ok := findFile(entry.Files, fileName)
	switch {
	case ok:
		plan.URL = info.URL
		if info.Size > 0 {
			plan.Size = info.Size
		}
		plan.SHA256 = info.Hashes["sha256"]
	case strings.HasSuffix(fileName, metadataSuffix):
		// PEP 658: the metadata file lives at the distribution's URL plus
		// ".metadata" and is only served when the index advertises it.
		base, found := findFile(entry.Files, strings.TrimSuffix(fileName, metadataSuffix))
		if !found {
			return plan, nil
		}
		hashes, advertised := base.Metadata()
		if !advertised {
			return plan, nil
		}
		plan.URL = metadataURL(base.URL)
		plan.SHA256 = hashes["sha256"]
	default:
		return plan, nil
	}
	plan.ContentType = contentTypeForFile(fileName)
	plan.ETag = quoteETag(plan.SHA256)

	if s.downloadTimeout <= 0 {
		slog.DebugContext(ctx, "Download timeout is 0, redirecting directly to PyPI",
			"package", packageName,
			"file", fileName)
		telemetry.Redirect(ctx, telemetry.RedirectCachingDisabled)
		plan.Action = ActionRedirect
		return plan, nil
	}

	plan.Action = ActionStreamAndCache
	plan.Timeout = s.downloadTimeout
	return plan, nil
}

// Fetch performs the deduplicated upstream fetch for plan, streaming the body to
// dst while caching it into storage. Exactly one caller per storage key does the
// work: it gets led=true plus the StreamResult. Concurrent callers for the same
// key block until the leader finishes and get led=false with nothing written to
// their dst — they must re-Plan and serve the now-cached object.
func (s *Service) Fetch(ctx context.Context, plan ServePlan, dst io.Writer) (*StreamResult, bool, error) {
	// The download populates the cache for every waiting request, so it must not
	// die with the client that happened to trigger it. It carries no deadline
	// either: the configured budget applies only until upstream response headers
	// arrive, and the downloader enforces that itself.
	fetchCtx := context.WithoutCancel(ctx)

	progress := s.inflight.join(plan)

	// Only the leader runs the closure, so this is written on the leader's own
	// stack before Do returns and read after it returns — no synchronization
	// needed.
	led := false

	value, err, _ := s.sf.Do(plan.StorageKey, func() (any, error) {
		led = true
		telemetry.DownloadsInFlight(fetchCtx, s.inflight.count())
		defer func() {
			s.inflight.leave(plan.StorageKey)
			telemetry.DownloadsInFlight(fetchCtx, s.inflight.count())
		}()

		dlCtx, span := telemetry.Tracer().Start(fetchCtx, "upstream.fetch")
		defer span.End()

		slog.InfoContext(dlCtx, "🚀 Starting streaming download with simultaneous cache",
			"package", plan.PackageName,
			"file", plan.FileName,
			"file_url", config.RedactURL(plan.URL),
			"file_size", plan.Size,
			"timeout", plan.Timeout)

		started := time.Now()
		result, err := s.downloader.DownloadAndStream(dlCtx, plan.URL, plan.StorageKey,
			&countingWriter{w: dst, n: &progress.bytes}, Expectation{
				SHA256: plan.SHA256,
				Size:   plan.Size,
			})
		outcome := telemetry.OutcomeOK
		if err != nil {
			outcome = telemetry.OutcomeError
			span.RecordError(err)
		}
		telemetry.UpstreamFetch(dlCtx, time.Since(started), outcome)
		return result, err
	})

	if !led {
		slog.DebugContext(ctx, "🔄 Waited for in-flight download", "package", plan.PackageName, "file", plan.FileName)
		if err != nil {
			return nil, false, fmt.Errorf("shared download of %q failed: %w", plan.StorageKey, err)
		}
		return nil, false, nil
	}

	if err != nil {
		return nil, true, fmt.Errorf("failed to stream %q: %w", config.RedactURL(plan.URL), err)
	}

	// A leader that reports no error must have produced a result; anything else
	// is a broken downloader, not something to dereference and find out.
	result, ok := value.(*StreamResult)
	if !ok || result == nil {
		return nil, true, fmt.Errorf("download of %q returned no result (%T)", plan.StorageKey, value)
	}
	return result, true, nil
}

// PlanAfterFetch decides how to serve a request that waited on another request's
// download. The object should now be cached, so it is re-planned onto storage;
// when it is not there (async or failed cache write) the client goes upstream.
func (s *Service) PlanAfterFetch(ctx context.Context, plan ServePlan) ServePlan {
	next, err := s.Plan(ctx, plan.PackageName, plan.FileName)
	if err == nil && next.Action == ActionFromStorage {
		slog.DebugContext(ctx, "✅ Serving from storage after coordinated download",
			"package", plan.PackageName,
			"file", plan.FileName)
		return next
	}

	slog.DebugContext(ctx, "⏭️ Redirecting to PyPI after download coordination",
		"package", plan.PackageName,
		"file", plan.FileName)
	telemetry.Redirect(ctx, telemetry.RedirectNotCached)
	plan.Action = ActionRedirect
	return plan
}

// Warm makes sure one package file is cached, downloading it with no client
// attached. Already-cached files are a no-op. The whole miss pipeline is reused,
// so a file that fails verification leaves nothing partial behind. It reports
// ErrCachingDisabled when downloads are configured as redirects and ErrNotListed
// when the index does not know the file.
func (s *Service) Warm(ctx context.Context, packageName, fileName string) error {
	plan, err := s.Plan(ctx, packageName, fileName)
	if err != nil {
		return err
	}
	switch plan.Action {
	case ActionFromStorage:
		return nil
	case ActionStreamAndCache:
		// io.Discard: the point is the cache write the downloader performs on the
		// way through, not the bytes.
		_, _, err := s.Fetch(ctx, plan, io.Discard)
		return err
	case ActionRedirect:
		return ErrCachingDisabled
	default:
		return fmt.Errorf("%w: %s/%s", ErrNotListed, packageName, fileName)
	}
}

func findFile(files []index.FileInfo, fileName string) (index.FileInfo, bool) {
	i := slices.IndexFunc(files, func(f index.FileInfo) bool { return f.Name == fileName })
	if i < 0 {
		return index.FileInfo{}, false
	}
	return files[i], true
}

// metadataSuffix is the PEP 658 filename suffix for a distribution's METADATA.
const metadataSuffix = ".metadata"

// metadataURL is the PEP 658 location of a distribution's metadata file: the
// file URL with ".metadata" appended to its path, dropping any #sha256= fragment
// a PEP 503 index carried on the href.
func metadataURL(fileURL string) string {
	u, err := url.Parse(fileURL)
	if err != nil {
		return strings.TrimSuffix(fileURL, "#") + metadataSuffix
	}
	u.Fragment = ""
	u.Path += metadataSuffix
	if u.RawPath != "" {
		u.RawPath += metadataSuffix
	}
	return u.String()
}

// quoteETag normalises an entity-tag to exactly one layer of quotes. Sources
// disagree: an index hash arrives bare, an S3 backend may echo the API's already
// quoted form. This is the only place either is quoted, so neither path can emit
// `""abc""` and break conditional requests.
func quoteETag(etag string) string {
	if etag == "" {
		return ""
	}
	if len(etag) > 1 && strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`) {
		return etag
	}
	return `"` + etag + `"`
}

// contentTypeForFile derives a content type from the distribution filename so
// the header can be sent before the first byte of the body.
func contentTypeForFile(fileName string) string {
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
	case strings.HasSuffix(name, metadataSuffix):
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
