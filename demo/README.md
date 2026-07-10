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

Docker and Docker Compose v2 (`docker compose version`). That's it — Postgres,
StateFlow, the workers, and the LLM planner adapter all run as compose
services; there's no local Go toolchain or `pip install` required.

```bash
# From the project root — brings up Postgres + StateFlow + all demo
# workers/adapter on one network.
docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build
```

---

## Interactive Demo (`run_demo.sh`)

Menu-driven, pauses at each key moment so you can explain what's happening.

```bash
# From project root
./demo/run_demo.sh
```

The script builds the compose images, resets the DB, and shows:

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
Requires the compose stack to already be up (see Prerequisites above).

```bash
python demo/crash_demo.py
```

**Flow:**
1. Build compose images (stateflow + workers)
2. Reset DB (TRUNCATE, schema already applied by the compose Postgres init)
3. Start 3 specialized worker containers (OCR sync, NER async, Summarize sync)
4. Start orchestrator (boot 1)
5. Create workflow + start run
6. Wait for OCR (step 1) to complete
7. **Kill orchestrator** (`docker compose kill stateflow`) while NER (step 2, async) is mid-flight
8. Wait for NER's background thread to cache its result
9. Restart orchestrator (boot 2) — recovery fires
10. NER callback re-delivered → Summarize runs → DONE

**What makes this different from run_demo.sh:**
- NER uses **async** mode (202 + callback), demonstrating more complex recovery
- Workers have **idempotency caches** — on re-dispatch, the cached result is returned immediately
- Prints a `PROOF MARKERS` log showing exactly which worker logs appeared before vs. after the crash
- After recovery, the script queries Postgres directly and asserts: the NER step
  has exactly one attempt with `failure_reason='orphaned'` (recovery's orphan-claim,
  whitepaper §8.3); the NER step's `created_at`/`decision` are byte-identical
  before and after the crash (the planner was asked for that step exactly once);
  and NER's worker logs show its actual extraction ran exactly once (the
  idempotency cache absorbed the re-dispatch)

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
├── Dockerfile               Shared Python image for workers + LLM adapter
├── .dockerignore
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
│   └── static_3step.yaml    Static 3-step config — reference only
└── requirements.txt         flask, requests, anthropic

../docker-compose.yml         Base stack: postgres + stateflow
../docker-compose.demo.yml    Overlay: step1, step2, llm-adapter, ocr/ner/summarize workers
```

---

## Troubleshooting

| Problem | Fix |
|---------|-----|
| Port already in use | `docker ps` → find what's bound to the port → stop it |
| `docker compose` not found | Install/upgrade to Docker Compose v2 (`docker compose version`) |
| DB connection failed | `docker compose ps postgres` — confirm it's healthy |
| Build failed | `docker compose -f docker-compose.yml -f docker-compose.demo.yml build` for full error |
| LLM adapter 500 / no logs | `docker compose -f docker-compose.yml -f docker-compose.demo.yml logs llm-adapter` |
