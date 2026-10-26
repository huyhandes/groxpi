package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
)

// TestVersion_OneConstant: the home page and every upstream User-Agent (package
// index, file download, root index) carry the same version constant.
func TestVersion_OneConstant(t *testing.T) {
	require.NotEmpty(t, config.Version)
	pkg, file := "versioned", "versioned-1.0.0.tar.gz"

	var mu sync.Mutex
	agents := map[string]string{}
	record := func(kind string, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		agents[kind] = r.Header.Get("User-Agent")
	}
	up := newFakeUpstream(t, upstreamSpec{
		pkg: pkg, file: file,
		serveFile: func(w http.ResponseWriter, r *http.Request) {
			record("file", r)
			_, _ = w.Write(testPayload(64))
		},
	})
	// Record the package-index and root-index requests in front of the upstream.
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			record("root", r)
		case "/" + pkg + "/":
			record("index", r)
		}
		up.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	router, _ := newCorrectnessServer(t, front.URL, 5*time.Second)

	home := httptest.NewRecorder()
	router.ServeHTTP(home, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Contains(t, home.Body.String(), "Version: "+config.Version+"<")

	_ = readBody(t, getFile(router, pkg, file))
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/simple/", nil))

	mu.Lock()
	defer mu.Unlock()
	want := "groxpi/" + config.Version
	for _, kind := range []string{"index", "file", "root"} {
		assert.Equal(t, want, agents[kind], "%s request User-Agent", kind)
	}
}
