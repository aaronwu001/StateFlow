# StateFlow Demo Playbook（中文版）

**Demo 模式：LLM / HTTP Planner**

每個場景示範 StateFlow 的一個可靠性保證。Planner 以 HTTP endpoint 形式運行——DUMMY 模式不需要 API key，REAL 模式接 Claude。所有元件（Postgres、StateFlow、worker、planner adapter）都以 **docker compose service** 運行。

> **想用自動化腳本跑完整個 crash-recovery demo？** 見 [../crash_demo.py](../crash_demo.py)

---

## 一次性準備

開 **2 個 terminal tab**，標好名字：`LOGS` `CMD`

```bash
# CMD tab — 從 project root 執行：build 並啟動 Postgres、StateFlow，
# 以及所有 demo service（step1、step2、llm-adapter、ocr/ner/summarize workers）。
docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build
```

```bash
# LOGS tab — 持續串流每個 service 的 stdout，取代以前分開的 terminal tab。
docker compose -f docker-compose.yml -f docker-compose.demo.yml \
  logs -f --no-log-prefix step1 step2 llm-adapter stateflow
```

驗證：`curl -s http://localhost:8080/runs/__probe__` 回傳 404（server 已啟動，路由不存在——預期行為）。

為簡潔起見，以下步驟都假設：

```bash
alias dc='docker compose -f docker-compose.yml -f docker-compose.demo.yml'
```

---

## 場景之間 Reset

```bash
# CMD tab
dc exec -T postgres psql -U stateflow -d stateflow -c "TRUNCATE workflows CASCADE;"
```

---

## 查詢速查

```bash
# Run 狀態 + 所有 steps
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool

# Postgres 原始狀態
dc exec -T postgres psql -U stateflow -d stateflow \
  -c "SELECT step_name, status, output IS NOT NULL AS done FROM steps ORDER BY seq;"

# DLQ
curl -s http://localhost:8080/dlq | python3 -m json.tool

# Worker 被呼叫幾次
dc logs --no-log-prefix step1 2>/dev/null | grep -c "received step"
dc logs --no-log-prefix step2 2>/dev/null | grep -c "received step"

# Planner 被呼叫幾次
dc logs --no-log-prefix llm-adapter 2>/dev/null | grep -c "Planner called"
```

---

# 場景 A：Happy Path

**示範：** LLM Planner 驅動 2 步 pipeline 到完成。

## 步驟

`step1`/`step2` 在一次性準備階段已經啟動（各自 delay=1s，來自
docker-compose.demo.yml 的預設值）。Dummy adapter 把 step1 導向
`step1:5010`，step2 導向 `step2:5011`，主機上也能用相同的 port 存取。

**CMD tab：**
```bash
WORKFLOW_ID=$(curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "demo-A",
    "planner_type": "http",
    "planner_config": {"url": "http://llm-adapter:9000/decide"}
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

**LOGS tab** 應看到剛好 3 次 planner 呼叫：
```
[ADAPTER] Planner called  history=[]                 → step1 決定（→ step1:5010）
[ADAPTER] Planner called  history=['step1']          → step2 決定（→ step2:5011）
[ADAPTER] Planner called  history=['step1', 'step2'] → done
```

**成功條件：** run `status: DONE`，兩個 steps 都是 `DONE`。

## Reset → 進場景 B

---

# 場景 B：Worker 掛掉 → DLQ → Replay

**示範：** Step2 worker 不在線 → 重試 3 次 → DLQ → Replay 恢復；step1 不重跑。

## 步驟

**CMD tab：停掉 step2（故意不在線）**
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

**等 ~15 秒**（StateFlow 重試 step2 三次，每次 ~5 秒間隔）

## 查看進度

**Step2 retry 歷史：**
```bash
dc exec -T postgres psql -U stateflow -d stateflow \
  -c "SELECT s.step_name, a.attempt_number, a.status
      FROM attempts a JOIN steps s ON a.step_id = s.step_id
      ORDER BY a.dispatched_at;"
```

**確認進 DLQ：**
```bash
curl -s http://localhost:8080/dlq | python3 -m json.tool
```

## Replay

**CMD tab：把 step2 啟動回來**
```bash
dc up -d step2
```

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
dc logs --no-log-prefix step1 2>/dev/null | grep -c "received step"   # → 1
```

**成功條件：**
- run `status: DONE`
- step1 被呼叫次數 = 1（Replay 只從 step2 繼續，不重跑已完成的 step1）

## Reset → 進場景 C

---

# 場景 C：Orchestrator 崩潰 → Recovery

**示範：** Kill orchestrator → 重啟 → Recovery 只 re-dispatch step1，planner 不被重新呼叫（Barrier 1）。

## 步驟

**CMD tab：把 step1 用 5s delay 重新建立，製造 crash 視窗**
```bash
STEP1_DELAY=5 dc up -d --force-recreate step1
dc up -d step2   # 確保 step2（delay=1s）也在跑
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

等 **LOGS tab** 看到第一次 Planner called（step1 已被 DECIDED 並 dispatch），然後：

**CMD tab：kill orchestrator container**
```bash
dc kill stateflow
```

## 查看 Crash 後狀態

```bash
dc exec -T postgres psql -U stateflow -d stateflow \
  -c "SELECT step_name, status, output IS NOT NULL AS checkpointed FROM steps WHERE run_id = '$RUN_ID' ORDER BY seq;"
# step1: RUNNING, checkpointed=f  ← Barrier 1 已 fire（decision in DB），Barrier 2 尚未
```

## 重啟 Orchestrator

```bash
dc up -d stateflow
```

**LOGS tab** 應看到 recovery log：
```
msg="[RECOVERY] found in-progress runs" count=1
msg="[RECOVERY] resuming run" steps_done=0 pending_step=step1
```

## 驗證

```bash
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool
```

**計算總呼叫次數（應 ≤ 3）：**
```bash
dc logs --no-log-prefix llm-adapter 2>/dev/null | grep -c "Planner called"
```

**成功條件：**
- run `status: DONE`
- Planner 總呼叫次數 ≤ 3（recovery re-dispatch 不觸發新的 planner call）
- `[RECOVERY] resuming run` log 出現

## 把 step1 的 delay 改回預設值，方便下次執行

```bash
STEP1_DELAY=1 dc up -d --force-recreate step1
```

---

## 接你自己的 Worker

把 `worker_url` 換成任何能接 `POST /run` 的服務。如果你的服務也是同一個
compose 網路上的 service，用 service 名稱定址（例如
`http://my-service:PORT/run`）；否則任何可連到的 URL 都可以（例如主機上跑的
服務用 `http://host.docker.internal:PORT/run`）：

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
