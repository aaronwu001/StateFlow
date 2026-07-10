// Package store implements core.StateStore backed by PostgreSQL.
//
// Authoritative: docs/StateFlow_Whitepaper_v1_0.md §19 (the Atomic
// Transaction Ledger) and §14.1 (schema). Every TXn method below is one
// BEGIN...COMMIT, matching the ledger entry named in its doc comment on
// core.StateStore.
package store

import (
	"context"
	"crypto/rand"
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

// newUUID generates a random UUID v4 as a lowercase hyphenated string.
// No external dependency is added for this — crypto/rand plus the two
// version/variant bit twiddles required by RFC 4122 is the entire algorithm.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("newUUID: crypto/rand failed: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// jsonOrNull renders raw as a JSONB literal, substituting the JSON literal
// "null" for an empty/nil raw so NOT NULL JSONB columns are always satisfied
// with a well-defined value rather than a Go zero-length string.
func jsonOrNull(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}
	return string(raw)
}

// plannerConfigExtract pulls the typed convenience fields of core.WorkflowDef
// out of the opaque PlannerConfig JSON. These are never persisted as their
// own columns — the JSON is the single source of truth (whitepaper §12.1,
// Session 2 CONFIRM: retry_limit lives under this key inside planner_config).
type plannerConfigExtract struct {
	RetryLimit            *int `json:"retry_limit"`
	DefaultTimeoutSeconds *int `json:"default_timeout_seconds"`
}

// ── Setup ──

// CreateWorkflow persists a new workflow definition.
// Ledger TX-W — a single INSERT is already atomic; no explicit BEGIN needed.
func (s *PostgresStore) CreateWorkflow(ctx context.Context, def core.WorkflowDef) (core.WorkflowID, error) {
	id := core.WorkflowID("wf-" + newUUID())

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflows (workflow_id, name, planner_type, planner_config)
		VALUES ($1, $2, $3, $4::jsonb)
	`, string(id), def.Name, def.PlannerType, jsonOrNull(def.PlannerConfig))
	if err != nil {
		return "", fmt.Errorf("CreateWorkflow: insert: %w", err)
	}

	return id, nil
}

// GetWorkflow returns a workflow definition by id, parsing RetryLimit and
// DefaultTimeoutSeconds out of the stored planner_config JSON.
func (s *PostgresStore) GetWorkflow(ctx context.Context, workflow core.WorkflowID) (core.WorkflowDef, error) {
	var def core.WorkflowDef
	var plannerConfig []byte

	err := s.db.QueryRowContext(ctx, `
		SELECT name, planner_type, planner_config
		FROM workflows
		WHERE workflow_id = $1
	`, string(workflow)).Scan(&def.Name, &def.PlannerType, &plannerConfig)
	if err == sql.ErrNoRows {
		return core.WorkflowDef{}, fmt.Errorf("GetWorkflow: workflow %q not found", workflow)
	}
	if err != nil {
		return core.WorkflowDef{}, fmt.Errorf("GetWorkflow: query: %w", err)
	}
	def.PlannerConfig = json.RawMessage(plannerConfig)

	var extract plannerConfigExtract
	if err := json.Unmarshal(plannerConfig, &extract); err != nil {
		return core.WorkflowDef{}, fmt.Errorf("GetWorkflow: parse planner_config: %w", err)
	}
	if extract.RetryLimit != nil {
		def.RetryLimit = *extract.RetryLimit
	}
	if extract.DefaultTimeoutSeconds != nil {
		def.DefaultTimeoutSeconds = *extract.DefaultTimeoutSeconds
	}

	return def, nil
}

// CreateRun persists a new run in RUNNING state.
// Ledger TX0 — a single INSERT is already atomic; no explicit BEGIN needed.
func (s *PostgresStore) CreateRun(ctx context.Context, workflow core.WorkflowID, input json.RawMessage) (core.RunID, error) {
	id := core.RunID("run-" + newUUID())

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (run_id, workflow_id, status, workflow_input)
		VALUES ($1, $2, 'RUNNING', $3::jsonb)
	`, string(id), string(workflow), jsonOrNull(input))
	if err != nil {
		return "", fmt.Errorf("CreateRun: insert: %w", err)
	}

	return id, nil
}

// ── The two write barriers ──

// CreateStepWithAttempt is Barrier 1 (Ledger TX1): step + first attempt +
// current_attempt_id, all in one transaction, committed before any dispatch.
func (s *PostgresStore) CreateStepWithAttempt(ctx context.Context, run core.RunID, spec core.StepSpec) (core.StepID, core.AttemptID, error) {
	stepID := core.StepID(fmt.Sprintf("%s:%s", run, spec.Name))
	attemptID := core.AttemptID(newUUID())

	decisionJSON, err := json.Marshal(spec)
	if err != nil {
		return "", "", fmt.Errorf("CreateStepWithAttempt: marshal spec: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("CreateStepWithAttempt: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		WITH next_seq AS (
			SELECT COALESCE(MAX(seq), 0) + 1 AS seq FROM steps WHERE run_id = $1
		)
		INSERT INTO steps (step_id, run_id, step_name, seq, status, attempt_count, decision)
		SELECT $2, $1, $3, next_seq.seq, 'RUNNING', 0, $4::jsonb
		FROM next_seq
	`, string(run), string(stepID), spec.Name, string(decisionJSON)); err != nil {
		return "", "", fmt.Errorf("CreateStepWithAttempt: insert step %q: %w", stepID, err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO attempts (attempt_id, step_id, status)
		VALUES ($1::uuid, $2, 'RUNNING')
	`, string(attemptID), string(stepID)); err != nil {
		return "", "", fmt.Errorf("CreateStepWithAttempt: insert attempt: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE steps SET current_attempt_id = $1::uuid WHERE step_id = $2
	`, string(attemptID), string(stepID)); err != nil {
		return "", "", fmt.Errorf("CreateStepWithAttempt: set current_attempt_id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("CreateStepWithAttempt: commit: %w", err)
	}

	return stepID, attemptID, nil
}

// CheckpointSuccess is Barrier 2 (Ledger TX2). CAS-A: the attempt UPDATE
// gates on both attempt_id+status='RUNNING' and steps.current_attempt_id
// matching — a report matching nothing is ReportSuperseded, never an error.
func (s *PostgresStore) CheckpointSuccess(ctx context.Context, step core.StepID, attempt core.AttemptID, output json.RawMessage) (core.ReportOutcome, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.ReportCommitted, fmt.Errorf("CheckpointSuccess: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.ExecContext(ctx, `
		UPDATE attempts
		SET status = 'DONE', resolved_at = now()
		WHERE attempt_id = $1::uuid
		  AND status = 'RUNNING'
		  AND EXISTS (
		      SELECT 1 FROM steps
		      WHERE step_id = $2 AND current_attempt_id = $1::uuid
		  )
	`, string(attempt), string(step))
	if err != nil {
		return core.ReportCommitted, fmt.Errorf("CheckpointSuccess: CAS update attempt: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.ReportSuperseded, nil
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE steps
		SET output = $1::jsonb, status = 'DONE', completed_at = now()
		WHERE step_id = $2
	`, jsonOrNull(output), string(step)); err != nil {
		return core.ReportCommitted, fmt.Errorf("CheckpointSuccess: update step %q: %w", step, err)
	}

	if err := tx.Commit(); err != nil {
		return core.ReportCommitted, fmt.Errorf("CheckpointSuccess: commit: %w", err)
	}

	return core.ReportCommitted, nil
}

// ── Worker-side failure, retry, and budget ──

// attemptFailureDetail is one row of the per-attempt context aggregated into
// a dead_letter_queue.context blob when RecordFailure's DLQ blade fires.
type attemptFailureDetail struct {
	AttemptID     string  `json:"attempt_id"`
	Status        string  `json:"status"`
	FailureReason *string `json:"failure_reason,omitempty"`
	Error         *string `json:"error,omitempty"`
}

// buildStepFailureContext aggregates every attempt of stepID (within tx) into
// the JSON blob stored as dead_letter_queue.context for a worker-side entry.
func (s *PostgresStore) buildStepFailureContext(ctx context.Context, tx *sql.Tx, stepID, detail string) (json.RawMessage, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT attempt_id::text, status, failure_reason, error
		FROM attempts
		WHERE step_id = $1
		ORDER BY created_at ASC
	`, stepID)
	if err != nil {
		return nil, fmt.Errorf("buildStepFailureContext: query attempts: %w", err)
	}
	defer rows.Close()

	var attempts []attemptFailureDetail
	for rows.Next() {
		var a attemptFailureDetail
		var reason, errMsg sql.NullString
		if err := rows.Scan(&a.AttemptID, &a.Status, &reason, &errMsg); err != nil {
			return nil, fmt.Errorf("buildStepFailureContext: scan: %w", err)
		}
		if reason.Valid {
			a.FailureReason = &reason.String
		}
		if errMsg.Valid {
			a.Error = &errMsg.String
		}
		attempts = append(attempts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("buildStepFailureContext: rows: %w", err)
	}

	payload := struct {
		StepID   string                 `json:"step_id"`
		Detail   string                 `json:"detail"`
		Attempts []attemptFailureDetail `json:"attempts"`
	}{StepID: stepID, Detail: detail, Attempts: attempts}

	out, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("buildStepFailureContext: marshal: %w", err)
	}
	return out, nil
}

// RecordFailure is Ledger TX3. One method for all four FailureReason values
// (worker_reported/timeout/malformed/orphaned) — the same CAS gate, the same
// attempt_count++ , and the same same-transaction DLQ blade at count==retryLimit.
func (s *PostgresStore) RecordFailure(ctx context.Context, step core.StepID, attempt core.AttemptID, reason core.FailureReason, detail string, retryLimit int) (core.FailureOutcome, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.FailureOutcome{}, fmt.Errorf("RecordFailure: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.ExecContext(ctx, `
		UPDATE attempts
		SET status = 'FAILED', failure_reason = $1, error = $2, resolved_at = now()
		WHERE attempt_id = $3::uuid
		  AND status = 'RUNNING'
		  AND EXISTS (
		      SELECT 1 FROM steps
		      WHERE step_id = $4 AND current_attempt_id = $3::uuid
		  )
	`, string(reason), detail, string(attempt), string(step))
	if err != nil {
		return core.FailureOutcome{}, fmt.Errorf("RecordFailure: CAS update attempt: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.FailureOutcome{Report: core.ReportSuperseded}, nil
	}

	var newCount int
	var runID string
	if err := tx.QueryRowContext(ctx, `
		UPDATE steps SET attempt_count = attempt_count + 1
		WHERE step_id = $1
		RETURNING attempt_count, run_id
	`, string(step)).Scan(&newCount, &runID); err != nil {
		return core.FailureOutcome{}, fmt.Errorf("RecordFailure: increment attempt_count: %w", err)
	}

	outcome := core.FailureOutcome{Report: core.ReportCommitted}

	if newCount >= retryLimit {
		if _, err := tx.ExecContext(ctx, `UPDATE steps SET status = 'DLQ' WHERE step_id = $1`, string(step)); err != nil {
			return core.FailureOutcome{}, fmt.Errorf("RecordFailure: dlq step: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = 'DLQ', updated_at = now() WHERE run_id = $1`, runID); err != nil {
			return core.FailureOutcome{}, fmt.Errorf("RecordFailure: dlq run: %w", err)
		}

		contextJSON, err := s.buildStepFailureContext(ctx, tx, string(step), detail)
		if err != nil {
			return core.FailureOutcome{}, fmt.Errorf("RecordFailure: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO dead_letter_queue (run_id, step_id, reason, context)
			VALUES ($1, $2, 'worker_retry_exhausted', $3::jsonb)
		`, runID, string(step), string(contextJSON)); err != nil {
			return core.FailureOutcome{}, fmt.Errorf("RecordFailure: insert dlq row: %w", err)
		}

		outcome.DLQed = true
	}

	if err := tx.Commit(); err != nil {
		return core.FailureOutcome{}, fmt.Errorf("RecordFailure: commit: %w", err)
	}

	return outcome, nil
}

// StartNewAttempt is Ledger TX4: a new attempt plus the current_attempt_id
// pointer move, in one transaction — no instant with two valid attempt ids.
func (s *PostgresStore) StartNewAttempt(ctx context.Context, step core.StepID) (core.AttemptID, error) {
	attemptID := core.AttemptID(newUUID())

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("StartNewAttempt: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO attempts (attempt_id, step_id, status)
		VALUES ($1::uuid, $2, 'RUNNING')
	`, string(attemptID), string(step)); err != nil {
		return "", fmt.Errorf("StartNewAttempt: insert attempt: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE steps SET current_attempt_id = $1::uuid WHERE step_id = $2
	`, string(attemptID), string(step))
	if err != nil {
		return "", fmt.Errorf("StartNewAttempt: update current_attempt_id: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", fmt.Errorf("StartNewAttempt: step %q not found", step)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("StartNewAttempt: commit: %w", err)
	}

	return attemptID, nil
}

// ── Replay (operator-triggered, from the DLQ) ──

// ReplayWorkerSide is Ledger TX5: resets the run's single worker-side DLQ
// step for retry — attempt_count→0, step→RUNNING, run→RUNNING, a new attempt,
// current_attempt_id set — all five writes in one transaction.
func (s *PostgresStore) ReplayWorkerSide(ctx context.Context, run core.RunID) (core.StepID, core.AttemptID, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("ReplayWorkerSide: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var stepID string
	err = tx.QueryRowContext(ctx, `
		SELECT step_id FROM steps WHERE run_id = $1 AND status = 'DLQ'
	`, string(run)).Scan(&stepID)
	if err == sql.ErrNoRows {
		return "", "", fmt.Errorf("ReplayWorkerSide: no DLQ step for run %q", run)
	}
	if err != nil {
		return "", "", fmt.Errorf("ReplayWorkerSide: find dlq step: %w", err)
	}

	attemptID := core.AttemptID(newUUID())

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO attempts (attempt_id, step_id, status)
		VALUES ($1::uuid, $2, 'RUNNING')
	`, string(attemptID), stepID); err != nil {
		return "", "", fmt.Errorf("ReplayWorkerSide: insert attempt: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE steps
		SET attempt_count = 0, status = 'RUNNING', current_attempt_id = $1::uuid, completed_at = NULL
		WHERE step_id = $2
	`, string(attemptID), stepID); err != nil {
		return "", "", fmt.Errorf("ReplayWorkerSide: reset step: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = 'RUNNING', updated_at = now() WHERE run_id = $1
	`, string(run)); err != nil {
		return "", "", fmt.Errorf("ReplayWorkerSide: reset run: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("ReplayWorkerSide: commit: %w", err)
	}

	return core.StepID(stepID), attemptID, nil
}

// ReplayPlannerSide is Ledger TX6: run→RUNNING only. A single UPDATE is
// already atomic; no explicit BEGIN needed.
func (s *PostgresStore) ReplayPlannerSide(ctx context.Context, run core.RunID) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = 'RUNNING', updated_at = now() WHERE run_id = $1
	`, string(run))
	if err != nil {
		return fmt.Errorf("ReplayPlannerSide: update run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ReplayPlannerSide: run %q not found", run)
	}
	return nil
}

// ── Run terminal states ──

// MarkRunDone is Ledger TX7. A single UPDATE is already atomic.
func (s *PostgresStore) MarkRunDone(ctx context.Context, run core.RunID) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = 'DONE', updated_at = now() WHERE run_id = $1
	`, string(run))
	if err != nil {
		return fmt.Errorf("MarkRunDone: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("MarkRunDone: run %q not found", run)
	}
	return nil
}

// MarkRunDLQPlannerDeclared is Ledger TX8: run→DLQ + a step_id-NULL DLQ row,
// reason planner_declared_fail, in one transaction.
func (s *PostgresStore) MarkRunDLQPlannerDeclared(ctx context.Context, run core.RunID, detail string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("MarkRunDLQPlannerDeclared: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	contextJSON, err := json.Marshal(map[string]string{"detail": detail})
	if err != nil {
		return fmt.Errorf("MarkRunDLQPlannerDeclared: marshal context: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO dead_letter_queue (run_id, step_id, reason, context)
		VALUES ($1, NULL, 'planner_declared_fail', $2::jsonb)
	`, string(run), string(contextJSON)); err != nil {
		return fmt.Errorf("MarkRunDLQPlannerDeclared: insert dlq row: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = 'DLQ', updated_at = now() WHERE run_id = $1
	`, string(run))
	if err != nil {
		return fmt.Errorf("MarkRunDLQPlannerDeclared: update run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("MarkRunDLQPlannerDeclared: run %q not found", run)
	}

	return tx.Commit()
}

// MarkRunDLQPlannerExhausted is Ledger TX9: run→DLQ + a step_id-NULL DLQ row
// with reason planner_unreachable or planner_malformed, in one transaction.
func (s *PostgresStore) MarkRunDLQPlannerExhausted(ctx context.Context, run core.RunID, reason core.DLQReason, detail string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("MarkRunDLQPlannerExhausted: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	contextJSON, err := json.Marshal(map[string]string{"detail": detail})
	if err != nil {
		return fmt.Errorf("MarkRunDLQPlannerExhausted: marshal context: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO dead_letter_queue (run_id, step_id, reason, context)
		VALUES ($1, NULL, $2, $3::jsonb)
	`, string(run), string(reason), string(contextJSON)); err != nil {
		return fmt.Errorf("MarkRunDLQPlannerExhausted: insert dlq row: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = 'DLQ', updated_at = now() WHERE run_id = $1
	`, string(run))
	if err != nil {
		return fmt.Errorf("MarkRunDLQPlannerExhausted: update run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("MarkRunDLQPlannerExhausted: run %q not found", run)
	}

	return tx.Commit()
}

// ── Reads ──

// LoadFrontier returns the full frontier for the loop's fast path and for
// crash recovery in a single read: DONE steps as History (seq ASC), and the
// single RUNNING step (if any) as PendingStep/PendingAttemptID/AttemptCount.
// DLQ steps are terminal and never drive the next decision.
func (s *PostgresStore) LoadFrontier(ctx context.Context, run core.RunID) (core.Frontier, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT step_name, status, decision, output, attempt_count, current_attempt_id::text
		FROM steps
		WHERE run_id = $1
		ORDER BY seq ASC
	`, string(run))
	if err != nil {
		return core.Frontier{}, fmt.Errorf("LoadFrontier: query: %w", err)
	}
	defer rows.Close()

	frontier := core.Frontier{RunID: run}

	for rows.Next() {
		var stepName, status string
		var decisionJSON, outputJSON []byte
		var attemptCount int
		var currentAttemptID sql.NullString

		if err := rows.Scan(&stepName, &status, &decisionJSON, &outputJSON, &attemptCount, &currentAttemptID); err != nil {
			return core.Frontier{}, fmt.Errorf("LoadFrontier: scan: %w", err)
		}

		switch status {
		case "DONE":
			frontier.History = append(frontier.History, core.HistoryEntry{
				Name:   stepName,
				Status: core.StepStatus(status),
				Output: json.RawMessage(outputJSON),
			})
		case "RUNNING":
			var spec core.StepSpec
			if decisionJSON != nil {
				if err := json.Unmarshal(decisionJSON, &spec); err != nil {
					return core.Frontier{}, fmt.Errorf("LoadFrontier: unmarshal decision for %q: %w", stepName, err)
				}
			}
			frontier.PendingStep = &spec
			frontier.AttemptCount = attemptCount
			if currentAttemptID.Valid {
				frontier.PendingAttemptID = core.AttemptID(currentAttemptID.String)
			}
			// DLQ: terminal, never drives the next decision.
		}
	}
	if err := rows.Err(); err != nil {
		return core.Frontier{}, fmt.Errorf("LoadFrontier: rows: %w", err)
	}

	return frontier, nil
}

// ListRunningRuns returns a reference for every run with status RUNNING.
// Used once at startup by crash recovery to find runs to resume.
func (s *PostgresStore) ListRunningRuns(ctx context.Context) ([]core.RunRef, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, workflow_id, workflow_input FROM runs WHERE status = 'RUNNING'
	`)
	if err != nil {
		return nil, fmt.Errorf("ListRunningRuns: query: %w", err)
	}
	defer rows.Close()

	var refs []core.RunRef
	for rows.Next() {
		var runID, workflowID string
		var input json.RawMessage
		if err := rows.Scan(&runID, &workflowID, &input); err != nil {
			return nil, fmt.Errorf("ListRunningRuns: scan row: %w", err)
		}
		refs = append(refs, core.RunRef{
			RunID:         core.RunID(runID),
			WorkflowID:    core.WorkflowID(workflowID),
			WorkflowInput: input,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListRunningRuns: rows iteration: %w", err)
	}

	return refs, nil
}

// GetRun returns a run's current status, its steps, and their attempt
// summaries, for the status API.
func (s *PostgresStore) GetRun(ctx context.Context, run core.RunID) (core.RunSummary, error) {
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id = $1`, string(run)).Scan(&status)
	if err == sql.ErrNoRows {
		return core.RunSummary{}, fmt.Errorf("GetRun: run %q not found", run)
	}
	if err != nil {
		return core.RunSummary{}, fmt.Errorf("GetRun: query run: %w", err)
	}

	summary := core.RunSummary{RunID: run, Status: core.RunStatus(status)}

	stepRows, err := s.db.QueryContext(ctx, `
		SELECT step_id, step_name, status FROM steps WHERE run_id = $1 ORDER BY seq ASC
	`, string(run))
	if err != nil {
		return core.RunSummary{}, fmt.Errorf("GetRun: query steps: %w", err)
	}

	var stepIDs []string
	for stepRows.Next() {
		var stepID, stepName, stepStatus string
		if err := stepRows.Scan(&stepID, &stepName, &stepStatus); err != nil {
			stepRows.Close()
			return core.RunSummary{}, fmt.Errorf("GetRun: scan step: %w", err)
		}
		summary.Steps = append(summary.Steps, core.StepSummary{
			StepID: core.StepID(stepID),
			Name:   stepName,
			Status: core.StepStatus(stepStatus),
		})
		stepIDs = append(stepIDs, stepID)
	}
	if err := stepRows.Err(); err != nil {
		stepRows.Close()
		return core.RunSummary{}, fmt.Errorf("GetRun: step rows: %w", err)
	}
	stepRows.Close()

	for i, stepID := range stepIDs {
		attemptRows, err := s.db.QueryContext(ctx, `
			SELECT attempt_id::text, status, failure_reason
			FROM attempts WHERE step_id = $1 ORDER BY created_at ASC
		`, stepID)
		if err != nil {
			return core.RunSummary{}, fmt.Errorf("GetRun: query attempts for %q: %w", stepID, err)
		}
		for attemptRows.Next() {
			var attemptID, attemptStatus string
			var reason sql.NullString
			if err := attemptRows.Scan(&attemptID, &attemptStatus, &reason); err != nil {
				attemptRows.Close()
				return core.RunSummary{}, fmt.Errorf("GetRun: scan attempt: %w", err)
			}
			as := core.AttemptSummary{AttemptID: core.AttemptID(attemptID), Status: core.AttemptStatus(attemptStatus)}
			if reason.Valid {
				r := core.FailureReason(reason.String)
				as.Reason = &r
			}
			summary.Steps[i].Attempts = append(summary.Steps[i].Attempts, as)
		}
		if err := attemptRows.Err(); err != nil {
			attemptRows.Close()
			return core.RunSummary{}, fmt.Errorf("GetRun: attempt rows: %w", err)
		}
		attemptRows.Close()
	}

	if summary.Status == core.RunStatusDLQ {
		var reason string
		err := s.db.QueryRowContext(ctx, `
			SELECT reason FROM dead_letter_queue WHERE run_id = $1 ORDER BY created_at DESC LIMIT 1
		`, string(run)).Scan(&reason)
		if err != nil && err != sql.ErrNoRows {
			return core.RunSummary{}, fmt.Errorf("GetRun: query dlq reason: %w", err)
		}
		if err == nil {
			r := core.DLQReason(reason)
			summary.DLQReason = &r
		}
	}

	return summary, nil
}

// ListDLQ returns every dead-letter-queue entry, for the DLQ listing API.
func (s *PostgresStore) ListDLQ(ctx context.Context) ([]core.DLQEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, step_id, reason, context FROM dead_letter_queue ORDER BY id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("ListDLQ: query: %w", err)
	}
	defer rows.Close()

	var entries []core.DLQEntry
	for rows.Next() {
		var id int64
		var runID, reason string
		var stepID sql.NullString
		var contextJSON []byte
		if err := rows.Scan(&id, &runID, &stepID, &reason, &contextJSON); err != nil {
			return nil, fmt.Errorf("ListDLQ: scan: %w", err)
		}
		e := core.DLQEntry{
			ID:      core.DLQEntryID(id),
			RunID:   core.RunID(runID),
			Reason:  core.DLQReason(reason),
			Context: json.RawMessage(contextJSON),
		}
		if stepID.Valid {
			sid := core.StepID(stepID.String)
			e.StepID = &sid
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListDLQ: rows: %w", err)
	}
	return entries, nil
}

// GetDLQEntry returns a single dead-letter-queue entry by id, so the replay
// handler can tell worker-side (StepID != nil → TX5) from planner-side
// (StepID == nil → TX6).
func (s *PostgresStore) GetDLQEntry(ctx context.Context, id core.DLQEntryID) (core.DLQEntry, error) {
	var runID, reason string
	var stepID sql.NullString
	var contextJSON []byte

	err := s.db.QueryRowContext(ctx, `
		SELECT run_id, step_id, reason, context FROM dead_letter_queue WHERE id = $1
	`, int64(id)).Scan(&runID, &stepID, &reason, &contextJSON)
	if err == sql.ErrNoRows {
		return core.DLQEntry{}, fmt.Errorf("GetDLQEntry: entry %d not found", id)
	}
	if err != nil {
		return core.DLQEntry{}, fmt.Errorf("GetDLQEntry: query: %w", err)
	}

	e := core.DLQEntry{
		ID:      id,
		RunID:   core.RunID(runID),
		Reason:  core.DLQReason(reason),
		Context: json.RawMessage(contextJSON),
	}
	if stepID.Valid {
		sid := core.StepID(stepID.String)
		e.StepID = &sid
	}
	return e, nil
}
