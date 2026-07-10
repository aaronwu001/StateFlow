// Package api implements the StateFlow HTTP API server.
// Authoritative: docs/StateFlow_Whitepaper_v1_0.md §11 (DLQ and replay), §13
// (worker contract, dispatch/reporting formats), §19 (Atomic Transaction
// Ledger — TX-W, TX0, TX5, TX6 are driven from this package).
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/transport"
)

// Server is the StateFlow HTTP API server.
//
// It holds both a raw *sql.DB (for read-only projections whose shape is
// specific to this API — GET /runs needs seq/attempt_count/created_at/
// per-attempt error detail that core.RunSummary/StepSummary/AttemptSummary
// do not carry; see the Session 6 report §5/§6 for why those types were not
// extended) and a core.StateStore (for every write that maps onto an Atomic
// Transaction Ledger entry: TX-W, TX0, TX5, TX6). Every TX-ledger write goes
// through the store interface — which is what actually enforces
// single-transaction atomicity (whitepaper §19/§20) — never through raw SQL.
//
// It also never writes step/run state from a callback handler: the handler
// validates the report is well-formed, hands it to the async transport, and
// returns 200 — nothing more (whitepaper §10.4, the single-writer
// principle). All step/run state writes happen inside the loop goroutine.
//
// startLoop is a factory provided by main.go (and, in tests, by the test's
// own wiring). It builds the Loop for a run — from RunID/WorkflowID/
// WorkflowInput alone, since Loop.Run reconstructs the planner from the
// workflow row itself (whitepaper §12.1) — and starts `go loop.Run(ctx)`.
// The server calls it when a new run is created and when a DLQ entry is
// replayed (both TX5 and TX6 branches re-enter the SAME generic loop entry
// point; see handleDLQReplay's doc comment for the worker-side nuance).
type Server struct {
	db        *sql.DB
	store     core.StateStore
	async     *transport.AsyncTransport
	ctx       context.Context // parent context for all loop goroutines
	startLoop func(ctx context.Context, runID core.RunID, workflowID core.WorkflowID, workflowInput json.RawMessage)
}

// New returns a Server ready to serve HTTP.
func New(
	db *sql.DB,
	store core.StateStore,
	async *transport.AsyncTransport,
	ctx context.Context,
	startLoop func(context.Context, core.RunID, core.WorkflowID, json.RawMessage),
) *Server {
	return &Server{db: db, store: store, async: async, ctx: ctx, startLoop: startLoop}
}

// Handler returns the HTTP mux with all routes registered.
// Uses Go 1.22+ pattern matching: "METHOD /path/{param}".
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /workflows", s.handleCreateWorkflow)
	mux.HandleFunc("POST /workflows/{workflow_id}/runs", s.handleStartRun)
	mux.HandleFunc("GET /runs/{run_id}", s.handleGetRun)
	mux.HandleFunc("POST /tasks/complete", s.handleTaskComplete)
	mux.HandleFunc("POST /tasks/fail", s.handleTaskFail)
	mux.HandleFunc("GET /dlq", s.handleGetDLQ)
	mux.HandleFunc("POST /dlq/{id}/replay", s.handleDLQReplay)
	return mux
}

// ── POST /workflows (Ledger TX-W) ─────────────────────────────────────────

func (s *Server) handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string          `json:"name"`
		PlannerType   string          `json:"planner_type"`
		PlannerConfig json.RawMessage `json:"planner_config"` // JSON stored as JSONB; retry_limit/default_timeout_seconds live inside this object (core.WorkflowDef doc comment)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		jsonErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.PlannerType != "static" && req.PlannerType != "http" {
		jsonErr(w, http.StatusBadRequest, "planner_type must be 'static' or 'http'")
		return
	}
	if len(req.PlannerConfig) == 0 || string(req.PlannerConfig) == "null" {
		req.PlannerConfig = json.RawMessage(`{}`)
	}

	workflowID, err := s.store.CreateWorkflow(r.Context(), core.WorkflowDef{
		Name:          req.Name,
		PlannerType:   req.PlannerType,
		PlannerConfig: req.PlannerConfig,
	})
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "store workflow: "+err.Error())
		return
	}

	jsonResp(w, http.StatusCreated, map[string]string{"workflow_id": string(workflowID)})
}

// ── POST /workflows/:workflow_id/runs (Ledger TX0) ────────────────────────

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	workflowID := core.WorkflowID(r.PathValue("workflow_id"))

	var req struct {
		WorkflowInput json.RawMessage `json:"workflow_input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.WorkflowInput) == 0 || string(req.WorkflowInput) == "null" {
		req.WorkflowInput = json.RawMessage(`{}`)
	}

	if _, err := s.store.GetWorkflow(r.Context(), workflowID); err != nil {
		if isNotFoundErr(err) {
			jsonErr(w, http.StatusNotFound, "workflow not found")
		} else {
			jsonErr(w, http.StatusInternalServerError, "get workflow: "+err.Error())
		}
		return
	}

	runID, err := s.store.CreateRun(r.Context(), workflowID, req.WorkflowInput)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "create run: "+err.Error())
		return
	}

	s.startLoop(s.ctx, runID, workflowID, req.WorkflowInput)

	jsonResp(w, http.StatusAccepted, map[string]string{"run_id": string(runID)})
}

// ── GET /runs/:run_id ──────────────────────────────────────────────────────
//
// The presentation read. Session 6 redesign (whitepaper §14.1 column names):
// run status/created_at/updated_at; per-step status/seq/attempt_count/
// created_at/completed_at plus a "current_attempt" summary (the step's most
// recent attempt — reason/error populated only when that attempt is
// FAILED); dlq_reason (most recent dead_letter_queue row for the run) when
// and only when run.status == DLQ. The renamed `created_at` column
// (whitepaper §14.1: "renamed from decided_at") is surfaced under its new
// name; `decided_at` never appears on the wire.
//
// This bypasses core.StateStore.GetRun/RunSummary: RunSummary/StepSummary/
// AttemptSummary (internal/core/interfaces.go, frozen — out of this
// session's scope) do not carry seq, attempt_count, or any timestamp, all of
// which this endpoint's redesigned shape requires. See the Session 6 report
// §5/§6 for the full inconsistency note and the recommended fix (extend
// those types in a future session so this handler can use the store
// abstraction instead of raw SQL).
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")

	var status string
	var workflowInput json.RawMessage
	var createdAt, updatedAt time.Time
	err := s.db.QueryRowContext(r.Context(), `
		SELECT status, workflow_input, created_at, updated_at FROM runs WHERE run_id = $1
	`, runID).Scan(&status, &workflowInput, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		jsonErr(w, http.StatusNotFound, "run not found")
		return
	} else if err != nil {
		jsonErr(w, http.StatusInternalServerError, "get run: "+err.Error())
		return
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT step_id, step_name, seq, status, attempt_count, output, created_at, completed_at
		FROM steps WHERE run_id = $1 ORDER BY seq
	`, runID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "get steps: "+err.Error())
		return
	}

	type currentAttemptView struct {
		AttemptID string  `json:"attempt_id"`
		Status    string  `json:"status"`
		Reason    *string `json:"reason,omitempty"`
		Error     *string `json:"error,omitempty"`
	}
	type stepView struct {
		StepID         string              `json:"step_id"`
		StepName       string              `json:"step_name"`
		Seq            int                 `json:"seq"`
		Status         string              `json:"status"`
		AttemptCount   int                 `json:"attempt_count"`
		Output         json.RawMessage     `json:"output,omitempty"`
		CreatedAt      time.Time           `json:"created_at"`
		CompletedAt    *time.Time          `json:"completed_at,omitempty"`
		CurrentAttempt *currentAttemptView `json:"current_attempt,omitempty"`
	}

	steps := make([]stepView, 0)
	var stepIDs []string
	for rows.Next() {
		var sv stepView
		var output []byte
		var completedAt sql.NullTime
		if err := rows.Scan(&sv.StepID, &sv.StepName, &sv.Seq, &sv.Status, &sv.AttemptCount, &output, &sv.CreatedAt, &completedAt); err != nil {
			rows.Close()
			jsonErr(w, http.StatusInternalServerError, "scan step: "+err.Error())
			return
		}
		if output != nil {
			sv.Output = json.RawMessage(output)
		}
		if completedAt.Valid {
			sv.CompletedAt = &completedAt.Time
		}
		steps = append(steps, sv)
		stepIDs = append(stepIDs, sv.StepID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		jsonErr(w, http.StatusInternalServerError, "steps iteration: "+err.Error())
		return
	}
	rows.Close()

	// One extra query per step for its most recent attempt. N+1, acceptable
	// at MVP scale (a run's step count is small); not worth a JOIN/LATERAL
	// rewrite for this session's scope.
	for i, stepID := range stepIDs {
		var av currentAttemptView
		var reason, attErr sql.NullString
		err := s.db.QueryRowContext(r.Context(), `
			SELECT attempt_id::text, status, failure_reason, error
			FROM attempts WHERE step_id = $1
			ORDER BY created_at DESC LIMIT 1
		`, stepID).Scan(&av.AttemptID, &av.Status, &reason, &attErr)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "get current attempt: "+err.Error())
			return
		}
		if reason.Valid {
			av.Reason = &reason.String
		}
		if attErr.Valid {
			av.Error = &attErr.String
		}
		steps[i].CurrentAttempt = &av
	}

	resp := map[string]any{
		"run_id":         runID,
		"status":         status,
		"workflow_input": workflowInput,
		"created_at":     createdAt,
		"updated_at":     updatedAt,
		"steps":          steps,
	}

	if status == string(core.RunStatusDLQ) {
		var dlqReason string
		err := s.db.QueryRowContext(r.Context(), `
			SELECT reason FROM dead_letter_queue WHERE run_id = $1 ORDER BY created_at DESC LIMIT 1
		`, runID).Scan(&dlqReason)
		if err != nil && err != sql.ErrNoRows {
			jsonErr(w, http.StatusInternalServerError, "get dlq reason: "+err.Error())
			return
		}
		if err == nil {
			resp["dlq_reason"] = dlqReason
		}
	}

	jsonResp(w, http.StatusOK, resp)
}

// ── POST /tasks/complete ─────────────────────────────────────────────────
//
// Async worker reports success. Handler discipline (whitepaper §10.4):
//  1. Validate the report is well-formed (ids present).
//  2. Hand it to the async transport's DeliverCallback, which itself
//     validates against the live current_attempt_id (a store read) before
//     routing it to the waiting Dispatch goroutine — see
//     internal/transport/async.go's DeliverCallback doc comment for the two
//     independent no-op paths it already covers (unregistered step,
//     superseded attempt_id).
//  3. Return 200 unconditionally.
//
// This handler NEVER writes step state — Barrier 2 is always the loop's
// responsibility. step_id/attempt_id absent from the body ⇒ 400, zero
// effect (whitepaper §7.1: "an async callback missing valid step_id/
// attempt_id gets a 400 and has zero effect"). A syntactically valid but
// stale/unknown pair is NOT rejected here — the transport's own read-time
// check silently absorbs it as a no-op, so the response is 200 either way,
// matching "late, duplicate, or superseded reports are ACKed with 200 and
// have zero state effect" (whitepaper §10).
func (s *Server) handleTaskComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StepID    string          `json:"step_id"`
		AttemptID string          `json:"attempt_id"`
		Output    json.RawMessage `json:"output"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.StepID == "" || req.AttemptID == "" {
		jsonErr(w, http.StatusBadRequest, "step_id and attempt_id are required")
		return
	}

	s.async.DeliverCallback(r.Context(), core.StepID(req.StepID), core.AttemptID(req.AttemptID),
		core.Result{Status: core.ResultStatusDone, Output: req.Output})
	jsonResp(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── POST /tasks/fail ─────────────────────────────────────────────────────
//
// Same three-step discipline as /tasks/complete. retry_after_seconds is
// optional, accepted, and ignored — reserved for future LLM-aware rate
// limiting (whitepaper §13.2, Temporary Design Registry #5) so the
// worker-facing contract stays stable when that lands.
func (s *Server) handleTaskFail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StepID            string `json:"step_id"`
		AttemptID         string `json:"attempt_id"`
		Error             string `json:"error"`
		RetryAfterSeconds *int   `json:"retry_after_seconds"` // optional; accepted, ignored (reserved, whitepaper §13.2)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.StepID == "" || req.AttemptID == "" {
		jsonErr(w, http.StatusBadRequest, "step_id and attempt_id are required")
		return
	}

	s.async.DeliverCallback(r.Context(), core.StepID(req.StepID), core.AttemptID(req.AttemptID),
		core.Result{
			Status: core.ResultStatusFailed,
			Failure: &core.ResultFailure{
				Reason: core.FailureReasonWorkerReported,
				Error:  req.Error,
			},
		})
	jsonResp(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── GET /dlq ─────────────────────────────────────────────────────────────

func (s *Server) handleGetDLQ(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, run_id, step_id, reason, context, created_at
		FROM dead_letter_queue
		ORDER BY created_at DESC
	`)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "query dlq: "+err.Error())
		return
	}
	defer rows.Close()

	type entry struct {
		ID        int64           `json:"id"`
		RunID     string          `json:"run_id"`
		StepID    *string         `json:"step_id,omitempty"`
		Reason    string          `json:"reason"`
		Context   json.RawMessage `json:"context"`
		CreatedAt time.Time       `json:"created_at"`
	}

	entries := make([]entry, 0)
	for rows.Next() {
		var e entry
		var stepID sql.NullString
		var ctxJSON []byte
		if err := rows.Scan(&e.ID, &e.RunID, &stepID, &e.Reason, &ctxJSON, &e.CreatedAt); err != nil {
			jsonErr(w, http.StatusInternalServerError, "scan dlq: "+err.Error())
			return
		}
		if stepID.Valid {
			e.StepID = &stepID.String
		}
		if ctxJSON != nil {
			e.Context = json.RawMessage(ctxJSON)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		jsonErr(w, http.StatusInternalServerError, "dlq iteration: "+err.Error())
		return
	}

	jsonResp(w, http.StatusOK, map[string]any{"entries": entries})
}

// ── POST /dlq/:id/replay ─────────────────────────────────────────────────
//
// Branches on the DLQ entry (whitepaper §11):
//   - worker-side (StepID != nil, run=DLQ/last_step=DLQ) → ReplayWorkerSide
//     (Ledger TX5: attempt_count→0, step→RUNNING, run→RUNNING, a fresh
//     attempt, current_attempt_id set) → re-enter the loop.
//   - planner-side (StepID == nil, run=DLQ/last_step=done) → ReplayPlannerSide
//     (Ledger TX6: run→RUNNING) → re-enter the loop.
//
// Both branches re-enter through the SAME generic startLoop/Loop.Run entry
// point used for a brand-new run and for crash recovery — "recovery is not
// a special mode; it is the same loop entered from a DB-persisted mid-run
// state" (whitepaper §2) applies equally to an operator-triggered replay.
//
// For the planner-side branch this is exact: after TX6, LoadFrontier's
// PendingStep is nil (the last step is still DONE), so Loop.Run's
// steady-state loop asks the planner directly — precisely TX6's prescribed
// "re-ask the planner".
//
// For the worker-side branch this is a deliberate, flagged approximation.
// The whitepaper describes TX5 as being followed directly by "dispatch the
// worker" — but Loop.Run has no entry point that dispatches an existing
// PendingStep without first performing the one-time recovery check (claim
// the pending attempt as failed(orphaned), whitepaper §8.3), and adding one
// would mean touching internal/orchestrator/loop.go, which is out of this
// session's scope (internal/api/ + cmd/stateflow/main.go only). Re-entering
// generically means the fresh attempt TX5 just created — which was never
// actually dispatched to a worker — gets claimed as `orphaned` (this is
// actually the correct bucket for it under the timeout taxonomy's "stuck
// before dispatch" rule, whitepaper §6: no worker was ever contacted for
// that attempt id, so no duplicate invocation occurs) before a SECOND new
// attempt is created via TX4 and actually dispatched. Net effect: replay is
// safe (no double dispatch, no data loss, still bounded and convergent) but
// costs one extra unit of the just-reset retry budget — the operator
// effectively gets X-1, not X, fresh attempts after a replay. For a
// workflow configured with retry_limit=1 this means a worker-side replay
// never dispatches at all before re-DLQing. Flagged as a 🔴 open question in
// the Session 6 report; the clean fix (an exported Loop method that resumes
// a PendingStep by dispatching directly, skipping the orphan-claim) belongs
// to a session that owns internal/orchestrator/.
func (s *Server) handleDLQReplay(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid DLQ id")
		return
	}

	entry, err := s.store.GetDLQEntry(r.Context(), core.DLQEntryID(id))
	if err != nil {
		if isNotFoundErr(err) {
			jsonErr(w, http.StatusNotFound, "DLQ entry not found")
		} else {
			jsonErr(w, http.StatusInternalServerError, "get dlq entry: "+err.Error())
		}
		return
	}

	// The run row is never deleted, so workflow_id/workflow_input are always
	// available to reconstruct the Loop — reading them is not itself part of
	// any TX ledger entry (it's a plain read), so a direct query is fine.
	var workflowID string
	var workflowInput json.RawMessage
	err = s.db.QueryRowContext(r.Context(), `
		SELECT workflow_id, workflow_input FROM runs WHERE run_id = $1
	`, string(entry.RunID)).Scan(&workflowID, &workflowInput)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "get run info: "+err.Error())
		return
	}

	if entry.StepID != nil {
		// Worker-side: TX5.
		if _, _, err := s.store.ReplayWorkerSide(r.Context(), entry.RunID); err != nil {
			jsonErr(w, http.StatusInternalServerError, "replay worker-side: "+err.Error())
			return
		}
	} else {
		// Planner-side: TX6.
		if err := s.store.ReplayPlannerSide(r.Context(), entry.RunID); err != nil {
			jsonErr(w, http.StatusInternalServerError, "replay planner-side: "+err.Error())
			return
		}
	}

	s.startLoop(s.ctx, entry.RunID, core.WorkflowID(workflowID), workflowInput)

	jsonResp(w, http.StatusAccepted, map[string]string{"run_id": string(entry.RunID)})
}

// ── Helpers ───────────────────────────────────────────────────────────────

// isNotFoundErr reports whether err is a core.StateStore "not found" read
// error. core.StateStore (frozen, out of this session's scope) defines no
// sentinel error type for this — every Get*/not-found case is a plain
// fmt.Errorf("...: %q not found", id) (see GetWorkflow/GetDLQEntry in
// internal/store/postgres.go) — so this is a deliberate, flagged
// string-match workaround rather than a typed check. Flagged in the Session
// 6 report; the clean fix is a core.ErrNotFound sentinel, added in whichever
// session next touches internal/core/.
func isNotFoundErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

func jsonResp(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	jsonResp(w, code, map[string]string{"error": msg})
}
