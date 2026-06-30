// Package orchestrator implements the durable driver loop.
// Authoritative design: whitepaper §2.3 (loop pseudocode), §2.2 (two write barriers),
// DESIGN.md §9.1 (Checkpoint paths), CLAUDE.md (barrier invariants).
package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/transport"
)

// Store extends core.StateStore with operations the loop requires but that are
// not yet in the minimal StateStore interface. These will be reviewed for
// promotion to core.StateStore when the interface is finalized with the project owner.
type Store interface {
	core.StateStore

	// RecordAttemptStart creates an attempts row (RUNNING) and updates
	// steps.current_attempt_id before each Dispatch call.
	RecordAttemptStart(run core.RunID, step core.StepSpec, attemptID core.AttemptID) error

	// ResetToDecided transitions a step from FAILED back to DECIDED so that
	// crash recovery can see it as a pending re-dispatch between retry attempts.
	ResetToDecided(run core.RunID, step core.StepSpec) error

	// MarkDLQ sets the step to DLQ, inserts a dead_letter_queue row, and marks
	// the run FAILED. Called when retries are exhausted.
	MarkDLQ(run core.RunID, step core.StepSpec, reason string, lastError string) error

	// MarkRunDone sets runs.status = DONE when the planner returns "done".
	MarkRunDone(run core.RunID) error

	// MarkRunFailed sets runs.status = FAILED when the planner declares failure
	// without a specific step being at fault.
	MarkRunFailed(run core.RunID, reason string) error

	// MarkPlannerFailedDLQ writes a dead_letter_queue entry with reason='planner_failed'
	// (step_id=NULL) and marks the run FAILED. Called when the planner returns "fail".
	MarkPlannerFailedDLQ(run core.RunID, detail string) error
}

// Loop is the durable driver loop for a single run.
//
// It is safe to construct and call Run again after a crash: PendingDecision
// detects the already-persisted decision and re-dispatches it without re-asking
// the planner — this is the recovery entry point into the normal loop body.
type Loop struct {
	RunID         core.RunID
	WorkflowInput json.RawMessage
	Store         Store
	Planner       core.NextStepPlanner
	Transport     core.WorkerTransport
	Retry         core.RetryPolicy
}

// Run drives the run to completion (or DLQ) by iterating the durable loop:
//
//	for each step:
//	    decide (planner or persisted pending decision)
//	    Barrier 1: PutDecision commits before Dispatch
//	    Barrier 2: Checkpoint commits before next Decide
//
// The two write barriers are enforced unconditionally and in order.
// The loop is oblivious to sync vs async transport — it calls Dispatch and
// receives a Result regardless of how the worker communicated.
func (l *Loop) Run(ctx context.Context) error {
	for {
		// ── Get the next step to dispatch ──────────────────────────────────
		//
		// Check for a persisted-but-undelivered decision first (recovery path).
		// If found, re-dispatch it — do NOT re-ask the planner (Barrier 1 already
		// fired; the decision is durable). This is recovery rules 1 and 2 from
		// CLAUDE.md: RUNNING-no-output or DECIDED-no-output → re-dispatch.
		pending, err := l.Store.PendingDecision(l.RunID)
		if err != nil {
			return fmt.Errorf("loop: PendingDecision: %w", err)
		}

		var spec core.StepSpec
		var alreadyPersisted bool

		if pending != nil {
			spec = *pending
			alreadyPersisted = true
		} else {
			// Normal path: read the frontier and ask the planner what's next.
			frontier, err := l.Store.LoadFrontier(l.RunID)
			if err != nil {
				return fmt.Errorf("loop: LoadFrontier: %w", err)
			}

			history := frontier.History
			if history == nil {
				history = []core.HistoryEntry{}
			}
			state := core.RunState{
				RunID:         l.RunID,
				WorkflowInput: l.WorkflowInput,
				History:       history,
			}

			decision, err := l.Planner.Decide(ctx, state)
			if err != nil {
				return fmt.Errorf("loop: planner.Decide: %w", err)
			}

			switch decision.Status {
			case "done":
				// Planner says the run is complete.
				if err := l.Store.MarkRunDone(l.RunID); err != nil {
					return fmt.Errorf("loop: MarkRunDone: %w", err)
				}
				return nil

			case "fail":
				// Planner declares the run unworkable — no specific step to blame.
				// Writes a DLQ entry (reason=planner_failed, step_id=NULL) and marks
				// the run FAILED so the operator can investigate.
				if err := l.Store.MarkPlannerFailedDLQ(l.RunID, "planner declared run unworkable"); err != nil {
					return fmt.Errorf("loop: MarkPlannerFailedDLQ: %w", err)
				}
				return fmt.Errorf("planner declared run %q failed", l.RunID)

			case "continue":
				if decision.Step == nil {
					return fmt.Errorf("loop: planner returned continue with nil step")
				}
				spec = *decision.Step

			default:
				return fmt.Errorf("loop: unknown planner status %q", decision.Status)
			}

			// ── Barrier 1: persist decision BEFORE dispatch ─────────────────
			// If the process crashes after this line, recovery reads this DECIDED
			// row and re-dispatches it without re-asking the planner.
			if err := l.Store.PutDecision(l.RunID, spec); err != nil {
				return fmt.Errorf("loop: PutDecision %q: %w", spec.Name, err)
			}
		}

		// ── Dispatch with retry ────────────────────────────────────────────
		for attemptNum := 1; ; attemptNum++ {
			_ = alreadyPersisted // used only to skip PutDecision above; dispatch always creates new attempt

			attemptID := newAttemptID()

			// Record the attempt start BEFORE calling Dispatch. This sets
			// steps.current_attempt_id so the async callback handler can validate
			// it, and creates the attempts row for audit history.
			if err := l.Store.RecordAttemptStart(l.RunID, spec, attemptID); err != nil {
				return fmt.Errorf("loop: RecordAttemptStart %q attempt %d: %w", spec.Name, attemptNum, err)
			}

			// Inject DispatchMeta so AsyncTransport can route the callback to
			// the right channel. SyncTransport ignores the meta.
			stepID := core.StepID(fmt.Sprintf("%s:%s", l.RunID, spec.Name))
			dispatchCtx := transport.WithDispatchMeta(ctx, transport.DispatchMeta{
				StepID:    stepID,
				AttemptID: attemptID,
			})
			result, err := l.Transport.Dispatch(dispatchCtx, spec)
			if err != nil {
				// Transport-level error (e.g., connection refused before response).
				// Treat as failed so Barrier 2 still fires.
				result = core.Result{Status: "failed", Error: err.Error()}
			}

			// ── Barrier 2: persist result BEFORE next Decide ────────────────
			// If the process crashes after this line, the step is DONE (or FAILED)
			// and the next loop iteration will ask the planner with the updated frontier.
			if err := l.Store.Checkpoint(l.RunID, spec, result); err != nil {
				return fmt.Errorf("loop: Checkpoint %q: %w", spec.Name, err)
			}

			if result.Status == "done" {
				break // step complete; continue outer loop to get next step
			}

			// Worker explicitly reported failure — consult the retry policy.
			delay, toDLQ := l.Retry.Next(attemptNum, fmt.Errorf("%s", result.Error))
			if toDLQ {
				if err := l.Store.MarkDLQ(l.RunID, spec, "retry_exhausted", result.Error); err != nil {
					return fmt.Errorf("loop: MarkDLQ %q: %w", spec.Name, err)
				}
				return fmt.Errorf("step %q exhausted retries after %d attempt(s): %s",
					spec.Name, attemptNum, result.Error)
			}

			// Reset step to DECIDED before sleeping. This ensures crash recovery
			// during the sleep window sees a re-dispatchable pending decision rather
			// than a stuck FAILED state (which has no recovery rule in CLAUDE.md).
			if err := l.Store.ResetToDecided(l.RunID, spec); err != nil {
				return fmt.Errorf("loop: ResetToDecided %q: %w", spec.Name, err)
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		// Step is DONE. Loop back to top: PendingDecision → nil → ask planner.
		alreadyPersisted = false
	}
}

// newAttemptID generates a random UUID v4 for a single dispatch attempt.
// Each call to Dispatch gets a fresh attempt_id, even for retries of the same step.
func newAttemptID() core.AttemptID {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("newAttemptID: crypto/rand failed: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // UUID version 4
	b[8] = (b[8] & 0x3f) | 0x80 // UUID variant bits
	h := fmt.Sprintf("%x", b)
	return core.AttemptID(h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32])
}
