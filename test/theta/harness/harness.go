// Package harness brings milestone theta's environment up and gives the
// automated suite the two interfaces SPEC.md makes authoritative: the HTTP API
// (SPEC.md 10) and database truth (SPEC.md 17.1).
//
// CLAUDE.md 5.5.4 requires the suite to reference the milestone's own compose
// file rather than define an environment of its own, so that the owner's
// hand-run demo (CLAUDE.md 4 step 5) and the suite exercise an identical
// definition. Every docker command below therefore runs demos/theta's
// docker-compose.yml, and nothing in this package writes a compose file of its
// own or edits that one.
//
// This is the fifth copy of a harness that four milestones already have.
// BACKLOG.md B18 records the duplication and R37-k records what it has already
// cost - a fix applied to one copy and not the others. It is not widened here:
// theta's copy differs from alpha's only where theta differs, in which
// environment directory it points at and in taking a workflow file by name,
// because theta submits three workflows rather than one.
//
// Nothing here was derived by reading an implementation. Raw dispatch does not
// exist at the time this is written - internal/dispatch answers it with "not
// implemented" - which is what CLAUDE.md 4 step 3 requires and what makes a
// failing run of this suite the expected state until the code lands.
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
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// BaseURL is where the orchestrator is reachable from the host: the port
// demos/theta/docker-compose.yml publishes.
const BaseURL = "http://localhost:8080"

// RunInput is the workflow input every group in this suite submits.
//
// No worker sees it. SPEC.md 9.5's envelope carries params and inputs and no
// workflow input, and a raw body carries params alone - which is one of the
// things theta asserts. It is checked where it does live: runs.input, SPEC.md
// 6.2, "stored verbatim".
const RunInput = `{"text":"hello"}`

// The workflow files demos/theta holds. Each group submits the one its scenario
// needs, by name, so that a group's subject is legible from its TestMain rather
// than hidden in a shared fixture.
const (
	// WorkflowHappy is theta's scenario: two raw steps and one envelope step
	// in a single run.
	WorkflowHappy = "workflow.json"

	// WorkflowNon2xx is one raw step against an endpoint that answers HTTP
	// 500 with an over-long body (SPEC.md 9.6, SPEC.md 6.4's 4 KB limit).
	WorkflowNon2xx = "workflow-raw-non2xx.json"

	// WorkflowNotJSON is one raw step against an endpoint that answers 200
	// with prose (SPEC.md 9.6: a raw worker's body MUST be valid JSON).
	WorkflowNotJSON = "workflow-raw-not-json.json"
)

const (
	// upTimeout is generous because the first run of a group builds the
	// orchestrator image from source.
	upTimeout = 10 * time.Minute

	// commandTimeout bounds one short docker or psql invocation.
	commandTimeout = 2 * time.Minute

	// HealthTimeout bounds the wait for GET /healthz. demos/theta has no
	// migration service, so the orchestrator applies migrations at boot and
	// binds its listener afterwards: a 200 here is what proves they finished.
	HealthTimeout = 4 * time.Minute

	// RunTimeout bounds how long a theta run may take to reach a terminal
	// state. Both workers answer in milliseconds, and the failure workflows
	// declare step_retry_delay_seconds: 0, so this is a ceiling and not an
	// expectation.
	RunTimeout = 3 * time.Minute
)

// moduleRoot walks up from this file to the directory holding go.mod.
//
// runtime.Caller reports the path this file had when it was compiled, which is
// the path it has when it runs: CLAUDE.md 8 requires the toolchain, the
// containers and the suite all to execute inside WSL, so there is no
// cross-machine build to invalidate it.
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

// DemoDir returns demos/theta, the milestone's own environment directory.
func DemoDir() (string, error) {
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "demos", "theta"), nil
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
// A previous group that failed to tear itself down therefore cannot leak state
// into this one.
func Up() error {
	ctx, cancel := context.WithTimeout(context.Background(), upTimeout)
	defer cancel()

	// Best effort: there is usually nothing to remove.
	_, _ = composeRun(ctx, "", "down", "-v", "--remove-orphans")

	out, err := composeRun(ctx, "", "up", "-d", "--build", "--wait", "--wait-timeout", "240")
	if err != nil {
		// Diagnosis first, then teardown. A half-started environment left
		// running holds the published port 8080 and would make the next group
		// - or the owner's hand-run demo - fail for a reason that has nothing
		// to do with what it was testing. The logs are captured into the error
		// before the containers go away, so nothing is lost by removing them.
		logs, _ := composeRun(ctx, "", "logs", "--tail=60", "orchestrator")
		_, _ = composeRun(ctx, "", "down", "-v", "--remove-orphans")
		return fmt.Errorf("docker compose up failed: %w\n%s\n--- orchestrator logs ---\n%s", err, out, logs)
	}
	return nil
}

// Down tears the group's environment down, including the volume wipe CLAUDE.md
// 5.5.1 requires before the next group starts.
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

// PSQL runs one SQL statement against the demo's database and returns its
// output, unaligned and with surrounding whitespace removed.
//
// It goes through "docker compose exec" rather than a Go driver because
// demos/theta/docker-compose.yml deliberately publishes no host port for
// postgres, and CLAUDE.md 5.5.4 forbids the suite from defining an environment
// of its own to obtain one. This is the same access path SPEC.md 17.1 gives the
// operator, and the one demo.sh uses.
//
// SQL is fed on stdin, never with -c: psql substitutes :'name' only for input
// it reads through its normal lexer, and with -c the placeholder would reach
// the server untouched and every query using one would fail to parse.
//
// Each vars entry is a psql variable in name=value form, referenced in SQL as
// :'name'. Values must not contain a single quote; nothing this suite passes
// does, and neither a UUID nor a compacted JSON document can.
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
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
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

// WorkflowJSON returns one of demos/theta's workflow files verbatim.
func WorkflowJSON(file string) ([]byte, error) {
	dir, err := DemoDir()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(dir, file))
}

// CreateWorkflow posts one of demos/theta's workflow files to POST /workflows
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
			"SPEC.md 16 and SPEC.md 9.8 list every reason this is a 400; the body says which",
			code, file, body)
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

// StartRun posts SPEC.md 10.1's run-creation body and returns the run_id.
//
// overrides is sent empty: SPEC.md 11.2 makes any non-empty value a 400 until
// milestone eta, and the field exists now only because the request shape is a
// contract other people build against.
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

// WaitTerminal polls runs.status until the run leaves RUNNING, and returns the
// state it reached.
//
// The wait polls the DATABASE, not the API: SPEC.md 17.1 makes database truth
// the interface, and SPEC.md 10.2 does not fix the JSON field name that carries
// a run's status in GET /runs/{run_id}, so polling the API here would mean
// inventing a wire contract no ruling covers. runs.status is specified, in
// SPEC.md 5.1 and 6.2.
func WaitTerminal(runID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		status, err := PSQL("SELECT status FROM runs WHERE run_id = :'run';", "run="+runID)
		if err != nil {
			return "", err
		}
		// SPEC.md 5.1: DONE and CANCELLED are terminal, and DLQ is left only by
		// an explicit replay or cancel. All three end this wait.
		switch status {
		case "DONE", "DLQ", "CANCELLED":
			return status, nil
		}
		if time.Now().After(deadline) {
			return status, fmt.Errorf("run %s was still %q after %s", runID, status, timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Seed submits one workflow file, starts one run, and waits for it to reach a
// terminal state.
func Seed(file string) (workflowID, runID, finalStatus string, err error) {
	if err = WaitHealthy(HealthTimeout); err != nil {
		return "", "", "", err
	}
	if workflowID, err = CreateWorkflow(file); err != nil {
		return "", "", "", err
	}
	if runID, err = StartRun(workflowID); err != nil {
		return workflowID, "", "", err
	}
	finalStatus, err = WaitTerminal(runID, RunTimeout)
	return workflowID, runID, finalStatus, err
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

// StepParams returns one static step's `params` object, compacted, as it must
// appear on the wire in raw mode: SPEC.md 9.5 makes the raw body "params,
// verbatim and nothing else".
func StepParams(file string, index int) (json.RawMessage, error) {
	specs, err := StaticSteps(file)
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(specs) {
		return nil, fmt.Errorf("%s has %d static steps; index %d is out of range",
			file, len(specs), index)
	}
	var spec struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(specs[index], &spec); err != nil {
		return nil, fmt.Errorf("static step %d of %s is not parseable: %w", index, file, err)
	}
	if len(spec.Params) == 0 {
		// SPEC.md 9.4: params is optional and defaults to {}.
		return json.RawMessage("{}"), nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, spec.Params); err != nil {
		return nil, err
	}
	return json.RawMessage(buf.Bytes()), nil
}

// ConfigSeconds reads one integer-valued key out of demos/theta/piton.yaml.
//
// The values that govern ownership are read from the file the orchestrator
// itself boots on, so an assertion and the configuration it depends on cannot
// drift apart. A regexp rather than a YAML parser, because go.mod declares no
// dependencies and one scalar does not justify the first one.
func ConfigSeconds(key string) (int, error) {
	dir, err := DemoDir()
	if err != nil {
		return 0, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "piton.yaml"))
	if err != nil {
		return 0, err
	}
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `:[ \t]*([0-9]+)`)
	m := re.FindSubmatch(raw)
	if m == nil {
		return 0, fmt.Errorf("harness: %s not found in piton.yaml", key)
	}
	return strconv.Atoi(string(m[1]))
}

// WorkflowSetting reads one integer-valued top-level key out of a workflow
// file, so that an assertion about a budget reads the number the workflow
// actually declared rather than repeating it as a literal.
func WorkflowSetting(file, key string) (int, error) {
	raw, err := WorkflowJSON(file)
	if err != nil {
		return 0, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, fmt.Errorf("%s is not parseable: %w", file, err)
	}
	v, ok := m[key]
	if !ok {
		return 0, fmt.Errorf("%s declares no %s", file, key)
	}
	var n int
	if err := json.Unmarshal(v, &n); err != nil {
		return 0, fmt.Errorf("%s in %s is not an integer: %w", key, file, err)
	}
	return n, nil
}
