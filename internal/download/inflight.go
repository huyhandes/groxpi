package download

import (
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Progress is one upstream download as the admin surface sees it.
type Progress struct {
	Package  string
	File     string
	Size     int64 // declared by the index, -1 if unknown
	Bytes    int64 // streamed so far
	Started  time.Time
	Requests int // clients coalesced onto this download, leader included
}

// inflight is the registry of running downloads: the singleflight group made
// readable. An entry appears when the first request for a key arrives and
// disappears when the leader's download returns.
//
// ponytail: a request that joins in the instant between the leader leaving and
// singleflight releasing its waiters is counted against a fresh entry. The
// registry is a display, not an accounting ledger.
type inflight struct {
	mu      sync.Mutex
	entries map[string]*progress
}

type progress struct {
	Progress
	bytes    atomic.Int64
	requests atomic.Int64
}

func (f *inflight) join(plan ServePlan) *progress {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entries == nil {
		f.entries = make(map[string]*progress)
	}
	p := f.entries[plan.StorageKey]
	if p == nil {
		p = &progress{Progress: Progress{
			Package: plan.PackageName,
			File:    plan.FileName,
			Size:    plan.Size,
			Started: time.Now(),
		}}
		f.entries[plan.StorageKey] = p
	}
	p.requests.Add(1)
	return p
}

func (f *inflight) leave(key string) {
	f.mu.Lock()
	delete(f.entries, key)
	f.mu.Unlock()
}

func (f *inflight) count() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.entries))
}

// InFlight lists the downloads running right now, oldest first.
func (s *Service) InFlight() []Progress {
	s.inflight.mu.Lock()
	out := make([]Progress, 0, len(s.inflight.entries))
	for _, p := range s.inflight.entries {
		snap := p.Progress
		snap.Bytes = p.bytes.Load()
		snap.Requests = int(p.requests.Load())
		out = append(out, snap)
	}
	s.inflight.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// countingWriter adds up the bytes that reach the leader's client.
type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}
