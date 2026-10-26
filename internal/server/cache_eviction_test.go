package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
)

// newEvictionServer builds a router with a long index TTL, so a refetch can only
// mean the index entry was dropped. The cache routes are part of the
// authenticated administrative group, so credentials are configured here and
// every request below carries them.
func newEvictionServer(t *testing.T, upstreamURL string) (http.Handler, string) {
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

func deletePackage(router http.Handler, pkg string) *http.Response {
	req := httptest.NewRequest("DELETE", "/cache/"+pkg, nil)
	req.SetBasicAuth(adminUser, adminPass)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Result()
}

// TestEvictPackage_NothingCachedSucceeds pins that evicting a package with no
// cached files is not an error.
func TestEvictPackage_NothingCachedSucceeds(t *testing.T) {
	up := newPyPIWith(t, "present", "present-1.0.0.tar.gz", testPayload(16))
	router, _ := newEvictionServer(t, up.URL)

	resp := deletePackage(router, "never-cached")
	_ = readBody(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Zero(t, up.FileHits("present-1.0.0.tar.gz"))
}
