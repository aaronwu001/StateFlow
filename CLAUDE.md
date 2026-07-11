# StateFlow — Development Discipline

**Authoritative design:** `docs/StateFlow_Whitepaper_v1_0.md`
**Rule-by-rule spec:** `docs/StateFlow_Rules_Consolidation_v3_EN.md` (a Chinese mirror,
`StateFlow_Rules_Consolidation_v3.md`, exists for convenience; English governs on any
divergence).

**VOID — do not read as authority, do not imitate:** `docs/archive/DESIGN.md`,
`docs/archive/StateFlow_Whitepaper_v0.8.md` (and any other pre-v1.0 whitepaper), and any
code or test that still asserts the old five-state model (`DECIDED`/`FAILED` step states,
four recovery rules, `planner_failed` as a single DLQ reason, `attempt_number`,
`dispatched_at`/`decided_at`, "retry budget restarts on recovery"). If existing code or a
doc ever contradicts the whitepaper or the rules spec, those two documents win — fix or
report the conflict, do not imitate the stale code.

---

## What This Project Is

StateFlow is a durable execution layer for AI pipelines. It checkpoints every step,
retries failures, and resumes after a crash exactly where it left off — without
re-running completed work. Mechanism: a **frontier model** — persist each (decision,
result) pair as it happens; on recovery, read the frontier and resume. No replay; no
determinism requirement; the planner (which may be an LLM) is asked exactly once per
persisted step.

---

## Quick Reference — the v1.0 Model

### States (3×3×3)

```
run:     RUNNING | DONE | DLQ
step:    RUNNING | DONE | DLQ
attempt: RUNNING | DONE | FAILED (failure_reason: worker_reported | timeout | malformed | orphaned)
```

There is no DECIDED or FAILED step state, and no bare FAILED run state — they were
removed in the v1.0 refactor because neither exclusively owned a recovery rule
(whitepaper §16). All four attempt failure reasons take the identical path (TX3) and
consume the retry budget equally — the classification exists for human triage, not for
machine branching (whitepaper §4.2).

### The two write barriers (TX1 / TX2)

```
Barrier 1 (TX1): create step(decision,seq,count=0) + first attempt + current_attempt_id
                 → commit → only then dispatch. The attempt's clock starts at creation.
Barrier 2 (TX2): attempt→DONE + step→DONE + output
                 → commit → only then ask the planner for the next step.
```

Barrier 1 means a crash never loses a decision — recovery re-dispatches the persisted
decision instead of re-asking the planner. Barrier 2 means the planner's view of history
is always complete. Recovery is a **read**, never a replay engine (whitepaper §2).

### The run × last_step combination table (recovery's only branch point)

| run | last_step | Meaning | Restart action |
|---|---|---|---|
| RUNNING | DONE (or no steps) | Waiting on a planner decision | Re-ask the planner |
| RUNNING | RUNNING | Waiting on a worker, or orphaned | Claim orphan (`attempt→FAILED(orphaned)`, count++, via TX3) → budget check → re-dispatch (TX4) or already DLQ'd inside the claim |
| DONE | DONE | Cleanly finished | Untouched |
| DLQ | DLQ | Worker-side DLQ | Untouched; replay = TX5 |
| DLQ | DONE | Planner-side DLQ | Untouched; replay = TX6 |

`run=DONE with last_step≠DONE` and `run=RUNNING with last_step=DLQ` are structurally
impossible (whitepaper §8.2) — the first because `run→DONE` requires Barrier 2 to have
fired on a DONE last step; the second because step→DLQ and run→DLQ are one transaction
(TX3), so no intermediate state exists. Recovery is re-entrant (every action is
transaction-guarded) and crash loops converge (each crash's orphan claim consumes one
unit of budget, so a crash-looping orchestrator still drives every in-flight step
monotonically toward the DLQ) — whitepaper §8.3.

### The Atomic Transaction Ledger (every entry = exactly one DB transaction)

| TX | Contents |
|---|---|
| TX-W | Create workflow definition (planner type + config) |
| TX0 | Create run (RUNNING) |
| TX1 | Create step (decision, seq, count=0) + first attempt + current_attempt_id — **Barrier 1** |
| TX2 | attempt→DONE + step→DONE + write output — **Barrier 2** |
| TX3 | attempt→FAILED(reason) + count++; **if count=X, same transaction:** step→DLQ + run→DLQ + DLQ record (`worker_retry_exhausted`) |
| TX4 | New attempt + CAS-update current_attempt_id (no dual-validity instant) |
| TX5 | Replay (worker-side): count→0 + step→RUNNING + run→RUNNING + new attempt + current_attempt_id |
| TX6 | Replay (planner-side): run→RUNNING |
| TX7 | run→DONE |
| TX8 | Planner declares fail: run→DLQ + DLQ record (`planner_declared_fail`) — a legitimate answer, consumes no budget |
| TX9 | Planner budget exhausted: run→DLQ + DLQ record (reason = final attempt's category: `planner_unreachable` or `planner_malformed`) |
| CAS-A | Report validation: `UPDATE ... WHERE attempt_id=? AND status='RUNNING'` — a single atomic statement |

Full ledger with rationale: whitepaper §19 ≡ rules §21. **Never split, merge, or reorder a
TX** — every gap between two adjacent persisted writes is a crash window, and the model
is closed only when every TX's post-commit state lands in one of the five legal
combinations above.

### Timeout doctrine

Every attempt is timed from its **creation** (TX1/TX4 commit), not from dispatch — this
is what makes the "decision exists, result doesn't" span claimed by exactly one rule, no
matter how it subdivides internally. Resolution order: `step.timeout_seconds` (StepSpec)
> workflow's `default_timeout_seconds` > system default **60s**. The loop computes
`deadline = attempt created_at + effective timeout` and passes it via
`context.WithDeadline` into `Dispatch`; transports honor the incoming deadline and never
start their own clock. **Timeout = failure** (reason `timeout`), consuming the retry
budget exactly like an explicit failure — the only behavioral difference from treating it
as "uncertain" is budget accounting, and charging the budget errs in the conservative
direction (reach a human sooner, whitepaper §6). Retry delay between attempts: fixed 5s
(skipped on crash-recovery re-dispatch — the crash itself already provided cooldown).
Planner calls get a separate, fixed budget: 30s per call × 3 total attempts, not
user-configurable.

### The orphan rule

On recovery, if `run=RUNNING` and `last_step=RUNNING`, and a `RUNNING` attempt exists,
recovery pronounces it `failed(orphaned)` unconditionally via TX3 (including its DLQ
branch if count reaches X) before doing anything else. *Why:* the attempt's timer lived
in the dead process's memory — nothing else will ever pronounce this attempt. This is the
timeout philosophy applied to the orchestrator itself: "this attempt did not complete
within its lifetime — because the process carrying it died — therefore it failed"
(whitepaper §8.3).

### The retry budget

`steps.attempt_count` is the **persisted** retry budget — incremented only in TX3, reset
only in TX5 (replay), touched nowhere else. It must never be fed from a loop-local
counter (that in-memory-budget defect, which silently resets to 1 on every crash, is
exactly what the v1.0 refactor eliminated).

### The CAS rule

Every report that lands — a sync response or an async callback, success or failure —
takes effect **only if** `attempt_id == current_attempt_id` **and** that attempt is still
`RUNNING`, enforced as one atomic conditional UPDATE (CAS-A). Late, duplicate, or
superseded reports are ACKed with 200 and have zero state effect. A success report
arriving after its attempt was already pronounced `failed(timeout)` is rejected (MVP) —
accepting it would require a failed→done resurrection transition (whitepaper §10).

### The single-writer rule

Only the orchestrator loop writes step/run state. The async callback handler does exactly
three things: validate the report against CAS-A's conditions, push it into the loop's
channel, and return 200 — it never writes state itself. A second writer would race the
timeout timer for the same attempt's terminal outcome (whitepaper §10).

### Wire formats (binding)

- **Sync** workers receive the **bare `input`** as the POST body (no wrapper — sync's
  zero-modification promise) plus headers `X-StateFlow-Step-ID` / `X-StateFlow-Attempt-ID`.
- **Async** workers receive the envelope `{step_id, attempt_id, input}`.
- **Every status string on the wire is UPPERCASE** ("DONE"), identical to the stored
  values — this applies to `history[].status` sent to the planner. The planner's own
  verdict field (`continue`/`done`/`fail`) is a different field with the opposite,
  lowercase casing rule; do not conflate the two (whitepaper §12.2–§12.3, §13.1).

### Other iron rules

- All persisted timestamps come from DB `now()` at commit — never `time.Now()`, never a
  worker/planner payload.
- History sent to the planner is ordered by `seq` only — never by timestamp or step_id.
- The planner never talks to the database; it is reconstructed from the workflow's
  persisted `planner_type`/`planner_config` row on every loop iteration and on every
  recovery — never from process-global state.
- MVP concurrency invariant: one run has exactly one loop goroutine; steps execute
  strictly serially (`seq = MAX(seq)+1` is safe without locking). DAG parallelism, when it
  lands, redesigns `seq` allocation (Phase 3).

---

## Deferred / Explicitly Out of Scope for v1.0

This is the whitepaper's Temporary Design Registry (§18) — every known MVP shortcut,
consciously registered: the behavior is understood, the impact is bounded, and the remedy
is scheduled. **Do not "fix" one of these ad hoc** — each remedy interacts with other
scheduled features and lands together with them.

| # | Item | Current behavior | Impact | Remedy | Phase |
|---|---|---|---|---|---|
| 1 | Full-history transmission *(the system's flagged weakest link)* | The planner receives every step's complete output on every call | LLM context bloat; DB row bloat | Summary-plus-fetch (`GET /runs/:id`) + pass-by-reference for large payloads | fast-follow / 2 |
| 2 | Late-result salvage | A success report after a timeout verdict is discarded; the work re-runs | One redundant execution per mis-kill | Optional salvage, only after re-verifying the state machine | 2+ |
| 3 | Planner retry count in memory | Resets on crash | None material — planner calls are side-effect-free, so re-asking is always safe | Persist alongside hardening, if ever needed | — |
| 4 | Storage orphans wait for a restart | A store outage orphans in-flight runs until the next orchestrator restart | Availability gap only; zero correctness impact | In-process orphan sweeper | 2 |
| 5 | `retry_after_seconds` ignored | Accepted (optional) on `/tasks/fail`, not acted on | None; contract stability only | LLM-aware rate limiting | 2 |
| 6 | Hardcoded assembly in `main.go` | Store/transport/retry wiring fixed in code | None until alternative implementations exist | Config-driven assembly | 2 |
| 7 | Init-only migration | Schema applied by Postgres's first-boot hook; no versioning | Fine for exactly one migration file | Real migration tool at migration 002 | 2 / on demand |
| 8 | No orchestrator healthcheck | Distroless image has no shell; liveness verified externally | Compose/K8s cannot gate on readiness | `/healthz` + self-check subcommand | 1.5–2 |

---

## Development Discipline (applies to every change, not just the v1.0 migration sessions)

1. Follow the current task's instructions exactly; respect its stated scope. If a file
   outside that scope appears to need a change, stop and report it rather than editing it
   silently.
2. Never weaken new-model code or docs to satisfy an old-model test or an old-model
   expectation — old-model behavior (DECIDED/FAILED step states, four recovery rules,
   `planner_failed`, "retry budget restarts on recovery", 30s timeout defaults) is gone;
   purge it, don't accommodate it.
3. Verify before reporting: run the actual completion-condition commands and paste
   verbatim output. Never report success from "the code looks correct." If a verifier
   doesn't exist yet, building it is the first task.
4. Pre-release freedom applies while the project is unpublished: schema changes rewrite
   `migrations/001_initial.sql` in place; reset with `docker compose down -v`; no
   migration tooling (registry item 7, above).

---

## Running Tests

Unit tests (no DB): `go test ./...`

Postgres-backed integration tests (in `internal/store`, `internal/api`, `internal/orchestrator`
— verified by grepping `TEST_DATABASE_URL` across `internal/*_test.go`) skip themselves
unless `TEST_DATABASE_URL` is set:

```bash
docker compose up -d postgres
TEST_DATABASE_URL="postgres://stateflow:stateflow@localhost:5432/stateflow?sslmode=disable" \
  go test -p 1 ./...
```

Each integration test calls `resetSchema` (drop + re-apply `migrations/001_initial.sql`) — it
**wipes** whatever demo/run data is in that database; don't run it while a demo run you
care about is in progress. **`-p 1` is required** when running more than one package: the
store/api/orchestrator packages each reset the same database's schema, and parallel
package execution makes them race (symptoms: `duplicate key value violates unique
constraint "pg_type_typname_nsp_index"`, `relation "steps" does not exist`). `-p 1`
serializes packages. A single package alone doesn't need it.

---

## Demo Infrastructure

Full stack from a clean clone:

```bash
docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build
```

- Interactive demo: `./demo/run_demo.sh` (3 scenarios, LLM planner via HTTP :9000; DUMMY
  mode needs no API key) — see `demo/README.md` / `demo/playbook/PLAYBOOK.en.md`.
- Automated crash proof: `python demo/crash_demo.py` (static planner; OCR :5001 sync →
  NER :5002 async 5s delay → Summarize :5003 sync; kills/restarts the `stateflow`
  container mid-NER; asserts the orphan-claim path directly against Postgres — exactly
  one `failure_reason=orphaned` attempt, the NER step's Barrier-1 record unchanged across
  the crash, and the worker's idempotency cache absorbing the re-dispatch).
- `step1`/`step2` run `demo/workers/worker.py`; delays via `STEP1_DELAY`/`STEP2_DELAY` host
  env vars (default 1s), e.g. `STEP1_DELAY=5 docker compose -f docker-compose.yml -f
  docker-compose.demo.yml up -d --force-recreate step1`
- DUMMY-planner worker URLs overridable via `STEP1_URL`/`STEP2_URL` (compose sets
  `http://step1:5010/run` / `http://step2:5011/run`)

API paths:

```
POST /workflows                      create workflow (name, planner_type, planner_config)
POST /workflows/{workflow_id}/runs   start run (workflow_input)
GET  /runs/{run_id}                  status + steps (seq/attempt_count/created_at/current_attempt) + dlq_reason when run=DLQ
GET  /dlq                            list DLQ entries
POST /dlq/{id}/replay                replay (worker-side = TX5, planner-side = TX6)
POST /tasks/complete                 async worker callback
POST /tasks/fail                     async worker failure callback (optional retry_after_seconds: accepted, ignored — registry item 5)
GET  /healthz                        liveness (Postgres reachability); no auth, not part of the versioned business API above
```

`planner_config` (one JSONB blob on the workflow): `url` (http planner, required),
`steps` (static planner, required), plus the two workflow-level overrides available to
either planner type — `retry_limit` (X, default 3) and `default_timeout_seconds` (default
unset → system 60s). See `docs/USER_MANUAL.md` §1.6 for the full table.

---

## Progress Reporting Protocol

Before reporting a task complete: run the stated completion-condition verifier, confirm
it passes, and paste its verbatim output. Never report success from "the code looks
correct." If the verifier doesn't exist yet, building it is the first task.
