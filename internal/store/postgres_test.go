package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/store"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// testAttemptID is a stable UUID used across Path B tests.
const testAttemptID = core.AttemptID("550e8400-e29b-41d4-a716-446655440001")

// openDB opens a *sql.DB from TEST_DATABASE_URL, or skips the test if not set.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping postgres integration test")
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

// resetSchema drops all StateFlow tables (if they exist) and re-applies the
// migration. Keeps the production DDL pure; test isolation is the test's job.
func resetSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	// Drop in reverse FK order; CASCADE removes dependent indexes automatically.
	for _, tbl := range []string{"dead_letter_queue", "attempts", "steps", "runs", "workflows"} {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + tbl + " CASCADE"); err != nil {
			t.Fatalf("drop table %s: %v", tbl, err)
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

// seedFixtures inserts the minimum FK-satisfying rows: one workflow, one run.
func seedFixtures(t *testing.T, db *sql.DB, workflowID, runID string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO workflows (workflow_id, name, planner_type, planner_config)
		VALUES ($1, 'test-workflow', 'static', '{}')
	`, workflowID)
	if err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO runs (run_id, workflow_id, status, workflow_input)
		VALUES ($1, $2, 'RUNNING', '{}')
	`, runID, workflowID)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

// TestBarrierInvariant verifies the core physical signal that makes StateFlow's
// crash-recovery model work (whitepaper §2.2, DESIGN.md §4):
//
//   decision non-null, output null  →  PendingDecision non-nil  (re-dispatch)
//   output non-null                 →  step in History           (done, ask planner)
//
// Phase 1 proves Barrier 1: PutDecision writes decision; LoadFrontier sees it as
// a pending re-dispatch (output is still NULL).
// Phase 2 proves Barrier 2: Checkpoint writes output; LoadFrontier sees the step
// as DONE in History, PendingDecision is nil.
func TestBarrierInvariant(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)

	const (
		workflowID = "wf-barrier-test"
		runID      = "run-barrier-test"
	)
	seedFixtures(t, db, workflowID, runID)

	s := store.New(db)
	run := core.RunID(runID)

	step := core.StepSpec{
		Name:           "ocr",
		WorkerURL:      "http://ocr-service/run",
		Mode:           "async",
		TimeoutSeconds: 30,
		Input:          json.RawMessage(`{"doc": "test.pdf"}`),
	}

	// ── Phase 1: Barrier 1 (PutDecision) ────────────────────────────────────
	// After PutDecision: decision column has value, output column is NULL.
	// LoadFrontier must return PendingDecision = &step, History empty.

	if err := s.PutDecision(run, step); err != nil {
		t.Fatalf("PutDecision: %v", err)
	}

	f1, err := s.LoadFrontier(run)
	if err != nil {
		t.Fatalf("LoadFrontier after PutDecision: %v", err)
	}

	if f1.PendingDecision == nil {
		t.Fatal("FAIL — Phase 1: PendingDecision is nil; " +
			"expected non-nil because decision is present but output is NULL")
	}
	if f1.PendingDecision.Name != step.Name {
		t.Fatalf("FAIL — Phase 1: PendingDecision.Name = %q, want %q",
			f1.PendingDecision.Name, step.Name)
	}
	if len(f1.History) != 0 {
		t.Fatalf("FAIL — Phase 1: History has %d entries, want 0", len(f1.History))
	}

	t.Logf("PASS — Phase 1 (Barrier 1): PendingDecision.Name=%q, History=[]",
		f1.PendingDecision.Name)

	// ── Phase 2: Barrier 2 (Checkpoint Path A) ──────────────────────────────
	// After Checkpoint: output column has value, step is DONE.
	// LoadFrontier must return History=[{ocr, DONE, output}], PendingDecision=nil.

	result := core.Result{
		Status: "done",
		Output: json.RawMessage(`{"text": "extracted text", "pages": 3}`),
	}

	if err := s.Checkpoint(run, step, result); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	f2, err := s.LoadFrontier(run)
	if err != nil {
		t.Fatalf("LoadFrontier after Checkpoint: %v", err)
	}

	if f2.PendingDecision != nil {
		t.Fatalf("FAIL — Phase 2: PendingDecision = %+v, want nil "+
			"(output is now non-null, step is DONE)", f2.PendingDecision)
	}
	if len(f2.History) != 1 {
		t.Fatalf("FAIL — Phase 2: History has %d entries, want 1", len(f2.History))
	}

	got := f2.History[0]
	if got.Name != "ocr" {
		t.Fatalf("FAIL — Phase 2: History[0].Name = %q, want %q", got.Name, "ocr")
	}
	if got.Status != "DONE" {
		t.Fatalf("FAIL — Phase 2: History[0].Status = %q, want %q", got.Status, "DONE")
	}

	var gotOutput map[string]any
	if err := json.Unmarshal(got.Output, &gotOutput); err != nil {
		t.Fatalf("FAIL — Phase 2: unmarshal History[0].Output: %v", err)
	}
	wantText := "extracted text"
	if fmt.Sprint(gotOutput["text"]) != wantText {
		t.Fatalf("FAIL — Phase 2: History[0].Output[text] = %q, want %q",
			gotOutput["text"], wantText)
	}

	t.Logf("PASS — Phase 2 (Barrier 2): History[0]={Name:%q Status:%q Output:%s}",
		got.Name, got.Status, got.Output)
	t.Log("PASS — Barrier invariant: decision-without-output=pending; output-present=done")
}

// TestCheckpointPathB_FailedStepNotInHistory verifies the Path B invariants
// from DESIGN.md §9.1:
//
//  1. steps.output stays NULL after a FAILED checkpoint — the DONE signal is not corrupted.
//  2. steps.status transitions to FAILED.
//  3. The attempts row records status=FAILED and the error message.
//  4. LoadFrontier.History is empty — a FAILED step must never appear as DONE.
//  5. LoadFrontier.PendingDecision is nil — FAILED does not match DECIDED/RUNNING.
//
// Points 4 and 5 are the critical planner-safety checks: if a failed step leaked
// into History, the planner would treat the failure output as a success and make
// decisions on a false premise.
func TestCheckpointPathB_FailedStepNotInHistory(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)

	const (
		workflowID = "wf-pathb-test"
		runID      = "run-pathb-test"
	)
	seedFixtures(t, db, workflowID, runID)

	s := store.New(db)
	run := core.RunID(runID)

	step := core.StepSpec{
		Name:           "ner",
		WorkerURL:      "http://ner-service/run",
		Mode:           "async",
		TimeoutSeconds: 30,
		Input:          json.RawMessage(`{"text": "hello world"}`),
	}

	// ── Setup: simulate PutDecision → RecordAttemptStart → worker returns failure ──

	if err := s.PutDecision(run, step); err != nil {
		t.Fatalf("PutDecision: %v", err)
	}

	// RecordAttemptStart simulates what the loop does just before Dispatch:
	// creates the attempts row (RUNNING) and sets steps.current_attempt_id.
	if err := s.RecordAttemptStart(run, step, testAttemptID); err != nil {
		t.Fatalf("RecordAttemptStart: %v", err)
	}

	// Checkpoint Path B: worker explicitly reported failure.
	failedResult := core.Result{
		Status: "failed",
		Error:  "worker crashed: out of memory",
	}
	if err := s.Checkpoint(run, step, failedResult); err != nil {
		t.Fatalf("Checkpoint Path B: %v", err)
	}

	stepID := fmt.Sprintf("%s:ner", runID)

	// ── Assert 1: steps.output IS NULL ──────────────────────────────────────────
	// This is the load-bearing check. steps.output non-null is the DONE signal.
	// If output were written here, recovery would classify this failed step as
	// DONE and feed its error payload to the planner as a success output.
	var outputIsNull bool
	if err := db.QueryRow(`SELECT output IS NULL FROM steps WHERE step_id = $1`, stepID).
		Scan(&outputIsNull); err != nil {
		t.Fatalf("query steps.output: %v", err)
	}
	if !outputIsNull {
		t.Fatal("FAIL — steps.output is NOT NULL after FAILED Checkpoint; " +
			"writing output on failure corrupts the DONE signal used by recovery")
	}
	t.Log("PASS — steps.output IS NULL (DONE signal not corrupted by failure)")

	// ── Assert 2: steps.status = FAILED ─────────────────────────────────────────
	var stepStatus string
	if err := db.QueryRow(`SELECT status FROM steps WHERE step_id = $1`, stepID).
		Scan(&stepStatus); err != nil {
		t.Fatalf("query steps.status: %v", err)
	}
	if stepStatus != "FAILED" {
		t.Fatalf("FAIL — steps.status = %q, want FAILED", stepStatus)
	}
	t.Logf("PASS — steps.status = FAILED")

	// ── Assert 3: attempts row — status=FAILED, error recorded ──────────────────
	var attemptStatus, attemptError string
	if err := db.QueryRow(`
		SELECT status, COALESCE(error, '') FROM attempts WHERE attempt_id = $1::uuid
	`, string(testAttemptID)).Scan(&attemptStatus, &attemptError); err != nil {
		t.Fatalf("query attempts row: %v", err)
	}
	if attemptStatus != "FAILED" {
		t.Fatalf("FAIL — attempts.status = %q, want FAILED", attemptStatus)
	}
	if attemptError != failedResult.Error {
		t.Fatalf("FAIL — attempts.error = %q, want %q", attemptError, failedResult.Error)
	}
	t.Logf("PASS — attempts row: status=FAILED error=%q", attemptError)

	// ── Assert 4+5: LoadFrontier excludes FAILED step ───────────────────────────
	// History must be empty: FAILED ≠ DONE, planner must not see this step as complete.
	// PendingDecision must be nil: FAILED ≠ DECIDED/RUNNING, no re-dispatch triggered.
	f, err := s.LoadFrontier(run)
	if err != nil {
		t.Fatalf("LoadFrontier: %v", err)
	}
	if len(f.History) != 0 {
		t.Fatalf("FAIL — LoadFrontier.History has %d entries, want 0; "+
			"a FAILED step must never appear in History (planner would treat failure as success)",
			len(f.History))
	}
	if f.PendingDecision != nil {
		t.Fatalf("FAIL — LoadFrontier.PendingDecision = %+v, want nil; "+
			"FAILED status does not trigger re-dispatch (loop decides retry/DLQ)",
			f.PendingDecision)
	}
	t.Log("PASS — LoadFrontier: History=[], PendingDecision=nil (FAILED excluded from frontier)")
	t.Log("PASS — Path B complete: FAILED state recorded faithfully; retry/DLQ is the loop's decision")
}

// TestRecordAttemptStart_AttemptNumberIncrements verifies that each call to
// RecordAttemptStart assigns a monotonically increasing attempt_number for the
// same step. This is the retry-history audit trail requirement (DESIGN.md §4).
func TestRecordAttemptStart_AttemptNumberIncrements(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)

	const (
		workflowID = "wf-attempt-test"
		runID      = "run-attempt-test"
	)
	seedFixtures(t, db, workflowID, runID)

	s := store.New(db)
	run := core.RunID(runID)

	step := core.StepSpec{
		Name:           "summarize",
		WorkerURL:      "http://llm/run",
		Mode:           "sync",
		TimeoutSeconds: 60,
		Input:          json.RawMessage(`{}`),
	}

	if err := s.PutDecision(run, step); err != nil {
		t.Fatalf("PutDecision: %v", err)
	}

	stepID := fmt.Sprintf("%s:summarize", runID)

	// First attempt
	id1 := core.AttemptID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if err := s.RecordAttemptStart(run, step, id1); err != nil {
		t.Fatalf("RecordAttemptStart attempt 1: %v", err)
	}

	// Simulate Path B so we can call RecordAttemptStart again (retry)
	if err := s.Checkpoint(run, step, core.Result{Status: "failed", Error: "timeout"}); err != nil {
		t.Fatalf("Checkpoint attempt 1 (failed): %v", err)
	}

	// For the retry, the loop would call PutDecision again. But since step_id is
	// the same (same step, same run), we need to reset the step status to DECIDED.
	// This simulates the loop's retry action (which belongs to the loop session).
	// We do it directly in the test to isolate the attempt_number increment check.
	if _, err := db.Exec(`UPDATE steps SET status = 'DECIDED' WHERE step_id = $1`, stepID); err != nil {
		t.Fatalf("reset step to DECIDED for retry: %v", err)
	}

	// Second attempt
	id2 := core.AttemptID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	if err := s.RecordAttemptStart(run, step, id2); err != nil {
		t.Fatalf("RecordAttemptStart attempt 2: %v", err)
	}

	// Check attempt_numbers
	rows, err := db.Query(`
		SELECT attempt_number, status FROM attempts WHERE step_id = $1 ORDER BY attempt_number ASC
	`, stepID)
	if err != nil {
		t.Fatalf("query attempts: %v", err)
	}
	defer rows.Close()

	type row struct{ num int; status string }
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.num, &r.status); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 attempt rows, got %d", len(got))
	}
	if got[0].num != 1 {
		t.Fatalf("first attempt_number = %d, want 1", got[0].num)
	}
	if got[1].num != 2 {
		t.Fatalf("second attempt_number = %d, want 2", got[1].num)
	}
	t.Logf("PASS — attempt_number increments: [%d:%s, %d:%s]",
		got[0].num, got[0].status, got[1].num, got[1].status)
}
