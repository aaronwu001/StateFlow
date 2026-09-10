package planner

import (
	"fmt"
	"testing"

	"github.com/aaronwu001/piton/test/zeta/harness"
)

// The assertions below are grouped by leg. Each one names the SPEC.md rule it
// comes from, so a failure says which rule broke and not merely which query
// returned false.
//
// WHAT CHANGED WHEN THE PLANNER CALL BECAME AN ENTITY
//
//	This suite was first written against a SPEC.md in which the dead-letter
//	entry named the KIND of planner failure - one reason per kind, and a rule
//	for what to call a set of failures that were not all of one kind. SPEC.md
//	3.2, 5.8 and 6.8 replaced that with a row per call. So the assertions moved
//	rather than weakened: the kind is now asserted on the call that had it, at
//	the moment it had it, and the entry is asserted to say only which of three
//	situations stopped the run (SPEC.md 6.5).
//
//	Two of them got sharper in the move. SPEC.md 5.8's clock rule can now be
//	tested at all - a planner that answers 500 instantly and a planner that
//	sleeps past its deadline used to produce one indistinguishable reason, and
//	now produce transport_error and timeout on their own rows. And the mixed
//	fixture, which existed only to trigger the rule that is now deleted, tests
//	something real instead: two kinds of failure inside one decision point, each
//	on its own row.

// ---------------------------------------------------------------------------
// Leg 1 - the planner decides from what a step actually produced
// ---------------------------------------------------------------------------

// TestBothDynamicRunsFinished is the floor the rest of leg 1 stands on: an
// external service answered SPEC.md 9.3's `continue` twice and `done` once for
// each run, and SPEC.md 5.5's L3 was reached.
func TestBothDynamicRunsFinished(t *testing.T) {
	if obs.dynamicFinalA != "DONE" || obs.dynamicFinalB != "DONE" {
		t.Fatalf("SPEC.md 4.2, 9.3: a run whose planner answers continue, continue, done reaches "+
			"DONE; these reached %q and %q", obs.dynamicFinalA, obs.dynamicFinalB)
	}
	for _, run := range []string{obs.dynamicRunA, obs.dynamicRunB} {
		harness.Bool(t, "SPEC.md 5.5 L3: the run is DONE and so is its last step",
			`SELECT r.status = 'DONE' AND
                    (SELECT s.status FROM steps s WHERE s.run_id = r.run_id ORDER BY s.seq DESC LIMIT 1) = 'DONE'
               FROM runs r WHERE r.run_id = :'run';`, harness.Var(run))
	}
}

// TestTheTwoRunsAreTheSameQuestion establishes what makes the next assertion an
// argument rather than an observation: same workflow row, same input bytes. If
// these two runs differed in anything the planner can see in SPEC.md 9.2's
// request, a difference in their paths would prove nothing.
func TestTheTwoRunsAreTheSameQuestion(t *testing.T) {
	harness.Bool(t, "leg 1's premise: both runs belong to one workflow and carry identical input",
		`SELECT a.workflow_id = b.workflow_id AND a.input::text = b.input::text
           FROM runs a, runs b WHERE a.run_id = :'runa' AND b.run_id = :'runb';`,
		"runa="+obs.dynamicRunA, "runb="+obs.dynamicRunB)
}

// TestIdenticalRunsTookDifferentPaths is milestone zeta's central claim.
//
// SPEC.md 6.1's static planner answers "planner_static_steps[n] where n is the
// number of steps the run already has" - a function of the step count alone, so
// two identical runs are one answer twice, always. An external planner is not
// restricted that way, and here it is not: the second step of these two runs
// differs, and nothing in SPEC.md 9.2's request differs between them.
func TestIdenticalRunsTookDifferentPaths(t *testing.T) {
	harness.Bool(t, "SPEC.md 9.3: the planner chose a different second step for each run",
		`SELECT (SELECT step_name FROM steps WHERE run_id = :'runa' AND seq = 2)
             <> (SELECT step_name FROM steps WHERE run_id = :'runb' AND seq = 2);`,
		"runa="+obs.dynamicRunA, "runb="+obs.dynamicRunB)
	for _, run := range []string{obs.dynamicRunA, obs.dynamicRunB} {
		harness.Bool(t, "SPEC.md 6.3: the run holds exactly the two steps the planner asked for, "+
			"numbered from 1 and contiguous",
			`SELECT count(*) = 2 AND min(seq) = 1 AND max(seq) = 2 AND count(DISTINCT seq) = 2
               FROM steps WHERE run_id = :'run';`, harness.Var(run))
	}
}

// TestTheBranchFollowsWhatTheWorkerProduced closes the one gap the previous
// test leaves: two different paths could in principle be two arbitrary answers.
// They are not - each run's second step is the one its FIRST step's stored
// output names.
//
// SPEC.md 9.6 fixes what is stored per mode: in envelope mode the step's output
// is the response's `output` field alone, because "status and output are Piton's
// protocol, not the worker's data". So the kind sits at the top of the stored
// document, and a planner reading it strips nothing.
func TestTheBranchFollowsWhatTheWorkerProduced(t *testing.T) {
	for _, run := range []string{obs.dynamicRunA, obs.dynamicRunB} {
		harness.Bool(t, "SPEC.md 9.2, 10.2: the second step is the one the first step's output names",
			`SELECT (SELECT step_name FROM steps WHERE run_id = :'run' AND seq = 2)
                  = 'extract-' || (SELECT output->>'kind' FROM steps
                                    WHERE run_id = :'run' AND seq = 1);`, harness.Var(run))
	}
}

// TestThePlannerReadTheOutputThroughTheReadAPI is the same fact read from the
// other side.
//
// SPEC.md 9.2: "history is a catalogue only. It never carries outputs - that is
// what output_bytes is for. A planner that wants an output fetches it from the
// read API." The database can only show that the branch matched the output; the
// fixture's own counter shows the fetch that made it possible.
func TestThePlannerReadTheOutputThroughTheReadAPI(t *testing.T) {
	for _, run := range []string{obs.dynamicRunA, obs.dynamicRunB} {
		if got := obs.statsAfterDynamic.FetchesFor(run); got < 1 {
			t.Errorf("SPEC.md 9.2, 10.2: the planner must fetch a step's output to decide, and the "+
				"fixture recorded %d fetches for run %s", got, run)
		}
	}
}

// TestEveryDecisionIsOnTheRecord is SPEC.md 4.2's new closing rule: "every one
// of those four rows also writes the call itself into planner_calls, in the
// same transaction - the first three as DONE with answer set to what the
// planner said".
//
// A run with two steps that reached DONE was asked three times: twice for the
// steps, once more for the `done` that ended it.
func TestEveryDecisionIsOnTheRecord(t *testing.T) {
	for _, run := range []string{obs.dynamicRunA, obs.dynamicRunB} {
		harness.Bool(t, "SPEC.md 4.2, 6.8: three calls, all DONE - continue, continue, done",
			`SELECT count(*) = 3
                    AND count(*) FILTER (WHERE status = 'DONE') = 3
                    AND count(*) FILTER (WHERE answer = 'continue') = 2
                    AND count(*) FILTER (WHERE answer = 'done') = 1
               FROM planner_calls WHERE run_id = :'run';`, harness.Var(run))
		harness.Bool(t, "SPEC.md 6.8: call_no is 1-based and contiguous within the run",
			`SELECT count(*) = 0 FROM (
                 SELECT call_no, row_number() OVER (ORDER BY call_no) AS expected
                   FROM planner_calls WHERE run_id = :'run') t
              WHERE t.call_no <> t.expected;`, harness.Var(run))
		harness.Bool(t, "SPEC.md 6.8: a run that was never replayed has every call in round 0",
			"SELECT bool_and(replay_round = 0) FROM planner_calls WHERE run_id = :'run';",
			harness.Var(run))
	}
}

// TestSuccessfulPlannerCallsBurnNoBudget: SPEC.md 6.2 makes
// planner_attempt_count "failed planner calls at the current decision point.
// Reset to 0 by any successful planner call", and SPEC.md 4.2's `continue` row
// resets the planner budget in the same transaction that inserts the step.
func TestSuccessfulPlannerCallsBurnNoBudget(t *testing.T) {
	for _, run := range []string{obs.dynamicRunA, obs.dynamicRunB} {
		harness.Bool(t, "SPEC.md 6.2: a run whose planner never failed carries no burnt budget",
			"SELECT planner_attempt_count = 0 FROM runs WHERE run_id = :'run';", harness.Var(run))
		harness.Bool(t, "SPEC.md 6.8: and no call of it is FAILED",
			"SELECT count(*) = 0 FROM planner_calls WHERE run_id = :'run' AND status = 'FAILED';",
			harness.Var(run))
	}
}

// TestTheStepSpecIsStoredAsThePlannerReturnedIt: SPEC.md 6.3 defines
// steps.decision as "the StepSpec exactly as the planner returned it", and
// SPEC.md 4.2's `continue` row says the step is inserted "storing the StepSpec
// verbatim". The columns beside it must therefore agree with what is inside it.
func TestTheStepSpecIsStoredAsThePlannerReturnedIt(t *testing.T) {
	for _, run := range []string{obs.dynamicRunA, obs.dynamicRunB} {
		harness.Bool(t, "SPEC.md 6.3: every step's decision holds the StepSpec it was created from",
			`SELECT count(*) = 0 FROM steps
              WHERE run_id = :'run'
                AND (decision->>'step_name' IS DISTINCT FROM step_name
                     OR decision->>'worker_url' <> 'http://worker:9090/work');`, harness.Var(run))
		harness.Bool(t, "SPEC.md 9.7: sync + envelope is the only combination legal before theta "+
			"and epsilon, and it is what the planner asked for",
			`SELECT count(*) = 0 FROM steps
              WHERE run_id = :'run'
                AND (decision->>'connection_mode' <> 'sync'
                     OR decision->>'dispatch_style' <> 'envelope');`, harness.Var(run))
	}
}

// ---------------------------------------------------------------------------
// Leg 2 - both planner types, side by side
// ---------------------------------------------------------------------------

// TestAStaticWorkflowRunsBesideAnHTTPOne is R40-f's ruling executed: "switching
// between planners" is the choice made at POST /workflows, so the thing to show
// is that both choices work in one system against one set of workers.
func TestAStaticWorkflowRunsBesideAnHTTPOne(t *testing.T) {
	if obs.staticFinal != "DONE" {
		t.Fatalf("SPEC.md 6.1: a static workflow walks its array and reaches DONE; it reached %q",
			obs.staticFinal)
	}
	harness.Bool(t, "SPEC.md 6.1: the static run executed both steps of its array",
		"SELECT count(*) = 2 FROM steps WHERE run_id = :'run';", harness.Var(obs.staticRun))
}

// TestTheStaticPlannerIsOnTheRecordToo is SPEC.md 12.1's no-exemption rule made
// visible: "these rules apply to every planner, including the built-in static
// one, with no exemption ... an implementation that special-cases the static
// planner out of the budget path has added a branch to work around a situation
// that cannot occur, and that branch will outlive the reason for it."
//
// So the static planner's answers are rows like any other planner's - three of
// them for a two-step workflow - and none of them can be FAILED, because
// SPEC.md 12.1 also says it "holds no state and makes no network call".
func TestTheStaticPlannerIsOnTheRecordToo(t *testing.T) {
	harness.Bool(t, "SPEC.md 4.2, 12.1: the static planner was asked three times and answered every time",
		`SELECT count(*) = 3
                AND count(*) FILTER (WHERE status = 'FAILED') = 0
                AND count(*) FILTER (WHERE answer = 'continue') = 2
                AND count(*) FILTER (WHERE answer = 'done') = 1
           FROM planner_calls WHERE run_id = :'run';`, harness.Var(obs.staticRun))
	harness.Bool(t, "SPEC.md 12.1: and its budget never left 0",
		"SELECT planner_attempt_count = 0 FROM runs WHERE run_id = :'run';", harness.Var(obs.staticRun))
}

// TestThePlannerChoiceIsFixedAtCreation asserts SPEC.md 6.1's invariant on the
// two rows the previous legs created: the http workflow carries planner_url and
// fetch_base_url and no static steps; the static workflow carries the array and
// neither URL.
func TestThePlannerChoiceIsFixedAtCreation(t *testing.T) {
	harness.Bool(t, "SPEC.md 6.1: an http workflow carries planner_url and fetch_base_url, and no array",
		`SELECT planner_type = 'http' AND planner_url IS NOT NULL AND fetch_base_url IS NOT NULL
                AND planner_static_steps IS NULL
           FROM workflows WHERE workflow_id = :'wf';`, "wf="+obs.dynamicWorkflow)
	harness.Bool(t, "SPEC.md 6.1: a static workflow carries the array, and neither URL",
		`SELECT planner_type = 'static' AND planner_url IS NULL AND fetch_base_url IS NULL
                AND planner_static_steps IS NOT NULL
           FROM workflows WHERE workflow_id = :'wf';`, "wf="+obs.staticWorkflow)
}

// ---------------------------------------------------------------------------
// Legs 3 to 6 - the planner-side dead-letter queue
// ---------------------------------------------------------------------------

// TestEveryPlannerFailureLandsAtL5 is SPEC.md 12.3's right-hand column, read
// off the combination table rather than off the dead-letter entry: "the branch
// is derived from current state, never from a dead-letter entry".
//
// SPEC.md 5.4 supplies the missing half - "a run with no steps is defined to
// have last_step = DONE" - which is why a run that died at its first decision
// point is L5 rather than a state of its own.
func TestEveryPlannerFailureLandsAtL5(t *testing.T) {
	for _, file := range dlqFiles {
		t.Run(file, func(t *testing.T) {
			harness.Bool(t, "SPEC.md 5.5 L5, 5.4: run = DLQ and last_step = DONE",
				`SELECT r.status = 'DLQ' AND
                        coalesce((SELECT s.status FROM steps s WHERE s.run_id = r.run_id
                                   ORDER BY s.seq DESC LIMIT 1), 'DONE') = 'DONE'
                   FROM runs r WHERE r.run_id = :'run';`, harness.Var(obs.dlqRun[file]))
		})
	}
}

// TestTheEntrySaysWhichSituationStoppedTheRun is SPEC.md 6.5 after the
// amendment: "reason says which of three situations stopped the run. It does
// not say why any individual exchange failed."
//
// Six of these seven fixtures fail their calls in different ways and all reach
// the same entry, which is the point: the entry is about the RUN, and the kind
// of failure is a fact about a CALL.
func TestTheEntrySaysWhichSituationStoppedTheRun(t *testing.T) {
	for _, file := range dlqFiles {
		want := "planner_budget_exhausted"
		if file == fileFail {
			want = "planner_declared_fail"
		}
		t.Run(file, func(t *testing.T) {
			harness.Bool(t, fmt.Sprintf("SPEC.md 6.5: reason is %s", want),
				fmt.Sprintf("SELECT reason = '%s' FROM dead_letter_queue WHERE run_id = :'run';", want),
				harness.Var(obs.dlqRun[file]))
			harness.Bool(t, "SPEC.md 12.3, 6.5: exactly one entry, and it names no step",
				`SELECT count(*) = 1 AND count(*) FILTER (WHERE step_id IS NULL) = 1
                   FROM dead_letter_queue WHERE run_id = :'run';`, harness.Var(obs.dlqRun[file]))
			harness.Bool(t, "SPEC.md 6.5: the entry records the round it belonged to",
				"SELECT replay_round = 0 FROM dead_letter_queue WHERE run_id = :'run';",
				harness.Var(obs.dlqRun[file]))
		})
	}
}

// TestNothingReachedAWorker: a planner-side dead letter happens where there is
// no step (SPEC.md 6.2), so no attempt row can exist. It is the observation
// that separates SPEC.md 12.3's two columns at the level of the tables rather
// than at the level of the reason text.
func TestNothingReachedAWorker(t *testing.T) {
	for _, file := range dlqFiles {
		t.Run(file, func(t *testing.T) {
			harness.Bool(t, "SPEC.md 12.3: a planner-side failure dispatches nothing",
				"SELECT count(*) = 0 FROM attempts WHERE run_id = :'run';",
				harness.Var(obs.dlqRun[file]))
		})
	}
}

// TestEachCallSaysHowItFailed is where the amendment pays for itself. SPEC.md
// 5.8 gives a planner call the same three failure reasons SPEC.md 5.3 gives an
// attempt, and each fixture produces one of them in a different way:
//
//	unreachable  a host that does not resolve   -> transport_error
//	http500      an answer, but a non-2xx one   -> transport_error
//	slow         no answer before the deadline  -> timeout
//	garbage      a status outside SPEC.md 9.3's -> invalid_response
//	badstep      a StepSpec SPEC.md 9.8 rejects -> invalid_response
//
// The pairing of `unreachable` and `slow` is SPEC.md 5.8's clock rule, which
// this milestone can test only because the two now land on different rows:
// "an exchange is timeout ONLY if the deadline passed; a connection refused at
// second 3 of a 30-second budget is transport_error."
func TestEachCallSaysHowItFailed(t *testing.T) {
	want := map[string]string{
		fileUnreachable: "transport_error",
		fileHTTP500:     "transport_error",
		fileSlow:        "timeout",
		fileGarbage:     "invalid_response",
		fileBadStep:     "invalid_response",
	}
	for file, reason := range want {
		t.Run(file, func(t *testing.T) {
			harness.Bool(t, fmt.Sprintf("SPEC.md 5.8, 6.8: every failed call of this run reads %s", reason),
				fmt.Sprintf(`SELECT count(*) > 0 AND bool_and(failure_reason = '%s')
                               FROM planner_calls WHERE run_id = :'run' AND status = 'FAILED';`, reason),
				harness.Var(obs.dlqRun[file]))
			harness.Bool(t, "SPEC.md 6.8 invariant 1: a FAILED call names a reason and carries no answer",
				`SELECT count(*) = 0 FROM planner_calls
                  WHERE run_id = :'run' AND status = 'FAILED'
                    AND (failure_reason IS NULL OR answer IS NOT NULL);`, harness.Var(obs.dlqRun[file]))
		})
	}
}

// TestAnInvalidStepSpecNeverCreatesAStep is SPEC.md 9.8's closing sentence:
// "An invalid StepSpec is a planner failure and consumes planner budget exactly
// like an unreachable planner. It never creates a step."
//
// It is worth its own test because it is the one rule here that an
// implementation could satisfy halfway - inserting the step and then failing
// would produce the same reason with a row that should not exist.
func TestAnInvalidStepSpecNeverCreatesAStep(t *testing.T) {
	harness.Bool(t, "SPEC.md 9.8: an invalid StepSpec creates nothing",
		"SELECT count(*) = 0 FROM steps WHERE run_id = :'run';", harness.Var(obs.dlqRun[fileBadStep]))
}

// TestTwoKindsOfFailureInOneDecisionPoint is what the `mixed` fixture tests now.
//
// It used to exist for a rule that no longer does: the dead-letter entry had to
// choose a single word for a set of failures, and a special value existed for a
// set that was not all of one kind. That rule was unsatisfiable from stored
// state, which is why SPEC.md 6.8 exists. What the fixture demonstrates instead
// is the thing the rule was reaching for: one decision point, two failures, two
// kinds, each on its own row and each true at the moment it was written.
func TestTwoKindsOfFailureInOneDecisionPoint(t *testing.T) {
	harness.Bool(t, "SPEC.md 5.8, 6.8: the round holds one transport_error and one invalid_response",
		`SELECT count(*) FILTER (WHERE failure_reason = 'transport_error') = 1
            AND count(*) FILTER (WHERE failure_reason = 'invalid_response') = 1
           FROM planner_calls WHERE run_id = :'run' AND status = 'FAILED';`,
		harness.Var(obs.dlqRun[fileMixed]))
	harness.Bool(t, "SPEC.md 6.5: and the entry says only that the budget ran out",
		"SELECT reason = 'planner_budget_exhausted' FROM dead_letter_queue WHERE run_id = :'run';",
		harness.Var(obs.dlqRun[fileMixed]))
}

// TestADeclaredFailIsNotAFailure is SPEC.md 12.1's exception, and the only
// dead-letter entry in this suite that costs nothing: "a fail response is not a
// planner failure - it is a valid answer, and it sends the run to DLQ
// immediately without consuming budget."
//
// SPEC.md 12.1 also fixes how it is recorded - "it is written as a DONE row
// whose answer is fail" - which is what makes the distinction inspectable
// rather than merely stated.
func TestADeclaredFailIsNotAFailure(t *testing.T) {
	run := obs.dlqRun[fileFail]
	harness.Bool(t, "SPEC.md 12.1, 6.8: one call, DONE, answering fail, and saying why",
		`SELECT count(*) = 1
                AND bool_and(status = 'DONE' AND answer = 'fail' AND length(coalesce(error_text, '')) > 0)
           FROM planner_calls WHERE run_id = :'run';`, harness.Var(run))
	harness.Bool(t, "SPEC.md 12.1: no call failed, so no budget was consumed",
		`SELECT (SELECT count(*) FROM planner_calls WHERE run_id = :'run' AND status = 'FAILED') = 0
            AND (SELECT planner_attempt_count FROM runs WHERE run_id = :'run') = 0;`,
		harness.Var(run))
	harness.Bool(t, "SPEC.md 12.4: the entry still says why it stopped",
		"SELECT length(error_text) > 0 FROM dead_letter_queue WHERE run_id = :'run';", harness.Var(run))
}

// TestTheBudgetStopsExactlyAtMaxAttempts is SPEC.md 12.2's planner line -
// "count < planner_max_attempts ? call again : run -> DLQ" - together with
// SPEC.md 11.1's insistence that the number is a TOTAL call count and not a
// retry count. The expected value is read from each fixture rather than written
// as a literal.
//
// The counter and the rows must agree. SPEC.md 12.2 writes them in one
// transaction, so a disagreement would mean one of the two was written outside
// it.
func TestTheBudgetStopsExactlyAtMaxAttempts(t *testing.T) {
	for _, file := range dlqFiles {
		if file == fileFail {
			continue // SPEC.md 12.1: not a failure, so it never enters the budget path.
		}
		t.Run(file, func(t *testing.T) {
			budget := plannerBudget[file]
			harness.Bool(t, fmt.Sprintf("SPEC.md 12.2, 11.1: the run stopped at exactly %d planner "+
				"calls, its total budget", budget),
				fmt.Sprintf("SELECT planner_attempt_count = %d FROM runs WHERE run_id = :'run';", budget),
				harness.Var(obs.dlqRun[file]))
			harness.Bool(t, fmt.Sprintf("SPEC.md 6.8, 12.2: and %d FAILED calls are on the record", budget),
				fmt.Sprintf(`SELECT count(*) = %d FROM planner_calls
                               WHERE run_id = :'run' AND status = 'FAILED';`, budget),
				harness.Var(obs.dlqRun[file]))
		})
	}
}

// TestTheDiagnosisIsInTheDatabase: SPEC.md 6.2 no longer keeps a copy of the
// newest planner error on the run, so the diagnosis has exactly one home - the
// call that produced it. SPEC.md 17.3 requires it to be in the database at all,
// and SPEC.md 6.8 invariant 3 requires a failed call to carry it.
func TestTheDiagnosisIsInTheDatabase(t *testing.T) {
	for _, file := range dlqFiles {
		if file == fileFail {
			continue // Covered by TestADeclaredFailIsNotAFailure, which has no failed call.
		}
		t.Run(file, func(t *testing.T) {
			harness.Bool(t, "SPEC.md 6.8 invariant 3, 17.3: every failed call says what went wrong",
				`SELECT count(*) = 0 FROM planner_calls
                  WHERE run_id = :'run' AND status = 'FAILED'
                    AND length(coalesce(error_text, '')) = 0;`, harness.Var(obs.dlqRun[file]))
			harness.Bool(t, "SPEC.md 12.4: the entry records the most recent failure",
				"SELECT length(error_text) > 0 FROM dead_letter_queue WHERE run_id = :'run';",
				harness.Var(obs.dlqRun[file]))
		})
	}
}

// ---------------------------------------------------------------------------
// Leg 7 - replaying a planner-side dead letter
// ---------------------------------------------------------------------------

// TestTheRunHadNoStepsWhileItWasInDLQ records the state the replay ended, and
// is the premise of everything below: this run failed at its first decision
// point, so SPEC.md 12.2 had nothing to stop it at and no step exists.
func TestTheRunHadNoStepsWhileItWasInDLQ(t *testing.T) {
	if obs.recoversStepsBefore != 0 {
		t.Fatalf("SPEC.md 12.3: a run that died on the planner's side holds no step; this one held %d",
			obs.recoversStepsBefore)
	}
	if obs.recoversRound0 != "DLQ" {
		t.Fatalf("leg 7's premise: the run must be in DLQ before the replay; it was %q",
			obs.recoversRound0)
	}
}

// TestReplayAsksThePlannerAgain is the entry in SPEC.md 12.3's table that delta
// could not reach: "what replay resumes - asking the planner again".
//
// Delta's replay re-dispatched a step. This one has no step to re-dispatch, and
// the only way the run can move is a fresh planner call. There are now two
// independent readings of that: the fixture's own counter, and - since SPEC.md
// 6.8 - the calls themselves.
func TestReplayAsksThePlannerAgain(t *testing.T) {
	if obs.recoversCallsAfter <= obs.recoversCallsBefore {
		t.Errorf("SPEC.md 12.3, 14: a replayed planner-side run must be planned again; the planner "+
			"was asked %d times before the replay and %d times after",
			obs.recoversCallsBefore, obs.recoversCallsAfter)
	}
	harness.Bool(t, "SPEC.md 4.2 L1, 14: steps now exist that could not exist before the replay",
		"SELECT count(*) = 2 FROM steps WHERE run_id = :'run';", harness.Var(obs.recoversRun))
}

// TestEachCallSaysWhichRoundItBelongedTo is SPEC.md 14's inspectability rule on
// the table the amendment added: "because replay_count is incremented first,
// inside the same transaction, every attempt dispatched and every planner call
// made afterwards carries the new number. The rows of the round that failed
// keep the old one, unchanged."
//
// It is the assertion that makes the fixture's counter unnecessary rather than
// merely corroborated - the same fact, in the database, where the operator can
// read it at a terminal (SPEC.md 17.1).
func TestEachCallSaysWhichRoundItBelongedTo(t *testing.T) {
	budget := plannerBudget[fileRecovers]
	harness.Bool(t, fmt.Sprintf("SPEC.md 14, 6.8: round 0 holds the %d calls that failed", budget),
		fmt.Sprintf(`SELECT count(*) = %d AND bool_and(status = 'FAILED')
                       FROM planner_calls WHERE run_id = :'run' AND replay_round = 0;`, budget),
		harness.Var(obs.recoversRun))
	harness.Bool(t, "SPEC.md 14, 6.8: round 1 holds the three that answered - continue, continue, done",
		`SELECT count(*) = 3 AND count(*) FILTER (WHERE status = 'DONE') = 3
                AND count(*) FILTER (WHERE answer = 'done') = 1
           FROM planner_calls WHERE run_id = :'run' AND replay_round = 1;`,
		harness.Var(obs.recoversRun))
	harness.Bool(t, "SPEC.md 6.8: call_no keeps ordering the whole conversation across both rounds",
		`SELECT count(*) = 0 FROM (
             SELECT call_no, row_number() OVER (ORDER BY call_no) AS expected
               FROM planner_calls WHERE run_id = :'run') t
          WHERE t.call_no <> t.expected;`, harness.Var(obs.recoversRun))
	harness.Bool(t, "SPEC.md 6.8, 14: the steps of round 1 were dispatched in round 1",
		"SELECT bool_and(replay_round = 1) FROM attempts WHERE run_id = :'run';",
		harness.Var(obs.recoversRun))
}

// TestTheReplayedRunFinished: SPEC.md 14 puts the run back to RUNNING and
// clears its coordination metadata "so the next sweep picks it up", and from
// there SPEC.md 4.2's loop is the ordinary one.
//
// It also proves the budget reset by inference, which is exact. SPEC.md 12.2
// refuses a further planner call once planner_attempt_count has reached
// planner_max_attempts, and this run had reached it - so a second round could
// not have happened at all unless SPEC.md 14's "reset planner_attempt_count to
// 0" had run first. Polling for a literal 0 was rejected for the reason R37-f
// gives: the window is up to one sweep wide, so the test would pass or fail on
// timing.
func TestTheReplayedRunFinished(t *testing.T) {
	if obs.recoversFinal != "DONE" {
		t.Fatalf("SPEC.md 14, 12.3: the replayed run resumes at the planner and finishes; it reached %q",
			obs.recoversFinal)
	}
	harness.Bool(t, "SPEC.md 5.5 L3: the run is DONE and so is its last step",
		`SELECT r.status = 'DONE' AND
                (SELECT s.status FROM steps s WHERE s.run_id = r.run_id ORDER BY s.seq DESC LIMIT 1) = 'DONE'
           FROM runs r WHERE r.run_id = :'run';`, harness.Var(obs.recoversRun))
	harness.Bool(t, "SPEC.md 6.2: the successful calls of round 1 left the budget at 0",
		"SELECT planner_attempt_count = 0 FROM runs WHERE run_id = :'run';", harness.Var(obs.recoversRun))
}

// TestTheFailedRoundIsStillOnTheRecord: SPEC.md 6.7 makes both history tables
// append-only, and SPEC.md 12.4 says an entry "is written once and never
// modified". A replay is the one operation that would be tempted to tidy them
// away, since what they recorded is no longer true.
func TestTheFailedRoundIsStillOnTheRecord(t *testing.T) {
	harness.Equal(t, "SPEC.md 6.7, 12.4: the entry is identical either side of the replay",
		obs.recoversDLQBefore, obs.recoversDLQAfter)
	harness.Bool(t, "SPEC.md 12.4, 6.5: one round in DLQ, one entry, and it names round 0",
		`SELECT count(*) = 1 AND count(*) FILTER (WHERE replay_round = 0) = 1
           FROM dead_letter_queue WHERE run_id = :'run';`, harness.Var(obs.recoversRun))
	harness.Bool(t, "SPEC.md 14, 6.2: one replay leaves replay_count = 1, and the run keeps its identity",
		"SELECT replay_count = 1 FROM runs WHERE run_id = :'run';", harness.Var(obs.recoversRun))
	harness.Bool(t, "SPEC.md 6.7: the failed round's calls survive the round that succeeded",
		`SELECT count(*) > 0 FROM planner_calls
          WHERE run_id = :'run' AND replay_round = 0 AND status = 'FAILED';`,
		harness.Var(obs.recoversRun))
}

// ---------------------------------------------------------------------------
// Leg 8 - what POST /workflows refuses
// ---------------------------------------------------------------------------

// TestTheControlWorkflowIsAccepted is what makes the three rejections below
// mean anything: the same body, unmutated, is accepted, so each 400 can be
// attributed to the one field that changed.
func TestTheControlWorkflowIsAccepted(t *testing.T) {
	if obs.controlCode < 200 || obs.controlCode >= 300 {
		t.Fatalf("leg 8's premise: the unmutated fixture must be accepted, or every rejection below "+
			"could be failing for a reason nobody named; POST /workflows returned %d: %s",
			obs.controlCode, obs.controlBody)
	}
}

// TestEveryMalformedWorkflowIsRefusedAtSubmission is SPEC.md 16's governing
// principle applied to the field this milestone added: "better to be too strict
// and be told we do not support something, than too lax and let a user fail
// silently", and "anything decidable at submission time must not be deferred to
// run time, where it costs budget, produces dead-letter entries and wastes
// triage".
//
// SPEC.md 10.5's table fixes the code: 400 is "the request is malformed or
// violates 16".
func TestEveryMalformedWorkflowIsRefusedAtSubmission(t *testing.T) {
	if len(obs.rejections) == 0 {
		t.Fatal("leg 8 recorded no submissions at all")
	}
	for _, r := range obs.rejections {
		if r.code != 400 {
			t.Errorf("%s\n  expected 400 (SPEC.md 10.5), got %d: %s", r.what, r.code, r.body)
		}
	}
}

// TestNoRejectedWorkflowWasStored: SPEC.md 16 rejects "before any run exists",
// and a rejection that nonetheless left a row would mean the strictness was
// cosmetic. Three bodies were refused, so the workflows table must hold exactly
// the ones the earlier legs created and nothing else.
func TestNoRejectedWorkflowWasStored(t *testing.T) {
	// leg 1's dynamic, leg 2's static, one per planner-side fixture, leg 7's
	// recovers, and leg 8's control - and nothing the three mutations left
	// behind.
	created := 1 + 1 + len(dlqFiles) + 1 + 1
	harness.Bool(t, "SPEC.md 16: a refused workflow is not stored",
		fmt.Sprintf("SELECT count(*) = %d FROM workflows;", created))
}

// ---------------------------------------------------------------------------
// Whole-database invariants, and what zeta does not demonstrate
// ---------------------------------------------------------------------------

// TestNoImpossibleCombinationExists sweeps SPEC.md 5.6 over every run this
// milestone created. Each row of that table is impossible "because of a
// transaction boundary, not because of a check somewhere", and the two that
// zeta could plausibly break are RUNNING/DLQ and DLQ/RUNNING - one transaction
// writes the planner-side verdict, and a replay moves a run out of DLQ.
func TestNoImpossibleCombinationExists(t *testing.T) {
	harness.Bool(t, "SPEC.md 5.6: no run holds a combination the transaction boundaries forbid",
		`SELECT count(*) = 0 FROM (
             SELECT r.status AS run_status,
                    coalesce((SELECT s.status FROM steps s WHERE s.run_id = r.run_id
                               ORDER BY s.seq DESC LIMIT 1), 'DONE') AS last_step
               FROM runs r) x
          WHERE (x.run_status, x.last_step) NOT IN (
                  ('RUNNING','DONE'), ('RUNNING','RUNNING'), ('DONE','DONE'),
                  ('DLQ','DLQ'), ('DLQ','DONE'),
                  ('CANCELLED','CANCELLED'), ('CANCELLED','DONE'), ('CANCELLED','DLQ'));`)
}

// TestEveryPlannerCallSatisfiesItsInvariants sweeps SPEC.md 6.8's three
// invariants across every row this milestone wrote. They are stated there as
// obligations on the backend, and this is the assertion that they hold of the
// system as a whole rather than of the one fixture that happened to be looked
// at.
func TestEveryPlannerCallSatisfiesItsInvariants(t *testing.T) {
	harness.Bool(t, "SPEC.md 6.8 invariant 1: FAILED implies a reason and no answer",
		`SELECT count(*) = 0 FROM planner_calls
          WHERE (status = 'FAILED') <> (failure_reason IS NOT NULL);`)
	harness.Bool(t, "SPEC.md 6.8 invariant 2: DONE implies an answer and no reason",
		`SELECT count(*) = 0 FROM planner_calls
          WHERE (status = 'DONE') <> (answer IS NOT NULL);`)
	harness.Bool(t, "SPEC.md 6.8 invariant 3: a failed call, and a declared fail, both say why",
		`SELECT count(*) = 0 FROM planner_calls
          WHERE (status = 'FAILED' OR answer = 'fail')
            AND length(coalesce(error_text, '')) = 0;`)
	harness.Bool(t, "SPEC.md 5.8: no planner call carries a reason that belongs to an attempt alone",
		`SELECT count(*) = 0 FROM planner_calls
          WHERE failure_reason IS NOT NULL
            AND failure_reason NOT IN ('transport_error', 'invalid_response', 'timeout');`)
	harness.Bool(t, "SPEC.md 5.8, 6.8: every call is finished, because the row is written at the outcome",
		"SELECT count(*) = 0 FROM planner_calls WHERE finished_at IS NULL OR status = 'RUNNING';")
	harness.Bool(t, "SPEC.md 6.8: every call names the orchestrator that made it",
		"SELECT count(*) = 0 FROM planner_calls WHERE coalesce(called_by, '') = '';")
}

// TestEveryRunIsTerminal guards the fixture rather than the system: a leg that
// silently left a run mid-flight would make several assertions above pass by
// accident.
func TestEveryRunIsTerminal(t *testing.T) {
	harness.Bool(t, "every run this group created reached a terminal state (SPEC.md 5.1)",
		"SELECT count(*) = 0 FROM runs WHERE status = 'RUNNING';")
}

// TestWhatZetaDoesNotDemonstrate states the absences rather than leaving them
// silent, which is the arrangement R34-m and R37-g settled.
//
//	Cancellation      SPEC.md 15, milestone iota. POST /runs/{run_id}/cancel
//	                  does not exist, so no run here is CANCELLED.
//	async             SPEC.md 9.5, 9.6, milestone epsilon.
//	raw dispatch      SPEC.md 9.5, milestone theta.
//	Overrides         SPEC.md 11.2, milestone eta.
//
// Each is asserted as absent, so that the day one of them arrives, this test
// fails and the milestone that brought it is told to say so.
func TestWhatZetaDoesNotDemonstrate(t *testing.T) {
	harness.Bool(t, "SPEC.md 15: cancellation is milestone iota; nothing here is CANCELLED",
		"SELECT count(*) = 0 FROM runs WHERE status = 'CANCELLED';")
	harness.Bool(t, "SPEC.md 9.7: every step of this milestone is sync + envelope",
		`SELECT count(*) = 0 FROM steps
          WHERE decision->>'connection_mode' <> 'sync' OR decision->>'dispatch_style' <> 'envelope';`)
	harness.Bool(t, "SPEC.md 11.2: step-level overrides are milestone eta and must be absent",
		`SELECT count(*) = 0 FROM steps
          WHERE decision ? 'timeout_seconds' OR decision ? 'max_attempts';`)
}
