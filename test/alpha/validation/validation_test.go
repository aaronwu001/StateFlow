package validation

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aaronwu001/piton/test/alpha/harness"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// workflowBody returns demos/alpha/workflow.json with one mutation applied.
//
// Every rejected body in this file differs from an accepted one in exactly the
// one way the rule under test names. A hand-written invalid document could be
// invalid for three reasons at once and still produce the 400 the test wanted,
// which would prove nothing about the rule it claims to test.
func workflowBody(t *testing.T, mutate func(wf map[string]any)) []byte {
	t.Helper()
	raw, err := harness.WorkflowJSON()
	if err != nil {
		t.Fatalf("cannot read the demo's workflow.json: %v", err)
	}
	var wf map[string]any
	if err := json.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("the demo's workflow.json is not a JSON object: %v", err)
	}
	mutate(wf)
	out, err := json.Marshal(wf)
	if err != nil {
		t.Fatalf("cannot re-encode the mutated workflow: %v", err)
	}
	return out
}

// stepBody applies its mutation to planner_static_steps[0] instead of to the
// workflow, for the rules of SPEC.md 9.8, which are about a StepSpec.
func stepBody(t *testing.T, mutate func(step map[string]any)) []byte {
	t.Helper()
	return workflowBody(t, func(wf map[string]any) {
		steps, ok := wf["planner_static_steps"].([]any)
		if !ok || len(steps) == 0 {
			t.Fatalf("the demo's workflow.json declares no planner_static_steps to mutate")
		}
		step, ok := steps[0].(map[string]any)
		if !ok {
			t.Fatalf("planner_static_steps[0] is not a JSON object")
		}
		mutate(step)
	})
}

// assertRejection checks a refusal against SPEC.md 10.5: the status code the
// rule calls for, and a body that names the reason.
//
// Only "error" and "message" are asserted. SPEC.md 10.5 also requires a
// rejection to carry "the identifier and current status of every entity the
// request named or would have touched" - but it says in the same breath that a
// POST /workflows rejection has no run to describe, and `workflows` has no
// status column at all (SPEC.md 6.1), so what a workflow-level rejection must
// carry beyond these two fields is not something SPEC.md settles. Asserting a
// guess would be inventing a rule (CLAUDE.md 9).
func assertRejection(t *testing.T, why string, wantCode, gotCode int, body []byte) {
	t.Helper()
	if gotCode != wantCode {
		t.Errorf("%s\n  expected HTTP %d, got %d\n  body: %s", why, wantCode, gotCode, body)
		return
	}
	var out struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Errorf("%s\n  SPEC.md 10.5: the rejection body is not a JSON object: %v\n  body: %s", why, err, body)
		return
	}
	if out.Error == "" {
		t.Errorf("%s\n  SPEC.md 10.5: the rejection carries no `error` slug\n  body: %s", why, body)
	}
	if out.Message == "" {
		t.Errorf("%s\n  SPEC.md 10.5: the rejection carries no `message`\n  body: %s", why, body)
	}
}

// rejectWorkflow posts a workflow definition that SPEC.md 16 requires to be a
// 400 "before any run exists".
func rejectWorkflow(t *testing.T, why string, body []byte) {
	t.Helper()
	code, resp, err := harness.Post("/workflows", body)
	if err != nil {
		t.Fatalf("%s\n  POST /workflows failed: %v", why, err)
	}
	assertRejection(t, why, http.StatusBadRequest, code, resp)
}

// rejectRun posts a run-creation body that SPEC.md 16 requires to be a 400.
func rejectRun(t *testing.T, why, body string) {
	t.Helper()
	code, resp, err := harness.Post("/workflows/"+validWorkflowID+"/runs", []byte(body))
	if err != nil {
		t.Fatalf("%s\n  POST /workflows/{id}/runs failed: %v", why, err)
	}
	assertRejection(t, why, http.StatusBadRequest, code, resp)
}

// acceptRun posts a run-creation body that must be accepted, and returns
// nothing but a verdict: SPEC.md 10.1 fixes the request shape, not the success
// code, so both 200 and 201 are honoured.
func acceptRun(t *testing.T, why, body string) {
	t.Helper()
	code, resp, err := harness.Post("/workflows/"+validWorkflowID+"/runs", []byte(body))
	if err != nil {
		t.Fatalf("%s\n  POST /workflows/{id}/runs failed: %v", why, err)
	}
	if code != http.StatusOK && code != http.StatusCreated {
		t.Errorf("%s\n  expected the run to be accepted, got HTTP %d\n  body: %s", why, code, resp)
		return
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(resp, &out); err != nil || out.RunID == "" {
		t.Errorf("%s\n  the acceptance carried no run_id\n  body: %s", why, resp)
	}
}

// ---------------------------------------------------------------------------
// SPEC.md 16, POST /workflows - the workflow definition itself
// ---------------------------------------------------------------------------

// The demo's own workflow.json must be accepted. Without this, every assertion
// below would pass just as happily against an orchestrator that rejected
// everything. TestMain creates it; this test states the fact as an assertion so
// that a failure is reported as a failure rather than as a setup error.
func TestTheDemoWorkflowIsAccepted(t *testing.T) {
	if validWorkflowID == "" {
		t.Fatal("SPEC.md 18.1: the demo's own workflow.json was not accepted")
	}
}

// SPEC.md 16 rule 1: planner_type not one of static / http - "this catches the
// typo 'htp'".
func TestRejectsUnknownPlannerType(t *testing.T) {
	rejectWorkflow(t, "SPEC.md 16 rule 1: planner_type 'htp' is not an enumerated value",
		workflowBody(t, func(wf map[string]any) { wf["planner_type"] = "htp" }))
}

// SPEC.md 16 rule 2: planner_type = "http" with no planner_url, or a
// planner_url that is not a valid absolute HTTP(S) URL.
//
// Each body below must be wrong in exactly ONE respect, or the rejection it
// produces says nothing about the rule it names.
//
// planner_static_steps is removed and fetch_base_url is supplied: SPEC.md 6.1's
// invariant is that planner_url and fetch_base_url are both present iff
// planner_type is 'http', and planner_static_steps iff it is 'static'. Leaving
// the array in, or omitting fetch_base_url, would make the body invalid for a
// second reason - and a test that passes because of a defect it did not intend
// would keep passing after the defect it did intend was fixed.
func TestRejectsHTTPPlannerWithoutUsableURL(t *testing.T) {
	rejectWorkflow(t, "SPEC.md 16 rule 2: planner_type 'http' with no planner_url",
		workflowBody(t, func(wf map[string]any) {
			wf["planner_type"] = "http"
			delete(wf, "planner_static_steps")
			wf["fetch_base_url"] = "http://orchestrator:8080"
		}))
	rejectWorkflow(t, "SPEC.md 16 rule 2: planner_url is not a valid absolute HTTP(S) URL",
		workflowBody(t, func(wf map[string]any) {
			wf["planner_type"] = "http"
			delete(wf, "planner_static_steps")
			wf["fetch_base_url"] = "http://orchestrator:8080"
			wf["planner_url"] = "planner:9000/decide"
		}))
}

// SPEC.md 16 rule 3, first two limbs: planner_type = "static" with no
// planner_static_steps, or an empty array.
func TestRejectsStaticPlannerWithoutSteps(t *testing.T) {
	rejectWorkflow(t, "SPEC.md 16 rule 3: planner_type 'static' with no planner_static_steps",
		workflowBody(t, func(wf map[string]any) { delete(wf, "planner_static_steps") }))
	rejectWorkflow(t, "SPEC.md 16 rule 3: planner_static_steps is an empty array",
		workflowBody(t, func(wf map[string]any) { wf["planner_static_steps"] = []any{} }))
}

// SPEC.md 16 rule 4: any unknown key. "A silently ignored retrylimit makes the
// user believe a setting took effect."
func TestRejectsUnknownWorkflowKey(t *testing.T) {
	rejectWorkflow(t, "SPEC.md 16 rule 4: an unknown top-level key",
		workflowBody(t, func(wf map[string]any) { wf["retrylimit"] = 5 }))
}

// SPEC.md 16 rule 5: any configuration field of the wrong JSON type - "'3' is
// not 3, and must not be coerced".
func TestRejectsWrongJSONType(t *testing.T) {
	rejectWorkflow(t, "SPEC.md 16 rule 5: step_max_attempts given as the string \"3\"",
		workflowBody(t, func(wf map[string]any) { wf["step_max_attempts"] = "3" }))
	rejectWorkflow(t, "SPEC.md 16 rule 5: step_timeout_seconds given as the string \"300\"",
		workflowBody(t, func(wf map[string]any) { wf["step_timeout_seconds"] = "300" }))
}

// SPEC.md 16 rule 6 and SPEC.md 11.1: any *_max_attempts below 1, any
// *_timeout_seconds below 1, or a negative step_retry_delay_seconds. These are
// also SPEC.md 6.1's column invariants.
//
// SPEC.md 11.1 states the reason a value below 1 is refused rather than
// interpreted: it removes the need to define what "zero attempts" would mean,
// a step created but never executed having no useful semantics.
func TestRejectsOutOfRangeConfiguration(t *testing.T) {
	cases := []struct {
		why   string
		field string
		value any
	}{
		{"step_max_attempts below 1", "step_max_attempts", 0},
		{"planner_max_attempts below 1", "planner_max_attempts", 0},
		{"step_timeout_seconds below 1", "step_timeout_seconds", 0},
		{"planner_timeout_seconds below 1", "planner_timeout_seconds", 0},
		{"a negative step_retry_delay_seconds", "step_retry_delay_seconds", -1},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			rejectWorkflow(t, "SPEC.md 16 rule 6, 11.1: "+c.why,
				workflowBody(t, func(wf map[string]any) { wf[c.field] = c.value }))
		})
	}
}

// ---------------------------------------------------------------------------
// SPEC.md 9.8 - every element of planner_static_steps is a StepSpec
// ---------------------------------------------------------------------------
//
// SPEC.md 6.1: "Every element of planner_static_steps is a StepSpec, and is
// validated as one - by 9.4 and 9.8 - at POST /workflows, before any run
// exists", and SPEC.md 18.1 puts that in alpha. The reason it cannot wait is
// stated in 6.1 and is about correctness rather than tidiness: a malformed
// StepSpec discovered at run time leaves a run that can neither progress nor
// fail, so it would sit RUNNING forever, reclaimed and re-failed by every
// sweep.

// SPEC.md 9.8 rule 1: worker_url is missing or not a valid absolute HTTP(S) URL.
func TestRejectsStepSpecWithoutUsableWorkerURL(t *testing.T) {
	rejectWorkflow(t, "SPEC.md 9.8 rule 1: worker_url is missing",
		stepBody(t, func(s map[string]any) { delete(s, "worker_url") }))
	rejectWorkflow(t, "SPEC.md 9.8 rule 1: worker_url is not an absolute HTTP(S) URL",
		stepBody(t, func(s map[string]any) { s["worker_url"] = "worker:9090/echo" }))
}

// SPEC.md 9.8 rule 2: connection_mode or dispatch_style is absent or not one of
// its enumerated values. SPEC.md 9.4 says why connection_mode is required
// rather than defaulted - so that sync assumptions never bake in silently.
func TestRejectsStepSpecWithBadMode(t *testing.T) {
	rejectWorkflow(t, "SPEC.md 9.8 rule 2: connection_mode is absent",
		stepBody(t, func(s map[string]any) { delete(s, "connection_mode") }))
	rejectWorkflow(t, "SPEC.md 9.8 rule 2: connection_mode is not an enumerated value",
		stepBody(t, func(s map[string]any) { s["connection_mode"] = "syncc" }))
	rejectWorkflow(t, "SPEC.md 9.8 rule 2: dispatch_style is absent",
		stepBody(t, func(s map[string]any) { delete(s, "dispatch_style") }))
	rejectWorkflow(t, "SPEC.md 9.8 rule 2: dispatch_style is not an enumerated value",
		stepBody(t, func(s map[string]any) { s["dispatch_style"] = "enveloppe" }))
}

// SPEC.md 9.8 rule 3: connection_mode = "async" with dispatch_style = "raw",
// which SPEC.md 9.7 makes an illegal combination in every milestone.
func TestRejectsAsyncRaw(t *testing.T) {
	rejectWorkflow(t, "SPEC.md 9.8 rule 3, 9.7: async + raw is never a legal combination",
		stepBody(t, func(s map[string]any) {
			s["connection_mode"] = "async"
			s["dispatch_style"] = "raw"
		}))
}

// SPEC.md 9.8 rule 4: input_from present at the StepSpec level together with
// dispatch_style = "raw". Raw mode has no inputs field to assemble it into, so
// a planner that sent one has misunderstood the mode, and ignoring it silently
// would let the user believe data was being delivered when it was not.
func TestRejectsInputFromInRawMode(t *testing.T) {
	rejectWorkflow(t, "SPEC.md 9.8 rule 4: input_from has no meaning in raw mode",
		stepBody(t, func(s map[string]any) {
			s["dispatch_style"] = "raw"
			s["input_from"] = []any{}
		}))
}

// SPEC.md 9.8 rule 5: timeout_seconds or max_attempts non-null before milestone
// eta. SPEC.md 11.2 makes this the step level of the same override feature the
// run level rejects, and SPEC.md 6.1 notes it applies to a static step exactly
// as it would to one from an HTTP planner.
//
// The mirror image is asserted too: an explicit null is legal, because SPEC.md
// 9.4 gives both fields the default null and rule 5 refuses only a non-null
// value. That is the same reading SPEC.md 16 now states for run-level
// overrides, and the two halves of one feature must not disagree.
func TestRejectsStepLevelOverridesButAcceptsExplicitNull(t *testing.T) {
	rejectWorkflow(t, "SPEC.md 9.8 rule 5: a non-null timeout_seconds before milestone eta",
		stepBody(t, func(s map[string]any) { s["timeout_seconds"] = 60 }))
	rejectWorkflow(t, "SPEC.md 9.8 rule 5: a non-null max_attempts before milestone eta",
		stepBody(t, func(s map[string]any) { s["max_attempts"] = 5 }))

	body := stepBody(t, func(s map[string]any) {
		s["timeout_seconds"] = nil
		s["max_attempts"] = nil
	})
	code, resp, err := harness.Post("/workflows", body)
	if err != nil {
		t.Fatalf("POST /workflows failed: %v", err)
	}
	if code != http.StatusOK && code != http.StatusCreated {
		t.Errorf("SPEC.md 9.4, 9.8 rule 5: an explicit null override is legal, rule 5 refusing only a non-null value\n"+
			"  expected the workflow to be accepted, got HTTP %d\n  body: %s", code, resp)
	}
}

// SPEC.md 9.8 rule 6: an unknown top-level key in a StepSpec. The rule exists
// for the same reason as SPEC.md 16 rule 4, and 9.8 spells out the cost it is
// paying for: a planner that adds a field of its own breaks against an
// orchestrator that has not learned it yet, with no forward-compatibility
// escape hatch, by choice.
func TestRejectsUnknownStepSpecKey(t *testing.T) {
	rejectWorkflow(t, "SPEC.md 9.8 rule 6: an unknown key in a StepSpec, the common cause being a typo",
		stepBody(t, func(s map[string]any) { s["worker_ur1"] = "http://worker:9090/echo" }))
}

// ---------------------------------------------------------------------------
// SPEC.md 16, POST /workflows/{id}/runs - the run-creation body
// ---------------------------------------------------------------------------

// SPEC.md 16 and 11.2: any non-empty overrides is a 400 until milestone eta.
//
// SPEC.md 11.2 states why it is a refusal and not silence: a setting silently
// ignored makes the user believe it took effect, which is the failure mode
// SPEC.md 16 exists to prevent. There is nowhere to put the value either -
// runs has no overrides column until eta.
func TestRejectsNonEmptyOverrides(t *testing.T) {
	rejectRun(t, "SPEC.md 16, 11.2: a non-empty overrides before milestone eta",
		`{"input":{"text":"hello"},"overrides":{"step_max_attempts":10}}`)
}

// SPEC.md 16: the three shapes that mean "no overrides" are all accepted -
// {}, null, and omission.
//
// SPEC.md 16 enumerates a missing input as a 400 and pointedly does not
// enumerate a missing overrides, and states that null and omission mean the
// same thing as {}, for the reason 9.4 and 9.8 rule 5 already establish at the
// step level.
func TestAcceptsEveryShapeThatMeansNoOverrides(t *testing.T) {
	acceptRun(t, "SPEC.md 10.1: overrides given as {}",
		`{"input":{"text":"hello"},"overrides":{}}`)
	acceptRun(t, "SPEC.md 16: overrides given as null means no overrides",
		`{"input":{"text":"hello"},"overrides":null}`)
	acceptRun(t, "SPEC.md 16: overrides omitted means no overrides",
		`{"input":{"text":"hello"}}`)
}

// SPEC.md 16: a missing input is a 400. Unlike overrides, input is enumerated.
func TestRejectsMissingInput(t *testing.T) {
	rejectRun(t, "SPEC.md 16: the run-creation body has no input",
		`{"overrides":{}}`)
}

// SPEC.md 16: an unknown key in the run-creation body is a 400, for the same
// reason as rule 4 at the workflow level.
func TestRejectsUnknownRunKey(t *testing.T) {
	rejectRun(t, "SPEC.md 16: an unknown key in the run-creation body",
		`{"input":{"text":"hello"},"overrides":{},"retrylimit":5}`)
}

// SPEC.md 10.5: 404 is "no such entity". A run cannot be started against a
// workflow that does not exist, and the refusal is a 404 rather than the 400s
// above because the request is well-formed.
func TestRejectsRunOnUnknownWorkflow(t *testing.T) {
	const unknown = "00000000-0000-4000-8000-000000000000"
	code, resp, err := harness.Post("/workflows/"+unknown+"/runs",
		[]byte(`{"input":{"text":"hello"},"overrides":{}}`))
	if err != nil {
		t.Fatalf("POST /workflows/{id}/runs failed: %v", err)
	}
	assertRejection(t, "SPEC.md 10.5: starting a run on a workflow that does not exist is a 404",
		http.StatusNotFound, code, resp)
}
