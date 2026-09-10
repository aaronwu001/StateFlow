-- The inverse of 000002_planner_call_entity.up.sql.
--
-- It restores the shape of version 1 exactly, including the constraints the up
-- migration replaced. What it cannot restore is DATA: the planner_calls rows
-- are dropped with the table, and dead_letter_queue.attempt_count and
-- runs.last_planner_error come back empty, because the columns that held their
-- values no longer exist to be read from. A down migration is a way back to a
-- schema, never a way back to a history.

BEGIN;

-- ------------------------------------------------------------ planner_calls
DROP INDEX IF EXISTS planner_calls_run_no_idx;
DROP TABLE IF EXISTS planner_calls;

-- -------------------------------------------------------- dead_letter_queue
ALTER TABLE dead_letter_queue DROP CONSTRAINT dlq_counters;
ALTER TABLE dead_letter_queue ADD COLUMN attempt_count INT NOT NULL DEFAULT 0;
ALTER TABLE dead_letter_queue ALTER COLUMN attempt_count DROP DEFAULT;
ALTER TABLE dead_letter_queue ADD CONSTRAINT dlq_counters
    CHECK (replay_round >= 0 AND attempt_count >= 0);

ALTER TABLE dead_letter_queue DROP CONSTRAINT dlq_reason;
ALTER TABLE dead_letter_queue ADD CONSTRAINT dlq_reason CHECK (reason IN (
    'worker_budget_exhausted',
    'planner_unreachable',
    'planner_invalid_response',
    'planner_budget_exhausted',
    'planner_declared_fail'));

-- ----------------------------------------------------------------- attempts
ALTER TABLE attempts DROP CONSTRAINT attempts_replay_round;
ALTER TABLE attempts DROP COLUMN replay_round;

-- --------------------------------------------------------------------- runs
ALTER TABLE runs ADD COLUMN last_planner_error TEXT;

-- ---------------------------------------------------------------- workflows
ALTER TABLE workflows DROP CONSTRAINT workflows_limits;
ALTER TABLE workflows ADD CONSTRAINT workflows_limits CHECK (
    step_timeout_seconds     >= 1 AND
    step_max_attempts        >= 1 AND
    step_retry_delay_seconds >= 0 AND
    planner_timeout_seconds  >= 1 AND
    planner_max_attempts     >= 1
);

ALTER TABLE workflows DROP CONSTRAINT workflows_planner_shape;
ALTER TABLE workflows ADD CONSTRAINT workflows_planner_shape CHECK (
    (planner_type = 'http'
         AND planner_url IS NOT NULL AND planner_static_steps IS NULL)
 OR (planner_type = 'static'
         AND planner_url IS NULL AND planner_static_steps IS NOT NULL)
);

ALTER TABLE workflows DROP COLUMN planner_retry_delay_seconds;
ALTER TABLE workflows DROP COLUMN fetch_base_url;

COMMIT;
