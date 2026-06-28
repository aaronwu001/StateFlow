package orchestrator_test

// Integration tests for the orchestrator loop's retry and DLQ behaviour.
// These tests write to a real PostgreSQL database and skip if TEST_DATABASE_URL
// is not set. All DLQ writes are verified by querying the DB directly.
//
// Stubs: planner and transport are in-process (no HTTP); only the store is real.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/orchestrator"
	"github.com/aaronwu000/stateflow/internal/store"
)

// ─── DB helpers (mirrors internal/store/postgres_test.go) ───────────────────

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("db.Ping: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func resetTestSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, tbl := range []string{"dead_letter_queue", "attempts", "steps", "runs", "workflows"} {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + tbl + " CASCADE"); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
	ddl, err := os.ReadFile("../../migrations/001_initial.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
}

func seedTestFixtures(t *testing.T, db *sql.DB, workflowID, runID string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO workflows (workflow_id, name, planner_type, planner_config)
		VALUES ($1, 'test-workflow', 'static', '{}')
	`, workflowID); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO runs (run_id, workflow_id, status, workflow_input)
		VALUES ($1, $2, 'RUNNING', '{}')
	`, runID, workflowID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

// ─── Stub types ──────────────────────────────────────────────────────────────

// countFailsThenSucceed returns failed for the first maxFails dispatches, then done.
type countFailsThenSucceed struct {
	calls    atomic.Int64
	maxFails int64
}

func (t *countFailsThenSucceed) Dispatch(_ context.Context, _ core.StepSpec) (core.Result, error) {
	n := t.calls.Add(1)
	if n <= t.maxFails {
		return core.Result{Status: "failed", Error: fmt.Sprintf("attempt %d failed", n)}, nil
	}
	return core.Result{Status: "done", Output: json.RawMessage(`{"ok":true}`)}, nil
}

// alwaysFailTransport always returns failed.
type alwaysFailTransport struct {
	calls atomic.Int64
}

func (t *alwaysFailTransport) Dispatch(_ context.Context, _ core.StepSpec) (core.Result, error) {
	n := t.calls.Add(1)
	return core.Result{Status: "failed", Error: fmt.Sprintf("persistent failure on attempt %d", n)}, nil
}

// singleStepPlanner returns one step on the first call, then "done".
type singleStepPlanner struct {
	step     *core.StepSpec
	returned bool
}

func (p *singleStepPlanner) Decide(_ context.Context, state core.RunState) (core.StepDecision, error) {
	if len(state.History) > 0 || p.returned {
		return core.StepDecision{Status: "done"}, nil
	}
	p.returned = true
	return core.StepDecision{Status: "continue", Step: p.step}, nil
}

// failPlanner always returns "fail" immediately.
type failPlanner struct{}

func (p *failPlanner) Decide(_ context.Context, _ core.RunState) (core.StepDecision, error) {
	return core.StepDecision{Status: "fail"}, nil
}

// ─── Test 1: retry exhausted → DLQ ──────────────────────────────────────────

// TestLoop_RetryExhausted_WritesDLQ runs a loop with a transport that always
// fails. After MaxRetries=3 attempts, the step must be in DLQ and the DLQ
// table must have a row with reason='retry_exhausted' containing the last error.
func TestLoop_RetryExhausted_WritesDLQ(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		workflowID = "wf-retry-exhausted"
		runID      = "run-retry-exhausted"
	)
	seedTestFixtures(t, db, workflowID, runID)

	s := store.New(db)
	step := &core.StepSpec{
		Name:           "extract",
		WorkerURL:      "http://stub/extract",
		Mode:           "sync",
		TimeoutSeconds: 5,
		Input:          json.RawMessage(`{}`),
	}

	transport := &alwaysFailTransport{}
	loop := &orchestrator.Loop{
		RunID:         core.RunID(runID),
		WorkflowInput: json.RawMessage(`{}`),
		Store:         s,
		Planner:       &singleStepPlanner{step: step},
		Transport:     transport,
		Retry:         &orchestrator.FixedCountPolicy{MaxRetries: 3, Delay: 0},
	}

	err := loop.Run(context.Background())
	if err == nil {
		t.Fatal("Run() should return error when step is exhausted")
	}
	t.Logf("Run() returned expected error: %v", err)

	// ── Assert: exactly 3 attempt rows ──────────────────────────────────────
	var attemptCount int
	stepID := fmt.Sprintf("%s:extract", runID)
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE step_id = $1`, stepID).
		Scan(&attemptCount); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attemptCount != 3 {
		t.Errorf("attempts table has %d rows, want 3 (one per dispatch)", attemptCount)
	}
	t.Logf("PASS — %d attempt rows in DB (MaxRetries=3)", attemptCount)

	// ── Assert: step.status = DLQ ───────────────────────────────────────────
	var stepStatus string
	if err := db.QueryRow(`SELECT status FROM steps WHERE step_id = $1`, stepID).
		Scan(&stepStatus); err != nil {
		t.Fatalf("query step status: %v", err)
	}
	if stepStatus != "DLQ" {
		t.Errorf("step.status = %q, want DLQ", stepStatus)
	}
	t.Logf("PASS — step.status = DLQ")

	// ── Assert: runs.status = FAILED ────────────────────────────────────────
	var runStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE run_id = $1`, runID).
		Scan(&runStatus); err != nil {
		t.Fatalf("query run status: %v", err)
	}
	if runStatus != "FAILED" {
		t.Errorf("run.status = %q, want FAILED", runStatus)
	}
	t.Logf("PASS — run.status = FAILED")

	// ── Assert: dead_letter_queue has one row with reason=retry_exhausted ───
	var dlqReason string
	var dlqContext []byte
	if err := db.QueryRow(`
		SELECT reason, context FROM dead_letter_queue WHERE run_id = $1
	`, runID).Scan(&dlqReason, &dlqContext); err != nil {
		t.Fatalf("query dlq: %v", err)
	}
	if dlqReason != "retry_exhausted" {
		t.Errorf("dlq.reason = %q, want retry_exhausted", dlqReason)
	}
	t.Logf("PASS — DLQ entry: reason=%q", dlqReason)

	// ── Assert: DLQ context contains last_error ──────────────────────────────
	var ctx map[string]any
	if err := json.Unmarshal(dlqContext, &ctx); err != nil {
		t.Fatalf("unmarshal dlq context: %v", err)
	}
	lastErr, _ := ctx["last_error"].(string)
	if lastErr == "" {
		t.Errorf("dlq context missing last_error; got: %s", dlqContext)
	}
	t.Logf("PASS — DLQ context has last_error=%q", lastErr)
}

// ─── Test 2: retry then succeed ─────────────────────────────────────────────

// TestLoop_RetryThenSucceed verifies that after 2 failures, the 3rd attempt
// succeeds: the run completes normally with no DLQ entry and run.status=DONE.
func TestLoop_RetryThenSucceed(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		workflowID = "wf-retry-succeed"
		runID      = "run-retry-succeed"
	)
	seedTestFixtures(t, db, workflowID, runID)

	s := store.New(db)
	step := &core.StepSpec{
		Name:           "process",
		WorkerURL:      "http://stub/process",
		Mode:           "sync",
		TimeoutSeconds: 5,
		Input:          json.RawMessage(`{}`),
	}

	transport := &countFailsThenSucceed{maxFails: 2}
	loop := &orchestrator.Loop{
		RunID:         core.RunID(runID),
		WorkflowInput: json.RawMessage(`{}`),
		Store:         s,
		Planner:       &singleStepPlanner{step: step},
		Transport:     transport,
		Retry:         &orchestrator.FixedCountPolicy{MaxRetries: 3, Delay: 0},
	}

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	// ── Assert: 3 attempt rows ───────────────────────────────────────────────
	stepID := fmt.Sprintf("%s:process", runID)
	var attemptCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE step_id = $1`, stepID).
		Scan(&attemptCount); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attemptCount != 3 {
		t.Errorf("attempts = %d, want 3 (2 failed + 1 done)", attemptCount)
	}
	t.Logf("PASS — %d attempts (2 fail + 1 done)", attemptCount)

	// ── Assert: no DLQ entry ─────────────────────────────────────────────────
	var dlqCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM dead_letter_queue WHERE run_id = $1`, runID).
		Scan(&dlqCount); err != nil {
		t.Fatalf("count dlq: %v", err)
	}
	if dlqCount != 0 {
		t.Errorf("DLQ has %d entries, want 0 (retry succeeded)", dlqCount)
	}
	t.Log("PASS — no DLQ entry (step succeeded on 3rd attempt)")

	// ── Assert: run.status = DONE ────────────────────────────────────────────
	var runStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE run_id = $1`, runID).
		Scan(&runStatus); err != nil {
		t.Fatalf("query run status: %v", err)
	}
	if runStatus != "DONE" {
		t.Errorf("run.status = %q, want DONE", runStatus)
	}
	t.Logf("PASS — run.status = DONE")

	// ── Assert: transport called exactly 3 times ─────────────────────────────
	if n := transport.calls.Load(); n != 3 {
		t.Errorf("transport called %d times, want 3", n)
	}
	t.Logf("PASS — transport dispatched %d times", transport.calls.Load())
}

// ─── Test 3: planner declares failure → DLQ(planner_failed) ─────────────────

// TestLoop_PlannerFail_WritesDLQ verifies that when the planner returns
// status:"fail", the loop writes a DLQ entry with reason='planner_failed'
// (step_id NULL, since no step is at fault) and marks the run FAILED.
func TestLoop_PlannerFail_WritesDLQ(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		workflowID = "wf-planner-fail"
		runID      = "run-planner-fail"
	)
	seedTestFixtures(t, db, workflowID, runID)

	s := store.New(db)
	loop := &orchestrator.Loop{
		RunID:         core.RunID(runID),
		WorkflowInput: json.RawMessage(`{}`),
		Store:         s,
		Planner:       &failPlanner{},
		Transport:     &alwaysFailTransport{},
		Retry:         &orchestrator.FixedCountPolicy{MaxRetries: 3, Delay: 0},
	}

	err := loop.Run(context.Background())
	if err == nil {
		t.Fatal("Run() should return error for planner failure")
	}
	t.Logf("Run() returned expected error: %v", err)

	// ── Assert: run.status = FAILED ──────────────────────────────────────────
	var runStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE run_id = $1`, runID).
		Scan(&runStatus); err != nil {
		t.Fatalf("query run status: %v", err)
	}
	if runStatus != "FAILED" {
		t.Errorf("run.status = %q, want FAILED", runStatus)
	}
	t.Logf("PASS — run.status = FAILED")

	// ── Assert: DLQ entry with reason=planner_failed, step_id IS NULL ────────
	var dlqReason string
	var dlqStepID sql.NullString
	if err := db.QueryRow(`
		SELECT reason, step_id FROM dead_letter_queue WHERE run_id = $1
	`, runID).Scan(&dlqReason, &dlqStepID); err != nil {
		t.Fatalf("query dlq: %v", err)
	}
	if dlqReason != "planner_failed" {
		t.Errorf("dlq.reason = %q, want planner_failed", dlqReason)
	}
	if dlqStepID.Valid {
		t.Errorf("dlq.step_id = %q, want NULL (no specific step to blame)", dlqStepID.String)
	}
	t.Logf("PASS — DLQ entry: reason=%q, step_id IS NULL", dlqReason)

	// ── Assert: no step rows (planner failed before any step was decided) ─────
	var stepCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM steps WHERE run_id = $1`, runID).
		Scan(&stepCount); err != nil {
		t.Fatalf("count steps: %v", err)
	}
	if stepCount != 0 {
		t.Errorf("steps table has %d rows, want 0 (planner failed before first step)", stepCount)
	}
	t.Logf("PASS — steps table empty (planner failed before any dispatch)")

	// ── Assert: no attempts (no dispatch happened) ────────────────────────────
	var attemptCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempts`, ).
		Scan(&attemptCount); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attemptCount != 0 {
		t.Errorf("attempts table has %d rows, want 0", attemptCount)
	}
	t.Logf("PASS — no dispatch happened before planner failure")
}

// Suppress "declared but not used" for the time package in case test runs fast.
var _ = time.Second
