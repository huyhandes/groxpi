// Package templates holds the administrative interface's HTML templates,
// compiled into the binary.
//
// The package lives beside the files because go:embed cannot reach outside the
// directory of the file that declares it. No JavaScript is vendored: the page
// loads Datastar from its CDN, and every control still works as a plain form or
// link when it cannot, so an air-gapped network gets a page without live updates.
package templates

import "embed"

// FS holds every administrative template.
//
//go:embed *.html
var FS embed.FS
