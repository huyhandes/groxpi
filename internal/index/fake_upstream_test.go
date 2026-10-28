package index

import (
	"context"
	"sync"
	"testing"

	"github.com/huyhandes/groxpi/internal/config"
)

// fakeUpstream is the second implementation of upstream: each index URL maps
// package names to an answer, and every query is counted per index. A package
// an index does not list is ErrNotFound there.
type fakeUpstream struct {
	mu      sync.Mutex
	answers map[string]map[string]fakeAnswer // index URL -> package -> answer
	queries map[string]int                   // index URL + " " + package -> count
}

type fakeAnswer struct {
	files []FileInfo
	err   error
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{answers: map[string]map[string]fakeAnswer{}, queries: map[string]int{}}
}

// set makes index answer pkg with files and err.
func (f *fakeUpstream) set(index, pkg string, files []FileInfo, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.answers[index] == nil {
		f.answers[index] = map[string]fakeAnswer{}
	}
	f.answers[index][pkg] = fakeAnswer{files: files, err: err}
}

func (f *fakeUpstream) GetPackageFiles(_ context.Context, index config.Index, pkg string) ([]FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries[index.URL+" "+pkg]++
	answer, ok := f.answers[index.URL][pkg]
	if !ok {
		return nil, ErrNotFound
	}
	return answer.files, answer.err
}

// queried is how many times index was asked for pkg.
func (f *fakeUpstream) queried(index, pkg string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queries[index+" "+pkg]
}

// newFakeService builds the index module over a fake upstream. Call it inside a
// synctest bubble: the cache sweeper then runs on the fake clock, and Close
// (registered here) stops it before the bubble ends.
func newFakeService(t *testing.T, cfg *config.Config, up upstream) *Service {
	t.Helper()
	svc := newService(cfg, newIndexCache(cfg.IndexCacheSize, sweepInterval(cfg)), up, newTransport(cfg))
	t.Cleanup(svc.Close)
	return svc
}
