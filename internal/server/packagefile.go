package server

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/phuslu/log"
	"golang.org/x/sync/singleflight"

	"github.com/huyhandes/groxpi/internal/cache"
	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/pypi"
	"github.com/huyhandes/groxpi/internal/storage"
	"github.com/huyhandes/groxpi/internal/streaming"
)

// ServeAction is the outcome of the package-file miss pipeline: what the
// transport layer has to do to satisfy the request.
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
	Size        int64         // from the PyPI index, -1 if unknown
	Timeout     time.Duration // ActionStreamAndCache
}

// packageIndex is the upstream index seam. It exists so the decision tree can be
// exercised without a live PyPI client.
type packageIndex interface {
	GetPackageList() ([]string, error)
	GetPackageFiles(packageName string) ([]pypi.FileInfo, error)
}

// packageListKey names the package list in both the index cache and the
// singleflight group.
const packageListKey = "package-list"

// PackageFileService owns index resolution and the package-file miss pipeline:
// storage lookup, caches, deduplicated upstream fetch. It never touches the HTTP
// layer. It owns the only singleflight.Group in the server: every upstream
// index fetch and every upstream download is deduplicated through it.
type PackageFileService struct {
	storage         storage.Storage
	indexCache      *cache.IndexCache
	index           packageIndex
	downloader      streaming.StreamingDownloader
	sf              singleflight.Group
	indexTTL        time.Duration
	downloadTimeout time.Duration
}

func newPackageFileService(
	cfg *config.Config,
	st storage.Storage,
	indexCache *cache.IndexCache,
	index packageIndex,
	downloader streaming.StreamingDownloader,
) *PackageFileService {
	return &PackageFileService{
		storage:         st,
		indexCache:      indexCache,
		index:           index,
		downloader:      downloader,
		indexTTL:        cfg.IndexTTL,
		downloadTimeout: cfg.DownloadTimeout,
	}
}

// Plan decides how a package file should be served. It performs no client I/O.
// A nil error with ActionNotFound means the index does not list the file; a
// non-nil error means the package index itself could not be resolved.
func (s *PackageFileService) Plan(ctx context.Context, packageName, fileName string) (ServePlan, error) {
	plan := ServePlan{
		Action:      ActionNotFound,
		PackageName: packageName,
		FileName:    fileName,
		StorageKey:  storageKeyFor(packageName, fileName),
		Size:        -1,
	}

	exists, err := s.storage.Exists(ctx, plan.StorageKey)
	if err != nil {
		// A backend hiccup must not fail the request: fall through to upstream.
		log.Error().Err(err).Str("key", plan.StorageKey).Msg("Failed to check storage")
	} else if exists {
		log.Debug().
			Str("package", packageName).
			Str("file", fileName).
			Msg("✅ Serving from storage cache")
		plan.Action = ActionFromStorage
		return plan, nil
	}

	files, err := s.resolveIndex(packageName)
	if err != nil {
		return plan, fmt.Errorf("failed to resolve index for package %q: %w", packageName, err)
	}

	info, ok := findFile(files, fileName)
	if !ok {
		return plan, nil
	}

	plan.URL = info.URL
	plan.ContentType = contentTypeForFile(fileName)
	if info.Size > 0 {
		plan.Size = info.Size
	}
	plan.ETag = quoteETag(info.Hashes["sha256"])

	if s.downloadTimeout <= 0 {
		log.Debug().
			Str("package", packageName).
			Str("file", fileName).
			Msg("Download timeout is 0, redirecting directly to PyPI")
		plan.Action = ActionRedirect
		return plan, nil
	}

	plan.Action = ActionStreamAndCache
	plan.Timeout = s.calculateDynamicTimeout(plan.Size)
	return plan, nil
}

// Fetch performs the deduplicated upstream fetch for plan, streaming the body to
// dst while caching it into storage. Exactly one caller per storage key does the
// work: it gets led=true plus the StreamResult. Concurrent callers for the same
// key block until the leader finishes and get led=false with nothing written to
// their dst — they must re-Plan and serve the now-cached object.
func (s *PackageFileService) Fetch(ctx context.Context, plan ServePlan, dst io.Writer) (*streaming.StreamResult, bool, error) {
	// The download populates the cache for every waiting request, so it must not
	// die with the client that happened to trigger it. Only the dynamic timeout
	// bounds it.
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), plan.Timeout)
	defer cancel()

	// Only the leader runs the closure, so this is written on the leader's own
	// stack before Do returns and read after it returns — no synchronization
	// needed.
	led := false

	value, err, _ := s.sf.Do(plan.StorageKey, func() (any, error) {
		led = true
		log.Info().
			Str("package", plan.PackageName).
			Str("file", plan.FileName).
			Str("file_url", plan.URL).
			Int64("file_size", plan.Size).
			Dur("timeout", plan.Timeout).
			Msg("🚀 Starting streaming download with simultaneous cache")
		return s.downloader.DownloadAndStream(fetchCtx, plan.URL, plan.StorageKey, dst)
	})

	if !led {
		log.Debug().
			Str("package", plan.PackageName).
			Str("file", plan.FileName).
			Msg("🔄 Waited for in-flight download")
		if err != nil {
			return nil, false, fmt.Errorf("shared download of %q failed: %w", plan.StorageKey, err)
		}
		return nil, false, nil
	}

	if err != nil {
		return nil, true, fmt.Errorf("failed to stream %q: %w", plan.URL, err)
	}

	// A leader that reports no error must have produced a result; anything else
	// is a broken downloader, not something to dereference and find out.
	result, ok := value.(*streaming.StreamResult)
	if !ok || result == nil {
		return nil, true, fmt.Errorf("download of %q returned no result (%T)", plan.StorageKey, value)
	}
	return result, true, nil
}

// PlanAfterFetch decides how to serve a request that waited on another request's
// download. The object should now be cached, so it is re-planned onto storage;
// when it is not there (async or failed cache write) the client goes upstream.
func (s *PackageFileService) PlanAfterFetch(ctx context.Context, plan ServePlan) ServePlan {
	next, err := s.Plan(ctx, plan.PackageName, plan.FileName)
	if err == nil && next.Action == ActionFromStorage {
		log.Debug().
			Str("package", plan.PackageName).
			Str("file", plan.FileName).
			Msg("✅ Serving from storage after coordinated download")
		return next
	}

	log.Debug().
		Str("package", plan.PackageName).
		Str("file", plan.FileName).
		Msg("⏭️ Redirecting to PyPI after download coordination")
	plan.Action = ActionRedirect
	return plan
}

// resolveIndex returns the index entries for a package, using the index cache
// and deduplicating concurrent upstream fetches. It is the only path to the
// upstream package index.
func (s *PackageFileService) resolveIndex(packageName string) ([]pypi.FileInfo, error) {
	if cached, found := s.indexCache.GetPackage(packageName); found {
		if files, ok := cached.([]pypi.FileInfo); ok {
			return files, nil
		}
	}

	result, err, _ := s.sf.Do("package-files:"+packageName, func() (any, error) {
		return s.index.GetPackageFiles(packageName)
	})
	if err != nil {
		return nil, err
	}

	files, ok := result.([]pypi.FileInfo)
	if !ok {
		return nil, fmt.Errorf("unexpected index result type %T for package %q", result, packageName)
	}
	// Never cache an empty index: a transient upstream fault would otherwise
	// poison this package for the whole TTL.
	if len(files) > 0 {
		s.indexCache.SetPackage(packageName, files, s.indexTTL)
	}
	return files, nil
}

// resolvePackageList returns the full package list, using the index cache and
// deduplicating concurrent upstream fetches. It is the only path to the upstream
// package list.
func (s *PackageFileService) resolvePackageList() ([]string, error) {
	if cached, found := s.indexCache.Get(packageListKey); found {
		if packages, ok := cached.([]string); ok {
			return packages, nil
		}
	}

	result, err, _ := s.sf.Do(packageListKey, func() (any, error) {
		return s.index.GetPackageList()
	})
	if err != nil {
		return nil, fmt.Errorf("failed to resolve package list: %w", err)
	}

	packages, ok := result.([]string)
	if !ok {
		return nil, fmt.Errorf("unexpected package list result type %T", result)
	}
	// Never cache an empty list, for the same reason as resolveIndex.
	if len(packages) > 0 {
		s.indexCache.Set(packageListKey, packages, s.indexTTL)
	}
	return packages, nil
}

// calculateDynamicTimeout budgets the download at a 100 KB/s floor, clamped to
// [2min, 1h]: 2 minutes covers network overhead on small files, 1 hour stops a
// stalled transfer from pinning the request forever. An unknown size (<= 0) gets
// the configured timeout unchanged.
func (s *PackageFileService) calculateDynamicTimeout(expectedSize int64) time.Duration {
	if expectedSize <= 0 {
		return s.downloadTimeout
	}
	const minSpeedBytesPerSec = 100 * 1024
	transfer := time.Duration(expectedSize/minSpeedBytesPerSec) * time.Second
	return min(max(transfer, 2*time.Minute), 60*time.Minute)
}

func storageKeyFor(packageName, fileName string) string {
	return "packages/" + packageName + "/" + fileName
}

func findFile(files []pypi.FileInfo, fileName string) (pypi.FileInfo, bool) {
	i := slices.IndexFunc(files, func(f pypi.FileInfo) bool { return f.Name == fileName })
	if i < 0 {
		return pypi.FileInfo{}, false
	}
	return files[i], true
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
	default:
		return "application/octet-stream"
	}
}
