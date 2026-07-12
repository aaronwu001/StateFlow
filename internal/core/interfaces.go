// Package core defines the shared types and the four interfaces of StateFlow's
// v1.0 three-state model.
//
// Authoritative: docs/StateFlow_Whitepaper_v1_0.md (design) and
// docs/StateFlow_Rules_Consolidation_v3_EN.md (rule-by-rule rationale). On any
// divergence between this file and those documents, the documents win.
//
// The entire orchestrator loop calls only NextStepPlanner, WorkerTransport,
// RetryPolicy, and StateStore; every other package in this module implements
// one of them (whitepaper §20).
package core

import (
	"context"
	"encoding/json"
	"time"
)

// --- Identity types ---
//
// These are distinct named types, not interchangeable strings: a StepID
// cannot be assigned where an AttemptID is expected, and vice versa (T3).

type RunID string      // "run-{uuid}", server-generated always
type StepID string     // "{run_id}:{step_name}", stored not computed
type AttemptID string  // UUID
type WorkflowID string // "wf-{uuid}" or client-supplied
type DLQEntryID int64  // dead_letter_queue.id (BIGSERIAL)

// --- Closed status enums ---
//
// Every value below is a Go string constant equal, byte-for-byte, to the
// corresponding Postgres CHECK constraint value (T3). Do not introduce a
// bare string anywhere one of these types is expected.

// RunStatus is one of the run's three states (whitepaper §4.2).
type RunStatus string

const (
	RunStatusRunning RunStatus = "RUNNING"
	RunStatusDone    RunStatus = "DONE"
	RunStatusDLQ     RunStatus = "DLQ"
)

// StepStatus is one of the step's three states (whitepaper §4.2). DECIDED
// and FAILED from the v0.9 five-state model no longer exist: neither
// exclusively owned a recovery rule (whitepaper §16, §Q7).
type StepStatus string

const (
	StepStatusRunning StepStatus = "RUNNING"
	StepStatusDone    StepStatus = "DONE"
	StepStatusDLQ     StepStatus = "DLQ"
)

// AttemptStatus is one of the attempt's three states (whitepaper §4.2).
type AttemptStatus string

const (
	AttemptStatusRunning AttemptStatus = "RUNNING"
	AttemptStatusDone    AttemptStatus = "DONE"
	AttemptStatusFailed  AttemptStatus = "FAILED"
)

// FailureReason is the mandatory reason an attempt carries when, and only
// when, its status is AttemptStatusFailed (whitepaper §4.2 table). All four
// reasons take the identical path (TX3) and consume the retry budget
// equally — the classification exists for human triage, not machine
// branching (whitepaper §7.1).
type FailureReason string

const (
	FailureReasonWorkerReported FailureReason = "worker_reported"
	FailureReasonTimeout        FailureReason = "timeout"
	FailureReasonMalformed      FailureReason = "malformed"
	FailureReasonOrphaned       FailureReason = "orphaned"
)

// DLQReason classifies a dead_letter_queue row. Purely informational —
// replay mechanics are identical for all four — but it drives human triage
// (whitepaper §11).
type DLQReason string

const (
	DLQReasonWorkerRetryExhausted DLQReason = "worker_retry_exhausted"
	DLQReasonPlannerUnreachable   DLQReason = "planner_unreachable"
	DLQReasonPlannerMalformed     DLQReason = "planner_malformed"
	DLQReasonPlannerDeclaredFail  DLQReason = "planner_declared_fail"
)

// ResultStatus is WorkerTransport.Dispatch's verdict on one attempt. It has
// only two values: a transport either produced a usable answer or it did
// not. RUNNING is not a transport concept — it is only a stored attempt
// state before any answer exists.
type ResultStatus string

const (
	ResultStatusDone   ResultStatus = "done"
	ResultStatusFailed ResultStatus = "failed"
)

// PlannerVerdict is the planner's own three-way vocabulary (whitepaper
// §12.3). It is distinct from RunStatus/StepStatus/AttemptStatus: this is
// the planner's wire contract, lowercase, not a stored state.
type PlannerVerdict string

const (
	PlannerVerdictContinue PlannerVerdict = "continue"
	PlannerVerdictDone     PlannerVerdict = "done"
	PlannerVerdictFail     PlannerVerdict = "fail"
)

// StepMode selects the dispatch/reporting contract for one step (whitepaper
// §13).
type StepMode string

const (
	StepModeSync  StepMode = "sync"
	StepModeAsync StepMode = "async"
)

// --- Data types exchanged between orchestrator, planner, and transport ---

// RunState is sent BY the orchestrator TO the planner on each Decide call.
// History is ordered by seq ASC; the planner must not re-order it. Every
// status string on the wire is UPPERCASE, identical to the stored values.
type RunState struct {
	RunID         RunID           `json:"run_id"`
	WorkflowInput json.RawMessage `json:"workflow_input"`
	History       []HistoryEntry  `json:"history"` // ordered by seq ASC
}

// HistoryEntry is one completed (DONE) step in the run history.
//
// Output is NOT always present on the wire. The orchestrator loop applies a
// two-tier size guard (registry #1, whitepaper §12.2) to every RunState it
// sends to the planner: an Output over the 2KB per-entry cap is replaced
// with a small pointer object (`_truncated`/`size_bytes`/a note pointing at
// GET /runs/{run_id}` for the real value), and once the cumulative wire size
// of the History array would exceed the 50KB total cap, older entries (past
// the most-recent-first budget walk) omit Output entirely — omitempty
// applies. This is purely a wire-marshaling concern: Postgres always keeps
// the full, untruncated output; nothing about persistence changes.
type HistoryEntry struct {
	Name   string          `json:"name"`
	Status StepStatus      `json:"status"`
	Output json.RawMessage `json:"output,omitempty"`
}

// StepDecision is returned BY the planner TO the orchestrator.
type StepDecision struct {
	Status PlannerVerdict `json:"status"`
	Step   *StepSpec      `json:"step,omitempty"` // present iff Status == PlannerVerdictContinue
}

// StepSpec is the planner's description of one unit of work. It is stored
// verbatim as the `decision` JSONB column on a step row at Barrier 1 (TX1);
// recovery re-dispatches from this stored copy, which is what guarantees the
// planner is asked exactly once per step.
type StepSpec struct {
	Name      string          `json:"name"`
	WorkerURL string          `json:"worker_url"`
	Mode      StepMode        `json:"mode"`
	Input     json.RawMessage `json:"input"`

	// TimeoutSeconds is this attempt's lifetime ceiling (whitepaper §6).
	// 0 ⇒ inherit the workflow's DefaultTimeoutSeconds ⇒ inherit the system
	// default of 60s if that is also 0. Resolution happens once, at
	// dispatch time; the resolved value is never written back into the
	// stored StepSpec.
	TimeoutSeconds int `json:"timeout_seconds"`

	// OutputField names the subtree of a sync 2xx body to checkpoint as
	// output; sync only (internal/transport/sync.go's extractOutput). Its
	// absence in the response body is a FailureReasonMalformed failure, not
	// a missing field.
	//
	// Async has no OutputField subtree concept — there is nothing here to
	// declare for an async step. Async gets its own, independent malformed
	// mechanism instead: internal/api/server.go's handleTaskComplete
	// classifies an async /tasks/complete callback whose "output" is absent
	// from the JSON body, or present but the JSON literal null, as
	// FailureReasonMalformed. The two mechanisms are unrelated checks over
	// unrelated wire shapes (a subtree-presence check against a declared
	// field name for sync; an absence/null check with no field name for
	// async) that both terminate at the same FailureReasonMalformed value.
	OutputField string `json:"output_field,omitempty"`
}

// ResultFailure is the detail attached to a failed Result. It exists only on
// the failure branch (T3): a done Result has Failure == nil, so a success
// cannot carry a failure_reason at the type layer.
//
// Reason is one of the two transport-observable reasons — WorkerReported
// (non-2xx, or an async fail callback) or Malformed (an unusable success
// body). Timeout and Orphaned are never produced by a transport: they are
// assigned by the loop itself (a context deadline, or a recovery orphan
// claim) and passed straight to StateStore.RecordFailure without ever
// passing through a Result.
type ResultFailure struct {
	Reason     FailureReason
	Error      string
	HTTPStatus int // populated by the sync transport only
}

// Result is returned by WorkerTransport.Dispatch. The loop routes on Status
// alone — done → Path A (Barrier 2/TX2), failed → Path B (TX3) — and never
// re-interprets HTTP codes itself.
//
// A non-nil error return from Dispatch (distinct from Result.Failure) means
// the transport could not classify the outcome itself — e.g. ctx's deadline
// fired — and the loop must classify it (a context deadline becomes
// FailureReasonTimeout).
type Result struct {
	Status  ResultStatus
	Output  json.RawMessage // set iff Status == ResultStatusDone
	Failure *ResultFailure  // set iff Status == ResultStatusFailed
}

// Frontier is returned by StateStore.LoadFrontier — everything the loop and
// crash recovery need about one run in a single read (whitepaper §20:
// "LoadFrontier ... subsumes last-step lookup").
//
//   - History holds every DONE step's output, ordered by seq ASC.
//   - PendingStep is nil when the last step is DONE, or there are no steps
//     yet (combination-table row 1, §8.2): the caller re-asks the planner.
//   - PendingStep is non-nil when the last step is RUNNING (combination-table
//     row 2): the caller must not re-ask the planner — Barrier 1 already
//     fired for this step — and instead drives recovery's three-step
//     (§8.3) using PendingAttemptID (the live attempt to claim/supersede)
//     and AttemptCount (the persisted retry budget, for the budget check).
type Frontier struct {
	RunID       RunID
	History     []HistoryEntry
	PendingStep *StepSpec

	// PendingAttemptID is the attempt to claim on recovery: pass it
	// straight to RecordFailure(reason=orphaned) unconditionally. Recovery
	// never reads the attempt's current status first — CAS-A inside
	// RecordFailure already handles "no longer RUNNING" as a no-op
	// (ReportSuperseded), so a claim on an attempt that somehow already
	// resolved is safe and requires no separate check.
	// Valid only when PendingStep != nil.
	PendingAttemptID AttemptID

	// AttemptCount is the persisted retry budget (steps.attempt_count).
	// Valid only when PendingStep != nil.
	AttemptCount int
}

// RunRef is a lightweight reference to a RUNNING run, returned by
// StateStore.ListRunningRuns for the crash-recovery scan at startup
// (whitepaper §8.3). WorkflowID lets recovery reconstruct the planner
// instance from the workflow row (whitepaper §12.1: "the loop and recovery
// reconstruct the planner instance from the workflow row every time").
type RunRef struct {
	RunID         RunID
	WorkflowID    WorkflowID
	WorkflowInput json.RawMessage
}

// WorkflowDef is a workflow definition, persisted by CreateWorkflow (TX-W)
// and re-read to reconstruct the planner on every loop iteration and on
// recovery (whitepaper §5, §12.1).
type WorkflowDef struct {
	Name          string
	PlannerType   string // "static" | "http"
	PlannerConfig json.RawMessage

	// DefaultTimeoutSeconds is the workflow-level timeout override.
	// 0 ⇒ inherit the system default of 60s (whitepaper §6).
	DefaultTimeoutSeconds int

	// RetryLimit is X: the attempt_count threshold at which a step's next
	// failure routes to the DLQ instead of a new attempt (whitepaper §7.1).
	// Not an independent persisted column: it lives inside PlannerConfig's
	// JSON under the key "retry_limit". This field is the parsed-out,
	// typed convenience value — CreateWorkflow/GetWorkflow are responsible
	// for keeping it in sync with PlannerConfig, never a second source of
	// truth.
	RetryLimit int
}

// RunSummary is the read-side projection for GET /runs/{run_id}.
type RunSummary struct {
	RunID  RunID
	Status RunStatus
	Steps  []StepSummary

	// DLQReason is non-nil only when Status == RunStatusDLQ. When a run has
	// been replayed and DLQ'd more than once, this is the reason of the
	// most recent dead_letter_queue row for this run.
	DLQReason *DLQReason
}

// StepSummary is one step within a RunSummary.
type StepSummary struct {
	StepID   StepID
	Name     string
	Status   StepStatus
	Attempts []AttemptSummary
}

// AttemptSummary is one attempt within a StepSummary.
type AttemptSummary struct {
	AttemptID AttemptID
	Status    AttemptStatus
	Reason    *FailureReason // non-nil only when Status == AttemptStatusFailed
}

// DLQEntry is one dead_letter_queue row, returned by ListDLQ/GetDLQEntry.
type DLQEntry struct {
	ID      DLQEntryID
	RunID   RunID
	StepID  *StepID // nil for planner-side entries (no step at fault)
	Reason  DLQReason
	Context json.RawMessage // per-attempt reasons/errors, run snapshot
}

// --- Typed CAS outcomes (architect review, T1) ---
//
// A report (success or failure) that matches nothing under CAS-A — stale,
// duplicate, or arriving after a verdict already landed — is a normal,
// expected outcome, not an error. Callers must not treat ReportSuperseded
// as a failure condition.

// ReportOutcome distinguishes a CAS-validated write that actually landed
// from a stale/duplicate report that matched nothing.
type ReportOutcome int

const (
	// ReportCommitted means attempt_id matched steps.current_attempt_id AND
	// the attempt's status was still RUNNING at the moment of the update;
	// the write happened.
	ReportCommitted ReportOutcome = iota
	// ReportSuperseded means the CAS predicate matched nothing: the report
	// is stale, a duplicate, or arrived after the attempt already reached a
	// terminal state by another path. Zero state effect (whitepaper §10).
	ReportSuperseded
)

// FailureOutcome is RecordFailure's typed result: whether the report landed
// at all, and — only when it did — whether that failure pushed the step
// into the DLQ.
type FailureOutcome struct {
	Report ReportOutcome
	DLQed  bool // valid only when Report == ReportCommitted
}

// --- The four interfaces ---

// NextStepPlanner decides the next step given the current run state. The
// loop calls Decide once per step; the answer is persisted (Barrier 1)
// before any dispatch, so a non-deterministic planner (e.g. an LLM) is
// safe. Planner retry counting for unreachable/malformed answers lives in
// memory and resets on crash — safe because a planner call is side-effect
// free (whitepaper §6, §7.2).
//
// Reference impls: StaticPlanner (internal/planner/static.go),
//
//	HTTPPlanner   (internal/planner/http.go).
type NextStepPlanner interface {
	Decide(ctx context.Context, state RunState) (StepDecision, error)
}

// WorkerTransport dispatches a step to a worker and returns its result.
// BOTH sync and async implementations BLOCK and return a Result — the loop
// is oblivious to the mode distinction (whitepaper §13). The caller must
// pass a ctx carrying the attempt's deadline (created_at + resolved
// timeout, §6); a ctx-deadline return is the "stuck / silent" case, which
// the loop — not the transport — classifies as FailureReasonTimeout.
//
// Reference impls: SyncTransport  (internal/transport/sync.go),
//
//	AsyncTransport (internal/transport/async.go).
type WorkerTransport interface {
	Dispatch(ctx context.Context, step StepSpec) (Result, error)
}

// RetryPolicy decides whether and when to retry a failed step. The
// reference impl is a fixed-count, fixed-delay policy (retry limit X,
// 5s delay).
//
// Binding contract: count MUST be the persisted steps.attempt_count read
// from the store, never a caller-local counter. This is the exact defect
// the v1.0 refactor targets — pre-refactor, the loop passed a loop-local
// counter that reset to 1 on every crash, silently discarding the retry
// budget. The signature is unchanged from v0.9; the contract on where
// count comes from is now binding (whitepaper §20; Session 2 instructions).
type RetryPolicy interface {
	// Next is called after a step failure with the attempt_count so far.
	// Returns the delay before the next attempt, or toDLQ=true if the
	// budget is exhausted and the step should route to the DLQ.
	Next(count int, err error) (delay time.Duration, toDLQ bool)
}

// StateStore is the durable source of truth. All correctness rests here.
// Its method set is derived directly from the Atomic Transaction Ledger
// (whitepaper §19): one method per TX (or parameterized merges) plus the
// read side. Contract clause (whitepaper §20): every TXn method MUST be
// implemented as a single database transaction — this is interface
// semantics, not an implementation suggestion. A backend that cannot
// provide equivalent atomicity is not a conforming implementation.
//
// No method on this interface accepts a persisted timestamp as a parameter
// (T2): every stored timestamp is produced by the store itself, inside the
// transaction, via the database's now() (clock rule, whitepaper §3/§4.1).
//
// Reference impl: PostgresStore (internal/store/postgres.go).
type StateStore interface {
	// ── Setup ──

	// CreateWorkflow persists a new workflow definition.
	// MUST execute as a single database transaction (Ledger TX-W).
	CreateWorkflow(ctx context.Context, def WorkflowDef) (WorkflowID, error)

	// CreateRun persists a new run in RUNNING state.
	// MUST execute as a single database transaction (Ledger TX0).
	CreateRun(ctx context.Context, workflow WorkflowID, input json.RawMessage) (RunID, error)

	// ── The two write barriers ──

	// CreateStepWithAttempt persists a new step (RUNNING, next seq,
	// attempt_count=0, decision=spec) and its first attempt (RUNNING), and
	// sets steps.current_attempt_id to point at it. This is Barrier 1: the
	// caller may dispatch only after this commits, and the attempt's
	// timeout clock starts at this transaction's commit, not at dispatch.
	// MUST execute as a single database transaction (Ledger TX1).
	CreateStepWithAttempt(ctx context.Context, run RunID, spec StepSpec) (StepID, AttemptID, error)

	// CheckpointSuccess lands a successful report: attempt→DONE,
	// step→DONE, output persisted. This is Barrier 2: the caller may ask
	// the planner again only after this commits. The write is CAS-gated on
	// attempt_id == current_attempt_id AND the attempt still RUNNING (CAS-A);
	// a report that matches nothing returns ReportSuperseded, never an
	// error (T1) — in particular, a success arriving after the attempt was
	// already pronounced failed(timeout) is rejected this way (whitepaper
	// §10.3).
	// MUST execute as a single database transaction (Ledger TX2).
	CheckpointSuccess(ctx context.Context, step StepID, attempt AttemptID, output json.RawMessage) (ReportOutcome, error)

	// ── Worker-side failure, retry, and budget ──

	// RecordFailure lands a failed report, or a loop-detected timeout/orphan
	// verdict: attempt→FAILED(reason), attempt_count++. If the new count
	// reaches retryLimit, the SAME transaction also sets step→DLQ,
	// run→DLQ, and inserts a dead_letter_queue row (reason
	// worker_retry_exhausted, context carrying per-attempt detail) — verdict
	// and DLQ fall in one blade (whitepaper §7.1). CAS-gated exactly as
	// CheckpointSuccess; a superseded report is a no-op, never an error (T1).
	// MUST execute as a single database transaction (Ledger TX3).
	RecordFailure(ctx context.Context, step StepID, attempt AttemptID, reason FailureReason, detail string, retryLimit int) (FailureOutcome, error)

	// StartNewAttempt creates a new attempt (RUNNING) for a step already
	// below its retry limit and CAS-updates steps.current_attempt_id to
	// point at it. No instant exists at which two attempt ids are both
	// valid (whitepaper §7.1).
	// MUST execute as a single database transaction (Ledger TX4).
	StartNewAttempt(ctx context.Context, step StepID) (AttemptID, error)

	// ── Replay (operator-triggered, from the DLQ) ──

	// ReplayWorkerSide resets a worker-side DLQ step for retry:
	// attempt_count→0, step→RUNNING, run→RUNNING, a new attempt created,
	// current_attempt_id set. The reset is mandatory — without it the
	// count still equals retryLimit and the step falls straight back to
	// the DLQ (whitepaper §11).
	// MUST execute as a single database transaction (Ledger TX5).
	ReplayWorkerSide(ctx context.Context, run RunID) (StepID, AttemptID, error)

	// ReplayPlannerSide resets a planner-side DLQ run for retry: run→RUNNING.
	// The next loop iteration re-asks the planner from the existing history.
	// MUST execute as a single database transaction (Ledger TX6).
	ReplayPlannerSide(ctx context.Context, run RunID) error

	// ── Run terminal states ──

	// MarkRunDone sets run→DONE. Valid only when the last step is DONE
	// (Barrier 2's postcondition) — the planner answering "done" is the
	// only caller.
	// MUST execute as a single database transaction (Ledger TX7).
	MarkRunDone(ctx context.Context, run RunID) error

	// MarkRunDLQPlannerDeclared sets run→DLQ and inserts a
	// dead_letter_queue row with reason planner_declared_fail (step_id
	// NULL), when the planner itself answers "fail". A legitimate answer,
	// not a failure — consumes no budget (whitepaper §7.2).
	// MUST execute as a single database transaction (Ledger TX8).
	MarkRunDLQPlannerDeclared(ctx context.Context, run RunID, detail string) error

	// MarkRunDLQPlannerExhausted sets run→DLQ and inserts a
	// dead_letter_queue row when the planner call budget (30s × 3 attempts)
	// is exhausted. reason must be DLQReasonPlannerUnreachable or
	// DLQReasonPlannerMalformed — the category of the final failed
	// attempt; detail carries every attempt's context (whitepaper §7.2).
	// MUST execute as a single database transaction (Ledger TX9).
	MarkRunDLQPlannerExhausted(ctx context.Context, run RunID, reason DLQReason, detail string) error

	// ── Reads ──

	// GetWorkflow returns a workflow definition by id. The loop and
	// recovery call this on every iteration to reconstruct the planner
	// instance from the stored row (whitepaper §12.1) — RunRef.WorkflowID
	// (from ListRunningRuns) and steps' owning run are what feed the id in.
	GetWorkflow(ctx context.Context, workflow WorkflowID) (WorkflowDef, error)

	// LoadFrontier returns the full frontier for the loop's fast path and
	// for crash recovery in a single read.
	LoadFrontier(ctx context.Context, run RunID) (Frontier, error)

	// ListRunningRuns returns a reference for every run with status
	// RUNNING. Used once at startup by crash recovery (§8.3) to find runs
	// to resume.
	ListRunningRuns(ctx context.Context) ([]RunRef, error)

	// GetRun returns a run's current status, its steps, and their attempt
	// summaries, for the status API.
	GetRun(ctx context.Context, run RunID) (RunSummary, error)

	// ListDLQ returns every dead-letter-queue entry, for the DLQ listing API.
	ListDLQ(ctx context.Context) ([]DLQEntry, error)

	// GetDLQEntry returns a single dead-letter-queue entry by id, so the
	// replay handler can tell worker-side (StepID != nil → TX5) from
	// planner-side (StepID == nil → TX6).
	GetDLQEntry(ctx context.Context, id DLQEntryID) (DLQEntry, error)
}
