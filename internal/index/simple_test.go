package index

import (
	"bytes"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

// TestParseSimpleHTML_AnyFormatting: an upstream page is HTML, not a
// line-per-anchor format. Each of these yields its real files, so an extra index
// cannot "succeed" with zero files and hide the primary.
func TestParseSimpleHTML_AnyFormatting(t *testing.T) {
	const base = "https://org/simple/foo/"
	testCases := []struct {
		name     string
		html     string
		expected []FileInfo
	}{
		{
			name: "minified, every anchor on one line",
			html: `<!DOCTYPE html><html><body><a href="a-1.0.whl">a-1.0.whl</a><br/><a href="a-1.0.tar.gz">a-1.0.tar.gz</a></body></html>`,
			expected: []FileInfo{
				{Name: "a-1.0.whl", URL: base + "a-1.0.whl"},
				{Name: "a-1.0.tar.gz", URL: base + "a-1.0.tar.gz"},
			},
		},
		{
			name:     "uppercase tag and attribute",
			html:     `<A HREF="a-1.0.whl" DATA-REQUIRES-PYTHON="&gt;=3.8">a-1.0.whl</A>`,
			expected: []FileInfo{{Name: "a-1.0.whl", URL: base + "a-1.0.whl", RequiresPython: ">=3.8"}},
		},
		{
			name:     "single-quoted attributes",
			html:     `<a href='a-1.0.whl' data-yanked='bad build'>a-1.0.whl</a>`,
			expected: []FileInfo{{Name: "a-1.0.whl", URL: base + "a-1.0.whl", Yanked: "bad build"}},
		},
		{
			name:     "unquoted attribute",
			html:     `<a href=a-1.0.whl>a-1.0.whl</a>`,
			expected: []FileInfo{{Name: "a-1.0.whl", URL: base + "a-1.0.whl"}},
		},
		{
			name: "anchor spread over several lines",
			html: "<a\n  href=\"a-1.0.whl\"\n  data-requires-python=\"&gt;=3.9\"\n>\n  a-1.0.whl\n</a>",
			expected: []FileInfo{
				{Name: "a-1.0.whl", URL: base + "a-1.0.whl", RequiresPython: ">=3.9"},
			},
		},
		{
			name:     "commented-out anchor is not a file",
			html:     "<!-- <a href=\"old-0.1.whl\">old-0.1.whl</a> -->\n<a href=\"a-1.0.whl\">a-1.0.whl</a>",
			expected: []FileInfo{{Name: "a-1.0.whl", URL: base + "a-1.0.whl"}},
		},
		{
			name:     "bare data-yanked means yanked",
			html:     `<a href="a-1.0.whl" data-yanked>a-1.0.whl</a>`,
			expected: []FileInfo{{Name: "a-1.0.whl", URL: base + "a-1.0.whl", Yanked: true}},
		},
		{
			name:     "anchor without href is skipped",
			html:     `<a name="top">top</a><a href="a-1.0.whl">a-1.0.whl</a>`,
			expected: []FileInfo{{Name: "a-1.0.whl", URL: base + "a-1.0.whl"}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			files, err := parseSimpleHTML(strings.NewReader(tc.html), mustURL(t, base))
			require.NoError(t, err)
			assert.Equal(t, tc.expected, files)
		})
	}
}

// TestParseSimpleHTML_ReadsThroughCap: an endless HTML body fails rather than
// exhausting memory.
func TestParseSimpleHTML_ReadsThroughCap(t *testing.T) {
	saved := maxPackageIndexBytes
	maxPackageIndexBytes = 64
	t.Cleanup(func() { maxPackageIndexBytes = saved })

	page := strings.Repeat(`<a href="a.whl">a.whl</a>`, 10)
	_, err := parseSimpleHTML(strings.NewReader(page), mustURL(t, "https://org/simple/foo/"))
	require.Error(t, err)
}

// TestSimpleHTML_RoundTrip: what the renderer writes, the parser reads back,
// hostile values included. Hashes are not carried (the rendered href has no
// #sha256= fragment; see docs/backlog.md), and the URL becomes the proxy's own.
func TestSimpleHTML_RoundTrip(t *testing.T) {
	const hostile = `"><script>alert('x')</script>&amp;<!--`
	const sum = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	testCases := []struct {
		name  string
		pkg   string
		files []FileInfo
	}{
		{
			name: "plain files and every attribute",
			pkg:  "demo",
			files: []FileInfo{
				{Name: "demo-1.0.tar.gz"},
				{Name: "demo-1.0-py3-none-any.whl", RequiresPython: ">=3.8,<4", CoreMetadata: map[string]any{"sha256": sum}},
				{Name: "demo-0.9.whl", Yanked: true, CoreMetadata: true},
				{Name: "demo-0.8.whl", Yanked: "broken & unusable"},
			},
		},
		{
			name: "hostile values",
			pkg:  "demo" + hostile,
			files: []FileInfo{
				{Name: "demo" + hostile + ".whl", RequiresPython: ">=3.8" + hostile, Yanked: hostile},
			},
		},
		{
			name:  "no files",
			pkg:   "demo",
			files: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			page := RenderSimpleHTML(tc.pkg, tc.files)
			got, err := parseSimpleHTML(bytes.NewReader(page), mustURL(t, "https://proxy/simple/demo/"))
			require.NoError(t, err)
			require.Len(t, got, len(tc.files))
			for i, want := range tc.files {
				assert.Equal(t, want.Name, got[i].Name)
				assert.Equal(t, want.RequiresPython, got[i].RequiresPython)
				assert.Equal(t, want.Yanked, got[i].Yanked)
				assert.Equal(t, want.CoreMetadata, got[i].CoreMetadata)
			}
		})
	}
}

// TestRenderSimpleHTML_Pin pins the rendered bytes, so moving the renderer into
// index changes nothing a client sees.
func TestRenderSimpleHTML_Pin(t *testing.T) {
	got := RenderSimpleHTML("demo", []FileInfo{
		{Name: "demo-1.0.whl", RequiresPython: ">=3.8", Yanked: "bad", CoreMetadata: map[string]any{"sha256": "abc"}},
	})
	want := "<!DOCTYPE html>\n<html>\n<head><title>Links for demo</title></head>\n<body>\n\t<h1>Links for demo</h1>\n" +
		"\t<a href=\"/simple/demo/demo-1.0.whl\" data-requires-python=\"&gt;=3.8\" data-yanked=\"bad\"" +
		" data-core-metadata=\"sha256=abc\" data-dist-info-metadata=\"sha256=abc\">demo-1.0.whl</a><br>\n" +
		"</body>\n</html>"
	assert.Equal(t, want, string(got))
}
