// Package storage is the interface SPEC.md 7 makes a conformance requirement,
// and nothing else. Postgres is today's only implementation (internal/storage/
// postgres); SPEC.md 4.4 makes the backend a value in the configuration file
// precisely so that it is not a hard-coded assumption.
//
// SPEC.md 7.1: every JSON document crosses this boundary as opaque []byte —
// run inputs, step decisions, step and attempt outputs. No signature here
// mentions jsonb or any other backend-specific type.
//
// The method set is shaped by SPEC.md 8 rather than by the tables. Each write
// that constitutes a decision is one method, because each one must open with
// the ownership fence of SPEC.md 8.2 and commit as a unit; exposing a
// transaction handle instead would move that obligation to the caller, and
// SPEC.md 8.2 rejects exactly that: "one forgotten statement silently breaks
// the whole guarantee".
package storage

import (
	"context"
	"errors"
	"time"

	"github.com/aaronwu001/piton/internal/model"
)

var (
	// ErrNotFound is "no such entity" — SPEC.md 10.5's 404.
	ErrNotFound = errors.New("storage: no such entity")

	// ErrNotOwner is the ownership fence of SPEC.md 8.2 returning zero rows.
	// It is not an error condition in the usual sense: SPEC.md 8.1 says "zero
	// rows affected means the expectation was wrong, and is not an error
	// condition — it is the answer". The driver's answer is to "roll back,
	// stop the driver, dispatch nothing, tell nobody".
	ErrNotOwner = errors.New("storage: the run is no longer owned by this orchestrator")

	// ErrAttemptNotRunning is the attempt CAS of SPEC.md 8.3 returning zero
	// rows: the attempt has already been expired, superseded or cancelled, so
	// this report is a late one and is discarded.
	ErrAttemptNotRunning = errors.New("storage: the attempt is no longer RUNNING")

	// ErrNotInDLQ is the replay gate of SPEC.md 14 returning zero rows: the
	// run exists, but its status is not DLQ, so it cannot be replayed.
	//
	// SPEC.md 14: "the idempotency gate is 'is this run in DLQ right now'. Not
	// 'has this run been replayed before'." It is the same answer SPEC.md 8.1
	// describes for the other conditional writes - "zero rows affected means
	// the expectation was wrong, and is not an error condition, it is the
	// answer" - and SPEC.md 14 states which answer: 409, "stating the actual
	// current status (SPEC.md 10.5)".
	ErrNotInDLQ = errors.New("storage: the run is not in DLQ and cannot be replayed")
)

// DriverState is what SPEC.md 4.2 step 2 reads: "the run's status and the
// status of its highest-seq step". The workflow comes with it because every
// branch of SPEC.md 4.2 needs one of its budgets, and the outstanding attempt
// because SPEC.md 5.5's L2 is resolved from it.
type DriverState struct {
	Run      *model.Run
	Workflow *model.Workflow

	// StepCount is what SPEC.md 6.1's static planner is defined in terms of:
	// "planner_static_steps[n] where n is the number of steps the run already
	// has".
	StepCount int

	// LastStep is the highest-seq step, or nil when the run has none — the
	// case SPEC.md 5.4 defines as last_step = DONE.
	LastStep *model.Step

	// LastAttempt is LastStep's highest-numbered attempt, whatever its status.
	// The status is what distinguishes SPEC.md 4.2's two L2 sub-cases — an
	// outstanding attempt to resolve, or a finished one whose budget must now
	// be checked — and its error_text is what SPEC.md 12.4 records as "the most
	// recent failure" if the budget turns out to be spent.
	LastAttempt *model.Attempt
}

// Outstanding is LastStep's RUNNING attempt, if it has one, and nil otherwise.
// At most one can exist: SPEC.md 8.3's CAS moves an attempt out of RUNNING
// before anything supersedes it, so the highest-numbered attempt is the only
// one that can still be waiting.
func (s *DriverState) Outstanding() *model.Attempt {
	if s.LastAttempt != nil && s.LastAttempt.Status == model.StatusRunning {
		return s.LastAttempt
	}
	return nil
}

// LastStepStatus implements SPEC.md 5.4's convention, including its load-
// bearing sentence: "a run with no steps is defined to have last_step = DONE".
func (s *DriverState) LastStepStatus() string {
	if s.LastStep == nil {
		return model.StatusDone
	}
	return s.LastStep.Status
}

// BeginStepInput creates the step of a `continue` decision (SPEC.md 4.2, L1)
// and its first attempt in the same transaction.
type BeginStepInput struct {
	RunID    string
	StepName *string

	// Decision is the StepSpec bytes exactly as the planner returned them.
	// SPEC.md 6.3 stores them unchanged; nothing re-serialises them.
	Decision []byte

	ConnectionMode string
	TimeoutSeconds int

	// Call is the `continue` that produced this step, written into
	// planner_calls in the same transaction (SPEC.md 4.2, 6.8).
	Call PlannerCallRecord
}

// BeginAttemptInput re-dispatches an existing RUNNING step whose budget check
// passed (SPEC.md 4.2, L2 / SPEC.md 12.2).
type BeginAttemptInput struct {
	RunID          string
	StepID         string
	ConnectionMode string
	TimeoutSeconds int
}

// Dispatch is what a committed dispatch transaction hands back: the identities
// the envelope of SPEC.md 9.5 needs, and the deadline SPEC.md 13.3 makes
// authoritative.
type Dispatch struct {
	StepID     string
	Seq        int
	AttemptID  string
	AttemptNo  int
	DeadlineAt time.Time
}

// AttemptFailure records one attempt's failure under SPEC.md 8.3's CAS.
type AttemptFailure struct {
	RunID     string
	AttemptID string

	// Reason is one of model.Failure*. Leaving it empty asks the backend to
	// decide between timeout and transport_error by the clock, which SPEC.md
	// 5.3 requires: "an attempt is timeout only if deadline_at has passed; a
	// connection refused at second 3 of a 300-second budget is
	// transport_error". The backend decides it because the deadline lives in
	// the row and the comparison must be made against the same clock that
	// wrote it.
	Reason string

	ErrorText string
}

// StepDeadLetterInput is the worker-side dead-letter of SPEC.md 12.2 and 12.3:
// step → DLQ, run → DLQ and the dead-letter entry, in one transaction.
type StepDeadLetterInput struct {
	RunID     string
	StepID    string
	ErrorText string
}

// PlannerCallRecord is one row of SPEC.md 6.8, handed to whichever method
// writes the effect that call produced, so that the two land in one
// transaction. SPEC.md 4.2: "every one of those four rows also writes the call
// itself into planner_calls, in the same transaction."
//
// The backend supplies call_no and replay_round: SPEC.md 6.8 defines the first
// as the run's own ordering, which only one writer may assign, and the second
// as "the value of runs.replay_count when the call was made", which is a column
// the caller would otherwise have to have read and could have read stale.
type PlannerCallRecord struct {
	// Answer is one of model.Answer* on a DONE call, empty on a FAILED one;
	// FailureReason is one of SPEC.md 5.8's three on a FAILED call, empty on a
	// DONE one. Exactly one is set — SPEC.md 6.8's invariants 1 and 2.
	Answer        string
	FailureReason string

	// ErrorText is required when the call FAILED and when the answer was fail
	// (SPEC.md 6.8 invariant 3), and permitted otherwise.
	ErrorText string

	StartedAt  time.Time
	FinishedAt time.Time
}

// Failed reports whether this record describes a failed call rather than an
// answered one.
func (c PlannerCallRecord) Failed() bool { return c.FailureReason != "" }

// PlannerFailureInput is one failed planner call (SPEC.md 4.2, SPEC.md 12.2).
// The backend writes the FAILED planner_calls row, increments
// runs.planner_attempt_count in the same transaction and, if that has reached
// the workflow's planner_max_attempts, writes the planner-side dead-letter
// entry and takes the run to DLQ.
//
// There is no dead-letter reason to choose: SPEC.md 6.5 leaves the planner side
// exactly one reason for a budget that ran out, because the KIND of each
// failure is on the call's own row.
type PlannerFailureInput struct {
	RunID string
	Call  PlannerCallRecord
}

// PlannerDeclaredFailInput is SPEC.md 9.3's `fail` answer: not a failure but a
// valid answer (SPEC.md 12.1), so it writes a DONE call whose answer is fail,
// the dead-letter entry and run → DLQ in one transaction, and burns no budget.
type PlannerDeclaredFailInput struct {
	RunID string
	Call  PlannerCallRecord

	// ErrorText is what the dead-letter entry records — SPEC.md 9.3 makes
	// `reason` part of a fail answer, and SPEC.md 12.4 requires the entry to
	// say why the run stopped.
	ErrorText string
}

// HistoryStep is one row of SPEC.md 9.2's catalogue: "step_id, step_name, seq,
// status, output_bytes, attempt_count, completed_at".
//
// It carries OutputBytes and never the output itself. SPEC.md 9.2: "history is
// a catalogue only. It never carries outputs — that is what output_bytes is
// for. A planner that wants an output fetches it from the read API."
type HistoryStep struct {
	StepID       string
	StepName     *string
	Seq          int
	Status       string
	OutputBytes  int
	AttemptCount int
	CompletedAt  *time.Time
}

// PlannerHistoryLimit is SPEC.md 9.2's cap: "history carries at most the most
// recent 100 steps".
const PlannerHistoryLimit = 100

// Store is the whole storage contract.
type Store interface {
	// Ping is what GET /healthz reports on (SPEC.md 10.4: "liveness,
	// including whether storage is reachable").
	Ping(ctx context.Context) error
	Close() error

	// Migrate brings the schema up to date. SPEC.md 18.1 requires migrations
	// to "run to completion before the orchestrator serves traffic".
	Migrate(ctx context.Context) error

	// --- the operator's surface (SPEC.md 10.1, 10.2) ------------------------

	CreateWorkflow(ctx context.Context, wf *model.Workflow) error
	GetWorkflow(ctx context.Context, workflowID string) (*model.Workflow, error)
	CreateRun(ctx context.Context, run *model.Run) error
	GetRun(ctx context.Context, runID string) (*model.Run, error)
	ListSteps(ctx context.Context, runID string) ([]*model.Step, error)
	ListAttempts(ctx context.Context, runID string) ([]*model.Attempt, error)

	// ReplayRun is SPEC.md 14, and it returns the run's new replay_count.
	//
	// It is an OPERATOR action rather than a driver action, which is why it
	// takes no orchestrator_id and does not open with the ownership fence of
	// SPEC.md 8.2. A run in DLQ has no owner - SPEC.md 6.2 makes owner_id
	// non-NULL only while status = 'RUNNING', and SPEC.md 8.7's fourth writer
	// cleared it in the same transaction that wrote the DLQ verdict - so there
	// is no ownership to test. SPEC.md 14 names its own gate instead, and it
	// is a CAS in the sense of SPEC.md 8.1: "the transaction that takes the
	// run out of DLQ IS the gate, so a double-click has exactly one winner."
	//
	// It returns ErrNotFound when no such run exists (SPEC.md 10.5's 404) and
	// ErrNotInDLQ when the run exists but the gate refused it (the 409).
	ReplayRun(ctx context.Context, runID string) (replayCount int, err error)

	// --- assembling SPEC.md 9.5's `inputs` ---------------------------------
	//
	// SPEC.md 9.4 defines input_from as "which completed steps' outputs to
	// assemble into inputs", so both of these return a step only when it is
	// DONE. They are two methods rather than one because SPEC.md 9.4 gives
	// input_from two ways of naming a step: by identity, and — when the key is
	// omitted — by position, "the previous step only".
	//
	// They fetch one output at a time on purpose. SPEC.md 10.2 gives the same
	// reason for splitting the read API in two: "the catalogue is cheap and may
	// be fetched whole, while outputs may be large and are fetched
	// individually", and an assembler that read every step of the run to find
	// one of them would defeat that.

	StepOutputAtSeq(ctx context.Context, runID string, seq int) (stepID string, output []byte, err error)
	StepOutputByID(ctx context.Context, runID, stepID string) (output []byte, err error)

	// GetStep serves SPEC.md 10.2's GET /steps/{step_id}/output, which
	// addresses a step by its own identity and knows no run. It returns
	// ErrNotFound when no such step exists (SPEC.md 10.5's 404); a step that
	// exists but holds no output is a state for the caller to report, not an
	// absence, so it comes back as a step whose Output is nil.
	GetStep(ctx context.Context, stepID string) (*model.Step, error)

	// --- coordination metadata (SPEC.md 3.4, 8.5, 8.7) ---------------------
	//
	// None of these is governed by the transaction rules of SPEC.md 8, which
	// is the whole point of SPEC.md 3.4's distinction: the heartbeat is a
	// write every ten seconds outside any business transaction, and it is not
	// a violation of the ownership rules because it is a different kind of
	// state.

	RegisterOrchestrator(ctx context.Context, orchestratorID string) error
	Heartbeat(ctx context.Context, orchestratorID string) error
	ReleaseOwned(ctx context.Context, orchestratorID string) error
	ClaimRuns(ctx context.Context, orchestratorID string, leaseTTL time.Duration) ([]string, error)
	OwnedRunningRuns(ctx context.Context, orchestratorID string) ([]string, error)

	// --- the driving loop (SPEC.md 4.2), every one of them fenced ----------

	LoadDriverState(ctx context.Context, orchestratorID, runID string) (*DriverState, error)
	BeginStep(ctx context.Context, orchestratorID string, in BeginStepInput) (*Dispatch, error)
	BeginAttempt(ctx context.Context, orchestratorID string, in BeginAttemptInput) (*Dispatch, error)

	// CompleteRun is the planner's `done`: run → DONE with owner_id and
	// claimed_at cleared, and the call that said so, in the same transaction
	// (SPEC.md 8.7 fourth writer; SPEC.md 4.2's L1 table).
	CompleteRun(ctx context.Context, orchestratorID, runID string, call PlannerCallRecord) error

	// PlannerHistory is SPEC.md 9.2's catalogue for one run: every DONE step,
	// oldest first, capped at PlannerHistoryLimit and keeping the most recent.
	//
	// SPEC.md 9.2 says every row has status DONE, and says why that is not an
	// independent rule: "the planner is asked only in state L1, where
	// last_step = DONE. This follows from SPEC.md 5.5."
	PlannerHistory(ctx context.Context, runID string) ([]HistoryStep, error)

	// RecordAttemptSuccess writes the attempt's outcome under SPEC.md 8.3 and
	// promotes its output to the step (SPEC.md 4.2, L2 / DONE).
	RecordAttemptSuccess(ctx context.Context, orchestratorID, runID, stepID, attemptID string, output []byte) error

	// RecordAttemptFailure writes the attempt's outcome under SPEC.md 8.3. It
	// makes no budget decision: that is SPEC.md 12.2's, taken on the driver's
	// next pass from the step's own counter.
	RecordAttemptFailure(ctx context.Context, orchestratorID string, in AttemptFailure) error

	// ExpireAttempt is SPEC.md 4.2's "RUNNING, deadline passed" and SPEC.md
	// 8.6's claim-time rule. The backend picks between timeout and orphaned,
	// because SPEC.md 5.3 defines orphaned as "timeout, where the attempt's
	// dispatching orchestrator was not live when the attempt was expired" —
	// a question only the database can answer.
	ExpireAttempt(ctx context.Context, orchestratorID string, in AttemptFailure, leaseTTL time.Duration) error

	DeadLetterStep(ctx context.Context, orchestratorID string, in StepDeadLetterInput) error

	// RecordPlannerFailure reports whether this failure exhausted the planner
	// budget and sent the run to DLQ (SPEC.md 12.2).
	RecordPlannerFailure(ctx context.Context, orchestratorID string, in PlannerFailureInput) (deadLettered bool, err error)

	// DeadLetterPlannerFail is SPEC.md 9.3's `fail`: the run stops at once and
	// no budget moves (SPEC.md 12.1).
	DeadLetterPlannerFail(ctx context.Context, orchestratorID string, in PlannerDeclaredFailInput) error
}
