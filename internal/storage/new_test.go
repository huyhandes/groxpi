package storage

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
)

// bucketServer answers every request 200, which is all New needs from S3: the
// bucket check. It records connection states so a test can see the durable
// store's idle connections closed.
type bucketServer struct {
	*httptest.Server
	mu    sync.Mutex
	conns map[net.Conn]http.ConnState
}

func newBucketServer(t *testing.T) *bucketServer {
	t.Helper()
	b := &bucketServer{conns: map[net.Conn]http.ConnState{}}
	b.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	b.Config.ConnState = func(c net.Conn, st http.ConnState) {
		b.mu.Lock()
		b.conns[c] = st
		b.mu.Unlock()
	}
	b.Start()
	t.Cleanup(b.Close)
	return b
}

// open counts connections the client has not closed.
func (b *bucketServer) open() (n, seen int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, st := range b.conns {
		if st != http.StateClosed && st != http.StateHijacked {
			n++
		}
	}
	return n, len(b.conns)
}

func s3Settings(cfg *config.Config, endpoint string) {
	cfg.S3Endpoint = endpoint
	cfg.S3Bucket = "b"
	cfg.S3AccessKeyID = "test"
	cfg.S3SecretAccessKey = "test"
	cfg.S3ForcePathStyle = true
}

// plantStale leaves a crash-leftover spool file in dir, old enough to reap.
func plantStale(t *testing.T, dir string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0750))
	p := filepath.Join(dir, tmpPrefix+"leftover")
	require.NoError(t, os.WriteFile(p, []byte("x"), 0600))
	old := time.Now().Add(-2 * touchInterval)
	require.NoError(t, os.Chtimes(p, old, old))
	return p
}

func TestNew(t *testing.T) {
	bucket := newBucketServer(t)

	cases := []struct {
		mode string
		// spool is a directory the mode keeps spool files in.
		want  func(cacheDir, localDir string) Description
		spool func(cacheDir, localDir string) string
	}{
		{
			mode:  "local",
			want:  func(c, _ string) Description { return Description{Mode: "local", CacheDir: c, CacheSize: 100} },
			spool: func(c, _ string) string { return filepath.Join(c, "packages", "demo") },
		},
		{
			mode:  "", // the zero Config is local storage
			want:  func(c, _ string) Description { return Description{Mode: "local", CacheDir: c, CacheSize: 100} },
			spool: func(c, _ string) string { return filepath.Join(c, "packages", "demo") },
		},
		{
			mode:  "hybrid",
			want:  func(_, l string) Description { return Description{Mode: "hybrid", CacheDir: l, CacheSize: 200} },
			spool: func(_, l string) string { return filepath.Join(l, "packages", "demo") },
		},
		{
			mode:  "s3",
			want:  func(string, string) Description { return Description{Mode: "s3"} },
			spool: func(c, _ string) string { return filepath.Join(c, "spool") },
		},
	}
	for _, tc := range cases {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			cfg := &config.Config{StorageType: tc.mode, CacheDir: t.TempDir(), CacheSize: 100,
				LocalCacheDir: t.TempDir(), LocalCacheSize: 200, DownloadTimeout: time.Second}
			s3Settings(cfg, bucket.URL)
			stale := plantStale(t, tc.spool(cfg.CacheDir, cfg.LocalCacheDir))

			s, err := New(cfg, &fakeFetcher{})
			require.NoError(t, err)
			t.Cleanup(func() { _ = s.Close() })

			assert.Equal(t, tc.want(cfg.CacheDir, cfg.LocalCacheDir), s.Describe())
			assert.NoFileExists(t, stale, "crash-leftover spool file reaped")
			assert.DirExists(t, filepath.Dir(stale), "spool directory exists")
			assert.False(t, s.Redirects())
		})
	}

	t.Run("s3 spool directory is created", func(t *testing.T) {
		cfg := &config.Config{StorageType: "s3", CacheDir: t.TempDir(), DownloadTimeout: time.Second}
		s3Settings(cfg, bucket.URL)
		s, err := New(cfg, &fakeFetcher{})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		assert.DirExists(t, filepath.Join(cfg.CacheDir, "spool"))
	})

	t.Run("no download budget is redirect mode", func(t *testing.T) {
		s, err := New(&config.Config{StorageType: "local", CacheDir: t.TempDir()}, &fakeFetcher{})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		assert.True(t, s.Redirects())
	})

	t.Run("an unknown mode is an error", func(t *testing.T) {
		cfg := &config.Config{StorageType: "bogus", CacheDir: t.TempDir()}
		s, err := New(cfg, &fakeFetcher{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bogus")
		assert.Nil(t, s)
	})
}

// TestNew_S3ResponseHeaderBudgetIsNotTheDownloadBudget: the download budget
// bounds a package fetch, not how long S3 may take to start answering an
// upload or delete; the S3 store keeps its own default.
func TestNew_S3ResponseHeaderBudgetIsNotTheDownloadBudget(t *testing.T) {
	bucket := newBucketServer(t)
	cfg := &config.Config{StorageType: "s3", CacheDir: t.TempDir(), DownloadTimeout: 900 * time.Millisecond}
	s3Settings(cfg, bucket.URL)
	s, err := New(cfg, &fakeFetcher{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	assert.Equal(t, 5*time.Minute, s.durable.transport.ResponseHeaderTimeout)
}

func TestNew_HybridLocalCacheFailureClosesDurable(t *testing.T) {
	bucket := newBucketServer(t)
	notADir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(notADir, nil, 0600))
	cfg := &config.Config{StorageType: "hybrid", CacheDir: t.TempDir(), LocalCacheDir: notADir,
		DownloadTimeout: time.Second}
	s3Settings(cfg, bucket.URL)

	s, err := New(cfg, &fakeFetcher{})
	require.Error(t, err)
	assert.Nil(t, s)
	require.Eventually(t, func() bool {
		n, seen := bucket.open()
		return seen > 0 && n == 0
	}, 5*time.Second, 10*time.Millisecond, "the durable store's connections are released")
}
