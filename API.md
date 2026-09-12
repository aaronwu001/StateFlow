# API.md — the wire contract

This document exists for one reader: **someone writing a client against Piton's HTTP API** who
cannot open `psql`.

## What authority this document has, and what it does not

`CLAUDE.md § 1` places it precisely:

> **Authority on the wire only** — the JSON field names and shapes a consumer builds against. It may
> never state a rule about behaviour: where it describes *what* a value means, it cites the
> `SPEC.md` section that rules it, and if the two disagree, `SPEC.md` wins and `API.md` is wrong.

The reason this file exists at all is that `SPEC.md § 10` fixes the **endpoints** and deliberately
does not fix the **field names**. That was never an oversight: for six milestones the only consumers
were a planner and an operator at a terminal, and `SPEC.md § 17.1` makes database truth the
interface. A browser cannot open a terminal, so the field names became a contract, and a contract
needs somewhere to live.

**Every field below names the database column it carries.** That is the point of the right-hand
column in each table: a client rendering "what the database looks like" should be able to see,
field by field, which row it is looking at.

## Conventions

| | |
|---|---|
| Base URL | Wherever the orchestrator listens. `SPEC.md § 4.4`; the demos publish `localhost:8080` |
| Content type | `application/json` on every request with a body, and on every response |
| Identifiers | UUID strings. `SPEC.md § 3.3` |
| Timestamps | RFC 3339 with nanoseconds, UTC — `2026-09-12T11:41:07.512338Z` |
| Nullable fields | Present and `null`, never omitted, unless a table below says otherwise |
| Authentication | **None.** `SPEC.md § 2.2`: "out of scope entirely". See the warning at the end |

Opaque JSON documents — a run's `input`, a step's `decision`, a step's `output` — are returned
**verbatim**, as the exact bytes stored. `SPEC.md § 7.1` keeps them opaque end to end and
`SPEC.md § 6.3` forbids the engine from selecting anything out of them.

---

## The four entities

Everything the API returns is one of these four rows. A client that understands this table can read
every response below.

| Entity | Table | What it is | States |
|---|---|---|---|
| **Run** | `runs` | One execution of a workflow | `RUNNING` · `DONE` · `DLQ` · `CANCELLED` (`§ 5.1`) |
| **Step** | `steps` | One unit of work the planner decided on | `RUNNING` · `DONE` · `DLQ` · `CANCELLED` (`§ 5.2`) |
| **Attempt** | `attempts` | **One dispatch to a worker.** A step has one row per try | `RUNNING` · `DONE` · `FAILED` (`§ 5.3`) |
| **Dead-letter entry** | `dead_letter_queue` | Why a run stopped. Append-only, accumulates across replays | — (`§ 6.5`) |

**The attempt is where the drama is.** A step carries `attempt_count`, which is a *budget being
spent* (`§ 12.2`), and each attempt row records one spend and why it failed.

### `failure_reason` on an attempt (`SPEC.md § 5.3`)

These four are what a client will actually see; the two below them arrive with later milestones.

| Value | What happened | What a person should fix |
|---|---|---|
| `worker_error` | The worker replied, in Piton's envelope, that the work failed | The worker's business logic |
| `transport_error` | No usable reply before the deadline — non-2xx, connection refused, DNS failure, reset | The network, or the address |
| `invalid_response` | A reply arrived and could not be parsed as the mode requires | The worker's output format |
| `timeout` | `deadline_at` passed before any outcome was written | The budget, or a slow worker |
| `orphaned` | `timeout`, where the dispatching orchestrator was not live when it expired | — |
| `cancelled` | The run was cancelled while this attempt was `RUNNING` | — (milestone ι) |

`timeout` and `transport_error` **are decided by the clock, not by the shape of the error**
(`§ 5.3`). A connection refused three seconds into a three-hundred-second budget is
`transport_error`, not `timeout`.

### `reason` on a dead-letter entry (`SPEC.md § 6.5`, `§ 12.3`)

| Value | Side | Meaning |
|---|---|---|
| `worker_budget_exhausted` | worker | The step used `step_max_attempts` without succeeding |
| `planner_budget_exhausted` | planner | The run used `planner_max_attempts` at one decision point without an answer |
| `planner_declared_fail` | planner | The planner answered `fail`. **Not a failure** — a valid answer |

**This is the field that tells a client whether the worker or the planner killed the run**, and it
is available nowhere else over HTTP.

---

## Control endpoints

### `POST /workflows` — create a workflow definition

`SPEC.md § 10.1`. Every rejection is `SPEC.md § 16`'s, and they all happen here, before any run
exists.

Request: the workflow definition (`SPEC.md § 6.1`). `201 Created`:

| Field | Type | Source |
|---|---|---|
| `workflow_id` | string | `workflows.workflow_id` |
| `name` | string | `workflows.name` |
| `created_at` | string | `workflows.created_at` |

### `POST /workflows/{workflow_id}/runs` — start a run

`SPEC.md § 10.1`. Request:

```json
{ "input": { }, "overrides": { } }
```

`overrides` must be `{}`, `null` or absent: **any non-empty value is a 400** until milestone η
(`§ 11.2`). `201 Created`:

| Field | Type | Source |
|---|---|---|
| `run_id` | string | `runs.run_id` |
| `workflow_id` | string | `runs.workflow_id` |
| `status` | string | `runs.status` — always `RUNNING` at creation (`§ 5.1`) |
| `created_at` | string | `runs.created_at` |

### `POST /runs/{run_id}/replay` — replay a run that is in DLQ

`SPEC.md § 10.1`, `§ 14`. `200 OK`:

| Field | Type | Source |
|---|---|---|
| `run_id` | string | `runs.run_id` |
| `status` | string | `RUNNING` — the run has been revived |
| `replay_count` | integer | `runs.replay_count`, **after** the increment |

`409 conflict` when the run is not in DLQ, and the body carries the run's actual status
(`§ 10.5`). **What a replay does not do is erase anything**: the dead-letter entry and every attempt
from the failed round stay, carrying their `replay_round` (`§ 14`).

---

## Read endpoints

### `GET /runs` — list runs

`SPEC.md § 10.2`. The one endpoint whose result set grows without bound, so its contract is fixed in
SPEC rather than here.

| Query parameter | Default | Meaning |
|---|---|---|
| `status` | all | Repeatable. Only runs in one of `§ 5.1`'s states. An unknown value is a **400** |
| `limit` | 50 | 1–200 |
| `cursor` | — | Opaque; the previous response's `next_cursor` |

Ordering is **newest first by `created_at`, `run_id` breaking ties**, and a cursor resumes exactly
after the run it names.

```json
{ "runs": [ { "run_id": "…", "workflow_id": "…", "status": "DLQ",
              "planner_attempt_count": 0, "replay_count": 0,
              "owner_id": null, "claimed_at": null,
              "created_at": "2026-09-12T11:41:07.512338Z" } ],
  "next_cursor": "…" }
```

| Field | Type | Source |
|---|---|---|
| `runs[].run_id` | string | `runs.run_id` |
| `runs[].workflow_id` | string | `runs.workflow_id` |
| `runs[].status` | string | `runs.status` (`§ 5.1`) |
| `runs[].planner_attempt_count` | integer | `runs.planner_attempt_count` — planner budget consumed at the current decision point (`§ 12.2`) |
| `runs[].replay_count` | integer | `runs.replay_count` — how many times this run has been replayed (`§ 14`) |
| `runs[].owner_id` | string \| null | `runs.owner_id`. **Non-null only while `RUNNING`** (`§ 6.2`) |
| `runs[].claimed_at` | string \| null | `runs.claimed_at`. Written and cleared with `owner_id`, always as a pair (`§ 6.2`) |
| `runs[].created_at` | string | `runs.created_at` |
| `next_cursor` | string | **Omitted when there are no more runs.** Its presence is the only "has more" signal |

The list carries no `input` and no steps: it is the cheap view. Fetch one run for those.

### `GET /runs/{run_id}` — one run, with its step catalogue

`SPEC.md § 10.2`. `200 OK`. Every field of the list view, plus:

| Field | Type | Source |
|---|---|---|
| `input` | any | `runs.input`, **verbatim** (`§ 6.2`) |
| `steps` | array | The step catalogue — see below. **Without attempts** |

### `GET /runs/{run_id}/steps` — the step catalogue, with attempts

`SPEC.md § 10.2`. This is the endpoint a live view polls: **it carries the full attempt detail**, not
a count.

```json
{ "run_id": "…",
  "steps": [ { "step_id": "…", "seq": 1, "step_name": "uppercase",
               "status": "DLQ", "decision": { }, "attempt_count": 2,
               "output_bytes": 0,
               "created_at": "…", "completed_at": "…",
               "attempts": [ { "attempt_id": "…", "attempt_no": 1,
                               "status": "FAILED", "connection_mode": "sync",
                               "deadline_at": "…", "dispatched_by": "…",
                               "failure_reason": "transport_error",
                               "error_text": "piton: worker … answered HTTP 500: …",
                               "started_at": "…", "finished_at": "…" } ] } ] }
```

| Step field | Type | Source |
|---|---|---|
| `step_id` | string | `steps.step_id` |
| `seq` | integer | `steps.seq` — position in the run, from 1, contiguous (`§ 3.3`) |
| `step_name` | string \| null | `steps.step_name` — an optional, **non-unique** display label (`§ 6.3`) |
| `status` | string | `steps.status` (`§ 5.2`) |
| `decision` | object | `steps.decision` — the StepSpec **exactly as the planner returned it** (`§ 6.3`). Read `worker_url`, `dispatch_style` and `connection_mode` from here |
| `attempt_count` | integer | `steps.attempt_count` — **budget consumed, not the number of attempt rows** (`§ 6.3`) |
| `output_bytes` | integer | `octet_length(steps.output)`, `0` when there is none |
| `created_at` | string | `steps.created_at` |
| `completed_at` | string \| null | `steps.completed_at` — set exactly when `status` leaves `RUNNING` (`§ 6.3`) |
| `attempts` | array | One per dispatch. **Only this endpoint populates it** |

| Attempt field | Type | Source |
|---|---|---|
| `attempt_id` | string | `attempts.attempt_id` — also the address of the async callback (`§ 6.4`) |
| `attempt_no` | integer | `attempts.attempt_no` — 1-based, **contiguous across replay rounds** (`§ 6.4`) |
| `replay_round` | integer | `attempts.replay_round` — `runs.replay_count` when this attempt was dispatched (`§ 6.4`). **This is what tells a client which round an attempt belonged to** |
| `status` | string | `attempts.status` (`§ 5.3`) |
| `connection_mode` | string | `attempts.connection_mode` — `sync` or `async`, copied from the StepSpec |
| `deadline_at` | string | `attempts.deadline_at` — when this attempt may be declared failed. **Authoritative** (`§ 13.3`) |
| `dispatched_by` | string | `attempts.dispatched_by` — the `orchestrator_id` that sent it |
| `failure_reason` | string \| null | `attempts.failure_reason` (`§ 5.3`) |
| `error_text` | string \| null | `attempts.error_text` — diagnostic text, **truncated to 4 KB** by the orchestrator before writing (`§ 6.4`) |
| `started_at` | string | `attempts.started_at` |
| `finished_at` | string \| null | `attempts.finished_at` — set exactly when `status` leaves `RUNNING` |

**A step's completion is signalled by `status = "DONE"` and by nothing else.** `SPEC.md § 6.3`
forbids reading anything into the presence or absence of an output: a client must never treat
`output_bytes > 0` as "finished".

### `GET /runs/{run_id}/dlq` — why the run stopped

`SPEC.md § 10.2`. Append-only, **oldest first**, and it accumulates across replay rounds. A run with
no entries is an **empty list with a 200**, not a 404.

```json
{ "run_id": "…",
  "entries": [ { "dlq_id": "…", "step_id": "…",
                 "reason": "worker_budget_exhausted", "replay_round": 0,
                 "error_text": "piton: worker … answered HTTP 500: …",
                 "created_at": "…" } ] }
```

| Field | Type | Source |
|---|---|---|
| `dlq_id` | string | `dead_letter_queue.dlq_id` |
| `step_id` | string \| null | `dead_letter_queue.step_id` — the step that exhausted its budget; **`null` for a planner-side entry** (`§ 6.5`) |
| `reason` | string | `dead_letter_queue.reason` — one of the three above (`§ 6.5`) |
| `replay_round` | integer | `dead_letter_queue.replay_round` — `runs.replay_count` when the entry was written |
| `error_text` | string | `dead_letter_queue.error_text` — never null, truncated to 4 KB (`§ 6.5`) |
| `created_at` | string | `dead_letter_queue.created_at` |

### `GET /steps/{step_id}/output` — the stored output, verbatim

`SPEC.md § 10.2`. Returns the stored bytes **exactly as stored** — not wrapped, not re-serialised.

What is stored depends on the step's mode (`SPEC.md § 9.6`), and the difference matters to a client:

| `dispatch_style` | What this endpoint returns |
|---|---|
| `envelope` | The worker's response `output` field **alone**. Piton's own wrapper is not there |
| `raw` | **The entire response body**, whatever it was |

`409 conflict` when the step exists but has not completed — `SPEC.md § 6.3` forbids treating the
presence of an output as completion, so the endpoint refuses rather than guessing.

---

## Operational

### `GET /healthz`

`SPEC.md § 10.4`. `200` — `{"status":"ok","storage":"reachable"}`.
`503` — `{"status":"unavailable","storage":"unreachable"}`.

A `200` also means migrations have finished: the orchestrator applies them at boot and binds its
listener only afterwards.

---

## Errors

`SPEC.md § 10.5`. Every rejection is JSON, and **it states the actual current state rather than only
that the request was refused** — because what the caller does next depends on what is true now.

```json
{ "error": "conflict",
  "message": "run is not in DLQ and cannot be replayed",
  "run_id": "018f…", "run_status": "RUNNING",
  "step_id": "018f…", "step_status": "RUNNING" }
```

| Field | Meaning |
|---|---|
| `error` | **Stable, machine-readable slug.** Branch on this |
| `message` | Human-readable. **May change; never branch on it** |
| `workflow_id` · `run_id` · `run_status` · `step_id` · `step_status` | Present for every entity the request named or would have touched; **omitted only when that entity does not exist** |

| Code | Slug | Used for |
|---|---|---|
| 400 | `invalid_request` | Malformed, or a violation of `SPEC.md § 16` / `§ 9.8` |
| 404 | `not_found` | No such entity |
| 409 | `conflict` | The entity exists but its state forbids the operation |
| 503 | `storage_unavailable` | Storage is unreachable |

---

## What this API does not expose

Stated so that a client author does not go looking.

| | Why |
|---|---|
| **The planner's own calls** (`planner_calls`, `§ 6.8`) | Every call the orchestrator makes to a planner is recorded — its answer, its failure reason, its round — and **no endpoint serves those rows**. `SPEC.md § 10.2` does not list one, and adding an endpoint SPEC does not list is new scope (R43-m). `runs.planner_attempt_count` and the dead-letter `reason` are what a client gets instead |
| `GET /workflows`, `GET /workflows/{id}` | Designed in `§ 10.1`, not implemented. A client keeps its own workflow ids |
| `GET /steps/{step_id}` | Designed in `§ 10.2`, not implemented — and `GET /runs/{run_id}/steps` already carries every field of it |
| `POST /runs/{run_id}/cancel` | Milestone ι. Cancellation semantics are `§ 15` and are not built |
| `POST /callbacks/{attempt_id}` | Milestone ε. Async workers are not built; every mode today is `sync` |

---

## Before you put this on the internet

**Piton has no authentication.** `SPEC.md § 2.2` — "out of scope entirely". Two consequences, and
the second is the serious one:

1. A publicly reachable Piton is an **unauthenticated control plane**. Anyone can create workflows
   and start runs against your database.
2. A StepSpec's `worker_url` is **any absolute HTTP(S) URL, and the orchestrator will POST to it**
   (`§ 9.4`, `§ 9.5`). Exposed publicly, that is a server-side request forgery engine: a stranger can
   point it at cloud metadata addresses or anything else your host can reach.

A public deployment therefore needs something in front that holds a credential, rate-limits run
creation, and **validates `worker_url` against an allowlist before forwarding `POST /workflows`**.
That is a deployment concern and not part of Piton, which is why it is a warning here rather than a
rule in `SPEC.md`.
