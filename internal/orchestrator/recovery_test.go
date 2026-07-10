package orchestrator_test

// Integration tests for crash recovery (whitepaper §8.3, §8.2 combination
// table). Require a real Postgres DB — skipped when TEST_DATABASE_URL is
// unset.
//
// Each test plants a mid-run DB state (simulating a crash at a specific
// point in the TX ledger) and calls RecoverRuns (or drives Loop.Run
// directly), then verifies the resulting DB state by querying it — not by
// trusting the loop's return value alone.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/orchestrator"
	"github.com/aaronwu000/stateflow/internal/store"
)

// ── Stubs ─────────────────────────────────────────────────────────────────

// countingDonePlanner returns "done" once History has at least minHistory
// entries, and records every call (history length at call time). Returning
// an error for fewer entries catches the bug where a DONE step was
// incorrectly re-decided or re-dispatched.
type countingDonePlanner struct {
	minHistory int
	calls      []int
}

func (p *countingDonePlanner) Decide(_ context.Context, s core.RunState) (core.StepDecision, error) {
	p.calls = append(p.calls, len(s.History))
	if len(s.History) < p.minHistory {
		return core.StepDecision{}, fmt.Errorf(
			"planner called with %d history entries, want >= %d (a DONE/pending step may have been re-decided)",
			len(s.History), p.minHistory)
	}
	return core.StepDecision{Status: core.PlannerVerdictDone}, nil
}

// panicPlanner fails the test immediately if Decide is ever called — used to
// assert that a budget-exhausted (DLQ'd) recovery never asks the planner.
type panicPlanner struct{ t *testing.T }

func (p *panicPlanner) Decide(_ context.Context, s core.RunState) (core.StepDecision, error) {
	p.t.Fatalf("planner.Decide called (history len=%d) — must not be asked once the run is already DLQ'd", len(s.History))
	return core.StepDecision{}, nil
}

// alwaysSucceedTransport returns "done" immediately (no network required).
type alwaysSucceedTransport struct{ calls int }

func (t *alwaysSucceedTransport) Dispatch(_ context.Context, _ core.StepSpec) (core.Result, error) {
	t.calls++
	return core.Result{Status: core.ResultStatusDone, Output: json.RawMessage(`{"recovered":true}`)}, nil
}

// panicTransport fails the test immediately if Dispatch is ever called —
// used to assert that a budget-exhausted (DLQ'd) recovery never dispatches.
type panicTransport struct{ t *testing.T }

func (t *panicTransport) Dispatch(_ context.Context, step core.StepSpec) (core.Result, error) {
	t.t.Fatalf("transport.Dispatch called for step %q — must not dispatch once the run is already DLQ'd", step.Name)
	return core.Result{}, nil
}

// staticFactory returns a Loop.PlannerFactory that always hands back p,
// ignoring the workflow row — the "reconstructed every time" property
// (whitepaper §12.1) is exercised for real by the wire-casing test
// (loop_integration_test.go), not by these DB-plumbing-focused tests.
func staticFactory(p core.NextStepPlanner) func(core.WorkflowDef) (core.NextStepPlanner, error) {
	return func(core.WorkflowDef) (core.NextStepPlanner, error) { return p, nil }
}

// ── Test: terminal runs are not touched ──────────────────────────────────

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

	s := store.New(db)
	factoryCalls := 0
	n, err := orchestrator.RecoverRuns(context.Background(), s,
		func(id core.RunID, _ core.WorkflowID, _ json.RawMessage) *orchestrator.Loop {
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

// ── Mandatory test (a): budget-boundary crash ────────────────────────────

// TestRecovery_BudgetBoundaryCrash_ClaimDLQsImmediately seeds a step with
// attempt_count = X-1 (retryLimit-1) and a single RUNNING attempt (a
// live-dispatch crash). Recovery's orphan claim is the retryLimit-th
// failure, so it must land the step and run in the DLQ inside the SAME
// transaction as the claim (TX3's one-blade guarantee) — nothing may be
// dispatched, and the planner must never be asked (run=DLQ, last_step=DLQ is
// a terminal, untouched combination — whitepaper §8.2).
func TestRecovery_BudgetBoundaryCrash_ClaimDLQsImmediately(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		retryLimit     = 3
		wfID           = "wf-rec-budget"
		runID          = "run-rec-budget"
		crashedAttempt = "10000000-0000-4000-8000-000000000001"
	)
	seedWorkflowAndRun(t, db, wfID, runID, "static", staticPlannerConfigWithRetryLimit(retryLimit))
	// attempt_count = retryLimit-1 = 2; one RUNNING attempt (never resolved
	// before the simulated crash).
	plantRunningStep(t, db, runID, "step1", 1, retryLimit-1, crashedAttempt, "RUNNING", "")

	s := store.New(db)
	n, err := orchestrator.RecoverRuns(context.Background(), s,
		func(id core.RunID, wfID core.WorkflowID, input json.RawMessage) *orchestrator.Loop {
			return &orchestrator.Loop{
				RunID:          id,
				WorkflowID:     wfID,
				WorkflowInput:  input,
				Store:          s,
				Transport:      &panicTransport{t: t},
				PlannerFactory: staticFactory(&panicPlanner{t: t}),
			}
		})
	if err != nil {
		t.Fatalf("RecoverRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("RecoverRuns n = %d, want 1", n)
	}

	finalStatus := pollRunStatus(t, db, runID, 5*time.Second)
	if finalStatus != "DLQ" {
		t.Errorf("run.status = %q, want DLQ", finalStatus)
	}
	t.Logf("PASS — run.status = DLQ")

	stepID := runID + ":step1"
	var stepStatus string
	var attemptCount int
	if err := db.QueryRow(`SELECT status, attempt_count FROM steps WHERE step_id = $1`, stepID).
		Scan(&stepStatus, &attemptCount); err != nil {
		t.Fatalf("query step: %v", err)
	}
	if stepStatus != "DLQ" {
		t.Errorf("step.status = %q, want DLQ", stepStatus)
	}
	if attemptCount != retryLimit {
		t.Errorf("step.attempt_count = %d, want %d", attemptCount, retryLimit)
	}
	t.Logf("PASS — step.status=DLQ, attempt_count=%d", attemptCount)

	// Exactly one attempt row (the seeded one), now FAILED(orphaned) — no
	// new attempt was ever created (TX4/dispatch never fired).
	var attemptRows int
	var attemptStatus, failureReason string
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE step_id = $1`, stepID).Scan(&attemptRows); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attemptRows != 1 {
		t.Errorf("attempt row count = %d, want 1 (no redispatch past the budget)", attemptRows)
	}
	if err := db.QueryRow(`SELECT status, failure_reason FROM attempts WHERE attempt_id = $1::uuid`, crashedAttempt).
		Scan(&attemptStatus, &failureReason); err != nil {
		t.Fatalf("query attempt: %v", err)
	}
	if attemptStatus != "FAILED" || failureReason != "orphaned" {
		t.Errorf("attempt status/reason = %s/%s, want FAILED/orphaned", attemptStatus, failureReason)
	}
	t.Logf("PASS — exactly 1 attempt row, claimed as FAILED(orphaned); nothing dispatched")

	var dlqReason string
	var dlqContext []byte
	if err := db.QueryRow(`SELECT reason, context FROM dead_letter_queue WHERE run_id = $1`, runID).
		Scan(&dlqReason, &dlqContext); err != nil {
		t.Fatalf("query dlq: %v", err)
	}
	if dlqReason != "worker_retry_exhausted" {
		t.Errorf("dlq.reason = %q, want worker_retry_exhausted", dlqReason)
	}
	var ctxBlob map[string]any
	if err := json.Unmarshal(dlqContext, &ctxBlob); err != nil {
		t.Fatalf("unmarshal dlq context: %v", err)
	}
	t.Logf("PASS — DLQ entry: reason=%q, context=%s", dlqReason, dlqContext)
}

// ── Mandatory test (b): recovery re-entrancy ─────────────────────────────

// TestRecovery_ReEntrancy_CountIncrementedExactlyOnce runs recovery TWICE
// over the same seeded state (well below the retry budget so the first pass
// fully resolves the run). The second pass's ListRunningRuns scan finds
// nothing (the run is now DONE) — proving an already-claimed orphan cannot
// be claimed or counted twice (whitepaper §8.3's re-entrancy property).
func TestRecovery_ReEntrancy_CountIncrementedExactlyOnce(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		retryLimit     = 3
		wfID           = "wf-rec-reentrant"
		runID          = "run-rec-reentrant"
		crashedAttempt = "20000000-0000-4000-8000-000000000002"
	)
	seedWorkflowAndRun(t, db, wfID, runID, "static", staticPlannerConfigWithRetryLimit(retryLimit))
	plantRunningStep(t, db, runID, "step1", 1, 0, crashedAttempt, "RUNNING", "")

	s := store.New(db)
	planner := &countingDonePlanner{minHistory: 1}
	transport := &alwaysSucceedTransport{}

	makeLoop := func(id core.RunID, wf core.WorkflowID, input json.RawMessage) *orchestrator.Loop {
		return &orchestrator.Loop{
			RunID:          id,
			WorkflowID:     wf,
			WorkflowInput:  input,
			Store:          s,
			Transport:      transport,
			PlannerFactory: staticFactory(planner),
		}
	}

	// First recovery pass: claims the orphan (count 0→1), redispatches,
	// succeeds, then the planner says done.
	n1, err := orchestrator.RecoverRuns(context.Background(), s, makeLoop)
	if err != nil {
		t.Fatalf("RecoverRuns (pass 1): %v", err)
	}
	if n1 != 1 {
		t.Fatalf("pass 1: RecoverRuns n = %d, want 1", n1)
	}
	finalStatus := pollRunStatus(t, db, runID, 5*time.Second)
	if finalStatus != "DONE" {
		t.Fatalf("pass 1: run.status = %q, want DONE", finalStatus)
	}

	// Second recovery pass over the SAME database: ListRunningRuns must find
	// nothing (the run is DONE now) — the factory must not even be called.
	n2, err := orchestrator.RecoverRuns(context.Background(), s,
		func(id core.RunID, _ core.WorkflowID, _ json.RawMessage) *orchestrator.Loop {
			t.Errorf("pass 2: makeLoop called for run %q — a DONE run must never be re-entered", id)
			return nil
		})
	if err != nil {
		t.Fatalf("RecoverRuns (pass 2): %v", err)
	}
	if n2 != 0 {
		t.Errorf("pass 2: RecoverRuns n = %d, want 0", n2)
	}

	stepID := runID + ":step1"
	var attemptCount int
	if err := db.QueryRow(`SELECT attempt_count FROM steps WHERE step_id = $1`, stepID).Scan(&attemptCount); err != nil {
		t.Fatalf("query attempt_count: %v", err)
	}
	if attemptCount != 1 {
		t.Errorf("step.attempt_count = %d, want 1 (incremented exactly once, by the single orphan claim)", attemptCount)
	}
	t.Logf("PASS — attempt_count = %d after two recovery passes (claimed exactly once)", attemptCount)

	var orphanedRows int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM attempts WHERE step_id = $1 AND failure_reason = 'orphaned'
	`, stepID).Scan(&orphanedRows); err != nil {
		t.Fatalf("count orphaned attempts: %v", err)
	}
	if orphanedRows != 1 {
		t.Errorf("orphaned attempt rows = %d, want 1 (never double-claimed)", orphanedRows)
	}
	t.Logf("PASS — exactly 1 attempt row carries failure_reason=orphaned")
}

// ── Mandatory test (c): crash between TX3 and TX4 ────────────────────────

// TestRecovery_CrashBetweenTX3AndTX4_DispatchesWithoutClaiming seeds a step
// whose sole attempt already reached a terminal FAILED state (simulating
// TX3 having already fired) with attempt_count below the retry limit, but no
// TX4 ever ran before the crash (current_attempt_id still points at the
// FAILED attempt — TX3 never moves that pointer). Recovery's unconditional
// orphan-claim call must be a CAS no-op (the attempt is not RUNNING), and
// recovery must proceed straight to TX4 + dispatch WITHOUT adding any new
// failure/orphaned record (whitepaper §7.1: "recovery's budget check picks
// this up; the window is claimed").
func TestRecovery_CrashBetweenTX3AndTX4_DispatchesWithoutClaiming(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		retryLimit      = 3
		wfID            = "wf-rec-tx3tx4"
		runID           = "run-rec-tx3tx4"
		preCrashAttempt = "30000000-0000-4000-8000-000000000003"
	)
	seedWorkflowAndRun(t, db, wfID, runID, "static", staticPlannerConfigWithRetryLimit(retryLimit))
	// attempt_count = 1 (as if one real TX3 already ran), the one attempt
	// already FAILED(timeout) — but no TX4 followed before the crash.
	plantRunningStep(t, db, runID, "step1", 1, 1, preCrashAttempt, "FAILED", "timeout")

	s := store.New(db)
	planner := &countingDonePlanner{minHistory: 1}
	transport := &alwaysSucceedTransport{}

	n, err := orchestrator.RecoverRuns(context.Background(), s,
		func(id core.RunID, wf core.WorkflowID, input json.RawMessage) *orchestrator.Loop {
			return &orchestrator.Loop{
				RunID:          id,
				WorkflowID:     wf,
				WorkflowInput:  input,
				Store:          s,
				Transport:      transport,
				PlannerFactory: staticFactory(planner),
			}
		})
	if err != nil {
		t.Fatalf("RecoverRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("RecoverRuns n = %d, want 1", n)
	}

	finalStatus := pollRunStatus(t, db, runID, 5*time.Second)
	if finalStatus != "DONE" {
		t.Errorf("run.status = %q, want DONE", finalStatus)
	}

	stepID := runID + ":step1"

	// attempt_count must be UNCHANGED by the claim attempt (still 1 from the
	// pre-crash TX3) — the claim was a CAS no-op, not a fresh count++.
	var attemptCount int
	if err := db.QueryRow(`SELECT attempt_count FROM steps WHERE step_id = $1`, stepID).Scan(&attemptCount); err != nil {
		t.Fatalf("query attempt_count: %v", err)
	}
	if attemptCount != 1 {
		t.Errorf("step.attempt_count = %d, want 1 (claim must be a no-op; only the pre-seeded TX3 count remains)", attemptCount)
	}
	t.Logf("PASS — attempt_count = %d (unchanged by the no-op claim)", attemptCount)

	// No attempt carries failure_reason='orphaned' — recovery claimed
	// nothing.
	var orphanedRows int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM attempts WHERE step_id = $1 AND failure_reason = 'orphaned'
	`, stepID).Scan(&orphanedRows); err != nil {
		t.Fatalf("count orphaned attempts: %v", err)
	}
	if orphanedRows != 0 {
		t.Errorf("orphaned attempt rows = %d, want 0 (recovery must not claim an already-FAILED attempt)", orphanedRows)
	}
	t.Logf("PASS — 0 attempt rows carry failure_reason=orphaned (nothing claimed)")

	// Exactly 2 attempts total: the pre-crash FAILED one + the new DONE
	// redispatch (TX4 fired, dispatched successfully).
	var totalAttempts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE step_id = $1`, stepID).Scan(&totalAttempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if totalAttempts != 2 {
		t.Errorf("total attempts = %d, want 2 (1 pre-crash FAILED + 1 new redispatch)", totalAttempts)
	}
	t.Logf("PASS — %d total attempts (pre-crash FAILED preserved + new redispatch)", totalAttempts)

	if transport.calls != 1 {
		t.Errorf("transport.Dispatch called %d times, want 1", transport.calls)
	}
	t.Logf("PASS — run.status = DONE; transport dispatched exactly once")
}

// ── Mandatory test (d): planner asked exactly once per step, across a
//    simulated crash ────────────────────────────────────────────────────

// TestRecovery_PlannerAskedExactlyOncePerStep seeds a 2-step run: step1 is
// DONE (pre-crash), step2 is RUNNING with a single RUNNING attempt (a
// live-dispatch crash for step2 — its decision (Barrier 1) is already
// persisted). Recovery must NOT re-ask the planner for either step1 (already
// resolved) or step2 (already decided, just re-dispatch it) — the planner is
// asked exactly ONCE total: after step2 resolves, to learn what comes next.
func TestRecovery_PlannerAskedExactlyOncePerStep(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		retryLimit     = 3
		wfID           = "wf-rec-once"
		runID          = "run-rec-once"
		doneAttempt    = "40000000-0000-4000-8000-000000000004"
		crashedAttempt = "40000000-0000-4000-8000-000000000005"
	)
	seedWorkflowAndRun(t, db, wfID, runID, "static", staticPlannerConfigWithRetryLimit(retryLimit))
	plantDoneStep(t, db, runID, "step1", 1, doneAttempt)
	plantRunningStep(t, db, runID, "step2", 2, 0, crashedAttempt, "RUNNING", "")

	s := store.New(db)
	// minHistory=2: once step2 also resolves, history has 2 entries — the
	// planner may legitimately be asked ("what's next?") exactly then.
	planner := &countingDonePlanner{minHistory: 2}
	transport := &alwaysSucceedTransport{}

	n, err := orchestrator.RecoverRuns(context.Background(), s,
		func(id core.RunID, wf core.WorkflowID, input json.RawMessage) *orchestrator.Loop {
			return &orchestrator.Loop{
				RunID:          id,
				WorkflowID:     wf,
				WorkflowInput:  input,
				Store:          s,
				Transport:      transport,
				PlannerFactory: staticFactory(planner),
			}
		})
	if err != nil {
		t.Fatalf("RecoverRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("RecoverRuns n = %d, want 1", n)
	}

	finalStatus := pollRunStatus(t, db, runID, 5*time.Second)
	if finalStatus != "DONE" {
		t.Fatalf("run.status = %q, want DONE", finalStatus)
	}

	if len(planner.calls) != 1 {
		t.Fatalf("planner.Decide called %d times, want exactly 1; call history (history-len per call): %v",
			len(planner.calls), planner.calls)
	}
	if planner.calls[0] != 2 {
		t.Errorf("the single Decide call had history len %d, want 2 (both steps already resolved)", planner.calls[0])
	}
	t.Logf("PASS — planner.Decide called exactly once (history len=%d): step1 (pre-DONE) and step2 "+
		"(pre-decided, only re-dispatched) were never re-asked", planner.calls[0])

	if transport.calls != 1 {
		t.Errorf("transport.Dispatch called %d times, want 1 (only step2's recovery redispatch)", transport.calls)
	}
	t.Logf("PASS — transport dispatched exactly once (step2's recovery redispatch)")
}
