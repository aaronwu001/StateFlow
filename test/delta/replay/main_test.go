// Package replay is milestone delta's test group: a run that has landed in the
// dead-letter queue is replayed, and must resume, refuse, and leave a record.
//
// It is one group in the sense of CLAUDE.md 5.5: one docker-compose environment
// is brought up for this package, every test in it runs against that
// environment, and it is torn down with a volume wipe before the next group
// starts.
//
// WHY ALL SIX LEGS ARE ONE GROUP AND NOT SIX
//
//	CLAUDE.md 5.5.1 brings up one environment "per test file (or per milestone
//	scenario)", and replay is one scenario. CLAUDE.md 5.5.3 - the rule that
//	forces alpha's ownership group and the whole of beta to stand alone - asks
//	whether the group "manipulates global coordination state (runs.owner_id,
//	orchestrators)". Delta does not: SPEC.md 14 clears owner_id and claimed_at
//	on ONE run, inside that run's own replay transaction, and nothing here kills
//	a process, expires a lease or reads the orchestrators table. The legs run
//	one after another all the same, because SPEC.md 4.4's deployment shape is
//	one orchestrator and a failure that could be explained by contention would
//	be a failure that explains nothing.
//
//	Leg 3 is concurrent WITHIN itself - eight replays of one run at once - and
//	that is the point of it rather than an exception to the rule above. SPEC.md
//	14's gate is a claim about two callers racing for one run, and it cannot be
//	reproduced by two callers who take turns.
//
// WHY THE FIXTURE IS A SCRIPT AND THE TESTS ONLY ASSERT
//
//	Beta's suite is written this way because a crash is only visible while it
//	happens, and delta has the same problem for a different reason: SPEC.md 14
//	puts a replayed run back into RUNNING and its step back into RUNNING, and a
//	step that succeeded in milliseconds would be DONE before any test function
//	ran. So TestMain drives the legs and records what it observed, with real
//	queries at the moment they were true, and each test asserts one rule against
//	that record plus the final state of the database.
//
// WHAT THIS SUITE DOES NOT CITE
//
//	SPEC.md 18.4 does not exist. CLAUDE.md 2 rule 1 forbids this agent from
//	writing it unsolicited, so no assertion below cites a demo script: every one
//	traces to a section that is already ratified - SPEC.md 14 (replay), 10.1
//	(the endpoint), 10.5 (error responses), 12.2 and 12.3 (the DLQ transaction
//	and which side replay resumes), 6.2 (replay_count), 6.3 (steps), 6.5
//	(replay_round), 6.7 (append-only), 5.5 (the combination table) and 5.6 (the
//	impossible ones). R34-e is the precedent, and it is a better arrangement
//	than gamma's in one respect: the suite's authority does not depend on the
//	demo script at all.
//
//	Nothing here was derived by reading internal/ (CLAUDE.md 5.1). Where SPEC.md
//	is silent - the success code of a performed replay, the `error` slug of a
//	404 - the assertion is absent rather than invented (R34-m).
package replay

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/aaronwu001/piton/test/delta/harness"
)

// The five workflow files, one per leg that needs one.
const (
	fileResumes   = "workflow-replay-resumes.json"
	fileTwoRounds = "workflow-replay-two-rounds.json"
	fileGate      = "workflow-replay-gate.json"
	fileInFlight  = "workflow-replay-in-flight.json"
	fileDone      = "workflow-replay-done.json"
)

// gateReplays is how many replays leg 3 fires at once. SPEC.md 14 says a
// double-click has exactly one winner; eight is a double-click with margin, and
// a larger number would only make the same claim more slowly.
const gateReplays = 8

// missingRunID is a syntactically well-formed identifier that names no run.
// SPEC.md 10.5's table gives 404 the meaning "no such entity", and a malformed
// identifier would be testing SPEC.md 10.5's 400 instead.
const missingRunID = "00000000-0000-0000-0000-0000000000ff"

// observation is what the fixture saw at moments that no longer exist by the
// time the tests run.
type observation struct {
	// --- leg 1: a worker-side DLQ is replayed and the run finishes ---------
	resumesRun string
	// resumesFirstRound is the state the run reached before any replay. It
	// must be DLQ, or the leg demonstrates nothing.
	resumesFirstRound string
	// stepsBeforeReplay is how many steps existed while the run was in DLQ.
	// SPEC.md 12.2 stops a run at the step that exhausted its budget, so the
	// second static step cannot exist yet - and it must exist afterwards.
	stepsBeforeReplay int
	resumesReplay     harness.ReplayResult
	resumesFinal      string
	// dlqBefore and dlqAfter bracket the replay. SPEC.md 6.7 makes
	// dead_letter_queue append-only and SPEC.md 12.4 says an entry "is written
	// once and never modified".
	resumesDLQBefore string
	resumesDLQAfter  string

	// --- leg 2: two rounds, and the furthest step reached ------------------
	twoRoundsRun string
	// The state after each round. Round 0 must fail at "first" and round 1 at
	// "second", or the leg is not about SPEC.md 14's accepted limitation.
	twoRoundsRound0 string
	twoRoundsRound1 string
	twoRoundsFinal  string
	replay1         harness.ReplayResult
	replay2         harness.ReplayResult
	// firstStepBefore and firstStepAfter bracket the SECOND replay. SPEC.md
	// 14: "a replay always resumes at the furthest step reached... replay
	// resumes from step 2 and never revisits step 1."
	firstStepBefore string
	firstStepAfter  string

	// --- leg 3: the idempotency gate --------------------------------------
	gateRun     string
	gateResults []harness.ReplayResult
	gateFinal   string

	// --- leg 4: the state a replay leaves behind, seen while it is true ----
	inFlightRun string
	// inFlightSnapshot is one reading of seven values, taken in a single query
	// so that they are consistent with one another, while the replayed step's
	// new attempt was on the wire. See snapshotSQL.
	inFlightSnapshot snapshot
	inFlightReplay   harness.ReplayResult
	// refusedWhileRunning is a second replay issued while the run was RUNNING.
	// SPEC.md 14: the gate is "is this run in DLQ right now", NOT "has this run
	// been replayed before" - so this must be refused even though the very same
	// run was replayable a moment earlier.
	refusedWhileRunning harness.ReplayResult
	inFlightFinal       string

	// --- leg 5: a run that never failed ------------------------------------
	doneRun          string
	doneFinal        string
	refusedWhileDone harness.ReplayResult

	// --- leg 6: no such run ------------------------------------------------
	missingRun harness.ReplayResult
}

// snapshot is leg 4's reading of the database in the window SPEC.md 14 creates.
type snapshot struct {
	runStatus  string
	stepStatus string
	// stepAttemptCount is steps.attempt_count. SPEC.md 14 resets it to 0, and
	// SPEC.md 4.2 burns one unit at the next dispatch - so during the first
	// attempt of a replayed round it must read exactly 1.
	stepAttemptCount int
	// attemptRows is how many rows the step has in `attempts`. SPEC.md 6.3:
	// replay "leaves the attempts rows in place", so this keeps climbing while
	// stepAttemptCount restarts.
	attemptRows int
	ownerHeld   bool
	dlqRows     int
	replayCount int
}

// snapshotSQL reads leg 4's seven values in ONE query.
//
// One query rather than seven, because these values are only true together for
// as long as one attempt is on the wire, and seven separate reads could
// straddle the moment it finished. SPEC.md 17.1 makes database truth the
// interface; this is that interface read at an instant.
//
// Ownership is emitted as a WORD rather than as a boolean, and that is not
// decoration. A boolean read as its own column prints as `t` under psql -At,
// but the same boolean concatenated into a string prints as `true` - the cast
// to text is what differs, not the value. A snapshot that packs seven values
// into one row goes through the second path, so spelling the two cases out
// removes the trap instead of relying on which of the two renderings applies.
const snapshotSQL = `
SELECT r.status || '|' || s.status || '|' || s.attempt_count || '|' ||
       (SELECT count(*) FROM attempts a WHERE a.step_id = s.step_id) || '|' ||
       (CASE WHEN r.owner_id IS NOT NULL THEN 'owned' ELSE 'unowned' END) || '|' ||
       (SELECT count(*) FROM dead_letter_queue d WHERE d.run_id = r.run_id) || '|' ||
       r.replay_count
  FROM runs r
  JOIN steps s ON s.run_id = r.run_id
 WHERE r.run_id = :'run'
 ORDER BY s.seq DESC
 LIMIT 1;`

// maxAttempts holds each workflow file's step_max_attempts, read from the files
// rather than written as literals, so that an assertion and the workflow it is
// about cannot drift apart.
var maxAttempts = map[string]int{}

var obs observation

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		fmt.Println("delta/replay: skipped in -short mode; this group needs docker compose")
		os.Exit(0)
	}

	for _, f := range []string{fileResumes, fileTwoRounds, fileGate, fileInFlight, fileDone} {
		n, err := harness.MaxAttempts(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, "delta/replay:", err)
			os.Exit(1)
		}
		maxAttempts[f] = n
	}

	if err := harness.Up(); err != nil {
		fmt.Fprintln(os.Stderr, "delta/replay: the environment did not come up:", err)
		os.Exit(1)
	}

	code := run(m)
	if err := harness.Down(); err != nil {
		fmt.Fprintln(os.Stderr, "delta/replay: teardown failed:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(m *testing.M) int {
	if err := fixture(); err != nil {
		fmt.Fprintln(os.Stderr, "delta/replay: the fixture could not be created:", err)
		fmt.Fprintln(os.Stderr, "\n--- orchestrator logs ---")
		fmt.Fprintln(os.Stderr, harness.OrchestratorLogs(80))
		return 1
	}
	return m.Run()
}

// fixture drives all six legs, in order, against one environment.
//
// It fails loudly rather than quietly whenever a leg does not reach the state
// it exists to reach. R34-i is why: a fixture that proceeds from a state it did
// not verify produces a suite that passes while testing almost nothing, and
// that failure mode is invisible in a green run.
func fixture() error {
	if err := harness.WaitHealthy(harness.HealthTimeout); err != nil {
		return err
	}
	for _, leg := range []func() error{legResumes, legTwoRounds, legGate, legInFlight, legDone, legMissing} {
		if err := leg(); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Leg 1 - a worker-side dead letter is replayed, and the run finishes
// ---------------------------------------------------------------------------

func legResumes() error {
	var err error
	if obs.resumesRun, err = harness.Begin(fileResumes); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	if obs.resumesFirstRound, err = harness.WaitTerminal(obs.resumesRun, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	if obs.resumesFirstRound != "DLQ" {
		return fmt.Errorf("leg 1: the run must reach DLQ before it can be replayed (SPEC.md 14); it reached %q",
			obs.resumesFirstRound)
	}

	if obs.stepsBeforeReplay, err = countSteps(obs.resumesRun); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	if obs.resumesDLQBefore, err = harness.PSQL(harness.DLQDigest, harness.Var(obs.resumesRun)); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}

	if obs.resumesReplay, err = harness.Replay(obs.resumesRun); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	if !obs.resumesReplay.Accepted() {
		return fmt.Errorf("leg 1: SPEC.md 14 - a replay of a run that is in DLQ proceeds; "+
			"POST /runs/{run_id}/replay returned %d: %s", obs.resumesReplay.Code, obs.resumesReplay.Body)
	}

	if obs.resumesFinal, err = harness.WaitTerminal(obs.resumesRun, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	if obs.resumesDLQAfter, err = harness.PSQL(harness.DLQDigest, harness.Var(obs.resumesRun)); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	fmt.Printf("delta/replay: leg 1 (resumes)      run_id=%s  DLQ -> replay -> %s\n",
		obs.resumesRun, obs.resumesFinal)
	return nil
}

// ---------------------------------------------------------------------------
// Leg 2 - two rounds, and the accepted limitation of SPEC.md 14
// ---------------------------------------------------------------------------

func legTwoRounds() error {
	var err error
	if obs.twoRoundsRun, err = harness.Begin(fileTwoRounds); err != nil {
		return fmt.Errorf("leg 2: %w", err)
	}
	if obs.twoRoundsRound0, err = harness.WaitTerminal(obs.twoRoundsRun, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 2: %w", err)
	}
	if obs.twoRoundsRound0 != "DLQ" {
		return fmt.Errorf("leg 2: round 0 must reach DLQ; it reached %q", obs.twoRoundsRound0)
	}
	// Round 0 must have died at "first". If it died anywhere else the two
	// rounds would not be about two different steps, and the whole leg would be
	// asserting something other than SPEC.md 14's accepted limitation.
	if err = requireDLQStep(obs.twoRoundsRun, "first", "leg 2 round 0"); err != nil {
		return err
	}

	if obs.replay1, err = harness.Replay(obs.twoRoundsRun); err != nil {
		return fmt.Errorf("leg 2: %w", err)
	}
	if !obs.replay1.Accepted() {
		return fmt.Errorf("leg 2: the first replay was refused with %d: %s", obs.replay1.Code, obs.replay1.Body)
	}
	if obs.twoRoundsRound1, err = harness.WaitTerminal(obs.twoRoundsRun, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 2: %w", err)
	}
	if obs.twoRoundsRound1 != "DLQ" {
		return fmt.Errorf("leg 2: round 1 must reach DLQ at the SECOND step; it reached %q",
			obs.twoRoundsRound1)
	}
	if err = requireDLQStep(obs.twoRoundsRun, "second", "leg 2 round 1"); err != nil {
		return err
	}

	// The reading that matters, taken before the second replay and again after
	// the run has finished.
	if obs.firstStepBefore, err = harness.PSQL(harness.StepDigest,
		harness.Var(obs.twoRoundsRun), harness.StepVar("first")); err != nil {
		return fmt.Errorf("leg 2: %w", err)
	}

	if obs.replay2, err = harness.Replay(obs.twoRoundsRun); err != nil {
		return fmt.Errorf("leg 2: %w", err)
	}
	if !obs.replay2.Accepted() {
		return fmt.Errorf("leg 2: the second replay was refused with %d: %s", obs.replay2.Code, obs.replay2.Body)
	}
	if obs.twoRoundsFinal, err = harness.WaitTerminal(obs.twoRoundsRun, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 2: %w", err)
	}
	if obs.firstStepAfter, err = harness.PSQL(harness.StepDigest,
		harness.Var(obs.twoRoundsRun), harness.StepVar("first")); err != nil {
		return fmt.Errorf("leg 2: %w", err)
	}
	fmt.Printf("delta/replay: leg 2 (two rounds)   run_id=%s  DLQ -> replay -> DLQ -> replay -> %s\n",
		obs.twoRoundsRun, obs.twoRoundsFinal)
	return nil
}

// ---------------------------------------------------------------------------
// Leg 3 - the idempotency gate
// ---------------------------------------------------------------------------

func legGate() error {
	var err error
	if obs.gateRun, err = harness.Begin(fileGate); err != nil {
		return fmt.Errorf("leg 3: %w", err)
	}
	round0, err := harness.WaitTerminal(obs.gateRun, harness.RunTimeout)
	if err != nil {
		return fmt.Errorf("leg 3: %w", err)
	}
	if round0 != "DLQ" {
		return fmt.Errorf("leg 3: the run must be in DLQ when the burst arrives; it is %q", round0)
	}

	if obs.gateResults, err = harness.ReplayConcurrently(obs.gateRun, gateReplays); err != nil {
		return fmt.Errorf("leg 3: %w", err)
	}
	if len(obs.gateResults) != gateReplays {
		return fmt.Errorf("leg 3: %d of %d replays returned a response", len(obs.gateResults), gateReplays)
	}
	if obs.gateFinal, err = harness.WaitTerminal(obs.gateRun, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 3: %w", err)
	}
	fmt.Printf("delta/replay: leg 3 (gate)         run_id=%s  %d concurrent replays -> %s\n",
		obs.gateRun, gateReplays, obs.gateFinal)
	return nil
}

// ---------------------------------------------------------------------------
// Leg 4 - the state a replay leaves behind, observed while it is true
// ---------------------------------------------------------------------------

func legInFlight() error {
	var err error
	if obs.inFlightRun, err = harness.Begin(fileInFlight); err != nil {
		return fmt.Errorf("leg 4: %w", err)
	}
	round0, err := harness.WaitTerminal(obs.inFlightRun, harness.RunTimeout)
	if err != nil {
		return fmt.Errorf("leg 4: %w", err)
	}
	if round0 != "DLQ" {
		return fmt.Errorf("leg 4: the run must reach DLQ first; it reached %q", round0)
	}

	if obs.inFlightReplay, err = harness.Replay(obs.inFlightRun); err != nil {
		return fmt.Errorf("leg 4: %w", err)
	}
	if !obs.inFlightReplay.Accepted() {
		return fmt.Errorf("leg 4: the replay was refused with %d: %s",
			obs.inFlightReplay.Code, obs.inFlightReplay.Body)
	}

	// Wait for the replayed step's NEW attempt to be on the wire. SPEC.md 14
	// hands the run to "the next sweep", so this is a wait for an event and
	// never a sleep for a computed duration.
	if err = harness.Await("the replayed step's second attempt to be RUNNING",
		`SELECT count(*) = 2 AND bool_or(status = 'RUNNING')
           FROM attempts WHERE run_id = :'run';`,
		harness.RunTimeout, harness.Var(obs.inFlightRun)); err != nil {
		return fmt.Errorf("leg 4: %w", err)
	}
	if obs.inFlightSnapshot, err = readSnapshot(obs.inFlightRun); err != nil {
		return fmt.Errorf("leg 4: %w", err)
	}

	// Still RUNNING, because the worker sleeps on its success path. This is the
	// second half of SPEC.md 14's gate: a run that HAS been replayed, and is
	// refused now because of what it is, not because of what it was.
	if obs.refusedWhileRunning, err = harness.Replay(obs.inFlightRun); err != nil {
		return fmt.Errorf("leg 4: %w", err)
	}

	if obs.inFlightFinal, err = harness.WaitTerminal(obs.inFlightRun, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 4: %w", err)
	}
	fmt.Printf("delta/replay: leg 4 (in flight)    run_id=%s  replay -> RUNNING (second replay %d) -> %s\n",
		obs.inFlightRun, obs.refusedWhileRunning.Code, obs.inFlightFinal)
	return nil
}

// ---------------------------------------------------------------------------
// Legs 5 and 6 - the refusals that need no dead letter at all
// ---------------------------------------------------------------------------

func legDone() error {
	var err error
	if obs.doneRun, err = harness.Begin(fileDone); err != nil {
		return fmt.Errorf("leg 5: %w", err)
	}
	if obs.doneFinal, err = harness.WaitTerminal(obs.doneRun, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 5: %w", err)
	}
	if obs.doneFinal != "DONE" {
		return fmt.Errorf("leg 5: the run must reach DONE; it reached %q", obs.doneFinal)
	}
	if obs.refusedWhileDone, err = harness.Replay(obs.doneRun); err != nil {
		return fmt.Errorf("leg 5: %w", err)
	}
	fmt.Printf("delta/replay: leg 5 (done)         run_id=%s  DONE, replay refused with %d\n",
		obs.doneRun, obs.refusedWhileDone.Code)
	return nil
}

func legMissing() error {
	var err error
	if obs.missingRun, err = harness.Replay(missingRunID); err != nil {
		return fmt.Errorf("leg 6: %w", err)
	}
	fmt.Printf("delta/replay: leg 6 (no such run)  run_id=%s  replay refused with %d\n",
		missingRunID, obs.missingRun.Code)
	return nil
}

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

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

// requireDLQStep fails the fixture unless the named step is the one that is in
// DLQ. SPEC.md 12.3's worker-side row puts step and run in DLQ together, so
// "which step died" is what tells the two rounds of leg 2 apart.
func requireDLQStep(runID, stepName, where string) error {
	out, err := harness.PSQL(
		`SELECT coalesce(string_agg(step_name || '=' || status, ',' ORDER BY seq), '<no steps>')
           FROM steps WHERE run_id = :'run';`, harness.Var(runID))
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	if !strings.Contains(out, stepName+"=DLQ") {
		return fmt.Errorf("%s: step %q must be the one in DLQ; the steps are: %s", where, stepName, out)
	}
	return nil
}

func readSnapshot(runID string) (snapshot, error) {
	out, err := harness.PSQL(snapshotSQL, harness.Var(runID))
	if err != nil {
		return snapshot{}, err
	}
	parts := strings.Split(out, "|")
	if len(parts) != 7 {
		return snapshot{}, fmt.Errorf("expected 7 fields from the snapshot query, got %q", out)
	}
	s := snapshot{runStatus: parts[0], stepStatus: parts[1]}
	switch parts[4] {
	case "owned":
		s.ownerHeld = true
	case "unowned":
		s.ownerHeld = false
	default:
		return snapshot{}, fmt.Errorf("expected owned/unowned in the snapshot, got %q (from %q)",
			parts[4], out)
	}
	for _, f := range []struct {
		raw string
		dst *int
	}{
		{parts[2], &s.stepAttemptCount},
		{parts[3], &s.attemptRows},
		{parts[5], &s.dlqRows},
		{parts[6], &s.replayCount},
	} {
		if _, err := fmt.Sscanf(f.raw, "%d", f.dst); err != nil {
			return snapshot{}, fmt.Errorf("expected an integer in the snapshot, got %q (from %q)", f.raw, out)
		}
	}
	return s, nil
}
