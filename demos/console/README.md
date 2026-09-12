# The console environment

Two things live here, deliberately as one environment:

1. **What `test/console` runs against.** `GET /runs` and `GET /runs/{run_id}/dlq` can only be
   asserted against a database holding runs in several states and dead-letter entries of *both*
   kinds, and no milestone environment produces a planner-side one on demand.
2. **The backend a browser demo points at.** `SPEC.md § 2.2` keeps a user interface out of Piton's
   scope, and `BACKLOG.md` B7 records that a UI was *deprioritised rather than cut* — "terminal-first
   is the requirement, not terminal-only". A front end is therefore a **consumer** of this API, never
   a part of the engine, and nothing in this directory changes the orchestrator.

```bash
cd demos/console
docker compose up -d --build --wait
./demo.sh            # the hand-run script
docker compose down -v
```

The wire contract a client builds against is [`API.md`](../../API.md). This file is about what a
person can *do* here.

---

## The two switches

Every earlier milestone declares its failures up front, in a StepSpec's `params`: the run is started
already doomed. That demonstrates the mechanism and not the experience. These two switches are the
other half — **a run that is already healthy, interrupted while it is in flight**.

```bash
# Break it for ONE run - what a shared demo should use
curl -X POST "localhost:9090/pause?run=$RUN"    # the worker goes silent for that run
curl -X POST "localhost:9090/resume?run=$RUN"
curl -X POST "localhost:9100/pause?run=$RUN"    # the planner goes silent for that run
curl -X POST "localhost:9100/resume?run=$RUN"

# Break it for everything - one operator at a terminal
curl -X POST localhost:9090/pause
curl -X POST localhost:9090/resume              # lifts ONLY the global pause
curl -X POST localhost:9090/reset               # clears everything

curl localhost:9090/state    # {"paused_all":false,"paused_runs":[...],"calls":{...}}
curl localhost:9100/state    # {"paused_all":false,"paused_runs":[...]}
```

**Each switch does exactly one thing**, and that is not cosmetic. An earlier
version had `/resume` also clear every per-run pause, which made going from
"everything paused" to "one run paused" take two requests — and the hold loop
re-reads the state every 200 ms, so a run could escape through the gap between
them. The isolation suite caught it.

**Prefer the per-run form.** A single global switch works for one person at a
terminal and fails the moment two people watch the same demo: one presses pause
and the other's run dies for a reason they did not cause — and it dies
convincingly, with real `timeout` attempts and a real dead-letter entry, so
nothing on screen says it was somebody else's doing.

Per-run works because `SPEC.md § 9.5`'s envelope carries `run_id` and `§ 9.2`'s
planner request carries it too. **Piton knows nothing about any of this** — it
keeps POSTing to the same `worker_url`; what changes is what answers.

**Paused means silent, not refusing.** The connection is accepted and nothing is written. That
matters: `SPEC.md § 5.3` decides between `timeout` and `transport_error` **by the clock, not by the
shape of the error**, so a silent worker produces `timeout` and a worker that refused the connection
would produce `transport_error` — two different rows pointing at two different repairs.

Nothing about this needs Piton to change. The orchestrator keeps POSTing to the same `worker_url`;
what changes is what the thing on the other end does when it is called.

### Why the timeouts here are 5 seconds

`SPEC.md § 11.1` ranges `step_timeout_seconds` and `planner_timeout_seconds` at `≥ 1` and defaults
them to 300 and 30. At those defaults, pausing a worker means watching nothing happen for five
minutes. `workflow.json` sets **5**, which is legal, needs no code, and makes the wait watchable —
and `SPEC.md § 13.3` guarantees that while the run's owner is live, failure is declared *precisely*
at the deadline rather than somewhere after it.

---

## What a person can do, and what appears in the database

This is the catalogue a front end should offer. Each row is one thing to try and the exact rows it
writes — which is what makes this a demo of a **durable** engine rather than of a job queue.

| Do this | What the database shows | Where it is ruled |
|---|---|---|
| Start a healthy run | `runs.status` `RUNNING` → `DONE`; one `steps` row per planner `continue`; one `attempts` row each, all `DONE` | `§ 5.1`, `§ 5.2` |
| **Pause the worker, then start a run** | The attempt sits `RUNNING` until `deadline_at`, then `FAILED` with `failure_reason = 'timeout'`. A second attempt is dispatched | `§ 5.3`, `§ 13.3` |
| **Pause the worker mid-run, then resume before the budget is gone** | Attempt 1 `FAILED`/`timeout`, attempt 2 `DONE`. `steps.attempt_count` is 2 and the step still reaches `DONE` | `§ 12.2` |
| **Pause the worker and leave it paused** | Both attempts time out, `steps.status` → `DLQ`, `runs.status` → `DLQ`, and one `dead_letter_queue` row with `reason = 'worker_budget_exhausted'` — all in one transaction | `§ 12.2`, `§ 12.3` |
| Run with `worker.mode = "http_500"` | The same ending, but `failure_reason = 'transport_error'` and the body in `error_text` | `§ 5.3`, `§ 9.6` |
| Run with `worker.mode = "worker_error"` | `failure_reason = 'worker_error'` — the worker replied, in Piton's envelope, that the work failed | `§ 5.3` |
| Run with `worker.fail_first = 1` | Attempt 1 `FAILED`, attempt 2 `DONE`, with no intervention. The run finishes | `§ 12.2` |
| **Pause the planner, then start a run** | The run dies **before it has any step at all**: `runs.planner_attempt_count` climbs, then one dead-letter row with `reason = 'planner_budget_exhausted'` and **`step_id` null** | `§ 12.3`, `§ 6.5` |
| Replay a DLQ'd run after fixing the worker | `runs.replay_count` increments, the run finishes — and **the old dead-letter row and the failed round's attempts are still there**, carrying `replay_round = 0` | `§ 14` |
| Replay a DLQ'd run without fixing it | A **second** dead-letter row joins the first, `replay_round = 1`. The history accumulates | `§ 10.2`, `§ 14` |

The pair worth building the demo around is **pause the worker** versus **pause the planner**. Both
end with `runs.status = 'DLQ'`, and nothing in the run row says which happened. The only thing that
does is the dead-letter `reason` — which is precisely why `SPEC.md § 12.3` separates the two sides
and why `GET /runs/{run_id}/dlq` exists.

---

## What the front end should offer

Requirements, not a design. Anything here that a client cannot do is a bug in this list, not in the
client.

**It must be able to:**

- Start a run whose behaviour comes from the run's **`input`**, not from a new workflow. The console
  planner reads `steps` (how many) and `worker` (each step's `params`) out of `runs.input`, so one
  workflow definition drives every scenario above. `POST /workflows/{id}/runs` is the only call.
- Show a run's steps **and its attempts**, refreshed while the run is in flight.
  `GET /runs/{run_id}/steps` already carries the full attempt list — `attempt_no`, `status`,
  `failure_reason`, `error_text`, `deadline_at`, `replay_round` — so a live view needs one poll, not
  one per step.
- Show **why** a run stopped, by fetching `GET /runs/{run_id}/dlq` whenever `status` is `DLQ`. A DLQ
  run whose reason is not on screen is the one state that looks like a bug when it is not.
- Offer the two pause switches as plain buttons **scoped to the run on screen** —
  `POST /pause?run={run_id}` — with the current state read back from `GET /state` on each service
  rather than remembered locally. Never offer the global form to a public audience: it lets one
  viewer kill every other viewer's run, invisibly.
- Offer **replay** on any DLQ'd run, and show `replay_count` afterwards.

**It must not:**

- Treat `output_bytes > 0` as "this step finished". `SPEC.md § 6.3` is explicit: completion is
  signalled by `status = 'DONE'` **and by nothing else**, and an output can be stored for a step that
  is not complete.
- Poll faster than about once a second. Nothing here changes faster than the 5-second deadlines, and
  the read endpoints are not free.
- Connect to Postgres. Everything a client needs is on the HTTP API; a browser holding a database
  password is a database password on the internet.

**Worth showing, because it is the point:**

- `steps.attempt_count` as a **budget being spent** against `step_max_attempts`, not as a number.
  A step at 1 of 2 is one failure away from DLQ, and that is the tension the whole demo is built on.
- `attempts.replay_round`, so a replayed run's history reads as *"these two burned in round 0; this
  one is the retry"* rather than as an undifferentiated list.

---

## Before this is reachable from the internet

`SPEC.md § 2.2`: **"Authentication or authorisation — out of scope entirely."** Two consequences,
and the second is the serious one.

A publicly reachable Piton is an unauthenticated control plane: anyone can create workflows and
start runs against your database. And a StepSpec's `worker_url` is **any absolute HTTP(S) URL that
the orchestrator will POST to** (`§ 9.4`, `§ 9.5`) — so an exposed deployment is a server-side
request forgery engine, able to reach cloud metadata addresses and anything else on your host's
network.

A public deployment needs something in front of it that holds a credential, rate-limits run
creation, and validates `worker_url` against an allowlist before forwarding `POST /workflows`. That
is a deployment concern rather than a rule about the system, which is why it is a warning here and
not a line in `SPEC.md`.
