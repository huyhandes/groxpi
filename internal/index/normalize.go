package index

import (
	"regexp"
	"strings"
)

// nameSeparatorRun matches a run of the separators PEP 503 treats as equivalent.
var nameSeparatorRun = regexp.MustCompile(`[-_.]+`)

// NormalizeName applies the PEP 503 normalisation rule: any run of `-`, `_` or
// `.` collapses to a single `-`, and the name is lowercased. Resolve,
// Invalidate and storage.Key apply it themselves, so callers need not.
func NormalizeName(name string) string {
	return strings.ToLower(nameSeparatorRun.ReplaceAllString(name, "-"))
}
