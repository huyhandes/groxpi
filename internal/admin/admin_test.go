package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/index"
	"github.com/huyhandes/groxpi/internal/storage"
)

// TestEvict_InvalidNameDeletesNothing pins A1 on the eviction side: a name that
// is not one path segment is refused before the index or the cache is touched,
// so a prefix spanning another package's directory is never deleted.
func TestEvict_InvalidNameDeletesNothing(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":  "a/b",
			"files": []map[string]any{{"filename": "b-1.0.tar.gz", "url": "https://example.invalid/b-1.0.tar.gz"}},
		})
	}))
	t.Cleanup(up.Close)

	dir := t.TempDir()
	planted := filepath.Join(dir, "packages", "a", "b", "b-1.0.tar.gz")
	require.NoError(t, os.MkdirAll(filepath.Dir(planted), 0o750))
	require.NoError(t, os.WriteFile(planted, []byte("keep"), 0o600))
	cache, err := storage.NewLocalStorage(dir, 0, 0)
	require.NoError(t, err)
	store := storage.NewStore(cache, nil, nil, "")
	t.Cleanup(func() { _ = store.Close() })

	cfg := &config.Config{IndexURL: up.URL, IndexTTL: time.Minute, IndexCacheSize: 1 << 20, DownloadTimeout: time.Second}
	idx := index.New(cfg)
	t.Cleanup(idx.Close)
	_, err = idx.Resolve(t.Context(), "a/b")
	require.NoError(t, err)
	require.Equal(t, int64(1), hits.Load())

	err = New(cfg, store, idx).Evict(t.Context(), "a/b")
	require.ErrorIs(t, err, storage.ErrInvalidKey)

	_, err = os.Stat(planted)
	assert.NoError(t, err, "nothing under the cache was deleted")
	_, err = idx.Resolve(t.Context(), "a/b")
	require.NoError(t, err)
	assert.Equal(t, int64(1), hits.Load(), "the index entry was left intact")
}
