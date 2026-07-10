package orchestrator_test

// Session 6.5 tests for Loop.ResumeReplayedStep — the entry point that
// resumes a step whose attempt was JUST created by ReplayWorkerSide (TX5)
// and never dispatched, WITHOUT going through Run()'s crash-recovery
// orphan-claim check (which would burn the retry budget TX5 just reset —
// see internal/orchestrator/replay.go's doc comment and the Session 6.5
// bug report for the failure this fixes: a worker-side DLQ replay could
// re-DLQ the run before a worker was ever contacted).
//
// Require a real Postgres DB — skipped when TEST_DATABASE_URL is unset.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/orchestrator"
	"github.com/aaronwu000/stateflow/internal/store"
)

// TestResumeReplayedStep_DispatchesWithoutOrphanClaim seeds a step directly
// in the exact shape ReplayWorkerSide (TX5) leaves behind — status RUNNING,
// attempt_count=0, one RUNNING attempt that was never dispatched to any
// worker — using the same plantRunningStep helper the crash-recovery tests
// use (attemptCount=0 here is the load-bearing difference from a genuine
// crash-orphan seed, which always has attemptCount>=1 by the time a real
// crash could occur — see plantRunningStep's own doc comment). It then
// calls ResumeReplayedStep directly (bypassing the HTTP layer/the TX5 call
// itself, which is covered by internal/store's own TX5 tests and
// internal/api's TestAPI_DLQ_ReplayWorkerSide) and asserts:
//   - the seeded attempt is dispatched and resolves DONE — no
//     failure_reason='orphaned' row is EVER written for it (the bug this
//     session fixes: Run()'s generic recovery check would have claimed it);
//   - attempt_count stays 0 — TX2 (success checkpoint) never writes
//     attempt_count, only TX3 (failure) and TX5 (reset) do;
//   - exactly one attempt row exists (no phantom second attempt from a
//     TX4 that should never have fired);
//   - the run proceeds to completion via the normal steady-state loop
//     afterward — proving ResumeReplayedStep falls through to
//     driveSteadyState instead of just resolving the one step and
//     stopping short.
func TestResumeReplayedStep_DispatchesWithoutOrphanClaim(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		retryLimit   = 2
		wfID         = "wf-replay-resume"
		runID        = "run-replay-resume"
		freshAttempt = "50000000-0000-4000-8000-000000000006"
	)
	seedWorkflowAndRun(t, db, wfID, runID, "static", staticPlannerConfigWithRetryLimit(retryLimit))
	// The exact TX5 aftermath: step RUNNING, attempt_count=0, ONE RUNNING
	// attempt that has never been dispatched to any worker.
	plantRunningStep(t, db, runID, "process", 1, 0, freshAttempt, "RUNNING", "")

	s := store.New(db)
	transport := &alwaysSucceedTransport{}
	planner := &countingDonePlanner{minHistory: 1}

	loop := &orchestrator.Loop{
		RunID:          core.RunID(runID),
		WorkflowID:     core.WorkflowID(wfID),
		WorkflowInput:  json.RawMessage(`{}`),
		Store:          s,
		Transport:      transport,
		PlannerFactory: staticFactory(planner),
	}

	if err := loop.ResumeReplayedStep(context.Background()); err != nil {
		t.Fatalf("ResumeReplayedStep() returned error: %v", err)
	}

	finalStatus := pollRunStatus(t, db, runID, 5*time.Second)
	if finalStatus != "DONE" {
		t.Fatalf("run.status = %q, want DONE", finalStatus)
	}
	t.Logf("PASS — run.status = DONE after ResumeReplayedStep")

	stepID := runID + ":process"

	// The core bug assertion: no phantom orphaned attempt.
	var orphanedRows int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM attempts WHERE step_id = $1 AND failure_reason = 'orphaned'
	`, stepID).Scan(&orphanedRows); err != nil {
		t.Fatalf("count orphaned attempts: %v", err)
	}
	if orphanedRows != 0 {
		t.Errorf("orphaned attempt rows = %d, want 0 (ResumeReplayedStep must never claim the freshly-replayed attempt as orphaned)", orphanedRows)
	}
	t.Logf("PASS — 0 attempt rows carry failure_reason=orphaned")

	// Exactly one attempt row: the seeded one, now DONE.
	var attemptRows int
	var attemptStatus string
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE step_id = $1`, stepID).Scan(&attemptRows); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM attempts WHERE attempt_id = $1::uuid`, freshAttempt).Scan(&attemptStatus); err != nil {
		t.Fatalf("query seeded attempt status: %v", err)
	}
	if attemptRows != 1 {
		t.Errorf("attempt row count = %d, want 1 (the seeded attempt, dispatched directly — no second attempt created)", attemptRows)
	}
	if attemptStatus != "DONE" {
		t.Errorf("seeded attempt status = %q, want DONE", attemptStatus)
	}
	t.Logf("PASS — exactly 1 attempt row (the TX5-seeded one), now DONE")

	// attempt_count must remain 0 — TX2 (success checkpoint) never
	// increments it; only TX3 (failure) and TX5 (reset) touch that column.
	var attemptCount int
	var stepStatus string
	if err := db.QueryRow(`SELECT status, attempt_count FROM steps WHERE step_id = $1`, stepID).
		Scan(&stepStatus, &attemptCount); err != nil {
		t.Fatalf("query step: %v", err)
	}
	if stepStatus != "DONE" {
		t.Errorf("step.status = %q, want DONE", stepStatus)
	}
	if attemptCount != 0 {
		t.Errorf("step.attempt_count = %d, want 0 (TX2 never writes attempt_count)", attemptCount)
	}
	t.Logf("PASS — step.status=DONE, attempt_count=%d (untouched by the successful dispatch)", attemptCount)

	if transport.calls != 1 {
		t.Errorf("transport.Dispatch called %d times, want 1 (the seeded attempt, dispatched exactly once)", transport.calls)
	}
	t.Logf("PASS — transport dispatched exactly once")

	if len(planner.calls) != 1 {
		t.Errorf("planner.Decide called %d times, want 1 (asked once after the replayed step resolved)", len(planner.calls))
	}
	t.Logf("PASS — planner asked exactly once after the replayed step resolved (driveSteadyState ran)")
}

// TestResumeReplayedStep_RefusesNonFreshAttempt verifies the safety guard:
// if the pending step's attempt_count is not 0 (i.e. this is NOT the
// immediate aftermath of a TX5 reset — e.g. mistakenly called on an
// ordinary crash-in-flight step), ResumeReplayedStep refuses to dispatch
// and returns an error rather than silently behaving like the
// crash-recovery path (which would defeat the entire point of having a
// separate entry point). Neither the transport nor the planner may be
// touched.
func TestResumeReplayedStep_RefusesNonFreshAttempt(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		retryLimit  = 3
		wfID        = "wf-replay-guard"
		runID       = "run-replay-guard"
		liveAttempt = "50000000-0000-4000-8000-000000000007"
	)
	seedWorkflowAndRun(t, db, wfID, runID, "static", staticPlannerConfigWithRetryLimit(retryLimit))
	// attempt_count=1 — NOT the TX5 invariant (always 0 immediately after a
	// reset). This simulates an ordinary in-flight step, not a replay.
	plantRunningStep(t, db, runID, "process", 1, 1, liveAttempt, "RUNNING", "")

	s := store.New(db)
	loop := &orchestrator.Loop{
		RunID:          core.RunID(runID),
		WorkflowID:     core.WorkflowID(wfID),
		WorkflowInput:  json.RawMessage(`{}`),
		Store:          s,
		Transport:      &panicTransport{t: t},
		PlannerFactory: staticFactory(&panicPlanner{t: t}),
	}

	err := loop.ResumeReplayedStep(context.Background())
	if err == nil {
		t.Fatal("ResumeReplayedStep() returned nil error, want a refusal error (attempt_count != 0)")
	}
	t.Logf("PASS — ResumeReplayedStep refused a non-fresh attempt: %v", err)

	var attemptCount int
	var stepStatus string
	stepID := runID + ":process"
	if err := db.QueryRow(`SELECT status, attempt_count FROM steps WHERE step_id = $1`, stepID).
		Scan(&stepStatus, &attemptCount); err != nil {
		t.Fatalf("query step: %v", err)
	}
	if stepStatus != "RUNNING" || attemptCount != 1 {
		t.Errorf("step state changed by the refused call: status=%q attempt_count=%d, want RUNNING/1 (untouched)", stepStatus, attemptCount)
	}
	t.Logf("PASS — step state untouched by the refusal (status=%q, attempt_count=%d)", stepStatus, attemptCount)
}
