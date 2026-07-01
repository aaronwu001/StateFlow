# StateFlow Interactive Demo — Requirements & Design

**Status:** Pre-implementation spec  
**Purpose:** Define the complete interactive demo that proves StateFlow's reliability claims. This document is the authoritative reference for building the demo infrastructure.

---

## 1. Why This Demo Matters

StateFlow's value proposition is one sentence: *"crash, restart, don't re-run completed work."* The demo must make this **visually undeniable** — not through test output, but through a live, interactive sequence an interviewer can watch and understand in under 5 minutes.

The demo proves three things:
1. **Durability works** — completed steps survive any crash
2. **Recovery works** — the system resumes from exactly where it left off
3. **The design is practical** — re-running failed work is a single API call, not a manual database dig

---

## 2. Demo Architecture

### 2.1 Components

```
┌─────────────────────────────────────────────────────────┐
│  Demo Setup                                             │
│                                                         │
│  ┌──────────────┐     ┌──────────────────────────────┐  │
│  │ Orchestrator  │────▶│ Planner (static or HTTP)     │  │
│  │ (Go binary)   │     └──────────────────────────────┘  │
│  │ port 8080     │                                       │
│  └──────┬────────┘                                       │
│         │ dispatches to workers                          │
│  ┌──────┼──────────────────────────────────────────┐     │
│  │      ▼              ▼              ▼            │     │
│  │  Worker A       Worker B       Worker C         │     │
│  │  port 5010      port 5011      port 5012        │     │
│  │  "ocr"          "ner"          "summarize"      │     │
│  │  delay=1s       delay=2s       delay=3s         │     │
│  └─────────────────────────────────────────────────┘     │
│                                                         │
│  ┌──────────────┐                                       │
│  │  PostgreSQL   │  (durable state — the whole point)   │
│  └──────────────┘                                       │
└─────────────────────────────────────────────────────────┘
```

### 2.2 Worker Design — Single Configurable Script

**One Python file**, configured via environment variables. NOT three separate files.

```
ENV vars:
  WORKER_NAME   — display name for logs (e.g. "ocr", "ner", "summarize")
  WORKER_PORT   — port to listen on (5010, 5011, 5012)
  WORKER_DELAY  — simulated processing time in seconds (1, 2, 3)
```

Behavior:
- Accepts `POST /run`, logs receipt with `[WORKER:{name}]` prefix
- Sleeps for `WORKER_DELAY` seconds (simulates real work)
- Returns `{"worker": name, "echo": <input>, "processed_at": <timestamp>}`
- Each response includes a timestamp so we can PROVE it ran (or didn't re-run)

### 2.3 Static Planner Config

A 3-step linear pipeline: `ocr → ner → summarize`

```yaml
planner:
  type: static
  steps:
    - name: ocr
      worker_url: http://localhost:5010/run
      mode: sync
      timeout_seconds: 30
    - name: ner
      worker_url: http://localhost:5011/run
      mode: sync
      timeout_seconds: 30
    - name: summarize
      worker_url: http://localhost:5012/run
      mode: sync
      timeout_seconds: 30
```

### 2.4 LLM Planner Config (for agent-native demo)

Uses the existing `llm_adapter.py` in dummy mode (no API key needed):

```yaml
planner:
  type: http
  url: http://localhost:9000/decide
```

---

## 3. Demo Scenarios — In Order

### Scenario 1: Happy Path (Static Planner)

**Goal:** Prove the basic pipeline works end-to-end.

**Steps:**
1. All 3 workers running
2. Orchestrator running with static planner config
3. Submit workflow: `curl -s -X POST http://localhost:8080/workflow/start -H 'Content-Type: application/json' -d '{"workflow_input": {"document": "Hello world", "language": "en"}}'`
4. Poll status until complete: `curl -s http://localhost:8080/workflow/status/<run_id> | jq`
5. **Verify:** All 3 steps show status `DONE`, each with output containing its `processed_at` timestamp

**Evidence:** Status endpoint shows 3 DONE steps with real outputs.

---

### Scenario 2: Worker Crash & DLQ Replay (Static Planner)

**Goal:** Prove that (a) partial results survive a worker crash, and (b) re-running from the DLQ is a single API call.

**Steps:**
1. All 3 workers running
2. Kill Worker B (ner, port 5011): `kill <pid>` or just Ctrl+C its terminal
3. Submit workflow (same curl as Scenario 1)
4. **Observe:** Step 1 (ocr) succeeds. Step 2 (ner) fails with connection refused. Retries exhaust. Step goes to DLQ. Run status becomes `FAILED`.
5. **Verify partial results:**
   - `curl -s http://localhost:8080/workflow/status/<run_id> | jq` → step ocr is DONE with output, step ner is FAILED
   - `curl -s http://localhost:8080/dlq | jq` → DLQ entry exists with full context
6. Restart Worker B: start it again on port 5011
7. **Replay from DLQ:** `curl -s -X POST http://localhost:8080/dlq/<entry_id>/replay`
8. Poll status again
9. **Verify:** Run completes. All 3 steps DONE. Step ocr was NOT re-run (check timestamp — same as before). Steps ner and summarize have new timestamps.

**Evidence:** 
- ocr timestamp is unchanged (was not re-run)
- ner and summarize have new timestamps (ran after replay)
- DLQ entry is consumed

**Critical UX question this scenario answers:** "How does the user re-run a failed workflow?" → Answer: `POST /dlq/:id/replay`. One API call. No database digging.

---

### Scenario 3: Orchestrator Crash & Recovery (Static Planner)

**Goal:** The headline demo. Prove the orchestrator can die and come back without losing progress.

**Steps:**
1. All 3 workers running (Worker C with WORKER_DELAY=5 to give us time)
2. Orchestrator running
3. Submit workflow
4. **Wait for step 1 (ocr) to complete** — watch the worker logs or poll status
5. **Kill the orchestrator:** `kill -9 <orchestrator_pid>` (or Ctrl+C)
6. **Show the database state while orchestrator is dead:**
   - `psql` query showing step ocr is DONE with output, step ner is DECIDED or RUNNING
   - This proves: "the data survived the crash"
7. **Restart the orchestrator:** `./stateflow` (or `go run`)
8. **Observe:** Orchestrator logs show it found the in-progress run, resumed from step ner (or whichever step was pending)
9. **Verify:** 
   - `curl -s http://localhost:8080/workflow/status/<run_id> | jq` → All 3 steps DONE
   - Worker A (ocr) log shows it was called ONCE — no second call after restart
   - Worker B (ner) may have been called twice (once before crash if dispatched, once after) — this is the at-least-once guarantee working correctly

**Evidence:**
- Worker A log: exactly 1 invocation
- Status endpoint: all DONE
- The orchestrator startup log should say something like: "recovering run <run_id>, resuming from step <name>"

**Timing strategy for killing the orchestrator:**
- Use a demo script that polls status until ocr is DONE, then sends SIGKILL
- Or: manual demo with Worker C delay = 8s to give plenty of time

---

### Scenario 4: Happy Path (LLM Planner — Dummy Mode)

**Goal:** Prove the agent-native architecture works — the planner is external, decisions are persisted.

**Steps:**
1. All workers running (use the same echo worker on port 5010)
2. LLM adapter running in dummy mode on port 9000 (no ANTHROPIC_API_KEY)
3. Orchestrator running with HTTP planner config
4. Submit workflow
5. **Verify:** Run completes with 2 steps (the dummy adapter does 2 steps)

**Evidence:** Status shows 2 steps decided by external planner, both DONE.

---

### Scenario 5: Orchestrator Crash with LLM Planner

**Goal:** The most impressive demo. Prove that even with an external decision-maker, crash recovery works. This is the v0.7 headline: non-deterministic planner + durable execution.

**Steps:**
1. Echo worker + LLM adapter (dummy mode) running
2. Submit workflow
3. After step 1 completes, kill orchestrator
4. Restart orchestrator
5. **Verify:** 
   - The orchestrator does NOT re-ask the planner for step 1's decision (it was already persisted via Barrier 1)
   - Step 2 proceeds correctly
   - The planner adapter's log shows it was called 1 time for step 1 before crash, and 1 time for step 2 after restart (NOT re-called for step 1)

**Evidence:** Planner call count matches expectations. Decisions are durable. No replay needed.

---

### Scenario 6 (Optional): Worker Crash with LLM Planner

Same as Scenario 2 but with LLM planner. On DLQ replay:
- The DECISION for the failed step was already persisted (Barrier 1)
- On replay, the orchestrator re-dispatches that persisted decision — does NOT re-ask the planner
- This proves: planner decisions are durable even through DLQ replay

---

## 4. What Must Exist in the Codebase

Before the demo can run, these features MUST be implemented and working:

### 4.1 Must-Have (check if already implemented)

| Feature | API | Expected Behavior |
|---|---|---|
| Start a run | `POST /workflow/start` | Accepts workflow_input, returns run_id |
| Check status | `GET /workflow/status/:run_id` | Returns per-step status + outputs |
| DLQ list | `GET /dlq` | Lists all DLQ entries with full context |
| **DLQ replay** | `POST /dlq/:id/replay` | Re-queues the failed step, resumes the run. **This is the critical UX.** |
| **Crash recovery on startup** | (automatic) | On start, read `runs WHERE status IN ('RUNNING', ...)`, re-enter driver loop |
| Retry with max_retries | (internal) | Failed steps retry up to max_retries, then go to DLQ |

### 4.2 DLQ Replay — Expected Behavior (IMPORTANT)

When `POST /dlq/:id/replay` is called:
1. Look up the DLQ entry, get its `run_id` and `step_name`
2. The failed step's DECISION is already persisted in the DB (Barrier 1 fired before the original dispatch)
3. Reset the step status back to `DECIDED` (or the appropriate state for re-dispatch)
4. Reset the run status to `RUNNING`
5. Re-enter the driver loop for this run
6. The loop sees a pending decision → re-dispatches to the worker
7. If the worker now succeeds → checkpoint (Barrier 2) → ask planner for next step → continue
8. Already-completed steps upstream are NOT re-run (their results are in the frontier)

If `POST /dlq/:id/replay` is NOT implemented yet, **this is the #1 implementation priority for this session.** Without it, Scenario 2 doesn't work and the demo has a gaping UX hole ("so... how do I re-run it?" "uh, you manually query the database..." — unacceptable).

### 4.3 Recovery Log Messages

On startup recovery, the orchestrator SHOULD log clearly:
```
[RECOVERY] Found N in-progress runs
[RECOVERY] Run <run_id>: resuming from step <step_name> (M steps already completed)
```

This makes the demo visually compelling. If these log messages don't exist, add them.

---

## 5. Demo Infrastructure Files

### 5.1 File Layout

```
demo/
├── worker.py                  # Single configurable worker (ENV-driven)
├── run_demo.sh                # Interactive demo runner script
├── configs/
│   ├── static_3step.yaml      # Static planner: 3-step pipeline
│   └── http_planner.yaml      # HTTP planner: points to llm_adapter
└── README.md                  # Step-by-step instructions for each scenario
```

The existing `llm_client_demo/` directory stays as-is. The new `demo/` directory is the polished, interview-ready version.

### 5.2 Demo Runner Script (`run_demo.sh`)

An interactive shell script that:
1. Presents a menu of scenarios
2. Starts/stops the required components
3. Runs the scenario with colored output
4. Shows verification evidence
5. Cleans up

Key behaviors:
- **Colored output:** GREEN for success, RED for failure, YELLOW for actions, CYAN for info
- **Named terminals/panes not required** — the script manages background processes itself
- **PID tracking** — stores PIDs so it can kill specific processes
- **Cleanup on exit** — trap EXIT to kill all background processes
- **Database reset between scenarios** — truncate tables (or use a helper endpoint if available)

The script should support running individual scenarios:
```bash
./demo/run_demo.sh              # interactive menu
./demo/run_demo.sh scenario1    # run specific scenario
./demo/run_demo.sh all          # run all scenarios in sequence
```

### 5.3 Verification Helpers

The demo script should include helper functions:
- `wait_for_status <run_id> <expected_status> <timeout>` — polls GET /workflow/status until status matches or timeout
- `wait_for_step <run_id> <step_name> <expected_status> <timeout>` — polls until a specific step reaches expected status
- `show_status <run_id>` — pretty-prints the run status with colors
- `show_dlq` — pretty-prints DLQ entries
- `check_worker_log <log_file> <step_name>` — counts how many times a step was invoked
- `pg_show_steps <run_id>` — direct psql query showing raw step data (for orchestrator-crash scenario)

---

## 6. Success Criteria

The demo is complete when ALL of the following can be demonstrated:

- [ ] **Scenario 1:** Happy path with static planner — 3 steps, all DONE
- [ ] **Scenario 2:** Worker crash → DLQ → replay → completion, with proof that completed steps were not re-run
- [ ] **Scenario 3:** Orchestrator crash → restart → recovery, with proof that completed steps were not re-run
- [ ] **Scenario 4:** Happy path with HTTP planner (dummy mode)
- [ ] **Scenario 5:** Orchestrator crash with HTTP planner — planner decisions survive the crash
- [ ] Each scenario can be run independently (database is reset between scenarios)
- [ ] The demo script runs without manual intervention (except for killing processes in crash scenarios, which the script handles)
- [ ] Clear, colored terminal output that an interviewer can follow
- [ ] A README.md that explains what each scenario proves and how to run it

---

## 7. What This Document Does NOT Cover

- The existing crash-recovery integration test (separate concern)
- Docker/K8s deployment (Stage 1-4 of the cloud-native track)
- Performance benchmarks
- The real LLM adapter with ANTHROPIC_API_KEY (optional bonus, not a requirement)

---

## 8. Post-Demo: Document Updates Needed

After the demo is built and verified:
1. Update `CLAUDE.md` with: demo infrastructure location, how to run it, what scenarios exist
2. Update root `README.md` with a "Try the Demo" section
3. Record the demo scenarios' expected output for regression (optional: snapshot test)
4. Cross-reference this document against the whitepaper and user manual for consistency

---

*StateFlow Demo Requirements · v1.0*
