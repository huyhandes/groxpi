package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
)

// countingUpstream is the fake-upstream harness with an index-request counter, so
// a test can tell an index cache hit from a refetch.
type countingUpstream struct {
	*httptest.Server
	indexHits atomic.Int64
	fileHits  atomic.Int64
}

func newCountingUpstream(t *testing.T, pkg, file string, payload []byte) *countingUpstream {
	t.Helper()

	up := &countingUpstream{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/"+pkg+"/":
			up.indexHits.Add(1)
			fileURL := up.URL + "/files/" + file
			if strings.Contains(r.Header.Get("Accept"), "application/vnd.pypi.simple.v1+json") {
				w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"name": pkg,
					"files": []map[string]any{{
						"filename": file,
						"url":      fileURL,
						"size":     len(payload),
						"hashes":   map[string]string{"sha256": sha256Hex(payload)},
					}},
				})
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprintf(w, `<html><body><a href=%q>%s</a></body></html>`, fileURL, file)
		case strings.HasPrefix(r.URL.Path, "/files/"):
			up.fileHits.Add(1)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			_, _ = w.Write(payload)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(up.Close)
	return up
}

// newEvictionServer builds a router with a long index TTL, so a refetch can only
// mean the index entry was dropped. The cache routes are part of the
// authenticated administrative group, so credentials are configured here and
// every request below carries them.
func newEvictionServer(t *testing.T, upstreamURL string) (*gin.Engine, string) {
	t.Helper()
	cacheDir := t.TempDir()
	srv, err := NewServer(&config.Config{
		IndexURL:        upstreamURL,
		IndexTTL:        time.Minute,
		IndexCacheSize:  1 << 20,
		CacheDir:        cacheDir,
		CacheSize:       1 << 30,
		DownloadTimeout: 5 * time.Second,
		LogLevel:        "ERROR",
		AdminUsername:   adminUser,
		AdminPassword:   adminPass,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	return srv.Router(), cacheDir
}

func deletePackage(router *gin.Engine, pkg string) *http.Response {
	req := httptest.NewRequest("DELETE", "/cache/"+pkg, nil)
	req.SetBasicAuth(adminUser, adminPass)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Result()
}

func getPackageIndex(router *gin.Engine, pkg string) *http.Response {
	req := httptest.NewRequest("GET", "/index/"+pkg, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Result()
}

// TestEvictPackage_DeletesCachedFiles is the regression test for the defect where
// invalidation reported success while leaving every cached file on disk. The
// proof is a second upstream fetch, not a filesystem check.
func TestEvictPackage_DeletesCachedFiles(t *testing.T) {
	payload := testPayload(64 * 1024)
	pkg, file := "evictme", "evictme-1.0.0.tar.gz"
	up := newCountingUpstream(t, pkg, file, payload)
	router, cacheDir := newEvictionServer(t, up.URL)

	_ = readBody(t, getFile(router, pkg, file))
	waitCached(t, cacheDir, pkg, file)

	_ = readBody(t, getFile(router, pkg, file))
	require.Equal(t, int64(1), up.fileHits.Load(), "second request should have been served from cache")

	resp := deletePackage(router, pkg)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_ = readBody(t, getFile(router, pkg, file))
	assert.Equal(t, int64(2), up.fileHits.Load(), "evicted file was not deleted: the request was still served from cache")
}

// TestEvictPackage_ClearsIndexEntry pins that eviction also drops the index
// entry, so the next index request cannot serve a list pointing at deleted files.
func TestEvictPackage_ClearsIndexEntry(t *testing.T) {
	payload := testPayload(1024)
	pkg, file := "indexevict", "indexevict-1.0.0.tar.gz"
	up := newCountingUpstream(t, pkg, file, payload)
	router, _ := newEvictionServer(t, up.URL)

	_ = readBody(t, getPackageIndex(router, pkg))
	_ = readBody(t, getPackageIndex(router, pkg))
	require.Equal(t, int64(1), up.indexHits.Load(), "second index request should have been cached")

	_ = readBody(t, deletePackage(router, pkg))

	_ = readBody(t, getPackageIndex(router, pkg))
	assert.Equal(t, int64(2), up.indexHits.Load(), "index entry survived eviction")
}

// TestEvictPackage_NothingCachedSucceeds pins that evicting a package with no
// cached files is not an error.
func TestEvictPackage_NothingCachedSucceeds(t *testing.T) {
	up := newCountingUpstream(t, "present", "present-1.0.0.tar.gz", testPayload(16))
	router, _ := newEvictionServer(t, up.URL)

	resp := deletePackage(router, "never-cached")
	_ = readBody(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int64(0), up.fileHits.Load())
}
