package admin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	pep440 "github.com/aquasecurity/go-pep440-version"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/index"
)

// sdistExtensions are the source-distribution suffixes whose filenames end in
// the version. Longest first, so ".tar.gz" is not truncated to ".gz".
var sdistExtensions = []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.Z", ".tgz", ".tbz2", ".zip", ".tar"}

// prefetchDeadline bounds one detached prefetch. It is deliberately far above
// any real package: it stops a wedged upstream pinning a goroutine for the life
// of the process, not a download from taking its time. It is far too long to
// bound shutdown — that is Close's context's job.
const prefetchDeadline = 30 * time.Minute

// Prefetch warms the newest release of each package in the background and
// returns at once. The downloads run on a context detached from ctx so a large
// package does not time out behind a reverse proxy; there is no job registry and
// no identifier, because the polling table already shows the files arriving.
// It returns ErrShuttingDown once Close has begun.
func (s *Service) Prefetch(ctx context.Context, pkgs ...string) error {
	ctx = context.WithoutCancel(ctx)
	for _, pkg := range pkgs {
		if !s.startPrefetch(ctx, pkg) {
			return ErrShuttingDown
		}
	}
	return nil
}

// startPrefetch runs one prefetch in the background. Everything that makes a
// detached goroutine dangerous is handled here rather than in the work itself:
// it is registered so shutdown waits for it, deduplicated so an impatient
// operator submitting the same name ten times gets one download, deadlined so it
// cannot outlive the process it is holding open, and its panics are contained —
// the server's recover middleware only wraps the request goroutine.
//
// It reports false once shutdown has begun, when the work could not have
// finished anyway. Taking the lock around the registration is what keeps the Add
// from racing Close's Wait.
func (s *Service) startPrefetch(ctx context.Context, packageName string) bool {
	s.prefetchMu.Lock()
	if s.shuttingDown {
		s.prefetchMu.Unlock()
		return false
	}
	s.prefetches.Add(1)
	s.prefetchMu.Unlock()

	go func() {
		defer s.prefetches.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Prefetch panicked", "package", packageName, "panic", r)
				s.errors.record(packageName, "prefetch failed unexpectedly")
			}
		}()

		ctx, cancel := context.WithTimeout(ctx, prefetchDeadline)
		defer cancel()

		_, _, _ = s.prefetchSF.Do(packageName, func() (any, error) {
			s.prefetchPackage(ctx, packageName)
			return nil, nil
		})
	}()

	return true
}

// prefetchPackage resolves the package's index, picks its newest release and
// warms that release's files. Failures are recorded for the page's
// recent-errors area as well as logged: an operator watching an empty table must
// not have to read the server log to learn why it is empty.
func (s *Service) prefetchPackage(ctx context.Context, packageName string) {
	entry, err := s.index.Resolve(ctx, packageName)
	if err != nil {
		if errors.Is(err, index.ErrNotFound) {
			s.errors.record(packageName, "not found on any configured index")
		} else {
			s.errors.record(packageName, "index lookup failed: "+config.RedactErrorText(err))
		}
		slog.ErrorContext(ctx, "Prefetch could not resolve index", "error", config.RedactErrorText(err), "package", packageName)
		return
	}

	files := newestReleaseFiles(entry.Files)
	if len(files) == 0 {
		s.errors.record(packageName, "no downloadable final release (all files yanked, pre-release or unparseable)")
		return
	}

	slog.InfoContext(ctx, "Prefetching newest release", "package", packageName, "files", len(files))

	for _, file := range files {
		if err := s.warm(ctx, packageName, file.Name); err != nil {
			// Redacted, not raw: the file URL is resolved against the index base, so
			// a credentialed private index puts its password inside this error — and
			// this message is both logged and rendered onto the admin page.
			s.errors.record(packageName, file.Name+": "+config.RedactErrorText(err))
			slog.ErrorContext(ctx, "Prefetch download failed",
				"error", config.RedactErrorText(err), "package", packageName, "file", file.Name)
		}
	}
}

// warm caches one file with no client attached: open it through the store and
// drain it. A hit is already cached; a miss drains the whole download path.
func (s *Service) warm(ctx context.Context, packageName, fileName string) error {
	if s.noCache {
		return errors.New("caching is disabled (download timeout is 0)")
	}
	obj, err := s.store.Open(ctx, packageName, fileName, true)
	if err != nil {
		return err
	}
	defer func() { _ = obj.Close() }()
	if !obj.InFlight {
		return nil
	}
	_, err = io.Copy(io.Discard, obj)
	return err
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
func newestReleaseFiles(files []index.FileInfo) []index.FileInfo {
	type candidate struct {
		file    index.FileInfo
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
			slog.Debug("Skipping file with unparseable version", "file", file.Name, "version", raw)
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
	selected := make([]index.FileInfo, 0, 4)
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
		i := strings.LastIndex(stem, "-")
		if i < 0 || i == len(stem)-1 {
			return "", false
		}
		return stem[i+1:], true
	}

	return "", false
}
