package parallel

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
)

const (
	minNumWorkers = 2
)

// Task is a function type for parallel manager.
type Task func() error

// Manager is a structure for running tasks in parallel.
//
// A Manager has no task queue. Run blocks the caller until one of the
// workers slots is free, so producers that walk huge listings are throttled
// to the speed of the workers and memory use does not grow with the number of
// pending objects.
type Manager struct {
	wg        *sync.WaitGroup
	semaphore chan struct{}
}

// WorkerCount returns the effective number of workers for the given
// configuration value. Negative values are multiplied with the number of
// CPUs, and the result is never smaller than two.
func WorkerCount(workercount int) int {
	if workercount < 0 {
		workercount = runtime.NumCPU() * -workercount
	}

	if workercount < minNumWorkers {
		workercount = minNumWorkers
	}

	return workercount
}

// New creates a new parallel.Manager.
func New(workercount int) *Manager {
	return &Manager{
		wg:        &sync.WaitGroup{},
		semaphore: make(chan struct{}, WorkerCount(workercount)),
	}
}

// acquire limits concurrency by trying to acquire the semaphore. It gives up
// and returns false if ctx is done first.
func (p *Manager) acquire(ctx context.Context) bool {
	// A free slot must not win against an already canceled context, select
	// picks randomly among ready cases.
	if ctx.Err() != nil {
		return false
	}

	select {
	case p.semaphore <- struct{}{}:
		p.wg.Add(1)
		return true
	case <-ctx.Done():
		return false
	}
}

// release releases the acquired semaphore to signal that a task is finished.
func (p *Manager) release() {
	p.wg.Done()
	<-p.semaphore
}

// Run runs the given task while limiting the concurrency.
func (p *Manager) Run(fn Task, waiter *Waiter) {
	p.RunContext(context.Background(), fn, waiter)
}

// RunContext runs the given task while limiting the concurrency. It blocks
// until a worker is available. If ctx is done before that, or before the task
// had a chance to start, the task is not run and RunContext returns false.
// Callers that feed tasks in a loop should stop feeding when it returns false.
//
// A panic inside the task is recovered and reported to the waiter as an
// error, so one bad task cannot kill the other transfers.
func (p *Manager) RunContext(ctx context.Context, fn Task, waiter *Waiter) bool {
	waiter.wg.Add(1)
	if !p.acquire(ctx) {
		waiter.wg.Done()
		return false
	}

	go func() {
		defer waiter.wg.Done()
		defer p.release()

		if ctx.Err() != nil {
			return
		}

		if err := runTask(fn); err != nil {
			waiter.errch <- err
		}
	}()

	return true
}

func runTask(fn Task) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in worker: %v\n%s", r, debug.Stack())
		}
	}()

	return fn()
}

// Close waits all tasks to finish. It can be called more than once.
func (p *Manager) Close() {
	p.wg.Wait()
}

// Waiter is a structure for waiting and reading
// error messages created by Manager.
type Waiter struct {
	wg    sync.WaitGroup
	errch chan error
	once  sync.Once
}

// NewWaiter creates a new parallel.Waiter.
func NewWaiter() *Waiter {
	return &Waiter{
		errch: make(chan error),
	}
}

// Wait blocks until the WaitGroup counter is zero
// and closes error channel.
func (w *Waiter) Wait() {
	w.wg.Wait()
	w.once.Do(func() { close(w.errch) })
}

// Err returns read-only error channel.
func (w *Waiter) Err() <-chan error {
	return w.errch
}
