package api_test

// Integration tests for the HTTP API server.
// Require a real Postgres DB (TEST_DATABASE_URL must be set).
// Each test uses httptest.Server so no ports are actually opened.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/aaronwu000/stateflow/internal/api"
	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/orchestrator"
	"github.com/aaronwu000/stateflow/internal/planner"
	"github.com/aaronwu000/stateflow/internal/store"
	"github.com/aaronwu000/stateflow/internal/transport"
)

// ── DB helpers (mirrors orchestrator integration test) ────────────────────

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

// newTestServer creates an API server backed by db and a real sync+async transport.
// Returns the httptest.Server and a cancel function for the loop context.
func newTestServer(t *testing.T, db *sql.DB) (*httptest.Server, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	s := store.New(db)
	syncT := transport.NewSyncTransport()
	asyncT := transport.NewAsyncTransport()
	routedT := &transport.MultiTransport{Sync: syncT, Async: asyncT}

	startLoop := func(loopCtx context.Context, runID core.RunID, workflowInput json.RawMessage, plannerType string, plannerConfig json.RawMessage) {
		var p core.NextStepPlanner
		var err error
		switch plannerType {
		case "static":
			p, err = planner.NewStaticPlanner(plannerConfig)
			if err != nil {
				t.Errorf("NewStaticPlanner: %v", err)
				return
			}
		case "http":
			p, err = planner.NewHTTPPlanner(plannerConfig)
			if err != nil {
				t.Errorf("NewHTTPPlanner: %v", err)
				return
			}
		default:
			t.Errorf("unknown planner_type %q", plannerType)
			return
		}
		l := &orchestrator.Loop{
			RunID:         runID,
			WorkflowInput: workflowInput,
			Store:         s,
			Planner:       p,
			Transport:     routedT,
			Retry:         &orchestrator.FixedCountPolicy{MaxRetries: 3, Delay: 0},
		}
		go l.Run(loopCtx)
	}

	srv := api.New(db, asyncT, ctx, startLoop)
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
		t.Logf("PASS — steps[0].status = %s", s0["status"])
	}
}

// ── Test 2: Callback dedup — superseded attempt_id ────────────────────────

// TestAPI_Callback_Dedup verifies that POST /tasks/complete with a superseded
// attempt_id returns 200 but leaves the step state unchanged.
//
// This tests the at-least-once delivery dedup guard (CLAUDE.md).
func TestAPI_Callback_Dedup(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)

	apiSrv, _ := newTestServer(t, db)
	base := apiSrv.URL

	// Plant a workflow + run + step directly (no loop running).
	const wfID  = "wf-dedup"
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
		INSERT INTO steps (step_id, run_id, step_name, seq, status, decision, current_attempt_id, decided_at)
		VALUES ($1, $2, 'step1', 1, 'RUNNING', $3::jsonb, $4::uuid, now())
	`, stepID, runID, decision, realAttempt); err != nil {
		t.Fatalf("seed step: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO attempts (attempt_id, step_id, attempt_number, status)
		VALUES ($1::uuid, $2, 1, 'RUNNING')
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
}

// ── Test 3: DLQ endpoints ────────────────────────────────────────────────

// TestAPI_DLQ_ListAndReplay verifies GET /dlq and POST /dlq/:id/replay.
//
// Plants a DLQ'd run directly in the DB (a step in DLQ status + a DLQ entry),
// then verifies GET /dlq lists it and POST /dlq/:id/replay re-queues it
// and the run eventually completes.
func TestAPI_DLQ_ListAndReplay(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)

	// Fake worker for post-replay dispatch: always succeeds.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"replayed":true}`)
	}))
	defer worker.Close()

	apiSrv, _ := newTestServer(t, db)
	base := apiSrv.URL

	// Plant the DLQ state directly.
	const wfID  = "wf-dlq"
	const runID = "run-dlq"
	plannerConfig := fmt.Sprintf(
		`{"steps":[{"name":"extract","worker_url":%q,"mode":"sync","timeout_seconds":5}]}`,
		worker.URL,
	)

	if _, err := db.Exec(`
		INSERT INTO workflows (workflow_id, name, planner_type, planner_config)
		VALUES ($1, 'dlq-wf', 'static', $2::jsonb)
	`, wfID, plannerConfig); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO runs (run_id, workflow_id, status, workflow_input)
		VALUES ($1, $2, 'FAILED', '{}')
	`, runID, wfID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	stepID := runID + ":extract"
	decision := fmt.Sprintf(`{"name":"extract","worker_url":%q,"mode":"sync","timeout_seconds":5}`, worker.URL)
	if _, err := db.Exec(`
		INSERT INTO steps (step_id, run_id, step_name, seq, status, decision, decided_at)
		VALUES ($1, $2, 'extract', 1, 'DLQ', $3::jsonb, now())
	`, stepID, runID, decision); err != nil {
		t.Fatalf("seed step: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO dead_letter_queue (run_id, step_id, reason, context)
		VALUES ($1, $2, 'retry_exhausted', '{"last_error":"persistent failure","step_name":"extract","worker_url":"http://stub"}')
	`, runID, stepID); err != nil {
		t.Fatalf("seed dlq: %v", err)
	}

	// ── GET /dlq → should list our entry ──────────────────────────────────
	resp := get(t, base+"/dlq")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /dlq: status %d", resp.StatusCode)
	}
	dlqBody := decodeJSON(t, resp)
	entries := dlqBody["entries"].([]any)
	if len(entries) == 0 {
		t.Fatal("GET /dlq: empty entries, want at least 1")
	}
	first := entries[0].(map[string]any)
	dlqIDFloat := first["id"].(float64)
	dlqID := int64(dlqIDFloat)
	if first["reason"] != "retry_exhausted" {
		t.Errorf("dlq[0].reason = %q, want retry_exhausted", first["reason"])
	}
	if first["run_id"] != runID {
		t.Errorf("dlq[0].run_id = %q, want %q", first["run_id"], runID)
	}
	t.Logf("PASS — GET /dlq returned %d entry(ies), id=%d reason=%s", len(entries), dlqID, first["reason"])

	// ── POST /dlq/:id/replay → should re-queue and eventually complete ────
	resp = post(t, fmt.Sprintf("%s/dlq/%d/replay", base, dlqID), ``)
	if resp.StatusCode != http.StatusAccepted {
		body := decodeJSON(t, resp)
		t.Fatalf("POST /dlq/replay: status %d, body=%v", resp.StatusCode, body)
	}
	replayData := decodeJSON(t, resp)
	if replayData["run_id"] != runID {
		t.Errorf("replay response run_id = %q, want %q", replayData["run_id"], runID)
	}
	t.Logf("PASS — POST /dlq/%d/replay → 202, run_id=%s", dlqID, replayData["run_id"])

	// Poll until run completes (the loop re-dispatches the DECIDED step via the fake worker).
	finalStatus := pollRunStatus(t, base, runID, 10*time.Second)
	if finalStatus != "DONE" {
		t.Errorf("run.status after replay = %q, want DONE", finalStatus)
	}
	t.Logf("PASS — run %s → %s after DLQ replay", runID, finalStatus)

	// The step should be DONE.
	var stepStatus string
	if err := db.QueryRow(`SELECT status FROM steps WHERE step_id = $1`, stepID).Scan(&stepStatus); err != nil {
		t.Fatalf("query step: %v", err)
	}
	if stepStatus != "DONE" {
		t.Errorf("step.status = %q, want DONE", stepStatus)
	}
	t.Logf("PASS — step.status = DONE after replay")
}

// ── Test 4: GET /runs/:id — unknown run returns 404 ──────────────────────

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

// ── Test 5: End-to-end with HTTP planner ─────────────────────────────────

// TestAPI_EndToEnd_HTTPPlanner verifies the full happy path with an HTTP planner:
//
//	POST /workflows (planner_type="http") → POST /workflows/:id/runs →
//	fake planner server → fake worker → run DONE
//
// The fake planner inspects RunState.History to decide:
//   - empty history → continue (dispatch step1)
//   - step1 in history → done
//
// This proves the loop, HTTPPlanner, and the API are wired together correctly.
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
	//   call 2 (step1 done)    → done
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
		} else {
			fmt.Fprint(w, `{"status":"done"}`)
		}
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
}
