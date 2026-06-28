package planner_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/planner"
)

// threeStepYAML is the reference YAML config used across all tests.
// Matches whitepaper §5.4 field names exactly.
var threeStepYAML = []byte(`
steps:
  - name: ocr
    worker_url: http://ocr-service/run
    mode: async
    timeout_seconds: 30
  - name: ner
    worker_url: http://ner-service/run
    mode: async
    timeout_seconds: 30
  - name: summarize
    worker_url: http://llm-proxy/summarize
    mode: sync
    timeout_seconds: 600
`)

// mustNew constructs a StaticPlanner from threeStepYAML and fails the test if
// construction fails. All tests that need a planner call this helper.
func mustNew(t *testing.T, yaml []byte) *planner.StaticPlanner {
	t.Helper()
	p, err := planner.NewStaticPlanner(yaml)
	if err != nil {
		t.Fatalf("NewStaticPlanner: %v", err)
	}
	return p
}

// ─── Test 1: 3 步 YAML 清單 + 空 history → 回第 1 步 ─────────────────────────

// TestDecide_EmptyHistory_ReturnsFirstStep verifies that when no steps have
// completed (empty history), Decide returns the first step in the YAML list
// with status "continue".
func TestDecide_EmptyHistory_ReturnsFirstStep(t *testing.T) {
	p := mustNew(t, threeStepYAML)

	state := core.RunState{
		RunID:         "run-1",
		WorkflowInput: json.RawMessage(`{"doc": "test.pdf"}`),
		History:       nil, // no steps done yet
	}

	decision, err := p.Decide(context.Background(), state)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.Status != "continue" {
		t.Errorf("Status = %q, want %q", decision.Status, "continue")
	}
	if decision.Step == nil {
		t.Fatal("Step is nil, want non-nil")
	}
	if decision.Step.Name != "ocr" {
		t.Errorf("Step.Name = %q, want %q", decision.Step.Name, "ocr")
	}
	if decision.Step.WorkerURL != "http://ocr-service/run" {
		t.Errorf("Step.WorkerURL = %q, want %q", decision.Step.WorkerURL, "http://ocr-service/run")
	}
	if decision.Step.Mode != "async" {
		t.Errorf("Step.Mode = %q, want %q", decision.Step.Mode, "async")
	}
	if decision.Step.TimeoutSeconds != 30 {
		t.Errorf("Step.TimeoutSeconds = %d, want 30", decision.Step.TimeoutSeconds)
	}

	// Verify input contains workflow_input and empty history.
	// Use semantic JSON comparison (unmarshal to map) — raw byte comparison
	// is fragile because json.Marshal compacts whitespace.
	var gotInput struct {
		WorkflowInput map[string]any  `json:"workflow_input"`
		History       []core.HistoryEntry `json:"history"`
	}
	if err := json.Unmarshal(decision.Step.Input, &gotInput); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if gotInput.WorkflowInput["doc"] != "test.pdf" {
		t.Errorf("input.workflow_input[doc] = %v, want %q", gotInput.WorkflowInput["doc"], "test.pdf")
	}
	if len(gotInput.History) != 0 {
		t.Errorf("input.history len = %d, want 0 (no completed steps yet)", len(gotInput.History))
	}

	t.Logf("PASS — empty history → step[0] %q (%s, %s, %ds)",
		decision.Step.Name, decision.Step.WorkerURL, decision.Step.Mode, decision.Step.TimeoutSeconds)
}

// ─── Test 2: 已完成 1 步 → 回第 2 步 ─────────────────────────────────────────

// TestDecide_OneHistoryEntry_ReturnsSecondStep verifies that after one step
// is in the history, Decide returns the second step in the YAML list.
// Also verifies that the completed step's output is included in the input.
func TestDecide_OneHistoryEntry_ReturnsSecondStep(t *testing.T) {
	p := mustNew(t, threeStepYAML)

	ocrOutput := json.RawMessage(`{"text": "Hello World", "pages": 2}`)

	state := core.RunState{
		RunID:         "run-2",
		WorkflowInput: json.RawMessage(`{"doc": "test.pdf"}`),
		History: []core.HistoryEntry{
			{Name: "ocr", Status: "DONE", Output: ocrOutput},
		},
	}

	decision, err := p.Decide(context.Background(), state)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.Status != "continue" {
		t.Errorf("Status = %q, want %q", decision.Status, "continue")
	}
	if decision.Step == nil {
		t.Fatal("Step is nil")
	}
	if decision.Step.Name != "ner" {
		t.Errorf("Step.Name = %q, want %q", decision.Step.Name, "ner")
	}
	if decision.Step.Mode != "async" {
		t.Errorf("Step.Mode = %q, want %q", decision.Step.Mode, "async")
	}

	// Verify input carries the ocr output in history.
	// Compare semantically (unmarshal to map) rather than raw bytes.
	var gotInput struct {
		WorkflowInput json.RawMessage `json:"workflow_input"`
		History       []struct {
			Name   string         `json:"name"`
			Status string         `json:"status"`
			Output map[string]any `json:"output"`
		} `json:"history"`
	}
	if err := json.Unmarshal(decision.Step.Input, &gotInput); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if len(gotInput.History) != 1 {
		t.Fatalf("input.history len = %d, want 1", len(gotInput.History))
	}
	if gotInput.History[0].Name != "ocr" {
		t.Errorf("input.history[0].Name = %q, want %q", gotInput.History[0].Name, "ocr")
	}
	// "pages" is decoded as float64 from JSON number
	if pages, ok := gotInput.History[0].Output["pages"].(float64); !ok || pages != 2 {
		t.Errorf("input.history[0].output[pages] = %v, want 2", gotInput.History[0].Output["pages"])
	}
	if text, _ := gotInput.History[0].Output["text"].(string); text != "Hello World" {
		t.Errorf("input.history[0].output[text] = %q, want %q", text, "Hello World")
	}

	t.Logf("PASS — 1 history entry → step[1] %q; input carries ocr output", decision.Step.Name)
}

// ─── Test 3: 已完成 3 步 → 回 status done ────────────────────────────────────

// TestDecide_AllStepsDone_ReturnsDone verifies that when all steps in the
// YAML list have completed (history length == step count), Decide returns
// status "done" with no Step field.
func TestDecide_AllStepsDone_ReturnsDone(t *testing.T) {
	p := mustNew(t, threeStepYAML)

	state := core.RunState{
		RunID:         "run-3",
		WorkflowInput: json.RawMessage(`{"doc": "test.pdf"}`),
		History: []core.HistoryEntry{
			{Name: "ocr", Status: "DONE", Output: json.RawMessage(`{"text": "hello"}`)},
			{Name: "ner", Status: "DONE", Output: json.RawMessage(`{"entities": ["Alice"]}`)},
			{Name: "summarize", Status: "DONE", Output: json.RawMessage(`{"summary": "brief"}`)},
		},
	}

	decision, err := p.Decide(context.Background(), state)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.Status != "done" {
		t.Errorf("Status = %q, want %q", decision.Status, "done")
	}
	if decision.Step != nil {
		t.Errorf("Step = %+v, want nil (status done implies no next step)", decision.Step)
	}

	t.Log("PASS — 3 history entries (all done) → status done, Step nil")
}

// ─── Test 4: 純函數性 — 同一 RunState 呼叫兩次 → 完全相同的決定 ────────────────

// TestDecide_PureFunction_SameInputSameOutput verifies that StaticPlanner is
// purely functional: calling Decide twice with identical RunState produces
// identical StepDecision. This property is required for safe crash recovery —
// the orchestrator may re-ask the planner with a reconstructed history and
// must receive the identical answer regardless of prior calls.
func TestDecide_PureFunction_SameInputSameOutput(t *testing.T) {
	p := mustNew(t, threeStepYAML)

	state := core.RunState{
		RunID:         "run-pure",
		WorkflowInput: json.RawMessage(`{"key": "value"}`),
		History: []core.HistoryEntry{
			{Name: "ocr", Status: "DONE", Output: json.RawMessage(`{"pages": 5}`)},
		},
	}

	d1, err := p.Decide(context.Background(), state)
	if err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	d2, err := p.Decide(context.Background(), state)
	if err != nil {
		t.Fatalf("second Decide: %v", err)
	}

	if d1.Status != d2.Status {
		t.Errorf("Status differs: first=%q second=%q", d1.Status, d2.Status)
	}
	if d1.Step == nil || d2.Step == nil {
		t.Fatal("both decisions must have a non-nil Step")
	}
	if d1.Step.Name != d2.Step.Name {
		t.Errorf("Step.Name differs: %q vs %q", d1.Step.Name, d2.Step.Name)
	}
	if d1.Step.WorkerURL != d2.Step.WorkerURL {
		t.Errorf("Step.WorkerURL differs: %q vs %q", d1.Step.WorkerURL, d2.Step.WorkerURL)
	}
	if d1.Step.Mode != d2.Step.Mode {
		t.Errorf("Step.Mode differs: %q vs %q", d1.Step.Mode, d2.Step.Mode)
	}
	if d1.Step.TimeoutSeconds != d2.Step.TimeoutSeconds {
		t.Errorf("Step.TimeoutSeconds differs: %d vs %d", d1.Step.TimeoutSeconds, d2.Step.TimeoutSeconds)
	}
	if string(d1.Step.Input) != string(d2.Step.Input) {
		t.Errorf("Step.Input differs:\n  first:  %s\n  second: %s", d1.Step.Input, d2.Step.Input)
	}

	t.Logf("PASS — two calls with same RunState (history len=1) both returned step %q (%s)",
		d1.Step.Name, d1.Step.Mode)
	t.Log("PASS — StaticPlanner is purely functional: internal state has no effect on output")
}

// ─── Test 5: 錯誤情況 ─────────────────────────────────────────────────────────

// TestNewStaticPlanner_Errors verifies validation errors from NewStaticPlanner.
func TestNewStaticPlanner_Errors(t *testing.T) {
	cases := []struct {
		name string
		yaml []byte
	}{
		{
			name: "empty steps list",
			yaml: []byte("steps: []"),
		},
		{
			name: "missing name",
			yaml: []byte(`steps:
  - worker_url: http://x/run
    mode: sync
    timeout_seconds: 10`),
		},
		{
			name: "missing worker_url",
			yaml: []byte(`steps:
  - name: step1
    mode: sync
    timeout_seconds: 10`),
		},
		{
			name: "invalid mode",
			yaml: []byte(`steps:
  - name: step1
    worker_url: http://x/run
    mode: batch
    timeout_seconds: 10`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := planner.NewStaticPlanner(tc.yaml)
			if err == nil {
				t.Errorf("NewStaticPlanner(%q): expected error, got nil", tc.name)
			} else {
				t.Logf("PASS — got expected error: %v", err)
			}
		})
	}
}
