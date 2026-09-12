package rawfailure

import (
	"fmt"
	"testing"

	"github.com/aaronwu001/piton/test/theta/harness"
)

// Every assertion below names the SPEC.md rule it came from (CLAUDE.md 5.1).
// Nothing here was derived by reading an implementation: at the time this file
// is written, internal/dispatch answers a raw step with "not implemented", so
// both runs would reach DLQ for the wrong reason - which is precisely what
// these assertions are able to tell apart.

func nv() string { return "run=" + non2xxRun }
func jv() string { return "run=" + notJSONRun }

// TestNon2xxRunReachedDLQ walks SPEC.md 12.2's ladder for the HTTP 500
// endpoint: every attempt fails, the budget is consumed, and the step and the
// run go to DLQ together.
func TestNon2xxRunReachedDLQ(t *testing.T) {
	if non2xxStatus != "DLQ" {
		t.Fatalf("SPEC.md 12.2: a step that exhausts step_max_attempts sends its run to DLQ; "+
			"the run reached %q", non2xxStatus)
	}
	harness.Equal(t, "SPEC.md 12.2: step and run go to DLQ in one transaction",
		"DLQ", "SELECT status FROM steps WHERE run_id = :'run';", nv())
	harness.Bool(t, "SPEC.md 6.3: output is set only when a step is DONE",
		"SELECT output IS NULL FROM steps WHERE run_id = :'run';", nv())
	harness.Bool(t, fmt.Sprintf("SPEC.md 12.2: the step consumed its whole budget (%d)", maxAttempts),
		fmt.Sprintf("SELECT attempt_count = %d FROM steps WHERE run_id = :'run';", maxAttempts), nv())
}

// TestNon2xxIsTransportError is SPEC.md 5.3's classification, which names the
// case in as many words: transport_error is "the HTTP exchange did not produce
// a usable reply before deadline_at - non-2xx, connection refused, DNS failure,
// connection reset".
//
// The negative half is the one 5.3 warns about at length: this endpoint refuses
// instantly, well inside a 300-second budget, so timeout would be the wrong
// label and would tell the operator to raise a budget that is not the problem.
func TestNon2xxIsTransportError(t *testing.T) {
	harness.Bool(t, fmt.Sprintf("SPEC.md 12.2: one attempt row per dispatch (%d)", maxAttempts),
		fmt.Sprintf("SELECT count(*) = %d FROM attempts WHERE run_id = :'run';", maxAttempts), nv())
	harness.Bool(t, "SPEC.md 9.6: a non-2xx is always a failure, whatever the body says",
		"SELECT bool_and(status = 'FAILED') FROM attempts WHERE run_id = :'run';", nv())
	harness.Bool(t, "SPEC.md 5.3: a non-2xx is transport_error",
		"SELECT bool_and(failure_reason = 'transport_error') FROM attempts WHERE run_id = :'run';", nv())
	harness.Bool(t, "SPEC.md 5.3: timeout is decided by the clock, and this endpoint answered at once",
		"SELECT count(*) = 0 FROM attempts WHERE run_id = :'run' AND failure_reason = 'timeout';", nv())
	harness.Bool(t, "SPEC.md 6.4: a failed attempt has no output",
		"SELECT bool_and(output IS NULL) FROM attempts WHERE run_id = :'run';", nv())
	harness.Bool(t, "SPEC.md 6.4: finished_at is set exactly when status leaves RUNNING",
		"SELECT bool_and(finished_at IS NOT NULL) FROM attempts WHERE run_id = :'run';", nv())
}

// TestNon2xxBodyIsStoredAsErrorText is SPEC.md 9.6's raw failure cell - "the
// truncated body is stored as error text" - and SPEC.md 6.4's limit on it.
//
// The endpoint answers a body of ten thousand B characters precisely so that
// both halves are observable: the body reached the row, and the row obeyed the
// 4 KB limit. Containment rather than equality, because 6.4 calls the column
// "diagnostic text" and does not forbid the orchestrator from naming the worker
// and the status code beside the body.
func TestNon2xxBodyIsStoredAsErrorText(t *testing.T) {
	harness.Bool(t, "SPEC.md 9.6: the body is stored as error text",
		"SELECT bool_and(error_text LIKE '%BBBBBBBB%') FROM attempts WHERE run_id = :'run';", nv())
	harness.Bool(t, "SPEC.md 6.4: error_text is truncated to 4 KB, and the orchestrator truncates before writing",
		"SELECT bool_and(octet_length(error_text) <= 4096) FROM attempts WHERE run_id = :'run';", nv())
}

// TestNotJSONRunReachedDLQ is the rule the theta amendment produced: SPEC.md
// 9.6 requires a raw worker's response body to be a valid JSON document, and a
// 2xx that is not one is invalid_response.
//
// This is where the two failure modes must be told apart. The endpoint answered
// HTTP 200 and was reachable, so transport_error would be false; it did not
// speak Piton's envelope, so worker_error would be false; it answered at once,
// so timeout would be false. SPEC.md 5.3's why-clause is the reason the
// distinction is worth a test: the three reasons name three different repairs,
// and this one points at the worker's output format.
func TestNotJSONRunReachedDLQ(t *testing.T) {
	if notJSONStatus != "DLQ" {
		t.Fatalf("SPEC.md 9.6, SPEC.md 12.2: a raw worker that never answers JSON exhausts its "+
			"budget and sends the run to DLQ; the run reached %q", notJSONStatus)
	}
	harness.Bool(t, "SPEC.md 9.6: a 2xx that cannot be parsed as JSON is a failed attempt",
		"SELECT bool_and(status = 'FAILED') FROM attempts WHERE run_id = :'run';", jv())
	harness.Bool(t, "SPEC.md 9.6, SPEC.md 5.3: it is invalid_response - a reply arrived and could not be parsed as the mode requires",
		"SELECT bool_and(failure_reason = 'invalid_response') FROM attempts WHERE run_id = :'run';", jv())
	harness.Bool(t, "SPEC.md 5.3: the endpoint was reachable and answered 2xx, so it is not transport_error",
		"SELECT count(*) = 0 FROM attempts WHERE run_id = :'run' AND failure_reason = 'transport_error';", jv())
	harness.Bool(t, "SPEC.md 5.3: it never spoke Piton's envelope, so it is not worker_error",
		"SELECT count(*) = 0 FROM attempts WHERE run_id = :'run' AND failure_reason = 'worker_error';", jv())
	harness.Bool(t, "SPEC.md 6.3: nothing unparseable was stored as an output",
		"SELECT output IS NULL FROM steps WHERE run_id = :'run';", jv())
	harness.Bool(t, "SPEC.md 9.6: the body that could not be parsed is kept as diagnostic text",
		"SELECT bool_and(error_text IS NOT NULL AND error_text <> '') FROM attempts WHERE run_id = :'run';", jv())
}

// TestBothRunsDeadLetterOnTheWorkerSide asserts SPEC.md 6.5 and 12.3 for both
// runs at once: a step that used step_max_attempts without succeeding is a
// worker-side entry, whatever the failures were labelled.
func TestBothRunsDeadLetterOnTheWorkerSide(t *testing.T) {
	for _, run := range []struct {
		what string
		v    string
	}{
		{"the non-2xx run", nv()},
		{"the not-JSON run", jv()},
	} {
		harness.Bool(t, "SPEC.md 6.5: exactly one dead-letter entry for "+run.what,
			"SELECT count(*) = 1 FROM dead_letter_queue WHERE run_id = :'run';", run.v)
		harness.Equal(t, "SPEC.md 6.5, SPEC.md 12.3: the reason is worker_budget_exhausted for "+run.what,
			"worker_budget_exhausted",
			"SELECT reason FROM dead_letter_queue WHERE run_id = :'run';", run.v)
		harness.Bool(t, "SPEC.md 6.5: a worker-side entry names the step that exhausted its budget - "+run.what,
			"SELECT d.step_id = s.step_id FROM dead_letter_queue d, steps s"+
				" WHERE d.run_id = :'run' AND s.run_id = :'run';", run.v)
		harness.Bool(t, "SPEC.md 6.5: error_text is not null, and is truncated to 4 KB as in 6.4 - "+run.what,
			"SELECT error_text IS NOT NULL AND octet_length(error_text) <= 4096"+
				" FROM dead_letter_queue WHERE run_id = :'run';", run.v)
		harness.Bool(t, "SPEC.md 6.5, SPEC.md 14: nothing replayed this run, so the entry belongs to round 0 - "+run.what,
			"SELECT replay_round = 0 FROM dead_letter_queue WHERE run_id = :'run';", run.v)
	}
}

// TestTheTwoFailuresAreDistinguishable is SPEC.md 5.3's why-clause as an
// assertion. Both runs end identically - DLQ, one worker-side dead-letter entry
// - and the only thing that tells the operator which repair to make is
// failure_reason. An implementation that collapsed the two would still pass
// every DLQ assertion above.
func TestTheTwoFailuresAreDistinguishable(t *testing.T) {
	non := harness.Scalar(t, "SELECT DISTINCT failure_reason FROM attempts WHERE run_id = :'run';", nv())
	notJSON := harness.Scalar(t, "SELECT DISTINCT failure_reason FROM attempts WHERE run_id = :'run';", jv())
	if non == notJSON {
		t.Errorf("SPEC.md 5.3: transport_error and invalid_response name two different repairs - "+
			"the network and the worker's output format - and both runs were labelled %q", non)
	}
}
