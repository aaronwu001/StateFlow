package rawpath

import (
	"fmt"
	"testing"

	"github.com/aaronwu001/piton/test/theta/harness"
)

// Every assertion below names the SPEC.md rule it came from. CLAUDE.md 5.1
// permits exactly one source, and nothing here was derived by reading an
// implementation: raw dispatch does not exist at the time this file is written.
//
// A note on the word "verbatim". SPEC.md 9.5 and 9.6 both use it, and the
// columns it lands in are JSONB, which normalises whitespace and key order.
// Document equality is therefore the strongest claim these queries can make,
// and it is the claim the rule is about: no field added, none removed, none
// rewritten.

// TestRunReachedDone is the milestone's headline. SPEC.md 5.1 makes DONE
// terminal, and a run whose every step ran against an endpoint that has never
// heard of Piton is exactly what SPEC.md 18's theta row promises.
func TestRunReachedDone(t *testing.T) {
	if finalStatus != "DONE" {
		t.Fatalf("SPEC.md 18 (theta): the run must reach DONE; it reached %q", finalStatus)
	}
	harness.Bool(t, "SPEC.md 6.2: a run that has left RUNNING holds no owner_id",
		"SELECT owner_id IS NULL FROM runs WHERE run_id = :'run';", rv())
	harness.Bool(t, "SPEC.md 6.2, SPEC.md 14: nothing replayed this run",
		"SELECT replay_count = 0 FROM runs WHERE run_id = :'run';", rv())
	harness.Bool(t, "SPEC.md 6.2, SPEC.md 12.3: the static planner never failed, so no planner budget was consumed",
		"SELECT planner_attempt_count = 0 FROM runs WHERE run_id = :'run';", rv())
	harness.Bool(t, "SPEC.md 6.2: runs.input is stored verbatim",
		"SELECT input = :'in'::jsonb FROM runs WHERE run_id = :'run';",
		rv(), "in="+harness.RunInput)
}

// TestStepsAreContiguousAndDone asserts the shape SPEC.md 6.3 and 3.3 require
// of the step rows: one per static step, seq contiguous from 1, every row DONE,
// and one attempt each because nothing failed.
func TestStepsAreContiguousAndDone(t *testing.T) {
	harness.Bool(t, fmt.Sprintf("SPEC.md 9.3: a static planner creates one step per static StepSpec (%d of them)", n),
		fmt.Sprintf("SELECT count(*) = %d FROM steps WHERE run_id = :'run';", n), rv())
	harness.Bool(t, "SPEC.md 3.3: seq is contiguous from 1",
		fmt.Sprintf("SELECT array_agg(seq ORDER BY seq) = (SELECT array_agg(g) FROM generate_series(1, %d) g)"+
			" FROM steps WHERE run_id = :'run';", n), rv())
	harness.Bool(t, "SPEC.md 5.2: every step reached DONE",
		"SELECT bool_and(status = 'DONE') FROM steps WHERE run_id = :'run';", rv())
	harness.Bool(t, "SPEC.md 6.3: nothing was retried, so every step consumed exactly one attempt",
		"SELECT bool_and(attempt_count = 1) FROM steps WHERE run_id = :'run';", rv())
	harness.Bool(t, "SPEC.md 6.3: completed_at is set exactly when status leaves RUNNING",
		"SELECT bool_and(completed_at IS NOT NULL) FROM steps WHERE run_id = :'run';", rv())
	harness.Bool(t, "SPEC.md 6.3: decision holds the StepSpec exactly as the planner returned it",
		"SELECT bool_and(decision -> 'dispatch_style' IS NOT NULL) FROM steps WHERE run_id = :'run';", rv())
}

// TestRawBodyWasParamsAndNothingElse is SPEC.md 9.5's raw rule, read back off
// the endpoint that received it: "the body is params, verbatim and nothing
// else".
//
// The negative half matters more than the positive one. An implementation that
// sent the envelope and *also* the params would satisfy "params arrived"; what
// it would not satisfy is that no Piton field arrived at all.
func TestRawBodyWasParamsAndNothingElse(t *testing.T) {
	harness.Bool(t, "SPEC.md 9.5: the raw body is params, verbatim",
		"SELECT (output -> 'seen_body') = :'params'::jsonb FROM steps"+
			" WHERE run_id = :'run' AND seq = 1;",
		rv(), "params="+string(rawParams))

	for _, field := range []string{
		"run_id", "step_id", "attempt_id", "connection_mode", "inputs", "callback_url",
	} {
		harness.Bool(t,
			"SPEC.md 9.5: a raw body carries params and NOTHING else, so Piton's envelope field "+
				field+" must never appear in it",
			"SELECT NOT (output -> 'seen_body') ? :'field' FROM steps"+
				" WHERE run_id = :'run' AND seq = 1;",
			rv(), "field="+field)
	}
}

// TestRawRequestDeclaredJSON is the header rule of SPEC.md 9.5: a raw request
// is sent with content-type: application/json, because 9.4 already requires
// params to be an object.
func TestRawRequestDeclaredJSON(t *testing.T) {
	harness.Bool(t, "SPEC.md 9.5: a raw request is sent with content-type: application/json",
		"SELECT (output ->> 'seen_content_type') LIKE 'application/json%' FROM steps"+
			" WHERE run_id = :'run' AND seq = 1;", rv())
}

// TestInputFromInsideParamsIsOrdinaryData is SPEC.md 9.5's last raw paragraph:
// "a key literally named input_from INSIDE params is ordinary data and is
// transmitted verbatim, becoming a top-level key of the raw body. raw does not
// interpret payload content."
//
// It is a rule about what the orchestrator must NOT do - strip the key, resolve
// it as a reference, or reject the step - so the endpoint having received it
// unchanged is the whole assertion.
func TestInputFromInsideParamsIsOrdinaryData(t *testing.T) {
	harness.Bool(t, "SPEC.md 9.5: a key named input_from inside params becomes a top-level key of the raw body",
		"SELECT (output -> 'seen_body') ? 'input_from' FROM steps"+
			" WHERE run_id = :'run' AND seq = 1;", rv())
	harness.Bool(t, "SPEC.md 9.5: it is transmitted verbatim, not interpreted",
		"SELECT (output -> 'seen_body' -> 'input_from') = (:'params'::jsonb -> 'input_from')"+
			" FROM steps WHERE run_id = :'run' AND seq = 1;",
		rv(), "params="+string(rawParams))
}

// TestRawOutputIsTheEntireBody is the storage half of SPEC.md 9.6: in raw mode
// "the entire response body verbatim is the output", and 9.6's per-mode
// paragraph says so in as many words - envelope mode stores the response's
// output field alone, raw mode stores the entire body.
func TestRawOutputIsTheEntireBody(t *testing.T) {
	for _, key := range []string{"upper", "seen_body", "seen_content_type", "seen_top_level_keys"} {
		harness.Bool(t,
			"SPEC.md 9.6: raw stores the ENTIRE response body, so the endpoint's key "+key+
				" must be present at the top level of steps.output",
			"SELECT output ? :'key' FROM steps WHERE run_id = :'run' AND seq = 1;",
			rv(), "key="+key)
	}
	harness.Equal(t, "SPEC.md 9.6: the endpoint answered with the uppercased text it was given",
		"HELLO",
		"SELECT output ->> 'upper' FROM steps WHERE run_id = :'run' AND seq = 1;", rv())
}

// TestRawDoesNotInterpretPayload is the sharpest rule theta carries, and the
// one an implementation is most likely to get wrong by being helpful.
//
// Step 2 answers HTTP 200 with a document of the shape
// {"status":"failure","error":"..."}.
// In envelope mode that is a worker reporting failure (SPEC.md 9.6). In raw
// mode it is nothing of the kind: 9.6's raw row says "any 2xx" succeeds and
// "the entire response body verbatim is the output", and the dispatch style
// decides whether Piton's protocol words mean anything at all. The step must
// therefore be DONE, with that document stored whole.
func TestRawDoesNotInterpretPayload(t *testing.T) {
	harness.Equal(t, "SPEC.md 9.6: in raw mode any 2xx is a success, whatever the body says",
		"DONE",
		"SELECT status FROM steps WHERE run_id = :'run' AND seq = 2;", rv())
	harness.Bool(t, "SPEC.md 9.6: the body is stored whole, including the words status and error",
		"SELECT output ? 'status' AND output ? 'error' FROM steps"+
			" WHERE run_id = :'run' AND seq = 2;", rv())
	harness.Equal(t, "SPEC.md 9.6: raw does not read Piton's protocol out of a stranger's document",
		"failure",
		"SELECT output ->> 'status' FROM steps WHERE run_id = :'run' AND seq = 2;", rv())
	harness.Bool(t, "SPEC.md 6.4: the attempt that produced it is DONE with no failure_reason",
		"SELECT bool_and(a.status = 'DONE' AND a.failure_reason IS NULL) FROM attempts a"+
			" JOIN steps s USING (step_id) WHERE s.run_id = :'run' AND s.seq = 2;", rv())
}

// TestEnvelopeStepIsStillUnwrapped asserts the other half of SPEC.md 9.6's
// per-mode rule inside the same run: the envelope step stores the response's
// `output` field alone.
//
// The two assertions together are what makes the rule observable. Either one on
// its own is satisfied by an implementation that treats both modes the same.
func TestEnvelopeStepIsStillUnwrapped(t *testing.T) {
	harness.Bool(t, "SPEC.md 9.6: envelope mode stores the response's output field alone, so Piton's wrapper is not in the row",
		"SELECT NOT (output ? 'status') FROM steps WHERE run_id = :'run' AND seq = 3;", rv())
	harness.Bool(t, "SPEC.md 9.6: what remains is the worker's own result",
		"SELECT output ? 'echo' FROM steps WHERE run_id = :'run' AND seq = 3;", rv())
}

// TestRawOutputReachesTheNextWorker is SPEC.md 9.5's inputs rule applied across
// a mode boundary: inputs is "a map from step_id to that step's stored output",
// with no exemption for a step that ran in raw mode.
//
// SPEC.md 9.4 supplies which step: input_from omitted means "the previous step
// only", and step 3 omits it, so the map must hold exactly step 2 - the raw
// step whose body was a stranger's document.
func TestRawOutputReachesTheNextWorker(t *testing.T) {
	harness.Bool(t, "SPEC.md 9.4: input_from omitted means the previous step only, so inputs holds exactly one entry",
		"SELECT count(*) = 1 FROM steps s,"+
			" jsonb_object_keys(s.output -> 'echo' -> 'inputs') k"+
			" WHERE s.run_id = :'run' AND s.seq = 3;", rv())
	harness.Bool(t, "SPEC.md 9.5: inputs is keyed by step_id, and the key is the previous step",
		"SELECT s3.output -> 'echo' -> 'inputs' ? s2.step_id::text"+
			" FROM steps s3, steps s2"+
			" WHERE s3.run_id = :'run' AND s3.seq = 3"+
			" AND s2.run_id = :'run' AND s2.seq = 2;", rv())
	harness.Bool(t, "SPEC.md 9.5: the value is that step's STORED output - a raw step's output travels like any other",
		"SELECT (s3.output -> 'echo' -> 'inputs' -> s2.step_id::text) = s2.output"+
			" FROM steps s3, steps s2"+
			" WHERE s3.run_id = :'run' AND s3.seq = 3"+
			" AND s2.run_id = :'run' AND s2.seq = 2;", rv())
}

// TestAttemptRowsMatchTheSteps asserts SPEC.md 6.4's row per dispatch: one per
// step here, since nothing was retried.
//
// connection_mode is asserted and dispatch_style is not, because 6.4 carries
// only the first. The style lives in steps.decision, which 6.3 stores verbatim.
func TestAttemptRowsMatchTheSteps(t *testing.T) {
	harness.Bool(t, fmt.Sprintf("SPEC.md 6.4: one attempt row per step (%d)", n),
		fmt.Sprintf("SELECT count(*) = %d FROM attempts WHERE run_id = :'run';", n), rv())
	harness.Bool(t, "SPEC.md 6.4: every attempt is DONE, with no failure_reason and no error_text",
		"SELECT bool_and(status = 'DONE' AND failure_reason IS NULL AND error_text IS NULL)"+
			" FROM attempts WHERE run_id = :'run';", rv())
	harness.Bool(t, "SPEC.md 6.4: connection_mode is copied from the StepSpec, and theta is sync throughout",
		"SELECT bool_and(connection_mode = 'sync') FROM attempts WHERE run_id = :'run';", rv())
	harness.Bool(t, "SPEC.md 6.4: finished_at is set exactly when status leaves RUNNING",
		"SELECT bool_and(finished_at IS NOT NULL) FROM attempts WHERE run_id = :'run';", rv())
	harness.Bool(t, "SPEC.md 6.4: attempt_no is 1-based and contiguous within its step",
		"SELECT bool_and(attempt_no = 1) FROM attempts WHERE run_id = :'run';", rv())
	harness.Bool(t, "SPEC.md 6.4, SPEC.md 14: nothing replayed this run, so every attempt belongs to round 0",
		"SELECT bool_and(replay_round = 0) FROM attempts WHERE run_id = :'run';", rv())
	harness.Bool(t, "SPEC.md 6.4: the attempt carries the same output the owner promoted onto the step",
		"SELECT bool_and(a.output = s.output) FROM attempts a JOIN steps s USING (step_id)"+
			" WHERE s.run_id = :'run';", rv())
}

// TestNothingDeadLettered asserts the absence SPEC.md 6.5 makes observable: the
// theta scenario is a happy path, and a dead-letter entry would mean a budget
// was exhausted somewhere.
func TestNothingDeadLettered(t *testing.T) {
	harness.Bool(t, "SPEC.md 6.5: nothing in the theta scenario exhausts a budget",
		"SELECT count(*) = 0 FROM dead_letter_queue WHERE run_id = :'run';", rv())
	harness.Bool(t, "SPEC.md 6.4: no attempt failed",
		"SELECT count(*) = 0 FROM attempts WHERE run_id = :'run' AND status = 'FAILED';", rv())
}
