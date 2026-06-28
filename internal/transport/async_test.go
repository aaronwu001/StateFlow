package transport_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/transport"
)

const (
	testStepID         = core.StepID("run-async-test:step1")
	testAttemptIDAsync = core.AttemptID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
)

// makeAsyncStep returns a StepSpec for async mode pointing at workerURL.
func makeAsyncStep(workerURL string, timeoutSec int) core.StepSpec {
	return core.StepSpec{
		Name:           "step1",
		WorkerURL:      workerURL,
		Mode:           "async",
		TimeoutSeconds: timeoutSec,
		Input:          json.RawMessage(`{"doc":"test.pdf"}`),
	}
}

// withTestMeta injects the standard test StepID and AttemptID into ctx.
func withTestMeta(ctx context.Context) context.Context {
	return transport.WithDispatchMeta(ctx, transport.DispatchMeta{
		StepID:    testStepID,
		AttemptID: testAttemptIDAsync,
	})
}

// ─── Test 1: happy path ───────────────────────────────────────────────────────

// TestAsyncTransport_HappyPath verifies the normal async flow:
// worker returns 202 → goroutine calls DeliverCallback with a success result →
// Dispatch wakes and returns Result{Status:"done"}.
//
// Also verifies that the dispatch body sent to the worker contains step_id and
// attempt_id (whitepaper §6.2), so the worker knows what to call back with.
func TestAsyncTransport_HappyPath(t *testing.T) {
	var gotStepID, gotAttemptID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("want POST, got %s", r.Method)
		}
		var body struct {
			StepID    string          `json:"step_id"`
			AttemptID string          `json:"attempt_id"`
			Input     json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode dispatch body: %v", err)
		}
		gotStepID = body.StepID
		gotAttemptID = body.AttemptID
		w.WriteHeader(http.StatusAccepted) // 202
	}))
	defer srv.Close()

	tr := transport.NewAsyncTransport()
	step := makeAsyncStep(srv.URL, 5)

	successResult := core.Result{
		Status: "done",
		Output: json.RawMessage(`{"text":"hello","pages":3}`),
	}

	// Simulate the async worker finishing 50ms after dispatch.
	go func() {
		time.Sleep(50 * time.Millisecond)
		tr.DeliverCallback(testStepID, testAttemptIDAsync, successResult)
	}()

	result, err := tr.Dispatch(withTestMeta(context.Background()), step)

	if err != nil {
		t.Fatalf("Dispatch returned non-nil error: %v", err)
	}
	if result.Status != "done" {
		t.Errorf("Status = %q, want done", result.Status)
	}
	if result.Error != "" {
		t.Errorf("Error = %q, want empty", result.Error)
	}

	// Verify dispatch body contained the IDs so the worker can call back.
	if gotStepID != string(testStepID) {
		t.Errorf("dispatch body step_id = %q, want %q", gotStepID, testStepID)
	}
	if gotAttemptID != string(testAttemptIDAsync) {
		t.Errorf("dispatch body attempt_id = %q, want %q", gotAttemptID, testAttemptIDAsync)
	}

	var out map[string]any
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("unmarshal Output: %v", err)
	}
	if out["text"] != "hello" {
		t.Errorf("Output[text] = %v, want %q", out["text"], "hello")
	}

	t.Logf("PASS — 202 + DeliverCallback(done) → Status=done, Output=%s", result.Output)
	t.Logf("       dispatch body: step_id=%q, attempt_id=%q", gotStepID, gotAttemptID)
}

// ─── Test 2: 失敗回撥 ─────────────────────────────────────────────────────────

// TestAsyncTransport_FailedCallback verifies that DeliverCallback with a failed
// result causes Dispatch to return Result{Status:"failed"}.
// Barrier 2 still fires: the loop calls Checkpoint with this failed result
// (Checkpoint Path B — does not write output, step becomes FAILED).
func TestAsyncTransport_FailedCallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	tr := transport.NewAsyncTransport()

	failedResult := core.Result{
		Status: "failed",
		Error:  "worker ran out of memory",
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		tr.DeliverCallback(testStepID, testAttemptIDAsync, failedResult)
	}()

	result, err := tr.Dispatch(withTestMeta(context.Background()), makeAsyncStep(srv.URL, 5))

	if err != nil {
		t.Fatalf("Dispatch non-nil error: %v", err)
	}
	if result.Status != "failed" {
		t.Errorf("Status = %q, want failed", result.Status)
	}
	if result.Error != failedResult.Error {
		t.Errorf("Error = %q, want %q", result.Error, failedResult.Error)
	}

	t.Logf("PASS — DeliverCallback(failed) → Status=failed, Error=%q", result.Error)
}

// ─── Test 3: timeout ──────────────────────────────────────────────────────────

// TestAsyncTransport_Timeout verifies that when the worker accepts the dispatch
// (202) but never calls back, Dispatch returns Result{Status:"failed"} when
// the context deadline expires. The loop's Barrier 2 still fires cleanly.
func TestAsyncTransport_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted) // accepts but never calls back
	}))
	defer srv.Close()

	tr := transport.NewAsyncTransport()

	// Apply a 120ms deadline via parent context (much shorter than step timeout of 30s).
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	start := time.Now()
	result, err := tr.Dispatch(withTestMeta(ctx), makeAsyncStep(srv.URL, 30))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Dispatch non-nil error: %v", err)
	}
	if result.Status != "failed" {
		t.Errorf("Status = %q, want failed", result.Status)
	}
	if result.Error == "" {
		t.Error("Error is empty — should describe the timeout")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Dispatch took %v — timeout was not enforced (want < 500ms)", elapsed)
	}

	t.Logf("PASS — no callback → Status=failed in %v, Error=%q", elapsed.Round(time.Millisecond), result.Error)
}

// ─── Test 4: 孤兒回撥 ────────────────────────────────────────────────────────

// TestAsyncTransport_OrphanCallback verifies that DeliverCallback for an
// unregistered stepID is silently ignored — no panic, no error.
//
// This occurs in two legitimate scenarios:
//  1. Crash + restart: the old in-process channel is gone; recovery re-dispatches
//     the step with a new attempt_id. The old worker's callback arrives late.
//  2. Timeout: Dispatch already returned (Result{failed}); the worker's belated
//     callback arrives after the channel was removed from the registry.
//
// The API handler validates attempt_id against the DB and rejects stale callbacks
// before calling DeliverCallback. But even if DeliverCallback is called with a
// stale stepID, it must not panic.
func TestAsyncTransport_OrphanCallback(t *testing.T) {
	tr := transport.NewAsyncTransport()

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("DeliverCallback panicked on orphan stepID: %v", r)
		}
	}()

	tr.DeliverCallback(
		core.StepID("run-unknown:step-never-registered"),
		core.AttemptID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"),
		core.Result{Status: "done", Output: json.RawMessage(`{}`)},
	)

	t.Log("PASS — orphan DeliverCallback silently ignored, no panic")
}

// ─── Test 5: 工人拒絕 dispatch (非 202) ───────────────────────────────────────

// TestAsyncTransport_WorkerRejectsDispatch verifies that a non-202 response
// from the worker causes Dispatch to return Result{failed} immediately,
// without waiting for a callback that will never arrive.
func TestAsyncTransport_WorkerRejectsDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 500 instead of 202
	}))
	defer srv.Close()

	tr := transport.NewAsyncTransport()
	result, err := tr.Dispatch(withTestMeta(context.Background()), makeAsyncStep(srv.URL, 5))

	if err != nil {
		t.Fatalf("Dispatch non-nil error: %v", err)
	}
	if result.Status != "failed" {
		t.Errorf("Status = %q, want failed (worker returned 500 not 202)", result.Status)
	}
	if result.Error == "" {
		t.Error("Error is empty")
	}

	t.Logf("PASS — 500 (not 202) → Status=failed immediately, Error=%q", result.Error)
}

// ─── Test 6: 並行安全 ─────────────────────────────────────────────────────────

// TestAsyncTransport_ConcurrentDispatch verifies that the registry mutex
// correctly serializes concurrent access: multiple Dispatch calls for different
// steps run concurrently, each receives only its own result.
func TestAsyncTransport_ConcurrentDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	tr := transport.NewAsyncTransport()

	type callResult struct {
		stepID core.StepID
		result core.Result
	}
	done := make(chan callResult, 3)

	for i := range 3 {
		stepID := core.StepID(fmt.Sprintf("run-concurrent:step%d", i))
		attemptID := core.AttemptID(fmt.Sprintf("cccccccc-cccc-4ccc-8ccc-cc%010d", i))
		step := core.StepSpec{
			Name:           fmt.Sprintf("step%d", i),
			WorkerURL:      srv.URL,
			Mode:           "async",
			TimeoutSeconds: 5,
			Input:          json.RawMessage(`{}`),
		}
		meta := transport.DispatchMeta{StepID: stepID, AttemptID: attemptID}

		// Dispatch in goroutine.
		go func() {
			ctx := transport.WithDispatchMeta(context.Background(), meta)
			result, _ := tr.Dispatch(ctx, step)
			done <- callResult{stepID: stepID, result: result}
		}()

		// Deliver callback staggered so goroutines are truly concurrent.
		localI := i
		go func() {
			time.Sleep(time.Duration(30+localI*15) * time.Millisecond)
			tr.DeliverCallback(stepID, attemptID, core.Result{
				Status: "done",
				Output: json.RawMessage(fmt.Sprintf(`{"step":%d}`, localI)),
			})
		}()
	}

	for range 3 {
		r := <-done
		if r.result.Status != "done" {
			t.Errorf("step %q: Status = %q, want done", r.stepID, r.result.Status)
		} else {
			t.Logf("PASS — %q: Status=done, Output=%s", r.stepID, r.result.Output)
		}
	}
}
