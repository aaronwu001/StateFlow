// Package store implements StateStore backed by PostgreSQL.
// Authoritative design: DESIGN.md §4 (schema), §9.1 (Checkpoint paths), §9.2 (seq).
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/aaronwu000/stateflow/internal/core"
)

// PostgresStore is the reference implementation of core.StateStore.
// It uses database/sql with the pgx driver (github.com/jackc/pgx/v5/stdlib).
type PostgresStore struct {
	db *sql.DB
}

// New returns a PostgresStore backed by db.
// The caller is responsible for opening the connection and verifying connectivity.
func New(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// PutDecision writes the planner's chosen step with status DECIDED.
// This is Barrier 1: the row is committed before any dispatch occurs.
//
// seq is assigned as MAX(seq)+1 within the run. This is safe because the MVP
// invariant guarantees at most one driver loop goroutine per run (DESIGN.md §9.2).
//
// current_attempt_id is NULL at this stage — no dispatch has happened yet.
// It is populated by RecordAttemptStart, called by the loop just before Dispatch.
func (s *PostgresStore) PutDecision(run core.RunID, step core.StepSpec) error {
	stepID := fmt.Sprintf("%s:%s", run, step.Name)

	decisionJSON, err := json.Marshal(step)
	if err != nil {
		return fmt.Errorf("PutDecision: marshal decision: %w", err)
	}

	_, err = s.db.Exec(`
		WITH next_seq AS (
			SELECT COALESCE(MAX(seq), 0) + 1 AS seq
			FROM steps
			WHERE run_id = $1
		)
		INSERT INTO steps (step_id, run_id, step_name, seq, status, decision, decided_at)
		SELECT $2, $1, $3, next_seq.seq, 'DECIDED', $4::jsonb, now()
		FROM next_seq
	`, string(run), stepID, step.Name, string(decisionJSON))
	if err != nil {
		return fmt.Errorf("PutDecision: insert step %q: %w", stepID, err)
	}

	return nil
}

// RecordAttemptStart records the beginning of a worker dispatch.
// It must be called after PutDecision and before WorkerTransport.Dispatch.
// It creates an attempts row (status RUNNING) and updates the step to RUNNING.
//
// This is NOT in the StateStore interface — the loop calls it on the concrete
// type. It will be promoted to the interface when the loop session defines needs.
//
// attempt_number increments per step (not per run): the Nth attempt of this step.
func (s *PostgresStore) RecordAttemptStart(run core.RunID, step core.StepSpec, attemptID core.AttemptID) error {
	stepID := fmt.Sprintf("%s:%s", run, step.Name)

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("RecordAttemptStart: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var attemptNumber int
	if err := tx.QueryRow(`
		SELECT COALESCE(MAX(attempt_number), 0) + 1
		FROM attempts
		WHERE step_id = $1
	`, stepID).Scan(&attemptNumber); err != nil {
		return fmt.Errorf("RecordAttemptStart: count attempts for %q: %w", stepID, err)
	}

	if _, err := tx.Exec(`
		INSERT INTO attempts (attempt_id, step_id, attempt_number, status, dispatched_at)
		VALUES ($1, $2, $3, 'RUNNING', now())
	`, string(attemptID), stepID, attemptNumber); err != nil {
		return fmt.Errorf("RecordAttemptStart: insert attempt: %w", err)
	}

	if _, err := tx.Exec(`
		UPDATE steps
		SET status             = 'RUNNING',
		    current_attempt_id = $1::uuid
		WHERE step_id = $2
	`, string(attemptID), stepID); err != nil {
		return fmt.Errorf("RecordAttemptStart: update step %q: %w", stepID, err)
	}

	return tx.Commit()
}

// Checkpoint writes the worker's result and advances the step status.
// This is Barrier 2: all writes commit before the next Decide call.
//
// Path A (r.Status == "done"): writes steps.output (the Barrier 2 JSONB signal),
// marks step DONE, resolves the attempts row as DONE.
//
// Path B (r.Status == "failed"): steps.output stays NULL (the step is NOT done —
// writing output here would falsely mark it done for recovery). Writes the error
// to the attempts row and marks step FAILED. Retry/DLQ decisions are the loop's
// responsibility (DESIGN.md §9.1) — this method does not consult RetryPolicy.
//
// Both paths update the attempts row atomically with the step update in one
// transaction. If current_attempt_id is NULL (no dispatch recorded), the
// attempts update is skipped — this only occurs in the barrier invariant test.
func (s *PostgresStore) Checkpoint(run core.RunID, step core.StepSpec, r core.Result) error {
	stepID := fmt.Sprintf("%s:%s", run, step.Name)

	// Look up the current attempt. NULL is valid (barrier test doesn't call RecordAttemptStart).
	var currentAttemptID sql.NullString
	if err := s.db.QueryRow(`
		SELECT current_attempt_id::text FROM steps WHERE step_id = $1
	`, stepID).Scan(&currentAttemptID); err == sql.ErrNoRows {
		return fmt.Errorf("Checkpoint: step %q not found", stepID)
	} else if err != nil {
		return fmt.Errorf("Checkpoint: lookup step %q: %w", stepID, err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("Checkpoint: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	switch r.Status {
	case "done":
		if err := checkpointDone(tx, stepID, currentAttemptID, r); err != nil {
			return err
		}
	case "failed":
		if err := checkpointFailed(tx, stepID, currentAttemptID, r); err != nil {
			return err
		}
	default:
		return fmt.Errorf("Checkpoint: unknown result status %q", r.Status)
	}

	return tx.Commit()
}

// checkpointDone implements Checkpoint Path A inside a transaction.
// steps.output non-null is the physical Barrier 2 signal for recovery.
// r.Error and r.HTTPStatus are NOT written to steps.output — they belong
// in attempts.error on failure (DESIGN.md §9.1).
func checkpointDone(tx *sql.Tx, stepID string, attemptID sql.NullString, r core.Result) error {
	outputStr := "null"
	if len(r.Output) > 0 {
		outputStr = string(r.Output)
	}

	res, err := tx.Exec(`
		UPDATE steps
		SET output       = $1::jsonb,
		    status       = 'DONE',
		    completed_at = now()
		WHERE step_id = $2
	`, outputStr, stepID)
	if err != nil {
		return fmt.Errorf("Checkpoint Path A: update step %q: %w", stepID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("Checkpoint Path A: step %q not found", stepID)
	}

	if attemptID.Valid {
		if _, err := tx.Exec(`
			UPDATE attempts
			SET status      = 'DONE',
			    resolved_at = now()
			WHERE attempt_id = $1::uuid
		`, attemptID.String); err != nil {
			return fmt.Errorf("Checkpoint Path A: update attempt: %w", err)
		}
	}

	return nil
}

// checkpointFailed implements Checkpoint Path B inside a transaction.
//
// INVARIANT: steps.output stays NULL.
// A FAILED step has not produced a valid result. Writing output here would
// cause LoadFrontier to classify it as DONE, feeding a failure payload to the
// planner as if it were a success. This is the primary correctness bug Path B
// must avoid — hence the explicit "no output write" and the comment below.
func checkpointFailed(tx *sql.Tx, stepID string, attemptID sql.NullString, r core.Result) error {
	// steps.output is intentionally NOT written — the NULL output column is the
	// "not done" signal that recovery and LoadFrontier rely on.
	res, err := tx.Exec(`
		UPDATE steps
		SET status = 'FAILED'
		WHERE step_id = $1
	`, stepID)
	if err != nil {
		return fmt.Errorf("Checkpoint Path B: update step %q: %w", stepID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("Checkpoint Path B: step %q not found", stepID)
	}

	if attemptID.Valid {
		if _, err := tx.Exec(`
			UPDATE attempts
			SET status      = 'FAILED',
			    error       = $1,
			    resolved_at = now()
			WHERE attempt_id = $2::uuid
		`, r.Error, attemptID.String); err != nil {
			return fmt.Errorf("Checkpoint Path B: update attempt: %w", err)
		}
	}

	return nil
}

// LoadFrontier reads the complete frontier for a run.
// Returns DONE steps as History (ordered by seq ASC) and the first
// DECIDED/RUNNING step with no output as PendingDecision.
//
// FAILED and DLQ steps are deliberately excluded:
//   - FAILED steps that will be retried will have their retry decision re-created
//     as DECIDED by the loop; that DECIDED row will appear as PendingDecision.
//   - DLQ steps are terminal and do not drive the next decision.
//
// This is the primary read for crash recovery and loop re-entry.
// It is NOT the status endpoint read — see DESIGN.md §9.4.
func (s *PostgresStore) LoadFrontier(run core.RunID) (core.Frontier, error) {
	rows, err := s.db.Query(`
		SELECT step_name, status, decision, output
		FROM steps
		WHERE run_id = $1
		ORDER BY seq ASC
	`, string(run))
	if err != nil {
		return core.Frontier{}, fmt.Errorf("LoadFrontier: query run %q: %w", run, err)
	}
	defer rows.Close()

	frontier := core.Frontier{RunID: run}

	for rows.Next() {
		var stepName, status string
		var decisionJSON, outputJSON []byte // NULL JSONB columns scan as nil []byte

		if err := rows.Scan(&stepName, &status, &decisionJSON, &outputJSON); err != nil {
			return core.Frontier{}, fmt.Errorf("LoadFrontier: scan row: %w", err)
		}

		switch {
		case status == "DONE" && outputJSON != nil:
			// Barrier 2 has fired. This step is complete and its output feeds the planner.
			frontier.History = append(frontier.History, core.HistoryEntry{
				Name:   stepName,
				Status: status,
				Output: json.RawMessage(outputJSON),
			})

		case (status == "DECIDED" || status == "RUNNING") && outputJSON == nil:
			// Barrier 1 fired but Barrier 2 did not. Re-dispatch without re-asking the planner.
			// Only the first such step is returned; a linear MVP run has at most one.
			if frontier.PendingDecision == nil && decisionJSON != nil {
				var spec core.StepSpec
				if err := json.Unmarshal(decisionJSON, &spec); err != nil {
					return core.Frontier{}, fmt.Errorf("LoadFrontier: unmarshal decision for %q: %w", stepName, err)
				}
				frontier.PendingDecision = &spec
			}

		// FAILED: output IS NULL and status is not DECIDED/RUNNING, so neither case above
		// matches. The step is deliberately invisible to LoadFrontier. The loop handles
		// retry/DLQ; on recovery the loop re-examines the step via direct query if needed.
		//
		// DLQ: terminal, never drives the next decision.
		}
	}

	if err := rows.Err(); err != nil {
		return core.Frontier{}, fmt.Errorf("LoadFrontier: rows iteration: %w", err)
	}

	return frontier, nil
}

// PendingDecision returns the first DECIDED/RUNNING step with no output for a run.
// Returns nil, nil when no such step exists (the run is ready for a new Decide call).
// This is the loop's fast path; LoadFrontier subsumes it for recovery.
func (s *PostgresStore) PendingDecision(run core.RunID) (*core.StepSpec, error) {
	var decisionJSON []byte

	err := s.db.QueryRow(`
		SELECT decision
		FROM steps
		WHERE run_id = $1
		  AND output IS NULL
		  AND status IN ('DECIDED', 'RUNNING')
		ORDER BY seq ASC
		LIMIT 1
	`, string(run)).Scan(&decisionJSON)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("PendingDecision: query run %q: %w", run, err)
	}
	if decisionJSON == nil {
		return nil, nil
	}

	var spec core.StepSpec
	if err := json.Unmarshal(decisionJSON, &spec); err != nil {
		return nil, fmt.Errorf("PendingDecision: unmarshal decision: %w", err)
	}
	return &spec, nil
}

// ResetToDecided transitions a step from FAILED back to DECIDED, clearing
// output and current_attempt_id. Called by the loop between retry attempts
// so that crash recovery can see the step as a pending decision and re-dispatch
// it rather than leaving it stuck in FAILED (which has no recovery rule).
func (s *PostgresStore) ResetToDecided(run core.RunID, step core.StepSpec) error {
	stepID := fmt.Sprintf("%s:%s", run, step.Name)

	res, err := s.db.Exec(`
		UPDATE steps
		SET status             = 'DECIDED',
		    output             = NULL,
		    current_attempt_id = NULL,
		    completed_at       = NULL
		WHERE step_id = $1
	`, stepID)
	if err != nil {
		return fmt.Errorf("ResetToDecided: update step %q: %w", stepID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ResetToDecided: step %q not found", stepID)
	}
	return nil
}

// MarkDLQ moves a step to DLQ status, inserts a dead_letter_queue row with
// full context, and marks the run FAILED. Called when RetryPolicy returns
// toDLQ=true (retries exhausted) or on a hard worker failure.
func (s *PostgresStore) MarkDLQ(run core.RunID, step core.StepSpec, reason string, lastError string) error {
	stepID := fmt.Sprintf("%s:%s", run, step.Name)

	contextJSON, _ := json.Marshal(map[string]any{
		"last_error": lastError,
		"step_name":  step.Name,
		"worker_url": step.WorkerURL,
	})

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("MarkDLQ: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec(`UPDATE steps SET status = 'DLQ' WHERE step_id = $1`, stepID); err != nil {
		return fmt.Errorf("MarkDLQ: update step %q: %w", stepID, err)
	}

	if _, err := tx.Exec(`
		INSERT INTO dead_letter_queue (run_id, step_id, reason, context)
		VALUES ($1, $2, $3, $4::jsonb)
	`, string(run), stepID, reason, string(contextJSON)); err != nil {
		return fmt.Errorf("MarkDLQ: insert dlq entry: %w", err)
	}

	if _, err := tx.Exec(`
		UPDATE runs SET status = 'FAILED', updated_at = now() WHERE run_id = $1
	`, string(run)); err != nil {
		return fmt.Errorf("MarkDLQ: mark run failed: %w", err)
	}

	return tx.Commit()
}

// MarkRunDone sets runs.status to DONE. Called by the loop when the planner
// returns status "done" (all steps completed successfully).
func (s *PostgresStore) MarkRunDone(run core.RunID) error {
	if _, err := s.db.Exec(`
		UPDATE runs SET status = 'DONE', updated_at = now() WHERE run_id = $1
	`, string(run)); err != nil {
		return fmt.Errorf("MarkRunDone: %w", err)
	}
	return nil
}

// MarkRunFailed sets runs.status to FAILED without a step DLQ entry.
// Used when the planner itself declares the run unworkable (status: "fail"),
// where no specific step is at fault.
func (s *PostgresStore) MarkRunFailed(run core.RunID, reason string) error {
	if _, err := s.db.Exec(`
		UPDATE runs SET status = 'FAILED', updated_at = now() WHERE run_id = $1
	`, string(run)); err != nil {
		return fmt.Errorf("MarkRunFailed: %w", err)
	}
	return nil
}

// MarkPlannerFailedDLQ writes a dead_letter_queue entry with reason='planner_failed'
// (step_id=NULL, because no specific step is at fault), then marks the run FAILED.
// Called by the loop when the planner returns status:"fail".
//
// The dead_letter_queue.step_id column is nullable for exactly this case.
func (s *PostgresStore) MarkPlannerFailedDLQ(run core.RunID, detail string) error {
	contextJSON, _ := json.Marshal(map[string]any{
		"detail": detail,
	})

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("MarkPlannerFailedDLQ: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec(`
		INSERT INTO dead_letter_queue (run_id, step_id, reason, context)
		VALUES ($1, NULL, 'planner_failed', $2::jsonb)
	`, string(run), string(contextJSON)); err != nil {
		return fmt.Errorf("MarkPlannerFailedDLQ: insert dlq: %w", err)
	}

	if _, err := tx.Exec(`
		UPDATE runs SET status = 'FAILED', updated_at = now() WHERE run_id = $1
	`, string(run)); err != nil {
		return fmt.Errorf("MarkPlannerFailedDLQ: mark run failed: %w", err)
	}

	return tx.Commit()
}
