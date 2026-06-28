# StateFlow v0.8 — Technical Design

**Status:** Design-final. This is the blueprint for the MVP implementation.
No implementation decision should contradict this document without explicit sign-off.
Authoritative source of truth: `docs/StateFlow_Whitepaper_v0.8.md`.

---

## 1. Core Types

*(Authoritative: §4.1)*

```go
package core

import (
    "context"
    "encoding/json"
    "time"
)

type RunID     string
type StepID    string   // "{run_id}:{step_name}"
type AttemptID string   // UUID

// RunState is sent BY the orchestrator TO the planner on each Decide call.
type RunState struct {
    RunID         RunID           `json:"run_id"`
    WorkflowInput json.RawMessage `json:"workflow_input"`
    History       []HistoryEntry  `json:"history"` // ordered by seq ASC
}

type HistoryEntry struct {
    Name   string          `json:"name"`
    Status string          `json:"status"`
    Output json.RawMessage `json:"output"` // no omitempty; present for DONE steps
}

// StepDecision is returned BY the planner TO the orchestrator.
type StepDecision struct {
    Status string    `json:"status"`        // "continue" | "done" | "fail"
    Step   *StepSpec `json:"step,omitempty"`
}

type StepSpec struct {
    Name           string          `json:"name"`
    WorkerURL      string          `json:"worker_url"`
    Mode           string          `json:"mode"`                  // "sync" | "async"
    TimeoutSeconds int             `json:"timeout_seconds"`
    Input          json.RawMessage `json:"input"`
    OutputField    string          `json:"output_field,omitempty"` // sync only
}

// Result is returned BY WorkerTransport.Dispatch.
// The transport layer determines success/failure and sets Status.
// The orchestrator loop only reads Status — it never re-interprets HTTP codes.
type Result struct {
    Status     string          `json:"status"`                // "done" | "failed"
    Output     json.RawMessage `json:"output,omitempty"`
    Error      string          `json:"error,omitempty"`
    HTTPStatus int             `json:"http_status,omitempty"` // sync transport only
}

// Frontier is returned BY StateStore.LoadFrontier.
// Carries everything recovery needs in one read: history for the planner,
// and any un-acted decision to re-dispatch without re-asking the planner.
type Frontier struct {
    RunID           RunID
    History         []HistoryEntry // DONE steps, ordered by seq ASC
    PendingDecision *StepSpec      // non-nil = DECIDED-not-DONE step; re-dispatch this
}
```

---

## 2. The Four Interfaces

*(Authoritative: §4)*

```go
// NextStepPlanner decides the next step given the current run state.
// Reference impls: StaticPlanner, HTTPPlanner.
// Extension point: rules engine, different LLM harness.
type NextStepPlanner interface {
    Decide(ctx context.Context, state RunState) (StepDecision, error)
}

// WorkerTransport dispatches a step to a worker and returns the result.
// BOTH sync and async implementations BLOCK — the loop is oblivious to mode.
// Reference impls: SyncTransport, AsyncTransport.
// Extension point: MCP transport, gRPC, message queue.
type WorkerTransport interface {
    Dispatch(ctx context.Context, step StepSpec) (Result, error)
}

// StateStore is the durable source of truth. All correctness rests here.
// Reference impl: PostgresStore.
// Extension point: MySQL, SQLite, cloud KV.
type StateStore interface {
    LoadFrontier(run RunID) (Frontier, error)
    PutDecision(run RunID, step StepSpec) error           // Barrier 1
    Checkpoint(run RunID, step StepSpec, r Result) error  // Barrier 2
    PendingDecision(run RunID) (*StepSpec, error)
}

// RetryPolicy decides whether and when to retry a failed step.
// Reference impl: FixedCountPolicy (max_retries=3, delay=5s fixed).
// Extension point: exponential backoff, LLM-aware retry_after (deferred §9.2).
type RetryPolicy interface {
    Next(attempt int, err error) (delay time.Duration, toDLQ bool)
}
```

---

## 3. Status State Machines

### Step Status

```
DECIDED ──► RUNNING ──► DONE
                    ╲
                     ╲─► FAILED ──(retry)──► DECIDED
                              ╲
                               ╲─(retries exhausted)─► DLQ
```

| Status | Meaning |
|--------|---------|
| `DECIDED` | Barrier 1 done; worker not yet dispatched |
| `RUNNING` | Dispatched; awaiting result |
| `DONE` | Barrier 2 done; result checkpointed |
| `FAILED` | Worker **explicitly** reported failure (async `/tasks/fail` or sync non-2xx). NOT the same as "not DONE." |
| `DLQ` | Retries exhausted or planner-declared failure; terminal |

**Critical distinction:** `RUNNING`-with-no-`output` is *uncertain* (the worker may have succeeded — the sync response was just lost to a crash). It is NOT `FAILED`. Recovery re-dispatches it. Treating uncertain-RUNNING as FAILED would corrupt a successfully-completed step.

### Run Status

`RUNNING` | `DONE` | `FAILED`

A run is `FAILED` when the planner declares `status: fail`, or when a step lands in DLQ and the run cannot continue.

### Three Recovery Rules (§3.1)

These are applied on restart, for each unfinished run, after reading the frontier:

1. **Step `RUNNING`, no `output`** → re-dispatch (new `attempt_id`). The in-process channel is gone; waiting is impossible.
2. **Step `DECIDED`, no `output`** → re-dispatch the recorded decision. Do NOT re-ask the planner; the decision is already persisted.
3. **Step `DONE`** → ask the planner for the next step, passing the updated frontier (all DONE steps in `seq` order).

---

## 4. Postgres Schema

*(Authoritative: §7.2 — five tables)*

```sql
-- migrations/001_initial.sql

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
    seq                 INT         NOT NULL,        -- Nth decision in this run; sole ordering source
    status              TEXT        NOT NULL CHECK (status IN ('DECIDED','RUNNING','DONE','FAILED','DLQ')),
    current_attempt_id  UUID,                        -- latest attempts row; see "no FK" note below
    decision            JSONB,                       -- StepSpec; written by PutDecision (Barrier 1)
    output              JSONB,                       -- Result.Output; written by Checkpoint (Barrier 2)
    decided_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ
);

-- current_attempt_id deliberately has NO foreign-key constraint to attempts.
-- Rationale: avoid a circular dependency. attempts.step_id REFERENCES steps,
-- so a step must exist before its first attempt. At the DECIDED stage
-- (PutDecision / Barrier 1) no attempt exists yet, so current_attempt_id is
-- NULL. It is populated only on the first dispatch, after the attempts row is
-- inserted. An FK here would either deadlock insertion order or require a
-- DEFERRABLE constraint for no real benefit. Do NOT add an FK on this column.

-- The two write barriers are physically the two JSONB columns:
--   decision non-null, output null  →  DECIDED-not-DONE  →  re-dispatch
--   output non-null                 →  DONE              →  ask planner

CREATE INDEX idx_steps_run_id ON steps(run_id);

CREATE TABLE attempts (
    attempt_id     UUID        PRIMARY KEY,
    step_id        TEXT        NOT NULL REFERENCES steps(step_id),
    attempt_number INT         NOT NULL,
    status         TEXT        NOT NULL CHECK (status IN ('RUNNING', 'DONE', 'FAILED')),
    error          TEXT,
    dispatched_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at    TIMESTAMPTZ
);

CREATE INDEX idx_attempts_step_id ON attempts(step_id);

CREATE TABLE dead_letter_queue (
    id         BIGSERIAL   PRIMARY KEY,
    run_id     TEXT        NOT NULL REFERENCES runs(run_id),   -- denormalized for queries
    step_id    TEXT        NOT NULL REFERENCES steps(step_id),
    reason     TEXT        NOT NULL CHECK (reason IN ('retry_exhausted', 'planner_failed', 'hard_failure')),
    context    JSONB       NOT NULL, -- snapshot: run state, last error, retry history, last output
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_dlq_run_id ON dead_letter_queue(run_id);
```

---

## 5. Package Structure

**Module:** `github.com/aaronwu000/stateflow` *(placeholder — update when GitHub remote is set)*

```
stateflow/
├── cmd/
│   └── stateflow/
│       └── main.go                  ← wires all components; starts HTTP server
│
├── internal/
│   ├── core/
│   │   └── interfaces.go            ← four interfaces + all core types (§2, §4)
│   │
│   ├── store/
│   │   └── postgres.go              ← PostgresStore (StateStore reference impl)
│   │
│   ├── planner/
│   │   ├── static.go                ← StaticPlanner (NextStepPlanner reference impl)
│   │   └── http.go                  ← HTTPPlanner   (NextStepPlanner reference impl)
│   │
│   ├── transport/
│   │   ├── sync.go                  ← SyncTransport  (WorkerTransport reference impl)
│   │   └── async.go                 ← AsyncTransport + callback channel registry
│   │
│   ├── orchestrator/
│   │   ├── loop.go                  ← durable driver loop (§2.3)
│   │   └── recovery.go              ← crash recovery on startup (reads frontier, §3.1)
│   │
│   └── api/
│       └── server.go                ← HTTP server + all handler funcs
│
├── migrations/
│   └── 001_initial.sql              ← schema DDL (§4 above)
│
└── docs/
    └── StateFlow_Whitepaper_v0.8.md
```

### Async Transport ↔ API Server Wiring

`AsyncTransport` owns the callback channel registry (`map[StepID]chan Result`, mutex-protected).
The API handler for `POST /tasks/complete` calls `asyncTransport.DeliverCallback(stepID, attemptID, result)` — the registry stays encapsulated inside the transport package.

Both are instantiated in `main.go` and the transport reference is passed to the API server at startup.

---

## 6. MVP HTTP API

*(Authoritative: §6.6 — these are the only valid endpoint forms)*

| Endpoint | Direction | Purpose |
|----------|-----------|---------|
| `POST /workflows` | client → StateFlow | Create workflow definition; planner config in body. Returns `workflow_id`. |
| `POST /workflows/:workflow_id/runs` | client → StateFlow | Start a run. Body: `{ "workflow_input": {...} }`. Returns `run_id`. |
| `GET /runs/:run_id` | client → StateFlow | Run status + per-step status + outputs. |
| `POST /tasks/complete` | worker → StateFlow | Async worker reports success. |
| `POST /tasks/fail` | worker → StateFlow | Async worker reports failure. |
| `GET /dlq` | client → StateFlow | List DLQ entries with full context. |
| `POST /dlq/:id/replay` | client → StateFlow | Re-queue a DLQ entry after investigation. |

### Callback Body Contracts

```jsonc
// POST /tasks/complete
{
  "step_id":    "run-abc-123:ocr",
  "attempt_id": "uuid-...",
  "output":     { "...": "..." }
}

// POST /tasks/fail
{
  "step_id":              "run-abc-123:ocr",
  "attempt_id":           "uuid-...",
  "error":                "human-readable error",
  "retry_after_seconds":  60   // accepted but IGNORED in MVP; reserved for Phase 2
}
```

---

## 7. ID Generation

| ID | Format | Generator | Notes |
|----|--------|-----------|-------|
| `workflow_id` | `wf-{uuid}` | Server (if client omits) | Client-supplied value accepted as-is |
| `run_id` | `run-{uuid}` | Server, always | Never client-supplied |
| `step_id` | `{run_id}:{step_name}` | Server (stored, not computed) | Constant across all retries |
| `attempt_id` | UUID | Server, at each (re-)dispatch | New value every dispatch |

---

## 8. Default Configuration Values

*(All config-overridable; §5.6)*

| Parameter | Default | Notes |
|-----------|---------|-------|
| `planner_timeout` | 30s | Per attempt |
| `planner_max_retries` | 2 | 3 total attempts before run → FAILED + DLQ |
| `worker_max_retries` | 3 | Before step → DLQ |
| `worker_retry_delay` | 5s | Fixed; no exponential backoff (deferred §9.2) |

---

## 9. Write Paths & Operational Mechanics

This section pins down timing and write-target details that the whitepaper leaves
implicit but which the implementation must get right.

### 9.1 `Checkpoint` Has Two Write Paths — DONE vs FAILED

`StateStore.Checkpoint(run, step, r Result)` receives the whole `Result`, but the
`Result` fields land in different tables depending on `r.Status`. The loop always
calls `Checkpoint` after `Dispatch` returns; `Checkpoint` branches internally:

**Path A — `r.Status == "done"` (success):**
- `steps.output` ← `r.Output` (Barrier 2 — this is the column that means DONE)
- `steps.status` ← `DONE`
- `steps.completed_at` ← `now()`
- `attempts` (current row) ← `status = DONE`, `resolved_at = now()`

**Path B — `r.Status == "failed"` (worker explicitly failed):**
- `steps.output` stays **NULL** (a failed step is not DONE; recovery must not treat it as done)
- `attempts` (current row) ← `status = FAILED`, `error = r.Error`, `resolved_at = now()`
- `steps.status` ← `FAILED`, then the loop consults `RetryPolicy.Next`:
  - retry → new `attempts` row, `steps.status` back to `DECIDED`, new `current_attempt_id`
  - exhausted → `steps.status = DLQ` + insert `dead_letter_queue` row

`r.Error` and `r.HTTPStatus` are **never** written to `steps.output`. The error
narrative lives in `attempts.error` and (on DLQ) in `dead_letter_queue.context`.
`steps.output` is reserved exclusively for the success output, because its
null/non-null state IS the Barrier 2 signal.

### 9.2 `seq` Generation

`seq` is assigned at `PutDecision` time as "the Nth decision in this run":

```sql
SELECT COALESCE(MAX(seq), 0) + 1 FROM steps WHERE run_id = $1
```

**This is safe without locking because of an MVP invariant: a single run has at
most one driver loop goroutine at any time, executing its steps serially.** There
is no intra-run concurrency in the MVP, so `MAX(seq)+1` cannot race within a run.

> This assumption breaks under DAG / fan-in parallelism (deferred, §9.2 of the
> whitepaper). When that lands, `seq` allocation needs a different mechanism. Do
> NOT add locking or a sequence generator now — the serial-loop assumption holds
> and over-engineering it is out of scope.

### 9.3 Recovery: Trigger Point & Scope

Recovery is the soul of the demo (kill → restart → it picks itself up). It runs in
`recovery.go`, invoked **once at startup** from `main.go`, before the HTTP server
begins accepting new runs.

```
On startup:
  rows = SELECT run_id FROM runs WHERE status = 'RUNNING'
  for each run_id:
      frontier = store.LoadFrontier(run_id)
      apply the three recovery rules (§3.1):
        - PendingDecision non-nil (DECIDED or RUNNING, no output) → re-dispatch
        - else → ask planner with frontier.History
      re-enter the driver loop for that run (one goroutine per run)
```

A run with `status = DONE` or `FAILED` is terminal and is not picked up. Only
`RUNNING` runs are resumed. Each resumed run gets its own loop goroutine, exactly
as a fresh run would — recovery and normal operation converge on the same loop.

### 9.4 `LoadFrontier` vs `GET /runs/:run_id` — Different Read Paths

These read the same `steps` table but produce different shapes and must NOT be
merged into one function:

| | `LoadFrontier(run)` | `GET /runs/:run_id` |
|---|---|---|
| Caller | recovery + the loop | external client |
| Returns | `Frontier`: DONE-step history (for planner) + `PendingDecision` | run status + every step's status & output |
| Includes non-DONE steps? | only as `PendingDecision` | yes, all steps with their current status |
| Purpose | drive execution | human/observability view |

The status endpoint is a presentation read; `LoadFrontier` is an execution read.
Keep them as separate queries.

### 9.5 HTTPPlanner Wire Contract

The `StaticPlanner` answers in-process. The `HTTPPlanner` (the second MVP
reference planner, §9.1 of the whitepaper) is a thin HTTP client:

- **Request:** `POST {planner.url}` with body = `RunState` JSON (§5.2 of whitepaper).
- **Response:** `StepDecision` JSON (§5.3 of whitepaper).
- **Output validation:** apply the acceptance criteria of whitepaper §5.5 (well-formed
  JSON, has `status`, on `continue` has `step.worker_url` + `step.mode`, nothing but JSON).
  Rejected output is treated as a planner failure and re-queried under the timeout policy.
- **Timeout & retries:** `planner_timeout` (30s) with 2 retries per §5.6 / §8 of this
  doc. A planner call is side-effect-free (Barrier 1 has not fired), so a timeout is
  safe to simply retry — no risk of double-dispatch.

Both planners satisfy the same `NextStepPlanner.Decide` signature; the loop cannot
tell them apart.

---

## 10. Design Decisions Not in the Whitepaper

### Why `seq` is the sole ordering source

History sent to the planner must be ordered by `seq`. If a dynamic/LLM planner receives history out of order it sees a scrambled past and decides wrongly. Do not order by `decided_at` (clock skew) or `step_id` (lexicographic, not temporal).

### Why Barrier 2 is written by the loop, not the callback handler

The callback handler (`POST /tasks/complete`) does exactly three things: validate `attempt_id`, push to channel, return 200. It does NOT write step state. Barrier 2 is written by the orchestrator loop after `Dispatch` returns. This is mandatory: Barrier 2's guarantee is "checkpoint before asking the planner next," and only the loop can enforce that ordered sequence. Two writers would race and break the barrier.

### Why the channel does not survive a crash

The channel is in-process. A crash kills the goroutine waiting on it. Recovery reads `RUNNING`-with-no-`output` from the DB and re-dispatches. The channel is only the within-one-process waiting mechanism; the cross-crash truth is always the DB.
