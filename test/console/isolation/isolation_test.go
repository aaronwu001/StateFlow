package isolation

import (
	"testing"

	"github.com/aaronwu001/piton/test/console/harness"
)

const healthy = `{"steps":1,"worker":{"mode":"ok"}}`

// TestPausingOneRunLeavesTheOthersAlone is the whole reason the pause switch is
// keyed by run.
//
// A single global switch works for one operator at a terminal and fails the
// moment two people watch the same demo: one presses pause and the other's run
// dies for a reason they did not cause — and it dies convincingly, with real
// `timeout` attempts and a real dead-letter entry, so nothing on screen says it
// was somebody else's doing.
//
// THE SEQUENCE, AND WHY IT IS THIS ORDER
//
//	The run's id does not exist until the run does, and the orchestrator may
//	dispatch its first attempt immediately afterwards. Pausing globally FIRST
//	closes that window: the victim's attempt is already being held when its id
//	becomes known, and the hold loop re-reads the pause state every 200 ms, so
//	narrowing the pause to that one run and then lifting the global one keeps
//	the victim held without ever letting it through.
func TestPausingOneRunLeavesTheOthersAlone(t *testing.T) {
	// Every run is held, so nothing can slip past while the victim is named.
	if err := harness.PauseWorker(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	defer func() { _ = harness.ResumeWorker() }()

	victim, err := harness.StartRun(workflowID, healthy)
	if err != nil {
		t.Fatalf("starting the victim run: %v", err)
	}

	// Narrow the pause to the victim, then lift the global one. The order
	// matters and the switches are built so that it is enough: naming the
	// victim happens BEFORE the global pause lifts, and lifting the global
	// pause does not touch per-run pauses, so there is no instant at which the
	// victim is free. An earlier version of these switches had resume clear
	// everything, which made that gap unavoidable — and this test caught it.
	if err := harness.PauseWorkerRun(victim); err != nil {
		t.Fatalf("pause victim: %v", err)
	}
	if err := harness.ResumeWorker(); err != nil {
		t.Fatalf("lift the global pause: %v", err)
	}

	bystander, err := harness.StartRun(workflowID, healthy)
	if err != nil {
		t.Fatalf("starting the bystander run: %v", err)
	}

	// The bystander must finish normally. This is the assertion that fails if
	// the switch is global.
	if got, err := harness.WaitTerminal(bystander, harness.RunTimeout); err != nil || got != "DONE" {
		t.Fatalf("SPEC.md 5.1: a run whose worker is answering reaches DONE; "+
			"the bystander reached %q (%v).\nPausing one run must not touch another.", got, err)
	}

	// And the victim must die, for the reason the pause actually causes.
	if got, err := harness.WaitTerminal(victim, harness.RunTimeout); err != nil || got != "DLQ" {
		t.Fatalf("SPEC.md 12.2: a step that exhausts its budget sends its run to DLQ; "+
			"the victim reached %q (%v)", got, err)
	}

	harness.Bool(t, "SPEC.md 5.3: a worker that accepts the connection and answers nothing "+
		"expires at deadline_at, which is `timeout` and not `transport_error`",
		"SELECT bool_and(failure_reason = 'timeout') FROM attempts WHERE run_id = :'run';",
		"run="+victim)
	harness.Bool(t, "SPEC.md 12.3: it is a worker-side dead-letter entry",
		"SELECT count(*) = 1 FROM dead_letter_queue"+
			" WHERE run_id = :'run' AND reason = 'worker_budget_exhausted';",
		"run="+victim)
	harness.Bool(t, "SPEC.md 6.5: the bystander stopped for no reason at all",
		"SELECT count(*) = 0 FROM dead_letter_queue WHERE run_id = :'run';",
		"run="+bystander)
}

// TestPausingOnePlannerRunLeavesTheOthersAlone is the same claim on the planner
// side, which fails differently: the run dies before it has any step at all, so
// the dead-letter entry is planner-side and carries no step_id (SPEC.md 6.5).
func TestPausingOnePlannerRunLeavesTheOthersAlone(t *testing.T) {
	if err := harness.PausePlanner(); err != nil {
		t.Fatalf("pause planner: %v", err)
	}
	defer func() { _ = harness.ResumePlanner() }()

	victim, err := harness.StartRun(workflowID, healthy)
	if err != nil {
		t.Fatalf("starting the victim run: %v", err)
	}
	if err := harness.PausePlannerRun(victim); err != nil {
		t.Fatalf("pause victim: %v", err)
	}
	if err := harness.ResumePlanner(); err != nil {
		t.Fatalf("lift the global pause: %v", err)
	}

	bystander, err := harness.StartRun(workflowID, healthy)
	if err != nil {
		t.Fatalf("starting the bystander run: %v", err)
	}

	if got, err := harness.WaitTerminal(bystander, harness.RunTimeout); err != nil || got != "DONE" {
		t.Fatalf("SPEC.md 5.1: a run whose planner is answering reaches DONE; "+
			"the bystander reached %q (%v)", got, err)
	}
	if got, err := harness.WaitTerminal(victim, harness.RunTimeout); err != nil || got != "DLQ" {
		t.Fatalf("SPEC.md 12.2: a run that exhausts planner_max_attempts goes to DLQ; "+
			"the victim reached %q (%v)", got, err)
	}

	harness.Bool(t, "SPEC.md 12.3: a planner that never answered is a planner-side entry",
		"SELECT count(*) = 1 FROM dead_letter_queue"+
			" WHERE run_id = :'run' AND reason = 'planner_budget_exhausted';",
		"run="+victim)
	harness.Bool(t, "SPEC.md 6.5: step_id is NULL, because no step was ever created",
		"SELECT step_id IS NULL FROM dead_letter_queue WHERE run_id = :'run';",
		"run="+victim)
	harness.Bool(t, "SPEC.md 6.3: the victim never got as far as having a step",
		"SELECT count(*) = 0 FROM steps WHERE run_id = :'run';", "run="+victim)
}
