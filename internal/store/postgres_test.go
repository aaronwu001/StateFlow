package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/store"
	"github.com/aaronwu000/stateflow/migrations"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// openDB opens a *sql.DB from TEST_DATABASE_URL, or skips the test if not set.
// Each call opens an independent connection pool — used deliberately in the
// TX3 atomicity test to observe state from a second, unrelated connection.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping postgres integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("db.Ping: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// resetSchema drops all StateFlow tables (if they exist) — including
// golang-migrate's own "schema_migrations" version-tracking table, so
// migrations.Apply below cannot see a stale "already at the latest version"
// bookkeeping row and skip re-running 000001_initial.up.sql — then
// re-applies the migrations via the exact same migrations.Apply function
// production startup uses (cmd/stateflow/main.go). Keeps the production DDL
// pure; test isolation is the test's job.
func resetSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, tbl := range []string{"dead_letter_queue", "attempts", "steps", "runs", "workflows", "schema_migrations"} {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + tbl + " CASCADE"); err != nil {
			t.Fatalf("drop table %s: %v", tbl, err)
		}
	}
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatalf("resetSchema: TEST_DATABASE_URL not set (should have been caught by openDB's skip already)")
	}
	if err := migrations.Apply(dsn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
}

// newTestWorkflow creates a workflow with the given retry limit encoded into
// planner_config under the confirmed key "retry_limit" (Session 2 CONFIRM).
func newTestWorkflow(t *testing.T, ctx context.Context, s *store.PostgresStore, retryLimit int) core.WorkflowID {
	t.Helper()
	cfg, _ := json.Marshal(map[string]any{"retry_limit": retryLimit})
	id, err := s.CreateWorkflow(ctx, core.WorkflowDef{
		Name:          "test-workflow",
		PlannerType:   "static",
		PlannerConfig: cfg,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	return id
}

func newTestRun(t *testing.T, ctx context.Context, s *store.PostgresStore, wf core.WorkflowID) core.RunID {
	t.Helper()
	id, err := s.CreateRun(ctx, wf, json.RawMessage(`{"doc":"test.pdf"}`))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return id
}

func testStepSpec(name string) core.StepSpec {
	return core.StepSpec{
		Name:           name,
		WorkerURL:      "http://worker/run",
		Mode:           core.StepModeSync,
		TimeoutSeconds: 30,
		Input:          json.RawMessage(`{"x":1}`),
	}
}

// ── (a) Barrier ordering ────────────────────────────────────────────────

// TestBarrierOrdering proves TX1's commit is visible before any dispatch
// simulation, and TX2's commit is visible before the next planner decision —
// the two write barriers (whitepaper §19).
func TestBarrierOrdering(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)
	ctx := context.Background()
	s := store.New(db)

	wf := newTestWorkflow(t, ctx, s, 3)
	run := newTestRun(t, ctx, s, wf)

	spec := testStepSpec("ocr")
	stepID, attemptID, err := s.CreateStepWithAttempt(ctx, run, spec)
	if err != nil {
		t.Fatalf("CreateStepWithAttempt: %v", err)
	}

	// TX1 must already be durably visible — we deliberately never call
	// Dispatch in this test; we only prove the row is there via a direct read.
	var stepStatus string
	var decisionJSON []byte
	if err := db.QueryRowContext(ctx, `SELECT status, decision FROM steps WHERE step_id=$1`, string(stepID)).
		Scan(&stepStatus, &decisionJSON); err != nil {
		t.Fatalf("read step after TX1: %v", err)
	}
	if stepStatus != "RUNNING" {
		t.Fatalf("FAIL — step status = %q, want RUNNING", stepStatus)
	}
	var gotSpec core.StepSpec
	if err := json.Unmarshal(decisionJSON, &gotSpec); err != nil {
		t.Fatalf("unmarshal decision: %v", err)
	}
	if gotSpec.Name != spec.Name {
		t.Fatalf("FAIL — decision.Name = %q, want %q", gotSpec.Name, spec.Name)
	}

	var attemptStatus string
	if err := db.QueryRowContext(ctx, `SELECT status FROM attempts WHERE attempt_id=$1::uuid`, string(attemptID)).
		Scan(&attemptStatus); err != nil {
		t.Fatalf("read attempt after TX1: %v", err)
	}
	if attemptStatus != "RUNNING" {
		t.Fatalf("FAIL — attempt status = %q, want RUNNING", attemptStatus)
	}

	var currentAttemptID sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT current_attempt_id::text FROM steps WHERE step_id=$1`, string(stepID)).
		Scan(&currentAttemptID); err != nil {
		t.Fatalf("read current_attempt_id: %v", err)
	}
	if !currentAttemptID.Valid || currentAttemptID.String != string(attemptID) {
		t.Fatalf("FAIL — current_attempt_id = %v, want %q", currentAttemptID, attemptID)
	}
	t.Log("PASS — TX1 committed and visible: step RUNNING, decision persisted, attempt RUNNING, current_attempt_id set")

	// TX2 must be visible before the next planner decision: immediately after
	// CheckpointSuccess, LoadFrontier must show no pending step.
	outcome, err := s.CheckpointSuccess(ctx, stepID, attemptID, json.RawMessage(`{"pages":3}`))
	if err != nil {
		t.Fatalf("CheckpointSuccess: %v", err)
	}
	if outcome != core.ReportCommitted {
		t.Fatalf("FAIL — CheckpointSuccess outcome = %v, want ReportCommitted", outcome)
	}

	frontier, err := s.LoadFrontier(ctx, run)
	if err != nil {
		t.Fatalf("LoadFrontier: %v", err)
	}
	if frontier.PendingStep != nil {
		t.Fatalf("FAIL — PendingStep = %+v, want nil (Barrier 2 must be visible before the next decision)", frontier.PendingStep)
	}
	if len(frontier.History) != 1 || frontier.History[0].Name != "ocr" {
		t.Fatalf("FAIL — History = %+v, want one entry named ocr", frontier.History)
	}
	t.Log("PASS — TX2 committed and visible: LoadFrontier shows the step DONE with no pending decision")
}

// ── (b) TX3 same-blade atomicity ────────────────────────────────────────

// TestTX3_SameBladeAtomicity drives a step to exactly retryLimit-1 failures,
// then issues the final RecordFailure that must land attempt=FAILED +
// count=retryLimit + step=DLQ + run=DLQ + one DLQ row atomically. A second,
// independent connection polls concurrently and must never observe a
// partial mix of pre- and post-state.
func TestTX3_SameBladeAtomicity(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)
	ctx := context.Background()
	s := store.New(db)

	const retryLimit = 3
	wf := newTestWorkflow(t, ctx, s, retryLimit)
	run := newTestRun(t, ctx, s, wf)

	stepID, attemptID, err := s.CreateStepWithAttempt(ctx, run, testStepSpec("flaky"))
	if err != nil {
		t.Fatalf("CreateStepWithAttempt: %v", err)
	}

	curAttempt := attemptID
	for i := 0; i < retryLimit-1; i++ {
		outcome, err := s.RecordFailure(ctx, stepID, curAttempt, core.FailureReasonWorkerReported, "fail", retryLimit)
		if err != nil {
			t.Fatalf("RecordFailure (warmup %d): %v", i, err)
		}
		if outcome.Report != core.ReportCommitted || outcome.DLQed {
			t.Fatalf("FAIL — warmup RecordFailure outcome = %+v, want Committed/not-DLQed", outcome)
		}
		curAttempt, err = s.StartNewAttempt(ctx, stepID)
		if err != nil {
			t.Fatalf("StartNewAttempt (warmup %d): %v", i, err)
		}
	}

	var countBefore int
	if err := db.QueryRowContext(ctx, `SELECT attempt_count FROM steps WHERE step_id=$1`, string(stepID)).
		Scan(&countBefore); err != nil {
		t.Fatalf("read attempt_count: %v", err)
	}
	if countBefore != retryLimit-1 {
		t.Fatalf("FAIL — attempt_count = %d, want %d before the blade fires", countBefore, retryLimit-1)
	}

	// Second, independent connection for concurrent observation.
	db2 := openDB(t)

	type snapshot struct {
		attemptStatus string
		stepStatus    string
		runStatus     string
		dlqCount      int
	}
	// A single SELECT statement is one atomic snapshot under Postgres's
	// statement-level consistency — four separate SELECTs would not be:
	// the write transaction could commit between them, producing a "torn
	// read" across the observer's own queries even though the underlying
	// transaction was atomic. This is what actually makes the test valid.
	observe := func() snapshot {
		var snap snapshot
		_ = db2.QueryRowContext(ctx, `
			SELECT
				(SELECT status FROM attempts WHERE attempt_id = $1::uuid),
				(SELECT status FROM steps WHERE step_id = $2),
				(SELECT status FROM runs WHERE run_id = $3),
				(SELECT count(*) FROM dead_letter_queue WHERE run_id = $3)
		`, string(curAttempt), string(stepID), string(run)).
			Scan(&snap.attemptStatus, &snap.stepStatus, &snap.runStatus, &snap.dlqCount)
		return snap
	}

	var observed []snapshot
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				observed = append(observed, observe())
			}
		}
	}()

	outcome, err := s.RecordFailure(ctx, stepID, curAttempt, core.FailureReasonTimeout, "final fail", retryLimit)
	close(stop)
	<-done

	if err != nil {
		t.Fatalf("RecordFailure (blade): %v", err)
	}
	if outcome.Report != core.ReportCommitted {
		t.Fatalf("FAIL — Report = %v, want ReportCommitted", outcome.Report)
	}
	if !outcome.DLQed {
		t.Fatalf("FAIL — DLQed = false, want true")
	}

	var finalAttemptStatus, finalStepStatus, finalRunStatus string
	var finalCount, dlqRows int
	_ = db.QueryRowContext(ctx, `SELECT status FROM attempts WHERE attempt_id=$1::uuid`, string(curAttempt)).Scan(&finalAttemptStatus)
	_ = db.QueryRowContext(ctx, `SELECT status, attempt_count FROM steps WHERE step_id=$1`, string(stepID)).Scan(&finalStepStatus, &finalCount)
	_ = db.QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id=$1`, string(run)).Scan(&finalRunStatus)
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM dead_letter_queue WHERE run_id=$1`, string(run)).Scan(&dlqRows)

	if finalAttemptStatus != "FAILED" {
		t.Errorf("FAIL — final attempt status = %q, want FAILED", finalAttemptStatus)
	}
	if finalStepStatus != "DLQ" {
		t.Errorf("FAIL — final step status = %q, want DLQ", finalStepStatus)
	}
	if finalRunStatus != "DLQ" {
		t.Errorf("FAIL — final run status = %q, want DLQ", finalRunStatus)
	}
	if finalCount != retryLimit {
		t.Errorf("FAIL — final attempt_count = %d, want %d", finalCount, retryLimit)
	}
	if dlqRows != 1 {
		t.Errorf("FAIL — dlq row count = %d, want 1", dlqRows)
	}

	for i, snap := range observed {
		pre := snap.attemptStatus == "RUNNING" && snap.stepStatus == "RUNNING" && snap.runStatus == "RUNNING" && snap.dlqCount == 0
		post := snap.attemptStatus == "FAILED" && snap.stepStatus == "DLQ" && snap.runStatus == "DLQ" && snap.dlqCount == 1
		if !pre && !post {
			t.Fatalf("FAIL — observed partial state at snapshot %d: %+v (neither fully-pre nor fully-post — TX3 is not atomic)", i, snap)
		}
	}
	t.Logf("PASS — TX3 same-blade atomicity: observed %d snapshots from a second connection, all fully-pre or fully-post, never partial", len(observed))
}

// ── (c) TX5 reset ────────────────────────────────────────────────────────

// TestReplayWorkerSide_ResetsForRetry drives a step to DLQ, then verifies
// ReplayWorkerSide performs all five writes: attempt_count→0, step→RUNNING,
// run→RUNNING, a new attempt (RUNNING), current_attempt_id set.
func TestReplayWorkerSide_ResetsForRetry(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)
	ctx := context.Background()
	s := store.New(db)

	const retryLimit = 2
	wf := newTestWorkflow(t, ctx, s, retryLimit)
	run := newTestRun(t, ctx, s, wf)

	stepID, attempt1, err := s.CreateStepWithAttempt(ctx, run, testStepSpec("scrape"))
	if err != nil {
		t.Fatalf("CreateStepWithAttempt: %v", err)
	}

	outcome1, err := s.RecordFailure(ctx, stepID, attempt1, core.FailureReasonWorkerReported, "fail 1", retryLimit)
	if err != nil {
		t.Fatalf("RecordFailure 1: %v", err)
	}
	if outcome1.DLQed {
		t.Fatalf("unexpectedly DLQ'd after first failure (retryLimit=%d)", retryLimit)
	}
	attempt2, err := s.StartNewAttempt(ctx, stepID)
	if err != nil {
		t.Fatalf("StartNewAttempt: %v", err)
	}
	outcome2, err := s.RecordFailure(ctx, stepID, attempt2, core.FailureReasonTimeout, "fail 2", retryLimit)
	if err != nil {
		t.Fatalf("RecordFailure 2: %v", err)
	}
	if !outcome2.DLQed {
		t.Fatalf("expected DLQ after reaching retryLimit=%d", retryLimit)
	}

	var runStatus, stepStatus string
	var count int
	_ = db.QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id=$1`, string(run)).Scan(&runStatus)
	_ = db.QueryRowContext(ctx, `SELECT status, attempt_count FROM steps WHERE step_id=$1`, string(stepID)).Scan(&stepStatus, &count)
	if runStatus != "DLQ" || stepStatus != "DLQ" || count != retryLimit {
		t.Fatalf("pre-replay state = run:%s step:%s count:%d, want DLQ/DLQ/%d", runStatus, stepStatus, count, retryLimit)
	}

	replayedStep, newAttempt, err := s.ReplayWorkerSide(ctx, run)
	if err != nil {
		t.Fatalf("ReplayWorkerSide: %v", err)
	}
	if replayedStep != stepID {
		t.Fatalf("FAIL — replayed step = %q, want %q", replayedStep, stepID)
	}
	if newAttempt == attempt2 {
		t.Fatalf("FAIL — expected a fresh attempt id, got the old one")
	}

	var newStepStatus, newRunStatus, currentAttemptID string
	var newCount int
	_ = db.QueryRowContext(ctx, `SELECT status, attempt_count, current_attempt_id::text FROM steps WHERE step_id=$1`, string(stepID)).
		Scan(&newStepStatus, &newCount, &currentAttemptID)
	_ = db.QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id=$1`, string(run)).Scan(&newRunStatus)

	if newStepStatus != "RUNNING" {
		t.Errorf("FAIL — step status = %q, want RUNNING", newStepStatus)
	}
	if newCount != 0 {
		t.Errorf("FAIL — attempt_count = %d, want 0", newCount)
	}
	if newRunStatus != "RUNNING" {
		t.Errorf("FAIL — run status = %q, want RUNNING", newRunStatus)
	}
	if currentAttemptID != string(newAttempt) {
		t.Errorf("FAIL — current_attempt_id = %q, want %q", currentAttemptID, newAttempt)
	}

	var newAttemptStatus string
	_ = db.QueryRowContext(ctx, `SELECT status FROM attempts WHERE attempt_id=$1::uuid`, string(newAttempt)).Scan(&newAttemptStatus)
	if newAttemptStatus != "RUNNING" {
		t.Errorf("FAIL — new attempt status = %q, want RUNNING", newAttemptStatus)
	}

	t.Log("PASS — ReplayWorkerSide: attempt_count=0, step=RUNNING, run=RUNNING, new attempt=RUNNING, current_attempt_id updated")
}

// ── (d) CAS superseded is a no-op ───────────────────────────────────────

// TestCAS_SupersededReport_NoOp verifies that a report whose attempt is no
// longer current/RUNNING is a silent no-op returning ReportSuperseded, never
// an error, with zero state effect.
func TestCAS_SupersededReport_NoOp(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)
	ctx := context.Background()
	s := store.New(db)

	wf := newTestWorkflow(t, ctx, s, 3)
	run := newTestRun(t, ctx, s, wf)

	stepID, attempt1, err := s.CreateStepWithAttempt(ctx, run, testStepSpec("once"))
	if err != nil {
		t.Fatalf("CreateStepWithAttempt: %v", err)
	}

	outcome1, err := s.CheckpointSuccess(ctx, stepID, attempt1, json.RawMessage(`{"result":1}`))
	if err != nil {
		t.Fatalf("CheckpointSuccess 1: %v", err)
	}
	if outcome1 != core.ReportCommitted {
		t.Fatalf("FAIL — first checkpoint outcome = %v, want ReportCommitted", outcome1)
	}

	// Duplicate success report on the now-DONE attempt must be a no-op.
	outcome2, err := s.CheckpointSuccess(ctx, stepID, attempt1, json.RawMessage(`{"result":2}`))
	if err != nil {
		t.Fatalf("CheckpointSuccess 2: %v", err)
	}
	if outcome2 != core.ReportSuperseded {
		t.Fatalf("FAIL — duplicate checkpoint outcome = %v, want ReportSuperseded", outcome2)
	}

	var outputJSON []byte
	_ = db.QueryRowContext(ctx, `SELECT output FROM steps WHERE step_id=$1`, string(stepID)).Scan(&outputJSON)
	var out map[string]int
	if err := json.Unmarshal(outputJSON, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out["result"] != 1 {
		t.Fatalf("FAIL — output = %v, want result=1 (the duplicate report must not overwrite state)", out)
	}

	// A late failure report on the same (already-DONE) attempt is also superseded.
	failOutcome, err := s.RecordFailure(ctx, stepID, attempt1, core.FailureReasonTimeout, "late timeout", 3)
	if err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if failOutcome.Report != core.ReportSuperseded {
		t.Fatalf("FAIL — late failure report = %+v, want ReportSuperseded", failOutcome)
	}

	// Stale-attempt scenario: a report against a superseded (no-longer-current) attempt.
	stepID2, attemptA, err := s.CreateStepWithAttempt(ctx, run, testStepSpec("retry-me"))
	if err != nil {
		t.Fatalf("CreateStepWithAttempt 2: %v", err)
	}
	if _, err := s.RecordFailure(ctx, stepID2, attemptA, core.FailureReasonWorkerReported, "first try failed", 3); err != nil {
		t.Fatalf("RecordFailure attemptA: %v", err)
	}
	attemptB, err := s.StartNewAttempt(ctx, stepID2)
	if err != nil {
		t.Fatalf("StartNewAttempt: %v", err)
	}
	if attemptB == attemptA {
		t.Fatalf("FAIL — expected a new attempt id")
	}

	staleOutcome, err := s.CheckpointSuccess(ctx, stepID2, attemptA, json.RawMessage(`{"late":true}`))
	if err != nil {
		t.Fatalf("CheckpointSuccess (stale): %v", err)
	}
	if staleOutcome != core.ReportSuperseded {
		t.Fatalf("FAIL — stale-attempt checkpoint = %v, want ReportSuperseded", staleOutcome)
	}

	liveOutcome, err := s.CheckpointSuccess(ctx, stepID2, attemptB, json.RawMessage(`{"ok":true}`))
	if err != nil {
		t.Fatalf("CheckpointSuccess (live): %v", err)
	}
	if liveOutcome != core.ReportCommitted {
		t.Fatalf("FAIL — live-attempt checkpoint = %v, want ReportCommitted", liveOutcome)
	}

	t.Log("PASS — CAS superseded reports are silent no-ops: duplicate success, late failure, and stale-attempt success all rejected with zero state effect")
}

// ── (e) status/output divergence impossible ─────────────────────────────

// TestInvariant_DoneStatusImpliesOutputNotNull proves that after
// CheckpointSuccess, status='DONE' if and only if output IS NOT NULL — no
// divergence is possible, regardless of what output payload was checkpointed.
func TestInvariant_DoneStatusImpliesOutputNotNull(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)
	ctx := context.Background()
	s := store.New(db)

	wf := newTestWorkflow(t, ctx, s, 3)
	run := newTestRun(t, ctx, s, wf)

	assertInvariant := func(stepID core.StepID) {
		t.Helper()
		var holds bool
		if err := db.QueryRowContext(ctx, `SELECT (status = 'DONE') = (output IS NOT NULL) FROM steps WHERE step_id=$1`, string(stepID)).
			Scan(&holds); err != nil {
			t.Fatalf("assertInvariant query: %v", err)
		}
		if !holds {
			t.Fatalf("FAIL — invariant violated for step %q: status='DONE' does not match output-not-null", stepID)
		}
	}

	cases := []struct {
		name   string
		output json.RawMessage
	}{
		{"with-output", json.RawMessage(`{"a":1}`)},
		{"empty-output", json.RawMessage(``)},
		{"null-output", json.RawMessage(`null`)},
	}

	for _, c := range cases {
		stepID, attemptID, err := s.CreateStepWithAttempt(ctx, run, testStepSpec(c.name))
		if err != nil {
			t.Fatalf("CreateStepWithAttempt %s: %v", c.name, err)
		}
		assertInvariant(stepID) // pre-checkpoint: RUNNING + output NULL, both sides false

		if _, err := s.CheckpointSuccess(ctx, stepID, attemptID, c.output); err != nil {
			t.Fatalf("CheckpointSuccess %s: %v", c.name, err)
		}
		assertInvariant(stepID)

		var status string
		var outputIsNull bool
		_ = db.QueryRowContext(ctx, `SELECT status, output IS NULL FROM steps WHERE step_id=$1`, string(stepID)).Scan(&status, &outputIsNull)
		if status != "DONE" || outputIsNull {
			t.Fatalf("FAIL — case %s: status=%s outputIsNull=%v, want DONE and output present", c.name, status, outputIsNull)
		}
	}
	t.Log("PASS — status='DONE' <=> output IS NOT NULL holds for every checkpointed output shape, including empty and JSON null")
}

// ── Additional coverage: remaining TX methods and reads ─────────────────

func TestCreateWorkflow_GetWorkflow_RoundTrip(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)
	ctx := context.Background()
	s := store.New(db)

	cfg, _ := json.Marshal(map[string]any{
		"url":                     "http://planner/decide",
		"retry_limit":             5,
		"default_timeout_seconds": 45,
	})
	id, err := s.CreateWorkflow(ctx, core.WorkflowDef{
		Name:          "my-pipeline",
		PlannerType:   "http",
		PlannerConfig: cfg,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	if id == "" {
		t.Fatalf("FAIL — empty workflow id")
	}

	def, err := s.GetWorkflow(ctx, id)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if def.Name != "my-pipeline" || def.PlannerType != "http" {
		t.Fatalf("FAIL — def = %+v", def)
	}
	if def.RetryLimit != 5 {
		t.Fatalf("FAIL — RetryLimit = %d, want 5", def.RetryLimit)
	}
	if def.DefaultTimeoutSeconds != 45 {
		t.Fatalf("FAIL — DefaultTimeoutSeconds = %d, want 45", def.DefaultTimeoutSeconds)
	}
}

func TestCreateRun_ListRunningRuns(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)
	ctx := context.Background()
	s := store.New(db)

	wf := newTestWorkflow(t, ctx, s, 3)
	run, err := s.CreateRun(ctx, wf, json.RawMessage(`{"doc":"a.pdf"}`))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	refs, err := s.ListRunningRuns(ctx)
	if err != nil {
		t.Fatalf("ListRunningRuns: %v", err)
	}
	var found *core.RunRef
	for i := range refs {
		if refs[i].RunID == run {
			found = &refs[i]
		}
	}
	if found == nil {
		t.Fatalf("FAIL — run %q not in ListRunningRuns", run)
	}
	if found.WorkflowID != wf {
		t.Fatalf("FAIL — WorkflowID = %q, want %q", found.WorkflowID, wf)
	}
}

func TestRecordFailure_IncrementsWithoutReachingLimit(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)
	ctx := context.Background()
	s := store.New(db)

	const retryLimit = 5
	wf := newTestWorkflow(t, ctx, s, retryLimit)
	run := newTestRun(t, ctx, s, wf)
	stepID, attempt, err := s.CreateStepWithAttempt(ctx, run, testStepSpec("flaky"))
	if err != nil {
		t.Fatalf("CreateStepWithAttempt: %v", err)
	}

	outcome, err := s.RecordFailure(ctx, stepID, attempt, core.FailureReasonMalformed, "bad json", retryLimit)
	if err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if outcome.Report != core.ReportCommitted {
		t.Fatalf("FAIL — Report = %v, want ReportCommitted", outcome.Report)
	}
	if outcome.DLQed {
		t.Fatalf("FAIL — DLQed = true, want false (count 1 < limit %d)", retryLimit)
	}

	var count int
	var stepStatus, runStatus string
	_ = db.QueryRowContext(ctx, `SELECT attempt_count, status FROM steps WHERE step_id=$1`, string(stepID)).Scan(&count, &stepStatus)
	_ = db.QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id=$1`, string(run)).Scan(&runStatus)
	if count != 1 {
		t.Fatalf("FAIL — attempt_count = %d, want 1", count)
	}
	if stepStatus != "RUNNING" {
		t.Fatalf("FAIL — step status = %q, want RUNNING", stepStatus)
	}
	if runStatus != "RUNNING" {
		t.Fatalf("FAIL — run status = %q, want RUNNING", runStatus)
	}

	var dlqCount int
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM dead_letter_queue WHERE run_id=$1`, string(run)).Scan(&dlqCount)
	if dlqCount != 0 {
		t.Fatalf("FAIL — dlq row count = %d, want 0", dlqCount)
	}
}

func TestReplayPlannerSide(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)
	ctx := context.Background()
	s := store.New(db)

	wf := newTestWorkflow(t, ctx, s, 3)
	run := newTestRun(t, ctx, s, wf)

	if err := s.MarkRunDLQPlannerDeclared(ctx, run, "planner said fail"); err != nil {
		t.Fatalf("MarkRunDLQPlannerDeclared: %v", err)
	}

	var status string
	_ = db.QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id=$1`, string(run)).Scan(&status)
	if status != "DLQ" {
		t.Fatalf("FAIL — status = %q, want DLQ", status)
	}

	if err := s.ReplayPlannerSide(ctx, run); err != nil {
		t.Fatalf("ReplayPlannerSide: %v", err)
	}

	_ = db.QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id=$1`, string(run)).Scan(&status)
	if status != "RUNNING" {
		t.Fatalf("FAIL — status = %q, want RUNNING after ReplayPlannerSide", status)
	}
}

func TestMarkRunDone(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)
	ctx := context.Background()
	s := store.New(db)
	wf := newTestWorkflow(t, ctx, s, 3)
	run := newTestRun(t, ctx, s, wf)

	if err := s.MarkRunDone(ctx, run); err != nil {
		t.Fatalf("MarkRunDone: %v", err)
	}
	var status string
	_ = db.QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id=$1`, string(run)).Scan(&status)
	if status != "DONE" {
		t.Fatalf("FAIL — status = %q, want DONE", status)
	}
}

func TestMarkRunDLQPlannerDeclared(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)
	ctx := context.Background()
	s := store.New(db)
	wf := newTestWorkflow(t, ctx, s, 3)
	run := newTestRun(t, ctx, s, wf)

	if err := s.MarkRunDLQPlannerDeclared(ctx, run, "the planner declared fail"); err != nil {
		t.Fatalf("MarkRunDLQPlannerDeclared: %v", err)
	}

	var runStatus string
	_ = db.QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id=$1`, string(run)).Scan(&runStatus)
	if runStatus != "DLQ" {
		t.Fatalf("FAIL — run status = %q, want DLQ", runStatus)
	}

	entries, err := s.ListDLQ(ctx)
	if err != nil {
		t.Fatalf("ListDLQ: %v", err)
	}
	var found *core.DLQEntry
	for i := range entries {
		if entries[i].RunID == run {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatalf("FAIL — no dlq entry for run %q", run)
	}
	if found.Reason != core.DLQReasonPlannerDeclaredFail {
		t.Fatalf("FAIL — reason = %q, want %q", found.Reason, core.DLQReasonPlannerDeclaredFail)
	}
	if found.StepID != nil {
		t.Fatalf("FAIL — StepID = %v, want nil for a planner-side entry", *found.StepID)
	}
}

func TestMarkRunDLQPlannerExhausted(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)
	ctx := context.Background()
	s := store.New(db)
	wf := newTestWorkflow(t, ctx, s, 3)
	run := newTestRun(t, ctx, s, wf)

	if err := s.MarkRunDLQPlannerExhausted(ctx, run, core.DLQReasonPlannerUnreachable, "3 attempts, all timed out"); err != nil {
		t.Fatalf("MarkRunDLQPlannerExhausted: %v", err)
	}

	entries, err := s.ListDLQ(ctx)
	if err != nil {
		t.Fatalf("ListDLQ: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("FAIL — len(entries) = %d, want 1", len(entries))
	}
	if entries[0].Reason != core.DLQReasonPlannerUnreachable {
		t.Fatalf("FAIL — reason = %q, want %q", entries[0].Reason, core.DLQReasonPlannerUnreachable)
	}
	if entries[0].StepID != nil {
		t.Fatalf("FAIL — StepID = %v, want nil for a planner-side entry", *entries[0].StepID)
	}

	var runStatus string
	_ = db.QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id=$1`, string(run)).Scan(&runStatus)
	if runStatus != "DLQ" {
		t.Fatalf("FAIL — run status = %q, want DLQ", runStatus)
	}
}

func TestListDLQ_And_GetDLQEntry(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)
	ctx := context.Background()
	s := store.New(db)
	wf := newTestWorkflow(t, ctx, s, 3)
	run := newTestRun(t, ctx, s, wf)

	if err := s.MarkRunDLQPlannerDeclared(ctx, run, "detail-x"); err != nil {
		t.Fatalf("MarkRunDLQPlannerDeclared: %v", err)
	}

	entries, err := s.ListDLQ(ctx)
	if err != nil {
		t.Fatalf("ListDLQ: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("FAIL — len(entries) = %d, want 1", len(entries))
	}

	got, err := s.GetDLQEntry(ctx, entries[0].ID)
	if err != nil {
		t.Fatalf("GetDLQEntry: %v", err)
	}
	if got.RunID != run {
		t.Fatalf("FAIL — RunID = %q, want %q", got.RunID, run)
	}
	if got.Reason != core.DLQReasonPlannerDeclaredFail {
		t.Fatalf("FAIL — Reason = %q, want %q", got.Reason, core.DLQReasonPlannerDeclaredFail)
	}
}

func TestGetRun_ReflectsStepsAndAttempts(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)
	ctx := context.Background()
	s := store.New(db)

	const retryLimit = 2
	wf := newTestWorkflow(t, ctx, s, retryLimit)
	run := newTestRun(t, ctx, s, wf)

	stepIDa, attempt1, err := s.CreateStepWithAttempt(ctx, run, testStepSpec("step-a"))
	if err != nil {
		t.Fatalf("CreateStepWithAttempt a: %v", err)
	}
	if _, err := s.CheckpointSuccess(ctx, stepIDa, attempt1, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("CheckpointSuccess a: %v", err)
	}

	stepIDb, attempt2, err := s.CreateStepWithAttempt(ctx, run, testStepSpec("step-b"))
	if err != nil {
		t.Fatalf("CreateStepWithAttempt b: %v", err)
	}
	if _, err := s.RecordFailure(ctx, stepIDb, attempt2, core.FailureReasonWorkerReported, "boom", retryLimit); err != nil {
		t.Fatalf("RecordFailure b1: %v", err)
	}
	attempt2b, err := s.StartNewAttempt(ctx, stepIDb)
	if err != nil {
		t.Fatalf("StartNewAttempt b: %v", err)
	}
	outcome2, err := s.RecordFailure(ctx, stepIDb, attempt2b, core.FailureReasonTimeout, "still failing", retryLimit)
	if err != nil {
		t.Fatalf("RecordFailure b2: %v", err)
	}
	if !outcome2.DLQed {
		t.Fatalf("FAIL — expected DLQ for step-b")
	}

	summary, err := s.GetRun(ctx, run)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if summary.Status != core.RunStatusDLQ {
		t.Fatalf("FAIL — run status = %q, want DLQ", summary.Status)
	}
	if summary.DLQReason == nil || *summary.DLQReason != core.DLQReasonWorkerRetryExhausted {
		t.Fatalf("FAIL — DLQReason = %v, want %q", summary.DLQReason, core.DLQReasonWorkerRetryExhausted)
	}
	if len(summary.Steps) != 2 {
		t.Fatalf("FAIL — len(Steps) = %d, want 2", len(summary.Steps))
	}

	var stepA, stepB *core.StepSummary
	for i := range summary.Steps {
		switch summary.Steps[i].Name {
		case "step-a":
			stepA = &summary.Steps[i]
		case "step-b":
			stepB = &summary.Steps[i]
		}
	}
	if stepA == nil || stepA.Status != core.StepStatusDone {
		t.Fatalf("FAIL — step-a = %+v, want status DONE", stepA)
	}
	if len(stepA.Attempts) != 1 || stepA.Attempts[0].Status != core.AttemptStatusDone {
		t.Fatalf("FAIL — step-a.Attempts = %+v, want one DONE attempt", stepA.Attempts)
	}

	if stepB == nil || stepB.Status != core.StepStatusDLQ {
		t.Fatalf("FAIL — step-b = %+v, want status DLQ", stepB)
	}
	if len(stepB.Attempts) != 2 {
		t.Fatalf("FAIL — len(step-b.Attempts) = %d, want 2", len(stepB.Attempts))
	}
	for _, a := range stepB.Attempts {
		if a.Status != core.AttemptStatusFailed || a.Reason == nil {
			t.Fatalf("FAIL — step-b attempt = %+v, want FAILED with a reason", a)
		}
	}
}

func TestLoadFrontier_NoSteps(t *testing.T) {
	db := openDB(t)
	resetSchema(t, db)
	ctx := context.Background()
	s := store.New(db)
	wf := newTestWorkflow(t, ctx, s, 3)
	run := newTestRun(t, ctx, s, wf)

	f, err := s.LoadFrontier(ctx, run)
	if err != nil {
		t.Fatalf("LoadFrontier: %v", err)
	}
	if f.PendingStep != nil {
		t.Fatalf("FAIL — PendingStep = %+v, want nil for a run with no steps", f.PendingStep)
	}
	if len(f.History) != 0 {
		t.Fatalf("FAIL — History = %+v, want empty", f.History)
	}
}
