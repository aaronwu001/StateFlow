# StateFlow State-Machine Refactor — Rules Consolidation v3 (Final)

**Purpose:** The authoritative pre-implementation specification for Whitepaper v1.0 and the codebase refactor. No whitepaper statement or code change may contradict this document.
**Notation:** 【TXn】marks an atomic-transaction requirement inline; the ledger at the end is the acceptance table — the two must agree. Every non-obvious rule carries a "**Why**".
**Language note:** This is the only edition of this document — no Chinese version exists (an earlier draft of this note referred to one, but it was never actually added to the repo).

---

## 1. Terminology and the Four System Components

| Component | Responsibility |
|---|---|
| **Orchestrator** | Drives the step loop; the **only writer** of state |
| **Storage (DB)** | The single source of truth; Postgres in the MVP, interface-abstracted (§18) |
| **Planner** | A side-effect-free external decision endpoint answering "what's next" |
| **Worker** | The external HTTP endpoint that performs actual work |

**The word "restart" refers exclusively to an orchestrator process restart — nothing else.** A planner or worker dying manifests to the system only as "a response did or did not arrive in time"; storage failure is a whole-system halt (§12).

Identity discipline: `step_id` (`{run_id}:{step_name}`, constant across retries) and `attempt_id` (UUID, new on every dispatch) are never conflated.

**Clock rule: every timestamp is assigned by the system side — the DB's `now()` at transaction commit. Timestamps are never taken from worker/planner payloads.** **Why:** a single Postgres is a single authoritative clock; process clocks drift across restarts and hosts, and externally reported times are untrusted.

---

## 2. Stored vs. Derived — the Field-Maintenance Policy

**Principle: derive what is a pure function of immutable data; store what is a judgment made at a moment in time under then-current configuration.**

| Field | Policy | Why |
|---|---|---|
| `attempts.status` | Stored | The event truth itself |
| `steps.status` | Stored | DLQ is a **judgment** against the then-current retry limit X, and X is mutable config — deriving it would flip historical DLQ verdicts when X changes |
| `runs.status` | Stored | Recovery's scan `WHERE status='running'` must be index-speed |
| `steps.output` | Stored (physical signal) | null/non-null is the ground truth of completion; written in the same TX as status=done【TX2】so the two cannot diverge; if they ever do, **output wins** (acceptance clause) |
| `steps.attempt_count` | Stored | The budget is explicit single-writer state; incremented only in【TX3】, reset only in【TX5】 |
| last_step | Derived (index lookup) | §16 |
| last_attempt | Derived (pointer) | `steps.current_attempt_id`, maintained by【TX1/TX4/TX5】 |
| Run failure sub-classification | Derived + DLQ reason | §14; a done run is never labeled |

---

## 3. State Definitions (the Single Truth List)

**Run — three states:** running (in progress, incl. orphans awaiting reclaim) / done (clean terminal; never touched again) / DLQ (awaiting a human; the only exit is replay).

**Step — three states:** running (decision persisted, completion unconfirmed — subsumes the old DECIDED/RUNNING/FAILED) / done (output persisted) / DLQ (count reached X at judgment time).

**Attempt — three states; failed carries a mandatory four-valued reason:**

| reason | Meaning |
|---|---|
| `worker_reported` | The worker explicitly reported failure (sync non-2xx, or the async fail callback) |
| `timeout` | The attempt exceeded its timeout with no valid report |
| `malformed` | The worker "succeeded" but the content is unusable: sync 2xx with a body that is not valid JSON, or a declared output_field that is absent; or an async callback with valid ids but unparseable output. **Why:** a success whose output cannot be extracted cannot feed the next step — it is a real failure, and must be distinguishable from "no response" for debugging |
| `orphaned` | Claimed by recovery on orchestrator restart — the process death killed the timer and the waiter, leaving a running attempt in the DB that this rule pronounces |

All four failure reasons take **exactly the same subsequent path**【TX3】and consume the budget equally. **Why:** the classification is for humans (DLQ context, triage), not for the machine to branch on — fewer branches, more verifiable state machine.

**Attempts columns (attempt_number removed):** `attempt_id` (UUID), `step_id`, `status`, `failure_reason`, `error`, `created_at`, `resolved_at`. `created_at` is renamed from `dispatched_at` — the row is inserted at TX1/TX4 commit, *before* the actual dispatch, and is the timeout anchor. **Ordering = `created_at` (system-side clock)**; attempts of one step are created serially by a single loop goroutine, so timestamp ordering is always valid.

**Other key step columns:** `decision` (JSONB, the planner's full StepSpec — recovery re-dispatches from here, guaranteeing the planner is asked exactly once per step), `seq` (the sole ordering source for history), `current_attempt_id` (the CAS comparison target).

---

## 4. Workflow and Run Creation (Formal)

1. **Create workflow** (`POST /workflows`): the user submits the definition — planner config (type = static or http, plus content), timeout defaults, per-step retry limit X. The orchestrator generates `workflow_id` and persists the definition【TX-W: single write】. Definitions are reusable across many runs.
2. **Start a run** (`POST /workflows/{workflow_id}/runs`): the user submits `workflow_input`. Create the run (status=running)【TX0: single write】, return `run_id`, start one step-loop goroutine for this run.
3. **MVP concurrency invariant: one run has exactly one loop goroutine; steps execute strictly serially.** This makes `seq = MAX(seq)+1` lock-free safe; DAG parallelism (deferred) will require a seq redesign.

---

## 5. The Step Loop (Formal)

Each iteration, in order (failure handling in §7–§8):

1. **Read the frontier**: all done steps' outputs of this run, ordered by seq ascending.
2. **Ask the planner**: send the RunState (`run_id` + `workflow_input` + the seq-ordered full history). The planner answers one of three:
   - **continue** — with a full StepSpec (worker URL, input data, mode, timeout)
   - **done** — the run is complete
   - **fail** — the planner semantically declares this run unworkable (e.g., unprocessable input)
3. **continue** → in one transaction: create the step (status=running, seq, attempt_count=0, decision=StepSpec) + create the first attempt (status=running) + set current_attempt_id【TX1 | **Barrier 1: dispatch only after commit**】. **The attempt's clock starts at this moment of creation.**
4. **Dispatch the worker** (delivery format is transport-specific, §9: sync = bare input + ID headers; async = JSON envelope).
5. **Await the result**: sync = hold the HTTP connection; async = select(callback channel, timeout timer). One blocking call either way, from the loop's perspective.
6. **Success passing CAS** → in one transaction: attempt→done + step→done + write output【TX2 | **Barrier 2: return to step 1 only after commit**】.
7. **Failure (one of four sources)** →【TX3】; below X → wait the retry delay →【TX4】create a new attempt → back to step 4.
8. Planner says **done** → run→done【TX7】. Planner says **fail** → run→DLQ + DLQ record (planner_declared_fail)【TX8】.

**Why (the barriers' reason for existing — the whitepaper must restate this):** Barrier 1 makes a crash re-dispatch the persisted decision instead of re-asking the planner — the foundation on which a non-deterministic LLM planner can safely exist (the root of versioning immunity). Barrier 2 makes the planner's view of history always complete — no finished work is ever lost before the next decision.

---

## 6. The Unified Timeout Rules and Taxonomy

**Philosophy: the responsibility of estimating a worker task's duration is returned to the user — we are a reliability layer, not a workload-prediction layer.**

**Two knobs — do not confuse them:**

| Knob | Value | Meaning |
|---|---|---|
| **timeout** | default 60 s; workflow-level override; step-level (StepSpec) override | One attempt's lifetime ceiling (persist → dispatch → execute → report received, end to end) |
| **retry delay** | fixed 5 s (MVP) | The cooldown between an attempt being pronounced failed and the next attempt being created. **Why:** don't hammer a struggling worker instantly; makes retries observable in a demo |

**Timeout variant taxonomy (the whitepaper must present it in full):**

| Variant | Detection mechanism | Semantics |
|---|---|---|
| Sync worker overruns | HTTP client deadline | attempt→failed(timeout)【TX3】 |
| Async worker goes silent | select(channel, timer) | same — never a bare channel wait |
| Stuck before dispatch (persisted, not yet sent) | the same timer (anchor = attempt creation) | same. **Why:** the entire "decision exists, no result" span is claimed by exactly one rule |
| Planner timeout / connection failure | HTTP client deadline | consumes the planner budget (§8), not an attempt |

Rules: **timeout = failure**, taking exactly the same path as any other failure. **Why:** the old "uncertain ≠ failed" doctrine guarded against a misjudgment destroying completed work, but old and new respond to silence with the identical action (re-dispatch + idempotency + CAS blocking stale reports); the only difference is budget accounting, whose worst case is reaching the DLQ (a human) earlier — an error in the conservative direction, acceptable. The default timeout must be finite (infinite = a dead worker is undetectable); mis-killing a long task is a user-side configuration consequence (§17). The planner's retry count lives in memory and resets on crash — acceptable because planner calls are side-effect-free (registered, §19).

---

## 7. Worker Failure Classification and Retry

1. The four sources (`worker_reported` / `timeout` / `malformed` / `orphaned`) converge on one path:
2. 【TX3】attempt→failed(reason) + attempt_count++; **if count reaches X: in the same transaction** step→DLQ + run→DLQ + insert the DLQ record (reason=`worker_retry_exhausted`, context carrying each attempt's reason and error detail). **Below X:** the transaction ends; wait the retry delay, then TX4. **Why (one blade):** if the Xth failure and the DLQ verdict were separate writes, a crash between them would dispatch one extra attempt that should have gone to the DLQ.
3. 【TX4】create the new attempt + CAS-update current_attempt_id → dispatch. **The handover must be atomic**: no instant exists at which two attempt_ids are both valid.
4. A crash between TX3 and TX4 leaves the DB showing step=running, last_attempt=failed, count<X → recovery's budget check takes over correctly (§11); the window is claimed.
5. **Malformed edge cases:** an async callback missing valid step_id/attempt_id → 400, zero state effect (the attempt's own timeout will claim it); valid ids but unparseable output → the malformed failure path.

---

## 8. Planner: Two Classes, Failure Classification, and the Budget

**Two classes (chosen at workflow-config time, persisted in the workflow definition):**

| Class | Who runs it | The user provides |
|---|---|---|
| **static** | StateFlow built-in (zero deployment) | A YAML step list (inside planner_config) |
| **http** | The user | Any HTTP endpoint satisfying the contract; for LLM cases we ship a prompt template and the exact format spec |

**The loop and recovery reconstruct the planner instance from the DB's workflow definition every time → an orchestrator restart recovers the planner automatically, and static vs. http are indistinguishable to the loop.**

**MCP stance: the planner does not speak MCP.** MCP standardizes "calling a tool"; a planner is a decision endpoint, not a tool — forcing it would import protocol weight and contradict the zero-SDK positioning. MCP's correct place is the worker transport (Phase 3).

**Failure classification:**

| Situation | Class | Handling |
|---|---|---|
| Answers **fail** | **A legitimate answer, not a failure** (semantic verdict) | Direct【TX8】run→DLQ, reason=`planner_declared_fail`; no budget consumed |
| Timeout / connection failure | unreachable | Consumes the planner budget |
| Answered but format-invalid (not JSON, missing status, continue without worker_url/mode, prose around the JSON) | malformed | Consumes the planner budget |

Budget: 30 s × 3 total attempts, `unreachable` and `malformed` drawing from the same pool. On exhaustion →【TX9】run→DLQ, **reason = the final attempt's failure class** (`planner_unreachable` or `planner_malformed`), with every attempt's detail in context. **Why:** mixed failures need a deterministic single value; the final attempt best represents what the operator must fix now, and the full history survives in context.

Format acceptance criteria (the malformed decision rules) carry over from the original design §5.5: well-formed JSON; contains status; on continue contains step.worker_url and step.mode; nothing but the JSON.

---

## 9. Data Contracts (the I/O Formats Users Must Read)

**Orchestrator → Planner (RunState):**
```json
{ "run_id": "...", "workflow_input": {...},
  "history": [ { "name": "...", "status": "DONE", "output": {...} } ] }
```
History is seq-ascending; per-entry and total wire size are now bounded (see below) — this is no longer a claim that every entry's real output is always sent in full. **Wire-casing rule: every status string on the wire is UPPERCASE ("DONE"), identical to the stored values.** **The MVP sends the full history — registered as the system's weakest link (§19):** LLM context bloat and DB row bloat are real risks; the committed slimming paths (summary-plus-fetch, pass-by-reference) are scheduled fast-follows. Users must currently design planners on the premise "you will receive the full history." **Phase 2 registry #1 mitigation (mechanical, not a protocol change):** any single entry's `output` over 2KB marshaled is replaced with a pointer object (`_truncated`/`size_bytes`/a `GET /runs/:run_id` note); the whole `history` array is further capped at 50KB cumulative, allocated most-recent-first — entries that don't fit drop `output` entirely (`name`+`status` only). Nothing changes about what Postgres persists or what `GET /runs/:run_id` returns; only what is marshaled per `Decide` call.

**Planner → Orchestrator (StepDecision):** `{ "status": "continue|done|fail", "step": { name, worker_url, mode, timeout_seconds, input, output_field? } }`. The format acceptance criteria are the malformed decision rules.

**Orchestrator → Worker (dispatch — transport-specific):**
- **Sync:** the POST body is the **bare `input`** (exactly what the planner decided — no wrapper), with the identifiers carried as HTTP headers `X-StateFlow-Step-ID` and `X-StateFlow-Attempt-ID`. **Why:** sync's promise is zero worker modification — an unmodifiable external API must be able to consume the call as-is; such APIs ignore unknown headers, while modifiable sync workers can read them to key idempotency on step_id.
- **Async:** the POST body is the envelope `{ "step_id": "...", "attempt_id": "...", "input": {...} }` — the worker must echo the ids in its callback, so they must be in-band.

Parsing the `input` content is the user's responsibility (the planner decides it).

**Worker → Orchestrator:** sync = 2xx + JSON body (output = whole body, or the output_field subtree); async = 202 then callback `/tasks/complete` (step_id, attempt_id, output) or `/tasks/fail` (step_id, attempt_id, error, **optional** `retry_after_seconds` — accepted but ignored in the MVP; a stability reservation for rate limiting; one whitepaper sentence suffices).

---

## 10. Derived Invariants and the Combination Table

Invariants (each upheld by a transaction boundary): (1) no steps ⇒ treated as last_step=done; (2) last_attempt=done ⇒ step=done【TX2 one blade】; (3) count=X ⇒ step=DLQ ∧ run=DLQ【TX3 one blade】; (4) last_attempt=running ⇒ step=running; (5) run=done ⇒ last_step=done (Barrier 2); (6) last_step=DLQ ⇒ run=DLQ【TX3】.

**The five legal combinations:**

| Combination | Meaning | Action on orchestrator restart |
|---|---|---|
| run=running, last_step=done (or no steps) | Waiting on a planner decision | Re-ask the planner |
| run=running, last_step=running | Waiting on a worker (or orphaned) | Recovery three-step (§11) |
| run=done, last_step=done | Cleanly finished | Untouched |
| run=DLQ, last_step=DLQ | Worker-side DLQ; replay after fixing the worker | Untouched |
| run=DLQ, last_step=done | Planner-side DLQ (three reasons, §14) | Untouched |

**The impossible combinations, and why (adversarial-review material):**

| Combination | Why it cannot exist |
|---|---|
| run=done, last_step=running or DLQ | run=done is produced only by the planner saying done, whose precondition is last_step=done (Barrier 2) |
| run=running, last_step=DLQ | step→DLQ and run→DLQ are one transaction【TX3】; no intermediate state |

---

## 11. Recovery (an Orchestrator Restart, Exclusively)

Scan all run=running (index-speed), and act by the combination table: last_step=done (or no steps) → re-ask the planner; last_step=running → **(a) claim the orphan** (running attempt → failed(orphaned) + count++, via【TX3】incl. its DLQ branch) **→ (b) budget check** (at X: already DLQ'd inside a) **→ (c)**【TX4】new attempt → dispatch. done and DLQ runs: never scanned, never touched.

Properties (whitepaper selling points): **re-entrant** (every step is transaction-guarded; re-running recovery re-derives from current state; a claimed orphan is failed, not running, so it cannot be double-claimed or double-counted); **crash loops converge** (each crash consumes one unit of budget → guaranteed convergence to the DLQ and a human; unbounded retry is structurally impossible).

---

## 12. Four-Component Failure Overview (the whitepaper must open its failure story with this table)

| Component | Down / disconnected / flaky | Handling | Section |
|---|---|---|---|
| Worker | Any form of no-response, disconnect, slowness, malformed output, explicit failure | timeout or the matching reason → budget → DLQ | §6, §7 |
| Planner | Same, plus a deliberate fail verdict | planner budget → DLQ, three reasons | §8 |
| Orchestrator | crash / restart | recovery | §11 |
| Storage | down / unreachable | **whole-system halt**: the API refuses new requests; each in-flight run's goroutine dies on its first failed write → the run orphans; after storage returns + orchestrator restart, recovery reclaims it through exactly the crash path | this section |

**The explicit storage assumption (MVP):** storage availability = system availability; **correctness is unaffected** — all writes are atomic transactions, persisted state is complete and consistent at every instant, zero data loss. This is deliberate: the SPOF moves from fragile pipeline code into a mature, replicable, well-backed-up database. **A standby/secondary DB is explicitly rejected** (dual writes = two sources of truth). Phase 2 proposal (only if it stays simple): an in-process sweeper reclaiming orphaned runs without a restart — an availability optimization, never a correctness need.

---

## 13. Report Handling (CAS and the Single Writer)

(1) A report (success/failure, sync response/async callback) takes effect only if "attempt_id == current_attempt_id AND that attempt is still running", implemented as one conditional UPDATE【CAS-A】. (2) Late / duplicate / superseded reports → ACK 200, zero state effect. (3) **A success report arriving after the timeout verdict is rejected (MVP)** — it would create a failed→done resurrection transition; "late-result salvage" is registered as a future optimization. (4) **Single writer:** the callback handler only validates and hands off (push to channel); state writes are always the loop's — a second writer would race the timeout timer for the attempt's terminal outcome.

---

## 14. DLQ Reasons and Human Triage

`dead_letter_queue.reason`, four values, **purely informational** — replay mechanics are identical; only the human triage differs:

| reason | Combination | Operator action |
|---|---|---|
| `worker_retry_exhausted` | run=DLQ, last_step=DLQ | Read the per-attempt reasons in context; fix the worker or the timeout → replay |
| `planner_unreachable` | run=DLQ, last_step=done | Fix planner connectivity → replay |
| `planner_malformed` | run=DLQ, last_step=done | Fix the planner's output (LLM case: off-template prompt) → replay |
| `planner_declared_fail` | run=DLQ, last_step=done | **Change the input data or the planner logic first**, then replay — otherwise the same verdict will likely repeat and burn cost |

**Explicitly not done:** a done run is never labeled; distinctions are always derived (combination table + reason join), never stored as new states.

---

## 15. DLQ Replay (the attempt_count Design; replay_round Rejected)

- run=DLQ, last_step=DLQ →【TX5】attempt_count **reset to 0** + step→running + run→running + new attempt + set current_attempt_id (all one transaction) → dispatch the worker.
- run=DLQ, last_step=done →【TX6】run→running → re-ask the planner (the three planner-side reasons share this identical replay action).
- **The reset is a mandatory clause**: without it the count already equals X at replay time and the step bounces straight back to DLQ — the button would be decorative.
- Which replay round an attempt belonged to is not stored; reconstruct from attempt timestamps + DLQ records when needed. **Why:** replay_round maintains one extra concept for an audit trail that is reconstructible after the fact — the MVP doesn't pay for it.

---

## 16. Efficient Lookups

- **last_step**: composite index `(run_id, seq)` + `ORDER BY seq DESC LIMIT 1` — an index seek, not a scan; a separate pointer table is rejected (two-table consistency cost > one index lookup).
- **last_attempt**: no query — `current_attempt_id` is it.
- **Recovery scan**: index on `runs.status`.
- **Attempt ordering**: `created_at` (system-side clock, §1).

---

## 17. User Requirements (the Quantified Operator's Contract)

Written as consequences and responsibility boundaries, not abstract demands:

1. **Concurrent idempotency (quantified):** one step produces at most X attempts per round (between resets); worst case (every failure is a timeout mis-kill while the previous execution still lives) = **up to X concurrent duplicate invocations of the same step_id running simultaneously**. The user's deduplication (recommended: a lock or upsert keyed on step_id) must withstand that concurrency. **If the configured X exceeds what the worker's deduplication can withstand, the resulting duplicate side effects and data corruption are the user's responsibility, not StateFlow's.** StateFlow's side: stale reports are blocked by CAS; the DB is never polluted by a superseded execution.
2. **Timeout estimation is the user's judgment:** too short mis-kills long tasks and double-bills; too long delays detection — StateFlow does not predict workloads.
3. **Data-format expectations:** workers must parse the §9 dispatch contract; planners must speak the RunState/StepDecision contracts (LLM: use our template verbatim).
4. Sync through an LB/proxy may be cut at 30–90 s → long tasks go async.
5. No built-in authentication → run in a trusted network; gateway/mesh in production.
6. The DLQ is a human queue; triage per §14 — especially: do not blind-replay `planner_declared_fail`.

---

## 18. Storage Abstraction (Extension Point)

The `StateStore` interface's method set is **derived directly from the transaction ledger** (one method per TX0–TX9, or parameterized merges, plus the read side: LoadFrontier — whose pending information subsumes last-step lookup — ListRunningRuns, GetRun, DLQ queries). **Contract clause: every TXn method must be implemented as a single transaction** — interface semantics, not an implementation suggestion; alternative backends (MySQL/SQLite/cloud KV) must provide equivalent atomicity. MVP reference implementation: Postgres. Final method naming is mapped at implementation time (Claude Code).

---

## 19. Registry Impact (the Complement of the Promise List)

| Item | Status |
|---|---|
| Old item 1 (in-memory retry counting), old item 4 (no async timeout) | **Deleted** — solved by the new model |
| Ghost Mode / sweeper | Shrunk to "reclaim storage orphans without restart" (Phase 2) |
| **Full-history transmission (new; flagged as the weakest link)** | context/row bloat; remedy = summary-plus-fetch + pass-by-reference (fast-follow) |
| Late-result salvage | Post-timeout success reports are discarded; optional future salvage |
| Planner retry count in memory | Resets on crash; safe because side-effect-free |
| Concurrent idempotency contract | User-responsibility expansion, quantified (§17); documentation must be prominent |
| retry_after_seconds | Optional, accepted-but-ignored; rate-limiting reservation |
| Old items 2 (hardcoded assembly), 5 (init-only migration), 6 (no healthcheck) | Unchanged, carried over |

---

## 20. Whitepaper Structure Conventions

English is the authoritative edition (the Chinese edition is a mirror translation; English governs on divergence). Theme-bound chapters; **historical narrative (the dead-zone lesson, the v0.9→v1.0 rationale) collected into one self-contained removable section whose deletion affects no other reference**; the closing trio: Q&A (realized as 17 questions in whitepaper §17 — including "should the planner speak MCP", "timeout vs. retry delay", "why DB-clock timestamps", and "what if a planner answer crashes before persistence — an unpersisted answer never happened; re-ask; an LLM may legitimately change its mind, because 'asked exactly once' covers only persisted decisions"), the Registry, and the TX ledger.

---

## 21. The Atomic Transaction Ledger (Acceptance Table — every entry must be a single transaction)

| ID | Contents | Notes |
|---|---|---|
| TX-W | Create workflow definition (incl. planner type + config) | Single write; planners are rebuilt from this row after restart |
| TX0 | Create run (running) | Single write |
| TX1 | Create step (decision, seq, count=0) + first attempt + set current_attempt_id | **Barrier 1: dispatch only after commit**; the timing anchor |
| TX2 | attempt→done + step→done + write output | **Barrier 2: ask the planner only after commit** |
| TX3 | attempt→failed (one of four reasons, one shared path) + attempt_count++; **if count=X, in the same transaction:** step→DLQ + run→DLQ + DLQ record (worker_retry_exhausted, per-attempt context) | Verdict and DLQ in one blade |
| TX4 | New attempt + CAS-update current_attempt_id | No dual-validity instant |
| TX5 | Replay (worker side): count→0 + step→running + run→running + new attempt + current_attempt_id | The reset is mandatory |
| TX6 | Replay (planner side): run→running | |
| TX7 | run→done | |
| TX8 | Planner declares fail: run→DLQ + DLQ record (planner_declared_fail) | A legitimate answer; no budget consumed |
| TX9 | Planner budget exhausted: run→DLQ + DLQ record (reason = final attempt's class; full detail in context) | |
| CAS-A | Report validation: `UPDATE ... WHERE attempt_id=? AND status='running'` | One statement, inherently atomic |

**Acceptance method:** every gap between two adjacent persisted writes is a crash window. Each transaction's post-commit state must land in one of the five legal combinations (§10) and be claimed by a recovery action (or "untouched"). Check every TX; the model is closed only when all pass. If steps.status and steps.output ever diverge (implementation defect), **output wins**.
