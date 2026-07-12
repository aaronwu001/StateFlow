// Package orchestrator — the in-process periodic orphan sweeper (Temporary
// Design Registry item #4, whitepaper §18).
//
// Scope (confirmed against loop.go's own doc comment on Run, and the Phase 2
// "Design decisions log"): RecoverRuns (recovery.go) already handles the
// whole-orchestrator-process-crash case, once, at startup, before the HTTP
// server accepts requests. This file handles the DIFFERENT case where a
// single run's driving goroutine dies while the orchestrator process itself
// stays alive — most commonly a transient Postgres outage causing Run() or
// ResumeReplayedStep() to return a genuine fault (a store read/write
// failure, ctx cancellation, or an internal invariant violation — see
// loop.go:100-101's doc comment for the precise, binding definition of what
// counts as "the goroutine died"). Before this file existed, nothing ever
// relaunched that goroutine short of a full manual orchestrator restart.
//
// The fix: run the exact same scan/claim/launch action RecoverRuns uses
// (resumeOneRun, recovery.go), on a fixed interval, for the life of the
// process — filtered so a run that already HAS a live goroutine driving it
// (the common case: nearly every RUNNING run, nearly all the time) is never
// touched by a concurrent tick.
package orchestrator

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
)

// defaultSweepInterval is the sweep interval used when the caller doesn't
// supply an override. Session 18 introduced it as a fixed, non-configurable
// constant; Session 21 (registry #6, config-driven assembly) added
// StartSweeperWithInterval so cmd/stateflow/main.go can override it via
// SWEEP_INTERVAL_SECONDS — this constant remains the "widely acceptable
// default" both StartSweeper and an unset/invalid override fall back to, so
// zero new env vars reproduces this exact 30s value.
const defaultSweepInterval = 30 * time.Second

// ── The live-run registry ───────────────────────────────────────────────

// liveRuns tracks which run_ids currently have a goroutine actively inside
// Loop.Run or Loop.ResumeReplayedStep — the two (and, by construction, only)
// entry points that drive a run's steps end to end. Marking happens inside
// those two methods themselves (see loop.go's Run and replay.go's
// ResumeReplayedStep), NOT at each call site that launches a goroutine —
// this is a deliberate deviation from doing it at the "startLoop" launch
// site, made because one of the three current launch sites
// (internal/api/server.go's handleDLQReplay worker-side branch, which
// constructs an orchestrator.Loop directly and calls
// `go l.ResumeReplayedStep(ctx)`) is outside this session's scope to edit.
// Registering inside the shared entry points instead covers ALL launch
// sites — main.go's startLoop closure, RecoverRuns' startup goroutines, this
// file's own sweep-launched goroutines, and server.go's direct call —
// automatically and uniformly, with zero wiring required at any call site
// now or in the future. See the Session 18 report for the full reasoning.
//
// This is intentionally process-local, in-memory, singleton state: by the
// MVP's own "one run has exactly one loop goroutine" concurrency invariant
// (whitepaper), at most one goroutine is ever live for a given run_id at a
// time, so a simple set (not a refcount) is sufficient. It does not need to
// survive a process restart — ListRunningRuns + RecoverRuns already cover
// that case (whitepaper §8.3).
var liveRuns = newLiveRunSet()

type liveRunSet struct {
	mu sync.Mutex
	m  map[core.RunID]struct{}
}

func newLiveRunSet() *liveRunSet {
	return &liveRunSet{m: make(map[core.RunID]struct{})}
}

func (s *liveRunSet) mark(id core.RunID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = struct{}{}
}

func (s *liveRunSet) unmark(id core.RunID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

func (s *liveRunSet) isLive(id core.RunID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[id]
	return ok
}

// ── The sweep tick ──────────────────────────────────────────────────────

// SweepOnce performs exactly one sweep pass: list all RUNNING runs, skip any
// with a currently-live goroutine (liveRuns.isLive), and resume the rest via
// resumeOneRun — the identical action RecoverRuns takes at startup. Exported
// so it can be driven directly and deterministically by tests (and, if ever
// useful operationally, by an admin trigger) instead of only through
// StartSweeper's real-time ticker.
//
// Returns the number of runs actually resumed (a goroutine launched) and any
// error listing RUNNING runs. Per-run errors (e.g. a LoadFrontier failure
// inside resumeOneRun) are logged, not returned — consistent with
// RecoverRuns: runs are independent, one run's problem must not block the
// sweep from continuing to the next (whitepaper §8.3, "crash loops converge"
// per run, not globally).
func SweepOnce(
	ctx context.Context,
	store core.StateStore,
	makeLoop func(runID core.RunID, workflowID core.WorkflowID, workflowInput json.RawMessage) *Loop,
) (int, error) {
	runs, err := store.ListRunningRuns(ctx)
	if err != nil {
		return 0, err
	}

	claimed := 0
	for _, r := range runs {
		if liveRuns.isLive(r.RunID) {
			// A goroutine is already inside Run()/ResumeReplayedStep() for
			// this run — touching it here would race that goroutine's own
			// TX2/TX3 checkpoint with a spurious concurrent orphan claim.
			// Skip it; that goroutine owns this run's progress.
			continue
		}
		if resumeOneRun(ctx, store, r, makeLoop, "[SWEEP]") {
			claimed++
		}
	}
	return claimed, nil
}

// ── The periodic ticker ─────────────────────────────────────────────────

// StartSweeper launches a goroutine that calls SweepOnce every
// defaultSweepInterval (30s, fixed — see that const's doc comment) for the
// life of the process. It is the long-lived counterpart to RecoverRuns'
// one-time startup scan (registry #4, whitepaper §18's Temporary Design
// Registry: "Storage orphans wait for a restart" — this closes that gap).
//
// Returns a stop function: calling it cancels the sweeper's internal ticker
// and blocks until its goroutine has fully exited (no in-flight tick is
// abandoned mid-log), giving the caller (cmd/stateflow/main.go) a clean
// shutdown path. stop is safe to call at most once; a second call is a
// no-op panic-free close of an already-canceled context (context.CancelFunc
// semantics).
//
// Kept as a thin wrapper around StartSweeperWithInterval so any existing
// caller/test that wants "the documented default, no questions asked" has
// one call with no interval argument to reason about.
func StartSweeper(
	ctx context.Context,
	store core.StateStore,
	makeLoop func(runID core.RunID, workflowID core.WorkflowID, workflowInput json.RawMessage) *Loop,
) (stop func()) {
	return startSweeperEvery(ctx, store, makeLoop, defaultSweepInterval)
}

// StartSweeperWithInterval is StartSweeper generalized to a caller-supplied
// interval (Session 21, registry #6 — config-driven assembly). It exists so
// cmd/stateflow/main.go can honor a SWEEP_INTERVAL_SECONDS override without
// duplicating the ticker/shutdown mechanics that already live in
// startSweeperEvery (Session 18 explicitly warns against reimplementing the
// scan/claim logic a second time; the same reasoning applies to the ticker
// wrapper around it).
//
// interval <= 0 is treated as "use the documented default"
// (defaultSweepInterval, 30s) rather than a caller error — this keeps a
// zero-value/unset caller-side variable behaviorally identical to calling
// StartSweeper, satisfying registry #6's "zero new config ⇒ identical
// behavior" principle without forcing every caller to resolve the default
// itself.
func StartSweeperWithInterval(
	ctx context.Context,
	store core.StateStore,
	makeLoop func(runID core.RunID, workflowID core.WorkflowID, workflowInput json.RawMessage) *Loop,
	interval time.Duration,
) (stop func()) {
	if interval <= 0 {
		interval = defaultSweepInterval
	}
	return startSweeperEvery(ctx, store, makeLoop, interval)
}

// startSweeperEvery is StartSweeper's/StartSweeperWithInterval's actual
// implementation, parameterized by interval so tests can drive many ticks
// quickly rather than waiting on the real 30s default.
func startSweeperEvery(
	ctx context.Context,
	store core.StateStore,
	makeLoop func(runID core.RunID, workflowID core.WorkflowID, workflowInput json.RawMessage) *Loop,
	interval time.Duration,
) (stop func()) {
	sweepCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		slog.Info("[SWEEP] sweeper started", "interval", interval.String())
		for {
			select {
			case <-sweepCtx.Done():
				slog.Info("[SWEEP] sweeper stopped")
				return
			case <-ticker.C:
				claimed, err := SweepOnce(sweepCtx, store, makeLoop)
				if err != nil {
					slog.Error("[SWEEP] tick failed", "err", err)
					continue
				}
				if claimed > 0 {
					slog.Info("[SWEEP] tick complete", "claimed", claimed)
				}
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}
