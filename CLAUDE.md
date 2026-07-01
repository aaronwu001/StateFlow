# StateFlow — Development Discipline

**Authoritative design:** `docs/StateFlow_Whitepaper_v0.8.md`
**Technical blueprint:** `DESIGN.md`

Read both before touching any code. When in doubt, the whitepaper wins.

---

## What This Project Is

StateFlow is a durable execution layer for AI pipelines. It checkpoints every step, retries failures, and resumes after a crash exactly where it left off — without re-running completed work. The core mechanism is a **frontier model**: persist each `(decision, result)` pair as it happens; on recovery, read the frontier and resume. No replay; no determinism requirement.

---

## The Two Write Barriers — Never Violate

These are the load-bearing correctness invariants of the entire system. Violating their ordering is a correctness bug, not a style issue. There are no exceptions.

### Barrier 1 — persist-decision-before-dispatch

```
store.PutDecision(run, step)   ← must commit
        ↓
transport.Dispatch(step)       ← only then
```

The planner's chosen next step is written to the DB (status `DECIDED`) **before** any worker is dispatched. If the process crashes between these two lines, recovery re-dispatches the persisted decision — the planner is not re-asked.

### Barrier 2 — persist-result-before-next-decision

```
store.Checkpoint(run, step, result)   ← must commit
        ↓
planner.Decide(state)                 ← only then
```

The worker's result is checkpointed (status `DONE`) **before** the planner is asked for the next step. If you find yourself calling `Decide` before `Checkpoint` returns, stop and fix it.

**If you are tempted to reorder these for performance or convenience, that is the signal to stop and ask for explicit sign-off.**

---

## Three Recovery Rules — The Heart of the Demo

On restart, for each unfinished run, read the frontier (`LoadFrontier`) and apply:

1. **Step `RUNNING`, `output` is null** → **re-dispatch** (generate a new `attempt_id`). The in-process channel that was awaiting the callback died with the crash. Waiting is impossible. Re-dispatch and rely on worker idempotency.
2. **Step `DECIDED`, no dispatch confirmed** → **re-dispatch the recorded `decision`** (generate a new `attempt_id` — every dispatch gets a fresh one). Do NOT call `planner.Decide`. The planner was already asked; its answer is in the DB.
3. **Step `DONE`** → **call `planner.Decide`** with the updated frontier (all DONE steps in `seq` order).

### Critical distinction: RUNNING-uncertain vs FAILED

A step that is `RUNNING`-with-no-`output` is **uncertain** — the worker may have successfully completed, but the sync response was lost to a crash. This is NOT `FAILED`. `FAILED` means the worker explicitly reported failure (`/tasks/fail` or sync non-2xx). Treating uncertain-RUNNING as FAILED would corrupt a successfully-completed step. Recovery for RUNNING-uncertain is always re-dispatch, never mark-failed.

---

## step_id / attempt_id — Never Conflate

| Field | Identifies | Lifetime | Format |
|-------|------------|----------|--------|
| `step_id` | The step within its run | Constant across all retries | `{run_id}:{step_name}` |
| `attempt_id` | One specific dispatch | New UUID every (re-)dispatch | UUID |

The callback handler validates that the incoming `attempt_id` matches `current_attempt_id` in the DB before acting. A superseded `attempt_id` is ACKed with 200 but has zero effect on run state — this is the dedup guard for at-least-once delivery.

Using `step_id` where `attempt_id` is required (or vice versa) breaks deduplication and risks double-execution. These are never interchangeable.

---

## Async Dispatch — Barrier 2 Lives in the Loop, Not the Handler

`POST /tasks/complete` callback handler does exactly three things:
1. Validate `attempt_id == current_attempt_id` (reads DB)
2. Push result into the in-process channel
3. Return 200 to the worker

It does NOT write step state. `store.Checkpoint` (Barrier 2) is called by the **orchestrator loop** after `transport.Dispatch` returns. This ordering is mandatory — two concurrent writers would race and break the barrier guarantee. The handler is a delivery mechanism; the loop is the authority.

---

## Session Discipline: One Step Per Session

Each work session addresses **exactly one clearly scoped step** with a verifiable completion condition.

**Before starting, state explicitly:**
- What you are building (one sentence)
- What "done" looks like (a runnable check or specific test output)

Do not begin the next step until the current one is verified complete. Do not implement multiple steps in one session without explicit sign-off from the project owner.

Example completion conditions:
- "Migration applies cleanly; `psql` shows all five tables matching DESIGN.md schema"
- "Interfaces and types compile: `go build ./internal/core/...` exits 0"
- "Barrier invariant test passes: insert decision, insert result, read frontier, assert barrier ordering holds"

---

## Deferred Items — Do Not Implement

§9.2 of the whitepaper lists features explicitly deferred to Phase 2 and Phase 3. Do not implement any of the following, regardless of how natural or easy they appear:

- **Ghost Mode** / soft-retry / patient retry
- **Async timeout sweeper**
- **LLM-aware rate limiting** (`retry_after` field behavior — accept the field, ignore it)
- **Full `response_mapping`** — only `output_field` is in MVP scope; `status`/`error`/`retry_after` extraction is deferred
- **Summary-plus-fetch** planner state (sending full history is the MVP behavior)
- **DAG / fan-in parallelism**
- **Replicated orchestrator** with leader election
- **MCP transport**
- **DLQ webhooks**
- **Semantic repair** of malformed LLM planner output
- **Redis activation** — provisioned but dark; Postgres carries the entire MVP

**If you find yourself starting to implement any item on this list, that is the explicit signal to stop and ask the project owner before continuing.**

The interfaces must be designed to allow these extensions later (§11 of the whitepaper), but they must not be implemented now.

---

## Progress Reporting Protocol

Before reporting a task complete:
1. Run the stated completion condition verifier (test, build, or manual check)
2. Confirm it passes
3. Report what changed, what was verified, and what the next step is

Do not report success based on "the code looks correct." Run the verifier. If the verifier does not exist yet, that is the first thing to build.

---

## Demo Infrastructure

**Interactive demo:** `./demo/run_demo.sh` (menu-driven, 3 LLM-planner scenarios)
**Automated crash proof:** `python demo/crash_demo.py` (single scenario, fully automated)

### Demo directory layout

```
demo/
├── run_demo.sh           Interactive 3-scenario menu (LLM planner mode)
├── crash_demo.py         Automated crash-recovery proof (static planner, specialized workers)
├── playbook/
│   ├── PLAYBOOK.zh.md    Manual step-by-step walkthrough (Chinese)
│   └── PLAYBOOK.en.md    Manual step-by-step walkthrough (English)
├── planner/
│   ├── llm_adapter.py    HTTP planner: REAL (Claude sonnet-4-6) or DUMMY (hardcoded 2-step)
│   └── echo_worker.py    Minimal sync echo worker (port 5010) for standalone planner testing
├── workers/
│   ├── worker.py         Generic configurable worker (WORKER_NAME/PORT/DELAY env vars)
│   ├── ocr_worker.py     crash_demo only — sync, port 5001, idempotency cache
│   ├── ner_worker.py     crash_demo only — async, port 5002, step_id-keyed cache + callback
│   └── summarize_worker.py  crash_demo only — sync, port 5003, idempotency cache
└── configs/
    ├── llm_planner.yaml  HTTP planner config (port 9000) — reference only
    └── static_3step.yaml Static 3-step config — used by crash_demo.py internally
```

### Interactive demo scenarios (run_demo.sh)

All three scenarios use LLM planner (HTTP, port 9000). DUMMY mode requires no API key.

1. **Happy Path** — planner drives 2-step pipeline (step1→:5010, step2→:5011) to completion
2. **Worker Crash & DLQ Replay** — step2 worker absent → retries → DLQ → replay → complete without re-running step1
3. **Orchestrator Crash & Recovery** — SIGKILL while step1 in-flight → restart → recovery re-dispatches (not re-decides) → planner calls ≤ 3

### Automated crash demo (crash_demo.py)

Single scenario: OCR (sync, port 5001) → NER (async, port 5002, 5s delay) → Summarize (sync, port 5003). Kills orchestrator while NER is in-flight. Proof: NER idempotency cache hit on re-dispatch; no steps re-run. Run from `demo/` directory.

### LLM planner adapter (demo/planner/llm_adapter.py)

- **DUMMY mode** (no API key): hardcoded 2-step pipeline — step1 → `:5010/run`, step2 → `:5011/run`
- **REAL mode** (`ANTHROPIC_API_KEY` set): calls Claude `claude-sonnet-4-6` with full RunState JSON; returns StepDecision

### Go change (recovery.go)
Added `[RECOVERY]` structured log messages with per-run step counts:
```
msg="[RECOVERY] found in-progress runs" count=1
msg="[RECOVERY] resuming run" run_id=... steps_done=1 pending_step=ner
```

### Actual API paths (whitepaper says /workflow/start — code differs)
```
POST /workflows                          create workflow (name, planner_type, planner_config)
POST /workflows/{workflow_id}/runs       start run (workflow_input)
GET  /runs/{run_id}                      status + steps
GET  /dlq                                list DLQ entries
POST /dlq/{id}/replay                    replay DLQ entry (resets step to DECIDED, re-enters loop)
POST /tasks/complete                     async worker callback
POST /tasks/fail                         async worker failure callback
```

---

## Quick Reference

```
Barrier 1:  PutDecision  → then Dispatch
Barrier 2:  Checkpoint   → then Decide

Recovery:
  RUNNING, no output  → re-dispatch (new attempt_id)
  DECIDED, no output  → re-dispatch (same decision, new attempt_id)
  DONE                → Decide (pass frontier)

step_id   = "{run_id}:{step_name}"  constant
attempt_id = UUID                   new every dispatch

Deferred: Ghost Mode | sweeper | rate limiting | full response_mapping |
          DAG | replicated orchestrator | MCP | DLQ webhooks | Redis
```
