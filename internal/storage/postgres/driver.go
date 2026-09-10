package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aaronwu001/piton/internal/model"
	"github.com/aaronwu001/piton/internal/storage"
)

// withFence runs fn inside one transaction that opens with the ownership fence
// of SPEC.md 8.2, verbatim:
//
//	SELECT 1 FROM runs WHERE run_id = :rid AND owner_id = :me FOR UPDATE;
//	-- 0 rows ⇒ roll back, stop the driver, dispatch nothing, tell nobody
//
// SPEC.md 8.2 says why the fence is one row lock at the top rather than the
// predicate repeated in every statement: a business transaction touches steps,
// attempts and runs, and "one forgotten statement silently breaks the whole
// guarantee". FOR UPDATE makes a concurrent claim block on that row until this
// transaction ends, "so one statement protects the transaction and the
// protection comes from the database rather than from the author remembering".
//
// SPEC.md 8.2 also says why no expiry check belongs here: if this process
// stalled and someone else claimed the run, owner_id is no longer :me and the
// fence fires; if nobody claimed it, this process is still the rightful owner.
func (s *Store) withFence(ctx context.Context, orchestratorID, runID string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres: cannot begin a transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM runs WHERE run_id = $1 AND owner_id = $2 FOR UPDATE;`,
		runID, orchestratorID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.ErrNotOwner
	}
	if err != nil {
		return fmt.Errorf("postgres: the ownership fence could not be taken: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres: commit failed: %w", err)
	}
	return nil
}

// LoadDriverState is SPEC.md 4.2 steps 1 and 2: take the fence, then read "the
// run's status and the status of its highest-seq step".
func (s *Store) LoadDriverState(ctx context.Context, orchestratorID, runID string) (*storage.DriverState, error) {
	var out storage.DriverState
	err := s.withFence(ctx, orchestratorID, runID, func(tx *sql.Tx) error {
		run, err := scanRun(tx.QueryRowContext(ctx,
			`SELECT `+runColumns+` FROM runs WHERE run_id = $1;`, runID))
		if err != nil {
			return fmt.Errorf("postgres: cannot read the run: %w", err)
		}
		out.Run = run

		wf, err := scanWorkflow(tx.QueryRowContext(ctx,
			`SELECT `+workflowColumns+` FROM workflows WHERE workflow_id = $1;`, run.WorkflowID))
		if err != nil {
			return fmt.Errorf("postgres: cannot read the run's workflow: %w", err)
		}
		out.Workflow = wf

		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM steps WHERE run_id = $1;`, runID).Scan(&out.StepCount); err != nil {
			return fmt.Errorf("postgres: cannot count the run's steps: %w", err)
		}

		// SPEC.md 5.4: last_step is derived, never stored — an index seek on
		// (run_id, seq). A run with no steps yields no row here, which SPEC.md
		// 5.4 defines as last_step = DONE.
		step, err := scanStep(tx.QueryRowContext(ctx,
			`SELECT `+stepColumns+` FROM steps WHERE run_id = $1 ORDER BY seq DESC LIMIT 1;`, runID))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("postgres: cannot read the run's last step: %w", err)
		}
		out.LastStep = step

		attempt, err := scanAttempt(tx.QueryRowContext(ctx,
			`SELECT `+attemptColumns+` FROM attempts
              WHERE step_id = $1 ORDER BY attempt_no DESC LIMIT 1;`, step.StepID))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("postgres: cannot read the step's last attempt: %w", err)
		}
		out.LastAttempt = attempt
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// insertAttempt is SPEC.md 4.2's dispatch, minus the HTTP request: "insert an
// attempts row with status RUNNING and deadline_at = now() + step_timeout_
// seconds, increment steps.attempt_count, commit, and only then send the HTTP
// request".
//
// SPEC.md 4.2 gives the reason for that order: "the attempt row must exist
// before the work does. If the process dies between the two, the worst case is
// an attempt that was never dispatched and will time out; if the order were
// reversed, work could be in flight with no row for a callback to write to and
// no deadline to expire it."
//
// The increment of steps.attempt_count happens here, at dispatch, because that
// is where SPEC.md 4.2 puts it. It is also the safe side of the ambiguity: a
// budget counted at dispatch can never be under-counted by a crash between the
// dispatch and its outcome.
func insertAttempt(ctx context.Context, tx *sql.Tx, in storage.BeginAttemptInput, orchestratorID string) (*storage.Dispatch, error) {
	var attemptNo int
	if err := tx.QueryRowContext(ctx,
		`SELECT coalesce(max(attempt_no), 0) + 1 FROM attempts WHERE step_id = $1;`,
		in.StepID).Scan(&attemptNo); err != nil {
		return nil, fmt.Errorf("postgres: cannot number the next attempt: %w", err)
	}

	d := &storage.Dispatch{StepID: in.StepID, AttemptID: model.NewID(), AttemptNo: attemptNo}
	// SPEC.md 6.4: replay_round is "the value of runs.replay_count when this
	// attempt was dispatched". It is read from the run inside this transaction
	// rather than passed in, so it cannot be a number the caller read before
	// SPEC.md 14 changed it.
	const q = `
INSERT INTO attempts (attempt_id, step_id, run_id, attempt_no, replay_round, status,
                      connection_mode, deadline_at, dispatched_by)
SELECT $1, $2, $3, $4, r.replay_count, 'RUNNING', $5,
       now() + make_interval(secs => $6), $7
  FROM runs r WHERE r.run_id = $3
RETURNING deadline_at;`
	if err := tx.QueryRowContext(ctx, q,
		d.AttemptID, in.StepID, in.RunID, attemptNo, in.ConnectionMode,
		float64(in.TimeoutSeconds), orchestratorID,
	).Scan(&d.DeadlineAt); err != nil {
		return nil, fmt.Errorf("postgres: cannot create the attempt: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE steps SET attempt_count = attempt_count + 1 WHERE step_id = $1;`,
		in.StepID); err != nil {
		return nil, fmt.Errorf("postgres: cannot burn one unit of step budget: %w", err)
	}
	return d, nil
}

// BeginStep is SPEC.md 4.2's `continue`: "insert a step at seq = last + 1 with
// status RUNNING, storing the StepSpec verbatim; reset the planner budget; then
// dispatch".
//
// SPEC.md 3.3: seq is assigned by exactly one writer — the orchestrator holding
// the run's ownership fence — inside the same transaction that creates the
// step. That is what makes "the last step" a well-defined thing to recover
// against, and it is load-bearing for SPEC.md 5.5.
func (s *Store) BeginStep(ctx context.Context, orchestratorID string, in storage.BeginStepInput) (*storage.Dispatch, error) {
	var d *storage.Dispatch
	err := s.withFence(ctx, orchestratorID, in.RunID, func(tx *sql.Tx) error {
		var seq int
		if err := tx.QueryRowContext(ctx,
			`SELECT coalesce(max(seq), 0) + 1 FROM steps WHERE run_id = $1;`,
			in.RunID).Scan(&seq); err != nil {
			return fmt.Errorf("postgres: cannot assign the next seq: %w", err)
		}

		stepID := model.NewID()
		const insertStep = `
INSERT INTO steps (step_id, run_id, seq, step_name, status, decision, attempt_count)
VALUES ($1, $2, $3, $4, 'RUNNING', $5, 0);`
		if _, err := tx.ExecContext(ctx, insertStep,
			stepID, in.RunID, seq, in.StepName, jsonParam(in.Decision)); err != nil {
			return fmt.Errorf("postgres: cannot create the step: %w", err)
		}

		// SPEC.md 4.2: "every one of those four rows also writes the call
		// itself into planner_calls, in the same transaction". This one is the
		// `continue` that produced the step above.
		if err := insertPlannerCall(ctx, tx, in.RunID, orchestratorID, in.Call); err != nil {
			return err
		}

		// SPEC.md 6.2: planner_attempt_count is "reset to 0 by any successful
		// planner call". This transaction is the record of one.
		if _, err := tx.ExecContext(ctx,
			`UPDATE runs SET planner_attempt_count = 0 WHERE run_id = $1;`,
			in.RunID); err != nil {
			return fmt.Errorf("postgres: cannot reset the planner budget: %w", err)
		}

		var err error
		d, err = insertAttempt(ctx, tx, storage.BeginAttemptInput{
			RunID:          in.RunID,
			StepID:         stepID,
			ConnectionMode: in.ConnectionMode,
			TimeoutSeconds: in.TimeoutSeconds,
		}, orchestratorID)
		if err != nil {
			return err
		}
		d.Seq = seq
		return nil
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// BeginAttempt re-dispatches an existing RUNNING step whose budget check has
// passed (SPEC.md 4.2's L2, SPEC.md 12.2).
func (s *Store) BeginAttempt(ctx context.Context, orchestratorID string, in storage.BeginAttemptInput) (*storage.Dispatch, error) {
	var d *storage.Dispatch
	err := s.withFence(ctx, orchestratorID, in.RunID, func(tx *sql.Tx) error {
		var seq int
		if err := tx.QueryRowContext(ctx,
			`SELECT seq FROM steps WHERE step_id = $1;`, in.StepID).Scan(&seq); err != nil {
			return fmt.Errorf("postgres: cannot read the step being re-dispatched: %w", err)
		}
		var err error
		if d, err = insertAttempt(ctx, tx, in, orchestratorID); err != nil {
			return err
		}
		d.Seq = seq
		return nil
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// CompleteRun is the planner's `done` (SPEC.md 4.2): run → DONE.
//
// owner_id and claimed_at are cleared in the same statement, which is SPEC.md
// 8.7's fourth writer of coordination metadata: "any transition of a run out of
// RUNNING". SPEC.md 8.7 says why it is not a tidy-up pass — "written with the
// status change, the invariant cannot be violated even for an instant" — and
// what it buys: the driver's next fence on this run returns zero rows and it
// stops silently, exactly as SPEC.md 4.2 step 1 prescribes.
func (s *Store) CompleteRun(ctx context.Context, orchestratorID, runID string, call storage.PlannerCallRecord) error {
	return s.withFence(ctx, orchestratorID, runID, func(tx *sql.Tx) error {
		// SPEC.md 4.2: the `done` that ended the run is a call like any other,
		// and lands in the same transaction as its effect (SPEC.md 6.8).
		if err := insertPlannerCall(ctx, tx, runID, orchestratorID, call); err != nil {
			return err
		}
		// SPEC.md 6.2: any successful planner call resets the budget, and this
		// is one. The run is terminal either way; the column is left honest
		// rather than left holding the last failure's count.
		res, err := tx.ExecContext(ctx, `
UPDATE runs SET status = 'DONE', planner_attempt_count = 0, owner_id = NULL, claimed_at = NULL
WHERE run_id = $1 AND owner_id = $2 AND status = 'RUNNING';`, runID, orchestratorID)
		if err != nil {
			return fmt.Errorf("postgres: cannot complete the run: %w", err)
		}
		return requireOneRow(res, storage.ErrNotOwner)
	})
}

// RecordAttemptSuccess writes the attempt's outcome under the CAS of SPEC.md
// 8.3 and then promotes its output to the step (SPEC.md 4.2's L2 / DONE row:
// "copy the attempt's output to the step, step → DONE").
//
// SPEC.md 6.4 says why the attempt carries an output of its own as well as the
// step: it is the only row a non-owner may write (SPEC.md 8.4), so an async
// callback landing on a non-owner has somewhere to deposit its result, and the
// owner promotes it on its next poll.
func (s *Store) RecordAttemptSuccess(ctx context.Context, orchestratorID, runID, stepID, attemptID string, output []byte) error {
	return s.withFence(ctx, orchestratorID, runID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE attempts SET status = 'DONE', output = $2, finished_at = now()
WHERE attempt_id = $1 AND status = 'RUNNING';`, attemptID, jsonParam(output))
		if err != nil {
			return fmt.Errorf("postgres: cannot record the attempt's success: %w", err)
		}
		if err := requireOneRow(res, storage.ErrAttemptNotRunning); err != nil {
			return err
		}

		// SPEC.md 6.3: "a step's completion is signalled by status = 'DONE'
		// and by nothing else", which is why status and output are written
		// together and neither is inferred from the other.
		res, err = tx.ExecContext(ctx, `
UPDATE steps SET status = 'DONE', output = $2, completed_at = now()
WHERE step_id = $1 AND status = 'RUNNING';`, stepID, jsonParam(output))
		if err != nil {
			return fmt.Errorf("postgres: cannot complete the step: %w", err)
		}
		return requireOneRow(res, storage.ErrAttemptNotRunning)
	})
}

// RecordAttemptFailure writes one attempt's failure under SPEC.md 8.3's CAS.
//
// When in.Reason is empty the backend decides between timeout and
// transport_error by the clock, which is what SPEC.md 5.3 requires: "timeout
// and transport_error are decided by the clock, not by the error's shape. An
// attempt is timeout only if deadline_at has passed". SPEC.md 5.3 also says
// what goes wrong otherwise — the operator "reads 'timeout' on an attempt that
// was refused instantly, and misdiagnoses a dead worker as a slow one".
func (s *Store) RecordAttemptFailure(ctx context.Context, orchestratorID string, in storage.AttemptFailure) error {
	return s.withFence(ctx, orchestratorID, in.RunID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE attempts SET status = 'FAILED', finished_at = now(), error_text = $2::text,
       failure_reason = coalesce(nullif($3::text, ''),
                                 CASE WHEN now() >= deadline_at
                                      THEN 'timeout' ELSE 'transport_error' END)
WHERE attempt_id = $1 AND status = 'RUNNING';`,
			in.AttemptID, model.TruncateError(in.ErrorText), in.Reason)
		if err != nil {
			return fmt.Errorf("postgres: cannot record the attempt's failure: %w", err)
		}
		return requireOneRow(res, storage.ErrAttemptNotRunning)
	})
}

// ExpireAttempt is SPEC.md 4.2's "RUNNING, deadline passed" and SPEC.md 8.6's
// claim-time rule for a sync attempt, which "may be expired immediately,
// regardless of deadline_at" because "its HTTP connection died with its
// previous owner, so no report can ever arrive".
//
// The choice between timeout and orphaned is made in SQL because SPEC.md 5.3
// defines orphaned as "timeout, where the attempt's dispatching orchestrator
// was not live when the attempt was expired" — the same predicate SPEC.md 8.5's
// claim uses, and one only the database can evaluate at the instant of the
// write. SPEC.md 5.3 stresses that this is only a label: "with deadline_at in a
// column, any live orchestrator expires any overdue attempt, and 'was its owner
// alive?' becomes a question for the operator rather than a branch in the code".
func (s *Store) ExpireAttempt(ctx context.Context, orchestratorID string, in storage.AttemptFailure, leaseTTL time.Duration) error {
	return s.withFence(ctx, orchestratorID, in.RunID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE attempts a SET status = 'FAILED', finished_at = now(), error_text = $2::text,
       failure_reason = CASE
           WHEN EXISTS (SELECT 1 FROM orchestrators o
                        WHERE o.orchestrator_id = a.dispatched_by
                          AND o.last_seen_at > now() - make_interval(secs => $3))
           THEN 'timeout' ELSE 'orphaned' END
WHERE a.attempt_id = $1 AND a.status = 'RUNNING';`,
			in.AttemptID, model.TruncateError(in.ErrorText), leaseTTL.Seconds())
		if err != nil {
			return fmt.Errorf("postgres: cannot expire the attempt: %w", err)
		}
		return requireOneRow(res, storage.ErrAttemptNotRunning)
	})
}

// DeadLetterStep is the worker-side dead-letter of SPEC.md 12.2 and 12.3, in
// one transaction: step → DLQ, run → DLQ and the dead-letter entry.
//
// SPEC.md 12.2 says why it must be one transaction: "it is what makes
// run=RUNNING, last_step=DLQ impossible" (SPEC.md 5.6). The combination reached
// is L4 (SPEC.md 5.5).
func (s *Store) DeadLetterStep(ctx context.Context, orchestratorID string, in storage.StepDeadLetterInput) error {
	return s.withFence(ctx, orchestratorID, in.RunID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE steps SET status = 'DLQ', completed_at = now()
WHERE step_id = $1 AND run_id = $2 AND status = 'RUNNING';`, in.StepID, in.RunID)
		if err != nil {
			return fmt.Errorf("postgres: cannot dead-letter the step: %w", err)
		}
		if err := requireOneRow(res, storage.ErrNotOwner); err != nil {
			return err
		}

		// SPEC.md 6.5: replay_round is "the value of runs.replay_count when
		// this entry was written", read inside this transaction rather than
		// passed in, so the entry cannot record a number that was already
		// stale. The budget consumed is deliberately NOT copied here — it is a
		// count of the attempts carrying that replay_round (SPEC.md 6.4), and
		// "a copy of a counter is one more thing that can disagree with the
		// rows it summarises".
		const insertEntry = `
INSERT INTO dead_letter_queue (dlq_id, run_id, step_id, reason, replay_round, error_text)
SELECT $1, r.run_id, s.step_id, 'worker_budget_exhausted', r.replay_count, $4::text
  FROM runs r JOIN steps s ON s.step_id = $3
 WHERE r.run_id = $2;`
		if _, err := tx.ExecContext(ctx, insertEntry,
			model.NewID(), in.RunID, in.StepID, model.TruncateError(in.ErrorText)); err != nil {
			return fmt.Errorf("postgres: cannot write the dead-letter entry: %w", err)
		}

		res, err = tx.ExecContext(ctx, `
UPDATE runs SET status = 'DLQ', owner_id = NULL, claimed_at = NULL
WHERE run_id = $1 AND owner_id = $2 AND status = 'RUNNING';`, in.RunID, orchestratorID)
		if err != nil {
			return fmt.Errorf("postgres: cannot dead-letter the run: %w", err)
		}
		return requireOneRow(res, storage.ErrNotOwner)
	})
}

// RecordPlannerFailure is SPEC.md 12.2's planner half: "call fails →
// runs.planner_attempt_count += 1 → count < planner_max_attempts ? call again :
// run → DLQ".
//
// SPEC.md 6.2 explains why the counter is a column rather than a loop variable,
// and it is the sentence that makes SPEC.md 12.2's claim true: "an in-memory
// planner budget resets on every restart, so an orchestrator that crashes while
// a planner is broken gets a fresh budget each time and the run never converges
// to DLQ — the exact failure 12.2 rules out."
//
// SPEC.md 6.2 also explains why the failure is recorded on the run and not in
// attempts: "an attempts row belongs to a step, and a planner failure happens
// where there is no step."
func (s *Store) RecordPlannerFailure(ctx context.Context, orchestratorID string, in storage.PlannerFailureInput) (bool, error) {
	deadLettered := false
	err := s.withFence(ctx, orchestratorID, in.RunID, func(tx *sql.Tx) error {
		errText := model.TruncateError(in.Call.ErrorText)

		// SPEC.md 12.2: "write the FAILED planner_calls row →
		// runs.planner_attempt_count += 1 (same transaction)". The row is the
		// record of what happened; the counter is what makes the run converge.
		if err := insertPlannerCall(ctx, tx, in.RunID, orchestratorID, in.Call); err != nil {
			return err
		}

		var budget int
		if err := tx.QueryRowContext(ctx, `
SELECT w.planner_max_attempts FROM runs r JOIN workflows w ON w.workflow_id = r.workflow_id
 WHERE r.run_id = $1;`, in.RunID).Scan(&budget); err != nil {
			return fmt.Errorf("postgres: cannot read the planner budget: %w", err)
		}

		var count int
		if err := tx.QueryRowContext(ctx, `
UPDATE runs SET planner_attempt_count = planner_attempt_count + 1
WHERE run_id = $1 AND owner_id = $2 AND status = 'RUNNING'
RETURNING planner_attempt_count;`,
			in.RunID, orchestratorID).Scan(&count); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return storage.ErrNotOwner
			}
			return fmt.Errorf("postgres: cannot burn one unit of planner budget: %w", err)
		}
		if count < budget {
			return nil
		}

		// SPEC.md 6.5: for a planner-side entry step_id is NULL, and there is
		// no reason to choose — a budget that ran out has exactly one, because
		// the KIND of each failure is on the call's own row (SPEC.md 6.8).
		const insertEntry = `
INSERT INTO dead_letter_queue (dlq_id, run_id, step_id, reason, replay_round, error_text)
SELECT $1, r.run_id, NULL, 'planner_budget_exhausted', r.replay_count, $3::text
  FROM runs r WHERE r.run_id = $2;`
		if _, err := tx.ExecContext(ctx, insertEntry,
			model.NewID(), in.RunID, errText); err != nil {
			return fmt.Errorf("postgres: cannot write the dead-letter entry: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
UPDATE runs SET status = 'DLQ', owner_id = NULL, claimed_at = NULL
WHERE run_id = $1 AND owner_id = $2 AND status = 'RUNNING';`, in.RunID, orchestratorID)
		if err != nil {
			return fmt.Errorf("postgres: cannot dead-letter the run: %w", err)
		}
		if err := requireOneRow(res, storage.ErrNotOwner); err != nil {
			return err
		}
		deadLettered = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return deadLettered, nil
}

// requireOneRow turns "zero rows affected" into the answer SPEC.md 8.1 says it
// is, rather than into silence: "zero rows affected means the expectation was
// wrong, and is not an error condition — it is the answer".
func requireOneRow(res sql.Result, zeroRows error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: cannot count affected rows: %w", err)
	}
	if n == 0 {
		return zeroRows
	}
	return nil
}

// ---------------------------------------------------------------------------
// The planner call (SPEC.md 6.8)
// ---------------------------------------------------------------------------

// insertPlannerCall writes one row of SPEC.md 6.8 inside the caller's
// transaction, which is the whole point of it being a helper rather than a
// method: SPEC.md 4.2 requires the call and the effect it produced to land
// together, so there is no path here that opens a transaction of its own.
//
// call_no and replay_round are read from the database rather than passed in.
// SPEC.md 6.8 makes call_no the run's own ordering, "contiguous across decision
// points and replay rounds", which only one writer may assign; and replay_round
// is "the value of runs.replay_count when the call was made", which SPEC.md 14
// can have changed since the caller last looked at the run.
//
// SPEC.md 5.8: there is no RUNNING row and no update. The row is written once,
// at the outcome, because "a row written first would be a row nothing could
// ever finish".
func insertPlannerCall(ctx context.Context, tx *sql.Tx, runID, orchestratorID string,
	call storage.PlannerCallRecord) error {

	status := model.PlannerCallDone
	if call.Failed() {
		status = model.PlannerCallFailed
	}

	const q = `
INSERT INTO planner_calls (planner_call_id, run_id, call_no, replay_round, status,
                           answer, failure_reason, error_text, called_by,
                           started_at, finished_at)
SELECT $1, r.run_id,
       coalesce((SELECT max(c.call_no) FROM planner_calls c WHERE c.run_id = r.run_id), 0) + 1,
       r.replay_count, $3::text, $4::text, $5::text, $6::text, $7::text, $8, $9
  FROM runs r WHERE r.run_id = $2;`
	if _, err := tx.ExecContext(ctx, q,
		model.NewID(), runID, status,
		nullString(call.Answer), nullString(call.FailureReason),
		nullString(model.TruncateError(call.ErrorText)),
		orchestratorID, call.StartedAt, call.FinishedAt,
	); err != nil {
		return fmt.Errorf("postgres: cannot record the planner call: %w", err)
	}
	return nil
}

// DeadLetterPlannerFail is SPEC.md 9.3's `fail` answer, which SPEC.md 12.1 is
// emphatic is not a failure: "it is a valid answer, and it sends the run to DLQ
// immediately without consuming budget".
//
// So this transaction writes a DONE call whose answer is fail, the dead-letter
// entry and run → DLQ, and touches planner_attempt_count nowhere. A run that
// stopped this way reads 0 there, and that zero is the difference between a
// planner that broke and a planner that decided.
func (s *Store) DeadLetterPlannerFail(ctx context.Context, orchestratorID string,
	in storage.PlannerDeclaredFailInput) error {

	return s.withFence(ctx, orchestratorID, in.RunID, func(tx *sql.Tx) error {
		if err := insertPlannerCall(ctx, tx, in.RunID, orchestratorID, in.Call); err != nil {
			return err
		}

		// SPEC.md 6.5: a planner-side entry names no step. SPEC.md 12.4: the
		// entry says why the run stopped, which here is the planner's own
		// `reason` (SPEC.md 9.3).
		const insertEntry = `
INSERT INTO dead_letter_queue (dlq_id, run_id, step_id, reason, replay_round, error_text)
SELECT $1, r.run_id, NULL, 'planner_declared_fail', r.replay_count, $3::text
  FROM runs r WHERE r.run_id = $2;`
		if _, err := tx.ExecContext(ctx, insertEntry,
			model.NewID(), in.RunID, model.TruncateError(in.ErrorText)); err != nil {
			return fmt.Errorf("postgres: cannot write the dead-letter entry: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
UPDATE runs SET status = 'DLQ', owner_id = NULL, claimed_at = NULL
WHERE run_id = $1 AND owner_id = $2 AND status = 'RUNNING';`, in.RunID, orchestratorID)
		if err != nil {
			return fmt.Errorf("postgres: cannot dead-letter the run: %w", err)
		}
		return requireOneRow(res, storage.ErrNotOwner)
	})
}
