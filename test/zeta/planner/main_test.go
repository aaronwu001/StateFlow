// Package planner is milestone zeta's test group: the planner stops being a
// list fixed at submission time and becomes an external HTTP service that is
// asked, synchronously, what to do next.
//
// It is one group in the sense of CLAUDE.md 5.5: one docker-compose environment
// is brought up for this package, every test in it runs against that
// environment, and it is torn down with a volume wipe before the next group
// starts.
//
// WHY EVERY LEG IS ONE GROUP AND NOT EIGHT
//
//	CLAUDE.md 5.5.1 brings up one environment "per test file (or per milestone
//	scenario)", and the HTTP planner is one scenario. CLAUDE.md 5.5.3 - the rule
//	that forces alpha's ownership group and the whole of beta to stand alone -
//	asks whether the group "manipulates global coordination state
//	(runs.owner_id, orchestrators)". Zeta does not: it kills no process, expires
//	no lease and reads the orchestrators table nowhere. Its one replay (leg 7)
//	clears owner_id on a single run inside that run's own transaction, which is
//	the same thing delta's group does and for the same reason.
//
// WHY THE FIXTURE IS A SCRIPT AND THE TESTS ONLY ASSERT
//
//	Two of zeta's facts are only true while they are happening. The planner
//	fixture's call and fetch counters live in the fixture process, so "how many
//	times was the planner asked BEFORE the replay" cannot be recovered
//	afterwards; and leg 7's run holds zero steps while it sits in the
//	dead-letter queue, a state the replay ends. So TestMain drives the legs and
//	records what it saw at the moment it was true, and each test asserts one
//	rule against that record plus the final state of the database. Beta and
//	delta are the precedent.
//
// WHAT THIS SUITE DOES NOT CITE
//
//	SPEC.md 18.5 does not exist. CLAUDE.md 2 rule 1 forbids this agent from
//	writing it unsolicited, so no assertion below cites a demo script: every one
//	traces to a section that is already ratified - SPEC.md 3.2 (the planner call
//	as an entity), 4.2 (the driving loop's L1 rows), 5.4 (the last_step
//	convention), 5.5 (the combination table), 5.6 (the impossible ones), 5.8
//	(planner call states and failure reasons), 6.1 (planner_type, planner_url,
//	fetch_base_url), 6.2 (planner_attempt_count), 6.3 (steps.decision), 6.5 (the
//	three dead-letter reasons), 6.7 (append-only), 6.8 (the planner_calls
//	table), 9.2 (the request the planner receives), 9.3 (the three answers), 9.8
//	(rejected StepSpecs), 10.2 (the read API), 11.1 (the planner budget fields),
//	12.1 (what counts as a planner failure), 12.2 (budget and the transition to
//	DLQ), 12.3 (the planner-side column), 14 (replay) and 16 (validation).
//
//	Nothing here was derived by reading internal/ (CLAUDE.md 5.1). Where SPEC.md
//	is silent the assertion is absent rather than invented (R34-m, R37-g): the
//	suite never asserts how many times the planner was asked for one decision,
//	because SPEC.md 13.2 item 4 publishes the opposite as a non-guarantee - only
//	PERSISTED decisions are made exactly once.
package planner

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/aaronwu001/piton/test/zeta/harness"
)

// The ten workflow files. Each planner-side leg has its own, because SPEC.md
// 6.1 fixes planner_url at submission time and R40-f settled that nothing may
// change it afterwards: one behaviour, one workflow.
const (
	fileDynamic     = "workflow-dynamic.json"
	fileStatic      = "workflow-static-beside-http.json"
	fileUnreachable = "workflow-planner-unreachable.json"
	fileHTTP500     = "workflow-planner-http500.json"
	fileSlow        = "workflow-planner-slow.json"
	fileGarbage     = "workflow-planner-garbage.json"
	fileBadStep     = "workflow-planner-badstep.json"
	fileMixed       = "workflow-planner-mixed.json"
	fileFail        = "workflow-planner-fail.json"
	fileRecovers    = "workflow-planner-recovers.json"
)

// dlqFiles are the seven fixtures whose runs must end in the dead-letter queue
// without a worker ever being involved. SPEC.md 12.3's right-hand column is
// what they are all about; they differ in which of SPEC.md 6.5's reasons the
// entry must carry.
var dlqFiles = []string{
	fileUnreachable, fileHTTP500, fileSlow, fileGarbage, fileBadStep, fileMixed, fileFail,
}

// observation is what the fixture saw at moments that no longer exist by the
// time the tests run.
type observation struct {
	// --- leg 1: one workflow, two runs, identical input --------------------
	//
	// Both runs are started from the SAME workflow row, so that "the two runs
	// differ only in what the worker told the planner" is a fact about the
	// fixture rather than a claim about two definitions being equivalent.
	dynamicWorkflow string
	dynamicRunA     string
	dynamicRunB     string
	dynamicFinalA   string
	dynamicFinalB   string
	// statsAfterDynamic is the planner fixture's own count of what it did:
	// how many times it was asked, and how many times it fetched a step's
	// output through the read API of SPEC.md 10.2.
	statsAfterDynamic harness.PlannerStats

	// --- leg 2: a static workflow beside the http one ----------------------
	staticWorkflow string
	staticRun      string
	staticFinal    string

	// --- legs 3 to 6: every way a planner can stop a run -------------------
	dlqRun   map[string]string // workflow file -> run_id
	dlqFinal map[string]string // workflow file -> terminal status

	// --- leg 7: replaying a run that died on the planner's side ------------
	recoversRun string
	// recoversRound0 must be DLQ, or the leg demonstrates nothing.
	recoversRound0 string
	// recoversStepsBefore is how many steps existed while the run sat in the
	// dead-letter queue. SPEC.md 12.3 puts a planner-side DLQ at L5, and this
	// run failed at its FIRST decision point, so the answer must be 0 - which
	// is what makes "a step exists afterwards" evidence of anything.
	recoversStepsBefore int
	recoversCallsBefore int
	recoversDLQBefore   string
	recoversReplay      harness.ReplayResult
	recoversFinal       string
	recoversCallsAfter  int
	recoversDLQAfter    string

	// --- leg 8: what POST /workflows refuses -------------------------------
	rejections []rejection
	// control is the unmutated body. SPEC.md 16's rules are only tested by a
	// rejection if the same body without the mutation is accepted; otherwise
	// every case could be failing for a reason nobody named.
	controlCode int
	controlBody string
}

// rejection records one submission and what came back.
type rejection struct {
	// what names the SPEC.md 16 rule the case is about.
	what string
	code int
	body string
}

var obs = observation{
	dlqRun:   map[string]string{},
	dlqFinal: map[string]string{},
}

// plannerBudget holds each workflow file's planner_max_attempts, read from the
// file rather than written as a literal, so that an assertion and the workflow
// it is about cannot drift apart. SPEC.md 11.1 defines the field as a TOTAL
// call count at one decision point, not a retry count.
var plannerBudget = map[string]int{}

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		fmt.Println("zeta/planner: skipped in -short mode; this group needs docker compose")
		os.Exit(0)
	}

	all := append([]string{fileDynamic, fileRecovers}, dlqFiles...)
	for _, f := range all {
		n, err := plannerMaxAttempts(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, "zeta/planner:", err)
			os.Exit(1)
		}
		plannerBudget[f] = n
	}

	if err := harness.Up(); err != nil {
		fmt.Fprintln(os.Stderr, "zeta/planner: the environment did not come up:", err)
		os.Exit(1)
	}

	code := run(m)
	if err := harness.Down(); err != nil {
		fmt.Fprintln(os.Stderr, "zeta/planner: teardown failed:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(m *testing.M) int {
	if err := fixture(); err != nil {
		fmt.Fprintln(os.Stderr, "zeta/planner: the fixture could not be created:", err)
		fmt.Fprintln(os.Stderr, "\n--- orchestrator logs ---")
		fmt.Fprintln(os.Stderr, harness.OrchestratorLogs(80))
		return 1
	}
	return m.Run()
}

// fixture drives every leg, in order, against one environment.
//
// It fails loudly rather than quietly whenever a leg does not reach the state
// it exists to reach. R34-i is why: a fixture that proceeds from a state it did
// not verify produces a suite that passes while testing almost nothing, and
// that failure mode is invisible in a green run.
func fixture() error {
	if err := harness.WaitHealthy(harness.HealthTimeout); err != nil {
		return err
	}
	if err := harness.WaitPlannerHealthy(harness.HealthTimeout); err != nil {
		return err
	}
	for _, leg := range []func() error{legDynamic, legStatic, legPlannerSide, legRecovers, legValidation} {
		if err := leg(); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Leg 1 - one workflow, two runs, identical input, different paths
// ---------------------------------------------------------------------------

// legDynamic is the milestone's central demonstration.
//
// The two runs are started from one workflow with one input, and the classify
// worker answers "invoice" to the first run and "receipt" to the second. The
// catalogue the planner receives carries output_bytes and no content (SPEC.md
// 9.2), so nothing in the request tells the two runs apart: the planner can
// only branch by fetching the step's output through the read API of SPEC.md
// 10.2. Two different paths out of two identical requests is therefore evidence
// that the fetch happened, and it is a shape the static planner cannot produce
// at all - SPEC.md 6.1 makes its answer a function of the step COUNT.
//
// The runs are sequential, not concurrent. The alternation is a property of the
// fixture worker, and two runs racing for it would make which run got which
// answer a matter of timing.
func legDynamic() error {
	var err error
	if obs.dynamicWorkflow, err = harness.CreateWorkflow(fileDynamic); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}

	if obs.dynamicRunA, err = harness.StartRun(obs.dynamicWorkflow); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	if obs.dynamicFinalA, err = harness.WaitTerminal(obs.dynamicRunA, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	if obs.dynamicFinalA != "DONE" {
		return fmt.Errorf("leg 1: the first run must finish before the second starts, so that the "+
			"worker's alternation is not a race; it reached %q", obs.dynamicFinalA)
	}

	if obs.dynamicRunB, err = harness.StartRun(obs.dynamicWorkflow); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	if obs.dynamicFinalB, err = harness.WaitTerminal(obs.dynamicRunB, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}

	if obs.statsAfterDynamic, err = harness.Stats(); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	fmt.Printf("zeta/planner: leg 1 (dynamic)      run_a=%s -> %s   run_b=%s -> %s\n",
		obs.dynamicRunA, obs.dynamicFinalA, obs.dynamicRunB, obs.dynamicFinalB)
	return nil
}

// ---------------------------------------------------------------------------
// Leg 2 - a static workflow in the same system, at the same time
// ---------------------------------------------------------------------------

// legStatic is what R40-f's ruling looks like when it is executed rather than
// stated: "switching between planners" is the choice made at POST /workflows
// and nothing else, so the demonstration is two workflows of two planner types
// over one set of workers, both of which work.
func legStatic() error {
	var err error
	if obs.staticWorkflow, err = harness.CreateWorkflow(fileStatic); err != nil {
		return fmt.Errorf("leg 2: %w", err)
	}
	if obs.staticRun, err = harness.StartRun(obs.staticWorkflow); err != nil {
		return fmt.Errorf("leg 2: %w", err)
	}
	if obs.staticFinal, err = harness.WaitTerminal(obs.staticRun, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 2: %w", err)
	}
	fmt.Printf("zeta/planner: leg 2 (static)       run_id=%s -> %s\n", obs.staticRun, obs.staticFinal)
	return nil
}

// ---------------------------------------------------------------------------
// Legs 3 to 6 - every way a planner stops a run
// ---------------------------------------------------------------------------

// legPlannerSide runs one workflow per way a planner call can end badly.
//
// SPEC.md 12.1 sorts them into two classes and the difference is the whole
// point: an unreachable planner, a timeout, a non-2xx, an unparseable body, an
// unknown status and an invalid StepSpec are FAILURES and burn budget, while a
// `fail` answer is "not a planner failure - it is a valid answer, and it sends
// the run to DLQ immediately without consuming budget".
func legPlannerSide() error {
	for _, file := range dlqFiles {
		runID, err := harness.Begin(file)
		if err != nil {
			return fmt.Errorf("legs 3-6 (%s): %w", file, err)
		}
		final, err := harness.WaitTerminal(runID, harness.RunTimeout)
		if err != nil {
			return fmt.Errorf("legs 3-6 (%s): %w", file, err)
		}
		if final != "DLQ" {
			return fmt.Errorf("legs 3-6 (%s): SPEC.md 12.2 sends a run whose planner cannot be "+
				"used to the dead-letter queue; this run reached %q", file, final)
		}
		obs.dlqRun[file] = runID
		obs.dlqFinal[file] = final
		fmt.Printf("zeta/planner: legs 3-6 %-34s run_id=%s -> %s\n", "("+file+")", runID, final)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Leg 7 - replaying a run that died on the planner's side
// ---------------------------------------------------------------------------

// legRecovers is the half of SPEC.md 12.3 that delta could not demonstrate.
//
// Delta replayed a run whose STEP had exhausted its budget, and the replay
// re-dispatched that step. Here the run never got a step at all: it exhausted
// planner_max_attempts at its first decision point, so SPEC.md 12.3's
// right-hand column applies and "what replay resumes" is "asking the planner
// again".
//
// The fixture planner answers properly from its third call onwards, so the
// second round is not a repeat of the first. That behaviour lives in the
// fixture, not in Piton - the same arrangement delta's fail_then_succeed worker
// uses, for the same reason.
func legRecovers() error {
	var err error
	if obs.recoversRun, err = harness.Begin(fileRecovers); err != nil {
		return fmt.Errorf("leg 7: %w", err)
	}
	if obs.recoversRound0, err = harness.WaitTerminal(obs.recoversRun, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 7: %w", err)
	}
	if obs.recoversRound0 != "DLQ" {
		return fmt.Errorf("leg 7: the run must reach DLQ before it can be replayed (SPEC.md 14); "+
			"it reached %q", obs.recoversRound0)
	}

	if obs.recoversStepsBefore, err = countSteps(obs.recoversRun); err != nil {
		return fmt.Errorf("leg 7: %w", err)
	}
	if obs.recoversDLQBefore, err = harness.PSQL(harness.DLQDigest, harness.Var(obs.recoversRun)); err != nil {
		return fmt.Errorf("leg 7: %w", err)
	}
	statsBefore, err := harness.Stats()
	if err != nil {
		return fmt.Errorf("leg 7: %w", err)
	}
	obs.recoversCallsBefore = statsBefore.CallsFor(obs.recoversRun)

	if obs.recoversReplay, err = harness.Replay(obs.recoversRun); err != nil {
		return fmt.Errorf("leg 7: %w", err)
	}
	if !obs.recoversReplay.Accepted() {
		return fmt.Errorf("leg 7: SPEC.md 14 - a replay of a run that is in DLQ proceeds; "+
			"POST /runs/{run_id}/replay returned %d: %s", obs.recoversReplay.Code, obs.recoversReplay.Body)
	}

	if obs.recoversFinal, err = harness.WaitTerminal(obs.recoversRun, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 7: %w", err)
	}
	if obs.recoversDLQAfter, err = harness.PSQL(harness.DLQDigest, harness.Var(obs.recoversRun)); err != nil {
		return fmt.Errorf("leg 7: %w", err)
	}
	statsAfter, err := harness.Stats()
	if err != nil {
		return fmt.Errorf("leg 7: %w", err)
	}
	obs.recoversCallsAfter = statsAfter.CallsFor(obs.recoversRun)

	fmt.Printf("zeta/planner: leg 7 (recovers)     run_id=%s  DLQ -> replay -> %s  (planner asked %d times, then %d)\n",
		obs.recoversRun, obs.recoversFinal, obs.recoversCallsBefore, obs.recoversCallsAfter)
	return nil
}

// ---------------------------------------------------------------------------
// Leg 8 - what POST /workflows refuses
// ---------------------------------------------------------------------------

// legValidation submits four bodies: three that SPEC.md 16 requires to be
// refused, and the unmutated original, which must be accepted.
//
// The control is not decoration. Each mutated body differs from an accepted one
// in exactly one respect, so a 400 can be attributed to that respect. Without
// it, a body rejected for an unrelated reason would produce a green test that
// proves nothing - the failure mode this suite's own author walked into once
// already (R38-h) and the one R34-i names.
func legValidation() error {
	control, err := harness.WorkflowJSON(fileDynamic)
	if err != nil {
		return fmt.Errorf("leg 8: %w", err)
	}
	_, controlCode, controlBody, err := harness.CreateWorkflowFromBody(control)
	if err != nil {
		return fmt.Errorf("leg 8: %w", err)
	}
	obs.controlCode = controlCode
	obs.controlBody = string(controlBody)

	cases := []struct {
		what   string
		mutate func(wf map[string]any)
	}{
		{
			what: "SPEC.md 16 rule 2: planner_type 'http' with no fetch_base_url",
			mutate: func(wf map[string]any) {
				delete(wf, "fetch_base_url")
			},
		},
		{
			what: "SPEC.md 16 rule 2: fetch_base_url is not a valid absolute HTTP(S) URL",
			mutate: func(wf map[string]any) {
				wf["fetch_base_url"] = "orchestrator:8080"
			},
		},
		{
			what: "SPEC.md 16 rule 3, SPEC.md 6.1: planner_type 'static' carrying fetch_base_url",
			mutate: func(wf map[string]any) {
				static, err := harness.WorkflowJSON(fileStatic)
				if err != nil {
					panic(err)
				}
				var base map[string]any
				if err := json.Unmarshal(static, &base); err != nil {
					panic(err)
				}
				for k := range wf {
					delete(wf, k)
				}
				for k, v := range base {
					wf[k] = v
				}
				wf["fetch_base_url"] = "http://orchestrator:8080"
			},
		},
	}

	for _, c := range cases {
		raw, err := workflowBody(fileDynamic, c.mutate)
		if err != nil {
			return fmt.Errorf("leg 8: %w", err)
		}
		_, code, body, err := harness.CreateWorkflowFromBody(raw)
		if err != nil {
			return fmt.Errorf("leg 8 (%s): %w", c.what, err)
		}
		obs.rejections = append(obs.rejections, rejection{what: c.what, code: code, body: string(body)})
	}

	fmt.Printf("zeta/planner: leg 8 (validation)   control=%d  rejections=%v\n",
		obs.controlCode, codes(obs.rejections))
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// workflowBody reads a fixture and applies one mutation to it, so that the body
// submitted differs from an accepted one in exactly one respect.
func workflowBody(file string, mutate func(wf map[string]any)) ([]byte, error) {
	raw, err := harness.WorkflowJSON(file)
	if err != nil {
		return nil, err
	}
	var wf map[string]any
	if err := json.Unmarshal(raw, &wf); err != nil {
		return nil, fmt.Errorf("%s is not a JSON object: %w", file, err)
	}
	mutate(wf)
	return json.Marshal(wf)
}

// plannerMaxAttempts reads planner_max_attempts out of a workflow fixture.
func plannerMaxAttempts(file string) (int, error) {
	raw, err := harness.WorkflowJSON(file)
	if err != nil {
		return 0, err
	}
	var wf struct {
		PlannerMaxAttempts *int `json:"planner_max_attempts"`
	}
	if err := json.Unmarshal(raw, &wf); err != nil {
		return 0, fmt.Errorf("%s is not a JSON object: %w", file, err)
	}
	if wf.PlannerMaxAttempts == nil {
		return 0, fmt.Errorf("%s declares no planner_max_attempts, and every zeta fixture must "+
			"declare one: the assertions are about the budget", file)
	}
	return *wf.PlannerMaxAttempts, nil
}

func countSteps(runID string) (int, error) {
	out, err := harness.PSQL("SELECT count(*) FROM steps WHERE run_id = :'run';", harness.Var(runID))
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(out, "%d", &n); err != nil {
		return 0, fmt.Errorf("expected a count, got %q", out)
	}
	return n, nil
}

func codes(rs []rejection) []int {
	out := make([]int, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.code)
	}
	return out
}
