package orchestrator_test

// Live-demo integration test for registry #1's bounded-history size guard
// (Session 19; whitepaper §12.2). Mirrors loop_integration_test.go's
// TestLoop_WireCasing_HistoryStatusUppercaseAndSeqOrdered pattern: a real
// Loop.Run, a real Postgres-backed store, and a real HTTPPlanner
// (planner_type="http", reconstructed from the workflow row — never a
// PlannerFactory override) talking to an httptest server standing in for the
// planner. This is the "fake/test planner" the session's completion
// condition calls for: the captured bytes are the genuine HTTP request body
// the orchestrator marshaled and sent over the wire, not a description of
// it. Requires TEST_DATABASE_URL; skipped otherwise.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/orchestrator"
	"github.com/aaronwu000/stateflow/internal/store"
)

// bigOutputTransport returns a worker output of the given size on its first
// dispatch (name "big"), then falls back to a tiny fixed output for any
// later step names — simulating "a throwaway worker returning a >2KB JSON
// blob" per the session's completion condition.
type bigOutputTransport struct {
	bigOutputSize int
}

func (t *bigOutputTransport) Dispatch(_ context.Context, step core.StepSpec) (core.Result, error) {
	if step.Name == "big" {
		payload := `"` + strings.Repeat("y", t.bigOutputSize-2) + `"`
		return core.Result{Status: core.ResultStatusDone, Output: json.RawMessage(payload)}, nil
	}
	return core.Result{Status: core.ResultStatusDone, Output: json.RawMessage(`{"ok":true}`)}, nil
}

// TestLoop_BoundedHistory_LargeOutputBecomesPointerOnTheWire is the
// session's mandatory live-demo evidence: a worker produces a >2KB output
// for step "big"; the next planner.Decide call's RunState.history[0].output
// must be the pointer object, not the real (large) value, and the captured
// raw JSON bytes are logged verbatim for the report.
func TestLoop_BoundedHistory_LargeOutputBecomesPointerOnTheWire(t *testing.T) {
	db := openTestDB(t)
	resetTestSchema(t, db)

	const bigOutputSize = 5000 // comfortably over the 2048-byte per-entry cap

	captured := &capturedRequest{}
	var callN int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		captured.add(raw)

		callN++
		w.Header().Set("Content-Type", "application/json")
		switch callN {
		case 1:
			w.Write([]byte(`{"status":"continue","step":{"name":"big","worker_url":"http://stub/big","mode":"sync","input":{}}}`))
		default:
			w.Write([]byte(`{"status":"done"}`))
		}
	}))
	defer srv.Close()

	const (
		wfID  = "wf-bounded-history"
		runID = "run-bounded-history"
	)
	cfg, _ := json.Marshal(map[string]string{"url": srv.URL})
	seedWorkflowAndRun(t, db, wfID, runID, "http", string(cfg))

	s := store.New(db)
	loop := &orchestrator.Loop{
		RunID:         core.RunID(runID),
		WorkflowID:    core.WorkflowID(wfID),
		WorkflowInput: json.RawMessage(`{"doc":"x"}`),
		Store:         s,
		Transport:     &bigOutputTransport{bigOutputSize: bigOutputSize},
		// PlannerFactory left nil: real HTTPPlanner reconstructed from the
		// workflow row — the genuine wire path, same as the Session 5
		// wire-casing test.
	}

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	if len(captured.raws) != 2 {
		t.Fatalf("captured %d planner requests, want 2 (initial ask, then post-\"big\" ask)", len(captured.raws))
	}

	// Call 2: history has exactly 1 entry (step "big"); its output must be
	// the truncation pointer, not the 5000-byte real value.
	call2Raw := captured.raws[1]
	t.Logf("REAL CAPTURED HTTP BODY (call 2, the request the fake/test planner actually received):\n%s", call2Raw)

	var call2 struct {
		History []struct {
			Name   string          `json:"name"`
			Status string          `json:"status"`
			Output json.RawMessage `json:"output"`
		} `json:"history"`
	}
	if err := json.Unmarshal(call2Raw, &call2); err != nil {
		t.Fatalf("unmarshal call 2 body: %v (raw: %s)", err, call2Raw)
	}
	if len(call2.History) != 1 {
		t.Fatalf("call 2: history has %d entries, want 1", len(call2.History))
	}
	entry := call2.History[0]
	if entry.Name != "big" {
		t.Fatalf("call 2: history[0].name = %q, want \"big\"", entry.Name)
	}
	if entry.Status != "DONE" {
		t.Errorf("call 2: history[0].status = %q, want \"DONE\"", entry.Status)
	}

	// The wire output must NOT be the 5000-byte real value — it must be the
	// small pointer object.
	if len(entry.Output) >= bigOutputSize {
		t.Fatalf("call 2: history[0].output is %d bytes — the real %d-byte value was sent verbatim, "+
			"the per-entry cap did not fire", len(entry.Output), bigOutputSize)
	}

	var ptr struct {
		Truncated bool   `json:"_truncated"`
		SizeBytes int    `json:"size_bytes"`
		Note      string `json:"note"`
	}
	if err := json.Unmarshal(entry.Output, &ptr); err != nil {
		t.Fatalf("call 2: history[0].output did not unmarshal as a pointer object: %v (raw: %s)", err, entry.Output)
	}
	if !ptr.Truncated {
		t.Errorf("call 2: pointer._truncated = false, want true")
	}
	if ptr.SizeBytes != bigOutputSize {
		t.Errorf("call 2: pointer.size_bytes = %d, want %d (the real marshaled size)", ptr.SizeBytes, bigOutputSize)
	}
	if !strings.Contains(ptr.Note, "GET /runs/") {
		t.Errorf("call 2: pointer.note = %q, want it to reference GET /runs/{run_id}", ptr.Note)
	}

	t.Logf("PASS — %d-byte real output was replaced on the wire with a %d-byte pointer object: %s",
		bigOutputSize, len(entry.Output), entry.Output)
}
