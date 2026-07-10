// Authoritative: docs/StateFlow_Whitepaper_v1_0.md §6 (Timeout Doctrine),
// §7.1, §13.1 (Dispatch formats), §10.4 (single-writer principle).
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/aaronwu000/stateflow/internal/core"
)

// ─── Context helpers ─────────────────────────────────────────────────────────

// DispatchMeta carries the execution-time IDs that a transport needs but
// that are not part of the step decision (StepSpec) — they are assigned by
// the store at TX1/TX4, before Dispatch is ever called. The orchestrator
// loop injects these into the context for every Dispatch call, regardless
// of transport mode: SyncTransport uses them for the ID headers (§13.1);
// AsyncTransport uses them for both the envelope and registry key.
type DispatchMeta struct {
	StepID    core.StepID
	AttemptID core.AttemptID
}

type dispatchMetaKey struct{}

// WithDispatchMeta embeds DispatchMeta into ctx. Called by the loop:
//
//	dispatchCtx := transport.WithDispatchMeta(ctx, transport.DispatchMeta{StepID: ..., AttemptID: ...})
//	result, err := transport.Dispatch(dispatchCtx, spec)
func WithDispatchMeta(ctx context.Context, meta DispatchMeta) context.Context {
	return context.WithValue(ctx, dispatchMetaKey{}, meta)
}

func dispatchMetaFrom(ctx context.Context) (DispatchMeta, bool) {
	m, ok := ctx.Value(dispatchMetaKey{}).(DispatchMeta)
	return m, ok
}

// ─── Async dispatch body ─────────────────────────────────────────────────────

// asyncDispatchBody is the {step_id, attempt_id, input} JSON envelope
// POSTed to an async worker (§13.1). The worker must echo the ids in its
// callback so the API handler can validate and route the result.
type asyncDispatchBody struct {
	StepID    string          `json:"step_id"`
	AttemptID string          `json:"attempt_id"`
	Input     json.RawMessage `json:"input"`
}

// ─── AttemptStore ─────────────────────────────────────────────────────────────

// AttemptStore is the minimal read-only view AsyncTransport needs to
// validate an incoming callback against the live current_attempt_id before
// routing it. It is a read, never a write: only the orchestrator loop
// writes step/run state (whitepaper §10.4, the single-writer principle).
// core.StateStore satisfies this interface structurally.
type AttemptStore interface {
	LoadFrontier(ctx context.Context, run core.RunID) (core.Frontier, error)
}

// ─── AsyncTransport ──────────────────────────────────────────────────────────

// AsyncTransport implements WorkerTransport using the async callback
// pattern (§13.1, §13.2):
//
//  1. Dispatch POSTs the envelope to the worker and expects HTTP 202.
//  2. Dispatch then blocks on an in-process channel awaiting the callback.
//  3. DeliverCallback — called by the API handler (Session 6) — validates
//     the callback against the live current_attempt_id (a store read) and,
//     if still current, pushes the result into that channel; Dispatch
//     wakes and returns.
//
// Block-in-dispatch: Dispatch blocks and returns a Result (or an error),
// just like SyncTransport. The orchestrator loop is oblivious to which
// transport it is using.
//
// State writes: neither Dispatch nor DeliverCallback writes step/run state.
// Barrier 2 is written by the orchestrator loop after Dispatch returns
// (whitepaper §10.4).
//
// Channel lifetime: channels are in-process and do not survive a crash.
// After a restart, recovery re-dispatches RUNNING steps rather than
// reconnecting to a dead channel — the DB is the only cross-crash truth.
type AsyncTransport struct {
	mu       sync.Mutex
	registry map[core.StepID]chan core.Result
	client   *http.Client
	store    AttemptStore
}

// NewAsyncTransport returns a ready-to-use AsyncTransport. store is used
// only for the read-only current_attempt_id check in DeliverCallback.
func NewAsyncTransport(store AttemptStore) *AsyncTransport {
	return &AsyncTransport{
		registry: make(map[core.StepID]chan core.Result),
		client:   &http.Client{},
		store:    store,
	}
}

// Dispatch sends the {step_id, attempt_id, input} envelope to the worker
// and BLOCKS until a callback arrives or ctx is done.
//
// ctx MUST carry a DispatchMeta injected via WithDispatchMeta. Panics if
// absent — this is a programming error, not a runtime failure (the loop
// must always inject it before calling Dispatch).
//
// Timeout is NOT resolved here — ctx's deadline is set by the caller (§6).
// This transport never starts its own clock and never fabricates a
// Reason=timeout Result: when ctx fires before a callback arrives, Dispatch
// returns (Result{}, ctx.Err()) and the loop classifies it.
//
// The channel is registered BEFORE the POST fires, so a very fast worker
// callback can never arrive before the channel is ready.
func (t *AsyncTransport) Dispatch(ctx context.Context, step core.StepSpec) (core.Result, error) {
	meta, ok := dispatchMetaFrom(ctx)
	if !ok {
		panic("AsyncTransport.Dispatch: context missing DispatchMeta — call WithDispatchMeta before Dispatch")
	}

	ch := make(chan core.Result, 1) // buffered: DeliverCallback never blocks on send
	t.mu.Lock()
	t.registry[meta.StepID] = ch
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.registry, meta.StepID)
		t.mu.Unlock()
	}()

	ackFailure, err := t.postToWorker(ctx, step, meta)
	if err != nil {
		// No valid response obtained for the dispatch POST itself — dial
		// error or ctx deadline exceeded mid-request. The loop classifies
		// this as failed(timeout) (§6); this transport never does.
		return core.Result{}, err
	}
	if ackFailure != nil {
		// A valid HTTP response was obtained, just not 202 — a legitimate
		// worker_reported failure, not a "no response" case.
		return *ackFailure, nil
	}

	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return core.Result{}, ctx.Err()
	}
}

// postToWorker POSTs the async dispatch envelope.
//
// Returns (nil, nil) when the worker replied 202 (proceed to await the
// callback); (*core.Result, nil) when a valid HTTP response was obtained
// but it was not 202 — a worker_reported failure Dispatch can return
// immediately; (nil, err) when no valid response was obtained at all (dial
// error, ctx deadline mid-request) — Dispatch must propagate err as-is so
// the loop, not this transport, classifies it as timeout.
func (t *AsyncTransport) postToWorker(ctx context.Context, step core.StepSpec, meta DispatchMeta) (*core.Result, error) {
	payload, err := json.Marshal(asyncDispatchBody{
		StepID:    string(meta.StepID),
		AttemptID: string(meta.AttemptID),
		Input:     step.Input,
	})
	if err != nil {
		return nil, fmt.Errorf("AsyncTransport: marshal dispatch envelope: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, step.WorkerURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("AsyncTransport: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return &core.Result{
			Status: core.ResultStatusFailed,
			Failure: &core.ResultFailure{
				Reason:     core.FailureReasonWorkerReported,
				Error:      fmt.Sprintf("worker returned HTTP %d for dispatch (want 202 Accepted)", resp.StatusCode),
				HTTPStatus: resp.StatusCode,
			},
		}, nil
	}
	return nil, nil
}

// DeliverCallback validates an incoming worker callback and, if it is still
// live, routes the result to the waiting Dispatch call. It performs NO
// state writes (§10.4: only the loop writes step/run state).
//
// Two independent, silent no-op paths — both mean "this callback is
// superseded, ACK it with zero effect":
//   - the registry has no channel for step: Dispatch already returned
//     (timeout fired and deregistered, or a result already arrived);
//   - the store's live current_attempt_id no longer equals attempt: a
//     NEWER attempt has already replaced this one. Since the registry is
//     keyed by StepID (not AttemptID) — at most one attempt runs per step
//     at a time — routing a stale attempt's result into the channel
//     registered for the step would hand the new Dispatch call someone
//     else's answer. This is exactly why the check must read the live
//     store state, not just the registry.
//
// Safe to call concurrently. Never panics.
func (t *AsyncTransport) DeliverCallback(ctx context.Context, step core.StepID, attempt core.AttemptID, result core.Result) {
	t.mu.Lock()
	ch, ok := t.registry[step]
	t.mu.Unlock()
	if !ok {
		return
	}

	run, err := runIDFromStepID(step)
	if err != nil {
		return
	}
	frontier, err := t.store.LoadFrontier(ctx, run)
	if err != nil {
		// Cannot ascertain freshness — route nothing rather than risk
		// delivering a stale result under uncertainty.
		return
	}
	if frontier.PendingStep == nil || frontier.PendingAttemptID != attempt {
		return
	}

	select {
	case ch <- result:
	default:
		// Buffer full: Dispatch already received a result (should not
		// happen in normal single-delivery operation, but safe to ignore).
	}
}

// runIDFromStepID recovers the run id from a step id of the stored form
// "{run_id}:{step_name}" (migrations/001_initial.sql, steps.step_id).
// RunID values are always "run-{uuid}" and never contain ':', so splitting
// on the first ':' is unambiguous regardless of what a step name contains.
func runIDFromStepID(step core.StepID) (core.RunID, error) {
	s := string(step)
	idx := strings.IndexByte(s, ':')
	if idx < 0 {
		return "", fmt.Errorf("step id %q missing run-id prefix", step)
	}
	return core.RunID(s[:idx]), nil
}
