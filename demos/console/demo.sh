#!/usr/bin/env bash
#
# The console demo - the read API, driven by hand.
#
# CLAUDE.md 4 step 2 asks for "literally: the commands the operator will type in
# a terminal, and the database state he must see afterwards", and CLAUDE.md 5.1
# permits exactly one source for an assertion - SPEC.md. Every check cites the
# section it came from.
#
# WHAT IT DEMONSTRATES
#   The two endpoints SPEC.md 10.2 designed and nothing had built:
#
#     GET /runs                   list runs, filterable by status
#     GET /runs/{run_id}/dlq      the append-only dead-letter history
#
#   and the one thing they exist for. Four runs are created; three of them end
#   in DLQ; and NOTHING IN runs.status SAYS WHY. Two died on their worker and
#   one died on its planner, and the only place that distinction is legible over
#   HTTP is the dead-letter `reason` (SPEC.md 12.3).
#
# HOW TO RUN IT
#   cd demos/console && ./demo.sh
#
#   Needs curl, jq and docker on the host; no psql, because it reaches the
#   database through `docker compose exec postgres psql`.
#
#   Exit status 0 means every assertion held.
#
# THE TWO PAUSE SWITCHES
#   Section 4 pauses the planner rather than declaring a failure in params. That
#   is the difference between this environment and every milestone before it:
#   the run is healthy when it starts and is interrupted in flight. It is also
#   the only way to reach a planner-side dead-letter entry on demand.
#
#   Paused means SILENT, not refusing: the connection is accepted and nothing is
#   written, so the call expires against its deadline and is classified
#   `timeout` (SPEC.md 5.3 decides timeout against transport_error by the clock,
#   not by the shape of the error).
#
# WHY THE TIMEOUTS ARE 5 SECONDS
#   workflow.json sets step_timeout_seconds and planner_timeout_seconds to 5.
#   SPEC.md 11.1 ranges both at >= 1 and defaults them to 300 and 30; at the
#   defaults this script would spend ten minutes waiting. SPEC.md 13.3
#   guarantees that while the run's owner is live, failure is declared PRECISELY
#   at the deadline, so 5 means 5.

set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

ORCH="localhost:8080"
WORKER="localhost:9090"
PLANNER="localhost:9100"
HEALTH_TIMEOUT=240
RUN_TIMEOUT=120
TEARDOWN_AT_END=0

usage() {
  cat <<'USAGE'
usage: ./demo.sh [--down] [--help]

  --down   tear the environment down (docker compose down -v) at the end,
           instead of leaving it up for inspection
  --help   this text

The environment is always torn down and rebuilt at the START of a run:
CLAUDE.md 5.5.2 requires a group to begin from a clean database, always.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --down) TEARDOWN_AT_END=1 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "demo.sh: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

for tool in docker curl jq; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "demo.sh: required tool not on PATH: $tool" >&2; exit 2; }
done

WF=""
RUN=""
RUN_DONE=""
RUN_WORKER=""
RUN_PLANNER=""
RUN_REPLAY=""
LAST_STATUS=""
PASSED=0
FAILED=0

hr()  { printf '\n===============================================================\n%s\n\n' "$*"; }
say() { printf '%s\n' "$*"; }

pgx() { docker compose exec -T postgres psql -U piton -d piton -v ON_ERROR_STOP=1 -v run="$RUN" "$@"; }
q()   { local sql="$1"; shift; printf '%s\n' "$sql" | pgx "$@" -At | tr -d '[:space:]'; }
show(){ local sql="$1"; shift; printf '\n%s\n' "$sql"; printf '%s\n' "$sql" | pgx "$@" || true; }

# check <label> <actual> <expected>
check() {
  local label="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    printf '  ok    %s\n' "$label"
    PASSED=$((PASSED + 1))
  else
    printf '  FAIL  %s\n          expected: %s\n          got:      %s\n' "$label" "$want" "$got"
    FAILED=$((FAILED + 1))
  fi
}

# start_run <input json> <expected terminal status>
#
# Reports through globals rather than by echoing: `X=$(start_run …)` would run
# the function in a command-substitution SUBSHELL, and every assignment it made
# would die with it.
start_run() {
  local input="$1" want="$2" run="" status="" deadline
  run=$(curl -sS -X POST "$ORCH/workflows/$WF/runs" -H 'content-type: application/json' \
          -d "{\"input\":$input,\"overrides\":{}}" | jq -r .run_id)
  if [ -z "$run" ] || [ "$run" = "null" ]; then
    say "FAILED: run creation returned no run_id" >&2
    return 1
  fi
  RUN="$run"
  deadline=$(( $(date +%s) + RUN_TIMEOUT ))
  while :; do
    status=$(q "SELECT status FROM runs WHERE run_id = :'run';")
    case "$status" in DONE|DLQ|CANCELLED) break ;; esac
    if [ "$(date +%s)" -ge "$deadline" ]; then
      say "FAILED: run $RUN was still $status after ${RUN_TIMEOUT}s" >&2
      return 1
    fi
    sleep 1
  done
  if [ "$status" != "$want" ]; then
    say "FAILED: run $RUN reached $status, but this demo needs $want" >&2
    return 1
  fi
  LAST_STATUS="$status"
}

finish() {
  local status=$?
  if [ "$TEARDOWN_AT_END" -eq 1 ]; then
    hr "TEARDOWN"
    docker compose down -v --remove-orphans || true
  else
    printf '\nThe environment is still up. Things to try by hand:\n\n'
    printf '  curl -sS localhost:8080/runs | jq .\n'
    printf '  curl -sS "localhost:8080/runs?status=DLQ" | jq .\n'
    printf '  curl -sS localhost:8080/runs/%s/dlq | jq .\n' "${RUN_PLANNER:-RUN_ID}"
    printf '  curl -X POST localhost:9090/pause     # break the worker\n'
    printf '  curl -X POST localhost:9100/pause     # break the planner\n'
    printf '  docker compose exec postgres psql -U piton -d piton\n\n'
    printf 'Tear it down with the volume wipe CLAUDE.md 5.5 requires:\n\n'
    printf '  docker compose down -v\n\n'
  fi
  exit "$status"
}
trap finish EXIT

# ---------------------------------------------------------------------------
# 1. Environment
# ---------------------------------------------------------------------------

hr "1. ENVIRONMENT"

say "Starting from a clean database (CLAUDE.md 5.5.2)."
docker compose down -v --remove-orphans >/dev/null 2>&1 || true

say "docker compose up -d --build"
if ! docker compose up -d --build --wait --wait-timeout "$HEALTH_TIMEOUT"; then
  say ""
  say "FAILED: the environment did not become healthy within ${HEALTH_TIMEOUT}s."
  say "If the failure names port 8080, another environment is still up from an"
  say "earlier hand-run. They all publish 8080."
  docker compose ps || true
  docker compose logs --tail=60 orchestrator || true
  exit 1
fi

deadline=$(( $(date +%s) + HEALTH_TIMEOUT ))
until curl -sS --max-time 5 "$ORCH/healthz" >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || { say "FAILED: /healthz never answered."; exit 1; }
  sleep 1
done

WF=$(curl -sS -X POST "$ORCH/workflows" -H 'content-type: application/json' \
       -d @workflow.json | jq -r .workflow_id)
[ -n "$WF" ] && [ "$WF" != "null" ] || { say "FAILED: POST /workflows returned no workflow_id"; exit 1; }
say ""
say "workflow_id = $WF"
say ""
say "One workflow drives every scenario below. The console planner decides from"
say "the RUN'S OWN INPUT - workflow_input.steps says how many steps to produce"
say "and workflow_input.worker becomes each step's params (SPEC.md 9.2) - so"
say "only the run's input changes from here on."

# ---------------------------------------------------------------------------
# 2. Four runs
# ---------------------------------------------------------------------------

hr "2. FOUR RUNS, THREE OF THEM DEAD"

say "a. a healthy two-step run"
start_run '{"steps":2,"worker":{"mode":"ok"}}' DONE
RUN_DONE="$RUN"; say "   $RUN_DONE -> $LAST_STATUS"

say "b. a worker that answers HTTP 500 until its budget is gone"
start_run '{"steps":1,"worker":{"mode":"http_500"}}' DLQ
RUN_WORKER="$RUN"; say "   $RUN_WORKER -> $LAST_STATUS"

say "c. the same, kept aside for the replay in section 5"
start_run '{"steps":1,"worker":{"mode":"http_500"}}' DLQ
RUN_REPLAY="$RUN"; say "   $RUN_REPLAY -> $LAST_STATUS"

say ""
say "d. the planner PAUSED, so its calls expire and the run dies before it ever"
say "   has a step. This is the one a params-declared failure cannot produce."
curl -sS -X POST "$PLANNER/pause" >/dev/null
start_run '{"steps":1,"worker":{"mode":"ok"}}' DLQ
RUN_PLANNER="$RUN"
curl -sS -X POST "$PLANNER/resume" >/dev/null
say "   $RUN_PLANNER -> $LAST_STATUS  (planner resumed)"

# ---------------------------------------------------------------------------
# 3. GET /runs
# ---------------------------------------------------------------------------

hr "3. GET /runs (SPEC.md 10.2)"

say '$ curl -sS localhost:8080/runs | jq ".runs[] | {run_id, status, created_at}"'
curl -sS "$ORCH/runs" | jq '.runs[] | {run_id, status, created_at}' || true

say ""
say "Assertions:"
check "every run is listed (SPEC.md 10.2)" \
      "$(curl -sS "$ORCH/runs" | jq '.runs | length')" "4"
check "newest first by created_at (SPEC.md 10.2)" \
      "$(curl -sS "$ORCH/runs" | jq -r '.runs[0].run_id')" "$RUN_PLANNER"
check "next_cursor is omitted when there are no more (SPEC.md 10.2)" \
      "$(curl -sS "$ORCH/runs" | jq 'has("next_cursor")')" "false"
check "?status=DONE returns only the healthy run (SPEC.md 10.2)" \
      "$(curl -sS "$ORCH/runs?status=DONE" | jq -r '(.runs | length), .runs[0].run_id' | tr '\n' ' ')" \
      "1 $RUN_DONE "
check "?status=DLQ returns the other three (SPEC.md 10.2)" \
      "$(curl -sS "$ORCH/runs?status=DLQ" | jq '.runs | length')" "3"
check "status is repeatable (SPEC.md 10.2)" \
      "$(curl -sS "$ORCH/runs?status=DONE&status=DLQ" | jq '.runs | length')" "4"
check "an unknown status is a 400, not an empty list (SPEC.md 10.2, 16)" \
      "$(curl -sS -o /dev/null -w '%{http_code}' "$ORCH/runs?status=NONSENSE")" "400"
check "and it says which slug (SPEC.md 10.5)" \
      "$(curl -sS "$ORCH/runs?status=NONSENSE" | jq -r .error)" "invalid_request"
check "limit is 1-200, so limit=0 is a 400 (SPEC.md 10.2)" \
      "$(curl -sS -o /dev/null -w '%{http_code}' "$ORCH/runs?limit=0")" "400"
check "limit=1 returns one run and offers a cursor (SPEC.md 10.2)" \
      "$(curl -sS "$ORCH/runs?limit=1" | jq -r '(.runs | length), (.next_cursor != null)' | tr '\n' ' ')" \
      "1 true "

say ""
say "Paging one run at a time must visit all four, in the same order, with none"
say "seen twice - which is what SPEC.md 10.2's fixed ordering exists to make"
say "possible: 'a cursor resumes exactly after the run it names'."
WALKED=""
CURSOR=""
for _ in 1 2 3 4 5; do
  if [ -z "$CURSOR" ]; then PAGE=$(curl -sS "$ORCH/runs?limit=1")
  else PAGE=$(curl -sS "$ORCH/runs?limit=1&cursor=$CURSOR"); fi
  ID=$(printf '%s' "$PAGE" | jq -r '.runs[0].run_id // empty')
  [ -n "$ID" ] || break
  WALKED="$WALKED$ID "
  CURSOR=$(printf '%s' "$PAGE" | jq -r '.next_cursor // empty')
  [ -n "$CURSOR" ] || break
done
check "the cursor walks every run exactly once (SPEC.md 10.2)" \
      "$WALKED" "$RUN_PLANNER $RUN_REPLAY $RUN_WORKER $RUN_DONE "

# ---------------------------------------------------------------------------
# 4. GET /runs/{run_id}/dlq
# ---------------------------------------------------------------------------

hr "4. GET /runs/{run_id}/dlq (SPEC.md 10.2, 6.5, 12.3)"

say "Three runs are DLQ and runs.status says nothing about why. This is where"
say "the difference lives."
say ""
say '$ curl -sS localhost:8080/runs/$RUN_WORKER/dlq | jq .entries'
curl -sS "$ORCH/runs/$RUN_WORKER/dlq" | jq '.entries[] | {reason, step_id, replay_round}' || true
say ""
say '$ curl -sS localhost:8080/runs/$RUN_PLANNER/dlq | jq .entries'
curl -sS "$ORCH/runs/$RUN_PLANNER/dlq" | jq '.entries[] | {reason, step_id, replay_round}' || true

say ""
say "Assertions:"
check "a run that stopped for no reason has an EMPTY list and a 200 (SPEC.md 10.2)" \
      "$(curl -sS -o /dev/null -w '%{http_code}' "$ORCH/runs/$RUN_DONE/dlq")" "200"
check "and the entries field is present and empty, not absent (SPEC.md 10.2)" \
      "$(curl -sS "$ORCH/runs/$RUN_DONE/dlq" | jq '.entries | length')" "0"
check "the worker-side entry is worker_budget_exhausted (SPEC.md 6.5, 12.3)" \
      "$(curl -sS "$ORCH/runs/$RUN_WORKER/dlq" | jq -r '.entries[0].reason')" \
      "worker_budget_exhausted"
check "and it names the step that exhausted its budget (SPEC.md 6.5)" \
      "$(curl -sS "$ORCH/runs/$RUN_WORKER/dlq" | jq -r '.entries[0].step_id')" \
      "$(q "SELECT step_id FROM steps WHERE run_id = :'run' AND seq = 1;" -v run="$RUN_WORKER")"
check "the planner-side entry is planner_budget_exhausted (SPEC.md 6.5, 12.3)" \
      "$(curl -sS "$ORCH/runs/$RUN_PLANNER/dlq" | jq -r '.entries[0].reason')" \
      "planner_budget_exhausted"
check "and its step_id is NULL - there was never a step (SPEC.md 6.5)" \
      "$(curl -sS "$ORCH/runs/$RUN_PLANNER/dlq" | jq -r '.entries[0].step_id')" "null"
check "a run that does not exist is a 404 (SPEC.md 10.5)" \
      "$(curl -sS -o /dev/null -w '%{http_code}' "$ORCH/runs/00000000-0000-0000-0000-000000000000/dlq")" \
      "404"

say ""
say "The database agrees, read the way SPEC.md 17.1 gives the operator:"
RUN="$RUN_PLANNER"
show "SELECT reason, step_id, replay_round, left(error_text, 60) AS error_text_60
        FROM dead_letter_queue WHERE run_id = :'run';"

# ---------------------------------------------------------------------------
# 5. The history accumulates
# ---------------------------------------------------------------------------

hr "5. REPLAY - THE HISTORY ACCUMULATES (SPEC.md 14, 10.2)"

say "SPEC.md 10.2 gives the dead-letter history its own endpoint because 'it"
say "accumulates across replay rounds'. Replaying a run whose worker is still"
say "broken is what makes that visible."
say ""

curl -sS -X POST "$ORCH/runs/$RUN_REPLAY/replay" -H 'content-type: application/json' -d '{}' | jq . || true
RUN="$RUN_REPLAY"
deadline=$(( $(date +%s) + RUN_TIMEOUT ))
while :; do
  s=$(q "SELECT status FROM runs WHERE run_id = :'run';")
  case "$s" in DONE|DLQ|CANCELLED) break ;; esac
  [ "$(date +%s)" -lt "$deadline" ] || break
  sleep 1
done

say ""
say '$ curl -sS localhost:8080/runs/$RUN_REPLAY/dlq | jq .entries'
curl -sS "$ORCH/runs/$RUN_REPLAY/dlq" | jq '.entries[] | {reason, replay_round}' || true

say ""
say "Assertions:"
check "the history now holds two entries (SPEC.md 10.2)" \
      "$(curl -sS "$ORCH/runs/$RUN_REPLAY/dlq" | jq '.entries | length')" "2"
check "oldest first, so round 0 precedes round 1 (SPEC.md 10.2)" \
      "$(curl -sS "$ORCH/runs/$RUN_REPLAY/dlq" | jq -r '[.entries[].replay_round] | join(",")')" "0,1"
check "a replay erases nothing (SPEC.md 14)" \
      "$(q "SELECT count(*) FROM dead_letter_queue WHERE run_id = :'run' AND replay_round = 0;")" "1"
check "attempts now span both rounds, and the wire says so (SPEC.md 6.4)" \
      "$(curl -sS "$ORCH/runs/$RUN_REPLAY/steps" | jq -r '[.steps[].attempts[].replay_round] | unique | join(",")')" \
      "0,1"

say ""
say "That last one is a field the database has always had and the API did not."
say "SPEC.md 14 resets steps.attempt_count on a replay, and SPEC.md 6.4 keeps"
say "attempt_no contiguous across rounds - so without replay_round nothing on"
say "the wire distinguishes an attempt of round 0 from one of round 1."

# ---------------------------------------------------------------------------
# 6. Result
# ---------------------------------------------------------------------------

hr "6. RESULT"

say "healthy        $RUN_DONE    -> DONE"
say "worker died    $RUN_WORKER  -> DLQ  worker_budget_exhausted"
say "planner died   $RUN_PLANNER -> DLQ  planner_budget_exhausted"
say "replayed       $RUN_REPLAY  -> DLQ  two entries, rounds 0 and 1"
say ""
say "assertions passed: $PASSED"
say "assertions failed: $FAILED"

if [ "$FAILED" -ne 0 ]; then
  say ""
  say "The read API did NOT land. Each failing assertion names the SPEC.md"
  say "section it came from; SPEC.md wins (CLAUDE.md 5.2)."
  exit 1
fi

say ""
say "Every assertion held."
say ""
say "CLAUDE.md 4 step 5 is not discharged by this script exiting 0. The owner"
say "reads the database himself, at a terminal - SPEC.md 17.4: a green suite the"
say "owner has never seen behind is not evidence that anything landed."
exit 0
