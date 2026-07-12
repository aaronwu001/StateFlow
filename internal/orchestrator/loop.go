// Package orchestrator implements the durable driver loop and crash
// recovery for the v1.0 three-state model.
//
// Authoritative: docs/StateFlow_Whitepaper_v1_0.md §5 (the step loop), §6
// (the timeout doctrine), §7 (failure classification, retry, and budgets),
// §8 (invariants, the combination table, and recovery), §19 (the Atomic
// Transaction Ledger). On any divergence between this file and those
// sections, the whitepaper wins.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/planner"
	"github.com/aaronwu000/stateflow/internal/transport"
)

// defaultRetryLimit is the fallback worker-side retry budget X when a
// workflow's planner_config does not set retry_limit (0 ⇒ unset — see
// core.WorkflowDef.RetryLimit's doc comment; the whitepaper does not specify
// a default for X the way it does for the 60s timeout default, so this
// value is an implementation choice, not a documented constant — flagged in
// the Session 5 report). Chosen to match the pre-existing
// DefaultRetryPolicy() MaxRetries and the planner budget's own "3 total
// attempts" (whitepaper §7.2), for one consistent MVP number across the
// system.
const defaultRetryLimit = 3

// systemDefaultTimeout is the timeout ceiling applied when neither the step
// nor the workflow specifies one (whitepaper §6).
const systemDefaultTimeout = 60 * time.Second

// Planner call budget (whitepaper §7.2): 30s deadline per call, 3 total
// attempts, fixed — not configurable per workflow.
const (
	plannerCallTimeout = 30 * time.Second
	plannerMaxAttempts = 3
)

// Bounded-history size guard (registry #1, whitepaper §12.2; "Design
// decisions log" in the Phase 2 session prompts — the deliberately
// mechanical, protocol-free two-tier alternative to the multi-round
// `need_more` design the owner explicitly rejected as overcomplicated).
// Neither cap is workflow-configurable — both are fixed MVP constants, like
// the planner call budget above.
const (
	// historyEntryCapBytes is the per-entry cap on a single HistoryEntry's
	// marshaled Output. Over this, Output is replaced with a small pointer
	// object rather than sent verbatim.
	historyEntryCapBytes = 2048

	// historyTotalCapBytes is the cumulative cap on the marshaled History
	// array (summed over each entry's post-per-entry-cap Output size).
	// Budget is allocated most-recent-step-first, walking backward; entries
	// that don't fit are reduced to name+status only (Output omitted, not
	// even a pointer).
	historyTotalCapBytes = 51200
)

// Loop is the durable driver loop for a single run.
//
// A Loop value is constructed fresh for every Run() invocation — both the
// normal "start a new run" path and the crash-recovery path (RecoverRuns)
// build a new Loop{} per run. Run() itself reconstructs the planner instance
// from the workflow row on every call (whitepaper §12.1: "the loop and
// recovery reconstruct the planner instance from the workflow row every
// time") — Loop deliberately does NOT accept a pre-built NextStepPlanner as
// a field, so there is no process-global planner state to go stale across a
// restart or a workflow config update.
//
// Run() is also the single entry point for BOTH normal operation and crash
// recovery: "Recovery is NOT a special mode; it is the same loop entered
// from a DB-persisted mid-run state" (whitepaper §2). On entry, Run() checks
// LoadFrontier's PendingStep exactly once; if non-nil, it performs the
// three-step recovery action (§8.3) for that one step before falling into
// the steady-state ask-planner loop for the rest of the run. For a run
// entering Run() for the very first time (POST /runs), LoadFrontier's
// PendingStep is always nil (no steps exist yet), so this check is a no-op
// and execution falls straight into the steady-state loop.
type Loop struct {
	RunID         core.RunID
	WorkflowID    core.WorkflowID
	WorkflowInput json.RawMessage
	Store         core.StateStore
	Transport     core.WorkerTransport

	// Retry is optional; nil defaults to DefaultRetryPolicy(). It is used
	// ONLY to compute the delay before the next attempt (whitepaper §6: a
	// fixed 5s cooldown). The authoritative worker-side DLQ decision is made
	// by StateStore.RecordFailure (TX3) against the workflow's persisted
	// retry_limit — RetryPolicy.Next's toDLQ return value is not consulted
	// by the loop; see policy.go's doc comment.
	Retry core.RetryPolicy

	// PlannerFactory is an optional test seam: if set, it is called instead
	// of the default planner_type switch (buildPlanner) to construct the
	// NextStepPlanner for this run. Production callers should leave this
	// nil so the planner is genuinely reconstructed from the persisted
	// workflow row every time (whitepaper §12.1).
	PlannerFactory func(core.WorkflowDef) (core.NextStepPlanner, error)
}

// Run drives the run to completion (DONE) or the DLQ.
//
// The two write barriers (whitepaper §2) are enforced unconditionally and in
// order: Barrier 1 (TX1/TX4 commits before Dispatch), Barrier 2 (TX2 commits
// before the next planner ask). The loop is oblivious to sync vs async
// transport — it calls Dispatch and receives a Result regardless of how the
// worker communicated.
//
// Run returns nil whenever the run reaches ANY terminal outcome — DONE,
// worker-side DLQ, or planner-side DLQ — since none of those are error
// conditions for the caller (main.go / RecoverRuns just stop tracking the
// goroutine). Run returns a non-nil error only for genuine faults: a store
// read/write failure, ctx cancellation, or an internal invariant violation.
func (l *Loop) Run(ctx context.Context) error {
	// Register this run as "live" for the whole duration of this call, so
	// the periodic sweeper (sweeper.go, registry #4) never claims it out
	// from under us — see liveRuns' doc comment in sweeper.go for why this
	// is marked here rather than at each goroutine-launch call site.
	liveRuns.mark(l.RunID)
	defer liveRuns.unmark(l.RunID)

	def, err := l.Store.GetWorkflow(ctx, l.WorkflowID)
	if err != nil {
		return fmt.Errorf("loop: GetWorkflow %q: %w", l.WorkflowID, err)
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
		return fmt.Errorf("loop: build planner for workflow %q: %w", l.WorkflowID, err)
	}

	// ── One-time recovery check (whitepaper §8.2 row 2, §8.3) ──────────────
	//
	// LoadFrontier's PendingStep is non-nil only when a decision was
	// persisted (Barrier 1 fired) but the step has not yet resolved. Within
	// a single live Run() call this can only be true at the very top: every
	// step dispatched later in this same call is fully resolved (DONE or
	// DLQ'd) by dispatchAndResolve before the loop asks for the next one.
	frontier, err := l.Store.LoadFrontier(ctx, l.RunID)
	if err != nil {
		return fmt.Errorf("loop: LoadFrontier: %w", err)
	}

	if frontier.PendingStep != nil {
		spec := *frontier.PendingStep
		stepID := core.StepID(fmt.Sprintf("%s:%s", l.RunID, spec.Name))

		slog.Info("[RECOVERY] resuming run with a pending step",
			"run_id", string(l.RunID),
			"steps_done", len(frontier.History),
			"pending_step", spec.Name,
			"attempt_count", frontier.AttemptCount)

		// (a) Claim the orphan, unconditionally. CAS-A inside RecordFailure
		// makes this a safe no-op (ReportSuperseded) when the pending
		// attempt already resolved by another path before the crash — e.g.
		// a crash between TX3 and TX4 (whitepaper §7.1) leaves
		// current_attempt_id pointing at an already-FAILED attempt, and the
		// CAS predicate (status='RUNNING') simply won't match. Either way,
		// since LoadFrontier only populates PendingStep for a RUNNING step,
		// we know the step has not yet reached the DLQ.
		outcome, err := l.Store.RecordFailure(ctx, stepID, frontier.PendingAttemptID,
			core.FailureReasonOrphaned,
			"orchestrator restart: attempt orphaned (no live process was waiting on it)",
			retryLimit)
		if err != nil {
			return fmt.Errorf("loop: recovery RecordFailure(orphaned) for %q: %w", stepID, err)
		}

		// (b) Budget check: a FRESH claim that pushed the count to the
		// limit already DLQ'd inside (a) — this run is finished here.
		if outcome.Report == core.ReportCommitted && outcome.DLQed {
			slog.Info("[RECOVERY] orphan claim exhausted the retry budget; step and run DLQ'd",
				"run_id", string(l.RunID), "step", spec.Name)
			return nil
		}

		// (c) Below budget (whether via a fresh claim or because TX3 had
		// already run pre-crash): TX4 + dispatch, no retry delay — the
		// crash itself already provided more than enough cooldown
		// (whitepaper §5).
		newAttemptID, err := l.Store.StartNewAttempt(ctx, stepID)
		if err != nil {
			return fmt.Errorf("loop: recovery StartNewAttempt for %q: %w", stepID, err)
		}

		dlqed, err := l.dispatchAndResolve(ctx, retry, def, retryLimit, stepID, spec, newAttemptID, time.Now())
		if err != nil {
			return err
		}
		if dlqed {
			return nil
		}
	}

	return l.driveSteadyState(ctx, def, pl, retry, retryLimit)
}

// driveSteadyState runs the ask-planner → TX1 → dispatch → resolve loop
// until the run reaches a terminal outcome (DONE, worker-side DLQ inside
// dispatchAndResolve, or planner-side DLQ via TX8/TX9).
//
// This is the shared tail of BOTH entry points into the loop:
//   - Run(), after its one-time crash-recovery check (§8.3) has either done
//     nothing (no pending step) or fully resolved the pending step;
//   - ResumeReplayedStep (Session 6.5), after it has dispatched the single
//     attempt TX5 just created and that attempt resolved DONE — the run
//     must keep going exactly as it would after any other step completes.
//
// Extracted verbatim from Run()'s prior steady-state loop; behavior is
// unchanged.
func (l *Loop) driveSteadyState(
	ctx context.Context,
	def core.WorkflowDef,
	pl core.NextStepPlanner,
	retry core.RetryPolicy,
	retryLimit int,
) error {
	for {
		frontier, err := l.Store.LoadFrontier(ctx, l.RunID)
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
			// boundHistory is computed fresh from the real, never-truncated
			// Postgres data on every call — nothing is persisted
			// differently; only what gets marshaled onto the wire for THIS
			// Decide call changes (registry #1, whitepaper §12.2).
			History: boundHistory(history),
		}

		decision, ok, err := l.decideWithBudget(ctx, pl, state)
		if err != nil {
			return fmt.Errorf("loop: planner budget: %w", err)
		}
		if !ok {
			// The planner budget was exhausted; decideWithBudget already
			// issued TX9. This run is terminated.
			return nil
		}

		switch decision.Status {
		case core.PlannerVerdictDone:
			if err := l.Store.MarkRunDone(ctx, l.RunID); err != nil {
				return fmt.Errorf("loop: MarkRunDone: %w", err)
			}
			return nil

		case core.PlannerVerdictFail:
			if err := l.Store.MarkRunDLQPlannerDeclared(ctx, l.RunID, "planner declared run unworkable"); err != nil {
				return fmt.Errorf("loop: MarkRunDLQPlannerDeclared: %w", err)
			}
			return nil

		case core.PlannerVerdictContinue:
			if decision.Step == nil {
				return fmt.Errorf("loop: planner returned continue with nil step")
			}
			spec := *decision.Step

			// ── Barrier 1 (TX1): persist decision + first attempt BEFORE
			// dispatch. The attempt's timeout clock starts at this
			// transaction's commit.
			stepID, attemptID, err := l.Store.CreateStepWithAttempt(ctx, l.RunID, spec)
			if err != nil {
				return fmt.Errorf("loop: CreateStepWithAttempt %q: %w", spec.Name, err)
			}
			createdAt := time.Now() // see the package doc note on the timestamp approximation

			dlqed, err := l.dispatchAndResolve(ctx, retry, def, retryLimit, stepID, spec, attemptID, createdAt)
			if err != nil {
				return err
			}
			if dlqed {
				return nil
			}
			// Step resolved DONE (Barrier 2 fired) — loop back to ask the
			// planner for the next step.

		default:
			return fmt.Errorf("loop: unknown planner status %q", decision.Status)
		}
	}
}

// dispatchAndResolve drives ONE step, whose current attempt already exists
// (persisted via TX1 or the recovery TX4 — never called before that), through
// dispatch, outcome classification, and TX2/TX3 checkpointing. On failure
// below the retry budget it sleeps the retry delay, performs TX4, and
// dispatches again — repeating until the step reaches DONE or the DLQ.
//
// Because this function is only ever invoked with an attempt that was JUST
// created by the caller (TX1's first attempt, or recovery's post-claim TX4),
// the "recovery skips the initial 5s retry delay" clause (whitepaper §5) is
// satisfied by construction: no delay is ever introduced before the FIRST
// dispatch of a given call. Only RETRIES within this function's own loop
// (ordinary in-run retries, not recovery-specific) sleep the normal delay.
//
// Timestamp note: createdAt is the loop's local wall-clock reading taken
// immediately after the TX1/TX4 call returns — core.StateStore's TXn methods
// return only ids, never timestamps (T2: no method accepts a persisted
// timestamp, and by the same discipline none hands one back either), so the
// loop cannot read the DB's actual now() for this commit without a further
// round trip. The gap between the transaction's commit and this line is
// microseconds to low milliseconds, negligible against any timeout ≥ 1s.
// This value is used ONLY to compute an in-memory context.Context deadline —
// it is never persisted — so it does not violate the clock rule (whitepaper
// §3: persisted timestamps always come from DB now()).
//
// Returns dlqed=true if the step (and therefore the run) was routed to the
// DLQ inside TX3; the caller must stop driving this run.
func (l *Loop) dispatchAndResolve(
	ctx context.Context,
	retry core.RetryPolicy,
	def core.WorkflowDef,
	retryLimit int,
	stepID core.StepID,
	spec core.StepSpec,
	attemptID core.AttemptID,
	createdAt time.Time,
) (dlqed bool, err error) {
	for {
		timeout := effectiveTimeout(spec.TimeoutSeconds, def.DefaultTimeoutSeconds)
		deadline := createdAt.Add(timeout)

		dispatchCtx := transport.WithDispatchMeta(ctx, transport.DispatchMeta{
			StepID:    stepID,
			AttemptID: attemptID,
		})
		dispatchCtx, cancel := context.WithDeadline(dispatchCtx, deadline)
		result, dispatchErr := l.Transport.Dispatch(dispatchCtx, spec)
		cancel()

		if dispatchErr == nil && result.Status == core.ResultStatusDone {
			// ── Barrier 2 (TX2) ──
			outcome, err := l.Store.CheckpointSuccess(ctx, stepID, attemptID, result.Output)
			if err != nil {
				return false, fmt.Errorf("loop: CheckpointSuccess %q attempt %q: %w", stepID, attemptID, err)
			}
			if outcome != core.ReportCommitted {
				// Under the single-writer invariant (whitepaper §10.4) the
				// loop is the ONLY caller of CheckpointSuccess for its own
				// dispatch, exactly once per attempt — this branch should be
				// unreachable. Surfacing it loudly (rather than silently
				// guessing at a recovery action) is deliberate: see the
				// Session 5 report for this reasoning.
				return false, fmt.Errorf(
					"loop: CheckpointSuccess superseded for step %q attempt %q — single-writer invariant violated",
					stepID, attemptID)
			}
			return false, nil
		}

		// ── Classify the failure (whitepaper §6 taxonomy, §7.1) ──
		var reason core.FailureReason
		var detail string
		var retryAfterSeconds *int
		if dispatchErr != nil {
			// No valid response was obtained at all: ctx deadline exceeded
			// (the creation-anchored timer), or a transport-level error.
			// Both take the timeout path — the entire "decision exists, no
			// result" span is claimed by exactly one rule (whitepaper §6).
			// There is no worker-reported Result here, so retryAfterSeconds
			// stays nil (registry item #5 only applies to a worker's own
			// reported failure, not to an orchestrator-side timeout).
			reason = core.FailureReasonTimeout
			detail = dispatchErr.Error()
		} else {
			reason = result.Failure.Reason
			detail = result.Failure.Error
			retryAfterSeconds = result.Failure.RetryAfterSeconds
		}

		// ── TX3 ──
		outcome, err := l.Store.RecordFailure(ctx, stepID, attemptID, reason, detail, retryLimit)
		if err != nil {
			return false, fmt.Errorf("loop: RecordFailure %q attempt %q: %w", stepID, attemptID, err)
		}
		if outcome.Report != core.ReportCommitted {
			// Same single-writer argument as CheckpointSuccess above.
			return false, fmt.Errorf(
				"loop: RecordFailure superseded for step %q attempt %q — single-writer invariant violated",
				stepID, attemptID)
		}
		if outcome.DLQed {
			return true, nil
		}

		// Below budget: read the persisted count back from the store (never
		// a loop-local counter — the exact defect this refactor targets)
		// and feed it, plus the worker's optionally-reported
		// retryAfterSeconds (registry item #5), to RetryPolicy.Next for the
		// delay.
		fr, err := l.Store.LoadFrontier(ctx, l.RunID)
		if err != nil {
			return false, fmt.Errorf("loop: LoadFrontier (post-failure): %w", err)
		}
		delay, _ := retry.Next(fr.AttemptCount, errors.New(detail), retryAfterSeconds)

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(delay):
		}

		// ── TX4 ──
		newAttemptID, err := l.Store.StartNewAttempt(ctx, stepID)
		if err != nil {
			return false, fmt.Errorf("loop: StartNewAttempt %q: %w", stepID, err)
		}
		attemptID = newAttemptID
		createdAt = time.Now()
	}
}

// effectiveTimeout resolves the attempt's lifetime ceiling: step override >
// workflow default > 60s system default (whitepaper §6). Resolution happens
// once per dispatch; the resolved value is never written back into the
// stored StepSpec.
func effectiveTimeout(stepSeconds, workflowSeconds int) time.Duration {
	if stepSeconds > 0 {
		return time.Duration(stepSeconds) * time.Second
	}
	if workflowSeconds > 0 {
		return time.Duration(workflowSeconds) * time.Second
	}
	return systemDefaultTimeout
}

// buildPlanner reconstructs the NextStepPlanner instance from the workflow's
// persisted planner_type/planner_config (whitepaper §12.1). If
// l.PlannerFactory is set (a test seam), it is used instead.
func (l *Loop) buildPlanner(def core.WorkflowDef) (core.NextStepPlanner, error) {
	if l.PlannerFactory != nil {
		return l.PlannerFactory(def)
	}
	switch def.PlannerType {
	case "static":
		return planner.NewStaticPlanner(def.PlannerConfig)
	case "http":
		return planner.NewHTTPPlanner(def.PlannerConfig)
	default:
		return nil, fmt.Errorf("unknown planner_type %q", def.PlannerType)
	}
}

// plannerAttemptDetail is one entry of the per-attempt context persisted
// into dead_letter_queue.context when the planner budget is exhausted (TX9).
type plannerAttemptDetail struct {
	Attempt int    `json:"attempt"`
	Class   string `json:"class"` // "unreachable" | "malformed"
	Error   string `json:"error"`
}

// decideWithBudget asks the planner for the next step under the fixed
// 30s-per-call × 3-total-attempts budget (whitepaper §7.2).
//
// StaticPlanner is infallible by construction (it is a pure function over
// immutable, already-validated config — §16, §7.2) and bypasses the budget
// entirely: it is called exactly once. Any error it somehow still returns is
// treated as a genuine implementation bug (e.g. json.Marshal failing on a
// value that should never fail to marshal) rather than an operational
// failure mode — the whitepaper defines no DLQ reason for "an infallible
// planner turned out fallible", so Run() propagates the error directly
// instead of manufacturing a TX9 verdict the model doesn't define.
//
// On success, returns (decision, true, nil). On budget exhaustion, TX9 is
// issued internally (MarkRunDLQPlannerExhausted) and this returns
// (zero, false, nil) — the caller must stop driving this run, but this is
// not itself a Go error.
func (l *Loop) decideWithBudget(ctx context.Context, p core.NextStepPlanner, state core.RunState) (core.StepDecision, bool, error) {
	if _, isStatic := p.(*planner.StaticPlanner); isStatic {
		d, err := p.Decide(ctx, state)
		if err != nil {
			return core.StepDecision{}, false, fmt.Errorf(
				"StaticPlanner.Decide returned an error (infallible-by-construction invariant violated): %w", err)
		}
		return d, true, nil
	}

	var details []plannerAttemptDetail
	var lastReason core.DLQReason

	for attempt := 1; attempt <= plannerMaxAttempts; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, plannerCallTimeout)
		d, derr := p.Decide(callCtx, state)
		cancel()
		if derr == nil {
			return d, true, nil
		}

		var malformed *planner.MalformedError
		class := "unreachable"
		reason := core.DLQReasonPlannerUnreachable
		if errors.As(derr, &malformed) {
			class = "malformed"
			reason = core.DLQReasonPlannerMalformed
		}
		lastReason = reason
		details = append(details, plannerAttemptDetail{Attempt: attempt, Class: class, Error: derr.Error()})
	}

	detailJSON, jerr := json.Marshal(details)
	if jerr != nil {
		detailJSON = []byte(fmt.Sprintf(`{"marshal_error":%q}`, jerr.Error()))
	}
	if err := l.Store.MarkRunDLQPlannerExhausted(ctx, l.RunID, lastReason, string(detailJSON)); err != nil {
		return core.StepDecision{}, false, fmt.Errorf("MarkRunDLQPlannerExhausted: %w", err)
	}
	return core.StepDecision{}, false, nil
}

// historyTruncationPointer replaces a HistoryEntry.Output that exceeds
// historyEntryCapBytes. It carries exactly the three facts registry #1's
// design calls for: that the value is truncated, its real size, and how to
// fetch it in full.
type historyTruncationPointer struct {
	Truncated bool   `json:"_truncated"`
	SizeBytes int    `json:"size_bytes"`
	Note      string `json:"note"`
}

// boundHistory returns the wire-ready History array for one planner.Decide
// call: a two-tier, purely mechanical size guard (registry #1; see the
// "Design decisions log" in the Phase 2 session prompts for why the
// alternative multi-round `need_more` protocol was proposed and explicitly
// rejected in favor of this). Nothing is persisted differently — history is
// read fresh from Postgres on every call (never truncated at rest); this
// function only decides how much detail per entry gets marshaled onto THIS
// wire payload.
//
// Tier 1 — per-entry cap (historyEntryCapBytes): any entry whose Output
// marshals to more than the cap is replaced with a historyTruncationPointer
// naming its real size and pointing at GET /runs/{run_id} for the full
// value.
//
// Tier 2 — total cap (historyTotalCapBytes): walking the (already
// per-entry-capped) entries from the most recent backward, once an entry
// would not fit within the remaining budget, that entry AND every older one
// is reduced to name+status only — Output omitted entirely (omitempty),
// not even a pointer, since the whole run is already reachable via GET
// /runs/{run_id}.
//
// Final order is unchanged: history is, and remains, seq ascending
// (whitepaper's existing binding wire-order rule) — this function only
// varies per-entry detail level, never order.
func boundHistory(history []core.HistoryEntry) []core.HistoryEntry {
	if len(history) == 0 {
		return history
	}

	capped := make([]core.HistoryEntry, len(history))
	copy(capped, history)

	// Tier 1: per-entry cap, independent of position.
	for i, e := range capped {
		if len(e.Output) <= historyEntryCapBytes {
			continue
		}
		ptr := historyTruncationPointer{
			Truncated: true,
			SizeBytes: len(e.Output),
			Note:      "output exceeds the per-entry wire cap; fetch the full value via GET /runs/{run_id}",
		}
		ptrJSON, err := json.Marshal(ptr)
		if err != nil {
			// historyTruncationPointer is a fixed, always-marshalable shape
			// (two scalars and a bool) — this should never happen. Fail
			// safe by omitting Output entirely rather than risk shipping a
			// broken payload to the planner.
			capped[i].Output = nil
			continue
		}
		capped[i].Output = ptrJSON
	}

	// Tier 2: total cap, walked most-recent-first. History is seq ASC, so
	// the most recent entry is the last element.
	var used int
	for i := len(capped) - 1; i >= 0; i-- {
		size := len(capped[i].Output)
		if used+size > historyTotalCapBytes {
			// This entry, and every older one, drops to name+status only.
			for j := i; j >= 0; j-- {
				capped[j].Output = nil
			}
			break
		}
		used += size
	}

	return capped
}
