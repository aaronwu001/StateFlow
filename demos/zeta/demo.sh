#!/usr/bin/env bash
#
# Milestone zeta - the demo script.
#
# CLAUDE.md 4 step 2 asks for "literally: the commands the operator will type
# in a terminal, and the database state he must see afterwards". This is that.
#
# WHAT IT IS NOT
#   SPEC.md 18.5 DOES NOT EXIST. Alpha's, gamma's, beta's and delta's demo
#   scripts each have a ratified section behind them; this one does not,
#   because CLAUDE.md 2 rule 1 forbids this agent from writing into SPEC.md on
#   its own initiative. So every assertion below cites a section that IS
#   ratified - SPEC.md 3.2, 4.2, 5.4, 5.5, 5.8, 6.1, 6.2, 6.3, 6.4, 6.5, 6.7,
#   6.8, 8.6, 8.7, 9.2, 9.3, 9.7, 9.8, 10.1, 10.2, 10.5, 11.1, 12.1, 12.2, 12.3,
#   12.4, 13.2, 14, 15, 16, 17.1 - and none cites a demo script or a test file.
#   If the owner ratifies an 18.5 that differs from this, the file changes and
#   the assertions do not have to.
#
# WHAT IT DEMONSTRATES
#   Until this milestone the only planner is the built-in static one: a
#   workflow's steps are a list fixed when it is created (SPEC.md 6.1). Zeta
#   adds the HTTP planner: the orchestrator asks an external HTTP service,
#   synchronously, what the next step is, and that service can branch on what
#   earlier steps actually produced (SPEC.md 9.2, 9.3). SPEC.md 3.2 also makes
#   the planner call itself an entity, the planner-side counterpart of an
#   attempt (SPEC.md 5.8, 6.8): the KIND of a planner failure - transport
#   error, timeout, invalid response - now lives on that call's own row, not
#   folded into the dead-letter entry's one-word reason (SPEC.md 6.5).
#
#     leg 1  one http workflow, two runs with identical input. The classify
#            worker answers "invoice" to the first run and "receipt" to the
#            second, and the planner can only tell the two runs apart by
#            fetching the first step's output through the read API (SPEC.md
#            9.2, 10.2) - the M1 request itself carries output_bytes, never
#            content. Each run's whole planner conversation is asserted too
#            (SPEC.md 6.8)
#     leg 2  a static workflow and an http workflow, run over the same workers
#            at the same time, both reaching DONE (SPEC.md 6.1) - and the
#            static planner's own conversation is asserted exactly like the
#            http one's, because SPEC.md 12.1 exempts no planner
#     leg 3  three ways a planner call itself fails - unreachable, too slow,
#            and a plain 500 - all exhausting the budget and landing in DLQ
#            under the same entry reason, while each call's own row names a
#            different kind of failure (SPEC.md 5.8, 6.5, 6.8, 12.1, 12.2,
#            12.3)
#     leg 4  two ways a planner's answer cannot be used - an unknown status,
#            and a StepSpec with an unknown key - both recorded as
#            invalid_response on the call, and the second of those creates no
#            step at all (SPEC.md 5.8, 9.3, 9.8)
#     leg 5  one decision point whose failed calls are not all of the same
#            kind - one transport_error, one invalid_response - which the
#            call rows now show directly, while the entry still reads the one
#            reason shared with legs 3 and 4 (SPEC.md 5.8, 6.5, 6.8)
#     leg 6  a planner that answers fail - a valid answer, not a failure, so
#            the run stops immediately, no budget is spent, and the call
#            itself is recorded DONE with that answer (SPEC.md 5.8, 6.8,
#            12.1)
#     leg 7  a run that died on the planner's side, with zero steps, replayed
#            (SPEC.md 12.3, 14) - the dead-letter entry survives untouched,
#            the planner is asked again, and the failed round's calls keep
#            replay_round 0 while everything the replay produces carries
#            replay_round 1 (SPEC.md 6.4, 6.8, 14)
#     leg 8  what POST /workflows refuses at submission time, and the same
#            body, unmutated, accepted (SPEC.md 16)
#
# WHAT ZETA IS SCOPED TO
#   Only the sync + envelope combination is used anywhere in this file
#   (SPEC.md 9.7) - async (SPEC.md 9.5, 9.6, milestone epsilon) and raw
#   dispatch (SPEC.md 9.5, milestone theta) are not demonstrated, and the
#   summary section asserts their absence rather than leaving it silent.
#
#   Cancellation is SPEC.md 15, at milestone iota, and POST
#   /runs/{run_id}/cancel does not exist; no run here is CANCELLED, and that
#   too is asserted rather than left silent.
#
#   No assertion below counts how many times the planner was asked for one
#   decision. SPEC.md 13.2 item 4 publishes the opposite as a non-guarantee -
#   "the asked exactly once guarantee covers persisted decisions only" - so
#   leg 7 asserts the DIRECTION of the fixture's own call counter (more calls
#   after the replay than before it), never an exact count.
#
# HOW TO RUN IT
#   cd demos/zeta && ./demo.sh
#
#   Everything runs inside WSL (CLAUDE.md 8). The script needs curl, jq and
#   docker on the host; it needs no psql, because it reaches the database
#   through `docker compose exec postgres psql`, and it reaches the planner
#   fixture's own counters through the host port docker-compose.yml publishes
#   for it.
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
PLANNER="http://localhost:9100"
HEALTH_TIMEOUT=240
RUN_TIMEOUT=300
TEARDOWN_AT_END=0

RUN_INPUT='{"text":"hello"}'

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

# assert_gt compares two integers the script itself has read: got must be
# strictly greater than baseline. Used only where SPEC.md fixes a DIRECTION
# and not a count - see leg 7 and SPEC.md 13.2 item 4.
assert_gt() {
  local why="$1" baseline="$2" got="$3"
  ASSERTIONS=$((ASSERTIONS + 1))
  if [ "$got" -gt "$baseline" ] 2>/dev/null; then
    printf '  \033[32mPASS\033[0m %s\n' "$why"
  else
    printf '  \033[31mFAIL\033[0m %s\n        baseline: %s\n        got:      %s\n' "$why" "$baseline" "$got"
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

# post_workflow posts a raw JSON body (not a fixture path) to POST /workflows
# and prints "<code> <body>". Unlike create_workflow, a non-2xx is the point
# here - leg 8 submits bodies SPEC.md 16 requires to be refused.
post_workflow() {
  curl -sS -o /tmp/piton-zeta-wf.$$ -w '%{http_code}' \
       -X POST "$ORCH/workflows" -H 'content-type: application/json' \
       --data-binary "$1"
  printf ' '
  cat /tmp/piton-zeta-wf.$$
  rm -f /tmp/piton-zeta-wf.$$
}

# replay issues SPEC.md 10.1's POST /runs/{run_id}/replay and prints
# "<code> <body>". The code is printed rather than checked against a fixed
# value: SPEC.md 10.5 enumerates codes for REFUSALS only, and no ruling fixes
# what a PERFORMED replay returns, so this script accepts any 2xx and asserts
# the EFFECT in the database, which is specified.
replay() {
  curl -sS -o /tmp/piton-zeta-replay.$$ -w '%{http_code}' \
       -X POST "$ORCH/runs/$1/replay"
  printf ' '
  cat /tmp/piton-zeta-replay.$$
  rm -f /tmp/piton-zeta-replay.$$
}

replay_code() { replay "$1" | awk '{print $1}'; }

is_2xx() { case "$1" in 2??) return 0 ;; *) return 1 ;; esac; }

# planner_max_attempts_of reads planner_max_attempts out of a fixture file
# rather than restating it as a literal, so an assertion and the workflow it
# is about cannot drift apart. SPEC.md 11.1 defines the field as a TOTAL call
# count at one decision point.
planner_max_attempts_of() { jq -r '.planner_max_attempts' "$1"; }

# await polls a SQL predicate until it is true. Every wait here is written
# this way rather than as a sleep: SPEC.md 14 clears owner_id/claimed_at "so
# the next sweep picks it up", and SPEC.md 8.6 runs that sweep "on a fixed
# interval" of its own rather than in response to a particular event - so
# sleeping for a computed duration would be asserting a schedule SPEC.md does
# not promise.
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

# calls_for / fetches_for read the planner fixture's own account of what it
# did for one run_id, via GET /stats. Neither number lives in the database:
# SPEC.md 9.2's M1 request is stateless and leaves no row of its own, so this
# is the only place either count can be read at all.
planner_stats() { curl -sS "$PLANNER/stats"; }
calls_for()   { planner_stats | jq -r --arg r "$1" '.calls[$r] // 0'; }
fetches_for() { planner_stats | jq -r --arg r "$1" '.fetches[$r] // 0'; }

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
note "healthy. The worker and planner fixtures are healthy too - the"
note "orchestrator's own compose entry depends on both, so its /healthz"
note "could not be answering yet if they were not."

# ---------------------------------------------------------------------------
# Leg 1 - the planner branches on what a step actually produced
# ---------------------------------------------------------------------------

banner "leg 1 - the planner branches on a step's real output (SPEC.md 9.2, 9.3, 10.2)"
note "SPEC.md 9.2: 'history is a catalogue only. It never carries outputs ..."
note "A planner that wants an output fetches it from the read API.' Two runs"
note "of one workflow, identical input, are started one after the other - not"
note "concurrently, because the worker's invoice/receipt alternation is a"
note "property of arrival order, and racing it would make which run got which"
note "answer a matter of timing rather than of the fixture's own design."

WF1="$(create_workflow workflow-dynamic.json)"
note "workflow_id = $WF1"

RUN1A="$(start_run "$WF1")"
note "run_id (A) = $RUN1A"
await_terminal "$RUN1A" || exit 1
assert_eq "run A finished before run B started, so the alternation is not a race" \
  "DONE" "$(run_status "$RUN1A")"

RUN1B="$(start_run "$WF1")"
note "run_id (B) = $RUN1B"
await_terminal "$RUN1B" || exit 1

show "both runs, one workflow, identical input - SPEC.md 6.2" \
  "SELECT a.run_id, b.run_id, a.workflow_id = b.workflow_id AS same_workflow,
          a.input::text = b.input::text AS same_input
     FROM runs a, runs b WHERE a.run_id = :'runa' AND b.run_id = :'runb';" \
  "runa=$RUN1A" "runb=$RUN1B"

show "each run's steps and the branch the planner took" \
  "SELECT r.run_id, r.status,
          (SELECT output->>'kind' FROM steps WHERE run_id = r.run_id AND seq = 1) AS classify_kind,
          (SELECT step_name FROM steps WHERE run_id = r.run_id AND seq = 2) AS second_step
     FROM runs r WHERE r.run_id IN (:'runa', :'runb') ORDER BY r.created_at;" \
  "runa=$RUN1A" "runb=$RUN1B"

note ""
note "the planner fixture's own counters (curl $PLANNER/stats):"
curl -sS "$PLANNER/stats" | jq . | sed 's/^/     /'

assert_eq "SPEC.md 4.2, 9.3: a run whose planner answers continue, continue, done reaches DONE (A)" \
  "DONE" "$(run_status "$RUN1A")"
assert_eq "SPEC.md 4.2, 9.3: a run whose planner answers continue, continue, done reaches DONE (B)" \
  "DONE" "$(run_status "$RUN1B")"
assert_true "SPEC.md 9.3: the planner chose a different second step for the two runs" \
  "SELECT (SELECT step_name FROM steps WHERE run_id = :'runa' AND seq = 2)
       <> (SELECT step_name FROM steps WHERE run_id = :'runb' AND seq = 2);" \
  "runa=$RUN1A" "runb=$RUN1B"
for run in "$RUN1A" "$RUN1B"; do
  assert_true "SPEC.md 9.2, 10.2: the second step is the one the first step's stored output names" \
    "SELECT (SELECT step_name FROM steps WHERE run_id = :'run' AND seq = 2)
          = 'extract-' || (SELECT output->>'kind' FROM steps WHERE run_id = :'run' AND seq = 1);" \
    "run=$run"
  assert_true "SPEC.md 6.3: exactly two steps, numbered 1 and 2" \
    "SELECT count(*) = 2 AND min(seq) = 1 AND max(seq) = 2 FROM steps WHERE run_id = :'run';" \
    "run=$run"
  assert_true "SPEC.md 6.2, 6.8: a run whose planner never failed burns no budget and has no failed call" \
    "SELECT r.planner_attempt_count = 0
         AND NOT EXISTS (SELECT 1 FROM planner_calls WHERE run_id = r.run_id AND status = 'FAILED')
       FROM runs r WHERE r.run_id = :'run';" \
    "run=$run"
  assert_gt "SPEC.md 9.2, 10.2: the planner fetched this run's classify output at least once" \
    0 "$(fetches_for "$run")"

  show "run $run's whole planner conversation" \
    "SELECT call_no, replay_round, status, answer, called_by
       FROM planner_calls WHERE run_id = :'run' ORDER BY call_no;" "run=$run"
  assert_true "SPEC.md 6.8: three calls, all DONE, call_no contiguous from 1" \
    "SELECT count(*) = 3 AND bool_and(status = 'DONE') AND min(call_no) = 1 AND max(call_no) = 3
       FROM planner_calls WHERE run_id = :'run';" "run=$run"
  assert_true "SPEC.md 6.8, 9.3: the answers, in call order, are continue, continue, done" \
    "SELECT array_agg(answer ORDER BY call_no) = ARRAY['continue', 'continue', 'done']
       FROM planner_calls WHERE run_id = :'run';" "run=$run"
done

# ---------------------------------------------------------------------------
# Leg 2 - both planner types, side by side
# ---------------------------------------------------------------------------

banner "leg 2 - both planner types, side by side (SPEC.md 6.1)"
note "SPEC.md 6.1: planner_type is chosen at POST /workflows and nothing else"
note "changes it afterwards. The demonstration is two workflows of two kinds,"
note "run over one set of workers, both of which work."

WF2="$(create_workflow workflow-static-beside-http.json)"
RUN2="$(start_run "$WF2")"
note "static workflow_id = $WF2, run_id = $RUN2"
await_terminal "$RUN2" || exit 1
assert_eq "SPEC.md 6.1: a static workflow walks its array and reaches DONE" \
  "DONE" "$(run_status "$RUN2")"

show "the two workflows' rows, next to each other" \
  "SELECT workflow_id, planner_type, planner_url, fetch_base_url,
          planner_static_steps IS NOT NULL AS has_static_steps
     FROM workflows WHERE workflow_id IN (:'wfa', :'wfb') ORDER BY created_at;" \
  "wfa=$WF1" "wfb=$WF2"

assert_true "SPEC.md 6.1: the http workflow carries planner_url and fetch_base_url, and no static array" \
  "SELECT planner_type = 'http' AND planner_url IS NOT NULL AND fetch_base_url IS NOT NULL
          AND planner_static_steps IS NULL FROM workflows WHERE workflow_id = :'wf';" "wf=$WF1"
assert_true "SPEC.md 6.1: the static workflow carries the array, and neither URL" \
  "SELECT planner_type = 'static' AND planner_url IS NULL AND fetch_base_url IS NULL
          AND planner_static_steps IS NOT NULL FROM workflows WHERE workflow_id = :'wf';" "wf=$WF2"
assert_true "SPEC.md 6.1: the static run executed both steps of its array" \
  "SELECT count(*) = 2 FROM steps WHERE run_id = :'run';" "run=$RUN2"
assert_true "SPEC.md 6.2, 6.8: the static planner cannot fail at run time, so planner_attempt_count never leaves 0 and no call is FAILED" \
  "SELECT r.planner_attempt_count = 0
       AND NOT EXISTS (SELECT 1 FROM planner_calls WHERE run_id = r.run_id AND status = 'FAILED')
     FROM runs r WHERE r.run_id = :'run';" \
  "run=$RUN2"

note ""
note "SPEC.md 12.1: 'these rules apply to every planner, including the built-in"
note "static one, with no exemption.' The static planner never makes an HTTP"
note "call and can never fail, but SPEC.md 4.2 still writes a planner_calls row"
note "for every one of its decisions, so its conversation is asserted exactly"
note "like the http planner's above."
show "the static run's whole planner conversation" \
  "SELECT call_no, replay_round, status, answer, called_by
     FROM planner_calls WHERE run_id = :'run' ORDER BY call_no;" "run=$RUN2"
assert_true "SPEC.md 6.8, 12.1: three calls, all DONE, call_no contiguous from 1 - the static planner too" \
  "SELECT count(*) = 3 AND bool_and(status = 'DONE') AND min(call_no) = 1 AND max(call_no) = 3
     FROM planner_calls WHERE run_id = :'run';" "run=$RUN2"
assert_true "SPEC.md 6.8, 9.3: the answers, in call order, are continue, continue, done" \
  "SELECT array_agg(answer ORDER BY call_no) = ARRAY['continue', 'continue', 'done']
     FROM planner_calls WHERE run_id = :'run';" "run=$RUN2"

# ---------------------------------------------------------------------------
# Leg 3 - a planner that cannot be called
# ---------------------------------------------------------------------------

banner "leg 3 - a planner that cannot be called (SPEC.md 5.8, 6.5, 6.8, 12.1, 12.2, 12.3)"
note "Three different events - a host that does not resolve, a planner that"
note "sleeps past its 1-second planner_timeout_seconds, and a planner that"
note "answers 500 - all exhaust planner_max_attempts and all land under the"
note "same dead_letter_queue.reason: planner_budget_exhausted. SPEC.md 6.5 was"
note "cut from five planner-side reasons to one: the entry now says only WHICH"
note "of three situations stopped the run, never which KIND of failure did it."
note "That is a fact about the call, recorded on the call's own planner_calls"
note "row (SPEC.md 5.8, 6.8) - so the three fixtures below agree on the entry"
note "and disagree on failure_reason. unreachable and slow are the pair worth"
note "watching closely: SPEC.md 5.8's clock rule says an exchange is timeout"
note "only if its deadline passed, never by the shape of the error - and this"
note "is the first milestone where the two can be told apart at all, because"
note "each now lands on its own row instead of one shared entry."

declare -A RUN3
for f in workflow-planner-unreachable.json workflow-planner-slow.json workflow-planner-http500.json; do
  r="$(begin "$f")"
  RUN3[$f]="$r"
  note "  $f -> run_id = $r"
  await_terminal "$r" || exit 1
done

show "the three runs and the one reason they share" \
  "SELECT r.run_id, d.reason, r.status
     FROM runs r JOIN dead_letter_queue d ON d.run_id = r.run_id
    WHERE r.run_id IN (:'u', :'s', :'h') ORDER BY r.created_at;" \
  "u=${RUN3[workflow-planner-unreachable.json]}" \
  "s=${RUN3[workflow-planner-slow.json]}" \
  "h=${RUN3[workflow-planner-http500.json]}"

show "the same three runs' planner_calls - where the KIND of each failure actually lives" \
  "SELECT run_id, call_no, status, failure_reason
     FROM planner_calls WHERE run_id IN (:'u', :'s', :'h') ORDER BY run_id, call_no;" \
  "u=${RUN3[workflow-planner-unreachable.json]}" \
  "s=${RUN3[workflow-planner-slow.json]}" \
  "h=${RUN3[workflow-planner-http500.json]}"

declare -A REASON3=(
  [workflow-planner-unreachable.json]=transport_error
  [workflow-planner-slow.json]=timeout
  [workflow-planner-http500.json]=transport_error
)
for f in workflow-planner-unreachable.json workflow-planner-slow.json workflow-planner-http500.json; do
  r="${RUN3[$f]}"
  budget="$(planner_max_attempts_of "$f")"
  reason3="${REASON3[$f]}"
  assert_eq "SPEC.md 12.2: $f's run stopped in the dead-letter queue" "DLQ" "$(run_status "$r")"
  assert_true "SPEC.md 5.5 L5, 5.4: run = DLQ and last_step = DONE (no step exists)" \
    "SELECT r.status = 'DLQ' AND
            coalesce((SELECT s.status FROM steps s WHERE s.run_id = r.run_id ORDER BY s.seq DESC LIMIT 1), 'DONE') = 'DONE'
       FROM runs r WHERE r.run_id = :'run';" "run=$r"
  assert_true "SPEC.md 6.5: the entry's reason is planner_budget_exhausted" \
    "SELECT reason = 'planner_budget_exhausted' FROM dead_letter_queue WHERE run_id = :'run';" "run=$r"
  assert_true "SPEC.md 12.3, 6.5: the entry names no step" \
    "SELECT step_id IS NULL FROM dead_letter_queue WHERE run_id = :'run';" "run=$r"
  assert_true "SPEC.md 12.3: a planner-side failure dispatches nothing - no attempt exists" \
    "SELECT count(*) = 0 FROM attempts WHERE run_id = :'run';" "run=$r"
  assert_true "SPEC.md 12.2, 11.1: the run stopped at exactly its total planner budget ($budget calls)" \
    "SELECT planner_attempt_count = $budget FROM runs WHERE run_id = :'run';" "run=$r"
  assert_true "SPEC.md 5.8, 6.8, 12.2: $f's $budget FAILED calls all carry failure_reason $reason3" \
    "SELECT count(*) FILTER (WHERE status = 'FAILED' AND failure_reason = '$reason3') = $budget
       FROM planner_calls WHERE run_id = :'run';" "run=$r"
done

# ---------------------------------------------------------------------------
# Leg 4 - a planner whose answer cannot be used
# ---------------------------------------------------------------------------

banner "leg 4 - a planner whose answer cannot be used (SPEC.md 5.8, 6.5, 6.8, 9.3, 9.8)"
note "garbage answers a status outside SPEC.md 9.3's three; badstep answers a"
note "StepSpec carrying an unknown top-level key, which SPEC.md 9.8 rule 6"
note "rejects. SPEC.md 5.8 gives both the same failure_reason, invalid_response"
note "- 'a reply arrived, but 9.3 or 9.8 rejects it' - and exhausting the"
note "budget on that reason lands the entry on planner_budget_exhausted"
note "(SPEC.md 6.5), the same entry reason leg 3 produced from a wholly"
note "different kind of call failure."

declare -A RUN4
for f in workflow-planner-garbage.json workflow-planner-badstep.json; do
  r="$(begin "$f")"
  RUN4[$f]="$r"
  note "  $f -> run_id = $r"
  await_terminal "$r" || exit 1
done

show "both runs and the reason they share" \
  "SELECT r.run_id, d.reason, r.status
     FROM runs r JOIN dead_letter_queue d ON d.run_id = r.run_id
    WHERE r.run_id IN (:'g', :'b') ORDER BY r.created_at;" \
  "g=${RUN4[workflow-planner-garbage.json]}" "b=${RUN4[workflow-planner-badstep.json]}"

show "both runs' planner_calls - each FAILED call's own kind" \
  "SELECT run_id, call_no, status, failure_reason
     FROM planner_calls WHERE run_id IN (:'g', :'b') ORDER BY run_id, call_no;" \
  "g=${RUN4[workflow-planner-garbage.json]}" "b=${RUN4[workflow-planner-badstep.json]}"

for f in workflow-planner-garbage.json workflow-planner-badstep.json; do
  r="${RUN4[$f]}"
  budget="$(planner_max_attempts_of "$f")"
  assert_eq "SPEC.md 12.2: $f's run stopped in the dead-letter queue" "DLQ" "$(run_status "$r")"
  assert_true "SPEC.md 6.5: the entry's reason is planner_budget_exhausted" \
    "SELECT reason = 'planner_budget_exhausted' FROM dead_letter_queue WHERE run_id = :'run';" "run=$r"
  assert_true "SPEC.md 12.2, 11.1: the run stopped at exactly its total planner budget ($budget calls)" \
    "SELECT planner_attempt_count = $budget FROM runs WHERE run_id = :'run';" "run=$r"
  assert_true "SPEC.md 5.8, 6.8, 9.3, 9.8: $f's $budget FAILED calls all carry failure_reason invalid_response" \
    "SELECT count(*) FILTER (WHERE status = 'FAILED' AND failure_reason = 'invalid_response') = $budget
       FROM planner_calls WHERE run_id = :'run';" "run=$r"
done

show "the badstep run's own steps - there must be none" \
  "SELECT count(*) AS step_count FROM steps WHERE run_id = :'run';" "run=${RUN4[workflow-planner-badstep.json]}"
assert_true "SPEC.md 9.8: 'An invalid StepSpec ... never creates a step'" \
  "SELECT count(*) = 0 FROM steps WHERE run_id = :'run';" "run=${RUN4[workflow-planner-badstep.json]}"

# ---------------------------------------------------------------------------
# Leg 5 - mixed failures in one round
# ---------------------------------------------------------------------------

banner "leg 5 - mixed failures at one decision point (SPEC.md 5.8, 6.5, 6.8)"
note "This fixture answers a 500 on its first call and an unknown status on"
note "every call after that, so one decision point's failed calls are not all"
note "of one kind. Before this amendment that fact could only be told by an"
note "entry reason reserved for exactly this situation; SPEC.md 6.5 removed"
note "that reason along with the rest of the planner-side enumeration, because"
note "the entry no longer needs to summarise a set of failures in one word -"
note "the set is now the planner_calls rows themselves (SPEC.md 6.8), each"
note "naming its own kind. The entry reads the same planner_budget_exhausted"
note "leg 3 and leg 4 produced; what makes this round 'mixed' is visible only"
note "on the call rows below."

RUN5="$(begin workflow-planner-mixed.json)"
note "run_id = $RUN5"
await_terminal "$RUN5" || exit 1
budget5="$(planner_max_attempts_of workflow-planner-mixed.json)"

show "the entry" \
  "SELECT reason, replay_round, left(error_text, 60) AS error_text
     FROM dead_letter_queue WHERE run_id = :'run';" "run=$RUN5"

show "the calls behind it - one of each kind" \
  "SELECT call_no, status, failure_reason FROM planner_calls WHERE run_id = :'run' ORDER BY call_no;" \
  "run=$RUN5"

assert_eq "SPEC.md 12.2: the run stopped in the dead-letter queue" "DLQ" "$(run_status "$RUN5")"
assert_true "SPEC.md 6.5: the entry's reason is planner_budget_exhausted" \
  "SELECT reason = 'planner_budget_exhausted' FROM dead_letter_queue WHERE run_id = :'run';" "run=$RUN5"
assert_true "SPEC.md 12.2, 11.1: the run stopped at exactly its total planner budget ($budget5 calls)" \
  "SELECT planner_attempt_count = $budget5 FROM runs WHERE run_id = :'run';" "run=$RUN5"
assert_true "SPEC.md 5.8, 6.8, 12.1: exactly one transport_error and exactly one invalid_response, not one kind repeated" \
  "SELECT count(*) FILTER (WHERE failure_reason = 'transport_error') = 1
       AND count(*) FILTER (WHERE failure_reason = 'invalid_response') = 1
       AND count(*) FILTER (WHERE status = 'FAILED') = 2
     FROM planner_calls WHERE run_id = :'run';" "run=$RUN5"

# ---------------------------------------------------------------------------
# Leg 6 - a planner that declares failure
# ---------------------------------------------------------------------------

banner "leg 6 - a planner that declares failure (SPEC.md 5.8, 6.5, 6.8, 12.1)"
note "SPEC.md 12.1: 'A fail response is not a planner failure - it is a valid"
note "answer, and it sends the run to DLQ immediately without consuming"
note "budget.' planner_max_attempts is 3 on this fixture for exactly this"
note "reason: nothing here should ever touch it. SPEC.md 5.8 makes the same"
note "point at the row level: the call itself is written DONE, with answer ="
note "'fail', never FAILED - a planner saying 'this cannot be done' is a"
note "successful exchange, and the row says so."

RUN6="$(begin workflow-planner-fail.json)"
note "run_id = $RUN6"
await_terminal "$RUN6" || exit 1

show "the entry - a valid answer, not a failure" \
  "SELECT reason, left(error_text, 60) AS error_text
     FROM dead_letter_queue WHERE run_id = :'run';" "run=$RUN6"

show "the call itself - DONE, answer fail, its own error_text" \
  "SELECT call_no, status, answer, left(error_text, 60) AS error_text
     FROM planner_calls WHERE run_id = :'run' ORDER BY call_no;" "run=$RUN6"

assert_eq "SPEC.md 12.1: a fail answer sends the run to DLQ immediately" "DLQ" "$(run_status "$RUN6")"
assert_true "SPEC.md 6.5: reason is planner_declared_fail" \
  "SELECT reason = 'planner_declared_fail' FROM dead_letter_queue WHERE run_id = :'run';" "run=$RUN6"
assert_true "SPEC.md 12.1: a fail answer consumes no budget" \
  "SELECT planner_attempt_count = 0 FROM runs WHERE run_id = :'run';" "run=$RUN6"
assert_true "SPEC.md 5.8, 6.8: exactly one call, DONE, answer fail, with error_text, and no FAILED call" \
  "SELECT count(*) = 1
       AND count(*) FILTER (WHERE status = 'DONE' AND answer = 'fail' AND error_text IS NOT NULL) = 1
       AND count(*) FILTER (WHERE status = 'FAILED') = 0
     FROM planner_calls WHERE run_id = :'run';" "run=$RUN6"

# ---------------------------------------------------------------------------
# Leg 7 - replaying a run that died on the planner's side
# ---------------------------------------------------------------------------

banner "leg 7 - replaying a planner-side dead letter (SPEC.md 12.3, 14)"
note "SPEC.md 12.3: for a planner-side entry, 'what replay resumes - asking"
note "the planner again', because there is no step to re-dispatch. This"
note "fixture's planner refuses twice (its planner_max_attempts is 2), then"
note "answers properly from its third call on."

RUN7="$(begin workflow-planner-recovers.json)"
note "run_id = $RUN7"
await_terminal "$RUN7" || exit 1
assert_eq "SPEC.md 14: the run must be in DLQ before it can be replayed" "DLQ" "$(run_status "$RUN7")"

STEPS_BEFORE="$(psql_q "SELECT count(*) FROM steps WHERE run_id = :'run';" "run=$RUN7")"
DLQ_BEFORE="$(psql_q "SELECT md5(string_agg(dlq_id::text || reason || replay_round ||
                                            error_text || created_at::text, ',' ORDER BY dlq_id))
                        FROM dead_letter_queue WHERE run_id = :'run';" "run=$RUN7")"
CALLS_BEFORE="$(calls_for "$RUN7")"

show "the state before the replay - zero steps, one dead-letter entry" \
  "SELECT r.status AS run,
          coalesce((SELECT s.status FROM steps s WHERE s.run_id = r.run_id ORDER BY s.seq DESC LIMIT 1), '<none>') AS last_step,
          (SELECT count(*) FROM steps s WHERE s.run_id = r.run_id) AS step_count,
          (SELECT count(*) FROM dead_letter_queue d WHERE d.run_id = r.run_id) AS dlq_entries
     FROM runs r WHERE r.run_id = :'run';" "run=$RUN7"

assert_eq "SPEC.md 12.3: a run that died at its first decision point holds no step" "0" "$STEPS_BEFORE"

note ""
note "the command the operator types (SPEC.md 10.1):"
note "  curl -X POST $ORCH/runs/$RUN7/replay"
CODE7="$(replay_code "$RUN7")"
note "  -> HTTP $CODE7"
ASSERTIONS=$((ASSERTIONS + 1))
if is_2xx "$CODE7"; then
  printf '  \033[32mPASS\033[0m %s\n' "SPEC.md 14: a run whose status is DLQ is replayed, not refused"
else
  printf '  \033[31mFAIL\033[0m %s\n        got HTTP %s\n' \
         "SPEC.md 14: a run whose status is DLQ is replayed, not refused" "$CODE7"
  FAILURES=$((FAILURES + 1))
fi

await_terminal "$RUN7" || exit 1
CALLS_AFTER="$(calls_for "$RUN7")"
DLQ_AFTER="$(psql_q "SELECT md5(string_agg(dlq_id::text || reason || replay_round ||
                                           error_text || created_at::text, ',' ORDER BY dlq_id))
                       FROM dead_letter_queue WHERE run_id = :'run';" "run=$RUN7")"

show "after the replay - the planner was asked again and the run finished" \
  "SELECT r.status AS run, r.replay_count, r.planner_attempt_count,
          (SELECT count(*) FROM steps s WHERE s.run_id = r.run_id) AS step_count
     FROM runs r WHERE r.run_id = :'run';" "run=$RUN7"

assert_eq "SPEC.md 14, 12.3: the replayed run resumed at the planner and reached DONE" \
  "DONE" "$(run_status "$RUN7")"
note ""
note "the fixture's own /stats counter is one reading of 'the planner was asked"
note "again' - the second, and the one SPEC.md actually specifies, is the"
note "replay_round on the persisted planner_calls and attempts rows below."
note "SPEC.md 13.2 item 4 still forbids counting how many times a FAILING call"
note "was retried beyond the budget; asserting the round split of a COMPLETED"
note "conversation is a statement about persisted decisions and is fine."
assert_gt "SPEC.md 12.3, 14: the fixture's own counter also saw more calls after the replay (corroboration only)" \
  "$CALLS_BEFORE" "$CALLS_AFTER"
assert_true "SPEC.md 4.2 L1, 14: steps now exist that could not exist before the replay" \
  "SELECT count(*) = 2 FROM steps WHERE run_id = :'run';" "run=$RUN7"
assert_true "SPEC.md 6.2: the successful round left the planner budget at 0" \
  "SELECT planner_attempt_count = 0 FROM runs WHERE run_id = :'run';" "run=$RUN7"
assert_true "SPEC.md 14, 6.2: one replay round, so replay_count = 1" \
  "SELECT replay_count = 1 FROM runs WHERE run_id = :'run';" "run=$RUN7"
assert_eq "SPEC.md 6.7, 12.4: the dead-letter entry is byte-identical either side of the replay" \
  "$DLQ_BEFORE" "$DLQ_AFTER"
assert_true "SPEC.md 12.4, 6.5: still one entry, and it still names round 0" \
  "SELECT count(*) = 1 AND count(*) FILTER (WHERE replay_round = 0) = 1
     FROM dead_letter_queue WHERE run_id = :'run';" "run=$RUN7"

show "the run's whole planner conversation, both rounds" \
  "SELECT call_no, replay_round, status, answer, failure_reason
     FROM planner_calls WHERE run_id = :'run' ORDER BY call_no;" "run=$RUN7"
assert_true "SPEC.md 5.8, 6.8: the two failed calls before the replay carry replay_round 0" \
  "SELECT count(*) = 2 AND bool_and(status = 'FAILED' AND failure_reason = 'transport_error')
     FROM planner_calls WHERE run_id = :'run' AND replay_round = 0;" "run=$RUN7"
assert_true "SPEC.md 14, 6.8: every call the replay produced carries replay_round 1" \
  "SELECT count(*) = 3 AND bool_and(status = 'DONE')
     FROM planner_calls WHERE run_id = :'run' AND replay_round = 1;" "run=$RUN7"
assert_true "SPEC.md 6.8: call_no orders the whole conversation, contiguous across both rounds" \
  "SELECT count(*) = 5 AND min(call_no) = 1 AND max(call_no) = 5 FROM planner_calls WHERE run_id = :'run';" \
  "run=$RUN7"
assert_true "SPEC.md 6.4, 14: both attempts were dispatched after the replay, so both carry replay_round 1" \
  "SELECT count(*) = 2 AND bool_and(replay_round = 1) FROM attempts WHERE run_id = :'run';" "run=$RUN7"

# ---------------------------------------------------------------------------
# Leg 8 - what POST /workflows refuses
# ---------------------------------------------------------------------------

banner "leg 8 - what POST /workflows refuses (SPEC.md 16)"
note "SPEC.md 16 rule 2: an http workflow with no fetch_base_url, or one whose"
note "fetch_base_url is not a valid absolute HTTP(S) URL, is a 400. Rule 3: a"
note "static workflow carrying fetch_base_url is a 400 too - that field"
note "belongs to http alone (SPEC.md 6.1). The same body, unmutated, is"
note "submitted first and must be accepted - otherwise a 400 below could be"
note "caused by something nobody named."

CONTROL_BODY="$(cat workflow-dynamic.json)"
CONTROL_RESULT="$(post_workflow "$CONTROL_BODY")"
CONTROL_CODE="${CONTROL_RESULT%% *}"
note "  curl -X POST $ORCH/workflows -d @workflow-dynamic.json   (unmutated)"
note "  -> HTTP $CONTROL_CODE"
ASSERTIONS=$((ASSERTIONS + 1))
if [ "$CONTROL_CODE" -ge 200 ] 2>/dev/null && [ "$CONTROL_CODE" -lt 300 ] 2>/dev/null; then
  printf '  \033[32mPASS\033[0m %s\n' "the control body is accepted, so the rejections below mean something"
else
  printf '  \033[31mFAIL\033[0m %s\n        got HTTP %s\n' \
         "the control body is accepted, so the rejections below mean something" "$CONTROL_CODE"
  FAILURES=$((FAILURES + 1))
fi

MISSING_FETCH_BODY="$(jq 'del(.fetch_base_url)' workflow-dynamic.json)"
MISSING_FETCH_RESULT="$(post_workflow "$MISSING_FETCH_BODY")"
MISSING_FETCH_CODE="${MISSING_FETCH_RESULT%% *}"
note ""
note "  http workflow, fetch_base_url deleted"
note "  -> HTTP $MISSING_FETCH_CODE"
assert_eq "SPEC.md 16 rule 2: an http workflow with no fetch_base_url is a 400" \
  "400" "$MISSING_FETCH_CODE"

BAD_URL_BODY="$(jq '.fetch_base_url = "orchestrator:8080"' workflow-dynamic.json)"
BAD_URL_RESULT="$(post_workflow "$BAD_URL_BODY")"
BAD_URL_CODE="${BAD_URL_RESULT%% *}"
note ""
note "  http workflow, fetch_base_url = \"orchestrator:8080\" (not absolute HTTP(S))"
note "  -> HTTP $BAD_URL_CODE"
assert_eq "SPEC.md 16 rule 2: a fetch_base_url that is not a valid absolute HTTP(S) URL is a 400" \
  "400" "$BAD_URL_CODE"

STATIC_WITH_FETCH_BODY="$(jq '. + {fetch_base_url: "http://orchestrator:8080"}' workflow-static-beside-http.json)"
STATIC_WITH_FETCH_RESULT="$(post_workflow "$STATIC_WITH_FETCH_BODY")"
STATIC_WITH_FETCH_CODE="${STATIC_WITH_FETCH_RESULT%% *}"
note ""
note "  static workflow, fetch_base_url added"
note "  -> HTTP $STATIC_WITH_FETCH_CODE"
assert_eq "SPEC.md 16 rule 3, 6.1: a static workflow carrying fetch_base_url is a 400" \
  "400" "$STATIC_WITH_FETCH_CODE"

# 11 accepted workflows: leg 1 (1), leg 2 (1), leg 3 (3), leg 4 (2), leg 5 (1),
# leg 6 (1), leg 7 (1), leg 8's control (1) = 11. The three rejections above
# must have stored none of them.
assert_true "SPEC.md 16: a refused workflow is not stored - only the 11 accepted submissions exist" \
  "SELECT count(*) = 11 FROM workflows;"

# ---------------------------------------------------------------------------
# Across the legs
# ---------------------------------------------------------------------------

banner "across the legs"

show "every run this script created, and its final state" \
  "SELECT r.run_id, r.status, r.replay_count, r.planner_attempt_count,
          (SELECT count(*) FROM steps s WHERE s.run_id = r.run_id) AS steps,
          (SELECT count(*) FROM dead_letter_queue d WHERE d.run_id = r.run_id) AS dlq_entries
     FROM runs r ORDER BY r.created_at;"

show "every dead-letter entry" \
  "SELECT run_id, reason, replay_round, step_id IS NULL AS planner_side
     FROM dead_letter_queue ORDER BY created_at;"

show "every planner call" \
  "SELECT run_id, call_no, replay_round, status, answer, failure_reason, called_by
     FROM planner_calls ORDER BY run_id, call_no;"

assert_true "11 runs were started across legs 1, 2, 3, 4, 5, 6 and 7" \
  "SELECT count(*) = 11 FROM runs;"
assert_true "every run this script created reached a terminal state" \
  "SELECT count(*) = 0 FROM runs WHERE status = 'RUNNING';"
assert_true "SPEC.md 5.5: every run is L3 (DONE/DONE) or L5 (DLQ/DONE) - zeta's worker never fails, so L4 never appears" \
  "SELECT count(*) = 0 FROM runs r WHERE NOT (
       (r.status = 'DONE' AND coalesce((SELECT s.status FROM steps s WHERE s.run_id = r.run_id ORDER BY s.seq DESC LIMIT 1), 'DONE') = 'DONE')
    OR (r.status = 'DLQ'  AND coalesce((SELECT s.status FROM steps s WHERE s.run_id = r.run_id ORDER BY s.seq DESC LIMIT 1), 'DONE') = 'DONE')
  );"
assert_true "SPEC.md 6.2, 8.7: coordination metadata is a pair, held only by a RUNNING run" \
  "SELECT count(*) = 0 FROM runs
    WHERE (owner_id IS NULL) <> (claimed_at IS NULL)
       OR (status <> 'RUNNING' AND owner_id IS NOT NULL);"
assert_true "SPEC.md 12.3, 6.5: zeta produces only planner-side dead letters - every entry names no step" \
  "SELECT count(*) = 0 FROM dead_letter_queue WHERE step_id IS NOT NULL;"
assert_true "8 dead-letter entries: legs 3-6 (7 fixtures) plus leg 7's surviving round-0 entry" \
  "SELECT count(*) = 8 FROM dead_letter_queue;"
assert_true "SPEC.md 6.5: every entry is numbered round 0 - nothing here replayed twice" \
  "SELECT bool_and(replay_round = 0) FROM dead_letter_queue;"

note ""
note "SPEC.md 6.5: 'reason says which of three situations stopped the run. It"
note "does not say why any individual exchange failed.' Six different kinds of"
note "planner failure across legs 3, 4 and 5 landed under one entry reason -"
note "the WHY is on the call rows checked below, not the entry."
assert_true "SPEC.md 6.8 invariant 1: every FAILED call has a failure_reason and no answer" \
  "SELECT count(*) = 0 FROM planner_calls
    WHERE status = 'FAILED' AND (failure_reason IS NULL OR answer IS NOT NULL);"
assert_true "SPEC.md 6.8 invariant 2: every DONE call has an answer and no failure_reason" \
  "SELECT count(*) = 0 FROM planner_calls
    WHERE status = 'DONE' AND (answer IS NULL OR failure_reason IS NOT NULL);"
assert_true "SPEC.md 6.8 invariant 3: error_text is present on every FAILED call and every fail answer" \
  "SELECT count(*) = 0 FROM planner_calls
    WHERE (status = 'FAILED' OR answer = 'fail') AND error_text IS NULL;"
assert_true "SPEC.md 5.8: no planner call carries a failure_reason that belongs to an attempt alone" \
  "SELECT count(*) = 0 FROM planner_calls
    WHERE failure_reason IN ('worker_error', 'orphaned', 'cancelled');"
assert_true "SPEC.md 6.8: every planner call is finished and names the orchestrator that made it" \
  "SELECT count(*) = 0 FROM planner_calls WHERE finished_at IS NULL OR called_by IS NULL;"

note ""
note "what this milestone does not demonstrate, asserted rather than left silent:"
assert_true "SPEC.md 15 is milestone iota: POST /runs/{run_id}/cancel does not exist, so nothing is CANCELLED" \
  "SELECT count(*) = 0 FROM runs WHERE status = 'CANCELLED';"
assert_true "SPEC.md 9.7: every step in this environment is sync + envelope - async (epsilon) and raw (theta) are absent" \
  "SELECT count(*) = 0 FROM steps
    WHERE decision->>'connection_mode' <> 'sync' OR decision->>'dispatch_style' <> 'envelope';"

# ---------------------------------------------------------------------------

banner "result"
printf '  %d assertions, %d failures\n' "$ASSERTIONS" "$FAILURES"

if [ "$TEARDOWN_AT_END" -eq 1 ]; then
  note "tearing the environment down (--down)"
  docker compose down -v --remove-orphans >/dev/null 2>&1
else
  note "the environment is left up for inspection. To look around:"
  note "  cd demos/zeta && docker compose exec -T postgres psql -U piton -d piton"
  note "  curl $PLANNER/stats | jq ."
  note "To tear it down:  docker compose down -v"
fi

[ "$FAILURES" -eq 0 ] || exit 1
