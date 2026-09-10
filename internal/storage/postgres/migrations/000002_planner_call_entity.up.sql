-- Piton, migration 2: the planner call becomes an entity.
--
-- This migration carries two SPEC.md amendments that landed on the same day.
--
--   1. SPEC.md 6.1 gained fetch_base_url: the address an http workflow's
--      planner is told to read back from, required iff planner_type = 'http'
--      and absent otherwise, alongside planner_url.
--
--   2. SPEC.md 3.2 made the PLANNER CALL an entity, SPEC.md 5.8 gave it states
--      and failure reasons, and SPEC.md 6.8 gave it a table. Three columns that
--      existed only because it had none are removed with it:
--
--        runs.last_planner_error         a hand-maintained copy of the newest
--                                        call (SPEC.md 6.2)
--        dead_letter_queue.attempt_count the only surviving record of a budget
--                                        that SPEC.md 14 resets - now a count
--                                        of the rows carrying that replay_round
--        two of the five dlq reasons     the enumeration that had to summarise
--                                        a set of failures in one word
--
--      and one column is added to two tables: replay_round, so that every row
--      says which replay round it belonged to (SPEC.md 6.4, 6.8, 14).
--
-- WHY THIS IS A NEW MIGRATION AND NOT AN EDIT TO 000001
--   golang-migrate decides what to run by version number, never by content. A
--   database that has recorded version 1 will not re-read that file, so editing
--   it would leave existing databases without these columns while fresh ones
--   got them - two databases both calling themselves version 1, with different
--   shapes and nothing able to tell them apart.
--
-- WHY THE ADDED NOT NULL COLUMNS TAKE A DEFAULT AND THEN LOSE IT
--   The default exists to give rows that already exist a value. It is dropped
--   immediately afterwards so that the orchestrator must supply one: a column
--   that silently fills itself in is a column that cannot report a caller which
--   forgot to write it, and SPEC.md 6.4's invariants are written as constraints
--   for exactly that reason - "cheap to make impossible and expensive to
--   notice".

BEGIN;

-- ---------------------------------------------------------------- workflows
--
-- A workflow row written before this migration cannot satisfy SPEC.md 6.1's
-- amended invariant: it has planner_type = 'http' and there is no address to
-- put in fetch_base_url. No address can be invented for it either - SPEC.md 6.1
-- makes fetch_base_url the operator's statement of how his planner reaches this
-- deployment, and a guess would be a wrong answer written into a definition.
--
-- Rather than let that surface as a bare check-constraint violation naming a
-- constraint the reader has never heard of, the migration refuses first and
-- says what is wrong. Any environment that starts from a clean database - which
-- is every demo and every test environment, per CLAUDE.md 5.5.2 - passes this
-- silently.
DO $$
DECLARE stranded INT;
BEGIN
    SELECT count(*) INTO stranded FROM workflows WHERE planner_type = 'http';
    IF stranded > 0 THEN
        RAISE EXCEPTION
            'migration 000002: % workflow row(s) have planner_type = ''http'' and predate fetch_base_url (SPEC.md 6.1)',
            stranded
        USING HINT =
            'No address can be invented for them. Delete those workflows, or add the fetch_base_url column and populate it by hand, before running this migration.';
    END IF;
END $$;

-- SPEC.md 6.1: fetch_base_url is "required iff planner_type = 'http'; must be
-- absent otherwise", and sits beside planner_url because the two are one
-- relationship seen from its two ends.
ALTER TABLE workflows ADD COLUMN fetch_base_url TEXT;

-- SPEC.md 11.1: the planner family gains the third field the step family
-- already had, completing the symmetry - an upper bound on one exchange, a
-- total number of exchanges, and a wait between them.
ALTER TABLE workflows
    ADD COLUMN planner_retry_delay_seconds INT NOT NULL DEFAULT 0;
ALTER TABLE workflows
    ALTER COLUMN planner_retry_delay_seconds DROP DEFAULT;

-- SPEC.md 6.1's invariant, restated with both http columns on the same side:
-- "planner_url and fetch_base_url are both present iff planner_type = 'http',
-- and planner_static_steps is present iff planner_type = 'static' - the two
-- sides are never mixed and never both absent".
ALTER TABLE workflows DROP CONSTRAINT workflows_planner_shape;
ALTER TABLE workflows ADD CONSTRAINT workflows_planner_shape CHECK (
    (planner_type = 'http'
         AND planner_url IS NOT NULL
         AND fetch_base_url IS NOT NULL
         AND planner_static_steps IS NULL)
 OR (planner_type = 'static'
         AND planner_url IS NULL
         AND fetch_base_url IS NULL
         AND planner_static_steps IS NOT NULL)
);

-- SPEC.md 6.1 and 11.1: "all six numeric configuration columns are >= 1 except
-- the two *_retry_delay_seconds, which are >= 0".
ALTER TABLE workflows DROP CONSTRAINT workflows_limits;
ALTER TABLE workflows ADD CONSTRAINT workflows_limits CHECK (
    step_timeout_seconds        >= 1 AND
    step_max_attempts           >= 1 AND
    step_retry_delay_seconds    >= 0 AND
    planner_timeout_seconds     >= 1 AND
    planner_max_attempts        >= 1 AND
    planner_retry_delay_seconds >= 0
);

-- --------------------------------------------------------------------- runs
-- SPEC.md 6.2: "it would be a copy of the newest planner_calls row for the
-- run, maintained by hand in every transaction that writes one - the class of
-- mistake SPEC.md 8.2 rejects, where one forgotten write leaves a column
-- silently lying".
ALTER TABLE runs DROP COLUMN last_planner_error;

-- ----------------------------------------------------------------- attempts
-- SPEC.md 6.4: "the value of runs.replay_count when this attempt was
-- dispatched". SPEC.md 14 leaves every earlier attempts row in place and
-- resets the step's counter, so without this column a step holds the rows of
-- two rounds with nothing on them saying which round is which.
ALTER TABLE attempts ADD COLUMN replay_round INT NOT NULL DEFAULT 0;
ALTER TABLE attempts ALTER COLUMN replay_round DROP DEFAULT;
ALTER TABLE attempts ADD CONSTRAINT attempts_replay_round CHECK (replay_round >= 0);

-- -------------------------------------------------------- dead_letter_queue
-- SPEC.md 6.5, cut from five reasons to three. The planner side no longer
-- names the KIND of failure, because every call now carries its own
-- failure_reason (SPEC.md 5.8): "reason says which of three situations stopped
-- the run. It does not say why any individual exchange failed."
ALTER TABLE dead_letter_queue DROP CONSTRAINT dlq_reason;
ALTER TABLE dead_letter_queue ADD CONSTRAINT dlq_reason CHECK (reason IN (
    'worker_budget_exhausted',
    'planner_budget_exhausted',
    'planner_declared_fail'));

-- SPEC.md 6.5: "the entry no longer records the budget consumed ... replay_round
-- on every attempt and every planner call now carries that history on the rows
-- themselves, so the number is a count rather than a copy - and a copy of a
-- counter is one more thing that can disagree with the rows it summarises."
ALTER TABLE dead_letter_queue DROP CONSTRAINT dlq_counters;
ALTER TABLE dead_letter_queue DROP COLUMN attempt_count;
ALTER TABLE dead_letter_queue ADD CONSTRAINT dlq_counters CHECK (replay_round >= 0);

-- The dlq_side constraint is unchanged and still correct: SPEC.md 12.3 still
-- gives worker-side entries a step and planner-side entries none, and
-- worker_budget_exhausted is still the only worker-side reason.

-- ------------------------------------------------------------ planner_calls
-- SPEC.md 6.8. One row per question put to a run's planner, written once at the
-- outcome (SPEC.md 5.8). It is to a planner call what attempts is to a step's
-- dispatch, and SPEC.md 6.7 puts it under the same discipline as
-- dead_letter_queue: append-only history, never updated, never deleted.
CREATE TABLE planner_calls (
    planner_call_id UUID        PRIMARY KEY,
    run_id          UUID        NOT NULL REFERENCES runs (run_id),
    -- SPEC.md 6.8: "1-based ordering within the run, contiguous across decision
    -- points and replay rounds - it orders the run's whole conversation with
    -- its planner".
    call_no         INT         NOT NULL,
    replay_round    INT         NOT NULL,
    -- SPEC.md 5.8: DONE or FAILED. "There is no RUNNING state, and the row is
    -- written once, at the outcome" - a row written first would be one that no
    -- other process could ever resolve.
    status          TEXT        NOT NULL,
    answer          TEXT,
    failure_reason  TEXT,
    error_text      TEXT,
    called_by       TEXT        NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL,
    -- SPEC.md 6.8: never NULL, because the row is written at the outcome.
    finished_at     TIMESTAMPTZ NOT NULL,

    CONSTRAINT planner_calls_status CHECK (status IN ('DONE', 'FAILED')),

    CONSTRAINT planner_calls_call_no CHECK (call_no >= 1),

    CONSTRAINT planner_calls_replay_round CHECK (replay_round >= 0),

    -- SPEC.md 9.3's three answers, and no fourth.
    CONSTRAINT planner_calls_answer_value CHECK (
        answer IS NULL OR answer IN ('continue', 'done', 'fail')),

    -- SPEC.md 5.8's three failure reasons - the same three names SPEC.md 5.3
    -- gives an attempt, with identical meanings. worker_error, orphaned and
    -- cancelled are absent for the reasons SPEC.md 5.8 states, not by omission.
    CONSTRAINT planner_calls_failure_reason_value CHECK (
        failure_reason IS NULL OR failure_reason IN (
            'transport_error', 'invalid_response', 'timeout')),

    -- SPEC.md 6.8 invariant 1: FAILED implies a reason and no answer.
    CONSTRAINT planner_calls_failed_shape CHECK (
        (status = 'FAILED') = (failure_reason IS NOT NULL)),

    -- SPEC.md 6.8 invariant 2: DONE implies an answer and no reason.
    CONSTRAINT planner_calls_done_shape CHECK (
        (status = 'DONE') = (answer IS NOT NULL)),

    -- SPEC.md 6.8 invariant 3: error_text is present whenever the call FAILED,
    -- and whenever the answer was fail - "a planner that refuses must say why,
    -- and SPEC.md 9.3 makes reason part of that answer". It is an implication,
    -- not an equivalence: a successful continue may also carry diagnostic text.
    CONSTRAINT planner_calls_error_text_required CHECK (
        (status <> 'FAILED' AND answer IS DISTINCT FROM 'fail')
        OR error_text IS NOT NULL),

    -- SPEC.md 6.4: "one limit, both tables" - and now three.
    CONSTRAINT planner_calls_error_text_limit CHECK (
        error_text IS NULL OR octet_length(error_text) <= 4096)
);

-- SPEC.md 7.2: ordering a run's planner conversation, assigning the next
-- call_no, and reading the newest call in place of the column SPEC.md 6.2
-- removed. Unique, which is also SPEC.md 6.8's "1-based ordering within the
-- run".
CREATE UNIQUE INDEX planner_calls_run_no_idx ON planner_calls (run_id, call_no);

COMMIT;
