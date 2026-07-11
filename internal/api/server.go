// Package api implements the StateFlow HTTP API server.
// Authoritative: docs/StateFlow_Whitepaper_v1_0.md §11 (DLQ and replay), §13
// (worker contract, dispatch/reporting formats), §19 (Atomic Transaction
// Ledger — TX-W, TX0, TX5, TX6 are driven from this package).
package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/orchestrator"
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
// The server calls it when a new run is created and for the planner-side
// (TX6) branch of a DLQ replay, where re-asking the planner via the generic
// entry point is exactly correct (see handleDLQReplay's doc comment). The
// worker-side (TX5) branch does NOT use startLoop — see replayTransport and
// handleDLQReplay's doc comment for why (Session 6.5).
type Server struct {
	db        *sql.DB
	store     core.StateStore
	async     *transport.AsyncTransport
	ctx       context.Context // parent context for all loop goroutines
	startLoop func(ctx context.Context, runID core.RunID, workflowID core.WorkflowID, workflowInput json.RawMessage)

	// replayTransport is a sync+async routed core.WorkerTransport, built
	// once here from the same `async` instance the server already receives
	// (so the async callback registry stays the single one the rest of the
	// system uses) plus a fresh transport.NewSyncTransport(). It exists
	// solely so handleDLQReplay's worker-side branch can construct an
	// orchestrator.Loop directly and call Loop.ResumeReplayedStep (Session
	// 6.5) without widening New()'s exported signature or touching
	// cmd/stateflow/main.go — both out of this fix session's scope. This
	// mirrors exactly what main.go's own buildLoop and the test suite's
	// newTestServer already construct for the SAME async instance; no new
	// transport concept is introduced.
	replayTransport core.WorkerTransport
}

// New returns a Server ready to serve HTTP.
func New(
	db *sql.DB,
	store core.StateStore,
	async *transport.AsyncTransport,
	ctx context.Context,
	startLoop func(context.Context, core.RunID, core.WorkflowID, json.RawMessage),
) *Server {
	return &Server{
		db:              db,
		store:           store,
		async:           async,
		ctx:             ctx,
		startLoop:       startLoop,
		replayTransport: &transport.MultiTransport{Sync: transport.NewSyncTransport(), Async: async},
	}
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
//  2. Classify the reported output (isAsyncOutputMalformed below) — this is
//     async's analogue of sync's extractOutput/OutputField gate
//     (internal/transport/sync.go): a "success" callback whose output is
//     absent or JSON null cannot feed the next step, so it is reclassified
//     as a failed(malformed) Result before it ever reaches the store
//     (whitepaper §4.2, §7.1's "valid ids with unparseable output →
//     malformed failure").
//  3. Hand the (possibly reclassified) Result to the async transport's
//     DeliverCallback, which itself validates against the live
//     current_attempt_id (a store read) before routing it to the waiting
//     Dispatch goroutine — see internal/transport/async.go's DeliverCallback
//     doc comment for the two independent no-op paths it already covers
//     (unregistered step, superseded attempt_id).
//  4. Return 200 unconditionally — including for a malformed report: bad
//     *content* on an otherwise well-formed callback is a normal, expected
//     failure outcome for the attempt (routed to TX3 like any other
//     failure), not an HTTP-level transport error, so the caller must not
//     retry-storm on a 4xx/5xx for something that isn't their transport's
//     fault.
//
// This handler NEVER writes step state — Barrier 2 (TX2) or the failure
// checkpoint (TX3) is always the loop's responsibility; this handler only
// decides which of the two Results to hand off. step_id/attempt_id absent
// from the body ⇒ 400, zero effect (whitepaper §7.1: "an async callback
// missing valid step_id/attempt_id gets a 400 and has zero effect"). A
// syntactically valid but stale/unknown pair is NOT rejected here — the
// transport's own read-time check silently absorbs it as a no-op, so the
// response is 200 either way, matching "late, duplicate, or superseded
// reports are ACKed with 200 and have zero state effect" (whitepaper §10).
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

	result := core.Result{Status: core.ResultStatusDone, Output: req.Output}
	if isAsyncOutputMalformed(req.Output) {
		result = core.Result{
			Status: core.ResultStatusFailed,
			Failure: &core.ResultFailure{
				Reason: core.FailureReasonMalformed,
				Error:  "async callback: output absent or null",
			},
		}
	}

	s.async.DeliverCallback(r.Context(), core.StepID(req.StepID), core.AttemptID(req.AttemptID), result)
	jsonResp(w, http.StatusOK, map[string]bool{"ok": true})
}

// isAsyncOutputMalformed is the async equivalent of sync's extractOutput
// malformed gate. Async has no OutputField subtree concept (unlike sync,
// there is no declared-field-absent case to check), so the rule is
// content-based and narrower: a JSON body whose "output" key is absent
// entirely, or present but equal to the JSON literal null, is unparseable —
// the worker "succeeded" but left nothing a downstream step could consume.
//
// This is the minimum bar that closes the schema invariant
// migrations/001_initial.sql documents ("output non-null → step DONE"):
// without this check, jsonOrNull() (internal/store/postgres.go) would turn
// an absent/null async output into a stored JSON `null`, and TX2 would
// still mark the step DONE with it.
//
// Deliberately NOT malformed: a present, syntactically valid empty object
// ({}) or empty array ([]) — the whitepaper's "unparseable output" wording
// reads most naturally as a JSON-syntax/absence failure, and {}/[] are
// neither; a worker may legitimately intend either as "no fields to
// report". Left as an open question for the project owner rather than
// silently guessed (see this session's report).
func isAsyncOutputMalformed(output json.RawMessage) bool {
	if len(output) == 0 {
		return true // key absent from the JSON body
	}
	return string(bytes.TrimSpace(output)) == "null"
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
//     attempt, current_attempt_id set) → dispatch that fresh attempt
//     directly via orchestrator.Loop.ResumeReplayedStep.
//   - planner-side (StepID == nil, run=DLQ/last_step=done) → ReplayPlannerSide
//     (Ledger TX6: run→RUNNING) → re-enter the loop via the generic
//     startLoop/Loop.Run entry point.
//
// The planner-side branch re-entering through startLoop/Loop.Run is exact:
// after TX6, LoadFrontier's PendingStep is nil (the last step is still
// DONE), so Loop.Run's steady-state loop asks the planner directly —
// precisely TX6's prescribed "re-ask the planner". "Recovery is not a
// special mode; it is the same loop entered from a DB-persisted mid-run
// state" (whitepaper §2) applies cleanly here.
//
// The worker-side branch does NOT use startLoop/Loop.Run (Session 6.5 fix).
// Session 6 originally routed it through the same generic entry point, but
// Loop.Run's one-time crash-recovery check unconditionally treats any
// PendingStep it finds as a possibly-orphaned attempt whose fate is
// unknown — correct after a real orchestrator crash, wrong here: TX5 just
// created a brand-new attempt that was never dispatched to any worker, so
// there is nothing "possibly orphaned" about it. Routing it through Run()
// burned one unit of the retry budget TX5 had just reset
// (RecordFailure(orphaned)) before a worker was ever contacted — for
// retry_limit=1 this re-DLQ'd the run without ever dispatching, reproducing
// exactly the failure whitepaper §11 says the TX5 reset exists to prevent
// ("without it... the button would be decorative"), by a different
// mechanism. Fixed by calling orchestrator.Loop.ResumeReplayedStep, a
// dedicated entry point (internal/orchestrator/replay.go) that dispatches
// the TX5-created attempt directly — matching the whitepaper's literal §11
// phrasing, "TX5: ... → dispatch the worker" — and only then falls into the
// same steady-state loop Run() uses for the rest of the run.
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
		// Worker-side: TX5, then dispatch the freshly-created attempt
		// directly (Session 6.5) — NOT the generic startLoop/Loop.Run entry
		// point; see this handler's doc comment for why.
		if _, _, err := s.store.ReplayWorkerSide(r.Context(), entry.RunID); err != nil {
			jsonErr(w, http.StatusInternalServerError, "replay worker-side: "+err.Error())
			return
		}

		l := &orchestrator.Loop{
			RunID:         entry.RunID,
			WorkflowID:    core.WorkflowID(workflowID),
			WorkflowInput: workflowInput,
			Store:         s.store,
			Transport:     s.replayTransport,
			Retry:         orchestrator.DefaultRetryPolicy(),
		}
		go func() {
			slog.Info("[REPLAY] resuming worker-side-replayed run", "run_id", string(entry.RunID))
			if err := l.ResumeReplayedStep(s.ctx); err != nil {
				slog.Error("[REPLAY] worker-side replay ended with error", "run_id", string(entry.RunID), "err", err)
			} else {
				slog.Info("[REPLAY] worker-side replay completed", "run_id", string(entry.RunID))
			}
		}()
	} else {
		// Planner-side: TX6, then re-enter the SAME generic loop entry point
		// used for a brand-new run — exactly correct here (see doc comment).
		if err := s.store.ReplayPlannerSide(r.Context(), entry.RunID); err != nil {
			jsonErr(w, http.StatusInternalServerError, "replay planner-side: "+err.Error())
			return
		}
		s.startLoop(s.ctx, entry.RunID, core.WorkflowID(workflowID), workflowInput)
	}

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
