package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
)

// upstreamSpec describes the fake index/file server a correctness test needs.
type upstreamSpec struct {
	pkg       string
	file      string
	sha256    string // advertised in the index hashes; omitted when empty
	indexSize int64  // advertised size; omitted when <= 0
	serveFile http.HandlerFunc
}

type fakeUpstream struct {
	*httptest.Server
	fileHits atomic.Int64
}

// newFakeUpstream stands up a PyPI-shaped index (JSON and HTML) plus a file
// endpoint whose behaviour the test supplies, counting file requests.
func newFakeUpstream(t *testing.T, spec upstreamSpec) *fakeUpstream {
	t.Helper()

	up := &fakeUpstream{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/"+spec.pkg+"/":
			fileURL := up.URL + "/files/" + spec.file
			if strings.Contains(r.Header.Get("Accept"), "application/vnd.pypi.simple.v1+json") {
				w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
				entry := map[string]any{"filename": spec.file, "url": fileURL}
				if spec.sha256 != "" {
					entry["hashes"] = map[string]string{"sha256": spec.sha256}
				}
				if spec.indexSize > 0 {
					entry["size"] = spec.indexSize
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"name":  spec.pkg,
					"files": []map[string]any{entry},
				})
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprintf(w, `<html><body><a href=%q>%s</a></body></html>`, fileURL, spec.file)
		case strings.HasPrefix(r.URL.Path, "/files/"):
			up.fileHits.Add(1)
			spec.serveFile(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(up.Close)
	return up
}

// newCorrectnessServer builds a router against upstream with an explicit
// time-to-first-byte budget and a per-test cache directory.
func newCorrectnessServer(t *testing.T, upstreamURL string, ttfb time.Duration) (http.Handler, string) {
	t.Helper()
	cacheDir := t.TempDir()
	srv := New(&config.Config{
		IndexURL:        upstreamURL,
		CacheDir:        cacheDir,
		CacheSize:       1 << 30,
		DownloadTimeout: ttfb,
		LogLevel:        "ERROR",
	})
	return srv.Router(), cacheDir
}

// getFile serves one file request in-process. An aborted response (a download
// that failed mid-stream) comes back as whatever was written before the abort.
func getFile(router http.Handler, pkg, file string) *http.Response {
	req := httptest.NewRequest("GET", "/index/"+pkg+"/"+file, nil)
	w := httptest.NewRecorder()
	func() {
		defer func() {
			if rec := recover(); rec != nil && rec != http.ErrAbortHandler {
				panic(rec)
			}
		}()
		router.ServeHTTP(w, req)
	}()
	return w.Result()
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return body
}

func cachedPath(cacheDir, pkg, file string) string {
	return filepath.Join(cacheDir, "packages", pkg, file)
}

func assertNotCached(t *testing.T, cacheDir, pkg, file string) {
	t.Helper()
	// Give the storage goroutine a moment to finish failing.
	time.Sleep(100 * time.Millisecond)

	_, err := os.Stat(cachedPath(cacheDir, pkg, file))
	assert.True(t, os.IsNotExist(err), "expected no cached object for %s/%s", pkg, file)

	// A failed write must also leave no temporary residue behind.
	entries, _ := os.ReadDir(filepath.Join(cacheDir, "packages", pkg))
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".tmp-"), "leftover temp file %s", e.Name())
	}
}

func waitCached(t *testing.T, cacheDir, pkg, file string) {
	t.Helper()
	path := cachedPath(cacheDir, pkg, file)
	for range 200 {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s was never cached", path)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func testPayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// TestDownload_HeaderDelayRedirects_BodyDelayDoesNot is the central regression
// test: the budget covers response headers only.
func TestDownload_HeaderDelayRedirects_BodyDelayDoesNot(t *testing.T) {
	payload := testPayload(256 * 1024)

	t.Run("header_delay_redirects", func(t *testing.T) {
		up := newFakeUpstream(t, upstreamSpec{
			pkg: "slowhdr", file: "slowhdr-1.0.0.tar.gz",
			sha256: sha256Hex(payload), indexSize: int64(len(payload)),
			serveFile: func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(600 * time.Millisecond)
				_, _ = w.Write(payload)
			},
		})
		router, cacheDir := newCorrectnessServer(t, up.URL, 100*time.Millisecond)

		resp := getFile(router, "slowhdr", "slowhdr-1.0.0.tar.gz")
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusFound, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("Location"), "/files/")
		assertNotCached(t, cacheDir, "slowhdr", "slowhdr-1.0.0.tar.gz")
	})

	t.Run("body_delay_streams_and_caches", func(t *testing.T) {
		up := newFakeUpstream(t, upstreamSpec{
			pkg: "slowbody", file: "slowbody-1.0.0.tar.gz",
			sha256: sha256Hex(payload), indexSize: int64(len(payload)),
			serveFile: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(payload[:1024])
				w.(http.Flusher).Flush()
				time.Sleep(400 * time.Millisecond)
				_, _ = w.Write(payload[1024:])
			},
		})
		router, cacheDir := newCorrectnessServer(t, up.URL, 100*time.Millisecond)

		resp := getFile(router, "slowbody", "slowbody-1.0.0.tar.gz")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, payload, readBody(t, resp))
		waitCached(t, cacheDir, "slowbody", "slowbody-1.0.0.tar.gz")
	})
}

// TestDownload_DefaultConfigurationCaches drives the router with the shipped
// default budget and asserts the second request never reaches upstream.
func TestDownload_DefaultConfigurationCaches(t *testing.T) {
	payload := testPayload(2 * 1024 * 1024)
	pkg, file := "defaults", "defaults-1.0.0.tar.gz"

	up := newFakeUpstream(t, upstreamSpec{
		pkg: pkg, file: file,
		sha256: sha256Hex(payload), indexSize: int64(len(payload)),
		serveFile: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload[:4096])
			w.(http.Flusher).Flush()
			// Longer than the 0.9s default: a body-spanning budget aborts here.
			time.Sleep(1200 * time.Millisecond)
			_, _ = w.Write(payload[4096:])
		},
	})

	cacheDir := t.TempDir()
	t.Setenv("GROXPI_INDEX_URL", up.URL)
	t.Setenv("GROXPI_CACHE_DIR", cacheDir)
	t.Setenv("GROXPI_LOGGING_LEVEL", "ERROR")
	router := New(config.Load()).Router()

	first := getFile(router, pkg, file)
	assert.Equal(t, http.StatusOK, first.StatusCode)
	assert.Equal(t, payload, readBody(t, first), "first response must be the complete file")
	waitCached(t, cacheDir, pkg, file)

	second := getFile(router, pkg, file)
	assert.Equal(t, http.StatusOK, second.StatusCode)
	assert.Equal(t, payload, readBody(t, second))
	assert.Equal(t, int64(1), up.fileHits.Load(), "second request must be served from cache")
}

// TestDownload_ChecksumMismatchIsNotCached covers issue #8's primary case.
func TestDownload_ChecksumMismatchIsNotCached(t *testing.T) {
	served := testPayload(64 * 1024)
	advertised := testPayload(64 * 1024)
	advertised[0] ^= 0xff
	pkg, file := "badhash", "badhash-1.0.0.tar.gz"

	up := newFakeUpstream(t, upstreamSpec{
		pkg: pkg, file: file,
		sha256: sha256Hex(advertised), indexSize: int64(len(served)),
		serveFile: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(served)
		},
	})
	router, cacheDir := newCorrectnessServer(t, up.URL, 5*time.Second)

	resp := getFile(router, pkg, file)
	_ = readBody(t, resp)
	assertNotCached(t, cacheDir, pkg, file)

	resp2 := getFile(router, pkg, file)
	_ = readBody(t, resp2)
	assert.Equal(t, int64(2), up.fileHits.Load(), "a rejected file must be refetched")
}

// TestDownload_TruncatedBodyIsNotCached covers issue #7.
func TestDownload_TruncatedBodyIsNotCached(t *testing.T) {
	payload := testPayload(512 * 1024)
	pkg, file := "truncated", "truncated-1.0.0.tar.gz"

	up := newFakeUpstream(t, upstreamSpec{
		pkg: pkg, file: file,
		indexSize: int64(len(payload)),
		serveFile: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload[:4096])
			w.(http.Flusher).Flush()
			// Kill the connection with the declared length unfulfilled.
			panic(http.ErrAbortHandler)
		},
	})
	router, cacheDir := newCorrectnessServer(t, up.URL, 5*time.Second)

	resp := getFile(router, pkg, file)
	_ = readBody(t, resp)
	assertNotCached(t, cacheDir, pkg, file)

	resp2 := getFile(router, pkg, file)
	_ = readBody(t, resp2)
	assert.Equal(t, int64(2), up.fileHits.Load(), "a truncated download must be refetched")
}

// TestDownload_LengthFallbackWithoutHash covers the weaker check used when the
// index supplies no hash.
func TestDownload_LengthFallbackWithoutHash(t *testing.T) {
	payload := testPayload(128 * 1024)

	t.Run("length_matches_caches", func(t *testing.T) {
		pkg, file := "lenok", "lenok-1.0.0.tar.gz"
		up := newFakeUpstream(t, upstreamSpec{
			pkg: pkg, file: file, indexSize: int64(len(payload)),
			serveFile: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(payload)
			},
		})
		router, cacheDir := newCorrectnessServer(t, up.URL, 5*time.Second)

		resp := getFile(router, pkg, file)
		assert.Equal(t, payload, readBody(t, resp))
		waitCached(t, cacheDir, pkg, file)
	})

	t.Run("length_mismatch_refuses", func(t *testing.T) {
		pkg, file := "lenbad", "lenbad-1.0.0.tar.gz"
		up := newFakeUpstream(t, upstreamSpec{
			pkg: pkg, file: file, indexSize: int64(len(payload)) + 4096,
			serveFile: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(payload)
			},
		})
		router, cacheDir := newCorrectnessServer(t, up.URL, 5*time.Second)

		resp := getFile(router, pkg, file)
		_ = readBody(t, resp)
		assertNotCached(t, cacheDir, pkg, file)

		resp2 := getFile(router, pkg, file)
		_ = readBody(t, resp2)
		assert.Equal(t, int64(2), up.fileHits.Load())
	})
}

// TestDownload_UnverifiableFileIsStillCached is the deliberate hole for sparse
// private indexes: neither hash nor length, so the file is cached unverified.
func TestDownload_UnverifiableFileIsStillCached(t *testing.T) {
	payload := testPayload(32 * 1024)
	pkg, file := "unverified", "unverified-1.0.0.tar.gz"

	up := newFakeUpstream(t, upstreamSpec{
		pkg: pkg, file: file,
		serveFile: func(w http.ResponseWriter, _ *http.Request) {
			// Chunked: no Content-Length either.
			_, _ = w.Write(payload[:1024])
			w.(http.Flusher).Flush()
			_, _ = w.Write(payload[1024:])
		},
	})
	router, cacheDir := newCorrectnessServer(t, up.URL, 5*time.Second)

	resp := getFile(router, pkg, file)
	assert.Equal(t, payload, readBody(t, resp))
	waitCached(t, cacheDir, pkg, file)
}

// readFromRecorder records what the response writer's ReadFrom is handed.
type readFromRecorder struct {
	*httptest.ResponseRecorder
	src io.Reader
}

func (w *readFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	w.src = src
	return io.Copy(w.ResponseRecorder, src)
}

// TestDownload_LocalHitHandsServeContentTheFile guards the copy path: a local
// hit must reach the writer's ReadFrom as the *os.File itself (under
// ServeContent's LimitedReader), not behind the storage.Object wrapper.
func TestDownload_LocalHitHandsServeContentTheFile(t *testing.T) {
	payload := testPayload(64 * 1024)
	pkg, file := "copypath", "copypath-1.0.0.tar.gz"
	up := newFakeUpstream(t, upstreamSpec{
		pkg: pkg, file: file, sha256: sha256Hex(payload), indexSize: int64(len(payload)),
		serveFile: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) },
	})
	router, cacheDir := newCorrectnessServer(t, up.URL, 5*time.Second)
	readBody(t, getFile(router, pkg, file))
	waitCached(t, cacheDir, pkg, file)

	w := &readFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	router.ServeHTTP(w, httptest.NewRequest("GET", "/index/"+pkg+"/"+file, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, payload, w.Body.Bytes())
	src := w.src
	if lr, ok := src.(*io.LimitedReader); ok {
		src = lr.R
	}
	assert.IsType(t, (*os.File)(nil), src)
}
