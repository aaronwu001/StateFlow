// Package readapi is the suite for the two read endpoints SPEC.md 10.2 designs
// and nothing had implemented: GET /runs and GET /runs/{run_id}/dlq.
//
// It is one group in the sense of CLAUDE.md 5.5 - one docker-compose
// environment, brought up once and torn down with a volume wipe.
//
// WHAT THE FIXTURE IS, AND WHY IT IS FOUR RUNS
//
//	These endpoints are about a COLLECTION, so a fixture of one run cannot
//	exercise them. The four below are the smallest set that makes every rule in
//	SPEC.md 10.2 observable:
//
//	  1. done        DONE           - a run with no dead-letter entry at all
//	  2. workerDLQ   DLQ            - worker-side, so step_id is set (SPEC.md 6.5)
//	  3. plannerDLQ  DLQ            - planner-side, so step_id is NULL
//	  4. replayDLQ   DLQ            - replayed while still broken, so its
//	                                  history accumulates a second entry
//
//	Runs 2 and 3 exist as a pair on purpose. Both end DLQ and look identical in
//	runs.status; the ONLY thing that says which side killed them is the
//	dead-letter reason, which is exactly what this endpoint carries and what
//	SPEC.md 12.3 makes the distinction for.
//
// Nothing here was derived by reading an implementation: at the time this file
// is written neither endpoint exists, so the whole group 404s.
package readapi

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/aaronwu001/piton/test/console/harness"
)

var (
	workflowID string

	// The fixture, in creation order - which is also the order GET /runs must
	// reverse, since SPEC.md 10.2 orders newest first.
	runDone      string
	runWorkerDLQ string
	runPlanrDLQ  string
	runReplayDLQ string

	// createdOrder is oldest first.
	createdOrder []string
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		fmt.Println("console/readapi: skipped in -short mode; this group needs docker compose")
		os.Exit(0)
	}

	if err := harness.Up(); err != nil {
		fmt.Fprintln(os.Stderr, "console/readapi: the environment did not come up:", err)
		os.Exit(1)
	}

	code := run(m)
	if err := harness.Down(); err != nil {
		fmt.Fprintln(os.Stderr, "console/readapi: teardown failed:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(m *testing.M) int {
	if err := seed(); err != nil {
		fmt.Fprintln(os.Stderr, "console/readapi: the fixture could not be created:", err)
		fmt.Fprintln(os.Stderr, "\n--- orchestrator ---")
		fmt.Fprintln(os.Stderr, harness.Logs("orchestrator", 40))
		fmt.Fprintln(os.Stderr, "\n--- planner ---")
		fmt.Fprintln(os.Stderr, harness.Logs("planner", 20))
		return 1
	}
	fmt.Printf("console/readapi: done=%s workerDLQ=%s plannerDLQ=%s replayDLQ=%s\n",
		runDone, runWorkerDLQ, runPlanrDLQ, runReplayDLQ)
	return m.Run()
}

func seed() error {
	if err := harness.WaitHealthy(harness.HealthTimeout); err != nil {
		return err
	}
	var err error
	if workflowID, err = harness.CreateWorkflow(); err != nil {
		return err
	}

	// 1. A healthy two-step run.
	if runDone, err = start(`{"steps":2,"worker":{"mode":"ok"}}`, "DONE"); err != nil {
		return fmt.Errorf("the healthy run: %w", err)
	}

	// 2. Worker-side DLQ: one step whose worker answers HTTP 500 until the
	//    budget is gone. SPEC.md 5.3 makes that transport_error.
	if runWorkerDLQ, err = start(`{"steps":1,"worker":{"mode":"http_500"}}`, "DLQ"); err != nil {
		return fmt.Errorf("the worker-side DLQ run: %w", err)
	}

	// 3. Planner-side DLQ: pause the planner so its calls expire, and the run
	//    dies before it ever has a step. SPEC.md 12.3's planner side.
	if err = harness.PausePlanner(); err != nil {
		return err
	}
	runPlanrDLQ, err = start(`{"steps":1,"worker":{"mode":"ok"}}`, "DLQ")
	if resumeErr := harness.ResumePlanner(); resumeErr != nil {
		return resumeErr
	}
	if err != nil {
		return fmt.Errorf("the planner-side DLQ run: %w", err)
	}

	// 4. A second worker-side DLQ, kept aside for the replay test so that
	//    replaying it cannot disturb any other assertion.
	if runReplayDLQ, err = start(`{"steps":1,"worker":{"mode":"http_500"}}`, "DLQ"); err != nil {
		return fmt.Errorf("the replay fixture run: %w", err)
	}

	createdOrder = []string{runDone, runWorkerDLQ, runPlanrDLQ, runReplayDLQ}
	return nil
}

// start creates a run and waits for it to reach the state the fixture needs,
// failing loudly if it reached a different one - a fixture that quietly ends up
// in another state would make every assertion below meaningless.
func start(input, want string) (string, error) {
	id, err := harness.StartRun(workflowID, input)
	if err != nil {
		return "", err
	}
	got, err := harness.WaitTerminal(id, harness.RunTimeout)
	if err != nil {
		return id, err
	}
	if got != want {
		return id, fmt.Errorf("run %s reached %q, but this fixture needs %q", id, got, want)
	}
	return id, nil
}
