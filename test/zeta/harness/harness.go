// Package harness brings milestone zeta's environment up and gives the
// automated suite the two interfaces SPEC.md makes authoritative: the HTTP API
// (SPEC.md 10) and database truth (SPEC.md 17.1). It also gives the suite a
// third, narrower reading: the HTTP planner fixture's own account of what it
// was asked and what it fetched (see PlannerStats below) - zeta is the first
// milestone where a service outside the orchestrator's own build participates
// in a run.
//
// CLAUDE.md 5.5.4 requires the suite to reference the milestone's own compose
// file rather than define an environment of its own, so that the owner's
// hand-run demo (CLAUDE.md 4 step 5) and the suite exercise an identical
// definition. Every docker command below therefore runs
// demos/zeta/docker-compose.yml, and nothing in this package writes a compose
// file of its own or edits that one.
//
// WHY THIS DUPLICATES test/alpha, test/gamma, test/beta AND test/delta's
// HARNESSES
//
//	They differ in which demo directory they point at and in the one
//	capability each milestone needs that the previous did not - beta can kill
//	and restart the orchestrator, delta can replay a run, zeta can read the
//	HTTP planner fixture's own counters and wait on a fourth service's health.
//	A shared package was possible and was not done: alpha's, gamma's and
//	beta's suites are the guards on three milestones the owner has already
//	verified by hand (R34-a, R36-a), and delta's suite guards the fourth,
//	awaiting his hand-run (R39-d, still outstanding as of R40-q). Refactoring
//	any of them to serve zeta would put those guards at risk for a later
//	milestone's convenience. BACKLOG.md records this as B18 - already past its
//	own stated trigger at delta (R37-k) and left untouched for the same
//	reason; BACKLOG.md is authority on nothing, and the trade is the owner's
//	to call, not this package's.
//
// WHAT ZETA'S HARNESS DROPS
//
//	ReplayConcurrently, delta's helper for firing several replays of the same
//	run at once to pin SPEC.md 14's idempotency gate, is not carried over.
//	Zeta's demo is "one static workflow and one http workflow over the same
//	workers, both reaching DONE" (GRILLING_LOG.md R40-f), plus - per R40-p -
//	SPEC.md 14's replay of an L5 (planner-side DLQ) run, which is a single
//	replay, not a race. The concurrent-replay guard on SPEC.md 14's gate stays
//	exactly where it is, in delta's own suite.
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// BaseURL is where the orchestrator is reachable from the host: the port
// demos/zeta/docker-compose.yml publishes.
const BaseURL = "http://localhost:8080"

// RunInput is the workflow input every leg submits. No worker sees it: SPEC.md
// 9.5's envelope carries params and inputs and no workflow input.
const RunInput = `{"text":"hello"}`

const (
	// upTimeout is generous because the first run of a group builds the
	// orchestrator and planner fixture images from source.
	upTimeout = 10 * time.Minute

	// commandTimeout bounds one short docker or psql invocation.
	commandTimeout = 2 * time.Minute

	// HealthTimeout bounds the wait for GET /healthz. There is no migration
	// service, so a 200 here is what proves the migrations finished.
	HealthTimeout = 4 * time.Minute

	// RunTimeout bounds how long one leg may take to reach a terminal state.
	// A run against the http planner adds a synchronous HTTP round trip per
	// step (SPEC.md 9.2, 9.3) on top of everything delta's legs already wait
	// for, and an unreachable planner still has to burn its budget and
	// converge to DLQ (SPEC.md 12.2) rather than hang - so this stays as
	// generous as delta's. It is still a ceiling, not an expectation.
	RunTimeout = 5 * time.Minute

	// Poll is how often the database is asked whether something has happened
	// yet. Zeta waits on events rather than on wall-clock guesses, so this is
	// the resolution of every wait in the suite.
	Poll = 250 * time.Millisecond
)

func moduleRoot() (string, error) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("harness: cannot locate its own source file")
	}
	dir := filepath.Dir(self)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("harness: no go.mod found above %s", self)
		}
		dir = parent
	}
}

// DemoDir returns demos/zeta, the milestone's own environment directory.
func DemoDir() (string, error) {
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "demos", "zeta"), nil
}

func composeRun(ctx context.Context, stdin string, args ...string) (string, error) {
	dir, err := DemoDir()
	if err != nil {
		return "", err
	}
	full := append([]string{"compose", "-f", filepath.Join(dir, "docker-compose.yml")}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	// The working directory matters as much as -f: the compose file mounts
	// ./piton.yaml, which resolves against it.
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	return out.String(), err
}

// Up starts the group's environment from a clean database.
//
// CLAUDE.md 5.5.2: a group always begins from a clean database, so the volume
// wipe happens before the environment comes up and not only after it goes down.
func Up() error {
	ctx, cancel := context.WithTimeout(context.Background(), upTimeout)
	defer cancel()

	// Best effort: there is usually nothing to remove.
	_, _ = composeRun(ctx, "", "down", "-v", "--remove-orphans")

	out, err := composeRun(ctx, "", "up", "-d", "--build", "--wait", "--wait-timeout", "240")
	if err != nil {
		// Diagnosis first, then teardown. A half-started environment left
		// running holds the published port 8080 and would make the next group -
		// or the owner's hand-run demo - fail for a reason that has nothing to
		// do with what it was testing.
		logs, _ := composeRun(ctx, "", "logs", "--tail=60", "orchestrator")
		_, _ = composeRun(ctx, "", "down", "-v", "--remove-orphans")
		return fmt.Errorf("docker compose up failed: %w\n%s\n--- orchestrator logs ---\n%s", err, out, logs)
	}
	return nil
}

// Down tears the group's environment down, including the volume wipe
// CLAUDE.md 5.5.1 requires before the next group starts.
func Down() error {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := composeRun(ctx, "", "down", "-v", "--remove-orphans")
	if err != nil {
		return fmt.Errorf("docker compose down -v failed: %w\n%s", err, out)
	}
	return nil
}

// OrchestratorLogs returns the tail of the orchestrator's log, for diagnosis
// only. SPEC.md 17.3 keeps error text in the database; the logs are the second
// resort, never the first.
func OrchestratorLogs(lines int) string {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, _ := composeRun(ctx, "", "logs", fmt.Sprintf("--tail=%d", lines), "orchestrator")
	return out
}

// ---------------------------------------------------------------------------
// Database truth
// ---------------------------------------------------------------------------

// PSQL runs one SQL statement against the demo's database and returns its
// output, unaligned and with surrounding whitespace removed.
//
// It goes through "docker compose exec" rather than a Go driver because
// demos/zeta/docker-compose.yml deliberately publishes no host port for
// postgres, and CLAUDE.md 5.5.4 forbids the suite from defining an environment
// of its own to obtain one. This is the same access path SPEC.md 17.1 gives the
// operator, and the one demo.sh uses.
//
// SQL is fed on stdin, never with -c: psql substitutes :'name' only for input
// it reads through its normal lexer.
func PSQL(sql string, vars ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	args := []string{
		"exec", "-T", "postgres",
		"psql", "-U", "piton", "-d", "piton",
		"-v", "ON_ERROR_STOP=1", "-At",
	}
	for _, v := range vars {
		args = append(args, "-v", v)
	}
	out, err := composeRun(ctx, sql, args...)
	if err != nil {
		return "", fmt.Errorf("psql failed: %w\nquery: %s\noutput: %s", err, sql, out)
	}
	return strings.TrimSpace(out), nil
}

// Await polls a SQL predicate until it is true, and reports what it last saw if
// it never became true.
//
// Every wait in zeta is written this way rather than as a sleep. SPEC.md 14
// ends a replay by clearing owner_id and claimed_at "so the next sweep picks it
// up", and SPEC.md 8.6 fixes no instant at which that sweep runs - so a test
// that slept for a computed duration and then asserted would be testing a
// schedule SPEC.md deliberately does not promise. Waiting for the state itself
// asserts only what is guaranteed, and a slow machine makes the suite slower
// rather than red.
func Await(what, sql string, timeout time.Duration, vars ...string) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		out, err := PSQL(sql, vars...)
		if err != nil {
			last = err.Error()
		} else {
			last = out
			if out == "t" {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s (last: %q)\n  query: %s",
				timeout, what, last, sql)
		}
		time.Sleep(Poll)
	}
}

// StepDigest is one line describing everything about one named step that a
// re-dispatch would change: its status, its budget, when it completed, its
// output, and every attempt it owns.
//
// SPEC.md 14's accepted limitation - "a replay always resumes at the furthest
// step reached... replay resumes from step 2 and never revisits step 1" - is
// asserted with this, taken either side of a second replay round. A digest is
// how "nothing moved" is asserted rather than assumed.
const StepDigest = `SELECT s.status || '|' || s.attempt_count || '|' || coalesce(s.completed_at::text, '') ||
                           '|' || coalesce(md5(s.output::text), '') || '|' ||
                           coalesce((SELECT string_agg(a.attempt_id::text || ':' || a.attempt_no ||
                                                       ':' || a.status, ',' ORDER BY a.attempt_no)
                                       FROM attempts a WHERE a.step_id = s.step_id), '')
                      FROM steps s WHERE s.run_id = :'run' AND s.step_name = :'step';`

// DLQDigest covers only the dead-letter history of a run, for the assertions
// that are about SPEC.md 6.7's append-only rule alone.
const DLQDigest = `SELECT coalesce(md5(string_agg(d.dlq_id::text || ':' || d.reason || ':' ||
                                                  d.replay_round || ':' ||
                                                  md5(d.error_text) || ':' ||
                                                  coalesce(d.step_id::text, 'NULL') || ':' ||
                                                  d.created_at::text,
                                                  ',' ORDER BY d.created_at, d.dlq_id)), '<none>')
                     FROM dead_letter_queue d WHERE d.run_id = :'run';`

// ---------------------------------------------------------------------------
// The HTTP API
// ---------------------------------------------------------------------------

func do(method, path string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, BaseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

// Post issues a JSON POST and returns the status code and body.
func Post(path string, body []byte) (int, []byte, error) { return do(http.MethodPost, path, body) }

// Get issues a GET and returns the status code and body.
func Get(path string) (int, []byte, error) { return do(http.MethodGet, path, nil) }

// WaitHealthy polls GET /healthz until it answers 200.
func WaitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		code, body, err := Get("/healthz")
		switch {
		case err != nil:
			last = err.Error()
		case code == http.StatusOK:
			return nil
		default:
			last = fmt.Sprintf("status %d: %s", code, strings.TrimSpace(string(body)))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("GET /healthz never answered 200 within %s (last: %s)", timeout, last)
		}
		time.Sleep(time.Second)
	}
}

// ---------------------------------------------------------------------------
// The planner fixture - what zeta needs and no earlier milestone did
// ---------------------------------------------------------------------------

// PlannerBaseURL is where the demo's HTTP planner fixture is reachable from
// the host: the port demos/zeta/docker-compose.yml publishes for the fourth
// service, planner.
const PlannerBaseURL = "http://localhost:9100"

// getFullURL issues a GET against an arbitrary URL rather than BaseURL+path.
// It exists only so that Stats and WaitPlannerHealthy can reach
// PlannerBaseURL with do's exact client construction and timeout, without
// changing do itself, which alpha through delta already rely on unmodified.
func getFullURL(url string) (int, []byte, error) {
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Get(url)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

// PlannerStats is one GET {PlannerBaseURL}/stats reading: how many times the
// fixture was called as a planner, and how many times it read a step's output
// back through the orchestrator's read API, both keyed by run_id.
//
// This exists because the database cannot show the second number at all.
// SPEC.md 9.2 hands the planner fetch_base_url precisely so that "a planner
// that wants an output fetches it from the read API" rather than being sent
// the output inline - but a run that reached the right decision proves only
// that the fixture answered correctly, not that it earned the answer by
// reading SPEC.md 10.2's endpoints rather than inferring it from the request
// body's history catalogue. Fetches is the fixture's own account of what it
// did, read directly rather than inferred from an effect the database happens
// to also show.
type PlannerStats struct {
	Calls   map[string]int `json:"calls"`
	Fetches map[string]int `json:"fetches"`
}

// CallsFor returns how many planner calls the fixture received for runID, or
// 0 if it received none.
func (s PlannerStats) CallsFor(runID string) int { return s.Calls[runID] }

// FetchesFor returns how many read-API fetches the fixture performed for
// runID, or 0 if it performed none.
func (s PlannerStats) FetchesFor(runID string) int { return s.Fetches[runID] }

// Stats reads the fixture's counters via GET {PlannerBaseURL}/stats.
func Stats() (PlannerStats, error) {
	code, body, err := getFullURL(PlannerBaseURL + "/stats")
	if err != nil {
		return PlannerStats{}, err
	}
	if code != http.StatusOK {
		return PlannerStats{}, fmt.Errorf("GET %s/stats returned %d: %s", PlannerBaseURL, code, strings.TrimSpace(string(body)))
	}
	var out PlannerStats
	if err := json.Unmarshal(body, &out); err != nil {
		return PlannerStats{}, fmt.Errorf("GET %s/stats returned unparseable JSON: %w: %s", PlannerBaseURL, err, body)
	}
	return out, nil
}

// WaitPlannerHealthy polls GET {PlannerBaseURL}/healthz until it answers 200,
// modelled exactly on WaitHealthy: the same wait for the same reason, aimed at
// the fixture rather than the orchestrator.
func WaitPlannerHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		code, body, err := getFullURL(PlannerBaseURL + "/healthz")
		switch {
		case err != nil:
			last = err.Error()
		case code == http.StatusOK:
			return nil
		default:
			last = fmt.Sprintf("status %d: %s", code, strings.TrimSpace(string(body)))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("GET %s/healthz never answered 200 within %s (last: %s)", PlannerBaseURL, timeout, last)
		}
		time.Sleep(time.Second)
	}
}

// ---------------------------------------------------------------------------
// Replay - carried over from delta, and needed again: SPEC.md 14's replay of
// an L5 (planner-side DLQ) run resumes by asking the planner again rather than
// re-dispatching a step (SPEC.md 12.3's planner-side column; GRILLING_LOG.md
// R40-p)
// ---------------------------------------------------------------------------

// Rejection is SPEC.md 10.5's error body.
//
// SPEC.md 10.5 prints it in full, and the example it prints is a REFUSED
// REPLAY, which is why every field below is named there rather than inferred:
//
//	{ "error": "conflict",
//	  "message": "run is not in DLQ and cannot be replayed",
//	  "run_id": "018f...", "run_status": "RUNNING",
//	  "step_id": "018f...", "step_status": "RUNNING" }
//
// The pointer types carry the difference SPEC.md 10.5 makes load-bearing: a
// rejection "carries the identifier and current status of every entity the
// request named or would have touched, and OMITS ONLY THOSE THAT DO NOT
// EXIST". An absent field and an empty one are therefore different answers,
// and a plain string could not tell them apart.
type Rejection struct {
	Error   string `json:"error"`
	Message string `json:"message"`

	RunID      *string `json:"run_id"`
	RunStatus  *string `json:"run_status"`
	StepID     *string `json:"step_id"`
	StepStatus *string `json:"step_status"`
}

// SlugConflict is the `error` slug SPEC.md 10.5's example body carries for a
// refused replay. It is quoted from SPEC.md and not chosen here: SPEC.md 10.5
// calls `error` "a stable machine-readable slug", and a suite that accepted any
// slug would be testing that a rejection happened rather than that the contract
// held.
const SlugConflict = "conflict"

// There is deliberately no constant for a 404's slug. SPEC.md 10.5's table
// gives 404 the meaning "no such entity" but fixes no slug for it - the one
// body it prints in full is a 409 - so the suite asserts the CODE exactly and
// says nothing about the slug. Naming a string here would be inventing a
// contract no ruling covers (CLAUDE.md 9).

// ReplayResult is one POST /runs/{run_id}/replay exchange.
type ReplayResult struct {
	Code int
	Body []byte
	// Rejection is the decoded body when the call was refused. It is nil when
	// the body did not parse as SPEC.md 10.5's shape.
	Rejection *Rejection
}

// Accepted reports whether the replay was performed.
//
// SPEC.md 14 says a replay of a run that is in DLQ "proceeds"; SPEC.md 10.5
// enumerates the codes for REFUSALS only - 400, 404, 409, 503 - and no ruling
// fixes which success code a performed replay returns. So the suite accepts any
// 2xx and asserts the EFFECT in the database, which is specified, rather than
// the code, which is not. R34-m is the precedent: where SPEC.md fixes no
// wording, the test checks what SPEC.md does fix and stays silent on the rest.
func (r ReplayResult) Accepted() bool { return r.Code >= 200 && r.Code < 300 }

// Replay issues POST /runs/{run_id}/replay (SPEC.md 10.1).
func Replay(runID string) (ReplayResult, error) {
	code, body, err := Post("/runs/"+runID+"/replay", nil)
	if err != nil {
		return ReplayResult{}, err
	}
	res := ReplayResult{Code: code, Body: body}
	if !res.Accepted() {
		var rej Rejection
		if json.Unmarshal(body, &rej) == nil {
			res.Rejection = &rej
		}
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Workflow files and configuration, read rather than restated
// ---------------------------------------------------------------------------

// WorkflowJSON returns one of demos/zeta's workflow files verbatim.
func WorkflowJSON(file string) ([]byte, error) {
	dir, err := DemoDir()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(dir, file))
}

// StaticSteps returns a workflow file's planner_static_steps, each element left
// as raw JSON so it can be compared against steps.decision, which SPEC.md 6.3
// stores as "the StepSpec exactly as the planner returned it".
func StaticSteps(file string) ([]json.RawMessage, error) {
	raw, err := WorkflowJSON(file)
	if err != nil {
		return nil, err
	}
	var wf struct {
		PlannerStaticSteps []json.RawMessage `json:"planner_static_steps"`
	}
	if err := json.Unmarshal(raw, &wf); err != nil {
		return nil, fmt.Errorf("%s is not parseable: %w", file, err)
	}
	if len(wf.PlannerStaticSteps) == 0 {
		return nil, fmt.Errorf("%s declares no planner_static_steps", file)
	}
	return wf.PlannerStaticSteps, nil
}

// MaxAttempts returns a workflow file's step_max_attempts, read from the file
// rather than written as a literal, so that an assertion and the workflow it is
// about cannot drift apart. SPEC.md 11.1 makes it a TOTAL attempt count, not a
// retry count.
func MaxAttempts(file string) (int, error) {
	raw, err := WorkflowJSON(file)
	if err != nil {
		return 0, err
	}
	var wf struct {
		StepMaxAttempts *int `json:"step_max_attempts"`
	}
	if err := json.Unmarshal(raw, &wf); err != nil {
		return 0, fmt.Errorf("%s is not parseable: %w", file, err)
	}
	if wf.StepMaxAttempts == nil {
		return 3, nil // SPEC.md 11.1's default
	}
	return *wf.StepMaxAttempts, nil
}

// Beta's harness reads sweep_interval_seconds and lease_ttl_seconds out of
// piton.yaml, because its waits are bounded by a lease that must expire before
// a takeover can happen. Delta and zeta have no equivalent: every wait here
// polls for the STATE it is waiting on rather than for a duration computed from
// the configuration, so there is nothing for those readers to keep in step with
// and they are deliberately absent.

// ---------------------------------------------------------------------------
// The two control calls every leg starts with
// ---------------------------------------------------------------------------

// CreateWorkflow posts one of demos/zeta's workflow files to POST /workflows
// and returns the workflow_id (SPEC.md 10.1).
func CreateWorkflow(file string) (string, error) {
	raw, err := WorkflowJSON(file)
	if err != nil {
		return "", err
	}
	code, body, err := Post("/workflows", raw)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return "", fmt.Errorf("POST /workflows returned %d for %s: %s\n"+
			"SPEC.md 16 lists every reason this is a 400; the body says which", code, file, body)
	}
	var out struct {
		WorkflowID string `json:"workflow_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("POST /workflows returned unparseable JSON: %w: %s", err, body)
	}
	if out.WorkflowID == "" {
		return "", fmt.Errorf("POST /workflows returned no workflow_id: %s", body)
	}
	return out.WorkflowID, nil
}

// CreateWorkflowFromBody posts raw bytes to POST /workflows and returns the
// created workflow_id (empty when the request was refused), the HTTP status
// code, and the raw response body.
//
// Unlike CreateWorkflow, a non-2xx is not an error here: some zeta assertions
// submit a deliberately malformed workflow and need the refusal itself - for
// example SPEC.md 16's rules 2 and 3, which refuse an http-typed workflow
// missing planner_url or fetch_base_url, or one that mixes planner_url /
// fetch_base_url with planner_static_steps. err is non-nil only for a
// transport-level problem: a request that never reached the server, or a body
// that could not be read.
func CreateWorkflowFromBody(body []byte) (string, int, []byte, error) {
	code, resp, err := Post("/workflows", body)
	if err != nil {
		return "", 0, nil, err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return "", code, resp, nil
	}
	var out struct {
		WorkflowID string `json:"workflow_id"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		// A 2xx with an unparseable body is a contract violation, not a
		// deliberate refusal - a caller asking for the refusal itself still
		// deserves an error rather than a silently empty workflow_id.
		return "", code, resp, fmt.Errorf("POST /workflows returned %d but unparseable JSON: %w: %s", code, err, resp)
	}
	return out.WorkflowID, code, resp, nil
}

// StartRun posts SPEC.md 10.1's run-creation body and returns the run_id.
//
// overrides is sent empty: SPEC.md 11.2 makes any non-empty value a 400 until
// milestone eta.
func StartRun(workflowID string) (string, error) {
	body := []byte(fmt.Sprintf(`{"input":%s,"overrides":{}}`, RunInput))
	code, resp, err := Post("/workflows/"+workflowID+"/runs", body)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return "", fmt.Errorf("POST /workflows/{id}/runs returned %d: %s", code, resp)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return "", fmt.Errorf("POST /workflows/{id}/runs returned unparseable JSON: %w: %s", err, resp)
	}
	if out.RunID == "" {
		return "", fmt.Errorf("POST /workflows/{id}/runs returned no run_id: %s", resp)
	}
	return out.RunID, nil
}

// Begin submits a workflow and starts a run from it, returning the run_id.
func Begin(file string) (string, error) {
	wf, err := CreateWorkflow(file)
	if err != nil {
		return "", err
	}
	return StartRun(wf)
}

// WaitTerminal polls runs.status until the run leaves RUNNING, and returns the
// state it reached.
//
// It polls the DATABASE, not the API: SPEC.md 17.1 makes database truth the
// interface, and SPEC.md 10.2 does not fix the JSON field name that carries a
// run's status in GET /runs/{run_id}, so polling the API here would mean
// inventing a wire contract no ruling covers. runs.status is specified, in
// SPEC.md 5.1 and 6.2.
func WaitTerminal(runID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		status, err := PSQL("SELECT status FROM runs WHERE run_id = :'run';", "run="+runID)
		if err == nil {
			// SPEC.md 5.1: DONE and CANCELLED are terminal, and DLQ is left
			// only by an explicit replay or cancel. All three end this wait.
			switch status {
			case "DONE", "DLQ", "CANCELLED":
				return status, nil
			}
			if time.Now().After(deadline) {
				return status, fmt.Errorf("run %s was still %q after %s", runID, status, timeout)
			}
		} else if time.Now().After(deadline) {
			return "", err
		}
		time.Sleep(Poll)
	}
}

// Var is the psql variable binding for a run.
func Var(runID string) string { return "run=" + runID }

// StepVar is the psql variable binding for a step name.
func StepVar(stepName string) string { return "step=" + stepName }
