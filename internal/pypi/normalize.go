package pypi

import (
	"regexp"
	"strings"
)

// nameSeparatorRun matches a run of the separators PEP 503 treats as equivalent.
var nameSeparatorRun = regexp.MustCompile(`[-_.]+`)

// NormalizeName applies the PEP 503 normalisation rule: any run of `-`, `_` or
// `.` collapses to a single `-`, and the name is lowercased. It must be applied
// wherever a package name becomes a cache key or an upstream path segment.
func NormalizeName(name string) string {
	return strings.ToLower(nameSeparatorRun.ReplaceAllString(name, "-"))
}
