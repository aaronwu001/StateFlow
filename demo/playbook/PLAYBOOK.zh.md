# StateFlow Demo Playbook（中文版）

**Demo 模式：LLM / HTTP Planner**

每個場景示範 StateFlow 的一個可靠性保證。Planner 以 HTTP endpoint 形式運行——DUMMY 模式不需要 API key，REAL 模式接 Claude。

> **想用自動化腳本跑完整個 crash-recovery demo？** 見 [../crash_demo.py](../crash_demo.py)

---

## 一次性準備

開 **4 個 terminal tab**，標好名字：`ORCH` `PLANNER` `WORKER` `CMD`

```bash
# 任一 tab — Build binary（從 project root 執行）
go build -o demo/stateflow ./cmd/stateflow/

# 任一 tab — 啟動 Postgres（若尚未跑）
docker run -d --name stateflow-pg-test \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=stateflow_test \
  -p 5432:5432 \
  postgres:16-alpine

# CMD tab — 等 Postgres ready，建 DB + schema
until docker exec stateflow-pg-test pg_isready -U postgres; do sleep 1; done
docker exec stateflow-pg-test psql -U postgres \
  -c "CREATE DATABASE stateflow_demo;" postgres
docker exec -i stateflow-pg-test psql -U postgres -d stateflow_demo \
  < migrations/001_initial.sql
```

驗證：`curl -s http://localhost:8080/health` 或 `docker ps | grep stateflow-pg-test`

---

## 每個場景的固定元件

**PLANNER tab** — 每個場景前啟動，整個 session 保持跑著：

```bash
# DUMMY 模式（不需要 API key）：
python3 demo/planner/llm_adapter.py

# 或接真正的 Claude：
# ANTHROPIC_API_KEY="sk-ant-..." python3 demo/planner/llm_adapter.py
```

應看到：
```
[ADAPTER] mode=DUMMY (ANTHROPIC_API_KEY not set — using hardcoded logic)
[ADAPTER] LLM adapter listening on :9000
```

**ORCH tab** — 每個場景前啟動：

```bash
DATABASE_URL="postgresql://postgres:postgres@localhost:5432/stateflow_demo?sslmode=disable" \
LISTEN_ADDR=":8080" ./demo/stateflow
```

---

## 場景之間 Reset

```bash
# CMD tab
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo \
  -c "TRUNCATE workflows CASCADE;"
> /tmp/worker_step1.log; > /tmp/worker_step2.log
```

---

## 查詢速查

```bash
# Run 狀態 + 所有 steps
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool

# Postgres 原始狀態
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo \
  -c "SELECT step_name, status, output IS NOT NULL AS done FROM steps ORDER BY seq;"

# DLQ
curl -s http://localhost:8080/dlq | python3 -m json.tool

# Worker 被呼叫幾次
grep -c "received step" /tmp/worker_step1.log
grep -c "received step" /tmp/worker_step2.log

# Planner 被呼叫幾次
grep -c "Planner called" /tmp/llm_adapter.log
```

---

# 場景 A：Happy Path

**示範：** LLM Planner 驅動 2 步 pipeline 到完成。

## 步驟

**WORKER tab：**
```bash
# Dummy adapter 把 step1 → :5010，step2 → :5011
WORKER_NAME=step1 WORKER_PORT=5010 WORKER_DELAY=1 python3 demo/workers/worker.py &
WORKER_NAME=step2 WORKER_PORT=5011 WORKER_DELAY=1 python3 demo/workers/worker.py
```

**CMD tab：**
```bash
# 建 workflow（HTTP planner）
WORKFLOW_ID=$(curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "demo-A",
    "planner_type": "http",
    "planner_config": {"url": "http://localhost:9000/decide"}
  }' | python3 -c "import json,sys; print(json.load(sys.stdin)['workflow_id'])")

# 啟動 run
RUN_ID=$(curl -s -X POST "http://localhost:8080/workflows/$WORKFLOW_ID/runs" \
  -H "Content-Type: application/json" \
  -d '{"workflow_input":{"task":"analyze quarterly report"}}' \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['run_id'])")

echo "run_id: $RUN_ID"
```

## 驗證

```bash
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool
```

**PLANNER tab** 應看到 3 次呼叫：
```
[ADAPTER] Planner called  history=[]                 → step1 決定（→ :5010）
[ADAPTER] Planner called  history=['step1']          → step2 決定（→ :5011）
[ADAPTER] Planner called  history=['step1', 'step2'] → done
```

**成功條件：** run `status: DONE`，兩個 steps 都是 `DONE`。

## Reset → 進場景 B

---

# 場景 B：Worker 掛掉 → DLQ → Replay

**示範：** Step2 worker 不在線 → 重試 3 次 → DLQ → Replay 恢復；step1 不重跑。

## 步驟

**WORKER tab：只啟動 step1（step2 故意不啟動）**
```bash
WORKER_NAME=step1 WORKER_PORT=5010 WORKER_DELAY=1 python3 demo/workers/worker.py
```

**CMD tab：**
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

**等 ~15 秒**（StateFlow 重試 step2 三次，每次 ~5 秒間隔）

## 查看進度

**Step2 retry 歷史：**
```bash
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo \
  -c "SELECT s.step_name, a.attempt_number, a.status
      FROM attempts a JOIN steps s ON a.step_id = s.step_id
      ORDER BY a.dispatched_at;"
```

**確認進 DLQ：**
```bash
curl -s http://localhost:8080/dlq | python3 -m json.tool
```

## Replay

**WORKER tab：另開 pane，啟動 step2**
```bash
WORKER_NAME=step2 WORKER_PORT=5011 WORKER_DELAY=1 python3 demo/workers/worker.py
```

**CMD tab：**
```bash
# 取 DLQ entry id
DLQ_ID=$(curl -s http://localhost:8080/dlq | \
  python3 -c "import json,sys; print(json.load(sys.stdin)['entries'][0]['id'])")

# Replay
curl -s -X POST "http://localhost:8080/dlq/$DLQ_ID/replay" \
  -H "Content-Type: application/json" -d '{}'
```

## 驗證

```bash
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool

# step1 必須只被呼叫 1 次
grep -c "received step" /tmp/worker_step1.log   # → 1
```

**成功條件：**
- run `status: DONE`
- step1 被呼叫次數 = 1（Replay 只從 step2 繼續，不重跑已完成的 step1）

## Reset → 進場景 C

---

# 場景 C：Orchestrator 崩潰 → Recovery

**示範：** SIGKILL orchestrator → 重啟 → Recovery 只 re-dispatch step1，planner 不被重新呼叫。

## 步驟

**WORKER tab：step1 設慢（5s）提供 crash 視窗**
```bash
WORKER_NAME=step1 WORKER_PORT=5010 WORKER_DELAY=5 python3 demo/workers/worker.py &
WORKER_NAME=step2 WORKER_PORT=5011 WORKER_DELAY=1 python3 demo/workers/worker.py
```

**CMD tab：**
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

等 **PLANNER tab** 看到第一次 Planner called（step1 已被 DECIDED），然後：

**ORCH tab：Ctrl+C**（或 `kill -9 $(pgrep -f demo/stateflow)`）

## 查看 Crash 後狀態

```bash
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo \
  -c "SELECT step_name, status, output IS NOT NULL AS checkpointed FROM steps ORDER BY seq;"
# step1: RUNNING, checkpointed=f  ← Barrier 1 已 fire（decision in DB），Barrier 2 尚未
```

## 重啟 Orchestrator

**ORCH tab：**
```bash
DATABASE_URL="postgresql://postgres:postgres@localhost:5432/stateflow_demo?sslmode=disable" \
LISTEN_ADDR=":8080" ./demo/stateflow
```

應看到 recovery log：
```
msg="[RECOVERY] found in-progress runs" count=1
msg="[RECOVERY] resuming run" steps_done=0 pending_step=step1
```

## 驗證

```bash
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool
```

**PLANNER tab** 計算總呼叫次數（應 ≤ 3）：
```bash
grep -c "Planner called" /tmp/llm_adapter.log
```

**成功條件：**
- run `status: DONE`
- Planner 總呼叫次數 ≤ 3（recovery re-dispatch 不觸發新的 planner call）
- `[RECOVERY] resuming run` log 出現

---

## 接你自己的 Worker

把 `worker_url` 換成任何能接 `POST /run` 的服務：

```json
{
  "name": "my-step",
  "worker_url": "http://YOUR_SERVICE/run",
  "mode": "sync",
  "timeout_seconds": 30,
  "input": {"key": "value"}
}
```

你的服務只需要回傳 JSON，StateFlow 會把它存成 step output 並傳給 planner 作為 history。
