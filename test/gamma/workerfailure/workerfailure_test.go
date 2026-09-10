package workerfailure

import (
	"fmt"
	"testing"

	"github.com/aaronwu001/piton/test/gamma/harness"
)

// Every assertion below names the SPEC.md section it came from. CLAUDE.md 5.1
// permits exactly one source for a test, and this file has no other: nothing
// here was derived by reading internal/, and where SPEC.md is silent the
// assertion is absent rather than invented.

// ---------------------------------------------------------------------------
// Leg 1 - the recovering retry (SPEC.md 18.2)
// ---------------------------------------------------------------------------

// TestRetryReachesDone is the half of "retries" that is not a dead letter: a
// worker that fails and then succeeds leaves a run in DONE.
func TestRetryReachesDone(t *testing.T) {
	if legRetry.Final != "DONE" {
		t.Fatalf("SPEC.md 18.2 leg 1: the run must reach DONE, it reached %q", legRetry.Final)
	}
	harness.Bool(t, "SPEC.md 18.2 leg 1: SELECT status FROM runs -- DONE",
		"SELECT status = 'DONE' FROM runs WHERE run_id = :'run';", legRetry.Var())
}

// TestRetryStepIsDoneOnTheLastAttemptTheBudgetAllows is where SPEC.md 11.1 is
// checked against the implementation rather than only stated: step_max_attempts
// is a TOTAL attempt count, so a worker that fails twice under a budget of
// three has exactly one dispatch left, and the step must be DONE with
// attempt_count equal to the whole budget.
func TestRetryStepIsDoneOnTheLastAttemptTheBudgetAllows(t *testing.T) {
	harness.Bool(t, "SPEC.md 18.2 leg 1: exactly one step",
		"SELECT count(*) = 1 FROM steps WHERE run_id = :'run';", legRetry.Var())
	harness.Bool(t,
		fmt.Sprintf("SPEC.md 18.2 leg 1 and 11.1: the step is DONE with attempt_count = %d, "+
			"which is step_max_attempts -- a total attempt count, not a retry count", maxAttempts),
		fmt.Sprintf("SELECT status = 'DONE' AND attempt_count = %d FROM steps WHERE run_id = :'run';", maxAttempts),
		legRetry.Var())
}

// TestRetryAttemptSequence asserts the attempt history SPEC.md 18.2 prints:
// every attempt but the last FAILED, the last DONE.
func TestRetryAttemptSequence(t *testing.T) {
	harness.Bool(t,
		fmt.Sprintf("SPEC.md 18.2 leg 1: %d attempts exist", maxAttempts),
		fmt.Sprintf("SELECT count(*) = %d FROM attempts WHERE run_id = :'run';", maxAttempts),
		legRetry.Var())
	harness.Bool(t,
		"SPEC.md 18.2 leg 1: every attempt before the last FAILED",
		fmt.Sprintf("SELECT bool_and(status = 'FAILED') FROM attempts "+
			"WHERE run_id = :'run' AND attempt_no < %d;", maxAttempts),
		legRetry.Var())
	harness.Bool(t,
		"SPEC.md 18.2 leg 1: the last attempt is DONE",
		fmt.Sprintf("SELECT status = 'DONE' FROM attempts "+
			"WHERE run_id = :'run' AND attempt_no = %d;", maxAttempts),
		legRetry.Var())
	// SPEC.md 6.4: attempt_no is 1-based ordering within the step. A gap or a
	// repeat would make "the last attempt" mean something other than what the
	// two assertions above just tested.
	harness.Bool(t,
		"SPEC.md 6.4: attempt_no is 1-based and contiguous within the step",
		fmt.Sprintf("SELECT array_agg(attempt_no ORDER BY attempt_no) = "+
			"(SELECT array_agg(g) FROM generate_series(1, %d) g) "+
			"FROM attempts WHERE run_id = :'run';", maxAttempts),
		legRetry.Var())
}

// TestRetryLeavesNoDeadLetter: a recovered run is not a dead letter, and
// SPEC.md 18.2 requires the count to be zero rather than merely unexamined.
func TestRetryLeavesNoDeadLetter(t *testing.T) {
	harness.Bool(t, "SPEC.md 18.2 leg 1: SELECT count(*) FROM dead_letter_queue -- 0",
		"SELECT count(*) = 0 FROM dead_letter_queue WHERE run_id = :'run';", legRetry.Var())
}

// TestRetryStepCarriesOutput: SPEC.md 6.3 sets steps.output when status is
// DONE. The two failed attempts must not have cost the run its result.
func TestRetryStepCarriesOutput(t *testing.T) {
	harness.Bool(t, "SPEC.md 6.3: output is set when a step is DONE",
		"SELECT output IS NOT NULL FROM steps WHERE run_id = :'run';", legRetry.Var())
}

// ---------------------------------------------------------------------------
// Legs 2, 3 and 4 - worker-side DLQ (SPEC.md 18.2, 12.3 L4)
// ---------------------------------------------------------------------------

// TestWorkerSideDLQ walks the three legs that exhaust their budget. They differ
// in exactly one thing - the SPEC.md 5.3 value their attempts carry - which is
// why they are one table and not three tests.
func TestWorkerSideDLQ(t *testing.T) {
	for _, leg := range dlqLegs {
		t.Run(leg.Leg.File, func(t *testing.T) {
			if leg.Leg.Final != "DLQ" {
				t.Fatalf("SPEC.md 18.2: %s must reach DLQ, it reached %q", leg.Leg.Name, leg.Leg.Final)
			}
			v := leg.Leg.Var()

			harness.Bool(t, "SPEC.md 18.2: SELECT status FROM runs -- DLQ",
				"SELECT status = 'DLQ' FROM runs WHERE run_id = :'run';", v)

			// SPEC.md 6.2 and 8.7: ownership is released when a run leaves
			// RUNNING. A DLQ run still holding owner_id would be swept forever.
			harness.Bool(t, "SPEC.md 18.2, 6.2, 8.7: owner_id and claimed_at are both NULL",
				"SELECT owner_id IS NULL AND claimed_at IS NULL FROM runs WHERE run_id = :'run';", v)

			harness.Bool(t,
				fmt.Sprintf("SPEC.md 18.2: the failing step is DLQ with attempt_count = %d", maxAttempts),
				fmt.Sprintf("SELECT status = 'DLQ' AND attempt_count = %d "+
					"FROM steps WHERE run_id = :'run' AND seq = 1;", maxAttempts),
				v)

			harness.Bool(t,
				fmt.Sprintf("SPEC.md 18.2: %d attempts, all FAILED", maxAttempts),
				fmt.Sprintf("SELECT count(*) = %d AND bool_and(status = 'FAILED') "+
					"FROM attempts WHERE run_id = :'run';", maxAttempts),
				v)

			harness.Bool(t,
				fmt.Sprintf("SPEC.md 5.3 and 18.2: every attempt carries failure_reason = %s", leg.Reason),
				"SELECT bool_and(failure_reason = :'reason') FROM attempts WHERE run_id = :'run';",
				v, "reason="+leg.Reason)

			// SPEC.md 6.4 invariants 1 and 3, which the backend enforces. A
			// FAILED row with no reason is a dead end for the operator, and
			// SPEC.md 17 promises the database explains itself.
			harness.Bool(t, "SPEC.md 6.4 invariant 1: a FAILED attempt names a reason",
				"SELECT count(*) = 0 FROM attempts "+
					"WHERE run_id = :'run' AND status = 'FAILED' AND failure_reason IS NULL;", v)
			harness.Bool(t, "SPEC.md 6.4 invariant 3: finished_at is present iff the attempt left RUNNING",
				"SELECT count(*) = 0 FROM attempts "+
					"WHERE run_id = :'run' AND (status = 'RUNNING') <> (finished_at IS NULL);", v)

			// SPEC.md 9.7: sync + envelope is the only legal combination before
			// theta and epsilon, and SPEC.md 6.4 copies connection_mode onto the
			// attempt because the claim path branches on it in SQL.
			harness.Bool(t, "SPEC.md 9.7, 6.4: every attempt is sync",
				"SELECT bool_and(connection_mode = 'sync') FROM attempts WHERE run_id = :'run';", v)

			// SPEC.md 6.3: a step is complete when status says so. A DLQ step
			// never ran to success, so it holds no output.
			harness.Bool(t, "SPEC.md 6.3: a DLQ step carries no output",
				"SELECT output IS NULL FROM steps WHERE run_id = :'run' AND seq = 1;", v)
		})
	}
}

// TestNoStepIsCreatedAfterTheFailingOne is SPEC.md 18.2 "no step created after
// it", and it is evidence rather than a tautology because each of these
// workflows declares a second static step that the planner would have been
// asked for had the run continued. SPEC.md 4.2: the planner is asked only from
// L1, and a run that has gone to DLQ is never scanned again (SPEC.md 5.5).
func TestNoStepIsCreatedAfterTheFailingOne(t *testing.T) {
	for _, leg := range dlqLegs {
		t.Run(leg.Leg.File, func(t *testing.T) {
			specs, err := harness.StaticSteps(leg.Leg.File)
			if err != nil {
				t.Fatal(err)
			}
			if len(specs) < 2 {
				t.Fatalf("%s must declare a second static step for this test to mean anything; it declares %d",
					leg.Leg.File, len(specs))
			}
			harness.Bool(t,
				fmt.Sprintf("SPEC.md 18.2: exactly one step exists, though %s declares %d",
					leg.Leg.File, len(specs)),
				"SELECT count(*) = 1 FROM steps WHERE run_id = :'run';", leg.Leg.Var())
		})
	}
}

// TestDeadLetterEntry checks what SPEC.md 12.4 and 6.5 require an entry to
// record, for each of the three legs.
func TestDeadLetterEntry(t *testing.T) {
	for _, leg := range dlqLegs {
		t.Run(leg.Leg.File, func(t *testing.T) {
			v := leg.Leg.Var()

			// SPEC.md 12.4: a run accumulates one entry per round it lands in
			// DLQ, and gamma runs one round.
			harness.Bool(t, "SPEC.md 12.4: exactly one dead-letter entry",
				"SELECT count(*) = 1 FROM dead_letter_queue WHERE run_id = :'run';", v)

			// SPEC.md 12.3: worker-side is worker_budget_exhausted and names
			// the step; step_id IS NULL is what distinguishes the planner side,
			// which gamma does not demonstrate.
			harness.Bool(t, "SPEC.md 12.3 L4: reason is worker_budget_exhausted and step_id names the failed step",
				"SELECT reason = 'worker_budget_exhausted' AND step_id = "+
					"(SELECT step_id FROM steps WHERE run_id = :'run' AND seq = 1) "+
					"FROM dead_letter_queue WHERE run_id = :'run';", v)

			// SPEC.md 6.5: replay_round is runs.replay_count at the moment of
			// the verdict.
			harness.Bool(t, "SPEC.md 6.5: replay_round = 0",
				"SELECT replay_round = 0 FROM dead_letter_queue WHERE run_id = :'run';", v)

			// The budget the round consumed is no longer copied onto the
			// entry. SPEC.md 6.5: "replay_round on every attempt and every
			// planner call now carries that history on the rows themselves, so
			// the number is a count rather than a copy - and a copy of a
			// counter is one more thing that can disagree with the rows it
			// summarises." So the same claim is made of the rows that hold it.
			harness.Bool(t,
				fmt.Sprintf("SPEC.md 6.4, 6.5: the round that ended in this entry burned %d attempts",
					maxAttempts),
				fmt.Sprintf(`SELECT count(*) = %d FROM attempts a
                               JOIN dead_letter_queue d ON d.step_id = a.step_id
                              WHERE d.run_id = :'run' AND a.replay_round = d.replay_round;`,
					maxAttempts),
				v)

			// SPEC.md 12.4: error_text records the most recent failure, and
			// SPEC.md 6.4 truncates it to 4 KB - one limit, both tables.
			harness.Bool(t, "SPEC.md 17, 12.4: the entry explains itself in at most 4 KB",
				"SELECT length(error_text) > 0 AND octet_length(error_text) <= 4096 "+
					"FROM dead_letter_queue WHERE run_id = :'run';", v)
		})
	}
}

// TestUnreachableIsTransportErrorNotTimeout is leg 4 own assertion, and the
// only place SPEC.md 5.3 clock rule can be tested: "an attempt is timeout ONLY
// if deadline_at has passed; a connection refused at second 3 of a 300-second
// budget is transport_error". Nothing ever answered this worker, which is
// exactly the case an implementation is tempted to label timeout.
func TestUnreachableIsTransportErrorNotTimeout(t *testing.T) {
	leg := dlqLegs[2].Leg
	if leg.File != "workflow-unreachable.json" {
		t.Fatalf("this test is about leg 4; the table gives %s", leg.File)
	}
	harness.Bool(t, "SPEC.md 5.3: nothing answered, yet no attempt is labelled timeout or orphaned",
		"SELECT count(*) = 0 FROM attempts "+
			"WHERE run_id = :'run' AND failure_reason IN ('timeout', 'orphaned');", leg.Var())
	harness.Bool(t, "SPEC.md 5.3: every attempt finished well inside its deadline",
		"SELECT bool_and(finished_at < deadline_at) FROM attempts WHERE run_id = :'run';", leg.Var())
}

// ---------------------------------------------------------------------------
// Across the legs
// ---------------------------------------------------------------------------

// TestBudgetIsBurnedAtDispatch: SPEC.md 4.2 increments steps.attempt_count when
// an attempt is DISPATCHED, not when its outcome is written, so within one
// replay round the counter and the number of attempt rows are equal. SPEC.md
// 6.3 names the only two things that make them differ - cancellation (5.7) and
// replay (14) - and gamma performs neither.
func TestBudgetIsBurnedAtDispatch(t *testing.T) {
	all := append([]*harness.Leg{legRetry}, legsOfDLQ()...)
	for _, leg := range all {
		t.Run(leg.File, func(t *testing.T) {
			harness.Bool(t,
				"SPEC.md 4.2, 6.3: attempt_count equals the number of attempt rows in a round "+
					"with no cancel and no replay",
				"SELECT s.attempt_count = (SELECT count(*) FROM attempts a WHERE a.step_id = s.step_id) "+
					"FROM steps s WHERE s.run_id = :'run' AND s.seq = 1;", leg.Var())
		})
	}
}

// TestEveryAttemptNamesItsOrchestrator: SPEC.md 6.4 makes dispatched_by the
// orchestrator_id that dispatched the attempt. It is what tells the operator,
// at a terminal, which process a failure belongs to.
func TestEveryAttemptNamesItsOrchestrator(t *testing.T) {
	all := append([]*harness.Leg{legRetry}, legsOfDLQ()...)
	for _, leg := range all {
		t.Run(leg.File, func(t *testing.T) {
			harness.Bool(t, "SPEC.md 6.4: every attempt names the orchestrator that dispatched it",
				"SELECT bool_and(dispatched_by IS NOT NULL AND dispatched_by <> '') "+
					"FROM attempts WHERE run_id = :'run';", leg.Var())
		})
	}
}

// TestImpossibleCombinationNeverPersisted: SPEC.md 5.6 calls run = RUNNING with
// last_step = DLQ impossible, "because of a transaction boundary, not because of
// a check somewhere" - SPEC.md 12.2 writes the step, the run and the dead-letter
// entry in one transaction. Gamma is the first milestone that produces any DLQ
// step at all, so it is the first that could ever have observed the violation.
func TestImpossibleCombinationNeverPersisted(t *testing.T) {
	harness.Bool(t, "SPEC.md 5.6, 12.2: no run is RUNNING with a DLQ last step",
		"SELECT count(*) = 0 FROM runs r WHERE r.status = 'RUNNING' AND "+
			"(SELECT s.status FROM steps s WHERE s.run_id = r.run_id ORDER BY s.seq DESC LIMIT 1) = 'DLQ';")
	harness.Bool(t, "SPEC.md 5.5 L4: a worker-side DLQ run has a DLQ last step",
		"SELECT bool_and((SELECT s.status FROM steps s WHERE s.run_id = r.run_id "+
			"ORDER BY s.seq DESC LIMIT 1) = 'DLQ') FROM runs r WHERE r.status = 'DLQ';")
}

// TestPlannerBudgetUntouched: SPEC.md 12.1 states that the budget rules apply to
// every planner including the static one, and that the static planner simply
// cannot fail at run time, so planner_attempt_count never leaves 0. Gamma is
// where that is worth testing: three of its four runs died, and none of them
// died on the planner side.
func TestPlannerBudgetUntouched(t *testing.T) {
	harness.Bool(t, "SPEC.md 12.1: the static planner cannot fail, so planner_attempt_count stays 0",
		"SELECT bool_and(planner_attempt_count = 0) FROM runs;")
	harness.Bool(t, "SPEC.md 12.3: gamma produces no planner-side entry -- every entry names a step",
		"SELECT count(*) = 0 FROM dead_letter_queue WHERE step_id IS NULL;")
}

// TestWhatGammaDoesNotDemonstrate asserts the absence of everything SPEC.md 18.2
// excludes, where that absence is observable. An unasserted absence is how a
// milestone quietly acquires the next one behaviour.
func TestWhatGammaDoesNotDemonstrate(t *testing.T) {
	harness.Bool(t, "SPEC.md 14 is milestone delta: nothing has been replayed",
		"SELECT bool_and(replay_count = 0) FROM runs;")
	harness.Bool(t, "SPEC.md 15 is milestone iota: nothing has been cancelled",
		"SELECT count(*) = 0 FROM runs WHERE status = 'CANCELLED';")
	harness.Bool(t, "SPEC.md 5.3: no attempt was orphaned -- beta is where an orchestrator dies",
		"SELECT count(*) = 0 FROM attempts WHERE failure_reason = 'orphaned';")
	harness.Bool(t, "SPEC.md 18.2: four runs, one DONE and three DLQ",
		"SELECT count(*) = 4 AND count(*) FILTER (WHERE status = 'DONE') = 1 "+
			"AND count(*) FILTER (WHERE status = 'DLQ') = 3 FROM runs;")
}

// legsOfDLQ is the three DLQ legs, without their expected reasons, for the
// tests that hold over every leg alike.
func legsOfDLQ() []*harness.Leg {
	out := make([]*harness.Leg, 0, len(dlqLegs))
	for _, l := range dlqLegs {
		out = append(out, l.Leg)
	}
	return out
}
