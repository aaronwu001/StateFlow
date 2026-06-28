// Package transport provides WorkerTransport implementations.
// Authoritative: whitepaper §6.1 (sync hold), §6.4 (success rule + output_field).
package transport

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

// SyncTransport implements WorkerTransport using HTTP sync-hold (whitepaper §6.1):
// POST to the worker URL, hold the connection open, read the result from the
// response body. The worker needs zero modification — any ordinary HTTP endpoint
// already behaves this way.
//
// Cost: connection held open for the duration of the call; long-running workers
// risk network-infrastructure timeouts (load balancers, proxies typically cut
// idle connections after 30–90 s). Use async transport for long-running steps.
type SyncTransport struct {
	client *http.Client
}

// NewSyncTransport returns a SyncTransport ready to use.
// The per-dispatch timeout is applied via context.WithTimeout using
// step.TimeoutSeconds, so the shared http.Client has no global timeout.
func NewSyncTransport() *SyncTransport {
	return &SyncTransport{client: &http.Client{}}
}

// Dispatch POSTs step.Input (JSON) to step.WorkerURL, holds the connection
// until the response arrives or step.TimeoutSeconds elapses, then returns
// the result. It never returns a non-nil error — all transport failures are
// encoded as Result{Status: "failed"} so the loop's Barrier 2 always fires.
//
// Success rule (whitepaper §6.4): HTTP 2xx → Result.Status = "done".
// Everything else (non-2xx, connection refused, timeout) → "failed".
//
// output_field (whitepaper §6.4, sync only):
//   - step.OutputField set  → extract that key from the response JSON body.
//   - step.OutputField empty → store the full response body as Result.Output.
func (t *SyncTransport) Dispatch(ctx context.Context, step core.StepSpec) (core.Result, error) {
	timeout := time.Duration(step.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, step.WorkerURL,
		bytes.NewReader(step.Input))
	if err != nil {
		return core.Result{
			Status: "failed",
			Error:  fmt.Sprintf("SyncTransport: build request: %v", err),
		}, nil
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return core.Result{
			Status: "failed",
			Error:  fmt.Sprintf("SyncTransport: HTTP request: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return core.Result{
			Status:     "failed",
			Error:      fmt.Sprintf("SyncTransport: read body: %v", err),
			HTTPStatus: resp.StatusCode,
		}, nil
	}

	// Non-2xx → failed; include the raw response body in Error for observability.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return core.Result{
			Status:     "failed",
			Error:      fmt.Sprintf("worker returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 512)),
			HTTPStatus: resp.StatusCode,
		}, nil
	}

	// 2xx → done; extract output per output_field rule.
	output, err := extractOutput(body, step.OutputField)
	if err != nil {
		// Body is 2xx but output_field extraction failed (malformed JSON or missing key).
		return core.Result{
			Status:     "failed",
			Error:      fmt.Sprintf("SyncTransport: extract output_field %q: %v", step.OutputField, err),
			HTTPStatus: resp.StatusCode,
		}, nil
	}

	return core.Result{
		Status:     "done",
		Output:     output,
		HTTPStatus: resp.StatusCode,
	}, nil
}

// extractOutput returns the result to checkpoint:
//   - field == "" → return body as-is (entire response body).
//   - field != "" → unmarshal body as a JSON object and return body[field].
func extractOutput(body []byte, field string) (json.RawMessage, error) {
	if field == "" {
		return json.RawMessage(body), nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("unmarshal response JSON: %w", err)
	}
	v, ok := m[field]
	if !ok {
		return nil, fmt.Errorf("field %q not present in response", field)
	}
	return v, nil
}

// truncate caps a string at maxLen bytes to avoid flooding error messages
// with large response bodies.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}
