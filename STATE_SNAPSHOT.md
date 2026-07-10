# STATE_SNAPSHOT

## 1. 進度指針
- 剛完成 Session：`7 —— Demos, CLAUDE.md, and full verification (demo/, docs/USER_MANUAL.md, CLAUDE.md, README.md pointers)`
- 下一個 Session：`8 —— Final audit & conformance report (TX ledger conformance table, global sweeps, doc-vs-code diff, full verification)`
- 本次 commit SHA（寫這份 snapshot當下的 HEAD，本 session 的變更尚未 commit）：`98f113d070ef8884f6eb2513563797c388718ca7`
- 分支：`main`

## 2. 驗證證據（verbatim）

Environment: Windows host, WSL2 Ubuntu distro has Docker/Go/Python. All four
completion-condition commands ran via `wsl.exe -d Ubuntu -- bash -lc '...'`
against a live `docker compose -f docker-compose.yml -f docker-compose.demo.yml
up -d --build` stack, per this session's own prompt's documented environment
quirks.

### 2a. 完成條件指令與輸出

**1. `docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build`**
```text
 Image stateflow-stateflow Built
 Image stateflow-step1 Built
 Image stateflow-step2 Built
 Image stateflow-summarize-worker Built
 Image stateflow-llm-adapter Built
 Image stateflow-ner-worker Built
 Image stateflow-ocr-worker Built
 Container stateflow-postgres-1 Running
 Container stateflow-summarize-worker-1 Created
 Container stateflow-llm-adapter-1 Created
 Container stateflow-ner-worker-1 Created
 Container stateflow-ocr-worker-1 Created
 Container stateflow-stateflow-1 Recreated
 Container stateflow-step1-1 Created
 Container stateflow-step2-1 Created
 Container stateflow-ner-worker-1 Started
 Container stateflow-summarize-worker-1 Started
 Container stateflow-llm-adapter-1 Started
 Container stateflow-ocr-worker-1 Started
 Container stateflow-step1-1 Started
 Container stateflow-step2-1 Started
 Container stateflow-postgres-1 Healthy
 Container stateflow-stateflow-1 Started
```
(Full raw output included a normal buildkit layer-export log for all 7 images; trimmed
here to the final per-service status lines — nothing was truncated that affects
success/failure.)

**2. `python demo/crash_demo.py`**
```text
════════════════════════════════════════════════════════════════
   StateFlow  —  Crash-Recovery Demo
════════════════════════════════════════════════════════════════
  Proves: kill orchestrator mid-run → restart → completed steps NOT re-run

  ✅ Images built
  ✅ Schema clean — 'stateflow' ready
  ✅ Workers ready  OCR:5001  NER:5002  Summarize:5003
  ✅ StateFlow (boot 1) ready on :8080  container=7d8f66d9bd2f

     workflow_id : wf-cdfad97a-908b-4892-8332-2220631b5636
     run_id      : run-34daf255-3c8b-4bed-b328-edeab7eeb7b9

[OCR] ✅ Extraction complete — 3 pages, confidence 0.98
  ✅ Step 1 (OCR, sync) DONE  ✓
     NER dispatched — it is sleeping 5s before sending its callback
[NER]  🏷️  Starting entity extraction
[NER]     step_id=run-34daf255-3c8b-4bed-b328-edeab7eeb7b9:ner  attempt_id=57cbeba2...
  💥 KILLING ORCHESTRATOR  —  container 7d8f66d9bd2f
  💥 NER's async callback channel dies with the process
  💥 DB still shows step 2 RUNNING (no output); step 3 never started
[NER]  ✅ Extraction done — 3 entities found
  🔄 RESTARTING ORCHESTRATOR  —  RecoverRuns fires at startup
[NER]  ⚡ Already processed step_id=run-34daf255-3c8b-4bed-b328-edeab7eeb7b9:ner
[NER]     Re-sending callback with NEW attempt_id=c4d7a7d3... (no re-processing)
[NER]  📤 Callback delivered — attempt_id=c4d7a7d3...  HTTP 200
[SUMMARIZE] ✍️  Generating summary from history: ['ocr', 'ner']
  ✅ StateFlow (boot 2 — recovery) ready on :8080  container=7d8f66d9bd2f
[SUMMARIZE] ✅ Summary ready — 17 words
2026/07/10 12:21:40 INFO [RECOVERY] run completed run_id=run-34daf255-3c8b-4bed-b328-edeab7eeb7b9

────────────────────────────────────────────────────────────────
  [VERIFY] Orphan claim, idempotency-cache absorption, planner-asked-once
────────────────────────────────────────────────────────────────
  ✅ NER step's attempt history shows exactly ONE attempt with failure_reason='orphaned' (recovery claimed the crash-killed attempt, per the combination table's run=RUNNING/last_step=RUNNING row)
  ✅ NER step's Barrier-1 record (created_at + decision) is byte-identical before and after the crash — the planner was asked for 'ner' exactly once, never re-asked on recovery
  ✅ NER worker's actual extraction work ran exactly ONCE — the post-recovery re-dispatch hit the worker's step_id-keyed idempotency cache instead of re-processing (no duplicate side effect)

════════════════════════════════════════════════════════════════
   DEMO COMPLETE
════════════════════════════════════════════════════════════════
  Run status : DONE
  Steps:
    [DONE  ] ocr
    [DONE  ] ner
    [DONE  ] summarize
  ✅ Crash-recovery demo successful — the run completed without re-running done steps.
```

**3. `./demo/run_demo.sh` scenarios 1–3** (driven non-interactively: `main "$@"`
temporarily stripped from a throwaway in-place copy so each `scenario_*`
function could be invoked directly with blank-line stdin satisfying the
script's own `pause()` prompts; the copy was deleted immediately after,
`git status --porcelain demo/` confirmed clean of it before committing)

Scenario 1 — Happy Path:
```text
   ✓  Images built
   ✓  Worker 'step2' ready ...   ✓  LLM adapter ready on :9000
   ✓  StateFlow ready on :8080
   ℹ  run_id = run-67e4e41b-d2b0-4608-9a77-350889168b78
   Run: run-67e4e41b-d2b0-4608-9a77-350889168b78
   Status: DONE
   [DONE    ] ✓ step1
   [DONE    ] ✓ step2
   ℹ  LLM adapter was called 3 time(s)  (expected: 3)
   ✓  PASS — LLM-driven pipeline completed; adapter called 3×
```

Scenario 2 — Worker Crash & DLQ Replay:
```text
   Run: run-67f6986c-c912-4111-9320-ab453f04f9f9
   Status: DLQ
   [DONE    ] ✓ step1
   [DLQ     ] ✗ step2
   DLQ Entries:
   ID=14  run_id=run-67f6986c-c912-4111-9320-ab453f04f9f9  reason=worker_retry_exhausted  step=run-67f6986c-c912-4111-9320-ab453f04f9f9:step2
   ✓  DLQ entry found: id=14
   ℹ  Checking DLQ reason and per-attempt context...
   ✓  OK — DLQ reason=worker_retry_exhausted, context carries per-attempt reason(s): ['timeout']
   ℹ  step1 invocation count before replay: 1
   ✓  Worker 'step2' ready on :5011  delay=1s
   ℹ  Replaying DLQ entry 14...
   Run: run-67f6986c-c912-4111-9320-ab453f04f9f9
   Status: DONE
   [DONE    ] ✓ step1
   [DONE    ] ✓ step2
   ℹ  step1 final invocation count: 1  (must be 1 — not re-run after replay)
   ✓  PASS — Run completed after DLQ replay; step1 not re-run (called 1×)
```

Scenario 3 — Orchestrator Crash & Recovery:
```text
   step_name | status  | dispatched
   -----------+---------+------------
   step1     | RUNNING | t
   (1 row)
   ✓  StateFlow ready on :8080
   Recovery log:
   2026/07/10 12:26:16 INFO [RECOVERY] found in-progress runs count=1
   Run: run-dabf5d04-9fec-4d9e-be36-ef3b9c5282fc
   Status: DONE
   [DONE    ] ✓ step1
   [DONE    ] ✓ step2
   ℹ  LLM adapter call count: 3
   ✓  PASS — Recovery complete; adapter called 3× (≤3 — no extra re-decision)
```

**4. `TEST_DATABASE_URL=... go test -p 1 ./...`**
```text
$ docker run --rm -v "$(pwd):/src" -w /src -e GOFLAGS=-buildvcs=false \
    golang:1.25 sh -c "go build ./... && echo BUILD_OK && go vet ./... && echo VET_OK && gofmt -l ."
BUILD_OK
VET_OK
internal/planner/static_test.go
```
(`gofmt -l` flags exactly the one pre-existing file already flagged and
explicitly left untouched by Session 6.5 — confirmed by this session's own
`git status --porcelain internal/` / `git diff --stat internal/` both being
empty; not this session's concern, see §3.)

```text
$ docker run --rm --network stateflow_default -v "$(pwd):/src" -w /src \
    -e GOFLAGS=-buildvcs=false \
    -e TEST_DATABASE_URL="postgres://stateflow:stateflow@postgres:5432/stateflow?sslmode=disable" \
    golang:1.25 go test -p 1 -count=1 ./...
?   	github.com/aaronwu000/stateflow/cmd/stateflow	[no test files]
ok  	github.com/aaronwu000/stateflow/internal/api	1.464s
?   	github.com/aaronwu000/stateflow/internal/core	[no test files]
ok  	github.com/aaronwu000/stateflow/internal/orchestrator	1.874s
ok  	github.com/aaronwu000/stateflow/internal/planner	0.223s
ok  	github.com/aaronwu000/stateflow/internal/store	4.286s
ok  	github.com/aaronwu000/stateflow/internal/transport	0.991s
```

### 2b. 測試計數（照實填，來源即上面輸出）
- 套件測試：`6/6` packages `ok` (2 with no test files); 失敗：無
- 舊模型測試依計畫刪除/預期失敗者：無 — this session touched zero `.go` files
  (docs/demo only), so no Go test was added, deleted, or modified.
- Demo/acceptance: `crash_demo.py` — PASS (including 3 new DB/log assertions
  this session added); `run_demo.sh` scenarios 1/2/3 — all PASS (scenario 2's
  new `worker_retry_exhausted` + per-attempt-reason-in-context assertion PASS).

### 2c. Owner oracle (informational — not this session's completion condition)
`test/acceptance/` is out of this session's scope and untouched (§3). Per the
Session 7 prompt, `crash_recovery_test.py` and `EXPECT_X=2 dlq_replay_test.py`
were already confirmed passing end-to-end as of HEAD `98f113d` (the "give
/sync/fail a configurable delay" follow-up) — stated context, not this
session's responsibility to re-verify. Re-running them was attempted as a
courtesy sanity check but failed at the very first HTTP call with
`ConnectionRefusedError` — not a regression, just this session's own
`run_demo.sh` scenario runs having `docker compose stop`'d the `stateflow`
service moments earlier via that script's cleanup trap (`stop_orchestrator`);
the stack was subsequently brought back up (`docker compose ... up -d`,
confirmed all containers `Running`) but the oracle re-run was not repeated
since it is explicitly out of scope. No code this session could have touched
(orchestrator/store/api are all untouched, §3) can have affected these
oracles' outcome.

## 3. 動過的檔 / 故意沒碰的檔

```text
$ git status --porcelain
 M CLAUDE.md
 M README.md
 M STATE_SNAPSHOT.md
 M demo/README.md
 M demo/README.zh.md
 M demo/crash_demo.py
 M demo/playbook/PLAYBOOK.en.md
 M demo/playbook/PLAYBOOK.zh.md
 M demo/run_demo.sh
 M demo/workers/ocr_worker.py
 M demo/workers/summarize_worker.py
 M docs/USER_MANUAL.md
?? StateFlow_v1_ClaudeCode_Prompts.md   (owner's own untracked planning doc — not committed)
```

`demo/configs/llm_planner.yaml`, `demo/configs/static_3step.yaml`,
`demo/planner/llm_adapter.py`, `demo/planner/echo_worker.py`, and the
`docker-compose*.yml` files were read and found to **already** satisfy this
session's requirements (explicit per-step `timeout_seconds` comfortably
exceeding worker delays; no worker-port-specific timeout defaults) — left
untouched, not a missed task (see §4).

**`test/acceptance/` 的 git 狀態（必填，應為無變動）：**
```text
$ git status --porcelain test/acceptance/
(empty — no changes)
```

**`internal/`, `cmd/`, `migrations/` 的 git 狀態（本 session 不應碰到任何 .go 檔）：**
```text
$ git status --porcelain internal/ cmd/ migrations/
(empty — no changes)
```

## 4. 與 session 指示的偏離點

1. **`demo/configs/*.yaml` and `demo/planner/llm_adapter.py`'s DUMMY plan were
   left completely unedited**, even though the prompt's item 3 asks to "add
   explicit per-step `timeout_seconds`". Reading both files first showed they
   already carry explicit `timeout_seconds` on every step (30s in both YAML
   configs against workers with no fixed/known delay in that context; 10s in
   the DUMMY plan against `worker.py`'s 1s default delay) — comfortably
   exceeding worker delays already. Editing further would have been a
   no-op or, worse, an unrequested rewrite. Reported instead of silently
   skipped per the prompt's own "if X needs no change, say so" spirit.
2. **`demo/README.md` and `demo/README.zh.md` gained a few extra bullet lines**
   describing the new DB/log assertions `crash_demo.py` now performs. Not
   explicitly asked for (the prompt names `crash_demo.py`, the playbooks, and
   the two top-level demo READMEs for old-model purges, not for describing new
   assertions) — added because leaving the READMEs silent about the new
   verification step would make them describe a weaker proof than the script
   actually performs; kept to one bullet list addition each, no restructuring.
3. **Switched `demo/workers/ocr_worker.py` and `demo/workers/summarize_worker.py`
   (the two sync crash-demo workers) to key their idempotency cache primarily
   on the `X-StateFlow-Step-ID` header**, falling back to an input-content hash
   only when the header is absent. This was explicitly flagged in the prompt
   as "optional but recommended... only if it's a small, contained change
   within `demo/`" — done because it is exactly that: an ~8-line change per
   file, fully contained in `demo/`, and it makes `docs/USER_MANUAL.md`'s
   §2.4 sync-idempotency recommendation concrete in actual running code
   instead of only prose. `demo/workers/ner_worker.py` (async) was already
   correctly keyed on `step_id` from the body and was left untouched.
4. **`README.md`'s "Fast workers (<30s)" line (Worker modes table) was left
   unedited**, even though it could be read as implying a stale 30s system
   timeout default. Judged out of the explicit scope ("pointer fixes only...
   old status names like DECIDED/FAILED") — it is a rough proxy-idle-cut
   guideline (LB/proxy behavior, whitepaper §15.4), not a StateFlow config
   claim or a broken link/old-status-name, so it was left alone rather than
   risk a content rewrite beyond the stated pointer-fix scope. Flagged here in
   case the owner disagrees with that line-drawing (see §7).
5. **CLAUDE.md's "Development Discipline" section keeps generalized versions
   of the old "During-Refactor Rules"** (follow scope, don't weaken code to
   satisfy stale tests, verify before reporting, pre-release freedom) rather
   than deleting them outright, since Session 8 (final audit) is still
   pending and these rules remain applicable engineering discipline beyond
   just the v0.9→v1.0 migration. Not explicitly requested, but the prompt's
   own item 6 says to "keep the session-discipline material" — interpreted
   as keep-but-generalize rather than keep-verbatim (verbatim would still
   say "the current session's prompt" language that presumes a numbered
   session is always in flight, which is no longer true after this session).
6. **Restored the demo compose stack to a fully running state
   (`docker compose ... up -d`) after `run_demo.sh`'s scenario functions
   stopped several services** (their own cleanup traps). Not asked for
   explicitly, but leaving the stack partially stopped after a verification
   session seemed like an unhelpful thing to hand back; this is a `docker`
   operational action, not a file edit, so it doesn't touch scope.

## 5. 本次定案的真實介面 / Schema
No Go interfaces or schema were touched this session (scope was `demo/`,
`docs/USER_MANUAL.md`, `CLAUDE.md`, `README.md` pointers only — confirmed
empty `git status --porcelain` for `internal/`, `cmd/`, `migrations/` in §3).

### 5a. 介面 / 型別定義（新增/改動的 Python helper functions, `demo/crash_demo.py`）
```python
def psql(sql: str) -> list:
    """Run a SQL query via `docker compose exec postgres psql`, return non-empty
    tab-separated rows (no header, no footer)."""
    ...

def psql_row(sql: str):
    ...

def psql_scalar(sql: str):
    ...

def count_log_lines(service: str, needle: str) -> int:
    """Count occurrences of an ASCII-only `needle` across `service`'s full
    compose logs (a fresh, non-streaming call)."""
    ...
```
Used to assert, directly against Postgres and container logs: exactly one
`failure_reason='orphaned'` attempt on the crashed step; the step's
`created_at`+`decision` (Barrier-1 record) byte-identical before/after the
crash (⇒ planner asked exactly once); and the worker's real processing log
line appearing exactly once (⇒ idempotency cache absorbed the re-dispatch).

### 5b. Schema
No changes — out of scope, untouched.

## 6. 未解問題（分類 —— 這欄最重要）

- 🟡 已停下、需裁示：**None new this session requiring an owner decision** —
  Session 6.5's flagged `dlq_replay_test.py` timing-race item is unaffected by
  this session (no orchestrator/store code touched) and remains whatever the
  owner already decided/is deciding on it; not re-litigated here.
- 🔴 我自行假設後繼續：
  1. Interpreted "keep the session-discipline material" (CLAUDE.md instructions
     item 6) as "keep the substance, generalize the framing away from
     'TRANSITIONAL'/'current session'" rather than verbatim retention — see §4
     deviation 5. If the owner wanted the old wording preserved character-for-
     character (just with "TRANSITIONAL" struck), that's a quick follow-up.
  2. Interpreted README.md's "old-model concepts" pointer-fix allowance
     narrowly (fixed "Three recovery rules on restart" → "Combination-table
     recovery with orphan-claim + budget check on restart", and "Ghost Mode
     retry" → "in-process orphan sweeper" in the Phase 2 roadmap line) but did
     NOT touch "Fast workers (<30s)" — see §4 deviation 4. A different line
     could reasonably be drawn either way; flagged rather than guessed silently
     past the point I was confident about.
  3. `demo/crash_demo.py`'s new "planner asked exactly once for 'ner'" proof
     uses the step row's `created_at`+`decision` being byte-identical
     pre/post-crash as the evidence, rather than counting an HTTP log line
     (StaticPlanner is in-process, not an HTTP service, so there is no
     "Planner called" log to grep for the static-planner crash demo the way
     `run_demo.sh`'s LLM-adapter scenarios do). This is a sound proof of the
     same invariant (Barrier 1: the step row is only ever written once by
     TX1) but is a different *mechanism* than "count planner calls", which is
     what the prompt's literal wording suggests. Flagging the substitution.
- 無其他：everything else in this session's instructions was either directly
  implementable or already satisfied by the existing files (see §4 deviation 1).

## 7. CONFIRM 值（unchanged this session）
- planner_config 內 HTTP planner URL 的欄位名：`url`（no action needed）
- retry limit X 在 planner_config 的欄位名：`retry_limit`（no action needed）
- `POST /workflows` / `POST /runs` 回傳的 id 欄位名：`workflow_id` / `run_id`（no action needed）
- workflow-level timeout override 的欄位名：`default_timeout_seconds`（confirmed
  by reading `internal/store/postgres.go`'s extraction struct directly this
  session — documented in `docs/USER_MANUAL.md` §1.6 and `CLAUDE.md`'s Demo
  Infrastructure section; still flagged by Session 3/5 as not formally
  owner-confirmed even though the code is consistent and this session found
  no contradicting evidence)

Open question for the owner (also listed in the end-of-session report, Q7):
should README.md's "Fast workers (<30s)" line (§4 deviation 4, §6 🔴 item 2)
be updated to avoid implying a stale 30s default, given it sits right next to
a table this session otherwise left untouched per the pointer-fix-only scope?

---

## 8. 流水帳（APPEND-ONLY —— 覆寫本檔時，這區只准往下加一行，永不刪改上面的行）
- Session 0 (`<sha7>`)：read-only 稽核完成，產出 test/coverage 盤點。
- Session 1 (`<sha7>`)：`<一句話結果>`
<!-- Session 2 起在此行下方繼續 append -->
- Session 2 (`ac7bb85`)：rewrote `internal/core/interfaces.go` — closed status enums matching DB CHECK values byte-for-byte, `StateStore` interface with one method per Atomic Transaction Ledger entry (TX-W..TX9) plus reads, typed CAS outcomes (`ReportOutcome`/`FailureOutcome` per T1) so a superseded report is a normal return value, `FailureReason` unrepresentable outside `AttemptStatusFailed` via nested `ResultFailure` (T3), zero timestamp parameters anywhere (T2). `go build ./internal/core/...` passes; rest of module intentionally still broken (store/transport/planner reference old fields) exactly as expected per the session's own scope note.
- Session 2 follow-up (`6e6fe70`)：owner-directed fixes on the same file — added `StateStore.GetWorkflow` (closes the §12.1 planner-reconstruction gap), corrected `WorkflowDef.RetryLimit`'s doc to say it's nested in `PlannerConfig` under key `retry_limit` (not an independent column), documented `Frontier.PendingAttemptID`'s unconditional-claim-via-CAS semantics. `go build ./internal/core/...` and `go vet` still pass; gofmt clean.
- Session 3 (`1dc98f6`)：rewrote `internal/store/postgres.go` implementing `core.StateStore` in full — every Ledger TXn (TX-W..TX9) as one BEGIN...COMMIT, CAS-A on both attempt state and `steps.current_attempt_id` for every terminal attempt write, TX3's same-transaction DLQ blade, TX5's five-write reset. Rewrote `internal/store/postgres_test.go` (15 tests, deleting the old three DECIDED/FAILED-model tests) covering the five mandatory cases plus every remaining method. `go test ./internal/store/... -v` 15/15 green against live Postgres; `internal/store` no longer appears in `go build ./...`'s error list (only `internal/transport`/`internal/planner` remain broken, unchanged from Session 2, owned by Sessions 4/5).
- Session 4 (`b21047b`)：rewrote `internal/transport/sync.go` and `async.go` against the frozen `core.Result{Status,Output,Failure}` shape and the timeout doctrine (whitepaper §6): transports never resolve their own timeout, honor the incoming ctx deadline only, and return `(Result{}, err)` — never a fabricated `Reason=timeout` — when no valid response is obtained. Sync sends the bare input plus the two ID headers; async sends the `{step_id,attempt_id,input}` envelope and expects 202. `AsyncTransport.DeliverCallback` now validates a callback against the live `current_attempt_id` via a new read-only `AttemptStore` interface before routing it, closing a stale-attempt/registry-collision bug (registry keyed by StepID, not AttemptID). Rewrote both test files (15 tests total) covering wire formats byte-for-byte, the full outcome-mapping matrix, and async registry/store-validation hygiene; all green including under `-race`. `internal/transport` no longer appears in `go build ./...`'s error list.
- Session 5 (`8b0411b`)：rewrote `internal/orchestrator/loop.go` and `recovery.go` against the v1.0 TX ledger (whitepaper §5/§6/§7/§8) — single Run() entry point for both normal operation and one-time crash recovery, planner reconstructed from the workflow row every call, retry-budget-source defect fixed (RetryPolicy.Next fed from persisted `steps.attempt_count`, never a loop-local counter), planner budget (30s×3) moved from `HTTPPlanner`'s internal retry loop into the loop with per-attempt unreachable/malformed classification (new `planner.MalformedError`) and TX9 detail. Deleted the four-rule recovery code and `TestRecovery_FailedNoOutputReDispatched`; rewrote all three orchestrator test files against the new `StateStore`/`Loop` shapes, including the five mandatory tests (budget-boundary crash, recovery re-entrancy, crash-between-TX3-and-TX4, planner-asked-exactly-once, wire-casing). Also fixed an unrelated pre-existing `internal/planner/static.go` compile error blocking that package. `internal/orchestrator` (15/15) and `internal/planner` (14/14) fully green, including under `-race`; `internal/api`/`cmd/stateflow` remain non-compiling for reasons outside this session's scope (flagged, not fixed — see §6 of the Session 5 report).
- Session 6 (`3b20d5d`)：rewrote `internal/api/server.go` against `core.StateStore` (TX-W/TX0/TX5/TX6 through the store interface, not raw SQL) and a redesigned `GET /runs/{id}` response (run status, per-step seq/attempt_count/created_at/current-attempt summary, dlq_reason on DLQ — `decided_at` retired for good); fixed `cmd/stateflow/main.go`'s three known post-Session-4/5 breaks and simplified it (planner construction moved entirely into `Loop.Run`, so `main.go` no longer needs it); rewrote `internal/api/server_test.go` incl. an explicit wire-casing contract test (history UPPERCASE + planner-verdict-casing enforcement) and dedicated TX5/TX6 DLQ-replay integration tests. `go build ./...`/`go vet ./...` clean for the whole tree; `go test -p 1 ./...` fully green (repeated twice, no flakiness); `internal/api` green under `-race`; live container smoke test via `docker compose up -d --build` confirms the actual production binary starts, recovers, and serves traffic. Owner-oracle scripts (`test/acceptance/*.py`) could not be run to completion — blocked by this session's own sandbox denying the network workarounds needed for this Windows/WSL2/Docker-Desktop topology; flagged for the owner, not a code defect (see §6 🟡).
- Session 6.5 (`d1c3aa7`)：fixed the Session-6-audit-flagged TX5 worker-side replay bug — added `orchestrator.Loop.ResumeReplayedStep` (`internal/orchestrator/replay.go`, new file) which dispatches a TX5-freshly-created attempt directly instead of routing through `Run()`'s crash-recovery orphan-claim check (which was burning the just-reset retry budget before any worker was ever contacted, fatal at `retry_limit=1`); extracted `Run()`'s steady-state loop into a shared `driveSteadyState` helper reused by both entry points, unchanged behavior. Rewired `handleDLQReplay`'s worker-side branch (`internal/api/server.go`) to call the new entry point via a `Server.replayTransport` field built inside `New()` — no change to `New()`'s exported signature, so `cmd/stateflow/main.go` needed no edit. Added 2 new `internal/orchestrator` tests seeding the exact TX5-aftermath state directly; tightened `TestAPI_DLQ_ReplayWorkerSide` to `retry_limit=1` with exact-dispatch-count and zero-orphaned-attempt assertions. `go test -p 1 ./...` fully green; `crash_recovery_test.py` PASSed unmodified; `EXPECT_X=2 dlq_replay_test.py`'s final transient-state assertion failed on a timing race in the frozen oracle's own polling design (diagnosed, not a code defect) — subsequently addressed by a small follow-up commit (`98f113d`, outside any numbered session) giving `/sync/fail` in `fake_worker.py` a configurable delay so that assertion could observe the TX5 reset; both oracle scripts confirmed passing end-to-end as of `98f113d` per this session's own prompt.
- Session 7 (pending commit)：aligned all demo scripts/docs and the project's two top-level governance docs to the v1.0 three-state model; zero `.go` files touched (confirmed empty `git status --porcelain internal/ cmd/ migrations/`). `demo/crash_demo.py` gained direct Postgres/log assertions after recovery: exactly one `failure_reason='orphaned'` attempt on the crashed step, the step's Barrier-1 record (`created_at`+`decision`) byte-identical pre/post-crash (proving the planner was asked exactly once), and the worker's real processing log line appearing exactly once (proving idempotency-cache absorption) — all passing against a live stack. `demo/run_demo.sh` gained a `check_dlq_worker_retry_exhausted` assertion in scenario 2 (DLQ reason + per-attempt failure reason present in `context`) and had its two remaining old-model status checks fixed (`FAILED`→`DLQ` for run/step terminal-state checks, since step status never has a `FAILED` value in v1.0); all 3 scenarios re-verified passing end-to-end. Purged the last old-model traces from `demo/playbook/PLAYBOOK.{en,zh}.md` (`attempt_number`/`dispatched_at` SQL → `status`/`failure_reason`/`created_at`; "DECIDED" wording → "decision persisted (TX1, Barrier 1)"). Switched `demo/workers/{ocr,summarize}_worker.py`'s idempotency cache to key primarily on the `X-StateFlow-Step-ID` header (input-hash fallback only), making `docs/USER_MANUAL.md`'s sync-idempotency recommendation concrete in running demo code. Fully rewrote `docs/USER_MANUAL.md`: corrected timeout defaults (60s system default, `default_timeout_seconds` workflow override, per-step override — no more "defaults to 30s" claims anywhere, including the planner-config table, which was also corrected to drop the no-longer-read `max_retries`/planner-local `timeout_seconds` keys and add the real `retry_limit`/`default_timeout_seconds` keys); replaced the single `planner_failed` DLQ reason with the real four-value set plus a full triage table (new §3, explicitly warning against blind-replaying `planner_declared_fail`); added a quantified concurrent-idempotency contract section (up to X concurrent duplicates, explicitly covering timeout-triggered re-dispatch racing a still-alive worker, not just crash re-dispatch); extended the superseded-callback section to cover a success report arriving after a timeout verdict; rewrote the dispatch-format/idempotency sections per whitepaper §13.1 (sync = bare input + `X-StateFlow-Step-ID`/`X-StateFlow-Attempt-ID` headers, header preferred over input-hash for the cache key). Replaced the transitional `CLAUDE.md` with a final version: full Quick Reference (3×3×3 states, the two barriers, the run×last_step combination table, the full TX ledger one-liner-each, the timeout doctrine, the orphan rule, the persisted-retry-budget rule, the CAS rule, the single-writer rule, wire formats), the whitepaper §18 Temporary Design Registry pulled forward verbatim as a named "Deferred / Explicitly Out of Scope" list, and a generalized (no-longer-"TRANSITIONAL") Development Discipline section; confirmed the `-p 1` test-package set is unchanged (`internal/api`, `internal/orchestrator`, `internal/store`, via a fresh grep). Fixed `README.md`'s two broken doc links (`DESIGN.md` → removed, content now lives in the whitepaper; `docs/StateFlow_Whitepaper_v0.8.md` → `docs/StateFlow_Whitepaper_v1_0.md`) and two old-model phrases ("Three recovery rules on restart" → "Combination-table recovery with orphan-claim + budget check on restart"; "Ghost Mode retry" → "in-process orphan sweeper", matching whitepaper §16/§18's explicit dissolution of Ghost Mode). All four completion-condition commands verified green against a live from-`--build` stack: `docker compose up -d --build`, `python demo/crash_demo.py` (PASS, new assertions included), `./demo/run_demo.sh` scenarios 1–3 (all PASS, scenario 2's new DLQ check included), `go test -p 1 ./...` (6/6 packages ok, 2 with no test files) — see §2 for verbatim output. `demo/configs/*.yaml` and `demo/planner/llm_adapter.py`'s DUMMY plan already had explicit, comfortably-exceeding `timeout_seconds` and needed no edits (verified by reading, not assumed).
