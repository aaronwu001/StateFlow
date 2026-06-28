package transport

import (
	"context"

	"github.com/aaronwu000/stateflow/internal/core"
)

// MultiTransport routes Dispatch to Sync or Async based on step.Mode.
// The loop is transport-oblivious; this adapter handles the per-step routing.
type MultiTransport struct {
	Sync  core.WorkerTransport
	Async core.WorkerTransport
}

func (m *MultiTransport) Dispatch(ctx context.Context, step core.StepSpec) (core.Result, error) {
	if step.Mode == "async" {
		return m.Async.Dispatch(ctx, step)
	}
	return m.Sync.Dispatch(ctx, step)
}
