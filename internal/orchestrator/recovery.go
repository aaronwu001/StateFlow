// Package orchestrator — crash recovery on startup.
// Authoritative: DESIGN.md §9.3, CLAUDE.md "Four Recovery Rules".
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
// Design (DESIGN.md §9.3):
//
//	store.ListRunningRuns(ctx)
//	for each run → call makeLoop → go loop.Run(ctx)
//
// The four CLAUDE.md recovery rules are handled transparently by Loop.Run:
//   - Loop.Run calls PendingDecision first; if non-nil (DECIDED, RUNNING, or
//     FAILED step with no output), it re-dispatches without re-asking the
//     planner (Barrier 1 already fired — rules 1, 2, and 4).
//   - If PendingDecision returns nil, the loop asks the planner against the
//     persisted frontier of DONE steps (rule 3).
//
// Recovery is NOT a special mode; it is the same loop entered from a
// DB-persisted mid-run state. Recovery and normal operation converge on
// identical code paths.
//
// RUNNING-uncertain (CLAUDE.md): a RUNNING step with no output is uncertain —
// the worker may have finished but the checkpoint was lost to the crash. It is
// NOT failed. The loop re-dispatches it (generates a new attempt_id) and relies
// on worker idempotency.
//
// makeLoop constructs a fully configured Loop for the given run. In production,
// main.go provides this factory (using the workflow's planner_type/planner_config).
// In tests, stubs are injected.
//
// Returns the count of goroutines started and any startup error. Individual run
// errors are logged but do not surface to the caller — runs are independent.
func RecoverRuns(
	ctx context.Context,
	store core.StateStore,
	makeLoop func(runID core.RunID, workflowInput json.RawMessage) *Loop,
) (int, error) {
	runs, err := store.ListRunningRuns(ctx)
	if err != nil {
		return 0, fmt.Errorf("RecoverRuns: list running runs: %w", err)
	}

	slog.Info("[RECOVERY] found in-progress runs", "count", len(runs))

	for _, r := range runs {
		frontier, err := store.LoadFrontier(r.RunID)
		if err != nil {
			slog.Error("[RECOVERY] load frontier", "run_id", string(r.RunID), "err", err)
			continue
		}

		step := "-"
		if frontier.PendingDecision != nil {
			step = frontier.PendingDecision.Name
		}
		slog.Info("[RECOVERY] resuming run",
			"run_id", string(r.RunID),
			"steps_done", len(frontier.History),
			"pending_step", step)

		l := makeLoop(r.RunID, r.WorkflowInput)
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
