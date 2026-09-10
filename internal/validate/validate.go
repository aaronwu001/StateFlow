// Package validate is SPEC.md 16 — everything refused at submission time —
// together with the StepSpec rules of SPEC.md 9.4 and 9.8 that SPEC.md 6.1
// applies to every element of planner_static_steps.
//
// SPEC.md 16's governing principle is stated once and applies to every function
// below: "better to be too strict and be told we do not support something, than
// too lax and let a user fail silently", and "anything decidable at submission
// time must not be deferred to run time".
//
// SPEC.md 18.1 puts all of this in milestone α, and SPEC.md 6.1 gives the
// reason it could not wait for ι: a malformed StepSpec discovered at run time
// leaves a run that can neither progress nor fail — it is not the planner
// refusing, and no attempt exists to burn budget — so it would sit RUNNING
// forever, reclaimed and re-failed by every sweep. Validating here makes that
// state unreachable.
package validate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/aaronwu001/piton/internal/model"
)

// Rejection is a refusal with the two fields SPEC.md 10.5 makes mandatory: a
// stable machine-readable slug and a human-readable message. The handler adds
// the identifiers and statuses of the entities the request named.
type Rejection struct {
	Slug    string
	Message string
}

func (r *Rejection) Error() string { return r.Slug + ": " + r.Message }

func reject(format string, args ...any) *Rejection {
	return &Rejection{Slug: "invalid_request", Message: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------------------
// POST /workflows — SPEC.md 16
// ---------------------------------------------------------------------------

// WorkflowRequest is the accepted shape of a workflow definition. Every field
// is a pointer so that "absent" and "present with a zero value" are different
// facts: SPEC.md 16 rule 6 refuses step_max_attempts = 0, which is not the same
// request as one that omitted the key and takes SPEC.md 11.1's default of 3.
type WorkflowRequest struct {
	Name               *string            `json:"name"`
	PlannerType        *string            `json:"planner_type"`
	PlannerURL         *string            `json:"planner_url"`
	FetchBaseURL       *string            `json:"fetch_base_url"`
	PlannerStaticSteps *[]json.RawMessage `json:"planner_static_steps"`

	StepTimeoutSeconds       *int `json:"step_timeout_seconds"`
	StepMaxAttempts          *int `json:"step_max_attempts"`
	StepRetryDelaySeconds    *int `json:"step_retry_delay_seconds"`
	PlannerTimeoutSeconds    *int `json:"planner_timeout_seconds"`
	PlannerMaxAttempts       *int `json:"planner_max_attempts"`
	PlannerRetryDelaySeconds *int `json:"planner_retry_delay_seconds"`
}

// SPEC.md 11.1's defaults, applied to a key the request omits.
const (
	DefaultStepTimeoutSeconds       = 300
	DefaultStepMaxAttempts          = 3
	DefaultStepRetryDelaySeconds    = 0
	DefaultPlannerTimeoutSeconds    = 30
	DefaultPlannerMaxAttempts       = 3
	DefaultPlannerRetryDelaySeconds = 0
)

// Workflow parses and validates a POST /workflows body and returns the
// workflow it describes, with SPEC.md 11.1's defaults filled in.
//
// The workflow_id is not assigned here: SPEC.md 3.3 makes it an identity, and
// minting one is the caller's job, not the validator's.
func Workflow(body []byte) (*model.Workflow, *Rejection) {
	var req WorkflowRequest
	// SPEC.md 16 rule 4 — any unknown key — and rule 5 — any configuration
	// field of the wrong JSON type, "'3' is not 3, and must not be coerced" —
	// are both this decoder's doing. Rule 5 needs no explicit check because
	// encoding/json refuses to coerce a string into an int; a hand-written
	// coercion is precisely what the rule forbids.
	if rej := decodeStrict(body, &req, "workflow definition"); rej != nil {
		return nil, rej
	}

	// SPEC.md 16 rule 1: planner_type not one of static / http — "this catches
	// the typo 'htp'". An absent planner_type is not one of them either.
	if req.PlannerType == nil {
		return nil, reject("planner_type is required and must be %q or %q (SPEC.md 16 rule 1)",
			model.PlannerStatic, model.PlannerHTTP)
	}
	switch *req.PlannerType {
	case model.PlannerStatic, model.PlannerHTTP:
	default:
		return nil, reject("planner_type %q is not one of %q or %q (SPEC.md 16 rule 1)",
			*req.PlannerType, model.PlannerStatic, model.PlannerHTTP)
	}

	wf := &model.Workflow{
		PlannerType:              *req.PlannerType,
		StepTimeoutSeconds:       DefaultStepTimeoutSeconds,
		StepMaxAttempts:          DefaultStepMaxAttempts,
		StepRetryDelaySeconds:    DefaultStepRetryDelaySeconds,
		PlannerTimeoutSeconds:    DefaultPlannerTimeoutSeconds,
		PlannerMaxAttempts:       DefaultPlannerMaxAttempts,
		PlannerRetryDelaySeconds: DefaultPlannerRetryDelaySeconds,
	}
	if req.Name != nil {
		wf.Name = *req.Name
	}

	// SPEC.md 6.1's invariant: "planner_url and fetch_base_url are both present
	// iff planner_type = 'http', and planner_static_steps is present iff it is
	// 'static' - the two sides are never mixed and never both absent". SPEC.md
	// 16 rules 2 and 3 are the two halves of enforcing it.
	//
	// SPEC.md 6.1 says why the two URLs travel together: "planner_url says
	// where the orchestrator calls this workflow's planner, and fetch_base_url
	// says where that same planner reads back - one relationship, two
	// directions, so both are settled at the same moment, live in the same row
	// and expire together."
	switch wf.PlannerType {
	case model.PlannerHTTP:
		if req.PlannerURL == nil {
			return nil, reject("planner_type %q requires planner_url (SPEC.md 16 rule 2)", model.PlannerHTTP)
		}
		if !absoluteHTTPURL(*req.PlannerURL) {
			return nil, reject("planner_url %q is not a valid absolute HTTP(S) URL (SPEC.md 16 rule 2)",
				*req.PlannerURL)
		}
		if req.FetchBaseURL == nil {
			return nil, reject("planner_type %q requires fetch_base_url (SPEC.md 16 rule 2)", model.PlannerHTTP)
		}
		if !absoluteHTTPURL(*req.FetchBaseURL) {
			return nil, reject("fetch_base_url %q is not a valid absolute HTTP(S) URL (SPEC.md 16 rule 2)",
				*req.FetchBaseURL)
		}
		if req.PlannerStaticSteps != nil {
			return nil, reject("planner_type %q must not carry planner_static_steps (SPEC.md 6.1)",
				model.PlannerHTTP)
		}
		wf.PlannerURL = *req.PlannerURL
		wf.FetchBaseURL = *req.FetchBaseURL

	case model.PlannerStatic:
		if req.PlannerStaticSteps == nil {
			return nil, reject("planner_type %q requires planner_static_steps (SPEC.md 16 rule 3)",
				model.PlannerStatic)
		}
		if len(*req.PlannerStaticSteps) == 0 {
			return nil, reject("planner_static_steps must not be empty (SPEC.md 16 rule 3)")
		}
		if req.PlannerURL != nil {
			return nil, reject("planner_type %q must not carry planner_url (SPEC.md 6.1)", model.PlannerStatic)
		}
		// SPEC.md 16 rule 3: a static workflow carrying fetch_base_url, "which
		// belongs to http alone".
		if req.FetchBaseURL != nil {
			return nil, reject("planner_type %q must not carry fetch_base_url (SPEC.md 16 rule 3)",
				model.PlannerStatic)
		}
		// SPEC.md 6.1: "Every element of planner_static_steps is a StepSpec,
		// and is validated as one — by 9.4 and 9.8 — at POST /workflows,
		// before any run exists."
		for i, raw := range *req.PlannerStaticSteps {
			if _, rej := StepSpec(raw); rej != nil {
				return nil, reject("planner_static_steps[%d] is not a valid StepSpec: %s", i, rej.Message)
			}
		}
		// Stored as the bytes that arrived, so that steps.decision can later
		// hold "the StepSpec exactly as the planner returned it" (SPEC.md 6.3).
		encoded, err := json.Marshal(*req.PlannerStaticSteps)
		if err != nil {
			return nil, reject("planner_static_steps could not be stored: %v", err)
		}
		wf.PlannerStaticSteps = encoded
	}

	// SPEC.md 16 rule 6 and SPEC.md 11.1: any *_max_attempts below 1, any
	// *_timeout_seconds below 1, or a negative *_retry_delay_seconds. The two
	// families are symmetric (SPEC.md 11.1), and so is this table.
	fields := []struct {
		name  string
		value *int
		min   int
		into  *int
	}{
		{"step_timeout_seconds", req.StepTimeoutSeconds, 1, &wf.StepTimeoutSeconds},
		{"step_max_attempts", req.StepMaxAttempts, 1, &wf.StepMaxAttempts},
		{"step_retry_delay_seconds", req.StepRetryDelaySeconds, 0, &wf.StepRetryDelaySeconds},
		{"planner_timeout_seconds", req.PlannerTimeoutSeconds, 1, &wf.PlannerTimeoutSeconds},
		{"planner_max_attempts", req.PlannerMaxAttempts, 1, &wf.PlannerMaxAttempts},
		{"planner_retry_delay_seconds", req.PlannerRetryDelaySeconds, 0, &wf.PlannerRetryDelaySeconds},
	}
	for _, f := range fields {
		if f.value == nil {
			continue
		}
		if *f.value < f.min {
			return nil, reject("%s must be >= %d, got %d (SPEC.md 16 rule 6, SPEC.md 11.1)",
				f.name, f.min, *f.value)
		}
		*f.into = *f.value
	}

	return wf, nil
}

// ---------------------------------------------------------------------------
// StepSpec — SPEC.md 9.4 and 9.8
// ---------------------------------------------------------------------------

// Spec is a parsed StepSpec. It is what the dispatcher reads; the bytes it was
// parsed from are what the database stores (SPEC.md 6.3).
type Spec struct {
	StepName       *string
	WorkerURL      string
	ConnectionMode string
	DispatchStyle  string

	// Params is the planner's literal values, already defaulted to {} when the
	// StepSpec omitted the key (SPEC.md 9.4).
	Params json.RawMessage

	// InputFrom carries SPEC.md 9.4's three-way distinction, which a plain
	// slice cannot: omitted means "the previous step only", [] means "nothing",
	// and a populated array means those steps.
	InputFrom *[]string
}

type specRequest struct {
	StepName       *string          `json:"step_name"`
	WorkerURL      *string          `json:"worker_url"`
	ConnectionMode *string          `json:"connection_mode"`
	DispatchStyle  *string          `json:"dispatch_style"`
	Params         *json.RawMessage `json:"params"`
	InputFrom      *[]string        `json:"input_from"`
	TimeoutSeconds *int             `json:"timeout_seconds"`
	MaxAttempts    *int             `json:"max_attempts"`
}

// StepSpec validates one StepSpec against SPEC.md 9.4 and 9.8.
//
// SPEC.md 9.8's closing rule governs every caller of this function that is a
// planner rather than an operator: "an invalid StepSpec is a planner failure
// and consumes planner budget exactly like an unreachable planner. It never
// creates a step."
func StepSpec(raw []byte) (*Spec, *Rejection) {
	var req specRequest
	// SPEC.md 9.8 rule 6: an unknown top-level key. "The most common cause is a
	// typo — worker_ur1 silently dropped becomes the error 'worker_url is
	// missing', which points the author at the wrong line."
	if rej := decodeStrict(raw, &req, "StepSpec"); rej != nil {
		return nil, rej
	}

	// SPEC.md 9.8 rule 1.
	if req.WorkerURL == nil {
		return nil, reject("worker_url is missing (SPEC.md 9.8 rule 1)")
	}
	if !absoluteHTTPURL(*req.WorkerURL) {
		return nil, reject("worker_url %q is not a valid absolute HTTP(S) URL (SPEC.md 9.8 rule 1)",
			*req.WorkerURL)
	}

	// SPEC.md 9.8 rule 2. SPEC.md 9.4 says why neither is defaulted: "every
	// message carries it explicitly so that sync assumptions never bake in
	// silently and a later async format break becomes impossible".
	if req.ConnectionMode == nil {
		return nil, reject("connection_mode is missing (SPEC.md 9.8 rule 2)")
	}
	switch *req.ConnectionMode {
	case model.ConnectionSync, model.ConnectionAsync:
	default:
		return nil, reject("connection_mode %q is not %q or %q (SPEC.md 9.8 rule 2)",
			*req.ConnectionMode, model.ConnectionSync, model.ConnectionAsync)
	}
	if req.DispatchStyle == nil {
		return nil, reject("dispatch_style is missing (SPEC.md 9.8 rule 2)")
	}
	switch *req.DispatchStyle {
	case model.DispatchEnvelope, model.DispatchRaw:
	default:
		return nil, reject("dispatch_style %q is not %q or %q (SPEC.md 9.8 rule 2)",
			*req.DispatchStyle, model.DispatchEnvelope, model.DispatchRaw)
	}

	// SPEC.md 9.8 rule 3 / SPEC.md 9.7: a raw body carries only params, so
	// there is nowhere to put callback_url.
	if *req.ConnectionMode == model.ConnectionAsync && *req.DispatchStyle == model.DispatchRaw {
		return nil, reject("connection_mode %q with dispatch_style %q is never legal (SPEC.md 9.8 rule 3, SPEC.md 9.7)",
			model.ConnectionAsync, model.DispatchRaw)
	}

	// SPEC.md 9.8 rule 4: input_from present at the StepSpec level together
	// with raw. Presence is what the rule tests, so [] is caught as surely as a
	// populated array. A key named input_from *inside* params is unaffected
	// (SPEC.md 9.5) and never reaches this check.
	if req.InputFrom != nil && *req.DispatchStyle == model.DispatchRaw {
		return nil, reject("input_from has no meaning with dispatch_style %q (SPEC.md 9.8 rule 4)",
			model.DispatchRaw)
	}

	// SPEC.md 9.8 rule 5: non-null before milestone η. An explicit null is
	// legal — SPEC.md 9.4 gives both fields the default null, and the rule
	// refuses only a non-null value.
	if req.TimeoutSeconds != nil {
		return nil, reject("timeout_seconds must be null until milestone eta (SPEC.md 9.8 rule 5, SPEC.md 11.2)")
	}
	if req.MaxAttempts != nil {
		return nil, reject("max_attempts must be null until milestone eta (SPEC.md 9.8 rule 5, SPEC.md 11.2)")
	}

	// SPEC.md 9.4 types params as an object. A StepSpec whose params were an
	// array or a string could not produce SPEC.md 9.5's envelope, whose params
	// field is an object.
	params := json.RawMessage("{}")
	if req.Params != nil && !isJSONNull(*req.Params) {
		if !isJSONObject(*req.Params) {
			return nil, reject("params must be a JSON object (SPEC.md 9.4)")
		}
		params = *req.Params
	}

	return &Spec{
		StepName:       req.StepName,
		WorkerURL:      *req.WorkerURL,
		ConnectionMode: *req.ConnectionMode,
		DispatchStyle:  *req.DispatchStyle,
		Params:         params,
		InputFrom:      req.InputFrom,
	}, nil
}

// ---------------------------------------------------------------------------
// POST /workflows/{id}/runs — SPEC.md 16, SPEC.md 10.1
// ---------------------------------------------------------------------------

type runRequest struct {
	Input     json.RawMessage  `json:"input"`
	Overrides *json.RawMessage `json:"overrides"`
}

// RunInput validates a run-creation body and returns the input to store
// verbatim (SPEC.md 6.2).
//
// SPEC.md 16: a non-empty overrides is a 400 (SPEC.md 11.2), a missing input is
// a 400, and an unknown key is a 400. "overrides may be {}, null, or omitted;
// all three mean 'no overrides'" — and SPEC.md 16 says why null and omission
// are accepted: SPEC.md 9.4 gives the same feature's step-level fields the
// default null, and SPEC.md 9.8 rule 5 rejects only a non-null value, so the
// two halves of one feature must not disagree about the same JSON value.
func RunInput(body []byte) (json.RawMessage, *Rejection) {
	var req runRequest
	if rej := decodeStrict(body, &req, "run-creation body"); rej != nil {
		return nil, rej
	}

	if len(req.Input) == 0 || isJSONNull(req.Input) {
		return nil, reject("input is required (SPEC.md 16)")
	}
	// SPEC.md 9.2 types workflow_input — which is runs.input verbatim — as an
	// object.
	if !isJSONObject(req.Input) {
		return nil, reject("input must be a JSON object (SPEC.md 9.2, SPEC.md 6.2)")
	}

	if req.Overrides != nil && !isJSONNull(*req.Overrides) {
		if !isJSONObject(*req.Overrides) {
			return nil, reject("overrides must be a JSON object (SPEC.md 10.1)")
		}
		if !isEmptyJSONObject(*req.Overrides) {
			return nil, reject("overrides must be empty until milestone eta (SPEC.md 16, SPEC.md 11.2)")
		}
	}

	return req.Input, nil
}

// ---------------------------------------------------------------------------
// Shared parsing helpers
// ---------------------------------------------------------------------------

// decodeStrict is where SPEC.md 16 rules 4 and 5 and SPEC.md 9.8 rule 6
// actually live. DisallowUnknownFields is rule 4 / rule 6; encoding/json's
// refusal to coerce "3" into an int is rule 5.
//
// The trailing-token check exists because a decoder that stopped at the first
// complete value would accept `{"a":1} garbage` — a body that is not one JSON
// document, and therefore malformed under SPEC.md 10.5's 400.
func decodeStrict(body []byte, into any, what string) *Rejection {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return reject("%s is not acceptable JSON: %v", what, err)
	}
	if dec.More() {
		return reject("%s carries trailing content after the JSON document", what)
	}
	return nil
}

// absoluteHTTPURL is SPEC.md 16 rule 2's and SPEC.md 9.8 rule 1's shared test:
// "a valid absolute HTTP(S) URL". "worker:9090/echo" fails it — it parses as an
// opaque URL with scheme "worker" and no host, which is exactly the mistake the
// rule is there to catch.
func absoluteHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}

func firstToken(raw []byte) byte {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return b
		}
	}
	return 0
}

func isJSONObject(raw []byte) bool { return firstToken(raw) == '{' }

func isJSONNull(raw []byte) bool { return firstToken(raw) == 'n' }

// isEmptyJSONObject decides SPEC.md 16's "non-empty overrides" by counting keys
// rather than by comparing bytes, so that {} and {  } are the same answer.
func isEmptyJSONObject(raw []byte) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	return len(m) == 0
}
