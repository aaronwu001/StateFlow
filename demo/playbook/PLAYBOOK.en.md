# StateFlow Demo Playbook (English)

**Demo mode: LLM / HTTP Planner**

Each scenario demonstrates one reliability guarantee of StateFlow. The planner runs as an HTTP endpoint — DUMMY mode requires no API key; REAL mode connects to Claude. Everything (Postgres, StateFlow, workers, planner adapter) runs as **docker compose services**.

> **Want to run the full crash-recovery demo automatically?** See [../crash_demo.py](../crash_demo.py)

---

## One-Time Setup

Open **2 terminal tabs** labeled: `LOGS` `CMD`

```bash
# CMD tab — from the project root: build + start Postgres, StateFlow, and all
# demo services (step1, step2, llm-adapter, ocr/ner/summarize workers) on one network.
docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build
```

```bash
# LOGS tab — stream every service's stdout so worker/planner/recovery
# markers are visible live, the way separate terminal tabs used to show them.
docker compose -f docker-compose.yml -f docker-compose.demo.yml \
  logs -f --no-log-prefix step1 step2 llm-adapter stateflow
```

Verify: `curl -s http://localhost:8080/runs/__probe__` returns 404 (server up, route not found — expected).

For brevity, the rest of this playbook assumes:

```bash
alias dc='docker compose -f docker-compose.yml -f docker-compose.demo.yml'
```

---

## Reset Between Scenarios

```bash
# CMD tab
dc exec -T postgres psql -U stateflow -d stateflow -c "TRUNCATE workflows CASCADE;"
```

---

## Quick Reference Commands

```bash
# Run status + all steps
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool

# Raw Postgres state
dc exec -T postgres psql -U stateflow -d stateflow \
  -c "SELECT step_name, status, output IS NOT NULL AS done FROM steps ORDER BY seq;"

# DLQ
curl -s http://localhost:8080/dlq | python3 -m json.tool

# Worker invocation counts
dc logs --no-log-prefix step1 2>/dev/null | grep -c "received step"
dc logs --no-log-prefix step2 2>/dev/null | grep -c "received step"

# Planner invocation count
dc logs --no-log-prefix llm-adapter 2>/dev/null | grep -c "Planner called"
```

---

# Scenario A: Happy Path

**Demonstrates:** LLM Planner drives a 2-step pipeline to completion.

## Steps

Workers `step1`/`step2` are already running from the one-time setup (delay=1s
each, per docker-compose.demo.yml's defaults). Dummy adapter routes step1 →
`step1:5010`, step2 → `step2:5011`, both reachable from the host at the same
ports.

**CMD tab:**
```bash
WORKFLOW_ID=$(curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "demo-A",
    "planner_type": "http",
    "planner_config": {"url": "http://llm-adapter:9000/decide"}
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

**LOGS tab** should show exactly 3 planner calls:
```
[ADAPTER] Planner called  history=[]                 → decides step1 (→ step1:5010)
[ADAPTER] Planner called  history=['step1']          → decides step2 (→ step2:5011)
[ADAPTER] Planner called  history=['step1', 'step2'] → done
```

**Success criteria:** run `status: DONE`, both steps `DONE`.

## Reset → Scenario B

---

# Scenario B: Worker Crash → DLQ → Replay

**Demonstrates:** Step2 worker is down → 3 retries → DLQ → Replay resumes; step1 is NOT re-run.

## Steps

**CMD tab: stop step2 (intentionally absent)**
```bash
dc stop step2
```

```bash
WORKFLOW_ID=$(curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "demo-B",
    "planner_type": "http",
    "planner_config": {"url": "http://llm-adapter:9000/decide"}
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
dc exec -T postgres psql -U stateflow -d stateflow \
  -c "SELECT s.step_name, a.attempt_number, a.status
      FROM attempts a JOIN steps s ON a.step_id = s.step_id
      ORDER BY a.dispatched_at;"
```

**Confirm DLQ entry:**
```bash
curl -s http://localhost:8080/dlq | python3 -m json.tool
```

## Replay

**CMD tab: start step2 back up**
```bash
dc up -d step2
```

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
dc logs --no-log-prefix step1 2>/dev/null | grep -c "received step"   # → 1
```

**Success criteria:**
- run `status: DONE`
- step1 invocation count = 1 (replay resumes from step2, does not re-run completed step1)

## Reset → Scenario C

---

# Scenario C: Orchestrator Crash → Recovery

**Demonstrates:** Kill orchestrator → restart → Recovery re-dispatches step1 WITHOUT calling the planner again (Barrier 1).

## Steps

**CMD tab: recreate step1 with a 5s delay to create a crash window**
```bash
STEP1_DELAY=5 dc up -d --force-recreate step1
dc up -d step2   # ensure step2 (delay=1s) is running too
```

```bash
WORKFLOW_ID=$(curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "demo-C",
    "planner_type": "http",
    "planner_config": {"url": "http://llm-adapter:9000/decide"}
  }' | python3 -c "import json,sys; print(json.load(sys.stdin)['workflow_id'])")

RUN_ID=$(curl -s -X POST "http://localhost:8080/workflows/$WORKFLOW_ID/runs" \
  -H "Content-Type: application/json" \
  -d '{"workflow_input":{"task":"crash recovery test"}}' \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['run_id'])")

echo "run_id: $RUN_ID"
```

Wait for the **LOGS tab** to show the first `Planner called` log (step1 has been DECIDED and dispatched). Then:

**CMD tab: kill the orchestrator container**
```bash
dc kill stateflow
```

## Inspect Post-Crash State

```bash
dc exec -T postgres psql -U stateflow -d stateflow \
  -c "SELECT step_name, status, output IS NOT NULL AS checkpointed FROM steps WHERE run_id = '$RUN_ID' ORDER BY seq;"
# step1: RUNNING, checkpointed=f  ← Barrier 1 fired (decision in DB), Barrier 2 not yet
```

## Restart Orchestrator

```bash
dc up -d stateflow
```

You should see recovery logs in the **LOGS tab**:
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
dc logs --no-log-prefix llm-adapter 2>/dev/null | grep -c "Planner called"
```

**Success criteria:**
- run `status: DONE`
- Total planner calls ≤ 3 (recovery re-dispatch does not trigger a new planner call)
- `[RECOVERY] resuming run` log appears in the LOGS tab

## Reset step1's delay back to default for the next run

```bash
STEP1_DELAY=1 dc up -d --force-recreate step1
```

---

## Plug In Your Own Worker

Replace `worker_url` with any service that accepts `POST /run`. If your
service also runs as a compose service on the same network, address it by
service name (e.g. `http://my-service:PORT/run`); otherwise any reachable URL
works (e.g. `http://host.docker.internal:PORT/run` for a service running on
the host):

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
