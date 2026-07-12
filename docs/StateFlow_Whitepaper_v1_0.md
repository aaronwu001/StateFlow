# StateFlow

**Durable Execution Layer for Agent-Native AI Pipelines**

White Paper · v1.0 · Architecture & Design Decisions

---

> **Abstract:** StateFlow is a language-agnostic sidecar service that makes multi-step AI pipelines **durable**: it checkpoints every step, recovers from crashes without re-running completed work, and retries failures — without forcing developers to adopt an SDK or modify their workers. StateFlow does **not own the workflow graph**. A pluggable **Next-Step Planner** decides each step at runtime; the planner can be a static config file or a live LLM/agent, which makes dynamic, agent-driven workflows durable without a deterministic replay engine. Any HTTP endpoint in any language is a valid Worker; any HTTP endpoint that answers "what's next?" is a valid Planner.

> **Document status:** Authoritative design as of v1.0. Supersedes v0.9 and all prior versions. The v1.0 state model is a ground-up simplification of the v0.9 five-state machine; the reasoning that led here is collected in §16 (Design History — a self-contained, removable section).

---

## Table of Contents

1. [Positioning](#1-positioning)
2. [The Core Model: Frontier Execution and the Two Write Barriers](#2-the-core-model)
3. [System Components and Terminology](#3-system-components-and-terminology)
4. [The State Model](#4-the-state-model)
5. [The Step Loop](#5-the-step-loop)
6. [The Timeout Doctrine](#6-the-timeout-doctrine)
7. [Failure Classification, Retry, and Budgets](#7-failure-classification-retry-and-budgets)
8. [Invariants, the Combination Table, and Recovery](#8-invariants-combination-table-recovery)
9. [Component Failure Overview](#9-component-failure-overview)
10. [Result Reporting: CAS and the Single Writer](#10-result-reporting)
11. [The DLQ and Replay](#11-the-dlq-and-replay)
12. [The Planner Contract](#12-the-planner-contract)
13. [The Worker Contract](#13-the-worker-contract)
14. [Persistence, Schema, and Performance](#14-persistence-schema-and-performance)
15. [What Every User Must Know (Operator's Contract)](#15-operators-contract)
16. [Design History (removable section)](#16-design-history)
17. [Q&A](#17-qa)
18. [Temporary Design Registry](#18-temporary-design-registry)
19. [The Atomic Transaction Ledger](#19-the-atomic-transaction-ledger)
20. [Extension Points](#20-extension-points)
21. [Roadmap](#21-roadmap)

---

## 1. Positioning

Modern AI applications are multi-step pipelines: ingestion, OCR, PII scrubbing, LLM inference, validation, persistence. Each step costs time, compute, and API money. Most systems treat the pipeline as a single transaction with no durable intermediate state, so failure means total loss: a crash at step 5 of 6 restarts from step 1.

StateFlow is a sidecar HTTP server that owns the **durability** of the pipeline so the application doesn't have to. It is not a framework, not an SDK, not a cloud platform. It sits beside your workers, drives them one step at a time, persists the result of every step, and on crash resumes exactly where it left off.

StateFlow's defining choice is that it holds a **durable loop**, not a graph. On each iteration it asks a pluggable **Next-Step Planner**: *given everything that has happened so far, what runs next — or are we done?* The planner can be a static YAML step list (zero dynamism, zero deployment), a live LLM/agent (fully dynamic), or any custom HTTP endpoint.

> **One-sentence positioning:** *The agent decides what to do; StateFlow makes sure it actually gets done, isn't done twice, and isn't lost if something crashes.*

### 1.1 The Competitive Landscape: Two Families

Durable-execution systems split cleanly by recovery model:

**The replay family (Temporal, Hatchet, DBOS)** persists an event log and recovers by **re-executing workflow code**, short-circuiting logged steps. Consequence: workflow code must be **deterministic** — so an LLM cannot be the decision-maker inside the workflow, and deploying changed workflow code mid-flight requires versioning/patching machinery.

**The frontier family (LangGraph, StateFlow)** persists state as it is produced and recovers by **reading**, never re-executing. LangGraph checkpoints its graph's state snapshot; StateFlow persists the **decision log** — every (decision, result) pair, in order — which is exactly the shape an LLM planner wants as input.

| Dimension | Replay family | LangGraph | **StateFlow** |
|---|---|---|---|
| Recovery model | Re-execute code, short-circuit via log | Load state snapshot | Read decision log (frontier) |
| Determinism required | **Yes** | No | No |
| LLM as the decision-maker | No (inside activities only) | Yes (inside the graph) | **Yes — as the planner, first-class** |
| Workflow versioning sensitivity | High | Low | **None** (versioning immunity, below) |
| Integration surface | SDK | Python/JS framework, in-process | **Plain HTTP, zero SDK** |

**Versioning immunity.** StateFlow never re-derives past decisions: each step's decision is persisted before any side effect and re-read on recovery, so the planner is asked **exactly once per step**. Redeploying the planner mid-flight (new prompt, new model, new rules) is inherently safe — in-flight runs get the new brain for future decisions while their past remains untouched data. The entire versioning problem class of replay systems does not exist here. This is not a feature; it is a consequence of the frontier model.

> Replay systems make *deterministic code* durable; LangGraph makes *its own graphs* durable; **StateFlow makes *any decider over HTTP* durable.**

### 1.2 Relationship to MCP and Agent Frameworks

MCP standardizes *how an agent calls a tool*; StateFlow makes *that sequence of calls durable*. We orchestrate MCP; we don't replace it. The planner contract deliberately stays plain HTTP + JSON rather than MCP: a planner is a decision endpoint, not a tool, and MCP's role model has no clean seat for it — forcing it would import protocol weight and contradict the zero-SDK positioning. MCP's correct place is as a future **worker transport** (a step = "invoke this MCP tool"; Phase 3, §21).

Agent frameworks (LangGraph, CrewAI, AutoGen) operate one layer up: the framework decides what to do; StateFlow ensures it gets done, once, durably. Most powerfully, the framework *is* the planner.

---

## 2. The Core Model: Frontier Execution and the Two Write Barriers

The entire durability guarantee reduces to two ordering rules — the load-bearing invariant of the system:

> **Barrier 1 — persist-decision-before-dispatch.** The planner's chosen next step (the full StepSpec) is committed to the database **before** any worker is dispatched. *(Physically: TX1, §19.)*
>
> **Barrier 2 — persist-result-before-next-decision.** A worker's result is committed **before** the planner is asked for the step after it. *(Physically: TX2, §19.)*

**Why:** Barrier 1 means a crash never loses a decision — recovery re-dispatches the persisted decision instead of re-asking the planner, which is what makes a non-deterministic planner safe inside a durable system. Barrier 2 means the planner's view of history is always complete — no finished work is ever invisible to the next decision.

Recovery is therefore not a replay engine. It is a read: load the **frontier** (the ordered set of completed steps and their outputs, plus any step holding a decision but no result), and either re-dispatch the pending step or ask the planner what's next. The past is data, never re-derived.

Durability is **synchronous by design**: both barriers are commits on the critical path. StateFlow's target pipelines are step-costly (seconds-to-minutes of OCR, inference, transformation, often with an API bill), so milliseconds of commit latency are noise — while the entire correctness argument reduces to "these writes committed, in this order, before those actions." No buffered or asynchronous durability mode is offered; any relaxation reopens crash windows.

---

## 3. System Components and Terminology

| Component | Responsibility |
|---|---|
| **Orchestrator** | Drives the step loop; the **only writer** of state |
| **Storage** | The single source of truth (Postgres reference implementation; abstracted, §20) |
| **Planner** | Answers "what's next"; a side-effect-free external decision endpoint |
| **Worker** | The external HTTP endpoint that performs actual work |

**The word "restart" in this document refers exclusively to an orchestrator process restart — nothing else.** A planner or worker going down manifests to the system only as "a response did or did not arrive in time" (§6); storage failure halts the system (§9).

**Vocabulary:** a **workflow** is a reusable definition (planner config + settings); a **run** is one execution instance (`run_id`); a **step** is one unit of work within a run; an **attempt** is one specific dispatch of a step.

Two identity fields, never conflated:

| Field | Identifies | Lifetime |
|---|---|---|
| `step_id` = `{run_id}:{step_name}` | The step, within its run | Constant across all retries |
| `attempt_id` (UUID) | One dispatch of that step | New value on every (re-)dispatch |

**Clock rule:** every timestamp is assigned by the system side — the database's `now()` at transaction commit. Timestamps are **never** taken from worker or planner payloads. *Why:* a single Postgres is a single authoritative clock; process clocks drift across restarts and hosts, and externally reported times are untrusted.

---

## 4. The State Model

### 4.1 Stored vs. Derived — the Governing Principle

> **Derive what is a pure function of immutable data; store what is a judgment made at a moment in time under then-current configuration.**

| Field | Strategy | Why |
|---|---|---|
| `attempts.status` | Stored | The event truth itself — the source all else derives from |
| `steps.status` | Stored | DLQ is a **judgment** made against the then-current retry limit X, and X is mutable config. Deriving it (`attempt_count >= X`) would let historical DLQ verdicts flip when X changes or when replay resets the counter. Judgments are recorded, not recomputed |
| `runs.status` | Stored | Recovery's startup scan (`WHERE status='running'`) must be an index lookup; deriving run status would require scanning every run's steps |
| `steps.output` | Stored (physical signal) | null vs. non-null **is** the completion truth — the landing point of Barrier 2. Written in the same transaction as `status=done` (TX2), so the two cannot diverge; if an implementation defect ever makes them diverge, **output wins** (acceptance clause) |
| `steps.attempt_count` | Stored | The retry budget is explicit single-writer state: incremented only in TX3, reset only in TX5, touched nowhere else |
| last step of a run | Derived (index lookup) | §14.2 |
| last attempt of a step | Derived (pointer) | `steps.current_attempt_id`, maintained by TX1/TX4/TX5 |

### 4.2 The Three-by-Three-by-Three State Space

**Run — three states:** `running` (in progress — waiting on the planner, waiting on a worker, or orphaned pending recovery) · `done` (clean terminal state; never touched again, never labeled) · `DLQ` (awaiting human intervention; the only exit is replay).

**Step — three states:** `running` ("a decision is persisted but completion is not confirmed" — the entire span) · `done` (output persisted) · `DLQ` (attempt_count reached the limit X at judgment time; terminal unless replay resets it).

**Attempt — three states, with `failed` carrying a mandatory reason:**

| Status / reason | Meaning |
|---|---|
| `running` | Created (its clock is ticking), no terminal outcome yet |
| `done` | A success report passed CAS validation |
| `failed` / **worker_reported** | The worker explicitly reported failure (sync non-2xx, or async fail callback) |
| `failed` / **timeout** | The attempt exceeded its timeout with no valid report |
| `failed` / **malformed** | The worker "succeeded" but the content is unusable: a sync 2xx whose body is not valid JSON, or a specified `output_field` that is absent; or an async callback with valid ids but unparseable output. *Why a distinct reason:* a success whose output cannot be extracted cannot feed the next step — it is a real failure, and it must be distinguishable from "no response" when debugging |
| `failed` / **orphaned** | Claimed by recovery after an orchestrator restart: the process death killed the attempt's timer and its waiting goroutine, leaving a `running` attempt in the database that nothing in the world is waiting on. Recovery pronounces it failed before re-dispatching (§8.3) |

All four failure reasons take **exactly the same path** (TX3) and consume the budget equally. *Why:* the classification exists for humans (DLQ context, triage, debugging), not for the machine to branch on — fewer branches, more verifiable state machine.

**Other key step columns:** `decision` (JSONB — the planner's full StepSpec; recovery re-dispatches from here, which is what guarantees the planner is asked exactly once per step), `seq` (the sole ordering source for history — never order by timestamp or step_id), `current_attempt_id` (the CAS comparison target).

**Attempt ordering:** attempts carry `attempt_id`, `created_at`, `resolved_at` (there is no attempt_number column; `created_at` was named `dispatched_at` pre-v1.0 — the row is inserted at TX1/TX4 commit, *before* the actual dispatch, and it is the timeout anchor). Ordering is by `created_at` under the clock rule of §3; attempts of one step are created serially by a single loop goroutine, so timestamp ordering is always valid.

---

## 5. The Step Loop

**Setup.** `POST /workflows` submits the workflow definition — planner config (type `static` or `http`, plus its content), timeout defaults, per-step retry limit X — persisted in TX-W. `POST /workflows/{id}/runs` submits `workflow_input`, creates the run (`running`, TX0), and starts one loop goroutine for that run. **MVP concurrency invariant: one run has exactly one loop goroutine; steps execute strictly serially** (this makes `seq = MAX(seq)+1` safe without locking; DAG parallelism, when it lands, redesigns seq allocation).

**Each loop iteration:**

1. **Read the frontier**: all done steps' outputs of this run, ordered by `seq` ascending.
2. **Ask the planner** with the RunState (`run_id` + `workflow_input` + the ordered full history). The planner answers one of three:
   - **continue** — with a full StepSpec (worker URL, input, mode, timeout);
   - **done** — the run is complete;
   - **fail** — the planner semantically declares the run unworkable (e.g., the input is unprocessable).
3. On **continue** → one transaction: create the step (`running`, `seq`, `attempt_count=0`, `decision=StepSpec`) + create the first attempt (`running`) + set `current_attempt_id` — **TX1, Barrier 1: dispatch only after commit. The attempt's clock starts at creation.**
4. **Dispatch** the worker (delivery format per §13.1: sync = bare input + ID headers; async = JSON envelope).
5. **Await the result**: sync = hold the HTTP connection; async = `select(callback channel, timeout timer)`. Both are one blocking call to the loop.
6. On a success report that passes CAS → one transaction: attempt→done + step→done + write output — **TX2, Barrier 2: return to step 1 only after commit.**
7. On failure (any of the four reasons) → TX3; if the budget is not exhausted → wait the retry delay → TX4 (new attempt) → back to step 4.
8. Planner says **done** → run→done (TX7). Planner says **fail** → run→DLQ with reason `planner_declared_fail` (TX8).

---

## 6. The Timeout Doctrine

> **Philosophy: the responsibility of estimating how long a task takes is returned to the user. StateFlow is a reliability layer, not a workload-prediction layer.**

**Two knobs — do not confuse them:**

| Knob | Value | Meaning |
|---|---|---|
| **timeout** | default 60 s; overridable at workflow level; overridable again per step (in the StepSpec) | The lifetime ceiling of one attempt — persist → dispatch → execute → report, end to end |
| **retry delay** | fixed 5 s (MVP) | The cooldown between an attempt being pronounced failed and the next attempt being created. *Why:* don't hammer a struggling worker instantly; makes retries observable in a demo |

They compose: an attempt fails at time T (by any reason); the next attempt is created at T+5s and carries its own timeout.

**The complete timeout taxonomy:**

| Variant | Detection mechanism | Semantics |
|---|---|---|
| Sync worker overruns | HTTP client deadline | attempt→failed(`timeout`), TX3 |
| Async worker goes silent | `select(channel, timer)` — the loop never waits bare on a channel | same |
| Stuck before dispatch (persisted, not yet sent) | the **same timer** — the clock starts at attempt creation | same. *Why:* the entire "decision exists, result doesn't" span is claimed by exactly one rule, no matter how it subdivides internally |
| Planner overruns / unreachable | HTTP client deadline | consumes the **planner budget** (§7.2), not an attempt |

**Timeout equals failure.** An overdue attempt is pronounced `failed(timeout)` and takes the same path as an explicit failure. *Why this is safe:* the danger the old "uncertain ≠ failed" doctrine guarded against was a misjudgment destroying completed work — but both the old and the new model respond to silence with the identical action: re-dispatch, absorb duplicates via worker idempotency, block stale reports via CAS. The only behavioral difference is that the new model charges the budget; the worst consequence is reaching the DLQ (human review) a little earlier — an error in the conservative direction.

**The default must be finite.** An infinite default timeout would make a dead worker undetectable. The default is deliberately small (60 s): better to over-kill into the DLQ, where a human decides, than to hang forever. Mis-killing a long task (e.g., an hours-long LLM job billed twice) is the consequence of a user-side timeout misconfiguration — see §15.

**Planner retry counting lives in memory** and resets on crash. This is acceptable *because a planner call is side-effect-free* — it precedes Barrier 1, so re-asking is always safe. Registered in §18.

---

## 7. Failure Classification, Retry, and Budgets

### 7.1 Worker side

All four attempt-failure reasons (`worker_reported`, `timeout`, `malformed`, `orphaned`) converge on one path:

- **TX3** (one transaction): attempt→failed(reason) + `attempt_count`++; **if the count reaches X, the same transaction** sets step→DLQ + run→DLQ + inserts the DLQ record (reason `worker_retry_exhausted`, context carrying the per-attempt reasons and errors). *Why one blade:* if the Xth failure and the DLQ verdict were separate writes, a crash between them would dispatch one extra attempt that should have gone to the DLQ.
- If the count is below X: wait the retry delay, then **TX4** — create the new attempt + CAS-update `current_attempt_id` → dispatch. **The handover is atomic**: there is no instant at which two attempt_ids are both valid.
- A crash between TX3 and TX4 leaves `step=running, last_attempt=failed, count<X` — recovery's budget check (§8.3) picks this up; the window is claimed.
- Malformed edge cases: an async callback missing valid `step_id`/`attempt_id` gets a 400 and has zero effect (the attempt's own timeout will claim it); valid ids with unparseable output → `malformed` failure.

### 7.2 Planner side

| Situation | Classification | Handling |
|---|---|---|
| Planner answers **fail** | **A legitimate answer, not a failure** | TX8: run→DLQ, reason `planner_declared_fail`; does not consume the budget |
| Timeout / connection failure | `unreachable` | Consumes the planner budget |
| Answer arrives but is malformed (not JSON; missing `status`; `continue` without `worker_url`/`mode`; prose around the JSON) | `malformed` | Consumes the planner budget |

Budget: 30 s per call × 3 total attempts, `unreachable` and `malformed` drawing from the same pool. On exhaustion → **TX9**: run→DLQ, **reason = the category of the final failed attempt** (`planner_unreachable` or `planner_malformed`), with every attempt's detail in the context. *Why the final category:* mixed failures need a deterministic single value, and the last attempt best represents what the operator must fix now; nothing is lost — the full history is in context.

---

## 8. Invariants, the Combination Table, and Recovery

### 8.1 Derived invariants (each upheld by a transaction boundary)

1. A run with no steps is treated as `last_step=done` → ask the planner. *(Claims the crash window between run creation and the first TX1; safe because planner calls are side-effect-free.)*
2. `last_attempt=done ⇒ step=done` (TX2, one blade).
3. `attempt_count=X ⇒ step=DLQ ∧ run=DLQ` (TX3, one blade).
4. `last_attempt=running ⇒ step=running`.
5. `run=done ⇒ last_step=done` (run=done is only ever produced by the planner saying done, and the planner is only asked after `last_step=done` — Barrier 2).
6. `last_step=DLQ ⇒ run=DLQ` (TX3 atomicity).

### 8.2 The run × last_step combination table

**The five legal combinations:**

| Combination | Meaning | Action on orchestrator restart |
|---|---|---|
| run=running, last_step=done (or no steps) | Waiting on a planner decision | Re-ask the planner |
| run=running, last_step=running | Waiting on a worker (or orphaned) | Recovery three-step (§8.3) |
| run=done, last_step=done | Cleanly finished | Untouched |
| run=DLQ, last_step=DLQ | Worker-side DLQ | Untouched |
| run=DLQ, last_step=done | Planner-side DLQ (three reasons, §11) | Untouched |

**The impossible combinations, and why (adversarial review material):**

| Combination | Why it cannot exist |
|---|---|
| run=done, last_step=running or DLQ | run=done is produced only by the planner saying done, whose precondition is last_step=done (Barrier 2) |
| run=running, last_step=DLQ | step→DLQ and run→DLQ are one transaction (TX3); no intermediate state exists |

### 8.3 Recovery (an orchestrator restart, and nothing else)

Scan all `run=running` (an index lookup, §14.2). For each run, by the combination table:

1. **last_step=done (or no steps)** → re-ask the planner.
2. **last_step=running** → three steps, in this fixed order:
   - **(a) Claim the orphan.** If a `running` attempt exists, pronounce it failed(`orphaned`) + count++ — via TX3, including its DLQ branch if the count reaches X. *Why:* the attempt's timer lived in the dead process's memory; nothing will ever pronounce this attempt otherwise. Semantically this is the timeout philosophy applied to ourselves: "this attempt did not complete within its lifetime — because the process carrying it died — therefore it failed."
   - **(b) Budget check.** count ≥ X → already sent to DLQ inside (a); this run is finished here.
   - **(c)** count < X → TX4: new attempt → dispatch.
3. run=done and run=DLQ: never scanned, never touched.

**Two properties worth advertising:**

- **Recovery is re-entrant.** A crash in the middle of recovery is harmless: every action is transaction-guarded, and re-running recovery re-derives from the current database state — an already-claimed orphan is `failed`, not `running`, so it cannot be claimed or counted twice.
- **Crash loops converge.** Every crash consumes one unit of budget (the orphan claim), so even a crash-looping orchestrator drives each in-flight step monotonically toward the DLQ and a human — unbounded retry is structurally impossible.

---

## 9. Component Failure Overview

| Component | Failure form | Handling | Where |
|---|---|---|---|
| **Worker** | No response, disconnect, slow, malformed output, explicit failure | Timeout or the matching reason → budget → DLQ | §6, §7.1 |
| **Planner** | Same, plus a deliberate `fail` verdict | Planner budget → DLQ (three reasons) | §7.2 |
| **Orchestrator** | Crash / restart | Recovery | §8.3 |
| **Storage** | Down / unreachable | **The system halts as a whole** — see below | this section |

**The storage stance (MVP, stated in the open).** When storage is down: the API rejects new requests; each in-flight run's goroutine dies on its first failed write, leaving the run `running` — **orphaned but intact**. When storage returns and the orchestrator restarts, recovery reclaims the orphan through exactly the same path as any crash. Therefore:

- **Storage availability = system availability.** This is deliberate: StateFlow moves the single point of failure out of fragile pipeline code and into a mature, replicable, well-backed-up database.
- **Correctness is unaffected by storage failure.** Every write is an atomic transaction, so persisted state is complete and consistent at every instant — zero data loss, no half-applied state, ever.
- **A standby/secondary database is explicitly rejected.** Dual writes mean two sources of truth, which is a consistency disaster and contradicts the SPOF-into-the-database design.
- Phase 2 proposal (adopted only if it stays simple): an in-process sweeper that detects orphaned runs and re-enters them without a restart — an availability optimization, never a correctness need.

---

## 10. Result Reporting: CAS and the Single Writer

1. Any report — success or failure, sync response or async callback — takes effect **only if** `attempt_id == current_attempt_id` **and** that attempt is still `running`, enforced as a single conditional UPDATE (**CAS-A**).
2. Late, duplicate, or superseded reports are ACKed with 200 and have zero state effect.
3. **A success report arriving after its attempt was pronounced failed(timeout) is rejected** (MVP). Accepting it would create a failed→done resurrection transition and force re-verification of every invariant — not worth saving one re-dispatch. "Late-result salvage" is registered as a future optimization (§18).
4. **The single-writer principle.** The async callback handler does exactly: validate, push into the channel, return 200. It never writes step state; all state writes are performed by the loop. *Why:* a second writer would race the timeout timer for the same attempt's terminal outcome.

---

## 11. The DLQ and Replay

The DLQ is a human queue, not a discard pile. `dead_letter_queue.reason` takes four values. The reason is **purely informational** — replay mechanics are identical — but it drives human triage:

| reason | Combination | Operator action |
|---|---|---|
| `worker_retry_exhausted` | run=DLQ, last_step=DLQ | Read the per-attempt reasons in context (reported / timeout / malformed / orphaned), fix the worker or the timeout, replay |
| `planner_unreachable` | run=DLQ, last_step=done | Fix planner connectivity, replay |
| `planner_malformed` | run=DLQ, last_step=done | Fix the planner's output format (LLM case: the prompt is off-template), replay |
| `planner_declared_fail` | run=DLQ, last_step=done | **Change the input data or the planner's logic first** — replaying unchanged will most likely reproduce the same verdict and burn cost |

The fourth reason is why the field exists at all: without it an operator cannot distinguish "the planner is broken" from "the planner judged this task itself to be unworkable."

**Replay:**

- run=DLQ, last_step=DLQ → **TX5** (one transaction): `attempt_count` reset to 0 + step→running + run→running + new attempt + `current_attempt_id` → dispatch the worker. **The reset is mandatory** — without it the count already equals X and the step returns to DLQ instantly; the button would be decorative.
- run=DLQ, last_step=done → **TX6**: run→running → re-ask the planner.
- Which replay round an attempt belonged to is not stored; when needed it is reconstructed from attempt timestamps and DLQ records. *(replay_round as a column was considered and rejected: it maintains one more concept for an audit trail that is reconstructible after the fact.)*

A `done` run is never labeled, flagged, or annotated. Distinctions are always derived (combination + reason join), never stored as new states.

---

## 12. The Planner Contract

### 12.1 Two built-in planner classes

| Class | Who hosts it | The user provides |
|---|---|---|
| **static** | StateFlow, in-binary — zero deployment | A YAML step list inside `planner_config` |
| **http** | The user | Any HTTP endpoint satisfying this contract; for LLM planners we ship a prompt template and this exact I/O spec |

The planner type and config are persisted in the workflow definition (TX-W). **The loop and recovery reconstruct the planner instance from the workflow row every time** — an orchestrator restart therefore recovers the planner automatically, and static vs. http are indistinguishable to the loop.

The planner **never talks to the database.** It receives everything over HTTP and answers over HTTP.

### 12.2 What the orchestrator sends (RunState)

```json
{
  "run_id": "run-abc-123",
  "workflow_input": { "...": "the original payload from run start" },
  "history": [
    { "name": "ocr", "status": "DONE", "output": { "...": "..." } }
  ]
}
```

`history` is ordered by `seq` ascending and carries **each step's full output**. Every status string on the wire is **UPPERCASE** ("DONE"), identical to the stored values.

> **Registered as the system's weakest link (§18): full-history transmission.** Sending complete outputs risks LLM context bloat and database row bloat. The MVP accepts this for simplicity; the committed fast-follows are **summary-plus-fetch** (send names+statuses; the planner fetches the outputs it wants via `GET /runs/:run_id` — still over HTTP, never the database) and **pass-by-reference** (large payloads live in blob storage; history carries a URI — already valid today since output is opaque JSON). Users must currently design planners with "you will receive the full history" as the premise.

As a first, deliberately mechanical mitigation (registry item #1, Phase 2), the orchestrator now applies a two-tier size guard when marshaling `history` for each `Decide` call, computed fresh from the untruncated stored data every time: any single entry's `output` over 2KB is replaced with a small pointer object (`_truncated`, `size_bytes`, and a note pointing at `GET /runs/:run_id` for the full value), and the cumulative marshaled size of the whole `history` array is capped at 50KB by walking entries most-recent-first — entries that don't fit the remaining budget carry `name`+`status` only, with `output` omitted entirely. `seq`-ascending wire order is unaffected; only per-entry detail level is computed by the recency walk. This changes nothing about what is persisted — Postgres always keeps the full output, reachable in full via `GET /runs/:run_id` — only what is marshaled onto the wire for a given planner call.

### 12.3 What the planner returns (StepDecision)

```json
{
  "status": "continue | done | fail",
  "step": {
    "name": "llm_analysis",
    "worker_url": "http://llm-proxy/run",
    "mode": "sync | async",
    "timeout_seconds": 600,
    "input": { "...": "the payload for this worker" },
    "output_field": "data"
  }
}
```

**Acceptance criteria (these are the `planner_malformed` rules):** well-formed JSON; contains `status`; on `continue`, contains `step.worker_url` and `step.mode`; contains nothing but the JSON — no prose, no markdown fences. The shipped LLM prompt template instructs the model to satisfy exactly these criteria.

---

## 13. The Worker Contract

### 13.1 Dispatch formats (orchestrator → worker; transport-specific)

**Sync — the body is the bare `input`, ids travel as headers:**

```
POST {worker_url}
X-StateFlow-Step-ID:    run-abc-123:ocr
X-StateFlow-Attempt-ID: uuid-...

{ "...": "exactly the input the planner decided — no wrapper" }
```

*Why:* sync's promise is **zero worker modification** — an unmodifiable external API must be able to consume the call as-is. Such APIs ignore unknown headers; modifiable sync workers can read them to key idempotency on `step_id`.

**Async — the body is the envelope** (the worker must echo the ids in its callback, so they must be in-band):

```json
{ "step_id": "run-abc-123:ocr", "attempt_id": "uuid-...", "input": { "...": "..." } }
```

Parsing the `input` content is the user's responsibility in both modes; the planner decides it (§15).

### 13.2 Reporting (worker → orchestrator)

- **Sync**: reply 2xx with a JSON body. The checkpointed output is the whole body, or the subtree named by the step's optional `output_field`. A 2xx whose body is not valid JSON, or whose declared `output_field` is absent, is a `malformed` failure. Non-2xx is `worker_reported` failure.
- **Async**: reply 202 immediately, then call `POST /tasks/complete` (`step_id`, `attempt_id`, `output`) or `POST /tasks/fail` (`step_id`, `attempt_id`, `error`, and an **optional** `retry_after_seconds` — accepted but ignored in the MVP; the field is reserved so the worker-facing contract stays stable when rate limiting lands).

### 13.3 Choosing the mode (per step, freely mixed)

| Worker situation | Mode | Cost to the user |
|---|---|---|
| Cannot be modified (external API, SaaS) | **sync** | Nothing — keep calls short (LB/proxies cut idle connections at 30–90 s) |
| Can be modified / long-running | **async** | Echo two ids and make one outbound POST |

While a long async step runs, the orchestrator's entire cost for it is one parked goroutine and one timer — no pinned connection, no polling, no reserved slot. This is why a single instance can shepherd many concurrent hours-long jobs.

---

## 14. Persistence, Schema, and Performance

### 14.1 Schema (five tables)

```sql
workflows( workflow_id PK, name, planner_type CHECK IN ('static','http'),
           planner_config JSONB, created_at )

runs( run_id PK, workflow_id FK,
      status CHECK IN ('RUNNING','DONE','DLQ'),
      workflow_input JSONB, created_at, updated_at )

steps( step_id PK,               -- "{run_id}:{step_name}", stored not computed
       run_id FK, step_name,
       seq INT,                  -- Nth decision in this run; sole ordering source
       status CHECK IN ('RUNNING','DONE','DLQ'),
       attempt_count INT NOT NULL DEFAULT 0,
       current_attempt_id UUID,  -- deliberately NO FK (insertion-order cycle)
       decision JSONB,           -- full StepSpec; Barrier 1's landing point
       output JSONB,             -- Barrier 2's landing point; null/non-null IS completion
       created_at, completed_at )   -- created_at: renamed from decided_at (name retired with the DECIDED state)

attempts( attempt_id PK UUID, step_id FK,
          status CHECK IN ('RUNNING','DONE','FAILED'),
          failure_reason CHECK IN ('worker_reported','timeout','malformed','orphaned') NULL,
          error TEXT, created_at, resolved_at )   -- created_at: renamed from dispatched_at (inserted at TX1/TX4, before dispatch; the timeout anchor). No attempt_number; order by created_at

dead_letter_queue( id PK, run_id FK, step_id FK NULL,
                   reason CHECK IN ('worker_retry_exhausted','planner_unreachable',
                                    'planner_malformed','planner_declared_fail'),
                   context JSONB,   -- per-attempt reasons/errors, run snapshot
                   created_at )
```

### 14.2 Performance notes (rationale for the derived lookups)

- **last_step**: composite index `(run_id, seq)`; `... WHERE run_id=? ORDER BY seq DESC LIMIT 1` is an **index seek, not a table scan** — milliseconds at millions of rows. A separate "last step pointer" table was considered and rejected: two-table synchronization risk outweighs one index lookup.
- **last_attempt**: no query — `steps.current_attempt_id` is the pointer, transactionally maintained.
- **Recovery scan**: index on `runs.status`.

---

## 15. What Every User Must Know (Operator's Contract)

Stated as consequences and responsibility boundaries, not abstract demands:

1. **Concurrent idempotency, quantified.** One step produces at most X attempts per round (between resets). Worst case — every failure is a timeout mis-kill while the previous execution is still alive — **up to X concurrent duplicate invocations of the same `step_id` may be running simultaneously.** Your deduplication (recommended: a lock or upsert keyed on `step_id`) must withstand that concurrency, not merely sequential duplicates. **If the X you configured exceeds what your worker's deduplication can withstand, the resulting duplicate side effects and data corruption are on your side, not StateFlow's.** StateFlow's side of the bargain: stale reports are blocked by CAS; the database is never polluted by a superseded execution.
2. **Timeout estimation is your judgment.** Too short mis-kills long tasks and double-bills them; too long delays failure detection. StateFlow will not predict your workload (§6).
3. **Data-format expectations.** Workers must parse the transport-specific dispatch format of §13.1 (sync: bare input + ID headers; async: envelope); planners must speak the contracts of §12 (LLM planners: use the shipped template verbatim).
4. **Sync mode and proxies.** A sync worker behind an LB/proxy may be cut at 30–90 s; long tasks belong in async mode.
5. **No built-in authentication.** Run inside a trusted network; put a gateway/mesh in front for production.
6. **The DLQ is a human queue.** Triage by reason (§11); in particular, do not blind-replay `planner_declared_fail`.

---

## 16. Design History *(self-contained; removable without affecting any other section)*

This section records why v1.0 looks the way it does. Nothing elsewhere references it.

**From five states to three.** v0.9's step machine had five states (DECIDED → RUNNING → DONE / FAILED → DLQ) and four recovery rules. Adversarial review found that DECIDED and FAILED did not *own* any recovery rule — their recovery action was identical to RUNNING's ("re-dispatch the persisted decision") — which by the project's own criterion ("a state earns existence only by exclusively owning a rule") made them removable. Collapsing them produced the three-state model and shrank four recovery rules to two combination-table actions.

**The dead zone lesson.** v0.9 itself had once shipped a recovery gap: a crash landing between the FAILED checkpoint and the retry reset left a state no rule claimed, permanently sticking the run. The fix at the time was a fourth rule; the lasting lesson became this project's organizing principle — **every persistable intermediate state must be claimed by exactly one recovery rule** — and in v1.0 the dead zone is gone *by construction*: the state that hosted it no longer exists.

**Timeout as failure.** v0.9 held "uncertain ≠ failed": silence was re-dispatched without charging the retry budget, and async silence had no detector at all (a stalled run waited for an incidental restart; the full remedy — Ghost Mode with patience windows and conditional re-dispatch — was deferred). v1.0 inverted the doctrine after observing that both doctrines respond to silence with the same action (re-dispatch + idempotency + CAS), differing only in budget accounting, whose worst case errs conservatively. The inversion dissolved most of Ghost Mode: a `select` on a timer replaced the deferred machinery, and the orphan-claiming rule turned crash-loop divergence into guaranteed convergence.

---

## 17. Q&A

**Q1. What happens if the orchestrator crashes between persisting the decision and dispatching the worker?**
Nothing is lost. The decision was committed in TX1 before dispatch (Barrier 1); the attempt's timer conceptually started at creation. Recovery finds `run=running, last_step=running`, claims the orphaned attempt, and re-dispatches the persisted decision — the planner is not re-asked.

**Q2. Why is a timeout treated as a failure instead of "uncertain"?**
Because the action is identical either way (re-dispatch, idempotency, CAS); the only difference is budget accounting, and charging the budget errs toward "reach a human sooner" — the conservative direction (§6).

**Q3. Why reject a success report that arrives after the timeout?**
Accepting it creates a failed→done resurrection transition, forcing re-verification of every invariant to save one re-dispatch (§10).

**Q4. Does a storage crash lose data? Why not a standby database?**
Zero loss — every write is atomic, so persisted state is always consistent; runs orphan and are reclaimed on restart. A standby DB means two sources of truth (§9).

**Q5. Why is step status a stored column instead of being derived?**
DLQ is a judgment against the then-current, mutable retry limit X; deriving it lets history flip when config changes (§4.1).

**Q6. Why attempt_count instead of a replay_round column?**
The audit trail replay_round would provide is reconstructible from timestamps and DLQ records; a counter that resets on replay is one concept fewer (§11).

**Q7. Why were DECIDED and FAILED removed?**
Neither exclusively owned a recovery rule; "a decision exists, a result doesn't" is one span claimed by one pair of mechanisms — timeout while alive, orphan-claim on restart (§16).

**Q8. Can a crash loop retry forever?**
No. Each crash's orphan claim consumes one unit of budget, so every step converges monotonically to the DLQ (§8.3).

**Q9. Why may planner retry counting live in memory?**
A planner call precedes Barrier 1 and has no side effects; re-asking is always safe (§6).

**Q10. Doesn't finding the last step scan the whole table?**
No — `(run_id, seq)` index seek (§14.2).

**Q11. What if an LLM planner returns garbage?**
It fails the acceptance criteria, consumes the planner budget as `malformed`, and persistent garbage lands the run in the DLQ with that reason (§7.2, §12.3).

**Q12. How many concurrent duplicate calls can my worker receive for one step?**
Up to X within one round — the quantified idempotency bar (§15).

**Q13. Can I tell "the planner is broken" apart from "the planner judged the task unworkable"?**
Yes — DLQ reasons `planner_unreachable`/`planner_malformed` vs. `planner_declared_fail` (§11).

**Q14. Should the planner speak MCP?**
No. MCP standardizes tool invocation; a planner is a decision endpoint, not a tool. MCP's place is as a worker transport (Phase 3) (§1.2).

**Q15. What's the difference between the timeout and the retry delay?**
Timeout = one attempt's lifetime ceiling (default 60 s, overridable). Retry delay = the 5 s cooldown between a failure verdict and the next attempt (§6).

**Q16. Why are timestamps assigned by the database clock?**
One Postgres = one authoritative clock; process clocks drift across restarts/hosts, and worker-reported times are untrusted (§3).

**Q17. What if the orchestrator crashes after the planner answers ("done", or anything) but before that answer is persisted?**
The unpersisted answer never happened. Recovery sees run=running, last_step=done and re-asks the planner — and an LLM planner may legitimately answer differently this time (even "continue" where it previously said "done"). This does not violate "asked exactly once per step": that guarantee covers *persisted* decisions; an answer that never reached TX1/TX7/TX8 has no standing (§8.2).

---

## 18. Temporary Design Registry

Every known MVP shortcut, consciously registered: the behavior is understood, the impact bounded, the remedy scheduled. **This table is the complement of the system's promises — the explicit list of what is *not yet* guaranteed.** Contributors must not fix registry items ad hoc; each remedy interacts with scheduled features and lands with them.

| # | Item | Current behavior | Impact | Remedy | Phase |
|---|---|---|---|---|---|
| 1 | **Full-history transmission** *(flagged: the system's weakest link)* | The planner receives every step's complete output each call | LLM context bloat; DB row bloat | Summary-plus-fetch + pass-by-reference convention (§12.2) | fast-follow / 2 |
| 2 | Late-result salvage | A success report after a timeout verdict is discarded; the work re-runs | One redundant execution per mis-kill | Optional salvage, only after re-verifying the state machine | 2+ |
| 3 | Planner retry count in memory | Resets on crash | None material — planner calls are side-effect-free | Persist alongside hardening, if ever needed | — |
| 4 | Storage orphans wait for a restart | A store outage orphans in-flight runs until the next orchestrator restart | Availability gap only; zero correctness impact | In-process orphan sweeper | 2 |
| 5 | `retry_after_seconds` ignored | Accepted (optional) on `/tasks/fail`, not acted on | None; contract stability only | LLM-aware rate limiting | 2 |
| 6 | Hardcoded assembly in `main.go` | Store/transport/retry wiring fixed in code | None until alternative impls exist | Config-driven assembly | 2 |
| 7 | Init-only migration | Schema applied by Postgres first-boot hook; no versioning | Fine for exactly one migration file | Real migration tool at migration 002 | 2 / on demand |
| 8 | No orchestrator healthcheck | Distroless image has no shell; liveness verified externally | Compose/K8s cannot gate on readiness | `/healthz` + self-check subcommand | 1.5–2 |

---

## 19. The Atomic Transaction Ledger

Every entry below **must** be implemented as a single database transaction. This ledger is simultaneously the correctness spec, the store-interface derivation source (§20), and the acceptance checklist.

| ID | Contents | Notes |
|---|---|---|
| TX-W | Create workflow definition (incl. planner type + config) | Single write; planners are rebuilt from this row after restart |
| TX0 | Create run (`running`) | Single write |
| TX1 | Create step (decision, seq, count=0) + first attempt + set current_attempt_id | **Barrier 1: dispatch only after commit.** Attempt clock starts |
| TX2 | attempt→done + step→done + write output | **Barrier 2: ask the planner only after commit** |
| TX3 | attempt→failed(reason: one of four) + count++; **if count=X, in the same transaction:** step→DLQ + run→DLQ + DLQ record (`worker_retry_exhausted`, per-attempt context) | Verdict and DLQ fall in one blade |
| TX4 | New attempt + CAS-update current_attempt_id | No dual-validity instant |
| TX5 | Replay (worker side): count→0 + step→running + run→running + new attempt + current_attempt_id | Reset is mandatory |
| TX6 | Replay (planner side): run→running | |
| TX7 | run→done | |
| TX8 | Planner declares fail: run→DLQ + DLQ record (`planner_declared_fail`) | A legitimate answer; no budget consumed |
| TX9 | Planner budget exhausted: run→DLQ + DLQ record (reason = final attempt's category; full detail in context) | |
| CAS-A | Report validation: `UPDATE ... WHERE attempt_id=? AND status='running'` | Single statement, inherently atomic |

**Verification method:** every gap between two adjacent persisted writes is a crash window. The post-commit state of each transaction above must land in one of the five legal combinations of §8.2 and be claimed by a recovery action (or "untouched"). Check every TX; the model is closed only when all pass. If `steps.status` and `steps.output` ever diverge (implementation defect), **output wins**.

---

## 20. Extension Points

The driver loop speaks only four interfaces; each ships with a reference implementation, and each is an open invitation:

| To add… | Implement… | Reference shipped |
|---|---|---|
| A new way to decide steps | `NextStepPlanner` | StaticPlanner, HTTPPlanner |
| A new way to reach workers (MCP, gRPC, queues) | `WorkerTransport` | sync-hold, async-callback (with timer) |
| **A new durable backend (MySQL, SQLite, cloud KV)** | `StateStore` | Postgres |
| A new retry strategy | `RetryPolicy` | fixed-count, fixed-delay |

**The `StateStore` contract is derived directly from the transaction ledger (§19):** one method per TX (or parameterized merges) plus the read side (LoadFrontier — whose pending information subsumes last-step lookup — ListRunningRuns, GetRun, DLQ queries). **Contract clause: every TXn method must be implemented as a single transaction** — this is interface semantics, not an implementation suggestion. A backend that cannot provide equivalent atomicity is not a conforming implementation. This interface is the entry point for users who prefer a different database.

---

## 21. Roadmap

| Phase | Scope | Key deliverables |
|---|---|---|
| **1 — Agent-Native MVP** *(this document)* | Durable dynamic execution, end to end | Three-state model; two barriers as TX1/TX2; unified timeout doctrine (incl. async timer); persisted budget + orphan claiming (crash-loop convergence); static + HTTP planners rebuilt from persisted config; CAS reporting; DLQ with four-reason triage + replay; containerized delivery |
| **1.5 — Publication** | Presentable and verifiable | CI (crash demo as integration test; ghcr.io publish); README rewrite; lightweight status UI; demo storybook + video; `/healthz` |
| **2 — Hardening & LLM semantics** | Production reliability | Summary-plus-fetch + pass-by-reference (registry #1, the flagged weakest link); in-process orphan sweeper (#4); LLM-aware rate limiting (#5); config-driven assembly (#6); migration tooling (#7); optional late-result salvage (#2) |
| **3 — Scale & ecosystem** | Concurrency, HA, integrations | DAG parallelism (seq redesign); replicated orchestrator via executor-ID ownership (no leader election); per-instance timers superseded by persisted deadlines (Redis); MCP worker transport; observability dashboard; Kubernetes/Helm |

---

StateFlow v1.0 · *Internal design document — not for distribution*
