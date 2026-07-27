package storage

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWorkerPool_ExecutesEverySubmittedJob verifies no job is lost while the
// queue has room for it.
func TestWorkerPool_ExecutesEverySubmittedJob(t *testing.T) {
	const jobs = 200

	var wg sync.WaitGroup
	var executed atomic.Int64
	seen := make([]atomic.Bool, jobs)

	pool := NewWorkerPool("test-all-jobs", jobs, 8, func(_ context.Context, i int) {
		defer wg.Done()
		seen[i].Store(true)
		executed.Add(1)
	})
	defer pool.Close()

	for i := range jobs {
		wg.Add(1)
		require.True(t, pool.Submit(i), "submit %d should succeed with a queue of %d", i, jobs)
	}
	wg.Wait()

	assert.Equal(t, int64(jobs), executed.Load())
	for i := range jobs {
		assert.True(t, seen[i].Load(), "job %d never ran", i)
	}
}

// TestWorkerPool_ConcurrencyNeverExceedsWorkers verifies the worker count caps
// how many jobs run at the same time.
func TestWorkerPool_ConcurrencyNeverExceedsWorkers(t *testing.T) {
	const (
		workers = 4
		jobs    = 64
	)

	var (
		mu           sync.Mutex
		inFlight     int
		peakInFlight int
		wg           sync.WaitGroup
	)

	pool := NewWorkerPool("test-concurrency", jobs, workers, func(_ context.Context, _ int) {
		defer wg.Done()

		mu.Lock()
		inFlight++
		if inFlight > peakInFlight {
			peakInFlight = inFlight
		}
		mu.Unlock()

		time.Sleep(2 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()
	})
	defer pool.Close()

	for i := range jobs {
		wg.Add(1)
		require.True(t, pool.Submit(i))
	}
	wg.Wait()

	mu.Lock()
	peak := peakInFlight
	mu.Unlock()

	assert.LessOrEqual(t, peak, workers, "concurrency exceeded the worker cap")
	assert.Positive(t, peak)
}

// TestWorkerPool_SubmitDropsWhenQueueFull verifies Submit is non-blocking and
// reports the drop instead of applying backpressure.
func TestWorkerPool_SubmitDropsWhenQueueFull(t *testing.T) {
	release := make(chan struct{})
	firstStarted := make(chan struct{})
	var once sync.Once

	pool := NewWorkerPool("test-full", 2, 1, func(_ context.Context, _ int) {
		once.Do(func() { close(firstStarted) })
		<-release
	})
	defer func() {
		close(release)
		pool.Close()
	}()

	require.True(t, pool.Submit(1))
	<-firstStarted // the single worker is now parked inside job 1

	require.True(t, pool.Submit(2), "queue slot 1")
	require.True(t, pool.Submit(3), "queue slot 2")
	assert.False(t, pool.Submit(4), "queue is full, job must be dropped")
}

// TestWorkerPool_CloseWaitsForInFlightJobs verifies Close does not return while
// a job is still running.
func TestWorkerPool_CloseWaitsForInFlightJobs(t *testing.T) {
	started := make(chan struct{})
	var finished atomic.Bool

	pool := NewWorkerPool("test-inflight", 1, 1, func(_ context.Context, _ int) {
		close(started)
		time.Sleep(50 * time.Millisecond)
		finished.Store(true)
	})

	require.True(t, pool.Submit(1))
	<-started
	pool.Close()

	assert.True(t, finished.Load(), "Close returned before the in-flight job finished")
}

// TestWorkerPool_DrainRunsQueuedJobs verifies Drain finishes queued work before
// closing, which is what stops a shutdown from silently discarding it.
func TestWorkerPool_DrainRunsQueuedJobs(t *testing.T) {
	const jobs = 20

	var executed atomic.Int64

	pool := NewWorkerPool("test-drain", jobs, 1, func(_ context.Context, _ int) {
		time.Sleep(time.Millisecond)
		executed.Add(1)
	})

	for i := range jobs {
		require.True(t, pool.Submit(i))
	}

	pool.Drain(5 * time.Second)

	assert.Equal(t, int64(jobs), executed.Load(), "Drain closed before the queue was empty")
}

// TestWorkerPool_DrainTimesOutCleanly verifies a queue that cannot be emptied in
// time is abandoned rather than hanging shutdown, and that Drain still waits for
// the job already running.
func TestWorkerPool_DrainTimesOutCleanly(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	var finished atomic.Bool

	pool := NewWorkerPool("test-drain-timeout", 8, 1, func(_ context.Context, _ int) {
		once.Do(func() { close(started) })
		<-release
		finished.Store(true)
	})

	for i := range 5 {
		require.True(t, pool.Submit(i))
	}
	<-started // the only worker is parked, so the rest cannot drain

	done := make(chan struct{})
	go func() {
		pool.Drain(50 * time.Millisecond)
		close(done)
	}()

	// Drain must not return while the running job is still going.
	select {
	case <-done:
		t.Fatal("Drain abandoned an in-flight job")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain never returned")
	}

	assert.True(t, finished.Load(), "the in-flight job was cancelled instead of finished")
}

// TestWorkerPool_CloseIsIdempotentAndReleasesWorkers verifies a double Close is
// safe, Submit is rejected afterwards, and no worker goroutine is leaked.
func TestWorkerPool_CloseIsIdempotentAndReleasesWorkers(t *testing.T) {
	baseline := runtime.NumGoroutine()

	pool := NewWorkerPool("test-close", 4, 3, func(_ context.Context, _ int) {})
	require.True(t, pool.Submit(1))

	pool.Close()
	assert.NotPanics(t, pool.Close, "Close must be idempotent")

	assert.False(t, pool.Submit(2), "Submit after Close must be rejected")

	assert.Eventually(t, func() bool {
		return runtime.NumGoroutine() <= baseline+1
	}, time.Second, 10*time.Millisecond, "worker goroutines leaked after Close")
}

// TestWorkerPool_ConcurrentSubmitDuringClose guards the send-on-closed-channel
// panic the two hand-rolled queues were exposed to.
func TestWorkerPool_ConcurrentSubmitDuringClose(t *testing.T) {
	pool := NewWorkerPool("test-close-race", 8, 4, func(_ context.Context, _ int) {})

	var submitters sync.WaitGroup
	for range 8 {
		submitters.Add(1)
		go func() {
			defer submitters.Done()
			for j := range 500 {
				pool.Submit(j)
			}
		}()
	}

	time.Sleep(time.Millisecond)
	assert.NotPanics(t, pool.Close)
	submitters.Wait() // a panicking Submit would take the test process down
}
