package api_test

// Integration tests for the HTTP API server.
// Require a real Postgres DB (TEST_DATABASE_URL must be set).
// Each test uses httptest.Server so no ports are actually opened.
//
// Authoritative: docs/StateFlow_Whitepaper_v1_0.md §11 (DLQ/replay), §12.2
// (wire-casing rule), §14.1 (schema).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/aaronwu000/stateflow/internal/api"
	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/orchestrator"
	"github.com/aaronwu000/stateflow/internal/store"
	"github.com/aaronwu000/stateflow/internal/transport"
)

// ── DB helpers (mirrors orchestrator/store integration tests) ─────────────

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("db.Ping: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func resetSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, tbl := range []string{"dead_letter_queue", "attempts", "steps", "runs", "workflows"} {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + tbl + " CASCADE"); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
	ddl, err := os.ReadFile("../../migrations/001_initial.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
}

// ── API server factory ────────────────────────────────────────────────────

// newTestServer creates an API server backed by db and a real sync+async
// transport. Returns the httptest.Server and a cancel function for the loop
// context. startLoop's signature matches Session 6's redesigned
// api.New/orchestrator.Loop shape: (ctx, runID, workflowID, workflowInput) —
// the planner is reconstructed by Loop.Run itself from the workflow row, so
// the factory needs no plannerType/plannerConfig parameters at all.
func newTestServer(t *testing.T, db *sql.DB) (*httptest.Server, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	s := store.New(db)
	syncT := transport.NewSyncTransport()
	asyncT := transport.NewAsyncTransport(s)
	routedT := &transport.MultiTransport{Sync: syncT, Async: asyncT}

	startLoop := func(loopCtx context.Context, runID core.RunID, workflowID core.WorkflowID, workflowInput json.RawMessage) {
		l := &orchestrator.Loop{
			RunID:         runID,
			WorkflowID:    workflowID,
			WorkflowInput: workflowInput,
			Store:         s,
			Transport:     routedT,
			Retry:         &orchestrator.FixedCountPolicy{MaxRetries: 3, Delay: 0},
		}
		go l.Run(loopCtx)
	}

	srv := api.New(db, s, asyncT, ctx, startLoop)
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		cancel()
		httpSrv.Close()
	})
	return httpSrv, cancel
}

// ── HTTP helpers ──────────────────────────────────────────────────────────

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return m
}

func pollRunStatus(t *testing.T, base, runID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp := get(t, base+"/runs/"+runID)
		body := decodeJSON(t, resp)
		if status, ok := body["status"].(string); ok && status != "RUNNING" {
			return status
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("run %s still RUNNING after %v", runID, timeout)
	return ""
}

// ── Test 1: End-to-end with sync transport ────────────────────────────────

// TestAPI_EndToEnd_Sync verifies the full happy path:
//
//	POST /workflows → POST /workflows/:id/runs → poll GET /runs/:id → DONE
//
// Uses a static planner (1 step, sync mode) and a fake httptest worker.
func TestAPI_EndToEnd_Sync(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)

	// Fake sync worker: returns 200 with JSON output.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"processed":true}`)
	}))
	defer worker.Close()

	apiSrv, _ := newTestServer(t, db)
	base := apiSrv.URL

	// Planner config: 1 step, sync mode, pointing to fake worker.
	plannerConfig := fmt.Sprintf(
		`{"steps":[{"name":"process","worker_url":%q,"mode":"sync","timeout_seconds":5}]}`,
		worker.URL,
	)

	// POST /workflows
	wfBody := fmt.Sprintf(`{"name":"test-wf","planner_type":"static","planner_config":%s}`, plannerConfig)
	resp := post(t, base+"/workflows", wfBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /workflows: status %d, want 201", resp.StatusCode)
	}
	wfData := decodeJSON(t, resp)
	workflowID := wfData["workflow_id"].(string)
	if workflowID == "" {
		t.Fatal("POST /workflows: empty workflow_id")
	}
	t.Logf("PASS — POST /workflows → workflow_id=%s", workflowID)

	// POST /workflows/:id/runs
	resp = post(t, base+"/workflows/"+workflowID+"/runs", `{"workflow_input":{"doc":"test.pdf"}}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /runs: status %d, want 202", resp.StatusCode)
	}
	runData := decodeJSON(t, resp)
	runID := runData["run_id"].(string)
	if runID == "" {
		t.Fatal("POST /runs: empty run_id")
	}
	t.Logf("PASS — POST /runs → run_id=%s", runID)

	// Poll GET /runs/:id until terminal
	finalStatus := pollRunStatus(t, base, runID, 10*time.Second)
	if finalStatus != "DONE" {
		t.Errorf("run.status = %q, want DONE", finalStatus)
	}
	t.Logf("PASS — run %s → %s", runID, finalStatus)

	// Verify steps via GET /runs/:id
	resp = get(t, base+"/runs/"+runID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /runs: status %d", resp.StatusCode)
	}
	runView := decodeJSON(t, resp)
	steps := runView["steps"].([]any)
	if len(steps) != 1 {
		t.Errorf("steps count = %d, want 1", len(steps))
	} else {
		s0 := steps[0].(map[string]any)
		if s0["status"] != "DONE" {
			t.Errorf("step status = %q, want DONE", s0["status"])
		}
		if s0["seq"].(float64) != 1 {
			t.Errorf("step seq = %v, want 1", s0["seq"])
		}
		if _, ok := s0["created_at"]; !ok {
			t.Error("step missing created_at (renamed from decided_at — must be present)")
		}
		if _, ok := s0["decided_at"]; ok {
			t.Error("step has decided_at — retired name must never appear on the wire")
		}
		t.Logf("PASS — steps[0].status=%s seq=%v attempt_count=%v", s0["status"], s0["seq"], s0["attempt_count"])
	}
}

// ── Test 2: Callback dedup — superseded attempt_id ────────────────────────

// TestAPI_Callback_Dedup verifies that POST /tasks/complete with a superseded
// attempt_id returns 200 but leaves the step state unchanged.
//
// This tests the at-least-once delivery dedup guard (whitepaper §10).
func TestAPI_Callback_Dedup(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)

	apiSrv, _ := newTestServer(t, db)
	base := apiSrv.URL

	// Plant a workflow + run + step directly (no loop running).
	const wfID = "wf-dedup"
	const runID = "run-dedup"
	const realAttempt = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const wrongAttempt = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

	if _, err := db.Exec(`
		INSERT INTO workflows (workflow_id, name, planner_type, planner_config)
		VALUES ($1, 'dedup-wf', 'static', '{}')
	`, wfID); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO runs (run_id, workflow_id, status, workflow_input)
		VALUES ($1, $2, 'RUNNING', '{}')
	`, runID, wfID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	stepID := runID + ":step1"
	decision := `{"name":"step1","worker_url":"http://stub","mode":"async","timeout_seconds":5}`
	if _, err := db.Exec(`
		INSERT INTO steps (step_id, run_id, step_name, seq, status, attempt_count, decision, current_attempt_id)
		VALUES ($1, $2, 'step1', 1, 'RUNNING', 0, $3::jsonb, $4::uuid)
	`, stepID, runID, decision, realAttempt); err != nil {
		t.Fatalf("seed step: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO attempts (attempt_id, step_id, status)
		VALUES ($1::uuid, $2, 'RUNNING')
	`, realAttempt, stepID); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}

	// POST /tasks/complete with the WRONG (superseded) attempt_id.
	body := fmt.Sprintf(`{"step_id":%q,"attempt_id":%q,"output":{"hijacked":true}}`, stepID, wrongAttempt)
	resp := post(t, base+"/tasks/complete", body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /tasks/complete: status %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	t.Log("PASS — superseded attempt_id returned 200")

	// Assert: step is still RUNNING with the original current_attempt_id.
	var stepStatus, currentAttempt string
	if err := db.QueryRow(`
		SELECT status, current_attempt_id::text FROM steps WHERE step_id = $1
	`, stepID).Scan(&stepStatus, &currentAttempt); err != nil {
		t.Fatalf("query step: %v", err)
	}
	if stepStatus != "RUNNING" {
		t.Errorf("step.status = %q, want RUNNING (superseded callback must not change state)", stepStatus)
	}
	if currentAttempt != realAttempt {
		t.Errorf("step.current_attempt_id = %q, want %q", currentAttempt, realAttempt)
	}
	t.Logf("PASS — step.status = RUNNING, current_attempt_id unchanged (%s)", currentAttempt)

	// GET /runs/:id should show run still RUNNING.
	resp = get(t, base+"/runs/"+runID)
	runView := decodeJSON(t, resp)
	if runView["status"] != "RUNNING" {
		t.Errorf("run.status = %q, want RUNNING", runView["status"])
	}
	t.Log("PASS — run.status = RUNNING (superseded callback left run unchanged)")

	// Missing ids ⇒ 400, zero effect (whitepaper §7.1).
	resp = post(t, base+"/tasks/complete", `{"step_id":"","attempt_id":"","output":{}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /tasks/complete with empty ids: status %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	t.Log("PASS — empty step_id/attempt_id → 400")
}

// ── Test 3: GET /runs/:id — unknown run returns 404 ──────────────────────

func TestAPI_GetRun_NotFound(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)

	apiSrv, _ := newTestServer(t, db)
	resp := get(t, apiSrv.URL+"/runs/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	t.Log("PASS — GET /runs/unknown → 404")
}

// ── Test 4: End-to-end with HTTP planner + the wire-casing contract ───────

// TestAPI_EndToEnd_HTTPPlanner verifies the full happy path with an HTTP
// planner AND pins the wire-casing contract explicitly (Session 6):
//
//   - Orchestrator → Planner: every history-entry status is UPPERCASE
//     ("DONE"), identical to the stored value (whitepaper §12.2).
//   - Planner → Orchestrator: the planner's OWN decision status is
//     lowercase ("continue"/"done"/"fail", whitepaper §12.3) — a distinct
//     field with a distinct casing rule. The sub-test proves the
//     orchestrator actually enforces (not just documents) this by sending
//     the wrong case and asserting rejection as planner_malformed.
func TestAPI_EndToEnd_HTTPPlanner(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)

	// Fake sync worker: always succeeds.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"result":"ok"}`)
	}))
	defer worker.Close()

	// Fake HTTP planner: stateless logic based on RunState.History length.
	//   call 1 (history empty) → continue, dispatch step1 to worker
	//   call 2 (step1 done)    → done; this is where we capture the
	//                            wire-casing of the incoming history.
	var secondCallSeen bool
	var capturedHistoryStatus string
	fakePlanner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var state core.RunState
		if err := json.NewDecoder(r.Body).Decode(&state); err != nil {
			t.Errorf("fake planner: decode RunState: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if len(state.History) == 0 {
			fmt.Fprintf(w, `{"status":"continue","step":{"name":"step1","worker_url":%q,"mode":"sync","timeout_seconds":5}}`, worker.URL)
			return
		}
		secondCallSeen = true
		capturedHistoryStatus = string(state.History[0].Status)
		fmt.Fprint(w, `{"status":"done"}`)
	}))
	defer fakePlanner.Close()

	apiSrv, _ := newTestServer(t, db)
	base := apiSrv.URL

	// POST /workflows with planner_type="http".
	plannerConfig := fmt.Sprintf(`{"url":%q}`, fakePlanner.URL)
	wfBody := fmt.Sprintf(`{"name":"http-planner-wf","planner_type":"http","planner_config":%s}`, plannerConfig)
	resp := post(t, base+"/workflows", wfBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /workflows: status %d, want 201", resp.StatusCode)
	}
	wfData := decodeJSON(t, resp)
	workflowID := wfData["workflow_id"].(string)
	t.Logf("PASS — POST /workflows → workflow_id=%s", workflowID)

	// POST /workflows/:id/runs.
	resp = post(t, base+"/workflows/"+workflowID+"/runs", `{"workflow_input":{"doc":"report.pdf"}}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /runs: status %d, want 202", resp.StatusCode)
	}
	runData := decodeJSON(t, resp)
	runID := runData["run_id"].(string)
	t.Logf("PASS — POST /runs → run_id=%s", runID)

	// Poll until the run reaches a terminal state.
	finalStatus := pollRunStatus(t, base, runID, 10*time.Second)
	if finalStatus != "DONE" {
		t.Errorf("run.status = %q, want DONE", finalStatus)
	}
	t.Logf("PASS — run %s → %s via HTTPPlanner", runID, finalStatus)

	if !secondCallSeen {
		t.Fatal("fake planner never received a second call with non-empty history — wire-casing assertion could not run")
	}
	if capturedHistoryStatus != string(core.StepStatusDone) {
		t.Errorf("history[0].status on the wire = %q, want %q (UPPERCASE per whitepaper §12.2)",
			capturedHistoryStatus, core.StepStatusDone)
	} else {
		t.Logf("PASS — RunState.history[0].status = %q (UPPERCASE, as sent by the orchestrator)", capturedHistoryStatus)
	}

	// Verify that step1 completed successfully.
	resp = get(t, base+"/runs/"+runID)
	runView := decodeJSON(t, resp)
	steps := runView["steps"].([]any)
	if len(steps) != 1 {
		t.Errorf("steps count = %d, want 1", len(steps))
	} else {
		s0 := steps[0].(map[string]any)
		if s0["status"] != "DONE" {
			t.Errorf("step status = %q, want DONE", s0["status"])
		}
		if s0["step_name"] != "step1" {
			t.Errorf("step step_name = %q, want step1", s0["step_name"])
		}
		t.Logf("PASS — steps[0].step_name=%s status=%s", s0["step_name"], s0["status"])
	}

	// The other half of the wire-casing contract: the planner's own verdict
	// casing is enforced, not merely a convention. An UPPERCASE verdict must
	// be rejected as malformed, not silently accepted.
	t.Run("planner_verdict_wrong_case_is_rejected", func(t *testing.T) {
		badPlanner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":"DONE"}`) // wrong case — whitepaper §12.3 requires lowercase
		}))
		defer badPlanner.Close()

		wfBody := fmt.Sprintf(`{"name":"bad-case-wf","planner_type":"http","planner_config":{"url":%q}}`, badPlanner.URL)
		resp := post(t, base+"/workflows", wfBody)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("POST /workflows: status %d, want 201", resp.StatusCode)
		}
		wfData := decodeJSON(t, resp)
		wfID := wfData["workflow_id"].(string)

		resp = post(t, base+"/workflows/"+wfID+"/runs", `{"workflow_input":{}}`)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("POST /runs: status %d, want 202", resp.StatusCode)
		}
		runData := decodeJSON(t, resp)
		badRunID := runData["run_id"].(string)

		finalStatus := pollRunStatus(t, base, badRunID, 15*time.Second)
		if finalStatus != "DLQ" {
			t.Fatalf("run.status = %q, want DLQ (uppercase planner verdict must be rejected as malformed)", finalStatus)
		}
		resp = get(t, base+"/runs/"+badRunID)
		runView := decodeJSON(t, resp)
		if runView["dlq_reason"] != "planner_malformed" {
			t.Errorf("dlq_reason = %v, want planner_malformed", runView["dlq_reason"])
		}
		t.Logf(`PASS — planner verdict "DONE" (wrong case) rejected → run DLQ'd, dlq_reason=%v`, runView["dlq_reason"])
	})
}

// ── Test 5: DLQ replay — worker-side (Ledger TX5) ─────────────────────────

// TestAPI_DLQ_ReplayWorkerSide plants a run with one DONE step and one
// worker-side DLQ'd step (attempt_count already at its retry_limit),
// verifies GET /runs surfaces dlq_reason and GET /dlq lists the entry, then
// replays it and asserts:
//   - the DONE step is never re-dispatched (its worker call count stays 0);
//   - attempt_count was genuinely reset (retry_limit is set equal to the
//     pre-replay count, so the run can only reach DONE if TX5 reset the
//     count back to 0 before the loop's post-replay orphan-claim + retry —
//     if the reset did not happen, the very first orphan-claim after replay
//     would immediately exceed retry_limit and re-DLQ the run instead).
func TestAPI_DLQ_ReplayWorkerSide(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)

	var extractCalls, processCalls int32

	extractWorker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&extractCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"extracted":true}`)
	}))
	defer extractWorker.Close()

	processWorker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&processCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"processed":true}`)
	}))
	defer processWorker.Close()

	apiSrv, _ := newTestServer(t, db)
	base := apiSrv.URL

	const wfID = "wf-dlq-worker"
	const runID = "run-dlq-worker"

	// retry_limit=2, matching the two FAILED attempts seeded below exactly —
	// see the test's doc comment for why this proves the reset happened.
	plannerConfig := fmt.Sprintf(
		`{"steps":[{"name":"extract","worker_url":%q,"mode":"sync","timeout_seconds":5},{"name":"process","worker_url":%q,"mode":"sync","timeout_seconds":5}],"retry_limit":2}`,
		extractWorker.URL, processWorker.URL,
	)
	if _, err := db.Exec(`
		INSERT INTO workflows (workflow_id, name, planner_type, planner_config)
		VALUES ($1, 'dlq-worker-wf', 'static', $2::jsonb)
	`, wfID, plannerConfig); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO runs (run_id, workflow_id, status, workflow_input)
		VALUES ($1, $2, 'DLQ', '{}')
	`, runID, wfID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// Step 1 "extract": already DONE — must NOT be re-dispatched by replay.
	extractStepID := runID + ":extract"
	extractDecision := fmt.Sprintf(`{"name":"extract","worker_url":%q,"mode":"sync","timeout_seconds":5}`, extractWorker.URL)
	if _, err := db.Exec(`
		INSERT INTO steps (step_id, run_id, step_name, seq, status, attempt_count, decision, output, completed_at)
		VALUES ($1, $2, 'extract', 1, 'DONE', 1, $3::jsonb, '{"extracted":true}'::jsonb, now())
	`, extractStepID, runID, extractDecision); err != nil {
		t.Fatalf("seed extract step: %v", err)
	}
	const extractAttempt = "aaaaaaaa-0000-4000-8000-000000000001"
	if _, err := db.Exec(`
		INSERT INTO attempts (attempt_id, step_id, status, resolved_at)
		VALUES ($1::uuid, $2, 'DONE', now())
	`, extractAttempt, extractStepID); err != nil {
		t.Fatalf("seed extract attempt: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE steps SET current_attempt_id = $1::uuid WHERE step_id = $2
	`, extractAttempt, extractStepID); err != nil {
		t.Fatalf("set extract current_attempt_id: %v", err)
	}

	// Step 2 "process": DLQ'd — attempt_count already equals retry_limit (2).
	processStepID := runID + ":process"
	processDecision := fmt.Sprintf(`{"name":"process","worker_url":%q,"mode":"sync","timeout_seconds":5}`, processWorker.URL)
	if _, err := db.Exec(`
		INSERT INTO steps (step_id, run_id, step_name, seq, status, attempt_count, decision)
		VALUES ($1, $2, 'process', 2, 'DLQ', 2, $3::jsonb)
	`, processStepID, runID, processDecision); err != nil {
		t.Fatalf("seed process step: %v", err)
	}
	const failedAttempt1 = "bbbbbbbb-0000-4000-8000-000000000001"
	const failedAttempt2 = "bbbbbbbb-0000-4000-8000-000000000002"
	if _, err := db.Exec(`
		INSERT INTO attempts (attempt_id, step_id, status, failure_reason, error, resolved_at)
		VALUES ($1::uuid, $2, 'FAILED', 'worker_reported', 'boom', now())
	`, failedAttempt1, processStepID); err != nil {
		t.Fatalf("seed process attempt 1: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO attempts (attempt_id, step_id, status, failure_reason, error, resolved_at)
		VALUES ($1::uuid, $2, 'FAILED', 'worker_reported', 'boom again', now())
	`, failedAttempt2, processStepID); err != nil {
		t.Fatalf("seed process attempt 2: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE steps SET current_attempt_id = $1::uuid WHERE step_id = $2
	`, failedAttempt2, processStepID); err != nil {
		t.Fatalf("set process current_attempt_id: %v", err)
	}

	var dlqID int64
	if err := db.QueryRow(`
		INSERT INTO dead_letter_queue (run_id, step_id, reason, context)
		VALUES ($1, $2, 'worker_retry_exhausted',
			'{"attempts":[{"reason":"worker_reported","error":"boom"},{"reason":"worker_reported","error":"boom again"}]}')
		RETURNING id
	`, runID, processStepID).Scan(&dlqID); err != nil {
		t.Fatalf("seed dlq entry: %v", err)
	}

	// GET /runs shows dlq_reason while DLQ'd (Session 6 response-shape requirement).
	resp := get(t, base+"/runs/"+runID)
	runView := decodeJSON(t, resp)
	if runView["dlq_reason"] != "worker_retry_exhausted" {
		t.Errorf("dlq_reason = %v, want worker_retry_exhausted", runView["dlq_reason"])
	} else {
		t.Logf("PASS — GET /runs/%s.dlq_reason = %v (run still DLQ)", runID, runView["dlq_reason"])
	}

	// GET /dlq lists it.
	resp = get(t, base+"/dlq")
	dlqBody := decodeJSON(t, resp)
	entries := dlqBody["entries"].([]any)
	found := false
	for _, e := range entries {
		em := e.(map[string]any)
		if int64(em["id"].(float64)) == dlqID {
			found = true
			if em["reason"] != "worker_retry_exhausted" {
				t.Errorf("dlq entry reason = %v, want worker_retry_exhausted", em["reason"])
			}
		}
	}
	if !found {
		t.Fatalf("GET /dlq: entry id=%d not found in %v", dlqID, entries)
	}
	t.Logf("PASS — GET /dlq lists entry id=%d", dlqID)

	// Replay.
	resp = post(t, fmt.Sprintf("%s/dlq/%d/replay", base, dlqID), ``)
	if resp.StatusCode != http.StatusAccepted {
		body := decodeJSON(t, resp)
		t.Fatalf("POST /dlq/replay: status %d, body=%v", resp.StatusCode, body)
	}
	replayData := decodeJSON(t, resp)
	if replayData["run_id"] != runID {
		t.Errorf("replay response run_id = %q, want %q", replayData["run_id"], runID)
	}
	t.Logf("PASS — POST /dlq/%d/replay → 202", dlqID)

	finalStatus := pollRunStatus(t, base, runID, 10*time.Second)
	if finalStatus != "DONE" {
		t.Fatalf("run.status after replay = %q, want DONE (would be DLQ again if TX5 failed to reset attempt_count)", finalStatus)
	}
	t.Logf("PASS — run %s → DONE after worker-side replay (proves attempt_count was reset)", runID)

	if calls := atomic.LoadInt32(&extractCalls); calls != 0 {
		t.Errorf("extract worker called %d times during replay; want 0 (a DONE step must never be re-dispatched)", calls)
	} else {
		t.Log("PASS — extract worker not re-invoked (done step not re-run)")
	}
	if calls := atomic.LoadInt32(&processCalls); calls == 0 {
		t.Error("process worker called 0 times; want >= 1 (replay must dispatch the DLQ'd step)")
	} else {
		t.Logf("PASS — process worker invoked %d time(s) after replay", calls)
	}

	var extractStatus, processStatus string
	if err := db.QueryRow(`SELECT status FROM steps WHERE step_id = $1`, extractStepID).Scan(&extractStatus); err != nil {
		t.Fatalf("query extract step: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM steps WHERE step_id = $1`, processStepID).Scan(&processStatus); err != nil {
		t.Fatalf("query process step: %v", err)
	}
	if extractStatus != "DONE" {
		t.Errorf("extract step status = %q, want DONE (untouched)", extractStatus)
	}
	if processStatus != "DONE" {
		t.Errorf("process step status = %q, want DONE", processStatus)
	}
	t.Logf("PASS — extract.status=%s (untouched), process.status=%s (replayed to completion)", extractStatus, processStatus)
}

// ── Test 6: DLQ replay — planner-side (Ledger TX6) ────────────────────────

// TestAPI_DLQ_ReplayPlannerSide plants a run in the DLQ with NO steps at all
// (step_id NULL — a planner-side entry, combination run=DLQ/last_step=done
// per the "no steps ⇒ treated as done" invariant, whitepaper §8.1) and a
// reason of planner_unreachable, then replays it and asserts the loop
// re-asks the planner (TX6's prescribed action) and drives the run to
// completion — proving the replay entered at "ask planner", not at
// "re-dispatch a step" (there is no step to re-dispatch).
func TestAPI_DLQ_ReplayPlannerSide(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)

	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"done":true}`)
	}))
	defer worker.Close()

	const wfID = "wf-dlq-planner"
	const runID = "run-dlq-planner"

	plannerConfig := fmt.Sprintf(`{"steps":[{"name":"only_step","worker_url":%q,"mode":"sync","timeout_seconds":5}]}`, worker.URL)
	if _, err := db.Exec(`
		INSERT INTO workflows (workflow_id, name, planner_type, planner_config)
		VALUES ($1, 'dlq-planner-wf', 'static', $2::jsonb)
	`, wfID, plannerConfig); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO runs (run_id, workflow_id, status, workflow_input)
		VALUES ($1, $2, 'DLQ', '{}')
	`, runID, wfID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	apiSrv, _ := newTestServer(t, db)
	base := apiSrv.URL

	var dlqID int64
	if err := db.QueryRow(`
		INSERT INTO dead_letter_queue (run_id, step_id, reason, context)
		VALUES ($1, NULL, 'planner_unreachable', '{"detail":"simulated planner outage"}')
		RETURNING id
	`, runID).Scan(&dlqID); err != nil {
		t.Fatalf("seed dlq entry: %v", err)
	}

	resp := get(t, base+"/runs/"+runID)
	runView := decodeJSON(t, resp)
	if runView["dlq_reason"] != "planner_unreachable" {
		t.Errorf("dlq_reason = %v, want planner_unreachable", runView["dlq_reason"])
	} else {
		t.Logf("PASS — GET /runs/%s.dlq_reason = %v (run still DLQ)", runID, runView["dlq_reason"])
	}

	resp = post(t, fmt.Sprintf("%s/dlq/%d/replay", base, dlqID), ``)
	if resp.StatusCode != http.StatusAccepted {
		body := decodeJSON(t, resp)
		t.Fatalf("POST /dlq/replay: status %d, body=%v", resp.StatusCode, body)
	}
	replayData := decodeJSON(t, resp)
	if replayData["run_id"] != runID {
		t.Errorf("replay response run_id = %q, want %q", replayData["run_id"], runID)
	}
	t.Logf("PASS — POST /dlq/%d/replay → 202", dlqID)

	finalStatus := pollRunStatus(t, base, runID, 10*time.Second)
	if finalStatus != "DONE" {
		t.Fatalf("run.status after replay = %q, want DONE", finalStatus)
	}
	t.Logf("PASS — run %s → DONE after planner-side replay (TX6 re-asked the planner)", runID)

	var doneSteps int
	if err := db.QueryRow(`SELECT count(*) FROM steps WHERE run_id = $1 AND status = 'DONE'`, runID).Scan(&doneSteps); err != nil {
		t.Fatalf("count steps: %v", err)
	}
	if doneSteps != 1 {
		t.Errorf("done step count = %d, want 1 (planner re-asked and dispatched only_step)", doneSteps)
	} else {
		t.Logf("PASS — %d step(s) completed after replay (planner was genuinely re-asked, not skipped)", doneSteps)
	}
}
