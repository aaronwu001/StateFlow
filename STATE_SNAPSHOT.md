# STATE_SNAPSHOT

## 1. 進度指針
- 剛完成 Session：`8 —— Final audit & conformance report (TX ledger conformance table, global sweeps, doc-vs-code diff, full verification) — REFACTOR COMPLETE, no further sessions planned`
- 下一個 Session：`None planned — Session 8 was the final session in the v1.0 refactor plan. One genuine defect was found (§6/§7 below) and is left for the owner to triage (fix now vs. accept as known-limitation).`
- 本次 commit SHA：`ab2c4f4274be566b23174efc1150f6a92addf447` (Session 8 is audit-only; no code was changed, so HEAD is unchanged from the follow-up commit this session started from. This snapshot commit adds only STATE_SNAPSHOT.md on top.)
- 分支：`main`

## 2. 驗證證據（verbatim）

Environment: Windows host, WSL2 Ubuntu distro has Docker/Go/Python. All commands
ran via `wsl.exe -d Ubuntu -- bash -lc '...'` against a live
`docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build`
stack started from a `docker compose down -v` state (fresh Postgres volume),
per this session's own prompt's documented environment quirks.

### 2a. 完成條件指令與輸出

**1. Global sweeps**

```text
$ grep -rn "decided_at\|dispatched_at\|attempt_number\|replay_round\|ResetToDecided" --include="*.go" --include="*.sql" --include="*.py" .
./internal/api/server.go:181:// (whitepaper §14.1: "renamed from decided_at") is surfaced under its new
./internal/api/server.go:182:// name; `decided_at` never appears on the wire.
./internal/api/server_test.go:228:			t.Error("step missing created_at (renamed from decided_at — must be present)")
./internal/api/server_test.go:230:		if _, ok := s0["decided_at"]; ok {
./internal/api/server_test.go:231:			t.Error("step has decided_at — retired name must never appear on the wire")
./migrations/001_initial.sql:34:    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),  -- renamed from decided_at (name retired with the DECIDED state)
./migrations/001_initial.sql:57:    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),  -- renamed from dispatched_at: inserted at TX1/TX4, before dispatch — the timeout anchor. No attempt_number; order by created_at
```
Every hit is a doc-comment/test-assertion referencing the *retired* name to
prove it is gone or to guard against its reappearance — zero hits are actual
usage. **Sweep verdict: clean**, matching the prompt's "must return nothing
[of substance]" bar (the only matches are the explicit anti-regression
guards this exact grep is designed to allow).

```text
$ grep -rn "DECIDED\|'FAILED'" --include="*.go" --include="*.sql" --include="*.py" .
./internal/orchestrator/helpers_test.go:149:   VALUES ($1::uuid, $2, 'FAILED', $3, 'seed: simulated pre-crash failure', now())
./internal/store/postgres.go:291:      SET status = 'FAILED', failure_reason = $1, error = $2, resolved_at = now()
./internal/api/server_test.go:586:     VALUES ($1::uuid, $2, 'FAILED', 'worker_reported', 'boom', now())
./internal/core/interfaces.go:45:// StepStatus is one of the step's three states (whitepaper §4.2). DECIDED
./demo/crash_demo.py:395:        f"AND a.status='FAILED' AND a.failure_reason='orphaned'"
./migrations/001_initial.sql:34:    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),  -- renamed from decided_at (name retired with the DECIDED state)
./migrations/001_initial.sql:54:    status          TEXT        NOT NULL CHECK (status IN ('RUNNING', 'DONE', 'FAILED')),
./migrations/001_initial.sql:60:        CHECK (status <> 'FAILED' OR failure_reason IS NOT NULL),
./migrations/001_initial.sql:62:        CHECK (status = 'FAILED' OR failure_reason IS NULL)
./test/acceptance/dlq_replay_test.py:63:        nf = H.scalar(f"SELECT count(*) FROM attempts WHERE step_id='{step_id}' AND status='FAILED'")
./test/acceptance/crash_recovery_test.py:106:                    f"AND a.status='FAILED' AND a.failure_reason='orphaned'")
./test/acceptance/crash_recovery_test.py:121:                   f"WHERE s.run_id='{run_id}' AND a.status='FAILED' AND a.failure_reason IS NULL")
./migrations/001_initial.sql:34: (dup of above)
```
Every hit is `attempts.status='FAILED'` — a real, current state for
**attempts** in the 3×3×3 model (never run/step) — or a doc comment
mentioning the retired step-state name `DECIDED` for historical
context. **Sweep verdict: clean**, matching the prompt's stated allowance
exactly ("the only legitimate hits are the attempts table's FAILED status").

```text
$ grep -rn 'attempt_count' --include='*.go' --include='*.sql' internal/ migrations/
```
(59 hits — full list retained in this session's own working notes, not
pasted verbatim here for length). Verified by direct read of
`internal/store/postgres.go`: the ONLY two non-initialization writers are
`RecordFailure` (TX3, `UPDATE steps SET attempt_count = attempt_count + 1 ...`,
line 309) and `ReplayWorkerSide` (TX5, `SET attempt_count = 0, ...`, line
417). `CreateStepWithAttempt` (TX1) sets the *initial* value to 0 as part of
row creation (matching the ledger's own TX1 content "count=0"), which is not
a second writer of an existing row. **Invariant verdict: holds** — "written
only by TX3 (++) and TX5 (=0)".

```text
$ grep -rn 'time\.Now()' --include='*.go' internal/ cmd/
internal/transport/async_test.go:175:	start := time.Now()
internal/orchestrator/helpers_test.go:161:	deadline := time.Now().Add(timeout)
internal/orchestrator/helpers_test.go:162:	for time.Now().Before(deadline) {
internal/orchestrator/loop.go:178:	dlqed, err := l.dispatchAndResolve(ctx, retry, def, retryLimit, stepID, spec, newAttemptID, time.Now())
internal/orchestrator/loop.go:262:	createdAt := time.Now() // see the package doc note on the timestamp approximation
internal/orchestrator/loop.go:399:	createdAt = time.Now()
internal/orchestrator/replay.go:105:	dlqed, err := l.dispatchAndResolve(ctx, retry, def, retryLimit, stepID, spec, frontier.PendingAttemptID, time.Now())
internal/planner/http_test.go:195:	start := time.Now()
internal/api/server_test.go:137:	deadline := time.Now().Add(timeout)
internal/api/server_test.go:138:	for time.Now().Before(deadline) {
```
Every production (non-`_test.go`) hit is in `internal/orchestrator/loop.go`
/`replay.go`, used ONLY to compute an in-memory `context.WithDeadline` input
(`createdAt`), immediately after a TX1/TX4/TX5 call returns — never passed
to any `StateStore` method, never persisted (confirmed: no `StateStore`
method in `internal/core/interfaces.go` accepts a `time.Time` parameter).
Test-file hits are polling-loop deadlines, not persisted state. **Invariant
verdict: holds** — no `time.Now()` is ever persisted for ordering; all
persisted timestamps are DB `now()` (verified directly in every
`postgres.go` TXn method: `now()` appears in every `created_at`/
`resolved_at`/`completed_at`/`updated_at` write, never a Go-side value).

**2. Clean-clone compose build** (`docker compose down -v` then
`docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build`):
```text
 Image stateflow-step1 Built
 Image stateflow-step2 Built
 Image stateflow-summarize-worker Built
 Image stateflow-llm-adapter Built
 Image stateflow-ner-worker Built
 Image stateflow-ocr-worker Built
 Image stateflow-stateflow Built
 Network stateflow_default Created
 Volume stateflow_pgdata Created
 Container stateflow-postgres-1 Created
 Container stateflow-ocr-worker-1 Created
 Container stateflow-ner-worker-1 Created
 Container stateflow-summarize-worker-1 Created
 Container stateflow-step1-1 Created
 Container stateflow-step2-1 Created
 Container stateflow-llm-adapter-1 Created
 Container stateflow-stateflow-1 Created
 Container stateflow-postgres-1 Healthy
 Container stateflow-stateflow-1 Started
```
`docker compose ... ps` afterward: all 8 containers `Up ... (healthy)` (or,
for `stateflow` itself, `Up` — it has no built-in healthcheck, Temporary
Design Registry item #8).

**3. `python3 demo/crash_demo.py`**
```text
════════════════════════════════════════════════════════════════
   StateFlow  —  Crash-Recovery Demo
════════════════════════════════════════════════════════════════
  Proves: kill orchestrator mid-run → restart → completed steps NOT re-run

  ✅ Images built
  ✅ Schema clean — 'stateflow' ready
  ✅ Workers ready  OCR:5001  NER:5002  Summarize:5003
  ✅ StateFlow (boot 1) ready on :8080  container=981ae4e1f948

     workflow_id : wf-b7186ab8-257c-4fda-aa0c-bb03cfc7ccc7
     run_id      : run-e640bd33-6e36-4e3e-b9ba-bb9ad892d6b7

[OCR] ✅ Extraction complete — 3 pages, confidence 0.98
[NER]  🏷️  Starting entity extraction
  ✅ Step 1 (OCR, sync) DONE  ✓
     NER dispatched — it is sleeping 5s before sending its callback

  💥 KILLING ORCHESTRATOR  —  container 981ae4e1f948
  💥 NER's async callback channel dies with the process
  💥 DB still shows step 2 RUNNING (no output); step 3 never started
[NER]  ✅ Extraction done — 3 entities found

  🔄 RESTARTING ORCHESTRATOR  —  RecoverRuns fires at startup
[NER]  ⚡ Already processed step_id=run-e640bd33-6e36-4e3e-b9ba-bb9ad892d6b7:ner
[NER]     Re-sending callback with NEW attempt_id=19f34611... (no re-processing)
[NER]  📤 Callback delivered — attempt_id=19f34611...  HTTP 200
[SUMMARIZE] ✍️  Generating summary from history: ['ocr', 'ner']
  ✅ StateFlow (boot 2 — recovery) ready on :8080  container=981ae4e1f948
[SUMMARIZE] ✅ Summary ready — 17 words
2026/07/10 14:12:18 INFO [RECOVERY] run completed run_id=run-e640bd33-6e36-4e3e-b9ba-bb9ad892d6b7

  ✅ NER step's attempt history shows exactly ONE attempt with failure_reason='orphaned'
  ✅ NER step's Barrier-1 record (created_at + decision) is byte-identical before and after the crash
  ✅ NER worker's actual extraction work ran exactly ONCE (idempotency cache absorbed the re-dispatch)

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
(Trimmed to the load-bearing lines; nothing truncated that affects
success/failure — full raw output was reviewed in full.)

**4. `./demo/run_demo.sh` scenarios 1–3** (same non-interactive technique as
Session 7: a throwaway in-place copy of `demo/run_demo.sh` with the trailing
`main "$@"` call stripped, functions invoked directly, blank-line stdin via
`yes ''` satisfying `pause()`'s `read -rp`; the copy was deleted immediately
after and `git status --porcelain demo/` confirmed clean — an incidental
`644→755` mode bit that `cp`/`sed` left on the **tracked** `run_demo.sh`
was caught and reverted with `chmod 644` before confirming clean, see §4
below).

Scenario 1 — Happy Path:
```text
   Run: run-087559fa-66e7-40dd-8e50-938c9213d024
   Status: DONE
   [DONE    ] ✓ step1
   [DONE    ] ✓ step2
   ℹ  LLM adapter was called 3 time(s)  (expected: 3)
   ✓  PASS — LLM-driven pipeline completed; adapter called 3×
```

Scenario 2 — Worker Crash & DLQ Replay:
```text
   Run: run-d1780927-1383-4494-85cb-33f38467bf57
   Status: DLQ
   [DONE    ] ✓ step1
   [DLQ     ] ✗ step2
   DLQ Entries:
   ID=1  run_id=run-d1780927-1383-4494-85cb-33f38467bf57  reason=worker_retry_exhausted  step=run-d1780927-1383-4494-85cb-33f38467bf57:step2
   ✓  OK — DLQ reason=worker_retry_exhausted, context carries per-attempt reason(s): ['timeout']
   ℹ  step1 invocation count before replay: 1
   ℹ  Replaying DLQ entry 1...
   Run: run-d1780927-1383-4494-85cb-33f38467bf57
   Status: DONE
   [DONE    ] ✓ step1
   [DONE    ] ✓ step2
   ℹ  step1 final invocation count: 1  (must be 1 — not re-run after replay)
   ✓  PASS — Run completed after DLQ replay; step1 not re-run (called 1×)
```

Scenario 3 — Orchestrator Crash & Recovery:
```text
   ⚠  KILLING orchestrator with 'docker compose kill' (SIGKILL)
   ⚠  Orchestrator dead. step1 is RUNNING in DB (Barrier 1 fired; Barrier 2 not yet).
   step_name | status  | dispatched
   -----------+---------+------------
   step1     | RUNNING | t
   (1 row)
   Recovery log:
   2026/07/10 16:08:02 INFO [RECOVERY] found in-progress runs count=1
   2026/07/10 16:08:02 INFO [RECOVERY] resuming run run_id=run-b4839b61-5da8-4f10-acba-ec2cea31595d steps_done=0 pending_step=step1 attempt_count=0
   2026/07/10 16:08:02 INFO [RECOVERY] complete resumed=1
   Run: run-b4839b61-5da8-4f10-acba-ec2cea31595d
   Status: DONE
   [DONE    ] ✓ step1
   [DONE    ] ✓ step2
   ℹ  LLM adapter call count: 3
   ✓  PASS — Recovery complete; adapter called 3× (≤3 — no extra re-decision)
```

**5. `TEST_DATABASE_URL=... go test -p 1 ./...`**
```text
$ docker run --rm -v "$(pwd):/src" -w /src -e GOFLAGS=-buildvcs=false \
    golang:1.25 sh -c "go build ./... && echo BUILD_OK && go vet ./... && echo VET_OK && gofmt -l ."
BUILD_OK
VET_OK
internal/planner/static_test.go
```
(`gofmt -l` flags exactly the one pre-existing file already flagged and
explicitly left untouched since Session 6.5; this session's own
`git status --porcelain internal/` is empty — see §3.)

```text
$ docker run --rm --network stateflow_default -v "$(pwd):/src" -w /src \
    -e GOFLAGS=-buildvcs=false \
    -e TEST_DATABASE_URL="postgres://stateflow:stateflow@postgres:5432/stateflow?sslmode=disable" \
    golang:1.25 go test -p 1 -count=1 ./...
?   	github.com/aaronwu000/stateflow/cmd/stateflow	[no test files]
ok  	github.com/aaronwu000/stateflow/internal/api	1.518s
?   	github.com/aaronwu000/stateflow/internal/core	[no test files]
ok  	github.com/aaronwu000/stateflow/internal/orchestrator	1.846s
ok  	github.com/aaronwu000/stateflow/internal/planner	0.218s
ok  	github.com/aaronwu000/stateflow/internal/store	1.376s
ok  	github.com/aaronwu000/stateflow/internal/transport	0.991s
```
6/6 packages `ok` (2 with no test files); 53 `func Test...` functions across
the DB-backed packages (`internal/api`, `internal/orchestrator`,
`internal/store`, `internal/transport`) per `grep -c 'func Test'`.

### 2b. 測試計數（照實填，來源即上面輸出）
- 套件測試：`6/6` packages `ok` (2 with no test files); 失敗：無
- 舊模型測試依計畫刪除/預期失敗者：無 — this session touched zero `.go`
  files (audit only; see §3), so no test was added, deleted, or modified.
- Demo/acceptance: `crash_demo.py` — PASS; `run_demo.sh` scenarios 1/2/3 —
  all PASS.

### 2c. Owner oracle（independently re-run, not trusted from prior self-reports）
```text
$ ADVERTISE_HOST=$(hostname -I | awk '{print $1}') ORCH_CONTAINER=stateflow-stateflow-1 \
  TEST_DATABASE_URL="postgres://stateflow:stateflow@localhost:5432/stateflow?sslmode=disable" \
  API_BASE=http://localhost:8080 python3 test/acceptance/crash_recovery_test.py
[setup] run_id=run-0d7c33eb-222d-4e28-ae85-3301594cf5db
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

$ ADVERTISE_HOST=$(hostname -I | awk '{print $1}') ORCH_CONTAINER=stateflow-stateflow-1 EXPECT_X=2 \
  TEST_DATABASE_URL="postgres://stateflow:stateflow@localhost:5432/stateflow?sslmode=disable" \
  API_BASE=http://localhost:8080 python3 test/acceptance/dlq_replay_test.py
[setup] run_id=run-4e7a3180-b448-4cfb-960c-d51d09c94e5e expecting X=2
  ok: run reached DLQ
  ok: step in DLQ with attempt_count=2
  ok: exactly 2 FAILED attempts
  ok: one DLQ row, reason=worker_retry_exhausted, context populated
  ok: replay reset attempt_count to 0 and returned step to RUNNING (TX5)
PASS: dlq_replay_test
```
Both oracles PASS end-to-end, independently re-run against a live stack
this session itself built from `docker compose down -v` — not taken on
faith from the `98f113d` self-report.

## 3. 動過的檔 / 故意沒碰的檔

```text
$ git status --porcelain
?? StateFlow_v1_ClaudeCode_Prompts.md   (owner's own untracked planning doc — not committed)
```
Zero `.go`, `.sql`, or demo/doc files were changed this session — this is an
audit session and the audit found no defect small enough / pre-approved
enough to fix without owner sign-off (see §6/§7: one real defect WAS found,
but per the Session 8 mandate it is reported, not fixed).

One incidental, non-content change was caught and reverted before it could
land in `git status`: copying `demo/run_demo.sh` to a throwaway in-place
copy (`cp` + `sed`) to drive scenarios 1–3 non-interactively left the
**original tracked file's** permission bits changed `100644→100755` (a
`cp`-then-`chmod`-inheritance quirk of the WSL9P filesystem layer, not a
content change — `git diff` showed only `old mode 100644` / `new mode
100755`, zero line diff). Caught via `git status --porcelain demo/` showing
`M demo/run_demo.sh` immediately after cleanup; fixed with `chmod 644
demo/run_demo.sh`; confirmed empty diff afterward. Flagged here in the
spirit of "verify before reporting" — an unnoticed mode-only diff would
have been an inaccurate "files changed: none" claim.

**`test/acceptance/` 的 git 狀態（必填，應為無變動）：**
```text
$ git status --porcelain test/acceptance/
(empty — no changes; a __pycache__/ directory left by running the oracles
 was removed before the final status check, not committed)
```

**`internal/`, `cmd/`, `migrations/` 的 git 狀態（本 session 不應碰到任何 .go 檔）：**
```text
$ git status --porcelain internal/ cmd/ migrations/
(empty — no changes)
```

## 4. 與 session 指示的偏離點

1. **Ran the two acceptance oracles under `ADVERTISE_HOST`/`ORCH_CONTAINER`
   env vars** exactly as the prompt's environment-quirks section anticipated
   — not itself a deviation, but noted because the prompt listed this as
   conditional ("if reaching the container ... needs ...") and it did in
   fact need it in this topology, consistent with every prior session that
   ran these oracles.
2. **Removed `test/acceptance/__pycache__/`** (Python bytecode cache
   generated by running the two oracle scripts) before the final
   `git status --porcelain` check. Not itself part of the repo (untracked,
   would never be committed) but removed as basic housekeeping so this
   session's "did you touch `test/acceptance/`" check reads unambiguously
   clean rather than merely "no tracked changes".
3. **Caught and reverted an incidental file-mode change** on
   `demo/run_demo.sh` (100644→100755, zero content diff) produced as a side
   effect of the `cp`/`sed` throwaway-copy technique — see §3. Not a content
   edit and not part of this session's audit findings; recorded here purely
   for completeness ("verify before reporting" applies to the audit's own
   footprint, not only to the target code).
4. **Did not attempt to fix the one genuine defect found** (§7 below: no
   `FailureReasonMalformed` path exists for async worker callbacks, despite
   whitepaper §4.2/§7.1 explicitly describing one). This is not a deviation
   from the prompt — it is the prompt's explicit instruction ("Code changes
   only if the audit uncovers a genuine defect, and only after you report it
   and get owner approval... this is the one session where 'fix it' is
   explicitly NOT your default action") — flagged here only so the
   zero-code-change claim in §1/§3 is not misread as "nothing was found".

## 5. 本次定案的真實介面 / Schema
No Go interfaces or schema were touched this session (confirmed empty
`git status --porcelain internal/ cmd/ migrations/` in §3). Session 8 is
audit-only per its own scope ("read everything; run everything").

### 5a. TX Ledger Conformance Table (this session's primary deliverable)

| ID | file:function | Transaction boundary (verbatim span) | Contents match ledger? |
|---|---|---|---|
| TX-W | `internal/store/postgres.go:67 CreateWorkflow` | Single `s.db.ExecContext(ctx, INSERT INTO workflows ...)` (no explicit `BeginTx` — a lone `ExecContext` is already atomic; doc comment: "a single INSERT is already atomic; no explicit BEGIN needed") | Yes — single write, workflow def incl. planner type+config |
| TX0 | `internal/store/postgres.go:116 CreateRun` | Single `s.db.ExecContext(ctx, INSERT INTO runs ... VALUES (..., 'RUNNING', ...))` | Yes — single write, run created RUNNING |
| TX1 | `internal/store/postgres.go:134 CreateStepWithAttempt` | `tx, err := s.db.BeginTx(ctx, nil)` (143) ... 3 `tx.ExecContext` calls (step INSERT w/ seq, attempt INSERT, `current_attempt_id` UPDATE) ... `tx.Commit()` (173) | Yes — step(decision,seq,count=0)+first attempt+current_attempt_id, one blade, dispatch happens only after the caller (loop.go:258-264) receives this call's return |
| TX2 | `internal/store/postgres.go:183 CheckpointSuccess` | `tx, err := s.db.BeginTx(ctx, nil)` (184) ... CAS `UPDATE attempts ... WHERE attempt_id=$1 AND status='RUNNING' AND EXISTS(...current_attempt_id...)` (190-199) ... `UPDATE steps SET output=..., status='DONE', completed_at=now()` (207-211) ... `tx.Commit()` (215) | Yes — attempt→done + step→done + output, one blade; CAS-gated |
| TX3 | `internal/store/postgres.go:282 RecordFailure` | `tx, err := s.db.BeginTx(ctx, nil)` (283) ... CAS `UPDATE attempts SET status='FAILED', failure_reason=$1,...` (289-298) ... `UPDATE steps SET attempt_count=attempt_count+1 RETURNING attempt_count,run_id` (308-312) ... **same tx**, `if newCount >= retryLimit`: `UPDATE steps SET status='DLQ'` (319) + `UPDATE runs SET status='DLQ'` (322) + `INSERT INTO dead_letter_queue (...,'worker_retry_exhausted',...)` (331-334) ... `tx.Commit()` (341) | Yes — attempt→failed(reason)+count++, DLQ blade fires inside the SAME transaction at count==retryLimit, exactly as the ledger requires ("Verdict and DLQ fall in one blade") |
| TX4 | `internal/store/postgres.go:350 StartNewAttempt` | `tx, err := s.db.BeginTx(ctx, nil)` (353) ... `INSERT INTO attempts (...) VALUES (...,'RUNNING')` (359-362) ... `UPDATE steps SET current_attempt_id=$1` (366-368) ... `tx.Commit()` (376) | Yes — new attempt + CAS-move of current_attempt_id, one blade, no dual-validity instant |
| TX5 | `internal/store/postgres.go:388 ReplayWorkerSide` | `tx, err := s.db.BeginTx(ctx, nil)` (389) ... find the DLQ step (396-404) ... `INSERT INTO attempts (...) VALUES (...,'RUNNING')` (408-413) ... `UPDATE steps SET attempt_count=0, status='RUNNING', current_attempt_id=$1, completed_at=NULL` (415-421) ... `UPDATE runs SET status='RUNNING'` (423-427) ... `tx.Commit()` (429) | Yes — count→0 + step→running + run→running + new attempt + current_attempt_id, all 5 writes in one blade. Actual worker dispatch correctly happens OUTSIDE this transaction, in `orchestrator/replay.go:59 ResumeReplayedStep` (a DB tx cannot contain an HTTP call) — matches the ledger's own convention (TX1/TX2 also describe "dispatch after commit" as a separate step from the tx's listed Contents) |
| TX6 | `internal/store/postgres.go:438 ReplayPlannerSide` | Single `s.db.ExecContext(ctx, UPDATE runs SET status='RUNNING' ...)` (no explicit `BeginTx` — doc comment: "a single UPDATE is already atomic") | Yes — run→running only |
| TX7 | `internal/store/postgres.go:454 MarkRunDone` | Single `s.db.ExecContext(ctx, UPDATE runs SET status='DONE' ...)` | Yes — run→done |
| TX8 | `internal/store/postgres.go:469 MarkRunDLQPlannerDeclared` | `tx, err := s.db.BeginTx(ctx, nil)` (470) ... `INSERT INTO dead_letter_queue (...,NULL,'planner_declared_fail',...)` (481-484) ... `UPDATE runs SET status='DLQ'` (488-490) ... `tx.Commit()` (via `return tx.Commit()`, 498) | Yes — run→DLQ + DLQ record (planner_declared_fail), one blade, called from `orchestrator/loop.go:244` on `PlannerVerdictFail` — no budget consumed (no retry loop precedes it) |
| TX9 | `internal/store/postgres.go:503 MarkRunDLQPlannerExhausted` | `tx, err := s.db.BeginTx(ctx, nil)` (504) ... `INSERT INTO dead_letter_queue (...,NULL,$2,...)` (515-518) ... `UPDATE runs SET status='DLQ'` (522-524) ... `tx.Commit()` (via `return tx.Commit()`, 532) | Yes — run→DLQ + DLQ record (reason=final attempt's class, full detail in context via `orchestrator/loop.go:458 decideWithBudget`'s 30s×3 loop), one blade |
| CAS-A | Embedded in `internal/store/postgres.go` `CheckpointSuccess` (190-199) and `RecordFailure` (289-298) | `UPDATE ... WHERE attempt_id = $1::uuid AND status = 'RUNNING' AND EXISTS (SELECT 1 FROM steps WHERE step_id=$2 AND current_attempt_id=$1::uuid)`; `RowsAffected()==0` → `ReportSuperseded`/`FailureOutcome{Report: ReportSuperseded}`, never an error | Yes — single conditional UPDATE, inherently atomic; realized as the WHERE-clause of TX2's/TX3's first statement rather than a standalone TX (correct: CAS-A is not itself a numbered ledger transaction, it's the mechanism inside TX2/TX3) |

All ten TXn methods were read in full (`internal/store/postgres.go`, 778
lines, entire file) and every `BeginTx`...`Commit` span was traced
statement-by-statement against its ledger row. No TX is split across two
Go functions, none merges two ledger entries, none reorders "commit → act"
into "act → commit". `defer tx.Rollback()` (no-op after a successful
`Commit()`, standard Go `database/sql` idiom) appears immediately after
every `BeginTx` in all seven multi-statement TXns — confirmed no code path
between `BeginTx` and `Commit` can return without either committing or
rolling back.

### 5b. Doc-vs-code diff, whitepaper §4–§11

- **§4 (The State Model):** Code matches exactly — `internal/core/interfaces.go`'s
  three closed enums (`RunStatus`/`StepStatus`/`AttemptStatus`) are
  byte-identical to `migrations/001_initial.sql`'s CHECK constraints and to
  the whitepaper's 3×3×3 table; `FailureReason`'s four values match §4.2's
  table. **One divergence found** (also affects §7, listed once here):
  whitepaper §4.2 states the `malformed` attempt-failure reason applies to
  "an async callback with valid ids but unparseable output", implying a code
  path that classifies some async `/tasks/complete` callback as
  `failed/malformed`. **No such path exists.** `grep -rn
  "FailureReasonMalformed" internal/` shows it is produced ONLY by
  `internal/transport/sync.go:97` (`SyncTransport.Dispatch`'s
  `extractOutput` gate). `internal/api/server.go:335 handleTaskComplete`
  decodes `req.Output` as an unvalidated `json.RawMessage` and unconditionally
  constructs `core.Result{Status: core.ResultStatusDone, Output: req.Output}`
  — there is no output-shape check, no `output_field` concept for async (per
  `core.StepSpec.OutputField`'s own doc comment: "sync only"), and therefore
  no way for an async callback with well-formed JSON ids to ever be classified
  `malformed`. Any `/tasks/complete` call with valid `step_id`/`attempt_id` is
  treated as success regardless of `output`'s content (including `output`
  being absent, which persists as a JSON `null` value, not SQL NULL). This is
  a real code/doc divergence, not a documentation nit — it means part of the
  attempt-state space the whitepaper describes as reachable (`failed`/
  `malformed` via async) is, in the current implementation, unreachable by
  design of the async path. See §6/§7 below.
- **§5 (The Step Loop):** Code matches — `internal/orchestrator/loop.go`'s
  `driveSteadyState` implements the 8-step loop verbatim (frontier read →
  planner ask → TX1 → dispatch → await → TX2/TX3 → planner done/fail →
  TX7/TX8), Barrier 1 and Barrier 2 both enforced as stated. **None found**
  beyond the §4/§7 async-malformed gap already noted.
- **§6 (The Timeout Doctrine):** Code matches — `systemDefaultTimeout = 60 *
  time.Second`, `effectiveTimeout()` resolves step>workflow>system exactly as
  documented, `plannerCallTimeout = 30s` / `plannerMaxAttempts = 3` match
  §7.2's budget verbatim, retry delay is `DefaultRetryPolicy(){Delay: 5 *
  time.Second}`. The creation-anchored deadline (`createdAt.Add(timeout)`,
  computed immediately after TX1/TX4/TX5 returns and passed via
  `context.WithDeadline`) makes §6's "the clock starts at attempt creation"
  literally true, as the whitepaper itself notes it should be. **None found.**
- **§7 (Failure Classification, Retry, and Budgets):** Worker side matches
  exactly (TX3's one-blade DLQ verdict, TX4's atomic handover, the
  crash-between-TX3-and-TX4 window explicitly claimed by recovery's budget
  check). Planner side matches exactly (`MalformedError` type +
  `errors.As` classification in `decideWithBudget`, `fail` verdict consuming
  no budget). **One divergence** — the same async-malformed gap noted under
  §4 belongs here too: §7.1's "Malformed edge cases" paragraph explicitly
  describes the async-unparseable-output case as a real path; it is not
  implemented. Also note (not a contradiction, a code-only detail the doc
  doesn't mention): `defaultRetryLimit = 3` (`internal/orchestrator/loop.go:33`)
  is used when a workflow's `planner_config` omits `retry_limit`. The
  whitepaper defines X as configurable per-workflow but states no fallback
  default the way it does for the 60s timeout; this constant is an
  implementation choice, already flagged as such in the code's own comment
  ("the whitepaper does not specify a default for X ... flagged in the
  Session 5 report"). Listed here as "behavior in code the doc doesn't
  mention" per this session's own instruction, not as a defect.
- **§8 (Invariants, the Combination Table, and Recovery):** Code matches —
  all six derived invariants hold by construction (verified via the TX
  table above: e.g. invariant 3 "attempt_count=X ⇒ step=DLQ ∧ run=DLQ" is
  exactly TX3's blade). `internal/orchestrator/recovery.go`'s `RecoverRuns`
  and `loop.go`'s one-time recovery check in `Run()` implement the
  run×last_step combination table's five legal rows and both impossible
  combinations are structurally unreachable (verified by reading, not just
  asserted — `run=done` is only ever set by `MarkRunDone`, called only after
  `PlannerVerdictDone`, which is only reached inside `driveSteadyState`'s
  loop top, itself only entered when `LoadFrontier`'s `PendingStep==nil`).
  Recovery's "skip the 5s retry delay" clause is satisfied by construction
  (no `time.After` call exists between `StartNewAttempt` and
  `dispatchAndResolve` in the recovery branch of `Run()`). **None found.**
- **§9 (Component Failure Overview):** Worker/Planner/Orchestrator rows
  match (§6/§7/§8 above cover their mechanisms). Storage-down row is an
  emergent property rather than an explicit code branch — no code
  specifically "detects storage down"; a failed write simply propagates a Go
  `error` up through `Loop.Run` (or a handler returns 500), leaving whatever
  was last committed as the true state, and recovery reclaims it on next
  restart exactly as recovery already does for any other crash. This
  matches the whitepaper's own framing ("the system halts as a whole" is a
  consequence, not a designed subsystem) — **none found**, but flagged as
  "implicit rather than explicit" for completeness.
- **§10 (Result Reporting: CAS and the Single Writer):** Code matches
  exactly — CAS-A verified in the TX table above; the single-writer
  principle is upheld by construction: `internal/api/server.go`'s
  `handleTaskComplete`/`handleTaskFail` call only `s.async.DeliverCallback`
  (validate + push + 200) and `internal/transport/async.go`'s
  `DeliverCallback` performs zero `StateStore` writes (grep confirms no
  `s.store.*` or DB write call anywhere in `async.go` — only a read,
  `LoadFrontier`, to check freshness before routing). All step/run state
  writes trace back to `orchestrator/loop.go`/`replay.go` alone. A success
  report arriving after a timeout verdict is correctly rejected by CAS-A
  (the timeout's TX3 already flipped the attempt to `FAILED`, so the late
  success's `CheckpointSuccess` CAS predicate `status='RUNNING'` no longer
  matches → `ReportSuperseded`). **None found.**
- **§11 (The DLQ and Replay):** Code matches — four DLQ reasons match
  `core.DLQReason`'s enum exactly; TX5/TX6 match the ledger table above;
  `replay_round` is confirmed absent from both schema and code (global
  sweep, §2a). `ResumeReplayedStep` (`internal/orchestrator/replay.go`)
  implements the "dispatch the worker" clause of TX5's whitepaper
  description correctly outside the transaction (a DB transaction cannot
  contain an HTTP call — see the TX5 table row above). **None found.**

**Overall: one genuine, reproducible code/doc divergence found** (async
callbacks never produce a `malformed` failure, contradicting whitepaper
§4.2 and §7.1's explicit description of that path) — reported in §6/§7
below per this session's "report, do not fix" mandate. No other
contradictions or undocumented behaviors were found in §4–§11.

## 6. 未解問題（分類 —— 這欄最重要）
- 🟡 已停下、需裁示：**One genuine defect found this session, NOT fixed per
  the Session 8 mandate** — whitepaper §4.2's attempt-state table and §7.1's
  "Malformed edge cases" paragraph both describe an async-worker path where
  "a callback with valid ids but unparseable output" is classified as
  `failed(malformed)`. No code implements this: `FailureReasonMalformed` is
  produced only by `internal/transport/sync.go`'s sync-only `extractOutput`
  gate; `internal/api/server.go:335 handleTaskComplete` and
  `internal/transport/async.go`'s `DeliverCallback` unconditionally treat
  any `/tasks/complete` call with valid `step_id`/`attempt_id` as a success,
  regardless of `output`'s content or absence. Options for the owner
  (framed, not decided, per this session's scope): (A) implement the
  documented behavior — define what "unparseable" means for an async
  payload (there is no `output_field` concept for async, unlike sync, so
  this needs its own acceptance rule, e.g. "output must be present and
  non-null" or similar) and add the classification in
  `handleTaskComplete`/`DeliverCallback`; (B) correct the whitepaper/rules
  docs to drop the async-malformed claim and document that async workers
  cannot currently produce a `malformed` verdict (only `worker_reported` via
  `/tasks/fail`, or `timeout`/`orphaned` via the loop); (C) accept as a
  known limitation and add it to the Temporary Design Registry (whitepaper
  §18) explicitly. This session did not choose an option, per its own
  "report it first, fix only with owner approval" rule. Full evidence:
  §5b above and §7 below.
- 🔴 我自行假設後繼續：
  1. Interpreted "the demo never relies on behavior listed in the Temporary
     Design Registry" (cross-session invariant checklist) as a spot-check
     against the registry's 8 items rather than an exhaustive proof; nothing
     found in `demo/`/`test/acceptance/` contradicts it (e.g. full-history
     transmission, registry item #1, is used by the demos but that is the
     registered/accepted MVP behavior, not a violation of it).
  2. Treated the `defaultRetryLimit = 3` fallback (§5b, §7) as "undocumented
     but already self-flagged in code" rather than a fresh finding requiring
     its own owner question, since the code's own comment already discloses
     it and attributes it to the Session 5 report.
- 無其他：everything else in this session's instructions (TX ledger table,
  global sweeps, doc-vs-code diff for §4–§11, full verification, both
  acceptance oracles) was directly executable and is reported in full above.

## 7. CONFIRM 值（unchanged this session）
- planner_config 內 HTTP planner URL 的欄位名：`url`（no action needed）
- retry limit X 在 planner_config 的欄位名：`retry_limit`（no action needed）
- `POST /workflows` / `POST /runs` 回傳的 id 欄位名：`workflow_id` / `run_id`（no action needed）
- workflow-level timeout override 的欄位名：`default_timeout_seconds`（unchanged
  since Session 7; still consistent, no contradicting evidence found this
  session either）

**Open questions for the owner (Session 8, final):**
1. The async-malformed gap above (§6 🟡) — which of options A/B/C, or
   another option the owner prefers.
2. Carried over from Session 7 (§7 of that session's own snapshot, restated
   here since it was never explicitly resolved): should `README.md`'s "Fast
   workers (<30s)" line be updated to avoid implying a stale 30s system
   timeout default? This session did not re-investigate it (out of the
   audit's file scope — README.md prose beyond doc-vs-code correctness was
   not this session's focus — but it is repeated here since Session 8 is
   explicitly the last chance to surface it before the refactor plan ends).
3. Whether the `defaultRetryLimit = 3` fallback (§5b/§6) should be promoted
   from an implementation-only constant to a documented whitepaper default,
   given the timeout knob already has one (60s) and this asymmetry is now
   visible to an external reader of the whitepaper who has no access to the
   code comment explaining it.

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
