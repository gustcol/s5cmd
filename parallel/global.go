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
func Run(task Task, waiter *Waiter) { global.Run(task, waiter) }

// RunContext runs global ParallelManager. See Manager.RunContext.
func RunContext(ctx context.Context, task Task, waiter *Waiter) bool {
	return global.RunContext(ctx, task, waiter)
}
