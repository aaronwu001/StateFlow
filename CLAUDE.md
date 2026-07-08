# StateFlow — Development Discipline (TRANSITIONAL — v1.0 refactor in progress)

> **⚠ REFACTOR IN PROGRESS.** The codebase is being migrated from the v0.9 five-state model to the v1.0 three-state model via numbered sessions (0–8). Until Session 7 replaces this file with the final version, this transitional file governs.

**Authoritative design:** `docs/StateFlow_Whitepaper_v1_0.md`
**Rule-by-rule spec:** `docs/StateFlow_Rules_Consolidation_v3_EN.md` (a Chinese mirror exists; English governs)
**Session instructions:** provided per-session by the owner (Master Context + Session N). The Master Context's rules override anything else, including this file.

**VOID — do not read as authority, do not imitate:** `docs/archive/DESIGN.md`, `docs/archive/StateFlow_Whitepaper_v0.8.md` (and any v0.9 whitepaper), the pre-refactor `docs/USER_MANUAL.md` (rewritten in Session 7), and any code or test that asserts the old model. If existing code contradicts the two authoritative documents, the documents win — report the conflict, do not imitate the code.

---

## What This Project Is

StateFlow is a durable execution layer for AI pipelines. It checkpoints every step, retries failures, and resumes after a crash exactly where it left off — without re-running completed work. Mechanism: a **frontier model** — persist each (decision, result) pair as it happens; on recovery, read the frontier and resume. No replay; no determinism requirement; the planner (which may be an LLM) is asked exactly once per persisted step.

---

## The v1.0 Model — Quick Reference

**States (3×3×3):**

```
run:     RUNNING | DONE | DLQ
step:    RUNNING | DONE | DLQ          (DECIDED and FAILED no longer exist)
attempt: RUNNING | DONE | FAILED(reason: worker_reported | timeout | malformed | orphaned)
```

**The two write barriers (now transactions TX1/TX2):**

```
TX1 (Barrier 1): create step+decision+first attempt+current_attempt_id  → commit → only then dispatch
TX2 (Barrier 2): attempt→DONE + step→DONE + output                     → commit → only then ask planner
```

**Combination table (run × last_step) and restart actions:**

```
RUNNING / done-or-no-steps → re-ask planner
RUNNING / running          → claim orphan (attempt→FAILED(orphaned), count++) → budget check → re-dispatch or already-DLQ'd
DONE    / done             → untouched
DLQ     / DLQ              → untouched (worker-side; replay = TX5)
DLQ     / done             → untouched (planner-side; replay = TX6)
```

**Timeout doctrine:** every attempt is timed from its creation (TX1/TX4 commit). The loop computes `deadline = attempt created_at + effective timeout` (step override > workflow default > 60s) and passes it via `context.WithDeadline` into `Dispatch`. **Timeout = failure** (reason `timeout`), consuming the budget like any other failure. Retry delay between attempts: 5s. The retry budget is the persisted `steps.attempt_count` — incremented only in TX3, reset only in TX5 (replay), touched nowhere else.

**The Atomic Transaction Ledger (every entry = exactly one DB transaction):**

```
TX-W create workflow      TX0 create run          TX1 step+attempt (Barrier 1)
TX2 success checkpoint (Barrier 2)
TX3 attempt→FAILED(reason)+count++; if count=X: same tx → step DLQ + run DLQ + DLQ record
TX4 new attempt + CAS current_attempt_id          TX5 replay worker-side (count→0, all-in-one)
TX6 replay planner-side   TX7 run→DONE            TX8 planner declared fail → DLQ
TX9 planner budget exhausted → DLQ                CAS-A: UPDATE … WHERE attempt_id=? AND status='RUNNING'
```

Full ledger with contents and rationale: whitepaper §19 ≡ rules §21. Never split, merge, or reorder a TX.

**Wire formats (binding):** sync workers receive the **bare input** as the POST body plus headers `X-StateFlow-Step-ID` / `X-StateFlow-Attempt-ID` (sync's zero-modification promise); async workers receive the `{step_id, attempt_id, input}` envelope; **every status string on the wire is UPPERCASE** ("DONE"), identical to the stored values.

**Other iron rules:** single writer (only the loop writes state; the callback handler validates + pushes to channel + returns 200, nothing more); every report lands through CAS; a success report arriving after a timeout verdict is rejected; all persisted timestamps come from DB `now()`, never `time.Now()`, never worker/planner payloads; history sent to the planner is ordered by `seq` only.

---

## During-Refactor Rules

1. Follow the current session's prompt exactly; respect its scope; stop and report instead of editing out-of-scope files.
2. Old-model tests are **expected to fail or be deleted** in their owning session (per the Session 0 audit). Never weaken new-model code to satisfy an old test.
3. Every session ends with the mandatory seven-question report defined in the Master Context.
4. The pre-release freedom applies: schema changes rewrite `migrations/001_initial.sql` in place; reset with `docker compose down -v`; no migration tooling.

---

## Running Tests

Unit tests (no DB): `go test ./...`

Postgres-backed integration tests (in `internal/store`, `internal/api`, `internal/orchestrator`) skip themselves unless `TEST_DATABASE_URL` is set:

```bash
docker compose up -d postgres
TEST_DATABASE_URL="postgres://stateflow:stateflow@localhost:5432/stateflow?sslmode=disable" \
  go test -p 1 ./...
```

Each integration test calls `resetSchema` (drop + re-apply `migrations/001_initial.sql`) — it **wipes** whatever demo/run data is in that database; don't run it while a demo run you care about is in progress. **`-p 1` is required** when running more than one package: the store/api/orchestrator packages each reset the same database's schema, and parallel package execution makes them race (symptoms: `duplicate key value violates unique constraint "pg_type_typname_nsp_index"`, `relation "steps" does not exist`). `-p 1` serializes packages. A single package alone doesn't need it.

---

## Demo Infrastructure (operational facts — still valid; scenario *semantics* are updated in Session 7)

Full stack from a clean clone:

```bash
docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build
```

- Interactive demo: `./demo/run_demo.sh` (3 scenarios, LLM planner via HTTP :9000; DUMMY mode needs no API key)
- Automated crash proof: `python demo/crash_demo.py` (static planner; OCR :5001 sync → NER :5002 async 5s delay → Summarize :5003 sync; kills/restarts the `stateflow` container mid-NER)
- `step1`/`step2` run `demo/workers/worker.py`; delays via `STEP1_DELAY`/`STEP2_DELAY` host env vars (default 1s), e.g. `STEP1_DELAY=5 docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --force-recreate step1`
- DUMMY-planner worker URLs overridable via `STEP1_URL`/`STEP2_URL` (compose sets `http://step1:5010/run` / `http://step2:5011/run`)

API paths:

```
POST /workflows                      create workflow (name, planner_type, planner_config)
POST /workflows/{workflow_id}/runs   start run (workflow_input)
GET  /runs/{run_id}                  status + steps (+ attempt summaries + dlq_reason after Session 6)
GET  /dlq                            list DLQ entries
POST /dlq/{id}/replay                replay (worker-side = TX5, planner-side = TX6, after Session 6)
POST /tasks/complete                 async worker callback
POST /tasks/fail                     async worker failure callback (optional retry_after_seconds: accepted, ignored)
```

---

## Progress Reporting Protocol

Before reporting a task complete: run the stated completion-condition verifier, confirm it passes, then deliver the seven-question report (Master Context). Never report success from "the code looks correct." If the verifier doesn't exist yet, building it is the first task.
