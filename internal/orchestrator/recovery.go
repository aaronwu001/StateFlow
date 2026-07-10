// Package orchestrator — crash recovery on startup.
// Authoritative: docs/StateFlow_Whitepaper_v1_0.md §8.3 (recovery), §8.2
// (the run × last_step combination table).
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/aaronwu000/stateflow/internal/core"
)

// RecoverRuns scans for all RUNNING runs at startup and re-enters the driver
// loop for each in a separate goroutine. This function is called ONCE from
// main.go, before the HTTP server begins accepting new requests.
//
// Design (whitepaper §8.3):
//
//	store.ListRunningRuns(ctx)
//	for each run → call makeLoop → go loop.Run(ctx)
//
// Recovery is NOT a special mode; it is the same loop entered from a
// DB-persisted mid-run state (whitepaper §2). The entire run×last_step
// combination table (§8.2) is handled transparently by Loop.Run:
//   - run=running, last_step=done (or no steps) → Run's steady-state loop
//     asks the planner directly (LoadFrontier's PendingStep is nil).
//   - run=running, last_step=running → Run's one-time recovery check finds
//     LoadFrontier's PendingStep non-nil and performs the three-step
//     recovery action (§8.3): claim the orphan (TX3, possibly DLQ'ing),
//     budget check, then TX4 + dispatch.
//   - run=done / run=DLQ: ListRunningRuns never returns them (it queries
//     status='RUNNING' only) — they are never touched, exactly as required.
//
// makeLoop constructs a fully configured Loop for the given run. In
// production, main.go provides this factory (wiring Store/Transport/Retry;
// the planner is NOT part of the factory's job — Loop.Run reconstructs it
// from the workflow row itself, whitepaper §12.1). In tests, stubs are
// injected via the returned Loop's fields (including PlannerFactory).
//
// Returns the count of goroutines started and any startup error. Individual
// run errors are logged but do not surface to the caller — runs are
// independent (whitepaper §8.3: "crash loops converge" per run, not
// globally).
func RecoverRuns(
	ctx context.Context,
	store core.StateStore,
	makeLoop func(runID core.RunID, workflowID core.WorkflowID, workflowInput json.RawMessage) *Loop,
) (int, error) {
	runs, err := store.ListRunningRuns(ctx)
	if err != nil {
		return 0, fmt.Errorf("RecoverRuns: list running runs: %w", err)
	}

	slog.Info("[RECOVERY] found in-progress runs", "count", len(runs))

	for _, r := range runs {
		frontier, err := store.LoadFrontier(ctx, r.RunID)
		if err != nil {
			slog.Error("[RECOVERY] load frontier", "run_id", string(r.RunID), "err", err)
			continue
		}

		step := "-"
		attemptCount := 0
		if frontier.PendingStep != nil {
			step = frontier.PendingStep.Name
			attemptCount = frontier.AttemptCount
		}
		slog.Info("[RECOVERY] resuming run",
			"run_id", string(r.RunID),
			"steps_done", len(frontier.History),
			"pending_step", step,
			"attempt_count", attemptCount)

		l := makeLoop(r.RunID, r.WorkflowID, r.WorkflowInput)
		if l == nil {
			slog.Warn("[RECOVERY] skipping run: makeLoop returned nil", "run_id", string(r.RunID))
			continue
		}
		go func(l *Loop) {
			if err := l.Run(ctx); err != nil {
				slog.Error("[RECOVERY] run ended with error", "run_id", string(l.RunID), "err", err)
			} else {
				slog.Info("[RECOVERY] run completed", "run_id", string(l.RunID))
			}
		}(l)
	}

	slog.Info("[RECOVERY] complete", "resumed", len(runs))
	return len(runs), nil
}
