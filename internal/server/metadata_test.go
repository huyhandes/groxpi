package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/index"
)

// TestMetadata_PassesThroughJSONAndHTML pins the PEP 714 rule: whatever the
// upstream says about metadata (under either key) is re-emitted as
// core-metadata only in JSON — PEP 714 says servers should not emit the old
// key in JSON — and under both attributes in HTML, where the legacy one MAY
// accompany data-core-metadata.
func TestMetadata_PassesThroughJSONAndHTML(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", index.JSONContentType)
		_, _ = fmt.Fprint(w, `{"name":"meta","meta":{"api-version":"1.1"},"files":[
			{"filename":"meta-1.0-py3-none-any.whl","url":"https://files/meta-1.0-py3-none-any.whl","core-metadata":{"sha256":"abc"}},
			{"filename":"meta-1.0.tar.gz","url":"https://files/meta-1.0.tar.gz","dist-info-metadata":true},
			{"filename":"meta-0.9.tar.gz","url":"https://files/meta-0.9.tar.gz"}]}`)
	}))
	t.Cleanup(up.Close)
	mux := newRoutesServer(t, up.URL)

	var body struct {
		Files []map[string]any `json:"files"`
	}
	require.NoError(t, json.Unmarshal(getIndex(t, mux, "meta"), &body))
	require.Len(t, body.Files, 3)
	assert.Equal(t, map[string]any{"sha256": "abc"}, body.Files[0]["core-metadata"])
	assert.Equal(t, true, body.Files[1]["core-metadata"])
	for i, f := range body.Files {
		_, legacy := f["dist-info-metadata"]
		assert.False(t, legacy, "file %d: JSON must not carry the legacy dist-info-metadata key", i)
	}
	_, has := body.Files[2]["core-metadata"]
	assert.False(t, has, "a file without metadata upstream must not claim one")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/simple/meta/", nil))
	page := w.Body.String()
	assert.Contains(t, page, `data-core-metadata="sha256=abc" data-dist-info-metadata="sha256=abc"`)
	assert.Contains(t, page, `data-core-metadata="true" data-dist-info-metadata="true"`)
	assert.Equal(t, 2, strings.Count(page, "data-core-metadata="))
}

// TestPEP691_YankedAndHashes pins the PEP 691 file keys: "yanked" is true or
// the reason string (there is no "yanked-reason" key), and "hashes" is always
// present, as an empty object when upstream declared none.
func TestPEP691_YankedAndHashes(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", index.JSONContentType)
		_, _ = fmt.Fprint(w, `{"name":"y","meta":{"api-version":"1.1"},"files":[
			{"filename":"y-1.0.tar.gz","url":"https://files/y-1.0.tar.gz","hashes":{},"yanked":"broken build"},
			{"filename":"y-0.9.tar.gz","url":"https://files/y-0.9.tar.gz","yanked":true},
			{"filename":"y-0.8.tar.gz","url":"https://files/y-0.8.tar.gz","hashes":{"sha256":"abc"}}]}`)
	}))
	t.Cleanup(up.Close)
	mux := newRoutesServer(t, up.URL)

	var body struct {
		Files []map[string]any `json:"files"`
	}
	require.NoError(t, json.Unmarshal(getIndex(t, mux, "y"), &body))
	require.Len(t, body.Files, 3)
	assert.Equal(t, "broken build", body.Files[0]["yanked"])
	assert.Equal(t, true, body.Files[1]["yanked"])
	_, yanked := body.Files[2]["yanked"]
	assert.False(t, yanked)
	for i, f := range body.Files {
		_, reason := f["yanked-reason"]
		assert.False(t, reason, "file %d: yanked-reason is not a PEP 691 key", i)
	}
	assert.Equal(t, map[string]any{}, body.Files[0]["hashes"])
	assert.Equal(t, map[string]any{}, body.Files[1]["hashes"], "hashes MUST be present even with none upstream")
	assert.Equal(t, map[string]any{"sha256": "abc"}, body.Files[2]["hashes"])
}
