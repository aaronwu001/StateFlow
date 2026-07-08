# StateFlow

**Durable Execution Layer for Agent-Native AI Pipelines**

White Paper · v0.8 · Architecture & Design Decisions

---

> **Abstract:** StateFlow is a language-agnostic sidecar service that makes multi-step AI pipelines **durable**: it checkpoints every step, recovers from crashes without re-running completed work, and retries failures — without forcing developers to adopt an SDK or modify their workers. StateFlow's defining choice is that it does **not own the workflow graph**. Instead, a pluggable **Next-Step Planner** decides each step at runtime. The planner can be a static config file or a live agent/LLM, which makes dynamic, agent-driven workflows durable for the first time without a deterministic replay engine. Any HTTP endpoint in any language is a valid Worker; any HTTP endpoint that answers "what's next?" is a valid Planner.

> **Document status:** This is the authoritative design for StateFlow as of v0.8. It supersedes all prior versions. Where it references "the previous design," that is for migration context only — the concepts described here are the current truth.

---

## Table of Contents

1. [Positioning & The Pivot](#1-positioning--the-pivot)
2. [The Core Model: Frontier Execution](#2-the-core-model-frontier-execution)
3. [Terminology](#3-terminology)
4. [The Four Abstraction Layers](#4-the-four-abstraction-layers)
5. [The Next-Step Planner Contract](#5-the-next-step-planner-contract)
6. [Worker Transport: Sync vs. Async](#6-worker-transport-sync-vs-async)
7. [Persistence: Postgres, Redis, and the Two Write Barriers](#7-persistence-postgres-redis-and-the-two-write-barriers)
8. [Why Lightweight · Why the SPOF Is Acceptable · Why It Scales](#8-why-lightweight--why-the-spof-is-acceptable--why-it-scales)
9. [MVP Scope & The Crash-Recovery Demo](#9-mvp-scope--the-crash-recovery-demo)
10. [Relationship to MCP and Agent Frameworks](#10-relationship-to-mcp-and-agent-frameworks)
11. [Extension Points for Contributors](#11-extension-points-for-contributors)
12. [What Every User Must Know (Operator's Contract)](#12-what-every-user-must-know-operators-contract)
13. [Roadmap](#13-roadmap)

---

## 1. Positioning & The Pivot

### 1.1 The Problem

Modern AI applications are multi-step pipelines: document ingestion, OCR, PII scrubbing, LLM inference, validation, database write. Each step costs time, compute, and API money. Three failure modes make this painful:

- **Total progress loss** — A 6-step pipeline fails at step 5. With no intermediate state, it restarts from step 1. The user pays full cost again.
- **Crash amnesia** — A worker or the orchestrating process dies mid-pipeline. Everything in flight, and everything already completed but un-persisted, is gone.
- **Brittle glue code** — The logic that sequences these steps, retries them, and recovers from partial failure is hand-rolled into each application, re-implemented and re-debugged every time.

The root cause: **most systems treat a pipeline as a single transaction with no durable intermediate state.** Failure means total loss.

### 1.2 What StateFlow Is

StateFlow is a sidecar HTTP server that owns the **durability** of your pipeline so your application doesn't have to. It is not a framework, not an SDK, not a cloud platform. It sits beside your workers, drives them one step at a time, persists the result of every step, and on crash resumes exactly where it left off.

### 1.3 The Pivot: StateFlow Does Not Own the Graph

The previous generation of StateFlow held a **fixed step graph**, declared up front. This made it durable but static: the sequence of steps had to be fully known before the workflow started.

**v0.8 externalizes the decision of "what runs next."** StateFlow no longer holds a graph. It holds a **durable loop**. On each iteration it asks a pluggable **Next-Step Planner**: *given everything that has happened so far, what is the next step — or are we done?* The planner answers; StateFlow executes that step durably; StateFlow records the result; StateFlow asks again.

The planner can be:

- **A static config** — a YAML list of steps. Reproduces the old fixed-graph behavior exactly, with zero dynamism. This is the safe, predictable default.
- **An agent or LLM** — decides each step dynamically based on prior results. This makes **agent-driven workflows durable** — the headline new capability.
- **Custom business logic** — any code the user wants, exposed over HTTP.

> **The one-sentence positioning:** *The agent decides what to do; StateFlow makes sure it actually gets done, isn't done twice, and isn't lost if something crashes.* StateFlow is the **reliability substrate** that agent logic runs on top of.

### 1.4 Why This Was Hard Before — And Why It's Cheap Now

The previous design listed dynamic graphs as an explicit non-goal, with a specific justification: *"dynamic graphs require a fundamentally different recovery model."* That was correct under the assumption of **replay-based recovery** (see §2.1). v0.8 changes the recovery model itself, and once you do, dynamic graphs stop being expensive. The pivot is not "we did the hard thing the old design avoided" — it's "we changed one foundational assumption, and the hard thing dissolved."

---

## 2. The Core Model: Frontier Execution

This section is the heart of the design. Everything else is consequence.

### 2.1 Two Recovery Models — And Why We Choose Frontier

When an orchestrator crashes and restarts, it must figure out where it was. There are two ways:

**Replay model (e.g. Temporal).** Re-run the workflow function from the top. Use a durable event log to "short-circuit" already-completed steps — when the re-execution reaches a step whose result is in the log, it returns the logged result instead of running it again. This requires the workflow code to be **deterministic**: the same inputs must always produce the same sequence of steps, or replay diverges from history.

**Frontier model (StateFlow's choice).** Persist each `(decision, result)` pair as it happens. On crash, read the current **frontier** — the set of completed steps and their outputs — and *ask the planner what to do next, given that frontier.* Nothing is re-run. The past is read, not re-executed.

**We must choose the frontier model, for one decisive reason:** the planner can be an LLM, and **an LLM is not deterministic.** Replay is impossible the moment your decision-maker is a frontier model — re-running it would produce different decisions and diverge from the recorded history. The frontier model has no determinism requirement: each decision is made once, persisted immediately, and never recomputed.

> This is the deep reason the pivot is cheap. We don't recover by *re-deciding*; we recover by *reading what was already decided.* Non-determinism in the planner becomes irrelevant.

### 2.2 The Correctness Invariant: Two Write Barriers

The entire durability guarantee reduces to two ordering rules. These are the load-bearing invariant of the system — they belong in `CLAUDE.md` and in every contributor's mental model.

> **Barrier 1 — persist-decision-before-dispatch.**
> The planner's chosen next step is written to the database (status `DECIDED`) **before** any worker is dispatched.
>
> **Barrier 2 — persist-result-before-next-decision.**
> A worker's result is checkpointed to the database **before** the planner is asked for the step after it.

Because of these two barriers, recovery is fully determined and trivial to reason about:

- A step with a **decision but no result** → the worker was (maybe) dispatched but we never confirmed completion. **Re-dispatch that recorded decision.** Do *not* re-ask the planner — the decision is already made and persisted. Rely on worker idempotency (§6.5) to absorb any duplicate execution.
- A step with **a result** → it's done. **Ask the planner for the next step**, passing the updated frontier.

The planner is therefore asked **exactly once per step**, and its answer is durable before any side effect occurs. This is what makes a non-deterministic planner safe inside a durable system.

### 2.3 The Durable Loop

In pseudocode, the entire orchestrator core is:

```
loop:
    # Barrier 1: if there is an undispatched decision, use it; else ask the planner.
    step = store.pending_decision(run) or planner.decide(state)
    store.put_decision(run, step)          # persisted BEFORE dispatch
    if step.is_done: break

    result = transport.dispatch(step)       # sync hold or async callback

    store.checkpoint(run, step, result)     # Barrier 2: persisted BEFORE next decide()
```

That is the whole spine. Recovery is just: re-enter the loop; `pending_decision` returns the un-acted decision if there is one, otherwise the loop asks the planner against the persisted frontier. There is no replay engine, no event-sourcing rebuild, no determinism requirement.

---

## 3. Terminology

Precise nouns. These are used consistently throughout and should be used consistently in code, docs, and conversation.

| Term | Definition |
|---|---|
| **workflow** | A static *definition* — the planner config plus settings. Reusable. |
| **run** | One *execution instance* of a workflow. Has a unique `run_id`. A workflow can be run many times; each is an independent run. (Replaces the old, overloaded term "job.") |
| **step** | One node within a run — one unit of work, pointing at a worker. |
| **worker** | The external HTTP endpoint a step invokes. Any language, any framework. |
| **planner** | The **Next-Step Planner**: the module that decides the next step. Static, agent/LLM, or custom. Its sole job is "what's next" — *nothing more*. |
| **attempt** | One specific dispatch of a step to a worker. New `attempt_id` (UUID) on every (re-)dispatch. |

Two identity fields, never conflated (this distinction prevented real bugs in the prior design and remains mandatory):

| Field | Identifies | Lifetime | Format |
|---|---|---|---|
| `step_id` | The step itself, within its run | Constant across all retries of that step | `{run_id}:{step_name}` |
| `attempt_id` | One dispatch of that step | New value every (re-)dispatch | UUID |

A worker reporting against a **superseded** `attempt_id` is acknowledged but has no effect on run state — this is the deduplication mechanism for at-least-once delivery (§6.5).

### 3.1 Status State Machines

**Step status:**

```
DECIDED ──► RUNNING ──► DONE
                    ╲
                     ╲─► FAILED ──(retry)──► DECIDED
                              ╲
                               ╲─(retries exhausted)─► DLQ
```

- `DECIDED` — planner decided, decision persisted (Barrier 1 done); worker not yet dispatched.
- `RUNNING` — dispatched; awaiting the worker's result.
- `DONE` — result checkpointed (Barrier 2 done).
- `FAILED` — the worker **explicitly reported failure** (async `/tasks/fail`, or sync non-2xx). This is a distinct status, **not** merely "not DONE." The distinction is load-bearing for recovery: a step that is `RUNNING`-with-no-result is *uncertain* (the worker may actually have finished — a dropped sync response), and must be **re-dispatched**, not treated as failed. Treating "not DONE" as failed would corrupt a successful sync step whose response was lost to a crash.
- `DLQ` — retries exhausted or planner-declared failure; terminal, awaits human replay. Mirrored by a row in the `dead_letter_queue` table holding full context.

**Run status:** `RUNNING` | `DONE` | `FAILED`. A run is `FAILED` when the planner declares `fail`, or a step lands in the DLQ and the run cannot proceed.

**Recovery rule (the heart of the demo).** On restart, for each unfinished run, read the frontier:
- A step `RUNNING` with no checkpointed `output` → **re-dispatch it** (new `attempt_id`). The in-process channel that was awaiting its callback died with the crash, so "waiting for it to come back" is impossible — the only correct action is to dispatch again and rely on worker idempotency (§6.5).
- A step `DECIDED` with no dispatch confirmed → **re-dispatch the recorded decision** (do not re-ask the planner).
- A step `DONE` → **ask the planner for the next step**, passing the updated frontier.

---

## 4. The Four Abstraction Layers

StateFlow is built around four Go interfaces. Each has at least one reference implementation shipped in the binary. **These are the contribution surface** — the project's invitation to extend it (§11).

```go
// Decides the next step. Static config, HTTP agent/LLM, or custom logic.
type NextStepPlanner interface {
    Decide(ctx context.Context, state RunState) (StepDecision, error)
}

// Dispatches a step to a worker and obtains its result.
// Reference impls: sync-hold, async-callback. Future: MCP, gRPC.
type WorkerTransport interface {
    Dispatch(ctx context.Context, step StepSpec) (Result, error)
}

// The durable source of truth. Reference impl: Postgres.
type StateStore interface {
    LoadFrontier(run RunID) (Frontier, error)
    PutDecision(run RunID, step StepSpec) error          // Barrier 1
    Checkpoint(run RunID, step StepSpec, r Result) error // Barrier 2
    PendingDecision(run RunID) (*StepSpec, error)
}

// Decides whether/when to retry a failed step. Reference impl: fixed-count.
// Future: exponential backoff, LLM-aware (retry_after).
type RetryPolicy interface {
    Next(attempt int, err error) (delay time.Duration, toDLQ bool)
}
```

Every concept in the system has a home in one of these four. Worker response interpretation lives in `WorkerTransport`. Patient-retry / Ghost Mode (deferred) lives in `RetryPolicy` plus a sweeper. There is no orphaned logic — the loop in §2.3 only ever calls these four interfaces.

### 4.1 The Core Types

The interfaces above reference these structs. This is the authoritative type definition for the MVP.

```go
type RunID string
type StepID string     // "{run_id}:{step_name}"
type AttemptID string  // UUID

// Sent BY the orchestrator TO the planner.
type RunState struct {
    RunID         RunID           `json:"run_id"`
    WorkflowInput json.RawMessage `json:"workflow_input"`
    History       []HistoryEntry  `json:"history"`   // ordered by seq
}

type HistoryEntry struct {
    Name   string          `json:"name"`
    Status string          `json:"status"`
    Output json.RawMessage `json:"output"`   // present for done steps
}

// Returned BY the planner TO the orchestrator.
type StepDecision struct {
    Status string    `json:"status"`          // "continue" | "done" | "fail"
    Step   *StepSpec `json:"step,omitempty"`
}

type StepSpec struct {
    Name           string          `json:"name"`
    WorkerURL      string          `json:"worker_url"`
    Mode           string          `json:"mode"`            // "sync" | "async"
    TimeoutSeconds int             `json:"timeout_seconds"`
    Input          json.RawMessage `json:"input"`
    OutputField    string          `json:"output_field,omitempty"` // sync only
}

// Returned BY a WorkerTransport.Dispatch. The transport layer determines
// success (§6.4) and puts the conclusion in Status — the loop only reads it.
type Result struct {
    Status     string          `json:"status"`      // "done" | "failed"
    Output     json.RawMessage `json:"output,omitempty"`
    Error      string          `json:"error,omitempty"`
    HTTPStatus int             `json:"http_status,omitempty"` // sync
}

// Returned BY StateStore.LoadFrontier for recovery. Carries both things
// recovery needs: the history to feed the planner, and any un-acted decision.
type Frontier struct {
    RunID           RunID
    History         []HistoryEntry  // done steps, ordered by seq
    PendingDecision *StepSpec       // non-nil = a DECIDED-not-DONE step to re-dispatch
}
```

---

## 5. The Next-Step Planner Contract

### 5.1 Who Calls Whom

**The orchestrator calls the planner.** The planner is a decision endpoint that the orchestrator queries; it has the same *shape* as a worker (HTTP POST in, JSON out). This symmetry is deliberate: a planner is "just another HTTP endpoint," so a static config, an LLM, and custom code all satisfy one interface.

Critically, the planner **never talks to the database.** It receives everything it needs from the orchestrator over HTTP and answers over HTTP. The store is StateFlow's private concern; the planner is external and must not know or care what backend StateFlow uses. This boundary is what lets a third party — or an LLM — be a planner.

### 5.2 What the Orchestrator Sends the Planner (RunState)

```json
{
  "run_id": "run-abc-123",
  "workflow_input": { "...": "the original payload given at run start" },
  "history": [
    { "name": "ocr", "status": "done", "output": { "...": "..." } },
    { "name": "ner", "status": "done", "output": { "...": "..." } }
  ]
}
```

- `run_id` — identifies this execution instance (one run, one id — **not** per step).
- `workflow_input` — the initial payload from `POST /workflows/:workflow_id/runs`. Constant for the whole run.
- `history` — the steps completed so far, **including their outputs**, in order.

**The planner reads the history and decides.** For the MVP, the orchestrator sends the **full history including each step's output** in one shot, so the planner needs no second round-trip.

> **Context-size note (a deferred refinement, recorded so it isn't forgotten).** Sending full outputs can grow large — the same payload-bloat concern that motivated input declaration in the prior design, and a real risk when the planner is an LLM with a bounded context window. The deferred fix: send only a **summary** (step names + statuses), and let the planner fetch specific outputs it actually wants via `GET /runs/:run_id`. Even then, the planner queries the orchestrator's **HTTP API — never the database.** The MVP sends full history for simplicity; the summary-plus-fetch optimization is a fast-follow.

### 5.3 What the Planner Returns (StepDecision)

```json
{
  "status": "continue | done | fail",
  "step": {
    "name": "llm_analysis",
    "worker_url": "http://llm-proxy/run",
    "mode": "sync | async",
    "timeout_seconds": 600,
    "input": { "...": "the payload to send this worker" },
    "output_field": "data"   // optional; sync workers only — see §6.4
  }
}
```

- `status: continue` → execute `step`.
- `status: done` → the run is complete.
- `status: fail` → the planner itself declares the run unworkable; route to DLQ.

A structural benefit of externalizing decisions: **the planner also decides each step's `input`.** In the old design, every step had to *declare* which upstream fields it consumed (`inputs: [ocr, ner]`). Now the entity that decides *what* runs next also decides *what data it receives*. The static planner reconstructs these fields from history itself; the concept disappears from StateFlow's core.

### 5.4 The Two Built-In Planners and How Users Switch

StateFlow ships **two reference planner implementations, both inside the binary.** The orchestrator's loop only ever calls `planner.Decide(state)` — it has no idea whether that reads a YAML file or makes an HTTP call. This is the key to both code clarity (one loop, no branching) and user ergonomics (switch by changing one field).

**Static planner — "batteries included."** The user does **not** stand up anything. They hand StateFlow a YAML step list; the built-in `StaticPlanner` walks it. This is the only planner StateFlow hosts for the user.

```yaml
planner:
  type: static
  steps:
    - name: ocr
      worker_url: http://ocr-service/run
      mode: async
      timeout_seconds: 30
    - name: ner
      worker_url: http://ner-service/run
      mode: async
      timeout_seconds: 30
    - name: summarize
      worker_url: http://llm-proxy/summarize
      mode: sync
      timeout_seconds: 600
```

**HTTP planner — "we give you the socket spec; you wire it up."** For an LLM or custom planner, the user runs their own decision endpoint and points StateFlow at it:

```yaml
planner:
  type: http
  url: http://my-planner/decide
```

The same `HTTPPlanner` serves **both** the LLM-planner and the custom-planner cases — to StateFlow they are identical: "an external decision endpoint." StateFlow does not need separate machinery for "LLM" vs "custom."

| Planner | Who hosts it | What the user provides |
|---|---|---|
| **static** | StateFlow (built-in) | A YAML step list. Nothing to run. |
| **LLM** | The user | Their LLM, prompted per our template (§5.5), behind an HTTP adapter. We ship an example adapter. |
| **custom** | The user | Any HTTP endpoint satisfying the contract. |

### 5.5 How an LLM Planner Learns the Rules (Goes in the User Manual)

An LLM only knows StateFlow's contract if the user tells it. The documentation **must** provide:

1. **A prompt template** the user pastes into their LLM setup, describing the `RunState` it will receive and the exact `StepDecision` JSON it must return.
2. **Acceptance / rejection criteria** for planner output. A valid decision: is well-formed JSON; contains a `status` field; when `status: continue`, contains `step.worker_url` and `step.mode`; contains nothing but the JSON (no prose, no explanation, no markdown fences). Output that fails these checks is **rejected**, and the orchestrator re-queries the planner under the planner-timeout policy (§5.6). Persistent invalid output fails the run into the DLQ.

This same document defines the `HTTPPlanner` contract for custom planners — LLM and custom authors read one spec. **The MVP ships with this section of the manual**, because without it the agent-native story cannot be executed by a user.

### 5.6 Planner Timeout

The orchestrator **must** bound how long it waits for a planner (an LLM can be slow or hang). Crucially, a planner call is **side-effect-free**: at the moment the planner is being asked, Barrier 1 has not yet fired — no decision is persisted, no worker is dispatched. Therefore a planner timeout is **safe to simply retry** — there is no risk of double-dispatching a worker.

**MVP behavior:** fixed `planner_timeout`; on timeout, re-ask a small number of times; if still failing, mark the run `FAILED` and route to the DLQ. No Ghost Mode machinery is needed here precisely because the call has no side effects — that complexity (§6.7) exists only for *workers*, which may be doing real, expensive work when they go silent.

**MVP defaults (all config-overridable):** `planner_timeout = 30s`, **2 retries** (3 attempts total) before the run fails into the DLQ. Worker retries (the `RetryPolicy` reference impl, fixed-count): **`max_retries = 3`**, **`retry_delay = 5s` fixed** (no exponential backoff — that is a §9.2 deferred item). These values are chosen to be observable in a live demo, not tuned for production.

---

## 6. Worker Transport: Sync vs. Async

### 6.1 The Fundamental Distinction: Who Reports the Result

StateFlow supports two worker connection modes because **many workers are external services the client cannot modify** — they cannot implement a callback, and may not even be able to echo back a `step_id`. The mode is the answer to "can you change this worker?"

**Sync hold — zero worker modification.** The orchestrator POSTs and **holds the connection open**, reading the result directly from the response body. The worker does nothing special — this is how any ordinary HTTP endpoint already behaves. *Cost:* network infrastructure (load balancers, proxies) typically closes idle connections after 30–90 seconds, so long-running calls can be cut off. *Use when:* the worker can't be changed, and the call completes quickly.

**Async callback — for workers you can modify.** The orchestrator POSTs; the worker returns `202 Accepted` immediately, releasing the connection; when finished, the worker makes one outbound `POST /tasks/complete` (or `/tasks/fail`) carrying `step_id` + `attempt_id`. *Cost to the worker:* accept and echo those two ids, and make one outbound call. *Use when:* the worker is yours, or the task is long-running.

### 6.2 How Dispatch Works Internally (Block-in-Dispatch)

The driver loop (§2.3) calls `result = transport.Dispatch(step)` and is oblivious to sync vs. async — that obliviousness is the whole point of the abstraction. To honor that single blocking signature, **both transports block and return a `Result`:**

- **Sync transport** — POSTs, holds the connection, reads the response body, returns. Naturally blocking.
- **Async transport** — POSTs, receives `202`, then **blocks on a Go channel** awaiting the callback. When `POST /tasks/complete` arrives, the callback handler pushes the result into that channel; `Dispatch` wakes and returns.

```
  async worker finishes
        │  POST /tasks/complete
        ▼
  ┌─────────────────┐
  │ callback handler│  1. validate attempt_id is current (reads DB)
  └─────────────────┘  2. push result into channel
        │              3. return 200 to worker
        ▼  [ channel ]
  ┌──────────────────────┐
  │ orchestrator loop    │  was blocked inside Dispatch; now wakes
  │ (Dispatch → returns) │  4. writes Barrier 2 (checkpoint) to DB
  └──────────────────────┘  5. asks planner for the next step
```

**Who writes Barrier 2 — and why it must be the loop, not the handler.** The callback handler does **not** write step business state. It only validates `attempt_id` (the §3 dedup guard) and hands the result to the channel. The **orchestrator loop** writes Barrier 2. This is mandatory: Barrier 2's guarantee is "checkpoint *before* asking the planner next," and only the loop runs that ordered sequence. If the handler also wrote step state, two writers would race and the ordering guarantee would break. Concentrating result-writes in the loop is what keeps the barrier honest.

**The channel does not survive a crash — and that's fine.** The channel is in-process. If the orchestrator crashes while an async step is in flight, the awaiting goroutine dies; on restart the DB still says `RUNNING`. Recovery therefore re-dispatches that step (§3.1) rather than trying to re-await a channel that no longer exists. The channel is only the *within-one-process* waiting mechanism; the cross-crash truth is always the database.

> *(Post-MVP, multi-instance:* a callback may land on an instance that doesn't hold the channel. That is the statelessness concern of §8.3 and is **not** solved in the single-instance MVP — do not build for it yet.)*

### 6.3 Mode Is Per-Step — Mixed Workers Are First-Class

`mode` is declared **per step** (the planner sets it in each decision), not globally. So within one run, an unmodifiable external API can run **sync**, while your own long-running LLM worker runs **async** — mixed freely. This directly resolves the "what if I can't change the worker?" concern: that worker is exactly the sync-hold case. **The existence of sync mode *is* the answer to unmodifiable workers.**

| Worker situation | Mode | What the user does |
|---|---|---|
| Cannot change it (external API, SaaS) | **sync** | Nothing — as long as the call is short |
| Can change it a little | **async** | Add one outbound POST to report back |
| Writing it fresh | **async** | Follow the callback contract directly |

### 6.4 Success Determination & Minimal Response Mapping

**MVP success rule: HTTP `2xx` = success, anything else = failure.** This is natural for sync workers — external APIs already express success/failure through HTTP status codes. For async workers, the worker explicitly calls `/tasks/complete` vs `/tasks/fail`, so the split is already unambiguous.

**The one piece of response mapping the MVP keeps: `output_field` (optional, sync only).** A sync worker returns its own JSON; StateFlow needs to know which part is the *output* to checkpoint and feed forward. Without guidance, StateFlow stores the **entire** response body — which works for the database but risks **context bloat** when that output is later handed to an LLM planner. `output_field` lets a step say "my result is under the `data` subtree," so StateFlow stores and forwards only that.

> **Why only `output_field`, when the prior design mapped four fields.** The previous `response_mapping` extracted `status`, `error`, `retry_after`, and `output`. In the MVP: `status` is handled by HTTP codes; `error` — on failure we store the whole body into the DLQ for a human, so no field-precise extraction is needed; `retry_after` is part of rate-limiting, which is deferred. Only `output` both persists *and* propagates, so it's the only field that affects downstream context size. The full four-field mapping is an extension point on `WorkerTransport`, fast-follow — needed specifically when a sync worker returns HTTP 200 but signals failure inside its body.

### 6.5 At-Least-Once Delivery & Worker Idempotency (Goes in the User Manual)

StateFlow guarantees **at-least-once** execution, not exactly-once. The honest consequence, which **must be documented prominently:**

If the orchestrator crashes during a sync-hold call, the worker may have finished, but its response died with the dropped connection. On recovery the orchestrator sees the step `RUNNING` with no checkpointed result (Barrier 2 never fired), judges it "needs re-dispatch," and **sends it again — so the worker may execute twice.**

- **Responsibility:** the **worker must be idempotent; this is the client's responsibility.**
- **What StateFlow provides:** a stable `attempt_id` / idempotency key on every dispatch. The worker uses it to recognize "I've already done this one — return the prior result instead of re-doing it."
- **Why we choose this:** exactly-once across arbitrary external HTTP workers costs dramatically more complexity and contradicts the lightweight positioning. At-least-once with idempotent workers is the standard durable-orchestration trade-off — stated in the open, not hidden.

### 6.6 The MVP HTTP API (Authoritative)

This is the complete, authoritative endpoint list for the MVP. It uses RESTful resource naming. No other endpoint forms are valid.

| Endpoint | Direction | Purpose |
|---|---|---|
| `POST /workflows` | client → StateFlow | Create a workflow definition (planner config in body). Returns `workflow_id`. |
| `POST /workflows/:workflow_id/runs` | client → StateFlow | Start a run of a workflow. Body carries `workflow_input`. Returns `run_id`. |
| `GET /runs/:run_id` | client → StateFlow | Run status: overall status plus per-step status and outputs. |
| `POST /tasks/complete` | worker → StateFlow | Async worker reports success. |
| `POST /tasks/fail` | worker → StateFlow | Async worker reports failure. |
| `GET /dlq` | client → StateFlow | List DLQ entries with full context. |
| `POST /dlq/:id/replay` | client → StateFlow | Re-queue a DLQ entry after operator investigation. |

**Workflow definition is submitted via the API, not loaded from a file at startup.** The planner config (including the static planner's YAML step list) travels in the `POST /workflows` body. YAML is the *format* of the config; the API is the *channel* by which it is submitted. The two are not in tension.

**Async callback bodies:**

```jsonc
// POST /tasks/complete
{
  "step_id":    "run-abc-123:ocr",
  "attempt_id": "uuid-...",
  "output":     { "...": "..." }
}

// POST /tasks/fail
{
  "step_id":    "run-abc-123:ocr",
  "attempt_id": "uuid-...",
  "error":      "human-readable error",
  "retry_after_seconds": 60   // optional; accepted but IGNORED in the MVP
}
```

`retry_after_seconds` is accepted on `/tasks/fail` so the worker-facing contract is stable from day one, but the MVP does **not** act on it — rate-limiting is deferred (§9.2). The field is reserved, documented as currently ignored, and activated when LLM-aware rate limiting lands.

### 6.7 Deferred: Ghost Mode & Async Timeout

The hard case — an **async** worker that goes silent (still working, or crashed — indistinguishable from outside) — requires **Ghost Mode**: a patience window, a partial checkpoint, and a *conditional atomic re-dispatch* (re-dispatch only succeeds if the step is still `RUNNING` with the same `current_attempt_id`, so a late original response and a new dispatch can't both resolve the step). This is **deferred** (§9). The MVP's async happy path assumes the worker reports back; detecting a silent async worker is the job of the async timeout sweeper, also deferred (§7.3).

---

## 7. Persistence: Postgres, Redis, and the Two Write Barriers

### 7.1 Postgres — The Single Source of Truth

Postgres holds everything authoritative:

- Workflow definitions.
- Run state (`run_id`, status).
- Per step: the **decision** (Barrier 1), the **result** (Barrier 2), status, and `current_attempt_id`.
- DLQ entries (full context: run state, last error, retry history, last partial output).

All correctness — frontier reconstruction, recovery, the two barriers — rests on Postgres alone.

### 7.2 The Barriers in the Schema

The schema must make the two barriers expressible as ordered, durable writes:

- **`PutDecision`** writes the planner's chosen step with status `DECIDED` — and this commits *before* any dispatch.
- **`Checkpoint`** writes the worker's result and advances step status to `DONE` — and this commits *before* the planner is asked again.

`LoadFrontier` reads the completed steps and outputs for recovery; `PendingDecision` returns a `DECIDED`-but-not-`DONE` step so recovery re-dispatches it rather than re-asking the planner.

**The concrete MVP schema.** Five tables. The load-bearing design choice is splitting `steps` (current state — what the frontier reads) from `attempts` (full dispatch history — what the DLQ needs for retry history).

```sql
workflows(
    workflow_id   PK,          -- client-supplied or server "wf-{uuid}"
    name,
    planner_type,              -- "static" | "http"
    planner_config JSONB,
    created_at
)

runs(
    run_id         PK,         -- server "run-{uuid}"
    workflow_id    FK,
    status,                    -- RUNNING | DONE | FAILED
    workflow_input JSONB,
    created_at, updated_at
)

steps(
    step_id            PK,     -- "{run_id}:{step_name}", stored, not computed
    run_id             FK,
    step_name,
    seq                INT,    -- Nth decision in this run; orders history
    status,                    -- DECIDED | RUNNING | DONE | FAILED | DLQ
    current_attempt_id UUID,   -- points at latest attempts row
    decision           JSONB,  -- the StepSpec; written by Barrier 1
    output             JSONB,  -- the result; written by Barrier 2
    decided_at, completed_at
)

attempts(
    attempt_id     PK UUID,
    step_id        FK,
    attempt_number INT,
    status,                    -- RUNNING | DONE | FAILED
    error          TEXT,
    dispatched_at, resolved_at
)

dead_letter_queue(
    id         PK,
    run_id     FK,             -- denormalized for "all DLQ entries of a run" queries
    step_id    FK,
    reason     TEXT,           -- retry_exhausted | planner_failed | hard_failure
    context    JSONB,          -- full snapshot: run state, last error, retry history, last output
    created_at
)
```

The two barriers are physically the two JSONB columns on `steps`: **`decision` non-null but `output` null = DECIDED-not-DONE → recovery re-dispatches; `output` non-null = DONE → ask the planner next.** This is the recovery rule made concrete in column state. `seq` is the sole source of history ordering — the planner must receive history sorted by `seq`, or a dynamic/LLM planner sees an out-of-order past and decides wrongly.

### 7.3 Redis — Reserved, Not Used in the MVP

Redis's role in the architecture is the **timing/scheduling layer**: holding async in-flight **deadlines** for the timeout sweeper, and (later) retry-after scheduling for rate limiting.

**The MVP does not use Redis.** Because the async timeout **sweeper** and **rate limiting** are both deferred, Redis has no active work to do. This is recorded deliberately:

> **Redis is reserved, not removed.** Its place in the architecture is defined; the MVP simply does not light it up. Postgres carries the entire MVP. When the async timeout sweeper lands, Redis activates to hold deadlines and let a sweeper scan for overdue tasks (operating on shared state, runnable on any/every instance with a lock or idempotent handling). This is a staged decision, not an architectural gap.

### 7.4 What Each Layer Stores

| Data | Postgres | Redis | Orchestrator memory |
|---|---|---|---|
| Workflow definition / planner config | ✓ durable | — | cached |
| Run + per-step status | ✓ durable | — | — |
| Step decision (Barrier 1) | ✓ durable | — | — |
| Step result / output (Barrier 2) | ✓ durable | — | — |
| DLQ entries | ✓ durable | — | — |
| Async in-flight deadlines | — | ✓ *(deferred)* | — |
| Retry-after schedule | — | ✓ *(deferred)* | — |
| Runtime step state | — | — | **never authoritative** |

The orchestrator's memory is **never authoritative** — it can restart at any time and rebuild everything it needs from Postgres. This property is the basis of §8.

---

## 8. Why Lightweight · Why the SPOF Is Acceptable · Why It Scales

### 8.1 Why It's Lightweight

1. Workers and planners are **plain HTTP** — no SDK, no required code changes.
2. **No deterministic replay engine** — the frontier model removes Temporal-style history-replay and workflow versioning entirely.
3. **Tiny state footprint** — each run is just a sequence of `(decision, result)` pairs.
4. **A single Go binary**, with Postgres as the only dependency for the durable demo.

> The sharpest framing: the prior design said dynamic graphs need "a fundamentally different recovery model." We obtained dynamic graphs *cheaply* — by externalizing the decision and persisting the frontier — **because we don't replay; we resume from the frontier.**

### 8.2 Why the SPOF Is Acceptable (It Was Worse Before)

StateFlow does not eliminate the single point of failure — it **moves it from your fragile pipeline code to a replicated database.**

- **Without StateFlow:** the pipeline code *is* the SPOF; a crash loses everything. This is the baseline.
- **All-in-one (single binary):** a process crash loses nothing (state is on disk); only host loss drops in-flight work. Strictly better than baseline.
- **Decoupled store:** the orchestrator becomes pure, stateless compute; the remaining SPOF is the database — a battle-tested, replicable component. The SPOF moved to where it belongs.

### 8.3 Why It Scales — Statelessness Without Leader Election

The orchestrator holds **no authoritative runtime state** (§7.4), so any instance can pick up any run by reading the store. Examining the async concern directly:

- **Async callback:** the in-flight task lives in Postgres (`RUNNING`, `current_attempt_id`); a callback is matched by `step_id`/`attempt_id` against the database, so **any instance can handle any callback.** Stateless holds.
- **The one thing not naturally stateless** is async timeout *timing*. The fix: persist deadlines (Redis) and run a **sweeper** over shared state — runnable on any or every instance with a lock or idempotent handling. This is the "except for the async timing" caveat, and it does **not** break statelessness.
- **Sync hold:** the open connection is pinned to one instance; if it dies, the call is lost — but since nothing was checkpointed, recovery just re-dispatches the step (safe via idempotency, §6.5).

Because compute is inherently stateless, **horizontal scale-out needs no leader election.** Leader election is needed only by the sweeper, and a lock suffices. Replicated-orchestrator HA therefore becomes far simpler than in the prior design.

---

## 9. MVP Scope & The Crash-Recovery Demo

### 9.1 In the MVP

The minimal set that proves the headline promise — *crash, restart, don't re-run completed steps:*

1. The durable driver loop with both write barriers (§2).
2. The `NextStepPlanner` contract plus the **built-in static planner** (§5.4).
3. Both transports: **sync hold** and **async callback** (§6).
4. A persistent **Postgres** store (§7).
5. **Crash recovery:** restart reads the frontier, resumes, re-runs nothing already done — *this is the demo.*
6. Basic **retry + DLQ**.
7. `step_id` / `attempt_id` discipline — non-negotiable; required for async dedup (§3, §6.5).
8. `GET /runs/:run_id` (status endpoint), plus the DLQ endpoints `GET /dlq` and `POST /dlq/:id/replay`.
9. The **user-manual sections** that make the system usable: the LLM-planner prompt template and acceptance criteria (§5.5), and the at-least-once / idempotency contract (§6.5).

### 9.2 Explicitly Deferred (Good-to-Have / Special-Case)

- **Ghost Mode / soft-retry / patient retry** and the **async timeout sweeper** (§6.7, §7.3).
- **LLM-aware rate limiting** (`retry_after`) — cheap, an early fast-follow.
- **Full `response_mapping`** (`status`/`error`/`retry_after` fields) — keep only `output_field` for now (§6.4).
- **Summary-plus-fetch** planner state for large outputs (§5.2).
- **DAG / fan-in parallelism**, with its single-transaction fan-in check.
- **Replicated orchestrator** with leader election (§8.3).
- **MCP transport** (§10).
- **DLQ webhooks**, **semantic repair** of malformed LLM output.
- **Redis activation** — reserved, dark until the sweeper lands (§7.3).

### 9.3 Storage Decision: Postgres From Day One

The headline demo is **process restart with recovery** — and in-memory storage cannot prove it. The MVP therefore runs on **Postgres from the start**, not in-memory. (Redis is provisioned but idle per §7.3.) The only pre-Postgres step is a schema-design checkpoint: define the four interfaces and the Postgres schema, and write a test asserting the two-barrier invariant — *before* building the loop on top.

### 9.4 The Demo Script

A 3-step run; static planner; mixed sync + async workers (Python Flask echo servers, proving language neutrality). Run to step 2, **kill the orchestrator**, restart it, and observe: step 3 runs; steps 1–2 do **not**. One `go run` (or `docker compose up`) plus a kill/restart script. This single demo is StateFlow's entire value proposition made concrete: *the worker died and came back, and we didn't re-run the work in front of it.*

---

## 10. Relationship to MCP and Agent Frameworks

### 10.1 Is This Reinventing MCP? No — They're Orthogonal

- **MCP standardizes** *how an agent calls a tool* — discovery and invocation.
- **StateFlow makes** *that sequence of calls durable* — checkpoint, retry, crash recovery.

> **MCP standardizes the call; StateFlow makes the call durable. We orchestrate MCP; we don't replace it.**

The integration direction (**post-MVP**, but the seat is reserved in `WorkerTransport` now): **MCP as a transport.** A step could be "invoke this MCP tool," so clients already in the MCP ecosystem point StateFlow at their tools directly instead of re-wrapping them as HTTP workers. This is the strongest agent-native story and the best proof that the transport abstraction earns its keep. Exposing StateFlow *as* an MCP server is low-value (the HTTP API is already simple) and is not planned.

### 10.2 Relationship to Agent Frameworks (LangGraph, CrewAI, AutoGen)

They operate at a different layer. An agent framework **decides what to do**; StateFlow **ensures it gets done, once, durably.** A framework's planned tool-call sequence can be submitted to StateFlow — or, more powerfully in this design, the framework *is* the planner — and gains checkpointing and crash recovery for free. StateFlow is the reliability substrate beneath the agent, not a competitor to it.

---

## 11. Extension Points for Contributors

StateFlow is designed to be extended along its four interfaces. Each ships with a reference implementation; each is an open invitation:

| To add… | Implement… | Reference impl shipped |
|---|---|---|
| A new way to decide steps (rules engine, different LLM harness) | `NextStepPlanner` | `StaticPlanner`, `HTTPPlanner` |
| A new way to reach workers (MCP, gRPC, message queue) | `WorkerTransport` | sync-hold, async-callback |
| A new durable backend (MySQL, SQLite, cloud KV) | `StateStore` | Postgres |
| A new retry strategy (exponential, LLM-aware, circuit-breaker) | `RetryPolicy` | fixed-count |

Because the driver loop (§2.3) speaks only these four interfaces, a contributor can replace any one of them without touching the others, and without understanding the whole system. **This is the basis of the open-source pitch: a small, comprehensible core with four clean seams.**

---

## 12. What Every User Must Know (Operator's Contract)

A consolidated checklist of things the documentation **must** make unmissable, because getting them wrong causes silent misbehavior:

1. **Workers must be idempotent.** StateFlow is at-least-once; on orchestrator crash a worker may run twice. StateFlow supplies a stable `attempt_id`; using it to deduplicate is the **client's responsibility** (§6.5).
2. **Choose the right mode per step.** Unmodifiable/external worker → **sync** (keep it short). Modifiable/long-running worker → **async** (add one outbound POST). Mode is per-step; mixing is fine (§6.3).
3. **If you use an LLM planner, prompt it with our template.** It must return strict JSON per the acceptance criteria, or its output is rejected (§5.5).
4. **Mind planner context size.** Full history is sent to the planner in the MVP; if outputs are large and your planner is an LLM, use `output_field` on sync steps to trim what gets stored and forwarded (§5.2, §6.4).
5. **Sync calls and proxy timeouts.** A sync worker behind a load balancer/proxy may be cut off after 30–90s. Long calls belong in async mode (§6.1).
6. **No built-in authentication.** StateFlow runs inside a trusted network; put it behind a gateway/service mesh for mTLS or token validation in production.
7. **The DLQ is a human queue, not a discard pile.** Exhausted retries, hard failures, and planner-declared failures land there with full context, and can be replayed after investigation.

---

## 13. Roadmap

| Phase | Scope | Key Deliverables |
|---|---|---|
| **Phase 1 — Agent-Native MVP** | Prove durable dynamic execution end-to-end | Frontier driver loop + two write barriers; `NextStepPlanner` contract + built-in static planner; sync-hold + async-callback transports; Postgres store; crash recovery; basic retry + DLQ; `step_id`/`attempt_id` discipline; status endpoint; user-manual (LLM prompt template + idempotency contract); the crash-recovery demo |
| **Phase 2 — Hardening & LLM Semantics** | Production reliability for the common cases | Async timeout sweeper (Redis activated); Ghost Mode with conditional atomic re-dispatch; LLM-aware rate limiting (`retry_after`); full `response_mapping`; summary-plus-fetch planner state; DLQ webhooks; example LLM-planner adapter |
| **Phase 3 — Scale & Extensibility** | Concurrency, HA, ecosystem | DAG parallelism with single-transaction fan-in; replicated orchestrator (leader election for the sweeper only); MCP transport; semantic repair (opt-in); observability dashboard |

---

StateFlow v0.8 · *Internal design document — not for distribution*
