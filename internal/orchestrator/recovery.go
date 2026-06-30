// Package orchestrator — crash recovery on startup.
// Authoritative: DESIGN.md §9.3, CLAUDE.md "Three Recovery Rules".
package orchestrator

import (
	"context"
	"database/sql"
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
//	SELECT run_id FROM runs WHERE status = 'RUNNING'
//	for each run_id → call makeLoop → go loop.Run(ctx)
//
// The three CLAUDE.md recovery rules are handled transparently by Loop.Run:
//   - Loop.Run calls PendingDecision first; if non-nil (DECIDED or RUNNING
//     step with no output), it re-dispatches without re-asking the planner
//     (Barrier 1 already fired — rules 1 and 2).
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
	db *sql.DB,
	makeLoop func(runID core.RunID, workflowInput json.RawMessage) *Loop,
) (int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT run_id, workflow_input
		FROM   runs
		WHERE  status = 'RUNNING'
	`)
	if err != nil {
		return 0, fmt.Errorf("RecoverRuns: query running runs: %w", err)
	}
	defer rows.Close()

	type runEntry struct {
		id    core.RunID
		input json.RawMessage
	}

	var pending []runEntry
	for rows.Next() {
		var id string
		var input json.RawMessage
		if err := rows.Scan(&id, &input); err != nil {
			return 0, fmt.Errorf("RecoverRuns: scan row: %w", err)
		}
		pending = append(pending, runEntry{id: core.RunID(id), input: input})
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("RecoverRuns: iterate rows: %w", err)
	}

	slog.Info("[RECOVERY] found in-progress runs", "count", len(pending))

	for _, r := range pending {
		// Query step context for the recovery log line.
		var doneCount int
		var pendingStep sql.NullString
		_ = db.QueryRowContext(ctx, `
			SELECT
				(SELECT COUNT(*) FROM steps WHERE run_id = $1 AND status = 'DONE'),
				(SELECT step_name FROM steps WHERE run_id = $1 AND status IN ('RUNNING','DECIDED') ORDER BY seq LIMIT 1)
		`, string(r.id)).Scan(&doneCount, &pendingStep)

		step := "-"
		if pendingStep.Valid {
			step = pendingStep.String
		}
		slog.Info("[RECOVERY] resuming run",
			"run_id", string(r.id),
			"steps_done", doneCount,
			"pending_step", step)

		l := makeLoop(r.id, r.input)
		if l == nil {
			slog.Warn("[RECOVERY] skipping run: makeLoop returned nil", "run_id", string(r.id))
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

	slog.Info("[RECOVERY] complete", "resumed", len(pending))
	return len(pending), nil
}
