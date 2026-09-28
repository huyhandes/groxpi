package index

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
