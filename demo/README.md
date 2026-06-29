# StateFlow — Crash-Recovery Demo

Proves the headline promise in one automated script:

> Kill the orchestrator mid-run. Restart it.  
> Completed steps are **not** re-run. The run resumes exactly where it left off.

---

## What the demo does

A 3-step pipeline runs with a **static planner** and **mixed sync/async workers**
(Python Flask — proving language neutrality):

| Step | Worker | Mode | What it does |
|------|--------|------|-------------|
| 1 — OCR | `ocr_worker.py` :5001 | **sync** | Extracts text from a PDF (2s sleep) |
| 2 — NER | `ner_worker.py` :5002 | **async** | Identifies entities via "LLM" (5s sleep, callback) |
| 3 — Summarize | `summarize_worker.py` :5003 | **sync** | Generates executive summary (2s sleep) |

**Kill point:** the orchestrator is killed while step 2 (NER async) is mid-flight.  
Its in-process callback channel dies with the process.  
The DB still shows step 2 `RUNNING` with no output.

**On restart:** `RecoverRuns` fires before the HTTP server accepts requests,
finds the `RUNNING` run, reads the frontier, and re-dispatches step 2 with a
fresh `attempt_id`. Step 3 runs. Steps 1–2 do **not**.

---

## Prerequisites

| Requirement | How to verify |
|-------------|--------------|
| Docker running with `stateflow-pg-test` container | `docker ps` |
| Go 1.22+ | `go version` |
| Python 3.9+ | `python --version` |
| Flask + requests | `pip install -r requirements.txt` |

Start the Postgres container if it is not already running:

```bash
docker run -d --name stateflow-pg-test \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=stateflow_test \
  -p 5432:5432 \
  postgres:16-alpine
```

---

## Running the demo

```bash
cd demo
pip install -r requirements.txt   # first time only
python crash_demo.py
```

The script handles everything automatically:
1. Builds the `stateflow` binary
2. Creates a fresh `stateflow_demo` database and applies the schema
3. Starts all three workers in the background
4. Starts the orchestrator (boot 1)
5. Creates the workflow + run
6. Kills the orchestrator while NER is mid-flight
7. Waits for NER's background thread to complete (so the idempotency cache is warm)
8. Restarts the orchestrator (boot 2) — recovery fires automatically
9. Polls until the run reaches `DONE`
10. Prints proof markers

Total wall-clock time: **~20 seconds**.

---

## What to watch for

### Step 1 — OCR (sync)
```
[OCR] 🔍 Processing document: quarterly_report_2026.pdf
[OCR] ✅ Extraction complete — 3 pages, confidence 0.98
```
Appears **exactly once**. OCR is `DONE` before the crash.

### Step 2 — NER (async) dispatched
```
[NER]  🏷️  Starting entity extraction
[NER]     step_id=run-...:ner  attempt_id=<A>...
[NER]     (async, sleeping 5s to simulate LLM entity extraction)
```
Appears **once**. NER is sleeping when the orchestrator is killed.

### The crash
```
💥 KILLING ORCHESTRATOR  — pid XXXXX
💥 NER's async callback channel dies with the process
💥 DB still shows step 2 RUNNING (no output); step 3 never started
```

### NER finishes and tries to callback (orchestrator is down)
```
[NER]  ✅ Extraction done — 3 entities found
```
The callback to the OLD attempt_id may fail or be superseded — both are safe.

### Recovery fires on restart
```
INFO RecoverRuns: complete count=1
INFO recovery complete resumed=1
INFO recovery: resuming run run_id=run-...
```
`count=1` proves exactly one `RUNNING` run was found and resumed.

### Idempotency in action
```
[NER]  ⚡ Already processed step_id=run-...:ner
[NER]     Re-sending callback with NEW attempt_id=<B>... (no re-processing)
[NER]  📤 Callback delivered — attempt_id=<B>...  HTTP 200
```
Recovery re-dispatched NER with a **new** `attempt_id` (`<B>`).  
The NER worker detected it had already processed this `step_id`,  
skipped the 5s sleep, and immediately sent the callback.

### Superseded callback (dedup guard)
```
INFO callback: superseded attempt_id, ignoring  step_id=run-...:ner  attempt_id=<A>...
```
The **old** callback (attempt `<A>`) arrives at the new orchestrator but is rejected:
`<A>` ≠ `current_attempt_id` (`<B>`). The run is not corrupted.

### Step 3 — Summarize (first time ever)
```
[SUMMARIZE] ✍️  Generating summary from history: ['ocr', 'ner']
[SUMMARIZE] ✅ Summary ready — N words
```
Runs **once**, **after** recovery. Receives the full history (OCR + NER outputs).

### Final state
```
  Run status : DONE
  Steps:
    [DONE  ] ocr
    [DONE  ] ner
    [DONE  ] summarize
```

---

## Proof checklist

| Claim | Evidence in the log |
|-------|---------------------|
| Step 1 NOT re-run | `[OCR] 🔍` appears **once** (before crash); absent after restart |
| Step 2 NOT re-processed | `[NER] 🏷️` appears **once**; after restart `[NER] ⚡` (cache hit) instead |
| Recovery picked up mid-run | `RecoverRuns: complete count=1` and `recovery: resuming run` |
| RUNNING-uncertain treated correctly | NER re-dispatched (not marked FAILED) |
| Dedup guard works | `callback: superseded attempt_id, ignoring` for old attempt |
| Step 3 runs exactly once | `[SUMMARIZE] ✍️` appears **once**, after recovery |
| Run reaches DONE | Final status `DONE`, all 3 steps `DONE` |

---

## Architecture notes

**Why recovery is trivial (whitepaper §2.1):**  
StateFlow uses the *frontier model*: every `(decision, result)` pair is persisted
before any side effect. On crash, reading the frontier is enough — nothing is
replayed. The two write barriers enforce this:

1. **Barrier 1** — `PutDecision` before `Dispatch`: the planner's choice is in
   the DB before any worker is called.
2. **Barrier 2** — `Checkpoint` before next `Decide`: the worker's result is in
   the DB before the planner is asked again.

Because of Barrier 1, a `DECIDED` step is never re-asked of the planner on recovery.  
Because of Barrier 2, a `DONE` step is never re-dispatched.  
A `RUNNING` step (crash mid-flight) is re-dispatched with a new `attempt_id` and
relies on worker idempotency — exactly what this demo shows.

**Worker language neutrality:**  
The workers are Python Flask. StateFlow speaks plain HTTP. Any language works.

**step_id vs attempt_id:**  
`step_id` (`run_id:step_name`) is constant across retries — the NER worker uses
it as its idempotency key. `attempt_id` is a new UUID every dispatch — the dedup
guard uses it to reject stale callbacks.
