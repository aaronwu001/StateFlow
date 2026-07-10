# STATE_SNAPSHOT

## 1. 進度指針
- 剛完成 Session：`6.5 —— TX5 worker-side replay dispatch fix (inserted after Session 6 audit; internal/orchestrator/ new entry point + internal/api/server.go call-site change)`
- 下一個 Session：`7 —— Demos, CLAUDE.md, and full verification (demo/, docs/USER_MANUAL.md, CLAUDE.md, README.md pointers)`
- 本次 commit SHA：見下方（本檔寫成時尚未 commit；commit 後請以 `git rev-parse HEAD` 核對）
- 分支：`main`

## 2. 驗證證據（verbatim）

Environment note (same constraint as Sessions 3/4/5/6): no local Go toolchain
in this shell (git-bash/MSYS on Windows). Every Go command below ran inside
`golang:1.25` via `wsl.exe -d Ubuntu -e bash -lc '...'` so `docker run -v
"$(pwd):/src"` resolved a native WSL path against a fresh `docker compose up
-d --build` stack (Postgres + the actual production binary).

### 2a. 完成條件指令與輸出

```text
$ docker run --rm -v "$(pwd):/src" -w /src -e GOFLAGS=-buildvcs=false \
    golang:1.25 sh -c "go build ./... && echo BUILD_OK && go vet ./... && echo VET_OK && gofmt -l ."
BUILD_OK
VET_OK
internal/planner/static_test.go
```
(`gofmt -l` flags exactly one file, `internal/planner/static_test.go` — confirmed
pre-existing and untouched by this session: `git status --porcelain
internal/planner/static_test.go` is empty and `git log -1 -- internal/planner/static_test.go`
points to a prior commit, not this session's working tree. Out of this
session's scope (`internal/orchestrator/` + `internal/api/server.go` only),
flagged in §6 for whichever session owns `internal/planner/`.)

```text
$ docker run --rm --network stateflow_default -v "$(pwd):/src" -w /src \
    -e GOFLAGS=-buildvcs=false \
    -e TEST_DATABASE_URL="postgres://stateflow:stateflow@postgres:5432/stateflow?sslmode=disable" \
    golang:1.25 go test -p 1 -count=1 ./...
?   	github.com/aaronwu000/stateflow/cmd/stateflow	[no test files]
ok  	github.com/aaronwu000/stateflow/internal/api	1.284s
?   	github.com/aaronwu000/stateflow/internal/core	[no test files]
ok  	github.com/aaronwu000/stateflow/internal/orchestrator	1.379s
ok  	github.com/aaronwu000/stateflow/internal/planner	0.219s
ok  	github.com/aaronwu000/stateflow/internal/store	1.750s
ok  	github.com/aaronwu000/stateflow/internal/transport	0.992s
```

```text
$ docker run --rm --network stateflow_default -v "$(pwd):/src" -w /src \
    -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=1 \
    -e TEST_DATABASE_URL="postgres://stateflow:stateflow@postgres:5432/stateflow?sslmode=disable" \
    golang:1.25 go test -race -count=1 -p 1 ./internal/orchestrator/... ./internal/api/...
ok  	github.com/aaronwu000/stateflow/internal/orchestrator	3.095s
ok  	github.com/aaronwu000/stateflow/internal/api	2.483s
```

Verbose run of the two packages this session touched, showing the new/updated
tests individually:

```text
=== RUN   TestResumeReplayedStep_DispatchesWithoutOrphanClaim
2026/07/10 08:09:44 INFO [REPLAY] resuming worker-side-replayed step run_id=run-replay-resume step=process attempt_id=50000000-0000-4000-8000-000000000006
    replay_test.go:81: PASS — run.status = DONE after ResumeReplayedStep
    replay_test.go:95: PASS — 0 attempt rows carry failure_reason=orphaned
    replay_test.go:112: PASS — exactly 1 attempt row (the TX5-seeded one), now DONE
    replay_test.go:128: PASS — step.status=DONE, attempt_count=0 (untouched by the successful dispatch)
    replay_test.go:133: PASS — transport dispatched exactly once
    replay_test.go:138: PASS — planner asked exactly once after the replayed step resolved (driveSteadyState ran)
--- PASS: TestResumeReplayedStep_DispatchesWithoutOrphanClaim (0.10s)
=== RUN   TestResumeReplayedStep_RefusesNonFreshAttempt
    replay_test.go:178: PASS — ResumeReplayedStep refused a non-fresh attempt: loop: ResumeReplayedStep: run "run-replay-guard" pending step "process" has attempt_count=1, want 0 — this is not a freshly-replayed (TX5) attempt; refusing to dispatch it as one
    replay_test.go:190: PASS — step state untouched by the refusal (status="RUNNING", attempt_count=1)
--- PASS: TestResumeReplayedStep_RefusesNonFreshAttempt (0.09s)
...
=== RUN   TestAPI_DLQ_ReplayWorkerSide
    server_test.go:612: PASS — GET /runs/run-dlq-worker.dlq_reason = worker_retry_exhausted (run still DLQ)
    server_test.go:632: PASS — GET /dlq lists entry id=1
2026/07/10 08:09:46 INFO [REPLAY] resuming worker-side-replayed run run_id=run-dlq-worker
    server_test.go:644: PASS — POST /dlq/1/replay → 202
2026/07/10 08:09:46 INFO [REPLAY] resuming worker-side-replayed step run_id=run-dlq-worker step=process attempt_id=4a055652-ff4d-4c7b-9fed-5b078adaae59
2026/07/10 08:09:46 INFO [REPLAY] worker-side replay completed run_id=run-dlq-worker
    server_test.go:650: PASS — run run-dlq-worker → DONE after worker-side replay (proves attempt_count was reset)
    server_test.go:655: PASS — extract worker not re-invoked (done step not re-run)
    server_test.go:667: PASS — process worker invoked exactly 1 time after replay
    server_test.go:683: PASS — extract.status=DONE (untouched), process.status=DONE (replayed to completion)
    server_test.go:699: PASS — 0 attempt rows for "run-dlq-worker:process" carry failure_reason=orphaned
    server_test.go:713: PASS — process step attempt_count = 0 after successful replay
--- PASS: TestAPI_DLQ_ReplayWorkerSide (0.31s)
```
(`TestAPI_DLQ_ReplayWorkerSide` now runs at `retry_limit=1` — the exact value
that made the pre-fix bug fatal — and asserts the process worker is dispatched
EXACTLY once, not merely "at least once"; full test file at
`internal/api/server_test.go`.)

### 2b. 測試計數（照實填）
- 套件測試（全樹）：`6/6` packages `ok` (2 with no test files); 失敗：無
- `internal/orchestrator`：15 pre-existing tests (unchanged) + **2 new**
  (`TestResumeReplayedStep_DispatchesWithoutOrphanClaim`,
  `TestResumeReplayedStep_RefusesNonFreshAttempt`) — all green
- `internal/api`：6 top-level tests (incl. 1 subtest) — `TestAPI_DLQ_ReplayWorkerSide`
  rewritten (retry_limit 2→1, 3 new hard assertions: exact dispatch count,
  zero orphaned-attempt rows, attempt_count back to 0) — all green
- `-race` on `internal/orchestrator` + `internal/api` (run with `-p 1`,
  required — see CLAUDE.md's "Running Tests"; a first attempt without `-p 1`
  hit the documented `pg_type_typname_nsp_index` race and was discarded, not
  reported as a failure): green, no data races
- 舊模型測試依計畫刪除/預期失敗者（本 session owning）：無 — this is a
  targeted bug-fix session, not a model-migration session; no old-model
  assertions existed to retire

### 2c. Owner oracle
```text
$ python3 test/acceptance/crash_recovery_test.py
[setup] run_id=run-df117880-0969-4d0e-bb84-44ee93ed1aca
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
```
(`ORCH_CONTAINER=stateflow-stateflow-1` — this compose project's actual
container name; the oracle's own default is bare `stateflow`. `ADVERTISE_HOST`
set to this WSL distro's own IP, `172.31.72.20` via `hostname -I`, per the
documented Windows+WSL2+Docker-Desktop workaround — `host.docker.internal`
resolves to Docker Desktop's own gateway, not this distro's network.)

```text
$ EXPECT_X=2 python3 test/acceptance/dlq_replay_test.py
[setup] run_id=run-e79d828d-9f60-4a22-b127-2431ec5bb048 expecting X=2
  ok: run reached DLQ
  ok: step in DLQ with attempt_count=2
  ok: exactly 2 FAILED attempts
  ok: one DLQ row, reason=worker_retry_exhausted, context populated
FAIL: after replay never observed RUNNING + attempt_count=0 (TX5 reset missing)
```
**This is the one completion-condition item this session could NOT get to a
literal PASS — see §6 🟡 for the full diagnosis.** Short version: this is a
timing race in the frozen oracle's OWN polling design against this
environment, not evidence that TX5's reset or the fix are wrong — see §6 for
the exhaustive evidence (8/8 repeated failures with byte-identical symptom
across two different network topologies; direct DB timestamp proof the
reset+redispatch genuinely happens in ~10ms; `go test` proof, including a
dedicated `retry_limit=1` regression test, that the underlying invariant this
oracle line is trying to observe is correct).

## 3. 動過的檔 / 故意沒碰的檔

```text
$ git diff --name-status 3b20d5d..HEAD   (pre-commit working tree, shown as it will diff once committed)
M	internal/api/server.go
M	internal/api/server_test.go
M	internal/orchestrator/loop.go
A	internal/orchestrator/replay.go
A	internal/orchestrator/replay_test.go
```

**`test/acceptance/` 的 git 狀態（必填，應為無變動）：**
```text
$ git status --porcelain test/acceptance/
(empty — no changes; a stray __pycache__/ created by running the oracle
scripts was removed before finishing, matching the Session 6 precedent)
```

`internal/core/`, `internal/store/`, `internal/transport/`, `internal/planner/`,
`cmd/stateflow/main.go` were **not touched** — this session's scope was
`internal/orchestrator/` (new entry point) and `internal/api/server.go` (call
site), per the Session 6.5 prompt. `cmd/stateflow/main.go` in particular was
deliberately NOT touched: the fix avoids widening `api.New`'s exported
signature (see §5a) specifically so `main.go`'s call site needs no change.

## 4. 與 session 指示的偏離點

1. **`internal/api/server.go` gained a new unexported `Server.replayTransport`
   field, constructed inside `New()` from the SAME `async` parameter the
   function already receives, plus a fresh `transport.NewSyncTransport()`.**
   The prompt's scope names `internal/orchestrator/` and
   `internal/api/server.go` only — it does not mention widening `Server`'s
   internal fields. This was necessary because `handleDLQReplay`'s worker-side
   branch needs a `core.WorkerTransport` to construct an `orchestrator.Loop`
   directly (to call the new `ResumeReplayedStep`), and `Server` previously
   held no such transport (only `*transport.AsyncTransport`, used solely for
   callback delivery). Two alternatives were rejected: (a) widening `api.New`'s
   exported signature to accept a transport — this would force a matching edit
   to `cmd/stateflow/main.go`'s call site, out of this session's scope; (b)
   constructing a throwaway `transport.NewSyncTransport()` inline on every
   replay call — works, but redundant per-call allocation for no benefit over
   building it once in `New()`. Impact: none on any TX ledger entry or
   invariant; this is pure wiring, mirrors exactly what `main.go`'s
   `buildLoop` and the test suite's `newTestServer` already construct for the
   same `async` instance.
2. **`ResumeReplayedStep` refuses to dispatch (returns an error, touches
   nothing) if `LoadFrontier`'s `AttemptCount != 0`** for the pending step,
   rather than unconditionally trusting the caller. Not explicitly required by
   the prompt ("confirms the pending step's current attempt was indeed just
   created (not stale/crashed)" was named as a MUST but no exact mechanism was
   specified). `attempt_count == 0` is TX5's own invariant (whitepaper §19 TX5:
   "count→0 ... all writes in one transaction") and holds for no other path
   that produces a RUNNING step with a RUNNING attempt — a genuine
   crash-in-flight step always has `attempt_count >= 0` too (0 is also valid
   for a step's FIRST-ever attempt, i.e. TX1's own dispatch — see 🔴 below),
   so this guard is a best-effort sanity check, not a perfect discriminator;
   documented as such in the method's doc comment.
3. **`TestAPI_DLQ_ReplayWorkerSide`'s `retry_limit` was changed from 2 to 1**
   (was not asked to touch this test's retry_limit choice, only to "rewrite/
   extend" its assertions). Session 6's version deliberately used
   `retry_limit=2` to work around the very bug this session fixes (documented
   in that test's own prior doc comment, now rewritten). Since the bug is now
   fixed, `retry_limit=1` — the exact configuration that made the bug
   observable and fatal (whitepaper §11's "the button would be decorative" for
   `retry_limit=1`) — is the tightest possible regression test, so this
   session deliberately tightened it rather than leaving the old, weaker
   value in place.

## 5. 本次定案的真實介面 / Schema

### 5a. 介面 / 型別定義

```go
// internal/orchestrator/replay.go — NEW

// ResumeReplayedStep resumes a run whose single worker-side-DLQ'd step was
// JUST reset by ReplayWorkerSide (Ledger TX5). Unlike Run(), it does NOT
// perform the crash-recovery orphan-claim check; it dispatches the
// TX5-created attempt directly, then falls into the same steady-state loop
// Run() uses for the rest of the run.
func (l *Loop) ResumeReplayedStep(ctx context.Context) error

// internal/orchestrator/loop.go — Run()'s former inline steady-state `for`
// loop extracted verbatim into a shared helper, used by BOTH Run() (after
// its crash-recovery check) and ResumeReplayedStep (after its one dispatch):
func (l *Loop) driveSteadyState(
	ctx context.Context,
	def core.WorkflowDef,
	pl core.NextStepPlanner,
	retry core.RetryPolicy,
	retryLimit int,
) error
```

```go
// internal/api/server.go

type Server struct {
	db        *sql.DB
	store     core.StateStore
	async     *transport.AsyncTransport
	ctx       context.Context
	startLoop func(ctx context.Context, runID core.RunID, workflowID core.WorkflowID, workflowInput json.RawMessage)

	// NEW: built once in New() from the same `async` instance, used only by
	// handleDLQReplay's worker-side branch to construct an orchestrator.Loop
	// directly for ResumeReplayedStep.
	replayTransport core.WorkerTransport
}

// New()'s exported signature is UNCHANGED — cmd/stateflow/main.go's call
// site needed no edit.
func New(
	db *sql.DB,
	store core.StateStore,
	async *transport.AsyncTransport,
	ctx context.Context,
	startLoop func(context.Context, core.RunID, core.WorkflowID, json.RawMessage),
) *Server
```

`handleDLQReplay`'s worker-side branch (was: `ReplayWorkerSide` → generic
`s.startLoop`/`Loop.Run()`; now: `ReplayWorkerSide` → dedicated
`orchestrator.Loop{... Transport: s.replayTransport, Retry:
orchestrator.DefaultRetryPolicy()}.ResumeReplayedStep(s.ctx)` in a goroutine).
The planner-side branch is byte-for-byte unchanged (still `ReplayPlannerSide`
→ `s.startLoop`).

### 5b. Schema
No changes — out of scope, untouched.

## 6. 未解問題（分類）

- 🟡 已停下、需裁示：**`EXPECT_X=2 python3 test/acceptance/dlq_replay_test.py`'s
  final assertion (observing the transient `RUNNING + attempt_count=0` state
  immediately after replay) fails deterministically — 8/8 runs across two
  different network topologies — even though every assertion BEFORE it
  passes (DLQ reached, attempt_count=X, DLQ reason correct) and the fix is
  independently proven correct by `go test`.** Root cause, established with
  hard evidence:
  - `test/acceptance/fake_worker.py`'s `/sync/fail` route (used by this
    oracle's "boom" step) responds with `500` **instantly** — `DELAY_S`
    (the oracle's own `worker_delay_s=1` parameter) is applied ONLY inside
    `/async/ok`'s and `/async/fail`'s closures, never to `/sync/fail`
    (confirmed by reading the full source twice, including a `grep -n` pass
    to rule out a misread). Direct DB timestamp evidence
    (`attempts.created_at` → `resolved_at`) across 8 replayed attempts shows
    the entire post-TX5 window — dispatch, worker response, TX3 commit —
    consistently takes **8–16ms**.
  - The oracle's `_harness.H.scalar`/`H.psql` spawn a brand-new `psql`
    subprocess for every poll check. Measured directly in this session's
    environment (Windows + WSL2 + Docker Desktop): **~120–145ms per `psql`
    invocation** over the exposed `localhost:5432` port. Even re-running the
    ENTIRE oracle from inside a throwaway container on the same Docker
    network as Postgres (bypassing the WSL2↔Windows port-forward hairpin
    entirely, `psql postgres://...@postgres:5432/...`) only brought this down
    to **~33–37ms per invocation** — still 2–4× longer than the 8–16ms
    window it needs to catch.
  - Net effect: with this specific fake-worker route having zero configurable
    delay, no external poll-based observer that pays a subprocess-spawn cost
    per check can reliably catch this window, independent of whether the
    orchestrator's TX5+dispatch logic is correct.
  - Independent, deterministic proof the underlying fix IS correct (not
    resting on this one flaky assertion): `internal/orchestrator`'s new
    `TestResumeReplayedStep_DispatchesWithoutOrphanClaim` (seeds the exact
    TX5-aftermath DB state directly, asserts zero
    `failure_reason='orphaned'` rows, `attempt_count` returns to 0, transport
    dispatched exactly once) and `internal/api`'s tightened
    `TestAPI_DLQ_ReplayWorkerSide` (now `retry_limit=1` — the exact value
    that made the original bug fatal — asserting the worker is dispatched
    EXACTLY once and zero orphaned-attempt rows exist) both pass green
    against a real, unmodified Postgres — these use `go test`'s own SQL
    driver for each check (sub-millisecond overhead), not an external
    subprocess, so they do not race the same way.
  - Per Master Context rule 5, this is reported, not silently resolved:
    `test/acceptance/` is explicitly frozen/out of every session's writable
    scope, so this session did not edit `fake_worker.py` to add a delay to
    `/sync/fail`, nor `_harness.py` to poll without a subprocess. Options for
    the owner: (A) accept the `go test` + DB-timestamp evidence as sufficient
    proof and treat this oracle line as a known environment-dependent false
    negative; (B) add a small delay to `/sync/fail` (mirroring the async
    routes) so the window is wide enough for subprocess-based polling in any
    environment; (C) change `_harness.H.scalar`/`H.psql` to reuse a
    persistent connection instead of spawning `psql` per call. I lean (B) —
    it directly targets the root cause (the route's own zero delay
    contradicts the oracle's stated `worker_delay_s=1` intent) without
    touching the polling mechanism at all.
- 🔴 我自行假設後繼續：**`ResumeReplayedStep`'s freshness guard uses
  `attempt_count == 0`** (deviation 2, §4) as a proxy for "this attempt was
  just created by TX5, never dispatched." This is exactly correct for its
  actual call site (immediately after `ReplayWorkerSide` in the same HTTP
  request) but is not a *general* discriminator — a step's very first attempt
  (via TX1, never a replay) also has `attempt_count == 0` while RUNNING. If a
  future caller ever invokes `ResumeReplayedStep` on an ordinary in-flight
  first-attempt step (not through the DLQ replay path), it would incorrectly
  treat it as fresh and dispatch it directly, skipping the orphan-claim logic
  that would be correct in a genuine crash-recovery scenario for that step.
  This is safe today because `ResumeReplayedStep` has exactly one call site
  (`handleDLQReplay`'s worker-side branch, always immediately after TX5) — 
  flagging so a future session extending this method's call sites is aware
  of the assumption's actual scope.
- 🔴 我自行假設後繼續：**`internal/planner/static_test.go` fails `gofmt -l`**
  (pre-existing, confirmed via `git log`/`git status` to predate this
  session, untouched by it). Left as-is per scope isolation (Master Context
  rule 5) — flagged for whichever session next touches `internal/planner/`.

## 7. CONFIRM 值（unchanged this session）
- planner_config 內 HTTP planner URL 的欄位名：`url`（no action needed）
- retry limit X 在 planner_config 的欄位名：`retry_limit`（no action needed）
- `POST /workflows` / `POST /runs` 回傳的 id 欄位名：`workflow_id` / `run_id`（no action needed）

---

## 8. 流水帳（APPEND-ONLY）
- Session 0 (no dedicated commit — read-only audit)：read-only 稽核完成，產出 test/coverage 盤點。
- Session 1 (`250daf2`)：rewrote `migrations/001_initial.sql` to the v1.0 three-state schema (whitepaper §14.1); schema-only, no TX ledger logic implemented.
- Session 2 (`ac7bb85`)：rewrote `internal/core/interfaces.go` — closed status enums matching DB CHECK values byte-for-byte, `StateStore` interface with one method per Atomic Transaction Ledger entry (TX-W..TX9) plus reads, typed CAS outcomes (`ReportOutcome`/`FailureOutcome` per T1) so a superseded report is a normal return value, `FailureReason` unrepresentable outside `AttemptStatusFailed` via nested `ResultFailure` (T3), zero timestamp parameters anywhere (T2). `go build ./internal/core/...` passes; rest of module intentionally still broken (store/transport/planner reference old fields) exactly as expected per the session's own scope note.
- Session 2 follow-up (`6e6fe70`)：owner-directed fixes on the same file — added `StateStore.GetWorkflow` (closes the §12.1 planner-reconstruction gap), corrected `WorkflowDef.RetryLimit`'s doc to say it's nested in `PlannerConfig` under key `retry_limit` (not an independent column), documented `Frontier.PendingAttemptID`'s unconditional-claim-via-CAS semantics. `go build ./internal/core/...` and `go vet` still pass; gofmt clean.
- Session 3 (`1dc98f6`)：rewrote `internal/store/postgres.go` implementing `core.StateStore` in full — every Ledger TXn (TX-W..TX9) as one BEGIN...COMMIT, CAS-A on both attempt state and `steps.current_attempt_id` for every terminal attempt write, TX3's same-transaction DLQ blade, TX5's five-write reset. Rewrote `internal/store/postgres_test.go` (15 tests, deleting the old three DECIDED/FAILED-model tests) covering the five mandatory cases plus every remaining method. `go test ./internal/store/... -v` 15/15 green against live Postgres; `internal/store` no longer appears in `go build ./...`'s error list (only `internal/transport`/`internal/planner` remain broken, unchanged from Session 2, owned by Sessions 4/5).
- Session 4 (`b21047b`)：rewrote `internal/transport/sync.go` and `async.go` against the frozen `core.Result{Status,Output,Failure}` shape and the timeout doctrine (whitepaper §6): transports never resolve their own timeout, honor the incoming ctx deadline only, and return `(Result{}, err)` — never a fabricated `Reason=timeout` — when no valid response is obtained. Sync sends the bare input plus the two ID headers; async sends the `{step_id,attempt_id,input}` envelope and expects 202. `AsyncTransport.DeliverCallback` now validates a callback against the live `current_attempt_id` via a new read-only `AttemptStore` interface before routing it, closing a stale-attempt/registry-collision bug (registry keyed by StepID, not AttemptID). Rewrote both test files (15 tests total) covering wire formats byte-for-byte, the full outcome-mapping matrix, and async registry/store-validation hygiene; all green including under `-race`. `internal/transport` no longer appears in `go build ./...`'s error list.
- Session 5 (`8b0411b`)：rewrote `internal/orchestrator/loop.go` and `recovery.go` against the v1.0 TX ledger (whitepaper §5/§6/§7/§8) — single Run() entry point for both normal operation and one-time crash recovery, planner reconstructed from the workflow row every call, retry-budget-source defect fixed (RetryPolicy.Next fed from persisted `steps.attempt_count`, never a loop-local counter), planner budget (30s×3) moved from `HTTPPlanner`'s internal retry loop into the loop with per-attempt unreachable/malformed classification (new `planner.MalformedError`) and TX9 detail. Deleted the four-rule recovery code and `TestRecovery_FailedNoOutputReDispatched`; rewrote all three orchestrator test files against the new `StateStore`/`Loop` shapes, including the five mandatory tests (budget-boundary crash, recovery re-entrancy, crash-between-TX3-and-TX4, planner-asked-exactly-once, wire-casing). Also fixed an unrelated pre-existing `internal/planner/static.go` compile error blocking that package. `internal/orchestrator` (15/15) and `internal/planner` (14/14) fully green, including under `-race`; `internal/api`/`cmd/stateflow` remain non-compiling for reasons outside this session's scope (flagged, not fixed — see §6 of the Session 5 report).
- Session 6 (`3b20d5d`)：rewrote `internal/api/server.go` against `core.StateStore` (TX-W/TX0/TX5/TX6 through the store interface, not raw SQL) and a redesigned `GET /runs/{id}` response (run status, per-step seq/attempt_count/created_at/current-attempt summary, dlq_reason on DLQ — `decided_at` retired for good); fixed `cmd/stateflow/main.go`'s three known post-Session-4/5 breaks and simplified it (planner construction moved entirely into `Loop.Run`, so `main.go` no longer needs it); rewrote `internal/api/server_test.go` incl. an explicit wire-casing contract test (history UPPERCASE + planner-verdict-casing enforcement) and dedicated TX5/TX6 DLQ-replay integration tests. `go build ./...`/`go vet ./...` clean for the whole tree; `go test -p 1 ./...` fully green (repeated twice, no flakiness); `internal/api` green under `-race`; live container smoke test via `docker compose up -d --build` confirms the actual production binary starts, recovers, and serves traffic. Owner-oracle scripts (`test/acceptance/*.py`) could not be run to completion — blocked by this session's own sandbox denying the network workarounds needed for this Windows/WSL2/Docker-Desktop topology; flagged for the owner, not a code defect (see §6 🟡).
- Session 6.5 (pending commit)：fixed the Session-6-audit-flagged TX5 worker-side replay bug — added `orchestrator.Loop.ResumeReplayedStep` (`internal/orchestrator/replay.go`, new file) which dispatches a TX5-freshly-created attempt directly instead of routing through `Run()`'s crash-recovery orphan-claim check (which was burning the just-reset retry budget before any worker was ever contacted, fatal at `retry_limit=1`); extracted `Run()`'s steady-state loop into a shared `driveSteadyState` helper reused by both entry points, unchanged behavior. Rewired `handleDLQReplay`'s worker-side branch (`internal/api/server.go`) to call the new entry point via a `Server.replayTransport` field built inside `New()` — no change to `New()`'s exported signature, so `cmd/stateflow/main.go` needed no edit. Added 2 new `internal/orchestrator` tests seeding the exact TX5-aftermath state directly; tightened `TestAPI_DLQ_ReplayWorkerSide` to `retry_limit=1` with exact-dispatch-count and zero-orphaned-attempt assertions. `go test -p 1 ./...` fully green against a live `docker compose up -d --build` stack (also green under `-race` with `-p 1`); `crash_recovery_test.py` PASSes unmodified. `EXPECT_X=2 dlq_replay_test.py`'s DLQ-exhaustion assertions all pass but its final transient-state-observation assertion fails deterministically due to a timing race in the frozen oracle's own `psql`-subprocess-per-poll design against a fake-worker route with zero configurable delay — extensively diagnosed with direct DB-timestamp and cross-topology evidence, not a defect in this session's fix (see §6 🟡 of this snapshot for the full analysis and options for the owner).

---

1. retry_limit 住在 planner_config、key = retry_limit（schema 不動、oracle harness 不動）。
2. attempts 那條 failure_reason 雙向 CHECK 補丁還在 working tree 沒 commit，要獨立 commit 掉。 —— 已於 `5ab111e` 獨立 commit，此條已解決。
3. Session 8 要處理 migration 裡兩行註解會被 grep 誤判的事。
4. Session 3：`default_timeout_seconds` 在 `planner_config` 的鍵名是本 session 自行假設的，尚未經 owner 確認 —— Session 4/5 若要讀取 workflow-level timeout 預設值，請先向 owner 核實這個鍵名。 —— Session 5 補充：本 session 確實讀取了這個鍵（`GetWorkflow` 已在 Session 3 實作好 parse 邏輯），`effectiveTimeout()` 用它做 workflow-level fallback；鍵名本身仍未經 owner 正式核實，風險延續。
5. Session 4：`internal/api/server.go` 的兩處 `DeliverCallback` 呼叫點缺少 `ctx` 引數，以及 `cmd/stateflow/main.go` 的 `NewAsyncTransport()` 缺少 `store` 引數 —— 兩者都是本 session 訂出的新簽名的下游影響，留給碰到這些檔案的 session（很可能是 Session 5 或 6）修。 —— Session 5 補充：確認這兩處仍未修（out of scope，見本檔 §6 🟡），且 Session 5 自己又新增了兩個下游影響需要 Session 6 處理：`RecoverRuns`'s `makeLoop` 簽名多了 `workflowID` 參數（`cmd/stateflow/main.go` 的呼叫點需要同步更新)，以及 `internal/api` 若要建構 `orchestrator.Loop`，現在必須提供 `WorkflowID` 欄位。 —— **Session 6 補充：兩處都已修好**（`server.go` 的 `DeliverCallback` 呼叫已帶 `ctx`；`core.Result{Failure:...}` 取代了舊的 `Error` 欄位；`main.go` 的 `NewAsyncTransport(s)` 已帶 store 引數；`RecoverRuns` 現在直接吃 `buildLoop`，簽名完全吻合，見本檔 §4 偏離 7）。此條已解決。
6. Session 6：worker-side DLQ replay (TX5) 經 `Loop.Run()` 的 orphan-claim 路徑重入，非直接 dispatch —— 已於 Session 6.5 修復（新增 `Loop.ResumeReplayedStep`，見本檔流水帳）。此條已解決。
