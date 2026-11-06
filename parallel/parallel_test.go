package parallel

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// drain collects the errors of a waiter until it is closed.
func drain(w *Waiter) (<-chan []error, func()) {
	out := make(chan []error, 1)
	go func() {
		var errs []error
		for err := range w.Err() {
			errs = append(errs, err)
		}
		out <- errs
	}()
	return out, w.Wait
}

func TestWorkerCount(t *testing.T) {
	if got := WorkerCount(0); got != minNumWorkers {
		t.Errorf("WorkerCount(0) = %d, want %d", got, minNumWorkers)
	}
	if got := WorkerCount(100); got != 100 {
		t.Errorf("WorkerCount(100) = %d, want 100", got)
	}
	if got, want := WorkerCount(-2), 2*runtime.NumCPU(); got != want && want >= minNumWorkers {
		t.Errorf("WorkerCount(-2) = %d, want %d", got, want)
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	const workers, tasks = 4, 50

	pm := New(workers)
	w := NewWaiter()
	errs, wait := drain(w)

	var cur, max int32
	for i := 0; i < tasks; i++ {
		pm.Run(func() error {
			n := atomic.AddInt32(&cur, 1)
			for {
				m := atomic.LoadInt32(&max)
				if n <= m || atomic.CompareAndSwapInt32(&max, m, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&cur, -1)
			return nil
		}, w)
	}
	wait()
	pm.Close()

	if got := <-errs; len(got) != 0 {
		t.Fatalf("unexpected errors: %v", got)
	}
	if max > workers {
		t.Fatalf("max concurrent tasks = %d, limit %d", max, workers)
	}
}

// Run must block the producer while all workers are busy: nothing is queued.
func TestRunAppliesBackpressure(t *testing.T) {
	pm := New(2)
	w := NewWaiter()
	_, wait := drain(w)

	release := make(chan struct{})
	started := make(chan struct{}, 2)
	block := func() error {
		started <- struct{}{}
		<-release
		return nil
	}
	pm.Run(block, w)
	pm.Run(block, w)
	<-started
	<-started

	var produced int32
	go func() {
		pm.Run(func() error { return nil }, w)
		atomic.StoreInt32(&produced, 1)
	}()

	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&produced) != 0 {
		t.Fatal("Run returned while all workers were busy")
	}

	close(release)
	wait()
	pm.Close()
	if atomic.LoadInt32(&produced) != 1 {
		t.Fatal("blocked Run did not return after workers were freed")
	}
}

func TestRunContextCancelUnblocksProducer(t *testing.T) {
	pm := New(2)
	w := NewWaiter()
	_, wait := drain(w)

	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	block := func() error {
		started <- struct{}{}
		<-release
		return nil
	}
	for i := 0; i < 2; i++ {
		if !pm.RunContext(ctx, block, w) {
			t.Fatal("RunContext refused a task before cancel")
		}
	}
	<-started
	<-started

	result := make(chan bool, 1)
	go func() {
		result <- pm.RunContext(ctx, func() error { return nil }, w)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case ok := <-result:
		if ok {
			t.Fatal("RunContext accepted a task after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunContext stayed blocked after cancel")
	}

	close(release)
	wait()
	pm.Close()
}

func TestNoTaskStartsAfterCancel(t *testing.T) {
	pm := New(8)
	w := NewWaiter()
	_, wait := drain(w)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var ran int32
	for i := 0; i < 100; i++ {
		if pm.RunContext(ctx, func() error {
			atomic.AddInt32(&ran, 1)
			return nil
		}, w) {
			t.Fatal("RunContext accepted a task with a canceled context")
		}
	}
	wait()
	pm.Close()

	if ran != 0 {
		t.Fatalf("%d tasks ran after cancel", ran)
	}
}

func TestErrorsAreReported(t *testing.T) {
	pm := New(4)
	w := NewWaiter()
	errs, wait := drain(w)

	boom := errors.New("boom")
	for i := 0; i < 10; i++ {
		pm.Run(func() error { return boom }, w)
	}
	wait()
	pm.Close()

	if got := <-errs; len(got) != 10 {
		t.Fatalf("got %d errors, want 10", len(got))
	}
}

func TestPanicIsReportedAndPoolSurvives(t *testing.T) {
	pm := New(2)
	w := NewWaiter()
	errs, wait := drain(w)

	var ok int32
	pm.Run(func() error { panic("bad task") }, w)
	for i := 0; i < 10; i++ {
		pm.Run(func() error {
			atomic.AddInt32(&ok, 1)
			return nil
		}, w)
	}
	wait()
	pm.Close()

	got := <-errs
	if len(got) != 1 || !strings.Contains(got[0].Error(), "bad task") {
		t.Fatalf("want one panic error, got %v", got)
	}
	if strings.Contains(got[0].Error(), "\n") {
		t.Fatalf("panic error should be one line, got %q", got[0])
	}
	if ok != 10 {
		t.Fatalf("%d healthy tasks ran, want 10", ok)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	pm := New(2)
	w := NewWaiter()
	_, wait := drain(w)
	pm.Run(func() error { return nil }, w)
	wait()
	wait() // Waiter.Wait twice must not panic either
	pm.Close()
	pm.Close()
}

func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	for round := 0; round < 5; round++ {
		pm := New(16)
		w := NewWaiter()
		_, wait := drain(w)
		ctx, cancel := context.WithCancel(context.Background())
		for i := 0; i < 200; i++ {
			if i == 100 {
				cancel()
			}
			pm.RunContext(ctx, func() error {
				time.Sleep(100 * time.Microsecond)
				return errors.New("x")
			}, w)
		}
		cancel()
		wait()
		pm.Close()
	}

	// goroutines exit asynchronously after their last send, give them a moment
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines: %d before, %d after", before, after)
	}
}
