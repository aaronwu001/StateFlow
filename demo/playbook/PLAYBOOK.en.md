# StateFlow Demo Playbook (English)

**Demo mode: LLM / HTTP Planner**

Each scenario demonstrates one reliability guarantee of StateFlow. The planner runs as an HTTP endpoint — DUMMY mode requires no API key; REAL mode connects to Claude.

> **Want to run the full crash-recovery demo automatically?** See [../crash_demo.py](../crash_demo.py)

---

## One-Time Setup

Open **4 terminal tabs** labeled: `ORCH` `PLANNER` `WORKER` `CMD`

```bash
# Any tab — Build binary (from project root)
go build -o demo/stateflow ./cmd/stateflow/

# Any tab — Start Postgres (if not already running)
docker run -d --name stateflow-pg-test \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=stateflow_test \
  -p 5432:5432 \
  postgres:16-alpine

# CMD tab — Wait for Postgres, create DB + apply schema
until docker exec stateflow-pg-test pg_isready -U postgres; do sleep 1; done
docker exec stateflow-pg-test psql -U postgres \
  -c "CREATE DATABASE stateflow_demo;" postgres
docker exec -i stateflow-pg-test psql -U postgres -d stateflow_demo \
  < migrations/001_initial.sql
```

---

## Fixed Components (Every Scenario)

**PLANNER tab** — start before each scenario, keep running throughout:

```bash
# DUMMY mode (no API key needed):
python3 demo/planner/llm_adapter.py

# Or connect to real Claude:
# ANTHROPIC_API_KEY="sk-ant-..." python3 demo/planner/llm_adapter.py
```

You should see:
```
[ADAPTER] mode=DUMMY (ANTHROPIC_API_KEY not set — using hardcoded logic)
[ADAPTER] LLM adapter listening on :9000
```

**ORCH tab** — start before each scenario:

```bash
DATABASE_URL="postgresql://postgres:postgres@localhost:5432/stateflow_demo?sslmode=disable" \
LISTEN_ADDR=":8080" ./demo/stateflow
```

---

## Reset Between Scenarios

```bash
# CMD tab
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo \
  -c "TRUNCATE workflows CASCADE;"
> /tmp/worker_step1.log; > /tmp/worker_step2.log
```

---

## Quick Reference Commands

```bash
# Run status + all steps
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool

# Raw Postgres state
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo \
  -c "SELECT step_name, status, output IS NOT NULL AS done FROM steps ORDER BY seq;"

# DLQ
curl -s http://localhost:8080/dlq | python3 -m json.tool

# Worker invocation counts
grep -c "received step" /tmp/worker_step1.log
grep -c "received step" /tmp/worker_step2.log

# Planner invocation count
grep -c "Planner called" /tmp/llm_adapter.log
```

---

# Scenario A: Happy Path

**Demonstrates:** LLM Planner drives a 2-step pipeline to completion.

## Steps

**WORKER tab:**
```bash
# Dummy adapter routes step1 → :5010, step2 → :5011
WORKER_NAME=step1 WORKER_PORT=5010 WORKER_DELAY=1 python3 demo/workers/worker.py &
WORKER_NAME=step2 WORKER_PORT=5011 WORKER_DELAY=1 python3 demo/workers/worker.py
```

**CMD tab:**
```bash
# Create workflow (HTTP planner)
WORKFLOW_ID=$(curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "demo-A",
    "planner_type": "http",
    "planner_config": {"url": "http://localhost:9000/decide"}
  }' | python3 -c "import json,sys; print(json.load(sys.stdin)['workflow_id'])")

# Start run
RUN_ID=$(curl -s -X POST "http://localhost:8080/workflows/$WORKFLOW_ID/runs" \
  -H "Content-Type: application/json" \
  -d '{"workflow_input":{"task":"analyze quarterly report"}}' \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['run_id'])")

echo "run_id: $RUN_ID"
```

## Verify

```bash
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool
```

**PLANNER tab** should show exactly 3 calls:
```
[ADAPTER] Planner called  history=[]                 → decides step1 (→ :5010)
[ADAPTER] Planner called  history=['step1']          → decides step2 (→ :5011)
[ADAPTER] Planner called  history=['step1', 'step2'] → done
```

**Success criteria:** run `status: DONE`, both steps `DONE`.

## Reset → Scenario B

---

# Scenario B: Worker Crash → DLQ → Replay

**Demonstrates:** Step2 worker is down → 3 retries → DLQ → Replay resumes; step1 is NOT re-run.

## Steps

**WORKER tab: start only step1 (step2 intentionally absent)**
```bash
WORKER_NAME=step1 WORKER_PORT=5010 WORKER_DELAY=1 python3 demo/workers/worker.py
```

**CMD tab:**
```bash
WORKFLOW_ID=$(curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "demo-B",
    "planner_type": "http",
    "planner_config": {"url": "http://localhost:9000/decide"}
  }' | python3 -c "import json,sys; print(json.load(sys.stdin)['workflow_id'])")

RUN_ID=$(curl -s -X POST "http://localhost:8080/workflows/$WORKFLOW_ID/runs" \
  -H "Content-Type: application/json" \
  -d '{"workflow_input":{"task":"worker crash test"}}' \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['run_id'])")

echo "run_id: $RUN_ID"
```

**Wait ~15 seconds** (StateFlow retries step2 three times, ~5s between each)

## Observe Retries

**Check retry history:**
```bash
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo \
  -c "SELECT s.step_name, a.attempt_number, a.status
      FROM attempts a JOIN steps s ON a.step_id = s.step_id
      ORDER BY a.dispatched_at;"
```

**Confirm DLQ entry:**
```bash
curl -s http://localhost:8080/dlq | python3 -m json.tool
```

## Replay

**WORKER tab: start step2**
```bash
WORKER_NAME=step2 WORKER_PORT=5011 WORKER_DELAY=1 python3 demo/workers/worker.py
```

**CMD tab:**
```bash
# Get DLQ entry ID
DLQ_ID=$(curl -s http://localhost:8080/dlq | \
  python3 -c "import json,sys; print(json.load(sys.stdin)['entries'][0]['id'])")

# Replay
curl -s -X POST "http://localhost:8080/dlq/$DLQ_ID/replay" \
  -H "Content-Type: application/json" -d '{}'
```

## Verify

```bash
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool

# step1 must have been called exactly once
grep -c "received step" /tmp/worker_step1.log   # → 1
```

**Success criteria:**
- run `status: DONE`
- step1 invocation count = 1 (replay resumes from step2, does not re-run completed step1)

## Reset → Scenario C

---

# Scenario C: Orchestrator Crash → Recovery

**Demonstrates:** SIGKILL orchestrator → restart → Recovery re-dispatches step1 WITHOUT calling the planner again (Barrier 1).

## Steps

**WORKER tab: step1 is slow (5s delay) to create a crash window**
```bash
WORKER_NAME=step1 WORKER_PORT=5010 WORKER_DELAY=5 python3 demo/workers/worker.py &
WORKER_NAME=step2 WORKER_PORT=5011 WORKER_DELAY=1 python3 demo/workers/worker.py
```

**CMD tab:**
```bash
WORKFLOW_ID=$(curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "demo-C",
    "planner_type": "http",
    "planner_config": {"url": "http://localhost:9000/decide"}
  }' | python3 -c "import json,sys; print(json.load(sys.stdin)['workflow_id'])")

RUN_ID=$(curl -s -X POST "http://localhost:8080/workflows/$WORKFLOW_ID/runs" \
  -H "Content-Type: application/json" \
  -d '{"workflow_input":{"task":"crash recovery test"}}' \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['run_id'])")

echo "run_id: $RUN_ID"
```

Wait for the **PLANNER tab** to show the first `Planner called` log (step1 has been DECIDED and dispatched). Then:

**ORCH tab: Ctrl+C** (or `kill -9 $(pgrep -f demo/stateflow)`)

## Inspect Post-Crash State

```bash
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo \
  -c "SELECT step_name, status, output IS NOT NULL AS checkpointed FROM steps ORDER BY seq;"
# step1: RUNNING, checkpointed=f  ← Barrier 1 fired (decision in DB), Barrier 2 not yet
```

## Restart Orchestrator

**ORCH tab:**
```bash
DATABASE_URL="postgresql://postgres:postgres@localhost:5432/stateflow_demo?sslmode=disable" \
LISTEN_ADDR=":8080" ./demo/stateflow
```

You should see recovery logs:
```
msg="[RECOVERY] found in-progress runs" count=1
msg="[RECOVERY] resuming run" steps_done=0 pending_step=step1
```

## Verify

```bash
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool
```

**Count total planner calls (should be ≤ 3):**
```bash
grep -c "Planner called" /tmp/llm_adapter.log
```

**Success criteria:**
- run `status: DONE`
- Total planner calls ≤ 3 (recovery re-dispatch does not trigger a new planner call)
- `[RECOVERY] resuming run` log appears in ORCH output

---

## Plug In Your Own Worker

Replace `worker_url` with any service that accepts `POST /run`:

```json
{
  "name": "my-step",
  "worker_url": "http://YOUR_SERVICE/run",
  "mode": "sync",
  "timeout_seconds": 30,
  "input": {"key": "value"}
}
```

Your service just returns any JSON — StateFlow stores it as the step output and passes it to the planner as history.
