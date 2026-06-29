package planner_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/planner"
)

// makeHTTPPlanner creates an HTTPPlanner pointed at srv.URL.
// timeoutSecs and maxRetries override defaults (0 = use default).
func makeHTTPPlanner(t *testing.T, url string, timeoutSecs, maxRetries int) *planner.HTTPPlanner {
	t.Helper()
	cfg := map[string]interface{}{"url": url}
	if timeoutSecs > 0 {
		cfg["timeout_seconds"] = timeoutSecs
	}
	if maxRetries > 0 {
		cfg["max_retries"] = maxRetries
	}
	raw, _ := json.Marshal(cfg)
	p, err := planner.NewHTTPPlanner(raw)
	if err != nil {
		t.Fatalf("NewHTTPPlanner: %v", err)
	}
	return p
}

// sampleState is a minimal RunState for test calls.
var sampleState = core.RunState{
	RunID:         "run-abc",
	WorkflowInput: json.RawMessage(`{"task":"test"}`),
	History:       []core.HistoryEntry{},
}

// TestHTTPPlanner_HappyPath_Continue verifies that a well-formed "continue"
// response is accepted and the StepDecision is returned correctly.
func TestHTTPPlanner_HappyPath_Continue(t *testing.T) {
	continueResp := `{
		"status": "continue",
		"step": {
			"name": "step1",
			"worker_url": "http://worker/run",
			"mode": "sync",
			"timeout_seconds": 30,
			"input": {"key": "value"}
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify that the body is valid RunState JSON.
		var state core.RunState
		if err := json.NewDecoder(r.Body).Decode(&state); err != nil {
			t.Errorf("request body is not valid RunState: %v", err)
		}
		if string(state.RunID) != "run-abc" {
			t.Errorf("unexpected run_id %q", state.RunID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(continueResp))
	}))
	defer srv.Close()

	p := makeHTTPPlanner(t, srv.URL, 5, 1)
	got, err := p.Decide(context.Background(), sampleState)
	if err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if got.Status != "continue" {
		t.Errorf("status: got %q, want %q", got.Status, "continue")
	}
	if got.Step == nil {
		t.Fatal("step is nil")
	}
	if got.Step.WorkerURL != "http://worker/run" {
		t.Errorf("worker_url: got %q, want %q", got.Step.WorkerURL, "http://worker/run")
	}
	if got.Step.Mode != "sync" {
		t.Errorf("mode: got %q, want %q", got.Step.Mode, "sync")
	}
}

// TestHTTPPlanner_HappyPath_Done verifies that status="done" is accepted
// without requiring a step field.
func TestHTTPPlanner_HappyPath_Done(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"done"}`))
	}))
	defer srv.Close()

	p := makeHTTPPlanner(t, srv.URL, 5, 1)
	got, err := p.Decide(context.Background(), sampleState)
	if err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if got.Status != "done" {
		t.Errorf("status: got %q, want %q", got.Status, "done")
	}
}

// TestHTTPPlanner_ValidationFailure_BadJSON verifies §5.5: output that is not
// valid JSON causes all attempts to be exhausted and an error to be returned.
func TestHTTPPlanner_ValidationFailure_BadJSON(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// Return malformed JSON (LLM wrapped output in markdown fences).
		w.Write([]byte("```json\n{\"status\":\"continue\"}\n```"))
	}))
	defer srv.Close()

	// max_retries=2 → 3 total attempts
	p := makeHTTPPlanner(t, srv.URL, 5, 2)
	_, err := p.Decide(context.Background(), sampleState)
	if err == nil {
		t.Fatal("expected error for bad JSON, got nil")
	}
	if n := int(calls.Load()); n != 3 {
		t.Errorf("expected 3 calls (1 initial + 2 retries), got %d", n)
	}
}

// TestHTTPPlanner_ValidationFailure_MissingWorkerURL verifies §5.5: a
// "continue" decision without worker_url is rejected even though it is valid JSON.
func TestHTTPPlanner_ValidationFailure_MissingWorkerURL(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// worker_url is absent — §5.5 requires it for status=continue.
		w.Write([]byte(`{"status":"continue","step":{"name":"s1","mode":"sync"}}`))
	}))
	defer srv.Close()

	// max_retries=2 → 3 total attempts
	p := makeHTTPPlanner(t, srv.URL, 5, 2)
	_, err := p.Decide(context.Background(), sampleState)
	if err == nil {
		t.Fatal("expected error for missing worker_url, got nil")
	}
	if n := int(calls.Load()); n != 3 {
		t.Errorf("expected 3 calls (1 initial + 2 retries), got %d", n)
	}
}

// TestHTTPPlanner_Timeout verifies §5.6: a slow planner triggers the timeout
// and the planner retries the specified number of times before returning error.
// Uses a short timeout (1s) and 2 total attempts to keep the test fast.
func TestHTTPPlanner_Timeout(t *testing.T) {
	var calls atomic.Int32

	// done is closed in a defer BEFORE srv.Close(), so handlers unblock before
	// the test server waits for in-flight connections to finish (LIFO defer order).
	done := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Simulate a hanging planner; unblocked either by client disconnect
		// or by the done channel when the test tears down.
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	defer srv.Close()   // runs second (LIFO) — handlers already unblocked
	defer close(done)   // runs first  (LIFO) — unblocks any live handlers

	// timeout_seconds=1, max_retries=1 → 2 total attempts × 1s ≈ 2s
	p := makeHTTPPlanner(t, srv.URL, 1, 1)

	start := time.Now()
	_, err := p.Decide(context.Background(), sampleState)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if n := int(calls.Load()); n != 2 {
		t.Errorf("expected 2 calls (1 initial + 1 retry), got %d", n)
	}
	// Sanity check: should have taken ≥1s (at least one timeout fired).
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed %v is suspiciously short — timeout may not have fired", elapsed)
	}
}

// TestHTTPPlanner_TrailingContent verifies §5.5: output that has extra non-JSON
// content after the JSON object is rejected (e.g. LLM reasoning appended).
func TestHTTPPlanner_TrailingContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Valid JSON followed by prose — must be rejected (§5.5 "nothing but the JSON").
		w.Write([]byte(`{"status":"done"} Here is my reasoning for this decision.`))
	}))
	defer srv.Close()

	p := makeHTTPPlanner(t, srv.URL, 5, 0)
	_, err := p.Decide(context.Background(), sampleState)
	if err == nil {
		t.Fatal("expected error for trailing non-JSON content, got nil")
	}
}

// TestHTTPPlanner_NonOKStatus verifies that a non-2xx HTTP response is treated
// as a planner failure and retried.
func TestHTTPPlanner_NonOKStatus(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// max_retries=2 → 3 total attempts
	p := makeHTTPPlanner(t, srv.URL, 5, 2)
	_, err := p.Decide(context.Background(), sampleState)
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
	if n := int(calls.Load()); n != 3 {
		t.Errorf("expected 3 calls, got %d", n)
	}
}
