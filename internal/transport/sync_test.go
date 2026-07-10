package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/transport"
)

const (
	syncTestStepID    = core.StepID("run-sync-test:step1")
	syncTestAttemptID = core.AttemptID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
)

// withSyncMeta injects the standard test StepID/AttemptID — Dispatch panics
// without a DispatchMeta in ctx (it needs the ids for the wire headers).
func withSyncMeta(ctx context.Context) context.Context {
	return transport.WithDispatchMeta(ctx, transport.DispatchMeta{
		StepID:    syncTestStepID,
		AttemptID: syncTestAttemptID,
	})
}

func makeSyncStep(url, outputField string) core.StepSpec {
	return core.StepSpec{
		Name:        "test-step",
		WorkerURL:   url,
		Mode:        core.StepModeSync,
		Input:       json.RawMessage(`{"key":"value","nested":{"n":1}}`),
		OutputField: outputField,
	}
}

// ── Wire test ────────────────────────────────────────────────────────────

// TestSyncTransport_Wire verifies the zero-modification wire contract
// (§13.1): the request body is EXACTLY the planner-decided input, byte for
// byte — no wrapper — and both ID headers are present with the correct
// values.
func TestSyncTransport_Wire(t *testing.T) {
	step := makeSyncStep("", "")

	var gotBody []byte
	var gotStepIDHeader, gotAttemptIDHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		buf.ReadFrom(r.Body)
		gotBody = buf.Bytes()
		gotStepIDHeader = r.Header.Get("X-StateFlow-Step-ID")
		gotAttemptIDHeader = r.Header.Get("X-StateFlow-Attempt-ID")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	step.WorkerURL = srv.URL

	tr := transport.NewSyncTransport()
	_, err := tr.Dispatch(withSyncMeta(context.Background()), step)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if !bytes.Equal(gotBody, step.Input) {
		t.Fatalf("FAIL — request body = %s, want byte-for-byte %s", gotBody, step.Input)
	}
	if gotStepIDHeader != string(syncTestStepID) {
		t.Fatalf("FAIL — X-StateFlow-Step-ID = %q, want %q", gotStepIDHeader, syncTestStepID)
	}
	if gotAttemptIDHeader != string(syncTestAttemptID) {
		t.Fatalf("FAIL — X-StateFlow-Attempt-ID = %q, want %q", gotAttemptIDHeader, syncTestAttemptID)
	}
	t.Log("PASS — sync wire: body is the bare input byte-for-byte, both ID headers present and correct")
}

// ── Outcome mapping ──────────────────────────────────────────────────────

// TestSyncTransport_SlowServer_ReturnsError verifies that a worker exceeding
// ctx's deadline produces a non-nil error (context.DeadlineExceeded), NOT a
// Result with any Failure.Reason — §6 assigns timeout classification to the
// loop, never the transport.
func TestSyncTransport_SlowServer_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	tr := transport.NewSyncTransport()
	result, err := tr.Dispatch(withSyncMeta(ctx), makeSyncStep(srv.URL, ""))

	if err == nil {
		t.Fatalf("FAIL — Dispatch returned nil error, want a non-nil deadline error; result=%+v", result)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FAIL — err = %v, want context.DeadlineExceeded", err)
	}
	if result.Status != "" || result.Output != nil || result.Failure != nil {
		t.Fatalf("FAIL — result = %+v, want the zero Result alongside a non-nil error", result)
	}
	t.Logf("PASS — slow server: Dispatch returned a non-nil deadline error (%v), zero Result", err)
}

// TestSyncTransport_DialRefused_ReturnsError verifies that a connection
// refused (server not listening) is likewise a non-nil error, not a Result.
func TestSyncTransport_DialRefused_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close() // now nothing is listening on this URL

	tr := transport.NewSyncTransport()
	result, err := tr.Dispatch(withSyncMeta(context.Background()), makeSyncStep(deadURL, ""))

	if err == nil {
		t.Fatalf("FAIL — Dispatch returned nil error for a refused connection; result=%+v", result)
	}
	t.Logf("PASS — dial refused: Dispatch returned a non-nil error (%v)", err)
}

// TestSyncTransport_GarbageBody_Malformed verifies that a 2xx response whose
// body is not valid JSON is failed(malformed), even with no output_field
// declared.
func TestSyncTransport_GarbageBody_Malformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`not json at all {{{`))
	}))
	defer srv.Close()

	tr := transport.NewSyncTransport()
	result, err := tr.Dispatch(withSyncMeta(context.Background()), makeSyncStep(srv.URL, ""))
	if err != nil {
		t.Fatalf("Dispatch returned an error (should encode failure in Result): %v", err)
	}
	if result.Status != core.ResultStatusFailed {
		t.Fatalf("FAIL — Status = %q, want failed", result.Status)
	}
	if result.Failure == nil || result.Failure.Reason != core.FailureReasonMalformed {
		t.Fatalf("FAIL — Failure = %+v, want Reason=malformed", result.Failure)
	}
	t.Logf("PASS — 200 + invalid JSON body → failed(malformed): %+v", result.Failure)
}

// TestSyncTransport_MissingOutputField_Malformed verifies that a valid JSON
// 2xx body missing the declared output_field is failed(malformed).
func TestSyncTransport_MissingOutputField_Malformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"other_field":123}`))
	}))
	defer srv.Close()

	tr := transport.NewSyncTransport()
	result, err := tr.Dispatch(withSyncMeta(context.Background()), makeSyncStep(srv.URL, "expected_field"))
	if err != nil {
		t.Fatalf("Dispatch returned an error: %v", err)
	}
	if result.Status != core.ResultStatusFailed {
		t.Fatalf("FAIL — Status = %q, want failed", result.Status)
	}
	if result.Failure == nil || result.Failure.Reason != core.FailureReasonMalformed {
		t.Fatalf("FAIL — Failure = %+v, want Reason=malformed", result.Failure)
	}
	t.Logf("PASS — declared output_field absent → failed(malformed): %+v", result.Failure)
}

// TestSyncTransport_NonTwoXX_WorkerReported verifies that non-2xx responses
// map to failed(worker_reported) with HTTPStatus set.
func TestSyncTransport_NonTwoXX_WorkerReported(t *testing.T) {
	cases := []int{400, 404, 500, 503}
	for _, code := range cases {
		code := code
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				w.Write([]byte(`{"error":"nope"}`))
			}))
			defer srv.Close()

			tr := transport.NewSyncTransport()
			result, err := tr.Dispatch(withSyncMeta(context.Background()), makeSyncStep(srv.URL, ""))
			if err != nil {
				t.Fatalf("Dispatch returned an error: %v", err)
			}
			if result.Status != core.ResultStatusFailed {
				t.Fatalf("FAIL — HTTP %d: Status = %q, want failed", code, result.Status)
			}
			if result.Failure == nil || result.Failure.Reason != core.FailureReasonWorkerReported {
				t.Fatalf("FAIL — HTTP %d: Failure = %+v, want Reason=worker_reported", code, result.Failure)
			}
			if result.Failure.HTTPStatus != code {
				t.Fatalf("FAIL — HTTP %d: Failure.HTTPStatus = %d, want %d", code, result.Failure.HTTPStatus, code)
			}
			t.Logf("PASS — HTTP %d → failed(worker_reported), HTTPStatus=%d", code, result.Failure.HTTPStatus)
		})
	}
}

// TestSyncTransport_Success_WholeBody verifies the done path with no
// output_field: the whole valid-JSON body becomes Result.Output.
func TestSyncTransport_Success_WholeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"result":"hello","count":3}`))
	}))
	defer srv.Close()

	tr := transport.NewSyncTransport()
	result, err := tr.Dispatch(withSyncMeta(context.Background()), makeSyncStep(srv.URL, ""))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if result.Status != core.ResultStatusDone {
		t.Fatalf("FAIL — Status = %q, want done", result.Status)
	}
	var got map[string]any
	if err := json.Unmarshal(result.Output, &got); err != nil {
		t.Fatalf("unmarshal Output: %v", err)
	}
	if got["result"] != "hello" {
		t.Fatalf("FAIL — Output[result] = %v, want hello", got["result"])
	}
	t.Logf("PASS — 200 + valid JSON, no output_field → done, Output=%s", result.Output)
}

// TestSyncTransport_Success_OutputField verifies the done path with
// output_field set: only that subtree is stored as Result.Output.
func TestSyncTransport_Success_OutputField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":{"text":"extracted"},"meta":{"took_ms":1}}`))
	}))
	defer srv.Close()

	tr := transport.NewSyncTransport()
	result, err := tr.Dispatch(withSyncMeta(context.Background()), makeSyncStep(srv.URL, "data"))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if result.Status != core.ResultStatusDone {
		t.Fatalf("FAIL — Status = %q, want done", result.Status)
	}
	var got map[string]any
	if err := json.Unmarshal(result.Output, &got); err != nil {
		t.Fatalf("unmarshal Output: %v", err)
	}
	if _, hasMeta := got["meta"]; hasMeta {
		t.Fatalf("FAIL — Output contains 'meta' — output_field should isolate only 'data'")
	}
	if got["text"] != "extracted" {
		t.Fatalf("FAIL — Output[text] = %v, want extracted", got["text"])
	}
	t.Logf("PASS — output_field=data → done, Output=%s (meta excluded)", result.Output)
}
