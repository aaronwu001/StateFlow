package orchestrator_test

// Integration tests for crash recovery (DESIGN.md §9.3, CLAUDE.md recovery rules).
// Require a real Postgres DB — skipped when TEST_DATABASE_URL is unset.
//
// Each test manually plants a mid-run DB state, calls RecoverRuns, and verifies
// that recovery converges to the correct terminal state without re-dispatching
// steps that were already DONE.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/orchestrator"
	"github.com/aaronwu000/stateflow/internal/store"
)

// ── Stubs ─────────────────────────────────────────────────────────────────

// recoveryDonePlanner returns "done" once History has at least minHistory entries.
// Returning an error for fewer entries catches the bug where a DONE step was
// incorrectly re-dispatched (history would then be shorter than expected).
type recoveryDonePlanner struct{ minHistory int }

func (p *recoveryDonePlanner) Decide(_ context.Context, s core.RunState) (core.StepDecision, error) {
	if len(s.History) < p.minHistory {
		return core.StepDecision{}, fmt.Errorf(
			"planner called with %d history entries, want >= %d (DONE step may have been re-dispatched)",
			len(s.History), p.minHistory)
	}
	return core.StepDecision{Status: "done"}, nil
}

// alwaysSucceedTransport returns "done" immediately (no network required).
type alwaysSucceedTransport struct{}

func (t *alwaysSucceedTransport) Dispatch(_ context.Context, _ core.StepSpec) (core.Result, error) {
	return core.Result{Status: "done", Output: json.RawMessage(`{"recovered":true}`)}, nil
}

// ── DB helpers ────────────────────────────────────────────────────────────

// decisionJSON builds a minimal but valid StepSpec JSON for insertion into
// steps.decision. The loop reads this back via PendingDecision/LoadFrontier.
func decisionJSON(name, workerURL string) string {
	return fmt.Sprintf(`{"name":%q,"worker_url":%q,"mode":"sync","timeout_seconds":5}`, name, workerURL)
}

// plantDoneStep inserts a step in DONE status (Barrier 1 + Barrier 2 both fired).
// Also inserts the corresponding DONE attempt row.
func plantDoneStep(t *testing.T, db *sql.DB, runID, stepName string, seq int, attemptUUID string) {
	t.Helper()
	stepID := runID + ":" + stepName
	dec := decisionJSON(stepName, "http://stub/"+stepName)
	out := `{"result":"done_before_crash"}`
	if _, err := db.Exec(`
		INSERT INTO steps
			(step_id, run_id, step_name, seq, status, decision, output,
			 current_attempt_id, decided_at, completed_at)
		VALUES ($1, $2, $3, $4, 'DONE', $5::jsonb, $6::jsonb, $7::uuid, now(), now())
	`, stepID, runID, stepName, seq, dec, out, attemptUUID); err != nil {
		t.Fatalf("plantDoneStep %s: %v", stepID, err)
	}
	if _, err := db.Exec(`
		INSERT INTO attempts (attempt_id, step_id, attempt_number, status, resolved_at)
		VALUES ($1::uuid, $2, 1, 'DONE', now())
	`, attemptUUID, stepID); err != nil {
		t.Fatalf("plantDoneStep attempt %s: %v", stepID, err)
	}
}

// plantDecidedStep inserts a step where Barrier 1 fired but the process crashed
// before dispatch (status=DECIDED, no attempt row, no current_attempt_id).
func plantDecidedStep(t *testing.T, db *sql.DB, runID, stepName string, seq int) {
	t.Helper()
	stepID := runID + ":" + stepName
	dec := decisionJSON(stepName, "http://stub/"+stepName)
	if _, err := db.Exec(`
		INSERT INTO steps (step_id, run_id, step_name, seq, status, decision, decided_at)
		VALUES ($1, $2, $3, $4, 'DECIDED', $5::jsonb, now())
	`, stepID, runID, stepName, seq, dec); err != nil {
		t.Fatalf("plantDecidedStep %s: %v", stepID, err)
	}
}

// plantRunningStep inserts a step where dispatch started (Barrier 1 fired +
// RecordAttemptStart called) but the process crashed before Checkpoint fired.
// The attempt row stays RUNNING — it is the "crashed" attempt.
func plantRunningStep(t *testing.T, db *sql.DB, runID, stepName string, seq int, attemptUUID string) {
	t.Helper()
	stepID := runID + ":" + stepName
	dec := decisionJSON(stepName, "http://stub/"+stepName)
	if _, err := db.Exec(`
		INSERT INTO steps
			(step_id, run_id, step_name, seq, status, decision, current_attempt_id, decided_at)
		VALUES ($1, $2, $3, $4, 'RUNNING', $5::jsonb, $6::uuid, now())
	`, stepID, runID, stepName, seq, dec, attemptUUID); err != nil {
		t.Fatalf("plantRunningStep %s: %v", stepID, err)
	}
	if _, err := db.Exec(`
		INSERT INTO attempts (attempt_id, step_id, attempt_number, status)
		VALUES ($1::uuid, $2, 1, 'RUNNING')
	`, attemptUUID, stepID); err != nil {
		t.Fatalf("plantRunningStep attempt %s: %v", stepID, err)
	}
}

// pollRunStatus polls runs.status every 50 ms until it leaves 'RUNNING'.
// Fails the test if the run is still RUNNING after timeout.
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
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %q still RUNNING after %v", runID, timeout)
	return ""
}

// ── Test 1: core recovery — DECIDED step ─────────────────────────────────

// TestRecovery_PicksUpDecidedStep is the canonical crash-recovery test.
//
// Planted state:
//
//	step1 = DONE  (output set, 1 DONE attempt)           — must NOT be re-dispatched
//	step2 = DECIDED  (Barrier 1 fired, no dispatch yet)  — must be picked up and run
//
// The planner returns "done" only after both steps are in history (minHistory=2).
// If step1 were re-dispatched, the planner would be called mid-recovery with only
// 1 history entry and would return an error, failing the test.
func TestRecovery_PicksUpDecidedStep(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		wfID     = "wf-rec-decided"
		runID    = "run-rec-decided"
		attempt1 = "10000000-0000-4000-8000-000000000001"
	)
	seedTestFixtures(t, db, wfID, runID)
	plantDoneStep(t, db, runID, "step1", 1, attempt1)
	plantDecidedStep(t, db, runID, "step2", 2)

	s := store.New(db)
	n, err := orchestrator.RecoverRuns(context.Background(), db,
		func(id core.RunID, input json.RawMessage) *orchestrator.Loop {
			return &orchestrator.Loop{
				RunID:         id,
				WorkflowInput: input,
				Store:         s,
				Planner:       &recoveryDonePlanner{minHistory: 2},
				Transport:     &alwaysSucceedTransport{},
				Retry:         &orchestrator.FixedCountPolicy{MaxRetries: 3, Delay: 0},
			}
		})
	if err != nil {
		t.Fatalf("RecoverRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("RecoverRuns n = %d, want 1", n)
	}

	finalStatus := pollRunStatus(t, db, runID, 5*time.Second)

	step1ID := runID + ":step1"
	step2ID := runID + ":step2"

	// ── step1: exactly 1 attempt (not re-dispatched) ───────────────────────
	var step1Attempts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE step_id = $1`, step1ID).
		Scan(&step1Attempts); err != nil {
		t.Fatalf("count step1 attempts: %v", err)
	}
	if step1Attempts != 1 {
		t.Errorf("step1 attempt count = %d, want 1 (DONE step must not be re-dispatched)", step1Attempts)
	}
	t.Logf("PASS — step1 attempt count unchanged at %d (not re-dispatched)", step1Attempts)

	// ── step1: output unchanged ────────────────────────────────────────────
	var rawOut []byte
	if err := db.QueryRow(`SELECT output FROM steps WHERE step_id = $1`, step1ID).
		Scan(&rawOut); err != nil {
		t.Fatalf("query step1 output: %v", err)
	}
	var gotOut map[string]any
	if err := json.Unmarshal(rawOut, &gotOut); err != nil {
		t.Fatalf("unmarshal step1 output: %v", err)
	}
	if gotOut["result"] != "done_before_crash" {
		t.Errorf("step1 output changed to %s; want original value", rawOut)
	}
	t.Logf("PASS — step1 output unchanged: %s", rawOut)

	// ── step2: exactly 1 attempt (the recovery dispatch) ──────────────────
	var step2Attempts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE step_id = $1`, step2ID).
		Scan(&step2Attempts); err != nil {
		t.Fatalf("count step2 attempts: %v", err)
	}
	if step2Attempts != 1 {
		t.Errorf("step2 attempt count = %d, want 1", step2Attempts)
	}
	t.Logf("PASS — step2 dispatched %d time(s) by recovery", step2Attempts)

	// ── step2: DONE ────────────────────────────────────────────────────────
	var step2Status string
	if err := db.QueryRow(`SELECT status FROM steps WHERE step_id = $1`, step2ID).
		Scan(&step2Status); err != nil {
		t.Fatalf("query step2 status: %v", err)
	}
	if step2Status != "DONE" {
		t.Errorf("step2.status = %q, want DONE", step2Status)
	}
	t.Logf("PASS — step2.status = DONE")

	// ── run: DONE ─────────────────────────────────────────────────────────
	if finalStatus != "DONE" {
		t.Errorf("run.status = %q, want DONE", finalStatus)
	}
	t.Logf("PASS — run.status = DONE")
}

// ── Test 2: RUNNING-uncertain → re-dispatch, never FAILED ────────────────

// TestRecovery_RunningUncertainReDispatched verifies the most critical rule in
// CLAUDE.md: a RUNNING step with no output is UNCERTAIN, not FAILED.
//
// Planted state:
//
//	step1 = RUNNING  (dispatch happened; crash occurred before Checkpoint)
//	         1 attempt row, status=RUNNING (the crashed dispatch)
//
// Expectations:
//   - step1 is re-dispatched with a NEW attempt_id → total attempts = 2
//   - step1 ends DONE (not FAILED, not DLQ)
//   - The crashed attempt row stays RUNNING (never retroactively marked FAILED)
//   - run ends DONE
func TestRecovery_RunningUncertainReDispatched(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		wfID           = "wf-rec-running"
		runID          = "run-rec-running"
		crashedAttempt = "20000000-0000-4000-8000-000000000002"
	)
	seedTestFixtures(t, db, wfID, runID)
	plantRunningStep(t, db, runID, "step1", 1, crashedAttempt)

	s := store.New(db)
	n, err := orchestrator.RecoverRuns(context.Background(), db,
		func(id core.RunID, input json.RawMessage) *orchestrator.Loop {
			return &orchestrator.Loop{
				RunID:         id,
				WorkflowInput: input,
				Store:         s,
				Planner:       &recoveryDonePlanner{minHistory: 1},
				Transport:     &alwaysSucceedTransport{},
				Retry:         &orchestrator.FixedCountPolicy{MaxRetries: 3, Delay: 0},
			}
		})
	if err != nil {
		t.Fatalf("RecoverRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("RecoverRuns n = %d, want 1", n)
	}

	finalStatus := pollRunStatus(t, db, runID, 5*time.Second)

	step1ID := runID + ":step1"

	// ── 2 attempts total (crashed + recovery) ─────────────────────────────
	var attemptCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE step_id = $1`, step1ID).
		Scan(&attemptCount); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attemptCount != 2 {
		t.Errorf("step1 attempt count = %d, want 2 (1 crashed + 1 recovery dispatch)", attemptCount)
	}
	t.Logf("PASS — step1 has %d attempts (crashed attempt preserved, recovery added one)", attemptCount)

	// ── step1: DONE (not FAILED) ───────────────────────────────────────────
	var step1Status string
	if err := db.QueryRow(`SELECT status FROM steps WHERE step_id = $1`, step1ID).
		Scan(&step1Status); err != nil {
		t.Fatalf("query step1 status: %v", err)
	}
	if step1Status != "DONE" {
		t.Errorf("step1.status = %q, want DONE — RUNNING-uncertain must never be treated as FAILED", step1Status)
	}
	t.Logf("PASS — step1.status = DONE (not FAILED; RUNNING-uncertain was re-dispatched)")

	// ── no FAILED attempt rows — the crashed attempt stays RUNNING ─────────
	var failedAttempts int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM attempts WHERE step_id = $1 AND status = 'FAILED'
	`, step1ID).Scan(&failedAttempts); err != nil {
		t.Fatalf("count failed attempts: %v", err)
	}
	if failedAttempts != 0 {
		t.Errorf("step1 has %d FAILED attempt rows, want 0", failedAttempts)
	}
	t.Logf("PASS — no FAILED attempt rows (crashed attempt stays RUNNING; recovery attempt is DONE)")

	// ── run: DONE ─────────────────────────────────────────────────────────
	if finalStatus != "DONE" {
		t.Errorf("run.status = %q, want DONE", finalStatus)
	}
	t.Logf("PASS — run.status = DONE")
}

// ── Test 3: terminal runs are not touched ────────────────────────────────

// TestRecovery_SkipsTerminalRuns verifies that only status='RUNNING' runs are
// resumed. A DONE run is terminal and must be completely ignored.
func TestRecovery_SkipsTerminalRuns(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		wfID  = "wf-rec-terminal"
		runID = "run-rec-terminal"
	)
	if _, err := db.Exec(`
		INSERT INTO workflows (workflow_id, name, planner_type, planner_config)
		VALUES ($1, 'test-terminal', 'static', '{}')
	`, wfID); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO runs (run_id, workflow_id, status, workflow_input)
		VALUES ($1, $2, 'DONE', '{}')
	`, runID, wfID); err != nil {
		t.Fatalf("seed done run: %v", err)
	}

	factoryCalls := 0
	n, err := orchestrator.RecoverRuns(context.Background(), db,
		func(id core.RunID, _ json.RawMessage) *orchestrator.Loop {
			factoryCalls++
			t.Errorf("makeLoop called for run %q — terminal DONE run must not be resumed", id)
			return nil
		})
	if err != nil {
		t.Fatalf("RecoverRuns: %v", err)
	}
	if n != 0 {
		t.Errorf("RecoverRuns n = %d, want 0 (DONE run is terminal)", n)
	}
	if factoryCalls != 0 {
		t.Errorf("factory called %d times, want 0", factoryCalls)
	}
	t.Logf("PASS — RecoverRuns returned n=0, factory called 0 times (DONE run skipped)")
}
