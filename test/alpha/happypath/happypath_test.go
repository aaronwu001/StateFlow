package happypath

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aaronwu001/piton/test/alpha/harness"
)

// ---------------------------------------------------------------------------
// The run
// ---------------------------------------------------------------------------

// SPEC.md 18.1: the run "walks through every static step against a sync
// envelope worker and reaches DONE". SPEC.md 5.1 makes DONE terminal.
func TestRunReachedDone(t *testing.T) {
	if finalStatus != "DONE" {
		t.Fatalf("SPEC.md 18.1 requires the run to reach DONE; it reached %q", finalStatus)
	}
	harness.Bool(t, "SPEC.md 18.1: the run is DONE in the database",
		"SELECT count(*) = 1 FROM runs WHERE run_id = :'run' AND status = 'DONE';", rv())
}

// SPEC.md 18.1 states both counters explicitly. planner_attempt_count = 0 is
// not an accident of timing: SPEC.md 12.1 and 6.1 make the built-in static
// planner incapable of failing, so no planner budget can ever be consumed.
func TestRunCountersAreZero(t *testing.T) {
	harness.Bool(t, "SPEC.md 18.1: replay_count = 0 - no replay happened",
		"SELECT count(*) = 1 FROM runs WHERE run_id = :'run' AND replay_count = 0;", rv())
	harness.Bool(t, "SPEC.md 18.1, 12.1: planner_attempt_count = 0 - the static planner cannot fail",
		"SELECT count(*) = 1 FROM runs WHERE run_id = :'run' AND planner_attempt_count = 0;", rv())
	// SPEC.md 6.2 no longer keeps a last_planner_error column: the planner's
	// own rows carry the diagnosis (SPEC.md 6.8). The claim is therefore about
	// the calls themselves - and it is a stronger one than the column allowed,
	// because SPEC.md 12.1 exempts no planner from the path: "these rules apply
	// to every planner, including the built-in static one, with no exemption",
	// so the static planner's answers are rows like any other's.
	//
	// A run with n steps that reached DONE was asked n + 1 times: once at each
	// decision point that produced a step, and once more for the `done` that
	// ended it (SPEC.md 4.2's L1).
	steps, err := harness.StaticSteps()
	if err != nil {
		t.Fatal(err)
	}
	harness.Bool(t, fmt.Sprintf("SPEC.md 4.2, 6.8: the planner was asked %d times and answered every time",
		len(steps)+1),
		fmt.Sprintf(`SELECT count(*) = %d
                          AND count(*) FILTER (WHERE status = 'DONE') = %d
                          AND count(*) FILTER (WHERE answer = 'continue') = %d
                          AND count(*) FILTER (WHERE answer = 'done') = 1
                       FROM planner_calls WHERE run_id = :'run';`,
			len(steps)+1, len(steps)+1, len(steps)),
		rv())
	harness.Bool(t, "SPEC.md 12.1: the static planner cannot fail, so no call is FAILED",
		"SELECT count(*) = 0 FROM planner_calls WHERE run_id = :'run' AND status = 'FAILED';", rv())
	harness.Bool(t, "SPEC.md 6.8: call_no is 1-based and contiguous within the run",
		`SELECT count(*) = 0 FROM (
             SELECT call_no, row_number() OVER (ORDER BY call_no) AS expected
               FROM planner_calls WHERE run_id = :'run') t
          WHERE t.call_no <> t.expected;`, rv())
}

// SPEC.md 6.2 stores the operator-supplied workflow input "verbatim".
//
// Compared as a JSON value rather than as bytes: SPEC.md 7.1 leaves the byte
// encoding to the backend, and the Postgres one normalises jsonb.
func TestRunInputStoredVerbatim(t *testing.T) {
	harness.Bool(t, "SPEC.md 6.2: runs.input is exactly what was submitted",
		"SELECT (input::jsonb) = (:'expected')::jsonb FROM runs WHERE run_id = :'run';",
		rv(), "expected="+harness.RunInput)
}

// SPEC.md 5.5: a cleanly finished run is combination L3 - run DONE, derived
// last_step DONE. SPEC.md 5.4 defines last_step as the status of the
// highest-seq step, derived and never stored.
func TestStateModelCombinationIsL3(t *testing.T) {
	harness.Bool(t, "SPEC.md 5.4, 5.5: the run ended in L3 (DONE / DONE)",
		`SELECT (SELECT status FROM runs WHERE run_id = :'run') = 'DONE'
            AND (SELECT status FROM steps WHERE run_id = :'run'
                  ORDER BY seq DESC LIMIT 1) = 'DONE';`, rv())
}

// ---------------------------------------------------------------------------
// The steps
// ---------------------------------------------------------------------------

// SPEC.md 18.1: "one row per static step, contiguous seq from 1". SPEC.md 6.1
// defines the static planner as answering planner_static_steps[n] where n is
// the number of steps the run already has, so the count is the array's length
// and nothing else. SPEC.md 3.3 and 6.3 require seq to be contiguous from 1.
func TestOneStepPerStaticStep(t *testing.T) {
	harness.Bool(t, fmt.Sprintf("SPEC.md 18.1, 6.1: exactly %d steps, one per static step", n),
		fmt.Sprintf("SELECT count(*) = %d FROM steps WHERE run_id = :'run';", n), rv())
	harness.Bool(t, "SPEC.md 3.3, 6.3: seq is contiguous from 1 and has no duplicate",
		fmt.Sprintf(`SELECT count(*) = %d AND min(seq) = 1 AND max(seq) = %d
                        AND count(DISTINCT seq) = %d
                       FROM steps WHERE run_id = :'run';`, n, n, n), rv())
}

// SPEC.md 18.1: "every row DONE, attempt_count = 1, output present".
//
// Note the direction of the two output assertions. SPEC.md 6.3 forbids treating
// "output is present" as meaning the step finished; completion is asserted from
// status, and the presence of an output is asserted separately as a fact of its
// own. Neither is inferred from the other.
func TestEveryStepFinishedCleanly(t *testing.T) {
	harness.Bool(t, "SPEC.md 18.1: every step is DONE",
		fmt.Sprintf(`SELECT count(*) FILTER (WHERE status = 'DONE') = %d
                       FROM steps WHERE run_id = :'run';`, n), rv())
	harness.Bool(t, "SPEC.md 18.1: every step consumed exactly one attempt of budget",
		fmt.Sprintf(`SELECT count(*) FILTER (WHERE attempt_count = 1) = %d
                       FROM steps WHERE run_id = :'run';`, n), rv())
	harness.Bool(t, "SPEC.md 18.1: every step has an output present",
		fmt.Sprintf(`SELECT count(*) FILTER (WHERE output IS NOT NULL
                                              AND octet_length(output::text) > 0) = %d
                       FROM steps WHERE run_id = :'run';`, n), rv())
	harness.Bool(t, "SPEC.md 6.3: completed_at is set exactly when status leaves RUNNING",
		fmt.Sprintf(`SELECT count(*) FILTER (WHERE completed_at IS NOT NULL) = %d
                       FROM steps WHERE run_id = :'run';`, n), rv())
}

// SPEC.md 6.3: steps.decision holds "the StepSpec exactly as the planner
// returned it", and SPEC.md 6.1 fixes what the static planner returns -
// planner_static_steps[n], element by element, in order.
func TestStepDecisionIsTheStepSpecVerbatim(t *testing.T) {
	for i, spec := range specs {
		seq := i + 1
		compact, err := json.Marshal(json.RawMessage(spec))
		if err != nil {
			t.Fatalf("planner_static_steps[%d] is not valid JSON: %v", i, err)
		}
		harness.Bool(t,
			fmt.Sprintf("SPEC.md 6.1, 6.3: steps.decision at seq %d is planner_static_steps[%d] verbatim", seq, i),
			fmt.Sprintf(`SELECT (decision::jsonb) = (:'spec')::jsonb
                           FROM steps WHERE run_id = :'run' AND seq = %d;`, seq),
			rv(), "spec="+string(compact))
	}
}

// ---------------------------------------------------------------------------
// The attempts
// ---------------------------------------------------------------------------

// SPEC.md 18.1: "one row per step, all DONE, connection_mode = 'sync',
// failure_reason NULL, finished_at set". The last two are SPEC.md 6.4's
// invariants 2 and 3, which the backend enforces rather than the caller.
func TestOneSuccessfulAttemptPerStep(t *testing.T) {
	harness.Bool(t, fmt.Sprintf("SPEC.md 18.1: exactly %d attempts, each numbered 1", n),
		fmt.Sprintf(`SELECT (SELECT count(*) FROM attempts WHERE run_id = :'run') = %d
                        AND (SELECT count(*) FILTER (WHERE attempt_no = 1)
                               FROM attempts WHERE run_id = :'run') = %d;`, n, n), rv())
	harness.Bool(t, "SPEC.md 18.1: every attempt is DONE",
		fmt.Sprintf(`SELECT count(*) FILTER (WHERE status = 'DONE') = %d
                       FROM attempts WHERE run_id = :'run';`, n), rv())
	harness.Bool(t, "SPEC.md 9.7: sync is the only connection_mode legal in alpha",
		fmt.Sprintf(`SELECT count(*) FILTER (WHERE connection_mode = 'sync') = %d
                       FROM attempts WHERE run_id = :'run';`, n), rv())
	harness.Bool(t, "SPEC.md 6.4 invariant 2: a non-FAILED attempt has no failure_reason",
		fmt.Sprintf(`SELECT count(*) FILTER (WHERE failure_reason IS NULL) = %d
                       FROM attempts WHERE run_id = :'run';`, n), rv())
	harness.Bool(t, "SPEC.md 6.4 invariant 3: finished_at is present iff status is not RUNNING",
		fmt.Sprintf(`SELECT count(*) FILTER (WHERE finished_at IS NOT NULL) = %d
                       FROM attempts WHERE run_id = :'run';`, n), rv())
}

// SPEC.md 18.1: "SELECT count(*) FROM dead_letter_queue WHERE run_id = :run; -- 0".
func TestNoDeadLetterEntries(t *testing.T) {
	harness.Bool(t, "SPEC.md 18.1: this run produced no dead-letter entry",
		"SELECT count(*) = 0 FROM dead_letter_queue WHERE run_id = :'run';", rv())
}

// ---------------------------------------------------------------------------
// The dispatch envelope
// ---------------------------------------------------------------------------
//
// SPEC.md 9.5 defines what a sync envelope carries: run_id, step_id,
// attempt_id, connection_mode, params and inputs, with callback_url "omitted
// entirely" in sync mode. The echo worker of demos/alpha/docker-compose.yml
// reports back what it actually received, so the envelope contract becomes a
// row in steps.output that can be asserted in SQL rather than something only a
// log line would ever have shown.

func TestEnvelopeCarriedExactlyTheSpecifiedFields(t *testing.T) {
	harness.Bool(t, "SPEC.md 9.5: every envelope carried all six required fields and no unknown field",
		fmt.Sprintf(`SELECT count(*) FILTER (
                            WHERE jsonb_array_length(
                                    (output::jsonb) -> 'echo' -> 'missing_envelope_fields') = 0
                              AND jsonb_array_length(
                                    (output::jsonb) -> 'echo' -> 'unknown_envelope_fields') = 0) = %d
                       FROM steps WHERE run_id = :'run';`, n), rv())
	harness.Bool(t, "SPEC.md 9.5: every envelope declared connection_mode = 'sync'",
		fmt.Sprintf(`SELECT count(*) FILTER (
                            WHERE (output::jsonb) -> 'echo' ->> 'connection_mode' = 'sync') = %d
                       FROM steps WHERE run_id = :'run';`, n), rv())
	harness.Bool(t, "SPEC.md 9.5: callback_url is omitted entirely in sync mode",
		fmt.Sprintf(`SELECT count(*) FILTER (
                            WHERE (output::jsonb) -> 'echo' ->> 'has_callback_url' = 'false') = %d
                       FROM steps WHERE run_id = :'run';`, n), rv())
	harness.Bool(t, "SPEC.md 9.5: each envelope's run_id and step_id addressed the row it belonged to",
		fmt.Sprintf(`SELECT count(*) FILTER (
                            WHERE (output::jsonb) -> 'echo' ->> 'run_id'  = run_id::text
                              AND (output::jsonb) -> 'echo' ->> 'step_id' = step_id::text) = %d
                       FROM steps WHERE run_id = :'run';`, n), rv())
	harness.Bool(t, "SPEC.md 9.5: each envelope's params were the StepSpec's params",
		fmt.Sprintf(`SELECT count(*) FILTER (
                            WHERE (output::jsonb) -> 'echo' -> 'params'
                                = coalesce((decision::jsonb) -> 'params', '{}'::jsonb)) = %d
                       FROM steps WHERE run_id = :'run';`, n), rv())
}

// SPEC.md 9.4 gives input_from two meanings this workflow uses, and they are
// the only two a static planner can express: an empty array means "nothing",
// and an absent key means "the previous step only". SPEC.md 9.5 then says
// inputs maps step_id to that step's output, so what arrived is checkable
// against the identity of the preceding row.
//
// Why a static planner cannot express anything else: input_from is an array of
// step_id, and step_ids are assigned at run time, which a static array written
// before the run existed cannot know.
func TestInputFromResolution(t *testing.T) {
	harness.Bool(t, "SPEC.md 9.4: seq 1 declared input_from [] and received nothing",
		`SELECT ((output::jsonb) -> 'echo' ->> 'input_count')::int = 0
           FROM steps WHERE run_id = :'run' AND seq = 1;`, rv())
	harness.Bool(t, "SPEC.md 9.4, 9.5: every later step omitted input_from and received exactly the previous step",
		fmt.Sprintf(`SELECT count(*) = %d
                       FROM steps s
                       JOIN steps p ON p.run_id = s.run_id AND p.seq = s.seq - 1
                      WHERE s.run_id = :'run' AND s.seq > 1
                        AND ((s.output::jsonb) -> 'echo' ->> 'input_count')::int = 1
                        AND (s.output::jsonb) -> 'echo' -> 'input_step_ids'
                          = jsonb_build_array(p.step_id::text);`, n-1), rv())
}

// ---------------------------------------------------------------------------
// What alpha deliberately does not demonstrate, asserted as absence
// ---------------------------------------------------------------------------
//
// SPEC.md 18.1 lists what alpha does not demonstrate: retries, DLQ, crash
// recovery, replay, cancellation, raw dispatch, async, an HTTP planner, or any
// override. Each has its own milestone. Asserting their absence is what makes
// "the happy path" mean something: a run that quietly retried once would still
// satisfy "reached DONE".

func TestNothingBeyondAlphaHappened(t *testing.T) {
	harness.Bool(t, "SPEC.md 18.1: no step went to DLQ",
		"SELECT count(*) = 0 FROM steps WHERE run_id = :'run' AND status = 'DLQ';", rv())
	harness.Bool(t, "SPEC.md 18.1: no attempt failed, so no retry was needed",
		"SELECT count(*) = 0 FROM attempts WHERE run_id = :'run' AND status = 'FAILED';", rv())
	harness.Bool(t, "SPEC.md 18.1, 9.7: no async attempt exists - async is milestone epsilon",
		"SELECT count(*) = 0 FROM attempts WHERE run_id = :'run' AND connection_mode = 'async';", rv())
	harness.Bool(t, "SPEC.md 18.1: no step was cancelled",
		"SELECT count(*) = 0 FROM steps WHERE run_id = :'run' AND status = 'CANCELLED';", rv())
}
