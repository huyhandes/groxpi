package download

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/index"
)

func TestMetadataURL(t *testing.T) {
	assert.Equal(t, "https://h/f/x.whl.metadata", metadataURL("https://h/f/x.whl"))
	assert.Equal(t, "https://h/f/x.whl.metadata", metadataURL("https://h/f/x.whl#sha256=abc"))
	assert.Equal(t, "https://h/f/x.whl.metadata?t=1", metadataURL("https://h/f/x.whl?t=1#sha256=abc"))
}

// TestPlan_Metadata pins PEP 658 planning: the .metadata sibling is planned
// from the distribution's index entry, only when the index advertises it.
func TestPlan_Metadata(t *testing.T) {
	const pkg = "meta"
	wheel := index.FileInfo{Name: "meta-1.0-py3-none-any.whl", URL: "https://files/meta-1.0-py3-none-any.whl#sha256=whl", CoreMetadata: map[string]any{"sha256": "md"}}
	sdist := index.FileInfo{Name: "meta-1.0.tar.gz", URL: "https://files/meta-1.0.tar.gz", DistInfoMetadata: true}
	plain := index.FileInfo{Name: "meta-0.9.tar.gz", URL: "https://files/meta-0.9.tar.gz"}
	svc := newTestService(t, newFakeStorage(), resolverWith(pkg, wheel, sdist, plain), time.Minute)

	plan, err := svc.Plan(t.Context(), pkg, wheel.Name+".metadata")
	require.NoError(t, err)
	assert.Equal(t, ActionStreamAndCache, plan.Action)
	assert.Equal(t, "https://files/meta-1.0-py3-none-any.whl.metadata", plan.URL)
	assert.Equal(t, "md", plan.SHA256)
	assert.Equal(t, `"md"`, plan.ETag)
	assert.Equal(t, "packages/meta/meta-1.0-py3-none-any.whl.metadata", plan.StorageKey)
	assert.Equal(t, "text/plain; charset=utf-8", plan.ContentType)
	assert.Equal(t, int64(-1), plan.Size)

	plan, err = svc.Plan(t.Context(), pkg, sdist.Name+".metadata")
	require.NoError(t, err)
	assert.Equal(t, ActionStreamAndCache, plan.Action)
	assert.Empty(t, plan.SHA256, "a bare true carries no hash to verify against")

	for _, name := range []string{plain.Name + ".metadata", "nosuch.whl.metadata", ".metadata"} {
		plan, err = svc.Plan(t.Context(), pkg, name)
		require.NoError(t, err)
		assert.Equal(t, ActionNotFound, plan.Action, name)
	}
}

// TestHandler_MetadataStreamsAndCaches drives the route end to end: the
// .metadata request goes upstream once, is verified, cached and then served from
// storage.
func TestHandler_MetadataStreamsAndCaches(t *testing.T) {
	const pkg, file = "mdpkg", "mdpkg-1.0-py3-none-any.whl"
	content := []byte("Metadata-Version: 2.1\nName: mdpkg\n")
	var hits int
	mux, st := newIndexedMuxWithMetadata(t, pkg, file, content, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".metadata") {
			http.NotFound(w, r)
			return
		}
		hits++
		_, _ = w.Write(content)
	})

	for i := range 2 {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/simple/"+pkg+"/"+file+".metadata", nil))
		resp := w.Result()
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, "request %d", i)
		assert.Equal(t, content, body)
		if i == 0 {
			// The streamed response carries the index's hash; the cached one is
			// handed to http.ServeContent, which sets its own validators.
			assert.Equal(t, `"`+sha256Hex(content)+`"`, resp.Header.Get("ETag"))
		}
	}
	assert.Equal(t, 1, hits, "the second request must come from the cache")

	exists, err := st.Exists(t.Context(), StorageKey(pkg, file+".metadata"))
	require.NoError(t, err)
	assert.True(t, exists)
}
