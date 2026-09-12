// Package httpapi is the HTTP surface of SPEC.md 10.
//
// SPEC.md 18.1 fixes which of it milestone α implements: "POST /workflows,
// POST /workflows/{id}/runs, GET /runs/{run_id}, GET /runs/{run_id}/steps and
// GET /healthz. The remaining read endpoints land with ζ, the first milestone
// that has a planner able to call them." SPEC.md 10.2 states the complete read
// surface because it is a contract a planner author builds against, not because
// α owes all of it.
//
// Milestone δ adds one control endpoint to that list and nothing else:
// POST /runs/{run_id}/replay (SPEC.md 10.1, SPEC.md 14). POST
// /runs/{run_id}/cancel is milestone ι and GET /runs/{run_id}/dlq lands with
// the rest of SPEC.md 10.2's read surface, so neither is registered here.
//
// SPEC.md 10.2 also settles who the read endpoints are for: "the planner's read
// access and the operator's read access are one surface, not two", because
// "they ask the same questions. Two surfaces would drift apart, as they did in
// the previous project."
package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aaronwu001/piton/internal/engine"
	"github.com/aaronwu001/piton/internal/model"
	"github.com/aaronwu001/piton/internal/storage"
	"github.com/aaronwu001/piton/internal/validate"
)

// maxBodyBytes bounds a request body. It is a guard rail, not a rule: SPEC.md
// puts no limit on a workflow definition's size, and this one is far above any
// plausible one.
const maxBodyBytes = 8 << 20

// API serves SPEC.md 10's endpoints.
type API struct {
	store  storage.Store
	engine *engine.Engine
	logger *log.Logger
}

func New(store storage.Store, eng *engine.Engine, logger *log.Logger) *API {
	return &API{store: store, engine: eng, logger: logger}
}

// Handler registers exactly the endpoints milestone α implements.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("POST /workflows", a.createWorkflow)
	mux.HandleFunc("POST /workflows/{workflow_id}/runs", a.createRun)
	// SPEC.md 10.1, added at milestone δ (SPEC.md 18, order 4: "replay in its
	// variants"). It is the first control endpoint since α.
	mux.HandleFunc("POST /runs/{run_id}/replay", a.replayRun)
	// SPEC.md 10.2's two remaining read endpoints. They were designed from the
	// start and implemented late, which SPEC.md 10.2 itself argues against —
	// "these endpoints are required early, not late" — and the reason they
	// could wait is that until now every consumer was a planner or an operator
	// at a terminal, and SPEC.md 17.1 makes database truth the interface. A
	// client that cannot open psql has neither.
	mux.HandleFunc("GET /runs", a.listRuns)
	mux.HandleFunc("GET /runs/{run_id}", a.getRun)
	mux.HandleFunc("GET /runs/{run_id}/steps", a.getRunSteps)
	mux.HandleFunc("GET /runs/{run_id}/dlq", a.getRunDLQ)
	// SPEC.md 10.2, added at milestone zeta. It is the endpoint a planner reads
	// with: SPEC.md 9.2 sends it a catalogue that carries output_bytes and no
	// content, "and a planner that wants an output fetches it from the read
	// API". SPEC.md 4.1 makes that the same surface the operator uses, "one
	// surface, not two", so nothing about it is planner-specific.
	mux.HandleFunc("GET /steps/{step_id}/output", a.getStepOutput)
	return mux
}

// ---------------------------------------------------------------------------
// SPEC.md 10.5 — error responses
// ---------------------------------------------------------------------------

// errorBody is SPEC.md 10.5's shape.
//
// SPEC.md 10.5: "a rejection states the actual current state, not merely that
// the request was refused", because "the operator's next action depends on what
// is true now. '409 Conflict' alone forces a second request to find out."
// Beyond `error` and `message`, a rejection "carries the identifier and current
// status of every entity the request named or would have touched, and omits
// only those that do not exist" — hence the omitempty on every remaining field.
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`

	WorkflowID string `json:"workflow_id,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	RunStatus  string `json:"run_status,omitempty"`
	StepID     string `json:"step_id,omitempty"`
	StepStatus string `json:"step_status,omitempty"`
}

// The `error` slugs. SPEC.md 10.5 requires them to be stable and machine
// readable, and pairs each with a code in its table: 400 for a malformed
// request or a violation of SPEC.md 16, 404 for no such entity, 409 for a state
// that forbids the operation, 503 for storage being unreachable.
const (
	slugInvalidRequest = "invalid_request"
	slugNotFound       = "not_found"
	slugUnavailable    = "storage_unavailable"

	// slugConflict is the one slug SPEC.md fixes by printing it. SPEC.md 10.5's
	// worked example is a refused replay, and it carries `"error": "conflict"`.
	slugConflict = "conflict"
)

func (a *API) writeJSON(w http.ResponseWriter, code int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		a.logf("cannot encode a response: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(raw)
}

func (a *API) writeError(w http.ResponseWriter, code int, body errorBody) {
	a.writeJSON(w, code, body)
}

// writeStorageError maps a storage failure onto SPEC.md 10.5's table. Anything
// that is not "no such entity" is storage being unreachable as far as the
// caller is concerned — SPEC.md 10.5's 503 — and the diagnosis stays in the
// log rather than being handed to a client that cannot act on it.
func (a *API) writeStorageError(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, storage.ErrNotFound) {
		a.writeError(w, http.StatusNotFound, errorBody{Error: slugNotFound, Message: what})
		return
	}
	a.logf("%s: %v", what, err)
	a.writeError(w, http.StatusServiceUnavailable, errorBody{
		Error:   slugUnavailable,
		Message: what + ": storage is unreachable",
	})
}

func (a *API) logf(format string, args ...any) {
	if a.logger != nil {
		a.logger.Printf(format, args...)
	}
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	defer r.Body.Close()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return nil, false
	}
	return raw, true
}

// contextWithTimeout bounds one handler's work. The request's own context is
// the parent, so a client that hangs up stops the query it started.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// ---------------------------------------------------------------------------
// SPEC.md 10.4 — operational
// ---------------------------------------------------------------------------

// healthz is SPEC.md 10.4: "liveness, including whether storage is reachable".
//
// In this milestone a 200 here also carries the weight SPEC.md 18.1 gives it:
// demos/alpha's environment has no migration service, so the orchestrator
// applies migrations at boot and binds its listener only afterwards, which
// makes a 200 the proof that "migrations run to completion before the
// orchestrator serves traffic".
func (a *API) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	if err := a.store.Ping(ctx); err != nil {
		a.logf("healthz: %v", err)
		a.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable", "storage": "unreachable",
		})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "storage": "reachable"})
}

// ---------------------------------------------------------------------------
// SPEC.md 10.1 — control
// ---------------------------------------------------------------------------

// createWorkflow is POST /workflows.
//
// Every refusal here is SPEC.md 16's, and SPEC.md 16 says when they happen:
// "before any run exists". SPEC.md 18.1 puts all of SPEC.md 16 — and therefore
// SPEC.md 9.4 and 9.8 in full for every element of planner_static_steps — in
// this milestone.
func (a *API) createWorkflow(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		a.writeError(w, http.StatusBadRequest, errorBody{
			Error: slugInvalidRequest, Message: "the request body could not be read"})
		return
	}

	wf, rej := validate.Workflow(body)
	if rej != nil {
		// SPEC.md 10.5: "a POST /workflows rejection has no run to describe".
		// There is no workflow to describe either — it was never created — so
		// the two mandatory fields are all this body can honestly carry.
		a.writeError(w, http.StatusBadRequest, errorBody{Error: rej.Slug, Message: rej.Message})
		return
	}

	wf.WorkflowID = model.NewID()
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()
	if err := a.store.CreateWorkflow(ctx, wf); err != nil {
		a.writeStorageError(w, "cannot create the workflow", err)
		return
	}

	a.writeJSON(w, http.StatusCreated, map[string]any{
		"workflow_id": wf.WorkflowID,
		"name":        wf.Name,
		"created_at":  wf.CreatedAt,
	})
}

// createRun is POST /workflows/{id}/runs.
//
// SPEC.md 5.1: RUNNING is the state a run is created in. owner_id is left NULL:
// SPEC.md 8.6's sweep is what claims it, and SPEC.md 8.5's claim is the only
// statement that may write ownership. The nudge afterwards asks the sweep to
// run now instead of at its next tick, which is latency and nothing else.
func (a *API) createRun(w http.ResponseWriter, r *http.Request) {
	workflowID := r.PathValue("workflow_id")

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	wf, err := a.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		// SPEC.md 10.5: 404 is "no such entity". The workflow does not exist,
		// so it is one of the entities the rejection "omits" rather than
		// describes.
		a.writeStorageError(w, "no workflow with that workflow_id", err)
		return
	}

	body, ok := readBody(w, r)
	if !ok {
		a.writeError(w, http.StatusBadRequest, errorBody{
			Error: slugInvalidRequest, Message: "the request body could not be read",
			WorkflowID: wf.WorkflowID})
		return
	}

	input, rej := validate.RunInput(body)
	if rej != nil {
		a.writeError(w, http.StatusBadRequest, errorBody{
			Error: rej.Slug, Message: rej.Message, WorkflowID: wf.WorkflowID})
		return
	}

	run := &model.Run{
		RunID:      model.NewID(),
		WorkflowID: wf.WorkflowID,
		Status:     model.StatusRunning,
		Input:      input,
	}
	if err := a.store.CreateRun(ctx, run); err != nil {
		a.writeStorageError(w, "cannot create the run", err)
		return
	}
	if a.engine != nil {
		a.engine.Nudge()
	}

	a.writeJSON(w, http.StatusCreated, map[string]any{
		"run_id":      run.RunID,
		"workflow_id": run.WorkflowID,
		"status":      run.Status,
		"created_at":  run.CreatedAt,
	})
}

// replayRun is POST /runs/{run_id}/replay (SPEC.md 10.1, SPEC.md 14).
//
// SPEC.md 14 targets the RUN and not a dead-letter entry, and says why: "a
// dead-letter entry is history and diverges from current reality after several
// rounds. The previous design exposed POST /dlq/{entry_id}/replay and then
// needed a patch rule saying the worker-side/planner-side branch must be
// derived from current state rather than from the entry. Targeting the run
// deletes that entire class of confusion." Nothing in this handler reads
// dead_letter_queue.
//
// The decision is not taken here. The whole of it is the conditional UPDATE
// inside storage.ReplayRun, because SPEC.md 14 makes that transaction the gate;
// this function's job is to turn its three answers into SPEC.md 10.5's three
// codes.
func (a *API) replayRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	replayCount, err := a.store.ReplayRun(ctx, runID)
	switch {
	case err == nil:
		// SPEC.md 14 fixes no success code and no success body, so this is the
		// smallest honest answer: what the run now is, and the counter SPEC.md
		// 14 requires to be inspectable. SPEC.md 6.2's replay_count is returned
		// because "the owner must be able to see, at a terminal, which round a
		// given attempt belonged to" and this is the round he just started.
		a.writeJSON(w, http.StatusOK, map[string]any{
			"run_id":       runID,
			"status":       model.StatusRunning,
			"replay_count": replayCount,
		})
		// The nudge asks the sweep to run now rather than at its next tick.
		// SPEC.md 14 hands the run to "the next sweep", and this changes only
		// which one that is — latency, not mechanism, exactly as in createRun.
		if a.engine != nil {
			a.engine.Nudge()
		}

	case errors.Is(err, storage.ErrNotInDLQ):
		a.writeError(w, http.StatusConflict, a.describeRefusedReplay(ctx, runID))

	default:
		// ErrNotFound becomes SPEC.md 10.5's 404, anything else its 503.
		a.writeStorageError(w, "no run with that run_id", err)
	}
}

// describeRefusedReplay builds SPEC.md 10.5's body for a replay the gate
// refused.
//
// SPEC.md 10.5: "a rejection states the actual current state, not merely that
// the request was refused... the operator's next action depends on what is true
// now. '409 Conflict' alone forces a second request to find out." And beyond
// `error` and `message`, "a rejection carries the identifier and current status
// of every entity the request named or would have touched, and omits only those
// that do not exist" — so the step is described too, when the run has one, and
// omitted when it does not. SPEC.md 10.5 gives the reason in the case that is
// exactly this one: "a refused replay is explained by the run's status, but
// what he does next depends on the step's."
//
// SPEC.md 5.4's last_step is the step described: it is the one a replay "would
// have touched" (SPEC.md 14 returns it to RUNNING when it is in DLQ).
//
// The reads are best effort. A rejection that could not describe the run is
// still a rejection, and answering 409 with two fields is better than turning a
// refusal into a 503 because a follow-up query failed.
func (a *API) describeRefusedReplay(ctx context.Context, runID string) errorBody {
	body := errorBody{
		Error:   slugConflict,
		Message: "run is not in DLQ and cannot be replayed",
		RunID:   runID,
	}

	run, err := a.store.GetRun(ctx, runID)
	if err != nil {
		a.logf("cannot describe the run a replay was refused for: %v", err)
		return body
	}
	body.RunStatus = run.Status

	steps, err := a.store.ListSteps(ctx, runID)
	if err != nil {
		a.logf("cannot describe the steps a replay was refused for: %v", err)
		return body
	}
	// SPEC.md 5.4: last_step is the step with the highest seq, and "a run with
	// no steps is defined to have last_step = DONE" — a definition, not a row,
	// so there is nothing to name and SPEC.md 10.5's "omits only those that do
	// not exist" applies.
	if len(steps) > 0 {
		last := steps[len(steps)-1]
		for _, st := range steps {
			if st.Seq > last.Seq {
				last = st
			}
		}
		body.StepID = last.StepID
		body.StepStatus = last.Status
	}
	return body
}

// ---------------------------------------------------------------------------
// SPEC.md 10.2 — reads
// ---------------------------------------------------------------------------

// stepSummary is a catalogue row. It carries output_bytes rather than the
// output itself, which is SPEC.md 10.2's two-layer split — "the catalogue is
// cheap and may be fetched whole, while outputs may be large and are fetched
// individually" — and the same shape SPEC.md 9.2 gives a planner's history.
type stepSummary struct {
	StepID       string          `json:"step_id"`
	Seq          int             `json:"seq"`
	StepName     *string         `json:"step_name"`
	Status       string          `json:"status"`
	Decision     json.RawMessage `json:"decision"`
	AttemptCount int             `json:"attempt_count"`
	OutputBytes  int             `json:"output_bytes"`
	CreatedAt    time.Time       `json:"created_at"`
	CompletedAt  *time.Time      `json:"completed_at"`

	// Attempts is populated only by GET /runs/{run_id}/steps, which SPEC.md
	// 10.2 defines as "the step catalogue, with attempt summaries".
	Attempts []attemptSummary `json:"attempts,omitempty"`
}

type attemptSummary struct {
	AttemptID string `json:"attempt_id"`
	AttemptNo int    `json:"attempt_no"`

	// ReplayRound is SPEC.md 6.4's "value of runs.replay_count when this
	// attempt was dispatched".
	//
	// It is on the wire because SPEC.md 14 resets steps.attempt_count on a
	// replay: after one, the live counter no longer says what the failed round
	// burned, and attempt_no stays contiguous across rounds, so nothing else in
	// this response distinguishes an attempt of round 0 from one of round 1.
	// The column has always been there; only the client could not see it.
	ReplayRound int `json:"replay_round"`

	Status         string     `json:"status"`
	ConnectionMode string     `json:"connection_mode"`
	DeadlineAt     time.Time  `json:"deadline_at"`
	DispatchedBy   string     `json:"dispatched_by"`
	FailureReason  *string    `json:"failure_reason"`
	ErrorText      *string    `json:"error_text"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
}

type runBody struct {
	RunID      string          `json:"run_id"`
	WorkflowID string          `json:"workflow_id"`
	Status     string          `json:"status"`
	Input      json.RawMessage `json:"input"`

	// SPEC.md 6.2 keeps no copy of the newest planner error on the run: the
	// diagnosis lives on the call that produced it (SPEC.md 6.8), where it is
	// one row among the run's whole conversation with its planner rather than
	// a single value overwritten by the next failure.
	PlannerAttemptCount int `json:"planner_attempt_count"`
	ReplayCount         int `json:"replay_count"`

	OwnerID   *string    `json:"owner_id"`
	ClaimedAt *time.Time `json:"claimed_at"`
	CreatedAt time.Time  `json:"created_at"`

	Steps []stepSummary `json:"steps"`
}

func summariseStep(st *model.Step) stepSummary {
	return stepSummary{
		StepID:       st.StepID,
		Seq:          st.Seq,
		StepName:     st.StepName,
		Status:       st.Status,
		Decision:     json.RawMessage(st.Decision),
		AttemptCount: st.AttemptCount,
		OutputBytes:  len(st.Output),
		CreatedAt:    st.CreatedAt,
		CompletedAt:  st.CompletedAt,
	}
}

// getRun is GET /runs/{run_id}: "one run, with a summary of its steps".
func (a *API) getRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	run, err := a.store.GetRun(ctx, runID)
	if err != nil {
		a.writeStorageError(w, "no run with that run_id", err)
		return
	}
	steps, err := a.store.ListSteps(ctx, runID)
	if err != nil {
		a.writeStorageError(w, "cannot read the run's steps", err)
		return
	}

	body := runBody{
		RunID:               run.RunID,
		WorkflowID:          run.WorkflowID,
		Status:              run.Status,
		Input:               json.RawMessage(run.Input),
		PlannerAttemptCount: run.PlannerAttemptCount,
		ReplayCount:         run.ReplayCount,
		OwnerID:             run.OwnerID,
		ClaimedAt:           run.ClaimedAt,
		CreatedAt:           run.CreatedAt,
		Steps:               make([]stepSummary, 0, len(steps)),
	}
	for _, st := range steps {
		body.Steps = append(body.Steps, summariseStep(st))
	}
	a.writeJSON(w, http.StatusOK, body)
}

// getRunSteps is GET /runs/{run_id}/steps: "the step catalogue, with attempt
// summaries".
func (a *API) getRunSteps(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	// The run is read first so that a request for a run that does not exist is
	// SPEC.md 10.5's 404 rather than an empty catalogue, which would say "this
	// run has no steps" about a run that is not there.
	if _, err := a.store.GetRun(ctx, runID); err != nil {
		a.writeStorageError(w, "no run with that run_id", err)
		return
	}
	steps, err := a.store.ListSteps(ctx, runID)
	if err != nil {
		a.writeStorageError(w, "cannot read the run's steps", err)
		return
	}
	attempts, err := a.store.ListAttempts(ctx, runID)
	if err != nil {
		a.writeStorageError(w, "cannot read the run's attempts", err)
		return
	}

	byStep := make(map[string][]attemptSummary, len(steps))
	for _, at := range attempts {
		byStep[at.StepID] = append(byStep[at.StepID], attemptSummary{
			AttemptID:      at.AttemptID,
			AttemptNo:      at.AttemptNo,
			ReplayRound:    at.ReplayRound,
			Status:         at.Status,
			ConnectionMode: at.ConnectionMode,
			DeadlineAt:     at.DeadlineAt,
			DispatchedBy:   at.DispatchedBy,
			FailureReason:  at.FailureReason,
			ErrorText:      at.ErrorText,
			StartedAt:      at.StartedAt,
			FinishedAt:     at.FinishedAt,
		})
	}

	out := make([]stepSummary, 0, len(steps))
	for _, st := range steps {
		summary := summariseStep(st)
		summary.Attempts = byStep[st.StepID]
		out = append(out, summary)
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "steps": out})
}

// ---------------------------------------------------------------------------
// SPEC.md 10.2 — the collection reads
// ---------------------------------------------------------------------------

// runSummary is one row of GET /runs.
//
// It carries neither the run's input nor its steps. SPEC.md 10.2 calls the
// catalogue "cheap and may be fetched whole" against outputs that "may be large
// and are fetched individually", and the same split applies one level up: a
// list of every run this deployment has ever executed is no place to inline a
// workflow input of unknown size.
type runSummary struct {
	RunID               string     `json:"run_id"`
	WorkflowID          string     `json:"workflow_id"`
	Status              string     `json:"status"`
	PlannerAttemptCount int        `json:"planner_attempt_count"`
	ReplayCount         int        `json:"replay_count"`
	OwnerID             *string    `json:"owner_id"`
	ClaimedAt           *time.Time `json:"claimed_at"`
	CreatedAt           time.Time  `json:"created_at"`
}

// SPEC.md 10.2's limits on GET /runs.
const (
	runsLimitDefault = 50
	runsLimitMax     = 200
)

// listRuns is SPEC.md 10.2's GET /runs.
func (a *API) listRuns(w http.ResponseWriter, r *http.Request) {
	q, bad := parseRunQuery(r)
	if bad != nil {
		a.writeError(w, http.StatusBadRequest, *bad)
		return
	}

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	// One more than asked for. Whether a further page exists is a question
	// about the row AFTER this page, and fetching it is the only way to answer
	// it without a second query — a count would be a second query whose answer
	// could already be stale.
	q.Limit++
	runs, err := a.store.ListRuns(ctx, q)
	if err != nil {
		a.writeStorageError(w, "cannot list runs", err)
		return
	}

	body := map[string]any{}
	if len(runs) == q.Limit {
		last := runs[q.Limit-2]
		runs = runs[:q.Limit-1]
		body["next_cursor"] = encodeCursor(last.CreatedAt, last.RunID)
	}

	out := make([]runSummary, 0, len(runs))
	for _, run := range runs {
		out = append(out, runSummary{
			RunID:               run.RunID,
			WorkflowID:          run.WorkflowID,
			Status:              run.Status,
			PlannerAttemptCount: run.PlannerAttemptCount,
			ReplayCount:         run.ReplayCount,
			OwnerID:             run.OwnerID,
			ClaimedAt:           run.ClaimedAt,
			CreatedAt:           run.CreatedAt,
		})
	}
	body["runs"] = out
	a.writeJSON(w, http.StatusOK, body)
}

// parseRunQuery reads SPEC.md 10.2's three parameters, refusing anything it
// does not recognise.
//
// SPEC.md 10.2 says an unknown `status` is a 400 "as §16 requires of every
// other input", and §16's strictness principle is why the same applies to a
// limit out of range and a cursor that does not decode: silently clamping a
// limit of 5000 to 200, or ignoring a corrupt cursor and returning page one,
// hands the caller a plausible answer to a question it did not ask.
func parseRunQuery(r *http.Request) (storage.RunQuery, *errorBody) {
	q := storage.RunQuery{Limit: runsLimitDefault}
	values := r.URL.Query()

	// SPEC.md 10.2: `status` is repeatable.
	for _, s := range values["status"] {
		if !model.IsRunStatus(s) {
			return q, &errorBody{
				Error: slugInvalidRequest,
				Message: fmt.Sprintf("status %q is not one of %s (SPEC.md 5.1, SPEC.md 10.2)",
					s, strings.Join(model.RunStatuses(), ", ")),
			}
		}
		q.Statuses = append(q.Statuses, s)
	}

	if raw := values.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > runsLimitMax {
			return q, &errorBody{
				Error: slugInvalidRequest,
				Message: fmt.Sprintf("limit must be an integer from 1 to %d (SPEC.md 10.2); got %q",
					runsLimitMax, raw),
			}
		}
		q.Limit = n
	}

	if raw := values.Get("cursor"); raw != "" {
		cursor, err := decodeCursor(raw)
		if err != nil {
			return q, &errorBody{
				Error:   slugInvalidRequest,
				Message: "cursor is not one this API issued (SPEC.md 10.2): " + err.Error(),
			}
		}
		q.Cursor = cursor
	}

	return q, nil
}

// The cursor is opaque to the client by SPEC.md 10.2 and carries the sort key
// of the last run of the previous page — the pair the ordering is defined on.
//
// Opaque means the client must not parse it, not that it must be encrypted:
// nothing in it is secret, and a run_id is already in the response beside it.
// The encoding exists so that a change of sort key is not a change of wire
// format.
func encodeCursor(createdAt time.Time, runID string) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(createdAt.UTC().Format(time.RFC3339Nano) + "|" + runID))
}

func decodeCursor(raw string) (*storage.RunCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("it is not valid base64")
	}
	at, id, ok := strings.Cut(string(decoded), "|")
	if !ok {
		return nil, errors.New("it does not name a position")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, errors.New("it does not carry a timestamp")
	}
	return &storage.RunCursor{CreatedAt: createdAt, RunID: id}, nil
}

// dlqEntryBody is one row of GET /runs/{run_id}/dlq (SPEC.md 6.5).
type dlqEntryBody struct {
	DLQID string `json:"dlq_id"`

	// StepID is null for a planner-side entry (SPEC.md 6.5). It is the field
	// that tells a client which side of SPEC.md 12.3 killed the run, and it is
	// a pointer rather than an empty string for exactly that reason: "" and
	// "no step" are different answers.
	StepID *string `json:"step_id"`

	Reason      string    `json:"reason"`
	ReplayRound int       `json:"replay_round"`
	ErrorText   string    `json:"error_text"`
	CreatedAt   time.Time `json:"created_at"`
}

// getRunDLQ is SPEC.md 10.2's GET /runs/{run_id}/dlq.
func (a *API) getRunDLQ(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	// The run is read first, so that a request for a run that does not exist
	// is SPEC.md 10.5's 404 rather than an empty history — which would say
	// "this run stopped for no reason" about a run that is not there. SPEC.md
	// 10.2 requires the two answers to be distinguishable: "a run with no
	// entries is an empty list with a 200, not a 404".
	if _, err := a.store.GetRun(ctx, runID); err != nil {
		a.writeStorageError(w, "no run with that run_id", err)
		return
	}
	entries, err := a.store.ListDeadLetters(ctx, runID)
	if err != nil {
		a.writeStorageError(w, "cannot read the run's dead-letter history", err)
		return
	}

	out := make([]dlqEntryBody, 0, len(entries))
	for _, e := range entries {
		out = append(out, dlqEntryBody{
			DLQID:       e.DLQID,
			StepID:      e.StepID,
			Reason:      e.Reason,
			ReplayRound: e.ReplayRound,
			ErrorText:   e.ErrorText,
			CreatedAt:   e.CreatedAt,
		})
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "entries": out})
}

// getStepOutput is SPEC.md 10.2's GET /steps/{step_id}/output: "that step's
// stored output bytes, verbatim".
//
// Verbatim is the whole contract. SPEC.md 7.1 keeps every JSON document opaque
// []byte end to end and SPEC.md 6.3 stores "the worker's whole response body",
// so this handler re-serialises nothing and selects nothing out of it:
// "selecting a field out of a worker's response is the planner's job, not the
// engine's".
func (a *API) getStepOutput(w http.ResponseWriter, r *http.Request) {
	stepID := r.PathValue("step_id")

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	step, err := a.store.GetStep(ctx, stepID)
	if err != nil {
		a.writeStorageError(w, "no step with that step_id", err)
		return
	}

	// SPEC.md 6.3: "a step's completion is signalled by status = 'DONE' and by
	// nothing else. No rule, query or implementation may treat 'output is
	// present' as meaning the step finished." So the status is what is tested,
	// and a step that has not finished is refused with the state it is in -
	// SPEC.md 10.5's 409, which "states the actual current state, not merely
	// that the request was refused".
	if step.Status != model.StatusDone {
		a.writeError(w, http.StatusConflict, errorBody{
			Error:      slugConflict,
			Message:    "step has no stored output because it has not completed",
			RunID:      step.RunID,
			StepID:     step.StepID,
			StepStatus: step.Status,
		})
		return
	}

	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(step.Output)
}
