package parallel

import (
	"context"

	"github.com/peak/s5cmd/v2/parallel/fdlimit"
)

var global *Manager

// Init tries to increase the soft limit of open files and
// creates new global ParallelManager.
func Init(workercount int) {
	_ = fdlimit.Raise()
	global = New(workercount)
}

// Close waits all jobs to finish.
func Close() {
	if global != nil {
		global.Close()
	}
}

// Run runs global ParallelManager.
// Init must be called before Run, otherwise this will panic.
func Run(task Task, waiter *Waiter) {
	if global == nil {
		panic("parallel: Run called before Init")
	}
	global.Run(task, waiter)
}

// RunContext runs global ParallelManager. See Manager.RunContext.
// Init must be called before RunContext, otherwise this will panic.
func RunContext(ctx context.Context, task Task, waiter *Waiter) bool {
	if global == nil {
		panic("parallel: RunContext called before Init")
	}
	return global.RunContext(ctx, task, waiter)
}
