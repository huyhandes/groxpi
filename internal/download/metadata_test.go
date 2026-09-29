package download

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/index"
)

// newService builds the fetcher over a real index.Service whose upstream is an
// httptest simple index serving files per package (404 for any other).
func newService(t *testing.T, files map[string][]index.FileInfo, ttfb time.Duration) *Service {
	t.Helper()
	simple := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fi, ok := files[strings.Trim(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"api-version": "1.0"}, "files": fi})
	}))
	t.Cleanup(simple.Close)
	cfg := &config.Config{IndexURL: simple.URL, IndexTTL: time.Minute, IndexCacheSize: 1 << 20, DownloadTimeout: ttfb}
	return New(cfg, index.New(cfg))
}

func TestMetadataURL(t *testing.T) {
	assert.Equal(t, "https://h/f/x.whl.metadata", metadataURL("https://h/f/x.whl"))
	assert.Equal(t, "https://h/f/x.whl.metadata", metadataURL("https://h/f/x.whl#sha256=abc"))
	assert.Equal(t, "https://h/f/x.whl.metadata?t=1", metadataURL("https://h/f/x.whl?t=1#sha256=abc"))
}

// TestResolve pins resolution: listed files, PEP 658 siblings only when
// advertised, ErrNotListed for anything else, and index failures kept apart.
func TestResolve(t *testing.T) {
	const pkg = "meta"
	wheel := index.FileInfo{Name: "meta-1.0-py3-none-any.whl", URL: "https://files/meta-1.0-py3-none-any.whl#sha256=whl", CoreMetadata: map[string]any{"sha256": "md"}, Size: 10, Hashes: map[string]string{"sha256": "whl"}}
	sdist := index.FileInfo{Name: "meta-1.0.tar.gz", URL: "https://files/meta-1.0.tar.gz", DistInfoMetadata: true}
	plain := index.FileInfo{Name: "meta-0.9.tar.gz", URL: "https://files/meta-0.9.tar.gz"}
	svc := newService(t, map[string][]index.FileInfo{pkg: {wheel, sdist, plain}}, time.Minute)

	got, err := svc.Resolve(t.Context(), pkg, wheel.Name)
	require.NoError(t, err)
	assert.Equal(t, Target{URL: wheel.URL, SHA256: "whl", Size: 10}, got)

	got, err = svc.Resolve(t.Context(), pkg, wheel.Name+".metadata")
	require.NoError(t, err)
	assert.Equal(t, Target{URL: "https://files/meta-1.0-py3-none-any.whl.metadata", SHA256: "md", Size: -1}, got)

	got, err = svc.Resolve(t.Context(), pkg, sdist.Name+".metadata")
	require.NoError(t, err)
	assert.Empty(t, got.SHA256, "a bare true carries no hash to verify against")

	for _, name := range []string{plain.Name + ".metadata", "nosuch.whl.metadata", ".metadata", "nosuch.tar.gz"} {
		_, err = svc.Resolve(t.Context(), pkg, name)
		assert.ErrorIs(t, err, ErrNotListed, name)
	}

	_, err = svc.Resolve(t.Context(), "nope", "nope-1.0.tar.gz")
	assert.ErrorIs(t, err, index.ErrNotFound)
	assert.False(t, errors.Is(err, ErrNotListed))
}

// TestFetch covers the budget (headers only, never the body), upstream failure
// and credential redaction.
func TestFetch(t *testing.T) {
	const secret = "sup3rs3cr3t"
	body := []byte("payload")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slowhdr":
			time.Sleep(300 * time.Millisecond)
		case "/slowbody":
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			_, _ = w.Write(body[:1])
			w.(http.Flusher).Flush()
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write(body[1:])
			return
		case "/gone":
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	u.User = url.UserPassword("deploy", secret)
	var fi []index.FileInfo
	for _, n := range []string{"slowhdr", "slowbody", "gone"} {
		fi = append(fi, index.FileInfo{Name: n, URL: u.String() + "/" + n})
	}
	svc := newService(t, map[string][]index.FileInfo{"p": fi}, 100*time.Millisecond)

	f, err := svc.Fetch(t.Context(), "p", "slowbody")
	require.NoError(t, err)
	got, err := io.ReadAll(f.Body)
	require.NoError(t, err, "the budget must not cut off a body already streaming")
	require.NoError(t, f.Body.Close())
	assert.Equal(t, body, got)
	assert.Equal(t, int64(len(body)), f.Length)

	for _, name := range []string{"slowhdr", "gone"} {
		_, err = svc.Fetch(t.Context(), "p", name)
		require.Error(t, err, name)
		assert.False(t, strings.Contains(err.Error(), secret), "credential leaked: %v", err)
	}
}
