package orchestrator_test

// Shared DB test helpers for internal/orchestrator's integration tests
// (loop_integration_test.go, recovery_test.go). Require a real Postgres DB;
// every test using openTestDB skips when TEST_DATABASE_URL is unset
// (CLAUDE.md's "Running Tests" section).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// openTestDB opens a connection to TEST_DATABASE_URL, skipping the test if
// unset.
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

// resetTestSchema drops and re-applies migrations/001_initial.sql — the
// project's "-p 1 is mandatory" reset pattern (CLAUDE.md).
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

// seedWorkflowAndRun inserts a workflow (planner_type/planner_config as
// given — tests that inject Loop.PlannerFactory can pass a placeholder;
// tests exercising the REAL HTTPPlanner/StaticPlanner reconstruction must
// pass a config that genuinely parses) and a RUNNING run under it.
func seedWorkflowAndRun(t *testing.T, db *sql.DB, workflowID, runID, plannerType, plannerConfigJSON string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO workflows (workflow_id, name, planner_type, planner_config)
		VALUES ($1, 'test-workflow', $2, $3::jsonb)
	`, workflowID, plannerType, plannerConfigJSON); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO runs (run_id, workflow_id, status, workflow_input)
		VALUES ($1, $2, 'RUNNING', '{}')
	`, runID, workflowID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

// staticPlannerConfigWithRetryLimit is the standard placeholder planner_config
// for tests that inject Loop.PlannerFactory (so the actual JSON content is
// never parsed into a real planner) but still need CreateWorkflow/GetWorkflow
// to round-trip a specific retry_limit (Session 2 CONFIRM: key = retry_limit).
func staticPlannerConfigWithRetryLimit(retryLimit int) string {
	b, _ := json.Marshal(map[string]int{"retry_limit": retryLimit})
	return string(b)
}

// decisionJSON builds a minimal but valid StepSpec JSON for insertion into
// steps.decision — used by the plant* helpers to simulate a step whose
// Barrier 1 (TX1) already fired before a simulated crash.
func decisionJSON(name, workerURL string) string {
	return fmt.Sprintf(`{"name":%q,"worker_url":%q,"mode":"sync","timeout_seconds":5}`, name, workerURL)
}

// plantDoneStep inserts a step in DONE status (Barrier 1 + Barrier 2 both
// fired) with one DONE attempt — simulates a step that completed before the
// crash and must NOT be re-dispatched or re-decided on recovery.
func plantDoneStep(t *testing.T, db *sql.DB, runID, stepName string, seq int, attemptUUID string) {
	t.Helper()
	stepID := runID + ":" + stepName
	dec := decisionJSON(stepName, "http://stub/"+stepName)
	out := `{"result":"done_before_crash"}`
	if _, err := db.Exec(`
		INSERT INTO steps
			(step_id, run_id, step_name, seq, status, attempt_count, decision, output, current_attempt_id, completed_at)
		VALUES ($1, $2, $3, $4, 'DONE', 1, $5::jsonb, $6::jsonb, $7::uuid, now())
	`, stepID, runID, stepName, seq, dec, out, attemptUUID); err != nil {
		t.Fatalf("plantDoneStep %s: %v", stepID, err)
	}
	if _, err := db.Exec(`
		INSERT INTO attempts (attempt_id, step_id, status, resolved_at)
		VALUES ($1::uuid, $2, 'DONE', now())
	`, attemptUUID, stepID); err != nil {
		t.Fatalf("plantDoneStep attempt %s: %v", stepID, err)
	}
}

// plantRunningStep inserts a step in RUNNING status (decision persisted —
// Barrier 1 fired) with attempt_count and exactly one attempt row, whose
// status/failure_reason are given by the caller. current_attempt_id always
// points at this one attempt — matching real TX1/TX3 behavior (TX3 never
// moves the pointer; only TX1/TX4/TX5 do), which is what lets this helper
// simulate BOTH:
//   - a live-dispatch crash (attemptStatus="RUNNING", failureReason=""):
//     nothing has resolved this attempt — recovery must claim it as orphaned.
//   - a crash between TX3 and TX4 (attemptStatus="FAILED", failureReason set,
//     attemptCount already reflecting that TX3's increment): recovery's
//     unconditional claim attempt must be a CAS no-op (Superseded), and
//     recovery must proceed straight to TX4 without claiming anything.
func plantRunningStep(t *testing.T, db *sql.DB, runID, stepName string, seq, attemptCount int, attemptUUID, attemptStatus, failureReason string) {
	t.Helper()
	stepID := runID + ":" + stepName
	dec := decisionJSON(stepName, "http://stub/"+stepName)
	if _, err := db.Exec(`
		INSERT INTO steps
			(step_id, run_id, step_name, seq, status, attempt_count, decision, current_attempt_id)
		VALUES ($1, $2, $3, $4, 'RUNNING', $5, $6::jsonb, $7::uuid)
	`, stepID, runID, stepName, seq, attemptCount, dec, attemptUUID); err != nil {
		t.Fatalf("plantRunningStep %s: %v", stepID, err)
	}
	if attemptStatus == "RUNNING" {
		if _, err := db.Exec(`
			INSERT INTO attempts (attempt_id, step_id, status)
			VALUES ($1::uuid, $2, 'RUNNING')
		`, attemptUUID, stepID); err != nil {
			t.Fatalf("plantRunningStep attempt %s: %v", stepID, err)
		}
	} else {
		if _, err := db.Exec(`
			INSERT INTO attempts (attempt_id, step_id, status, failure_reason, error, resolved_at)
			VALUES ($1::uuid, $2, 'FAILED', $3, 'seed: simulated pre-crash failure', now())
		`, attemptUUID, stepID, failureReason); err != nil {
			t.Fatalf("plantRunningStep attempt %s: %v", stepID, err)
		}
	}
}

// pollRunStatus polls runs.status every 25ms until it leaves 'RUNNING' (or
// timeout elapses, failing the test). Used to wait for a Loop.Run goroutine
// started via RecoverRuns/go loop.Run(ctx) to converge without a fixed sleep.
func pollRunStatus(t *testing.T, db *sql.DB, runID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status string
		if err := db.QueryRow(`SELECT status FROM runs WHERE run_id = $1`, runID).Scan(&status); err != nil {
			t.Fatalf("pollRunStatus: %v", err)
		}
		if status != "RUNNING" {
			return status
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("run %q still RUNNING after %v", runID, timeout)
	return ""
}
