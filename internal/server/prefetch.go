package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	pep440 "github.com/aquasecurity/go-pep440-version"
	"github.com/gin-gonic/gin"
	"github.com/phuslu/log"

	"github.com/huyhandes/groxpi/internal/pypi"
)

// sdistExtensions are the source-distribution suffixes whose filenames end in
// the version. Longest first, so ".tar.gz" is not truncated to ".gz".
var sdistExtensions = []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.Z", ".tgz", ".tbz2", ".zip", ".tar"}

// handleAdminPrefetch accepts a prefetch and returns immediately. The download
// runs on a context detached from the request so a large package does not time
// out behind a reverse proxy; there is no job registry and no identifier,
// because the polling table already shows the files arriving.
func (s *Server) handleAdminPrefetch(c *gin.Context) {
	packageName := pypi.NormalizeName(strings.TrimSpace(c.PostForm("package")))
	if packageName == "" {
		c.String(http.StatusBadRequest, "Package name required")
		return
	}

	ctx := context.WithoutCancel(c.Request.Context())
	go s.prefetchPackage(ctx, packageName)

	c.String(http.StatusAccepted, "Prefetching %s", packageName)
}

// prefetchPackage resolves the package's index, picks its newest release and
// downloads that release's files. Failures are recorded for the page's
// recent-errors area as well as logged: an operator watching an empty table must
// not have to read the server log to learn why it is empty.
func (s *Server) prefetchPackage(ctx context.Context, packageName string) {
	entry, err := s.packageFiles.resolveIndex(ctx, packageName)
	if err != nil {
		if errors.Is(err, pypi.ErrNotFound) {
			s.adminErrors.record(packageName, "not found on any configured index")
		} else {
			s.adminErrors.record(packageName, "index lookup failed: "+err.Error())
		}
		log.Error().Err(err).Str("package", packageName).Msg("Prefetch could not resolve index")
		return
	}

	files := newestReleaseFiles(entry.Files)
	if len(files) == 0 {
		s.adminErrors.record(packageName, "no downloadable final release (all files yanked, pre-release or unparseable)")
		return
	}

	log.Info().Str("package", packageName).Int("files", len(files)).Msg("Prefetching newest release")

	for _, file := range files {
		plan, err := s.packageFiles.Plan(ctx, packageName, file.Name)
		if err != nil {
			s.adminErrors.record(packageName, file.Name+": "+err.Error())
			continue
		}

		switch plan.Action {
		case ActionFromStorage:
			// Already cached; nothing to warm.
		case ActionStreamAndCache:
			// io.Discard: the point is the cache write the downloader performs on the
			// way through, not the bytes. A failure aborts the cache write too, so a
			// verification error leaves nothing partial behind.
			if _, _, err := s.packageFiles.Fetch(ctx, plan, io.Discard); err != nil {
				s.adminErrors.record(packageName, file.Name+": "+err.Error())
				log.Error().Err(err).Str("package", packageName).Str("file", file.Name).Msg("Prefetch download failed")
			}
		default:
			// GROXPI_DOWNLOAD_TIMEOUT of 0 turns every download into a redirect, so
			// there is nothing to cache and prefetch cannot do its job.
			s.adminErrors.record(packageName, file.Name+": caching is disabled (download timeout is 0)")
		}
	}
}

// newestReleaseFiles returns the files of the newest final release in a
// package's index. It is the whole of prefetch's selection policy:
//
//   - yanked files are excluded, because the maintainer withdrew them;
//   - pre-releases are excluded, because pip does not install them by default;
//   - versions are compared under PEP 440 ordering, not as strings, so 1.10.0
//     sorts above 1.9.0.
//
// The index carries no version field — PEP 691 does not supply one — so the
// version comes from the filename. A filename whose version will not parse is
// skipped rather than guessed at.
func newestReleaseFiles(files []pypi.FileInfo) []pypi.FileInfo {
	type candidate struct {
		file    pypi.FileInfo
		version pep440.Version
	}

	candidates := make([]candidate, 0, len(files))
	for _, file := range files {
		if file.IsYanked() {
			continue
		}
		raw, ok := versionFromFilename(file.Name)
		if !ok {
			continue
		}
		parsed, err := pep440.Parse(raw)
		if err != nil {
			log.Debug().Str("file", file.Name).Str("version", raw).Msg("Skipping file with unparseable version")
			continue
		}
		if parsed.IsPreRelease() {
			continue
		}
		candidates = append(candidates, candidate{file: file, version: parsed})
	}

	if len(candidates) == 0 {
		return nil
	}

	newest := candidates[0].version
	for _, c := range candidates[1:] {
		if c.version.GreaterThan(newest) {
			newest = c.version
		}
	}

	// Every file of that release, not just the one that happened to be the
	// maximum: a release is a wheel per platform plus a source distribution.
	selected := make([]pypi.FileInfo, 0, 4)
	for _, c := range candidates {
		if c.version.Equal(newest) {
			selected = append(selected, c.file)
		}
	}
	return selected
}

// versionFromFilename extracts the version from a distribution filename.
//
// Wheels and eggs are hyphen-delimited with the version in second position
// (PEP 427). A source distribution is "<name>-<version><ext>", and since a
// project name may itself contain hyphens the version is what follows the last
// one.
func versionFromFilename(name string) (string, bool) {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".whl") || strings.HasSuffix(lower, ".egg") {
		parts := strings.Split(name[:len(name)-4], "-")
		if len(parts) < 2 || parts[1] == "" {
			return "", false
		}
		return parts[1], true
	}

	for _, ext := range sdistExtensions {
		if !strings.HasSuffix(lower, strings.ToLower(ext)) {
			continue
		}
		stem := name[:len(name)-len(ext)]
		_, version, found := lastCut(stem, "-")
		if !found || version == "" {
			return "", false
		}
		return version, true
	}

	return "", false
}

// lastCut is strings.Cut around the last separator rather than the first.
func lastCut(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}
