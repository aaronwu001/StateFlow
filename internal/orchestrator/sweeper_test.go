package orchestrator_test

// Integration tests for the periodic orphan sweeper (Session 18, registry
// #4, whitepaper §18). Require a real Postgres DB — skipped when
// TEST_DATABASE_URL is unset, exactly like every other file in this
// package's integration suite.
//
// These cover the two mandatory cases from the session spec:
//   (a) a run whose driving goroutine "died" (no live-registry entry) while
//       run=RUNNING, last_step=RUNNING must be claimed and re-dispatched by
//       the next sweep tick — no process restart required.
//   (b) a run WITH a live goroutine actively driving it must NOT be touched
//       by a concurrent sweep tick — no duplicate orphan-claim, no
//       double-dispatch.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/orchestrator"
	"github.com/aaronwu000/stateflow/internal/store"
)

// blockingTransport blocks inside Dispatch until release is closed, then
// returns done. Used to hold a real Loop.Run goroutine "live" — genuinely
// inside dispatchAndResolve, mid in-flight dispatch — for exactly as long as
// a test needs, so the sweeper's live-registry skip can be exercised against
// a REAL live goroutine rather than a simulated one.
type blockingTransport struct {
	release chan struct{}
	calls   atomic.Int64
}

func (t *blockingTransport) Dispatch(ctx context.Context, _ core.StepSpec) (core.Result, error) {
	t.calls.Add(1)
	select {
	case <-t.release:
	case <-ctx.Done():
		return core.Result{}, ctx.Err()
	}
	return core.Result{Status: core.ResultStatusDone, Output: json.RawMessage(`{"ok":true}`)}, nil
}

// ── Mandatory test (a): sweep claims an orphan with no live goroutine ──────

// TestSweeper_ClaimsOrphanWithNoLiveGoroutine seeds the exact same
// "live-dispatch crash" DB state the recovery tests use (run=RUNNING,
// last_step=RUNNING, one RUNNING attempt) but WITHOUT ever calling
// RecoverRuns or Loop.Run for it — nothing in this test process has ever
// registered this run_id as live, which is exactly what "the driving
// goroutine died" looks like from the sweeper's point of view (it has no
// way to distinguish "a goroutine that never existed" from "a goroutine that
// crashed"; both present as a RUNNING run/step with no live-registry entry).
// A single orchestrator.SweepOnce call must claim the orphan (TX3),
// redispatch (TX4), and drive the run to completion — proving recovery does
// not require a process restart, only the next sweep tick.
func TestSweeper_ClaimsOrphanWithNoLiveGoroutine(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		retryLimit     = 3
		wfID           = "wf-sweep-orphan"
		runID          = "run-sweep-orphan"
		crashedAttempt = "50000000-0000-4000-8000-000000000001"
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

	claimed, err := orchestrator.SweepOnce(context.Background(), s, makeLoop)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("SweepOnce claimed = %d, want 1", claimed)
	}
	t.Logf("PASS — SweepOnce claimed=%d", claimed)

	finalStatus := pollRunStatus(t, db, runID, 5*time.Second)
	if finalStatus != "DONE" {
		t.Fatalf("run.status = %q, want DONE", finalStatus)
	}
	t.Logf("PASS — run.status = DONE (recovered by a sweep tick, no process restart)")

	stepID := runID + ":step1"
	var attemptCount int
	if err := db.QueryRow(`SELECT attempt_count FROM steps WHERE step_id = $1`, stepID).Scan(&attemptCount); err != nil {
		t.Fatalf("query attempt_count: %v", err)
	}
	if attemptCount != 1 {
		t.Errorf("step.attempt_count = %d, want 1 (exactly one orphan claim)", attemptCount)
	}

	var orphanedRows int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM attempts WHERE step_id = $1 AND failure_reason = 'orphaned'
	`, stepID).Scan(&orphanedRows); err != nil {
		t.Fatalf("count orphaned attempts: %v", err)
	}
	if orphanedRows != 1 {
		t.Errorf("orphaned attempt rows = %d, want 1", orphanedRows)
	}
	t.Logf("PASS — exactly 1 attempt row carries failure_reason=orphaned; attempt_count=%d", attemptCount)

	// A second tick over the same (now DONE) DB state must find nothing to
	// claim — re-entrancy holds for the sweeper exactly as it does for
	// RecoverRuns (whitepaper §8.3).
	claimed2, err := orchestrator.SweepOnce(context.Background(), s, func(id core.RunID, _ core.WorkflowID, _ json.RawMessage) *orchestrator.Loop {
		t.Errorf("second sweep tick: makeLoop called for run %q — a DONE run must never be re-entered", id)
		return nil
	})
	if err != nil {
		t.Fatalf("SweepOnce (second tick): %v", err)
	}
	if claimed2 != 0 {
		t.Errorf("second sweep tick claimed = %d, want 0", claimed2)
	}
	t.Logf("PASS — a second sweep tick over the same DB state claims nothing (re-entrant)")
}

// ── Mandatory test (b): sweep skips a run with a live goroutine ────────────

// TestSweeper_SkipsRunWithLiveGoroutine starts a REAL Loop.Run goroutine for
// a brand-new run and holds it mid-dispatch (blockingTransport) so it is
// genuinely registered live (Loop.Run marks liveRuns at entry — see
// sweeper.go) for the whole window this test drives a concurrent SweepOnce
// call. That call must skip the run entirely: makeLoop must never be invoked
// for it, no second dispatch must happen, and no orphaned-failure_reason
// attempt may appear once the live goroutine is finally allowed to finish.
func TestSweeper_SkipsRunWithLiveGoroutine(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		wfID  = "wf-sweep-live"
		runID = "run-sweep-live"
	)
	seedWorkflowAndRun(t, db, wfID, runID, "static", staticPlannerConfigWithRetryLimit(3))

	s := store.New(db)
	bt := &blockingTransport{release: make(chan struct{})}
	step := &core.StepSpec{
		Name: "step1", WorkerURL: "http://stub/step1", Mode: core.StepModeSync,
		TimeoutSeconds: 30, Input: json.RawMessage(`{}`),
	}
	planner := &singleStepPlanner{step: step}

	loop := &orchestrator.Loop{
		RunID:          core.RunID(runID),
		WorkflowID:     core.WorkflowID(wfID),
		WorkflowInput:  json.RawMessage(`{}`),
		Store:          s,
		Transport:      bt,
		PlannerFactory: staticFactory(planner),
	}

	runDone := make(chan error, 1)
	go func() { runDone <- loop.Run(context.Background()) }()

	// Wait until the live goroutine has actually reached Dispatch (proves
	// Barrier 1 — TX1 — already committed AND liveRuns.mark(runID) already
	// fired, since marking happens at Run()'s entry, well before TX1).
	deadline := time.Now().Add(5 * time.Second)
	for bt.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the live goroutine to reach Dispatch")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("PASS — live goroutine is mid-dispatch (Dispatch called, blocked on release)")

	// A concurrent sweep tick must skip this run entirely.
	claimed, err := orchestrator.SweepOnce(context.Background(), s,
		func(id core.RunID, _ core.WorkflowID, _ json.RawMessage) *orchestrator.Loop {
			t.Errorf("SweepOnce: makeLoop called for run %q — it has a live goroutine and must be skipped", id)
			return nil
		})
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if claimed != 0 {
		t.Errorf("SweepOnce claimed = %d, want 0 (run has a live goroutine)", claimed)
	}
	t.Logf("PASS — concurrent SweepOnce claimed=0, makeLoop never called for the live run")

	// Let the live goroutine finish normally.
	close(bt.release)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("loop.Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loop.Run did not return after release")
	}

	finalStatus := pollRunStatus(t, db, runID, 5*time.Second)
	if finalStatus != "DONE" {
		t.Fatalf("run.status = %q, want DONE", finalStatus)
	}

	stepID := runID + ":step1"
	var totalAttempts, orphanedRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE step_id = $1`, stepID).Scan(&totalAttempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM attempts WHERE step_id = $1 AND failure_reason = 'orphaned'
	`, stepID).Scan(&orphanedRows); err != nil {
		t.Fatalf("count orphaned attempts: %v", err)
	}
	if totalAttempts != 1 {
		t.Errorf("total attempts = %d, want 1 (no phantom orphan-claim attempt from the concurrent sweep)", totalAttempts)
	}
	if orphanedRows != 0 {
		t.Errorf("orphaned attempt rows = %d, want 0", orphanedRows)
	}
	if bt.calls.Load() != 1 {
		t.Errorf("transport.Dispatch called %d times, want 1 (no double-dispatch race)", bt.calls.Load())
	}
	t.Logf("PASS — run.status=DONE; exactly 1 attempt total, 0 orphaned, 1 Dispatch call (no double-dispatch)")
}
