package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
)

// newCorrectnessServer builds a router against upstream with an explicit
// time-to-first-byte budget and a per-test cache directory.
func newCorrectnessServer(t *testing.T, upstreamURL string, ttfb time.Duration) (http.Handler, string) {
	t.Helper()
	srv := newCorrectnessSrv(t, upstreamURL, ttfb)
	return srv.Router(), srv.config.CacheDir
}

// newCorrectnessSrv is newCorrectnessServer for a test that needs the server
// itself, to wait for its downloads.
func newCorrectnessSrv(t *testing.T, upstreamURL string, ttfb time.Duration) *Server {
	t.Helper()
	return New(&config.Config{
		IndexURL:        upstreamURL,
		CacheDir:        t.TempDir(),
		CacheSize:       1 << 30,
		DownloadTimeout: ttfb,
		LogLevel:        "ERROR",
	})
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

// assertNotCached waits for srv's downloads to finish, then asserts the file
// was not stored. A flight renames into the cache before it unregisters, so an
// empty InFlight means a stored file would already be there.
func assertNotCached(t *testing.T, srv *Server, pkg, file string) {
	t.Helper()
	require.Eventually(t, func() bool { return len(srv.store.InFlight()) == 0 },
		5*time.Second, 5*time.Millisecond, "the failed download never finished")

	cacheDir := srv.config.CacheDir
	_, err := os.Stat(cachedPath(cacheDir, pkg, file))
	assert.True(t, os.IsNotExist(err), "expected no cached object for %s/%s", pkg, file)

	// A failed write must also leave no temporary residue behind. The spool is
	// removed just after the flight unregisters, so that removal is waited for.
	assert.Eventually(t, func() bool {
		entries, _ := os.ReadDir(filepath.Join(cacheDir, "packages", pkg))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				return false
			}
		}
		return true
	}, 5*time.Second, 5*time.Millisecond, "leftover temp file for %s/%s", pkg, file)
}

func waitCached(t *testing.T, cacheDir, pkg, file string) {
	t.Helper()
	path := cachedPath(cacheDir, pkg, file)
	require.Eventually(t, func() bool { _, err := os.Stat(path); return err == nil },
		2*time.Second, 10*time.Millisecond, "file %s was never cached", path)
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
		up := NewFakePyPI(t).Add("slowhdr", "slowhdr-1.0.0.tar.gz", FakeFile{SHA256: Sum(payload), Size: int64(len(payload)),
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(600 * time.Millisecond)
				_, _ = w.Write(payload)
			},
		})
		srv := newCorrectnessSrv(t, up.URL, 100*time.Millisecond)
		router := srv.Router()

		resp := getFile(router, "slowhdr", "slowhdr-1.0.0.tar.gz")
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusFound, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("Location"), "/files/")
		assertNotCached(t, srv, "slowhdr", "slowhdr-1.0.0.tar.gz")
	})

	t.Run("body_delay_streams_and_caches", func(t *testing.T) {
		up := NewFakePyPI(t).Add("slowbody", "slowbody-1.0.0.tar.gz", FakeFile{SHA256: Sum(payload), Size: int64(len(payload)),
			Handler: func(w http.ResponseWriter, _ *http.Request) {
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

	up := NewFakePyPI(t).Add(pkg, file, FakeFile{SHA256: Sum(payload), Size: int64(len(payload)),
		Handler: func(w http.ResponseWriter, _ *http.Request) {
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
	assert.Equal(t, 1, up.FileHits(file), "second request must be served from cache")
}

// TestDownload_ChecksumMismatchIsNotCached covers issue #8's primary case.
func TestDownload_ChecksumMismatchIsNotCached(t *testing.T) {
	served := testPayload(64 * 1024)
	advertised := testPayload(64 * 1024)
	advertised[0] ^= 0xff
	pkg, file := "badhash", "badhash-1.0.0.tar.gz"

	up := NewFakePyPI(t).Add(pkg, file, FakeFile{SHA256: Sum(advertised), Size: int64(len(served)),
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(served)
		},
	})
	srv := newCorrectnessSrv(t, up.URL, 5*time.Second)
	router := srv.Router()

	resp := getFile(router, pkg, file)
	_ = readBody(t, resp)
	assertNotCached(t, srv, pkg, file)

	resp2 := getFile(router, pkg, file)
	_ = readBody(t, resp2)
	assert.Equal(t, 2, up.FileHits(file), "a rejected file must be refetched")
}

// TestDownload_TruncatedBodyIsNotCached covers issue #7.
func TestDownload_TruncatedBodyIsNotCached(t *testing.T) {
	payload := testPayload(512 * 1024)
	pkg, file := "truncated", "truncated-1.0.0.tar.gz"

	up := NewFakePyPI(t).Add(pkg, file, FakeFile{Size: int64(len(payload)),
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload[:4096])
			w.(http.Flusher).Flush()
			// Kill the connection with the declared length unfulfilled.
			panic(http.ErrAbortHandler)
		},
	})
	srv := newCorrectnessSrv(t, up.URL, 5*time.Second)
	router := srv.Router()

	resp := getFile(router, pkg, file)
	_ = readBody(t, resp)
	assertNotCached(t, srv, pkg, file)

	resp2 := getFile(router, pkg, file)
	_ = readBody(t, resp2)
	assert.Equal(t, 2, up.FileHits(file), "a truncated download must be refetched")
}

// TestDownload_LengthFallbackWithoutHash covers the weaker check used when the
// index supplies no hash.
func TestDownload_LengthFallbackWithoutHash(t *testing.T) {
	payload := testPayload(128 * 1024)

	t.Run("length_matches_caches", func(t *testing.T) {
		pkg, file := "lenok", "lenok-1.0.0.tar.gz"
		up := NewFakePyPI(t).Add(pkg, file, FakeFile{Size: int64(len(payload)),
			Handler: func(w http.ResponseWriter, _ *http.Request) {
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
		up := NewFakePyPI(t).Add(pkg, file, FakeFile{Size: int64(len(payload)) + 4096,
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(payload)
			},
		})
		srv := newCorrectnessSrv(t, up.URL, 5*time.Second)
		router := srv.Router()

		resp := getFile(router, pkg, file)
		_ = readBody(t, resp)
		assertNotCached(t, srv, pkg, file)

		resp2 := getFile(router, pkg, file)
		_ = readBody(t, resp2)
		assert.Equal(t, 2, up.FileHits(file))
	})
}

// TestDownload_UnverifiableFileIsStillCached is the deliberate hole for sparse
// private indexes: neither hash nor length, so the file is cached unverified.
func TestDownload_UnverifiableFileIsStillCached(t *testing.T) {
	payload := testPayload(32 * 1024)
	pkg, file := "unverified", "unverified-1.0.0.tar.gz"

	up := NewFakePyPI(t).Add(pkg, file, FakeFile{
		Handler: func(w http.ResponseWriter, _ *http.Request) {
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
	up := NewFakePyPI(t).Add(pkg, file, FakeFile{SHA256: Sum(payload), Size: int64(len(payload)),
		Handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) },
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
