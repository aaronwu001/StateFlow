# StateFlow v1.0 Refactor — Claude Code Session Prompts

**How to use this file:** Each session below is one self-contained prompt for Claude Code. Paste the **Master Context** block at the top of *every* session, followed by that session's block. Do not run two sessions in one go. Do not start session N+1 until session N's completion condition is verified and signed off.

---

## MASTER CONTEXT (prepend to every session)

You are modifying **StateFlow**, a Go durable-execution orchestrator, already containerized (multi-stage Dockerfile → distroless static runtime; docker-compose.yml runs Postgres + orchestrator; docker-compose.demo.yml adds Python demo workers and an LLM planner adapter). Module: `github.com/aaronwu000/stateflow`.

**Authoritative documents (read before touching code; when in doubt, they win over existing code and over your instincts):**
- `docs/StateFlow_Whitepaper_v1_0.md` — the target design. Key sections: §4 state model, §5 step loop, §6 timeout doctrine, §7 failure classification, §8 invariants/recovery, §14 schema, §19 the Atomic Transaction Ledger.
- `docs/StateFlow_Rules_Consolidation_v3_EN.md` — the rule-by-rule spec with rationale (the only edition — no Chinese mirror exists).

**The design in one paragraph:** run/step/attempt each have exactly three states (run: RUNNING/DONE/DLQ; step: RUNNING/DONE/DLQ; attempt: RUNNING/DONE/FAILED with mandatory failure_reason ∈ {worker_reported, timeout, malformed, orphaned}). The old DECIDED and FAILED step states are **gone**. Every attempt is timed from creation (default 60s, workflow- then step-overridable); timeout = failure. Retry budget is the persisted `steps.attempt_count`, incremented only in TX3, reset only in TX5. Recovery = scan RUNNING runs → combination table (§8.2) → claim orphaned attempts → budget check → re-dispatch or already-DLQ'd. The two write barriers are TX1 (persist decision+attempt before dispatch) and TX2 (persist result before next planner call). **Wire formats (binding):** sync workers receive the **bare input** as the POST body plus headers `X-StateFlow-Step-ID` / `X-StateFlow-Attempt-ID`; async workers receive the `{step_id, attempt_id, input}` envelope; **every status string on the wire is UPPERCASE** ("DONE"), identical to the stored values.

**Non-negotiable rules for every session:**
1. **The TX ledger (§19) is law.** Every TXn must be a single database transaction with exactly the listed contents. Never split a TX, never merge two TXs, never reorder "commit → then act" into "act → then commit".
2. **Single writer:** only the orchestrator loop writes step/run state. The async callback handler validates (CAS) and pushes to the channel — nothing else.
3. **CAS everywhere a report lands:** `UPDATE ... WHERE attempt_id=$X AND status='RUNNING'`. A report that matches nothing is ACKed (200) with zero state effect.
4. **Timestamps:** always DB `now()` at commit; never accept timestamps from worker/planner payloads; never use process time for ordering.
5. **Scope isolation:** touch only the files listed in the session's scope. If you believe an out-of-scope file must change, STOP and report instead of editing.
6. **Verification before reporting:** run the session's completion condition verifier. Never report success from "the code looks correct". If a verifier doesn't exist, building it is part of the session.
7. **Pre-release freedom:** the project is unpublished. Schema changes rewrite `migrations/001_initial.sql` in place and assume a fresh Postgres volume (`docker compose down -v`). Do NOT add a migration tool.
8. Tests that assert old-model behavior (DECIDED/FAILED step states, four recovery rules, in-memory retry counting) are expected to be rewritten or deleted in their owning session — do not "fix" them by weakening new-model code.

Postgres for integration tests: `docker compose up -d postgres`, then
`TEST_DATABASE_URL="postgres://stateflow:stateflow@localhost:5432/stateflow?sslmode=disable" go test -p 1 ./...` (`-p 1` is mandatory across packages: they share one DB and each resets the schema).

**Operative checklists (what "complete" means):** (a) the **Atomic Transaction Ledger** (whitepaper §19 ≡ rules v3 §21) is the correctness checklist — every TX must exist, be one transaction, and contain exactly the listed writes; (b) the **cross-session invariant checklist** at the end of this file is the mechanical checklist — re-verified at the end of every session; (c) the **Session 0 audit's coverage map** is the completeness checklist — no file it flags may remain unclaimed by a session.

**Required end-of-session report — answer all seven questions, every session:**
1. Files changed (complete list, one-line summary each).
2. Completion-condition command(s) run, with verbatim output.
3. TX mapping: every ledger TX this session implemented or touched → `file:function`, plus confirmation each is a single BEGIN…COMMIT.
4. Deviations: anything done that is not literally in this prompt — what and why.
5. Inconsistencies discovered between whitepaper / rules v3 / existing code / this prompt. Report them; do NOT silently resolve them.
6. Out-of-scope needs: files outside this session's scope you believe must change (list only — do not edit them).
7. Open questions for the project owner.
8. **State Snapshot Generation:** After answering the 7 questions, you MUST create or overwrite a file named `STATE_SNAPSHOT.md` in the project root. This file must be written in Markdown and contain:
    - **Current Pointer:** Explicitly state which Session was just completed (e.g., "Just completed Session 1").
    - **Core Changes:** A brief technical summary of the schema/code changes made, specifically referencing which whitepaper sections or TX rules were implemented.
    - **Deviations & Blockers:** Any tests skipped, blocked environments (like Docker hangs), or open questions that the architecture checker (Opus) needs to resolve before the next session.


<!--
  STATE_SNAPSHOT.md — 每個 session 結束時由 Claude Code 覆寫（流水帳區除外，見 §8）。
  規則：貼實體證據，不接受文字轉述。凡標「verbatim」的欄位，貼終端機原始輸出，
  不要改寫、不要摘要、不要只寫「通過」。冷啟動的 Opus 審的是這些實體，不是你的總結。
  任何一欄無法誠實填寫 —— 停下來回報，不要編一個看起來合格的內容。
-->

# STATE_SNAPSHOT

## 1. 進度指針
- 剛完成 Session：`N —— <一句話 scope>`
- 下一個 Session：`N+1 —— <一句話 scope>`
- 本次 commit SHA：`<git rev-parse HEAD 的完整 SHA>`
- 分支：`<branch>`

## 2. 驗證證據（verbatim —— 貼原始輸出）
> 對照 prompts 檔裡本 session 完成條件的每一條。每條指令貼「指令 + 它的完整輸出」。

### 2a. 完成條件指令與輸出
```text
$ <你跑的第一條完成條件指令>
<verbatim 輸出，包含測試統計行，例如 ok/FAIL、PASS/FAIL 計數>

$ <第二條……>
<verbatim 輸出>
```

### 2b. 測試計數（照實填，來源即上面輸出）
- 套件測試：`<passed>/<total>` 通過；失敗：`<列出失敗的測試名，或「無」>`
- 舊模型測試依計畫刪除/預期失敗者：`<列出，並註明是本 session owning 的>`

### 2c. Owner oracle（僅 Session 6 起適用；更早的 session 填「N/A — 尚不可跑」）
```text
$ python3 test/acceptance/crash_recovery_test.py
<verbatim；必須看到 PASS: crash_recovery_test 那一行>

$ EXPECT_X=2 python3 test/acceptance/dlq_replay_test.py
<verbatim；必須看到 PASS: dlq_replay_test 那一行>
```

## 3. 動過的檔 / 故意沒碰的檔
- 本 session 修改/新增的檔（`git diff --name-status <上個 SHA>..HEAD`，verbatim）：
```text
<貼 name-status 輸出>
```
- **`test/acceptance/` 的 git 狀態（必填，應為無變動）：**
```text
$ git status --porcelain test/acceptance/
<貼輸出；預期為空。若非空 —— 在 §6 紅字說明為什麼動了它>
```

## 4. 與 session 指示的偏離點
> 逐條列。空白欄位可疑：真實實作幾乎不可能零偏離。若真的零偏離，寫「無，且我已逐條核對完成條件」。
- 偏離 1：`<做了什麼跟指示不同> —— 原因：<為什麼> —— 影響面：<哪些規則/斷言可能受影響>`
- 偏離 2：……

## 5. 本次定案的真實介面 / Schema（貼程式碼，不接受轉述）
> 冷啟動的 Opus 要審真的定義。貼本 session 產出或改動的**實際**程式碼區塊。

### 5a. 介面 / 型別定義
```go
<貼本 session 定案的 interface / type 定義原文>
```

### 5b. Schema（若本 session 觸及）
```sql
<貼 migrations/001_initial.sql 的相關段落原文>
```

## 6. 未解問題（分類 —— 這欄最重要）
> 把「卡住停下來問」和「我自己假設一個答案繼續做了」分開。後者用 🔴 標，每條都可能是規格偏移。
- 🟡 已停下、需裁示：`<問題><選項 A/B><你的傾向>`
- 🔴 我自行假設後繼續：`<假設了什麼><據此做了什麼><若假設錯，要回退什麼>`
- 無：`<若真的沒有，明說>`

## 7. CONFIRM 值（一旦本 session 定了就填，供 oracle 環境變數對齊）
- planner_config 內 HTTP planner URL 的欄位名：`<例如 url —— 或「本 session 尚未定」>`
- retry limit X 在 planner_config 的欄位名：`<例如 retry_limit —— 或「尚未定」>`
- `POST /workflows` / `POST /runs` 回傳的 id 欄位名：`<例如 workflow_id / run_id —— 或「尚未定」>`

---

## 8. 流水帳（APPEND-ONLY —— 覆寫本檔時，這區只准往下加一行，永不刪改上面的行）
<!-- 每個 session 結束加一行：Session N (SHA 前7碼)：一句話結果。讓冷啟動的 Opus 看得到整條軌跡。 -->
- Session 0 (`<sha7>`)：read-only 稽核完成，產出 test/coverage 盤點。
- Session 1 (`<sha7>`)：`<一句話結果>`
<!-- Session 2 起在此行下方繼續 append -->
 
---

## SESSION 0 — Read-only audit (no code changes)

**Build:** a complete change-impact audit, for human review before any edit is made. You must not modify any file in this session.

**Scope:** read everything; produce the audit report in your response. Write nothing to disk.

**Instructions:** produce three inventories:
1. **Test inventory.** Every `*_test.go` file plus every asserting demo script (`crash_demo.py`, `run_demo.sh`): for each, what it asserts today, and a verdict — `keep as-is` / `rewrite in Session N` / `delete in Session N`. Every old-model assertion (DECIDED/FAILED step states, four recovery rules, in-memory retry counting, run status FAILED) must end with a rewrite/delete verdict and an owning session.
2. **Old-model reference inventory.** `grep -rn` results (file:line) for: `DECIDED`, run/step-status `FAILED`, `attempt_number`, `decided_at`, `dispatched_at`, `ResetToDecided`, `replay_round`, five-state CHECK constraints, and any in-memory attempt counter.
3. **Coverage map.** Every file in the repo → the session (1–8) that will touch it, or `untouched`. **Flag any file that appears in inventory 2 but is claimed by no session — that is a coverage gap and it blocks Session 1.**

**Completion condition:** the three inventories delivered; the owner reviews the test inventory and the coverage map and explicitly approves before Session 1 begins.

---

## SESSION 1 — Schema rewrite

**Build:** the v1.0 schema, replacing the old five-state schema in place.

**Scope:** `migrations/001_initial.sql` only.

**Instructions:** Rewrite the migration to match whitepaper §14.1 exactly:
- `runs.status` CHECK IN ('RUNNING','DONE','DLQ') — FAILED removed.
- `steps.status` CHECK IN ('RUNNING','DONE','DLQ') — DECIDED and FAILED removed. Add `attempt_count INT NOT NULL DEFAULT 0`. Keep `decision JSONB`, `output JSONB`, `seq`, `current_attempt_id UUID` (still deliberately **no FK** — preserve the existing comment explaining the insertion-order cycle), `created_at` (renamed from `decided_at` — the old name is retired with the DECIDED state; nothing may reference `decided_at` afterward), `completed_at`.
- `attempts`: **drop `attempt_number`**. Add `failure_reason TEXT CHECK (failure_reason IN ('worker_reported','timeout','malformed','orphaned'))` (nullable; must be non-null when status='FAILED' — enforce with a CHECK). Keep `error`, `resolved_at`; rename `dispatched_at` → `created_at` (the row is inserted at TX1/TX4, *before* the actual dispatch — it is the timeout anchor). Ordering is by `created_at`.
- `dead_letter_queue.reason` CHECK IN ('worker_retry_exhausted','planner_unreachable','planner_malformed','planner_declared_fail'). `step_id` stays nullable (planner-side entries have no step at fault).
- Indexes: keep `idx_steps_run_id`; add composite `idx_steps_run_seq ON steps(run_id, seq)`; add `idx_runs_status ON runs(status)`; keep `idx_attempts_step_id`, `idx_dlq_run_id`.

**Completion condition:** `docker compose down -v && docker compose up -d postgres`; then `psql` shows all five tables with the new CHECK constraints and indexes; paste `\d steps`, `\d attempts`, `\d runs`, `\d dead_letter_queue` output in the report.

---

## SESSION 2 — Core types and the StateStore contract

**Build:** the new `internal/core/interfaces.go` — types and the store interface derived from the TX ledger.

**Scope:** `internal/core/` only. The rest of the codebase will not compile after this session; that is expected and acceptable. Do not "fix" other packages.

**Instructions:**
- Statuses: constants for the 3×3×3 model + the four `FailureReason` values.
- Keep `RunID/StepID/AttemptID`, `RunState`, `HistoryEntry`, `StepDecision`, `StepSpec` (StepSpec keeps `TimeoutSeconds`; document: 0 ⇒ inherit workflow default ⇒ inherit system default 60), `Result` (Status "done"|"failed" + `Reason FailureReason` + Output/Error/HTTPStatus), `Frontier{RunID, History, PendingStep *StepSpec, PendingAttemptID AttemptID, AttemptCount int}` (recovery needs the count and the live attempt id), `RunRef`.
- `NextStepPlanner`, `WorkerTransport` (still one blocking `Dispatch`) unchanged in shape. `RetryPolicy`: **keep the `Next(count int, err error) → (delay, toDLQ)` shape** (it stays a stable extension point per whitepaper §20), but its doc comment must state that `count` is **the persisted `steps.attempt_count` read from the store**, never a caller-local variable. This is the exact defect the refactor targets: today the loop passes a loop-local counter that resets to 1 on every crash. The signature is unchanged; the contract on where `count` comes from is now binding.
- `StateStore` — one method per ledger entry plus reads. Suggested names (adjust only for Go idiom, keep the TX mapping in doc comments):
  `CreateWorkflow`(TX-W), `CreateRun`(TX0), `CreateStepWithAttempt`(TX1), `CheckpointSuccess`(TX2), `RecordFailure`(TX3 — takes reason; returns `dlqed bool`), `StartNewAttempt`(TX4), `ReplayWorkerSide`(TX5), `ReplayPlannerSide`(TX6), `MarkRunDone`(TX7), `MarkRunDLQPlannerDeclared`(TX8), `MarkRunDLQPlannerExhausted`(TX9); reads: `LoadFrontier`, `ListRunningRuns`, `GetRun`, `ListDLQ`, `GetDLQEntry`.
- Every TX method's doc comment must state: "MUST execute as a single database transaction (Ledger TXn)."

**Completion condition:** `go build ./internal/core/...` exits 0; interface doc comments include the full TX mapping.

---

## SESSION 3 — PostgresStore rewrite

**Build:** `internal/store/postgres.go` implementing the new StateStore, plus its integration tests.

**Scope:** `internal/store/` only.

**Instructions:**
- Each TX method = one `BEGIN…COMMIT`. TX3 branches **inside** the transaction: increment count; if count==X (X passed in), also step→DLQ + run→DLQ + insert DLQ row with per-attempt context (aggregate the attempt reasons/errors for the step into `context`).
- TX2 and reporting paths use CAS semantics: the attempt row update carries `WHERE attempt_id=$1 AND status='RUNNING'`; zero rows affected ⇒ return a typed "superseded" result, not an error.
- TX5 resets `attempt_count=0` and does all five writes in one transaction.
- `LoadFrontier`: history = done steps ordered by seq; pending = the single step with `status='RUNNING'` (there can be at most one under the serial-loop invariant) with its decision, current_attempt_id, attempt_count.
- Rewrite integration tests. Mandatory new tests: (a) barrier ordering (TX1 commit visible before any dispatch simulation; TX2 before next decision); (b) **TX3 same-blade**: with count=X-1, one RecordFailure call yields attempt FAILED + count=X + step DLQ + run DLQ + DLQ row, atomically (verify by crash-simulation: no intermediate state observable between them — assert via a second connection inside the test); (c) TX5 reset; (d) CAS superseded report is a no-op; (e) status/output divergence impossible: after TX2, `status='DONE' ⇔ output IS NOT NULL`.

**Completion condition:** `docker compose up -d postgres && TEST_DATABASE_URL=... go test ./internal/store/... -v` all green.

---

## SESSION 4 — Transports and the timeout mechanism

**Build:** timeout-aware sync and async transports with malformed detection.

**Scope:** `internal/transport/` only.

**Instructions:**
- Resolve effective timeout: step override > workflow default > 60s. **The deadline is anchored at attempt creation, not at dispatch:** the loop computes `deadline = attempt creation time + effective timeout` and passes it via `context.WithDeadline` into `Dispatch`; transports honor the ctx deadline rather than starting their own clock. (This makes the whitepaper §6 claim — "the clock starts at attempt creation, covering the pre-dispatch window" — literally true in code.)
- **Sync:** HTTP client deadline = the ctx deadline. Map outcomes: non-2xx ⇒ failed/worker_reported (HTTPStatus set); deadline exceeded or transport error ⇒ failed/timeout; 2xx but body not valid JSON, or `output_field` declared but absent ⇒ failed/malformed; else done with output (whole body or the output_field subtree).
- **Async:** POST; expect 202 (non-202 ⇒ failed/worker_reported); then `select { case r := <-ch: …; case <-ctx.Done(): return failed/timeout }` — the ctx deadline is the creation-anchored one passed in by the loop. The channel registry stays encapsulated in the transport; `DeliverCallback` keeps validating against `current_attempt_id` via the store read, but state writes remain the loop's job.
- A timer firing must leave the registry clean (deregister the channel) so a later stale callback finds nothing and is ACKed as superseded.
- **Dispatch wire formats — this session implements and TESTS them:** sync sends the **bare `input` as the POST body** (no wrapper of any kind) plus headers `X-StateFlow-Step-ID` and `X-StateFlow-Attempt-ID`; async sends the `{step_id, attempt_id, input}` JSON envelope. Mandatory tests: an httptest sync worker asserts the received body equals the planner-decided input byte-for-byte AND both headers are present with the correct values; an async test asserts the envelope shape. Getting this wrong silently breaks either the zero-modification promise (sync) or callback dedup (async).
- `multi.go` (the per-step mode router) stays as-is: it must keep passing the loop's ctx through unchanged so the creation-anchored deadline reaches both transports; the non-async→sync fallthrough is acceptable (missing mode is already rejected upstream by planner acceptance criteria).

**Completion condition:** `go test ./internal/transport/... -v` green, including new tests for each mapping row (use httptest servers: slow server → timeout; 200-with-garbage → malformed; missing output_field → malformed; async silence → timeout).

---

## SESSION 5 — Loop and recovery rewrite

**Build:** the v1.0 driver loop and recovery.

**Scope:** `internal/orchestrator/` and `internal/planner/` (the latter only for the HTTPPlanner validation/serialization updates required below).

**Instructions:**
- **Loop** per whitepaper §5: read frontier → (pending step? re-dispatch it : ask planner) → TX1 before dispatch → dispatch → TX2 on success / TX3 on failure (pass reason from Result) → if not DLQ'd: sleep retry delay (5s) → TX4 → dispatch again. Planner answers done→TX7, fail→TX8. **The loop owns the deadline:** after TX1/TX4 commits, compute `deadline = attempt creation timestamp + effective timeout` and pass it to `Dispatch` via `context.WithDeadline` (see Session 4). On recovery re-dispatch (TX4), the deadline anchors at the *new* attempt's creation. Note: recovery re-dispatch intentionally skips the 5s retry delay — the crash itself already provided more than enough cooldown.
- **Planner budget** in the loop: 30s deadline per call, 3 total attempts; classify each failure as unreachable (timeout/conn) or malformed (fails §12.3 acceptance criteria — reuse/extend the HTTPPlanner validation); on exhaustion TX9 with reason = final attempt's category and full per-attempt detail in context. StaticPlanner is infallible by construction and skips the budget path.
- **Retry budget source (binding):** the loop must feed `RetryPolicy.Next` with the persisted `steps.attempt_count` (read via the store / returned by TX3), NOT a loop-local counter. Delete the `for attemptNum := 1; ; attemptNum++` local-counter pattern in loop.go entirely — its survival is the in-memory-budget defect the refactor exists to fix.
- **Planner reconstruction:** loop and recovery build the planner instance from the workflow row (planner_type + planner_config) each time a run (re-)enters the loop — never from process-global state.
- **Recovery** per §8.3: scan RUNNING runs; no steps or last_step done ⇒ enter loop at "ask planner"; last_step RUNNING ⇒ (a) if frontier shows a RUNNING attempt, RecordFailure(orphaned) — this may DLQ inside TX3; (b) if DLQ'd, stop; (c) else TX4 + dispatch, then continue the normal loop. Keep `[RECOVERY]` structured logs (update fields: run_id, steps_done, pending_step, attempt_count).
- Delete the four-rule recovery code and `TestRecovery_FailedNoOutputReDispatched`. Mandatory new tests: (a) budget-boundary crash: seed DB with count=X-1 and a RUNNING attempt, run recovery, assert orphan claim lands the step in DLQ and nothing is dispatched; (b) recovery re-entrancy: run recovery twice over the same seeded state, assert count incremented exactly once; (c) crash-between-TX3-and-TX4: seed step RUNNING with last attempt FAILED and count<X, assert recovery dispatches a new attempt without claiming anything; (d) planner asked exactly once per step across a simulated crash (count Decide calls); (e) **wire-casing test**: capture the exact JSON HTTPPlanner sends and assert every history entry's status is UPPERCASE ("DONE") and entries are in seq order — this is a contract test, not a style check.

**Completion condition:** `TEST_DATABASE_URL=... go test -p 1 ./...` fully green (this is the session where the whole tree must compile again).

---

## SESSION 6 — HTTP API layer

**Build:** API surface aligned to the new model.

**Scope:** `internal/api/` and `cmd/stateflow/main.go`. (Amended after the Session 5 audit: `main.go` was claimed by no session and is broken by signature changes accumulated across Sessions 4-5 — `Loop.Planner` field removed, `RecoverRuns` now takes `(runID, workflowID, workflowInput)`, `NewAsyncTransport` now requires a store argument. It is thin wiring with no design content that would conflict with this session's API redesign; fix its call sites to match the current `internal/orchestrator`/`internal/transport` signatures as part of this session so the full tree compiles per the completion condition.)

**Instructions:**
- Endpoint set unchanged (`POST /workflows`, `POST /workflows/{id}/runs`, `GET /runs/{id}`, `POST /tasks/complete`, `POST /tasks/fail`, `GET /dlq`, `POST /dlq/{id}/replay`).
- `GET /runs/{id}` — **redesign the whole response shape in one pass** (do not scatter it): run status RUNNING/DONE/DLQ; per-step status, seq, attempt_count, and a current-attempt summary (reason/error of the latest failed attempt if any); if run=DLQ include `dlq_reason` joined from the DLQ table. Note the step timestamp field is now `created_at` (renamed from `decided_at` in Session 1) — surface it under the new name; do not emit `decided_at` anywhere in the JSON.
- **Upgrade `TestAPI_EndToEnd_HTTPPlanner` (do NOT keep as-is):** promote its incidental casing check into an explicit wire-casing contract test — assert every history-entry `status` sent to the planner is UPPERCASE ("DONE") while the planner's own decision `status` stays lowercase (continue/done/fail). These are two different fields with two different casing rules; the test must pin both.
- `/tasks/fail` body: `retry_after_seconds` **optional**, accepted, ignored (comment it as reserved).
- `POST /dlq/{id}/replay`: branch on the entry — worker-side (step_id non-null) ⇒ TX5 then re-enter the loop goroutine at dispatch; planner-side ⇒ TX6 then re-enter at "ask planner".
- Callback handlers keep the validate-push-200 shape; superseded ⇒ 200 no-op; unparseable ids ⇒ 400 no-op.

**Completion condition:** `go test ./internal/api/... -v` green, including: replay-worker-side resets count and completes without re-running done steps; replay-planner-side re-asks the planner; GET /runs shows dlq_reason.

---

## SESSION 6.5 — TX5 worker-side replay dispatch fix (inserted after Session 6 audit)

**Why this session exists:** Session 6's independent audit found that `POST /dlq/{id}/replay`'s worker-side branch calls `store.ReplayWorkerSide` (TX5, which correctly resets `attempt_count=0` and creates a fresh RUNNING attempt) and then re-enters via the generic `Loop.Run()` entry point. `Loop.Run()`'s unconditional one-time recovery check sees a `PendingStep` with a RUNNING attempt and treats it as a crash-orphaned attempt: it immediately calls `RecordFailure(orphaned)`, burning the retry budget TX5 just reset, before the worker is ever actually dispatched to. Reproduced live against the real acceptance oracle: `EXPECT_X=2 python3 test/acceptance/dlq_replay_test.py` FAILs with "after replay never observed RUNNING + attempt_count=0 (TX5 reset missing)". This contradicts whitepaper §11's stated rationale for the TX5 reset ("without it... the button would be decorative") — the bug reproduces exactly that failure by a different mechanism. Affects every worker-side replay, not just `retry_limit=1`.

**Build:** a `Loop` entry point that resumes an existing `PendingStep` (freshly created by TX5, never yet dispatched) by dispatching it directly — skipping the initial orphan-claim check that's only correct for *actual* post-crash recovery, not for a replay that just created a brand-new attempt.

**Scope:** `internal/orchestrator/` (add the new entry point; do not change the existing crash-recovery entry point's behavior) and `internal/api/server.go` (change the worker-side branch of `handleDLQReplay` to call the new entry point instead of the generic `startLoop`/`Loop.Run()`).

**Instructions:**
- In `internal/orchestrator/`, add a method (e.g. `Loop.ResumeReplayedStep` or similar — pick a name consistent with existing conventions) that: loads the frontier, confirms the pending step's current attempt was indeed just created (not stale/crashed), and dispatches it directly via the normal TX2/TX3 success/failure path — without going through the orphan-claim branch that `Run()`'s crash-recovery path uses.
- Do not touch or weaken the existing crash-recovery orphan-claim logic — that behavior is correct and tested (Session 5) for its actual purpose (recovering from a real crash where an attempt's fate is unknown). This new entry point is for the different case where the caller (DLQ replay) *knows* the attempt is fresh and has never been dispatched.
- Update `internal/api/server.go`'s `handleDLQReplay` worker-side branch to call the new entry point.
- Rewrite/extend `TestAPI_DLQ_ReplayWorkerSide` to assert the post-replay attempt_count stays at the value the worker's actual outcome implies (i.e., confirm no phantom `orphaned` attempt is recorded when the worker is dispatched and responds normally), not just that replay "succeeds" in some loose sense.
- Add or extend an `internal/orchestrator` test that seeds a TX5-replayed state directly (RUNNING step, RUNNING attempt with `attempt_count=0`, never dispatched) and asserts the new entry point dispatches without any `RecordFailure(orphaned)` call.

**Completion condition:** `TEST_DATABASE_URL=... go test -p 1 ./...` fully green; additionally run `EXPECT_X=2 python3 test/acceptance/dlq_replay_test.py` against a live `docker compose up -d --build` stack and confirm it now PASSes (note: reaching the container from the acceptance script may require `ADVERTISE_HOST=<host IP>` depending on the Docker networking topology — check `test/acceptance/README.md`).

---

## SESSION 7 — Demos, CLAUDE.md, and full verification

**Build:** demo scripts and project docs aligned; end-to-end proof.

**Scope:** `demo/` (including `demo/playbook/`, `demo/configs/`, `demo/README.md`, `demo/README.zh.md`), `docs/USER_MANUAL.md`, `CLAUDE.md`, `README.md` (pointer updates only in README).

**Instructions:**
- `crash_demo.py`: assertions updated to new statuses; the kill-during-async-NER scenario must now also tolerate/verify the orphan-claim path: after restart, assert the NER step's attempt history shows one `orphaned` attempt and the idempotency cache absorbed the re-dispatch; assert planner Decide-count ≤ one per step.
- `run_demo.sh` scenario 2 (worker absent): assert DLQ reason `worker_retry_exhausted` and per-attempt reasons in context; scenario 3: unchanged semantics, updated status strings.
- `demo/configs/*.yaml`: add explicit per-step `timeout_seconds` to both configs, each comfortably exceeding that worker's artificial delay (the 60s default would work, but the demo must model deliberate configuration); the DUMMY plan in `llm_adapter.py` likewise sets explicit timeouts. Worker timeouts must always exceed worker delays so the happy path never mis-kills.
- `demo/playbook/PLAYBOOK.en.md` + `PLAYBOOK.zh.md`, `demo/README.md` + `README.zh.md`: purge all old-model content — DECIDED/FAILED step states, the four-recovery-rules narrative, old DLQ reason names (`planner_failed`, `retry_exhausted`), "retry budget restarts on recovery". Describe recovery as: combination table → orphan claim (`orphaned` attempt, budget++) → budget check → re-dispatch. Keep the walkthrough structure; only the semantics change.
- **Rewrite `docs/USER_MANUAL.md` (targeted, keep its skeleton):** (a) timeout defaults: 60s system default, workflow-level then step-level overrides; remove all "defaults to 30s" claims; (b) DLQ reasons: replace `planner_failed` with the four-value set `worker_retry_exhausted` / `planner_unreachable` / `planner_malformed` / `planner_declared_fail`, incl. the triage table (do not blind-replay declared_fail); (c) add the quantified concurrent-idempotency contract: up to X concurrent duplicate invocations of one step_id (timeout re-dispatch can race a still-alive worker — not only crash re-dispatch), responsibility boundary per whitepaper §15; (d) add: a success report arriving after the timeout verdict is rejected (extends the superseded-callback section); (e) rewrite the manual's dispatch-format sections per whitepaper §13.1: sync workers receive the bare input **plus** `X-StateFlow-Step-ID` / `X-StateFlow-Attempt-ID` headers — update §2.2 and rewrite the sync idempotency recommendation (§2.3) to prefer the header `step_id` as the cache key, demoting input-hash to fallback; async unchanged; confirm every wire status in examples/template stays UPPERCASE; update the template's timeout guidance to the 60s-default/override model. Optionally (recommended): switch the sync demo workers' idempotency caches to key on the `X-StateFlow-Step-ID` header, showcasing the new contract.
- **Replace the transitional `CLAUDE.md`** (installed before Session 0) with the final version: new Quick Reference (three-state tables, combination table, TX ledger summary, timeout doctrine, orphan rule, CAS rule, single-writer rule), new deferred list (from whitepaper §18/§21), keep session discipline and test-running sections (update `-p 1` note if package set changed).
- Do NOT touch Dockerfile or compose files unless a demo env var must be added; if so, report it first.

**Completion condition:** from a clean clone: `docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build`; `python demo/crash_demo.py` passes; `./demo/run_demo.sh` scenarios 1–3 pass; `TEST_DATABASE_URL=... go test -p 1 ./...` green. Paste the demo output in the report.

---

## SESSION 8 — Final audit & conformance report

**Build:** the end-to-end conformance audit. Code changes only if the audit uncovers a defect — report it first, fix only with owner approval.

**Scope:** read everything; run everything.

**Instructions:**
1. **TX ledger conformance table:** for each of TX-W, TX0–TX9, CAS-A → `file:function`, quoting the transaction boundaries (the BEGIN…COMMIT span) proving each is one transaction.
2. **Global sweeps (paste outputs):** the full cross-session invariant checklist below, plus `grep -rn "decided_at\|dispatched_at\|attempt_number\|replay_round\|ResetToDecided" --include="*.go" --include="*.sql" --include="*.py"` — must return nothing.
3. **Doc-vs-code diff:** read whitepaper §4–§11 against the code; list (a) any behavior in code that contradicts the doc, (b) any behavior in code the doc does not mention. State "none found" explicitly per section if empty.
4. **Full verification:** clean-clone compose build; `python demo/crash_demo.py`; `./demo/run_demo.sh` scenarios 1–3; `TEST_DATABASE_URL=... go test -p 1 ./...` — paste all outputs.
5. Answer the standard seven-question report, plus one extra: **which parts of the refactor are you least confident about, and what test would raise that confidence?**

**Completion condition:** conformance table complete; all sweeps clean; all tests and demos green; the confidence question answered substantively.

---

## SESSION 8.5 — Async malformed-output detection (inserted after Session 8 audit)

**Why this session exists:** Session 8's conformance audit (independently re-verified) found that async workers can never produce a `FailureReasonMalformed` failure, even though whitepaper §4.2 ("...or an async callback with valid ids but unparseable output") and §7.1 ("valid ids with unparseable output → `malformed` failure") both explicitly promise this path. `internal/api/server.go`'s `handleTaskComplete` decodes the callback's `output` as an unvalidated `json.RawMessage` and unconditionally builds `core.Result{Status: core.ResultStatusDone, Output: req.Output}` — no content check, no absence check. `FailureReasonMalformed` is producible today only via `internal/transport/sync.go`'s `extractOutput` (sync's `OutputField` mechanism, whose own doc comment says "sync only"). The independent audit additionally found this breaks the schema's own documented invariant (`migrations/001_initial.sql`: "output non-null → step DONE"): `jsonOrNull()` (`internal/store/postgres.go`) turns an absent/empty async `output` into JSON `null`, and `CheckpointSuccess` (TX2) will mark the step DONE with that null output anyway — a buggy (not even malicious) async worker that omits `output` is silently checkpointed as a success, and the null output flows into planner history despite `HistoryEntry.Output`'s doc comment claiming it's "always present" for a DONE step. Owner decision: implement the missing detection now rather than defer.

**Build:** async-path malformed-output detection, reusing the existing TX2/TX3 success/failure machinery (this is a new *classification*, not a new Ledger transaction — `RecordFailure`/TX3 already handles any `FailureReasonMalformed` result correctly once one is actually produced for the async path).

**Scope:** `internal/api/server.go` (the `handleTaskComplete` handler — this is where the raw external `output` JSON first enters the system for async, the natural analogue of sync's `extractOutput` boundary in `internal/transport/sync.go`), `internal/api/server_test.go`, `internal/core/interfaces.go` (only the `StepSpec.OutputField` / related doc comments, to stop asserting "sync only" once async gets its own rule — do not change the type's shape), and `docs/USER_MANUAL.md` (only if you find it currently asserts async has no malformed detection — check first, edit only if genuinely contradicted; do not do a broader pass). Do NOT touch `docs/StateFlow_Whitepaper_v1_0.md` or `docs/StateFlow_Rules_Consolidation_v3_EN.md` (authoritative, owner-owned) or `CLAUDE.md` unless you find it makes a claim this session's fix directly contradicts — if so, STOP and report rather than editing it.

**Instructions:**
- Define the async equivalent of "unparseable output" precisely, and document the exact rule you chose in your report (this is a real interface decision, not a mechanical port of sync's logic — async has no `OutputField` subtree concept, the whitepaper text is directional, not a literal spec). The minimum bar that closes the confirmed schema-invariant violation: `output` absent from the JSON body, or present but JSON `null`, must be classified `failed`/`malformed`, not `done`. If you want to also treat empty-object/empty-array as malformed, justify it against the whitepaper text and document the choice; if genuinely ambiguous whether to include that case, note it as an open question rather than guessing silently.
- Do not touch sync's `extractOutput` / `OutputField` logic (`internal/transport/sync.go`) — it is correct and out of this session's concern.
- `handleTaskComplete` must still ACK with HTTP 200 for a malformed report (per the existing CAS/superseded-report contract — a malformed *content* is a normal, expected failure outcome for the attempt, not an HTTP-level error; the caller should not retry-storm on a 4xx/5xx for something that isn't their transport's fault).
- Add tests to `internal/api/server_test.go`: (a) `/tasks/complete` with valid `step_id`/`attempt_id` and `output` absent from the JSON body → resulting attempt has `failure_reason='malformed'`, step stays RUNNING (or DLQ's if this was the last budget attempt) — reuse the existing budget/DLQ test patterns already in that file; (b) same with `output: null`; (c) confirm the existing happy-path async test (real output) still passes unchanged — no regression on the common case.
- Update `internal/core/interfaces.go`'s doc comment(s) that currently claim malformed-via-`OutputField` is sync-only, to instead describe the two independent mechanisms (sync: `OutputField` subtree-presence check; async: output-absent/null check) — keep this factual and minimal, do not restructure the type.

**Completion condition:** `TEST_DATABASE_URL=... go test -p 1 ./...` fully green including the new tests, run against a live `docker compose up -d --build` stack; paste verbatim output. Since this session touches `internal/api/server.go` (production code, not docs/demos), also re-run the full Session 7/8-style verification to confirm nothing else broke: `python demo/crash_demo.py`, `./demo/run_demo.sh` scenarios 1–3, and both acceptance oracles (`python3 test/acceptance/crash_recovery_test.py`, `EXPECT_X=2 python3 test/acceptance/dlq_replay_test.py`) — paste all outputs.

---

## PHASE 1.5 — Publication

Phase 1 (the v1.0 three-state refactor, Sessions 0–8.5) is complete and fully verified as of
commit `d1d4ee9`. Phase 1.5 is whitepaper §21's next roadmap phase: make the project publishable,
not change its correctness model. None of the sessions below touch the TX ledger, the state
machine, or any file under `internal/orchestrator/`, `internal/store/`, `internal/transport/`,
or `migrations/` — if a session below finds it needs to, STOP and report rather than editing.

Scope confirmed with the owner on 2026-07-11: this phase covers CI (GitHub Actions), the
`/healthz` + self-check subcommand (Temporary Design Registry item #8), a full README rewrite,
and a lightweight static status UI. ghcr.io image publication and the demo storybook/video are
explicitly deferred, not part of this phase — do not add them speculatively.

---

## SESSION 9 — CI (GitHub Actions)

**Build:** a GitHub Actions workflow that automates what has been run manually, by hand, at the
end of every session so far: `go test -p 1 ./...` against a live Postgres, `python3
demo/crash_demo.py`, and the two frozen owner acceptance oracles
(`test/acceptance/crash_recovery_test.py`, `EXPECT_X=2 test/acceptance/dlq_replay_test.py`).

**Scope:** `.github/workflows/` (new). Do not edit `test/acceptance/` — it is frozen/owner-owned
(see its own `README.md`: "not part of any Claude Code session's writable scope"); if the CI run
needs a networking accommodation to reach it, solve that from the workflow YAML/compose side
only. Do not edit files under `internal/`, `cmd/`, or `migrations/`.

**Instructions:**
- Two jobs (or one workflow with sequential steps) on `push` and `pull_request` to `main`:
  1. **Unit + integration tests:** bring up Postgres (service container or
     `docker compose up -d postgres`), run
     `TEST_DATABASE_URL=postgres://stateflow:stateflow@localhost:5432/stateflow?sslmode=disable go test -p 1 ./...`,
     fail the job on any non-`ok` package.
  2. **End-to-end:** `docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build`,
     then run `python3 demo/crash_demo.py` and both acceptance oracles per
     `test/acceptance/README.md`'s documented invocation (Linux CI runners are a much simpler
     Docker networking topology than this project's Windows+WSL2+Docker Desktop dev environment —
     the `--add-host=host.docker.internal:host-gateway` approach that README already documents
     should just work; do not port over any of the `ADVERTISE_HOST`/nested-`wsl.exe` workarounds
     from `STATE_SNAPSHOT.md` §4, those are dev-environment-specific, not CI-relevant).
- `./demo/run_demo.sh` is interactive (menu-driven, uses `read -p`) and is a human-facing demo
  script, not a regression oracle — the three things above (`go test`, `crash_demo.py`, the two
  acceptance oracles) are the actual correctness gates and are already non-interactive. Do not
  add non-interactive flags to `run_demo.sh` to force it into CI; that would expand this
  session's scope into `demo/` for marginal benefit. If you believe CI genuinely needs
  `run_demo.sh` coverage, report it as an out-of-scope need instead of editing it.
- Add a status badge line to `README.md` is **out of scope for this session** — Session 11
  (README rewrite) owns `README.md`; note the badge markdown in your end-of-session report so
  Session 11 can add it.
- Cache Go modules/build cache between runs (`actions/setup-go`'s built-in cache is sufficient;
  do not hand-roll a cache key scheme).

**Completion condition:** push the workflow, trigger it (a workflow_dispatch run or a throwaway
PR/push is fine), and paste the Actions run's job summary/log showing all steps green. If you
cannot trigger a real run from this environment, say so explicitly and explain exactly what a
human needs to do to confirm it (do not claim success from "the YAML looks correct").

---

## SESSION 10 — `/healthz` + self-check subcommand

**Build:** close Temporary Design Registry item #8 (whitepaper §18) — the distroless runtime
image has no shell, so today liveness/readiness can only be verified externally (e.g. hitting
the API from outside the container). Add an HTTP `/healthz` endpoint plus a self-check CLI
subcommand the container's own `HEALTHCHECK`/readiness probe can invoke without a shell.

**Scope:** `internal/api/server.go` (new route), `cmd/stateflow/main.go` (currently ~100 lines,
single `func main()` — add a subcommand dispatch), `Dockerfile`, `docker-compose.yml` (the
`stateflow` service currently has no healthcheck — Session 8.5's snapshot notes it shows plain
`Up`, never `Up (healthy)`, unlike the other 7 compose services).

**Instructions:**
- `GET /healthz`: no auth, not part of the versioned API surface documented in `CLAUDE.md`'s
  path table (add it there as a one-line addition if you touch that table — check first whether
  it already lists non-business endpoints). Returns 200 with a small JSON body if the server can
  reach Postgres (a trivial `SELECT 1` or equivalent ping through the existing store connection —
  do not open a new ad hoc connection), 503 if it cannot. Keep it fast and side-effect-free; it
  must not touch run/step/attempt state (the single-writer rule still applies — this is a read,
  not a writer).
- Self-check subcommand on the same binary (e.g. `stateflow healthcheck`, distinct from the
  default no-args server-start behavior): makes an HTTP GET to its own `/healthz` on the
  configured listen address and exits 0 on 200, non-zero otherwise. This is what lets
  `Dockerfile`'s `HEALTHCHECK` instruction and `docker-compose.yml`'s `healthcheck:` block work
  without `curl`/`wget`/a shell — they invoke `["/stateflow", "healthcheck"]` directly as the
  distroless entrypoint binary.
- `main.go`'s existing behavior (start the server on `http.ListenAndServe`) must remain the
  default when invoked with no subcommand/args — do not break `ENTRYPOINT ["/stateflow"]`'s
  existing ambient behavior for the normal run case.
- Update `Dockerfile` to add a `HEALTHCHECK` instruction using the new subcommand, and
  `docker-compose.yml`'s `stateflow` service to add a matching `healthcheck:` block (mirror the
  shape already used by the other services in that file for consistency).

**Completion condition:** clean-clone `docker compose down -v && docker compose up -d --build`;
`docker compose ps` shows the `stateflow` service reaching `Up (healthy)` (not just `Up`); paste
that output. Also: `go build ./... && go vet ./...` clean, and `TEST_DATABASE_URL=... go test -p 1 ./...`
still fully green (confirm no regression in `internal/api`).

---

## SESSION 11 — README rewrite

**Build:** a full, publication-facing rewrite of `README.md`. Session 7 only did a pointer-fix
pass (broken doc links, two old-model phrases); this session owns the whole document's content
and structure.

**Scope:** `README.md` only. Do not edit `docs/USER_MANUAL.md`, the whitepaper, or the rules
spec — link to them, don't duplicate them.

**Instructions:**
- Cover, at minimum: what StateFlow is (one paragraph, matching `CLAUDE.md`'s "What This Project
  Is" framing — durable execution layer, frontier model, checkpoint/retry/resume without replay);
  a quickstart (`docker compose up -d --build`, create a workflow, start a run, check its status —
  copy-pasteable curl examples using the real API shape from `CLAUDE.md`'s path table); the
  current project status (Phase 1 complete — three-state model, TX ledger, CAS, DLQ, timeout
  doctrine, all verified; Phase 1.5 in progress — CI/healthz/status-UI); links to
  `docs/StateFlow_Whitepaper_v1_0.md`, `docs/StateFlow_Rules_Consolidation_v3_EN.md`,
  `docs/USER_MANUAL.md`, and the demo (`demo/README.md`); a short architecture summary (run/step/
  attempt states, the two write barriers, recovery) with a pointer to the whitepaper for the full
  model — do not re-derive the whole state machine inline, that's what the whitepaper is for.
- If Session 9 ran first and reported a CI badge snippet in its end-of-session report, add it
  near the top of `README.md`. If Session 9 has not run yet, leave a placeholder comment
  (`<!-- CI badge: add once Session 9 lands -->`) rather than guessing the workflow's badge URL.
- If Session 12 (status UI) has already landed, add a one-line pointer to it; if not, skip that
  mention rather than promising a feature that doesn't exist yet in this branch.
- Verify every link and every code/curl example actually works against a live stack you start
  yourself — do not hand-write an example and assume it's correct.

**Completion condition:** every internal doc link resolves (no 404s to repo-relative paths); every
curl/command example in the README run verbatim against a live `docker compose up -d --build`
stack and paste the output proving it works as written.

---

## SESSION 12 — Lightweight status UI

**Build:** a static, no-build-step HTML+JS page that shows run and DLQ status by reading the
existing API — no new backend logic, no new list/query endpoints (confirmed with the owner: the
UI is a thin read-only client over `GET /runs/{run_id}` and `GET /dlq`, not a reason to design new
aggregate endpoints).

**Scope:** `internal/api/server.go` (one new route to serve the static page — see below),
a new static asset (single `.html` file, inline `<script>`/`<style>`, no external CDN
dependencies — the distroless runtime has no network access to fetch a CDN script reliably and
shouldn't depend on one anyway). Do not touch `internal/orchestrator/`, `internal/store/`, or add
new query methods to `core.StateStore`.

**Instructions:**
- Serve the page from the orchestrator itself via Go's `embed` package (so it ships inside the
  same distroless binary with zero extra deployment surface) at a new route, e.g. `GET /` or
  `GET /ui` — pick whichever doesn't collide with the existing route table in `Handler()`
  (`internal/api/server.go`); same-origin `fetch()` calls to `/runs/{id}` and `/dlq` from the page
  avoid any CORS question entirely (do not add CORS headers to solve a problem that doesn't exist
  if you serve same-origin).
- The page needs, at minimum: an input for a `run_id` that fetches and renders `GET
  /runs/{run_id}`'s response (run status, per-step seq/status/attempt_count, dlq_reason if
  present); a DLQ table rendering `GET /dlq`'s list (id, reason, step_id if present, created_at).
  Keep styling minimal — this is an ops status page, not a product UI.
- No auth, no write actions (no "replay from the UI" button — replay stays a `POST
  /dlq/{id}/replay` API call for now; adding a UI write path is a bigger decision than "lightweight
  status page" and is out of scope here).
- If `GET /dlq` or `GET /runs/{id}`'s current response shape is missing something the UI
  genuinely needs to render, report it — do not silently add a new field to those handlers'
  response structs without flagging it first (that would be a backend API change, which the owner
  explicitly said this session should avoid).

**Completion condition:** clean-clone `docker compose up -d --build`; start a run via the
existing quickstart flow, open the new UI route in a browser (or `curl` it to confirm the HTML is
served), and confirm the run's status and the DLQ table render correctly for at least one DONE
run and one DLQ'd run (reuse a scenario from `demo/run_demo.sh` to produce both). Paste the
`curl` output and describe what was seen when opened in a browser (or paste a screenshot path if
this session's environment supports capturing one).

---

## PHASE 2 — Hardening & LLM semantics

Phase 1 (three-state refactor) and Phase 1.5 (Publication) are both complete and merged to
`main` as of `8d25b7c`. Phase 2 is whitepaper §21's next roadmap phase: "production reliability."
It covers Temporary Design Registry items **#1, #4, #5, #6, #7** (whitepaper §18). Registry item
**#2 (late-result salvage) is explicitly NOT part of this phase** — the whitepaper itself marks it
"2+" (later than Phase 2); do not pull it in.

Design for every item below was worked out with the owner in a dedicated planning conversation
(2026-07-12) before any session prompt was written. **Read the "Design decisions log" below before
touching any of these sessions — it records not just what was decided but why, including a design
that was proposed and explicitly rejected**, so a cold session doesn't accidentally re-propose it.

### Design decisions log (read this first)

**Registry #1 (full-history transmission, whitepaper's own flagged "weakest link"):**
The first design explored in the planning conversation was a multi-round `need_more` negotiation
protocol — a new `StepDecision` verdict letting the planner explicitly request full output for
named steps, with a 3-round cap, budget interaction with the existing planner-failure retry
counter, and a lenient/strict fork on invalid step-name handling. **The owner rejected this as
overcomplicated** ("這應該要只是一個很簡單的非常非常單純的小功能" — "this should just be a very,
very simple small feature") — specifically flagging that a "simple" feature needing changes to
the core decision contract (a new verdict type) and to the authoritative whitepaper's Planner
Contract section was a sign the design had drifted from the registry item's actual stated remedy
("Summary-plus-fetch + pass-by-reference for large payloads" — one sentence, not a protocol).
**Do not implement the `need_more` round-trip design if you encounter references to it in older
context — it was discussed and explicitly discarded, not deferred.**

The design actually adopted (Session 19 below) is a two-tier mechanical size guard requiring zero
new protocol surface:
1. **Per-entry cap, 2KB**: any single `HistoryEntry.Output` whose marshaled JSON exceeds 2048
   bytes gets replaced with a small pointer object (`_truncated`/`size_bytes`/a note pointing at
   the existing, unchanged `GET /runs/{run_id}` endpoint for the full value).
2. **Total payload cap, 50KB**: the owner separately caught that per-entry capping alone doesn't
   bound a long run's *cumulative* history size (many steps each just under 2KB still adds up).
   Budget is allocated **most-recent-step-first, working backward**; steps that don't fit in the
   remaining 50KB budget are reduced to `name`+`status` only — no `Output` field, not even a
   pointer (the whole run is already reachable via `GET /runs/{run_id}`, so there's no need to
   give every dropped step its own pointer).
3. Wire order is unchanged — still `seq` ascending (whitepaper's existing rule). Only "how much
   detail per entry" is computed via the reverse/recency pass; final marshaled order is unaffected.
4. This is computed fresh on every `Decide` call from the real, never-truncated Postgres data —
   nothing new is persisted, and `GET /runs/{run_id}` needs zero changes since it already returns
   full output.
5. `HistoryEntry.Output`'s current doc comment ("Output has no omitempty — it is always present
   for a DONE step") becomes inaccurate under this design (budget-cut entries omit it entirely) —
   Session 19 must correct it.
6. **The owner explicitly agreed this minimal version DOES still warrant a small, additive note in
   `docs/StateFlow_Whitepaper_v1_0.md` §12** (Planner Contract) — unlike the rejected `need_more`
   design, this doesn't change the decision contract's shape, so the doc footprint is one
   paragraph, not a section redesign. Pre-approved for Session 19 to touch, scoped tightly.
7. Confirmed future directions, explicitly NOT part of this phase: a planner implementation could
   later choose to actively call `GET /runs/{run_id}` itself using the pointer's `run_id` (today's
   design doesn't block this, it just doesn't require any planner to do it — the reference
   `demo/planner/llm_adapter.py` is a single-shot LLM call today, not agentic, per direct code
   inspection during the planning conversation); a future phase might let an agent operate across
   multiple run_ids directly. Neither is scoped work — noted here only so a future planning
   conversation doesn't need to re-derive that these were considered and deliberately deferred.

**Registry #4 (in-process orphan sweeper):** Scope precisely confirmed against
`internal/orchestrator/loop.go:100-101`'s own doc comment ("Run returns a non-nil error only for
genuine faults: a store read/write failure, ctx cancellation, or an internal invariant
violation"). This is **not** about the whole orchestrator process crashing (that's still handled,
unchanged, by the existing once-at-startup `RecoverRuns` call) — it's about a *single run's driving
goroutine* dying while the orchestrator process itself stays alive (most commonly: a transient
Postgres outage). Today nothing relaunches that goroutine until a manual restart. The fix: run the
same orphan-claim scan periodically (not just once at startup) for the life of the process.

**Registry #5 (LLM-aware rate limiting):** Owner confirmed the effective retry delay should be
`max(worker's reported retry_after_seconds, system default 5s)` — respect the worker's requested
backoff as a floor, never go below the system default.

**Registry #6 (config-driven assembly):** Owner's explicit standing principle, to apply here and
to any future configurability work: **"configurable but come with a widely acceptable default
setting"** — every new environment variable must default to reproducing today's exact hardcoded
behavior; a fresh clone with zero new config must behave identically to before the session. Do not
invent configurability for choices that don't have a second option yet (e.g. there is exactly one
`StateStore` implementation — don't build a plugin-selection mechanism for it).

**Registry #7 (migration tooling):** Owner deferred to the recommendation: `golang-migrate`
(most widely used in the Go ecosystem, has both a CLI and a Go library — the library matters here
since the distroless runtime image has no shell to run a CLI in).

---

## SESSION 18 — In-process orphan sweeper (registry #4)

**Build:** extend the existing once-at-startup `RecoverRuns` scan into a periodic background sweep
that runs for the life of the process, so a run whose driving goroutine died mid-flight (see
"Design decisions log" above for the precise, owner-confirmed scope) gets picked back up without
requiring a manual restart.

**Scope:** `internal/orchestrator/` (new sweeper logic — a new file, e.g. `sweeper.go`, is
recommended; factor it to share the existing combination-table/orphan-claim logic with
`RecoverRuns` rather than duplicating it), `cmd/stateflow/main.go` (start the sweeper goroutine
after the initial `RecoverRuns` call completes; wire a clean shutdown path).

**Instructions:**
- Reuse `RecoverRuns`'s existing scan logic (`ListRunningRuns` → for each run, if `LoadFrontier`
  shows a `RUNNING` last step with no live goroutine currently tracking it, treat it exactly like
  startup recovery does — same TX3 orphan-claim, same budget check, same TX4 re-dispatch). Do not
  reimplement the combination-table logic a second time.
- Interval: default 30s, fixed for this session (not user-configurable yet — don't preempt Session
  21's config-driven-assembly scope; a configurable interval can be added there later if genuinely
  needed).
- The sweeper must not re-claim a run that already has a live goroutine actively driving it. This
  requires new in-memory state: a registry of run_ids with a currently-live goroutine, populated
  when `startLoop` launches one and cleared when it exits. This is process-local, in-memory state
  — consistent with the MVP's single-orchestrator-instance concurrency invariant (whitepaper's
  "one run has exactly one loop goroutine" rule); it does not need to survive a restart (that's
  what `ListRunningRuns` + `RecoverRuns` are for).
- Must not change what "claiming an orphan" means or how TX3/TX4 work — only when/how often the
  scan for one happens. The single-writer rule and CAS semantics are unaffected.
- Mandatory tests: (a) simulate a run whose goroutine "died" (no live-registry entry) while
  `run=RUNNING, last_step=RUNNING` — assert the next sweep tick claims the orphan and re-dispatches
  without any process restart; (b) a run WITH a live goroutine actively driving it must NOT be
  touched by a concurrent sweep tick — assert no duplicate orphan-claim / double-dispatch race.

**Completion condition:** `TEST_DATABASE_URL=... go test -p 1 -race ./...` green including the new
tests (`-race` matters here — concurrent goroutines touching shared state is exactly where a subtle
data race could hide). Live demo: kill Postgres briefly while a run is mid-flight, WITHOUT
restarting the orchestrator process, and confirm the run recovers within one sweep interval once
Postgres returns — paste the timeline/logs proving no manual restart was needed.

---

## SESSION 19 — Bounded history payload (registry #1)

**Build:** implement the two-tier size guard from the "Design decisions log" above — per-entry 2KB
cap with a pointer replacement, plus a 50KB total-payload cap allocated most-recent-first. No new
`StepDecision` verdict, no round-trips, no protocol change of any kind.

**Scope:** `internal/orchestrator/loop.go` (wherever `RunState`/`History` is built before calling
`planner.Decide` — locate the exact function first, don't assume), `internal/core/interfaces.go`
(correct `HistoryEntry.Output`'s doc comment, which currently overclaims it's always present),
`docs/StateFlow_Whitepaper_v1_0.md` §12 (Planner Contract — owner pre-approved a small, additive
paragraph describing this behavior; do NOT restructure or redesign that section, and do not touch
any other whitepaper section). Check `docs/StateFlow_Rules_Consolidation_v3_EN.md` for the same
"always present" assertion — if found, make the equivalent minimal correction; if it's more than a
one-paragraph change, STOP and report instead of proceeding. Do NOT touch `internal/api/server.go`
— `GET /runs/{run_id}` needs zero changes, it already returns full per-step output.

**Instructions:**
- Per-entry cap: 2048 bytes on the marshaled JSON size of a single `HistoryEntry.Output`. Over
  that, replace `Output` with a pointer object — exact shape is the implementer's call within this
  spirit: `{"_truncated": true, "size_bytes": <original marshaled size>, "note": "fetch full
  output via GET /runs/{run_id}"}` (field names may be refined, but must carry the same three
  facts: that it's truncated, its real size, and how to get the full value).
- Total cap: 51200 bytes (50KB) on the cumulative marshaled size of the whole `History` array
  *after* per-entry capping. Walk the history from the most recent completed step backward,
  allocating budget; once a step wouldn't fit in the remaining budget, that step (and every older
  one) gets reduced to `name`+`status` only — omit `Output` entirely, not even the pointer.
- Final marshaled array order is unchanged: `seq` ascending, per the whitepaper's existing binding
  rule — the recency walk is only for computing per-entry detail level, not for reordering.
- Nothing is persisted differently — Postgres keeps the full, untruncated output exactly as today;
  this only changes what gets marshaled into the wire payload sent to the planner on each `Decide`
  call.
- Mandatory tests: (a) an entry under both caps passes through byte-for-byte unchanged (regression
  check — the common case must be untouched); (b) an entry over the 2KB per-entry cap gets the
  pointer object with the correct `size_bytes`; (c) a run with enough steps that cumulative size
  exceeds the 50KB total cap — assert the most recent N steps keep their output (per-entry-capped
  if needed) and the older ones have no `Output` field at all; (d) seq-ascending wire order holds
  regardless of which entries got cut.

**Completion condition:** `TEST_DATABASE_URL=... go test -p 1 ./...` green including the new tests.
Live demo: inject an artificially large step output (a throwaway worker returning a >2KB JSON
blob) and capture the *actual* HTTP body the fake/test planner received, showing the pointer
object in place of the large value — paste the real captured payload, not a description of it.

---

## SESSION 20 — LLM-aware rate limiting (registry #5)

**Build:** honor a worker's `retry_after_seconds` (accepted on `/tasks/fail` today but ignored —
CLAUDE.md registry item 5) as a floor on the retry delay: effective delay =
`max(retry_after_seconds, system default 5s)`.

**Scope:** `internal/orchestrator/policy.go` (or wherever the retry-delay decision is actually
made — confirm by reading the current code path, don't assume `FixedCountPolicy` is untouched),
`internal/api/server.go` (already parses `retry_after_seconds` off `/tasks/fail` — trace where
that value currently goes, likely nowhere yet), `internal/core/interfaces.go` only if
`core.Result`/`core.ResultFailure` genuinely needs a new field to carry the value through to where
the delay decision happens.

**Instructions:**
- Effective retry delay = `max(worker's reported retry_after_seconds, 5s system default)`. If the
  worker didn't supply `retry_after_seconds` (it's optional), behavior is unchanged (5s default).
- This only affects the worker-side retry delay between a failed attempt and the next TX4
  re-dispatch — unrelated to the planner budget or to Session 19's history-truncation work.
- Read the actual current code path from `/tasks/fail`'s request parsing through to wherever the
  retry-delay sleep happens before deciding the exact plumbing — don't guess the shape of the
  threading mechanism in advance.
- Mandatory tests: (a) worker reports `retry_after_seconds: 30`, system default 5s → next
  dispatch delayed ~30s, not 5s; (b) worker reports `retry_after_seconds: 1` (below system
  default) → still waits the full 5s system default, not 1s (max, never less than the floor); (c)
  worker omits `retry_after_seconds` → unchanged 5s behavior (regression check).

**Completion condition:** `TEST_DATABASE_URL=... go test -p 1 ./...` green including the new tests.

---

## SESSION 21 — Config-driven assembly (registry #6)

**Build:** make `cmd/stateflow/main.go`'s hardcoded wiring (retry policy parameters, sweeper
interval if Session 18 has landed, any other genuinely-tunable-today parameter) configurable via
environment variables — see the "Design decisions log" above for the binding "configurable but
with a widely-accepted default" principle.

**Scope:** `cmd/stateflow/main.go`; a new small `internal/config/` package only if the parsing
logic genuinely warrants its own file — keep it minimal, this system has exactly one `StateStore`
implementation and one retry policy today, don't build a generic plugin-selection framework for
choices that don't exist yet. `README.md` for documenting any new environment variable (small,
additive table — do not restructure Session 11's rewrite).

**Instructions:**
- Do NOT invent configurability for anything without a genuine second option today (e.g. no
  store-backend selection — there's only Postgres).
- Every new environment variable's default must reproduce EXACTLY today's hardcoded behavior — a
  fresh `docker compose up -d --build` with zero new env vars set must be byte-for-byte behaviorally
  identical to pre-session. This is the core of the completion condition.
- Candidates worth making configurable (confirm against whatever has actually landed by the time
  this session runs — Sessions 18/20 may have added new tunables): retry policy's
  `MaxRetries`/`Delay`, the orphan sweeper's interval (if Session 18 landed first).
  `LISTEN_ADDR`/`DATABASE_URL` are already env-configurable — leave those as-is, don't touch.
- Mandatory checks: (a) zero new env vars set — full `go test -p 1 ./...` and the demo scripts
  unchanged from pre-session behavior; (b) at least one new env var set to a non-default value —
  confirm the override actually takes effect (e.g. a different retry delay, observed via timing in
  a live test), not just accepted and silently ignored.

**Completion condition:** clean-clone `docker compose up -d --build` with no new env vars — full
demo/test suite unchanged, paste evidence. A second run with at least one overridden env var —
confirm the override is honored, paste evidence.

---

## SESSION 22 — Migration tooling (registry #7)

**Build:** replace the init-only single-SQL-file schema application with `golang-migrate`-based
versioned migrations.

**Scope:** `migrations/` (restructure `001_initial.sql` into `golang-migrate`'s numbered
`up`/`down` pairing), `docker-compose.yml` (the Postgres service currently applies the SQL file via
an init-script mount — this needs to change), `cmd/stateflow/main.go` (apply migrations via
`golang-migrate`'s Go library — NOT by shelling out to its CLI, since the distroless runtime image
has no shell, per Session 10's `/healthz` work preserving that constraint), `README.md` and
`CLAUDE.md`'s "Pre-release freedom" §.

**Instructions:**
- Convert the existing single schema file into an initial migration, preserving its content
  exactly — a mechanical split into an `up` file (existing content, unchanged) and a `down` file
  (a clean reverse — `DROP TABLE`s etc.), not a schema redesign.
- Apply migrations from `cmd/stateflow/main.go`'s startup, before `RecoverRuns` runs, via
  `golang-migrate`'s Go library.
- **This is the one item in Phase 2 that changes a documented project convention.** `CLAUDE.md`'s
  "Pre-release freedom" section currently says: "schema changes rewrite `migrations/001_initial.sql`
  in place; reset with `docker compose down -v`; no migration tooling." That sentence becomes false
  once this session lands. **STOP and report the exact proposed `CLAUDE.md` wording change to the
  owner before finalizing it** — do not silently retire a documented convention, even though this
  whole phase was owner-directed; the specific wording is still worth a sign-off pass, per this
  project's standing "authoritative docs need explicit confirmation" rule.
- Mandatory tests: (a) a fresh `docker compose down -v && up -d --build` applies the migration and
  the resulting schema matches today's exactly — diff `\d` output against the pre-session schema,
  table by table; (b) confirm the existing `resetSchema` test helper (used by every `_test.go`
  package gated on `TEST_DATABASE_URL`) still works under the new migration-based application, or
  update it if the underlying mechanism changed and explain exactly what changed.

**Completion condition:** clean-clone `docker compose down -v && up -d --build`, confirm schema
matches pre-session state exactly (paste the `\d` diff — empty diff is the bar). `TEST_DATABASE_URL=...
go test -p 1 ./...` fully green.

---

## Cross-session invariant checklist (re-verify at the end of every session)

- [ ] No code path dispatches before TX1 commits; none asks the planner before TX2 commits.
- [ ] `attempt_count` is written only by TX3 (++) and TX5 (=0).
- [ ] Every attempt terminal write goes through CAS (`AND status='RUNNING'`).
- [ ] No DECIDED/FAILED step-state strings remain anywhere (`grep -rn "DECIDED\|'FAILED'" --include="*.go" --include="*.sql" --include="*.py"` — attempts' FAILED is the only legitimate hit).
- [ ] `grep -rn "decided_at\|dispatched_at\|attempt_number\|replay_round\|ResetToDecided" --include="*.go" --include="*.sql" --include="*.py"` returns nothing — the renames and removals are complete.
- [ ] Wire-format tests pass: sync = bare input + ID headers (byte-for-byte body assertion), async = envelope, planner history statuses UPPERCASE in seq order.
- [ ] Timestamps come from DB `now()`; no `time.Now()` is persisted for ordering.
- [ ] The demo never relies on behavior listed in the Temporary Design Registry.
