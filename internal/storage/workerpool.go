package storage

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// WorkerPool runs jobs of type T on a fixed set of goroutines fed by a bounded
// queue. It owns queueing, the concurrency cap and shutdown; callers supply
// only the job function.
//
// Submit never blocks: when the queue is full the job is dropped and the caller
// decides how to report it. The worker count is the concurrency cap, so no
// separate semaphore is needed.
type WorkerPool[T any] struct {
	name      string
	queue     chan T
	run       func(context.Context, T)
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// NewWorkerPool starts workers goroutines draining a queueSize-deep queue,
// handing every job to run along with the pool's lifetime context (cancelled by
// Close). name only appears in log lines.
func NewWorkerPool[T any](name string, queueSize, workers int, run func(context.Context, T)) *WorkerPool[T] {
	if workers < 1 {
		workers = 1
	}
	if queueSize < 1 {
		queueSize = 1
	}

	ctx, cancel := context.WithCancel(context.Background()) // #nosec G118 -- cancel is kept on the pool and called by Close
	pool := &WorkerPool[T]{
		name:   name,
		queue:  make(chan T, queueSize),
		run:    run,
		ctx:    ctx,
		cancel: cancel,
	}

	pool.wg.Add(workers)
	for i := range workers {
		go pool.worker(i)
	}

	slog.Info("Worker pool initialized", "pool", name, "workers", workers, "queue_size", queueSize)

	return pool
}

// worker drains the queue until the pool is closed.
func (p *WorkerPool[T]) worker(id int) {
	defer p.wg.Done()

	slog.Debug("Worker started", "pool", p.name, "worker_id", id)

	for {
		select {
		case <-p.ctx.Done():
			slog.Debug("Worker shutting down", "pool", p.name, "worker_id", id)
			return
		case job := <-p.queue:
			p.run(p.ctx, job)
		}
	}
}

// Submit queues job without blocking. It returns false — dropping the job —
// when the queue is full or the pool has been closed.
func (p *WorkerPool[T]) Submit(job T) bool {
	select {
	case <-p.ctx.Done():
		return false
	default:
	}

	select {
	case p.queue <- job:
		return true
	default:
		return false
	}
}

// Drain waits for the queue to empty, up to timeout, and then closes the pool.
// Jobs still queued when the timeout expires are dropped; a job already running
// is always waited for by Close.
func (p *WorkerPool[T]) Drain(timeout time.Duration) {
	deadline := time.After(timeout)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()

	for len(p.queue) > 0 {
		select {
		case <-deadline:
			slog.Warn("Drain timed out, dropping queued jobs", "pool", p.name, "dropped", len(p.queue))
			p.Close()
			return
		case <-tick.C:
		}
	}

	p.Close()
}

// Close stops the workers and waits for jobs already in flight to finish. Jobs
// still waiting in the queue are dropped. Safe to call more than once and safe
// to race against Submit.
func (p *WorkerPool[T]) Close() {
	p.closeOnce.Do(func() {
		p.cancel()
		p.wg.Wait()
		slog.Info("Worker pool shut down", "pool", p.name)
	})
}
