// Package api implements the StateFlow HTTP API server.
// Authoritative: whitepaper §6.6 (endpoint list + callback bodies),
// DESIGN.md §6 (MVP HTTP API table), §5 (Async Transport ↔ API Server Wiring),
// CLAUDE.md "Async Dispatch — Barrier 2 Lives in the Loop, Not the Handler".
package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/transport"
)

// Server is the StateFlow HTTP API server.
//
// It holds a reference to the async transport so it can route async worker
// callbacks to the waiting Dispatch goroutine (DESIGN.md §5). It never writes
// step state itself — that is always the loop's responsibility (CLAUDE.md:
// "Barrier 2 lives in the loop, not the handler").
//
// startLoop is a factory provided by main.go. It builds the planner, creates
// the Loop, and starts go loop.Run(ctx). The server calls it when a new run
// is created and when a DLQ entry is replayed.
type Server struct {
	db        *sql.DB
	async     *transport.AsyncTransport
	ctx       context.Context // parent context for all loop goroutines
	startLoop func(ctx context.Context, runID core.RunID, workflowInput json.RawMessage, plannerType string, plannerConfig json.RawMessage)
}

// New returns a Server ready to serve HTTP.
func New(
	db *sql.DB,
	async *transport.AsyncTransport,
	ctx context.Context,
	startLoop func(context.Context, core.RunID, json.RawMessage, string, json.RawMessage),
) *Server {
	return &Server{db: db, async: async, ctx: ctx, startLoop: startLoop}
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

// ── POST /workflows ───────────────────────────────────────────────────────

func (s *Server) handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkflowID    string          `json:"workflow_id"`    // optional; auto-generated if absent
		Name          string          `json:"name"`
		PlannerType   string          `json:"planner_type"`
		PlannerConfig json.RawMessage `json:"planner_config"` // JSON stored as JSONB
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
	if req.WorkflowID == "" {
		req.WorkflowID = "wf-" + newUUID()
	}

	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO workflows (workflow_id, name, planner_type, planner_config)
		VALUES ($1, $2, $3, $4::jsonb)
	`, req.WorkflowID, req.Name, req.PlannerType, string(req.PlannerConfig)); err != nil {
		jsonErr(w, http.StatusInternalServerError, "store workflow: "+err.Error())
		return
	}

	jsonResp(w, http.StatusCreated, map[string]string{"workflow_id": req.WorkflowID})
}

// ── POST /workflows/:workflow_id/runs ────────────────────────────────────

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	workflowID := r.PathValue("workflow_id")

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

	// Look up workflow to get planner config.
	var plannerType string
	var plannerConfig json.RawMessage
	err := s.db.QueryRowContext(r.Context(), `
		SELECT planner_type, planner_config FROM workflows WHERE workflow_id = $1
	`, workflowID).Scan(&plannerType, &plannerConfig)
	if err == sql.ErrNoRows {
		jsonErr(w, http.StatusNotFound, "workflow not found")
		return
	} else if err != nil {
		jsonErr(w, http.StatusInternalServerError, "get workflow: "+err.Error())
		return
	}

	runID := "run-" + newUUID()
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO runs (run_id, workflow_id, status, workflow_input)
		VALUES ($1, $2, 'RUNNING', $3::jsonb)
	`, runID, workflowID, string(req.WorkflowInput)); err != nil {
		jsonErr(w, http.StatusInternalServerError, "create run: "+err.Error())
		return
	}

	s.startLoop(s.ctx, core.RunID(runID), req.WorkflowInput, plannerType, plannerConfig)

	jsonResp(w, http.StatusAccepted, map[string]string{"run_id": runID})
}

// ── GET /runs/:run_id ────────────────────────────────────────────────────
//
// This is the "presentation read" (DESIGN.md §9.4). It returns ALL steps with
// their current statuses — NOT LoadFrontier, which is the execution read.

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
		SELECT step_name, seq, status, output, decided_at, completed_at
		FROM steps WHERE run_id = $1 ORDER BY seq
	`, runID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "get steps: "+err.Error())
		return
	}
	defer rows.Close()

	type stepView struct {
		StepName    string          `json:"step_name"`
		Seq         int             `json:"seq"`
		Status      string          `json:"status"`
		Output      json.RawMessage `json:"output,omitempty"`
		DecidedAt   *time.Time      `json:"decided_at,omitempty"`
		CompletedAt *time.Time      `json:"completed_at,omitempty"`
	}

	steps := make([]stepView, 0)
	for rows.Next() {
		var sv stepView
		var output []byte
		var decidedAt, completedAt sql.NullTime
		if err := rows.Scan(&sv.StepName, &sv.Seq, &sv.Status, &output, &decidedAt, &completedAt); err != nil {
			jsonErr(w, http.StatusInternalServerError, "scan step: "+err.Error())
			return
		}
		if output != nil {
			sv.Output = json.RawMessage(output)
		}
		if decidedAt.Valid {
			sv.DecidedAt = &decidedAt.Time
		}
		if completedAt.Valid {
			sv.CompletedAt = &completedAt.Time
		}
		steps = append(steps, sv)
	}
	if err := rows.Err(); err != nil {
		jsonErr(w, http.StatusInternalServerError, "steps iteration: "+err.Error())
		return
	}

	jsonResp(w, http.StatusOK, map[string]any{
		"run_id":         runID,
		"status":         status,
		"workflow_input": workflowInput,
		"created_at":     createdAt,
		"updated_at":     updatedAt,
		"steps":          steps,
	})
}

// ── POST /tasks/complete ─────────────────────────────────────────────────
//
// Async worker reports success. Handler discipline (CLAUDE.md):
//  1. Validate attempt_id == current_attempt_id (reads DB).
//  2. Push result into the in-process channel (DeliverCallback).
//  3. Return 200.
//
// This handler NEVER writes step state. Barrier 2 is the loop's responsibility.
// A superseded (stale) attempt_id → 200 but no action (dedup guard for
// at-least-once delivery).

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

	if !s.validateAttempt(w, r, req.StepID, req.AttemptID) {
		return // response already written
	}

	s.async.DeliverCallback(
		core.StepID(req.StepID),
		core.AttemptID(req.AttemptID),
		core.Result{Status: "done", Output: req.Output},
	)
	jsonResp(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── POST /tasks/fail ─────────────────────────────────────────────────────
//
// Same three-step discipline as /tasks/complete.
// retry_after_seconds is accepted in the body but ignored (deferred §9.2).

func (s *Server) handleTaskFail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StepID             string `json:"step_id"`
		AttemptID          string `json:"attempt_id"`
		Error              string `json:"error"`
		RetryAfterSeconds  int    `json:"retry_after_seconds"` // accepted, ignored — deferred §9.2
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if !s.validateAttempt(w, r, req.StepID, req.AttemptID) {
		return
	}

	s.async.DeliverCallback(
		core.StepID(req.StepID),
		core.AttemptID(req.AttemptID),
		core.Result{Status: "failed", Error: req.Error},
	)
	jsonResp(w, http.StatusOK, map[string]bool{"ok": true})
}

// validateAttempt checks attempt_id == current_attempt_id.
// Returns true if the attempt is current and the handler should proceed.
// Returns false if the attempt is superseded or the step is not found;
// in that case the handler writes 200 and the caller should return immediately.
func (s *Server) validateAttempt(w http.ResponseWriter, r *http.Request, stepID, attemptID string) bool {
	var current sql.NullString
	err := s.db.QueryRowContext(r.Context(), `
		SELECT current_attempt_id::text FROM steps WHERE step_id = $1
	`, stepID).Scan(&current)

	if err == sql.ErrNoRows {
		// Step not found — safe to acknowledge (at-least-once delivery).
		slog.Info("callback: step not found, acknowledging safely", "step_id", stepID)
		jsonResp(w, http.StatusOK, map[string]bool{"ok": true})
		return false
	}
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "validate attempt: "+err.Error())
		return false
	}

	if !current.Valid || current.String != attemptID {
		// Superseded: the channel for this attempt no longer exists.
		// Acknowledge with 200 so the worker stops retrying.
		slog.Info("callback: superseded attempt_id, ignoring",
			"step_id", stepID, "attempt_id", attemptID)
		jsonResp(w, http.StatusOK, map[string]bool{"ok": true})
		return false
	}

	return true
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
		var ctx []byte
		if err := rows.Scan(&e.ID, &e.RunID, &stepID, &e.Reason, &ctx, &e.CreatedAt); err != nil {
			jsonErr(w, http.StatusInternalServerError, "scan dlq: "+err.Error())
			return
		}
		if stepID.Valid {
			e.StepID = &stepID.String
		}
		if ctx != nil {
			e.Context = json.RawMessage(ctx)
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
// Re-queues a DLQ entry. For retry_exhausted: resets the step to DECIDED and
// the run to RUNNING, then re-enters the driver loop (which will re-dispatch
// the DECIDED step without re-asking the planner).
// For planner_failed (step_id IS NULL): resets the run to RUNNING only.
// The DLQ entry is preserved as an audit record.

func (s *Server) handleDLQReplay(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	dlqID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid DLQ id")
		return
	}

	// Get DLQ entry.
	var runID string
	var stepID sql.NullString
	err = s.db.QueryRowContext(r.Context(), `
		SELECT run_id, step_id FROM dead_letter_queue WHERE id = $1
	`, dlqID).Scan(&runID, &stepID)
	if err == sql.ErrNoRows {
		jsonErr(w, http.StatusNotFound, "DLQ entry not found")
		return
	} else if err != nil {
		jsonErr(w, http.StatusInternalServerError, "get dlq entry: "+err.Error())
		return
	}

	// Get run + workflow info for restarting the loop.
	var workflowInput json.RawMessage
	var plannerType string
	var plannerConfig json.RawMessage
	err = s.db.QueryRowContext(r.Context(), `
		SELECT r.workflow_input, w.planner_type, w.planner_config
		FROM runs r JOIN workflows w ON r.workflow_id = w.workflow_id
		WHERE r.run_id = $1
	`, runID).Scan(&workflowInput, &plannerType, &plannerConfig)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "get run info: "+err.Error())
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "begin tx: "+err.Error())
		return
	}
	defer tx.Rollback() //nolint:errcheck

	// If there's a step (retry_exhausted), reset it to DECIDED so the loop
	// can re-dispatch it on recovery without re-asking the planner.
	if stepID.Valid {
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE steps
			SET status = 'DECIDED', current_attempt_id = NULL, output = NULL, completed_at = NULL
			WHERE step_id = $1
		`, stepID.String); err != nil {
			jsonErr(w, http.StatusInternalServerError, "reset step: "+err.Error())
			return
		}
	}

	// Reset run to RUNNING.
	if _, err := tx.ExecContext(r.Context(), `
		UPDATE runs SET status = 'RUNNING', updated_at = now() WHERE run_id = $1
	`, runID); err != nil {
		jsonErr(w, http.StatusInternalServerError, "reset run: "+err.Error())
		return
	}

	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, "commit replay: "+err.Error())
		return
	}

	// Re-enter the driver loop. The loop's first action (PendingDecision) will
	// find the DECIDED step (if any) and re-dispatch it without re-asking the planner.
	s.startLoop(s.ctx, core.RunID(runID), workflowInput, plannerType, plannerConfig)

	jsonResp(w, http.StatusAccepted, map[string]string{"run_id": runID})
}

// ── Helpers ───────────────────────────────────────────────────────────────

func jsonResp(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	jsonResp(w, code, map[string]string{"error": msg})
}

// newUUID generates a random UUID v4 string.
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("newUUID: crypto/rand failed: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := fmt.Sprintf("%x", b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
