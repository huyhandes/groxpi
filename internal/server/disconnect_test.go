package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// disconnectingWriter stands in for net/http's response writer when the client
// has gone: the write fails and the server cancels the request context, which is
// what net/http does on a failed connection write.
type disconnectingWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *disconnectingWriter) Write([]byte) (int, error) {
	w.cancel()
	return 0, errors.New("write: broken pipe")
}

// errorRecords returns the messages of every ERROR record in a JSON log.
func errorRecords(t *testing.T, logs string) []string {
	t.Helper()
	var msgs []string
	sc := bufio.NewScanner(strings.NewReader(logs))
	for sc.Scan() {
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		require.NoError(t, json.Unmarshal(sc.Bytes(), &rec))
		if rec.Level == "ERROR" {
			msgs = append(msgs, rec.Msg)
		}
	}
	return msgs
}

// serveAborting runs one file request through the router and reports whether the
// handler aborted the response.
func serveAborting(router http.Handler, w http.ResponseWriter, r *http.Request) (aborted bool) {
	defer func() {
		if rec := recover(); rec != nil {
			if rec != http.ErrAbortHandler {
				panic(rec)
			}
			aborted = true
		}
	}()
	router.ServeHTTP(w, r)
	return false
}

func TestDownload_ClientDisconnectIsNotAnError(t *testing.T) {
	payload := testPayload(64 * 1024)
	pkg, file := "leaver", "leaver-1.0.0.tar.gz"
	up := NewFakePyPI(t).Add(pkg, file, FakeFile{
		Handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) },
	})
	router, cacheDir := newCorrectnessServer(t, up.URL, 5*time.Second)

	logs := &lockedBuffer{}
	restore := captureLogs(t, logs)
	defer restore()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/index/"+pkg+"/"+file, nil)
	w := &disconnectingWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}

	assert.True(t, serveAborting(router, w, req), "a disconnected client's response is still aborted")
	waitCached(t, cacheDir, pkg, file)
	assert.Empty(t, errorRecords(t, logs.String()), "a client leaving mid-body is not an ERROR")
	assert.Contains(t, logs.String(), "Client disconnected mid-download")
}

func TestDownload_UpstreamFailureMidBodyIsAnError(t *testing.T) {
	payload := testPayload(512 * 1024)
	pkg, file := "breaker", "breaker-1.0.0.tar.gz"
	up := NewFakePyPI(t).Add(pkg, file, FakeFile{Size: int64(len(payload)),
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload[:4096])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		},
	})
	router, _ := newCorrectnessServer(t, up.URL, 5*time.Second)

	logs := &lockedBuffer{}
	restore := captureLogs(t, logs)
	defer restore()

	req := httptest.NewRequest(http.MethodGet, "/index/"+pkg+"/"+file, nil)
	assert.True(t, serveAborting(router, httptest.NewRecorder(), req))
	assert.Contains(t, errorRecords(t, logs.String()), "Download failed mid-stream, aborting response")
}
