package transport_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/transport"
)

// makeStep returns a minimal StepSpec pointing at the given URL.
func makeStep(url string, outputField string, timeoutSec int) core.StepSpec {
	return core.StepSpec{
		Name:           "test-step",
		WorkerURL:      url,
		Mode:           "sync",
		TimeoutSeconds: timeoutSec,
		Input:          json.RawMessage(`{"key":"value"}`),
		OutputField:    outputField,
	}
}

// ─── Test 1: 成功 — 200 回應,無 output_field ─────────────────────────────────

// TestSyncTransport_Success verifies that a 200 response is classified as
// Result.Status="done" and the full response body becomes Result.Output.
func TestSyncTransport_Success(t *testing.T) {
	responseBody := `{"result":"hello","count":3}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the transport sends the right method and Content-Type.
		if r.Method != http.MethodPost {
			t.Errorf("want POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(responseBody))
	}))
	defer srv.Close()

	tr := transport.NewSyncTransport()
	result, err := tr.Dispatch(context.Background(), makeStep(srv.URL, "", 5))

	if err != nil {
		t.Fatalf("Dispatch returned non-nil error: %v", err)
	}
	if result.Status != "done" {
		t.Errorf("Status = %q, want %q", result.Status, "done")
	}
	if result.HTTPStatus != 200 {
		t.Errorf("HTTPStatus = %d, want 200", result.HTTPStatus)
	}
	if result.Error != "" {
		t.Errorf("Error = %q, want empty", result.Error)
	}

	// Full body should be Result.Output (no output_field set).
	var got map[string]any
	if err := json.Unmarshal(result.Output, &got); err != nil {
		t.Fatalf("unmarshal Output: %v", err)
	}
	if got["result"] != "hello" {
		t.Errorf("Output[result] = %v, want %q", got["result"], "hello")
	}
	if got["count"] != float64(3) {
		t.Errorf("Output[count] = %v, want 3", got["count"])
	}

	t.Logf("PASS — 200 → Status=done, HTTPStatus=200, Output=%s", result.Output)
}

// ─── Test 2: output_field 抽取 ────────────────────────────────────────────────

// TestSyncTransport_OutputField verifies that when step.OutputField is set to
// "data", only the "data" sub-object from the response body is stored as
// Result.Output — the "meta" key is not included.
func TestSyncTransport_OutputField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Response has two top-level keys; only "data" should be forwarded.
		w.Write([]byte(`{"data":{"text":"extracted","pages":5},"meta":{"took_ms":123}}`))
	}))
	defer srv.Close()

	tr := transport.NewSyncTransport()
	result, err := tr.Dispatch(context.Background(), makeStep(srv.URL, "data", 5))

	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if result.Status != "done" {
		t.Errorf("Status = %q, want done", result.Status)
	}

	var got map[string]any
	if err := json.Unmarshal(result.Output, &got); err != nil {
		t.Fatalf("unmarshal Output: %v", err)
	}
	if _, hasMeta := got["meta"]; hasMeta {
		t.Errorf("Output contains 'meta' key — should only contain the 'data' sub-object")
	}
	if got["text"] != "extracted" {
		t.Errorf("Output[text] = %v, want %q", got["text"], "extracted")
	}
	if got["pages"] != float64(5) {
		t.Errorf("Output[pages] = %v, want 5", got["pages"])
	}

	t.Logf("PASS — output_field=data: Output=%s (meta excluded)", result.Output)
}

// ─── Test 3: 500 失敗 ────────────────────────────────────────────────────────

// TestSyncTransport_ServerError verifies that an HTTP 500 response is
// classified as Result.Status="failed", HTTPStatus=500, and Error is non-empty.
func TestSyncTransport_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"database connection lost"}`))
	}))
	defer srv.Close()

	tr := transport.NewSyncTransport()
	result, err := tr.Dispatch(context.Background(), makeStep(srv.URL, "", 5))

	if err != nil {
		t.Fatalf("Dispatch returned non-nil error (should encode failure in Result): %v", err)
	}
	if result.Status != "failed" {
		t.Errorf("Status = %q, want %q", result.Status, "failed")
	}
	if result.HTTPStatus != 500 {
		t.Errorf("HTTPStatus = %d, want 500", result.HTTPStatus)
	}
	if result.Error == "" {
		t.Error("Error is empty — should contain the failure message")
	}
	if !strings.Contains(result.Error, "500") {
		t.Errorf("Error %q does not mention HTTP status 500", result.Error)
	}
	if result.Output != nil {
		t.Errorf("Output = %s, want nil on failure", result.Output)
	}

	t.Logf("PASS — 500 → Status=failed, HTTPStatus=500, Error=%q", result.Error)
}

// ─── Test 4: timeout ──────────────────────────────────────────────────────────

// TestSyncTransport_Timeout verifies that when the worker takes longer than
// step.TimeoutSeconds, Dispatch returns Result.Status="failed" (not a panic or
// non-nil error). The loop's Barrier 2 must still fire cleanly.
func TestSyncTransport_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep longer than the transport timeout.
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"result":"too late"}`))
	}))
	defer srv.Close()

	tr := transport.NewSyncTransport()

	start := time.Now()
	// TimeoutSeconds=0 is treated as the 30s default — too long for a test.
	// Use a very short timeout via a custom step with TimeoutSeconds set to
	// something small. Since TimeoutSeconds is int (seconds), the minimum is 1s.
	// Instead we apply a 100ms deadline via the parent context.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	step := makeStep(srv.URL, "", 30) // step timeout is 30s, but ctx deadline is 100ms
	result, err := tr.Dispatch(ctx, step)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Dispatch returned non-nil error: %v", err)
	}
	if result.Status != "failed" {
		t.Errorf("Status = %q, want %q", result.Status, "failed")
	}
	if result.Error == "" {
		t.Error("Error is empty — should describe the timeout")
	}
	// Should complete well before the worker's 500ms sleep.
	if elapsed > 400*time.Millisecond {
		t.Errorf("Dispatch took %v — timeout was not enforced", elapsed)
	}

	t.Logf("PASS — timeout: Status=failed in %v, Error=%q", elapsed.Round(time.Millisecond), result.Error)
}

// ─── Test 5: 非 2xx 各種狀態碼 ───────────────────────────────────────────────

// TestSyncTransport_Various4xx verifies that 4xx codes also map to "failed".
func TestSyncTransport_Various4xx(t *testing.T) {
	cases := []int{400, 401, 403, 404, 422, 429}

	for _, code := range cases {
		code := code
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				w.Write([]byte(`{"error":"client problem"}`))
			}))
			defer srv.Close()

			tr := transport.NewSyncTransport()
			result, err := tr.Dispatch(context.Background(), makeStep(srv.URL, "", 5))
			if err != nil {
				t.Fatalf("non-nil error: %v", err)
			}
			if result.Status != "failed" {
				t.Errorf("HTTP %d → Status=%q, want failed", code, result.Status)
			}
			if result.HTTPStatus != code {
				t.Errorf("HTTPStatus=%d, want %d", result.HTTPStatus, code)
			}
			t.Logf("PASS — HTTP %d → Status=failed, HTTPStatus=%d", code, result.HTTPStatus)
		})
	}
}
