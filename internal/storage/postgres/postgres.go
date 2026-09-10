// Package postgres is the Postgres implementation of storage.Store.
//
// It is the only implementation today. SPEC.md 4.4 makes the backend a value in
// the configuration file so that it stays one implementation of an interface
// rather than an assumption baked through the system, and SPEC.md 7.1 keeps
// every JSON document opaque at the boundary: jsonb appears here and nowhere
// above.
//
// SPEC.md 7.3 lists the four atomicity and isolation obligations the
// correctness argument of SPEC.md 8 rests on. Postgres provides all four under
// READ COMMITTED, which is its default and the isolation level used here — in
// particular obligation 3, that "an UPDATE blocked by a row lock must
// re-evaluate its WHERE clause after the lock is released", without which two
// orchestrators could both conclude a run was unowned.
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/lib/pq"

	"github.com/aaronwu001/piton/internal/model"
	"github.com/aaronwu001/piton/internal/storage"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Store is a storage.Store backed by one Postgres database.
type Store struct {
	db *sql.DB
}

// Open connects to the database named by the DSN. It does not verify the
// connection: SPEC.md 13.1 case 5 wants a startup failure that "names storage
// as the cause", and that message is produced by the caller's Ping, where the
// difference between "cannot parse the DSN" and "cannot reach the server" is
// still visible.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot open storage: %w", err)
	}
	// One process, one heartbeat, one sweep and a handful of drivers. The pool
	// is bounded so that a storm of drivers cannot open connections without
	// limit, which would turn a slow worker into a database outage.
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(time.Hour)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("postgres: storage is unreachable: %w", err)
	}
	return nil
}

// Migrate applies every migration in migrations/. SPEC.md 18.1 requires this to
// run to completion before the orchestrator serves traffic; demos/alpha's
// environment has no migration service, so a 200 from GET /healthz is what
// proves it finished.
func (s *Store) Migrate(ctx context.Context) error {
	src, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("postgres: cannot read embedded migrations: %w", err)
	}
	drv, err := migratepg.WithInstance(s.db, &migratepg.Config{})
	if err != nil {
		return fmt.Errorf("postgres: cannot prepare the migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", drv)
	if err != nil {
		return fmt.Errorf("postgres: cannot prepare migrations: %w", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("postgres: migrations failed: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The operator's surface (SPEC.md 10.1, 10.2)
// ---------------------------------------------------------------------------

func (s *Store) CreateWorkflow(ctx context.Context, wf *model.Workflow) error {
	const q = `
INSERT INTO workflows (workflow_id, name, planner_type, planner_url, fetch_base_url,
                       planner_static_steps,
                       step_timeout_seconds, step_max_attempts, step_retry_delay_seconds,
                       planner_timeout_seconds, planner_max_attempts,
                       planner_retry_delay_seconds)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING created_at;`
	err := s.db.QueryRowContext(ctx, q,
		wf.WorkflowID, wf.Name, wf.PlannerType,
		nullString(wf.PlannerURL), nullString(wf.FetchBaseURL),
		jsonParam(wf.PlannerStaticSteps),
		wf.StepTimeoutSeconds, wf.StepMaxAttempts, wf.StepRetryDelaySeconds,
		wf.PlannerTimeoutSeconds, wf.PlannerMaxAttempts, wf.PlannerRetryDelaySeconds,
	).Scan(&wf.CreatedAt)
	if err != nil {
		return fmt.Errorf("postgres: cannot create workflow: %w", err)
	}
	return nil
}

const workflowColumns = `workflow_id, name, planner_type, planner_url, fetch_base_url,
       planner_static_steps,
       step_timeout_seconds, step_max_attempts, step_retry_delay_seconds,
       planner_timeout_seconds, planner_max_attempts, planner_retry_delay_seconds, created_at`

func scanWorkflow(row interface{ Scan(...any) error }) (*model.Workflow, error) {
	var (
		wf    model.Workflow
		url   sql.NullString
		fetch sql.NullString
		steps []byte
	)
	err := row.Scan(&wf.WorkflowID, &wf.Name, &wf.PlannerType, &url, &fetch, &steps,
		&wf.StepTimeoutSeconds, &wf.StepMaxAttempts, &wf.StepRetryDelaySeconds,
		&wf.PlannerTimeoutSeconds, &wf.PlannerMaxAttempts, &wf.PlannerRetryDelaySeconds,
		&wf.CreatedAt)
	if err != nil {
		return nil, err
	}
	// SPEC.md 6.1: both are NULL for a static workflow and both present for an
	// http one, so the zero value carries the same meaning the column does.
	wf.PlannerURL = url.String
	wf.FetchBaseURL = fetch.String
	wf.PlannerStaticSteps = steps
	return &wf, nil
}

func (s *Store) GetWorkflow(ctx context.Context, workflowID string) (*model.Workflow, error) {
	if !isUUID(workflowID) {
		// SPEC.md 10.5's 404 is "no such entity". A malformed identifier names
		// no entity, and letting it reach Postgres would turn it into a type
		// error — a 500 for a question whose answer is simply "no".
		return nil, storage.ErrNotFound
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+workflowColumns+` FROM workflows WHERE workflow_id = $1;`, workflowID)
	wf, err := scanWorkflow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot read workflow: %w", err)
	}
	return wf, nil
}

func (s *Store) CreateRun(ctx context.Context, run *model.Run) error {
	const q = `
INSERT INTO runs (run_id, workflow_id, status, input)
VALUES ($1, $2, $3, $4)
RETURNING created_at;`
	err := s.db.QueryRowContext(ctx, q, run.RunID, run.WorkflowID, run.Status, jsonParam(run.Input)).
		Scan(&run.CreatedAt)
	if err != nil {
		return fmt.Errorf("postgres: cannot create run: %w", err)
	}
	return nil
}

const runColumns = `run_id, workflow_id, status, input, planner_attempt_count, replay_count,
       owner_id, claimed_at, created_at`

func scanRun(row interface{ Scan(...any) error }) (*model.Run, error) {
	var (
		run       model.Run
		ownerID   sql.NullString
		claimedAt sql.NullTime
	)
	err := row.Scan(&run.RunID, &run.WorkflowID, &run.Status, &run.Input,
		&run.PlannerAttemptCount, &run.ReplayCount, &ownerID, &claimedAt, &run.CreatedAt)
	if err != nil {
		return nil, err
	}
	if ownerID.Valid {
		run.OwnerID = &ownerID.String
	}
	if claimedAt.Valid {
		run.ClaimedAt = &claimedAt.Time
	}
	return &run, nil
}

func (s *Store) GetRun(ctx context.Context, runID string) (*model.Run, error) {
	if !isUUID(runID) {
		return nil, storage.ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE run_id = $1;`, runID)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot read run: %w", err)
	}
	return run, nil
}

const stepColumns = `step_id, run_id, seq, step_name, status, decision, attempt_count,
       output, created_at, completed_at`

func scanStep(row interface{ Scan(...any) error }) (*model.Step, error) {
	var (
		st          model.Step
		name        sql.NullString
		completedAt sql.NullTime
	)
	err := row.Scan(&st.StepID, &st.RunID, &st.Seq, &name, &st.Status, &st.Decision,
		&st.AttemptCount, &st.Output, &st.CreatedAt, &completedAt)
	if err != nil {
		return nil, err
	}
	if name.Valid {
		st.StepName = &name.String
	}
	if completedAt.Valid {
		st.CompletedAt = &completedAt.Time
	}
	return &st, nil
}

func (s *Store) ListSteps(ctx context.Context, runID string) ([]*model.Step, error) {
	if !isUUID(runID) {
		return nil, storage.ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+stepColumns+` FROM steps WHERE run_id = $1 ORDER BY seq;`, runID)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot list steps: %w", err)
	}
	defer rows.Close()

	out := []*model.Step{}
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: cannot read a step: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

const attemptColumns = `attempt_id, step_id, run_id, attempt_no, replay_round, status,
       connection_mode, deadline_at, dispatched_by, output, failure_reason, error_text,
       started_at, finished_at`

func scanAttempt(row interface{ Scan(...any) error }) (*model.Attempt, error) {
	var (
		at         model.Attempt
		reason     sql.NullString
		errText    sql.NullString
		finishedAt sql.NullTime
	)
	err := row.Scan(&at.AttemptID, &at.StepID, &at.RunID, &at.AttemptNo, &at.ReplayRound,
		&at.Status, &at.ConnectionMode, &at.DeadlineAt, &at.DispatchedBy, &at.Output,
		&reason, &errText, &at.StartedAt, &finishedAt)
	if err != nil {
		return nil, err
	}
	if reason.Valid {
		at.FailureReason = &reason.String
	}
	if errText.Valid {
		at.ErrorText = &errText.String
	}
	if finishedAt.Valid {
		at.FinishedAt = &finishedAt.Time
	}
	return &at, nil
}

func (s *Store) ListAttempts(ctx context.Context, runID string) ([]*model.Attempt, error) {
	if !isUUID(runID) {
		return nil, storage.ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+attemptColumns+` FROM attempts WHERE run_id = $1 ORDER BY started_at, attempt_no;`,
		runID)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot list attempts: %w", err)
	}
	defer rows.Close()

	out := []*model.Attempt{}
	for rows.Next() {
		at, err := scanAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: cannot read an attempt: %w", err)
		}
		out = append(out, at)
	}
	return out, rows.Err()
}

// ReplayRun is SPEC.md 14, in one transaction, and it returns the run's new
// replay_count.
//
// SPEC.md 14 lists what the transaction does: "increment runs.replay_count; run
// → RUNNING; if last_step = DLQ, that step returns to RUNNING with
// attempt_count reset to 0; reset planner_attempt_count to 0; clear owner_id
// and claimed_at so the next sweep picks it up".
//
// WHY THE FIRST STATEMENT IS THE WHOLE GATE
//
//	SPEC.md 14: "the idempotency gate is 'is this run in DLQ right now'. Not
//	'has this run been replayed before'... the transaction that takes the run
//	out of DLQ IS the gate, so a double-click has exactly one winner."
//
//	That is a CAS in the sense of SPEC.md 8.1 - "an UPDATE ... WHERE <expected
//	state>, evaluated atomically with the write", where "zero rows affected
//	means the expectation was wrong, and is not an error condition, it is the
//	answer". Two replays arriving together do not race: the first takes the
//	row lock, and the second blocks on that lock until the first commits, then
//	re-evaluates status = 'DLQ' against what the first wrote. It finds RUNNING,
//	affects zero rows, and is refused. Nothing outside this statement decides
//	the winner, which is what SPEC.md 14 asks for - a SELECT followed by an
//	UPDATE would be the shape SPEC.md 8.1 rejects outright.
//
// WHY THERE IS NO OWNERSHIP FENCE
//
//	SPEC.md 8.2's fence asks "am I still this run's owner?", and it is asked by
//	a driver about a run it is driving. A replay is the operator's, and a run
//	in DLQ has no owner to test: SPEC.md 6.2 makes owner_id non-NULL only while
//	status = 'RUNNING', and SPEC.md 8.7's fourth writer cleared it in the same
//	transaction that wrote the DLQ verdict. The gate above is what stands in
//	its place, and SPEC.md 14 is the ruling that puts it there.
//
// WHY owner_id AND claimed_at ARE STILL WRITTEN
//
//	SPEC.md 14 asks for it and then says why it costs nothing: "under 8.7 a DLQ
//	run already holds both as NULL, so this is a restatement for the reader,
//	not a second mechanism". Writing NULL over NULL cannot make this a fifth
//	writer of coordination metadata in the sense SPEC.md 8.7 enumerates - the
//	gate has already established that the run is in DLQ, and SPEC.md 6.2's
//	invariant makes both columns NULL for every such row.
func (s *Store) ReplayRun(ctx context.Context, runID string) (int, error) {
	if !isUUID(runID) {
		return 0, storage.ErrNotFound
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("postgres: cannot begin a transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var replayCount int
	err = tx.QueryRowContext(ctx, `
UPDATE runs
   SET status                = 'RUNNING',
       replay_count          = replay_count + 1,
       planner_attempt_count = 0,
       owner_id              = NULL,
       claimed_at            = NULL
 WHERE run_id = $1 AND status = 'DLQ'
RETURNING replay_count;`, runID).Scan(&replayCount)
	if errors.Is(err, sql.ErrNoRows) {
		// Zero rows is the answer, but it does not yet say WHICH answer.
		// SPEC.md 10.5 separates them: 404 is "no such entity", 409 is "the
		// entity exists but is in a state that forbids this operation". The
		// question is asked inside the same transaction so that the reply
		// cannot describe a run that has since changed.
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT true FROM runs WHERE run_id = $1;`, runID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, storage.ErrNotFound
			}
			return 0, fmt.Errorf("postgres: cannot read the run: %w", err)
		}
		return 0, storage.ErrNotInDLQ
	}
	if err != nil {
		return 0, fmt.Errorf("postgres: cannot replay the run: %w", err)
	}

	// SPEC.md 14: "if last_step = DLQ, that step returns to RUNNING with
	// attempt_count reset to 0". SPEC.md 5.4 makes last_step derived and says
	// how - the highest-seq step - and SPEC.md 12.3 is why the branch is on
	// CURRENT STATE rather than on the dead-letter entry: "after several rounds
	// an old entry and current reality diverge. This is the whole reason replay
	// targets the run."
	//
	// Zero rows here is legitimate and is not an error: it is SPEC.md 12.3's
	// planner-side column, L5, where the run is in DLQ with last_step = DONE
	// and what replay resumes is "asking the planner again" - which needs no
	// step to be touched at all.
	//
	// completed_at goes back to NULL because SPEC.md 6.3 sets it "exactly when
	// status leaves RUNNING", so a step that has returned to RUNNING cannot
	// still carry one.
	if _, err := tx.ExecContext(ctx, `
UPDATE steps
   SET status = 'RUNNING', attempt_count = 0, completed_at = NULL
 WHERE step_id = (SELECT step_id FROM steps WHERE run_id = $1 ORDER BY seq DESC LIMIT 1)
   AND status = 'DLQ';`, runID); err != nil {
		return 0, fmt.Errorf("postgres: cannot return the dead-lettered step to RUNNING: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("postgres: commit failed: %w", err)
	}
	return replayCount, nil
}

// StepOutputAtSeq returns the identity and stored output of the run's step at
// one position, provided that step is DONE.
//
// SPEC.md 9.4's default for input_from — "omitted ⇒ the previous step only" —
// is a position, so this is how that default is resolved. storage.ErrNotFound
// means there is no such completed step, which at seq 0 is simply "this is the
// run's first step".
func (s *Store) StepOutputAtSeq(ctx context.Context, runID string, seq int) (string, []byte, error) {
	if !isUUID(runID) {
		return "", nil, storage.ErrNotFound
	}
	var (
		stepID string
		output []byte
	)
	// SPEC.md 6.3: completion is read from status and never from the presence
	// of an output, "a worker may legitimately return the JSON document null,
	// or an empty object, as its result".
	err := s.db.QueryRowContext(ctx,
		`SELECT step_id, output FROM steps WHERE run_id = $1 AND seq = $2 AND status = 'DONE';`,
		runID, seq).Scan(&stepID, &output)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, storage.ErrNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("postgres: cannot read a step's output: %w", err)
	}
	return stepID, output, nil
}

// StepOutputByID returns one completed step's stored output, by identity.
func (s *Store) StepOutputByID(ctx context.Context, runID, stepID string) ([]byte, error) {
	if !isUUID(runID) || !isUUID(stepID) {
		return nil, storage.ErrNotFound
	}
	var output []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT output FROM steps WHERE run_id = $1 AND step_id = $2 AND status = 'DONE';`,
		runID, stepID).Scan(&output)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot read a step's output: %w", err)
	}
	return output, nil
}

// GetStep reads one step by its own identity, for SPEC.md 10.2's
// GET /steps/{step_id}/output.
//
// SPEC.md 10.2 requires that endpoint early rather than late, and says why: "a
// planner must be able to fetch what SPEC.md 9.2's catalogue cap omits", and
// the catalogue omits every output by design.
func (s *Store) GetStep(ctx context.Context, stepID string) (*model.Step, error) {
	if !isUUID(stepID) {
		// SPEC.md 10.5's 404 is "no such entity". A malformed identifier names
		// none, and letting it reach Postgres would turn it into a type error.
		return nil, storage.ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+stepColumns+` FROM steps WHERE step_id = $1;`, stepID)
	st, err := scanStep(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot read the step: %w", err)
	}
	return st, nil
}

// ---------------------------------------------------------------------------
// Coordination metadata (SPEC.md 3.4, 8.5, 8.7)
// ---------------------------------------------------------------------------

// RegisterOrchestrator writes this process's row (SPEC.md 6.6: one row per
// process boot).
func (s *Store) RegisterOrchestrator(ctx context.Context, orchestratorID string) error {
	const q = `
INSERT INTO orchestrators (orchestrator_id, started_at, last_seen_at)
VALUES ($1, now(), now());`
	if _, err := s.db.ExecContext(ctx, q, orchestratorID); err != nil {
		return fmt.Errorf("postgres: cannot register orchestrator: %w", err)
	}
	return nil
}

// Heartbeat is SPEC.md 8.7's statement, unchanged. One row per process, O(1)
// regardless of how many runs the process owns, and last_seen_at is the only
// column it touches (SPEC.md 6.6).
func (s *Store) Heartbeat(ctx context.Context, orchestratorID string) error {
	const q = `UPDATE orchestrators SET last_seen_at = now() WHERE orchestrator_id = $1;`
	if _, err := s.db.ExecContext(ctx, q, orchestratorID); err != nil {
		return fmt.Errorf("postgres: heartbeat failed: %w", err)
	}
	return nil
}

// ReleaseOwned is SPEC.md 8.7's clean-shutdown release. It "is an optimisation
// that makes failover immediate rather than lease_ttl later; correctness does
// not depend on it".
func (s *Store) ReleaseOwned(ctx context.Context, orchestratorID string) error {
	const q = `UPDATE runs SET owner_id = NULL, claimed_at = NULL WHERE owner_id = $1;`
	if _, err := s.db.ExecContext(ctx, q, orchestratorID); err != nil {
		return fmt.Errorf("postgres: release failed: %w", err)
	}
	return nil
}

// ClaimRuns is SPEC.md 8.5, unchanged: one atomic statement, so that "exactly
// one orchestrator wins each run even when every replica sweeps at the same
// instant. There is no designated sweeper and no election."
//
// SPEC.md 8.6: because it filters on status = 'RUNNING', DONE, DLQ and
// CANCELLED runs are never claimed, and "never scanned" in SPEC.md 5.5's table
// is mechanical rather than a convention.
func (s *Store) ClaimRuns(ctx context.Context, orchestratorID string, leaseTTL time.Duration) ([]string, error) {
	const q = `
UPDATE runs r SET owner_id = $1, claimed_at = now()
WHERE r.status = 'RUNNING'
  AND (r.owner_id IS NULL
       OR NOT EXISTS (SELECT 1 FROM orchestrators o
                      WHERE o.orchestrator_id = r.owner_id
                        AND o.last_seen_at > now() - make_interval(secs => $2)))
RETURNING r.run_id;`
	rows, err := s.db.QueryContext(ctx, q, orchestratorID, leaseTTL.Seconds())
	if err != nil {
		return nil, fmt.Errorf("postgres: claim failed: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("postgres: claim failed: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// OwnedRunningRuns lists the RUNNING runs this orchestrator already owns.
//
// It is not part of SPEC.md 8.5's claim. It exists because SPEC.md 13.1 case 2
// requires "a single run's driver dies while the process lives" to be survived:
// such a run is still owned by a live orchestrator, so ClaimRuns will never
// return it, and only the owner itself can notice that nothing is driving it.
func (s *Store) OwnedRunningRuns(ctx context.Context, orchestratorID string) ([]string, error) {
	const q = `SELECT run_id FROM runs WHERE owner_id = $1 AND status = 'RUNNING';`
	rows, err := s.db.QueryContext(ctx, q, orchestratorID)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot list owned runs: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("postgres: cannot list owned runs: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// jsonParam binds one of SPEC.md 7.1's opaque JSON documents to a jsonb
// parameter, and NULL when there is none.
//
// The conversion to string is not cosmetic. lib/pq encodes a []byte parameter
// as bytea, so a document passed as bytes would arrive at a jsonb column as the
// hex text \x7b… and be rejected. A string is sent as text and parsed as jsonb,
// which is what SPEC.md 7.1 describes: "the Postgres implementation stores
// these as jsonb internally", while the interface above deals only in bytes.
func jsonParam(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// isUUID recognises the canonical hyphenated form. It is a shape test, not a
// validity test: its only job is to keep a malformed path parameter from
// reaching a uuid-typed column, where it would raise a type error instead of
// the 404 SPEC.md 10.5 calls for.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// PlannerHistory is SPEC.md 9.2's catalogue: the run's completed steps, oldest
// first, capped at storage.PlannerHistoryLimit and keeping the MOST RECENT.
//
// SPEC.md 9.2 carries output_bytes and never the output itself — "history is a
// catalogue only ... a planner that wants an output fetches it from the read
// API" — which is also why the cap is safe to apply: the rows are small and
// bounded, and the read API of SPEC.md 10.2 is how a planner reaches anything
// the cap left out.
func (s *Store) PlannerHistory(ctx context.Context, runID string) ([]storage.HistoryStep, error) {
	const q = `
SELECT step_id, step_name, seq, status,
       coalesce(octet_length(output::text), 0), attempt_count, completed_at
  FROM (SELECT * FROM steps
         WHERE run_id = $1 AND status = 'DONE'
         ORDER BY seq DESC
         LIMIT $2) recent
 ORDER BY seq;`
	rows, err := s.db.QueryContext(ctx, q, runID, storage.PlannerHistoryLimit)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot read the planner history: %w", err)
	}
	defer rows.Close()

	out := []storage.HistoryStep{}
	for rows.Next() {
		var (
			h           storage.HistoryStep
			name        sql.NullString
			completedAt sql.NullTime
		)
		if err := rows.Scan(&h.StepID, &name, &h.Seq, &h.Status,
			&h.OutputBytes, &h.AttemptCount, &completedAt); err != nil {
			return nil, fmt.Errorf("postgres: cannot read a history step: %w", err)
		}
		if name.Valid {
			h.StepName = &name.String
		}
		if completedAt.Valid {
			h.CompletedAt = &completedAt.Time
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
