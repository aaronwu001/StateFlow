package replay

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/aaronwu001/piton/test/delta/harness"
)

// Every assertion below names the SPEC.md section it came from. CLAUDE.md 5.1
// permits exactly one source for a test, and this file has no other: nothing
// here was derived by reading internal/, and where SPEC.md is silent the
// assertion is absent rather than invented.

// ---------------------------------------------------------------------------
// Leg 1 - a worker-side dead letter is replayed, and the run finishes
// ---------------------------------------------------------------------------

// TestReplayTakesTheRunOutOfDLQ is SPEC.md 14's first row: for a run whose
// status is DLQ, "replay proceeds". SPEC.md 12.3 says what it resumes on the
// worker side - "re-dispatching that step" - and the run reaching DONE is what
// that looks like from a terminal.
func TestReplayTakesTheRunOutOfDLQ(t *testing.T) {
	if obs.resumesFinal != "DONE" {
		t.Fatalf("SPEC.md 14, 12.3: a replayed worker-side DLQ run re-dispatches its step and "+
			"continues; this run reached %q", obs.resumesFinal)
	}
	harness.Bool(t, "SPEC.md 5.5 L3: the run is DONE and so is its last step",
		`SELECT r.status = 'DONE' AND
                (SELECT s.status FROM steps s WHERE s.run_id = r.run_id ORDER BY s.seq DESC LIMIT 1) = 'DONE'
           FROM runs r WHERE r.run_id = :'run';`, harness.Var(obs.resumesRun))
}

// TestReplayResetsTheBudgetWithoutErasingTheHistory is the heart of SPEC.md 14:
// "if last_step = DLQ, that step returns to RUNNING with attempt_count reset to
// 0".
//
// It is asserted after the fact rather than in the instant the reset happened,
// and the inference is exact. SPEC.md 4.2 burns one unit of budget AT DISPATCH,
// so a step that was re-dispatched once and then succeeded can only read
// attempt_count = 1 if the counter had been put back to 0 first - it stood at
// step_max_attempts when the run entered the dead-letter queue. Meanwhile the
// attempts rows are untouched, which is exactly the divergence SPEC.md 6.3
// describes: "a step may hold three attempt rows and an attempt_count of 0",
// and why that column is stored rather than derived as COUNT(attempts).
func TestReplayResetsTheBudgetWithoutErasingTheHistory(t *testing.T) {
	budget := maxAttempts[fileResumes]
	harness.Bool(t,
		"SPEC.md 14, 4.2: the replayed step burned one fresh unit of budget, so attempt_count = 1",
		"SELECT status = 'DONE' AND attempt_count = 1 FROM steps "+
			"WHERE run_id = :'run' AND step_name = 'stumbles';", harness.Var(obs.resumesRun))
	harness.Bool(t,
		fmt.Sprintf("SPEC.md 6.3: replay leaves the attempts rows in place, so the step holds "+
			"%d of them - %d from the round that failed, one from the round that succeeded",
			budget+1, budget),
		fmt.Sprintf(`SELECT count(*) = %d FROM attempts a
                       JOIN steps s ON s.step_id = a.step_id
                      WHERE a.run_id = :'run' AND s.step_name = 'stumbles';`, budget+1),
		harness.Var(obs.resumesRun))
	harness.Bool(t,
		fmt.Sprintf("SPEC.md 5.3, 6.4: the %d attempts of the failed round are still FAILED and still "+
			"name their reason; replay rewrites no history", budget),
		fmt.Sprintf(`SELECT count(*) = %d FROM attempts
                      WHERE run_id = :'run' AND status = 'FAILED' AND failure_reason = 'worker_error';`, budget),
		harness.Var(obs.resumesRun))
	harness.Bool(t, "SPEC.md 6.3: the step that finally succeeded carries its output",
		"SELECT output IS NOT NULL FROM steps WHERE run_id = :'run' AND step_name = 'stumbles';",
		harness.Var(obs.resumesRun))
}

// TestReplayIncrementsReplayCount is SPEC.md 14's "increment runs.replay_count"
// and SPEC.md 6.2's column. SPEC.md 14 requires the number to be inspectable
// from a terminal, which is what makes it a stored counter rather than
// something reconstructed from timestamps.
func TestReplayIncrementsReplayCount(t *testing.T) {
	harness.Bool(t, "SPEC.md 14, 6.2: one replay leaves replay_count = 1",
		"SELECT replay_count = 1 FROM runs WHERE run_id = :'run';", harness.Var(obs.resumesRun))
}

// TestReplayLeavesTheDeadLetterHistoryUntouched: SPEC.md 6.7 makes
// dead_letter_queue append-only and SPEC.md 12.4 says an entry "is written once
// and never modified". A replay is the one operation that could be tempted to
// tidy the entry away, since the condition it recorded is no longer true.
func TestReplayLeavesTheDeadLetterHistoryUntouched(t *testing.T) {
	harness.Equal(t, "SPEC.md 6.7, 12.4: the dead-letter entry is identical either side of the replay",
		obs.resumesDLQBefore, obs.resumesDLQAfter)
	harness.Bool(t, "SPEC.md 12.4: one round in DLQ, one entry - and it survives the replay",
		"SELECT count(*) = 1 FROM dead_letter_queue WHERE run_id = :'run';", harness.Var(obs.resumesRun))
	harness.Bool(t, "SPEC.md 6.5: the entry still records the round it belonged to",
		"SELECT replay_round = 0 FROM dead_letter_queue WHERE run_id = :'run';", harness.Var(obs.resumesRun))
}

// TestTheRunContinuesPastTheReplayedStep: SPEC.md 12.2 stops a run at the step
// that exhausted its budget, so while the run was in DLQ the workflow's second
// static step could not exist. After the replay the planner is asked again
// (SPEC.md 4.2, L1) and it does.
//
// It is evidence rather than a tautology because the workflow declares that
// second step: without it, "no step was created" and "the run did not continue"
// would be the same observation.
func TestTheRunContinuesPastTheReplayedStep(t *testing.T) {
	specs, err := harness.StaticSteps(fileResumes)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) < 2 {
		t.Fatalf("%s must declare a second static step for this test to mean anything; it declares %d",
			fileResumes, len(specs))
	}
	if obs.stepsBeforeReplay != 1 {
		t.Errorf("SPEC.md 12.2: while the run was in DLQ exactly one step could exist, "+
			"though the workflow declares %d; there were %d", len(specs), obs.stepsBeforeReplay)
	}
	harness.Bool(t,
		fmt.Sprintf("SPEC.md 4.2: after the replay the planner was asked again, so all %d steps exist and are DONE",
			len(specs)),
		fmt.Sprintf("SELECT count(*) = %d AND bool_and(status = 'DONE') FROM steps WHERE run_id = :'run';",
			len(specs)),
		harness.Var(obs.resumesRun))
}

// ---------------------------------------------------------------------------
// Leg 2 - two rounds, and SPEC.md 14's accepted limitation
// ---------------------------------------------------------------------------

// TestEachRoundLeavesItsOwnEntry is SPEC.md 14's closing requirement: "every
// replay round must leave an inspectable record... runs.replay_count and
// dead_letter_queue.replay_round together let the operator see, from a
// terminal, which round any given entry belonged to."
//
// SPEC.md 6.5 defines replay_round as "the value of runs.replay_count when this
// entry was written", so a run that has died in rounds 0 and 1 must carry
// exactly those two values - and the second entry must name the SECOND step,
// which is what makes the two rounds distinguishable at all.
func TestEachRoundLeavesItsOwnEntry(t *testing.T) {
	v := harness.Var(obs.twoRoundsRun)
	harness.Bool(t, "SPEC.md 12.4: a run accumulates one entry per round it lands in DLQ - here, two",
		"SELECT count(*) = 2 FROM dead_letter_queue WHERE run_id = :'run';", v)
	harness.Bool(t, "SPEC.md 6.5, 14: the rounds are numbered 0 and 1",
		`SELECT array_agg(replay_round ORDER BY replay_round) = ARRAY[0, 1]
           FROM dead_letter_queue WHERE run_id = :'run';`, v)
	harness.Bool(t, "SPEC.md 14, 6.2: two rounds completed, so replay_count = 2",
		"SELECT replay_count = 2 FROM runs WHERE run_id = :'run';", v)
	harness.Bool(t,
		"SPEC.md 6.5, 12.3: round 0's entry names the first step and round 1's names the second",
		`SELECT (SELECT s.seq FROM steps s WHERE s.step_id = d0.step_id) = 1
            AND (SELECT s.seq FROM steps s WHERE s.step_id = d1.step_id) = 2
           FROM dead_letter_queue d0, dead_letter_queue d1
          WHERE d0.run_id = :'run' AND d0.replay_round = 0
            AND d1.run_id = :'run' AND d1.replay_round = 1;`, v)
}

// TestReplayNeverRevisitsAnEarlierStep is SPEC.md 14's accepted limitation,
// stated there in the same words this test asserts: "a replay always resumes at
// the furthest step reached. If round 1 failed at step 1 and round 2 fails at
// step 2, replay resumes from step 2 and never revisits step 1."
//
// The digest is taken either side of the SECOND replay, so what is asserted is
// that nothing about step 1 - its status, its budget, its completion time, its
// output, or any attempt row it owns - moved when the run was replayed out of a
// dead letter that belonged to step 2.
func TestReplayNeverRevisitsAnEarlierStep(t *testing.T) {
	if obs.firstStepBefore == "" {
		t.Fatal("the fixture recorded no digest for step \"first\"")
	}
	harness.Equal(t,
		"SPEC.md 14: replay resumes at the furthest step reached and never revisits an earlier one",
		obs.firstStepBefore, obs.firstStepAfter)
	if obs.twoRoundsFinal != "DONE" {
		t.Errorf("SPEC.md 14: the second replay must carry the run to DONE; it reached %q",
			obs.twoRoundsFinal)
	}
}

// ---------------------------------------------------------------------------
// Leg 3 - the idempotency gate
// ---------------------------------------------------------------------------

// TestDoubleClickHasExactlyOneWinner is SPEC.md 14, quoted: "the transaction
// that takes the run out of DLQ IS the gate, so a double-click has exactly one
// winner."
//
// The database is asked as well as the responses, because the two could
// disagree in the direction that matters most: several transactions each
// incrementing replay_count while only one HTTP response reported success would
// satisfy a count of the replies and still have replayed the run three times.
func TestDoubleClickHasExactlyOneWinner(t *testing.T) {
	accepted, refused := 0, 0
	for _, r := range obs.gateResults {
		if r.Accepted() {
			accepted++
		} else {
			refused++
		}
	}
	if accepted != 1 {
		t.Errorf("SPEC.md 14: a double-click has exactly one winner; %d of %d replays were accepted",
			accepted, len(obs.gateResults))
	}
	if refused != len(obs.gateResults)-1 {
		t.Errorf("SPEC.md 14: every replay but the winner must be refused; %d of %d were",
			refused, len(obs.gateResults))
	}
	harness.Bool(t,
		fmt.Sprintf("SPEC.md 14, 6.2: %d concurrent replays advanced replay_count by exactly 1",
			len(obs.gateResults)),
		"SELECT replay_count = 1 FROM runs WHERE run_id = :'run';", harness.Var(obs.gateRun))
	harness.Bool(t,
		"SPEC.md 12.4: the losing replays wrote nothing - the run still has its one dead-letter entry",
		"SELECT count(*) = 1 FROM dead_letter_queue WHERE run_id = :'run';", harness.Var(obs.gateRun))
	if obs.gateFinal != "DONE" {
		t.Errorf("SPEC.md 14: the one winning replay carries the run forward; it reached %q", obs.gateFinal)
	}
}

// TestEveryRefusedReplayIsAConflictThatStatesTheTruth walks every refusal this
// milestone produced against an existing run - the seven losers of leg 3, the
// one issued while the run was RUNNING, and the one issued against a DONE run.
//
// SPEC.md 14's table gives 409 for RUNNING, DONE and CANCELLED, "stating the
// actual current status (SPEC.md 10.5)". SPEC.md 10.5 prints the body, and the
// body it prints is a refused replay - so `error`, `message`, `run_id` and
// `run_status` are quoted from SPEC.md rather than chosen here.
func TestEveryRefusedReplayIsAConflictThatStatesTheTruth(t *testing.T) {
	type refusal struct {
		what string
		res  harness.ReplayResult
		run  string
	}
	var all []refusal
	for i, r := range obs.gateResults {
		if !r.Accepted() {
			all = append(all, refusal{fmt.Sprintf("leg 3, losing replay %d", i), r, obs.gateRun})
		}
	}
	all = append(all,
		refusal{"leg 4, replay of a RUNNING run", obs.refusedWhileRunning, obs.inFlightRun},
		refusal{"leg 5, replay of a DONE run", obs.refusedWhileDone, obs.doneRun})

	for _, c := range all {
		t.Run(c.what, func(t *testing.T) {
			if c.res.Code != http.StatusConflict {
				t.Fatalf("SPEC.md 14, 10.5: a replay of a run that is not in DLQ is a 409; got %d: %s",
					c.res.Code, c.res.Body)
			}
			if c.res.Rejection == nil {
				t.Fatalf("SPEC.md 10.5 fixes the shape of a rejection body; this one did not parse as it: %s",
					c.res.Body)
			}
			rej := c.res.Rejection
			if rej.Error != harness.SlugConflict {
				t.Errorf("SPEC.md 10.5: `error` is a stable machine-readable slug, and the body it "+
					"prints for a refused replay carries %q; got %q", harness.SlugConflict, rej.Error)
			}
			if rej.Message == "" {
				t.Errorf("SPEC.md 10.5: `message` is human-readable and must be present")
			}
			// SPEC.md 10.5: "a rejection carries the identifier and current
			// status of every entity the request named or would have touched,
			// and omits only those that do not exist". The run exists.
			if rej.RunID == nil || *rej.RunID != c.run {
				t.Errorf("SPEC.md 10.5: the rejection must name the run it refused; got %v, want %s",
					deref(rej.RunID), c.run)
			}
			if rej.RunStatus == nil {
				t.Fatalf("SPEC.md 10.5: \"a rejection states the actual current state, not merely that " +
					"the request was refused\" - run_status is absent")
			}
			switch *rej.RunStatus {
			case "RUNNING", "DONE", "DLQ", "CANCELLED":
			default:
				t.Errorf("SPEC.md 5.1, 10.5: run_status must be one of the four run states; got %q",
					*rej.RunStatus)
			}
			if *rej.RunStatus == "DLQ" {
				t.Errorf("SPEC.md 14: a run whose status is DLQ is replayable, so a refusal cannot " +
					"honestly report DLQ as the current status")
			}
			// SPEC.md 10.5 explains why the step is reported too, and it is the
			// one sentence in that section that is specifically about replay:
			// "a refused replay is explained by the run's status, but what he
			// does next depends on the step's."
			if rej.StepID == nil || rej.StepStatus == nil {
				t.Errorf("SPEC.md 10.5: this run has a step, so the rejection must carry step_id and "+
					"step_status; got step_id=%v step_status=%v",
					deref(rej.StepID), deref(rej.StepStatus))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Leg 4 - the state a replay leaves behind, seen while it was true
// ---------------------------------------------------------------------------

// TestReplayPutsTheRunAndItsStepBackIntoRunning asserts SPEC.md 14's
// transaction as a state rather than as an outcome: "run -> RUNNING; if
// last_step = DLQ, that step returns to RUNNING".
//
// That pair is SPEC.md 5.5's L2, and it is the only combination from which the
// claim path can re-dispatch. A run put back to RUNNING whose step stayed in
// DLQ would be SPEC.md 5.6's first impossible row.
func TestReplayPutsTheRunAndItsStepBackIntoRunning(t *testing.T) {
	s := obs.inFlightSnapshot
	if s.runStatus != "RUNNING" || s.stepStatus != "RUNNING" {
		t.Fatalf("SPEC.md 14, 5.5 L2: after a replay the run and its last step are both RUNNING; "+
			"the run was %q and the step %q", s.runStatus, s.stepStatus)
	}
	if !s.ownerHeld {
		t.Errorf("SPEC.md 14, 8.5: replay clears owner_id so the next sweep picks the run up; " +
			"by the time its step had an attempt on the wire the run must be owned again, and it was not")
	}
}

// TestReplayResetsAttemptCountToZero is the same sentence of SPEC.md 14 read
// through the budget: "that step returns to RUNNING with attempt_count reset to
// 0."
//
// The reading was taken while the replayed step's new attempt was still on the
// wire, and it is exact rather than approximate. The step had spent its whole
// budget - step_max_attempts is 1 in this leg - so attempt_count stood at 1
// when the run entered DLQ. SPEC.md 4.2 burns one unit at dispatch. A reading
// of 1 alongside a SECOND attempt row can therefore only mean the counter was
// put back to 0 first; had it not been, the budget check of SPEC.md 12.2 would
// have refused the dispatch and this attempt would not exist.
func TestReplayResetsAttemptCountToZero(t *testing.T) {
	budget := maxAttempts[fileInFlight]
	s := obs.inFlightSnapshot
	if s.attemptRows != budget+1 {
		t.Fatalf("SPEC.md 6.3: the step should hold %d attempt rows by now (%d from the failed round, "+
			"one from the replayed one); it holds %d", budget+1, budget, s.attemptRows)
	}
	if s.stepAttemptCount != 1 {
		t.Errorf("SPEC.md 14, 4.2, 12.2: attempt_count is reset to 0 by the replay and burned once at "+
			"dispatch, so it must read 1 while the new attempt is running; it read %d", s.stepAttemptCount)
	}
	if s.dlqRows != 1 {
		t.Errorf("SPEC.md 6.7, 12.4: replay writes no dead-letter entry and removes none; the run "+
			"should still have exactly 1, it had %d", s.dlqRows)
	}
	if s.replayCount != 1 {
		t.Errorf("SPEC.md 14, 6.2: replay_count must read 1 during the first replayed round; it read %d",
			s.replayCount)
	}
}

// TestTheGateIsCurrentStatusAndNotHistory is SPEC.md 14's second sentence, and
// it is emphasised there rather than merely stated: "the idempotency gate is
// 'is this run in DLQ right now'. NOT 'has this run been replayed before'."
//
// Leg 4 is where the two readings can be told apart. The run has been replayed,
// and is refused - not because it was replayed, but because it is RUNNING now.
// A gate implemented as "replay_count = 0" would refuse this call too, and for
// the wrong reason; the run_status in the body is what tells them apart.
func TestTheGateIsCurrentStatusAndNotHistory(t *testing.T) {
	r := obs.refusedWhileRunning
	if r.Code != http.StatusConflict {
		t.Fatalf("SPEC.md 14: replaying a RUNNING run is a 409; got %d: %s", r.Code, r.Body)
	}
	if r.Rejection == nil || r.Rejection.RunStatus == nil {
		t.Fatalf("SPEC.md 10.5: the refusal must state the actual current status: %s", r.Body)
	}
	if got := *r.Rejection.RunStatus; got != "RUNNING" {
		t.Errorf("SPEC.md 14, 10.5: the run was RUNNING when the second replay arrived, and the "+
			"refusal must say so; it said %q", got)
	}
	harness.Bool(t, "SPEC.md 14: the refused replay incremented nothing - replay_count is still 1",
		"SELECT replay_count = 1 FROM runs WHERE run_id = :'run';", harness.Var(obs.inFlightRun))
	if obs.inFlightFinal != "DONE" {
		t.Errorf("SPEC.md 14: the accepted replay must still carry the run to DONE; it reached %q",
			obs.inFlightFinal)
	}
}

// ---------------------------------------------------------------------------
// Legs 5 and 6 - refusals that need no dead letter at all
// ---------------------------------------------------------------------------

// TestReplayingADoneRunIsRefused is SPEC.md 14's second table row for the one
// status of the three that delta can produce. DONE is terminal (SPEC.md 5.1),
// and a replay that reopened it would contradict that.
func TestReplayingADoneRunIsRefused(t *testing.T) {
	r := obs.refusedWhileDone
	if r.Code != http.StatusConflict {
		t.Fatalf("SPEC.md 14: replaying a DONE run is a 409; got %d: %s", r.Code, r.Body)
	}
	if r.Rejection == nil || r.Rejection.RunStatus == nil || *r.Rejection.RunStatus != "DONE" {
		t.Errorf("SPEC.md 10.5: the refusal must state DONE as the actual current status: %s", r.Body)
	}
	if r.Rejection != nil && r.Rejection.StepStatus != nil && *r.Rejection.StepStatus != "DONE" {
		t.Errorf("SPEC.md 5.5 L3, 10.5: a DONE run's last step is DONE; the refusal reported %q",
			*r.Rejection.StepStatus)
	}
	v := harness.Var(obs.doneRun)
	harness.Bool(t, "SPEC.md 14: a refused replay changes nothing - replay_count is still 0",
		"SELECT replay_count = 0 FROM runs WHERE run_id = :'run';", v)
	harness.Bool(t, "SPEC.md 5.1, 14: the run is still DONE",
		"SELECT status = 'DONE' FROM runs WHERE run_id = :'run';", v)
	harness.Bool(t, "SPEC.md 12.4: a run that never failed has no dead-letter entry",
		"SELECT count(*) = 0 FROM dead_letter_queue WHERE run_id = :'run';", v)
}

// TestReplayingAnUnknownRunIs404: SPEC.md 10.5's table gives 404 the meaning
// "no such entity", and the identifier used names none.
//
// The `error` slug is deliberately NOT asserted. SPEC.md 10.5 prints one body
// in full and it is a 409; it calls the slug stable and machine-readable but
// fixes no value for a 404. Asserting a string here would invent a contract no
// ruling covers (CLAUDE.md 9), so the test checks what SPEC.md does fix - the
// code - and the absence of any side effect.
func TestReplayingAnUnknownRunIs404(t *testing.T) {
	if obs.missingRun.Code != http.StatusNotFound {
		t.Errorf("SPEC.md 10.5: replaying a run_id that names no run is a 404; got %d: %s",
			obs.missingRun.Code, obs.missingRun.Body)
	}
	harness.Bool(t, "SPEC.md 10.5: a 404 creates nothing",
		"SELECT count(*) = 0 FROM runs WHERE run_id = :'run';", harness.Var(missingRunID))
}

// ---------------------------------------------------------------------------
// Across the legs
// ---------------------------------------------------------------------------

// TestRunIDNeverChanges is SPEC.md 14 in bold: "run_id never changes. A run is
// the unit of history; forking it would duplicate step history and break 'one
// run, one story'."
//
// The evidence is that every identifier the API returned before any replay
// still resolves to exactly one row after all of them, and that the environment
// holds exactly as many runs as the fixture started - five. A fork would show
// up as a sixth.
func TestRunIDNeverChanges(t *testing.T) {
	for _, run := range allRuns() {
		harness.Bool(t, "SPEC.md 14, 6.2: run_id is the primary key and never changes, "+
			"including across replay rounds",
			"SELECT count(*) = 1 FROM runs WHERE run_id = :'run';", harness.Var(run))
	}
	harness.Bool(t, "SPEC.md 14: no replay forked a run - the environment holds the five the fixture started",
		"SELECT count(*) = 5 FROM runs;")
}

// TestEveryRoundIsInspectable generalises SPEC.md 14's closing requirement over
// every run in the environment: "runs.replay_count and
// dead_letter_queue.replay_round together let the operator see, from a
// terminal, which round any given entry belonged to."
//
// Two things follow from SPEC.md 6.5's definition - replay_round is
// runs.replay_count at the moment the entry was written - and both are asserted
// as invariants rather than per leg: a run's entries carry the rounds 0, 1, 2 …
// with no gap and no repeat, and no entry can name a round the run has not
// reached.
func TestEveryRoundIsInspectable(t *testing.T) {
	// Sorted within each run, the rounds must read 0, 1, 2 … — which is what
	// comparing each row against its own position asserts. A gap breaks the
	// equality, and so does a repeat, because row_number() is distinct where
	// replay_round would not be.
	harness.Bool(t,
		"SPEC.md 6.5, 14: each run's dead-letter rounds are numbered 0, 1, 2 ... with no gap and no repeat",
		`SELECT count(*) = 0 FROM (
             SELECT d.replay_round,
                    row_number() OVER (PARTITION BY d.run_id ORDER BY d.replay_round) - 1 AS expected
               FROM dead_letter_queue d) t
          WHERE t.replay_round <> t.expected;`)
	harness.Bool(t,
		"SPEC.md 6.5, 14: no entry names a round beyond the number of replays the run has had - "+
			"replay_round is replay_count at the moment of the verdict, and replay_count only grows",
		`SELECT count(*) = 0 FROM dead_letter_queue d JOIN runs r ON r.run_id = d.run_id
          WHERE d.replay_round > r.replay_count;`)
}

// TestAttemptNumbersStayContiguousAcrossRounds: SPEC.md 6.4 makes attempt_no
// "1-based ordering within the step", and SPEC.md 14 resets attempt_count while
// leaving the attempts rows in place - so a step that has been through two
// rounds holds rows from both.
//
// Delta is the first milestone where that can be got wrong: a replayed round
// that restarted its numbering at 1 would give one step two rows numbered 1,
// and "ordering within the step" would no longer be true of the column.
func TestAttemptNumbersStayContiguousAcrossRounds(t *testing.T) {
	harness.Bool(t,
		"SPEC.md 6.4: attempt_no is 1-based and contiguous within each step, replay rounds included",
		`SELECT count(*) = 0 FROM (
             SELECT a.attempt_no,
                    row_number() OVER (PARTITION BY a.step_id ORDER BY a.attempt_no) AS expected
               FROM attempts a) t
          WHERE t.attempt_no <> t.expected;`)
	harness.Bool(t, "SPEC.md 6.4 invariant 1: every FAILED attempt still names a reason",
		"SELECT count(*) = 0 FROM attempts WHERE status = 'FAILED' AND failure_reason IS NULL;")
	harness.Bool(t, "SPEC.md 6.4 invariant 3: finished_at is present iff the attempt left RUNNING",
		"SELECT count(*) = 0 FROM attempts WHERE (status = 'RUNNING') <> (finished_at IS NULL);")
}

// TestNoImpossibleCombinationSurvivedAReplay: SPEC.md 5.6 calls these
// impossible "because of a transaction boundary, not because of a check
// somewhere", and SPEC.md 14 performs its work "in one transaction". A replay
// that moved the run without its step, or the step without its run, would show
// up here and nowhere else.
func TestNoImpossibleCombinationSurvivedAReplay(t *testing.T) {
	harness.Bool(t, "SPEC.md 5.6: no run is RUNNING with a DLQ last step",
		`SELECT count(*) = 0 FROM runs r WHERE r.status = 'RUNNING' AND
            (SELECT s.status FROM steps s WHERE s.run_id = r.run_id ORDER BY s.seq DESC LIMIT 1) = 'DLQ';`)
	harness.Bool(t, "SPEC.md 5.6: no run is DLQ with a RUNNING last step",
		`SELECT count(*) = 0 FROM runs r WHERE r.status = 'DLQ' AND
            (SELECT s.status FROM steps s WHERE s.run_id = r.run_id ORDER BY s.seq DESC LIMIT 1) = 'RUNNING';`)
	harness.Bool(t, "SPEC.md 5.6: no run is DONE with a step that is not",
		`SELECT count(*) = 0 FROM runs r WHERE r.status = 'DONE' AND
            (SELECT s.status FROM steps s WHERE s.run_id = r.run_id ORDER BY s.seq DESC LIMIT 1) <> 'DONE';`)
	// SPEC.md 6.2: owner_id and claimed_at are non-NULL only while the run is
	// RUNNING, and are always written and cleared as a pair (SPEC.md 8.7).
	// SPEC.md 14 clears both, which is the one place a replay touches them.
	harness.Bool(t, "SPEC.md 6.2, 8.7: coordination metadata is a pair, and only a RUNNING run holds it",
		`SELECT count(*) = 0 FROM runs
          WHERE (owner_id IS NULL) <> (claimed_at IS NULL)
             OR (status <> 'RUNNING' AND owner_id IS NOT NULL);`)
}

// TestDeltaIsWorkerSideOnly records the ruling this milestone was scoped by:
// delta demonstrates SPEC.md 12.3's worker-side column and leaves the
// planner-side one to milestone zeta.
//
// SPEC.md 12.1 is why it cannot be otherwise today: "the static planner simply
// cannot fail at run time - SPEC.md 6.1 validates its steps at submission, and
// it holds no state and makes no network call, so planner_attempt_count never
// leaves 0", and it never answers `fail` (SPEC.md 6.1). L5 is therefore
// unreachable with the planners that exist, and a leg that reached it by
// pointing an http planner at a dead address would be demonstrating an unbuilt
// milestone rather than a mechanism.
//
// The absence is asserted rather than left silent: an unasserted absence is how
// a milestone quietly acquires the next one's behaviour.
func TestDeltaIsWorkerSideOnly(t *testing.T) {
	harness.Bool(t, "SPEC.md 12.3: every dead-letter entry in delta is worker-side and names its step",
		"SELECT count(*) = 0 FROM dead_letter_queue WHERE step_id IS NULL;")
	harness.Bool(t, "SPEC.md 12.3 L4: every entry's reason is worker_budget_exhausted",
		"SELECT bool_and(reason = 'worker_budget_exhausted') FROM dead_letter_queue;")
	harness.Bool(t, "SPEC.md 12.1: the static planner cannot fail, so planner_attempt_count stays 0",
		"SELECT bool_and(planner_attempt_count = 0) FROM runs;")
	harness.Bool(t, "SPEC.md 12.1: and no run carries a planner error",
		"SELECT count(*) = 0 FROM runs WHERE last_planner_error IS NOT NULL;")
}

// TestWhatDeltaDoesNotDemonstrate asserts the absence of everything this
// milestone excludes, where that absence is observable.
//
// SPEC.md 14's table gives CANCELLED as the third status a replay is refused
// for. Delta cannot produce it: cancellation is SPEC.md 15, and SPEC.md 18
// orders it at milestone iota, so POST /runs/{run_id}/cancel does not exist.
// Staging a CANCELLED row by hand would mean the suite writing a state the
// product cannot reach, which is not evidence about the product. The row is
// therefore left to iota and its absence is asserted here.
func TestWhatDeltaDoesNotDemonstrate(t *testing.T) {
	harness.Bool(t, "SPEC.md 15 is milestone iota: nothing has been cancelled, "+
		"so SPEC.md 14's CANCELLED row is not demonstrated here",
		"SELECT count(*) = 0 FROM runs WHERE status = 'CANCELLED';")
	harness.Bool(t, "SPEC.md 5.3: no attempt was orphaned - beta is where an orchestrator dies",
		"SELECT count(*) = 0 FROM attempts WHERE failure_reason = 'orphaned';")
	harness.Bool(t, "SPEC.md 5.3: no attempt timed out - every failure here is the worker's own answer",
		"SELECT count(*) = 0 FROM attempts WHERE failure_reason = 'timeout';")
	harness.Bool(t, "SPEC.md 9.7: sync + envelope is the only legal combination before theta and epsilon",
		"SELECT bool_and(connection_mode = 'sync') FROM attempts;")
}

// TestEveryRunLeftTheDeadLetterQueue is the milestone's closing statement, read
// off the whole environment: five runs, five DONE, and the five dead-letter
// entries still there as history.
//
// SPEC.md 5.5's L4 says a run in DLQ has exactly two exits, "replay, or cancel".
// Delta took the first one four times, and the entries that recorded those
// failures survive every one of them (SPEC.md 6.7).
func TestEveryRunLeftTheDeadLetterQueue(t *testing.T) {
	harness.Bool(t, "SPEC.md 14, 5.5 L4: every run was replayed out of DLQ and finished",
		"SELECT count(*) = 5 AND bool_and(status = 'DONE') FROM runs;")
	harness.Bool(t, "SPEC.md 6.7, 12.4: the dead-letter history outlives the runs' recovery",
		"SELECT count(*) = 5 FROM dead_letter_queue;")
	harness.Bool(t, "SPEC.md 14, 6.2: the four runs that were replayed carry a non-zero replay_count",
		"SELECT count(*) = 4 FROM runs WHERE replay_count > 0;")
}

// allRuns is every run the fixture started, in leg order.
func allRuns() []string {
	return []string{obs.resumesRun, obs.twoRoundsRun, obs.gateRun, obs.inFlightRun, obs.doneRun}
}

func deref(s *string) string {
	if s == nil {
		return "<absent>"
	}
	return *s
}
