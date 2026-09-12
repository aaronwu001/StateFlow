// Package harness brings the console environment up and gives the suite the
// two interfaces SPEC.md makes authoritative: the HTTP API (SPEC.md 10) and
// database truth (SPEC.md 17.1).
//
// CLAUDE.md 5.5.4 requires the suite to reference the environment's own compose
// file rather than define one of its own, so that what the owner runs by hand
// and what the suite runs are the same definition. Every docker command below
// runs demos/console/docker-compose.yml.
//
// This suite differs from every milestone suite in what it is guarding. A
// milestone suite asserts a behaviour of the engine. This one asserts a WIRE
// CONTRACT: that GET /runs and GET /runs/{run_id}/dlq return what SPEC.md 10.2
// says, in the shape API.md publishes. Where a field's meaning is in question
// the assertion cites SPEC.md, because API.md is authority on the wire only and
// SPEC.md wins wherever the two could disagree (CLAUDE.md 1).
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

// Where the three published ports are reachable from the host.
const (
	BaseURL    = "http://localhost:8080"
	WorkerURL  = "http://localhost:9090"
	PlannerURL = "http://localhost:9100"
)

const (
	upTimeout      = 10 * time.Minute
	commandTimeout = 2 * time.Minute

	// HealthTimeout bounds the wait for GET /healthz. There is no migration
	// service, so a 200 is what proves migrations finished.
	HealthTimeout = 4 * time.Minute

	// RunTimeout bounds one run. demos/console/workflow.json sets
	// step_timeout_seconds and planner_timeout_seconds to 5 with two attempts
	// each, so even a run that dies entirely on timeouts finishes well inside
	// this.
	RunTimeout = 2 * time.Minute
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

// DemoDir returns demos/console, the environment's own directory.
func DemoDir() (string, error) {
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "demos", "console"), nil
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

// Up starts the environment from a clean database (CLAUDE.md 5.5.2).
func Up() error {
	ctx, cancel := context.WithTimeout(context.Background(), upTimeout)
	defer cancel()

	_, _ = composeRun(ctx, "", "down", "-v", "--remove-orphans")

	out, err := composeRun(ctx, "", "up", "-d", "--build", "--wait", "--wait-timeout", "240")
	if err != nil {
		logs, _ := composeRun(ctx, "", "logs", "--tail=60", "orchestrator")
		_, _ = composeRun(ctx, "", "down", "-v", "--remove-orphans")
		return fmt.Errorf("docker compose up failed: %w\n%s\n--- orchestrator logs ---\n%s", err, out, logs)
	}
	return nil
}

// Down tears it down, volume wipe included (CLAUDE.md 5.5.1).
func Down() error {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := composeRun(ctx, "", "down", "-v", "--remove-orphans")
	if err != nil {
		return fmt.Errorf("docker compose down -v failed: %w\n%s", err, out)
	}
	return nil
}

// Logs returns the tail of one service's log, for diagnosis only. SPEC.md 17.3
// keeps error text in the database; logs are the second resort.
func Logs(service string, lines int) string {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, _ := composeRun(ctx, "", "logs", fmt.Sprintf("--tail=%d", lines), service)
	return out
}

// PSQL runs one statement against the demo's database, unaligned.
//
// It goes through `docker compose exec` because the compose file publishes no
// host port for postgres. This is SPEC.md 17.1's own access path.
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

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func do(method, url string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
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

// Get issues a GET against the orchestrator and returns status and body.
func Get(path string) (int, []byte, error) { return do(http.MethodGet, BaseURL+path, nil) }

// Post issues a JSON POST against the orchestrator.
func Post(path string, body []byte) (int, []byte, error) {
	return do(http.MethodPost, BaseURL+path, body)
}

// GetJSON fetches a path and decodes it into v, failing if the status is not
// 200 so that a test never asserts against an error body by accident.
func GetJSON(path string, v any) error {
	code, body, err := Get(path)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("GET %s returned %d: %s", path, code, body)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("GET %s returned unparseable JSON: %w: %s", path, err, body)
	}
	return nil
}

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
// The pause switches
// ---------------------------------------------------------------------------

func switchTo(base, action, runID string) error {
	url := base + action
	if runID != "" {
		url += "?run=" + runID
	}
	code, body, err := do(http.MethodPost, url, []byte("{}"))
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("POST %s returned %d: %s", url, code, body)
	}
	return nil
}

// PauseWorker makes the worker hold its connections open and answer nothing, so
// that attempts expire at attempts.deadline_at and are classified `timeout`
// (SPEC.md 5.3). With no run named it applies to every run.
func PauseWorker() error { return switchTo(WorkerURL, "/pause", "") }

// ResumeWorker undoes it, for every run.
func ResumeWorker() error { return switchTo(WorkerURL, "/resume", "") }

// PauseWorkerRun silences the worker for ONE run and leaves every other run
// running normally. SPEC.md 9.5's envelope carries run_id, which is what makes
// this possible without Piton knowing anything about it.
func PauseWorkerRun(runID string) error { return switchTo(WorkerURL, "/pause", runID) }

// ResumeWorkerRun undoes it for that run.
func ResumeWorkerRun(runID string) error { return switchTo(WorkerURL, "/resume", runID) }

// PausePlanner does the same to the planner, which is what produces a
// planner-side dead-letter entry (SPEC.md 12.3).
func PausePlanner() error { return switchTo(PlannerURL, "/pause", "") }

// ResumePlanner undoes it.
func ResumePlanner() error { return switchTo(PlannerURL, "/resume", "") }

// PausePlannerRun silences the planner for one run only. SPEC.md 9.2 puts
// run_id in every planner request.
func PausePlannerRun(runID string) error { return switchTo(PlannerURL, "/pause", runID) }

// ResumePlannerRun undoes it for that run.
func ResumePlannerRun(runID string) error { return switchTo(PlannerURL, "/resume", runID) }

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// WorkflowJSON returns demos/console/workflow.json verbatim.
func WorkflowJSON() ([]byte, error) {
	dir, err := DemoDir()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(dir, "workflow.json"))
}

// CreateWorkflow posts it and returns the workflow_id (SPEC.md 10.1).
func CreateWorkflow() (string, error) {
	raw, err := WorkflowJSON()
	if err != nil {
		return "", err
	}
	code, body, err := Post("/workflows", raw)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return "", fmt.Errorf("POST /workflows returned %d: %s", code, body)
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

// StartRun starts a run whose input is the given JSON document.
//
// The console planner decides from that input: `steps` says how many steps to
// produce and `worker` becomes each step's params. One workflow definition
// therefore drives every scenario, which is what lets this suite - and a front
// end - vary a run without submitting a new workflow.
func StartRun(workflowID, input string) (string, error) {
	body := []byte(fmt.Sprintf(`{"input":%s,"overrides":{}}`, input))
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
		return "", fmt.Errorf("run creation returned unparseable JSON: %w: %s", err, resp)
	}
	if out.RunID == "" {
		return "", fmt.Errorf("run creation returned no run_id: %s", resp)
	}
	return out.RunID, nil
}

// WaitTerminal polls runs.status until the run leaves RUNNING.
//
// It polls the DATABASE rather than the API, so that a defect in the read
// endpoints this suite is testing cannot also decide when the suite stops
// waiting.
func WaitTerminal(runID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		status, err := PSQL("SELECT status FROM runs WHERE run_id = :'run';", "run="+runID)
		if err != nil {
			return "", err
		}
		switch status {
		case "DONE", "DLQ", "CANCELLED":
			return status, nil
		}
		if time.Now().After(deadline) {
			return status, fmt.Errorf("run %s was still %q after %s", runID, status, timeout)
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// Replay posts SPEC.md 10.1's replay and returns the new replay_count.
func Replay(runID string) (int, error) {
	code, body, err := Post("/runs/"+runID+"/replay", []byte("{}"))
	if err != nil {
		return 0, err
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("POST /runs/{id}/replay returned %d: %s", code, body)
	}
	var out struct {
		ReplayCount int `json:"replay_count"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("replay returned unparseable JSON: %w: %s", err, body)
	}
	return out.ReplayCount, nil
}
