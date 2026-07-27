package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

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
	SHA256      string        // from the index hashes, empty if unknown
	Size        int64         // from the PyPI index, -1 if unknown
	Timeout     time.Duration // ActionStreamAndCache: time-to-first-byte budget
}

// packageIndex is the upstream index seam. It exists so the decision tree can be
// exercised without a live PyPI client.
type packageIndex interface {
	GetPackageFiles(packageName string) ([]pypi.FileInfo, error)
}

// jsonContentType is the PEP 691 media type of every index body served as JSON.
const jsonContentType = "application/vnd.pypi.simple.v1+json"

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
func encodePackageFiles(packageName string, files []pypi.FileInfo) ([]byte, error) {
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
		if file.IsYanked() {
			wf.Yanked = true
			wf.YankedReason = file.GetYankedReason()
		}
		body.Files = append(body.Files, wf)
	}
	return json.Marshal(body)
}

func proxyFileURL(packageName, fileName string) string {
	return "/simple/" + packageName + "/" + fileName
}

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

	// The root index is proxied rather than parsed, so it needs an HTTP client of
	// its own instead of the typed index client.
	rootURL    string
	rootClient *http.Client
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
		rootURL:         strings.TrimSuffix(cfg.IndexURL, "/") + "/",
		// ponytail: one fixed timeout for the whole proxied response. The root list
		// is large but a single upstream GET; a byte-rate budget can come later if a
		// slow index actually shows up.
		rootClient: &http.Client{Timeout: 60 * time.Second},
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
		slog.Error("Failed to check storage", "error", err, "key", plan.StorageKey)
	} else if exists {
		slog.Debug("✅ Serving from storage cache", "package", packageName, "file", fileName)
		plan.Action = ActionFromStorage
		return plan, nil
	}

	entry, err := s.resolveIndex(packageName)
	if err != nil {
		return plan, fmt.Errorf("failed to resolve index for package %q: %w", packageName, err)
	}

	info, ok := findFile(entry.Files, fileName)
	if !ok {
		return plan, nil
	}

	plan.URL = info.URL
	plan.ContentType = contentTypeForFile(fileName)
	if info.Size > 0 {
		plan.Size = info.Size
	}
	plan.SHA256 = info.Hashes["sha256"]
	plan.ETag = quoteETag(plan.SHA256)

	if s.downloadTimeout <= 0 {
		slog.Debug("Download timeout is 0, redirecting directly to PyPI",
			"package", packageName,
			"file", fileName)
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
func (s *PackageFileService) Fetch(ctx context.Context, plan ServePlan, dst io.Writer) (*streaming.StreamResult, bool, error) {
	// The download populates the cache for every waiting request, so it must not
	// die with the client that happened to trigger it. It carries no deadline
	// either: the configured budget applies only until upstream response headers
	// arrive, and the downloader enforces that itself.
	fetchCtx := context.WithoutCancel(ctx)

	// Only the leader runs the closure, so this is written on the leader's own
	// stack before Do returns and read after it returns — no synchronization
	// needed.
	led := false

	value, err, _ := s.sf.Do(plan.StorageKey, func() (any, error) {
		led = true
		slog.Info("🚀 Starting streaming download with simultaneous cache",
			"package", plan.PackageName,
			"file", plan.FileName,
			"file_url", plan.URL,
			"file_size", plan.Size,
			"timeout", plan.Timeout)
		return s.downloader.DownloadAndStream(fetchCtx, plan.URL, plan.StorageKey, dst, streaming.Expectation{
			SHA256: plan.SHA256,
			Size:   plan.Size,
		})
	})

	if !led {
		slog.Debug("🔄 Waited for in-flight download", "package", plan.PackageName, "file", plan.FileName)
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
		slog.Debug("✅ Serving from storage after coordinated download",
			"package", plan.PackageName,
			"file", plan.FileName)
		return next
	}

	slog.Debug("⏭️ Redirecting to PyPI after download coordination",
		"package", plan.PackageName,
		"file", plan.FileName)
	plan.Action = ActionRedirect
	return plan
}

// resolveIndex returns the cache entry for a package — parsed file list and
// serialised bodies, produced together — using the index cache and
// deduplicating concurrent upstream fetches. It is the only path to the upstream
// package index.
func (s *PackageFileService) resolveIndex(packageName string) (*cache.Entry, error) {
	if entry, found := s.indexCache.GetPackage(packageName); found {
		return entry, nil
	}

	result, err, _ := s.sf.Do("package-files:"+packageName, func() (any, error) {
		files, err := s.index.GetPackageFiles(packageName)
		if err != nil {
			return nil, err
		}
		body, err := encodePackageFiles(packageName, files)
		if err != nil {
			return nil, fmt.Errorf("failed to encode index for package %q: %w", packageName, err)
		}
		entry := cache.NewPackageEntry(files, body)
		// Never cache an empty index: a transient upstream fault would otherwise
		// poison this package for the whole TTL.
		if len(files) > 0 {
			s.indexCache.SetPackage(packageName, entry, s.indexTTL)
		}
		return entry, nil
	})
	if err != nil {
		return nil, err
	}

	entry, ok := result.(*cache.Entry)
	if !ok {
		return nil, fmt.Errorf("unexpected index result type %T for package %q", result, packageName)
	}
	return entry, nil
}

// rootResponse is one upstream root-index response, held only as long as it
// takes to answer the burst of clients that shared its fetch.
type rootResponse struct {
	status          int
	contentType     string
	contentEncoding string
	body            []byte
}

// ProxyRoot fetches the upstream root index verbatim: the client's accepted
// content type and encoding are forwarded and the body is copied back
// undecoded. Nothing is cached — the full project list is tens of megabytes and
// is served through, not stored — but concurrent callers asking for the same
// representation still share one upstream fetch.
func (s *PackageFileService) ProxyRoot(ctx context.Context, accept, acceptEncoding string) (*rootResponse, error) {
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

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
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
