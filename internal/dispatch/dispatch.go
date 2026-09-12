// Package dispatch is the orchestrator → worker boundary of SPEC.md 9.5 and
// the worker → orchestrator boundary of SPEC.md 9.6.
//
// It writes nothing. Everything it produces is an Outcome handed back to the
// driver, which records it under the fence and the attempt CAS of SPEC.md 8 —
// SPEC.md 8.4's distinction, in the shape of a package boundary: a report is a
// fact about work that already happened, and only the decision about what the
// run does next belongs to the owner.
package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aaronwu001/piton/internal/model"
	"github.com/aaronwu001/piton/internal/validate"
)

// Identity is what SPEC.md 9.5's envelope carries about the work being asked
// for.
type Identity struct {
	RunID     string
	StepID    string
	AttemptID string
}

// Outcome is what came back, in the vocabulary of SPEC.md 5.3.
type Outcome struct {
	Success bool

	// Output is the step's result on success. SPEC.md 9.6 fixes what that is
	// per mode: for sync + envelope it is the response's `output` field, and
	// for sync + raw it is "the entire response body verbatim".
	Output []byte

	// Reason is one of model.Failure*, or empty to mean "decide between
	// timeout and transport_error by the clock". SPEC.md 5.3 requires that
	// choice to be made against deadline_at rather than against the error's
	// shape, so it is left to the write, where the row's own deadline is
	// visible.
	Reason string

	ErrorText string
}

func failure(reason, format string, args ...any) Outcome {
	return Outcome{Reason: reason, ErrorText: fmt.Sprintf(format, args...)}
}

// envelope is SPEC.md 9.5's body, field for field.
//
// CallbackURL carries `omitempty` because SPEC.md 9.5 requires it to be
// "present iff connection_mode = 'async'" and "omitted entirely" in sync mode —
// and says why not an empty string: "an empty string is a URL-shaped slot that
// invites a worker to use it, which would force this document to define what
// happens when it does".
type envelope struct {
	RunID          string                     `json:"run_id"`
	StepID         string                     `json:"step_id"`
	AttemptID      string                     `json:"attempt_id"`
	ConnectionMode string                     `json:"connection_mode"`
	Params         json.RawMessage            `json:"params"`
	Inputs         map[string]json.RawMessage `json:"inputs"`
	CallbackURL    string                     `json:"callback_url,omitempty"`
}

// workerReply is SPEC.md 9.6's envelope-mode response, in both directions:
// {"status":"success","output":{ }} and {"status":"failure","error":"…"}.
type workerReply struct {
	Status string          `json:"status"`
	Output json.RawMessage `json:"output"`
	Error  string          `json:"error"`
}

// Do performs one attempt and reports its outcome.
//
// inputs is the map SPEC.md 9.5 calls `inputs` — "a map from step_id to that
// step's stored output, assembled by the orchestrator from input_from". The
// envelope has no input_from field of its own, because "the resolution has
// already happened" before this function is called.
func Do(ctx context.Context, client *http.Client, spec *validate.Spec, id Identity,
	inputs map[string][]byte, timeout time.Duration) Outcome {

	// SPEC.md 9.7's table of legal mode combinations names the milestone each
	// one arrives in. α implemented sync + envelope and θ adds sync + raw; the
	// remaining legal combination is async + envelope, which is ε, and SPEC.md
	// 19.3 keeps it designed in and unbuilt. async + raw never becomes legal,
	// and internal/validate rejects it at submission (SPEC.md 9.8 rule 3), so
	// it cannot arrive here.
	//
	// An unimplemented combination is reported as one failed attempt rather
	// than silently skipped, so that the run converges to DLQ under SPEC.md
	// 12.2's budget instead of sitting RUNNING forever, and so that the reason
	// is legible in the database (SPEC.md 17.3) rather than only in a log.
	if spec.ConnectionMode != model.ConnectionSync {
		return failure(model.FailureTransportError,
			"piton: connection_mode %q is milestone epsilon and is not implemented in this build "+
				"(SPEC.md 9.7, SPEC.md 19.3); nothing was sent to %s",
			spec.ConnectionMode, spec.WorkerURL)
	}

	// What goes on the wire, per SPEC.md 9.5.
	//
	// In raw mode the body is "params, verbatim and nothing else" — no run_id,
	// no step_id, no attempt_id, no connection_mode, no inputs. SPEC.md 9.5
	// says why: raw exists so that an unmodifiable endpoint is a valid worker,
	// and "you cannot add fields to such an endpoint's request body".
	//
	// `inputs` is therefore not assembled into anything here. A raw StepSpec
	// cannot carry input_from at all (SPEC.md 9.8 rule 4), so nothing the
	// planner asked for is being dropped.
	var body []byte
	if spec.DispatchStyle == model.DispatchRaw {
		// spec.Params is already defaulted to {} when the StepSpec omitted the
		// key (SPEC.md 9.4), so this is never empty and never needs shaping.
		// Shaping it is precisely what raw forbids: a key inside params named
		// input_from is ordinary data and becomes a top-level key of this body
		// (SPEC.md 9.5), which is true here by construction rather than by a
		// rule this function has to remember.
		body = spec.Params
	} else {
		var err error
		body, err = json.Marshal(envelope{
			RunID:          id.RunID,
			StepID:         id.StepID,
			AttemptID:      id.AttemptID,
			ConnectionMode: spec.ConnectionMode,
			Params:         spec.Params,
			Inputs:         rawInputs(inputs),
		})
		if err != nil {
			return failure(model.FailureTransportError, "piton: cannot build the dispatch envelope: %v", err)
		}
	}

	// SPEC.md 13.3: the in-process timer is "a latency optimisation that makes
	// the common case fast and is never load-bearing" — attempts.deadline_at
	// is authoritative. This deadline exists so that a hung worker does not
	// hold the driver past its budget; the row is what actually decides.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.WorkerURL, bytes.NewReader(body))
	if err != nil {
		return failure(model.FailureTransportError, "piton: cannot build the dispatch request: %v", err)
	}
	req.Header.Set("content-type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// Reason is left empty: SPEC.md 5.3 decides between timeout and
		// transport_error by whether deadline_at has passed, not by the shape
		// of this error. "A connection refused at second 3 of a 300-second
		// budget is transport_error."
		return Outcome{ErrorText: fmt.Sprintf("piton: dispatch to %s failed: %v", spec.WorkerURL, err)}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit))
	if err != nil {
		return Outcome{ErrorText: fmt.Sprintf("piton: cannot read the worker's response: %v", err)}
	}

	// SPEC.md 9.6: "a transport-level failure is always a failure regardless of
	// body — non-2xx, connection refused, timeout."
	//
	// One rule, both dispatch styles. In raw mode SPEC.md 9.6 adds what becomes
	// of the body — "the truncated body is stored as error text" — and that is
	// what this ErrorText is; the 4 KB cut is applied at the write (SPEC.md
	// 6.4: "the orchestrator truncates before writing").
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Outcome{ErrorText: fmt.Sprintf("piton: worker %s answered HTTP %d: %s",
			spec.WorkerURL, resp.StatusCode, string(raw))}
	}

	// SPEC.md 9.6, raw mode: "any 2xx" succeeds and "the entire response body
	// verbatim is the output". The reply is NOT inspected — a raw worker was
	// never told about Piton, so a body that happens to carry the words
	// `status` and `error` is a stranger's document and not this system's
	// protocol. Reading it as an envelope would put Piton's meaning on a value
	// the worker never intended.
	//
	// The one thing required of it is that it be a JSON document. SPEC.md 9.6:
	// a 2xx whose body cannot be parsed as JSON is invalid_response, because
	// SPEC.md 6.3 and 6.4 store an output as JSON bytes and the alternatives —
	// wrapping the body in a JSON string, or widening the column — were ruled
	// against. An empty body is caught by the same check: it is not a document.
	if spec.DispatchStyle == model.DispatchRaw {
		if !json.Valid(raw) {
			return failure(model.FailureInvalidResponse,
				"piton: worker %s answered HTTP %d with a body that is not a JSON document "+
					"(SPEC.md 9.6 requires one in raw mode): %s",
				spec.WorkerURL, resp.StatusCode, string(raw))
		}
		return Outcome{Success: true, Output: raw}
	}

	var reply workerReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		// SPEC.md 5.3: invalid_response is "a reply arrived, but could not be
		// parsed as the mode requires", and it is its own value because the
		// three reasons "name three different repairs — the worker's business
		// logic, the worker's output format, the network".
		return failure(model.FailureInvalidResponse,
			"piton: worker %s replied with unparseable JSON: %v: %s", spec.WorkerURL, err, string(raw))
	}

	switch reply.Status {
	case "success":
		out := reply.Output
		if len(out) == 0 {
			// SPEC.md 6.3 permits a worker to return the JSON document null as
			// its result, and forbids reading anything into the presence or
			// absence of an output: "a step's completion is signalled by
			// status = 'DONE' and by nothing else".
			out = json.RawMessage("null")
		}
		return Outcome{Success: true, Output: out}
	case "failure":
		// SPEC.md 5.3: worker_error is "the worker replied, in Piton's
		// envelope, that the work failed". SPEC.md 9.6: a business-level
		// failure and a transport-level failure burn one attempt alike.
		return failure(model.FailureWorkerError, "piton: worker %s reported failure: %s",
			spec.WorkerURL, reply.Error)
	default:
		return failure(model.FailureInvalidResponse,
			"piton: worker %s replied with status %q, which is neither %q nor %q (SPEC.md 9.6)",
			spec.WorkerURL, reply.Status, "success", "failure")
	}
}

// responseLimit bounds how much of a worker's reply is read into memory. It is
// far above any legitimate output this milestone produces and exists only so
// that a misbehaving worker cannot exhaust the orchestrator's memory; SPEC.md
// puts no limit on an output's size, so this is a guard rail and not a rule.
const responseLimit = 32 << 20

// rawInputs converts the assembled outputs into the envelope's `inputs` field.
//
// The map is always allocated, never left nil, so that it serialises as {} and
// the field is present. SPEC.md 9.5 lists inputs among the fields a sync
// envelope carries with no exception for the empty case, and a worker that
// checked for its presence would otherwise see it disappear whenever
// input_from resolved to nothing.
func rawInputs(inputs map[string][]byte) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(inputs))
	for id, b := range inputs {
		if len(b) == 0 {
			b = []byte("null")
		}
		out[id] = json.RawMessage(b)
	}
	return out
}
