package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An upstream controls every value interpolated into the PEP 503 page: a
// hostile yanked reason, requires-python or file name must come out escaped.
func TestPackageHTML_EscapesUpstreamValues(t *testing.T) {
	const hostile = `"><script>alert('x')</script>&`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"meta": map[string]any{"api-version": "1.0"},
			"name": "evil",
			"files": []any{
				map[string]any{
					"filename": "evil-1.0.tar.gz", "url": "https://files.test/evil-1.0.tar.gz",
					"hashes":          map[string]any{},
					"requires-python": ">=3.8" + hostile,
					"yanked":          hostile,
				},
				map[string]any{
					"filename": "evil-<b>&1.0.tar.gz", "url": "https://files.test/evil-2.tar.gz",
					"hashes":          map[string]any{},
					"requires-python": ">=3.8",
				},
			},
		})
	}))
	t.Cleanup(upstream.Close)
	srv := newMultiIndexServer(t, nil, &fakeUpstreamIndex{Server: upstream})

	body, status := getBody(t, srv, "/simple/evil/?format=text/html")
	require.Equal(t, http.StatusOK, status)

	const escaped = `&#34;&gt;&lt;script&gt;alert(&#39;x&#39;)&lt;/script&gt;&amp;`
	assert.NotContains(t, body, "<script>")
	assert.NotContains(t, body, "<b>")
	assert.Contains(t, body, `data-requires-python="&gt;=3.8`+escaped+`"`)
	assert.Contains(t, body, `data-yanked="`+escaped+`"`)
	assert.Contains(t, body, `<a href="/simple/evil/evil-%3Cb%3E&amp;1.0.tar.gz" data-requires-python="&gt;=3.8">evil-&lt;b&gt;&amp;1.0.tar.gz</a>`)
}
