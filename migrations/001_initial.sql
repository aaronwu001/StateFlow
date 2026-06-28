-- StateFlow v0.8 — initial schema
-- Authoritative: DESIGN.md §4
-- Five tables: workflows, runs, steps, attempts, dead_letter_queue

CREATE TABLE workflows (
    workflow_id    TEXT        PRIMARY KEY,          -- "wf-{uuid}" or client-supplied
    name           TEXT        NOT NULL,
    planner_type   TEXT        NOT NULL CHECK (planner_type IN ('static', 'http')),
    planner_config JSONB       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE runs (
    run_id         TEXT        PRIMARY KEY,          -- "run-{uuid}", server-generated always
    workflow_id    TEXT        NOT NULL REFERENCES workflows(workflow_id),
    status         TEXT        NOT NULL CHECK (status IN ('RUNNING', 'DONE', 'FAILED')),
    workflow_input JSONB       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE steps (
    step_id             TEXT        PRIMARY KEY,    -- "{run_id}:{step_name}", stored not computed
    run_id              TEXT        NOT NULL REFERENCES runs(run_id),
    step_name           TEXT        NOT NULL,
    seq                 INT         NOT NULL,        -- Nth decision in this run; sole ordering source (DESIGN.md §9.2)
    status              TEXT        NOT NULL CHECK (status IN ('DECIDED','RUNNING','DONE','FAILED','DLQ')),
    current_attempt_id  UUID,                        -- NO FK constraint; see note below
    decision            JSONB,                       -- StepSpec; written by PutDecision (Barrier 1)
    output              JSONB,                       -- Result.Output; written by Checkpoint (Barrier 2)
    decided_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ
    -- NOTE: current_attempt_id has NO foreign-key constraint to attempts.
    -- Reason: attempts.step_id REFERENCES steps, so a step must exist before its
    -- first attempt. At the DECIDED stage (Barrier 1) no attempt exists yet;
    -- current_attempt_id is NULL until first dispatch. Adding an FK would create
    -- a circular dependency or require DEFERRABLE with no benefit.
    -- See DESIGN.md §4 for full rationale.
);

CREATE INDEX IF NOT EXISTS idx_steps_run_id ON steps(run_id);

-- The two write barriers are the two JSONB columns on steps:
--   decision non-null, output null  →  DECIDED-not-DONE  →  re-dispatch on recovery
--   output non-null                 →  DONE              →  ask planner for next step

CREATE TABLE attempts (
    attempt_id     UUID        PRIMARY KEY,
    step_id        TEXT        NOT NULL REFERENCES steps(step_id),
    attempt_number INT         NOT NULL,
    status         TEXT        NOT NULL CHECK (status IN ('RUNNING', 'DONE', 'FAILED')),
    error          TEXT,
    dispatched_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_attempts_step_id ON attempts(step_id);

CREATE TABLE dead_letter_queue (
    id         BIGSERIAL   PRIMARY KEY,
    run_id     TEXT        NOT NULL REFERENCES runs(run_id),   -- denormalized for "all DLQ of a run" queries
    step_id    TEXT        REFERENCES steps(step_id),           -- NULL for planner_failed (no step at fault)
    reason     TEXT        NOT NULL CHECK (reason IN ('retry_exhausted', 'planner_failed', 'hard_failure')),
    context    JSONB       NOT NULL, -- snapshot: run state, last error, retry history, last output
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_dlq_run_id ON dead_letter_queue(run_id);
