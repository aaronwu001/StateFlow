package planner_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/planner"
)

// makeHTTPPlanner creates an HTTPPlanner pointed at url.
func makeHTTPPlanner(t *testing.T, url string) *planner.HTTPPlanner {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"url": url})
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

	p := makeHTTPPlanner(t, srv.URL)
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

	p := makeHTTPPlanner(t, srv.URL)
	got, err := p.Decide(context.Background(), sampleState)
	if err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if got.Status != "done" {
		t.Errorf("status: got %q, want %q", got.Status, "done")
	}
}

// TestHTTPPlanner_SingleAttempt_NoInternalRetry verifies the Session 5
// binding contract: Decide makes exactly ONE HTTP round trip per call. The
// orchestrator loop, not HTTPPlanner, owns the 30s×3 planner budget (§7.2) —
// a planner package that retried internally would make the loop's per-attempt
// classification (unreachable vs malformed) and per-attempt detail
// impossible to construct.
func TestHTTPPlanner_SingleAttempt_NoInternalRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := makeHTTPPlanner(t, srv.URL)
	_, err := p.Decide(context.Background(), sampleState)
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
	if n := int(calls.Load()); n != 1 {
		t.Errorf("expected exactly 1 call (no internal retry), got %d", n)
	}
}

// TestHTTPPlanner_ValidationFailure_BadJSON_IsMalformed verifies §12.3:
// output that is not valid JSON is rejected as *planner.MalformedError, the
// type the loop's budget classifier keys on.
func TestHTTPPlanner_ValidationFailure_BadJSON_IsMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return malformed JSON (LLM wrapped output in markdown fences).
		w.Write([]byte("```json\n{\"status\":\"continue\"}\n```"))
	}))
	defer srv.Close()

	p := makeHTTPPlanner(t, srv.URL)
	_, err := p.Decide(context.Background(), sampleState)
	if err == nil {
		t.Fatal("expected error for bad JSON, got nil")
	}
	var malformed *planner.MalformedError
	if !errors.As(err, &malformed) {
		t.Errorf("error is not *planner.MalformedError: %v (%T)", err, err)
	}
}

// TestHTTPPlanner_ValidationFailure_MissingWorkerURL_IsMalformed verifies
// §12.3: a "continue" decision without worker_url is rejected even though it
// is valid JSON, and classified as malformed.
func TestHTTPPlanner_ValidationFailure_MissingWorkerURL_IsMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// worker_url is absent — §12.3 requires it for status=continue.
		w.Write([]byte(`{"status":"continue","step":{"name":"s1","mode":"sync"}}`))
	}))
	defer srv.Close()

	p := makeHTTPPlanner(t, srv.URL)
	_, err := p.Decide(context.Background(), sampleState)
	if err == nil {
		t.Fatal("expected error for missing worker_url, got nil")
	}
	var malformed *planner.MalformedError
	if !errors.As(err, &malformed) {
		t.Errorf("error is not *planner.MalformedError: %v (%T)", err, err)
	}
}

// TestHTTPPlanner_Timeout_IsNotMalformed verifies §6/§7.2: a slow planner
// causes Decide to honor ctx's deadline and return a NON-MalformedError
// error (the caller must classify this as unreachable, not malformed).
// Decide never starts its own clock — the ctx deadline is the only bound.
func TestHTTPPlanner_Timeout_IsNotMalformed(t *testing.T) {
	var calls atomic.Int32

	// done is closed in a defer BEFORE srv.Close(), so handlers unblock before
	// the test server waits for in-flight connections to finish (LIFO defer order).
	done := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	defer srv.Close()
	defer close(done)

	p := makeHTTPPlanner(t, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := p.Decide(ctx, sampleState)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	var malformed *planner.MalformedError
	if errors.As(err, &malformed) {
		t.Errorf("timeout was classified as *planner.MalformedError; want a plain (unreachable) error: %v", err)
	}
	if n := int(calls.Load()); n != 1 {
		t.Errorf("expected exactly 1 call (Decide never retries internally), got %d", n)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("elapsed %v is suspiciously short — ctx deadline may not have been honored", elapsed)
	}
}

// TestHTTPPlanner_TrailingContent_IsMalformed verifies §12.3: output that has
// extra non-JSON content after the JSON object is rejected (e.g. LLM
// reasoning appended), classified as malformed.
func TestHTTPPlanner_TrailingContent_IsMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Valid JSON followed by prose — must be rejected (§12.3 "nothing but the JSON").
		w.Write([]byte(`{"status":"done"} Here is my reasoning for this decision.`))
	}))
	defer srv.Close()

	p := makeHTTPPlanner(t, srv.URL)
	_, err := p.Decide(context.Background(), sampleState)
	if err == nil {
		t.Fatal("expected error for trailing non-JSON content, got nil")
	}
	var malformed *planner.MalformedError
	if !errors.As(err, &malformed) {
		t.Errorf("error is not *planner.MalformedError: %v (%T)", err, err)
	}
}

// TestHTTPPlanner_NonOKStatus_IsMalformed verifies that a non-2xx HTTP
// response is classified as malformed (a response WAS obtained, unlike a
// dial failure or timeout) — see the doc comment on MalformedError.
func TestHTTPPlanner_NonOKStatus_IsMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := makeHTTPPlanner(t, srv.URL)
	_, err := p.Decide(context.Background(), sampleState)
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
	var malformed *planner.MalformedError
	if !errors.As(err, &malformed) {
		t.Errorf("error is not *planner.MalformedError: %v (%T)", err, err)
	}
}

// TestHTTPPlanner_DialFailure_IsNotMalformed verifies that a connection
// failure (no server listening) is NOT classified as malformed — the caller
// must treat it as unreachable.
func TestHTTPPlanner_DialFailure_IsNotMalformed(t *testing.T) {
	p := makeHTTPPlanner(t, "http://127.0.0.1:1") // reserved, nothing listens here
	_, err := p.Decide(context.Background(), sampleState)
	if err == nil {
		t.Fatal("expected dial error, got nil")
	}
	var malformed *planner.MalformedError
	if errors.As(err, &malformed) {
		t.Errorf("dial failure was classified as *planner.MalformedError; want a plain (unreachable) error: %v", err)
	}
}
