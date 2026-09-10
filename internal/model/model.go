// Package model holds the entities of SPEC.md 3.2 and the enumerated values of
// SPEC.md 5. It carries no behaviour beyond what a value's own definition
// implies, and it knows nothing about storage or HTTP.
//
// Every JSON-valued field is []byte, not a parsed structure. SPEC.md 7.1 makes
// that a requirement of the storage interface — "opaque []byte for every JSON
// document" — and the same bytes travel through this package unchanged, which
// is what lets SPEC.md 6.3 store "the StepSpec exactly as the planner returned
// it" and SPEC.md 6.2 store the run's input "verbatim".
package model

import "time"

// Run states (SPEC.md 5.1), step states (SPEC.md 5.2) and attempt states
// (SPEC.md 5.3). They are plain strings because they are plain strings in the
// database: SPEC.md 6.2, 6.3 and 6.4 give every status column type TEXT.
const (
	StatusRunning   = "RUNNING"
	StatusDone      = "DONE"
	StatusDLQ       = "DLQ"
	StatusCancelled = "CANCELLED"
	StatusFailed    = "FAILED"
)

// Failure reasons (SPEC.md 5.3). Each is "a diagnostic label, not a distinct
// mechanism"; every value below burns one unit of budget except Cancelled.
//
// SPEC.md 5.8 gives a planner call three of these six, with identical meanings
// — TransportError, InvalidResponse and Timeout — and says why the other three
// have no planner-side form. They are deliberately the same constants and not a
// parallel set: "the operator asking what is killing my runs is asking one
// question, and an answer split across two enumerations with different words
// for the same event would make him ask it twice".
const (
	FailureWorkerError     = "worker_error"
	FailureTransportError  = "transport_error"
	FailureInvalidResponse = "invalid_response"
	FailureTimeout         = "timeout"
	FailureOrphaned        = "orphaned"
	FailureCancelled       = "cancelled"
)

// Dead-letter reasons (SPEC.md 6.5). Three values, and each names a SITUATION
// that stopped a run rather than the kind of any one failure — the kind lives
// on the attempt (SPEC.md 5.3) or on the planner call (SPEC.md 5.8) that had
// it.
const (
	DLQWorkerBudgetExhausted  = "worker_budget_exhausted"
	DLQPlannerBudgetExhausted = "planner_budget_exhausted"
	DLQPlannerDeclaredFail    = "planner_declared_fail"
)

// Planner call states (SPEC.md 5.8). There is no RUNNING: the row is written
// once, at the outcome.
const (
	PlannerCallDone   = StatusDone
	PlannerCallFailed = StatusFailed
)

// The three answers of SPEC.md 9.3, as they are recorded in
// planner_calls.answer (SPEC.md 6.8).
const (
	AnswerContinue = "continue"
	AnswerDone     = "done"
	AnswerFail     = "fail"
)

// Planner types (SPEC.md 6.1).
const (
	PlannerStatic = "static"
	PlannerHTTP   = "http"
)

// Connection modes and dispatch styles (SPEC.md 9.4).
const (
	ConnectionSync  = "sync"
	ConnectionAsync = "async"

	DispatchEnvelope = "envelope"
	DispatchRaw      = "raw"
)

// ErrorTextLimit is SPEC.md 6.4's 4 KB cap on diagnostic text — "one limit,
// both tables", attempts.error_text and dead_letter_queue.error_text. SPEC.md
// 6.4 also fixes who applies it: "the orchestrator truncates before writing;
// it is never the backend's job".
const ErrorTextLimit = 4 << 10

// TruncateError applies that cap. It cuts on a byte boundary rather than a
// rune boundary only when the cap falls mid-rune; the column is diagnostic
// text read by a human, and a trailing partial rune is not worth a second rule.
func TruncateError(s string) string {
	if len(s) <= ErrorTextLimit {
		return s
	}
	cut := ErrorTextLimit
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}

// Workflow is SPEC.md 6.1: a definition, "a template and is never executed
// itself".
type Workflow struct {
	WorkflowID string
	Name       string

	PlannerType string
	// PlannerURL and FetchBaseURL are both present iff PlannerType is http;
	// PlannerStaticSteps is present iff it is static (SPEC.md 6.1's invariant).
	//
	// SPEC.md 6.1: the two URLs are "one relationship, two directions" —
	// PlannerURL is where the orchestrator calls this workflow's planner, and
	// FetchBaseURL is where that same planner reads back (SPEC.md 9.2, 10.2).
	PlannerURL         string
	FetchBaseURL       string
	PlannerStaticSteps []byte

	StepTimeoutSeconds       int
	StepMaxAttempts          int
	StepRetryDelaySeconds    int
	PlannerTimeoutSeconds    int
	PlannerMaxAttempts       int
	PlannerRetryDelaySeconds int

	CreatedAt time.Time
}

// Run is SPEC.md 6.2: one execution of a workflow, "the unit of history and the
// unit of ownership".
type Run struct {
	RunID      string
	WorkflowID string
	Status     string
	Input      []byte

	// PlannerAttemptCount is SPEC.md 6.2's budget counter. It remains a stored
	// column even though every call now has a row (SPEC.md 6.8): it is read on
	// the path that decides whether to call the planner again, and counting
	// rows would make that decision depend on correctly excluding the calls of
	// earlier decision points and earlier replay rounds.
	PlannerAttemptCount int
	ReplayCount         int

	// OwnerID and ClaimedAt are coordination metadata (SPEC.md 3.4), non-NULL
	// only while Status is RUNNING and always written and cleared as a pair
	// (SPEC.md 6.2, 8.7).
	OwnerID   *string
	ClaimedAt *time.Time

	CreatedAt time.Time
}

// Step is SPEC.md 6.3: one decided unit of work at a fixed position seq.
type Step struct {
	StepID   string
	RunID    string
	Seq      int
	StepName *string
	Status   string

	// Decision is the StepSpec exactly as the planner returned it (SPEC.md
	// 6.3), which is why it never leaves this package as a parsed struct and
	// is re-serialised nowhere.
	Decision []byte

	AttemptCount int
	Output       []byte

	CreatedAt   time.Time
	CompletedAt *time.Time
}

// Attempt is SPEC.md 6.4: one execution of a step, one dispatch and one
// outcome.
type Attempt struct {
	AttemptID string
	StepID    string
	RunID     string
	AttemptNo int
	// ReplayRound is SPEC.md 6.4: "the value of runs.replay_count when this
	// attempt was dispatched". SPEC.md 14 leaves earlier attempts in place, so
	// without it a step that has been replayed holds the rows of two rounds
	// with nothing on them saying which round is which.
	ReplayRound    int
	Status         string
	ConnectionMode string
	DeadlineAt     time.Time
	DispatchedBy   string
	Output         []byte
	FailureReason  *string
	ErrorText      *string
	StartedAt      time.Time
	FinishedAt     *time.Time
}

// DeadLetterEntry is SPEC.md 6.5: an append-only historical record that a run
// stopped because a budget was exhausted or the planner refused to continue.
type DeadLetterEntry struct {
	DLQID       string
	RunID       string
	StepID      *string
	Reason      string
	ReplayRound int
	ErrorText   string
	CreatedAt   time.Time
}

// PlannerCall is SPEC.md 6.8: one question put to a run's planner and the
// answer — or the failure — that came back. It is the planner-side counterpart
// of an Attempt (SPEC.md 3.2), and like a dead-letter entry it is append-only
// history (SPEC.md 6.7): written once, at the outcome, and never modified.
type PlannerCall struct {
	PlannerCallID string
	RunID         string

	// CallNo orders the run's whole conversation with its planner, contiguous
	// across decision points and replay rounds (SPEC.md 6.8). ReplayRound is
	// the value of runs.replay_count when the call was made.
	CallNo      int
	ReplayRound int

	// Status is PlannerCallDone or PlannerCallFailed. Exactly one of Answer
	// and FailureReason is set, which is SPEC.md 6.8's invariants 1 and 2.
	Status        string
	Answer        *string
	FailureReason *string
	ErrorText     *string

	CalledBy   string
	StartedAt  time.Time
	FinishedAt time.Time
}

// Orchestrator is SPEC.md 6.6: one row per process boot, whose last_seen_at is
// "the only column a heartbeat touches".
type Orchestrator struct {
	OrchestratorID string
	StartedAt      time.Time
	LastSeenAt     time.Time
}
