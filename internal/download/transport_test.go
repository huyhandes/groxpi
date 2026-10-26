package download

import (
	"context"
	"crypto/tls"
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

// TestUpstreamTLSVerificationSetting pins that GROXPI_DISABLE_INDEX_SSL_VERIFICATION
// reaches every upstream call: the per-package index, the root proxy and the
// file download all go through the one transport index builds. Against a
// self-signed server, verification on fails with a certificate error and
// verification off succeeds.
func TestUpstreamTLSVerificationSetting(t *testing.T) {
	tlsUp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<a href="/p/">p</a>`)
		case "/p/":
			w.Header().Set("Content-Type", index.JSONContentType)
			_, _ = io.WriteString(w, `{"meta":{"api-version":"1.0"},"files":[{"filename":"p-1.0.tar.gz","url":"https://example.invalid/p-1.0.tar.gz"}]}`)
		default:
			_, _ = io.WriteString(w, "payload")
		}
	}))
	t.Cleanup(tlsUp.Close)

	// The download case lists a TLS file URL on a plain-HTTP index, so only the
	// download itself meets the self-signed certificate.
	plainIndex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/p/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", index.JSONContentType)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"meta":  map[string]string{"api-version": "1.0"},
			"files": []index.FileInfo{{Name: "p-1.0.tar.gz", URL: tlsUp.URL + "/files/p-1.0.tar.gz"}},
		})
	}))
	t.Cleanup(plainIndex.Close)

	calls := []struct {
		name     string
		indexURL string
		call     func(ctx context.Context, idx *index.Service, dl *Service) error
	}{
		{"index.Resolve", tlsUp.URL, func(ctx context.Context, idx *index.Service, _ *Service) error {
			_, err := idx.Resolve(ctx, "p")
			return err
		}},
		{"index.ProxyRoot", tlsUp.URL, func(ctx context.Context, idx *index.Service, _ *Service) error {
			_, err := idx.ProxyRoot(ctx, "", "")
			return err
		}},
		{"download.Fetch", plainIndex.URL, func(ctx context.Context, _ *index.Service, dl *Service) error {
			tgt, err := dl.Resolve(ctx, "p", "p-1.0.tar.gz")
			if err != nil {
				return err
			}
			f, err := dl.Fetch(ctx, tgt)
			if err != nil {
				return err
			}
			defer func() { _ = f.Body.Close() }()
			_, err = io.ReadAll(f.Body)
			return err
		}},
	}
	for _, c := range calls {
		for _, skip := range []bool{false, true} {
			name := c.name + "/verify"
			if skip {
				name = c.name + "/skip-verify"
			}
			t.Run(name, func(t *testing.T) {
				cfg := &config.Config{
					IndexURL:               c.indexURL,
					IndexTTL:               time.Minute,
					IndexCacheSize:         1 << 20,
					DownloadTimeout:        time.Minute,
					DisableSSLVerification: skip,
				}
				idx := index.New(cfg)
				t.Cleanup(idx.Close)
				err := c.call(t.Context(), idx, New(cfg, idx))
				if skip {
					require.NoError(t, err)
					return
				}
				var certErr *tls.CertificateVerificationError
				assert.True(t, errors.As(err, &certErr), "want a certificate verification error, got %v", err)
			})
		}
	}
}
