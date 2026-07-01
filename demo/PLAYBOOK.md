# StateFlow Demo Playbook

---

## 一次性準備

開 **5 個 terminal tab**，標好名字：`ORCH` `OCR` `NER` `SUM` `CMD`

```bash
# 任一 tab — Build binary
go build -o demo/stateflow ./cmd/stateflow/

# 任一 tab — 啟動 Postgres
docker run -d --name stateflow-pg-test \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=stateflow_test \
  -p 5432:5432 postgres:16-alpine

# CMD tab — 建 DB + schema
docker exec stateflow-pg-test psql -U postgres \
  -c "CREATE DATABASE stateflow_demo;" postgres
docker exec -i stateflow-pg-test psql -U postgres -d stateflow_demo \
  < migrations/001_initial.sql
```

---

## 啟動所有元件

在各自的 tab 執行，**整個 session 保持跑著**：

```bash
# ORCH tab
DATABASE_URL="postgresql://postgres:postgres@localhost:5432/stateflow_demo?sslmode=disable" \
LISTEN_ADDR=":8080" ./demo/stateflow

# OCR tab
WORKER_NAME=ocr WORKER_PORT=5010 WORKER_DELAY=1 python3 demo/worker.py

# NER tab
WORKER_NAME=ner WORKER_PORT=5011 WORKER_DELAY=2 python3 demo/worker.py

# SUM tab
WORKER_NAME=summarize WORKER_PORT=5012 WORKER_DELAY=1 python3 demo/worker.py
```

驗證：`curl -s http://localhost:8080/dlq` → 應回傳 `{"entries":[]}`

---

## 場景之間 Reset（每個場景結束後跑）

```bash
# CMD tab
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo \
  -c "TRUNCATE workflows CASCADE;"
> /tmp/worker_ocr.log; > /tmp/worker_ner.log; > /tmp/worker_summarize.log
```

---

## 查詢速查（隨時可用）

```bash
# Run 狀態 + 所有 steps
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool

# Postgres 原始狀態
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo \
  -c "SELECT step_name, status, output IS NOT NULL AS done FROM steps ORDER BY seq;"

# Worker 被呼叫幾次
grep -c "received step" /tmp/worker_ocr.log
grep -c "received step" /tmp/worker_ner.log
```

---

# 場景 A：Happy Path

## 步驟

**CMD tab：**

```bash
# 建 workflow
WORKFLOW_ID=$(curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "demo-A",
    "planner_type": "static",
    "planner_config": {
      "steps": [
        {"name":"ocr",       "worker_url":"http://localhost:5010/run","mode":"sync","timeout_seconds":30},
        {"name":"ner",       "worker_url":"http://localhost:5011/run","mode":"sync","timeout_seconds":30},
        {"name":"summarize", "worker_url":"http://localhost:5012/run","mode":"sync","timeout_seconds":30}
      ]
    }
  }' | python3 -c "import json,sys; print(json.load(sys.stdin)['workflow_id'])")

# 啟動 run
RUN_ID=$(curl -s -X POST "http://localhost:8080/workflows/$WORKFLOW_ID/runs" \
  -H "Content-Type: application/json" \
  -d '{"workflow_input":{"doc":"quarterly_report.pdf"}}' \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['run_id'])")

echo $RUN_ID
```

## 查看進度

```bash
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool
```

看到 `"status": "DONE"` 和三個 steps 都是 `DONE` 即成功。

## Reset → 進場景 B

---

# 場景 B：Worker 掛掉 → DLQ → Replay

## 步驟

**1. NER tab：Ctrl+C 停掉 ner worker**

**2. CMD tab：建 workflow + 啟動 run**（和場景 A 完全一樣的指令）

```bash
WORKFLOW_ID=$(curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "demo-B",
    "planner_type": "static",
    "planner_config": {
      "steps": [
        {"name":"ocr",       "worker_url":"http://localhost:5010/run","mode":"sync","timeout_seconds":30},
        {"name":"ner",       "worker_url":"http://localhost:5011/run","mode":"sync","timeout_seconds":30},
        {"name":"summarize", "worker_url":"http://localhost:5012/run","mode":"sync","timeout_seconds":30}
      ]
    }
  }' | python3 -c "import json,sys; print(json.load(sys.stdin)['workflow_id'])")

RUN_ID=$(curl -s -X POST "http://localhost:8080/workflows/$WORKFLOW_ID/runs" \
  -H "Content-Type: application/json" \
  -d '{"workflow_input":{"doc":"report.pdf"}}' \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['run_id'])")
```

**3. 等 ~15 秒**（StateFlow 自動重試 ner 3 次，每次 5 秒間隔）

**4. ⏸ 看 retry 歷史**

```bash
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo \
  -c "SELECT s.step_name, a.attempt_number, a.status
      FROM attempts a JOIN steps s ON a.step_id = s.step_id
      ORDER BY a.dispatched_at;"
```

**5. ⏸ 確認進 DLQ**

```bash
curl -s http://localhost:8080/dlq | python3 -m json.tool
```

```bash
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo \
  -c "SELECT step_name, status FROM steps ORDER BY seq;"
# ocr=DONE, ner=DLQ, summarize 不存在
```

**6. 重啟 NER，然後 Replay**

```bash
# NER tab：重啟
WORKER_NAME=ner WORKER_PORT=5011 WORKER_DELAY=2 python3 demo/worker.py
```

```bash
# CMD tab：取 DLQ id 然後 replay
DLQ_ID=$(curl -s http://localhost:8080/dlq | \
  python3 -c "import json,sys; print(json.load(sys.stdin)['entries'][0]['id'])")

curl -s -X POST "http://localhost:8080/dlq/$DLQ_ID/replay" \
  -H "Content-Type: application/json" -d '{}'
```

**7. ⏸ 確認完成 + ocr 沒有重跑**

```bash
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool

grep -c "received step" /tmp/worker_ocr.log   # 必須是 1
```

## Reset → 進場景 C

---

# 場景 C：Orchestrator 崩潰 → Recovery

## 步驟

**1. NER tab：把 delay 改成 8 秒**（Ctrl+C 後重啟）

```bash
WORKER_NAME=ner WORKER_PORT=5011 WORKER_DELAY=8 python3 demo/worker.py
```

**2. CMD tab：啟動 run**

```bash
WORKFLOW_ID=$(curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "demo-C",
    "planner_type": "static",
    "planner_config": {
      "steps": [
        {"name":"ocr",       "worker_url":"http://localhost:5010/run","mode":"sync","timeout_seconds":30},
        {"name":"ner",       "worker_url":"http://localhost:5011/run","mode":"sync","timeout_seconds":30},
        {"name":"summarize", "worker_url":"http://localhost:5012/run","mode":"sync","timeout_seconds":30}
      ]
    }
  }' | python3 -c "import json,sys; print(json.load(sys.stdin)['workflow_id'])")

RUN_ID=$(curl -s -X POST "http://localhost:8080/workflows/$WORKFLOW_ID/runs" \
  -H "Content-Type: application/json" \
  -d '{"workflow_input":{"doc":"report.pdf"}}' \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['run_id'])")
```

**3. ⏸ 等 ocr 完成，再 kill orchestrator**

```bash
# 輪詢直到看到 ocr=DONE, ner=RUNNING
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool
```

**4. ORCH tab：Ctrl+C**（或 `kill -9 $(pgrep -f "demo/stateflow")`）

**5. ⏸ 查 Postgres — orchestrator 已死，資料還在**

```bash
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo \
  -c "SELECT step_name, status, output IS NOT NULL AS checkpointed FROM steps ORDER BY seq;"
# ocr: DONE, checkpointed=t
# ner: RUNNING, checkpointed=f   ← crash 點
# summarize: 不存在
```

**6. ORCH tab：重啟**

```bash
DATABASE_URL="postgresql://postgres:postgres@localhost:5432/stateflow_demo?sslmode=disable" \
LISTEN_ADDR=":8080" ./demo/stateflow
```

看到 recovery log：

```
msg="[RECOVERY] found in-progress runs" count=1
msg="[RECOVERY] resuming run" steps_done=1 pending_step=ner
```

**7. ⏸ Run 繼續跑，確認 ocr 沒有重跑**

```bash
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool

grep -c "received step" /tmp/worker_ocr.log   # 必須是 1，不是 2
grep -c "received step" /tmp/worker_ner.log   # 2（crash前1 + recovery後1）
```

## Reset → 結束或繼續場景 D

---

# 場景 D：LLM Planner（可選）

## 步驟

**1. 新 tab：啟動 LLM adapter**

```bash
# 不需要 API key（DUMMY 模式）
python3 llm_client_demo/llm_adapter.py

# 或用真正的 Claude：
ANTHROPIC_API_KEY="sk-ant-..." python3 llm_client_demo/llm_adapter.py
```

**2. CMD tab：建 http planner workflow**

```bash
WORKFLOW_ID=$(curl -s -X POST http://localhost:8080/workflows \
  -H "Content-Type: application/json" \
  -d '{
    "name": "demo-D",
    "planner_type": "http",
    "planner_config": {"url": "http://localhost:9000/decide"}
  }' | python3 -c "import json,sys; print(json.load(sys.stdin)['workflow_id'])")

RUN_ID=$(curl -s -X POST "http://localhost:8080/workflows/$WORKFLOW_ID/runs" \
  -H "Content-Type: application/json" \
  -d '{"workflow_input":{"task":"analyze document"}}' \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['run_id'])")
```

**3. ⏸ LLM adapter tab 看 planner 被呼叫**

```
[ADAPTER] Planner called  history=[]         → decides step1
[ADAPTER] Planner called  history=['step1']  → decides step2
[ADAPTER] Planner called  history=['step1','step2'] → done
```

**4. 確認完成**

```bash
curl -s http://localhost:8080/runs/$RUN_ID | python3 -m json.tool
```

---

## 接你自己的 Worker（隨時可以加）

在任何場景的 workflow 裡，把 `worker_url` 換成你的服務 URL：

```json
{"name":"my-step","worker_url":"http://YOUR_SERVICE/run","mode":"sync","timeout_seconds":30}
```

你的服務只需要：

```python
@app.route("/run", methods=["POST"])
def run():
    body = request.get_json()
    # body["workflow_input"]  原始輸入
    # body["history"]         前面所有 step 的結果
    return jsonify({"result": "your output here"})
```
