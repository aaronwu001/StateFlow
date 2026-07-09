# Acceptance oracles (owner-owned; read-only to Claude Code)

These are the frozen correctness oracles for StateFlow's crash-recovery and DLQ
paths. They are **not** part of any Claude Code session's writable scope. Claude
Code must run them where the session prompts require, and make them pass without
editing them. If one fails, the orchestrator is wrong.

Self-contained: they launch a fake HTTP planner (`fake_planner.py`) and a fake
worker (`fake_worker.py`) as local subprocesses — no dependency on `demo/`.

## Status: DRAFT until smoke-tested
Written before the stack was runnable. Before freezing (Session 6), run each once
against the live stack and confirm the assertions match intent. Three spots are
marked **CONFIRM** (they depend on impl details not fixed by the whitepaper):
- `_harness.PLANNER_CONFIG_KEY` — the key inside `planner_config` holding the
  HTTP planner URL (env `PLANNER_CONFIG_KEY`, default `url`).
- `dlq_replay_test.DLQ_EXTRA_CONFIG_JSON` — how the retry limit X is set in
  `planner_config` (default `{"retry_limit": 2}`).
- The `POST /workflows` / `POST /runs` response field names (`workflow_id`/`run_id`
  vs `id`) — the harness accepts either.

## Run
```bash
docker compose up -d          # orchestrator + postgres (demo workers NOT needed)
export TEST_DATABASE_URL="postgres://stateflow:stateflow@localhost:5432/stateflow?sslmode=disable"
export API_BASE=http://localhost:8080
# Linux only: start the orchestrator container with
#   --add-host=host.docker.internal:host-gateway
# (or set ADVERTISE_HOST to the docker bridge IP) so it can reach the host fakes.

python3 test/acceptance/crash_recovery_test.py
EXPECT_X=2 python3 test/acceptance/dlq_replay_test.py
```
Each prints `PASS: <name>` on success, or `FAIL: ...` and exits non-zero.
