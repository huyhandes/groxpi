package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/download"
)

// TestCredentialsNeverLeakFromDownloadFailures is the regression test for the
// credential leak on the *download* paths. It does not mock the leak: the index
// is served as PEP 503 HTML with a relative href, so the file URL is produced by
// url.ResolveReference against the credentialed index base and really does carry
// the password. The upstream then answers 503, which puts that URL inside the
// error string itself ("HTTP 503 from <url>") — which is why redaction has to
// happen on the rendered error text and not merely on a URL field beside it.
//
// Two sinks are asserted: the log stream, and the admin page's recent-errors
// area, which renders a prefetch failure's message through {{.Message}}.
func TestCredentialsNeverLeakFromDownloadFailures(t *testing.T) {
	const (
		secret   = "sup3rs3cr3tpassw0rd"
		pkg      = "leaky"
		fileName = "leaky-1.0-py3-none-any.whl"
	)

	// One upstream acting as both index and file host: the package page lists the
	// file with a *relative* href, and any request for the file itself fails.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		// The anchor is on its own line and its href is relative: that is what makes
		// the resolved file URL inherit the index base's user-info.
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body>\n" +
			`<a href="` + fileName + `">` + fileName + "</a>\n" +
			"</body></html>\n"))
	}))
	t.Cleanup(upstream.Close)

	logs := &lockedBuffer{}
	restore := captureLogs(t, logs)
	defer restore()

	cfg := &config.Config{
		IndexURL:        credentialedURL(t, upstream.URL, "indexuser:"+secret),
		CacheDir:        t.TempDir(),
		IndexTTL:        time.Hour,
		IndexCacheSize:  1 << 20,
		DownloadTimeout: time.Second,
		LogLevel:        "DEBUG",
		AdminUsername:   "operator",
		AdminPassword:   "adminpw",
	}
	srv := New(cfg)
	t.Cleanup(func() { _ = srv.Close() })

	// Sanity: the resolved file URL really does carry the credential, so the
	// assertions below are about redaction and not about an absent secret.
	plan, err := srv.downloads.Plan(t.Context(), pkg, fileName)
	require.NoError(t, err)
	require.Equal(t, download.ActionStreamAndCache, plan.Action)
	require.Contains(t, plan.URL, secret, "the leak mechanism itself regressed: the file URL no longer carries user-info")

	// 1. Download failure on the client path.
	_, status := getBody(t, srv, "/simple/"+pkg+"/"+fileName)
	require.Equal(t, http.StatusFound, status, "a failed download falls back to a redirect")

	// 2. Prefetch failure, which both logs and records onto the admin page.
	form := strings.NewReader("package=" + pkg)
	req := httptest.NewRequest(http.MethodPost, "/admin/prefetch", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(cfg.AdminUsername, cfg.AdminPassword)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code)

	// The prefetch is detached: poll the page until its failure has landed.
	var body string
	require.Eventually(t, func() bool {
		rows := httptest.NewRequest(http.MethodGet, "/admin/rows", nil)
		rows.SetBasicAuth(cfg.AdminUsername, cfg.AdminPassword)
		rowsRec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rowsRec, rows)
		body = rowsRec.Body.String()
		return rowsRec.Code == http.StatusOK && strings.Contains(body, fileName)
	}, 5*time.Second, 10*time.Millisecond, "the prefetch failure must have reached the page for this assertion to mean anything")

	assert.NotContains(t, body, secret, "the admin page must not render index credentials")
	assert.NotContains(t, logs.String(), secret, "credentials must appear nowhere in log output")
}
