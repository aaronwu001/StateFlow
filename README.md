# StateFlow

[![CI](https://github.com/aaronwu001/StateFlow/actions/workflows/ci.yml/badge.svg)](https://github.com/aaronwu001/StateFlow/actions/workflows/ci.yml)

**Durable execution for AI pipelines. Crash, restart, don't re-run completed steps.**

![Crash-recovery demo](demo/demo.gif)

StateFlow is a durable execution layer for AI pipelines. It checkpoints every step,
retries failures, and resumes after a crash exactly where it left off — without
re-running completed work. The mechanism is a **frontier model**: StateFlow persists
each `(decision, result)` pair as it happens, and on recovery it reads that frontier
and resumes. There is no replay, no determinism requirement, and the planner (which
may be an LLM) is asked exactly once per persisted step.

---

## The 30-second picture

```
Your LLM or static planner
        |
        | "next step: call this URL with this input"
        v
   StateFlow (orchestrator)
        |
        |---> TX1: persist decision + attempt (Barrier 1 — before dispatch)
        |
        |---> HTTP POST ---------> Your worker  (any language)
        |                                |
        |<-- result ---- POST /tasks/complete  (or sync response)
        |
        |---> TX2: persist result + output (Barrier 2 — before next decision)
        |
        | "what's next?"
        v
Your LLM or static planner
```

Two write barriers make recovery a **read**, never a replay engine:

1. **TX1 (Barrier 1) — persist the decision before dispatch.** If the process
   crashes between these, recovery re-dispatches the persisted decision; the
   planner is not re-asked.
2. **TX2 (Barrier 2) — persist the result before the next decision.** If the
   process crashes here, the completed result is safe; the step is not re-run.

Workers need no SDK — they speak plain HTTP, any language — but they must be
**idempotent**, since StateFlow's re-dispatch-on-crash guarantee is at-least-once,
not exactly-once. See the [User Manual](docs/USER_MANUAL.md) for the idempotency
contract.

---

## Quick start

**Prerequisites:** Docker with Compose v2 (`docker compose version`). No local Go
toolchain needed — the orchestrator builds inside the container.

```bash
docker compose up -d --build
```

This starts Postgres and the `stateflow` orchestrator, listening on `:8080`.
`stateflow` applies its schema migrations itself at startup (via
[golang-migrate](https://github.com/golang-migrate/migrate)'s Go library,
before crash recovery runs) — no separate migration step needed. Give it a
few seconds, then confirm both services report healthy:

```console
$ docker compose ps
NAME                    IMAGE                 COMMAND                  SERVICE     CREATED          STATUS                    PORTS
stateflow-postgres-1    postgres:16           "docker-entrypoint.s…"   postgres    20 seconds ago   Up 19 seconds (healthy)   0.0.0.0:5432->5432/tcp, [::]:5432->5432/tcp
stateflow-stateflow-1   stateflow-stateflow   "/stateflow"             stateflow   20 seconds ago   Up 13 seconds (healthy)   0.0.0.0:8080->8080/tcp, [::]:8080->8080/tcp
```

`stateflow`'s healthiness comes from `GET /healthz` (checked internally via the
`stateflow healthcheck` subcommand, since the distroless runtime image has no
shell for a `curl`-based probe) — see the whitepaper's Temporary Design Registry
(§18) for why that mechanism exists.

### Create a workflow, start a run, check its status

The example below uses a **static planner** (a fixed step list) with one step
that calls a worker over plain HTTP. `docker-compose.yml` already routes
`host.docker.internal` to the host from inside the `stateflow` container, so a
worker listening on your host is reachable — this is how the commands below
were actually verified end-to-end. Start a trivial one, in a separate
terminal:

```bash
python3 -c "
from http.server import BaseHTTPRequestHandler, HTTPServer
import json

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers['Content-Length'])
        body = json.loads(self.rfile.read(n))
        resp = json.dumps({'echoed': body}).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(resp)
    def log_message(self, *a): pass

HTTPServer(('0.0.0.0', 5099), H).serve_forever()
"
```

Create the workflow:

```bash
curl -s -X POST http://localhost:8080/workflows \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "readme-quickstart",
    "planner_type": "static",
    "planner_config": {
      "steps": [
        {"name": "step1", "worker_url": "http://host.docker.internal:5099/run", "mode": "sync"}
      ]
    }
  }'
```

```json
{"workflow_id":"wf-3de74378-16a8-40d1-abd8-ec9823899666"}
```

Start a run (substitute your own `workflow_id`):

```bash
curl -s -X POST http://localhost:8080/workflows/wf-3de74378-16a8-40d1-abd8-ec9823899666/runs \
  -H 'Content-Type: application/json' \
  -d '{"workflow_input": {"doc": "report.pdf"}}'
```

```json
{"run_id":"run-8f5a7a0d-b11c-4035-a6ac-6a9b1c755873"}
```

Check its status (substitute your own `run_id`):

```bash
curl -s http://localhost:8080/runs/run-8f5a7a0d-b11c-4035-a6ac-6a9b1c755873
```

```json
{"created_at":"2026-07-11T08:33:22.333901Z","run_id":"run-8f5a7a0d-b11c-4035-a6ac-6a9b1c755873","status":"DONE","steps":[{"step_id":"run-8f5a7a0d-b11c-4035-a6ac-6a9b1c755873:step1","step_name":"step1","seq":1,"status":"DONE","attempt_count":0,"output":{"echoed":{"history":[],"workflow_input":{"doc":"report.pdf"}}},"created_at":"2026-07-11T08:33:22.339693Z","completed_at":"2026-07-11T08:33:22.352703Z","current_attempt":{"attempt_id":"e2667a81-1c67-4e3f-a30b-375a8609658a","status":"DONE"}}],"updated_at":"2026-07-11T08:33:22.357304Z","workflow_input":{"doc":"report.pdf"}}
```

`run.status` is one of `RUNNING` / `DONE` / `DLQ`; each step carries its own
`status`, `seq`, `attempt_count`, and (if its latest attempt failed)
`current_attempt.reason`/`error`. If `status` is `DLQ`, the response also
includes `dlq_reason`. Full endpoint reference: `CLAUDE.md`'s API path table
and the [User Manual](docs/USER_MANUAL.md).

---

## Full API surface

```
POST /workflows                      create workflow (name, planner_type, planner_config)
POST /workflows/{workflow_id}/runs   start run (workflow_input)
GET  /runs/{run_id}                  status + steps (seq/attempt_count/created_at/current_attempt) + dlq_reason when run=DLQ
GET  /dlq                            list DLQ entries
POST /dlq/{id}/replay                replay (worker-side or planner-side, depending on the entry)
POST /tasks/complete                 async worker success callback
POST /tasks/fail                     async worker failure callback
GET  /healthz                        liveness/readiness probe (200 if Postgres is reachable, 503 otherwise)
```

- **Sync** workers receive the **bare `input`** as the POST body (no wrapper) plus
  headers `X-StateFlow-Step-ID` / `X-StateFlow-Attempt-ID`.
- **Async** workers receive the envelope `{step_id, attempt_id, input}`, respond
  `202`, and report back to `/tasks/complete` or `/tasks/fail`.
- Every status string on the wire is **UPPERCASE** (`"DONE"`), matching the stored
  values.

Full request/response shapes, the LLM planner's system-prompt template, DLQ triage
guidance, and the idempotency contract: [User Manual](docs/USER_MANUAL.md).

---

## Configuration

All configuration is via environment variables on the `stateflow` binary/container.
Every variable below is optional — a fresh clone with none of them set behaves exactly
as before this table existed.

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | *(required)* | Postgres connection string. |
| `LISTEN_ADDR` | `:8080` | HTTP listen address. |
| `RETRY_MAX_ATTEMPTS` | `3` | Worker-side retry policy's attempt ceiling (`orchestrator.FixedCountPolicy.MaxRetries`). |
| `RETRY_DELAY_SECONDS` | `5` | Fixed floor on the delay between a failed attempt and its retry (`orchestrator.FixedCountPolicy.Delay`) — a worker's `retry_after_seconds` can only push this delay up, never below it. |
| `SWEEP_INTERVAL_SECONDS` | `30` | How often the in-process orphan sweeper scans for runs whose driving goroutine died without the process itself crashing (registry #4). |

`RETRY_MAX_ATTEMPTS`/`RETRY_DELAY_SECONDS`/`SWEEP_INTERVAL_SECONDS` must be positive
integers if set; the process fails fast at startup on an invalid value rather than
silently falling back to the default. This does not add a store-backend or transport
selection mechanism — there is exactly one implementation of each today, so none was
built.

---

## Architecture, in brief

Run, step, and attempt each have exactly three states:

```
run:     RUNNING | DONE | DLQ
step:    RUNNING | DONE | DLQ
attempt: RUNNING | DONE | FAILED (failure_reason: worker_reported | timeout | malformed | orphaned)
```

Every write to that state happens inside one of a fixed set of database
transactions (the **Atomic Transaction Ledger**) — nothing else touches run/step
state, so recovery never has to guess at a partially-applied write. Every attempt
is timed from its own creation (not from dispatch), with a default 60s timeout,
overridable per workflow or per step. On restart, recovery reads which
`run`/`last_step` combination each in-flight run is in, claims any orphaned
attempt (the orchestrator that owned its timer died, so nothing else ever will),
checks the retry budget, and either re-dispatches or the run is already in the
DLQ. This is a summary, not the full model — the whitepaper is the source of
truth:

- [Whitepaper](docs/StateFlow_Whitepaper_v1_0.md) — architecture, schema, API
  contract, and design rationale (state model §4, step loop §5, timeout doctrine
  §6, invariants/recovery §8, schema §14, the full Transaction Ledger §19).
- [Rules Consolidation](docs/StateFlow_Rules_Consolidation_v3_EN.md) — the
  rule-by-rule spec with rationale (the authoritative English edition).
- [User Manual](docs/USER_MANUAL.md) — LLM prompt template, idempotency contract,
  DLQ triage.

---

## Crash-recovery demo

The [demo](demo/) runs a 3-step pipeline (OCR → NER → Summarize) behind an LLM/HTTP
planner, kills the orchestrator mid-flight on a step dispatched asynchronously, and
proves that after restart:

- OCR is **not** re-run (already `DONE`).
- NER's in-flight attempt is claimed as `orphaned` (its timer lived in the dead
  process), a fresh attempt is dispatched, and the worker's idempotency cache
  absorbs the re-dispatch instead of reprocessing.
- Summarize runs for the first time, downstream of the recovered state.

```bash
docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build
python3 demo/crash_demo.py
```

See [demo/README.md](demo/README.md) for the interactive, menu-driven walkthrough
(`./demo/run_demo.sh`) and the [playbook](demo/playbook/PLAYBOOK.en.md) for a
narrated version of all three scenarios.

---

## Project status

**Phase 1 — core correctness model: complete and fully verified.**

- [x] Three-state model (run/step/attempt), no `DECIDED`/`FAILED` step states
- [x] Atomic Transaction Ledger — every state write is exactly one DB transaction
- [x] Two write barriers (TX1/TX2) enforced; recovery is a read, not a replay engine
- [x] Combination-table recovery with orphan-claim + persisted retry budget on restart
- [x] CAS-guarded reports (`attempt_id` + `status='RUNNING'`); late/duplicate reports are no-ops
- [x] Static planner and HTTP/LLM planner (with its own retry + validation budget)
- [x] Sync and async dispatch, creation-anchored timeouts
- [x] DLQ with four reasons and worker-side/planner-side replay
- [x] Crash-recovery demo and two frozen owner acceptance oracles

**Phase 1.5 — publication, in progress:**

- [x] CI (GitHub Actions): `go test -p 1 ./...` against live Postgres, the crash
  demo, and both acceptance oracles run on every push
- [x] `GET /healthz` + `stateflow healthcheck` self-check subcommand, wired into
  the Docker/compose healthchecks shown above
- [x] This README

Deferred beyond Phase 1.5 (whitepaper §18's Temporary Design Registry): full-history
transmission's summary-plus-fetch alternative and late-result salvage. None of these
affect Phase 1's correctness guarantees; see the whitepaper for the full registry and
rationale. (The registry's other Phase-2 items — in-process orphan sweeper,
`retry_after_seconds`-aware rate limiting, config-driven assembly, and versioned
migration tooling via `golang-migrate` — have since landed.)
