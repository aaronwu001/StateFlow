#!/usr/bin/env bash
#
# Milestone delta - the demo script.
#
# CLAUDE.md 4 step 2 asks for "literally: the commands the operator will type in
# a terminal, and the database state he must see afterwards". This is that.
#
# WHAT IT IS NOT
#   SPEC.md 18.4 DOES NOT EXIST. Alpha's, gamma's and beta's demo scripts each
#   have a ratified section behind them; this one does not, because CLAUDE.md 2
#   rule 1 forbids this agent from writing into SPEC.md on its own initiative.
#   So every assertion below cites a section that IS ratified - SPEC.md 14,
#   10.1, 10.5, 12.2, 12.3, 6.2, 6.3, 6.4, 6.5, 6.7, 5.5, 5.6, 4.2 - and none
#   cites a demo script. If the owner ratifies an 18.4 that differs from this,
#   the file changes and the assertions do not have to.
#
# WHAT IT DEMONSTRATES
#   An operator whose run has stopped in the dead-letter queue replays it, and
#   watches it resume from where it stopped - while everything that recorded the
#   failure stays exactly where it is.
#
#     leg 1  a worker-side DLQ run is replayed; the step is re-dispatched, it
#            succeeds, and the run walks on to the step that never existed
#     leg 2  two rounds. Round 0 dies at step 1, round 1 dies at step 2, round 2
#            finishes - and step 1 is never revisited (SPEC.md 14's accepted
#            limitation)
#     leg 3  eight replays of one run fired at once: exactly one winner
#     leg 4  a replay caught in flight - run RUNNING, step RUNNING, budget reset
#            then burned once - and a second replay refused with 409 RUNNING
#     leg 5  replaying a DONE run is a 409 that names the run AND its step
#     leg 6  replaying a run_id that names no run is a 404
#
# WHAT DELTA IS SCOPED TO
#   Worker-side replay only - SPEC.md 12.3's left-hand column. The planner-side
#   column (L5) is left to milestone zeta, because SPEC.md 12.1 makes it
#   unreachable with the planners that exist: "the static planner simply cannot
#   fail at run time... so planner_attempt_count never leaves 0", and SPEC.md
#   6.1 says it never answers `fail`. The owner ruled the same way for gamma
#   (R32-a) and again for delta.
#
#   SPEC.md 14's CANCELLED row is not demonstrated either: cancellation is
#   SPEC.md 15, at milestone iota, and POST /runs/{run_id}/cancel does not
#   exist. Both absences are asserted rather than left silent.
#
# HOW TO RUN IT
#   cd demos/delta && ./demo.sh
#
#   Everything runs inside WSL (CLAUDE.md 8). The script needs curl, jq and
#   docker on the host; it needs no psql, because it reaches the database
#   through `docker compose exec postgres psql`.
#
#   Exit status 0 means every assertion held. Any other status means the
#   milestone did not land, and the failing assertion is named on stdout.
#
# WHAT THE SCRIPT PRINTS, AND IN WHICH VOICE
#   Each leg prints its queries and their output unedited, for the operator's
#   eye. That is step 5 of CLAUDE.md 4 - the hand-run inspection the automated
#   suite may never replace. It then asserts.

set -uo pipefail

ORCH="http://localhost:8080"
HEALTH_TIMEOUT=240
RUN_TIMEOUT=300
TEARDOWN_AT_END=0

RUN_INPUT='{"text":"hello"}'

# Leg 3 fires this many replays at once. SPEC.md 14: "a double-click has exactly
# one winner".
GATE_REPLAYS=8

# A syntactically well-formed identifier that names no run, for leg 6. A
# malformed one would be testing SPEC.md 10.5's 400 instead of its 404.
MISSING_RUN_ID="00000000-0000-0000-0000-0000000000ff"

usage() {
  cat <<'USAGE'
usage: ./demo.sh [--down] [--help]

  --down   tear the environment down (docker compose down -v) when the script
           finishes, instead of leaving it up for inspection
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

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

FAILURES=0
ASSERTIONS=0

# ---------------------------------------------------------------------------
# Plumbing
# ---------------------------------------------------------------------------

banner() { printf '\n\033[1m=== %s ===\033[0m\n' "$*"; }
note()   { printf '  %s\n' "$*"; }

# psql: SQL on stdin, never with -c - psql substitutes :'name' only for input it
# reads through its normal lexer. This is SPEC.md 17.1's access path, and the
# one the automated suite uses.
psql_q() {
  local sql="$1"; shift
  local args=(exec -T postgres psql -U piton -d piton -v ON_ERROR_STOP=1 -At)
  local v
  for v in "$@"; do args+=(-v "$v"); done
  printf '%s' "$sql" | docker compose "${args[@]}"
}

# show prints a query and its output, unedited, for the operator to read.
show() {
  local label="$1" sql="$2"; shift 2
  printf '\n  -- %s\n' "$label"
  printf '%s\n' "$sql" | sed 's/^/     /'
  psql_q "$sql" "$@" | sed 's/^/     > /'
}

# assert_true runs a query that must return exactly one boolean.
assert_true() {
  local why="$1" sql="$2"; shift 2
  ASSERTIONS=$((ASSERTIONS + 1))
  local out
  out="$(psql_q "$sql" "$@")"
  if [ "$out" = "t" ]; then
    printf '  \033[32mPASS\033[0m %s\n' "$why"
  else
    printf '  \033[31mFAIL\033[0m %s\n        expected t, got %s\n        query: %s\n' \
           "$why" "${out:-<no row>}" "$sql"
    FAILURES=$((FAILURES + 1))
  fi
}

# assert_eq compares two values the script itself has read.
assert_eq() {
  local why="$1" want="$2" got="$3"
  ASSERTIONS=$((ASSERTIONS + 1))
  if [ "$want" = "$got" ]; then
    printf '  \033[32mPASS\033[0m %s\n' "$why"
  else
    printf '  \033[31mFAIL\033[0m %s\n        want: %s\n        got:  %s\n' "$why" "$want" "$got"
    FAILURES=$((FAILURES + 1))
  fi
}

create_workflow() {
  curl -sS -X POST "$ORCH/workflows" -H 'content-type: application/json' \
       --data-binary "@$1" | jq -r '.workflow_id // empty'
}

start_run() {
  curl -sS -X POST "$ORCH/workflows/$1/runs" -H 'content-type: application/json' \
       -d "{\"input\":$RUN_INPUT,\"overrides\":{}}" | jq -r '.run_id // empty'
}

begin() {
  local wf; wf="$(create_workflow "$1")"
  [ -n "$wf" ] || { echo "demo.sh: POST /workflows failed for $1" >&2; exit 1; }
  local run; run="$(start_run "$wf")"
  [ -n "$run" ] || { echo "demo.sh: POST /workflows/{id}/runs failed for $1" >&2; exit 1; }
  printf '%s' "$run"
}

# replay issues SPEC.md 10.1's POST /runs/{run_id}/replay and prints
# "<code> <body>". The code is printed rather than checked here because SPEC.md
# 10.5 enumerates codes for REFUSALS only: no ruling fixes what a performed
# replay returns, so this script accepts any 2xx and asserts the EFFECT in the
# database, which is specified.
replay() {
  curl -sS -o /tmp/piton-delta-replay.$$ -w '%{http_code}' \
       -X POST "$ORCH/runs/$1/replay"
  printf ' '
  cat /tmp/piton-delta-replay.$$
  rm -f /tmp/piton-delta-replay.$$
}

replay_code() { replay "$1" | awk '{print $1}'; }

is_2xx() { case "$1" in 2??) return 0 ;; *) return 1 ;; esac; }

# await polls a SQL predicate until it is true. Every wait here is written this
# way rather than as a sleep: SPEC.md 14 hands a replayed run to "the next
# sweep" and SPEC.md 8.6 fixes no instant at which that sweep runs, so sleeping
# for a computed duration would be asserting a schedule SPEC.md does not
# promise.
await() {
  local what="$1" sql="$2" timeout="$3"; shift 3
  local deadline=$(( $(date +%s) + timeout )) out
  while :; do
    out="$(psql_q "$sql" "$@" 2>/dev/null)"
    [ "$out" = "t" ] && return 0
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "demo.sh: timed out after ${timeout}s waiting for $what (last: ${out:-<none>})" >&2
      return 1
    fi
    sleep 0.5
  done
}

await_terminal() {
  await "run $1 to leave RUNNING" \
        "SELECT status <> 'RUNNING' FROM runs WHERE run_id = :'run';" "$RUN_TIMEOUT" "run=$1"
}

run_status() { psql_q "SELECT status FROM runs WHERE run_id = :'run';" "run=$1"; }

# ---------------------------------------------------------------------------
# Bring the environment up
# ---------------------------------------------------------------------------

banner "environment"
note "CLAUDE.md 5.5.2: a run always begins from a clean database."
docker compose down -v --remove-orphans >/dev/null 2>&1
docker compose up -d --build --wait --wait-timeout "$HEALTH_TIMEOUT" || {
  echo "demo.sh: the environment did not come up" >&2
  docker compose logs --tail=60 orchestrator >&2
  exit 1
}

note "waiting for GET /healthz - there is no migration service, so a 200 here"
note "is what proves the migrations finished before traffic was served."
deadline=$(( $(date +%s) + HEALTH_TIMEOUT ))
until curl -sf "$ORCH/healthz" >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || { echo "demo.sh: /healthz never answered 200" >&2; exit 1; }
  sleep 1
done
note "healthy."

# ---------------------------------------------------------------------------
# Leg 1 - a worker-side dead letter is replayed, and the run finishes
# ---------------------------------------------------------------------------

banner "leg 1 - replay resumes a worker-side dead letter (SPEC.md 14, 12.3)"

RUN1="$(begin workflow-replay-resumes.json)"
note "run_id = $RUN1"
await_terminal "$RUN1" || exit 1
assert_eq "SPEC.md 12.2: the run stopped in the dead-letter queue" "DLQ" "$(run_status "$RUN1")"

show "the state SPEC.md 5.5 calls L4 - run DLQ, last step DLQ" \
  "SELECT r.status AS run, s.seq, s.step_name, s.status AS step, s.attempt_count
     FROM runs r JOIN steps s ON s.run_id = r.run_id
    WHERE r.run_id = :'run' ORDER BY s.seq;" "run=$RUN1"

show "SPEC.md 6.5: the dead-letter entry, and the round it belonged to" \
  "SELECT reason, replay_round, left(error_text, 60) AS error_text
     FROM dead_letter_queue WHERE run_id = :'run';" "run=$RUN1"

# SPEC.md 6.5 no longer copies the budget consumed onto the entry. How much the
# failed round burned is a count of the attempts carrying its replay_round, read
# from the table that owns the number (SPEC.md 6.4).
show "SPEC.md 6.4, 6.5: how many attempts that round actually burned" \
  "SELECT a.replay_round, count(*) AS attempts_in_that_round
     FROM attempts a WHERE a.run_id = :'run' GROUP BY 1 ORDER BY 1;" "run=$RUN1"

STEPS_BEFORE="$(psql_q "SELECT count(*) FROM steps WHERE run_id = :'run';" "run=$RUN1")"
DLQ_BEFORE="$(psql_q "SELECT md5(string_agg(dlq_id::text || reason || replay_round ||
                                            error_text || created_at::text, ',' ORDER BY dlq_id))
                        FROM dead_letter_queue WHERE run_id = :'run';" "run=$RUN1")"

note ""
note "the command the operator types (SPEC.md 10.1):"
note "  curl -X POST $ORCH/runs/$RUN1/replay"
CODE1="$(replay_code "$RUN1")"
note "  -> HTTP $CODE1"
ASSERTIONS=$((ASSERTIONS + 1))
if is_2xx "$CODE1"; then
  printf '  \033[32mPASS\033[0m %s\n' "SPEC.md 14: a run whose status is DLQ is replayed, not refused"
else
  printf '  \033[31mFAIL\033[0m %s\n        got HTTP %s\n' \
         "SPEC.md 14: a run whose status is DLQ is replayed, not refused" "$CODE1"
  FAILURES=$((FAILURES + 1))
fi

await_terminal "$RUN1" || exit 1

show "after the replay - the run walked on to the step that never existed" \
  "SELECT r.status AS run, r.replay_count, s.seq, s.step_name, s.status AS step, s.attempt_count,
          (SELECT count(*) FROM attempts a WHERE a.step_id = s.step_id) AS attempt_rows
     FROM runs r JOIN steps s ON s.run_id = r.run_id
    WHERE r.run_id = :'run' ORDER BY s.seq;" "run=$RUN1"

assert_true "SPEC.md 14, 12.3: the replayed run re-dispatched its step and reached DONE" \
  "SELECT status = 'DONE' FROM runs WHERE run_id = :'run';" "run=$RUN1"
assert_true "SPEC.md 14, 6.2: replay_count = 1" \
  "SELECT replay_count = 1 FROM runs WHERE run_id = :'run';" "run=$RUN1"
assert_true "SPEC.md 14, 4.2: attempt_count was reset to 0 and burned once, so it reads 1" \
  "SELECT attempt_count = 1 AND status = 'DONE' FROM steps
    WHERE run_id = :'run' AND step_name = 'stumbles';" "run=$RUN1"
assert_true "SPEC.md 6.3: the attempts rows are left in place - three of them, against a count of 1" \
  "SELECT count(*) = 3 FROM attempts a JOIN steps s ON s.step_id = a.step_id
    WHERE a.run_id = :'run' AND s.step_name = 'stumbles';" "run=$RUN1"
assert_eq "SPEC.md 12.2: while it was in DLQ, only the failing step existed" "1" "$STEPS_BEFORE"
assert_true "SPEC.md 4.2: the planner was asked again, so both static steps now exist and are DONE" \
  "SELECT count(*) = 2 AND bool_and(status = 'DONE') FROM steps WHERE run_id = :'run';" "run=$RUN1"
assert_eq "SPEC.md 6.7, 12.4: the dead-letter entry is byte-identical after the replay" \
  "$DLQ_BEFORE" \
  "$(psql_q "SELECT md5(string_agg(dlq_id::text || reason || replay_round ||
                                   error_text || created_at::text, ',' ORDER BY dlq_id))
               FROM dead_letter_queue WHERE run_id = :'run';" "run=$RUN1")"

# ---------------------------------------------------------------------------
# Leg 2 - two rounds, and the accepted limitation
# ---------------------------------------------------------------------------

banner "leg 2 - two rounds; replay resumes at the furthest step reached (SPEC.md 14)"
note "SPEC.md 14: 'a replay always resumes at the furthest step reached. If round 1"
note "failed at step 1 and round 2 fails at step 2, replay resumes from step 2 and"
note "never revisits step 1.'"

RUN2="$(begin workflow-replay-two-rounds.json)"
note "run_id = $RUN2"
await_terminal "$RUN2" || exit 1
assert_true "round 0 died at the FIRST step" \
  "SELECT status = 'DLQ' FROM steps WHERE run_id = :'run' AND step_name = 'first';" "run=$RUN2"

note "  curl -X POST $ORCH/runs/$RUN2/replay      # round 1"
CODE2A="$(replay_code "$RUN2")"; note "  -> HTTP $CODE2A"
await_terminal "$RUN2" || exit 1
assert_true "round 1 died at the SECOND step" \
  "SELECT status = 'DLQ' FROM steps WHERE run_id = :'run' AND step_name = 'second';" "run=$RUN2"

# Everything about step 1 that a re-dispatch would change.
FIRST_BEFORE="$(psql_q "SELECT s.status || '|' || s.attempt_count || '|' || coalesce(s.completed_at::text,'') ||
                               '|' || coalesce((SELECT string_agg(a.attempt_id::text || ':' || a.status, ','
                                                                  ORDER BY a.attempt_no)
                                                  FROM attempts a WHERE a.step_id = s.step_id), '')
                          FROM steps s WHERE s.run_id = :'run' AND s.step_name = 'first';" "run=$RUN2")"

show "SPEC.md 6.5: two rounds, two entries, and which step each one names" \
  "SELECT d.replay_round, s.seq, s.step_name, d.reason,
          (SELECT count(*) FROM attempts a
            WHERE a.step_id = d.step_id AND a.replay_round = d.replay_round) AS attempts_that_round
     FROM dead_letter_queue d JOIN steps s ON s.step_id = d.step_id
    WHERE d.run_id = :'run' ORDER BY d.replay_round;" "run=$RUN2"

note "  curl -X POST $ORCH/runs/$RUN2/replay      # round 2"
CODE2B="$(replay_code "$RUN2")"; note "  -> HTTP $CODE2B"
await_terminal "$RUN2" || exit 1

show "after the second replay - step 'first' is exactly as it was" \
  "SELECT s.seq, s.step_name, s.status, s.attempt_count, s.completed_at,
          (SELECT count(*) FROM attempts a WHERE a.step_id = s.step_id) AS attempt_rows
     FROM steps s WHERE s.run_id = :'run' ORDER BY s.seq;" "run=$RUN2"

assert_eq "SPEC.md 14: the second replay never revisited step 'first'" \
  "$FIRST_BEFORE" \
  "$(psql_q "SELECT s.status || '|' || s.attempt_count || '|' || coalesce(s.completed_at::text,'') ||
                    '|' || coalesce((SELECT string_agg(a.attempt_id::text || ':' || a.status, ','
                                                       ORDER BY a.attempt_no)
                                       FROM attempts a WHERE a.step_id = s.step_id), '')
               FROM steps s WHERE s.run_id = :'run' AND s.step_name = 'first';" "run=$RUN2")"
assert_true "SPEC.md 14, 6.2: two rounds completed, so replay_count = 2" \
  "SELECT replay_count = 2 FROM runs WHERE run_id = :'run';" "run=$RUN2"
assert_true "SPEC.md 6.5: the entries are numbered round 0 and round 1" \
  "SELECT array_agg(replay_round ORDER BY replay_round) = ARRAY[0,1]
     FROM dead_letter_queue WHERE run_id = :'run';" "run=$RUN2"
assert_true "SPEC.md 6.5, 12.3: round 0 names step 1 and round 1 names step 2" \
  "SELECT (SELECT s.seq FROM steps s WHERE s.step_id = d0.step_id) = 1
      AND (SELECT s.seq FROM steps s WHERE s.step_id = d1.step_id) = 2
     FROM dead_letter_queue d0, dead_letter_queue d1
    WHERE d0.run_id = :'run' AND d0.replay_round = 0
      AND d1.run_id = :'run' AND d1.replay_round = 1;" "run=$RUN2"
assert_true "SPEC.md 14: the run finished" \
  "SELECT status = 'DONE' FROM runs WHERE run_id = :'run';" "run=$RUN2"

# ---------------------------------------------------------------------------
# Leg 3 - the idempotency gate
# ---------------------------------------------------------------------------

banner "leg 3 - a double-click has exactly one winner (SPEC.md 14)"
note "SPEC.md 14: 'the transaction that takes the run out of DLQ IS the gate, so a"
note "double-click has exactly one winner.'"

RUN3="$(begin workflow-replay-gate.json)"
note "run_id = $RUN3"
await_terminal "$RUN3" || exit 1
assert_eq "the run is in DLQ when the burst arrives" "DLQ" "$(run_status "$RUN3")"

note ""
note "  for i in \$(seq $GATE_REPLAYS); do curl -X POST $ORCH/runs/$RUN3/replay & done; wait"
CODES_FILE="$(mktemp)"
for _ in $(seq "$GATE_REPLAYS"); do
  ( curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$ORCH/runs/$RUN3/replay" >> "$CODES_FILE" ) &
done
wait
note "  the $GATE_REPLAYS response codes:"
sort "$CODES_FILE" | uniq -c | sed 's/^/     /'
WINNERS="$(grep -c '^2' "$CODES_FILE" || true)"
CONFLICTS="$(grep -c '^409' "$CODES_FILE" || true)"
rm -f "$CODES_FILE"

assert_eq "SPEC.md 14: exactly one replay was accepted" "1" "$WINNERS"
assert_eq "SPEC.md 14, 10.5: every other replay was a 409" "$((GATE_REPLAYS - 1))" "$CONFLICTS"
await_terminal "$RUN3" || exit 1
assert_true "SPEC.md 14, 6.2: the burst advanced replay_count by exactly 1" \
  "SELECT replay_count = 1 FROM runs WHERE run_id = :'run';" "run=$RUN3"
assert_true "SPEC.md 12.4: the losers wrote nothing - one entry, as before" \
  "SELECT count(*) = 1 FROM dead_letter_queue WHERE run_id = :'run';" "run=$RUN3"
assert_true "SPEC.md 14: the one winner carried the run to DONE" \
  "SELECT status = 'DONE' FROM runs WHERE run_id = :'run';" "run=$RUN3"

# ---------------------------------------------------------------------------
# Leg 4 - a replay caught in flight
# ---------------------------------------------------------------------------

banner "leg 4 - the state a replay leaves behind, seen while it is true (SPEC.md 14, 5.5 L2)"
note "SPEC.md 14: 'run -> RUNNING; if last_step = DLQ, that step returns to RUNNING"
note "with attempt_count reset to 0.' This leg's worker sleeps 30 s on its success"
note "path, so the operator can look at that state before it becomes DONE."

RUN4="$(begin workflow-replay-in-flight.json)"
note "run_id = $RUN4"
await_terminal "$RUN4" || exit 1
assert_eq "the run reached DLQ" "DLQ" "$(run_status "$RUN4")"

note "  curl -X POST $ORCH/runs/$RUN4/replay"
CODE4="$(replay_code "$RUN4")"; note "  -> HTTP $CODE4"

await "the replayed step's second attempt to be RUNNING" \
      "SELECT count(*) = 2 AND bool_or(status = 'RUNNING') FROM attempts WHERE run_id = :'run';" \
      "$RUN_TIMEOUT" "run=$RUN4" || exit 1

show "SPEC.md 5.5 L2 - run RUNNING, last step RUNNING, and the budget put back" \
  "SELECT r.status AS run, r.replay_count, r.owner_id IS NOT NULL AS owned,
          s.status AS step, s.attempt_count,
          (SELECT count(*) FROM attempts a WHERE a.step_id = s.step_id) AS attempt_rows,
          (SELECT count(*) FROM dead_letter_queue d WHERE d.run_id = r.run_id) AS dlq_rows
     FROM runs r JOIN steps s ON s.run_id = r.run_id
    WHERE r.run_id = :'run' ORDER BY s.seq DESC LIMIT 1;" "run=$RUN4"

assert_true "SPEC.md 14, 5.5 L2: the run and its step are both RUNNING again" \
  "SELECT r.status = 'RUNNING' AND s.status = 'RUNNING'
     FROM runs r JOIN steps s ON s.run_id = r.run_id
    WHERE r.run_id = :'run' AND s.seq = 1;" "run=$RUN4"
assert_true "SPEC.md 14, 4.2: attempt_count was reset to 0, then burned once by the new dispatch" \
  "SELECT attempt_count = 1 FROM steps WHERE run_id = :'run' AND seq = 1;" "run=$RUN4"
assert_true "SPEC.md 6.3: two attempt rows, against a count of 1" \
  "SELECT count(*) = 2 FROM attempts WHERE run_id = :'run';" "run=$RUN4"
assert_true "SPEC.md 14, 8.5: the sweep picked the run back up, so it is owned again" \
  "SELECT owner_id IS NOT NULL AND claimed_at IS NOT NULL FROM runs WHERE run_id = :'run';" "run=$RUN4"
assert_true "SPEC.md 6.7: replay wrote no dead-letter entry and removed none" \
  "SELECT count(*) = 1 FROM dead_letter_queue WHERE run_id = :'run';" "run=$RUN4"

note ""
note "and now the same run, replayed a second time WHILE IT IS RUNNING:"
note "  curl -X POST $ORCH/runs/$RUN4/replay"
REPLAY4B="$(replay "$RUN4")"
CODE4B="${REPLAY4B%% *}"
BODY4B="${REPLAY4B#* }"
note "  -> HTTP $CODE4B"
printf '%s\n' "$BODY4B" | jq . 2>/dev/null | sed 's/^/     /' || printf '     %s\n' "$BODY4B"

assert_eq "SPEC.md 14: the gate is 'is this run in DLQ RIGHT NOW', not 'has it been replayed'" \
  "409" "$CODE4B"
assert_eq "SPEC.md 10.5: the refusal states the actual current status" \
  "RUNNING" "$(printf '%s' "$BODY4B" | jq -r '.run_status // empty')"
assert_eq "SPEC.md 10.5: 'error' is a stable machine-readable slug" \
  "conflict" "$(printf '%s' "$BODY4B" | jq -r '.error // empty')"
assert_eq "SPEC.md 10.5: the rejection names the run it refused" \
  "$RUN4" "$(printf '%s' "$BODY4B" | jq -r '.run_id // empty')"
assert_true "SPEC.md 14: the refused replay incremented nothing" \
  "SELECT replay_count = 1 FROM runs WHERE run_id = :'run';" "run=$RUN4"

await_terminal "$RUN4" || exit 1
assert_true "SPEC.md 14: the accepted replay still carried the run to DONE" \
  "SELECT status = 'DONE' FROM runs WHERE run_id = :'run';" "run=$RUN4"

# ---------------------------------------------------------------------------
# Legs 5 and 6 - refusals that need no dead letter at all
# ---------------------------------------------------------------------------

banner "leg 5 - replaying a DONE run is a 409 (SPEC.md 14, 10.5)"

RUN5="$(begin workflow-replay-done.json)"
note "run_id = $RUN5"
await_terminal "$RUN5" || exit 1
assert_eq "the run finished without ever failing" "DONE" "$(run_status "$RUN5")"

note "  curl -X POST $ORCH/runs/$RUN5/replay"
REPLAY5="$(replay "$RUN5")"
CODE5="${REPLAY5%% *}"
BODY5="${REPLAY5#* }"
note "  -> HTTP $CODE5"
printf '%s\n' "$BODY5" | jq . 2>/dev/null | sed 's/^/     /' || printf '     %s\n' "$BODY5"

assert_eq "SPEC.md 14: a DONE run cannot be replayed" "409" "$CODE5"
assert_eq "SPEC.md 10.5: the refusal states DONE as the actual current status" \
  "DONE" "$(printf '%s' "$BODY5" | jq -r '.run_status // empty')"
assert_eq "SPEC.md 10.5: 'a refused replay is explained by the run's status, but what he
        does next depends on the step's' - so the step is named too" \
  "DONE" "$(printf '%s' "$BODY5" | jq -r '.step_status // empty')"
assert_true "SPEC.md 14: nothing changed - replay_count is still 0" \
  "SELECT replay_count = 0 FROM runs WHERE run_id = :'run';" "run=$RUN5"

banner "leg 6 - replaying a run_id that names no run is a 404 (SPEC.md 10.5)"
note "  curl -X POST $ORCH/runs/$MISSING_RUN_ID/replay"
CODE6="$(replay_code "$MISSING_RUN_ID")"
note "  -> HTTP $CODE6"
assert_eq "SPEC.md 10.5: 404 is 'no such entity'" "404" "$CODE6"
assert_true "SPEC.md 10.5: a 404 creates nothing" \
  "SELECT count(*) = 0 FROM runs WHERE run_id = :'run';" "run=$MISSING_RUN_ID"

# ---------------------------------------------------------------------------
# Across the legs
# ---------------------------------------------------------------------------

banner "across the legs"

show "every run, and the rounds it went through" \
  "SELECT r.run_id, r.status, r.replay_count,
          (SELECT count(*) FROM dead_letter_queue d WHERE d.run_id = r.run_id) AS dlq_entries
     FROM runs r ORDER BY r.created_at;"

assert_true "SPEC.md 14: run_id never changes - five runs started, five runs exist" \
  "SELECT count(*) = 5 FROM runs;"
assert_true "SPEC.md 14, 5.5 L4: every run was replayed out of DLQ and finished" \
  "SELECT count(*) = 5 AND bool_and(status = 'DONE') FROM runs;"
assert_true "SPEC.md 6.7, 12.4: the dead-letter history outlives the runs' recovery - five entries" \
  "SELECT count(*) = 5 FROM dead_letter_queue;"
assert_true "SPEC.md 6.5: each run's rounds are numbered 0, 1, 2 ... with no gap and no repeat" \
  "SELECT count(*) = 0 FROM (
     SELECT d.replay_round,
            row_number() OVER (PARTITION BY d.run_id ORDER BY d.replay_round) - 1 AS expected
       FROM dead_letter_queue d) t
  WHERE t.replay_round <> t.expected;"
assert_true "SPEC.md 6.4: attempt_no is 1-based and contiguous within each step, rounds included" \
  "SELECT count(*) = 0 FROM (
     SELECT a.attempt_no,
            row_number() OVER (PARTITION BY a.step_id ORDER BY a.attempt_no) AS expected
       FROM attempts a) t
  WHERE t.attempt_no <> t.expected;"
assert_true "SPEC.md 5.6: no run is RUNNING with a DLQ last step" \
  "SELECT count(*) = 0 FROM runs r WHERE r.status = 'RUNNING' AND
     (SELECT s.status FROM steps s WHERE s.run_id = r.run_id ORDER BY s.seq DESC LIMIT 1) = 'DLQ';"
assert_true "SPEC.md 6.2, 8.7: coordination metadata is a pair, held only by a RUNNING run" \
  "SELECT count(*) = 0 FROM runs
    WHERE (owner_id IS NULL) <> (claimed_at IS NULL)
       OR (status <> 'RUNNING' AND owner_id IS NOT NULL);"
assert_true "SPEC.md 12.3: delta is worker-side only - every entry names a step" \
  "SELECT count(*) = 0 FROM dead_letter_queue WHERE step_id IS NULL;"
assert_true "SPEC.md 12.1: the static planner cannot fail, so planner_attempt_count stays 0" \
  "SELECT bool_and(planner_attempt_count = 0) FROM runs;"
assert_true "SPEC.md 15 is milestone iota: nothing has been cancelled, so SPEC.md 14's
        CANCELLED row is not demonstrated here" \
  "SELECT count(*) = 0 FROM runs WHERE status = 'CANCELLED';"

# ---------------------------------------------------------------------------

banner "result"
printf '  %d assertions, %d failures\n' "$ASSERTIONS" "$FAILURES"

if [ "$TEARDOWN_AT_END" -eq 1 ]; then
  note "tearing the environment down (--down)"
  docker compose down -v --remove-orphans >/dev/null 2>&1
else
  note "the environment is left up for inspection. To look around:"
  note "  cd demos/delta && docker compose exec -T postgres psql -U piton -d piton"
  note "To tear it down:  docker compose down -v"
fi

[ "$FAILURES" -eq 0 ] || exit 1
