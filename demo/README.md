# StateFlow Demo

Two ways to run the demo:

| Script | Mode | Best for |
|--------|------|----------|
| [`run_demo.sh`](#interactive-demo-run_demosh) | Interactive, menu-driven | Live presentations, explaining each step |
| [`crash_demo.py`](#automated-demo-crash_demopy) | Fully automated | Quick verification, recording |

Both use an **LLM / HTTP Planner** — the planner runs as a separate HTTP service. No API key needed (DUMMY mode); set `ANTHROPIC_API_KEY` to use real Claude.

> **Chinese version:** [README.zh.md](README.zh.md)

---

## Prerequisites

```bash
# Docker Postgres
docker run -d --name stateflow-pg-test \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=stateflow_test \
  -p 5432:5432 \
  postgres:16-alpine

# Python packages
pip install flask requests

# Go 1.22+ (for building the binary)
go version
```

---

## Interactive Demo (`run_demo.sh`)

Menu-driven, pauses at each key moment so you can explain what's happening.

```bash
# From project root
./demo/run_demo.sh
```

The script builds the binary, creates a fresh DB, and shows:

```
   StateFlow Interactive Demo  (LLM Planner)
   ══════════════════════════════════════════
   1) Happy Path
   2) Worker Crash & DLQ Replay
   3) Orchestrator Crash & Recovery
   A) Run All Scenarios
   Q) Quit
```

### The Three Scenarios

**Scenario 1 — Happy Path**
- LLM adapter decides step1 → step2 → done
- Shows: planner called exactly 3 times

**Scenario 2 — Worker Crash & DLQ Replay**
- step2 worker is intentionally absent → 3 retries → DLQ
- Restart step2 worker, POST /dlq/{id}/replay → run completes
- Proves: step1 is not re-run (called once only)

**Scenario 3 — Orchestrator Crash & Recovery** ⭐ the headline demo
- step1 has a 5s delay to create a kill window
- SIGKILL orchestrator while step1 is in-flight
- Restart orchestrator → recovery fires → step1 re-dispatched (NOT re-decided)
- Proves: total planner calls ≤ 3, even with a crash

For manual step-by-step instructions, see the playbook:
- [playbook/PLAYBOOK.en.md](playbook/PLAYBOOK.en.md)
- [playbook/PLAYBOOK.zh.md](playbook/PLAYBOOK.zh.md)

---

## Automated Demo (`crash_demo.py`)

Runs the full crash-recovery proof in ~20 seconds, no interaction needed.

```bash
cd demo
python crash_demo.py
```

**Flow:**
1. Build binary
2. Create fresh DB
3. Start 3 specialized workers (OCR sync, NER async, Summarize sync)
4. Start orchestrator (boot 1)
5. Create workflow + start run
6. Wait for OCR (step 1) to complete
7. **Kill orchestrator** while NER (step 2, async) is mid-flight
8. Wait for NER's background thread to cache its result
9. Restart orchestrator (boot 2) — recovery fires
10. NER callback re-delivered → Summarize runs → DONE

**What makes this different from run_demo.sh:**
- NER uses **async** mode (202 + callback), demonstrating more complex recovery
- Workers have **idempotency caches** — on re-dispatch, the cached result is returned immediately
- Prints a `PROOF MARKERS` log showing exactly which worker logs appeared before vs. after the crash

**Sample output:**
```
[OCR] 🔍 Processing document          ← appears once only
[NER] 🏷️  Starting entity extraction   ← appears once only
💥 KILLING ORCHESTRATOR
[NER] ⚡ Already processed             ← idempotent re-dispatch (cache hit)
msg="[RECOVERY] resuming run" steps_done=1 pending_step=ner
[SUMMARIZE] ✍️  Generating summary      ← first time after crash
Run status: DONE
```

---

## Directory Layout

```
demo/
├── run_demo.sh              Interactive 3-scenario demo (LLM planner)
├── crash_demo.py            Automated crash-recovery proof
├── playbook/
│   ├── PLAYBOOK.en.md       Manual step-by-step walkthrough (English)
│   └── PLAYBOOK.zh.md       Manual step-by-step walkthrough (Chinese)
├── planner/
│   ├── llm_adapter.py       HTTP planner: DUMMY (hardcoded) or REAL (Claude)
│   └── echo_worker.py       Minimal echo worker for standalone planner testing
├── workers/
│   ├── worker.py            Generic configurable worker (WORKER_NAME/PORT/DELAY)
│   ├── ocr_worker.py        crash_demo only — sync, port 5001, idempotency cache
│   ├── ner_worker.py        crash_demo only — async, port 5002, step_id-keyed cache
│   └── summarize_worker.py  crash_demo only — sync, port 5003, idempotency cache
├── configs/
│   ├── llm_planner.yaml     HTTP planner config (port 9000) — reference only
│   └── static_3step.yaml    Static 3-step config — used by crash_demo.py
└── requirements.txt         flask, requests
```

---

## Troubleshooting

| Problem | Fix |
|---------|-----|
| Port already in use | `lsof -i :5010` → find PID → kill it |
| Worker won't start | `pip install flask` |
| DB connection failed | `docker ps` — confirm container is running |
| Build failed | `go build ./cmd/stateflow/` for full error |
| LLM adapter 500 | Check `/tmp/llm_adapter.log` |
