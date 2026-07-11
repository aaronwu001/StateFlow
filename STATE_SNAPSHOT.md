# STATE_SNAPSHOT

## 1. 進度指針
- 剛完成 Session：`8.5 —— Async malformed-output detection for /tasks/complete (inserted after Session 8 audit; owner decided to fix now rather than defer)`
- 下一個 Session：`None currently planned. Session 8.5 was owner-inserted specifically to close the one open item from Session 8's final audit. The refactor's numbered plan (0–8) was already complete; 8.5 is a targeted fix, not a new phase. No Session 9 exists in StateFlow_v1_ClaudeCode_Prompts.md as of this snapshot.`
- 本次 commit SHA：`30a2d34c4793be8f3af8ab1b9d5e105dd29f1742` (code commit — this snapshot commit lands on top of it as a second, snapshot-only commit, matching the Session 8 precedent of `956dc6c` code + `ab2c4f4` snapshot)
- 分支：`main`

## 2. 驗證證據（verbatim）

Environment: Windows host; the Bash tool itself is Git Bash (MSYS), NOT WSL — `go`/`docker` are not on its PATH. All Go/Docker/Python commands were run via `wsl.exe -d Ubuntu -- bash ...` against a live `docker compose up -d --build` stack started from a `docker compose down -v` state (fresh Postgres volume), per this session's documented environment quirks. **New environment finding this session** (see §4): command substitution (`$(...)`) silently evaluates to empty when embedded directly in a `wsl.exe -d Ubuntu -- bash -lc '...'` one-liner invoked from this Bash tool — worked around by writing the command to a `.sh` file (via the Write tool, so no shell-quoting layer touches it) and executing that file with `MSYS_NO_PATHCONV=1 wsl.exe -d Ubuntu -- bash /absolute/path/script.sh`.

### 2a. 完成條件指令與輸出

**1. Build / vet / gofmt**
```
$ docker run --rm -v "$(pwd):/src" -w /src -e GOFLAGS=-buildvcs=false \
    golang:1.25 sh -c "go build ./... && echo BUILD_OK && go vet ./... && echo VET_OK && gofmt -l ."
BUILD_OK
VET_OK
internal/planner/static_test.go
```
(`gofmt -l` flags exactly the one pre-existing file already flagged and left untouched since Session 6.5 — unrelated to this session; `git status --porcelain internal/` before this session's edits confirmed it was already the sole pre-existing gofmt offender.)

**2. Clean-clone compose build** (`docker compose down -v` on both the base and demo overlay, then `docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build`): all 8 containers reached `Up ... (healthy)` (or `Up` for `stateflow` itself, which has no built-in healthcheck — Temporary Design Registry item #8), confirmed via `docker compose ... ps`.

**3. `TEST_DATABASE_URL=... go test -p 1 -count=1 ./...`** (run against the live compose stack, container network `stateflow_default`):
```
$ docker run --rm --network stateflow_default -v "$(pwd):/src" -w /src \
    -e GOFLAGS=-buildvcs=false \
    -e TEST_DATABASE_URL="postgres://stateflow:stateflow@postgres:5432/stateflow?sslmode=disable" \
    golang:1.25 go test -p 1 -count=1 ./...
?   	github.com/aaronwu000/stateflow/cmd/stateflow	[no test files]
ok  	github.com/aaronwu000/stateflow/internal/api	2.274s
?   	github.com/aaronwu000/stateflow/internal/core	[no test files]
ok  	github.com/aaronwu000/stateflow/internal/orchestrator	1.573s
ok  	github.com/aaronwu000/stateflow/internal/planner	0.221s
ok  	github.com/aaronwu000/stateflow/internal/store	1.465s
ok  	github.com/aaronwu000/stateflow/internal/transport	0.988s
```
6/6 packages `ok` (2 with no test files). The three new tests are inside `internal/api`; verbose run of that package alone:
```
=== RUN   TestAPI_TaskComplete_AsyncMalformedOutput_Absent
    server_test.go:921: PASS — POST /tasks/complete with output absent → HTTP 200
    server_test.go:927: PASS — attempt 72f4ca53-115c-4a64-be49-0b033743126a failure_reason = malformed
    server_test.go:943: PASS — step.status=RUNNING attempt_count=1 after one malformed(absent-output) failure
--- PASS: TestAPI_TaskComplete_AsyncMalformedOutput_Absent (0.23s)
=== RUN   TestAPI_TaskComplete_AsyncMalformedOutput_Null
    server_test.go:968: PASS — POST /tasks/complete with output=null → HTTP 200
    server_test.go:974: PASS — run run-ab8b3061-ccef-48bc-b4da-7c996c4f7c94 → DLQ after one malformed(null-output) failure at retry_limit=1
    server_test.go:980: PASS — attempt 0d84761b-482e-4fea-9a95-81b70e79563f failure_reason = malformed
    server_test.go:992: PASS — dlq entry reason = worker_retry_exhausted
    server_test.go:1000: PASS — no extra dispatch envelope after the budget-exhausting malformed failure
--- PASS: TestAPI_TaskComplete_AsyncMalformedOutput_Null (0.44s)
=== RUN   TestAPI_TaskComplete_AsyncRealOutput_Regression
    server_test.go:1029: PASS — run run-95513eca-16a5-49b3-9818-6935c9ae4dca → DONE with real async output (no regression)
    server_test.go:1048: PASS — step output = map[count:7 processed:true] (real output preserved verbatim)
--- PASS: TestAPI_TaskComplete_AsyncRealOutput_Regression (0.26s)
PASS
ok  	github.com/aaronwu000/stateflow/internal/api	2.399s
```
All pre-existing `internal/api` tests (`TestAPI_EndToEnd_Sync`, `TestAPI_Callback_Dedup`, `TestAPI_GetRun_NotFound`, `TestAPI_EndToEnd_HTTPPlanner` incl. its `planner_verdict_wrong_case_is_rejected` subtest, `TestAPI_DLQ_ReplayWorkerSide`, `TestAPI_DLQ_ReplayPlannerSide`) also PASS unchanged in the same run — no regression.

**4. `python3 demo/crash_demo.py`** (trimmed to load-bearing lines):
```
[NER]  ✅ Extraction done — 3 entities found
  🔄 RESTARTING ORCHESTRATOR  —  RecoverRuns fires at startup
[NER]  ⚡ Already processed step_id=run-466484e5-...:ner
[NER]     Re-sending callback with NEW attempt_id=ffa8ff59... (no re-processing)
[NER]  📤 Callback delivered — attempt_id=ffa8ff59...  HTTP 200
[SUMMARIZE] ✅ Summary ready — 17 words
2026/07/11 01:51:09 INFO [RECOVERY] run completed run_id=run-466484e5-...
  ✅ NER step's attempt history shows exactly ONE attempt with failure_reason='orphaned'
  ✅ NER step's Barrier-1 record (created_at + decision) is byte-identical before and after the crash
  ✅ NER worker's actual extraction work ran exactly ONCE
  Run status : DONE
    [DONE  ] ocr
    [DONE  ] ner
    [DONE  ] summarize
  ✅ Crash-recovery demo successful — the run completed without re-running done steps.
```

**5. `./demo/run_demo.sh` scenarios 1–3** (in-place throwaway copy technique — see §4 for the exact mechanics used this session; `git status --porcelain demo/` confirmed clean immediately after, zero mode-bit or content residue):

Scenario 1 — Happy Path:
```
   Run: run-b8046872-6253-4def-b8b6-ff0982d892c9
   Status: DONE
   [DONE    ] ✓ step1
   [DONE    ] ✓ step2
   ℹ  LLM adapter was called 3 time(s)  (expected: 3)
   ✓  PASS — LLM-driven pipeline completed; adapter called 3×
```

Scenario 2 — Worker Crash & DLQ Replay:
```
   Run: run-e6a70ab7-42fa-413f-a56a-e6bfc63e680b
   Status: DLQ
   [DONE    ] ✓ step1
   [DLQ     ] ✗ step2
   DLQ Entries:
   ID=1  ... reason=worker_retry_exhausted  step=...:step2
   ✓  OK — DLQ reason=worker_retry_exhausted, context carries per-attempt reason(s): ['timeout']
   ℹ  step1 invocation count before replay: 1
   Run: run-e6a70ab7-42fa-413f-a56a-e6bfc63e680b
   Status: DONE
   [DONE    ] ✓ step1
   [DONE    ] ✓ step2
   ℹ  step1 final invocation count: 1  (must be 1 — not re-run after replay)
   ✓  PASS — Run completed after DLQ replay; step1 not re-run (called 1×)
```

Scenario 3 — Orchestrator Crash & Recovery:
```
   ⚠  KILLING orchestrator with 'docker compose kill' (SIGKILL)
   ⚠  Orchestrator dead. step1 is RUNNING in DB (Barrier 1 fired; Barrier 2 not yet).
   step_name | status  | dispatched
   -----------+---------+------------
   step1     | RUNNING | t
   Recovery log:
   ... INFO [RECOVERY] found in-progress runs count=1
   Run: run-b2c136ea-5dc8-400e-b1c8-b9839f2715fc
   Status: DONE
   [DONE    ] ✓ step1
   [DONE    ] ✓ step2
   ℹ  LLM adapter call count: 3
   ✓  PASS — Recovery complete; adapter called 3× (≤3 — no extra re-decision)
```

### 2b. 測試計數（照實填，來源即上面輸出）
- 套件測試：`6/6` packages `ok` (2 with no test files); 失敗：無。`internal/api` package test count went from 6 top-level `func Test...` to 9 (3 new: `TestAPI_TaskComplete_AsyncMalformedOutput_Absent`, `TestAPI_TaskComplete_AsyncMalformedOutput_Null`, `TestAPI_TaskComplete_AsyncRealOutput_Regression`), all passing; zero pre-existing tests modified or deleted.
- 舊模型測試依計畫刪除/預期失敗者：無 — this session added tests, it did not delete or weaken any existing test.
- Demo/acceptance: `crash_demo.py` — PASS; `run_demo.sh` scenarios 1/2/3 — all PASS; `crash_recovery_test.py` — PASS; `EXPECT_X=2 dlq_replay_test.py` — PASS.

### 2c. Owner oracle（independently re-run against a live stack this session itself rebuilt）
```
$ ADVERTISE_HOST=172.31.72.20 ORCH_CONTAINER=stateflow-stateflow-1 \
  TEST_DATABASE_URL="postgres://stateflow:stateflow@localhost:5432/stateflow?sslmode=disable" \
  API_BASE=http://localhost:8080 python3 test/acceptance/crash_recovery_test.py
[setup] run_id=run-2744fff6-f861-4c5b-bbd9-d9ab0413261e
[catch] done=s1 running=s2 -> kill stateflow-stateflow-1
[restart] waiting for terminal state
[final] run status=DONE
  ok: completed step s1 untouched across crash (no re-run)
  ok: completed step s1 has exactly one DONE attempt
  ok: mid-flight step s2 has an orphaned attempt
  ok: mid-flight step s2 reached DONE after re-dispatch
  ok: run reached DONE
  ok: every FAILED attempt carries a reason
PASS: crash_recovery_test

$ ADVERTISE_HOST=172.31.72.20 ORCH_CONTAINER=stateflow-stateflow-1 EXPECT_X=2 \
  TEST_DATABASE_URL="postgres://stateflow:stateflow@localhost:5432/stateflow?sslmode=disable" \
  API_BASE=http://localhost:8080 python3 test/acceptance/dlq_replay_test.py
[setup] run_id=run-5c7937ed-8b05-407e-908c-9666e9e849b3 expecting X=2
  ok: run reached DLQ
  ok: step in DLQ with attempt_count=2
  ok: exactly 2 FAILED attempts
  ok: one DLQ row, reason=worker_retry_exhausted, context populated
  ok: replay reset attempt_count to 0 and returned step to RUNNING (TX5)
PASS: dlq_replay_test
```
Both oracles PASS end-to-end. Note: the first attempt at running these (with `ADVERTISE_HOST=$(hostname -I | awk '{print $1}')` embedded directly in a `wsl.exe ... bash -lc '...'` one-liner) silently resolved `ADVERTISE_HOST` to an EMPTY string (not an error — `os.environ.get` returns the empty string, not the default, when a var is set-but-empty), which sent the fake HTTP planner's URL to the orchestrator as `http://:7102/` and DLQ'd the run instantly with `planner_unreachable` — a pure environment/tooling artifact, not a code defect. Diagnosed via direct DB inspection (`dead_letter_queue.context` showed the literal `http://:7102/` URL) and fixed by moving the substitution into a real script file (see §4). Recorded here so a future session does not waste time re-diagnosing the same nested-shell quirk.

## 3. 動過的檔 / 故意沒碰的檔

```
$ git status --porcelain internal/ cmd/ migrations/ docs/ test/acceptance/ demo/
 M internal/api/server.go
 M internal/api/server_test.go
 M internal/core/interfaces.go
```
(post-commit `30a2d34`; this is the diff that commit contains, shown here pre-commit for the record — after the commit `git status --porcelain` for these paths is empty.)

Files changed (all in this session's declared scope):
- `internal/api/server.go` — `handleTaskComplete` now classifies an async callback's `output` (absent from the JSON body, or present as the JSON literal `null`) as `failed(malformed)` before handing the `core.Result` to `DeliverCallback`; added `isAsyncOutputMalformed` helper; expanded the handler's doc comment to describe the new classification step; added `"bytes"` import.
- `internal/api/server_test.go` — added `asyncEnvelope` type, `startAsyncStepRun`/`pollAttemptFailed` helpers, and three new tests (`TestAPI_TaskComplete_AsyncMalformedOutput_Absent`, `_Null`, `TestAPI_TaskComplete_AsyncRealOutput_Regression`). Zero existing tests modified.
- `internal/core/interfaces.go` — `StepSpec.OutputField`'s doc comment rewritten to describe both mechanisms (sync `OutputField` subtree-presence; async absent/null) instead of asserting malformed-via-`OutputField` is sync only. No field added, removed, renamed, or retyped — `StepSpec`'s shape is byte-identical to before this session.

Files deliberately NOT touched (checked, not contradicted):
- `docs/USER_MANUAL.md` — read in full for "malformed"/"unparseable"/"async" mentions (via a research subagent); found no sentence asserting async lacks malformed detection or that async always succeeds given valid ids. The one `output_field` mention (line 100, "For sync workers: extract one field...") remains accurate — `output_field` genuinely is sync-only; only the OLD, now-corrected `OutputField` Go doc comment overclaimed that malformed-detection-in-general was sync-only. No edit made, per the session's own "check first, edit only if genuinely contradicted" instruction.
- `docs/StateFlow_Whitepaper_v1_0.md`, `docs/StateFlow_Rules_Consolidation_v3_EN.md`, `CLAUDE.md` — none contradicted by this session's fix; not touched, as instructed.
- `internal/transport/sync.go` — sync's `extractOutput`/`OutputField` logic is untouched, exactly as instructed.
- `internal/transport/async.go` — `DeliverCallback` is untouched; it still performs zero classification, exactly as before. The classification decision is made entirely in `internal/api/server.go`, upstream of the transport layer, per this session's scope.

One incidental artifact, caught and cleaned up before finishing: the throwaway in-place copy of `demo/run_demo.sh` (used to drive scenarios 1–3 non-interactively) was deleted immediately after use; `git status --porcelain demo/` confirmed clean (no mode-bit residue this time, unlike the Session 8 incident).

## 4. 與 session 指示的偏離點

1. **New environment-tooling finding, not a code deviation**: command substitution (`$(...)`) embedded directly inside a `wsl.exe -d Ubuntu -- bash -lc '...'` one-liner invoked from this session's Bash tool silently evaluates to an empty string instead of erroring — reproduced deterministically with even a trivial case (`Y=$(echo hi); echo "[$Y]"` → `[]`), while the same command run as a `> file` redirection, or as a standalone (non-substitution) statement, works correctly. This is almost certainly an artifact of how this specific Bash tool's underlying process (Git Bash / MSYS) pipes stdin/stdout to the nested `wsl.exe` process, not a WSL or bash bug per se. Workaround used: write the full command (including the `ADVERTISE_HOST=$(hostname -I | awk '{print $1}')` substitution) to a `.sh` file via the Write tool — which bypasses all shell-quoting layers entirely — then execute that file directly with `MSYS_NO_PATHCONV=1 wsl.exe -d Ubuntu -- bash /absolute/posix/path/script.sh` (the `MSYS_NO_PATHCONV=1` prefix was also required — without it, Git Bash rewrites the leading `/home/...` POSIX path into a bogus `C:/Program Files/Git/home/...` path before `wsl.exe` ever sees it, since `wsl.exe` is a native Windows executable and MSYS auto-converts POSIX-looking arguments passed to native executables). Both `.sh` files were deleted after use; `git status --porcelain` confirms no residue. Flagged here per "verify before reporting" so a future session doesn't lose time rediscovering this.
2. Wrote three new tests rather than extending an existing one, because no existing test in `internal/api/server_test.go` actually exercises a REAL async success/failure round-trip through a live `Loop.Run()` goroutine — the closest existing test, `TestAPI_Callback_Dedup`, plants DB rows directly without ever calling `POST /workflows/:id/runs`, so `AsyncTransport`'s registry never has a channel registered for that step, and `DeliverCallback` always hits its `unregistered step` no-op path regardless of correctness. This session's tests therefore had to drive a real dispatch (worker captures the loop's own envelope, test replies as the worker) to actually exercise the new classification logic — see `startAsyncStepRun` in `server_test.go`. This is not a deviation from the prompt's instructions (which asked for exactly this behavior to be tested) but is flagged because the prompt's phrasing ("confirm the existing happy-path async test... still passes unchanged") presupposed such a test already existed; none did, so `TestAPI_TaskComplete_AsyncRealOutput_Regression` was written fresh rather than merely re-run.
3. **The exact "malformed" classification rule chosen** (asked for in both the Build section and Question 4 below, stated once here for visibility): `isAsyncOutputMalformed(output json.RawMessage) bool { return len(output) == 0 || string(bytes.TrimSpace(output)) == "null" }`. I.e., `output` key absent from the JSON body, OR present but equal to the JSON literal `null` (after trimming incidental whitespace) → `failed(malformed)`. A present, syntactically valid `{}` or `[]` is deliberately left classified as `done` (NOT malformed) — see Question 4 for the full justification. This was a judgment call under genuine ambiguity in the whitepaper text, not a mechanical instruction from the prompt.

## 5. 本次定案的真實介面 / Schema

No schema change (none was in scope; none was needed). `core.StepSpec`'s shape is unchanged — only its `OutputField` doc comment was reworded. `core.Result`/`core.ResultFailure`/`core.FailureReason` are all unchanged — this session's fix works entirely by choosing WHICH pre-existing `core.Result` value to construct in `handleTaskComplete`, never by adding a new field, type, or Ledger transaction.

**New (unexported) function**: `isAsyncOutputMalformed(output json.RawMessage) bool` in `internal/api/server.go` — the async analogue of `internal/transport/sync.go`'s `extractOutput`. Not part of any public interface; a private classification helper local to the handler.

## 6. 未解問題（分類 —— 這欄最重要）
- 🟡 已停下、需裁示：**None this session that blocks completion** — the one 🟡 item carried in from Session 8 (the async-malformed gap itself) is now RESOLVED by this session's fix. See Question 7 below for the two open sub-questions this session's own design choices raise (both are genuinely ambiguous per the whitepaper text, not blockers — the minimum bar was implemented and verified; these are refinements for the owner to consider, not defects).
- 🔴 我自行假設後繼續：
  1. Interpreted the whitepaper's single phrase "valid ids but unparseable output" as covering exactly two cases — key absent, key present as JSON `null` — and NOT covering a present-but-empty `{}`/`[]` object/array. Justification: "unparseable" most naturally reads as a JSON-syntax/absence failure (mirroring sync's two prongs: invalid JSON body, or a *named* field absent), and `{}`/`[]` are neither — they are syntactically valid, semantically empty payloads a worker might legitimately intend ("no fields to report" is a valid business outcome, not necessarily a bug). This is the single most consequential judgment call in this session; see Question 7 for the case for the alternative.
  2. Used a research subagent (not manual reading) to verify `docs/USER_MANUAL.md` contains no assertion contradicting this session's fix, per the prompt's own instruction to "check first, edit only if genuinely contradicted." Treated the subagent's negative result (no contradicting sentence found) as sufficient grounds to leave the file untouched, rather than re-reading it manually end-to-end myself.
- 無其他：everything else in this session's instructions (the classification rule, the handler wiring, the three tests, the doc-comment update, all four completion-condition command categories) was directly executable and is reported in full above.

## 7. CONFIRM 值（unchanged this session）
- planner_config 內 HTTP planner URL 的欄位名：`url`（no action needed）
- retry limit X 在 planner_config 的欄位名：`retry_limit`（no action needed）
- `POST /workflows` / `POST /runs` 回傳的 id 欄位名：`workflow_id` / `run_id`（no action needed）
- workflow-level timeout override 的欄位名：`default_timeout_seconds`（unchanged）

**Open questions for the owner (Session 8.5):**
1. Should a present-but-empty `{}` or `[]` async output also be classified `malformed`? This session's answer is NO (see §6 🔴 1 for the reasoning), but the whitepaper's wording is genuinely ambiguous here — an argument for YES exists too: an empty object is exactly as useless to a downstream planner/step as a null one (`HistoryEntry.Output`'s doc comment says a DONE step's output is "always present," which is technically satisfied by `{}` but arguably not satisfied *in spirit*). If the owner wants `{}`/`[]` included, the fix is a one-line change to `isAsyncOutputMalformed` in `internal/api/server.go`.
2. This session's fix only classifies the OUTPUT content of an otherwise well-formed `/tasks/complete` callback. It does not, and was not asked to, touch the "malformed JSON body entirely" case (e.g. `POST /tasks/complete` with a body that isn't valid JSON at all) — that already returns 400 via the pre-existing `json.NewDecoder(...).Decode(&req)` error path, before `step_id`/`attempt_id` can even be read, matching whitepaper §7.1's "an async callback missing valid step_id/attempt_id gets a 400." Confirming this reading is correct: is a syntactically-broken JSON body meant to be indistinguishable from a missing-ids callback (both 400, zero effect), or should it also somehow reach `malformed` classification? The current code (unchanged by this session) treats it as the former. Not re-litigated this session since it was outside the stated scope (the prompt's "unparseable output" language is about the `output` field's own value, not body-level JSON validity), but flagged since it's adjacent enough to be worth an explicit owner sign-off.
3. Carried over from Session 8 (never explicitly re-raised since): should `defaultRetryLimit = 3` (`internal/orchestrator/loop.go`) be promoted from an implementation-only constant to a documented whitepaper default? Out of this session's scope; restated here only because Session 8 flagged it as "Session 8 is the last chance to surface it" before 8.5 existed — now that a 8.5 exists, it is surfaced again in case the owner wants to fold it into a future micro-session.

---

## 8. 流水帳（APPEND-ONLY —— 覆寫本檔時，這區只准往下加一行，永不刪改上面的行）
- Session 0 (no dedicated commit — read-only audit)：read-only 稽核完成，產出 test/coverage 盤點。
- Session 1 (`250daf2`)：rewrote `migrations/001_initial.sql` to the v1.0 three-state schema (whitepaper §14.1); schema-only, no TX ledger logic implemented.
<!-- Session 2 起在此行下方繼續 append -->
- Session 2 (`ac7bb85`)：rewrote `internal/core/interfaces.go` — closed status enums matching DB CHECK values byte-for-byte, `StateStore` interface with one method per Atomic Transaction Ledger entry (TX-W..TX9) plus reads, typed CAS outcomes (`ReportOutcome`/`FailureOutcome` per T1) so a superseded report is a normal return value, `FailureReason` unrepresentable outside `AttemptStatusFailed` via nested `ResultFailure` (T3), zero timestamp parameters anywhere (T2). `go build ./internal/core/...` passes; rest of module intentionally still broken (store/transport/planner reference old fields) exactly as expected per the session's own scope note.
- Session 2 follow-up (`6e6fe70`)：owner-directed fixes on the same file — added `StateStore.GetWorkflow` (closes the §12.1 planner-reconstruction gap), corrected `WorkflowDef.RetryLimit`'s doc to say it's nested in `PlannerConfig` under key `retry_limit` (not an independent column), documented `Frontier.PendingAttemptID`'s unconditional-claim-via-CAS semantics. `go build ./internal/core/...` and `go vet` still pass; gofmt clean.
- Session 3 (`1dc98f6`)：rewrote `internal/store/postgres.go` implementing `core.StateStore` in full — every Ledger TXn (TX-W..TX9) as one BEGIN...COMMIT, CAS-A on both attempt state and `steps.current_attempt_id` for every terminal attempt write, TX3's same-transaction DLQ blade, TX5's five-write reset. Rewrote `internal/store/postgres_test.go` (15 tests, deleting the old three DECIDED/FAILED-model tests) covering the five mandatory cases plus every remaining method. `go test ./internal/store/... -v` 15/15 green against live Postgres; `internal/store` no longer appears in `go build ./...`'s error list (only `internal/transport`/`internal/planner` remain broken, unchanged from Session 2, owned by Sessions 4/5).
- Session 4 (`b21047b`)：rewrote `internal/transport/sync.go` and `async.go` against the frozen `core.Result{Status,Output,Failure}` shape and the timeout doctrine (whitepaper §6): transports never resolve their own timeout, honor the incoming ctx deadline only, and return `(Result{}, err)` — never a fabricated `Reason=timeout` — when no valid response is obtained. Sync sends the bare input plus the two ID headers; async sends the `{step_id,attempt_id,input}` envelope and expects 202. `AsyncTransport.DeliverCallback` now validates a callback against the live `current_attempt_id` via a new read-only `AttemptStore` interface before routing it, closing a stale-attempt/registry-collision bug (registry keyed by StepID, not AttemptID). Rewrote both test files (15 tests total) covering wire formats byte-for-byte, the full outcome-mapping matrix, and async registry/store-validation hygiene; all green including under `-race`. `internal/transport` no longer appears in `go build ./...`'s error list.
- Session 5 (`8b0411b`)：rewrote `internal/orchestrator/loop.go` and `recovery.go` against the v1.0 TX ledger (whitepaper §5/§6/§7/§8) — single Run() entry point for both normal operation and one-time crash recovery, planner reconstructed from the workflow row every call, retry-budget-source defect fixed (RetryPolicy.Next fed from persisted `steps.attempt_count`, never a loop-local counter), planner budget (30s×3) moved from `HTTPPlanner`'s internal retry loop into the loop with per-attempt unreachable/malformed classification (new `planner.MalformedError`) and TX9 detail. Deleted the four-rule recovery code and `TestRecovery_FailedNoOutputReDispatched`; rewrote all three orchestrator test files against the new `StateStore`/`Loop` shapes, including the five mandatory tests (budget-boundary crash, recovery re-entrancy, crash-between-TX3-and-TX4, planner-asked-exactly-once, wire-casing). Also fixed an unrelated pre-existing `internal/planner/static.go` compile error blocking that package. `internal/orchestrator` (15/15) and `internal/planner` (14/14) fully green, including under `-race`; `internal/api`/`cmd/stateflow` remain non-compiling for reasons outside this session's scope (flagged, not fixed — see §6 of the Session 5 report).
- Session 6 (`3b20d5d`)：rewrote `internal/api/server.go` against `core.StateStore` (TX-W/TX0/TX5/TX6 through the store interface, not raw SQL) and a redesigned `GET /runs/{id}` response (run status, per-step seq/attempt_count/created_at/current-attempt summary, dlq_reason on DLQ — `decided_at` retired for good); fixed `cmd/stateflow/main.go`'s three known post-Session-4/5 breaks and simplified it (planner construction moved entirely into `Loop.Run`, so `main.go` no longer needs it); rewrote `internal/api/server_test.go` incl. an explicit wire-casing contract test (history UPPERCASE + planner-verdict-casing enforcement) and dedicated TX5/TX6 DLQ-replay integration tests. `go build ./...`/`go vet ./...` clean for the whole tree; `go test -p 1 ./...` fully green (repeated twice, no flakiness); `internal/api` green under `-race`; live container smoke test via `docker compose up -d --build` confirms the actual production binary starts, recovers, and serves traffic. Owner-oracle scripts (`test/acceptance/*.py`) could not be run to completion — blocked by this session's own sandbox denying the network workarounds needed for this Windows/WSL2/Docker-Desktop topology; flagged for the owner, not a code defect (see §6 🟡).
- Session 6.5 (`d1c3aa7`)：fixed the Session-6-audit-flagged TX5 worker-side replay bug — added `orchestrator.Loop.ResumeReplayedStep` (`internal/orchestrator/replay.go`, new file) which dispatches a TX5-freshly-created attempt directly instead of routing through `Run()`'s crash-recovery orphan-claim check (which was burning the just-reset retry budget before any worker was ever contacted, fatal at `retry_limit=1`); extracted `Run()`'s steady-state loop into a shared `driveSteadyState` helper reused by both entry points, unchanged behavior. Rewired `handleDLQReplay`'s worker-side branch (`internal/api/server.go`) to call the new entry point via a `Server.replayTransport` field built inside `New()` — no change to `New()`'s exported signature, so `cmd/stateflow/main.go` needed no edit. Added 2 new `internal/orchestrator` tests seeding the exact TX5-aftermath state directly; tightened `TestAPI_DLQ_ReplayWorkerSide` to `retry_limit=1` with exact-dispatch-count and zero-orphaned-attempt assertions. `go test -p 1 ./...` fully green against a live `docker compose up -d --build` stack (also green under `-race` with `-p 1`); `crash_recovery_test.py` PASSes unmodified. `EXPECT_X=2 dlq_replay_test.py`'s DLQ-exhaustion assertions all pass but its final transient-state-observation assertion fails deterministically due to a timing race in the frozen oracle's own `psql`-subprocess-per-poll design against a fake-worker route with zero configurable delay — extensively diagnosed with direct DB-timestamp and cross-topology evidence, not a defect in this session's fix (see that session's own report for the full analysis and options for the owner).
- Follow-up (`98f113d`, no session number, owner-approved oracle fix)：gave `/sync/fail` in `test/acceptance/fake_worker.py` a configurable delay (mirroring `/async/ok`/`/async/fail`) so the frozen acceptance oracle's `psql`-subprocess polling could observe the ~8ms TX5-reset-to-dispatch window flagged by Session 6.5 as the root cause of its one remaining oracle failure; also folded in an uncommitted `go.mod` correction (`gopkg.in/yaml.v3` promoted from indirect to direct, matching `internal/planner/static.go`'s direct import since Session 5). Confirmed both `crash_recovery_test.py` and `EXPECT_X=2 dlq_replay_test.py` PASS end-to-end against a live stack afterward.
- Session 7 (`d7925b7`)：aligned all demo scripts/docs and the project's two top-level governance docs to the v1.0 three-state model; zero `.go` files touched (confirmed empty `git status --porcelain internal/ cmd/ migrations/`). `demo/crash_demo.py` gained direct Postgres/log assertions after recovery: exactly one `failure_reason='orphaned'` attempt on the crashed step, the step's Barrier-1 record (`created_at`+`decision`) byte-identical pre/post-crash (proving the planner was asked exactly once), and the worker's real processing log line appearing exactly once (proving idempotency-cache absorption) — all passing against a live stack. `demo/run_demo.sh` gained a `check_dlq_worker_retry_exhausted` assertion in scenario 2 (DLQ reason + per-attempt failure reason present in `context`) and had its two remaining old-model status checks fixed (`FAILED`→`DLQ` for run/step terminal-state checks, since step status never has a `FAILED` value in v1.0); all 3 scenarios re-verified passing end-to-end. Purged the last old-model traces from `demo/playbook/PLAYBOOK.{en,zh}.md` (`attempt_number`/`dispatched_at` SQL → `status`/`failure_reason`/`created_at`; "DECIDED" wording → "decision persisted (TX1, Barrier 1)"). Switched `demo/workers/{ocr,summarize}_worker.py`'s idempotency cache to key primarily on the `X-StateFlow-Step-ID` header (input-hash fallback only), making `docs/USER_MANUAL.md`'s sync-idempotency recommendation concrete in running demo code. Fully rewrote `docs/USER_MANUAL.md`: corrected timeout defaults (60s system default, `default_timeout_seconds` workflow override, per-step override — no more "defaults to 30s" claims anywhere, including the planner-config table, which was also corrected to drop the no-longer-read `max_retries`/planner-local `timeout_seconds` keys and add the real `retry_limit`/`default_timeout_seconds` keys); replaced the single `planner_failed` DLQ reason with the real four-value set plus a full triage table (new §3, explicitly warning against blind-replaying `planner_declared_fail`); added a quantified concurrent-idempotency contract section (up to X concurrent duplicates, explicitly covering timeout-triggered re-dispatch racing a still-alive worker, not just crash re-dispatch); extended the superseded-callback section to cover a success report arriving after a timeout verdict; rewrote the dispatch-format/idempotency sections per whitepaper §13.1 (sync = bare input + `X-StateFlow-Step-ID`/`X-StateFlow-Attempt-ID` headers, header preferred over input-hash for the cache key). Replaced the transitional `CLAUDE.md` with a final version: full Quick Reference (3×3×3 states, the two barriers, the run×last_step combination table, the full TX ledger one-liner-each, the timeout doctrine, the orphan rule, the persisted-retry-budget rule, the CAS rule, the single-writer rule, wire formats), the whitepaper §18 Temporary Design Registry pulled forward verbatim as a named "Deferred / Explicitly Out of Scope" list, and a generalized (no-longer-"TRANSITIONAL") Development Discipline section; confirmed the `-p 1` test-package set is unchanged (`internal/api`, `internal/orchestrator`, `internal/store`, via a fresh grep). Fixed `README.md`'s two broken doc links (`DESIGN.md` → removed, content now lives in the whitepaper; `docs/StateFlow_Whitepaper_v0.8.md` → `docs/StateFlow_Whitepaper_v1_0.md`) and two old-model phrases ("Three recovery rules on restart" → "Combination-table recovery with orphan-claim + budget check on restart"; "Ghost Mode retry" → "in-process orphan sweeper", matching whitepaper §16/§18's explicit dissolution of Ghost Mode). All four completion-condition commands verified green against a live from-`--build` stack: `docker compose up -d --build`, `python demo/crash_demo.py` (PASS, new assertions included), `./demo/run_demo.sh` scenarios 1–3 (all PASS, scenario 2's new DLQ check included), `go test -p 1 ./...` (6/6 packages ok, 2 with no test files) — see §2 for verbatim output. `demo/configs/*.yaml` and `demo/planner/llm_adapter.py`'s DUMMY plan already had explicit, comfortably-exceeding `timeout_seconds` and needed no edits (verified by reading, not assumed).
- Session 8 (`ab2c4f4` — this snapshot commit adds only STATE_SNAPSHOT.md on top, zero code changed)：final end-to-end conformance audit. Built the full TX ledger conformance table (TX-W through TX9 + CAS-A, all traced statement-by-statement in `internal/store/postgres.go` against whitepaper §19/rules v3 §21 — all ten TXn methods confirmed single `BeginTx...Commit` blades with exactly their ledger-specified contents, or a single already-atomic statement where the ledger specifies one write). Ran the full global-sweep checklist: both mandated greps clean (only anti-regression doc-comments/test-guards, zero real hits), `attempt_count` confirmed written only by TX3(++)/TX5(=0), `time.Now()` confirmed never persisted (used only for in-memory `context.WithDeadline` inputs). Did the whitepaper §4–§11 doc-vs-code diff section by section, explicit "none found" per section except one genuine, reproducible divergence: whitepaper §4.2/§7.1 describe an async-worker `malformed` failure path ("valid ids but unparseable output") that no code implements — `FailureReasonMalformed` is producible only by the sync transport (`internal/transport/sync.go`); `internal/api/server.go`'s `handleTaskComplete` and `internal/transport/async.go`'s `DeliverCallback` unconditionally treat any well-formed `/tasks/complete` call as success regardless of `output`'s content. Reported to the owner (§6/§7 of this snapshot) with three framed options, NOT fixed, per this session's explicit "report it first, fix only with owner approval" mandate — this is the one finding of the entire refactor plan left open for owner decision. Full verification all green: clean-clone `docker compose down -v && up -d --build` (all 8 containers healthy), `python3 demo/crash_demo.py` PASS, `./demo/run_demo.sh` scenarios 1–3 all PASS, `go build`/`go vet`/`gofmt -l` (one pre-existing unrelated flag, unchanged since 6.5), `TEST_DATABASE_URL=... go test -p 1 ./...` 6/6 packages ok, and — independently re-run rather than trusted from the `98f113d` self-report — both owner acceptance oracles (`crash_recovery_test.py`, `EXPECT_X=2 dlq_replay_test.py`) PASS end-to-end against a stack this session itself rebuilt from a fresh Postgres volume. Zero `.go`/`.sql` files changed (confirmed empty `git status --porcelain internal/ cmd/ migrations/`); one incidental file-mode-only change on `demo/run_demo.sh` (100644→100755, a `cp`/`chmod` side effect of the non-interactive scenario-runner technique, zero content diff) was caught and reverted before being reported as "no changes". This is the final session of the numbered plan (0–8, plus 6.5 and two unnumbered follow-ups) — no Session 9 is planned; the one open item is the async-malformed owner decision above.
- Session 8.5 (`30a2d34`)：owner decided to implement the Session-8-flagged async-malformed gap now rather than defer it. Rewrote `internal/api/server.go`'s `handleTaskComplete`: a new unexported `isAsyncOutputMalformed(output json.RawMessage) bool` classifies an async `/tasks/complete` callback's `output` as `failed(malformed)` when the key is absent from the JSON body OR present but equal to the JSON literal `null` (a present `{}`/`[]` is deliberately left `done` — judgment call, documented and flagged as an open question, not silently guessed); the handler still ACKs HTTP 200 for a malformed report, consistent with the existing CAS/superseded-report contract. No new Ledger transaction: the fix works entirely by choosing which pre-existing `core.Result` (done vs. failed(malformed)) the handler constructs before calling `DeliverCallback` — TX2/TX3 route it exactly as they already did for every other Result. This closes the `migrations/001_initial.sql` schema-invariant violation Session 8 found ("output non-null → step DONE" was being violated by a null-output async success silently checkpointing DONE). Updated `core.StepSpec.OutputField`'s doc comment (`internal/core/interfaces.go`) to describe both the sync subtree-presence mechanism and the new async absent/null mechanism, replacing its old "sync only" overclaim; no field/type shape changed. Added 3 new tests to `internal/api/server_test.go` that drive a REAL async dispatch through a live `Loop.Run()` goroutine (capturing the loop's own dispatch envelope and replying to `/tasks/complete` as the worker would — the pre-existing `TestAPI_Callback_Dedup` plants DB rows directly and never has a live `Dispatch` registered, so it could not have exercised this logic): absent-output stays below budget (RUNNING, attempt_count=1, malformed), null-output at retry_limit=1 exhausts the budget (DLQ, worker_retry_exhausted, zero extra dispatch), real-output is unaffected (regression check). `docs/USER_MANUAL.md` was checked (via a research subagent) for any contradicting claim and found to have none — left untouched, per the session's own "check first, edit only if genuinely contradicted" instruction. Did NOT touch `internal/transport/sync.go`, `internal/transport/async.go`, `docs/StateFlow_Whitepaper_v1_0.md`, `docs/StateFlow_Rules_Consolidation_v3_EN.md`, or `CLAUDE.md` — none required a change and none were in scope. Full verification all green against a live `docker compose down -v && up -d --build` stack: `go build`/`go vet`/`gofmt -l` (same one pre-existing unrelated flag as every session since 6.5), `TEST_DATABASE_URL=... go test -p 1 ./...` 6/6 packages ok including the 3 new tests plus all pre-existing `internal/api` tests unchanged, `python3 demo/crash_demo.py` PASS, `./demo/run_demo.sh` scenarios 1–3 all PASS, and both owner acceptance oracles (`crash_recovery_test.py`, `EXPECT_X=2 dlq_replay_test.py`) PASS — the latter two required diagnosing and working around a nested-shell command-substitution quirk specific to invoking `wsl.exe` from this session's Bash tool (documented in §4/§2c above) that silently produced an empty `ADVERTISE_HOST`, unrelated to any StateFlow code. `git status --porcelain` confirms only the three declared in-scope files changed.
