// Authoritative: docs/StateFlow_Whitepaper_v1_0.md §12 (the Planner
// Contract), §12.3 (acceptance criteria), §7.2 (planner failure
// classification and budget).
package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/aaronwu000/stateflow/internal/core"
)

// HTTPPlanner calls an external HTTP endpoint to decide the next step.
// The endpoint receives RunState JSON (§12.2) and must return StepDecision
// JSON (§12.3).
//
// This is the "agent-native" planner: an LLM adapter, a rules engine, or any
// custom decision service can implement the NextStepPlanner contract via HTTP.
//
// No internal state (§12.1: "the planner never talks to the database"): each
// Decide call is an independent HTTP round-trip. Same RunState input → same
// HTTP request.
//
// Retry/budget ownership (whitepaper §7.2, §20; Session 5 binding): Decide
// makes exactly ONE HTTP round trip and never retries internally. The
// orchestrator loop owns the planner budget (30s deadline per call × 3 total
// attempts) and the unreachable/malformed classification of each failed
// attempt — this mirrors the transport discipline of §6 (SyncTransport/
// AsyncTransport never resolve their own timeout either). Decide honors
// whatever deadline ctx carries; it never starts its own clock.
type HTTPPlanner struct {
	url    string
	client *http.Client
}

// httpPlannerConfig is the JSON shape stored in workflows.planner_config when
// planner_type = "http". Only url is required. (Pre-Session-5 config keys
// timeout_seconds/max_retries are no longer read: the orchestrator loop now
// owns the fixed 30s×3 planner budget uniformly, whitepaper §7.2 — see
// Session 5 report §5 for the rationale.)
type httpPlannerConfig struct {
	URL string `json:"url"`
}

// NewHTTPPlanner creates an HTTPPlanner from the JSON config bytes stored in
// the DB's planner_config column.
func NewHTTPPlanner(configJSON []byte) (*HTTPPlanner, error) {
	var cfg httpPlannerConfig
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, fmt.Errorf("HTTPPlanner: invalid config JSON: %w", err)
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("HTTPPlanner: config missing url")
	}

	return &HTTPPlanner{
		url:    cfg.URL,
		client: &http.Client{},
	}, nil
}

// MalformedError marks a Decide failure as "a response was obtained, but it
// fails the §12.3 acceptance criteria" — as opposed to no response being
// obtained at all (dial failure, ctx deadline). The orchestrator loop's
// planner budget (§7.2) uses errors.As against this type to classify each
// failed attempt into DLQReasonPlannerMalformed vs DLQReasonPlannerUnreachable
// (the fallback for any other error).
//
// A non-2xx HTTP response is classified as malformed too: some response was
// received (unlike a dial failure or timeout), it merely doesn't satisfy the
// contract — the same two-bucket forced choice a sync worker's non-2xx would
// face if there were a third "worker_reported" planner bucket, which there
// is not (whitepaper §7.2 defines only unreachable/malformed).
type MalformedError struct {
	Err error
}

func (e *MalformedError) Error() string {
	return fmt.Sprintf("planner: malformed decision: %v", e.Err)
}

func (e *MalformedError) Unwrap() error { return e.Err }

// Decide sends state to the planner URL and returns the validated
// StepDecision, or a classifiable error:
//   - *MalformedError: a response was obtained but is unusable (non-2xx,
//     invalid JSON, missing status, continue without worker_url/mode, prose
//     around the JSON — §12.3's acceptance criteria).
//   - any other error: no valid response was obtained at all (dial error,
//     ctx deadline exceeded) — the caller (the loop) classifies this as
//     unreachable.
func (p *HTTPPlanner) Decide(ctx context.Context, state core.RunState) (core.StepDecision, error) {
	body, err := json.Marshal(state)
	if err != nil {
		return core.StepDecision{}, fmt.Errorf("HTTPPlanner: marshal RunState: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return core.StepDecision{}, fmt.Errorf("HTTPPlanner: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		// No valid response obtained — dial error or ctx deadline exceeded
		// mid-request. The caller classifies this as unreachable (§7.2).
		return core.StepDecision{}, fmt.Errorf("HTTPPlanner: HTTP POST: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return core.StepDecision{}, &MalformedError{
			Err: fmt.Errorf("planner returned HTTP %d (want 2xx)", resp.StatusCode),
		}
	}

	decision, err := validateDecision(resp.Body)
	if err != nil {
		return core.StepDecision{}, &MalformedError{Err: err}
	}
	return decision, nil
}

// validateDecision implements the §12.3 acceptance / rejection criteria:
//
//   - Valid JSON (json.Unmarshal fails on syntax errors AND on trailing non-JSON content)
//   - Has status field (non-empty)
//   - status="continue" → must have step.worker_url and step.mode
//   - No prose / markdown fences (trailing non-whitespace after JSON is rejected)
//
// Any failure is returned as a plain error; the caller (Decide) wraps it in
// *MalformedError.
func validateDecision(body io.Reader) (core.StepDecision, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return core.StepDecision{}, fmt.Errorf("read response body: %w", err)
	}

	// json.Unmarshal validates the entire byte slice — it rejects:
	//   - syntax errors ("bad JSON")
	//   - trailing non-whitespace content ("...}\nsome explanation")
	// This satisfies the "nothing but JSON" requirement of §12.3.
	var decision core.StepDecision
	if err := json.Unmarshal(raw, &decision); err != nil {
		return core.StepDecision{}, fmt.Errorf("planner output not valid JSON: %w", err)
	}

	if decision.Status == "" {
		return core.StepDecision{}, fmt.Errorf("planner output missing status field")
	}

	switch decision.Status {
	case core.PlannerVerdictDone, core.PlannerVerdictFail:
		// No step required; valid as-is.

	case core.PlannerVerdictContinue:
		if decision.Step == nil {
			return core.StepDecision{}, fmt.Errorf("planner output: status=continue but step is absent")
		}
		if decision.Step.WorkerURL == "" {
			return core.StepDecision{}, fmt.Errorf("planner output: status=continue but step.worker_url is empty (§12.3)")
		}
		if decision.Step.Mode == "" {
			return core.StepDecision{}, fmt.Errorf("planner output: status=continue but step.mode is empty (§12.3)")
		}

	default:
		return core.StepDecision{}, fmt.Errorf("planner output: unknown status %q", decision.Status)
	}

	return decision, nil
}
