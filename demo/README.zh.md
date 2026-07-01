# StateFlow Demo（中文版）

兩種 demo 方式：

| 腳本 | 模式 | 適合場合 |
|------|------|----------|
| [`run_demo.sh`](#互動式-demo-run_demosh) | 互動式，選單驅動 | 現場演示、逐步解說 |
| [`crash_demo.py`](#自動化-demo-crash_demopy) | 全自動 | 快速驗證、錄影 |

兩者都使用 **LLM / HTTP Planner**——Planner 以獨立 HTTP 服務運行。DUMMY 模式不需要 API key；設定 `ANTHROPIC_API_KEY` 可接真正的 Claude。

> **English version:** [README.md](README.md)

---

## Prerequisites

```bash
# Docker Postgres
docker run -d --name stateflow-pg-test \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=stateflow_test \
  -p 5432:5432 \
  postgres:16-alpine

# Python 套件
pip install flask requests

# Go 1.22+（build 用）
go version
```

---

## 互動式 Demo (`run_demo.sh`)

選單驅動，每個關鍵時刻暫停，讓你有時間解說。

```bash
# 從 project root 執行
./demo/run_demo.sh
```

腳本會自動 build binary、建立乾淨的 DB，然後顯示：

```
   StateFlow Interactive Demo  (LLM Planner)
   ══════════════════════════════════════════
   1) Happy Path
   2) Worker Crash & DLQ Replay
   3) Orchestrator Crash & Recovery
   A) Run All Scenarios
   Q) Quit
```

### 三個場景說明

**場景 1 — Happy Path**
- LLM adapter 決定 step1 → step2 → done
- 驗證：planner 被呼叫剛好 3 次

**場景 2 — Worker 掛掉 → DLQ → Replay**
- step2 worker 故意不啟動 → 重試 3 次 → 進 DLQ
- 啟動 step2 worker，POST /dlq/{id}/replay → run 完成
- 驗證：step1 沒有重跑（只被呼叫 1 次）

**場景 3 — Orchestrator 崩潰 → Recovery** ⭐ 最核心的 demo
- step1 設 5 秒 delay，製造 crash 視窗
- SIGKILL orchestrator（step1 正在 in-flight）
- 重啟 orchestrator → Recovery 觸發 → step1 被 re-dispatch（NOT re-decide）
- 驗證：planner 總呼叫次數 ≤ 3，即使發生了 crash

手動逐步說明請見 playbook：
- [playbook/PLAYBOOK.zh.md](playbook/PLAYBOOK.zh.md)
- [playbook/PLAYBOOK.en.md](playbook/PLAYBOOK.en.md)

---

## 自動化 Demo (`crash_demo.py`)

約 20 秒跑完整個 crash-recovery 驗證，不需要任何手動操作。

```bash
cd demo
python crash_demo.py
```

**流程：**
1. Build binary
2. 建立乾淨 DB
3. 啟動 3 個專用 worker（OCR sync、NER async、Summarize sync）
4. 啟動 orchestrator（第 1 次）
5. 建立 workflow + 開始 run
6. 等 OCR（step 1）完成
7. **Kill orchestrator**（NER step 2 async 正在 in-flight）
8. 等 NER 背景執行緒完成並 cache 結果
9. 重啟 orchestrator（第 2 次）— Recovery 自動觸發
10. NER callback 重新送達 → Summarize 執行 → DONE

**和 run_demo.sh 的差別：**
- NER 使用 **async** 模式（202 + callback），展示更複雜的 recovery 情境
- Worker 有 **idempotency cache**——re-dispatch 時直接回傳 cache 結果
- 印出 `PROOF MARKERS` log，清楚標示哪些 worker log 在 crash 前後各出現幾次

**輸出範例：**
```
[OCR] 🔍 Processing document          ← 只出現一次
[NER] 🏷️  Starting entity extraction   ← 只出現一次
💥 KILLING ORCHESTRATOR
[NER] ⚡ Already processed             ← 幂等 re-dispatch（cache hit）
msg="[RECOVERY] resuming run" steps_done=1 pending_step=ner
[SUMMARIZE] ✍️  Generating summary      ← crash 後第一次執行
Run status: DONE
```

---

## 檔案結構

```
demo/
├── run_demo.sh              互動式 3 場景 demo（LLM planner）
├── crash_demo.py            自動化 crash-recovery 驗證
├── playbook/
│   ├── PLAYBOOK.zh.md       手動逐步說明（中文）
│   └── PLAYBOOK.en.md       手動逐步說明（英文）
├── planner/
│   ├── llm_adapter.py       HTTP planner：DUMMY（硬編碼）或 REAL（Claude）
│   └── echo_worker.py       最簡 echo worker，用於單獨測試 planner
├── workers/
│   ├── worker.py            通用可配置 worker（WORKER_NAME/PORT/DELAY env vars）
│   ├── ocr_worker.py        crash_demo 專用——sync，port 5001，idempotency cache
│   ├── ner_worker.py        crash_demo 專用——async，port 5002，step_id 為 key 的 cache
│   └── summarize_worker.py  crash_demo 專用——sync，port 5003，idempotency cache
├── configs/
│   ├── llm_planner.yaml     HTTP planner config（port 9000）——僅供參考
│   └── static_3step.yaml    Static 3-step config——crash_demo.py 使用
└── requirements.txt         flask, requests
```

---

## Troubleshooting

| 問題 | 解法 |
|------|------|
| Port 被佔用 | `lsof -i :5010` → 找出 PID → kill |
| Worker 啟動失敗 | `pip install flask` |
| DB 連線失敗 | `docker ps` 確認 container 在跑 |
| Build 失敗 | `go build ./cmd/stateflow/` 看完整錯誤 |
| LLM adapter 500 | 查 `/tmp/llm_adapter.log` |
