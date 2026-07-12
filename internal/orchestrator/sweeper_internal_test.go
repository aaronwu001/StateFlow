package orchestrator

// White-box unit test for startSweeperEvery's ticker/shutdown mechanics —
// deliberately package orchestrator (not orchestrator_test) so it can reach
// the unexported test seam directly, and deliberately DB-free (a minimal
// core.StateStore stub stands in for Postgres) so it runs under plain
// `go test ./...` with no TEST_DATABASE_URL, unlike every other file in this
// package. The sweeper's actual claim/skip semantics against a real
// database are covered by sweeper_test.go (package orchestrator_test); this
// file only proves the ticker fires repeatedly and stop() shuts it down
// cleanly with no goroutine left ticking after — the "wire a clean shutdown
// path" half of the Session 18 completion condition.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aaronwu000/stateflow/internal/core"
)

// tickCountingStore is a minimal core.StateStore stub: it embeds the
// (nil) interface so any method other than ListRunningRuns panics if
// called — deliberately, since this test only exercises the ticker, never
// the claim path (ListRunningRuns always reports zero RUNNING runs, so
// SweepOnce never reaches LoadFrontier or makeLoop).
type tickCountingStore struct {
	core.StateStore
	ticks atomic.Int64
}

func (s *tickCountingStore) ListRunningRuns(context.Context) ([]core.RunRef, error) {
	s.ticks.Add(1)
	return nil, nil
}

func TestStartSweeperEvery_TicksRepeatedlyAndStopsCleanly(t *testing.T) {
	st := &tickCountingStore{}
	makeLoop := func(core.RunID, core.WorkflowID, json.RawMessage) *Loop {
		t.Fatalf("makeLoop must never be called — ListRunningRuns always returns 0 runs")
		return nil
	}

	stop := startSweeperEvery(context.Background(), st, makeLoop, 10*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for st.ticks.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("sweeper only ticked %d times in 2s at a 10ms interval — ticker not firing", st.ticks.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Logf("PASS — sweeper ticked >= 3 times at a 10ms interval (observed %d)", st.ticks.Load())

	stopReturned := make(chan struct{})
	go func() {
		stop()
		close(stopReturned)
	}()
	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return within 2s — sweeper goroutine did not shut down cleanly")
	}
	t.Logf("PASS — stop() returned promptly (sweeper goroutine exited)")

	afterStop := st.ticks.Load()
	time.Sleep(50 * time.Millisecond)
	if st.ticks.Load() != afterStop {
		t.Errorf("ticks increased after stop() returned (%d -> %d) — ticker kept firing post-shutdown", afterStop, st.ticks.Load())
	}
	t.Logf("PASS — no further ticks observed after stop() returned")
}
