// Package recovery is milestone beta's test group: the orchestrator is killed
// mid-run and must resume, and content already in the dead-letter queue must be
// left alone.
//
// It is one group in the sense of CLAUDE.md 5.5, and 5.5.3 is what makes that
// mandatory rather than convenient: this group manipulates global coordination
// state - runs.owner_id and the orchestrators table - so a test running beside
// it would see a database that no single test put into that condition. It also
// kills the process every other test would be talking to.
//
// WHY THE FIXTURE IS A SCRIPT AND THE TESTS ONLY ASSERT
//
//	Most of what beta demonstrates is only visible WHILE it happens: that a run
//	still has an owner_id in the instant after a SIGKILL, that a clean shutdown
//	has already cleared it before anything else runs. By the time Go's test
//	functions execute, the system has moved on. So TestMain drives the legs and
//	records what it observed, with real queries at the moment they were true,
//	and each test asserts one rule against that record plus the final state of
//	the database.
//
// Nothing here was derived by reading internal/. CLAUDE.md 5.1 permits exactly
// one source, and every assertion below names the ratified section it came
// from: SPEC.md 13.1 (what recovery handles), 13.2 (what it does not), 8.5
// (claiming), 8.6 (the sweep and the claim-time rule), 8.7 (heartbeat and
// release), 5.3 (orphaned), 5.5 (the combination table) and 12.2 (the DLQ
// transaction).
package recovery

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/aaronwu001/piton/test/beta/harness"
)

// observation is what the fixture saw at a moment that no longer exists by the
// time the tests run.
type observation struct {
	// --- leg 1: SIGKILL mid-run -------------------------------------------
	crashRun string
	// ownerBeforeKill is runs.owner_id while the orchestrator was alive and
	// driving. SPEC.md 6.2 makes it non-NULL only while status = RUNNING.
	ownerBeforeKill string
	// ownerAfterKill is runs.owner_id read immediately after SIGKILL. SPEC.md
	// 8.7 lists exactly four writers of coordination metadata, and none of them
	// runs when a process is killed - so this must still be ownerBeforeKill.
	ownerAfterKill string
	// dispatcherBeforeKill is the orchestrator_id that dispatched the attempt
	// that was in flight when the process died (SPEC.md 6.4).
	dispatcherBeforeKill string
	// doneStepBefore is a digest of the step that had already completed before
	// the crash. SPEC.md 13.1.1: "completed steps never re-run".
	doneStepBefore string
	doneStepAfter  string
	crashFinal     string

	// --- leg 2: the DLQ'd run the crash must not touch ---------------------
	dlqRun string
	// dlqDigestBefore and dlqDigestAfter bracket the crash of leg 1. SPEC.md
	// 13.2.7: "recovery never auto-replays a DLQ'd run".
	dlqDigestBefore string
	dlqDigestAfter  string

	// --- leg 3: clean shutdown --------------------------------------------
	cleanRun string
	// ownerAfterCleanStop is runs.owner_id read after SIGTERM and after the
	// process exited. SPEC.md 8.7: a clean shutdown releases.
	ownerAfterCleanStop string
	cleanFinal          string

	// --- leg 4: the crash loop --------------------------------------------
	loopRun   string
	loopKills int
	loopFinal string

	// --- leg 5: storage unreachable at startup -----------------------------
	badBootExit   int
	badBootOutput string
}

var obs observation

// maxAttempts is the crash-loop workflow's step_max_attempts, read from the
// file so that "one kill per unit of budget" cannot drift apart from the
// workflow it is about.
var maxAttempts int

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		fmt.Println("beta/recovery: skipped in -short mode; this group needs docker compose")
		os.Exit(0)
	}

	var err error
	if maxAttempts, err = harness.MaxAttempts("workflow-crash-loop.json"); err != nil {
		fmt.Fprintln(os.Stderr, "beta/recovery:", err)
		os.Exit(1)
	}

	if err = harness.Up(); err != nil {
		fmt.Fprintln(os.Stderr, "beta/recovery: the environment did not come up:", err)
		os.Exit(1)
	}

	code := run(m)
	if err := harness.Down(); err != nil {
		fmt.Fprintln(os.Stderr, "beta/recovery: teardown failed:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(m *testing.M) int {
	if err := fixture(); err != nil {
		fmt.Fprintln(os.Stderr, "beta/recovery: the fixture could not be created:", err)
		fmt.Fprintln(os.Stderr, "\n--- orchestrator logs ---")
		fmt.Fprintln(os.Stderr, harness.OrchestratorLogs(80))
		return 1
	}
	return m.Run()
}

// stepDigest is one line describing everything about a step that a re-run would
// change. SPEC.md 13.1.1 guarantees completed steps never re-run, and a digest
// taken before and after the crash is how that is asserted rather than assumed.
const stepDigest = `SELECT s.status || '|' || s.attempt_count || '|' || coalesce(s.completed_at::text, '') ||
                           '|' || coalesce(md5(s.output::text), '') || '|' ||
                           coalesce((SELECT string_agg(a.attempt_id::text || ':' || a.status, ','
                                                       ORDER BY a.attempt_no)
                                       FROM attempts a WHERE a.step_id = s.step_id), '')
                      FROM steps s WHERE s.run_id = :'run' AND s.step_name = :'step';`

// runDigest covers a whole run: its own row, every step, every attempt, and
// every dead-letter entry. Leg 2 takes it either side of a crash.
const runDigest = `SELECT md5(
      (SELECT coalesce(string_agg(x, '|'), '') FROM (
         SELECT r.status || r.replay_count || r.planner_attempt_count ||
                coalesce(r.owner_id, '') || coalesce(r.claimed_at::text, '') AS x
           FROM runs r WHERE r.run_id = :'run') t1)
   || (SELECT coalesce(string_agg(x, '|' ORDER BY x), '') FROM (
         SELECT s.step_id::text || s.status || s.attempt_count ||
                coalesce(s.completed_at::text, '') || coalesce(md5(s.output::text), '') AS x
           FROM steps s WHERE s.run_id = :'run') t2)
   || (SELECT coalesce(string_agg(x, '|' ORDER BY x), '') FROM (
         SELECT a.attempt_id::text || a.status || coalesce(a.failure_reason, '') ||
                coalesce(a.finished_at::text, '') || a.dispatched_by AS x
           FROM attempts a WHERE a.run_id = :'run') t3)
   || (SELECT coalesce(string_agg(x, '|' ORDER BY x), '') FROM (
         SELECT d.dlq_id::text || d.reason || d.replay_round ||
                coalesce(d.step_id::text, '') AS x
           FROM dead_letter_queue d WHERE d.run_id = :'run') t4));`

// fixture drives all five legs, in an order chosen so that one crash serves two
// of them: the DLQ'd run of leg 2 is created BEFORE leg 1 kills the process, so
// that the same restart which resumes one run must also leave the other alone.
func fixture() error {
	if err := harness.WaitHealthy(harness.HealthTimeout); err != nil {
		return err
	}
	takeover, err := harness.TakeoverBudget()
	if err != nil {
		return err
	}

	// --- leg 2, first half: a run that is already dead before the crash ----
	if obs.dlqRun, err = harness.Begin("workflow-dlq.json"); err != nil {
		return fmt.Errorf("leg 2: %w", err)
	}
	final, err := harness.WaitTerminal(obs.dlqRun, harness.RunTimeout)
	if err != nil {
		return fmt.Errorf("leg 2: %w", err)
	}
	if final != "DLQ" {
		return fmt.Errorf("leg 2: the fixture needs a DLQ'd run to protect; this run reached %q", final)
	}
	if obs.dlqDigestBefore, err = harness.PSQL(runDigest, harness.Var(obs.dlqRun)); err != nil {
		return fmt.Errorf("leg 2: %w", err)
	}

	// --- leg 1: SIGKILL while a step is in flight --------------------------
	if obs.crashRun, err = harness.Begin("workflow-survives-crash.json"); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	if obs.dispatcherBeforeKill, err = harness.WaitAttemptRunning(
		obs.crashRun, "during-the-crash", harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	// The first step is necessarily DONE by now: the planner produces steps one
	// at a time (SPEC.md 4.2), so a second step cannot have an attempt until the
	// first has finished.
	if obs.doneStepBefore, err = harness.PSQL(stepDigest,
		harness.Var(obs.crashRun), "step=before-the-crash"); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	if obs.ownerBeforeKill, err = harness.PSQL(
		"SELECT coalesce(owner_id, '') FROM runs WHERE run_id = :'run';",
		harness.Var(obs.crashRun)); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}

	if err = harness.Kill(); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	// Read immediately, while nothing is running. Nothing else CAN write this
	// column now: SPEC.md 8.7 names four writers of coordination metadata and
	// all four live in an orchestrator process.
	if obs.ownerAfterKill, err = harness.PSQL(
		"SELECT coalesce(owner_id, '') FROM runs WHERE run_id = :'run';",
		harness.Var(obs.crashRun)); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}

	if err = harness.Start(); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	if obs.crashFinal, err = harness.WaitTerminal(obs.crashRun, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}
	if obs.doneStepAfter, err = harness.PSQL(stepDigest,
		harness.Var(obs.crashRun), "step=before-the-crash"); err != nil {
		return fmt.Errorf("leg 1: %w", err)
	}

	// --- leg 2, second half: the same crash must not have touched the DLQ --
	if obs.dlqDigestAfter, err = harness.PSQL(runDigest, harness.Var(obs.dlqRun)); err != nil {
		return fmt.Errorf("leg 2: %w", err)
	}

	// --- leg 3: a clean shutdown releases ownership ------------------------
	if obs.cleanRun, err = harness.Begin("workflow-clean-shutdown.json"); err != nil {
		return fmt.Errorf("leg 3: %w", err)
	}
	if _, err = harness.WaitAttemptRunning(
		obs.cleanRun, "running-when-asked-to-stop", harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 3: %w", err)
	}
	if err = harness.StopClean(); err != nil {
		return fmt.Errorf("leg 3: %w", err)
	}
	// No wait, deliberately. SPEC.md 8.7's release happens IN the shutdown, so
	// once the process has exited the column is already NULL. Waiting for it
	// would hide the difference between a release and a lease expiry, which is
	// the entire point of this leg.
	if obs.ownerAfterCleanStop, err = harness.PSQL(
		"SELECT coalesce(owner_id, '') FROM runs WHERE run_id = :'run';",
		harness.Var(obs.cleanRun)); err != nil {
		return fmt.Errorf("leg 3: %w", err)
	}
	if err = harness.Start(); err != nil {
		return fmt.Errorf("leg 3: %w", err)
	}
	if obs.cleanFinal, err = harness.WaitTerminal(obs.cleanRun, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 3: %w", err)
	}

	// --- leg 4: a crash loop converges rather than spinning -----------------
	//
	// SPEC.md 13.1.4: "a crash loop. Each pass burns one unit of budget, so
	// every in-flight step converges to DLQ." The step is killed on every
	// attempt it is ever given, so if the budget were NOT burned by a crash
	// this loop would never end - which is what makes the assertion meaningful.
	if obs.loopRun, err = harness.Begin("workflow-crash-loop.json"); err != nil {
		return fmt.Errorf("leg 4: %w", err)
	}
	for i := 1; i <= maxAttempts; i++ {
		// Wait for attempt number i specifically, not merely for "an attempt is
		// RUNNING". After the previous kill the previous attempt is still
		// RUNNING in the database - nothing expired it - so the weaker wait
		// would return the instant the process booted, and the kill would land
		// on an orchestrator that had not yet claimed the run, dispatched
		// anything, or burned a single unit of budget.
		if _, err = harness.WaitAttemptNoRunning(
			obs.loopRun, "killed-every-time", i, takeover); err != nil {
			return fmt.Errorf("leg 4, kill %d: %w", i, err)
		}
		if err = harness.Kill(); err != nil {
			return fmt.Errorf("leg 4, kill %d: %w", i, err)
		}
		obs.loopKills++
		if err = harness.Start(); err != nil {
			return fmt.Errorf("leg 4, restart %d: %w", i, err)
		}
	}
	if obs.loopFinal, err = harness.WaitTerminal(obs.loopRun, harness.RunTimeout); err != nil {
		return fmt.Errorf("leg 4: %w", err)
	}

	// --- leg 5: storage unreachable at startup ------------------------------
	obs.badBootOutput, obs.badBootExit, err = harness.BootWith("/etc/piton/piton-badstorage.yaml")
	if err != nil {
		return fmt.Errorf("leg 5: %w", err)
	}

	fmt.Printf("beta/recovery: crash=%s(%s) dlq=%s clean=%s(%s) loop=%s(%s, %d kills) badboot=exit %d\n",
		obs.crashRun, obs.crashFinal, obs.dlqRun, obs.cleanRun, obs.cleanFinal,
		obs.loopRun, obs.loopFinal, obs.loopKills, obs.badBootExit)
	return nil
}
