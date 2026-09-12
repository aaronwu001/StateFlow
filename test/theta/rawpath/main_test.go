// Package rawpath is milestone theta's first test group: the scenario the
// milestone exists for, asserted against database truth.
//
// SPEC.md 18's table states theta's capability in one line - "sync raw body: an
// unmodifiable HTTP endpoint works as a worker" - and demos/theta/workflow.json
// is that sentence as a run: two steps against an ordinary JSON API that has
// never heard of Piton, and one step against Piton's own envelope worker, so
// that the two dispatch styles are compared inside a single run rather than
// across two environments.
//
// It is one group in the sense of CLAUDE.md 5.5: one docker-compose environment
// is brought up for this package, every test in it runs against that
// environment, and it is torn down with a volume wipe before the next group
// starts. Groups never share a live environment (5.5.2), which the suite
// enforces by running one package at a time - see test/theta/run.sh.
//
// The fixture is created once, in TestMain, because the scenario is a single
// run. Each test then asserts one rule about the state that run left behind.
package rawpath

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/aaronwu001/piton/test/theta/harness"
)

var (
	// workflowID and runID are the fixture every test in this group reads.
	workflowID string
	runID      string

	// finalStatus is the state the run reached. It is asserted, not assumed:
	// see TestRunReachedDone.
	finalStatus string

	// specs is demos/theta/workflow.json's planner_static_steps and n is how
	// many there are, read from the file rather than written as literals so
	// that the workflow and its assertions cannot drift apart.
	specs []json.RawMessage
	n     int

	// rawParams is the `params` object of the first static step, compacted.
	// SPEC.md 9.5 makes it the entire raw body, so it is what the endpoint
	// must report having received.
	rawParams json.RawMessage
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		fmt.Println("theta/rawpath: skipped in -short mode; this group needs docker compose")
		os.Exit(0)
	}

	var err error
	if specs, err = harness.StaticSteps(harness.WorkflowHappy); err != nil {
		fmt.Fprintln(os.Stderr, "theta/rawpath:", err)
		os.Exit(1)
	}
	n = len(specs)
	if rawParams, err = harness.StepParams(harness.WorkflowHappy, 0); err != nil {
		fmt.Fprintln(os.Stderr, "theta/rawpath:", err)
		os.Exit(1)
	}

	if err = harness.Up(); err != nil {
		fmt.Fprintln(os.Stderr, "theta/rawpath: the environment did not come up:", err)
		os.Exit(1)
	}

	code := run(m)
	if err := harness.Down(); err != nil {
		fmt.Fprintln(os.Stderr, "theta/rawpath: teardown failed:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// run seeds the fixture and runs the tests, so that TestMain's teardown happens
// through one return path rather than being duplicated on every failure.
func run(m *testing.M) int {
	var err error
	workflowID, runID, finalStatus, err = harness.Seed(harness.WorkflowHappy)
	if err != nil {
		fmt.Fprintln(os.Stderr, "theta/rawpath: the fixture could not be created:", err)
		fmt.Fprintln(os.Stderr, "\nIf the run never left RUNNING or landed in DLQ, the likely reason is")
		fmt.Fprintln(os.Stderr, "simply that raw dispatch is not implemented yet: this suite is written")
		fmt.Fprintln(os.Stderr, "before the code (CLAUDE.md 4 step 3), and until it lands every attempt")
		fmt.Fprintln(os.Stderr, "against a raw step fails and the run converges to DLQ.")
		fmt.Fprintln(os.Stderr, "\n--- orchestrator logs ---")
		fmt.Fprintln(os.Stderr, harness.OrchestratorLogs(60))
		return 1
	}
	fmt.Printf("theta/rawpath: workflow_id=%s run_id=%s final=%s\n", workflowID, runID, finalStatus)
	return m.Run()
}

// rv is the psql variable every query in this group binds: the fixture's run.
func rv() string { return "run=" + runID }
