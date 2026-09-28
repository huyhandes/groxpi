package server_test

// HTTP characterization suite (issue #60, ADR 0004). It pins today's externally
// visible behaviour of server.Router() for local and hybrid storage, asserting
// status, headers and body only. It builds the server through config +
// NewServer + Router and touches no index/download/storage/admin type, so the
// module refactor must leave it passing unchanged.

import (
	"bytes"
	"compress/gzip"
	"crypto/md5" // #nosec G501 -- S3 ETag shape in a test double, not security
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/server"
)

const (
	charUser = "op"
	charPass = "s3cret"
	bucket   = "groxpi"

	absent  = "\x00absent"
	present = "\x00present"
)

// ---- upstream PyPI double -------------------------------------------------

type upFile struct {
	body    []byte
	sha256  string // advertised
	size    int64  // advertised, 0 = omitted
	metaSHA string // advertised core-metadata sha256, "" = not advertised
	gated   bool   // first half, flush, wait for release, rest
}

type upstream struct {
	*httptest.Server
	pkgs    map[string][]string // package -> filenames
	files   map[string]upFile   // filename -> file (".metadata" names included)
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	hits    map[string]int
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func payload(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i%251) ^ seed
	}
	return b
}

var (
	demoWheel = payload(4096, 1)
	demoMeta  = []byte("Metadata-Version: 2.1\nName: demo\nVersion: 1.0.0\n")
	demoOld   = payload(16384, 2) // > net/http buffer: forces chunked when unsized
	badBody   = payload(1024, 3)
	slowBody  = payload(65536, 4) // half exceeds net/http buffering, so headers flush
)

const (
	demoFile = "demo-1.0.0.tar.gz"
	oldFile  = "demo-0.9.0.tar.gz"
	badFile  = "bad-1.0.0.tar.gz"
	slowFile = "slow-1.0.0.tar.gz"
)

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	up := &upstream{
		pkgs: map[string][]string{
			"demo": {demoFile, oldFile},
			"bad":  {badFile},
			"slow": {slowFile},
		},
		files: map[string]upFile{
			demoFile:               {body: demoWheel, sha256: sum(demoWheel), size: int64(len(demoWheel)), metaSHA: sum(demoMeta)},
			demoFile + ".metadata": {body: demoMeta},
			oldFile:                {body: demoOld, sha256: sum(demoOld)},
			badFile:                {body: badBody, sha256: sum([]byte("something else"))},
			slowFile:               {body: slowBody, sha256: sum(slowBody), size: int64(len(slowBody)), gated: true},
		},
		release: make(chan struct{}),
		hits:    map[string]int{},
	}
	up.Server = httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(up.Close)
	t.Cleanup(up.open) // runs first: never leave a gated handler blocking Close
	return up
}

func (u *upstream) open() { u.once.Do(func() { close(u.release) }) }

func (u *upstream) fileHits(name string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[name]
}

func (u *upstream) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/":
		u.serveRoot(w, r)
	case path == "/broken/":
		http.Error(w, "boom", http.StatusInternalServerError)
	case strings.HasPrefix(path, "/files/"):
		name := strings.TrimPrefix(path, "/files/")
		f, ok := u.files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		u.mu.Lock()
		u.hits[name]++
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(f.body)))
		if !f.gated {
			_, _ = w.Write(f.body)
			return
		}
		half := len(f.body) / 2
		_, _ = w.Write(f.body[:half])
		w.(http.Flusher).Flush()
		<-u.release
		_, _ = w.Write(f.body[half:])
	default:
		pkg := strings.Trim(path, "/")
		names, ok := u.pkgs[pkg]
		if !ok {
			http.NotFound(w, r)
			return
		}
		files := make([]map[string]any, 0, len(names))
		for _, n := range names {
			f := u.files[n]
			e := map[string]any{"filename": n, "url": u.URL + "/files/" + n, "hashes": map[string]string{"sha256": f.sha256}}
			if f.size > 0 {
				e["size"] = f.size
			}
			if f.metaSHA != "" {
				e["core-metadata"] = map[string]string{"sha256": f.metaSHA}
			}
			files = append(files, e)
		}
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"api-version": "1.0"}, "name": pkg, "files": files})
	}
}

const (
	rootHTML = `<!DOCTYPE html><html><body><a href="/simple/demo/">demo</a></body></html>`
	rootJSON = `{"meta":{"api-version":"1.0"},"projects":[{"name":"demo"}]}`
)

func (u *upstream) serveRoot(w http.ResponseWriter, r *http.Request) {
	body, ctype := rootHTML, "text/html"
	if strings.Contains(r.Header.Get("Accept"), "json") {
		body, ctype = rootJSON, "application/vnd.pypi.simple.v1+json"
	}
	w.Header().Set("Content-Type", ctype)
	if r.Header.Get("Accept-Encoding") == "gzip" {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gz(body))
		return
	}
	_, _ = io.WriteString(w, body)
}

func gz(s string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(s))
	_ = zw.Close()
	return buf.Bytes()
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	return out
}

// ---- minimal in-process S3 double (path-style, map-backed) ----------------
//
// Handles exactly what the S3 backend calls: HeadBucket, Head/Get/Put/Delete
// Object, ListObjectsV2 and DeleteObjects. Signatures are not checked.

type s3Obj struct {
	body  []byte
	ctype string
	mod   time.Time
}

type fakeS3 struct {
	*httptest.Server
	mu   sync.Mutex
	objs map[string]s3Obj
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()
	f := &fakeS3{objs: map[string]s3Obj{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeS3) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objs[key]
	return ok
}

func etagOf(b []byte) string { s := md5.Sum(b); return `"` + hex.EncodeToString(s[:]) + `"` } // #nosec G401

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/"+bucket)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	key := strings.TrimPrefix(rest, "/")
	f.mu.Lock()
	defer f.mu.Unlock()

	if key == "" {
		switch {
		case r.Method == http.MethodHead:
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			prefix := r.URL.Query().Get("prefix")
			var sb strings.Builder
			n := 0
			for k, o := range f.objs {
				if strings.HasPrefix(k, prefix) {
					n++
					fmt.Fprintf(&sb, "<Contents><Key>%s</Key><Size>%d</Size></Contents>", k, len(o.body))
				}
			}
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<ListBucketResult><Name>%s</Name><Prefix>%s</Prefix><KeyCount>%d</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>%s</ListBucketResult>`,
				bucket, prefix, n, sb.String())
		case r.Method == http.MethodPost && r.URL.Query().Has("delete"):
			var req struct {
				Objects []struct{ Key string } `xml:"Object"`
			}
			_ = xml.NewDecoder(r.Body).Decode(&req)
			for _, o := range req.Objects {
				delete(f.objs, o.Key)
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<DeleteResult></DeleteResult>`)
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
		return
	}

	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.objs[key] = s3Obj{body: body, ctype: r.Header.Get("Content-Type"), mod: time.Now().UTC()}
		w.Header().Set("ETag", etagOf(body))
	case http.MethodDelete:
		delete(f.objs, key)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet, http.MethodHead:
		o, ok := f.objs[key]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
			}
			return
		}
		w.Header().Set("Content-Type", o.ctype)
		w.Header().Set("ETag", etagOf(o.body))
		http.ServeContent(w, r, "", o.mod, bytes.NewReader(o.body)) // Range, Content-Length, Last-Modified
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// ---- harness --------------------------------------------------------------

type env struct {
	mode string
	up   *upstream
	s3   *fakeS3 // hybrid and s3 only
}

func (e *env) config(t *testing.T, opts ...func(*config.Config)) *config.Config {
	t.Helper()
	cfg := &config.Config{
		IndexURL:        e.up.URL + "/",
		IndexTTL:        time.Minute,
		IndexCacheSize:  1 << 20,
		CacheDir:        t.TempDir(),
		CacheSize:       1 << 30,
		DownloadTimeout: 5 * time.Second,
		LogLevel:        "ERROR",
		StorageType:     e.mode,
	}
	if e.mode == "hybrid" {
		cfg.LocalCacheDir = t.TempDir()
		cfg.LocalCacheSize = 1 << 30
	}
	if e.s3 != nil {
		cfg.S3Endpoint = e.s3.URL
		cfg.S3Bucket = bucket
		cfg.S3Region = "us-east-1"
		cfg.S3AccessKeyID = "test"
		cfg.S3SecretAccessKey = "test"
		cfg.S3ForcePathStyle = true
	}
	for _, o := range opts {
		o(cfg)
	}
	return cfg
}

func withAdmin(cfg *config.Config) { cfg.AdminUsername, cfg.AdminPassword = charUser, charPass }

type proxy struct {
	t      *testing.T
	url    string
	client *http.Client
	stop   func()
	settle func() // pure s3: wait until the upload behind a miss is committed
}

// start serves cfg through a real HTTP server so HEAD, Content-Length and
// streaming behave exactly as they do in production.
func start(t *testing.T, cfg *config.Config) *proxy {
	t.Helper()
	srv, err := server.NewServer(cfg)
	require.NoError(t, err)
	hs := httptest.NewServer(srv.Router())
	var once sync.Once
	stop := func() { once.Do(func() { hs.Close(); _ = srv.Close() }) }
	t.Cleanup(stop)
	settle := func() {}
	if cfg.StorageType == "s3" {
		settle = func() {
			require.Eventually(t, func() bool { return server.InFlight(srv) == 0 }, 5*time.Second, time.Millisecond)
		}
	}
	return &proxy{
		t:      t,
		url:    hs.URL,
		settle: settle,
		client: &http.Client{
			Transport:     &http.Transport{DisableCompression: true},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		stop: stop,
	}
}

// do sends a request; hdr is key/value pairs. The body is read and closed.
func (p *proxy) do(method, path string, body io.Reader, hdr ...string) (*http.Response, []byte) {
	p.t.Helper()
	req, err := http.NewRequest(method, p.url+path, body)
	require.NoError(p.t, err)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := p.client.Do(req)
	require.NoError(p.t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(p.t, err)
	p.settle()
	return resp, b
}

func (p *proxy) get(path string, hdr ...string) (*http.Response, []byte) {
	p.t.Helper()
	return p.do(http.MethodGet, path, nil, hdr...)
}

func (p *proxy) admin(method, path string, body io.Reader, hdr ...string) (*http.Response, []byte) {
	p.t.Helper()
	req, err := http.NewRequest(method, p.url+path, body)
	require.NoError(p.t, err)
	req.SetBasicAuth(charUser, charPass)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := p.client.Do(req)
	require.NoError(p.t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(p.t, err)
	return resp, b
}

// pin asserts each header: an exact value, absent, or present with any value.
func pin(t *testing.T, resp *http.Response, want map[string]string) {
	t.Helper()
	for k, v := range want {
		got := resp.Header.Values(k)
		switch v {
		case absent:
			assert.Empty(t, got, "header %s must be absent", k)
		case present:
			assert.NotEmpty(t, got, "header %s must be present", k)
		default:
			assert.Equal(t, []string{v}, got, "header %s", k)
		}
	}
}

func q(s string) string { return `"` + s + `"` }

// Header sets per serve branch, as sent today (#52 unifies them later).
func streamedMissHeaders(ctype, etag string, length int) map[string]string {
	h := map[string]string{
		"Content-Type": ctype, "ETag": etag,
		"Accept-Ranges": absent, "Last-Modified": absent,
		"Content-Disposition": absent, "Cache-Control": absent,
	}
	// Unsized: net/http decides (buffered small body gets a length, else chunked).
	if length > 0 {
		h["Content-Length"] = fmt.Sprint(length)
	}
	return h
}

func cachedHitHeaders(ctype string, length int) map[string]string {
	return map[string]string{
		"Content-Type": ctype, "Content-Length": fmt.Sprint(length),
		"Accept-Ranges": "bytes", "Last-Modified": present, "ETag": absent,
		"Content-Disposition": absent, "Cache-Control": absent,
	}
}

func decodeJSON(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal(b, &v), "body: %s", b)
	return v
}

// ---- the suite ------------------------------------------------------------

func TestCharacterization(t *testing.T) {
	for _, mode := range []string{"local", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			newEnv := func(t *testing.T) *env {
				e := &env{mode: mode, up: newUpstream(t)}
				if mode == "hybrid" {
					e.s3 = newFakeS3(t)
				}
				return e
			}
			t.Run("index", func(t *testing.T) { characterizeIndex(t, newEnv(t)) })
			t.Run("download", func(t *testing.T) { characterizeDownload(t, newEnv) })
			t.Run("redirect_mode", func(t *testing.T) { characterizeRedirectMode(t, newEnv(t)) })
			t.Run("admin", func(t *testing.T) { characterizeAdmin(t, newEnv) })
			t.Run("home_and_health", func(t *testing.T) { characterizeHomeHealth(t, newEnv(t), mode) })
		})
	}
}

// TestCharacterizationS3 runs the download suite against a pure-s3 backend, so
// S3 hits are held to the same Range and conditional behaviour as local hits.
func TestCharacterizationS3(t *testing.T) {
	characterizeDownload(t, func(t *testing.T) *env {
		return &env{mode: "s3", up: newUpstream(t), s3: newFakeS3(t)}
	})
}

func characterizeIndex(t *testing.T, e *env) {
	p := start(t, e.config(t))
	const jsonAccept = "application/vnd.pypi.simple.v1+json"

	wantPkgJSON := map[string]any{
		"meta": map[string]any{"api-version": "1.0"},
		"name": "demo",
		"files": []any{
			map[string]any{
				"filename": demoFile, "url": "/simple/demo/" + demoFile,
				"hashes":             map[string]any{"sha256": sum(demoWheel)}, // upstream "size" is not re-emitted today
				"core-metadata":      map[string]any{"sha256": sum(demoMeta)},
				"dist-info-metadata": map[string]any{"sha256": sum(demoMeta)},
			},
			map[string]any{
				"filename": oldFile, "url": "/simple/demo/" + oldFile,
				"hashes": map[string]any{"sha256": sum(demoOld)},
			},
		},
	}
	wantPkgHTML := "<!DOCTYPE html>\n<html>\n<head><title>Links for demo</title></head>\n<body>\n\t<h1>Links for demo</h1>\n" +
		"\t<a href=\"/simple/demo/" + demoFile + "\" data-core-metadata=\"sha256=" + sum(demoMeta) + "\" data-dist-info-metadata=\"sha256=" + sum(demoMeta) + "\">" + demoFile + "</a><br>\n" +
		"\t<a href=\"/simple/demo/" + oldFile + "\">" + oldFile + "</a><br>\n" +
		"</body>\n</html>"

	t.Run("root_html_default", func(t *testing.T) {
		resp, body := p.get("/simple/")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": "text/html", "Vary": "Accept, Accept-Encoding", "Content-Encoding": absent})
		assert.Equal(t, rootHTML, string(body))
	})
	t.Run("root_json_by_accept", func(t *testing.T) {
		resp, body := p.get("/simple/", "Accept", jsonAccept)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": jsonAccept, "Vary": "Accept, Accept-Encoding"})
		assert.Equal(t, rootJSON, string(body))
	})
	t.Run("root_json_by_format_query", func(t *testing.T) {
		resp, body := p.get("/simple/?format="+url.QueryEscape(jsonAccept), "Accept", "text/html")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, rootJSON, string(body))
	})
	t.Run("root_gzip_passthrough", func(t *testing.T) {
		resp, body := p.get("/simple/", "Accept", jsonAccept, "Accept-Encoding", "gzip, deflate")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Encoding": "gzip", "Content-Type": jsonAccept, "Vary": "Accept, Accept-Encoding"})
		assert.Equal(t, gz(rootJSON), body, "root body is passed through byte for byte")
	})
	t.Run("root_index_alias", func(t *testing.T) {
		resp, body := p.get("/index/")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, rootHTML, string(body))
	})
	t.Run("root_head", func(t *testing.T) {
		resp, body := p.do(http.MethodHead, "/simple/", nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": "text/html", "Vary": "Accept, Accept-Encoding", "Content-Length": fmt.Sprint(len(rootHTML))})
		assert.Empty(t, body)
	})

	t.Run("package_html_default", func(t *testing.T) {
		resp, body := p.get("/simple/demo/")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": "text/html", "Vary": absent, "Content-Encoding": absent})
		assert.Equal(t, wantPkgHTML, string(body))
	})
	t.Run("package_json_by_accept", func(t *testing.T) {
		resp, body := p.get("/simple/demo/", "Accept", jsonAccept)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": jsonAccept, "Vary": "Accept-Encoding", "Content-Encoding": absent})
		assert.Equal(t, wantPkgJSON, decodeJSON(t, body))
	})
	t.Run("package_json_by_format_query", func(t *testing.T) {
		resp, body := p.get("/simple/demo/?format=" + url.QueryEscape(jsonAccept))
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, wantPkgJSON, decodeJSON(t, body))
	})
	t.Run("package_format_query_beats_accept", func(t *testing.T) {
		resp, body := p.get("/simple/demo/?format=text/html", "Accept", jsonAccept)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, wantPkgHTML, string(body))
	})
	t.Run("package_json_gzip", func(t *testing.T) {
		resp, body := p.get("/simple/demo/", "Accept", jsonAccept, "Accept-Encoding", "gzip")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": jsonAccept, "Vary": "Accept-Encoding", "Content-Encoding": "gzip"})
		assert.Equal(t, wantPkgJSON, decodeJSON(t, gunzip(t, body)))
	})
	t.Run("package_html_ignores_gzip", func(t *testing.T) {
		resp, body := p.get("/simple/demo/", "Accept-Encoding", "gzip")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Encoding": absent})
		assert.Equal(t, wantPkgHTML, string(body))
	})
	t.Run("package_index_alias", func(t *testing.T) {
		resp, body := p.get("/index/demo")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, wantPkgHTML, string(body))
	})
	t.Run("package_name_normalized", func(t *testing.T) {
		resp, body := p.get("/simple/Demo/")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, wantPkgHTML, string(body))
	})
	t.Run("package_missing_slash_redirects", func(t *testing.T) {
		resp, _ := p.get("/simple/demo")
		assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
		pin(t, resp, map[string]string{"Location": "/simple/demo/"})
	})
	t.Run("package_unknown_404", func(t *testing.T) {
		resp, body := p.get("/simple/nope/")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": "text/plain; charset=utf-8"})
		assert.Equal(t, "Package not found\n", string(body))
	})
	t.Run("package_upstream_error_500", func(t *testing.T) {
		resp, body := p.get("/simple/broken/")
		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		assert.True(t, strings.HasPrefix(string(body), "Error fetching package: "), "body: %s", body)
	})
	t.Run("package_head", func(t *testing.T) {
		resp, body := p.do(http.MethodHead, "/simple/demo/", nil, "Accept", jsonAccept)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": jsonAccept, "Vary": "Accept-Encoding", "Content-Length": present})
		assert.Empty(t, body)
	})
	t.Run("post_is_405", func(t *testing.T) {
		resp, _ := p.do(http.MethodPost, "/simple/demo/", nil)
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("Allow"), "GET")
	})
}

func characterizeDownload(t *testing.T, newEnv func(*testing.T) *env) {
	const gzipType = "application/gzip"
	demoPath := "/simple/demo/" + demoFile

	t.Run("miss_streams_and_caches_then_hit", func(t *testing.T) {
		e := newEnv(t)
		p := start(t, e.config(t))

		resp, body := p.get(demoPath)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, streamedMissHeaders(gzipType, q(sum(demoWheel)), len(demoWheel)))
		assert.Equal(t, demoWheel, body)

		resp, body = p.get(demoPath)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, cachedHitHeaders(gzipType, len(demoWheel)))
		assert.Equal(t, demoWheel, body)
		assert.Equal(t, 1, e.up.fileHits(demoFile), "the hit is served without going upstream")

		resp, body = p.get("/index/demo/" + demoFile)
		assert.Equal(t, http.StatusOK, resp.StatusCode, "/index/ alias serves the same cached file")
		assert.Equal(t, demoWheel, body)
	})

	t.Run("miss_without_advertised_size_is_chunked", func(t *testing.T) {
		e := newEnv(t)
		p := start(t, e.config(t))
		resp, body := p.get("/simple/demo/" + oldFile)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, streamedMissHeaders(gzipType, q(sum(demoOld)), 0))
		pin(t, resp, map[string]string{"Content-Length": absent})
		assert.Equal(t, []string{"chunked"}, resp.TransferEncoding)
		assert.Equal(t, demoOld, body)
	})

	t.Run("head_miss_and_hit", func(t *testing.T) {
		e := newEnv(t)
		p := start(t, e.config(t))

		resp, body := p.do(http.MethodHead, demoPath, nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, streamedMissHeaders(gzipType, q(sum(demoWheel)), len(demoWheel)))
		assert.Empty(t, body)

		resp, body = p.do(http.MethodHead, demoPath, nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, cachedHitHeaders(gzipType, len(demoWheel)))
		assert.Empty(t, body)
	})

	t.Run("cached_range_and_conditionals", func(t *testing.T) {
		e := newEnv(t)
		p := start(t, e.config(t))
		_, _ = p.get(demoPath) // warm

		resp, body := p.get(demoPath, "Range", "bytes=0-9")
		assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
		pin(t, resp, map[string]string{
			"Content-Range": fmt.Sprintf("bytes 0-9/%d", len(demoWheel)), "Content-Length": "10",
			"Content-Type": gzipType, "Accept-Ranges": "bytes",
		})
		assert.Equal(t, demoWheel[:10], body)

		resp, _ = p.get(demoPath)
		lastMod := resp.Header.Get("Last-Modified")
		require.NotEmpty(t, lastMod)

		resp, body = p.get(demoPath, "If-Modified-Since", lastMod)
		assert.Equal(t, http.StatusNotModified, resp.StatusCode)
		pin(t, resp, map[string]string{"Last-Modified": lastMod, "Content-Length": absent, "ETag": absent})
		assert.Empty(t, body)

		// No ETag is sent on a cached hit today, so If-None-Match never matches.
		resp, body = p.get(demoPath, "If-None-Match", q(sum(demoWheel)))
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, demoWheel, body)

		resp, body = p.get(demoPath, "If-None-Match", "*")
		assert.Equal(t, http.StatusNotModified, resp.StatusCode, "* matches any existing file")
		assert.Empty(t, body)
	})

	t.Run("cached_multi_range", func(t *testing.T) {
		e := newEnv(t)
		p := start(t, e.config(t))
		_, _ = p.get(demoPath) // warm

		resp, body := p.get(demoPath, "Range", "bytes=20-29,0-9")
		assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
		assert.True(t, strings.HasPrefix(resp.Header.Get("Content-Type"), "multipart/byteranges"))
		assert.True(t, bytes.Contains(body, demoWheel[20:30]))
		assert.True(t, bytes.Contains(body, demoWheel[:10]))
	})

	t.Run("coalesced_second_client", func(t *testing.T) {
		e := newEnv(t)
		p := start(t, e.config(t))
		slowPath := "/simple/slow/" + slowFile

		// Leader: headers arrive once the first half is on the wire, so the
		// download is provably still in flight when the follower is sent.
		leader, err := p.client.Get(p.url + slowPath)
		require.NoError(t, err)
		defer func() { _ = leader.Body.Close() }()
		assert.Equal(t, http.StatusOK, leader.StatusCode)
		pin(t, leader, streamedMissHeaders(gzipType, q(sum(slowBody)), len(slowBody)))
		head := make([]byte, len(slowBody)/2)
		_, err = io.ReadFull(leader.Body, head)
		require.NoError(t, err)

		type result struct {
			resp *http.Response
			body []byte
		}
		follower := make(chan result, 1)
		go func() {
			resp, err := p.client.Get(p.url + slowPath)
			if err != nil {
				follower <- result{}
				return
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			follower <- result{resp, b}
		}()
		// ponytail: no external signal says the follower has joined the flight;
		// the outcome below is identical either way, only the sharing is timing.
		time.Sleep(50 * time.Millisecond)
		e.up.open()

		rest, err := io.ReadAll(leader.Body)
		require.NoError(t, err)
		assert.Equal(t, slowBody, append(head, rest...))

		r := <-follower
		require.NotNil(t, r.resp)
		assert.Equal(t, http.StatusOK, r.resp.StatusCode)
		// Follower headers not pinned: #62 has it tail the spool (streamed headers).
		assert.Equal(t, slowBody, r.body)
		assert.Equal(t, 1, e.up.fileHits(slowFile), "one upstream fetch serves both clients")
	})

	t.Run("range_ignored_during_inflight_download", func(t *testing.T) {
		e := newEnv(t)
		p := start(t, e.config(t))
		req, err := http.NewRequest(http.MethodGet, p.url+"/simple/slow/"+slowFile, nil)
		require.NoError(t, err)
		req.Header.Set("Range", "bytes=0-9")
		resp, err := p.client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		e.up.open()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Range": absent, "Content-Length": fmt.Sprint(len(slowBody))})
		assert.Equal(t, slowBody, body)
	})

	t.Run("sha256_mismatch_is_not_cached", func(t *testing.T) {
		e := newEnv(t)
		p := start(t, e.config(t))
		badPath := "/simple/bad/" + badFile

		// A reader never gets a clean end on bad bytes: the response is aborted
		// before the final byte. Nothing is cached, so a retry goes upstream.
		fetch := func() {
			resp, err := p.client.Get(p.url + badPath)
			if err != nil {
				return // aborted before the status line: not a redirect either
			}
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusOK, resp.StatusCode,
				"a failed verification must not redirect the client to the bytes that failed it")
			body, err := io.ReadAll(resp.Body)
			assert.False(t, err == nil && bytes.Equal(body, badBody),
				"a failed verification must not complete cleanly with the full body")
		}
		fetch()
		fetch()
		assert.Equal(t, 2, e.up.fileHits(badFile), "a failed verification is refetched, not served from cache")
	})

	t.Run("not_listed_404", func(t *testing.T) {
		e := newEnv(t)
		p := start(t, e.config(t))

		resp, body := p.get("/simple/demo/demo-9.9.9.tar.gz")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": "text/plain; charset=utf-8"})
		assert.Equal(t, "File not found\n", string(body))

		resp, body = p.get("/simple/nope/nope-1.0.tar.gz")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		assert.Equal(t, "Package not found\n", string(body))

		resp, _ = p.do(http.MethodHead, "/simple/demo/demo-9.9.9.tar.gz", nil)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("pep658_metadata", func(t *testing.T) {
		e := newEnv(t)
		p := start(t, e.config(t))
		metaPath := demoPath + ".metadata"
		const textType = "text/plain; charset=utf-8"

		resp, body := p.get(metaPath)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, streamedMissHeaders(textType, q(sum(demoMeta)), 0))
		assert.Equal(t, demoMeta, body)

		resp, body = p.get(metaPath)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, cachedHitHeaders(textType, len(demoMeta)))
		assert.Equal(t, demoMeta, body)
		assert.Equal(t, 1, e.up.fileHits(demoFile+".metadata"))

		resp, body = p.get("/simple/demo/" + oldFile + ".metadata")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "metadata not advertised by the index")
		assert.Equal(t, "File not found\n", string(body))
	})

	if e := newEnv(t); e.mode == "hybrid" {
		t.Run("hit_from_remote_tier", func(t *testing.T) {
			first := e.config(t)
			p := start(t, first)
			_, _ = p.get(demoPath)
			p.stop() // drains the queued upload to the object store
			require.True(t, e.s3.has("packages/demo/"+demoFile))

			second := *first
			second.LocalCacheDir = t.TempDir() // cold L1, warm L2
			p = start(t, &second)
			resp, body := p.get(demoPath)
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			// Only Content-Length pinned: #63 serves S3 hits via ServeContent.
			pin(t, resp, map[string]string{"Content-Length": fmt.Sprint(len(demoWheel))})

			assert.Equal(t, demoWheel, body)
			assert.Equal(t, 1, e.up.fileHits(demoFile))
		})
	}
}

func characterizeRedirectMode(t *testing.T, e *env) {
	warm := e.config(t)
	p := start(t, warm)
	_, _ = p.get("/simple/demo/" + demoFile)
	p.stop()

	redirect := *warm
	redirect.DownloadTimeout = 0
	p = start(t, &redirect)

	t.Run("cached_is_served", func(t *testing.T) {
		resp, body := p.get("/simple/demo/" + demoFile)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, cachedHitHeaders("application/gzip", len(demoWheel)))
		assert.Equal(t, demoWheel, body)
	})
	t.Run("uncached_is_302", func(t *testing.T) {
		resp, _ := p.get("/simple/demo/" + oldFile)
		assert.Equal(t, http.StatusFound, resp.StatusCode)
		pin(t, resp, map[string]string{"Location": e.up.URL + "/files/" + oldFile})
		assert.Equal(t, 0, e.up.fileHits(oldFile))
	})
	t.Run("uncached_metadata_is_302", func(t *testing.T) {
		resp, _ := p.get("/simple/demo/" + demoFile + ".metadata")
		assert.Equal(t, http.StatusFound, resp.StatusCode)
		pin(t, resp, map[string]string{"Location": e.up.URL + "/files/" + demoFile + ".metadata"})
	})
}

func characterizeAdmin(t *testing.T, newEnv func(*testing.T) *env) {
	surface := []struct{ method, path string }{
		{http.MethodGet, "/admin"},
		{http.MethodGet, "/admin/rows"},
		{http.MethodGet, "/admin/htmx.min.js"},
		{http.MethodPost, "/admin/prefetch"},
		{http.MethodDelete, "/cache/list"},
		{http.MethodDelete, "/cache/demo"},
	}

	t.Run("unconfigured_404", func(t *testing.T) {
		p := start(t, newEnv(t).config(t))
		for _, s := range surface {
			resp, _ := p.admin(s.method, s.path, nil)
			assert.Equal(t, http.StatusNotFound, resp.StatusCode, "%s %s", s.method, s.path)
		}
	})

	t.Run("no_auth_401", func(t *testing.T) {
		p := start(t, newEnv(t).config(t, withAdmin))
		for _, s := range surface {
			resp, body := p.do(s.method, s.path, nil)
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "%s %s", s.method, s.path)
			pin(t, resp, map[string]string{"WWW-Authenticate": `Basic realm="groxpi admin"`})
			assert.Equal(t, "Unauthorized\n", string(body))
		}
		req, _ := http.NewRequest(http.MethodGet, p.url+"/admin", nil)
		req.SetBasicAuth(charUser, "wrong")
		resp, err := p.client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "wrong password")
	})

	t.Run("cross_site_rejected", func(t *testing.T) {
		p := start(t, newEnv(t).config(t, withAdmin))
		for _, site := range []string{"cross-site", "same-site"} {
			for _, s := range surface {
				resp, body := p.admin(s.method, s.path, nil, "Sec-Fetch-Site", site)
				assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s %s %s", site, s.method, s.path)
				assert.Equal(t, "Forbidden: cross-site request\n", string(body))
			}
			// Rejected before the credential check: no auth still yields 403.
			resp, _ := p.do(http.MethodGet, "/admin", nil, "Sec-Fetch-Site", site)
			assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		}
		for _, site := range []string{"same-origin", "none"} {
			resp, _ := p.admin(http.MethodGet, "/admin", nil, "Sec-Fetch-Site", site)
			assert.Equal(t, http.StatusOK, resp.StatusCode, site)
		}
	})

	t.Run("page_and_asset", func(t *testing.T) {
		p := start(t, newEnv(t).config(t, withAdmin))
		resp, body := p.admin(http.MethodGet, "/admin", nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": "text/html; charset=utf-8"})
		assert.Contains(t, string(body), "<html")

		resp, body = p.admin(http.MethodGet, "/admin/htmx.min.js", nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": "text/javascript; charset=utf-8", "Cache-Control": "public, max-age=86400"})
		assert.NotEmpty(t, body)
	})

	t.Run("listing", func(t *testing.T) {
		e := newEnv(t)
		p := start(t, e.config(t, withAdmin))
		resp, body := p.admin(http.MethodGet, "/admin/rows", nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": "text/html; charset=utf-8"})
		assert.NotContains(t, string(body), demoFile)

		_, _ = p.get("/simple/demo/" + demoFile)
		for _, path := range []string{"/admin/rows", "/admin"} {
			resp, body = p.admin(http.MethodGet, path, nil)
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Contains(t, string(body), "demo", path)
			assert.Contains(t, string(body), demoFile, path)
		}
	})

	t.Run("evict", func(t *testing.T) {
		e := newEnv(t)
		p := start(t, e.config(t, withAdmin))
		_, _ = p.get("/simple/demo/" + demoFile)
		if e.s3 != nil {
			// Evict only after the async L2 copy landed, or it could resurrect.
			require.Eventually(t, func() bool { return e.s3.has("packages/demo/" + demoFile) },
				5*time.Second, 5*time.Millisecond)
		}

		resp, body := p.admin(http.MethodDelete, "/cache/Demo", nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": "application/json; charset=utf-8"})
		assert.JSONEq(t, `{"status":"success","data":null}`, string(body))

		_, body = p.admin(http.MethodGet, "/admin/rows", nil)
		assert.NotContains(t, string(body), demoFile)

		resp, body = p.get("/simple/demo/" + demoFile)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, streamedMissHeaders("application/gzip", q(sum(demoWheel)), len(demoWheel)))
		assert.Equal(t, demoWheel, body)
		assert.Equal(t, 2, e.up.fileHits(demoFile), "evicted file is fetched upstream again")

		resp, body = p.admin(http.MethodDelete, "/cache/list", nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.JSONEq(t, `{"status":"success","data":null}`, string(body))
	})

	t.Run("prefetch", func(t *testing.T) {
		e := newEnv(t)
		p := start(t, e.config(t, withAdmin))
		form := func(v string) io.Reader { return strings.NewReader(url.Values{"package": {v}}.Encode()) }
		ct := []string{"Content-Type", "application/x-www-form-urlencoded"}

		resp, body := p.admin(http.MethodPost, "/admin/prefetch", form(""), ct...)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "Package name required\n", string(body))

		resp, body = p.admin(http.MethodPost, "/admin/prefetch", form("Demo"), ct...)
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": "text/plain; charset=utf-8"})
		assert.Equal(t, "Prefetching demo", string(body))

		require.Eventually(t, func() bool {
			_, rows := p.admin(http.MethodGet, "/admin/rows", nil)
			return strings.Contains(string(rows), demoFile)
		}, 5*time.Second, 10*time.Millisecond, "prefetched file appears in the listing")

		resp, body = p.get("/simple/demo/" + demoFile)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, cachedHitHeaders("application/gzip", len(demoWheel)))
		assert.Equal(t, demoWheel, body)
		assert.Equal(t, 1, e.up.fileHits(demoFile), "prefetch warmed the cache")
	})
}

func characterizeHomeHealth(t *testing.T, e *env, mode string) {
	t.Run("home", func(t *testing.T) {
		p := start(t, e.config(t))
		resp, body := p.get("/")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": "text/html; charset=utf-8"})
		s := string(body)
		assert.Contains(t, s, "<title>groxpi - PyPI Cache</title>")
		assert.Contains(t, s, "<li>Index URL: "+e.up.URL+"/</li>")
		assert.Contains(t, s, "<li>Cache Size: 1024 MB</li>")
		assert.Contains(t, s, "<li>Index TTL: 1m0s</li>")
		assert.Contains(t, s, `<a href="/health">Health Check</a>`)
		assert.NotContains(t, s, `href="/admin"`)

		p = start(t, e.config(t, withAdmin))
		_, body = p.get("/")
		assert.Contains(t, string(body), `<a href="/admin">Cache admin</a>`)

		resp, _ = p.get("/nope")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("health", func(t *testing.T) {
		cfg := e.config(t, func(c *config.Config) {
			c.IndexURL = strings.Replace(e.up.URL, "http://", "http://user:pw@", 1) + "/"
			c.ExtraIndexURLs = []string{"http://u:p@extra.invalid/simple/"}
		})
		p := start(t, cfg)
		resp, body := p.get("/health")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		pin(t, resp, map[string]string{"Content-Type": "application/json; charset=utf-8"})
		var got struct {
			Status    string         `json:"status"`
			Timestamp int64          `json:"timestamp"`
			Data      map[string]any `json:"data"`
		}
		require.NoError(t, json.Unmarshal(body, &got))
		assert.Equal(t, "success", got.Status)
		assert.NotZero(t, got.Timestamp)
		assert.Equal(t, map[string]any{
			"cache_dir":         cfg.CacheDir,
			"index_url":         strings.Replace(e.up.URL, "http://", "http://redacted@", 1) + "/",
			"extra_index_urls":  []any{"http://redacted@extra.invalid/simple/"},
			"cache_size":        float64(1 << 30),
			"index_ttl_seconds": float64(60),
			"storage_type":      mode,
		}, got.Data)
		assert.NotContains(t, string(body), "pw")
	})
}
