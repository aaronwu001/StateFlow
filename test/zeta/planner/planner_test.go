package planner

import (
	"fmt"
	"testing"

	"github.com/aaronwu001/piton/test/zeta/harness"
)

// The assertions below are grouped by leg. Each one names the SPEC.md rule it
// comes from, so a failure says which rule broke and not merely which query
// returned false.

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
// SPEC.md 6.3 stores "the worker's whole response body, verbatim", so the kind
// sits one level in, under SPEC.md 9.6's `output` key.
func TestTheBranchFollowsWhatTheWorkerProduced(t *testing.T) {
	for _, run := range []string{obs.dynamicRunA, obs.dynamicRunB} {
		harness.Bool(t, "SPEC.md 9.2, 10.2: the second step is the one the first step's output names",
			`SELECT (SELECT step_name FROM steps WHERE run_id = :'run' AND seq = 2)
                  = 'extract-' || (SELECT output->'output'->>'kind' FROM steps
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

// TestSuccessfulPlannerCallsBurnNoBudget: SPEC.md 6.2 makes
// planner_attempt_count "failed planner calls at the current decision point.
// Reset to 0 by any successful planner call", and SPEC.md 4.2's `continue` row
// resets the planner budget in the same transaction that inserts the step.
func TestSuccessfulPlannerCallsBurnNoBudget(t *testing.T) {
	for _, run := range []string{obs.dynamicRunA, obs.dynamicRunB} {
		harness.Bool(t, "SPEC.md 6.2, 4.2: a run whose planner never failed carries no burnt budget "+
			"and no diagnosis",
			"SELECT planner_attempt_count = 0 AND last_planner_error IS NULL FROM runs WHERE run_id = :'run';",
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

// TestTheStaticPlannerNeverBurnsBudget is SPEC.md 12.1 stated as a measurement:
// "the static planner simply cannot fail at run time ... so
// planner_attempt_count never leaves 0". SPEC.md 12.1 asks for exactly this -
// that the rule be relied on rather than special-cased - so the run is driven
// through the same budget path as every other and the counter is read
// afterwards.
func TestTheStaticPlannerNeverBurnsBudget(t *testing.T) {
	harness.Bool(t, "SPEC.md 12.1: the static planner makes no network call and cannot fail",
		"SELECT planner_attempt_count = 0 AND last_planner_error IS NULL FROM runs WHERE run_id = :'run';",
		harness.Var(obs.staticRun))
}

// TestThePlannerChoiceIsFixedAtCreation asserts SPEC.md 6.1's invariant on the
// two rows the previous legs created: the http workflow carries planner_url and
// fetch_base_url and no static steps; the static workflow carries the array and
// neither URL. The amendment that added fetch_base_url put both http columns on
// the same side of that invariant.
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

// TestEveryPlannerSideEntryNamesNoStep: SPEC.md 12.3 gives the planner-side
// column dead_letter_queue.step_id = NULL, and SPEC.md 6.5 says "step_id IS
// NULL already distinguishes the two sides, so no separate column records it".
func TestEveryPlannerSideEntryNamesNoStep(t *testing.T) {
	for _, file := range dlqFiles {
		t.Run(file, func(t *testing.T) {
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

// TestUnreachableTimeoutAndNon2xxAllReadUnreachable pins SPEC.md 6.5's first
// planner reason, which deliberately covers three different events with one
// value: "the planner could not be called: connection refused, timeout,
// non-2xx".
//
// The three fixtures produce one each - a host that does not resolve, a planner
// that sleeps past planner_timeout_seconds, and a planner that answers 500 -
// and the column must not tell them apart.
func TestUnreachableTimeoutAndNon2xxAllReadUnreachable(t *testing.T) {
	for _, file := range []string{fileUnreachable, fileSlow, fileHTTP500} {
		t.Run(file, func(t *testing.T) {
			assertReason(t, file, "planner_unreachable",
				"SPEC.md 6.5: connection refused, timeout and non-2xx are one reason")
		})
	}
}

// TestAnUnusableAnswerReadsInvalidResponse pins SPEC.md 6.5's second reason:
// "the planner replied with something 9.3 / 9.8 rejects". The two fixtures are
// the two halves of that sentence - a status outside SPEC.md 9.3's three, and a
// StepSpec that SPEC.md 9.8 rule 6 rejects for an unknown top-level key.
func TestAnUnusableAnswerReadsInvalidResponse(t *testing.T) {
	for _, file := range []string{fileGarbage, fileBadStep} {
		t.Run(file, func(t *testing.T) {
			assertReason(t, file, "planner_invalid_response",
				"SPEC.md 6.5, 9.3, 9.8: an answer the system cannot act on")
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

// TestMixedFailuresReadBudgetExhausted pins the sentence that gives SPEC.md
// 6.5's third reason its meaning: planner_budget_exhausted is "set instead of
// the two above when the round's failures were not all of one kind".
//
// The fixture answers 500 once and then an unknown status, so the round holds
// one of each. An implementation that simply reported the last failure's kind,
// or always reported budget exhaustion, gets exactly one of these three tests
// wrong.
func TestMixedFailuresReadBudgetExhausted(t *testing.T) {
	assertReason(t, fileMixed, "planner_budget_exhausted",
		"SPEC.md 6.5: a round whose failures were not all of one kind")
}

// TestADeclaredFailIsNotAFailure is SPEC.md 12.1's exception, and the only
// dead-letter entry in this suite that costs nothing: "a fail response is not a
// planner failure - it is a valid answer, and it sends the run to DLQ
// immediately without consuming budget".
func TestADeclaredFailIsNotAFailure(t *testing.T) {
	run := obs.dlqRun[fileFail]
	assertReason(t, fileFail, "planner_declared_fail",
		"SPEC.md 6.5: the planner answered fail")
	harness.Bool(t, "SPEC.md 12.1: a fail answer consumes no budget",
		"SELECT planner_attempt_count = 0 FROM runs WHERE run_id = :'run';", harness.Var(run))
	harness.Bool(t, "SPEC.md 6.5: the entry records the budget consumed at the verdict, which is none",
		"SELECT attempt_count = 0 FROM dead_letter_queue WHERE run_id = :'run';", harness.Var(run))
	harness.Bool(t, "SPEC.md 12.4: the entry still says why it stopped",
		"SELECT length(error_text) > 0 FROM dead_letter_queue WHERE run_id = :'run';", harness.Var(run))
}

// TestTheBudgetStopsExactlyAtMaxAttempts is SPEC.md 12.2's planner line -
// "count < planner_max_attempts ? call again : run -> DLQ" - together with
// SPEC.md 11.1's insistence that the number is a TOTAL call count and not a
// retry count. The expected value is read from each fixture rather than written
// as a literal.
//
// SPEC.md 6.5's attempt_count column must agree with the run's: "the budget
// consumed at the moment of the verdict ... runs.planner_attempt_count for a
// planner-side one".
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
			harness.Bool(t, "SPEC.md 6.5: the entry records the same number the run does",
				`SELECT d.attempt_count = r.planner_attempt_count
                   FROM dead_letter_queue d JOIN runs r ON r.run_id = d.run_id
                  WHERE d.run_id = :'run';`, harness.Var(obs.dlqRun[file]))
		})
	}
}

// TestTheDiagnosisIsInTheDatabase: SPEC.md 6.2 keeps last_planner_error because
// "a planner failure happens where there is no step" and there is no attempts
// row to hold it, and SPEC.md 17.3 requires error text to live in the database
// rather than only in logs. SPEC.md 12.4 requires the same of the entry.
//
// The declared-fail run is excluded from the first half: SPEC.md 6.2 defines
// last_planner_error as the text of the most recent FAILED call, and SPEC.md
// 12.1 says a fail answer is not one. SPEC.md does not say what the column
// holds in that case, so this suite asserts nothing about it (R37-g).
func TestTheDiagnosisIsInTheDatabase(t *testing.T) {
	for _, file := range dlqFiles {
		if file == fileFail {
			continue
		}
		t.Run(file, func(t *testing.T) {
			harness.Bool(t, "SPEC.md 6.2, 17.3: the run names the most recent planner failure",
				"SELECT length(coalesce(last_planner_error, '')) > 0 FROM runs WHERE run_id = :'run';",
				harness.Var(obs.dlqRun[file]))
			harness.Bool(t, "SPEC.md 12.4: the entry says why it stopped",
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
// the only way the run can move is a fresh planner call. The fixture's counter
// is the direct reading; the steps that now exist are the same fact seen in the
// database.
func TestReplayAsksThePlannerAgain(t *testing.T) {
	if obs.recoversCallsAfter <= obs.recoversCallsBefore {
		t.Errorf("SPEC.md 12.3, 14: a replayed planner-side run must be planned again; the planner "+
			"was asked %d times before the replay and %d times after",
			obs.recoversCallsBefore, obs.recoversCallsAfter)
	}
	harness.Bool(t, "SPEC.md 4.2 L1, 14: steps now exist that could not exist before the replay",
		"SELECT count(*) = 2 FROM steps WHERE run_id = :'run';", harness.Var(obs.recoversRun))
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

// TestTheFailedRoundIsStillOnTheRecord: SPEC.md 6.7 makes dead_letter_queue
// append-only and SPEC.md 12.4 says an entry "is written once and never
// modified". A replay is the one operation that would be tempted to tidy the
// entry away, since what it recorded is no longer true.
func TestTheFailedRoundIsStillOnTheRecord(t *testing.T) {
	harness.Equal(t, "SPEC.md 6.7, 12.4: the entry is identical either side of the replay",
		obs.recoversDLQBefore, obs.recoversDLQAfter)
	harness.Bool(t, "SPEC.md 12.4, 6.5: one round in DLQ, one entry, and it names round 0",
		`SELECT count(*) = 1 AND count(*) FILTER (WHERE replay_round = 0) = 1
           FROM dead_letter_queue WHERE run_id = :'run';`, harness.Var(obs.recoversRun))
	harness.Bool(t, "SPEC.md 14, 6.2: one replay leaves replay_count = 1, and the run keeps its identity",
		"SELECT replay_count = 1 FROM runs WHERE run_id = :'run';", harness.Var(obs.recoversRun))
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

// assertReason checks one fixture's dead-letter reason against SPEC.md 6.5's
// enumeration.
func assertReason(t *testing.T, file, want, why string) {
	t.Helper()
	harness.Bool(t, fmt.Sprintf("%s: reason must be %q", why, want),
		fmt.Sprintf("SELECT reason = '%s' FROM dead_letter_queue WHERE run_id = :'run';", want),
		harness.Var(obs.dlqRun[file]))
}
