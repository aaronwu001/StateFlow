package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aaronwu001/piton/internal/model"
)

// This file is the HTTP planner of SPEC.md 6.1 — "planner_type: http" — and the
// wire protocol of SPEC.md 9.2 and 9.3. It performs exactly one exchange and
// classifies its outcome; it decides nothing about budgets, writes nothing, and
// retries nothing. SPEC.md 4.2 and 12.2 own those, and keeping them out of here
// is what lets the same classification serve a first call and a last one.
//
// SPEC.md 9.2: "planner calls are always synchronous. The planner is a pure
// function." So there is no callback address, no attempt row and no deadline in
// the database — the only clock is the one this call is made under.

// Request is SPEC.md 9.2's M1 body, field for field.
type Request struct {
	RunID         string          `json:"run_id"`
	WorkflowInput json.RawMessage `json:"workflow_input"`
	History       []HistoryRow    `json:"history"`
	FetchBaseURL  string          `json:"fetch_base_url"`
}

// HistoryRow is one entry of SPEC.md 9.2's catalogue: "step_id, step_name, seq,
// status, output_bytes, attempt_count, completed_at".
//
// It carries output_bytes and never the output. SPEC.md 9.2 is explicit about
// why, and about what the planner is to do instead: "history is a catalogue
// only. It never carries outputs — that is what output_bytes is for. A planner
// that wants an output fetches it from the read API."
type HistoryRow struct {
	StepID       string  `json:"step_id"`
	StepName     *string `json:"step_name"`
	Seq          int     `json:"seq"`
	Status       string  `json:"status"`
	OutputBytes  int     `json:"output_bytes"`
	AttemptCount int     `json:"attempt_count"`
	CompletedAt  *string `json:"completed_at"`
}

// Failure is one planner call that produced no usable answer. Reason is one of
// SPEC.md 5.8's three values, which are the same three names SPEC.md 5.3 gives
// an attempt.
type Failure struct {
	Reason string
	Text   string

	// interrupted is unexported so that only this package can set it: it is a
	// statement about how the call ended inside this process, not a property
	// of the planner, and nothing outside may claim it.
	interrupted bool
}

// Interrupted reports the failure of a call that was cut short by the caller's
// own context rather than by the planner — a shutdown, in practice.
//
// It is distinguished because SPEC.md 12.2's budget is a statement about the
// PLANNER's behaviour: burning a unit because this process was asked to stop
// would move a run towards DLQ for something the planner did not do, and
// SPEC.md 13.2 item 4 already says what happens instead — the run is reclaimed
// and the planner is asked again.
func (f *Failure) Interrupted() bool { return f != nil && f.interrupted }

// HTTPTimeoutSlack is how long the transport is given beyond the planner
// deadline before it is torn down. The deadline itself is enforced by the
// context, so this only prevents a socket outliving the call it belonged to.
const HTTPTimeoutSlack = 5 * time.Second

// HTTP performs one call to an HTTP planner and returns either a Decision or a
// Failure — never both, and never neither.
//
// The classification is SPEC.md 12.1's list, mapped onto SPEC.md 5.8's three
// reasons:
//
//	transport_error   the planner could not be reached, or answered non-2xx
//	timeout           the deadline passed before any answer arrived
//	invalid_response  an answer arrived that SPEC.md 9.3 or 9.8 rejects
//
// SPEC.md 5.3's clock rule is what separates the first two, and it is applied
// here rather than left to the shape of the error: an exchange is `timeout`
// only when the deadline actually passed. A connection refused in the first
// second of a thirty-second budget is `transport_error`, and reporting it as a
// timeout would tell the operator to raise a limit when the address is wrong.
func HTTP(ctx context.Context, client *http.Client, url string, req Request,
	timeout time.Duration) (*Decision, *Failure) {

	body, err := json.Marshal(req)
	if err != nil {
		// Nothing in Request can fail to marshal, so this is a defect rather
		// than a planner's doing. It is still reported as a failed call: the
		// run must converge (SPEC.md 12.2) rather than stall on it.
		return nil, &Failure{Reason: model.FailureTransportError,
			Text: fmt.Sprintf("piton: cannot encode the planner request: %v", err)}
	}

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, &Failure{Reason: model.FailureTransportError,
			Text: fmt.Sprintf("piton: cannot build the planner request: %v", err)}
	}
	httpReq.Header.Set("content-type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		// The parent context being done means this process is stopping, not
		// that the planner failed. See Failure.Interrupted.
		if ctx.Err() != nil {
			return nil, &Failure{Reason: model.FailureTransportError, interrupted: true,
				Text: fmt.Sprintf("piton: the planner call was interrupted: %v", err)}
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return nil, &Failure{Reason: model.FailureTimeout,
				Text: fmt.Sprintf("piton: the planner did not answer within %s", timeout)}
		}
		return nil, &Failure{Reason: model.FailureTransportError,
			Text: fmt.Sprintf("piton: the planner could not be called: %v", err)}
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPlannerBody))
	if err != nil {
		if ctx.Err() != nil {
			return nil, &Failure{Reason: model.FailureTransportError, interrupted: true,
				Text: fmt.Sprintf("piton: the planner call was interrupted while reading: %v", err)}
		}
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return nil, &Failure{Reason: model.FailureTimeout,
				Text: fmt.Sprintf("piton: the planner did not finish answering within %s", timeout)}
		}
		return nil, &Failure{Reason: model.FailureTransportError,
			Text: fmt.Sprintf("piton: cannot read the planner's reply: %v", err)}
	}

	// SPEC.md 12.1 counts "a non-2xx response" among the ways a planner call
	// fails, and SPEC.md 5.8 puts it under transport_error: the exchange did
	// not produce a usable reply, whatever the body says.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &Failure{Reason: model.FailureTransportError,
			Text: fmt.Sprintf("piton: the planner answered HTTP %d: %s",
				resp.StatusCode, snippet(raw))}
	}

	return parse(raw)
}

// maxPlannerBody bounds what one reply may be read into. A planner is the
// user's own service and a reply is one of three small documents (SPEC.md 9.3);
// this exists so that a misconfigured URL pointing at something enormous fails
// as an invalid response instead of consuming memory.
const maxPlannerBody = 1 << 20

// parse turns one reply into SPEC.md 9.3's decision, or into the invalid
// response of SPEC.md 5.8.
//
// SPEC.md 9.3: "exactly three responses ... anything else — an unparseable
// body, an unknown status, continue without step — is a planner failure."
//
// Unknown TOP-LEVEL keys are accepted. SPEC.md 9.8's rule 6 rejects an unknown
// key inside a StepSpec and gives its reason there; SPEC.md 9.3's list of what
// is rejected does not include one in the envelope, and this file does not add
// rules the document does not state.
func parse(raw []byte) (*Decision, *Failure) {
	var m2 struct {
		Status string          `json:"status"`
		Step   json.RawMessage `json:"step"`
		Reason string          `json:"reason"`
	}
	if err := json.Unmarshal(raw, &m2); err != nil {
		return nil, &Failure{Reason: model.FailureInvalidResponse,
			Text: fmt.Sprintf("piton: the planner's reply is not JSON the protocol accepts: %v: %s",
				err, snippet(raw))}
	}

	switch m2.Status {
	case StatusContinue:
		// SPEC.md 9.3: "StepSpec is one field of the response, not the
		// response itself", so `continue` without one is not a step at all.
		// The StepSpec's own validity is SPEC.md 9.8's question and is asked
		// by the caller, which is where the same check already stands for the
		// static planner.
		if len(m2.Step) == 0 || string(m2.Step) == "null" {
			return nil, &Failure{Reason: model.FailureInvalidResponse,
				Text: "piton: the planner answered continue and carried no step (SPEC.md 9.3)"}
		}
		return &Decision{Status: StatusContinue, Step: m2.Step}, nil

	case StatusDone:
		return &Decision{Status: StatusDone}, nil

	case StatusFail:
		// SPEC.md 9.3 shows `reason` on a fail answer and SPEC.md 6.8's third
		// invariant requires the recorded call to say why. An empty reason is
		// not made a rejection here — SPEC.md 9.3 does not list one — so the
		// absence is reported as what it is.
		reason := m2.Reason
		if reason == "" {
			reason = "the planner answered fail and gave no reason"
		}
		return &Decision{Status: StatusFail, Reason: reason}, nil

	default:
		return nil, &Failure{Reason: model.FailureInvalidResponse,
			Text: fmt.Sprintf("piton: the planner answered status %q, which is not one of "+
				"continue, done or fail (SPEC.md 9.3)", m2.Status)}
	}
}

// snippet keeps a diagnostic short enough to read. SPEC.md 6.4 caps what is
// stored at 4 KB and says why - "past that, a wall of text stops being
// diagnosis" - and this is the same judgement applied one step earlier.
func snippet(raw []byte) string {
	const max = 512
	s := string(bytes.TrimSpace(raw))
	if len(s) > max {
		return s[:max] + "…"
	}
	if s == "" {
		return "<empty body>"
	}
	return s
}
