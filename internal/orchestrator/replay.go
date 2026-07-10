// Package orchestrator — the DLQ worker-side replay entry point.
//
// Authoritative: docs/StateFlow_Whitepaper_v1_0.md §11 (the DLQ and
// replay — TX5's prescribed sequence is "reset the budget, then dispatch
// the worker"), §19 (Ledger TX5).
//
// Session 6.5 background: Session 6 wired POST /dlq/{id}/replay's
// worker-side branch to call TX5 (ReplayWorkerSide) and then re-enter
// through the SAME generic entry point used for a brand-new run and for
// crash recovery (Loop.Run). That is wrong for this one case: Run()'s
// one-time recovery check (whitepaper §8.3) unconditionally treats any
// PendingStep it finds as a possibly-orphaned attempt whose fate is
// UNKNOWN — appropriate after an actual orchestrator crash, where nothing
// in the world is waiting on that attempt. But TX5 just created a brand
// new attempt that has never been dispatched to any worker; its fate is
// not unknown, it is "not started yet". Routing it through Run() burns one
// unit of the retry budget TX5 just reset (RecordFailure(orphaned)) before
// a worker is ever contacted — for retry_limit=1 this DLQs the run again
// without ever dispatching, making the replay button decorative (exactly
// the failure whitepaper §11 says the reset exists to prevent, reproduced
// by a different mechanism).
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
)

// ResumeReplayedStep resumes a run whose single worker-side-DLQ'd step was
// JUST reset by ReplayWorkerSide (Ledger TX5) — attempt_count=0, step and
// run RUNNING, a fresh attempt with current_attempt_id already pointing at
// it, and NO dispatch performed yet (whitepaper §11's TX5 entry ends with
// "→ dispatch the worker", which is exactly what this method does).
//
// Unlike Run(), this entry point does NOT perform the crash-recovery
// orphan-claim check (whitepaper §8.3) — that check exists for a step whose
// attempt's fate is genuinely unknown after a process crash. Here the
// caller (handleDLQReplay) knows the attempt is fresh: it was created by
// the TX5 call that immediately precedes this one, in the same request. To
// make sure the two never drift apart, ResumeReplayedStep re-reads the
// frontier and asserts AttemptCount == 0, which is TX5's own invariant (§19
// TX5: "count→0 ... all writes in one transaction") and holds for no other
// path that produces a RUNNING step with a RUNNING attempt (a genuine
// crash-orphan candidate always has AttemptCount >= 1, since the step could
// only reach RUNNING via TX1 with count=0-then-dispatch or a prior TX3/TX4
// cycle that incremented it) — if that ever fails to hold, this method
// refuses to guess and returns an error rather than silently reusing the
// crash-recovery path (which would reintroduce the bug this method exists
// to fix).
//
// After the one pending attempt resolves (DONE or DLQ'd via the normal
// TX2/TX3 path), execution falls into the same steady-state loop Run() uses
// — the run keeps going exactly as it would after any other step
// completes.
func (l *Loop) ResumeReplayedStep(ctx context.Context) error {
	def, err := l.Store.GetWorkflow(ctx, l.WorkflowID)
	if err != nil {
		return fmt.Errorf("loop: ResumeReplayedStep: GetWorkflow %q: %w", l.WorkflowID, err)
	}

	retryLimit := def.RetryLimit
	if retryLimit <= 0 {
		retryLimit = defaultRetryLimit
	}

	retry := l.Retry
	if retry == nil {
		retry = DefaultRetryPolicy()
	}

	pl, err := l.buildPlanner(def)
	if err != nil {
		return fmt.Errorf("loop: ResumeReplayedStep: build planner for workflow %q: %w", l.WorkflowID, err)
	}

	frontier, err := l.Store.LoadFrontier(ctx, l.RunID)
	if err != nil {
		return fmt.Errorf("loop: ResumeReplayedStep: LoadFrontier: %w", err)
	}

	if frontier.PendingStep == nil {
		return fmt.Errorf(
			"loop: ResumeReplayedStep: run %q has no pending step — TX5 must run immediately before this call",
			l.RunID)
	}
	if frontier.AttemptCount != 0 {
		return fmt.Errorf(
			"loop: ResumeReplayedStep: run %q pending step %q has attempt_count=%d, want 0 — "+
				"this is not a freshly-replayed (TX5) attempt; refusing to dispatch it as one",
			l.RunID, frontier.PendingStep.Name, frontier.AttemptCount)
	}

	spec := *frontier.PendingStep
	stepID := core.StepID(fmt.Sprintf("%s:%s", l.RunID, spec.Name))

	slog.Info("[REPLAY] resuming worker-side-replayed step",
		"run_id", string(l.RunID),
		"step", spec.Name,
		"attempt_id", string(frontier.PendingAttemptID))

	dlqed, err := l.dispatchAndResolve(ctx, retry, def, retryLimit, stepID, spec, frontier.PendingAttemptID, time.Now())
	if err != nil {
		return fmt.Errorf("loop: ResumeReplayedStep: dispatchAndResolve %q: %w", stepID, err)
	}
	if dlqed {
		return nil
	}

	return l.driveSteadyState(ctx, def, pl, retry, retryLimit)
}
