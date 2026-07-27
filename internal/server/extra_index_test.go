package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"log/slog"

	"github.com/huyhandes/groxpi/internal/config"
)

// fakeUpstreamIndex is one upstream index with its own package set, its own
// latency and its own hit counter, so a test can assert which index a response
// came from and which indexes were consulted at all.
type fakeUpstreamIndex struct {
	*httptest.Server
	name  string
	delay time.Duration

	mu        sync.Mutex
	packages  map[string][]string // package name -> file names it serves
	hits      map[string]int
	cancelled chan struct{} // closed the first time a query is cancelled mid-flight
	closeOnce sync.Once
}

func newFakeUpstreamIndex(t *testing.T, name string, packages map[string][]string) *fakeUpstreamIndex {
	t.Helper()
	f := &fakeUpstreamIndex{
		name:      name,
		packages:  packages,
		hits:      map[string]int{},
		cancelled: make(chan struct{}),
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeUpstreamIndex) serve(w http.ResponseWriter, r *http.Request) {
	pkg := packageFromPath(r.URL.Path)

	// Counted before the delay: a query that is cancelled while in flight still
	// counts as having consulted this index.
	f.mu.Lock()
	f.hits[pkg]++
	files := f.packages[pkg]
	f.mu.Unlock()

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			f.closeOnce.Do(func() { close(f.cancelled) })
			return
		}
	}

	if files == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	body := wireFiles{Meta: wireMeta{APIVersion: "1.0"}, Name: pkg}
	for _, file := range files {
		body.Files = append(body.Files, wireFile{
			Filename: file,
			URL:      "https://files." + f.name + ".test/" + file,
		})
	}
	w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
	_ = json.NewEncoder(w).Encode(body)
}

// packageFromPath extracts the package name from a "/<package>/" index path.
func packageFromPath(path string) string {
	if len(path) >= 2 && path[0] == '/' && path[len(path)-1] == '/' {
		return path[1 : len(path)-1]
	}
	return path
}

func (f *fakeUpstreamIndex) hitsFor(pkg string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[pkg]
}

// newMultiIndexServer wires a server whose resolution order is the extras in the
// order given, then the primary.
func newMultiIndexServer(t *testing.T, cfg *config.Config, primary *fakeUpstreamIndex, extras ...*fakeUpstreamIndex) *Server {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.IndexURL = primary.URL
	for _, extra := range extras {
		cfg.ExtraIndexURLs = append(cfg.ExtraIndexURLs, extra.URL)
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = t.TempDir()
	}
	if cfg.IndexTTL == 0 {
		cfg.IndexTTL = time.Hour
	}
	if cfg.IndexCacheSize == 0 {
		cfg.IndexCacheSize = 1 << 20
	}
	if cfg.DownloadTimeout == 0 {
		cfg.DownloadTimeout = time.Second
	}
	cfg.LogLevel = "ERROR"

	srv := New(cfg)
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// fileNames returns the filenames of the JSON index response for pkg, which
// identify the index the list came from (the URLs are rewritten to this proxy).
func fileNames(t *testing.T, srv *Server, pkg string) []string {
	t.Helper()
	body := getIndex(t, srv.Router(), pkg)
	var parsed wireFiles
	require.NoError(t, json.Unmarshal(body, &parsed))
	names := make([]string, 0, len(parsed.Files))
	for _, f := range parsed.Files {
		names = append(names, f.Filename)
	}
	return names
}

func indexStatus(t *testing.T, srv *Server, pkg string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/simple/"+pkg+"/", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	return w.Code
}

// TestResolution_ExtraIsConsultedBeforeThePrimary: extras answer first, and an
// index that never had to be asked records no hits at all.
func TestResolution_ExtraIsConsultedBeforeThePrimary(t *testing.T) {
	extra := newFakeUpstreamIndex(t, "extra", map[string][]string{"shared": {"shared-1.0-from-extra.whl"}})
	primary := newFakeUpstreamIndex(t, "primary", map[string][]string{"shared": {"shared-9.9-from-primary.whl"}})

	srv := newMultiIndexServer(t, nil, primary, extra)

	assert.Equal(t, []string{"shared-1.0-from-extra.whl"}, fileNames(t, srv, "shared"))
	assert.Equal(t, 1, extra.hitsFor("shared"))
	assert.Zero(t, primary.hitsFor("shared"), "the primary must not be consulted for a package an extra has")
}

// TestResolution_ConfiguredOrderWinsOverResponseOrder is the test that catches a
// first-to-respond implementation: the first extra is deliberately slow and the
// second is instant, and the first must still win.
func TestResolution_ConfiguredOrderWinsOverResponseOrder(t *testing.T) {
	slow := newFakeUpstreamIndex(t, "slow", map[string][]string{"shared": {"shared-from-slow-extra.whl"}})
	slow.delay = 200 * time.Millisecond
	fast := newFakeUpstreamIndex(t, "fast", map[string][]string{"shared": {"shared-from-fast-extra.whl"}})
	primary := newFakeUpstreamIndex(t, "primary", map[string][]string{"shared": {"shared-from-primary.whl"}})

	srv := newMultiIndexServer(t, nil, primary, slow, fast)

	assert.Equal(t, []string{"shared-from-slow-extra.whl"}, fileNames(t, srv, "shared"),
		"selection must follow configured order, not arrival order")
}

// TestResolution_NeverMergesFileListsAcrossIndexes: the answer is one index's
// list exactly, never a union.
func TestResolution_NeverMergesFileListsAcrossIndexes(t *testing.T) {
	extra := newFakeUpstreamIndex(t, "extra", map[string][]string{"shared": {"a-1.0.whl", "a-1.1.whl"}})
	primary := newFakeUpstreamIndex(t, "primary", map[string][]string{"shared": {"a-2.0.whl", "a-2.1.whl"}})

	srv := newMultiIndexServer(t, nil, primary, extra)

	got := fileNames(t, srv, "shared")
	assert.ElementsMatch(t, []string{"a-1.0.whl", "a-1.1.whl"}, got,
		"the file set must equal the winning index's set exactly")
	assert.Len(t, got, 2, "a union would have four files")
}

// TestResolution_FallsThroughToThePrimary: a package no extra has still resolves.
func TestResolution_FallsThroughToThePrimary(t *testing.T) {
	extraOne := newFakeUpstreamIndex(t, "extra1", map[string][]string{"internal": {"internal-1.0.whl"}})
	extraTwo := newFakeUpstreamIndex(t, "extra2", nil)
	primary := newFakeUpstreamIndex(t, "primary", map[string][]string{"numpy": {"numpy-1.26.0.whl"}})

	srv := newMultiIndexServer(t, nil, primary, extraOne, extraTwo)

	assert.Equal(t, []string{"numpy-1.26.0.whl"}, fileNames(t, srv, "numpy"))
	assert.Equal(t, 1, extraOne.hitsFor("numpy"))
	assert.Equal(t, 1, extraTwo.hitsFor("numpy"))
	assert.Equal(t, 1, primary.hitsFor("numpy"))
}

// TestResolution_NotFoundOnlyAfterEveryIndexMissed.
func TestResolution_NotFoundOnlyAfterEveryIndexMissed(t *testing.T) {
	extraOne := newFakeUpstreamIndex(t, "extra1", nil)
	extraTwo := newFakeUpstreamIndex(t, "extra2", nil)
	primary := newFakeUpstreamIndex(t, "primary", nil)

	srv := newMultiIndexServer(t, nil, primary, extraOne, extraTwo)

	assert.Equal(t, http.StatusNotFound, indexStatus(t, srv, "nonexistent"))
	for _, upstream := range []*fakeUpstreamIndex{extraOne, extraTwo, primary} {
		assert.Equal(t, 1, upstream.hitsFor("nonexistent"),
			"every configured index must have been consulted before a not-found")
	}
}

// TestResolution_EntryExpiresOnTheAnsweringIndexTTL: the extra answers with a
// short TTL while the primary's is an hour, and the entry expires on the extra's
// schedule.
func TestResolution_EntryExpiresOnTheAnsweringIndexTTL(t *testing.T) {
	extra := newFakeUpstreamIndex(t, "extra", map[string][]string{"internal": {"internal-1.0.whl"}})
	primary := newFakeUpstreamIndex(t, "primary", nil)

	cfg := &config.Config{
		IndexTTL:       time.Hour,
		ExtraIndexTTLs: []time.Duration{60 * time.Millisecond},
	}
	srv := newMultiIndexServer(t, cfg, primary, extra)

	fileNames(t, srv, "internal")
	require.Equal(t, 1, extra.hitsFor("internal"))

	// Well inside the primary's hour but past the extra's TTL.
	time.Sleep(120 * time.Millisecond)
	fileNames(t, srv, "internal")
	assert.Equal(t, 2, extra.hitsFor("internal"),
		"the entry must expire on the answering index's TTL, not the primary's")
}

// TestDependencyConfusion_PublicIndexCannotShadowAPrivatePackage is the
// regression test for the dependency-confusion attack: an attacker registers an
// internal package name on the public index, and the client must still receive
// the private index's files. Named for the attack so its purpose survives
// refactoring — do not "fix" this by merging index results.
func TestDependencyConfusion_PublicIndexCannotShadowAPrivatePackage(t *testing.T) {
	const pkg = "acme-internal-utils"
	private := newFakeUpstreamIndex(t, "private", map[string][]string{
		pkg: {"acme_internal_utils-1.4.0-py3-none-any.whl"},
	})
	public := newFakeUpstreamIndex(t, "public", map[string][]string{
		pkg: {"acme_internal_utils-99.0.0-py3-none-any.whl"}, // attacker's higher version
	})

	srv := newMultiIndexServer(t, nil, public, private)

	got := fileNames(t, srv, pkg)
	assert.Equal(t, []string{"acme_internal_utils-1.4.0-py3-none-any.whl"}, got,
		"the private file set must be what the client receives")
	assert.NotContains(t, got, "acme_internal_utils-99.0.0-py3-none-any.whl",
		"the attacker's file must never appear, not even alongside the genuine one")
	assert.Zero(t, public.hitsFor(pkg), "the public index must not even be asked")
}

// TestResolution_CancelsLowerPriorityQueriesOnceAnswered.
func TestResolution_CancelsLowerPriorityQueriesOnceAnswered(t *testing.T) {
	winner := newFakeUpstreamIndex(t, "winner", map[string][]string{"internal": {"internal-1.0.whl"}})
	loser := newFakeUpstreamIndex(t, "loser", map[string][]string{"internal": {"internal-2.0.whl"}})
	loser.delay = 10 * time.Second
	primary := newFakeUpstreamIndex(t, "primary", nil)

	srv := newMultiIndexServer(t, nil, primary, winner, loser)

	assert.Equal(t, []string{"internal-1.0.whl"}, fileNames(t, srv, "internal"))
	select {
	case <-loser.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the lower-priority query was not cancelled once a higher-priority index answered")
	}
}

// TestResolution_ConcurrentRequestsFetchEachIndexOnce: coalescing survives the
// fan-out.
func TestResolution_ConcurrentRequestsFetchEachIndexOnce(t *testing.T) {
	extra := newFakeUpstreamIndex(t, "extra", nil)
	extra.delay = 50 * time.Millisecond
	primary := newFakeUpstreamIndex(t, "primary", map[string][]string{"numpy": {"numpy-1.26.0.whl"}})
	primary.delay = 50 * time.Millisecond

	srv := newMultiIndexServer(t, nil, primary, extra)

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/simple/numpy/", nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code)
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, extra.hitsFor("numpy"))
	assert.Equal(t, 1, primary.hitsFor("numpy"))
}

// TestResolution_IndexFailureDoesNotFallThroughToThePrimary: a higher-priority
// index that fails is not a miss. Falling through would let an outage on the
// private index reopen the shadowing hole.
func TestResolution_IndexFailureDoesNotFallThroughToThePrimary(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)
	primary := newFakeUpstreamIndex(t, "primary", map[string][]string{"internal": {"internal-attacker.whl"}})

	cfg := &config.Config{IndexURL: primary.URL, ExtraIndexURLs: []string{broken.URL}}
	srv := newMultiIndexServer(t, cfg, primary)

	assert.Equal(t, http.StatusInternalServerError, indexStatus(t, srv, "internal"))
}

// --- Credential redaction ----------------------------------------------------

func TestRedactURL(t *testing.T) {
	tests := []struct {
		raw, want string
	}{
		{"https://pypi.org/simple/", "https://pypi.org/simple/"},
		{"https://user:s3cr3t@private.example.com/simple/", "https://redacted@private.example.com/simple/"},
		{"https://token@private.example.com/simple/", "https://redacted@private.example.com/simple/"},
		{"://nonsense", "[unparseable-url]"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, config.RedactURL(tt.raw), tt.raw)
	}
	assert.Equal(t, "https://redacted@private.example.com/simple/",
		config.Index{URL: "https://user:s3cr3t@private.example.com/simple/"}.Redacted())
}

// lockedBuffer collects log output from concurrent writers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// credentialedURL rewrites an upstream URL to carry user-info.
func credentialedURL(t *testing.T, raw, userinfo string) string {
	t.Helper()
	const scheme = "http://"
	require.True(t, len(raw) > len(scheme) && raw[:len(scheme)] == scheme, raw)
	return scheme + userinfo + "@" + raw[len(scheme):]
}

// TestCredentialsNeverAppearInLogsOrHealth exercises a request path, an error
// path and the health endpoint with credentials in both a primary and an extra
// index URL. This is a security assertion: any appearance of the secret fails the
// test.
func TestCredentialsNeverAppearInLogsOrHealth(t *testing.T) {
	const secret = "sup3rs3cr3tpassw0rd"

	extra := newFakeUpstreamIndex(t, "extra", map[string][]string{"internal": {"internal-1.0.whl"}})
	primary := newFakeUpstreamIndex(t, "primary", map[string][]string{"numpy": {"numpy-1.26.0.whl"}})
	// An index that fails, to drive the error path: its URL carries credentials
	// too, and net/http embeds the URL it failed on in transport errors.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(broken.Close)

	logs := &lockedBuffer{}
	restore := captureLogs(t, logs)
	defer restore()

	cfg := &config.Config{
		IndexURL: credentialedURL(t, primary.URL, "primaryuser:"+secret),
		ExtraIndexURLs: []string{
			credentialedURL(t, extra.URL, "extrauser:"+secret),
			credentialedURL(t, broken.URL, "brokenuser:"+secret),
		},
		CacheDir:        t.TempDir(),
		IndexTTL:        time.Hour,
		IndexCacheSize:  1 << 20,
		DownloadTimeout: time.Second,
		LogLevel:        "DEBUG",
	}
	srv := New(cfg)
	t.Cleanup(func() { _ = srv.Close() })

	// Request path: resolved by the first extra, which never reaches the broken one.
	require.Equal(t, http.StatusOK, indexStatus(t, srv, "internal"))

	// Error path: the first extra misses, the second fails.
	body, status := getBody(t, srv, "/simple/numpy/")
	require.Equal(t, http.StatusInternalServerError, status)
	assert.NotContains(t, body, secret, "an error surfaced to the client must not carry credentials")

	// Health payload.
	health, status := getBody(t, srv, "/health")
	require.Equal(t, http.StatusOK, status)
	assert.NotContains(t, health, secret, "the health payload must not carry credentials")
	assert.Contains(t, health, "redacted@", "the health payload should show the redacted form")

	// Home page renders the index URL too.
	home, _ := getBody(t, srv, "/")
	assert.NotContains(t, home, secret)

	assert.NotContains(t, logs.String(), secret, "credentials must appear nowhere in log output")
}

func getBody(t *testing.T, srv *Server, path string) (string, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	resp := w.Result()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return string(raw), resp.StatusCode
}

// captureLogs points the structured logger and gin's writers at w for the
// duration of a test, and returns the restore function.
func captureLogs(t *testing.T, w io.Writer) func() {
	t.Helper()
	previousLogger := slog.Default()
	previousOut, previousErr := gin.DefaultWriter, gin.DefaultErrorWriter
	slog.SetDefault(slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))
	gin.DefaultWriter, gin.DefaultErrorWriter = w, w
	return func() {
		slog.SetDefault(previousLogger)
		gin.DefaultWriter, gin.DefaultErrorWriter = previousOut, previousErr
	}
}
