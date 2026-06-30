# StateFlow

**Durable execution for AI pipelines. Crash, restart, don't re-run completed steps.**

![Crash-recovery demo](demo/demo.gif)

StateFlow checkpoints every `(decision, result)` pair as it happens.
On crash it reads the checkpoint frontier and resumes exactly where it left off —
no replay, no determinism requirement, no SDK on the worker side.

---

## The 30-second picture

```
Your LLM or static planner
        |
        | "next step: call this URL with this input"
        v
   StateFlow (orchestrator)
        |
        |---> PutDecision (Barrier 1: persist before dispatch)
        |
        |---> HTTP POST ---------> Your worker  (any language)
        |                                |
        |<-- result ---- POST /tasks/complete  (or sync response)
        |
        |---> Checkpoint (Barrier 2: persist before next decision)
        |
        | "what's next?"
        v
Your LLM or static planner
```

Two write barriers make recovery trivial:

1. **PutDecision before Dispatch** — if the process crashes between these,
   recovery re-dispatches the persisted decision; the planner is not re-asked.
2. **Checkpoint before next Decide** — if the process crashes here,
   the completed result is safe; the step is not re-run.

Workers must be **idempotent** (StateFlow is at-least-once). The
[User Manual](docs/USER_MANUAL.md) explains how.

---

## Quick start

**Prerequisites:** Docker (Postgres), Go 1.22+

```bash
# 1. Start Postgres
docker run -d --name stateflow-pg \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=stateflow \
  -p 5432:5432 postgres:16-alpine

# 2. Build
go build -o stateflow ./cmd/stateflow/

# 3. Apply schema  (binary does NOT auto-migrate; psql runs inside the container)
docker exec -i stateflow-pg psql -U postgres -d stateflow < migrations/001_initial.sql

# 4. Run
DATABASE_URL=postgres://postgres:postgres@localhost:5432/stateflow?sslmode=disable \
  ./stateflow
```

StateFlow listens on `:8080`. Override with `LISTEN_ADDR=:9090`.

---

## API at a glance

> **Worker URLs below are placeholders.** StateFlow calls whatever URL you
> provide — replace them with your actual worker endpoints.
> For a fully wired end-to-end example with real workers, see the
> [crash-recovery demo](demo/).

```bash
# Create a workflow (static planner)
curl -X POST http://localhost:8080/workflows \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "my-pipeline",
    "planner_type": "static",
    "planner_config": {
      "steps": [
        {"name":"step1","worker_url":"http://YOUR-WORKER/run","mode":"sync"},
        {"name":"step2","worker_url":"http://YOUR-WORKER/run","mode":"async"}
      ]
    }
  }'

# Start a run
curl -X POST http://localhost:8080/workflows/{workflow_id}/runs \
  -H 'Content-Type: application/json' \
  -d '{"workflow_input": {"doc": "report.pdf"}}'

# Check status
curl http://localhost:8080/runs/{run_id}
```

**Async worker callback:**

```bash
curl -X POST http://localhost:8080/tasks/complete \
  -H 'Content-Type: application/json' \
  -d '{
    "step_id":    "run-abc-123:step2",
    "attempt_id": "<from dispatch body>",
    "output":     {"result": "..."}
  }'
```

---

## Planner types

| Type | Use when |
|------|----------|
| `static` | Steps are known upfront; order is fixed |
| `http` | An LLM or external service decides the next step dynamically |

The `http` planner POSTs the full run history to your endpoint and expects
a JSON decision back. See [User Manual §1](docs/USER_MANUAL.md) for the
system prompt template and output contract.

---

## Worker modes

| Mode | Worker responds | Use when |
|------|----------------|----------|
| `sync` | Inline in the HTTP response body | Fast workers (<30s) |
| `async` | HTTP 202 immediately; calls `/tasks/complete` later | LLM calls, batch jobs |

Workers need no SDK — they speak plain HTTP. Any language, any framework.

---

## Crash-recovery demo

The [demo](demo/) runs a 3-step pipeline (OCR → NER → Summarize), kills
the orchestrator while NER is mid-flight, and proves that after restart:

- OCR is **not** re-run (already `DONE`)
- NER is re-dispatched but hits the **idempotency cache** (not re-processed)
- Summarize runs for the **first time** with the full history

```bash
cd demo
pip install -r requirements.txt
python crash_demo.py
```

Total runtime: ~20 seconds.

---

## Docs

- [User Manual](docs/USER_MANUAL.md) — LLM prompt template, idempotency contract
- [Design](DESIGN.md) — schema, API contract, component interfaces
- [Whitepaper](docs/StateFlow_Whitepaper_v0.8.md) — architectural rationale

---

## Project status

MVP (Phase 1) — all core invariants implemented and tested:

- [x] Postgres-backed frontier store (5 tables)
- [x] Sync and async dispatch
- [x] Two write barriers enforced
- [x] Three recovery rules on restart
- [x] Static planner
- [x] HTTP/LLM planner (with retry + validation)
- [x] Attempt dedup guard
- [x] Crash-recovery demo

Phase 2 (planned): async timeout sweeper, Ghost Mode retry, full `response_mapping`,
DAG fan-in, replicated orchestrator.
