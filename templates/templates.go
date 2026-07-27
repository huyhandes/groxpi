// Package templates holds the administrative interface's assets: the HTML
// templates and the hypertext interaction library, compiled into the binary.
//
// The package lives beside the files because go:embed cannot reach outside the
// directory of the file that declares it. Embedding is what makes the page work
// in an air-gapped network: it issues no external asset requests, and building
// groxpi remains a single `go build` with no JavaScript toolchain.
package templates

import "embed"

// FS holds every administrative asset. htmx.min.js is vendored verbatim from
// the upstream release; it is not built, minified or transformed here.
//
//go:embed *.html htmx.min.js
var FS embed.FS
