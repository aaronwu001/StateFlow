# StateFlow v1.0 Refactor — State Snapshot

## Current Pointer

Just completed **Session 1 — Schema rewrite**. Next session: **Session 2**.

## Core Changes

Rewrote `migrations/001_initial.sql` in place to the v1.0 three-state schema, per whitepaper §14.1:

- `runs.status`: CHECK narrowed to `('RUNNING','DONE','DLQ')` — `FAILED` removed.
- `steps.status`: CHECK narrowed to `('RUNNING','DONE','DLQ')` — `DECIDED` and `FAILED` removed. Added `attempt_count INT NOT NULL DEFAULT 0` (the persisted retry budget referenced by TX3/TX5). Renamed `decided_at` → `created_at` (name retired with the DECIDED state). Kept `decision`/`output` JSONB as the TX1/TX2 barrier landing points, `seq`, and `current_attempt_id UUID` with no FK (insertion-order-cycle comment preserved, reworded to not reference the retired DECIDED state).
- `attempts`: dropped `attempt_number` (ordering is now by `created_at` only). Added `failure_reason TEXT CHECK IN ('worker_reported','timeout','malformed','orphaned')`, with an additional CHECK constraint (`attempts_failure_reason_required_when_failed`) enforcing non-null `failure_reason` whenever `status='FAILED'`. Renamed `dispatched_at` → `created_at` (row inserted at TX1/TX4, before actual dispatch — the timeout anchor per §6).
- `dead_letter_queue.reason`: CHECK updated to `('worker_retry_exhausted','planner_unreachable','planner_malformed','planner_declared_fail')`. `step_id` stays nullable (planner-side DLQ entries have no step at fault).
- Indexes: added `idx_steps_run_seq ON steps(run_id, seq)` (composite, supports the `last_step` lookup per §14.2) and `idx_runs_status ON runs(status)` (supports the recovery scan per §8/§14.2). Kept `idx_steps_run_id`, `idx_attempts_step_id`, `idx_dlq_run_id`.

No TX ledger logic (TX0/TX1/TX2/TX3/TX4/TX5/TX6/TX7/TX8/TX9/CAS-A) was implemented this session — Session 1 is schema-only; the store/orchestrator/api code that will write these transactions is untouched and still targets the old five-state schema (see Blockers below).

## Deviations & Blockers

**Deviations:**
- Reworded the `current_attempt_id` no-FK explanatory comment: original text cited "the DECIDED stage," which no longer exists in v1.0. Changed to "step creation (Barrier 1)." Rationale unchanged.

**Blockers / environment notes for the architecture checker:**
- A transient Docker Desktop hang (filtered `docker`/`docker compose` queries and image pulls stalling indefinitely) blocked verification earlier in this session. It was resolved by an external Docker Desktop restart (not a code fix) and **the completion condition has now been fully run and passed**: `docker compose down -v && docker compose up -d postgres` succeeded, and `psql \d` on all four non-`workflows` tables (`steps`, `attempts`, `runs`, `dead_letter_queue`) confirms every CHECK constraint, column rename, and index listed above is live. No outstanding verification gap.

**Out-of-scope needs (files that reference the old schema and will desync until their owning session lands):**
`internal/store/postgres.go`, `internal/store/postgres_test.go`, `internal/orchestrator/loop.go`, `internal/orchestrator/loop_test.go`, `internal/orchestrator/loop_integration_test.go`, `internal/orchestrator/recovery_test.go`, `internal/planner/http.go`, `internal/api/server.go`, `internal/api/server_test.go`, `internal/core/interfaces.go` — all still reference `decided_at`, `dispatched_at`, `attempt_number`, the `'DECIDED'` step status, or the old DLQ reason strings (`retry_exhausted`/`planner_failed`/`hard_failure`). These will not compile/pass against the new schema until Session 2+ rewrites them. This is expected per the Session 0 audit and rule 8 (old-model tests/code are expected to be rewritten in their owning session, not patched here).

**Open questions:** none outstanding — the one open item from the initial report (Docker health blocking live verification) is now resolved.
