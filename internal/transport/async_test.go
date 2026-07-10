package transport_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/transport"
)

const (
	asyncTestRunID     = core.RunID("run-async-test")
	asyncTestStepID    = core.StepID("run-async-test:step1")
	asyncTestAttemptID = core.AttemptID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
)

// fakeAttemptStore is a minimal in-memory read-only store fake satisfying
// transport.AttemptStore. It exercises the real DeliverCallback validation
// path (no stubbing-out of the check) without needing live Postgres.
type fakeAttemptStore struct {
	mu        sync.Mutex
	frontiers map[core.RunID]core.Frontier
}

func newFakeAttemptStore() *fakeAttemptStore {
	return &fakeAttemptStore{frontiers: make(map[core.RunID]core.Frontier)}
}

// setCurrent records that run's live pending step has attempt as its
// current_attempt_id — mirroring what LoadFrontier would return from a real
// store for a step in status='RUNNING'.
func (f *fakeAttemptStore) setCurrent(run core.RunID, attempt core.AttemptID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frontiers[run] = core.Frontier{
		RunID:            run,
		PendingStep:      &core.StepSpec{Name: "step1"},
		PendingAttemptID: attempt,
	}
}

func (f *fakeAttemptStore) LoadFrontier(ctx context.Context, run core.RunID) (core.Frontier, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fr, ok := f.frontiers[run]
	if !ok {
		return core.Frontier{RunID: run}, nil // no pending step for this run
	}
	return fr, nil
}

func withAsyncMeta(ctx context.Context, step core.StepID, attempt core.AttemptID) context.Context {
	return transport.WithDispatchMeta(ctx, transport.DispatchMeta{StepID: step, AttemptID: attempt})
}

func makeAsyncStep(url string) core.StepSpec {
	return core.StepSpec{
		Name:      "step1",
		WorkerURL: url,
		Mode:      core.StepModeAsync,
		Input:     json.RawMessage(`{"doc":"test.pdf"}`),
	}
}

// ── Wire test ────────────────────────────────────────────────────────────

// TestAsyncTransport_Wire verifies the {step_id, attempt_id, input}
// envelope shape (§13.1).
func TestAsyncTransport_Wire(t *testing.T) {
	var gotBody struct {
		StepID    string          `json:"step_id"`
		AttemptID string          `json:"attempt_id"`
		Input     json.RawMessage `json:"input"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode dispatch envelope: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	store := newFakeAttemptStore()
	store.setCurrent(asyncTestRunID, asyncTestAttemptID)
	tr := transport.NewAsyncTransport(store)
	step := makeAsyncStep(srv.URL)

	go func() {
		time.Sleep(20 * time.Millisecond)
		tr.DeliverCallback(context.Background(), asyncTestStepID, asyncTestAttemptID, core.Result{
			Status: core.ResultStatusDone,
			Output: json.RawMessage(`{}`),
		})
	}()

	_, err := tr.Dispatch(withAsyncMeta(context.Background(), asyncTestStepID, asyncTestAttemptID), step)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if gotBody.StepID != string(asyncTestStepID) {
		t.Fatalf("FAIL — envelope step_id = %q, want %q", gotBody.StepID, asyncTestStepID)
	}
	if gotBody.AttemptID != string(asyncTestAttemptID) {
		t.Fatalf("FAIL — envelope attempt_id = %q, want %q", gotBody.AttemptID, asyncTestAttemptID)
	}
	if string(gotBody.Input) != `{"doc":"test.pdf"}` {
		t.Fatalf("FAIL — envelope input = %s, want the step's input", gotBody.Input)
	}
	t.Logf("PASS — async wire: envelope = {step_id:%q attempt_id:%q input:%s}", gotBody.StepID, gotBody.AttemptID, gotBody.Input)
}

// ── Outcome mapping ──────────────────────────────────────────────────────

// TestAsyncTransport_HappyPath verifies DeliverCallback(done) wakes Dispatch
// with the delivered Result.
func TestAsyncTransport_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	store := newFakeAttemptStore()
	store.setCurrent(asyncTestRunID, asyncTestAttemptID)
	tr := transport.NewAsyncTransport(store)

	go func() {
		time.Sleep(30 * time.Millisecond)
		tr.DeliverCallback(context.Background(), asyncTestStepID, asyncTestAttemptID, core.Result{
			Status: core.ResultStatusDone,
			Output: json.RawMessage(`{"text":"hello"}`),
		})
	}()

	result, err := tr.Dispatch(withAsyncMeta(context.Background(), asyncTestStepID, asyncTestAttemptID), makeAsyncStep(srv.URL))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if result.Status != core.ResultStatusDone {
		t.Fatalf("FAIL — Status = %q, want done", result.Status)
	}
	var out map[string]any
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("unmarshal Output: %v", err)
	}
	if out["text"] != "hello" {
		t.Fatalf("FAIL — Output[text] = %v, want hello", out["text"])
	}
	t.Log("PASS — 202 + DeliverCallback(done) → Dispatch returns Status=done with the delivered output")
}

// TestAsyncTransport_Silence_ReturnsError verifies that when the worker
// accepts (202) but never calls back, Dispatch returns a non-nil error once
// ctx's deadline fires — never a fabricated Result with Reason=timeout.
func TestAsyncTransport_Silence_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted) // accepts but never calls back
	}))
	defer srv.Close()

	store := newFakeAttemptStore()
	store.setCurrent(asyncTestRunID, asyncTestAttemptID)
	tr := transport.NewAsyncTransport(store)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	result, err := tr.Dispatch(withAsyncMeta(ctx, asyncTestStepID, asyncTestAttemptID), makeAsyncStep(srv.URL))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("FAIL — Dispatch returned nil error for a silent worker; result=%+v", result)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FAIL — err = %v, want context.DeadlineExceeded", err)
	}
	if result.Status != "" || result.Output != nil || result.Failure != nil {
		t.Fatalf("FAIL — result = %+v, want the zero Result alongside a non-nil error", result)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("FAIL — Dispatch took %v, want close to the 100ms deadline", elapsed)
	}
	t.Logf("PASS — silent worker: Dispatch returned a non-nil deadline error (%v) after %v, zero Result", err, elapsed.Round(time.Millisecond))
}

// TestAsyncTransport_NonAccepted_WorkerReported verifies a non-202 dispatch
// ack maps to failed(worker_reported), returned immediately without
// waiting for a callback that will never arrive.
func TestAsyncTransport_NonAccepted_WorkerReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	store := newFakeAttemptStore()
	tr := transport.NewAsyncTransport(store)

	result, err := tr.Dispatch(withAsyncMeta(context.Background(), asyncTestStepID, asyncTestAttemptID), makeAsyncStep(srv.URL))
	if err != nil {
		t.Fatalf("Dispatch returned an error (should encode failure in Result): %v", err)
	}
	if result.Status != core.ResultStatusFailed {
		t.Fatalf("FAIL — Status = %q, want failed", result.Status)
	}
	if result.Failure == nil || result.Failure.Reason != core.FailureReasonWorkerReported {
		t.Fatalf("FAIL — Failure = %+v, want Reason=worker_reported", result.Failure)
	}
	if result.Failure.HTTPStatus != http.StatusInternalServerError {
		t.Fatalf("FAIL — HTTPStatus = %d, want 500", result.Failure.HTTPStatus)
	}
	t.Logf("PASS — non-202 ack (500) → failed(worker_reported) immediately: %+v", result.Failure)
}

// TestAsyncTransport_FailCallback_WorkerReported verifies that a worker's
// explicit POST /tasks/fail (simulated directly via DeliverCallback, since
// the HTTP handler itself is Session 6's API layer) wakes Dispatch with the
// delivered failed(worker_reported) result.
func TestAsyncTransport_FailCallback_WorkerReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	store := newFakeAttemptStore()
	store.setCurrent(asyncTestRunID, asyncTestAttemptID)
	tr := transport.NewAsyncTransport(store)

	failed := core.Result{
		Status: core.ResultStatusFailed,
		Failure: &core.ResultFailure{
			Reason: core.FailureReasonWorkerReported,
			Error:  "worker ran out of memory",
		},
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		tr.DeliverCallback(context.Background(), asyncTestStepID, asyncTestAttemptID, failed)
	}()

	result, err := tr.Dispatch(withAsyncMeta(context.Background(), asyncTestStepID, asyncTestAttemptID), makeAsyncStep(srv.URL))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if result.Status != core.ResultStatusFailed {
		t.Fatalf("FAIL — Status = %q, want failed", result.Status)
	}
	if result.Failure == nil || result.Failure.Reason != core.FailureReasonWorkerReported {
		t.Fatalf("FAIL — Failure = %+v, want Reason=worker_reported", result.Failure)
	}
	t.Logf("PASS — POST /tasks/fail callback → failed(worker_reported): %+v", result.Failure)
}

// ── Registry hygiene ─────────────────────────────────────────────────────

// TestAsyncTransport_RegistryHygiene_StaleCallbackAfterTimeout verifies that
// once ctx's deadline fires and Dispatch returns, the channel is
// deregistered — a subsequently-arriving callback for that same step is a
// silent no-op (never panics, never blocks), even when the store still
// reports that attempt as current.
func TestAsyncTransport_RegistryHygiene_StaleCallbackAfterTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted) // accepts but never calls back
	}))
	defer srv.Close()

	store := newFakeAttemptStore()
	store.setCurrent(asyncTestRunID, asyncTestAttemptID)
	tr := transport.NewAsyncTransport(store)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	_, err := tr.Dispatch(withAsyncMeta(ctx, asyncTestStepID, asyncTestAttemptID), makeAsyncStep(srv.URL))
	if err == nil {
		t.Fatalf("FAIL — expected Dispatch to time out before the late callback")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// The channel was deregistered when Dispatch returned above. Even
		// though the store still reports asyncTestAttemptID as current,
		// there is no channel left to route into.
		tr.DeliverCallback(context.Background(), asyncTestStepID, asyncTestAttemptID, core.Result{
			Status: core.ResultStatusDone,
			Output: json.RawMessage(`{"late":true}`),
		})
	}()

	select {
	case <-done:
		t.Log("PASS — stale callback after deadline is a silent no-op: returned immediately, no panic")
	case <-time.After(2 * time.Second):
		t.Fatal("FAIL — DeliverCallback blocked on a callback with no registered channel")
	}
}

// TestAsyncTransport_DeliverCallback_StaleAttempt_StoreMismatch verifies the
// store-read half of the validation: even while the channel IS still
// registered (Dispatch still waiting), a callback whose attempt no longer
// matches the store's live current_attempt_id must not be routed — it
// would otherwise hand the waiting Dispatch call a stale answer.
func TestAsyncTransport_DeliverCallback_StaleAttempt_StoreMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	const newerAttempt = core.AttemptID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")

	store := newFakeAttemptStore()
	// The store says a NEWER attempt is now current — as if a retry already
	// superseded asyncTestAttemptID via TX3/TX4 in another path.
	store.setCurrent(asyncTestRunID, newerAttempt)
	tr := transport.NewAsyncTransport(store)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	type dispatchOutcome struct {
		result core.Result
		err    error
	}
	outcomeCh := make(chan dispatchOutcome, 1)
	go func() {
		result, err := tr.Dispatch(withAsyncMeta(ctx, asyncTestStepID, asyncTestAttemptID), makeAsyncStep(srv.URL))
		outcomeCh <- dispatchOutcome{result, err}
	}()

	// Give Dispatch time to register the channel and POST to the worker
	// (near-instant against an httptest server) before delivering the stale
	// callback — otherwise DeliverCallback would hit the "no channel"
	// no-op path instead of exercising the store-mismatch check.
	time.Sleep(30 * time.Millisecond)
	tr.DeliverCallback(context.Background(), asyncTestStepID, asyncTestAttemptID, core.Result{
		Status: core.ResultStatusDone,
		Output: json.RawMessage(`{"stale":true}`),
	})

	outcome := <-outcomeCh
	if outcome.err == nil {
		t.Fatalf("FAIL — expected Dispatch to time out (the stale callback must not have satisfied it); result=%+v", outcome.result)
	}
	if !errors.Is(outcome.err, context.DeadlineExceeded) {
		t.Fatalf("FAIL — err = %v, want context.DeadlineExceeded", outcome.err)
	}
	t.Logf("PASS — stale-attempt callback rejected by the store-read check: Dispatch still timed out (%v)", outcome.err)
}
