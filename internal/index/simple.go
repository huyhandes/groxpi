package index

import (
	"bytes"
	"errors"
	"html"
	"io"
	"log/slog"
	"net/url"
	"strings"

	xhtml "golang.org/x/net/html"

	"github.com/huyhandes/groxpi/internal/config"
)

// parseSimpleHTML reads a PEP 503 simple page into its files. It tokenizes the
// page as HTML, so minified, uppercase, single- or unquoted and multi-line
// anchors all count, and a commented-out anchor does not. base is the URL the
// page was served from: hrefs resolve against it so FileInfo.URL is usable on
// its own. A nil base leaves hrefs verbatim. A page with no anchors is a valid,
// empty package.
func parseSimpleHTML(r io.Reader, base *url.URL) ([]FileInfo, error) {
	page, err := readCapped(r, maxPackageIndexBytes)
	if err != nil {
		return nil, err
	}

	files := make([]FileInfo, 0, 50)
	var (
		attrs map[string]string // the open anchor's attributes, nil when none is open
		text  strings.Builder
	)
	closeAnchor := func() {
		if attrs != nil {
			if file, ok := simpleFile(attrs, text.String(), base); ok {
				files = append(files, file)
			}
		}
		attrs = nil
		text.Reset()
	}

	z := xhtml.NewTokenizer(bytes.NewReader(page))
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			if err := z.Err(); !errors.Is(err, io.EOF) {
				return nil, err
			}
			closeAnchor()
			return files, nil
		case xhtml.StartTagToken:
			name, hasAttr := z.TagName()
			if string(name) != "a" {
				continue
			}
			closeAnchor()
			attrs = map[string]string{}
			for hasAttr {
				var key, val []byte
				key, val, hasAttr = z.TagAttr()
				if _, seen := attrs[string(key)]; !seen {
					attrs[string(key)] = string(val)
				}
			}
		case xhtml.EndTagToken:
			if name, _ := z.TagName(); string(name) == "a" {
				closeAnchor()
			}
		case xhtml.TextToken:
			if attrs != nil {
				text.Write(z.Text())
			}
		}
	}
}

// simpleFile builds one file from an anchor's (already unescaped) attributes
// and text. An anchor without an href, or with one that does not parse, is not
// a file.
func simpleFile(attrs map[string]string, text string, base *url.URL) (FileInfo, bool) {
	href, ok := attrs["href"]
	if !ok {
		return FileInfo{}, false
	}
	fileURL := href
	if base != nil {
		ref, err := url.Parse(href)
		if err != nil {
			slog.Warn("Skipping package file with unparseable href",
				"error", err,
				"href", config.RedactURL(href),
				"base_url", config.RedactURL(base.String()))
			return FileInfo{}, false
		}
		// ResolveReference handles absolute, protocol-relative, root-relative
		// and path-relative hrefs, and carries the #sha256=... fragment through.
		fileURL = base.ResolveReference(ref).String()
	}

	// PEP 503 carries the digest in the href fragment as #sha256=<hex>; lift
	// it into the same hash map shape the JSON index produces so downstream
	// code needs no format branch.
	var hashes map[string]string
	if _, fragment, found := strings.Cut(href, "#"); found {
		if sum, ok := strings.CutPrefix(fragment, "sha256="); ok && sum != "" {
			hashes = map[string]string{"sha256": sum}
		}
	}

	// PEP 592: the attribute's presence means yanked, its value is the reason.
	var yanked any
	if reason, ok := attrs["data-yanked"]; ok {
		if reason == "" {
			yanked = true
		} else {
			yanked = reason
		}
	}

	return FileInfo{
		Name:           strings.TrimSpace(text),
		URL:            fileURL,
		Hashes:         hashes,
		RequiresPython: attrs["data-requires-python"],
		Yanked:         yanked,
		CoreMetadata:   htmlMetadataAttr(attrs),
	}, true
}

// htmlMetadataAttr lifts a PEP 658 data-core-metadata (or the older
// data-dist-info-metadata) attribute into the PEP 691 shape: "true" becomes
// true, "sha256=<hex>" becomes {"sha256": "<hex>"}, absent stays nil.
func htmlMetadataAttr(attrs map[string]string) any {
	for _, attr := range []string{"data-core-metadata", "data-dist-info-metadata"} {
		value, ok := attrs[attr]
		if !ok {
			continue
		}
		if algo, sum, found := strings.Cut(value, "="); found && sum != "" {
			return map[string]any{algo: sum}
		}
		return true
	}
	return nil
}

// RenderSimpleHTML renders a package's PEP 503 page from its file list, every
// href pointing back at this proxy. Every interpolated value is HTML-escaped.
// HTML is the uncommon content type, so it is produced on demand rather than
// stored as a third copy per package.
func RenderSimpleHTML(pkg string, files []FileInfo) []byte {
	var sb strings.Builder
	sb.Grow(1024 + len(files)*200)

	sb.WriteString(`<!DOCTYPE html>
<html>
<head><title>Links for `)
	sb.WriteString(html.EscapeString(pkg))
	sb.WriteString(`</title></head>
<body>
	<h1>Links for `)
	sb.WriteString(html.EscapeString(pkg))
	sb.WriteString(`</h1>
`)

	for _, file := range files {
		sb.WriteString(`	<a href="`)
		sb.WriteString(html.EscapeString(ProxyFileURL(pkg, file.Name)))
		sb.WriteString(`"`)

		if file.RequiresPython != "" {
			sb.WriteString(` data-requires-python="`)
			sb.WriteString(html.EscapeString(file.RequiresPython))
			sb.WriteString(`"`)
		}
		if file.IsYanked() {
			sb.WriteString(` data-yanked="`)
			if reason := file.GetYankedReason(); reason != "" {
				sb.WriteString(html.EscapeString(reason))
			}
			sb.WriteString(`"`)
		}
		if hashes, ok := file.Metadata(); ok {
			value := "true"
			if sum := hashes["sha256"]; sum != "" {
				value = "sha256=" + html.EscapeString(sum)
			}
			sb.WriteString(` data-core-metadata="` + value + `" data-dist-info-metadata="` + value + `"`)
		}

		sb.WriteString(`>`)
		sb.WriteString(html.EscapeString(file.Name))
		sb.WriteString(`</a><br>
`)
	}

	sb.WriteString(`</body>
</html>`)
	return []byte(sb.String())
}
