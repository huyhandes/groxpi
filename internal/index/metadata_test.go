package index

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
)

// TestHTMLMetadataAttr covers PEP 658's attribute in both spellings and both
// value forms.
func TestHTMLMetadataAttr(t *testing.T) {
	assert.Nil(t, htmlMetadataAttr(`<a href="x">x</a>`))
	assert.Equal(t, true, htmlMetadataAttr(`<a href="x" data-core-metadata="true">x</a>`))
	assert.Equal(t, true, htmlMetadataAttr(`<a href="x" data-dist-info-metadata="true">x</a>`))
	assert.Equal(t, map[string]any{"sha256": "abc"}, htmlMetadataAttr(`<a href="x" data-core-metadata="sha256=abc">x</a>`))
}

func TestFileInfo_Metadata(t *testing.T) {
	var none FileInfo
	_, ok := none.Metadata()
	assert.False(t, ok)

	bare := FileInfo{DistInfoMetadata: true}
	hashes, ok := bare.Metadata()
	assert.True(t, ok)
	assert.Empty(t, hashes)

	// The JSON decoder hands back map[string]any; CoreMetadata wins over the old name.
	hashed := FileInfo{CoreMetadata: map[string]any{"sha256": "abc"}, DistInfoMetadata: false}
	hashes, ok = hashed.Metadata()
	assert.True(t, ok)
	assert.Equal(t, map[string]string{"sha256": "abc"}, hashes)
}

// TestMetadata_PassesThroughJSONAndHTML pins the PEP 714 rule: whatever the
// upstream says about metadata is re-emitted under both keys in JSON and both
// attributes in HTML, pointing at this proxy.
func TestMetadata_PassesThroughJSONAndHTML(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", JSONContentType)
		_, _ = fmt.Fprint(w, `{"name":"meta","meta":{"api-version":"1.1"},"files":[
			{"filename":"meta-1.0-py3-none-any.whl","url":"https://files/meta-1.0-py3-none-any.whl","core-metadata":{"sha256":"abc"}},
			{"filename":"meta-1.0.tar.gz","url":"https://files/meta-1.0.tar.gz","dist-info-metadata":true},
			{"filename":"meta-0.9.tar.gz","url":"https://files/meta-0.9.tar.gz"}]}`)
	}))
	t.Cleanup(up.Close)
	mux, _ := newTestMux(t, &config.Config{IndexURL: up.URL, IndexTTL: time.Minute, IndexCacheSize: 1 << 20})

	var body struct {
		Files []map[string]any `json:"files"`
	}
	require.NoError(t, json.Unmarshal(getIndex(t, mux, "meta"), &body))
	require.Len(t, body.Files, 3)
	assert.Equal(t, map[string]any{"sha256": "abc"}, body.Files[0]["core-metadata"])
	assert.Equal(t, map[string]any{"sha256": "abc"}, body.Files[0]["dist-info-metadata"])
	assert.Equal(t, true, body.Files[1]["core-metadata"])
	assert.Equal(t, true, body.Files[1]["dist-info-metadata"])
	_, has := body.Files[2]["core-metadata"]
	assert.False(t, has, "a file without metadata upstream must not claim one")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/simple/meta/", nil))
	page := w.Body.String()
	assert.Contains(t, page, `data-core-metadata="sha256=abc" data-dist-info-metadata="sha256=abc"`)
	assert.Contains(t, page, `data-core-metadata="true" data-dist-info-metadata="true"`)
	assert.Equal(t, 2, strings.Count(page, "data-core-metadata="))
}
