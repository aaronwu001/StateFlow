-- StateFlow v1.0 — initial schema
-- Authoritative: docs/StateFlow_Whitepaper_v1_0.md §14.1
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
    status         TEXT        NOT NULL CHECK (status IN ('RUNNING', 'DONE', 'DLQ')),
    workflow_input JSONB       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);

CREATE TABLE steps (
    step_id             TEXT        PRIMARY KEY,    -- "{run_id}:{step_name}", stored not computed
    run_id              TEXT        NOT NULL REFERENCES runs(run_id),
    step_name           TEXT        NOT NULL,
    seq                 INT         NOT NULL,        -- Nth decision in this run; sole ordering source
    status              TEXT        NOT NULL CHECK (status IN ('RUNNING','DONE','DLQ')),
    attempt_count       INT         NOT NULL DEFAULT 0,
    current_attempt_id  UUID,                        -- NO FK constraint; see note below
    decision            JSONB,                       -- StepSpec; written at Barrier 1 (TX1)
    output              JSONB,                       -- Result.Output; written at Barrier 2 (TX2)
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),  -- renamed from decided_at (name retired with the DECIDED state)
    completed_at        TIMESTAMPTZ
    -- NOTE: current_attempt_id has NO foreign-key constraint to attempts.
    -- Reason: attempts.step_id REFERENCES steps, so a step must exist before its
    -- first attempt. At step creation (Barrier 1) no attempt exists yet;
    -- current_attempt_id is NULL until first dispatch. Adding an FK would create
    -- a circular dependency or require DEFERRABLE with no benefit.
    -- See whitepaper §14.1 for full rationale.
);

CREATE INDEX IF NOT EXISTS idx_steps_run_id ON steps(run_id);
CREATE INDEX IF NOT EXISTS idx_steps_run_seq ON steps(run_id, seq);

-- The two write barriers (whitepaper §19 TX1/TX2) are the two JSONB columns on steps:
--   decision non-null, output null  →  step RUNNING  →  re-dispatch on recovery
--   output non-null                 →  step DONE     →  ask planner for next step

CREATE TABLE attempts (
    attempt_id      UUID        PRIMARY KEY,
    step_id         TEXT        NOT NULL REFERENCES steps(step_id),
    status          TEXT        NOT NULL CHECK (status IN ('RUNNING', 'DONE', 'FAILED')),
    failure_reason  TEXT        CHECK (failure_reason IN ('worker_reported','timeout','malformed','orphaned')),
    error           TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),  -- renamed from dispatched_at: inserted at TX1/TX4, before dispatch — the timeout anchor. No attempt_number; order by created_at
    resolved_at     TIMESTAMPTZ,
    CONSTRAINT attempts_failure_reason_required_when_failed
        CHECK (status <> 'FAILED' OR failure_reason IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_attempts_step_id ON attempts(step_id);

CREATE TABLE dead_letter_queue (
    id         BIGSERIAL   PRIMARY KEY,
    run_id     TEXT        NOT NULL REFERENCES runs(run_id),   -- denormalized for "all DLQ of a run" queries
    step_id    TEXT        REFERENCES steps(step_id),           -- NULL for planner-side entries (no step at fault)
    reason     TEXT        NOT NULL CHECK (reason IN ('worker_retry_exhausted', 'planner_unreachable', 'planner_malformed', 'planner_declared_fail')),
    context    JSONB       NOT NULL, -- snapshot: run state, last error, retry history, last output
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_dlq_run_id ON dead_letter_queue(run_id);
