package orchestrator_test

// Integration tests for the orchestrator loop's steady-state behavior: the
// happy path, worker retry/DLQ, planner "fail" verdict, planner budget
// exhaustion (unreachable/malformed), and the wire-casing contract test.
// Require a real Postgres DB — skipped when TEST_DATABASE_URL is unset.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/orchestrator"
	"github.com/aaronwu000/stateflow/internal/store"
)

// ── Stub types ────────────────────────────────────────────────────────────

// countFailsThenSucceed returns failed(worker_reported) for the first
// maxFails dispatches, then done.
type countFailsThenSucceed struct {
	calls    atomic.Int64
	maxFails int64
}

func (t *countFailsThenSucceed) Dispatch(_ context.Context, _ core.StepSpec) (core.Result, error) {
	n := t.calls.Add(1)
	if n <= t.maxFails {
		return core.Result{
			Status:  core.ResultStatusFailed,
			Failure: &core.ResultFailure{Reason: core.FailureReasonWorkerReported, Error: fmt.Sprintf("attempt %d failed", n)},
		}, nil
	}
	return core.Result{Status: core.ResultStatusDone, Output: json.RawMessage(`{"ok":true}`)}, nil
}

// alwaysFailTransport always returns failed(worker_reported).
type alwaysFailTransport struct {
	calls atomic.Int64
}

func (t *alwaysFailTransport) Dispatch(_ context.Context, _ core.StepSpec) (core.Result, error) {
	n := t.calls.Add(1)
	return core.Result{
		Status:  core.ResultStatusFailed,
		Failure: &core.ResultFailure{Reason: core.FailureReasonWorkerReported, Error: fmt.Sprintf("persistent failure on attempt %d", n)},
	}, nil
}

// singleStepPlanner returns one step on the first call, then "done".
type singleStepPlanner struct {
	step     *core.StepSpec
	returned bool
}

func (p *singleStepPlanner) Decide(_ context.Context, state core.RunState) (core.StepDecision, error) {
	if len(state.History) > 0 || p.returned {
		return core.StepDecision{Status: core.PlannerVerdictDone}, nil
	}
	p.returned = true
	return core.StepDecision{Status: core.PlannerVerdictContinue, Step: p.step}, nil
}

// failPlanner always returns "fail" immediately.
type failPlanner struct{}

func (p *failPlanner) Decide(_ context.Context, _ core.RunState) (core.StepDecision, error) {
	return core.StepDecision{Status: core.PlannerVerdictFail}, nil
}

// ─── Test 0: happy path, 3 steps, barrier order ─────────────────────────────

// verifyingTransport asserts, on every Dispatch, that the step's decision
// row is ALREADY committed in the DB (non-null steps.decision) before the
// call arrives — the concrete, DB-observable form of Barrier 1 ("no code
// path dispatches before TX1 commits", the cross-session invariant
// checklist). It then returns done immediately. Looks the step up by
// (run_id, step_name) — step_id itself is only known inside the loop/store,
// not to a WorkerTransport, which only ever sees the StepSpec.
type verifyingTransport struct {
	t     *testing.T
	db    *sql.DB
	runID string
}

func (vt *verifyingTransport) Dispatch(_ context.Context, step core.StepSpec) (core.Result, error) {
	var decision sql.NullString
	if err := vt.db.QueryRow(
		`SELECT decision::text FROM steps WHERE run_id = $1 AND step_name = $2`,
		vt.runID, step.Name,
	).Scan(&decision); err != nil {
		vt.t.Fatalf("verifyingTransport: query decision for %q: %v", step.Name, err)
	}
	if !decision.Valid || decision.String == "" || decision.String == "null" {
		vt.t.Fatalf("BARRIER 1 VIOLATED: Dispatch(%s) called but steps.decision is not yet committed", step.Name)
	}
	return core.Result{Status: core.ResultStatusDone, Output: json.RawMessage(fmt.Sprintf(`{"step":%q}`, step.Name))}, nil
}

// threeStepPlanner returns step1, step2, step3 in order, then done.
type threeStepPlanner struct {
	steps []*core.StepSpec
	idx   int
	calls int
}

func (p *threeStepPlanner) Decide(_ context.Context, _ core.RunState) (core.StepDecision, error) {
	p.calls++
	if p.idx >= len(p.steps) {
		return core.StepDecision{Status: core.PlannerVerdictDone}, nil
	}
	s := p.steps[p.idx]
	p.idx++
	return core.StepDecision{Status: core.PlannerVerdictContinue, Step: s}, nil
}

// TestLoop_HappyPath_ThreeSteps_BarrierOrder verifies that the loop drives a
// run to completion across 3 consecutive steps, and that Barrier 1 holds for
// every single dispatch (verifyingTransport fails the test the instant it
// observes a Dispatch call preceding TX1's commit).
func TestLoop_HappyPath_ThreeSteps_BarrierOrder(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		wfID  = "wf-happy-path"
		runID = "run-happy-path"
	)
	seedWorkflowAndRun(t, db, wfID, runID, "static", staticPlannerConfigWithRetryLimit(3))

	steps := []*core.StepSpec{
		{Name: "step1", WorkerURL: "http://stub/step1", Mode: core.StepModeSync, TimeoutSeconds: 5, Input: json.RawMessage(`{}`)},
		{Name: "step2", WorkerURL: "http://stub/step2", Mode: core.StepModeSync, TimeoutSeconds: 5, Input: json.RawMessage(`{}`)},
		{Name: "step3", WorkerURL: "http://stub/step3", Mode: core.StepModeSync, TimeoutSeconds: 5, Input: json.RawMessage(`{}`)},
	}
	planner := &threeStepPlanner{steps: steps}

	s := store.New(db)
	loop := &orchestrator.Loop{
		RunID:          core.RunID(runID),
		WorkflowID:     core.WorkflowID(wfID),
		WorkflowInput:  json.RawMessage(`{"input":"test"}`),
		Store:          s,
		Transport:      &verifyingTransport{t: t, db: db, runID: runID},
		PlannerFactory: staticFactory(planner),
	}

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	var runStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE run_id = $1`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("query run status: %v", err)
	}
	if runStatus != "DONE" {
		t.Errorf("run.status = %q, want DONE", runStatus)
	}

	var stepCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM steps WHERE run_id = $1 AND status = 'DONE'`, runID).Scan(&stepCount); err != nil {
		t.Fatalf("count done steps: %v", err)
	}
	if stepCount != 3 {
		t.Errorf("DONE steps = %d, want 3", stepCount)
	}

	// seq must reflect creation order (1, 2, 3) — the sole ordering source.
	rows, err := db.Query(`SELECT step_name FROM steps WHERE run_id = $1 ORDER BY seq ASC`, runID)
	if err != nil {
		t.Fatalf("query step order: %v", err)
	}
	defer rows.Close()
	var order []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		order = append(order, name)
	}
	want := []string{"step1", "step2", "step3"}
	if len(order) != len(want) {
		t.Fatalf("step order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("step order[%d] = %q, want %q", i, order[i], want[i])
		}
	}
	t.Logf("PASS — run DONE, 3 steps in seq order %v, Barrier 1 held for every dispatch", order)

	if planner.calls != 4 {
		t.Errorf("planner.Decide called %d times, want 4 (3 continue + 1 done)", planner.calls)
	}
	t.Logf("PASS — planner called %d times (3 steps + final done)", planner.calls)
}

// ─── Test 1: retry exhausted → DLQ ──────────────────────────────────────────

// TestLoop_RetryExhausted_WritesDLQ runs a loop with a transport that always
// fails. After retryLimit=3 attempts, the step must be in DLQ and the DLQ
// table must have a row with reason='worker_retry_exhausted' containing the
// per-attempt context.
func TestLoop_RetryExhausted_WritesDLQ(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		retryLimit = 3
		wfID       = "wf-retry-exhausted"
		runID      = "run-retry-exhausted"
	)
	seedWorkflowAndRun(t, db, wfID, runID, "static", staticPlannerConfigWithRetryLimit(retryLimit))

	s := store.New(db)
	step := &core.StepSpec{
		Name:           "extract",
		WorkerURL:      "http://stub/extract",
		Mode:           core.StepModeSync,
		TimeoutSeconds: 5,
		Input:          json.RawMessage(`{}`),
	}

	transport := &alwaysFailTransport{}
	loop := &orchestrator.Loop{
		RunID:          core.RunID(runID),
		WorkflowID:     core.WorkflowID(wfID),
		WorkflowInput:  json.RawMessage(`{}`),
		Store:          s,
		Transport:      transport,
		Retry:          &orchestrator.FixedCountPolicy{MaxRetries: retryLimit, Delay: 0},
		PlannerFactory: staticFactory(&singleStepPlanner{step: step}),
	}

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	stepID := fmt.Sprintf("%s:extract", runID)
	var attemptCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE step_id = $1`, stepID).
		Scan(&attemptCount); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attemptCount != retryLimit {
		t.Errorf("attempts table has %d rows, want %d (one per dispatch)", attemptCount, retryLimit)
	}
	t.Logf("PASS — %d attempt rows in DB (retryLimit=%d)", attemptCount, retryLimit)

	var stepStatus string
	if err := db.QueryRow(`SELECT status FROM steps WHERE step_id = $1`, stepID).
		Scan(&stepStatus); err != nil {
		t.Fatalf("query step status: %v", err)
	}
	if stepStatus != "DLQ" {
		t.Errorf("step.status = %q, want DLQ", stepStatus)
	}
	t.Logf("PASS — step.status = DLQ")

	var runStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE run_id = $1`, runID).
		Scan(&runStatus); err != nil {
		t.Fatalf("query run status: %v", err)
	}
	if runStatus != "DLQ" {
		t.Errorf("run.status = %q, want DLQ", runStatus)
	}
	t.Logf("PASS — run.status = DLQ")

	var dlqReason string
	var dlqContext []byte
	if err := db.QueryRow(`
		SELECT reason, context FROM dead_letter_queue WHERE run_id = $1
	`, runID).Scan(&dlqReason, &dlqContext); err != nil {
		t.Fatalf("query dlq: %v", err)
	}
	if dlqReason != "worker_retry_exhausted" {
		t.Errorf("dlq.reason = %q, want worker_retry_exhausted", dlqReason)
	}
	t.Logf("PASS — DLQ entry: reason=%q context=%s", dlqReason, dlqContext)
}

// ─── Test 2: retry then succeed ─────────────────────────────────────────────

// TestLoop_RetryThenSucceed verifies that after 2 failures, the 3rd attempt
// succeeds: the run completes normally with no DLQ entry and run.status=DONE.
func TestLoop_RetryThenSucceed(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		retryLimit = 3
		wfID       = "wf-retry-succeed"
		runID      = "run-retry-succeed"
	)
	seedWorkflowAndRun(t, db, wfID, runID, "static", staticPlannerConfigWithRetryLimit(retryLimit))

	s := store.New(db)
	step := &core.StepSpec{
		Name:           "process",
		WorkerURL:      "http://stub/process",
		Mode:           core.StepModeSync,
		TimeoutSeconds: 5,
		Input:          json.RawMessage(`{}`),
	}

	transport := &countFailsThenSucceed{maxFails: 2}
	loop := &orchestrator.Loop{
		RunID:          core.RunID(runID),
		WorkflowID:     core.WorkflowID(wfID),
		WorkflowInput:  json.RawMessage(`{}`),
		Store:          s,
		Transport:      transport,
		Retry:          &orchestrator.FixedCountPolicy{MaxRetries: retryLimit, Delay: 0},
		PlannerFactory: staticFactory(&singleStepPlanner{step: step}),
	}

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	stepID := fmt.Sprintf("%s:process", runID)
	var attemptCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE step_id = $1`, stepID).
		Scan(&attemptCount); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attemptCount != 3 {
		t.Errorf("attempts = %d, want 3 (2 failed + 1 done)", attemptCount)
	}
	t.Logf("PASS — %d attempts (2 fail + 1 done)", attemptCount)

	var dlqCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM dead_letter_queue WHERE run_id = $1`, runID).
		Scan(&dlqCount); err != nil {
		t.Fatalf("count dlq: %v", err)
	}
	if dlqCount != 0 {
		t.Errorf("DLQ has %d entries, want 0 (retry succeeded)", dlqCount)
	}
	t.Log("PASS — no DLQ entry (step succeeded on 3rd attempt)")

	var runStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE run_id = $1`, runID).
		Scan(&runStatus); err != nil {
		t.Fatalf("query run status: %v", err)
	}
	if runStatus != "DONE" {
		t.Errorf("run.status = %q, want DONE", runStatus)
	}
	t.Logf("PASS — run.status = DONE")

	if n := transport.calls.Load(); n != 3 {
		t.Errorf("transport called %d times, want 3", n)
	}
	t.Logf("PASS — transport dispatched %d times", transport.calls.Load())
}

// ─── Test 3: planner declares failure → DLQ(planner_declared_fail) ─────────

// TestLoop_PlannerFail_WritesDLQ verifies that when the planner returns
// status:"fail", the loop writes a DLQ entry with reason='planner_declared_fail'
// (step_id NULL, since no step is at fault) and marks the run DLQ.
func TestLoop_PlannerFail_WritesDLQ(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		wfID  = "wf-planner-fail"
		runID = "run-planner-fail"
	)
	seedWorkflowAndRun(t, db, wfID, runID, "static", staticPlannerConfigWithRetryLimit(3))

	s := store.New(db)
	loop := &orchestrator.Loop{
		RunID:          core.RunID(runID),
		WorkflowID:     core.WorkflowID(wfID),
		WorkflowInput:  json.RawMessage(`{}`),
		Store:          s,
		Transport:      &alwaysFailTransport{},
		PlannerFactory: staticFactory(&failPlanner{}),
	}

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	var runStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE run_id = $1`, runID).
		Scan(&runStatus); err != nil {
		t.Fatalf("query run status: %v", err)
	}
	if runStatus != "DLQ" {
		t.Errorf("run.status = %q, want DLQ", runStatus)
	}
	t.Logf("PASS — run.status = DLQ")

	var dlqReason string
	var dlqStepID *string
	if err := db.QueryRow(`
		SELECT reason, step_id FROM dead_letter_queue WHERE run_id = $1
	`, runID).Scan(&dlqReason, &dlqStepID); err != nil {
		t.Fatalf("query dlq: %v", err)
	}
	if dlqReason != "planner_declared_fail" {
		t.Errorf("dlq.reason = %q, want planner_declared_fail", dlqReason)
	}
	if dlqStepID != nil {
		t.Errorf("dlq.step_id = %q, want NULL (no specific step to blame)", *dlqStepID)
	}
	t.Logf("PASS — DLQ entry: reason=%q, step_id IS NULL", dlqReason)

	var stepCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM steps WHERE run_id = $1`, runID).
		Scan(&stepCount); err != nil {
		t.Fatalf("count steps: %v", err)
	}
	if stepCount != 0 {
		t.Errorf("steps table has %d rows, want 0 (planner failed before first step)", stepCount)
	}
	t.Logf("PASS — steps table empty (planner failed before any dispatch)")
}

// ─── Test 4: planner budget exhausted — malformed ───────────────────────────

// alwaysMalformedHandler always responds 200 with a body that fails §12.3's
// acceptance criteria (missing status). HTTPPlanner.Decide classifies this
// as *planner.MalformedError near-instantly — no ctx-deadline wait needed,
// so this test stays fast despite exercising the full 3-attempt budget.
func alwaysMalformedHandler(calls *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"oops":"no status field"}`))
	}
}

// TestLoop_PlannerBudgetExhausted_Malformed verifies §7.2: 3 consecutive
// malformed planner answers exhaust the budget and land the run in the DLQ
// with reason=planner_malformed, full per-attempt detail in context, and the
// planner is called exactly 3 times (never more — no unbounded retry).
func TestLoop_PlannerBudgetExhausted_Malformed(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	var calls atomic.Int32
	srv := httptest.NewServer(alwaysMalformedHandler(&calls))
	defer srv.Close()

	const (
		wfID  = "wf-planner-malformed"
		runID = "run-planner-malformed"
	)
	cfg, _ := json.Marshal(map[string]string{"url": srv.URL})
	seedWorkflowAndRun(t, db, wfID, runID, "http", string(cfg))

	s := store.New(db)
	loop := &orchestrator.Loop{
		RunID:         core.RunID(runID),
		WorkflowID:    core.WorkflowID(wfID),
		WorkflowInput: json.RawMessage(`{}`),
		Store:         s,
		Transport:     &alwaysFailTransport{},
		// PlannerFactory left nil: exercises the REAL planner_type="http"
		// reconstruction path (whitepaper §12.1) end to end.
	}

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	if n := calls.Load(); n != 3 {
		t.Errorf("planner called %d times, want exactly 3 (the fixed budget)", n)
	}
	t.Logf("PASS — planner called exactly %d times", calls.Load())

	var runStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE run_id = $1`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("query run status: %v", err)
	}
	if runStatus != "DLQ" {
		t.Errorf("run.status = %q, want DLQ", runStatus)
	}

	var dlqReason string
	var dlqContext []byte
	if err := db.QueryRow(`SELECT reason, context FROM dead_letter_queue WHERE run_id = $1`, runID).
		Scan(&dlqReason, &dlqContext); err != nil {
		t.Fatalf("query dlq: %v", err)
	}
	if dlqReason != "planner_malformed" {
		t.Errorf("dlq.reason = %q, want planner_malformed", dlqReason)
	}
	// dead_letter_queue.context is always the store's own wrapper
	// {"detail": "<detail param, verbatim>"} (MarkRunDLQPlannerExhausted,
	// internal/store/postgres.go — out of this session's scope). loop.go's
	// decideWithBudget passes a JSON-ARRAY-encoded string as that detail
	// param, so the per-attempt list requires unwrapping twice.
	var wrapper struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(dlqContext, &wrapper); err != nil {
		t.Fatalf("unmarshal dlq context wrapper: %v (raw: %s)", err, dlqContext)
	}
	var detail []map[string]any
	if err := json.Unmarshal([]byte(wrapper.Detail), &detail); err != nil {
		t.Fatalf("unmarshal dlq context detail: %v (raw: %s)", err, wrapper.Detail)
	}
	if len(detail) != 3 {
		t.Errorf("dlq context has %d attempt entries, want 3", len(detail))
	}
	for i, d := range detail {
		if d["class"] != "malformed" {
			t.Errorf("attempt %d: class = %v, want malformed", i, d["class"])
		}
	}
	t.Logf("PASS — DLQ entry: reason=%q, %d per-attempt detail entries, all class=malformed", dlqReason, len(detail))
}

// ─── Test 5: planner budget exhausted — unreachable ─────────────────────────

// TestLoop_PlannerBudgetExhausted_Unreachable verifies §7.2: 3 consecutive
// dial failures (nothing listening) are classified as unreachable, exhaust
// the budget, and land the run in the DLQ with reason=planner_unreachable.
func TestLoop_PlannerBudgetExhausted_Unreachable(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const (
		wfID  = "wf-planner-unreachable"
		runID = "run-planner-unreachable"
	)
	// Reserved port, nothing listens here — dial fails near-instantly.
	cfg, _ := json.Marshal(map[string]string{"url": "http://127.0.0.1:1"})
	seedWorkflowAndRun(t, db, wfID, runID, "http", string(cfg))

	s := store.New(db)
	loop := &orchestrator.Loop{
		RunID:         core.RunID(runID),
		WorkflowID:    core.WorkflowID(wfID),
		WorkflowInput: json.RawMessage(`{}`),
		Store:         s,
		Transport:     &alwaysFailTransport{},
	}

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	var runStatus, dlqReason string
	if err := db.QueryRow(`SELECT status FROM runs WHERE run_id = $1`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("query run status: %v", err)
	}
	if runStatus != "DLQ" {
		t.Errorf("run.status = %q, want DLQ", runStatus)
	}
	if err := db.QueryRow(`SELECT reason FROM dead_letter_queue WHERE run_id = $1`, runID).Scan(&dlqReason); err != nil {
		t.Fatalf("query dlq: %v", err)
	}
	if dlqReason != "planner_unreachable" {
		t.Errorf("dlq.reason = %q, want planner_unreachable", dlqReason)
	}
	t.Logf("PASS — run.status=DLQ, dlq.reason=%q", dlqReason)
}

// ─── Mandatory test (e): wire-casing contract test ──────────────────────────

// capturedRequest holds every raw JSON body sent to the fake planner HTTP
// endpoint, for byte-level wire assertions.
type capturedRequest struct {
	mu   sync.Mutex
	raws [][]byte
}

func (c *capturedRequest) add(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raws = append(c.raws, b)
}

// alwaysSucceedTransport is defined in recovery_test.go (same package) and
// reused here.

// TestLoop_WireCasing_HistoryStatusUppercaseAndSeqOrdered is the mandatory
// Session 5 wire-casing contract test: it captures the EXACT JSON bytes the
// real HTTPPlanner sends to its endpoint across a 2-step run driven by the
// real loop (planner_type="http", reconstructed from the workflow row — no
// PlannerFactory override), and asserts every history entry's status is the
// literal uppercase string "DONE" on the wire, and that history entries
// appear in seq order (step "alpha" before step "beta").
func TestLoop_WireCasing_HistoryStatusUppercaseAndSeqOrdered(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	captured := &capturedRequest{}
	var callN atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		captured.add(raw)

		n := callN.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			w.Write([]byte(`{"status":"continue","step":{"name":"alpha","worker_url":"http://stub/alpha","mode":"sync","input":{}}}`))
		case 2:
			w.Write([]byte(`{"status":"continue","step":{"name":"beta","worker_url":"http://stub/beta","mode":"sync","input":{}}}`))
		default:
			w.Write([]byte(`{"status":"done"}`))
		}
	}))
	defer srv.Close()

	const (
		wfID  = "wf-wire-casing"
		runID = "run-wire-casing"
	)
	cfg, _ := json.Marshal(map[string]string{"url": srv.URL})
	seedWorkflowAndRun(t, db, wfID, runID, "http", string(cfg))

	s := store.New(db)
	loop := &orchestrator.Loop{
		RunID:         core.RunID(runID),
		WorkflowID:    core.WorkflowID(wfID),
		WorkflowInput: json.RawMessage(`{"doc":"x"}`),
		Store:         s,
		Transport:     &alwaysSucceedTransport{},
		// PlannerFactory left nil: real HTTPPlanner reconstructed from the
		// workflow row (whitepaper §12.1) — this is the genuine wire path.
	}

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	if len(captured.raws) != 3 {
		t.Fatalf("captured %d planner requests, want 3 (alpha, beta, done)", len(captured.raws))
	}

	// Call 2 (asking what comes after "alpha"): history has exactly 1 entry,
	// status must be the literal uppercase "DONE" on the wire.
	var call2 struct {
		History []map[string]json.RawMessage `json:"history"`
	}
	if err := json.Unmarshal(captured.raws[1], &call2); err != nil {
		t.Fatalf("unmarshal call 2 body: %v (raw: %s)", err, captured.raws[1])
	}
	if len(call2.History) != 1 {
		t.Fatalf("call 2: history has %d entries, want 1", len(call2.History))
	}
	if string(call2.History[0]["status"]) != `"DONE"` {
		t.Errorf(`call 2: history[0].status = %s, want the literal wire value "DONE"`, call2.History[0]["status"])
	}
	if string(call2.History[0]["name"]) != `"alpha"` {
		t.Errorf(`call 2: history[0].name = %s, want "alpha"`, call2.History[0]["name"])
	}
	t.Logf("PASS — call 2 wire body: history[0] = %s", captured.raws[1])

	// Call 3 (asking what comes after "beta"): history has 2 entries, BOTH
	// uppercase "DONE", in seq order (alpha, then beta).
	var call3 struct {
		History []map[string]json.RawMessage `json:"history"`
	}
	if err := json.Unmarshal(captured.raws[2], &call3); err != nil {
		t.Fatalf("unmarshal call 3 body: %v (raw: %s)", err, captured.raws[2])
	}
	if len(call3.History) != 2 {
		t.Fatalf("call 3: history has %d entries, want 2", len(call3.History))
	}
	wantNames := []string{`"alpha"`, `"beta"`}
	for i, want := range wantNames {
		if string(call3.History[i]["name"]) != want {
			t.Errorf("call 3: history[%d].name = %s, want %s (seq order violated)", i, call3.History[i]["name"], want)
		}
		if string(call3.History[i]["status"]) != `"DONE"` {
			t.Errorf(`call 3: history[%d].status = %s, want the literal wire value "DONE"`, i, call3.History[i]["status"])
		}
	}
	t.Logf("PASS — call 3 wire body: both history entries UPPERCASE \"DONE\", in seq order (alpha, beta): %s", captured.raws[2])
}
