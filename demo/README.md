# StateFlow Demo Guide

StateFlow 有兩種 demo 可以跑：

| 腳本 | 特性 |
|------|------|
| [`run_demo.sh`](#interactive-demo-run_demosh) | **互動式** — 選單驅動，5 個情境，每個關鍵時刻都會暫停等你確認 |
| [`crash_demo.py`](#automated-demo-crash_demopy) | **自動** — 一鍵跑完，輸出完整的 proof markers log |

---

## Prerequisites

所有 demo 都需要：

```bash
# 1. Docker Postgres
docker run -d --name stateflow-pg-test \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=stateflow_test \
  -p 5432:5432 \
  postgres:16-alpine

# 2. Python 套件
pip install flask requests

# 3. Go 1.22+（build 用）
go version
```

驗證 container 正在跑：`docker ps | grep stateflow-pg-test`

---

## Interactive Demo (`run_demo.sh`)

這是你可以手動控制節奏、在每個關鍵時刻停下來觀察狀況的版本。

### 啟動

```bash
# 從 project root 執行
./demo/run_demo.sh
```

腳本會自動 build binary、建立乾淨的 DB，然後顯示選單：

```
   StateFlow Interactive Demo
   ══════════════════════════
   1) Happy Path (Static Planner)
   2) Worker Crash & DLQ Replay
   3) Orchestrator Crash & Recovery
   4) Happy Path (LLM Planner)
   5) Orchestrator Crash (LLM Planner)
   A) Run All Scenarios
   Q) Quit
```

### 五個情境說明

#### 情境 1 — Happy Path（靜態 Planner）

**證明：** StateFlow 能驅動一個 3-step pipeline 跑到完成。

```
ocr (5010) → ner (5011) → summarize (5012)
```

腳本會在送出 workflow 前暫停，讓你確認三個 worker 都已啟動。
完成後顯示每個 step 的狀態。

**預期結果：**
```
[DONE    ] ✓ ocr
[DONE    ] ✓ ner
[DONE    ] ✓ summarize
Status: DONE
PASS — All 3 steps completed
```

---

#### 情境 2 — Worker Crash & DLQ Replay

**證明：** worker 掛掉 → step 進 DLQ → 用一個 API call replay → run 繼續跑。
**關鍵：** replay 後，ocr（已完成的 step）不會重跑。

流程：
1. ocr (5010) 和 summarize (5012) 啟動，**ner (5011) 故意不啟動**
2. 送出 workflow → ocr 完成 → ner 連線失敗 3 次（每次 5s delay）→ 進 DLQ
3. 腳本暫停 → 你看到 DLQ entry 的 id
4. 按 Enter → 腳本啟動 ner worker，然後 POST /dlq/{id}/replay
5. run 繼續從 ner 跑到結束

**觀察重點：**
- DLQ 顯示 `reason=retry_exhausted`
- Replay 後 run status 從 FAILED → DONE
- `ocr_calls = 1`（不是 2）— 這是 durability 的核心證明

---

#### 情境 3 — Orchestrator Crash & Recovery ⭐ 最核心的 demo

**證明：** SIGKILL orchestrator → 重啟 → 從中斷點繼續；已完成的 step 不重跑。

流程：
1. 三個 worker 啟動（**ner 設 8 秒 delay** 製造 crash 視窗）
2. 送出 workflow → 腳本等待 ocr 變 DONE
3. 腳本暫停 → **你親眼看到 ocr 已完成**
4. 按 Enter → 腳本對 orchestrator 執行 `kill -9`（SIGKILL）
5. 腳本暫停 → 查詢 Postgres 原始狀態：
   ```sql
   SELECT step_name, status, dispatched FROM steps WHERE run_id = '...'
   ```
   你會看到 `ner = RUNNING`（沒有 output）、`summarize` 根本不存在
6. 按 Enter → 重啟 orchestrator → 自動 recovery 觸發
7. 看到 recovery log：
   ```
   msg="[RECOVERY] found in-progress runs" count=1
   msg="[RECOVERY] resuming run" steps_done=1 pending_step=ner
   ```
8. ner 重新 dispatch → 完成 → summarize 執行 → run DONE
9. 最後計數：`ocr 被呼叫了 1 次`

**觀察重點：**
- Recovery 不是從頭跑，是從 ner 繼續
- ocr 的 worker log `/tmp/worker_ocr.log` 只有 1 筆記錄
- ner 有 2 筆（crash 前一次 + recovery 後一次）

---

#### 情境 4 — Happy Path（LLM Planner）

**證明：** StateFlow 可以用任何 HTTP endpoint 當 planner，包括 LLM。

使用 `llm_client_demo/llm_adapter.py` 的 **DUMMY 模式**（不需要 API key）：
- 沒有 `ANTHROPIC_API_KEY` 時，adapter 用硬編碼邏輯跑 2-step pipeline
- 有 key 時自動切換成真正的 Claude

```
llm_adapter (:9000) → decides step1 → dispatches to echo_worker (:5010)
                     → decides step2 → dispatches to echo_worker (:5010)
                     → declares done
```

**預期結果：** 2 個 step 完成，adapter 被呼叫 3 次（step1 決策、step2 決策、done 判斷）

---

#### 情境 5 — Orchestrator Crash（LLM Planner）

**證明：** crash 後 recovery，planner **不會**被重新呼叫處理已決策的 step。

這展示 Barrier 1 的意義：決策一旦 persist 到 DB，crash 重啟後只是 re-dispatch，不是 re-decide。

流程類似情境 3，但 planner 是 LLM adapter。
**驗證標準：** adapter 被呼叫次數 ≤ 3（無 crash 也是 3 次，crash 不會增加）。

---

### 手動 API 互動

每個情境暫停時，你也可以直接用 curl 查詢狀態：

```bash
# 看 run 狀態
curl -s http://localhost:8080/runs/<run_id> | python3 -m json.tool

# 看 DLQ
curl -s http://localhost:8080/dlq | python3 -m json.tool

# 查原始 Postgres
docker exec stateflow-pg-test psql -U postgres -d stateflow_demo \
  -c "SELECT step_name, status, current_attempt_id IS NOT NULL as dispatched FROM steps ORDER BY seq;"

# 看 worker 呼叫次數
grep -c "received step" /tmp/worker_ocr.log
grep -c "received step" /tmp/worker_ner.log

# 看 orchestrator logs（情境 3 重啟後）
cat /tmp/stateflow.log | grep RECOVERY
```

### Troubleshooting

| 問題 | 解法 |
|------|------|
| Port already in use | `lsof -i :5010` 找出 PID 然後 kill，或重新開 terminal |
| Worker 啟動失敗 | `pip install flask` |
| DB 連線失敗 | `docker ps` 確認 container 在跑 |
| Build 失敗 | `go build ./cmd/stateflow/` 看完整錯誤 |

---

## Automated Demo (`crash_demo.py`)

全自動跑完，適合快速驗證或錄影。

```bash
cd demo
python crash_demo.py
```

**流程（約 20 秒）：**
1. Build binary
2. 建立乾淨 DB（`stateflow_demo`）
3. 啟動 3 個 worker（OCR sync 5001、NER async 5002、Summarize sync 5003）
4. 啟動 orchestrator（boot 1）
5. 建立 workflow + 開始 run
6. NER async 執行中時 **kill orchestrator**
7. NER 完成，callback 發向舊 orchestrator（失敗，safe to ignore）
8. 重啟 orchestrator（boot 2）— recovery 自動觸發
9. poll 直到 DONE，印出 proof markers

**這個 demo 的特點（vs run_demo.sh）：**
- NER 用 **async** 模式（需要 callback），展示更複雜的 recovery 情境
- NER worker 有 **idempotency cache**：recovery re-dispatch 時，worker 偵測到已處理過，直接回傳結果不重跑
- 印出 dedup guard 作動的 log：`callback: superseded attempt_id, ignoring`

**Proof log 範例：**
```
[OCR] 🔍 Processing document: quarterly_report_2026.pdf   ← 只出現一次
[NER] 🏷️  Starting entity extraction                      ← 只出現一次
💥 KILLING ORCHESTRATOR
[NER] ⚡ Already processed — returning cached result       ← idempotent re-dispatch
INFO [RECOVERY] found in-progress runs count=1
INFO [RECOVERY] resuming run steps_done=1 pending_step=ner
[SUMMARIZE] ✍️  Generating summary                        ← crash 後第一次執行
Run status: DONE
```

---

## 檔案結構

```
demo/
├── run_demo.sh              # 互動式 demo（你要的那個）
├── worker.py                # 通用 worker（WORKER_NAME/PORT/DELAY env vars）
├── crash_demo.py            # 自動化 demo
├── configs/
│   ├── static_3step.yaml    # 3-step static planner config（JSON，ocr/ner/summarize）
│   └── http_planner.yaml    # HTTP planner config（指向 llm_adapter:9000）
├── workers/
│   ├── ocr_worker.py        # crash_demo 專用，有 idempotency cache
│   ├── ner_worker.py        # crash_demo 專用，async 模式，有 cache
│   └── summarize_worker.py  # crash_demo 專用
└── README.md                # 本文件

llm_client_demo/
├── llm_adapter.py           # HTTP planner adapter（DUMMY/REAL 雙模式）
└── echo_worker.py           # 最簡單的 echo worker（llm_client_demo 用）
```

---

## 互動式 vs 自動化：如何選擇

| 場合 | 建議 |
|------|------|
| 面試 demo、想講解原理 | `run_demo.sh` — 每個關鍵步驟都能停下來解說 |
| 快速驗證功能正確 | `crash_demo.py` — 一條線跑完 |
| 展示 LLM 整合 | `run_demo.sh` 情境 4 或 5 |
| 展示 DLQ replay | `run_demo.sh` 情境 2（這個 crash_demo.py 沒有） |
