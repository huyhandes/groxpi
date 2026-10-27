package server

// The one fake PyPI upstream every server test uses. It is exported (from a
// _test file, so only test binaries see it) because the black-box
// characterization suite in package server_test shares it.

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Root listing the fake serves at "/", in the two content types.
const (
	FakeRootHTML = `<!DOCTYPE html><html><body><a href="/simple/demo/">demo</a></body></html>`
	FakeRootJSON = `{"meta":{"api-version":"1.0"},"projects":[{"name":"demo"}]}`
)

// FakeFile is one file the fake serves under /files/<name>.
type FakeFile struct {
	Body    []byte
	SHA256  string           // advertised in the index; "" = omitted
	Size    int64            // advertised in the index; 0 = omitted
	MetaSHA string           // advertised core-metadata sha256; "" = not advertised
	Gated   bool             // first half, flush, wait for Release, rest
	Handler http.HandlerFunc // if set, serves the request instead of Body
}

// FakePyPI is a PyPI-shaped upstream: a root listing, per-package indexes (JSON
// with relative file URLs when asked for, HTML with absolute ones otherwise),
// files, and request counters for each.
type FakePyPI struct {
	*httptest.Server

	release chan struct{}
	once    sync.Once

	mu        sync.Mutex
	pkgs      map[string][]string // package -> listed filenames
	files     map[string]FakeFile // filename -> file (unlisted siblings included)
	failing   map[string]bool     // package ("" = root) -> answers 500
	delay     time.Duration
	rootHits  int
	indexHits map[string]int
	fileHits  map[string]int
}

func NewFakePyPI(t testing.TB) *FakePyPI {
	t.Helper()
	u := &FakePyPI{
		release:   make(chan struct{}),
		pkgs:      map[string][]string{},
		files:     map[string]FakeFile{},
		failing:   map[string]bool{},
		indexHits: map[string]int{},
		fileHits:  map[string]int{},
	}
	u.Server = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.Close)
	t.Cleanup(u.Release) // runs first: never leave a gated handler blocking Close
	return u
}

// Add lists name in pkg's index and serves it.
func (u *FakePyPI) Add(pkg, name string, f FakeFile) *FakePyPI {
	u.mu.Lock()
	u.pkgs[pkg] = append(u.pkgs[pkg], name)
	u.mu.Unlock()
	return u.AddFile(name, f)
}

// AddFile serves name without listing it (a PEP 658 ".metadata" sibling).
func (u *FakePyPI) AddFile(name string, f FakeFile) *FakePyPI {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.files[name] = f
	return u
}

// Fail makes pkg's index answer 500; Fail("") fails the root listing.
func (u *FakePyPI) Fail(pkg string) *FakePyPI {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.failing[pkg] = true
	return u
}

// SetDelay holds every response (root, index, file) for d before it starts.
func (u *FakePyPI) SetDelay(d time.Duration) *FakePyPI {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.delay = d
	return u
}

// Release lets every gated file finish.
func (u *FakePyPI) Release() { u.once.Do(func() { close(u.release) }) }

func (u *FakePyPI) FileHits(name string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.fileHits[name]
}

func (u *FakePyPI) IndexHits(pkg string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.indexHits[pkg]
}

func (u *FakePyPI) RootHits() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.rootHits
}

func (u *FakePyPI) serve(w http.ResponseWriter, r *http.Request) {
	switch path := r.URL.Path; {
	case path == "/":
		u.serveRoot(w, r)
	case strings.HasPrefix(path, "/files/"):
		u.serveFile(w, r, strings.TrimPrefix(path, "/files/"))
	default:
		u.serveIndex(w, r, strings.Trim(path, "/"))
	}
}

func (u *FakePyPI) serveRoot(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.rootHits++
	delay, failing := u.delay, u.failing[""]
	u.mu.Unlock()
	time.Sleep(delay)

	if failing {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}

	body, ctype := FakeRootHTML, "text/html"
	if strings.Contains(r.Header.Get("Accept"), "json") {
		body, ctype = FakeRootJSON, "application/vnd.pypi.simple.v1+json"
	}
	w.Header().Set("Content-Type", ctype)
	if r.Header.Get("Accept-Encoding") == "gzip" {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(Gzip(body))
		return
	}
	_, _ = io.WriteString(w, body)
}

func (u *FakePyPI) serveIndex(w http.ResponseWriter, r *http.Request, pkg string) {
	u.mu.Lock()
	u.indexHits[pkg]++
	names, ok := u.pkgs[pkg]
	failing := u.failing[pkg]
	delay := u.delay
	listed := make([]FakeFile, len(names))
	for i, n := range names {
		listed[i] = u.files[n]
	}
	u.mu.Unlock()
	time.Sleep(delay)

	if failing {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}

	if !strings.Contains(r.Header.Get("Accept"), "application/vnd.pypi.simple.v1+json") {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>")
		for _, n := range names {
			_, _ = fmt.Fprintf(w, `<a href=%q>%s</a>`, u.URL+"/files/"+url.PathEscape(n), n)
		}
		_, _ = io.WriteString(w, "</body></html>")
		return
	}

	// JSON file URLs are relative (PEP 691 allows it; the HTML page above uses
	// absolute ones), so every download through the JSON index proves they are
	// resolved against the page URL.
	files := make([]map[string]any, 0, len(names))
	for i, n := range names {
		f := listed[i]
		e := map[string]any{"filename": n, "url": "../files/" + url.PathEscape(n)}
		if f.SHA256 != "" {
			e["hashes"] = map[string]string{"sha256": f.SHA256}
		}
		if f.Size > 0 {
			e["size"] = f.Size
		}
		if f.MetaSHA != "" {
			e["core-metadata"] = map[string]string{"sha256": f.MetaSHA}
		}
		files = append(files, e)
	}
	w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
	_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"api-version": "1.0"}, "name": pkg, "files": files})
}

func (u *FakePyPI) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	u.mu.Lock()
	u.fileHits[name]++
	f, ok := u.files[name]
	delay := u.delay
	u.mu.Unlock()
	time.Sleep(delay)

	switch {
	case !ok:
		http.NotFound(w, r)
		return
	case f.Handler != nil:
		f.Handler(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(f.Body)))
	if !f.Gated {
		_, _ = w.Write(f.Body)
		return
	}
	half := len(f.Body) / 2
	_, _ = w.Write(f.Body[:half])
	w.(http.Flusher).Flush()
	<-u.release
	_, _ = w.Write(f.Body[half:])
}

// verified is a file whose index entry advertises its true sha256 and size.
func verified(body []byte) FakeFile {
	return FakeFile{Body: body, SHA256: Sum(body), Size: int64(len(body))}
}

// newPyPIWith is the common single-file fake: pkg lists file, served verified.
func newPyPIWith(t *testing.T, pkg, file string, body []byte) *FakePyPI {
	t.Helper()
	return NewFakePyPI(t).Add(pkg, file, verified(body))
}

// Sum is the hex sha256 of b.
func Sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// Gzip compresses s.
func Gzip(s string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(s))
	_ = zw.Close()
	return buf.Bytes()
}
