// Package core defines the four interfaces and all shared types for StateFlow.
// Authoritative: DESIGN.md §1 (types) and §2 (interfaces).
//
// The entire orchestrator loop calls only these four interfaces; every other
// package in this module implements one of them.
package core

import (
	"context"
	"encoding/json"
	"time"
)

// --- Identity types ---

type RunID     string
type StepID    string   // "{run_id}:{step_name}"
type AttemptID string   // UUID

// --- Data types exchanged between orchestrator, planner, and transport ---

// RunState is sent BY the orchestrator TO the planner on each Decide call.
// History is ordered by seq ASC; the planner must not re-order it.
type RunState struct {
	RunID         RunID           `json:"run_id"`
	WorkflowInput json.RawMessage `json:"workflow_input"`
	History       []HistoryEntry  `json:"history"` // ordered by seq ASC
}

// HistoryEntry is one completed step in the run history.
// Output has no omitempty — it is always present for DONE steps and null for others.
type HistoryEntry struct {
	Name   string          `json:"name"`
	Status string          `json:"status"`
	Output json.RawMessage `json:"output"`
}

// StepDecision is returned BY the planner TO the orchestrator.
type StepDecision struct {
	Status string    `json:"status"`        // "continue" | "done" | "fail"
	Step   *StepSpec `json:"step,omitempty"`
}

// StepSpec is the planner's description of one unit of work.
// It is stored as the `decision` JSONB column on a step row (Barrier 1).
type StepSpec struct {
	Name           string          `json:"name"`
	WorkerURL      string          `json:"worker_url"`
	Mode           string          `json:"mode"`                   // "sync" | "async"
	TimeoutSeconds int             `json:"timeout_seconds"`
	Input          json.RawMessage `json:"input"`
	OutputField    string          `json:"output_field,omitempty"` // sync only; see DESIGN.md §6.4
}

// Result is returned BY WorkerTransport.Dispatch.
// The transport layer determines success/failure and writes Status.
// The orchestrator loop only reads Status — it never re-interprets HTTP codes.
// Checkpoint routes on Status: "done" → Path A; "failed" → Path B.
type Result struct {
	Status     string          `json:"status"`                 // "done" | "failed"
	Output     json.RawMessage `json:"output,omitempty"`
	Error      string          `json:"error,omitempty"`
	HTTPStatus int             `json:"http_status,omitempty"`  // populated by sync transport only
}

// Frontier is returned BY StateStore.LoadFrontier.
// Carries everything recovery needs in a single read:
//   - History for the planner (DONE steps in seq order)
//   - PendingDecision: a DECIDED-or-RUNNING step with no output, to re-dispatch
//     without re-asking the planner (Barrier 1 already fired for it)
type Frontier struct {
	RunID           RunID
	History         []HistoryEntry // DONE steps, ordered by seq ASC
	PendingDecision *StepSpec      // non-nil = re-dispatch this; do NOT re-ask the planner
}

// --- The four interfaces ---

// NextStepPlanner decides the next step given the current run state.
// The loop calls Decide once per step; the answer is persisted (Barrier 1) before
// any dispatch, so a non-deterministic planner (e.g. an LLM) is safe.
//
// Reference impls: StaticPlanner (internal/planner/static.go),
//                  HTTPPlanner   (internal/planner/http.go).
// Extension point: rules engine, different LLM harness.
type NextStepPlanner interface {
	Decide(ctx context.Context, state RunState) (StepDecision, error)
}

// WorkerTransport dispatches a step to a worker and returns its result.
// BOTH sync and async implementations BLOCK and return a Result — the loop is
// oblivious to the mode distinction. See DESIGN.md §6.2 for the block-in-dispatch
// design and why Barrier 2 must be written by the loop, not the callback handler.
//
// Reference impls: SyncTransport  (internal/transport/sync.go),
//                  AsyncTransport (internal/transport/async.go).
// Extension point: MCP transport, gRPC, message queue.
type WorkerTransport interface {
	Dispatch(ctx context.Context, step StepSpec) (Result, error)
}

// StateStore is the durable source of truth. All correctness rests here.
// The two write barriers are the two durable writes this interface enforces:
//   PutDecision  → Barrier 1 (persist decision before dispatch)
//   Checkpoint   → Barrier 2 (persist result before next Decide)
//
// Reference impl: PostgresStore (internal/store/postgres.go).
// Extension point: MySQL, SQLite, cloud KV.
type StateStore interface {
	// LoadFrontier returns the full frontier for recovery and loop re-entry.
	// DONE steps go into History (ordered by seq ASC).
	// A DECIDED/RUNNING step with no output becomes PendingDecision.
	LoadFrontier(run RunID) (Frontier, error)

	// PutDecision writes the planner's chosen step with status DECIDED.
	// This is Barrier 1: it must commit before Dispatch is called.
	PutDecision(run RunID, step StepSpec) error

	// Checkpoint writes the worker's result and advances the step to DONE.
	// This is Barrier 2: it must commit before the next Decide is called.
	// On r.Status == "failed", Path B handles retry / DLQ (DESIGN.md §9.1).
	Checkpoint(run RunID, step StepSpec, r Result) error

	// PendingDecision returns the DECIDED/RUNNING step with no output, if any.
	// Used by the loop's fast path; LoadFrontier subsumes this for recovery.
	PendingDecision(run RunID) (*StepSpec, error)
}

// RetryPolicy decides whether and when to retry a failed step.
// The reference impl is FixedCountPolicy: max_retries=3, retry_delay=5s fixed.
// Exponential backoff and LLM-aware retry_after are deferred (DESIGN.md §9.2).
type RetryPolicy interface {
	// Next is called after a step failure with the number of attempts so far.
	// Returns the delay before the next attempt, or toDLQ=true if retries are
	// exhausted and the step should be routed to the dead-letter queue.
	Next(attempt int, err error) (delay time.Duration, toDLQ bool)
}
