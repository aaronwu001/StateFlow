// Package transport provides WorkerTransport implementations.
// Authoritative: docs/StateFlow_Whitepaper_v1_0.md §6 (Timeout Doctrine),
// §7.1, §13.1 (Dispatch formats).
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/aaronwu000/stateflow/internal/core"
)

// SyncTransport implements WorkerTransport via HTTP sync-hold (whitepaper
// §13.1): POST the bare input, hold the connection open, read the result
// from the response body. The worker needs zero modification.
//
// Timeout is NOT resolved here. The caller (the orchestrator loop, Session
// 5) computes the creation-anchored deadline (attempt created_at + the
// effective timeout, §6) and passes it in via context.WithDeadline; this
// transport only ever honors the incoming ctx — it never starts its own
// clock. This is what makes §6's "the clock starts at attempt creation,
// covering the pre-dispatch window" literally true in code.
type SyncTransport struct {
	client *http.Client
}

// NewSyncTransport returns a SyncTransport ready to use.
func NewSyncTransport() *SyncTransport {
	return &SyncTransport{client: &http.Client{}}
}

// Dispatch POSTs step.Input as the bare request body — no wrapper of any
// kind, sync's zero-modification promise (§13.1) — plus the
// X-StateFlow-Step-ID / X-StateFlow-Attempt-ID headers, bound to ctx's
// deadline. ctx MUST carry a DispatchMeta (see async.go); the loop injects
// it before every Dispatch call regardless of mode.
//
// Outcome mapping (§13.2 — the whole of this method's contract):
//   - 2xx + valid body (whole body, or the output_field subtree) → done.
//   - 2xx but the body is not valid JSON, or a declared output_field is
//     absent → failed(malformed).
//   - non-2xx → failed(worker_reported), HTTPStatus set.
//   - no valid response obtained at all — ctx deadline fired, dial error,
//     or a mid-flight disconnect — → (Result{}, err). This transport never
//     fabricates a Result with Reason=timeout: §6 assigns timeout
//     detection to the loop's await, not the transport.
func (t *SyncTransport) Dispatch(ctx context.Context, step core.StepSpec) (core.Result, error) {
	meta, ok := dispatchMetaFrom(ctx)
	if !ok {
		panic("SyncTransport.Dispatch: context missing DispatchMeta — call WithDispatchMeta before Dispatch")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, step.WorkerURL, bytes.NewReader(step.Input))
	if err != nil {
		return core.Result{}, fmt.Errorf("SyncTransport: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-StateFlow-Step-ID", string(meta.StepID))
	req.Header.Set("X-StateFlow-Attempt-ID", string(meta.AttemptID))

	resp, err := t.client.Do(req)
	if err != nil {
		// No valid response obtained — ctx deadline exceeded, connection
		// refused, or any other dial/transport error. The loop (not this
		// transport) classifies this as failed(timeout) (§6).
		return core.Result{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// A mid-flight disconnect while reading the body is likewise "no
		// valid response obtained".
		return core.Result{}, fmt.Errorf("SyncTransport: read body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return core.Result{
			Status: core.ResultStatusFailed,
			Failure: &core.ResultFailure{
				Reason:     core.FailureReasonWorkerReported,
				Error:      fmt.Sprintf("worker returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 512)),
				HTTPStatus: resp.StatusCode,
			},
		}, nil
	}

	output, err := extractOutput(body, step.OutputField)
	if err != nil {
		return core.Result{
			Status: core.ResultStatusFailed,
			Failure: &core.ResultFailure{
				Reason:     core.FailureReasonMalformed,
				Error:      fmt.Sprintf("SyncTransport: %v", err),
				HTTPStatus: resp.StatusCode,
			},
		}, nil
	}

	return core.Result{
		Status: core.ResultStatusDone,
		Output: output,
	}, nil
}

// extractOutput returns the result to checkpoint, and is the sole malformed
// gate (§13.2): a body that is not valid JSON is malformed regardless of
// whether an output_field is declared; a declared-but-absent output_field
// is also malformed.
//   - field == "" → the whole body, once confirmed to be valid JSON.
//   - field != "" → body[field], where body must unmarshal as a JSON object
//     and field must be present.
func extractOutput(body []byte, field string) (json.RawMessage, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("response body is not valid JSON")
	}
	if field == "" {
		return json.RawMessage(body), nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("unmarshal response JSON: %w", err)
	}
	v, ok := m[field]
	if !ok {
		return nil, fmt.Errorf("output_field %q not present in response", field)
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
