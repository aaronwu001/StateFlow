# StateFlow Demo Playbook

這份文件是給 **你（presenter）** 使用的操作手冊。

你自己決定什麼時候啟動什麼、什麼時候 kill 掉哪個元件、什麼時候暫停說明。
每個場景都有「此刻可以查什麼」的指令，讓你能即時向對方展示系統內部狀態。

---

## 目錄

1. [環境準備（一次性）](#1-環境準備)
2. [啟動各元件的指令](#2-啟動各元件)
3. [即時狀態查詢速查表](#3-即時狀態查詢速查表)
4. [設定說明：如何客製化 pipeline](#4-設定說明)
5. [場景 A：基本 Pipeline（靜態 Planner）](#場景-a基本-pipeline靜態-planner)
6. [場景 B：Worker 掛掉 → DLQ → Replay](#場景-bworker-掛掉--dlq--replay)
7. [場景 C：Orchestrator 崩潰 → Recovery](#場景-corchestrator-崩潰--recovery)
8. [場景 D：LLM 驅動 Pipeline](#場景-dllm-驅動-pipeline)
9. [場景 E：接入你自己的 Worker](#場景-e接入你自己的-worker)

---

## 1. 環境準備

只需要做一次。

### Postgres

```bash
docker run -d --name stateflow-pg-test \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=stateflow_test \
  -p 5432:5432 \
  postgres:16-alpine
```

驗證：`docker ps | grep stateflow-pg-test`

### 建立 demo 用的 database

```bash
docker exec stateflow-pg-test psql -U postgres -c "CREATE DATABASE stateflow_demo;" postgres
docker exec -i stateflow-pg-test psql -U postgres -d stateflow_demo \
  < migrations/001_initial.sql
```

### Build binary

```bash
go build -o demo/stateflow ./cmd/stateflow/
```

### Python 套件

```bash
pip install flask requests
```

---

## 2. 啟動各元件

**每個指令都在獨立的 terminal window 執行**，這樣你可以即時看到每個元件的 log。

### Orchestrator

```bash
DATABASE_URL="postgresql://postgres:postgres@localhost:5432/stateflow_demo?sslmode=disable" \
LISTEN_ADDR=":8080" \
./demo/stateflow
```

成功看到：`starting HTTP server addr=:8080`

### 通用 Worker（`demo/worker.py`）

環境變數控制行為：

| 變數 | 預設值 | 說明 |
|------|--------|------|
| `WORKER_NAME` | `worker` | log 裡顯示的名稱，也是辨識用 |
| `WORKER_PORT` | `5010` | 監聽 port |
| `WORKER_DELAY` | `1` | 模擬處理時間（秒） |

```bash
# 範例：啟動三個 worker，各自在不同 terminal
WORKER_NAME=ocr       WORKER_PORT=5010 WORKER_DELAY=1 python3 demo/worker.py
WORKER_NAME=ner       WORKER_PORT=5011 WORKER_DELAY=2 python3 demo/worker.py
WORKER_NAME=summarize WORKER_PORT=5012 WORKER_DELAY=1 python3 demo/worker.py
```

### Async Worker（有 callback 的版本，用於場景 B/C 進階展示）

```bash
# 這個 worker 接受 job 後立刻回 202，在背景處理完再 callback
STATEFLOW_URL="http://localhost:8080" python3 demo/workers/ner_worker.py
```

端口：5002

### LLM Adapter（HTTP Planner）

```bash
# DUMMY 模式（不需要 API key）：
python3 llm_client_demo/llm_adapter.py

# REAL 模式（呼叫真正的 Claude）：
ANTHROPIC_API_KEY="sk-ant-..." python3 llm_client_demo/llm_adapter.py
```

端口：9000。模式會自動偵測並印在啟動 log 裡。

---

## 3. 即時狀態查詢速查表

**隨時可以下的指令**，貼到另一個 terminal 用。

### 查 Run 狀態（最常用）

```bash
curl -s http://localhost:8080/runs/<run_id> | python3 -m json.tool
```

輸出重點：
- `"status"`: `RUNNING` / `DONE` / `FAILED`
- `"steps"`: 每個 step 的 `status`（`DECIDED` / `RUNNING` / `DONE` / `FAILED` / `DLQ`）
- step 的 `"output"`: Barrier 2 寫入的結果（有值 = checkpoint 完成）

### 查 DLQ

```bash
curl -s http://localhost:8080/dlq | python3 -m json.tool
```

### 查 Postgres 原始狀態

```bash
# 看所有 steps（最直接）
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo -c \
  "SELECT step_name, status, current_attempt_id IS NOT NULL AS dispatched, output IS NOT NULL AS checkpointed FROM steps ORDER BY seq;"

# 看 run
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo -c \
  "SELECT run_id, status, created_at, updated_at FROM runs;"

# 看 DLQ
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo -c \
  "SELECT id, run_id, reason, created_at FROM dead_letter_queue;"

# 看某個 step 的所有 dispatch 嘗試
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo -c \
  "SELECT attempt_id, attempt_number, status, dispatched_at, resolved_at FROM attempts ORDER BY dispatched_at;"
```

### 查 Worker 被呼叫幾次

```bash
# worker.py 的 log 存在 /tmp/worker_{name}.log
grep -c "received step" /tmp/worker_ocr.log
grep -c "received step" /tmp/worker_ner.log

# 看完整 worker log
cat /tmp/worker_ocr.log
```

### 查 Orchestrator Log（看 Recovery 訊息）

```bash
# 如果 orchestrator 是用 >> 重導到 log 的
grep RECOVERY /tmp/stateflow.log

# 如果 orchestrator 直接跑在 terminal，就在那個視窗看
```

### 重置 DB（場景之間清空）

```bash
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo \
  -c "TRUNCATE workflows CASCADE;"
```

---

## 4. 設定說明

### 如何建立一個 Workflow

StateFlow 是 **API-first**。沒有設定檔；你透過 HTTP API 定義 pipeline。

**第一步：建立 Workflow（定義 pipeline 結構）**

```bash
curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-pipeline",
    "planner_type": "static",
    "planner_config": {
      "steps": [
        {"name": "step1", "worker_url": "http://localhost:5010/run", "mode": "sync",  "timeout_seconds": 30},
        {"name": "step2", "worker_url": "http://localhost:5011/run", "mode": "sync",  "timeout_seconds": 30},
        {"name": "step3", "worker_url": "http://localhost:5012/run", "mode": "async", "timeout_seconds": 60}
      ]
    }
  }'
# 回傳 {"workflow_id": "wf-..."}
```

**第二步：啟動一個 Run（帶入你的資料）**

```bash
curl -s -X POST http://localhost:8080/workflows/<workflow_id>/runs \
  -H "Content-Type: application/json" \
  -d '{"workflow_input": {"doc": "my_file.pdf", "lang": "zh"}}'
# 回傳 {"run_id": "run-..."}
```

---

### Planner 類型

#### Static Planner（寫死順序）

```json
"planner_type": "static",
"planner_config": {
  "steps": [
    {"name": "ocr", "worker_url": "http://your-ocr:8000/run", "mode": "sync", "timeout_seconds": 30},
    {"name": "ner", "worker_url": "http://your-ner:8001/run", "mode": "sync", "timeout_seconds": 30}
  ]
}
```

- 步驟順序固定
- Planner 在每次 step 完成後把完整 history 傳給下一個 worker
- 適合 ETL pipeline、文件處理等順序明確的場景

#### HTTP Planner（LLM 或任何決策服務）

```json
"planner_type": "http",
"planner_config": {
  "url": "http://localhost:9000/decide",
  "timeout_seconds": 30
}
```

StateFlow 每次需要決定下一步時，POST 給這個 URL：

```json
// StateFlow 送出
{
  "run_id": "run-...",
  "workflow_input": {"task": "..."},
  "history": [
    {"name": "step1", "status": "DONE", "output": {...}}
  ]
}

// Planner 必須回傳（只能是 JSON，不能有 markdown）
{"status": "continue", "step": {"name": "step2", "worker_url": "...", "mode": "sync", "timeout_seconds": 30, "input": {...}}}
// 或
{"status": "done"}
// 或
{"status": "fail"}
```

---

### Worker 模式：Sync vs Async

#### Sync Worker（簡單，適合大多數場景）

```
StateFlow  →  POST /run  →  Worker
StateFlow  ←  200 + JSON result  ←  Worker
```

Worker 只需要實作：

```python
@app.route("/run", methods=["POST"])
def run():
    body = request.get_json()
    # body 包含 {"workflow_input": {...}, "history": [...]}
    # 或 static planner 會包裝好的 input
    result = do_work(body)
    return jsonify(result)  # 回傳任何 JSON，StateFlow 存下來
```

#### Async Worker（長時間任務，避免 HTTP timeout）

```
StateFlow  →  POST /run  →  Worker（立刻回 202）
                              Worker 在背景處理
StateFlow  ←  POST /tasks/complete  ←  Worker（完成後 callback）
```

Worker 的 `/run` endpoint 必須：
1. 立刻回傳 `202 Accepted`（不然 StateFlow 認為失敗）
2. 在 background 處理
3. 完成後 POST 到 StateFlow：

```python
import requests, threading

@app.route("/run", methods=["POST"])
def run():
    body = request.get_json()
    step_id    = body["step_id"]      # StateFlow 提供
    attempt_id = body["attempt_id"]   # StateFlow 提供（每次 dispatch 都不同）
    input_data = body["input"]        # 你的實際資料

    threading.Thread(target=process, args=(step_id, attempt_id, input_data)).start()
    return jsonify({"accepted": True}), 202

def process(step_id, attempt_id, input_data):
    result = do_heavy_work(input_data)
    requests.post("http://localhost:8080/tasks/complete", json={
        "step_id":    step_id,
        "attempt_id": attempt_id,   # 必須用 StateFlow 給的 attempt_id
        "output":     result
    })
```

> **關鍵**：`attempt_id` 是 StateFlow 給的一次性 UUID，不是你自己生成的。
> 每次 re-dispatch（crash recovery 或 retry）都是新的 `attempt_id`。
> 這是 dedup guard：舊的 callback 到了 StateFlow，發現 `attempt_id` 不符，直接忽略。

---

### 連接你自己的 Worker

只要你的服務能 handle `POST /run`，就可以直接接上：

```bash
# 建立 workflow，指向你的服務
curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-custom-pipeline",
    "planner_type": "static",
    "planner_config": {
      "steps": [
        {
          "name": "my-step",
          "worker_url": "http://192.168.1.100:8000/run",
          "mode": "sync",
          "timeout_seconds": 60
        }
      ]
    }
  }'
```

Worker URL 可以是：
- `http://localhost:PORT/run`（本機）
- `http://192.168.1.x:PORT/run`（同網段）
- `http://my-service.internal/run`（內部 DNS）
- 任何 HTTP 可達的位置

---

## 場景 A：基本 Pipeline（靜態 Planner）

**展示重點：** StateFlow 驅動一個 3-step pipeline 從頭到尾。

### 準備（分開 4 個 terminal）

```bash
# Terminal 1: Orchestrator
DATABASE_URL="postgresql://postgres:postgres@localhost:5432/stateflow_demo?sslmode=disable" \
LISTEN_ADDR=":8080" ./demo/stateflow

# Terminal 2: OCR worker
WORKER_NAME=ocr WORKER_PORT=5010 WORKER_DELAY=1 python3 demo/worker.py

# Terminal 3: NER worker
WORKER_NAME=ner WORKER_PORT=5011 WORKER_DELAY=2 python3 demo/worker.py

# Terminal 4: Summarize worker
WORKER_NAME=summarize WORKER_PORT=5012 WORKER_DELAY=1 python3 demo/worker.py
```

### 步驟

**1. 建立 workflow（定義 pipeline）**

```bash
curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "demo-pipeline",
    "planner_type": "static",
    "planner_config": {
      "steps": [
        {"name": "ocr",       "worker_url": "http://localhost:5010/run", "mode": "sync", "timeout_seconds": 30},
        {"name": "ner",       "worker_url": "http://localhost:5011/run", "mode": "sync", "timeout_seconds": 30},
        {"name": "summarize", "worker_url": "http://localhost:5012/run", "mode": "sync", "timeout_seconds": 30}
      ]
    }
  }'
```

> 💬 *"Pipeline 就是一個 JSON 定義，每個 step 指向一個 URL。沒有設定檔，也不需要重啟服務。"*

記下 `workflow_id`。

**2. 啟動 Run**

```bash
curl -s -X POST http://localhost:8080/workflows/<workflow_id>/runs \
  -H "Content-Type: application/json" \
  -d '{"workflow_input": {"doc": "quarterly_report.pdf"}}'
```

記下 `run_id`。

**3. 🔍 即時查看進度**

```bash
# 在另一個 terminal 持續 poll
watch -n 1 'curl -s http://localhost:8080/runs/<run_id> | python3 -m json.tool'
```

或手動查：

```bash
curl -s http://localhost:8080/runs/<run_id> | python3 -m json.tool
```

你會看到 steps 一個一個從 `RUNNING` 變成 `DONE`。

**4. 確認完成**

```bash
# 最終狀態
curl -s http://localhost:8080/runs/<run_id> | \
  python3 -c "import json,sys; d=json.load(sys.stdin); [print(s['step_name'], s['status']) for s in d['steps']]"
```

---

## 場景 B：Worker 掛掉 → DLQ → Replay

**展示重點：**
- Step 失敗 → 自動重試 → 超過限制進 DLQ
- DLQ 不是終點：一個 API call 就能 replay
- 已完成的步驟不會重跑

### 準備

```bash
# Terminal 1: Orchestrator
DATABASE_URL="postgresql://postgres:postgres@localhost:5432/stateflow_demo?sslmode=disable" \
LISTEN_ADDR=":8080" ./demo/stateflow

# Terminal 2: OCR（正常）
WORKER_NAME=ocr WORKER_PORT=5010 WORKER_DELAY=1 python3 demo/worker.py

# Terminal 3: Summarize（正常）
WORKER_NAME=summarize WORKER_PORT=5012 WORKER_DELAY=1 python3 demo/worker.py

# NER（port 5011）先不啟動
```

### 步驟

**1. 建立 workflow 並啟動 run**（同場景 A）

**2. ⏸ 暫停觀察：OCR 完成，NER 開始重試**

```bash
# 查 run 狀態 — 你會看到 ocr=DONE, ner=RUNNING（重試中）
curl -s http://localhost:8080/runs/<run_id> | python3 -m json.tool
```

```bash
# 查 attempts — 看到 ner 有多個 FAILED 嘗試
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo -c \
  "SELECT attempt_number, status, dispatched_at FROM attempts ORDER BY dispatched_at;"
```

> 💬 *"ner 連線失敗，StateFlow 自動重試，預設 3 次，每次間隔 5 秒。這些都是可以設定的。"*

等 ~15 秒讓重試耗盡。

**3. ⏸ 查看 DLQ**

```bash
curl -s http://localhost:8080/dlq | python3 -m json.tool
```

```bash
# Postgres 看 DLQ
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo -c \
  "SELECT id, reason, context FROM dead_letter_queue;"
```

> 💬 *"DLQ entry 裡有完整的 context，包含 run_id、step_id、最後一次錯誤。這是審計記錄，不會被刪除。"*

記下 DLQ `id`。

**4. 啟動 NER worker**

```bash
# 在新的 terminal 啟動
WORKER_NAME=ner WORKER_PORT=5011 WORKER_DELAY=1 python3 demo/worker.py
```

**5. Replay**

```bash
curl -s -X POST http://localhost:8080/dlq/<id>/replay \
  -H "Content-Type: application/json" -d '{}'
```

> 💬 *"就這樣，一個 API call。不需要重新送 job，因為 StateFlow 在 Barrier 1 就已經把決策存進 DB。Replay 只是告訴 orchestrator 重新 dispatch 那個已決策的 step。"*

**6. ⏸ 確認 ocr 沒有重跑**

```bash
# Worker log
grep -c "received step" /tmp/worker_ocr.log
# 預期：1（不是 2）
```

```bash
# DB 確認
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo -c \
  "SELECT step_name, status FROM steps ORDER BY seq;"
```

> 💬 *"ocr 只被呼叫了一次。它的結果在 Barrier 2 時已經 checkpoint 進 DB，replay 不會碰它。"*

---

## 場景 C：Orchestrator 崩潰 → Recovery

**展示重點：這是最核心的 demo。**
- SIGKILL orchestrator（沒有 graceful shutdown 機會）
- Restart → 自動 recovery → run 繼續
- 已完成的 step 不重跑

### 準備

**關鍵設定：NER 用 8 秒 delay，製造 crash 視窗**

```bash
# Terminal 1: Orchestrator（等一下會被 kill）
DATABASE_URL="postgresql://postgres:postgres@localhost:5432/stateflow_demo?sslmode=disable" \
LISTEN_ADDR=":8080" ./demo/stateflow

# Terminal 2: OCR（快，1秒）
WORKER_NAME=ocr WORKER_PORT=5010 WORKER_DELAY=1 python3 demo/worker.py

# Terminal 3: NER（慢，8秒 — 製造 crash 視窗）
WORKER_NAME=ner WORKER_PORT=5011 WORKER_DELAY=8 python3 demo/worker.py

# Terminal 4: Summarize（快，1秒）
WORKER_NAME=summarize WORKER_PORT=5012 WORKER_DELAY=1 python3 demo/worker.py
```

### 步驟

**1. 啟動 Run**（建立 workflow + 開始 run，同場景 A）

**2. ⏸ 等 OCR 完成，NER 開始執行**

```bash
# Poll 直到 ocr=DONE（大約 1-2 秒後）
curl -s http://localhost:8080/runs/<run_id> | python3 -m json.tool
```

你會看到：
```json
"steps": [
  {"step_name": "ocr", "status": "DONE"},   ← 已完成
  {"step_name": "ner", "status": "RUNNING"} ← 進行中（8秒 worker）
]
```

> 💬 *"看這裡：ocr 完成了，ner 正在跑。這個時候 orchestrator 在等 ner 的回應。"*

**3. Kill orchestrator（在 Terminal 1 按 Ctrl+C，或找 PID kill -9）**

```bash
# 找 PID
pgrep -f stateflow

# SIGKILL（模擬最壞情況：process 直接被 OS 殺掉）
kill -9 <pid>
```

> 💬 *"kill -9，沒有 graceful shutdown，沒有機會做任何清理。這是最壞的情況。"*

**4. ⏸ 查看 Postgres 原始狀態（orchestrator 已死）**

```bash
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo -c \
  "SELECT step_name, status, current_attempt_id IS NOT NULL AS dispatched, output IS NOT NULL AS checkpointed
   FROM steps ORDER BY seq;"
```

你會看到：
```
 step_name | status  | dispatched | checkpointed
-----------+---------+------------+--------------
 ocr       | DONE    | t          | t            ← Barrier 2 已完成
 ner       | RUNNING | t          | f            ← dispatched 但 output=NULL
```

> 💬 *"注意 ner 的狀態：dispatched=true 但 checkpointed=false。這是 Barrier 2 還沒觸發的狀態。DB 知道 ner 曾經被 dispatch，但沒有結果。*
> *Summarize 根本不在表裡，因為 planner 還沒有被問到下一步。"*

```bash
# 也可以看 runs 表
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo -c \
  "SELECT run_id, status FROM runs;"
```

> 💬 *"run 還是 RUNNING 狀態，因為 Postgres 裡沒有任何東西說它失敗了。事實就是：job 被中斷了，但沒有丟失。"*

**5. 重啟 Orchestrator**

```bash
DATABASE_URL="postgresql://postgres:postgres@localhost:5432/stateflow_demo?sslmode=disable" \
LISTEN_ADDR=":8080" ./demo/stateflow
```

你立刻就會看到：

```
msg="[RECOVERY] found in-progress runs" count=1
msg="[RECOVERY] resuming run" run_id=run-... steps_done=1 pending_step=ner
```

> 💬 *"Recovery 在 HTTP server 接受新請求之前就跑完了。它讀 DB，找到那個 RUNNING run，知道 ner 是 pending step，直接 re-dispatch。沒有重播，沒有從頭開始。"*

**6. ⏸ 觀察 recovery 過程**

```bash
# 持續 poll
watch -n 1 'curl -s http://localhost:8080/runs/<run_id> | python3 -m json.tool'
```

你會看到 ner 變回 RUNNING（新的 attempt），然後 DONE，然後 summarize 出現並 DONE。

**7. 確認 ocr 沒有重跑**

```bash
grep -c "received step" /tmp/worker_ocr.log
# 預期：1
```

```bash
# 確認 ocr 的 output 沒有改變（timestamp 和第一次一樣）
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo -c \
  "SELECT step_name, output->>'processed_at' AS processed_at FROM steps ORDER BY seq;"
```

> 💬 *"ocr 的 processed_at timestamp 和 crash 之前完全相同。它沒有被重跑，StateFlow 讀的是 DB 裡 checkpoint 好的結果。"*

```bash
# ner 有 2 次 dispatch（但因為 worker 沒有 idempotency cache，會真的跑兩次）
grep -c "received step" /tmp/worker_ner.log
# 預期：2（crash 前一次 + recovery 後一次）
```

> 💬 *"ner 被 dispatch 了兩次。這在 sync 模式下是預期行為：第一次的 response 因為 crash 消失了，所以 recovery 重新 dispatch。*
> *如果你的 worker 有 idempotency（比如基於 step_id 的 cache），它可以偵測到重複，跳過處理直接回傳。crash_demo.py 裡的 NER worker 展示了這個模式。"*

---

## 場景 D：LLM 驅動 Pipeline

**展示重點：** StateFlow 可以用 LLM（或任何 HTTP 服務）當 planner，動態決定下一步。

### 準備

```bash
# Terminal 1: Orchestrator
DATABASE_URL="postgresql://postgres:postgres@localhost:5432/stateflow_demo?sslmode=disable" \
LISTEN_ADDR=":8080" ./demo/stateflow

# Terminal 2: Worker（LLM adapter 會把 job dispatch 到這裡）
WORKER_NAME=worker WORKER_PORT=5010 WORKER_DELAY=1 python3 demo/worker.py

# Terminal 3: LLM Adapter
# DUMMY 模式（不需要 API key）：
python3 llm_client_demo/llm_adapter.py

# 或 REAL 模式（用真正的 Claude）：
ANTHROPIC_API_KEY="sk-ant-..." python3 llm_client_demo/llm_adapter.py
```

### 步驟

**1. 建立 HTTP Planner workflow**

```bash
curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "llm-pipeline",
    "planner_type": "http",
    "planner_config": {
      "url": "http://localhost:9000/decide",
      "timeout_seconds": 30
    }
  }'
```

> 💬 *"注意這裡的差異：static planner 裡你列出所有 steps；http planner 你只給一個 URL。每次 step 完成後，StateFlow 問這個 URL：下一步是什麼？可以是 LLM，可以是你的業務邏輯服務，可以是任何東西。"*

**2. 啟動 Run**

```bash
curl -s -X POST http://localhost:8080/workflows/<workflow_id>/runs \
  -H "Content-Type: application/json" \
  -d '{"workflow_input": {"task": "analyze this document", "priority": "high"}}'
```

**3. ⏸ 觀察 Planner 被呼叫的過程**

在 Terminal 3（LLM adapter）你會看到：

```
[ADAPTER] Planner called  run_id=run-...  history=[]
[ADAPTER] Decision: {"status":"continue","step":{"name":"step1",...}}
...
[ADAPTER] Planner called  run_id=run-...  history=['step1']
[ADAPTER] Decision: {"status":"continue","step":{"name":"step2",...}}
...
[ADAPTER] Planner called  run_id=run-...  history=['step1', 'step2']
[ADAPTER] Decision: {"status":"done"}
```

> 💬 *"每次 step 完成，StateFlow 把完整的 history 送給 planner。Planner 可以根據之前步驟的輸出決定下一步要做什麼。這是 agent-native 的設計。"*

**4. 切換到 REAL 模式（optional，需要 API key）**

```bash
# 先清資料
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo -c "TRUNCATE workflows CASCADE;"

# 啟動帶 key 的 adapter（Ctrl+C 舊的，重新啟動）
ANTHROPIC_API_KEY="sk-ant-..." python3 llm_client_demo/llm_adapter.py
```

> 💬 *"現在 Claude 真正在做決策。它看到 history，決定下一步做什麼。StateFlow 只負責把決定執行出去並確保不丟失。"*

---

## 場景 E：接入你自己的 Worker

**展示重點：** 任何語言、任何服務，只要能 handle POST /run，就能接上。

### 最簡單的 Worker（Python）

```python
# my_worker.py
from flask import Flask, request, jsonify
app = Flask(__name__)

@app.route("/run", methods=["POST"])
def run():
    body = request.get_json()
    # StateFlow static planner 送來：
    # {"workflow_input": {...}, "history": [...]}
    
    # 你的業務邏輯
    result = {"processed": True, "data": body["workflow_input"]}
    return jsonify(result)

app.run(port=9001)
```

### 最簡單的 Worker（Go）

```go
http.HandleFunc("/run", func(w http.ResponseWriter, r *http.Request) {
    var body map[string]any
    json.NewDecoder(r.Body).Decode(&body)
    
    result := map[string]any{"processed": true}
    json.NewEncoder(w).Encode(result)
})
http.ListenAndServe(":9001", nil)
```

### 接入 StateFlow

```bash
# 建立一個 workflow 指向你的 worker
curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "custom-worker-demo",
    "planner_type": "static",
    "planner_config": {
      "steps": [
        {"name": "my-step", "worker_url": "http://localhost:9001/run", "mode": "sync", "timeout_seconds": 30}
      ]
    }
  }'
```

### Worker 收到的資料格式

**Sync mode 收到的 body：**

```json
{
  "workflow_input": {"doc": "my_file.pdf"},
  "history": [
    {
      "name": "previous-step",
      "status": "DONE",
      "output": {"result": "..."}
    }
  ]
}
```

> 💬 *"Worker 拿到完整的 history，所以它能用前面 step 的結果。StateFlow 不做資料轉換，把一切都給你，你自己決定用什麼。"*

**Async mode 收到的 body：**

```json
{
  "step_id":    "run-xxx:my-step",
  "attempt_id": "uuid-per-dispatch",
  "input": {
    "workflow_input": {...},
    "history": [...]
  }
}
```

---

## 常見問答

**Q: Worker 掛掉之後怎麼知道 StateFlow 會重試？**
A: Retry policy 是全局的（目前預設 3 次，5 秒間隔）。失敗後自動重試，超過就進 DLQ。

**Q: 兩個 step 可以平行跑嗎？**
A: MVP 版本是一個 run 只有一個 loop，步驟是 sequential 的。DAG/fan-in 是後續版本的功能。

**Q: LLM 的 API call 會被算兩次 token 費用嗎（如果 crash 重啟）？**
A: 不會。Barrier 1 是關鍵：planner 的決定（包含呼叫 LLM 的結果）在 dispatch 之前就已經 persist 到 DB。crash 後是 re-dispatch，不是 re-decide，所以不會再呼叫 LLM。

**Q: 如果 LLM 本身掛掉怎麼辦？**
A: HTTP planner 有 retry（預設 3 次）。全部失敗後 run 進 DLQ（reason=planner_failed）。同樣可以 replay。

**Q: Worker 的 result 存在哪裡？**
A: Postgres `steps.output` 欄位（JSONB）。你可以直接 psql 查，也可以透過 `GET /runs/{run_id}` 的 steps[].output 拿到。

---

## 現有 Demo 工具一覽

```
demo/
├── PLAYBOOK.md          ← 你現在在看的這份文件
├── README.md            ← 技術說明（API 格式、troubleshooting）
├── worker.py            ← 通用 worker（WORKER_NAME/PORT/DELAY）
├── crash_demo.py        ← 自動化完整 demo（NER async + idempotency）
├── run_demo.sh          ← 互動式 menu 腳本（半自動化）
├── configs/
│   ├── static_3step.yaml   ← 3-step planner_config（可直接 cat 進 curl）
│   └── http_planner.yaml   ← HTTP planner config
└── workers/
    ├── ocr_worker.py       ← crash_demo 用，有 idempotency
    ├── ner_worker.py       ← crash_demo 用，async + idempotency
    └── summarize_worker.py ← crash_demo 用

llm_client_demo/
├── llm_adapter.py   ← HTTP planner（DUMMY/REAL 雙模式）
└── echo_worker.py   ← 最簡 echo worker（llm_client_demo 用）
```
