package download

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/index"
)

// newBodyUpstream serves one package "p" whose single file is written by body,
// and returns a download service with the given idle timeout plus the resolved
// target.
func newBodyUpstream(t *testing.T, idle time.Duration, body http.HandlerFunc) (*Service, Target) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/p/" {
			w.Header().Set("Content-Type", index.JSONContentType)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"meta":  map[string]string{"api-version": "1.0"},
				"files": []index.FileInfo{{Name: "p-1.0.tar.gz", URL: srv.URL + "/files/p-1.0.tar.gz"}},
			})
			return
		}
		body(w, r)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		IndexURL:            srv.URL,
		IndexTTL:            time.Minute,
		IndexCacheSize:      1 << 20,
		DownloadTimeout:     time.Minute,
		DownloadIdleTimeout: idle,
	}
	idx := index.New(cfg)
	t.Cleanup(idx.Close)
	dl := New(cfg, idx)
	tgt, err := dl.Resolve(t.Context(), "p", "p-1.0.tar.gz")
	require.NoError(t, err)
	return dl, tgt
}

// TestFetch_StalledBodyFails pins that an upstream body that stops sending
// bytes fails with errUpstreamStalled after about the idle timeout instead of
// hanging every request tailing it, and that the error names no URL.
func TestFetch_StalledBodyFails(t *testing.T) {
	const idle = 200 * time.Millisecond
	release := make(chan struct{})
	dl, tgt := newBodyUpstream(t, idle, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		_, _ = io.WriteString(w, "01234")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })

	f, err := dl.Fetch(t.Context(), tgt)
	require.NoError(t, err)
	defer func() { _ = f.Body.Close() }()

	start := time.Now()
	got, err := io.ReadAll(f.Body)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(err, errUpstreamStalled), "want errUpstreamStalled, got %v", err)
	assert.Equal(t, "01234", string(got))
	assert.Less(t, elapsed, 2*idle, "stall took %v to surface", elapsed)
	assert.NotContains(t, err.Error(), "http")
	assert.NotContains(t, err.Error(), "p-1.0.tar.gz")
}

// TestFetch_DribblingBodyCompletes pins that the idle timeout bounds each gap,
// not the transfer: a body that sends a byte within every idle window completes
// intact although the whole transfer outlasts the timeout.
func TestFetch_DribblingBodyCompletes(t *testing.T) {
	const idle = 200 * time.Millisecond
	const payload = "0123456789"
	dl, tgt := newBodyUpstream(t, idle, func(w http.ResponseWriter, _ *http.Request) {
		for _, b := range []byte(payload) {
			_, _ = w.Write([]byte{b})
			w.(http.Flusher).Flush()
			time.Sleep(idle / 4)
		}
	})

	f, err := dl.Fetch(t.Context(), tgt)
	require.NoError(t, err)
	defer func() { _ = f.Body.Close() }()

	got, err := io.ReadAll(f.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, string(got))
}

// TestFetch_GzipLabelledBodyIsNotDecoded pins that a package file is fetched
// byte for byte. An upstream that labels a .tar.gz "Content-Encoding: gzip"
// must not have net/http transparently decompress it: the cached bytes would
// differ from the advertised hash.
func TestFetch_GzipLabelledBodyIsNotDecoded(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte("tarball contents"))
	require.NoError(t, zw.Close())
	upstream := buf.Bytes()

	dl, tgt := newBodyUpstream(t, time.Minute, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(upstream)
	})

	f, err := dl.Fetch(t.Context(), tgt)
	require.NoError(t, err)
	defer func() { _ = f.Body.Close() }()
	got, err := io.ReadAll(f.Body)
	require.NoError(t, err)
	assert.Equal(t, upstream, got)
}
