# Piton — Specification

Items marked ⚠ are decisions Claude inferred where `GRILLING_LOG.md` was silent. They are collected
in §19 and each needs an explicit owner ruling before the section that contains it is treated as
settled.

---

## 0. Status and Authority

This document is the single authority on the behaviour of Piton. If the code and this document
disagree, the code is wrong.

1. Only the owner may ratify a change to this document.
2. `notes/`, `GRILLING_LOG.md` and `legacy/` are explanatory. **None of them may ever be cited as
   the reason the system behaves a certain way.** If a behaviour is real, it is here. If it is not
   here, it is not a rule.
3. `BACKLOG.md` is a parking lot. Nothing in it is promised, and nothing in it may be relied upon.
4. Every non-obvious rule below carries a `Why:` line. The rationale is part of the specification's
   purpose, not decoration.

---

## 1. Governing Principle

> **The database is the only coordination mechanism. Every in-memory structure is a cache that may
> vanish at any instant without affecting correctness.**

This sentence is the tie-breaker for every future question of the form *"should this live in
memory?"*. The answer is always: it may live in memory **as well**, never **instead**.

Three consequences are already settled and are visible throughout this document:

| Consequence | Where it is specified |
|---|---|
| An attempt's deadline lives in a column (`attempts.deadline_at`). The in-process timer is only a latency optimisation | §8.6, §13.3 |
| Run ownership lives in a column (`runs.owner_id`) with an expiry derived from a heartbeat row. Process identity means nothing on its own | §8.2, §8.7 |
| An async result is delivered through a row. A channel would be only a latency optimisation | §8.4, §9.6 |

**Why:** the previous system kept each of these three in process memory, and each one produced a
distinct class of unrecoverable failure — a dead timer that meant nobody would ever declare an
attempt failed, an owner that could not be superseded, and a callback landing on a replica with no
channel to deliver it to.

---

## 2. Purpose and Scope

### 2.1 What Piton is

Piton is a crash-safe orchestration engine for workflows whose **next step is decided one step at a
time**. A component called the planner is asked, after every completed step, what to do next; the
orchestrator persists that decision, dispatches it to a worker over HTTP, records the outcome, and
asks again. Every decision, every attempt and every result is written to a plain relational schema,
so the entire history of a run is inspectable with ordinary SQL from a terminal.

The design target is workflows that are **not knowable in advance** — in particular AI pipelines,
where what to do next depends on what the last step actually produced.

### 2.2 What Piton is not

These are deliberate exclusions, not oversights:

| Excluded | Note |
|---|---|
| **A DAG engine** | Steps form a total order by `seq`. Declared dependency edges are a different product |
| **Fan-out / parallel steps** | One step is in flight at a time |
| **Authentication or authorisation** | Out of scope entirely |
| **A user interface** | The interface is the database and the HTTP read API, seen from a terminal |
| **A message broker or external queue** | It would be a second source of truth |
| **Distributed transactions across workers** | A worker's side effects are its own; Piton never rolls them back |

### 2.3 The audience this document is written for

This project exists partly to demonstrate the design, not only to run it. Rationale is therefore
first-class: every non-obvious rule carries a `Why:`, and a reader who has never seen the system
should be able to use this document as reference material without consulting anything else.

Long derivations, eliminated alternatives and worked examples do **not** belong here — they belong
in `notes/`. The test is: *a field table is a MUST, so it is SPEC; a walkthrough that aids
understanding is notes.*

---

## 3. Definitions

*Nothing earlier in this document uses a term defined here without a forward reference; nothing
later uses a term that is not defined here.*

### 3.1 Actors

| Actor | Uniquely responsible for |
|---|---|
| **Orchestrator** | Driving runs: asking the planner, persisting decisions, dispatching to workers, recording outcomes. It is the only component that writes business state |
| **Planner** | Deciding, given everything that has already happened in a run, what the next step is — or that the run is finished, or that it cannot continue. It decides; it never executes |
| **Worker** | Performing one unit of work over HTTP and reporting success or failure. It never decides what happens next |
| **Operator** | The human. Creates workflows, starts runs, inspects state, replays and cancels |

The planner is a **pure function**: one request in, one decision out. It holds no state between
calls.
**Why:** a stateful planner would need its own crash-recovery story, and there would then be two
recovery designs in one system.

### 3.2 Entities

| Entity | Definition |
|---|---|
| **Workflow** | A definition: which planner to use and the configuration limits that govern runs created from it. It is a template and is never executed itself |
| **Run** | One execution of a workflow, with its own input. The unit of history and the unit of ownership |
| **Step** | One decided unit of work inside a run, at a fixed position `seq`. It carries the planner's decision and, when successful, the output |
| **Attempt** | One execution of a step. A step may have several attempts; each attempt is one dispatch to a worker and one outcome |
| **StepSpec** | The document the planner returns describing a step to be created — a worker URL, how to talk to it, and what to send. Defined field by field in §9.4 |
| **Dead-letter entry** | An append-only historical record that a run stopped because a budget was exhausted or the planner refused to continue |

Containment: a workflow has many runs; a run has zero or more steps ordered by `seq`; a step has one
or more attempts; a run has zero or more dead-letter entries.

### 3.3 Identity and ordering

| Identifier | Type | Notes |
|---|---|---|
| `run_id` | UUID | Assigned at run creation. **Never changes**, including across replay rounds |
| `step_id` | UUID | The identity of a step |
| `attempt_id` | UUID | The identity of an attempt. Also the address of the async callback endpoint |
| `orchestrator_id` | UUID | Generated fresh at each process boot. Stored as a plain string with no foreign key |
| `seq` | integer | The position of a step within its run, starting at 1, contiguous and unique per run |

`step_name` is a **non-unique, optional display label**. A planner may omit it entirely.
**Why:** UUIDs are the identity. Requiring a name would force a lazy planner to invent one, and a
name that is not an identity must never be used as one.

`seq` is assigned by exactly one writer — the orchestrator holding the run's ownership fence —
inside the same transaction that creates the step (§8.2).
**Why:** this is what makes "the last step" a well-defined thing to recover against. It is
load-bearing for §5.5.

### 3.4 The two kinds of state

| | **Business state** | **Coordination metadata** |
|---|---|---|
| What it is | Run / step / attempt status, outputs, budgets, dead-letter entries | `orchestrators.last_seen_at`, `runs.owner_id`, `runs.claimed_at` |
| Who may write it | Only under the rules of §8 | Only claim, heartbeat and release (§8.5, §8.7) |
| Governed by the transaction rules in §8? | **Yes** | **No** |

**Why:** without this distinction, the heartbeat — a write that happens every ten seconds outside
any business transaction — reads as a violation of the ownership rules. It is not one; it is a
different kind of state.

---

## 4. Architecture

### 4.1 Components and the boundaries between them

```
   operator ──HTTP──┐
                    │
   planner ──HTTP──▶ ORCHESTRATOR ──HTTP──▶ worker
             ◀──────      │        ◀──HTTP── (async callback)
                          │
                        SQL
                          │
                          ▼
                      DATABASE
```

| Boundary | Transport | What crosses it |
|---|---|---|
| Operator → orchestrator | HTTP / JSON | Workflow definitions, run creation, replay, cancel, all reads |
| Orchestrator → planner | HTTP / JSON | The run's identity, its input, and a catalogue of completed steps (§9.2) |
| Planner → orchestrator | HTTP / JSON | One of three decisions (§9.3), and optional reads against the same read API the operator uses |
| Orchestrator → worker | HTTP / JSON | An envelope, or a raw body (§9.5) |
| Worker → orchestrator | HTTP / JSON | A result, either as the response to the dispatch or as a callback (§9.6) |
| Orchestrator → database | SQL | Everything else |

The planner's read access and the operator's read access are **one surface, not two** (§10.2).
**Why:** they ask the same questions. Two surfaces would drift apart, as they did in the previous
project.

### 4.2 The driving loop

Everything else in this document is a modification of this loop. A driver is the unit of work
inside the orchestrator that advances one owned run.

```
loop for one owned run:
  1. open a transaction and take the ownership fence (§8.2); zero rows ⇒ stop, silently
  2. read the run's status and the status of its highest-seq step  ("last_step", §5.4)
  3. dispatch on the combination table (§5.5):
       L1 (RUNNING / DONE)    → ask the planner
       L2 (RUNNING / RUNNING) → resolve the outstanding attempt
       anything else          → the run is terminal; stop
  4. persist the outcome of step 3 in one transaction
  5. repeat
```

**Asking the planner (L1)** produces one of three results (§9.3):

| Planner answers | Effect, in one transaction |
|---|---|
| `continue` | Insert a step at `seq = last + 1` with status `RUNNING`, storing the StepSpec verbatim; reset the planner budget; then dispatch (below) |
| `done` | `run → DONE` |
| `fail` | Planner-side dead-letter: write a dead-letter entry and `run → DLQ` (§12.3) |
| the call itself failed | Increment `runs.planner_attempt_count`; if it has reached `planner_max_attempts`, planner-side dead-letter as above; otherwise the loop retries the call |

**Dispatching** is: insert an `attempts` row with status `RUNNING` and
`deadline_at = now() + step_timeout_seconds`, increment `steps.attempt_count`, **commit**, and only
then send the HTTP request.
**Why:** the attempt row must exist before the work does. If the process dies between the two, the
worst case is an attempt that was never dispatched and will time out; if the order were reversed,
work could be in flight with no row for a callback to write to and no deadline to expire it.

**Resolving the outstanding attempt (L2)**:

| Attempt state | Action |
|---|---|
| `RUNNING`, deadline not passed | Wait — for the HTTP response in sync mode, by polling the attempt row in async mode (§8.4) |
| `RUNNING`, deadline passed | Expire it: `attempt → FAILED(timeout)`, then fall through to the budget check |
| `DONE` | Copy the attempt's output to the step, `step → DONE`, loop back to L1 |
| `FAILED` | Budget check: `steps.attempt_count < step_max_attempts` ⇒ dispatch a new attempt; otherwise worker-side dead-letter — `step → DLQ` and `run → DLQ` in one transaction (§12.2) |

### 4.3 Ownership and liveness

A run has an **owner**: the `orchestrator_id` of the process currently driving it. Ownership exists
so that two orchestrators never make decisions about one run at the same time.

Ownership **expires**, and it expires because of the orchestrator, not because of the run:

- An orchestrator writes one heartbeat row (`orchestrators.last_seen_at`) every heartbeat interval.
- An orchestrator is **live** iff `last_seen_at > now() - lease_ttl`.
- A run may be claimed if it is `RUNNING` and its owner is absent (`NULL`) or not live (§8.5).

**Why liveness belongs to the process, not to the run:** the alternative — an expiry column on every
run, renewed by a ticking `UPDATE` — rewrites every running run's row every few seconds, producing
continuous churn on the busiest table in the system, and mixes coordination metadata into business
state. One row per process is O(1) regardless of load, and `runs.owner_id` then changes only on the
rare, meaningful events of claim, release, and a run becoming terminal (§8.7).

**Why ownership must expire at all:** if it did not, a dead orchestrator would own its runs forever
and something else would have to detect its death and clear the field — which is the original
problem restated. Renewal *is* the liveness signal.

Renewal is a **liveness signal, not a progress signal**. A run waiting three hours on a single
worker call keeps its owner the whole time, because the process holding it is alive and ticking.

### 4.4 Deployment shape

The orchestrator is **stateless**. Everything it needs to resume is in the database.

Multiple replicas must remain **possible**. This is a constraint on the design — no rule in this
document may foreclose it — and **not a feature being built**. With a single replica, the
non-owner callback exception of §8.4 is simply never exercised.
**Why it is specified anyway:** it determines the async delivery mechanism (the driver polls the
attempt row rather than blocking on an in-memory channel), which would be awkward to retrofit.

**Container topology.** The orchestrator and the database each run in their own container. The
database is not embedded in the orchestrator image and is not assumed to be on the same host.

**Configuration file.** The orchestrator reads one YAML configuration file at boot. It declares at
minimum the storage backend and its connection details, the HTTP listen address, the sweep interval,
the heartbeat interval and the lease TTL. **The storage backend is a value in that file**, in
support of the storage abstraction of §7 — Postgres is today's only implementation, not a
hard-coded assumption.

**Per-scenario environments.** Each milestone or demo scenario owns one directory containing one
`docker-compose.yml` and one hand-run demo script. Automated tests reference that same compose file
rather than defining an environment of their own.
**Why:** the owner's manual demo run and the automated suite must run against an identical
environment definition, or a green suite proves nothing about what he saw by hand.

---

## 5. State Model

### 5.1 Run states

| State | Entered when | Left when |
|---|---|---|
| `RUNNING` | The run is created; or a replay takes it out of `DLQ` | It reaches any other state |
| `DONE` | The planner answers `done` | Never — terminal |
| `DLQ` | A step exhausts `step_max_attempts`, or the planner refuses or exhausts `planner_max_attempts` | By an explicit replay (§14) or an explicit cancel (§15) |
| `CANCELLED` | The operator cancels a `RUNNING` or `DLQ` run | Never — terminal |

### 5.2 Step states

| State | Entered when | Left when |
|---|---|---|
| `RUNNING` | The step is created from a `continue` decision | It reaches any other state |
| `DONE` | An attempt reports success | Never — terminal |
| `DLQ` | The step's attempts reach `step_max_attempts` without success | By a replay of the run (§14) |
| `CANCELLED` | The run is cancelled while this step is `RUNNING` | Never — terminal |

### 5.3 Attempt states

| State | Meaning |
|---|---|
| `RUNNING` | Dispatched, outcome not yet written |
| `DONE` | The worker reported success. `attempts.output` holds the response body verbatim |
| `FAILED` | The attempt did not succeed, for one of the reasons below |

`failure_reason` is a **diagnostic label, not a distinct mechanism**, and no value below changes
`steps.attempt_count`: §4.2 burns one unit of budget when an attempt is **dispatched**, not when its
outcome is written. `cancelled` is the exception in the other direction — the cancel transaction
zeroes the counter (§5.7):

| `failure_reason` | Meaning |
|---|---|
| `worker_error` | The worker replied, in Piton's envelope, that the work failed |
| `transport_error` | The HTTP exchange did not produce a usable reply **before `deadline_at`** — non-2xx, connection refused, DNS failure, connection reset |
| `invalid_response` | A reply arrived, but could not be parsed as the mode requires |
| `timeout` | **`deadline_at` passed** before any outcome was written |
| `orphaned` | `timeout`, where the attempt's dispatching orchestrator was not live when the attempt was expired |
| `cancelled` | The run was cancelled while this attempt was `RUNNING`. **The step's `attempt_count` is zeroed** (§5.7) |

**Why `invalid_response` is its own value rather than folded into `worker_error` or
`transport_error`:** the three name three different repairs — the worker's business logic, the
worker's output format, the network — and the operator picks his next move from this column. It is
also an enumerated value on an existing row, so a value introduced later would leave every earlier
attempt mis-classified.

**`timeout` and `transport_error` are decided by the clock, not by the error's shape.** An attempt
is `timeout` **only** if `deadline_at` has passed; a connection refused at second 3 of a 300-second
budget is `transport_error`.
**Why this is spelled out:** an implementation that dispatches under a deadline-bearing context sees
both cases surface as one error from one HTTP call, and collapsing them is the path of least
resistance. The operator then reads "timeout" on an attempt that was refused instantly, and
misdiagnoses a dead worker as a slow one. The two also point at different fixes — raise the budget,
versus fix the address.

**Why `orphaned` is only a label:** in the previous design an orphaned attempt needed its own
recovery path, because the timer that would have failed it died with its process. With
`deadline_at` in a column, any live orchestrator expires any overdue attempt, and "was its owner
alive?" becomes a question for the operator rather than a branch in the code.

### 5.4 The `last_step` convention

Throughout this document, **`last_step`** means the status of the step with the highest `seq` in a
run.

> **A run with no steps is defined to have `last_step = DONE`.**

**Why:** a run with no steps behaves identically to a run whose last step just completed — the
planner is asked next. Without this convention, `run=RUNNING/no steps`, `run=DONE/no steps`,
`run=DLQ/no steps` and `run=CANCELLED/no steps` would each need their own row, and two of them
(a planner answering `done` or `fail` on its very first call) were holes in the previous design.
This one sentence removes four cells.

`last_step` is **derived, never stored**: `SELECT status FROM steps WHERE run_id=? ORDER BY seq DESC
LIMIT 1`, an index seek on `(run_id, seq)`.
**Why:** it is a pure function of data the orchestrator already holds. A stored pointer could
disagree with reality, and §5.5 is the entire recovery design — a stale pointer would turn it into a
lie, to save microseconds.

### 5.5 The run × last_step combination table

**This table is load-bearing.** Recovery performs exactly one of two actions, and every legal state
of the system appears here.

| # | `run` | `last_step` | Meaning | Action when claimed |
|---|---|---|---|---|
| **L1** | `RUNNING` | `DONE` | Waiting on a planner decision (includes the no-steps case) | **Ask the planner** |
| **L2** | `RUNNING` | `RUNNING` | Waiting on a worker, or its owner died | **The claim path:** expire the overdue or orphaned attempt → budget check → re-dispatch, or go to DLQ |
| **L3** | `DONE` | `DONE` | Cleanly finished | Never scanned |
| **L4** | `DLQ` | `DLQ` | Worker-side DLQ | Never scanned. Exit: replay, or cancel |
| **L5** | `DLQ` | `DONE` | Planner-side DLQ | Never scanned. Exit: replay, or cancel |
| **L6** | `CANCELLED` | `CANCELLED` | Cancelled while a step was in flight | Never scanned |
| **L7** | `CANCELLED` | `DONE` | Cancelled while waiting on the planner, or cancelled out of a planner-side DLQ | Never scanned |
| **L8** | `CANCELLED` | `DLQ` | Cancelled out of a worker-side DLQ; the step keeps its DLQ verdict as a historical fact | Never scanned |

"Never scanned" is mechanical, not a convention: the sweep filters on `status = 'RUNNING'` (§8.6).

### 5.6 Impossible combinations

Each of these is impossible because of a transaction boundary, not because of a check somewhere.

| `run` | `last_step` | Why it cannot exist |
|---|---|---|
| `RUNNING` | `DLQ` | `step → DLQ` and `run → DLQ` are one transaction; no instant separates them |
| `RUNNING` | `CANCELLED` | `step → CANCELLED` occurs only inside the cancel transaction, which sets `run → CANCELLED` in the same commit |
| `DONE` | `RUNNING` | `run → DONE` is produced only by the planner answering `done`, whose precondition is `last_step = DONE` |
| `DONE` | `DLQ` | Same |
| `DONE` | `CANCELLED` | Same |
| `DLQ` | `RUNNING` | Worker-side DLQ sets both in one transaction; planner-side DLQ requires `last_step = DONE` |
| `DLQ` | `CANCELLED` | Cancel always sets `run = CANCELLED`, never `run = DLQ` |
| `CANCELLED` | `RUNNING` | The cancel transaction terminates a `RUNNING` last step in the same commit |

### 5.7 The uniform cancel rule

One sentence covers all three cancelled combinations with no special cases:

> `run → CANCELLED`. If `last_step = RUNNING`, then `last_step → CANCELLED`, its `RUNNING`
> attempt → `FAILED(cancelled)`, and that step's `attempt_count` is **set to 0**. Otherwise the last
> step keeps the terminal state it already had (`DONE` or `DLQ`).

**Why `attempt_count` is zeroed rather than left as it was:** the number is never read again. A
`CANCELLED` run is terminal (§5.1), and §14 replays only a run that is in `DLQ`, so no budget check
will ever consult it. Leaving a half-spent budget behind would therefore buy nothing and would cost
a rule — what a partly-consumed budget means on a step that will never be dispatched again. Zero is
also the one value that cannot be misread as a verdict about the worker: cancellation is not the
worker's failure, and the step is terminal either way.

**Why this does not contradict "budget is burned at dispatch" (§4.2):** it is not a refund. The
attempts that were dispatched keep their rows and their `failure_reason`, so what happened is still
visible; only the counter that governs a future dispatch is cleared, and there is no future
dispatch.

**Why a DLQ verdict is never rewritten:** it is a historical fact about what happened to that step.
Cancelling the run afterwards does not change what the step did, and L8 exists precisely to record
both truths at once.

---

## 6. Data Model

*Column meanings and their conditional logic live here; the reasoning behind the shape of the schema
lives in `notes/`.*

All JSON-valued columns are handled by the storage layer as opaque bytes (§7.1). The types below are
the Postgres implementation's choices; another backend may choose differently for those columns.

### 6.1 `workflows`

| Column | Type | Null | Meaning |
|---|---|---|---|
| `workflow_id` | UUID | no | Primary key |
| `name` | TEXT | no | Display label. Not an identity |
| `planner_type` | TEXT | no | `static` \| `http` |
| `planner_url` | TEXT | yes | Required iff `planner_type = 'http'`; must be absent otherwise |
| `fetch_base_url` | TEXT | yes | Required iff `planner_type = 'http'`; must be absent otherwise. The base URL this workflow's planner is told to read from, sent verbatim as §9.2's field of the same name |
| `planner_static_steps` | JSON bytes | yes | Required iff `planner_type = 'static'`: an ordered array of StepSpecs (§9.4) |
| `step_timeout_seconds` | INT | no | §11.1 |
| `step_max_attempts` | INT | no | §11.1 |
| `step_retry_delay_seconds` | INT | no | §11.1 |
| `planner_timeout_seconds` | INT | no | §11.1 |
| `planner_max_attempts` | INT | no | §11.1 |
| `created_at` | TIMESTAMPTZ | no | |

Invariants: `planner_url` and `fetch_base_url` are both present iff `planner_type = 'http'`, and
`planner_static_steps` is present iff `planner_type = 'static'` — the two sides are never mixed and
never both absent; all five numeric configuration columns are ≥ 1 except
`step_retry_delay_seconds`, which is ≥ 0 (§11.1).

**Why `fetch_base_url` is a column of the workflow rather than a value in the orchestrator's
configuration file (§4.4):** it is one half of a pair. `planner_url` says where the orchestrator
calls this workflow's planner, and `fetch_base_url` says where that same planner reads back — one
relationship, two directions, so both are settled at the same moment, live in the same row and
expire together. It also lets two workflows name two different routes to the same orchestrator,
which a single process-wide value cannot: a planner inside the cluster and a planner hosted outside
it do not reach the read API by the same address.
**The accepted cost, stated plainly:** §10.1 has no endpoint that modifies a workflow, so a
deployment that changes its externally visible address strands every existing `http` workflow —
including the replay (§14) of a run that is sitting in DLQ under one, which will ask the planner
again and send it an address that no longer answers. The remedy is to create a new workflow. This is
not a new class of cost: `planner_url` is already immutable in exactly the same way and fails in
exactly the same way when a planner moves.

**The built-in static planner.** It holds no state. Asked for a decision, it answers with
`planner_static_steps[n]` where `n` is the number of steps the run already has, and answers `done`
once `n` has reached the end of the array. It never answers `fail`.

**Every element of `planner_static_steps` is a StepSpec, and is validated as one — by §9.4 and §9.8
— at `POST /workflows`, before any run exists** (§16).
**Why one type rather than a reduced static-step form:** two shapes would mean two validators, two
sets of defaults, and a capability (`input_from`, `dispatch_style`) available to one kind of planner
and not the other for no reason. It follows that a static step carrying `timeout_seconds` or
`max_attempts` is a 400 until milestone η, exactly as it would be from an HTTP planner.
**Why validation happens at submission:** otherwise a malformed static plan is discovered by the
driver at run time, where the run cannot progress and cannot fail — it is not the planner refusing,
and no attempt exists to burn budget — so it would sit `RUNNING` forever, reclaimed and re-failed by
every sweep. Validating at submission makes that state unreachable.

### 6.2 `runs`

| Column | Type | Null | Meaning |
|---|---|---|---|
| `run_id` | UUID | no | Primary key. Never changes, including across replay rounds |
| `workflow_id` | UUID | no | The definition this run executes |
| `status` | TEXT | no | `RUNNING` \| `DONE` \| `DLQ` \| `CANCELLED` |
| `input` | JSON bytes | no | The operator-supplied workflow input, stored verbatim |
| `planner_attempt_count` | INT | no | Failed planner calls at the *current* decision point. Reset to 0 by any successful planner call |
| `replay_count` | INT | no | Number of completed replay rounds. Starts at 0 |
| `last_planner_error` | TEXT | yes | Error text of the most recent failed planner call |
| `owner_id` | TEXT | yes | **Coordination metadata.** `NULL` means unclaimed. A plain string; no foreign key |
| `claimed_at` | TIMESTAMPTZ | yes | **Coordination metadata.** When the current owner claimed it |
| `created_at` | TIMESTAMPTZ | no | |

Invariants: `owner_id` and `claimed_at` are non-`NULL` only while `status = 'RUNNING'`, and are
always written and cleared as a pair (§8.7); `planner_attempt_count ≤
planner_max_attempts` of the workflow; the pair (`status`, derived `last_step`) is always one of
L1–L8.

**Why the planner budget is a persisted column and not a loop counter:** §12.2 claims that unbounded
retry is structurally impossible. That claim is **only** true if the counter survives a crash. An
in-memory planner budget resets on every restart, so an orchestrator that crashes while a planner is
broken gets a fresh budget each time and the run never converges to DLQ — the exact failure §12.2
rules out. This column is what makes that sentence true, and it is §1 applied to a budget.

**Why the planner's failures are recorded on the run rather than in `attempts`:** an `attempts` row
belongs to a step, and a planner failure happens where there is no step. `last_planner_error` keeps
the diagnosis in the database (§17.3); the full verdict lands in the dead-letter entry (§12.4).

**Why `replay_count` is a stored counter:** the owner must be able to see, at a terminal, which
round a given attempt belonged to. The alternative — bucketing attempts by timestamp against
dead-letter rows — is reconstructable but not inspectable by eye.

**Why there is no `updated_at`:** it would have to be written by every business transaction, and a
rule that must be remembered in every statement is the same class of mistake §8.2 rejects — one
forgotten transaction and the column silently lies. `created_at` here, `steps.created_at`,
`attempts.started_at` / `finished_at` and `dead_letter_queue.created_at` each record a specific
event and cannot drift.

### 6.3 `steps`

| Column | Type | Null | Meaning |
|---|---|---|---|
| `step_id` | UUID | no | Primary key |
| `run_id` | UUID | no | Owning run |
| `seq` | INT | no | Position in the run, from 1, contiguous. Unique with `run_id` |
| `step_name` | TEXT | yes | Optional, non-unique display label |
| `status` | TEXT | no | `RUNNING` \| `DONE` \| `DLQ` \| `CANCELLED` |
| `decision` | JSON bytes | no | The StepSpec exactly as the planner returned it |
| `attempt_count` | INT | no | Budget consumed. **Not** the number of `attempts` rows |
| `output` | JSON bytes | yes | Set when `status = 'DONE'`: the worker's whole response body, verbatim |
| `created_at` | TIMESTAMPTZ | no | |
| `completed_at` | TIMESTAMPTZ | yes | Set exactly when `status` leaves `RUNNING` |

**A step's completion is signalled by `status = 'DONE'` and by nothing else. No rule, query or
implementation may treat "`output` is present" as meaning the step finished.**
**Why:** a worker may legitimately return the JSON document `null`, or an empty object, as its
result. Once stored, that is a present output belonging to a step that may or may not be complete,
and a backend is free to encode it in a way that makes "absent" and "the value null"
indistinguishable at the SQL level. `status` is unambiguous in every backend.

**Why `attempt_count` is stored rather than derived as `COUNT(attempts)`:** the two numbers
legitimately differ. §5.7 zeroes it on cancellation and §14 resets it on replay, and both leave the
`attempts` rows in place — so a step may hold three attempt rows and an `attempt_count` of 0.
Deriving it would make cancellation and replay consume budget as a side effect.

**Why the whole response body is stored:** Piton does not shape outputs. Selecting a field out of a
worker's response is the planner's job, not the engine's.

### 6.4 `attempts`

| Column | Type | Null | Meaning |
|---|---|---|---|
| `attempt_id` | UUID | no | Primary key. Also the address of the callback endpoint |
| `step_id` | UUID | no | Owning step |
| `run_id` | UUID | no | Denormalised copy of the step's run |
| `attempt_no` | INT | no | 1-based ordering within the step |
| `status` | TEXT | no | `RUNNING` \| `DONE` \| `FAILED` |
| `connection_mode` | TEXT | no | `sync` \| `async`, copied from the StepSpec |
| `deadline_at` | TIMESTAMPTZ | no | When this attempt may be declared failed |
| `dispatched_by` | TEXT | no | The `orchestrator_id` that dispatched it |
| `output` | JSON bytes | yes | On success, the worker's response body verbatim |
| `failure_reason` | TEXT | yes | §5.3 |
| `error_text` | TEXT | yes | Diagnostic text, truncated to 4 KB |
| `started_at` | TIMESTAMPTZ | no | |
| `finished_at` | TIMESTAMPTZ | yes | Set exactly when `status` leaves `RUNNING` |

**Error text is truncated to 4 KB**, here and in `dead_letter_queue.error_text` — one limit, both
tables. The orchestrator truncates before writing; it is never the backend's job.
**Why 4 KB:** this column exists so the operator can read, at a terminal, why something failed
(§17). Four kilobytes is tens of lines — past that, a wall of text stops being diagnosis. Carrying a
worker's full error body was never this column's job.

Invariants, enforced by the backend rather than by the caller:

1. `status = 'FAILED'` **⇒ `failure_reason` is present.**
2. `status ≠ 'FAILED'` **⇒ `failure_reason` is absent.**
3. `finished_at` is present iff `status ≠ 'RUNNING'`.

**Why 1 and 2 are a constraint and not a convention:** a `FAILED` row with no reason is a dead end
for the operator — §17 promises the database explains itself — and a reason attached to a `DONE` row
is a contradiction that a later reader will trust. Both are cheap to make impossible and expensive
to notice.

**Why the attempt carries `output` as well as the step:** the only write a non-owner is permitted to
make is to this table (§8.4). A successful async callback landing on a non-owner must be able to
deposit the result somewhere; the owner promotes it to `steps.output` on its next poll.

**Why `dispatch_style` is *not* on the attempt** although `connection_mode` is: §8.6's claim-time
rule branches on `connection_mode`, and it must do so in SQL, before any decision document has been
parsed. Nothing branches on `dispatch_style` outside the act of dispatching, where
`steps.decision` has necessarily already been read.

**Why `run_id` is denormalised here:** the callback endpoint is addressed by `attempt_id` alone and
must locate the run without a join.

**Why `connection_mode` is persisted:** a new owner claiming a run needs it at claim time (§8.6) —
a sync attempt's HTTP connection died with its previous owner and can be expired immediately, while
an async attempt's callback can still arrive and must be honoured.

### 6.5 `dead_letter_queue`

| Column | Type | Null | Meaning |
|---|---|---|---|
| `dlq_id` | UUID | no | Primary key |
| `run_id` | UUID | no | The run that stopped |
| `step_id` | UUID | yes | The step that exhausted its budget; `NULL` for a planner-side entry |
| `reason` | TEXT | no | One of the five values below |
| `replay_round` | INT | no | The value of `runs.replay_count` when this entry was written |
| `attempt_count` | INT | no | The budget consumed at the moment of the verdict — `steps.attempt_count` for a worker-side entry, `runs.planner_attempt_count` for a planner-side one |
| `error_text` | TEXT | no | Why it stopped. Truncated to 4 KB, as in §6.4 |
| `created_at` | TIMESTAMPTZ | no | |

| `reason` | Side | Meaning |
|---|---|---|
| `worker_budget_exhausted` | worker | The step used `step_max_attempts` without succeeding |
| `planner_unreachable` | planner | The planner could not be called: connection refused, timeout, non-2xx |
| `planner_invalid_response` | planner | The planner replied with something §9.3 / §9.8 rejects |
| `planner_budget_exhausted` | planner | `planner_max_attempts` reached. Set instead of the two above when the round's failures were not all of one kind |
| `planner_declared_fail` | planner | The planner answered `fail`. **Not a failure** — a valid answer (§12.1) |

`step_id IS NULL` already distinguishes the two sides, so no separate column records it.

**Why `reason` is an enumerated column rather than free text inside `error_text`:** the three planner
causes call for three different responses from the operator — fix the network, fix the planner, or
accept the planner's judgement — and only a column lets him ask which one his runs are dying of.
`dead_letter_queue` is append-only (§6.7), so a column added after rows exist is permanently blank
for the history that mattered most; by the §3 admission test this had to be settled now.

### 6.6 `orchestrators`

| Column | Type | Null | Meaning |
|---|---|---|---|
| `orchestrator_id` | TEXT | no | Primary key. A UUID generated once at process boot |
| `started_at` | TIMESTAMPTZ | no | |
| `last_seen_at` | TIMESTAMPTZ | no | The only column a heartbeat touches |

Rows are never deleted by the system. They accumulate at one row per process boot; the table is only
ever hit by primary-key point lookups, so this is disk housekeeping and not a correctness or
performance concern.

### 6.7 Table discipline

| Table | Discipline |
|---|---|
| `workflows`, `runs`, `steps`, `attempts`, `orchestrators` | **Current state.** One row per entity, mutated in place |
| `dead_letter_queue` | **Append-only history.** A row is never updated or deleted, and one run may accumulate many rows across replay rounds |

**Why:** these are two different questions — *"what is true now?"* and *"what happened?"* — and the
previous project's confusion came from asking the second question of a table that only answered the
first. Replay acts on the run's current state; the dead-letter entries are the record of the rounds
that came before.

---

## 7. Storage Requirements

*A backend that does not meet these is not a conforming implementation.*

### 7.1 Interface obligations

The storage interface takes and returns **opaque `[]byte`** for every JSON document — run inputs,
step decisions, step and attempt outputs. It must not assume `jsonb`, or any other backend-specific
type, anywhere in its signatures.

The Postgres implementation stores these as `jsonb` internally. An implementer on another relational
database chooses their own encoding — `TEXT`, `BLOB`, or a native JSON type.

Accepted cost: the Go layer gets no JSON query capability. Nothing in this document requires one.

### 7.2 Required indexes

These are part of the contract, not implementation footnotes. Behaviour specified elsewhere in this
document depends on each of them.

| Index | Depended on by |
|---|---|
| `runs(status)`, **partial: `WHERE status = 'RUNNING'`** | The sweep (§8.6) |
| `runs(owner_id)` | Release on shutdown (§8.7) |
| `steps(run_id, seq)` unique | Deriving `last_step` (§5.4); assigning the next `seq` |
| `attempts(step_id, attempt_no)` unique | Resolving the outstanding attempt (§4.2) |
| `attempts(deadline_at)`, **partial: `WHERE status = 'RUNNING'`** | Expiring overdue attempts |
| `dead_letter_queue(run_id, created_at)` | `GET /runs/{run_id}/dlq` |

**Why the partial index on running runs is called out:** it is what makes the sweep's cost
proportional to **current concurrency** rather than to accumulated history. Terminal runs are not in
the index at all, so a run that finished six months ago costs nothing to skip, and **no purge is
required to keep the system healthy**. A backend without partial indexes may use a plain index on
`status`, which seeks to the `RUNNING` range at the same order of cost.

### 7.3 Atomicity and isolation obligations

1. A single `UPDATE … WHERE … RETURNING` must be atomic: two concurrent executions cannot both
   observe the pre-state and both apply.
2. `SELECT … FOR UPDATE` must take a row lock that blocks a concurrent write to that row until the
   holding transaction commits or aborts.
3. **An `UPDATE` blocked by a row lock must re-evaluate its `WHERE` clause after the lock is
   released.**
   **Why:** without this, a claim that waited on a lock would decide using the snapshot it took
   before waiting, and two orchestrators could both conclude a run was unowned. Postgres provides
   this under `READ COMMITTED`.
4. A transaction must be all-or-nothing across the tables it touches. §5.6's impossible combinations
   are impossible only because of this.

---

## 8. Concurrency Control

### 8.1 The primitive

**Compare-and-set (CAS)** is a write whose effect is conditional on the current state matching an
expectation, **evaluated atomically with the write**. In SQL that is `UPDATE … WHERE <expected
state>`. **Zero rows affected means the expectation was wrong**, and is not an error condition —
it is the answer.

A check followed by a write is **not** a CAS and is never acceptable here:

```sql
SELECT owner_id FROM runs WHERE run_id = :rid;   -- (1) still mine, good
                                                 -- (2) ← someone else claims it here
UPDATE steps SET status = 'DONE' WHERE ...;      -- (3) no longer entitled; the write lands anyway
```

Piton has exactly **two** CAS predicates. Everything in this section is one of them.

| | **Ownership fence** (§8.2) | **Attempt CAS** (§8.3) |
|---|---|---|
| Question it asks | "Am I still this run's owner?" | "Has this attempt's outcome not been written yet?" |
| Row it tests | the `runs` row | the `attempts` row |
| Predicate | `run_id = :rid AND owner_id = :me` | `attempt_id = :aid AND status = 'RUNNING'` |
| Prevents | Two drivers making **decisions** about one run | One attempt's outcome being written twice, including by a late report |
| Zero rows means | I am out — abort, stop, dispatch nothing | Someone already recorded this — discard the report, reply 409 |
| Who must satisfy it | Every write that constitutes a **decision** | Every write of an attempt outcome, owner or not |

### 8.2 The ownership fence

Every transaction that writes business state opens with:

```sql
SELECT 1 FROM runs WHERE run_id = :rid AND owner_id = :me FOR UPDATE;
-- 0 rows ⇒ roll back, stop the driver, dispatch nothing, tell nobody
```

The driver **tests** whether the run is still its own and walks away if it is not. It does not
signal anyone, and nobody kills it from outside: **a superseded writer discovers it has been
superseded at its next write.**

**Why one row lock at the top rather than the predicate repeated in every statement:** a business
transaction touches `steps`, `attempts` and `runs`. Repeating `AND EXISTS (SELECT 1 FROM runs …)` in
every statement also closes the TOCTOU window, but it is noisy and **one forgotten statement
silently breaks the whole guarantee**. `FOR UPDATE` makes a concurrent claim block on that row until
this transaction ends, so one statement protects the transaction and the protection comes from the
database rather than from the author remembering.

**Why no expiry check is needed in the fence:** if this process stalled and someone else claimed the
run, `owner_id` is no longer `:me` and the fence fires. If nobody claimed it, this process is still
the rightful owner and proceeding is correct.

### 8.3 The attempt CAS

Every write of an attempt's outcome is conditioned on:

```sql
UPDATE attempts SET status = :outcome, ... WHERE attempt_id = :aid AND status = 'RUNNING';
```

This guarantees exactly one writer per attempt outcome. It also rejects **late reports** with no
separate mechanism: once an attempt has been expired, superseded by a retry, or cancelled, it is no
longer `RUNNING`, so a report arriving afterwards affects zero rows and is refused with 409.

**Why no comparison against a "current attempt" pointer is needed:** a superseded attempt has
already been moved out of `RUNNING` by whatever superseded it.

### 8.4 The single exception

**An async callback arriving at an orchestrator that does not own the run may write the `attempts`
table under §8.3 alone.** It must not write `steps`, `runs`, or `dead_letter_queue`.

**Why this exception exists:** in async mode the worker POSTs to a callback URL. With more than one
replica, that POST can land on a replica that does not own the run. If the fence applied without
exception, a result the worker actually computed would be thrown away, the attempt would time out,
and the work would be repeated for nothing.

**Why it is safe:** a report is a **fact about work that already happened**; a decision is about
**what the run should do next**. Facts may be recorded by whoever receives them; decisions belong to
the owner. The only row the exception can touch is one that already admits exactly one winner.

**Why it stops at the `attempts` table:** whether a failed step retries or goes to the dead-letter
queue depends on a budget check, and a budget check is a decision.

**Consequence — how the owner finds out:** the driver awaiting an async result **polls the attempt
row**, once per second, rather than blocking on an in-memory channel.
**Why:** a channel exists only in one process. This is §1 doing its job.

Ownership does **not** move to the replica that received the callback.
**Why:** it would fence the real owner off mid-run for no reason and make ownership follow traffic
routing. Ownership changes only when the owner is absent or not live.

### 8.5 Claiming

Claiming is **one atomic statement**:

```sql
UPDATE runs r SET owner_id = :me, claimed_at = now()
WHERE r.status = 'RUNNING'
  AND (r.owner_id IS NULL
       OR NOT EXISTS (SELECT 1 FROM orchestrators o
                      WHERE o.orchestrator_id = r.owner_id
                        AND o.last_seen_at > now() - :lease_ttl))
RETURNING r.run_id;
```

Because it is one statement taking a row lock, **exactly one orchestrator wins each run** even when
every replica sweeps at the same instant. There is no designated sweeper and no election.

**Why claiming needs no load balancer in front of it:** claiming *is* the load balancer — whoever
has capacity claims. A balancer would need liveness knowledge of its own, and a dead replica's runs
would stay stranded until its membership view converged.

### 8.6 The sweep

Every orchestrator runs its own sweep on a fixed interval, **5 seconds by default**. It scans **the
database only** — there is no in-process registry of live drivers.
**Why 5 seconds:** the sweep interval is the width of §13.3's uncertainty window — an attempt with
no live owner is declared failed somewhere in `[deadline, deadline + sweep_interval]`. Five seconds
makes that window negligible against any realistic `step_timeout_seconds`, and the cost is one
indexed query per interval against the partial index of §7.2, which touches only currently running
runs.
**Why:** an in-process registry cannot see other replicas and is lost on restart. `owner_id` plus
the `orchestrators` table **are** the database-visible representation of "is anyone driving this".

The sweep finds runs where `status = 'RUNNING'` and the owner is `NULL` or not live, and claims
them (§8.5). Because it filters on `status = 'RUNNING'`, `DONE`, `DLQ` and `CANCELLED` runs are
never claimed.

**The sweep only claims. It never touches business state.** Expiring overdue attempts, checking
budgets and re-dispatching are done afterwards by the driver that now owns the run, through the
normal fenced path.
**Why:** this keeps "business writes require ownership" true with only the §8.4 exception, instead
of scattering exceptions across the codebase.

**Startup recovery is not a separate code path — it is the first sweep.**

**At claim time**, the new owner resolves the outstanding attempt of an L2 run as follows:

| Attempt's `connection_mode` | Rule |
|---|---|
| `sync` | May be expired **immediately**, regardless of `deadline_at`. Its HTTP connection died with its previous owner, so no report can ever arrive |
| `async` | Runs to its `deadline_at`. Its callback can still arrive and be honoured by the new owner |

An overdue attempt on a run whose owner **is** live is expired by that owner, not by the sweep.

### 8.7 Heartbeat and release

```sql
UPDATE orchestrators SET last_seen_at = now() WHERE orchestrator_id = :me;   -- every heartbeat interval
```

One row per process, O(1) regardless of how many runs the process owns. An orchestrator is **live**
iff `last_seen_at > now() - lease_ttl`. Defaults: heartbeat every 10 s, `lease_ttl` 30 s.
**Why these numbers:** the trade-off is straight — a shorter TTL fails over faster and writes more
often. 30 s tolerates two missed heartbeats before another orchestrator may take over.

On a clean shutdown the orchestrator **releases**: `UPDATE runs SET owner_id = NULL, claimed_at =
NULL WHERE owner_id = :me`. This is an optimisation that makes failover immediate rather than
`lease_ttl` later; correctness does not depend on it.

Coordination metadata is written in exactly **four** places, and nowhere else:

| Operation | Effect |
|---|---|
| **claim** (§8.5) | `owner_id = :me`, `claimed_at = now()` |
| **heartbeat** | `orchestrators.last_seen_at = now()`; the run's own columns are not touched |
| **release**, on clean shutdown | `owner_id = NULL, claimed_at = NULL WHERE owner_id = :me` |
| **any transition of a run out of `RUNNING`** | `owner_id = NULL, claimed_at = NULL`, in the same transaction as the status change |

The fourth is what makes §6.2's invariant — "`owner_id` and `claimed_at` are non-`NULL` only while
`status = 'RUNNING'`" — true rather than merely asserted. It covers all three exits: `→ DONE`
(§4.2), `→ DLQ` (§12.2, worker-side and planner-side alike), and `→ CANCELLED` (§15).

**Why in the same transaction, and not in a tidy-up pass:** a separate pass is a rule that has to be
remembered at every call site, which is the same class of mistake §8.2 rejects — one forgotten
transaction and the column silently lies. Written with the status change, the invariant cannot be
violated even for an instant.

**Why this is not cosmetic tidying:** the driver that writes the terminal status clears its own
ownership in the same breath, so its next ownership fence (§8.2) on that run returns zero rows and
it stops silently — exactly what §4.2 step 1 already prescribes. The fence and the state machine
agree by construction rather than by coincidence.

Cancellation is not doing this "belt-and-braces": it is this rule applied to one of the three exits.

---

## 9. Wire Protocol

### 9.1 Format boundary

| Location | Format |
|---|---|
| Configuration files on disk | **YAML** |
| HTTP request and response bodies | **JSON** |
| Database JSON columns | **JSON** |

**No file may have an extension that disagrees with its content.**
**Why:** the previous repository shipped `.yaml` files whose contents were JSON. YAML on disk earns
its place by permitting comments, which matters for demo and reference material; the wire and the
database have no such need.

### 9.2 Orchestrator → planner (M1)

Planner calls are **always synchronous**. The planner is a pure function.

```json
POST {planner_url}
{
  "run_id": "018f...",
  "workflow_input": { "doc_url": "s3://..." },
  "history": [
    { "step_id": "018f...", "step_name": "ocr", "seq": 1, "status": "DONE",
      "output_bytes": 14203, "attempt_count": 2, "completed_at": "2026-08-10T12:00:00Z" }
  ],
  "fetch_base_url": "http://orchestrator:8080"
}
```

| Field | Type | Required | Meaning |
|---|---|---|---|
| `run_id` | string | yes | The run being decided |
| `workflow_input` | object | yes | `runs.input`, verbatim |
| `history` | array | yes | Catalogue of completed steps, oldest first. May be empty |
| `fetch_base_url` | string | yes | Base URL of the read API (§10.2). The workflow's `fetch_base_url` column (§6.1), verbatim |

`history` row fields: `step_id`, `step_name`, `seq`, `status`, `output_bytes`, `attempt_count`,
`completed_at`.

**`history` is a catalogue only. It never carries outputs** — that is what `output_bytes` is for. A
planner that wants an output fetches it from the read API.

Every step in `history` has `status = "DONE"`.
**Why:** the planner is asked only in state L1, where `last_step = DONE`. This follows from §5.5
rather than being an independent rule.

`attempt_count` is included because it is the planner's **only** window onto failure history —
attempts are not in the catalogue — and it lets a planner react to a flaky worker at the cost of one
integer that already exists.

**The cap.** `history` carries at most the **most recent 100** steps.

> **history MAY be truncated. A planner MUST NOT assume it is complete; use the read API for the
> full list.**

**Why a sentence rather than a `history_truncated` flag:** the contract expectation is identical and
the sentence costs nothing to maintain.

### 9.3 Planner → orchestrator (M2)

Exactly three responses:

```json
{ "status": "continue", "step": { ...StepSpec... } }
{ "status": "done" }
{ "status": "fail", "reason": "cannot determine the next step from the document" }
```

**`StepSpec` is one field of the response, not the response itself.**
**Why:** `done` and `fail` carry no step, and a response whose shape changes with its meaning is the
kind of ambiguity that turned planner-response validation into the previous project's largest time
sink.

Anything else — an unparseable body, an unknown `status`, `continue` without `step` — is a **planner
failure** and consumes planner budget (§9.8).

### 9.4 StepSpec

```jsonc
{
  "step_name": "ocr",                     // optional display label, non-unique
  "worker_url": "https://...",
  "connection_mode": "sync" | "async",
  "dispatch_style": "envelope" | "raw",
  "params": { },
  "input_from": ["<step_id>", ...],
  "timeout_seconds": null,
  "max_attempts": null
}
```

| Field | Type | Required | Meaningful in | Meaning |
|---|---|---|---|---|
| `step_name` | string | no | all modes | Display label. Non-unique; may be omitted entirely |
| `worker_url` | string | **yes** | all modes | Where to POST |
| `connection_mode` | enum | **yes** | all modes | `sync`: the result is the HTTP response. `async`: the result arrives at a callback |
| `dispatch_style` | enum | **yes** | all modes | `envelope`: Piton's own JSON body. `raw`: `params` verbatim, no Piton fields |
| `params` | object | no, default `{}` | all modes | The planner's literal, config-like values |
| `input_from` | array of `step_id` | no | **`envelope` only** | Which completed steps' outputs to assemble into `inputs`. Omitted ⇒ the previous step only. `[]` ⇒ nothing |
| `timeout_seconds` | int | no, default `null` | all modes | Per-step override. **Must be `null` until milestone η** |
| `max_attempts` | int | no, default `null` | all modes | Per-step override. **Must be `null` until milestone η** |

**Why `connection_mode` is required rather than defaulted:** every message carries it explicitly so
that sync assumptions never bake in silently and a later async format break becomes impossible.

**Why both `params` and `input_from` exist:** they compose and neither is expensive. `params` is the
planner's literal values, small and config-like; `input_from` is a reference for bulk data, so the
same bytes are not copied into `steps.decision` and re-sent on every planner call. A planner that
genuinely needs to reshape bulk data fetches it and inlines it into `params`, so no capability is
missing and the cheap path is the default path.

**Why `input_from` defaults to the previous step rather than to everything:** the overwhelmingly
common case is a pipeline, and a default of "everything" would make every worker's input grow with
the run's length.

### 9.5 Orchestrator → worker (M4)

**Envelope** (`dispatch_style: "envelope"`):

```json
// sync
POST {worker_url}
{ "run_id": "018f...", "step_id": "018f...", "attempt_id": "018f...",
  "connection_mode": "sync",
  "params": { "lang": "zh-TW" },
  "inputs": { "018f...": { "text": "..." } } }

// async — identical, plus callback_url
{ "run_id": "018f...", "step_id": "018f...", "attempt_id": "018f...",
  "connection_mode": "async",
  "params": { }, "inputs": { },
  "callback_url": "http://orchestrator:8080/callbacks/018f..." }
```

`inputs` is a map from `step_id` to that step's stored output, assembled by the orchestrator from
`input_from`. The envelope has **no `input_from` field** — the resolution has already happened.

`callback_url` is **present iff `connection_mode = "async"`**, and is **omitted entirely** in sync
mode.
**Why not an empty string:** an empty string is a URL-shaped slot that invites a worker to use it,
which would force this document to define what happens when it does. Omission is unambiguous because
`connection_mode` is always present.

**Raw** (`dispatch_style: "raw"`) — the body is `params`, verbatim, and nothing else:

```json
POST {worker_url}
{ "lang": "zh-TW" }
```

**Why `raw` exists:** any unmodifiable HTTP endpoint — an existing internal API, a third-party API —
is then a valid worker. You cannot add fields to such an endpoint's request body.

A key literally named `input_from` **inside `params`** is ordinary data and is transmitted verbatim,
becoming a top-level key of the raw body. `raw` does not interpret payload content.

### 9.6 Worker → orchestrator (M5)

| Mode | Success | Failure |
|---|---|---|
| **sync + envelope** | Response body `{"status":"success","output":{ }}` | Response body `{"status":"failure","error":"…"}` |
| **sync + raw** | Any 2xx. **The entire response body verbatim is the output** | Any non-2xx. The truncated body is stored as error text |
| **async + envelope** | `POST /callbacks/{attempt_id}` with `{"attempt_id":"…","status":"success","output":{ }}` | The same, with `{"status":"failure","error":"…"}` |

**A transport-level failure is always a failure regardless of body** — non-2xx, connection refused,
timeout. A business-level failure and a transport-level failure burn one attempt alike.
**Why:** the difference is diagnostic, recorded in `failure_reason`, and never affects the state
machine. One rule is one rule.

**The dispatch response in async mode means only "accepted".** 2xx ⇒ the attempt stays `RUNNING` and
awaits the callback; non-2xx ⇒ the attempt fails immediately, because the worker refused the job.

### 9.7 Legal mode combinations

| `connection_mode` | `dispatch_style` | Legal | Milestone |
|---|---|---|---|
| `sync` | `envelope` | **yes** | α |
| `sync` | `raw` | **yes** | θ |
| `async` | `envelope` | **yes** | ε |
| `async` | `raw` | **no** | — |

**Why the fourth cannot exist:** a raw body carries only `params`, so there is nowhere to put
`callback_url`. Supporting it would force the orchestrator to conform to each worker's own callback
shape. A user who needs it writes a small adapter.

### 9.8 Rejected StepSpecs

A StepSpec is invalid if any of the following hold:

1. `worker_url` is missing or not a valid absolute HTTP(S) URL.
2. `connection_mode` or `dispatch_style` is absent or not one of its enumerated values.
3. `connection_mode = "async"` with `dispatch_style = "raw"` (§9.7).
4. `input_from` is present at the StepSpec level together with `dispatch_style = "raw"`.
   **Why:** `input_from` has no meaning in raw mode — there is no `inputs` field to assemble it
   into — so a planner that sent one has misunderstood the mode, and silently ignoring it would let
   the user believe data was being delivered when it was not. A key named `input_from` *inside*
   `params` is unaffected (§9.5).
5. `timeout_seconds` or `max_attempts` is non-`null` before milestone η.
6. An unknown top-level key is present.
   **Why:** the planner did not write the fields the orchestrator expects, and the most common cause
   is a typo — `worker_ur1` silently dropped becomes the error "worker_url is missing", which points
   the author at the wrong line. §16's strictness principle does not weaken because the message came
   from a planner instead of an operator.
   **Accepted cost:** a planner that adds a field of its own breaks against an orchestrator that has
   not learned it yet. There is no forward-compatibility escape hatch, by choice.

**An invalid StepSpec is a planner failure and consumes planner budget** exactly like an unreachable
planner. It never creates a step.
**Why:** the planner produced an answer the system cannot act on, which is the same situation as no
answer at all — and a run whose planner keeps producing invalid StepSpecs must converge to DLQ
rather than loop forever.

---

## 10. HTTP API

*The read endpoints are shared by the operator and the planner; they are one surface, not two.*

### 10.1 Control endpoints

```
POST /workflows                     create a workflow definition
GET  /workflows                     list workflow definitions
GET  /workflows/{id}                read one workflow definition
POST /workflows/{id}/runs           start a run
POST /runs/{run_id}/replay          replay a run that is currently in DLQ
POST /runs/{run_id}/cancel          cancel a run
```

`POST /workflows/{id}/runs` body:

```json
{ "input": { }, "overrides": { } }
```

`overrides` is accepted and **any non-empty value is a 400** until milestone η (§11.2).
**Why the field exists now:** the shape of the run-creation request is a contract other people build
against. Adding a sub-object later is a format change; rejecting a value inside an existing
sub-object is not.

### 10.2 Read endpoints

```
GET  /runs                          list runs, filterable by status
GET  /runs/{run_id}                 one run, with a summary of its steps
GET  /runs/{run_id}/steps           the step catalogue, with attempt summaries
GET  /runs/{run_id}/dlq             the append-only dead-letter history for this run
GET  /steps/{step_id}               one step in full
GET  /steps/{step_id}/output        that step's stored output bytes, verbatim
```

The two-layer split is deliberate: the catalogue is cheap and may be fetched whole, while outputs
may be large and are fetched individually, so a planner pulls only what it needs.
`/output` returns the stored bytes verbatim (§7.1).

**Why the dead-letter history is its own endpoint** rather than a field of `GET /runs/{run_id}`: it
accumulates across replay rounds, and keeping it separate keeps the base run read cheap.

**These endpoints are required early, not late.** A planner must be able to fetch what §9.2's
catalogue cap omits.

### 10.3 Callback endpoint

```
POST /callbacks/{attempt_id}        an async worker reports its result
```

Governed by §8.3 and §8.4. A report for an attempt that is no longer `RUNNING` is refused with 409.

### 10.4 Operational endpoints

```
GET  /healthz                       liveness, including whether storage is reachable
```

### 10.5 Error responses

```json
{ "error": "conflict",
  "message": "run is not in DLQ and cannot be replayed",
  "run_id": "018f...",
  "run_status": "RUNNING",
  "step_id": "018f...",
  "step_status": "RUNNING" }
```

**A rejection states the actual current state, not merely that the request was refused.**
**Why:** the operator's next action depends on what is true now. "409 Conflict" alone forces a
second request to find out.

`error` is a stable machine-readable slug; `message` is human-readable and may change. Beyond those
two, **a rejection carries the identifier and current status of every entity the request named or
would have touched, and omits only those that do not exist** — a `POST /workflows` rejection has no
run to describe, and a run-level rejection on a run with no steps has no step.
**Why report more than the entity that caused the rejection:** the two statuses answer different
questions, and the operator usually needs both. A refused replay is explained by the run's status,
but what he does next depends on the step's. Recording the extra field costs one lookup the handler
has already done.

| Code | Used for |
|---|---|
| 400 | The request is malformed or violates §16 |
| 404 | No such entity |
| 409 | The entity exists but is in a state that forbids this operation |
| 503 | Storage is unreachable |

---

## 11. Configuration

### 11.1 Workflow-level fields

| Field | Default | Range | Meaning |
|---|---|---|---|
| `step_timeout_seconds` | 300 | ≥ 1 | Upper bound on one attempt |
| `step_max_attempts` | 3 | ≥ 1 | **Total** attempts before the step goes to DLQ |
| `step_retry_delay_seconds` | 0 | ≥ 0 | Wait between a failure verdict and the next attempt |
| `planner_timeout_seconds` | 30 | ≥ 1 | Upper bound on one planner call |
| `planner_max_attempts` | 3 | ≥ 1 | **Total** planner call attempts at one decision point before the run goes to DLQ |

**`step_max_attempts` is a total attempt count, not a retry count.** `step_max_attempts = 1` means
one dispatch and no retry.
**Why this is stated so loudly:** the previous system had `retry_limit`, `RETRY_MAX_ATTEMPTS` and a
hard-coded 30-second value, and nobody could tell which governed what. The symmetric `step_*` /
`planner_*` prefixes make the governing actor visible at a glance.

**A value below 1 for either `*_max_attempts` is rejected at submission time with a 400.**
**Why:** it removes the need to define what "zero attempts" would mean — a step that is created but
never executed has no useful semantics — and it follows §16's principle of deciding at submission
time what can be decided at submission time.

`step_retry_delay_seconds` is enforced in memory by the driver. It is **not** a guarantee across a
crash: if the process dies during the wait, the next owner re-dispatches as soon as it claims the
run, and the remainder of the delay is discarded.
**Why this is acceptable:** the delay is a courtesy to the worker, not a correctness property, and
per §1 an in-memory mechanism may never be load-bearing. The default is 0, so nothing in the early
milestones depends on it.
**The cost, stated plainly:** if the delay existed because a worker was rate-limiting, a crash
removes the backoff exactly when the worker can least afford it. The remedy, should it ever be
needed, is a `steps.next_attempt_at` column — a nullable addition where `NULL` means "dispatchable
now", so it can be introduced later with no backfill and no change of meaning to any existing row.

### 11.2 Layering

| Level | Status |
|---|---|
| **Workflow** | Implemented. Set at `POST /workflows` |
| **Run** | Designed in — `overrides` in the run-creation body — **rejected with 400 until milestone η** |
| **Step** | Designed in — `timeout_seconds` / `max_attempts` in the StepSpec — **rejected as an invalid StepSpec until milestone η** (§9.8) |

**The request shapes exist now; the storage for them arrives with η.** `runs` has no `overrides`
column and `steps` has no override columns until that milestone.
**Why the API field exists now but the column does not:** the shape of a request is a contract other
people build against, so `overrides` must be there from the first release — adding a sub-object
later is a format change, while rejecting a value inside an existing sub-object is not. A column, by
contrast, is invisible to every caller, can hold nothing before η by construction, and is a nullable
addition whenever it is wanted. Creating it early would only put a permanently empty column in front
of an operator reading the schema.

**A rejection is a 400, never silence.**
**Why:** a setting silently ignored makes the user believe it took effect. That is the failure mode
§16 exists to prevent.

---

## 12. Failure, Retry and the Dead-Letter Queue

### 12.1 What counts as a failure

For a **worker attempt**: any of the `failure_reason` values in §5.3. Business failure, transport
failure, unparseable response and timeout are all failures and all burn one attempt.

For a **planner call**: an unreachable planner, a timeout, a non-2xx response, an unparseable body,
a `status` that is not one of the three, or an invalid StepSpec (§9.8). A `fail` response is **not**
a planner failure — it is a valid answer, and it sends the run to DLQ immediately without consuming
budget.

**These rules apply to every planner, including the built-in static one, with no exemption.** The
static planner simply cannot fail at run time: §6.1 validates its steps at submission, and it holds
no state and makes no network call, so `planner_attempt_count` never leaves 0.
**Why this is stated rather than left implicit:** an implementation that special-cases the static
planner out of the budget path has added a branch to work around a situation that cannot occur, and
that branch will outlive the reason for it.

### 12.2 Budget and the transition to DLQ

```
step:    attempt dispatched → steps.attempt_count += 1     (§4.2, at dispatch, not at the outcome)
         attempt fails      → attempt_count < step_max_attempts ? dispatch again
                                                                : step → DLQ, run → DLQ
planner: call fails         → runs.planner_attempt_count += 1
                            → count < planner_max_attempts ? call again : run → DLQ
```

`step → DLQ`, `run → DLQ` and the dead-letter entry are written in **one transaction**.
**Why:** it is what makes `run=RUNNING, last_step=DLQ` impossible (§5.6).

**Unbounded retry is structurally impossible.** Every dispatch increments a persisted counter
**before the work begins** (§4.2), so no crash afterwards — including one during recovery — can undo
it, and every in-flight step converges monotonically towards DLQ. A crash loop cannot spin forever;
it burns budget on each pass and stops.

The two counters are incremented at different moments, and the asymmetry is not an oversight: a
worker attempt has a dispatch to hang the increment on, and a planner call does not — there is no
row and no step, so the failure itself is the only event there is (§6.2).

### 12.3 Worker-side versus planner-side DLQ

| | Worker-side | Planner-side |
|---|---|---|
| Cause | A step exhausted `step_max_attempts` | The planner answered `fail`, or exhausted `planner_max_attempts` |
| Combination reached | **L4** — `run=DLQ, last_step=DLQ` | **L5** — `run=DLQ, last_step=DONE` |
| `dead_letter_queue.reason` | `worker_budget_exhausted` | One of the four `planner_*` values (§6.5) |
| `dead_letter_queue.step_id` | The failed step | `NULL` |
| What replay resumes | Re-dispatching that step | Asking the planner again |

The branch is derived from **current state**, never from a dead-letter entry.
**Why:** after several rounds, an old entry and current reality diverge. This is the whole reason
replay targets the run (§14).

### 12.4 What a dead-letter entry records

`run_id`, `step_id` (or `NULL`), `reason`, `replay_round`, `attempt_count`, `error_text`,
`created_at` — see §6.5. It is written once and never modified. A run accumulates one entry per
round it lands in DLQ.

`error_text` records the **most recent** failure, not every failure of the round. The per-attempt
history is already in `attempts` for a worker-side entry, and in `runs.last_planner_error` for a
planner-side one; duplicating it here would make an append-only table grow with data it does not own.

---

## 13. Recovery

### 13.1 What recovery handles

These six situations are guaranteed to be survived:

1. **The orchestrator is killed at any instant.** In-flight runs resume; completed steps never
   re-run.
2. **A single run's driver dies while the process lives** — a storage blip, a panic. Reclaimed by
   the next sweep, because a driver that vanished simply stops renewing nothing and its run's
   ownership is tested against process liveness.
3. **A crash during recovery.** The path is re-entrant and does not double-count: an already-claimed
   attempt is no longer `RUNNING`.
4. **A crash loop.** Each pass burns one unit of budget, so every in-flight step converges to DLQ
   (§12.2).
5. **Storage unreachable at startup.** Fail fast: non-zero exit, and an error message that names
   storage as the cause.
6. **Storage unreachable at runtime.** Runs orphan intact and are reclaimed when storage returns.

### 13.2 What recovery does not handle

These are **published non-guarantees**. They are part of the contract.

1. **Duplicate worker execution.** A re-dispatch may re-run a step that actually completed but whose
   result was never persisted. **The worker must be idempotent on `step_id`.** Worst case, up to
   `step_max_attempts` executions of one step.
2. **Work lost in flight.** A result computed but not persisted is gone and will be recomputed.
3. **Side effects already committed by a worker** — mail sent, payment made — are never rolled back.
4. **Planner non-determinism across a crash.** If a crash preceded persistence of the planner's
   answer, the planner is asked again, and an LLM may answer differently. The "asked exactly once"
   guarantee covers **persisted** decisions only.
5. **Loss or corruption of the database itself.** Zero recovery. The database is deliberately both
   the single source of truth and the single point of failure.
6. **A worker that hangs.** Nothing outside `deadline_at` will ever declare it failed, which is why
   the default timeout is finite and why `step_timeout_seconds` may not be unbounded.
7. **Recovery never auto-replays a DLQ'd run.** Reaching DLQ means a human decides.
8. **Cancellation does not abort work already handed to a worker** (§15).

### 13.3 Timeouts are lower bounds

**A timeout is a lower bound on when failure is declared, not an exact instant.**

| Situation | When failure is declared |
|---|---|
| The run's owner is live | Precisely at the deadline — the driver is already waiting and enforces its own in-process timer |
| The owner is dead, or the run was never claimed | Somewhere in `[deadline, deadline + sweep_interval]` |

**Which is authoritative:** `attempts.deadline_at` in the database. The in-process timer is a
latency optimisation that makes the common case fast and is never load-bearing. Stating the ranking
explicitly is deliberate — "which one wins?" is exactly the ambiguity that produced the previous
project's contradictions.

---

## 14. Replay

**Replay targets the run, not a dead-letter entry:** `POST /runs/{run_id}/replay`.
**Why:** a dead-letter entry is history and diverges from current reality after several rounds. The
previous design exposed `POST /dlq/{entry_id}/replay` and then needed a patch rule saying the
worker-side/planner-side branch must be derived from current state rather than from the entry.
Targeting the run deletes that entire class of confusion.

**The idempotency gate is "is this run in DLQ right now".** Not "has it been replayed before".

| Current run status | Result |
|---|---|
| `DLQ` | Replay proceeds. The transaction that takes the run out of DLQ *is* the gate, so a double-click has exactly one winner |
| `RUNNING`, `DONE`, `CANCELLED` | 409, stating the actual current status (§10.5) |

**What replay does**, in one transaction: increment `runs.replay_count`; `run → RUNNING`; if
`last_step = DLQ`, that step returns to `RUNNING` with `attempt_count` reset to 0; reset
`planner_attempt_count` to 0; clear `owner_id` and `claimed_at` so the next sweep picks it up —
under §8.7 a `DLQ` run already holds both as `NULL`, so this is a restatement for the reader, not a
second mechanism.

**`run_id` never changes.** A run is the unit of history; forking it would duplicate step history
and break "one run, one story". A run may be replayed as many times as it lands in DLQ, and
`dead_letter_queue` accumulates one row per round.

**Accepted limitation: a replay always resumes at the furthest step reached.** If round 1 failed at
step 1 and round 2 fails at step 2, replay resumes from step 2 and never revisits step 1.
**Why it is accepted:** it is unambiguous, and it means "round 2 fails earlier than round 1" cannot
arise, so no frontier truncation is needed. This is a scope decision, not a principle.

**Every replay round must leave an inspectable record.** `runs.replay_count` and
`dead_letter_queue.replay_round` together let the operator see, from a terminal, which round any
given entry belonged to.

---

## 15. Cancellation

**A run may be cancelled if its status is `RUNNING` or `DLQ`.** Cancelling a `DONE` or `CANCELLED`
run is a 409.
**Why `DLQ` is included:** it gives a dead-lettered run an exit other than replay. Otherwise a run
the operator has decided to abandon stays in DLQ forever.

The transition is §5.7's uniform rule, applied in one transaction:

```sql
UPDATE runs SET status = 'CANCELLED', owner_id = NULL, claimed_at = NULL
WHERE run_id = :rid AND status IN ('RUNNING', 'DLQ');
-- plus, if the last step is RUNNING: step → CANCELLED with attempt_count = 0,
--                                    its RUNNING attempt → FAILED('cancelled')
```

Resulting combinations: **L6**, **L7**, **L8** (§5.5). If the last step's attempt has already
finished successfully, the step is `DONE` and both the step and the attempt are left alone — only
the run becomes `CANCELLED`.

**Cancellation needs no new mechanism.** It races the driver and the race is resolved by the
ownership fence: once the cancel commits, the driver's next write finds `owner_id` no longer its own
and the driver walks away. Cancelling the driver's in-process context as well is a **speed**
optimisation, never the correctness mechanism.

**Published non-guarantee: cancellation stops the orchestrator from advancing the run; it does not
abort work already handed to a worker.** A worker dispatched moments before the cancel runs to
completion, and its report is refused by the attempt CAS. *Cancelled* means **"this run will make no
further progress"**, not "everything stopped instantly".

---

## 16. Validation

> **Governing principle: better to be too strict and be told we do not support something, than too
> lax and let a user fail silently.**

**Anything decidable at submission time must not be deferred to run time**, where it costs budget,
produces dead-letter entries and wastes triage.

`POST /workflows` returns 400, before any run exists, for:

1. `planner_type` not one of `static` / `http` — this catches the typo `"htp"`.
2. `planner_type = "http"` with no `planner_url` or no `fetch_base_url`, or either of them not a
   valid absolute HTTP(S) URL.
3. `planner_type = "static"` with no `planner_static_steps`, an empty array, or any element that is
   not a valid StepSpec by §9.4 and §9.8 (§6.1) — or carrying `fetch_base_url`, which belongs to
   `http` alone (§6.1).
4. Any unknown key. A silently ignored `retrylimit` makes the user believe a setting took effect.
5. Any configuration field of the wrong JSON type — `"3"` is not `3`, and must not be coerced.
6. Any `*_max_attempts` below 1, or any `*_timeout_seconds` below 1, or a negative
   `step_retry_delay_seconds` (§11.1).

`POST /workflows/{id}/runs` returns 400 for a non-empty `overrides` (§11.2), a missing `input`, or
an unknown key. `overrides` may be `{}`, `null`, or omitted; all three mean "no overrides".
**Why `null` and omission are accepted:** §9.4 gives the same feature's step-level fields the default
`null` and permits omission, and §9.8 rule 5 rejects only a non-`null` value. The two halves of one
feature must not disagree about the same JSON value.

Planner responses are validated at run time under §9.3 and §9.8, because they cannot be seen
earlier.

---

## 17. Observability for the Operator

**Hard requirement: the owner must be able to run the system by hand and see inside it from a
terminal.** There is no UI. Anything whose state is visible only through the automated test suite
has failed this requirement.

1. **Database truth is the interface.** Every fact that matters is a row: the run's status, each
   step's decision and output, each attempt's deadline, failure reason and error text, each
   dead-letter entry, each orchestrator's last heartbeat.
2. **The read API (§10.2) is the second interface**, and it is the same one the planner uses.
3. **Error text is kept in the database**, not only in logs. Standard output stays minimal.
4. **Automated tests exist to guarantee that what the owner saw by hand stays true.** A green suite
   the owner has never seen behind is not evidence that a milestone landed.

---

## 18. Milestones

*Milestones are demo scenarios, not layers. Each one is a user-visible capability that can be shown
end to end.*

| Order | Milestone | Capability demonstrated |
|---|---|---|
| 1 | **α** | Basic happy-path run: static planner, sync envelope worker, run reaches `DONE` |
| 2 | **γ** | Retries and the dead-letter queue: kill a worker, watch attempts burn, watch the run land in DLQ |
| 3 | **β** | Crash recovery: kill the orchestrator mid-run, watch it resume; DLQ'd content is untouched |
| 4 | **δ** | Replay in its variants |
| 5 | **ζ** | Custom HTTP / LLM planner, and switching between planners |
| 6 | **θ** | Sync raw body — an unmodifiable HTTP endpoint works as a worker |
| 7 | **ε** | Async envelope |
| 8 | **η** | Per-step and per-run overrides |
| 9 | **ι** | Cancellation and submission-time validation |

**Why DLQ (γ) comes before crash recovery (β):** the crash-recovery demo's point is partly that
DLQ'd content is *not* touched by recovery and must wait for an explicit replay. That cannot be
shown until DLQ exists.

**Why the custom planner (ζ) sits right after replay:** it is the first milestone that makes the
system useful to someone else, and everything before it is the engine proving it is trustworthy.

Each milestone section states its demo script: what the operator does by hand, and what he must see
in the database afterwards. α's, γ's and β's are written; the rest are written when that milestone
starts.
**Why:** a demo script is an operational verification artefact, not an architectural commitment.
Writing the later ones now would mean inventing detail that no ruling covers.

### 18.1 Milestone α — demo script

**Capability:** an operator creates a workflow with a static planner, starts a run, and the run walks
through every static step against a sync envelope worker and reaches `DONE`.

**Environment** — `demos/alpha/`:

| File | Content |
|---|---|
| `docker-compose.yml` | Three services: `postgres`, `orchestrator`, `worker` (a trivial echo worker) |
| `piton.yaml` | Orchestrator configuration: storage backend and DSN, listen address, sweep interval, heartbeat interval, lease TTL |
| `workflow.json` | The workflow definition to POST |
| `demo.sh` | The commands below, runnable unattended |

**What the operator types:**

```bash
cd demos/alpha
docker compose up -d
# migrations run to completion before the orchestrator serves traffic

curl -sS localhost:8080/healthz

WF=$(curl -sS -X POST localhost:8080/workflows \
       -H 'content-type: application/json' -d @workflow.json | jq -r .workflow_id)

RUN=$(curl -sS -X POST localhost:8080/workflows/$WF/runs \
       -H 'content-type: application/json' \
       -d '{"input":{"text":"hello"},"overrides":{}}' | jq -r .run_id)

curl -sS localhost:8080/runs/$RUN
curl -sS localhost:8080/runs/$RUN/steps
```

**What he must see in the database afterwards:**

```sql
SELECT status, replay_count, planner_attempt_count, owner_id FROM runs WHERE run_id = :run;
-- RUNNING → DONE;  replay_count = 0;  planner_attempt_count = 0

SELECT seq, step_name, status, attempt_count, octet_length(output) FROM steps
 WHERE run_id = :run ORDER BY seq;
-- one row per static step, contiguous seq from 1, every row DONE, attempt_count = 1, output present

SELECT attempt_no, status, connection_mode, failure_reason, finished_at FROM attempts
 WHERE run_id = :run ORDER BY attempt_no;
-- one row per step, all DONE, connection_mode = 'sync', failure_reason NULL, finished_at set

SELECT count(*) FROM dead_letter_queue WHERE run_id = :run;   -- 0

SELECT orchestrator_id, last_seen_at FROM orchestrators;       -- exactly one row, recently seen
```

**Which endpoints α implements.** §10.2 states the complete read surface because it is a contract a
planner author builds against, but α's planner is the built-in static one, which runs in process and
fetches nothing. α therefore implements `POST /workflows`, `POST /workflows/{id}/runs`,
`GET /runs/{run_id}`, `GET /runs/{run_id}/steps` and `GET /healthz`. The remaining read endpoints
land with **ζ**, the first milestone that has a planner able to call them.

**Which validation α implements.** All of §16, and therefore §9.4 and §9.8 in full for every element
of `planner_static_steps`.
**Why it is not deferred to ι:** §6.1 already requires the static plan to be validated at
`POST /workflows` — a malformed StepSpec discovered at run time leaves a run that can neither
progress nor fail — so the validator exists in α regardless, and the rest of §16's list is a few
checks on the same parse. **ι demonstrates validation; it does not introduce it.**

**What α deliberately does not demonstrate:** retries, DLQ, crash recovery, replay, cancellation,
raw dispatch, async, an HTTP planner, or any override. Each has its own milestone.

### 18.2 Milestone γ — demo script

**Capability:** an operator watches a step's attempts burn against a failing worker — recovering on
the last one in one case, and exhausting `step_max_attempts` into the dead-letter queue in three
others, one for each way a worker can fail.

**Environment** — `demos/gamma/`:

| File | Content |
|---|---|
| `docker-compose.yml` | `postgres`, `orchestrator`, and one worker whose behaviour each step selects through its own `params` |
| `piton.yaml` | As α's |
| `workflow-retry.json` | Leg 1 — the worker fails twice, then succeeds |
| `workflow-worker-error.json` | Leg 2 — the worker reports failure in Piton's envelope |
| `workflow-http-500.json` | Leg 3 — the worker replies non-2xx |
| `workflow-unreachable.json` | Leg 4 — `worker_url` names a port nothing listens on |
| `demo.sh` | All four legs against one environment, runnable unattended |

Every leg uses the default `step_max_attempts = 3` (§11.1) and the static planner of α.

**What the operator must see — leg 1, the recovering retry:**

```sql
SELECT status, attempt_count FROM steps WHERE run_id = :run;
-- DONE, attempt_count = 3   (§11.1: a total attempt count, not a retry count)

SELECT attempt_no, status, failure_reason FROM attempts
 WHERE run_id = :run ORDER BY attempt_no;
-- 1 FAILED, 2 FAILED, 3 DONE

SELECT count(*) FROM dead_letter_queue WHERE run_id = :run;   -- 0
SELECT status FROM runs WHERE run_id = :run;                  -- DONE
```

**What he must see — legs 2, 3 and 4, which differ only in `failure_reason`:**

```sql
SELECT status, owner_id, claimed_at FROM runs WHERE run_id = :run;
-- DLQ;  owner_id and claimed_at both NULL   (§6.2, §8.7)

SELECT status, attempt_count FROM steps WHERE run_id = :run ORDER BY seq;
-- the failing step DLQ with attempt_count = 3;  no step created after it

SELECT attempt_no, status, failure_reason FROM attempts
 WHERE run_id = :run ORDER BY attempt_no;
-- three rows, all FAILED, all carrying the leg's reason:
--   leg 2  worker_error       leg 3  transport_error       leg 4  transport_error

SELECT reason, step_id, replay_round, attempt_count FROM dead_letter_queue
 WHERE run_id = :run;
-- exactly one row: worker_budget_exhausted, step_id = the failing step,
-- replay_round = 0, attempt_count = 3   (§6.5, §12.4)
```

Leg 4 is where §5.3's clock rule is visible: the connection is refused immediately, well inside
`step_timeout_seconds`, and the attempt is `transport_error` — **not** `timeout`.

`run = RUNNING, last_step = DLQ` is never observable, because §12.2 writes the step, the run and the
dead-letter entry in one transaction. §5.6 calls that combination impossible; this is the milestone
that can see it.

**Which endpoints γ implements.** None beyond α's.

**What γ deliberately does not demonstrate:** the planner side of §12.3. The static planner cannot
fail at run time (§12.1), so the only way to reach L5 today is an unbuilt planner reporting that it
is unbuilt — which would demonstrate the milestone's absence, not the mechanism. It is demonstrated
at **ζ**, the first milestone with a planner that can genuinely fail. Also absent: crash recovery
(β), replay (δ), and everything α already excluded.

### 18.3 Milestone β — demo script

**Capability:** an operator kills the orchestrator while a step is in flight, restarts it, and
watches the run resume — while a run that had already reached the dead-letter queue is left exactly
as it was.

**Environment** — `demos/beta/`:

| File | Content |
|---|---|
| `docker-compose.yml` | `postgres`, `orchestrator` and one worker. The orchestrator carries `restart: "no"` |
| `piton.yaml` | As α's, but with faster coordination timings — see below |
| `piton-badstorage.yaml` | A second configuration naming a database host that does not resolve, for leg 5 |
| `workflow-survives-crash.json` | Leg 1 — three steps: one completes before the crash, one is in flight during it, one runs after |
| `workflow-dlq.json` | Leg 2 — a run driven into DLQ **before** the crash |
| `workflow-clean-shutdown.json` | Leg 3 — a step in flight when the orchestrator is asked to stop politely |
| `workflow-crash-loop.json` | Leg 4 — a step that is killed on every attempt it is given |
| `demo.sh` | All five legs against one environment, runnable unattended |

**`restart: "no"` is required, not incidental.** A container runtime that restarts the orchestrator
by itself takes the demonstration away from the operator: he never sees the state the crash left
behind, because something else repaired it before he looked. The recovery under test is Piton's
(§13), never the runtime's.

**β does not use §8.6 and §8.7's default timings**, and runs at sweep 2 s, heartbeat 2 s, lease TTL
6 s. §8.5's claim cannot take a run from an orchestrator that is still live, so every kill is
followed by a mandatory wait for the dead process's lease to expire. The values change how *long*
failover takes and never what recovery *does*; §4.4 makes all three configuration precisely so a
deployment may choose, and §8.7's relationship — a lease tolerating two missed heartbeats — is
preserved.

**Legs 1 and 2 share one crash.** The DLQ'd run is created *before* the kill, so the same restart
that resumes one run must leave the other alone.

**What the operator types, and what he must see.**

*Leg 1 — the orchestrator is killed at any instant (§13.1.1):*

```bash
# start the run, wait until the slow step is on the wire, then:
docker compose kill -s KILL orchestrator
```

```sql
SELECT owner_id FROM runs WHERE run_id = :run;
-- unchanged. §8.7 names four writers of coordination metadata and all four live
-- inside an orchestrator process, so a killed process releases nothing
```

```bash
docker compose start orchestrator
```

```sql
SELECT seq, step_name, status, attempt_count FROM steps WHERE run_id = :run ORDER BY seq;
-- every static step present and DONE;  the run resumed past the crash

SELECT s.step_name, a.attempt_no, a.status, a.failure_reason, a.dispatched_by
  FROM attempts a JOIN steps s ON s.step_id = a.step_id
 WHERE a.run_id = :run ORDER BY s.seq, a.attempt_no;
-- the step that completed BEFORE the crash is untouched: one attempt, DONE,
--   the same attempt_id and the same completed_at (§13.1.1, "completed steps never re-run")
-- the step that was IN FLIGHT has attempt 1 FAILED with failure_reason = 'orphaned'
--   (§5.3, §8.6 — a sync attempt is expired immediately at claim time, regardless
--   of deadline_at, because its HTTP connection died with its previous owner)
-- attempt 2 names a DIFFERENT dispatched_by (§6.4): a restarted orchestrator
--   takes a fresh orchestrator_id
```

*Leg 2 — recovery never auto-replays a DLQ'd run (§13.2.7):*

The whole run — its row, every step, every attempt, every dead-letter entry — is digested before the
crash of leg 1 and again after it. **The two digests must be identical.** In addition:

```sql
SELECT status, owner_id, replay_count FROM runs WHERE run_id = :run;
-- DLQ;  owner_id NULL — the sweep filters on status = 'RUNNING' (§8.6), so a DLQ
-- run is never claimed;  replay_count = 0 — reaching DLQ means a human decides
```

*Leg 3 — a clean shutdown releases (§8.7):*

```bash
docker compose stop orchestrator          # SIGTERM, not SIGKILL
```

```sql
SELECT owner_id FROM runs WHERE run_id = :run;   -- already NULL, with nothing else running
```

This is the contrast that gives leg 1 its meaning: §8.7 calls release *"an optimisation that makes
failover immediate rather than `lease_ttl` later; correctness does not depend on it"*, and legs 1
and 3 are what that sentence looks like from a terminal. The orchestrator is then restarted and the
run finishes.

*Leg 4 — a crash loop converges (§13.1.4):*

The orchestrator is killed once per unit of `step_max_attempts`, each time only after the next
attempt has actually been dispatched.

```sql
SELECT attempt_no, status, failure_reason FROM attempts WHERE run_id = :run ORDER BY attempt_no;
-- step_max_attempts rows, all FAILED, all 'orphaned'

SELECT status, attempt_count FROM steps WHERE run_id = :run;   -- DLQ, attempt_count = step_max_attempts
SELECT reason, step_id FROM dead_letter_queue WHERE run_id = :run;
-- exactly one row, worker_budget_exhausted, naming the step
```

If a crash did not burn budget this loop would not terminate. §12.2: *"every dispatch increments a
persisted counter before the work begins, so no crash afterwards — including one during recovery —
can undo it."*

*Leg 5 — storage unreachable at startup (§13.1.5):*

```bash
docker compose run --rm --no-deps orchestrator --config /etc/piton/piton-badstorage.yaml
```

A non-zero exit, and an error message that names storage as the cause. This section fixes those two
properties and **not the wording**.

*Across the legs:*

```sql
SELECT orchestrator_id, last_seen_at FROM orchestrators;
-- one row per process that ran, and exactly one of them live by §8.7's definition.
-- The dead rows are not a defect: §5.3's 'orphaned' is decided against this table
-- long after the process is gone

SELECT count(*) FROM runs WHERE status <> 'RUNNING' AND (owner_id IS NOT NULL OR claimed_at IS NOT NULL);
-- 0 (§6.2, §8.7)
```

**Which endpoints β implements.** None beyond α's.

**Which of §13.1's six situations β demonstrates:** 1, 4 and 5 directly, and 3 without isolating it —
leg 4's kills each land on a process that has just recovered the previous one.

**What β deliberately does not demonstrate.** §13.1.2 — a single run's driver dying while the process
lives — has no external handle: staging it would mean adding a test-only hook to the orchestrator for
the benefit of its own test. §13.1.6 — storage unreachable at runtime — is stageable, but its
assertion is the state leg 1 already reaches, by a slower and far more timing-dependent path. Both
remain guaranteed by §13.1; neither is shown here.
**Why this is stated rather than left silent:** an unlisted absence reads as an oversight, and the
next reader cannot tell whether a situation was considered and set aside or simply forgotten.

Also absent, as in every milestone before δ: replay, cancellation, `raw` dispatch, `async`, an HTTP
planner, and overrides.

**§13.2.1 is visible here and is a non-guarantee, not a defect.** The re-dispatch after a crash
re-runs a step that was already executing, so the demo's worker counts how many times each step
reached it and prints the count. *"The worker must be idempotent on `step_id`."*

---

## 19. Open Assumptions

Everything not yet settled or not yet guaranteed, in one place, so that no reader consults a second
registry to find out whether something is true.

### 19.1 Inferred, awaiting an owner ruling (the ⚠ items)

**None outstanding.** O1–O8 have all been ruled on; identifiers are stable and are not reused. O1 —
the definition of the static planner in §6.1 — was the last, and was ratified on 2026-09-01.

This section is kept rather than deleted: it is where the next ⚠ goes, and its emptiness is itself
the statement that nothing in this document is currently an unratified inference.

### 19.2 Published non-guarantees

The recovery non-guarantees of §13.2, the cancellation non-guarantee of §15, and the timeout
lower-bound rule of §13.3. They are contract, not gaps. One more belongs with them:

**A run's length is unbounded.** Nothing limits how many steps a planner may create. A planner that
answers `continue` forever produces a run that never terminates, and no budget catches it: the
planner budget counts *failures*, and this planner is not failing. The static planner is bounded by
its array; an HTTP planner is bounded only by itself.
**Why this is published rather than fixed:** a `max_steps` limit is an optional configuration field,
and by the §3 admission test an optional field added later costs nothing — so it goes to
`BACKLOG.md` rather than into the current milestone. What would be expensive is discovering the
property later and mistaking it for a defect.

### 19.3 Deliberately unbuilt but designed in

| Thing | Where it is designed | When it is built |
|---|---|---|
| Multi-replica orchestration | §4.4, §8.4, §8.5 | Not scheduled. A constraint on the design, never a milestone |
| Run-level and step-level overrides | §11.2, §9.4 | Milestone η |
| `raw` dispatch | §9.5 | Milestone θ |
| `async` connection mode | §9.5, §9.6, §8.4 | Milestone ε |
| HTTP planner | §6.1 | Milestone ζ |
| Cancellation | §15 | Milestone ι |

Everything else that has been considered and set aside is in `BACKLOG.md`, which is authority on
nothing.

---

## Appendix A. Glossary index

| Term | Defined in |
|---|---|
| Attempt | §3.2 |
| Attempt CAS | §8.1, §8.3 |
| `attempt_id` | §3.3 |
| Business state | §3.4 |
| CAS (compare-and-set) | §8.1 |
| Claim | §8.5 |
| `connection_mode` | §9.4 |
| Coordination metadata | §3.4 |
| Dead-letter entry | §3.2, §12.4 |
| `dispatch_style` | §9.4, §9.5 |
| Driver / the driving loop | §4.2 |
| Envelope | §9.5 |
| Fence (ownership fence) | §8.1, §8.2 |
| Heartbeat | §8.7 |
| `input_from` | §9.4 |
| `inputs` | §9.5 |
| L1–L8 | §5.5 |
| `last_step` | §5.4 |
| Live (orchestrator) | §4.3, §8.7 |
| Operator | §3.1 |
| Orchestrator | §3.1 |
| `orchestrator_id` | §3.3 |
| Owner / ownership | §4.3 |
| `params` | §9.4 |
| Planner | §3.1 |
| Planner-side DLQ | §12.3 |
| Raw (dispatch style) | §9.5 |
| Release | §8.7 |
| Replay | §14 |
| Run | §3.2 |
| `run_id` | §3.3 |
| `seq` | §3.3 |
| Step | §3.2 |
| `step_id` | §3.3 |
| `step_name` | §3.3 |
| StepSpec | §3.2, §9.4 |
| Sweep | §8.6 |
| Worker | §3.1 |
| Worker-side DLQ | §12.3 |
| Workflow | §3.2 |
