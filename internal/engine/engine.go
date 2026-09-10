// Package engine is the orchestrator: the sweep of SPEC.md 8.6, the heartbeat
// of SPEC.md 8.7, and the driving loop of SPEC.md 4.2.
//
// SPEC.md 1 governs every line of it: "the database is the only coordination
// mechanism. Every in-memory structure is a cache that may vanish at any
// instant without affecting correctness." The two in-memory structures here are
// the set of drivers this process is running and the HTTP client; losing either
// costs latency and nothing else, because ownership lives in runs.owner_id,
// liveness in orchestrators.last_seen_at, and every deadline and budget in a
// column.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/aaronwu001/piton/internal/config"
	"github.com/aaronwu001/piton/internal/dispatch"
	"github.com/aaronwu001/piton/internal/model"
	"github.com/aaronwu001/piton/internal/planner"
	"github.com/aaronwu001/piton/internal/storage"
	"github.com/aaronwu001/piton/internal/validate"
)

// Engine is one orchestrator process.
type Engine struct {
	// ID is SPEC.md 3.3's orchestrator_id: "generated fresh at each process
	// boot. Stored as a plain string with no foreign key."
	ID string

	store  storage.Store
	cfg    *config.Config
	client *http.Client
	logger *log.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// drivers is the in-process record of which runs this process is already
	// driving. It exists to stop one process starting two drivers for one run;
	// it is not what stops two processes doing so — SPEC.md 8.5's claim is,
	// and SPEC.md 8.2's fence is what makes a superseded driver harmless.
	mu      sync.Mutex
	drivers map[string]struct{}

	// nudge asks the sweep to run now rather than at its next tick. It is a
	// latency optimisation over SPEC.md 8.6's fixed interval and nothing more:
	// a lost nudge costs one sweep interval of delay and no correctness.
	nudge chan struct{}
}

// New builds an engine. Nothing is written and no goroutine runs until Start.
func New(store storage.Store, cfg *config.Config, logger *log.Logger) *Engine {
	return &Engine{
		ID:      model.NewID(),
		store:   store,
		cfg:     cfg,
		logger:  logger,
		drivers: make(map[string]struct{}),
		nudge:   make(chan struct{}, 1),
		// No client-level timeout: each dispatch carries its own deadline,
		// derived from the attempt's deadline_at (SPEC.md 13.3), and a second
		// timeout here would silently override the one the row records.
		client: &http.Client{},
	}
}

// Start registers this process and begins the heartbeat and the sweep.
//
// SPEC.md 8.6: "startup recovery is not a separate code path — it is the first
// sweep." There is therefore no recovery routine below, only a sweep that runs
// once before its ticker does.
func (e *Engine) Start(parent context.Context) error {
	e.ctx, e.cancel = context.WithCancel(parent)

	if err := e.store.RegisterOrchestrator(e.ctx, e.ID); err != nil {
		return err
	}
	e.logf("orchestrator %s registered; sweep %s, heartbeat %s, lease TTL %s",
		e.ID, e.cfg.SweepInterval(), e.cfg.HeartbeatInterval(), e.cfg.LeaseTTL())

	e.wg.Add(2)
	go func() { defer e.wg.Done(); e.heartbeatLoop() }()
	go func() { defer e.wg.Done(); e.sweepLoop() }()
	return nil
}

// Stop ends every loop and driver, then releases this process's runs.
//
// SPEC.md 8.7: the release "is an optimisation that makes failover immediate
// rather than lease_ttl later; correctness does not depend on it". It is
// therefore done on a fresh context — the shutdown has already cancelled the
// engine's own — and a failure is logged rather than propagated.
func (e *Engine) Stop() {
	if e.cancel == nil {
		return
	}
	e.cancel()
	e.wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.store.ReleaseOwned(ctx, e.ID); err != nil {
		e.logf("release on shutdown failed (correctness does not depend on it, SPEC.md 8.7): %v", err)
	}
}

// Nudge asks for a sweep now. The API calls it after creating a run so that the
// run starts without waiting out a sweep interval.
func (e *Engine) Nudge() {
	select {
	case e.nudge <- struct{}{}:
	default: // a sweep is already pending; one is enough
	}
}

func (e *Engine) logf(format string, args ...any) {
	if e.logger != nil {
		e.logger.Printf(format, args...)
	}
}

// ---------------------------------------------------------------------------
// The heartbeat (SPEC.md 8.7)
// ---------------------------------------------------------------------------

// heartbeatLoop writes SPEC.md 8.7's one statement every heartbeat interval.
//
// SPEC.md 3.4 is what makes this legal outside a business transaction: it is
// coordination metadata, not business state, and is "governed by the
// transaction rules of SPEC.md 8? No". SPEC.md 4.3: renewal is "a liveness
// signal, not a progress signal" — a run waiting three hours on a single worker
// call keeps its owner the whole time.
func (e *Engine) heartbeatLoop() {
	ticker := time.NewTicker(e.cfg.HeartbeatInterval())
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			if err := e.store.Heartbeat(e.ctx, e.ID); err != nil && e.ctx.Err() == nil {
				// SPEC.md 13.1 case 6: storage unreachable at runtime is
				// survived — the runs orphan intact and are reclaimed when
				// storage returns. Stopping the process here would throw away
				// a recovery the design already has.
				e.logf("heartbeat failed: %v", err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The sweep (SPEC.md 8.6)
// ---------------------------------------------------------------------------

func (e *Engine) sweepLoop() {
	ticker := time.NewTicker(e.cfg.SweepInterval())
	defer ticker.Stop()

	e.sweep() // SPEC.md 8.6: startup recovery is the first sweep
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			e.sweep()
		case <-e.nudge:
			e.sweep()
		}
	}
}

// sweep claims what it may and starts a driver for every run this process owns.
//
// SPEC.md 8.6: "the sweep only claims. It never touches business state.
// Expiring overdue attempts, checking budgets and re-dispatching are done
// afterwards by the driver that now owns the run, through the normal fenced
// path." That is why nothing below writes anything except through ClaimRuns.
func (e *Engine) sweep() {
	if _, err := e.store.ClaimRuns(e.ctx, e.ID, e.cfg.LeaseTTL()); err != nil {
		if e.ctx.Err() == nil {
			e.logf("sweep: claim failed: %v", err)
		}
		return
	}

	// Everything this process owns and has not finished, whether it was just
	// claimed or claimed several sweeps ago. Re-reading the set rather than
	// driving only what ClaimRuns returned is what covers SPEC.md 13.1 case 2:
	// a driver that died while the process lived leaves a run owned by a live
	// orchestrator, which no claim will ever return.
	owned, err := e.store.OwnedRunningRuns(e.ctx, e.ID)
	if err != nil {
		if e.ctx.Err() == nil {
			e.logf("sweep: cannot list owned runs: %v", err)
		}
		return
	}
	for _, runID := range owned {
		e.startDriver(runID)
	}
}

func (e *Engine) startDriver(runID string) {
	e.mu.Lock()
	if _, running := e.drivers[runID]; running {
		e.mu.Unlock()
		return
	}
	e.drivers[runID] = struct{}{}
	e.mu.Unlock()

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer func() {
			e.mu.Lock()
			delete(e.drivers, runID)
			e.mu.Unlock()
		}()
		e.drive(runID)
	}()
}

// ---------------------------------------------------------------------------
// The driving loop (SPEC.md 4.2)
// ---------------------------------------------------------------------------

// drive advances one owned run until it stops.
//
// It is SPEC.md 4.2's five steps, with SPEC.md 5.5's combination table as the
// dispatch in step 3. The loop stops silently on ErrNotOwner, which is SPEC.md
// 8.2's instruction word for word: "roll back, stop the driver, dispatch
// nothing, tell nobody".
func (e *Engine) drive(runID string) {
	for {
		if e.ctx.Err() != nil {
			return
		}

		st, err := e.store.LoadDriverState(e.ctx, e.ID, runID)
		if err != nil {
			if !errors.Is(err, storage.ErrNotOwner) && e.ctx.Err() == nil {
				e.logf("run %s: cannot read driver state: %v", runID, err)
			}
			return
		}
		if st.Run.Status != model.StatusRunning {
			return
		}

		var carryOn bool
		switch st.LastStepStatus() {
		case model.StatusDone:
			// L1 — waiting on a planner decision, including the no-steps case
			// that SPEC.md 5.4's convention folds in here.
			carryOn = e.plan(st)
		case model.StatusRunning:
			// L2 — waiting on a worker, or its owner died.
			carryOn = e.resolve(st)
		default:
			// SPEC.md 5.6: run=RUNNING with a DLQ or CANCELLED last step is
			// impossible because of a transaction boundary, not a check. If it
			// is ever seen, the invariant is broken and guessing would make it
			// worse.
			e.logf("run %s: run is RUNNING but its last step is %s, which SPEC.md 5.6 makes impossible",
				runID, st.LastStepStatus())
			return
		}
		if !carryOn {
			return
		}
	}
}

// plan is SPEC.md 4.2's L1: ask the planner, and persist the answer.
//
// The two planner types differ in exactly one place - how the decision is
// obtained - and in nothing after it. SPEC.md 12.1 requires that: "these rules
// apply to every planner, including the built-in static one, with no
// exemption ... an implementation that special-cases the static planner out of
// the budget path has added a branch to work around a situation that cannot
// occur, and that branch will outlive the reason for it."
//
// So a call is timed, classified and recorded the same way whichever planner
// answered, and SPEC.md 4.2's four rows below are one switch and not two.
func (e *Engine) plan(st *storage.DriverState) bool {
	runID := st.Run.RunID
	startedAt := time.Now().UTC()

	var (
		decision *planner.Decision
		failure  *planner.Failure
	)

	switch st.Workflow.PlannerType {
	case model.PlannerStatic:
		d, err := planner.Static(st.Workflow, st.StepCount)
		if err != nil {
			// SPEC.md 12.1 says this cannot happen - the static planner "holds
			// no state and makes no network call", and SPEC.md 6.1 validated
			// its steps at submission. It is routed through the ordinary path
			// anyway rather than special-cased.
			failure = &planner.Failure{Reason: model.FailureInvalidResponse, Text: err.Error()}
		} else {
			decision = d
		}

	case model.PlannerHTTP:
		history, err := e.store.PlannerHistory(e.ctx, runID)
		if err != nil {
			// Storage being unreachable is not the planner failing, and must
			// not burn its budget. SPEC.md 13.1 item 6: "runs orphan intact
			// and are reclaimed when storage returns."
			e.logUnexpected(runID, "cannot read the planner history", err)
			return false
		}
		decision, failure = planner.HTTP(e.ctx, e.client, st.Workflow.PlannerURL,
			planner.Request{
				RunID:         runID,
				WorkflowInput: st.Run.Input,
				History:       historyRows(history),
				FetchBaseURL:  st.Workflow.FetchBaseURL,
			},
			time.Duration(st.Workflow.PlannerTimeoutSeconds)*time.Second)

	default:
		// SPEC.md 6.1 enumerates two planner types and SPEC.md 16 rule 1
		// rejects anything else before a run can exist, so an accepted
		// workflow cannot reach here. It is reported rather than ignored,
		// because a run that cannot be planned must converge (SPEC.md 12.2)
		// instead of sitting RUNNING for ever.
		failure = &planner.Failure{Reason: model.FailureInvalidResponse,
			Text: fmt.Sprintf("piton: workflow %s has planner_type %q, which SPEC.md 6.1 does not define",
				st.Workflow.WorkflowID, st.Workflow.PlannerType)}
	}

	if failure != nil {
		return e.plannerFailed(st, startedAt, failure)
	}

	// SPEC.md 4.2: the answer and its effect are written together, and SPEC.md
	// 6.8 is where the answer itself is written.
	call := storage.PlannerCallRecord{StartedAt: startedAt, FinishedAt: time.Now().UTC()}

	switch decision.Status {
	case planner.StatusDone:
		call.Answer = model.AnswerDone
		if err := e.store.CompleteRun(e.ctx, e.ID, runID, call); err != nil {
			e.logUnexpected(runID, "cannot complete the run", err)
		}
		return false

	case planner.StatusContinue:
		call.Answer = model.AnswerContinue
		return e.beginStep(st, decision.Step, call)

	case planner.StatusFail:
		// SPEC.md 12.1: "a fail response is not a planner failure - it is a
		// valid answer, and it sends the run to DLQ immediately without
		// consuming budget."
		call.Answer = model.AnswerFail
		call.ErrorText = decision.Reason
		if err := e.store.DeadLetterPlannerFail(e.ctx, e.ID, storage.PlannerDeclaredFailInput{
			RunID:     runID,
			Call:      call,
			ErrorText: decision.Reason,
		}); err != nil {
			e.logUnexpected(runID, "cannot dead-letter the run the planner refused", err)
		}
		return false

	default:
		// planner.HTTP and planner.Static both answer with one of the three or
		// with a failure, so reaching here would mean this function and one of
		// them disagree about SPEC.md 9.3.
		e.logf("run %s: the planner answered %q, which SPEC.md 9.3 does not define",
			runID, decision.Status)
		return false
	}
}

// historyRows turns SPEC.md 9.2's catalogue into the wire shape. The timestamp
// is rendered as RFC 3339 in UTC, which is what SPEC.md 9.2's example shows.
func historyRows(steps []storage.HistoryStep) []planner.HistoryRow {
	rows := make([]planner.HistoryRow, 0, len(steps))
	for _, s := range steps {
		row := planner.HistoryRow{
			StepID:       s.StepID,
			StepName:     s.StepName,
			Seq:          s.Seq,
			Status:       s.Status,
			OutputBytes:  s.OutputBytes,
			AttemptCount: s.AttemptCount,
		}
		if s.CompletedAt != nil {
			at := s.CompletedAt.UTC().Format(time.RFC3339)
			row.CompletedAt = &at
		}
		rows = append(rows, row)
	}
	return rows
}

// beginStep persists a `continue` decision and dispatches its first attempt.
func (e *Engine) beginStep(st *storage.DriverState, decision []byte,
	call storage.PlannerCallRecord) bool {

	runID := st.Run.RunID

	// SPEC.md 9.8: an invalid StepSpec "is a planner failure and consumes
	// planner budget exactly like an unreachable planner. It never creates a
	// step." For the static planner this is unreachable — SPEC.md 6.1 rejected
	// such a StepSpec at POST /workflows — and it is checked here anyway
	// because the rule is about planners, not about one planner.
	//
	// The call that carried it is therefore recorded as FAILED rather than as
	// the `continue` it claimed to be: SPEC.md 5.8 gives that invalid_response,
	// and the run gains no step (SPEC.md 6.8's invariants keep the two facts
	// from contradicting each other).
	spec, rej := validate.StepSpec(decision)
	if rej != nil {
		return e.plannerFailed(st, call.StartedAt, &planner.Failure{
			Reason: model.FailureInvalidResponse,
			Text:   "piton: the planner returned an invalid StepSpec: " + rej.Message,
		})
	}

	d, err := e.store.BeginStep(e.ctx, e.ID, storage.BeginStepInput{
		RunID:          runID,
		StepName:       spec.StepName,
		Decision:       decision,
		ConnectionMode: spec.ConnectionMode,
		TimeoutSeconds: st.Workflow.StepTimeoutSeconds,
		Call:           call,
	})
	if err != nil {
		e.logUnexpected(runID, "cannot create the next step", err)
		return false
	}

	e.execute(st, spec, d)
	return true
}

// resolve is SPEC.md 4.2's L2, and SPEC.md 5.5 calls it "the claim path":
// expire the overdue or orphaned attempt → budget check → re-dispatch, or go to
// DLQ.
func (e *Engine) resolve(st *storage.DriverState) bool {
	runID := st.Run.RunID
	step := st.LastStep

	if attempt := st.Outstanding(); attempt != nil {
		switch {
		case attempt.ConnectionMode == model.ConnectionSync:
			// SPEC.md 8.6's claim-time rule: a sync attempt "may be expired
			// immediately, regardless of deadline_at. Its HTTP connection died
			// with its previous owner, so no report can ever arrive."
			//
			// Reaching here means no driver is waiting on that connection: a
			// driver that dispatched an attempt itself records its outcome
			// inline and never returns to the top of the loop with the attempt
			// still RUNNING.
			return e.expire(st, attempt,
				"piton: expired at claim time — a sync attempt's HTTP connection did not survive "+
					"the driver that opened it, so no report can arrive (SPEC.md 8.6)")

		case time.Now().After(attempt.DeadlineAt):
			// SPEC.md 4.2: "RUNNING, deadline passed → expire it."
			return e.expire(st, attempt,
				"piton: deadline_at passed with no outcome written (SPEC.md 4.2, SPEC.md 13.3)")

		default:
			// SPEC.md 8.4: "the driver awaiting an async result polls the
			// attempt row, once per second, rather than blocking on an
			// in-memory channel", because "a channel exists only in one
			// process. This is SPEC.md 1 doing its job."
			return e.pause(time.Second)
		}
	}

	// No outstanding attempt: SPEC.md 12.2's budget check.
	if step.AttemptCount < st.Workflow.StepMaxAttempts {
		// SPEC.md 11.1: step_retry_delay_seconds "is enforced in memory by the
		// driver. It is not a guarantee across a crash" — per SPEC.md 1 an
		// in-memory mechanism may never be load-bearing, and this one is a
		// courtesy to the worker rather than a correctness property.
		if delay := time.Duration(st.Workflow.StepRetryDelaySeconds) * time.Second; delay > 0 {
			if !e.pause(delay) {
				return false
			}
		}

		spec, rej := validate.StepSpec(step.Decision)
		if rej != nil {
			e.logf("run %s: the stored decision at seq %d is not a valid StepSpec: %s",
				runID, step.Seq, rej.Message)
			return false
		}
		d, err := e.store.BeginAttempt(e.ctx, e.ID, storage.BeginAttemptInput{
			RunID:          runID,
			StepID:         step.StepID,
			ConnectionMode: spec.ConnectionMode,
			TimeoutSeconds: st.Workflow.StepTimeoutSeconds,
		})
		if err != nil {
			e.logUnexpected(runID, "cannot re-dispatch the step", err)
			return false
		}
		e.execute(st, spec, d)
		return true
	}

	// SPEC.md 12.2 and 12.3: the worker-side dead-letter. step → DLQ, run →
	// DLQ and the entry, in one transaction, reaching combination L4.
	//
	// SPEC.md 12.4: error_text records "the most recent failure, not every
	// failure of the round" — the per-attempt history is already in attempts.
	errText := fmt.Sprintf("piton: step %d used its budget of %d attempts without succeeding",
		step.Seq, st.Workflow.StepMaxAttempts)
	if st.LastAttempt != nil && st.LastAttempt.ErrorText != nil {
		errText = *st.LastAttempt.ErrorText
	}
	if err := e.store.DeadLetterStep(e.ctx, e.ID, storage.StepDeadLetterInput{
		RunID:     runID,
		StepID:    step.StepID,
		ErrorText: errText,
	}); err != nil {
		e.logUnexpected(runID, "cannot dead-letter the step", err)
	}
	return false
}

// expire writes an attempt's timeout verdict. The backend chooses between
// timeout and orphaned (SPEC.md 5.3), because only it can ask whether the
// dispatching orchestrator was live at the instant of the write.
func (e *Engine) expire(st *storage.DriverState, attempt *model.Attempt, why string) bool {
	err := e.store.ExpireAttempt(e.ctx, e.ID, storage.AttemptFailure{
		RunID:     st.Run.RunID,
		AttemptID: attempt.AttemptID,
		ErrorText: why,
	}, e.cfg.LeaseTTL())
	switch {
	case err == nil:
		return true
	case errors.Is(err, storage.ErrAttemptNotRunning):
		// SPEC.md 8.3: someone already recorded this outcome. Re-read and act
		// on what is now true.
		return true
	default:
		e.logUnexpected(st.Run.RunID, "cannot expire the outstanding attempt", err)
		return false
	}
}

// execute performs one dispatch and records its outcome.
//
// SPEC.md 4.2 fixes the order this function depends on: the attempt row was
// inserted and committed by BeginStep or BeginAttempt before anything was sent,
// "because the attempt row must exist before the work does".
func (e *Engine) execute(st *storage.DriverState, spec *validate.Spec, d *storage.Dispatch) {
	runID := st.Run.RunID

	inputs, err := e.assembleInputs(runID, d.Seq, spec)
	if err != nil {
		e.recordFailure(runID, d.AttemptID, model.FailureTransportError,
			fmt.Sprintf("piton: cannot assemble the envelope's inputs: %v", err))
		return
	}

	// SPEC.md 13.3: attempts.deadline_at is authoritative and the in-process
	// timer is the latency optimisation. Deriving the timer from the row keeps
	// the two from disagreeing about when this attempt runs out.
	timeout := time.Until(d.DeadlineAt)
	if timeout < time.Second {
		timeout = time.Second
	}

	outcome := dispatch.Do(e.ctx, e.client, spec, dispatch.Identity{
		RunID:     runID,
		StepID:    d.StepID,
		AttemptID: d.AttemptID,
	}, inputs, timeout)

	if outcome.Success {
		err := e.store.RecordAttemptSuccess(e.ctx, e.ID, runID, d.StepID, d.AttemptID, outcome.Output)
		if err != nil && !errors.Is(err, storage.ErrAttemptNotRunning) {
			e.logUnexpected(runID, "cannot record the attempt's success", err)
		}
		return
	}
	e.recordFailure(runID, d.AttemptID, outcome.Reason, outcome.ErrorText)
}

func (e *Engine) recordFailure(runID, attemptID, reason, errText string) {
	err := e.store.RecordAttemptFailure(e.ctx, e.ID, storage.AttemptFailure{
		RunID:     runID,
		AttemptID: attemptID,
		Reason:    reason,
		ErrorText: errText,
	})
	if err != nil && !errors.Is(err, storage.ErrAttemptNotRunning) {
		e.logUnexpected(runID, "cannot record the attempt's failure", err)
	}
}

// assembleInputs resolves SPEC.md 9.4's input_from into SPEC.md 9.5's inputs.
//
// SPEC.md 9.4 gives the field three meanings, and all three are here:
//
//	omitted  ⇒ the previous step only
//	[]       ⇒ nothing
//	[ids...] ⇒ those completed steps
//
// SPEC.md 9.4 also says why the default is the previous step rather than
// everything: "the overwhelmingly common case is a pipeline, and a default of
// 'everything' would make every worker's input grow with the run's length".
//
// input_from is meaningful in envelope mode only (SPEC.md 9.4), and SPEC.md 9.8
// rule 4 has already refused it anywhere else, so a raw dispatch assembles
// nothing.
func (e *Engine) assembleInputs(runID string, seq int, spec *validate.Spec) (map[string][]byte, error) {
	inputs := map[string][]byte{}
	if spec.DispatchStyle != model.DispatchEnvelope {
		return inputs, nil
	}

	if spec.InputFrom == nil {
		stepID, output, err := e.store.StepOutputAtSeq(e.ctx, runID, seq-1)
		if errors.Is(err, storage.ErrNotFound) {
			// The run's first step has no previous step. Nothing to deliver,
			// and not an error.
			return inputs, nil
		}
		if err != nil {
			return nil, err
		}
		inputs[stepID] = output
		return inputs, nil
	}

	for _, stepID := range *spec.InputFrom {
		output, err := e.store.StepOutputByID(e.ctx, runID, stepID)
		if errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("input_from names step %s, which is not a completed step of this run", stepID)
		}
		if err != nil {
			return nil, err
		}
		inputs[stepID] = output
	}
	return inputs, nil
}

// plannerFailed is SPEC.md 4.2's fourth L1 row: "increment
// runs.planner_attempt_count; if it has reached planner_max_attempts,
// planner-side dead-letter; otherwise the loop retries the call".
func (e *Engine) plannerFailed(st *storage.DriverState, startedAt time.Time,
	f *planner.Failure) bool {

	// A call cut short because this process is stopping is not the planner's
	// failure, and must not move the run towards DLQ for something the planner
	// did not do. SPEC.md 13.2 item 4 says what happens instead: the run is
	// reclaimed and the planner is asked again.
	if f.Interrupted() || e.ctx.Err() != nil {
		return false
	}

	deadLettered, err := e.store.RecordPlannerFailure(e.ctx, e.ID, storage.PlannerFailureInput{
		RunID: st.Run.RunID,
		Call: storage.PlannerCallRecord{
			FailureReason: f.Reason,
			ErrorText:     f.Text,
			StartedAt:     startedAt,
			FinishedAt:    time.Now().UTC(),
		},
	})
	if err != nil {
		e.logUnexpected(st.Run.RunID, "cannot record the planner failure", err)
		return false
	}
	if deadLettered {
		return false
	}

	// SPEC.md 11.1's planner_retry_delay_seconds, which exists so that a
	// planner refusing connections is retried rather than spun on. The same
	// section forbids the fixed interval this line used to hold: "an
	// implementation that waits a fixed interval of its own between planner
	// calls has invented a value the operator cannot see, cannot change, and
	// will not find in this document." The budget, never the wait, is what
	// makes the run converge (SPEC.md 12.2).
	return e.pause(time.Duration(st.Workflow.PlannerRetryDelaySeconds) * time.Second)
}

// pause waits, and reports whether the driver should carry on afterwards.
func (e *Engine) pause(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-e.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// logUnexpected reports an error that is not one of SPEC.md 8's two answers.
// ErrNotOwner is not an error at all — SPEC.md 8.2: "tell nobody" — and a
// cancelled context is a shutdown in progress.
func (e *Engine) logUnexpected(runID, what string, err error) {
	if errors.Is(err, storage.ErrNotOwner) || e.ctx.Err() != nil {
		return
	}
	e.logf("run %s: %s: %v", runID, what, err)
}
