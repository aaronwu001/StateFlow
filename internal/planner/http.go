// Authoritative: whitepaper §5.1 (no DB), §5.2 (RunState body), §5.3 (StepDecision),
// §5.5 (acceptance / rejection criteria), §5.6 (timeout + retry),
// DESIGN.md §9.5 (HTTPPlanner wire contract).
package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
)

const (
	defaultPlannerTimeout  = 30 * time.Second
	defaultPlannerRetries  = 2 // 3 total attempts (initial + 2 retries)
)

// HTTPPlanner calls an external HTTP endpoint to decide the next step.
// The endpoint receives RunState JSON (§5.2) and must return StepDecision JSON (§5.3).
//
// This is the "agent-native" planner: an LLM adapter, a rules engine, or any
// custom decision service can implement the NextStepPlanner contract via HTTP.
//
// No internal state (whitepaper §5.1): each Decide call is an independent HTTP
// round-trip. No DB access. Same RunState input → same HTTP request.
//
// Retry semantics (§5.6): the planner call is side-effect-free because Barrier 1
// has not fired at Decide time — no decision is persisted and no worker is
// dispatched. Retrying a timed-out call is therefore safe: there is no risk
// of double-dispatching a worker.
type HTTPPlanner struct {
	url     string
	timeout time.Duration
	retries int // number of RETRIES; total attempts = retries + 1
	client  *http.Client
}

// httpPlannerConfig is the JSON shape stored in workflows.planner_config when
// planner_type = "http". Only url is required.
type httpPlannerConfig struct {
	URL            string `json:"url"`
	TimeoutSeconds int    `json:"timeout_seconds"` // 0 → default 30s
	MaxRetries     int    `json:"max_retries"`     // 0 → default 2 (3 total attempts)
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

	timeout := defaultPlannerTimeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	retries := defaultPlannerRetries
	if cfg.MaxRetries > 0 {
		retries = cfg.MaxRetries
	}

	return &HTTPPlanner{
		url:     cfg.URL,
		timeout: timeout,
		retries: retries,
		client:  &http.Client{},
	}, nil
}

// Decide sends state to the planner URL and returns the StepDecision.
// Retries on timeout or invalid output (§5.6). Returns error if all attempts
// fail; the loop will route the run to the DLQ with reason=planner_failed.
func (p *HTTPPlanner) Decide(ctx context.Context, state core.RunState) (core.StepDecision, error) {
	body, err := json.Marshal(state)
	if err != nil {
		return core.StepDecision{}, fmt.Errorf("HTTPPlanner: marshal RunState: %w", err)
	}

	var lastErr error
	total := p.retries + 1
	for attempt := 1; attempt <= total; attempt++ {
		decision, err := p.call(ctx, body)
		if err == nil {
			return decision, nil
		}
		lastErr = err
		// Retry is safe (Barrier 1 not yet fired; no worker dispatched).
	}
	return core.StepDecision{}, fmt.Errorf("HTTPPlanner: all %d attempt(s) failed: %w", total, lastErr)
}

// call makes a single HTTP attempt. Returns a validated StepDecision or an error.
func (p *HTTPPlanner) call(ctx context.Context, body []byte) (core.StepDecision, error) {
	reqCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return core.StepDecision{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return core.StepDecision{}, fmt.Errorf("HTTP POST: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return core.StepDecision{}, fmt.Errorf("planner returned HTTP %d (want 2xx)", resp.StatusCode)
	}

	return validateDecision(resp.Body)
}

// validateDecision implements the §5.5 acceptance / rejection criteria:
//
//   - Valid JSON (json.Unmarshal fails on syntax errors AND on trailing non-JSON content)
//   - Has status field (non-empty)
//   - status="continue" → must have step.worker_url and step.mode
//   - No prose / markdown fences (trailing non-whitespace after JSON is rejected)
//
// Any failure is returned as an error, which causes the caller to retry or fail the run.
func validateDecision(body io.Reader) (core.StepDecision, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return core.StepDecision{}, fmt.Errorf("read response body: %w", err)
	}

	// json.Unmarshal validates the entire byte slice — it rejects:
	//   - syntax errors ("bad JSON")
	//   - trailing non-whitespace content ("...}\nsome explanation")
	// This satisfies the "nothing but JSON" requirement of §5.5.
	var decision core.StepDecision
	if err := json.Unmarshal(raw, &decision); err != nil {
		return core.StepDecision{}, fmt.Errorf("planner output not valid JSON: %w", err)
	}

	if decision.Status == "" {
		return core.StepDecision{}, fmt.Errorf("planner output missing status field")
	}

	switch decision.Status {
	case "done", "fail":
		// No step required; valid as-is.

	case "continue":
		if decision.Step == nil {
			return core.StepDecision{}, fmt.Errorf("planner output: status=continue but step is absent")
		}
		if decision.Step.WorkerURL == "" {
			return core.StepDecision{}, fmt.Errorf("planner output: status=continue but step.worker_url is empty (§5.5)")
		}
		if decision.Step.Mode == "" {
			return core.StepDecision{}, fmt.Errorf("planner output: status=continue but step.mode is empty (§5.5)")
		}

	default:
		return core.StepDecision{}, fmt.Errorf("planner output: unknown status %q", decision.Status)
	}

	return decision, nil
}
