package orchestrator_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/orchestrator"
)

// ─── Stub implementations ───────────────────────────────────────────────────

// stubPlanner returns steps from a pre-loaded slice in order, then "done".
type stubPlanner struct {
	mu    sync.Mutex
	steps []*core.StepSpec
	idx   int
	calls []string // records each Decide call with the history length
}

func (p *stubPlanner) Decide(_ context.Context, state core.RunState) (core.StepDecision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, fmt.Sprintf("Decide(historyLen=%d)", len(state.History)))
	if p.idx >= len(p.steps) {
		return core.StepDecision{Status: "done"}, nil
	}
	step := p.steps[p.idx]
	p.idx++
	return core.StepDecision{Status: "continue", Step: step}, nil
}

// stubTransport returns results from a pre-loaded slice in order.
// Panics if Dispatch is called more times than results are provided.
type stubTransport struct {
	mu      sync.Mutex
	results []core.Result
	idx     int
	calls   []string // records each dispatch's step name
}

func (t *stubTransport) Dispatch(_ context.Context, step core.StepSpec) (core.Result, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.idx >= len(t.results) {
		panic(fmt.Sprintf("stubTransport: unexpected Dispatch #%d for step %q", t.idx+1, step.Name))
	}
	t.calls = append(t.calls, fmt.Sprintf("Dispatch(%s)", step.Name))
	r := t.results[t.idx]
	t.idx++
	return r, nil
}

// fixedRetryPolicy allows up to `max` attempts then routes to DLQ.
// delay=0 so tests don't sleep.
type fixedRetryPolicy struct {
	max int
}

func (r *fixedRetryPolicy) Next(attempt int, _ error) (time.Duration, bool) {
	return 0, attempt >= r.max
}

// ─── In-memory stub Store ───────────────────────────────────────────────────

// stubStore is a thread-safe in-memory implementation of orchestrator.Store.
// It tracks call order for barrier-order verification.
type stubStore struct {
	mu      sync.Mutex
	history []core.HistoryEntry
	pending *core.StepSpec // current DECIDED-not-DONE step

	calls     []string // ordered call log
	runStatus string   // "DONE" | "FAILED"
	dlqed     bool
	dlqReason string
}

func newStubStore() *stubStore {
	return &stubStore{runStatus: "RUNNING"}
}

func (s *stubStore) log(op string) {
	s.calls = append(s.calls, op)
}

func (s *stubStore) PendingDecision(_ core.RunID) (*core.StepSpec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log("PendingDecision")
	if s.pending == nil {
		return nil, nil
	}
	cp := *s.pending
	return &cp, nil
}

func (s *stubStore) LoadFrontier(_ core.RunID) (core.Frontier, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log("LoadFrontier")
	var pd *core.StepSpec
	if s.pending != nil {
		cp := *s.pending
		pd = &cp
	}
	hist := make([]core.HistoryEntry, len(s.history))
	copy(hist, s.history)
	return core.Frontier{History: hist, PendingDecision: pd}, nil
}

func (s *stubStore) PutDecision(_ core.RunID, step core.StepSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log(fmt.Sprintf("PutDecision(%s)", step.Name))
	cp := step
	s.pending = &cp
	return nil
}

func (s *stubStore) RecordAttemptStart(_ core.RunID, step core.StepSpec, _ core.AttemptID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log(fmt.Sprintf("RecordAttemptStart(%s)", step.Name))
	return nil
}

func (s *stubStore) Checkpoint(_ core.RunID, step core.StepSpec, r core.Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Status == "done" {
		s.log(fmt.Sprintf("Checkpoint(%s:done)", step.Name))
		s.history = append(s.history, core.HistoryEntry{
			Name:   step.Name,
			Status: "DONE",
			Output: r.Output,
		})
		s.pending = nil
	} else {
		s.log(fmt.Sprintf("Checkpoint(%s:failed)", step.Name))
		// Path B: pending stays set (step stays DECIDED/FAILED — needs ResetToDecided to retry)
	}
	return nil
}

func (s *stubStore) ResetToDecided(_ core.RunID, step core.StepSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log(fmt.Sprintf("ResetToDecided(%s)", step.Name))
	return nil
}

func (s *stubStore) MarkDLQ(_ core.RunID, step core.StepSpec, reason string, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log(fmt.Sprintf("MarkDLQ(%s)", step.Name))
	s.dlqed = true
	s.dlqReason = reason
	s.runStatus = "FAILED"
	s.pending = nil
	return nil
}

func (s *stubStore) MarkRunDone(_ core.RunID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log("MarkRunDone")
	s.runStatus = "DONE"
	return nil
}

func (s *stubStore) MarkRunFailed(_ core.RunID, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log("MarkRunFailed")
	s.runStatus = "FAILED"
	return nil
}

func (s *stubStore) MarkPlannerFailedDLQ(_ core.RunID, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log("MarkPlannerFailedDLQ")
	s.dlqed = true
	s.dlqReason = "planner_failed"
	s.runStatus = "FAILED"
	return nil
}

// callsContaining returns all calls that contain substr.
func (s *stubStore) callsContaining(substr string) []string {
	var out []string
	for _, c := range s.calls {
		if strings.Contains(c, substr) {
			out = append(out, c)
		}
	}
	return out
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func makeStep(name string) *core.StepSpec {
	return &core.StepSpec{
		Name:           name,
		WorkerURL:      "http://stub/" + name,
		Mode:           "sync",
		TimeoutSeconds: 30,
		Input:          json.RawMessage(`{}`),
	}
}

func doneResult(name string) core.Result {
	return core.Result{
		Status: "done",
		Output: json.RawMessage(fmt.Sprintf(`{"step":%q}`, name)),
	}
}

func failResult(msg string) core.Result {
	return core.Result{Status: "failed", Error: msg}
}

// ─── Test 1: Happy path — 3 steps, all succeed ──────────────────────────────

// TestLoop_HappyPath_ThreeSteps verifies that the loop drives a run to
// completion when the planner returns three consecutive steps and the
// transport reports success for each. Run() must return nil and the
// store must end in DONE status with all three steps in history.
func TestLoop_HappyPath_ThreeSteps(t *testing.T) {
	steps := []*core.StepSpec{makeStep("step1"), makeStep("step2"), makeStep("step3")}

	store := newStubStore()
	planner := &stubPlanner{steps: steps}
	transport := &stubTransport{
		results: []core.Result{
			doneResult("step1"),
			doneResult("step2"),
			doneResult("step3"),
		},
	}

	loop := &orchestrator.Loop{
		RunID:         core.RunID("run-happy"),
		WorkflowInput: json.RawMessage(`{"input":"test"}`),
		Store:         store,
		Planner:       planner,
		Transport:     transport,
		Retry:         &fixedRetryPolicy{max: 3},
	}

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	// ── Assert: run ended DONE ──
	if store.runStatus != "DONE" {
		t.Errorf("runStatus = %q, want DONE", store.runStatus)
	}

	// ── Assert: all 3 steps in history ──
	if len(store.history) != 3 {
		t.Fatalf("history has %d entries, want 3; entries: %v", len(store.history), store.history)
	}
	for i, want := range []string{"step1", "step2", "step3"} {
		got := store.history[i].Name
		if got != want {
			t.Errorf("history[%d].Name = %q, want %q", i, got, want)
		}
	}

	// ── Assert: transport was called exactly 3 times ──
	if len(transport.calls) != 3 {
		t.Errorf("transport.Dispatch called %d times, want 3", len(transport.calls))
	}

	// ── Assert: planner was called exactly 4 times (3 "continue" + 1 "done") ──
	if len(planner.calls) != 4 {
		t.Errorf("planner.Decide called %d times, want 4 (3 steps + 1 done)", len(planner.calls))
	}

	t.Logf("PASS — store.calls: %v", store.calls)
}

// ─── Test 2: Barrier order verification ─────────────────────────────────────

// TestLoop_BarrierOrder verifies that the two write barriers fire in the
// correct order for every step:
//
//	PutDecision  (Barrier 1) before RecordAttemptStart
//	RecordAttemptStart before Dispatch
//	Checkpoint   (Barrier 2) before next PendingDecision/LoadFrontier/Decide
//
// This test asserts on the actual call sequence recorded by stubStore and
// stubTransport combined — not just that they were called, but that they
// were called in the required durable order.
func TestLoop_BarrierOrder(t *testing.T) {
	steps := []*core.StepSpec{makeStep("alpha"), makeStep("beta")}

	store := newStubStore()
	transport := &stubTransport{
		results: []core.Result{doneResult("alpha"), doneResult("beta")},
	}
	// Combined call log: store calls + transport dispatch calls, in arrival order.
	// We use a shared mutex-protected slice and wrap transport.Dispatch to also log.
	var mu sync.Mutex
	var fullLog []string

	logStore := &loggingStoreProxy{
		inner: store,
		onCall: func(op string) {
			mu.Lock()
			fullLog = append(fullLog, op)
			mu.Unlock()
		},
	}
	logTransport := &loggingTransport{
		inner: transport,
		onCall: func(name string) {
			mu.Lock()
			fullLog = append(fullLog, fmt.Sprintf("Dispatch(%s)", name))
			mu.Unlock()
		},
	}

	loop := &orchestrator.Loop{
		RunID:         core.RunID("run-order"),
		WorkflowInput: json.RawMessage(`{}`),
		Store:         logStore,
		Planner:       &stubPlanner{steps: steps},
		Transport:     logTransport,
		Retry:         &fixedRetryPolicy{max: 3},
	}

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// ── Check that for each step, PutDecision appears before Dispatch,
	//    and Checkpoint appears after Dispatch, before the NEXT PendingDecision ──
	t.Logf("full call log: %v", fullLog)

	for _, stepName := range []string{"alpha", "beta"} {
		putIdx := indexOf(fullLog, fmt.Sprintf("PutDecision(%s)", stepName))
		recIdx := indexOf(fullLog, fmt.Sprintf("RecordAttemptStart(%s)", stepName))
		dispIdx := indexOf(fullLog, fmt.Sprintf("Dispatch(%s)", stepName))
		ckIdx := indexOf(fullLog, fmt.Sprintf("Checkpoint(%s:done)", stepName))

		if putIdx < 0 {
			t.Errorf("step %q: PutDecision not found in log", stepName)
			continue
		}
		if recIdx < 0 {
			t.Errorf("step %q: RecordAttemptStart not found in log", stepName)
			continue
		}
		if dispIdx < 0 {
			t.Errorf("step %q: Dispatch not found in log", stepName)
			continue
		}
		if ckIdx < 0 {
			t.Errorf("step %q: Checkpoint(done) not found in log", stepName)
			continue
		}

		// Barrier 1: PutDecision before RecordAttemptStart before Dispatch
		if putIdx > recIdx {
			t.Errorf("BARRIER 1 VIOLATED for %q: PutDecision (idx %d) after RecordAttemptStart (idx %d)",
				stepName, putIdx, recIdx)
		}
		if recIdx > dispIdx {
			t.Errorf("BARRIER 1 VIOLATED for %q: RecordAttemptStart (idx %d) after Dispatch (idx %d)",
				stepName, recIdx, dispIdx)
		}

		// Barrier 2: Checkpoint after Dispatch
		if dispIdx > ckIdx {
			t.Errorf("BARRIER 2 VIOLATED for %q: Dispatch (idx %d) after Checkpoint (idx %d)",
				stepName, dispIdx, ckIdx)
		}

		t.Logf("PASS — %q: PutDecision@%d RecordAttemptStart@%d Dispatch@%d Checkpoint@%d",
			stepName, putIdx, recIdx, dispIdx, ckIdx)
	}

	// After the last Checkpoint, MarkRunDone must come last.
	lastCk := lastIndexOf(fullLog, "Checkpoint(beta:done)")
	doneIdx := indexOf(fullLog, "MarkRunDone")
	if doneIdx < 0 {
		t.Error("MarkRunDone not found in call log")
	} else if doneIdx < lastCk {
		t.Errorf("MarkRunDone (idx %d) before last Checkpoint (idx %d)", doneIdx, lastCk)
	} else {
		t.Logf("PASS — MarkRunDone@%d after last Checkpoint@%d", doneIdx, lastCk)
	}
}

// ─── Test 3: Retry — fail once, succeed second time ─────────────────────────

// TestLoop_Retry_FailThenSucceed verifies the retry path:
//
//  1. Worker fails on attempt 1 → Checkpoint(Path B) fires (Barrier 2 still holds).
//  2. Loop calls ResetToDecided to make the step recoverable during the sleep window.
//  3. Worker succeeds on attempt 2 → Checkpoint(Path A) fires.
//  4. Planner then returns "done" → MarkRunDone.
//
// Run() must return nil (the run completed successfully after one retry).
func TestLoop_Retry_FailThenSucceed(t *testing.T) {
	store := newStubStore()
	transport := &stubTransport{
		results: []core.Result{
			failResult("worker timeout"),
			doneResult("step1"),
		},
	}

	loop := &orchestrator.Loop{
		RunID:         core.RunID("run-retry"),
		WorkflowInput: json.RawMessage(`{}`),
		Store:         store,
		Planner:       &stubPlanner{steps: []*core.StepSpec{makeStep("step1")}},
		Transport:     transport,
		Retry:         &fixedRetryPolicy{max: 2}, // allow 1 retry (fail on attempt>=2 → DLQ)
	}

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	// ── Assert: run completed DONE ──
	if store.runStatus != "DONE" {
		t.Errorf("runStatus = %q, want DONE", store.runStatus)
	}

	// ── Assert: transport dispatched exactly twice (1 fail + 1 success) ──
	if len(transport.calls) != 2 {
		t.Errorf("transport.Dispatch called %d times, want 2 (1 fail + 1 success)", len(transport.calls))
	}

	// ── Assert: Checkpoint was called twice ──
	ckFailed := store.callsContaining("Checkpoint(step1:failed)")
	ckDone := store.callsContaining("Checkpoint(step1:done)")
	if len(ckFailed) != 1 {
		t.Errorf("Checkpoint(step1:failed) called %d times, want 1", len(ckFailed))
	}
	if len(ckDone) != 1 {
		t.Errorf("Checkpoint(step1:done) called %d times, want 1", len(ckDone))
	}

	// ── Assert: Barrier 2 holds on failure — Checkpoint(failed) before ResetToDecided ──
	fullLog := store.calls
	ckFailIdx := indexOf(fullLog, "Checkpoint(step1:failed)")
	resetIdx := indexOf(fullLog, "ResetToDecided(step1)")
	if ckFailIdx < 0 {
		t.Error("Checkpoint(step1:failed) not in call log")
	}
	if resetIdx < 0 {
		t.Error("ResetToDecided(step1) not in call log")
	}
	if ckFailIdx >= 0 && resetIdx >= 0 && ckFailIdx > resetIdx {
		t.Errorf("BARRIER 2 VIOLATED: Checkpoint(failed) idx=%d after ResetToDecided idx=%d",
			ckFailIdx, resetIdx)
	}

	// ── Assert: second RecordAttemptStart fires after ResetToDecided ──
	ras := store.callsContaining("RecordAttemptStart(step1)")
	if len(ras) != 2 {
		t.Errorf("RecordAttemptStart(step1) called %d times, want 2", len(ras))
	}
	secondRasIdx := lastIndexOf(fullLog, "RecordAttemptStart(step1)")
	if secondRasIdx >= 0 && resetIdx >= 0 && secondRasIdx < resetIdx {
		t.Errorf("second RecordAttemptStart (idx %d) before ResetToDecided (idx %d)",
			secondRasIdx, resetIdx)
	}

	// ── Assert: no DLQ entry — retry succeeded ──
	if store.dlqed {
		t.Errorf("step was incorrectly routed to DLQ (retry should have succeeded)")
	}

	t.Logf("PASS — store.calls: %v", store.calls)
	t.Logf("PASS — retry path: fail→Checkpoint(failed)@%d→ResetToDecided@%d→RecordAttemptStart@%d→done",
		ckFailIdx, resetIdx, secondRasIdx)
}

// ─── Logging proxy types ─────────────────────────────────────────────────────

// loggingStoreProxy delegates all Store calls to inner, calling onCall(op) before each.
type loggingStoreProxy struct {
	inner  *stubStore
	onCall func(string)
}

func (p *loggingStoreProxy) PendingDecision(run core.RunID) (*core.StepSpec, error) {
	p.onCall("PendingDecision")
	return p.inner.PendingDecision(run)
}
func (p *loggingStoreProxy) LoadFrontier(run core.RunID) (core.Frontier, error) {
	p.onCall("LoadFrontier")
	return p.inner.LoadFrontier(run)
}
func (p *loggingStoreProxy) PutDecision(run core.RunID, step core.StepSpec) error {
	p.onCall(fmt.Sprintf("PutDecision(%s)", step.Name))
	return p.inner.PutDecision(run, step)
}
func (p *loggingStoreProxy) Checkpoint(run core.RunID, step core.StepSpec, r core.Result) error {
	suffix := "done"
	if r.Status != "done" {
		suffix = "failed"
	}
	p.onCall(fmt.Sprintf("Checkpoint(%s:%s)", step.Name, suffix))
	return p.inner.Checkpoint(run, step, r)
}
func (p *loggingStoreProxy) RecordAttemptStart(run core.RunID, step core.StepSpec, id core.AttemptID) error {
	p.onCall(fmt.Sprintf("RecordAttemptStart(%s)", step.Name))
	return p.inner.RecordAttemptStart(run, step, id)
}
func (p *loggingStoreProxy) ResetToDecided(run core.RunID, step core.StepSpec) error {
	p.onCall(fmt.Sprintf("ResetToDecided(%s)", step.Name))
	return p.inner.ResetToDecided(run, step)
}
func (p *loggingStoreProxy) MarkDLQ(run core.RunID, step core.StepSpec, reason string, lastError string) error {
	p.onCall(fmt.Sprintf("MarkDLQ(%s)", step.Name))
	return p.inner.MarkDLQ(run, step, reason, lastError)
}
func (p *loggingStoreProxy) MarkRunDone(run core.RunID) error {
	p.onCall("MarkRunDone")
	return p.inner.MarkRunDone(run)
}
func (p *loggingStoreProxy) MarkRunFailed(run core.RunID, reason string) error {
	p.onCall("MarkRunFailed")
	return p.inner.MarkRunFailed(run, reason)
}
func (p *loggingStoreProxy) MarkPlannerFailedDLQ(run core.RunID, detail string) error {
	p.onCall("MarkPlannerFailedDLQ")
	return p.inner.MarkPlannerFailedDLQ(run, detail)
}

// loggingTransport delegates Dispatch, calling onCall before forwarding.
type loggingTransport struct {
	inner  *stubTransport
	onCall func(string)
}

func (t *loggingTransport) Dispatch(ctx context.Context, step core.StepSpec) (core.Result, error) {
	t.onCall(step.Name)
	return t.inner.Dispatch(ctx, step)
}

// ─── Utility ─────────────────────────────────────────────────────────────────

func indexOf(slice []string, target string) int {
	for i, s := range slice {
		if s == target {
			return i
		}
	}
	return -1
}

func lastIndexOf(slice []string, target string) int {
	for i := len(slice) - 1; i >= 0; i-- {
		if slice[i] == target {
			return i
		}
	}
	return -1
}
