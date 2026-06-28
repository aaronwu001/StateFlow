// Authoritative: whitepaper §6.1 (async callback), §6.2 (block-in-dispatch,
// Barrier 2 ownership, channel lifetime), DESIGN.md §5 (Async Transport ↔ API
// Server Wiring), CLAUDE.md "Async Dispatch — Barrier 2 Lives in the Loop".
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
)

// ─── Context helpers ─────────────────────────────────────────────────────────

// DispatchMeta carries the execution-time IDs that AsyncTransport needs but that
// are not part of the step decision (StepSpec). The orchestrator loop injects
// these into the context after generating attemptID and before calling Dispatch.
// SyncTransport ignores them.
type DispatchMeta struct {
	StepID    core.StepID
	AttemptID core.AttemptID
}

type dispatchMetaKey struct{}

// WithDispatchMeta embeds DispatchMeta into ctx. Called by the loop:
//
//	attemptID := newAttemptID()
//	store.RecordAttemptStart(runID, spec, attemptID)
//	ctx = transport.WithDispatchMeta(ctx, transport.DispatchMeta{StepID: ..., AttemptID: attemptID})
//	result, _ = transport.Dispatch(ctx, spec)
func WithDispatchMeta(ctx context.Context, meta DispatchMeta) context.Context {
	return context.WithValue(ctx, dispatchMetaKey{}, meta)
}

func dispatchMetaFrom(ctx context.Context) (DispatchMeta, bool) {
	m, ok := ctx.Value(dispatchMetaKey{}).(DispatchMeta)
	return m, ok
}

// ─── Async dispatch body ─────────────────────────────────────────────────────

// asyncDispatchBody is the JSON body POSTed to an async worker.
// The worker must echo step_id and attempt_id in its callback so the API
// handler can validate and route the result (whitepaper §6.2, §6.6).
type asyncDispatchBody struct {
	StepID    string          `json:"step_id"`
	AttemptID string          `json:"attempt_id"`
	Input     json.RawMessage `json:"input"`
}

// ─── AsyncTransport ──────────────────────────────────────────────────────────

// AsyncTransport implements WorkerTransport using the async callback pattern
// (whitepaper §6.1, §6.2):
//
//  1. Dispatch POSTs to the worker and expects HTTP 202 ("I got it, I'll call back").
//  2. Dispatch then blocks on an in-process channel awaiting the callback result.
//  3. DeliverCallback — called by the API handler after it validates attempt_id —
//     pushes the result into that channel; Dispatch wakes and returns.
//
// Block-in-dispatch: Dispatch blocks and returns a Result, just like SyncTransport.
// The orchestrator loop is oblivious to which transport it is using.
//
// Barrier 2 (Checkpoint) is written by the orchestrator loop after Dispatch
// returns. This transport does NOT write step state. The callback handler does
// NOT write step state (CLAUDE.md, DESIGN.md §5 "Why Barrier 2 is written by
// the loop, not the callback handler").
//
// Channel lifetime: channels are in-process and do not survive a crash. After
// a restart, recovery re-dispatches RUNNING steps — it does not try to
// reconnect to a dead channel. The DB is the only cross-crash truth.
type AsyncTransport struct {
	mu       sync.Mutex
	registry map[core.StepID]chan core.Result
	client   *http.Client
}

// NewAsyncTransport returns a ready-to-use AsyncTransport.
func NewAsyncTransport() *AsyncTransport {
	return &AsyncTransport{
		registry: make(map[core.StepID]chan core.Result),
		client:   &http.Client{},
	}
}

// Dispatch sends step.Input (wrapped with step_id and attempt_id) to the
// worker and BLOCKS until a callback arrives or the timeout expires.
//
// ctx MUST carry a DispatchMeta injected via WithDispatchMeta.
// Panics if DispatchMeta is absent — this is a programming error, not a
// runtime failure (the loop must always inject it before calling Dispatch).
//
// Timeout: uses the earlier of ctx's existing deadline and step.TimeoutSeconds.
// On timeout: returns Result{Status: "failed"} — never returns a non-nil error.
//
// The channel is registered BEFORE the POST fires, so a very fast worker
// callback can never arrive before the channel is ready.
func (t *AsyncTransport) Dispatch(ctx context.Context, step core.StepSpec) (core.Result, error) {
	meta, ok := dispatchMetaFrom(ctx)
	if !ok {
		panic("AsyncTransport.Dispatch: context missing DispatchMeta — call WithDispatchMeta before Dispatch")
	}

	timeout := time.Duration(step.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Register BEFORE the POST: guarantees the channel exists if the worker
	// somehow delivers a callback extremely quickly.
	ch := make(chan core.Result, 1) // buffered: DeliverCallback never blocks on send
	t.mu.Lock()
	t.registry[meta.StepID] = ch
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.registry, meta.StepID)
		t.mu.Unlock()
	}()

	if err := t.postToWorker(ctx, step, meta); err != nil {
		return core.Result{
			Status: "failed",
			Error:  fmt.Sprintf("AsyncTransport: POST to worker: %v", err),
		}, nil
	}

	// Block: wait for DeliverCallback to push the result, or for timeout.
	select {
	case result := <-ch:
		return result, nil
	case <-ctx.Done():
		return core.Result{
			Status: "failed",
			Error:  fmt.Sprintf("AsyncTransport: timed out waiting for callback: %v", ctx.Err()),
		}, nil
	}
}

// postToWorker sends the async dispatch POST. Expects HTTP 202.
// Any non-202 response is a transport-level failure; the worker rejected the dispatch.
func (t *AsyncTransport) postToWorker(ctx context.Context, step core.StepSpec, meta DispatchMeta) error {
	payload, err := json.Marshal(asyncDispatchBody{
		StepID:    string(meta.StepID),
		AttemptID: string(meta.AttemptID),
		Input:     step.Input,
	})
	if err != nil {
		return fmt.Errorf("marshal dispatch body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, step.WorkerURL,
		bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP POST: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("worker returned HTTP %d (want 202 Accepted)", resp.StatusCode)
	}
	return nil
}

// DeliverCallback routes a worker callback result to the Dispatch call
// waiting for stepID. Called by the API handler for POST /tasks/complete and
// POST /tasks/fail, AFTER the handler has validated that attemptID matches
// the DB's current_attempt_id (CLAUDE.md: "The callback handler validates
// that the incoming attempt_id matches current_attempt_id in the DB before
// acting").
//
// The attempt_id dedup guard lives in the API handler, not here. This method
// only does channel routing. It does NOT write any step state.
//
// Orphan callbacks (step not in registry) are silently ignored — they occur
// legitimately after a crash+restart or when a timeout already fired. Never panics.
// Safe to call concurrently.
func (t *AsyncTransport) DeliverCallback(stepID core.StepID, _ core.AttemptID, result core.Result) {
	t.mu.Lock()
	ch, ok := t.registry[stepID]
	t.mu.Unlock()

	if !ok {
		// Orphan: the Dispatch goroutine no longer exists (crash, timeout, or
		// already received a result). The API handler has already returned 200
		// to the worker; nothing left to do here.
		return
	}

	// Non-blocking send into the buffered channel. If Dispatch has already
	// returned (context cancelled between registry lookup and this send), the
	// value lands in the buffer and is GC'd with the channel — no goroutine leak.
	select {
	case ch <- result:
	default:
		// Buffer full: Dispatch already received a result (should not happen in
		// normal single-delivery operation, but safe to ignore).
	}
}
