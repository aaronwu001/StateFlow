#!/usr/bin/env bash
#
# Milestone beta - the demo script.
#
# Beta is crash recovery. CLAUDE.md 4 step 2 asks for "literally: the commands
# the operator will type in a terminal, and the database state he must see
# afterwards", and CLAUDE.md 5.1 permits exactly one source - SPEC.md. Every
# assertion below cites the ratified section it came from:
#
#   SPEC.md 13.1  what recovery handles (six situations, four of them here)
#   SPEC.md 13.2  what it does not handle - 13.2.7 in particular
#   SPEC.md 8.5   claiming
#   SPEC.md 8.6   the sweep, and the claim-time rule for a sync attempt
#   SPEC.md 8.7   heartbeat, and release on a clean shutdown
#   SPEC.md 5.3   orphaned
#   SPEC.md 12.2  the budget is burned at dispatch and no crash refunds it
#
# HOW TO RUN IT
#   cd demos/beta && ./demo.sh
#
#   Everything runs inside WSL (CLAUDE.md 8). Needs curl, jq and docker; needs
#   no psql, because it reaches the database through
#   `docker compose exec postgres psql`.
#
#   Exit 0 means every assertion held. Any other status names the failure.
#
# THE FIVE LEGS
#   1  SIGKILL while a step is in flight       SPEC.md 13.1.1
#   2  a DLQ'd run the crash must not touch    SPEC.md 13.2.7
#   3  a clean shutdown releases ownership     SPEC.md 8.7
#   4  a crash loop converges to DLQ           SPEC.md 13.1.4
#   5  storage unreachable at startup          SPEC.md 13.1.5
#
#   Legs 1 and 2 share one crash on purpose: the DLQ'd run is created BEFORE the
#   kill, so the same restart that resumes one run must leave the other alone.
#
# WHAT YOU ARE WATCHING FOR, IN ONE SENTENCE
#   After a SIGKILL the run still carries the dead process's owner_id, and only
#   another orchestrator finding that owner no longer live can take it away
#   (SPEC.md 8.5) - whereas after a clean stop the column is already NULL.
#   Those two facts are the whole of beta's coordination story.

set -uo pipefail

ORCH="http://localhost:8080"
HEALTH_TIMEOUT=240
RUN_TIMEOUT=300
TEARDOWN_AT_END=0

RUN_INPUT='{"text":"hello"}'

usage() {
  cat <<'USAGE'
usage: ./demo.sh [--down] [--help]

  --down   tear the environment down (docker compose down -v) at the end
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

# Read from the files themselves, so an assertion and the configuration it
# depends on cannot drift apart.
LEASE_TTL=$(sed -n 's/^lease_ttl_seconds:[[:space:]]*\([0-9][0-9]*\).*/\1/p' piton.yaml)
SWEEP=$(sed -n 's/^sweep_interval_seconds:[[:space:]]*\([0-9][0-9]*\).*/\1/p' piton.yaml)
MAX_ATTEMPTS=$(jq '.step_max_attempts' workflow-crash-loop.json)
[ -n "$LEASE_TTL" ] && [ -n "$SWEEP" ] || {
  echo "demo.sh: lease_ttl_seconds / sweep_interval_seconds not found in piton.yaml" >&2; exit 2; }

# How long a takeover may take before the demo calls it a failure: the lease
# must expire, then a sweep must run, then the claim happens. Slack for a loaded
# machine, never a promise - SPEC.md 13.3 makes these lower bounds.
TAKEOVER=$(( 4 * (LEASE_TTL + SWEEP) + 30 ))

RUN=""
PASSED=0
FAILED=0

hr()  { printf '\n===============================================================\n%s\n\n' "$*"; }
say() { printf '%s\n' "$*"; }

pgx() { docker compose exec -T postgres psql -U piton -d piton -v ON_ERROR_STOP=1 -v run="$RUN" "$@"; }
q()   { local sql="$1"; shift; printf '%s\n' "$sql" | pgx "$@" -At | tr -d '[:space:]'; }
qraw(){ local sql="$1"; shift; printf '%s\n' "$sql" | pgx "$@" -At; }
show(){ local sql="$1"; shift; printf '\n%s\n' "$sql"; printf '%s\n' "$sql" | pgx "$@" || true; }

check() {
  local label="$1" sql="$2"; shift 2
  local out=""
  if ! out="$(q "$sql" "$@" 2>&1)"; then
    printf '  FAIL  %s\n          query error: %s\n' "$label" "$out"
    FAILED=$((FAILED + 1)); return 0
  fi
  if [ "$out" = "t" ]; then
    printf '  ok    %s\n' "$label"; PASSED=$((PASSED + 1))
  else
    printf '  FAIL  %s\n          expected t, got: %s\n' "$label" "${out:-<empty>}"
    FAILED=$((FAILED + 1))
  fi
  return 0
}

# same <label> <a> <b> - two strings that SPEC.md requires to be identical.
same() {
  local label="$1" a="$2" b="$3"
  if [ "$a" = "$b" ]; then
    printf '  ok    %s\n' "$label"; PASSED=$((PASSED + 1))
  else
    printf '  FAIL  %s\n          before: %s\n          after:  %s\n' "$label" "${a:-<empty>}" "${b:-<empty>}"
    FAILED=$((FAILED + 1))
  fi
}

finish() {
  local status=$?
  if [ "$TEARDOWN_AT_END" -eq 1 ]; then
    hr "TEARDOWN"; docker compose down -v --remove-orphans || true
  else
    printf '\nThe environment is still up (SPEC.md 17 - the terminal is the interface):\n\n'
    printf '  docker compose exec postgres psql -U piton -d piton\n'
    printf '  docker compose logs -f orchestrator\n'
    printf '  docker compose exec worker wget -q -O- http://127.0.0.1:9090/calls\n'
    printf '                      # how many times each step reached the worker\n\n'
    printf 'Tear it down with the volume wipe CLAUDE.md 5.5 requires:\n\n  docker compose down -v\n\n'
  fi
  exit "$status"
}
trap finish EXIT

# ---------------------------------------------------------------------------
# Helpers that speak SPEC.md 10.1 and the container runtime
# ---------------------------------------------------------------------------

begin() {   # begin <workflow file> -> sets RUN
  local wf body
  body=$(curl -sS -X POST "$ORCH/workflows" -H 'content-type: application/json' -d @"$1")
  wf=$(printf '%s' "$body" | jq -r '.workflow_id // empty')
  [ -n "$wf" ] || { say "FAILED: POST /workflows returned no workflow_id for $1"; say "  $body"; return 1; }
  body=$(curl -sS -X POST "$ORCH/workflows/$wf/runs" -H 'content-type: application/json' \
           -d "{\"input\":$RUN_INPUT,\"overrides\":{}}")
  RUN=$(printf '%s' "$body" | jq -r '.run_id // empty')
  [ -n "$RUN" ] || { say "FAILED: POST /runs returned no run_id for $1"; say "  $body"; return 1; }
  say "  $1 -> run_id=$RUN"
}

wait_healthy() {
  local deadline=$(( $(date +%s) + HEALTH_TIMEOUT ))
  until curl -sS --max-time 5 "$ORCH/healthz" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || { say "FAILED: /healthz never answered." >&2; return 1; }
    sleep 1
  done
}

# wait_attempt_running <step_name> - waits until that step has an attempt on the
# wire, so that "kill it mid-run" is precise rather than a hopeful sleep.
wait_attempt_running() {
  local step="$1" deadline=$(( $(date +%s) + TAKEOVER ))
  while :; do
    local who
    who=$(q "SELECT a.dispatched_by FROM attempts a JOIN steps s ON s.step_id = a.step_id
              WHERE a.run_id = :'run' AND s.step_name = :'step' AND a.status = 'RUNNING'
              ORDER BY a.attempt_no DESC LIMIT 1;" -v step="$step")
    [ -z "$who" ] || { printf '%s' "$who"; return 0; }
    [ "$(date +%s)" -lt "$deadline" ] || {
      say "FAILED: step $step never had a RUNNING attempt within ${TAKEOVER}s." >&2; return 1; }
    sleep 1
  done
}

# wait_attempt_no <step_name> <attempt_no> - waits for a SPECIFIC attempt number.
#
# The distinction matters, and getting it wrong weakens the demo silently rather
# than failing it. After a SIGKILL the killed attempt is still RUNNING in the
# database - nothing expired it, because the process that would have died with
# it - so "is an attempt RUNNING?" is true again the instant the next
# orchestrator boots, long before it has claimed the run (SPEC.md 8.5 makes it
# wait for the dead owner's lease to expire first). Killing on that signal would
# kill a process that had not yet dispatched anything, and leg 4 would burn no
# budget at all.
wait_attempt_no() {
  local step="$1" no="$2" deadline=$(( $(date +%s) + TAKEOVER ))
  while :; do
    local who
    who=$(q "SELECT a.dispatched_by FROM attempts a JOIN steps s ON s.step_id = a.step_id
              WHERE a.run_id = :'run' AND s.step_name = :'step'
                AND a.attempt_no = :attempt AND a.status = 'RUNNING';" -v step="$step" -v attempt="$no")
    [ -z "$who" ] || { printf '%s' "$who"; return 0; }
    [ "$(date +%s)" -lt "$deadline" ] || {
      say "FAILED: attempt $no of step $step was never RUNNING within ${TAKEOVER}s." >&2; return 1; }
    sleep 1
  done
}

wait_terminal() {
  local deadline=$(( $(date +%s) + RUN_TIMEOUT )) status
  while :; do
    status=$(q "SELECT status FROM runs WHERE run_id = :'run';")
    case "$status" in DONE|DLQ|CANCELLED) printf '%s' "$status"; return 0 ;; esac
    [ "$(date +%s)" -lt "$deadline" ] || {
      say "FAILED: run $RUN was still \"$status\" after ${RUN_TIMEOUT}s." >&2
      docker compose logs --tail=60 orchestrator >&2 || true; return 1; }
    sleep 1
  done
}

owner_of() { q "SELECT coalesce(owner_id, '') FROM runs WHERE run_id = :'run';"; }

kill_orchestrator() {
  say "  \$ docker compose kill -s KILL orchestrator"
  docker compose kill -s KILL orchestrator >/dev/null 2>&1
}

start_orchestrator() {
  say "  \$ docker compose start orchestrator"
  docker compose start orchestrator >/dev/null 2>&1
  wait_healthy
}

RUN_DIGEST="SELECT md5(
      (SELECT coalesce(string_agg(x, '|'), '') FROM (
         SELECT r.status || r.replay_count || r.planner_attempt_count ||
                coalesce(r.owner_id, '') || coalesce(r.claimed_at::text, '') AS x
           FROM runs r WHERE r.run_id = :'run') t1)
   || (SELECT coalesce(string_agg(x, '|' ORDER BY x), '') FROM (
         SELECT s.step_id::text || s.status || s.attempt_count ||
                coalesce(s.completed_at::text, '') || coalesce(md5(s.output::text), '') AS x
           FROM steps s WHERE s.run_id = :'run') t2)
   || (SELECT coalesce(string_agg(x, '|' ORDER BY x), '') FROM (
         SELECT a.attempt_id::text || a.status || coalesce(a.failure_reason, '') ||
                coalesce(a.finished_at::text, '') || a.dispatched_by AS x
           FROM attempts a WHERE a.run_id = :'run') t3)
   || (SELECT coalesce(string_agg(x, '|' ORDER BY x), '') FROM (
         SELECT d.dlq_id::text || d.reason || d.replay_round || d.attempt_count ||
                coalesce(d.step_id::text, '') AS x
           FROM dead_letter_queue d WHERE d.run_id = :'run') t4));"

STEP_DIGEST="SELECT s.status || '|' || s.attempt_count || '|' || coalesce(s.completed_at::text, '') ||
                    '|' || coalesce(md5(s.output::text), '') || '|' ||
                    coalesce((SELECT string_agg(a.attempt_id::text || ':' || a.status, ','
                                                ORDER BY a.attempt_no)
                                FROM attempts a WHERE a.step_id = s.step_id), '')
               FROM steps s WHERE s.run_id = :'run' AND s.step_name = :'step';"

# ---------------------------------------------------------------------------
# 1. Environment
# ---------------------------------------------------------------------------

hr "1. ENVIRONMENT"

say "Starting from a clean database (CLAUDE.md 5.5.2)."
docker compose down -v --remove-orphans >/dev/null 2>&1 || true

say "docker compose up -d --build"
say "(The orchestrator carries restart: \"no\". Docker restarting it by itself"
say " would take the demonstration away from you - the recovery under test is"
say " Piton's, SPEC.md 13, never the container runtime's.)"
if ! docker compose up -d --build --wait --wait-timeout "$HEALTH_TIMEOUT"; then
  say ""; say "FAILED: the environment did not become healthy within ${HEALTH_TIMEOUT}s."
  docker compose ps || true; docker compose logs --tail=60 orchestrator || true; exit 1
fi
wait_healthy || exit 1
say ""
say "  lease_ttl_seconds=$LEASE_TTL  sweep_interval_seconds=$SWEEP"
say "  A killed orchestrator's runs cannot be taken over until its lease expires"
say "  (SPEC.md 8.5, 8.7), so every takeover below waits at least ${LEASE_TTL}s."

# ---------------------------------------------------------------------------
# 2. Leg 2, first half - a run that is already dead
# ---------------------------------------------------------------------------

hr "2. LEG 2 (first half) - A RUN THAT IS ALREADY IN DLQ BEFORE THE CRASH"

say "SPEC.md 13.2.7: recovery never auto-replays a DLQ'd run. To show that, one"
say "must exist before the crash happens."
begin workflow-dlq.json || exit 1
DLQ_RUN="$RUN"
state=$(wait_terminal) || exit 1
say "  run reached: $state"
[ "$state" = "DLQ" ] || { say "FAILED: this leg needs a DLQ'd run; it reached $state"; exit 1; }

show "SELECT s.step_name, s.status, s.attempt_count FROM steps s WHERE s.run_id = :'run';"
show "SELECT reason, attempt_count FROM dead_letter_queue WHERE run_id = :'run';"

DLQ_BEFORE=$(qraw "$RUN_DIGEST")
say ""
say "  digest of the whole run (run row + steps + attempts + dead-letter rows):"
say "  $DLQ_BEFORE"

# ---------------------------------------------------------------------------
# 3. Leg 1 - SIGKILL while a step is in flight
# ---------------------------------------------------------------------------

hr "3. LEG 1 - SIGKILL WHILE A STEP IS IN FLIGHT (SPEC.md 13.1.1)"

begin workflow-survives-crash.json || exit 1
CRASH_RUN="$RUN"

say ""
say "Waiting until the slow step is actually on the wire, so that the kill is"
say "mid-run rather than hopefully-mid-run."
DISPATCHER_BEFORE=$(wait_attempt_running during-the-crash) || exit 1
say "  attempt in flight, dispatched by: $DISPATCHER_BEFORE"

# The first step is necessarily DONE by now: SPEC.md 4.2 creates steps one at a
# time, so a second step cannot have an attempt until the first has finished.
STEP1_BEFORE=$(qraw "$STEP_DIGEST" -v step=before-the-crash)
OWNER_BEFORE=$(owner_of)
say "  step 1 digest before the crash: $STEP1_BEFORE"
say "  runs.owner_id before the crash: $OWNER_BEFORE"

say ""
kill_orchestrator
OWNER_AFTER_KILL=$(owner_of)
say "  runs.owner_id immediately after SIGKILL: ${OWNER_AFTER_KILL:-<empty>}"
say ""
say "  SPEC.md 8.7 names four writers of coordination metadata - claim,"
say "  heartbeat, release, and any transition out of RUNNING - and all four live"
say "  inside an orchestrator process. A SIGKILL runs none of them."

say ""
say "Assertions:"
check "the run was owned while it was being driven (SPEC.md 6.2)" \
      "SELECT owner_id IS NOT NULL FROM runs WHERE run_id = :'run';"
same  "ownership survived the SIGKILL (SPEC.md 8.7 - no release path ran)" \
      "$OWNER_BEFORE" "$OWNER_AFTER_KILL"

say ""
say "Nothing is running now. Restarting - SPEC.md 8.6: startup recovery is not a"
say "separate code path, it is the first sweep."
start_orchestrator || exit 1

state=$(wait_terminal) || exit 1
say "  run reached: $state"

say ""
say "SPEC.md 13.1.1 - what the operator must see:"
show "SELECT seq, step_name, status, attempt_count FROM steps
       WHERE run_id = :'run' ORDER BY seq;"
show "SELECT s.step_name, a.attempt_no, a.status, a.failure_reason, a.dispatched_by
        FROM attempts a JOIN steps s ON s.step_id = a.step_id
       WHERE a.run_id = :'run' ORDER BY s.seq, a.attempt_no;"

STEP1_AFTER=$(qraw "$STEP_DIGEST" -v step=before-the-crash)
DISPATCHER_AFTER=$(q "SELECT a.dispatched_by FROM attempts a JOIN steps s ON s.step_id = a.step_id
                       WHERE a.run_id = :'run' AND s.step_name = 'during-the-crash' AND a.attempt_no = 2;")

say ""
say "Assertions (SPEC.md 13.1.1, 8.6, 5.3, 6.4):"
check "the run resumed and reached DONE" \
      "SELECT status = 'DONE' FROM runs WHERE run_id = :'run';"
check "every static step exists and is DONE - the run continued past the crash" \
      "SELECT count(*) = $(jq '.planner_static_steps | length' workflow-survives-crash.json)
          AND bool_and(status = 'DONE') FROM steps WHERE run_id = :'run';"
same  "the completed step never re-ran (SPEC.md 13.1.1)" "$STEP1_BEFORE" "$STEP1_AFTER"
check "the in-flight attempt was expired as orphaned (SPEC.md 5.3, 8.6)" \
      "SELECT a.status = 'FAILED' AND a.failure_reason = 'orphaned'
         FROM attempts a JOIN steps s ON s.step_id = a.step_id
        WHERE a.run_id = :'run' AND s.step_name = 'during-the-crash' AND a.attempt_no = 1;"
check "it was expired immediately, well inside its deadline (SPEC.md 8.6)" \
      "SELECT a.finished_at < a.deadline_at
         FROM attempts a JOIN steps s ON s.step_id = a.step_id
        WHERE a.run_id = :'run' AND s.step_name = 'during-the-crash' AND a.attempt_no = 1;"
if [ -z "$DISPATCHER_AFTER" ]; then
  printf '  FAIL  %s\n' "no replacement attempt was dispatched, so the run did not resume"
  FAILED=$((FAILED + 1))
elif [ "$DISPATCHER_AFTER" = "$DISPATCHER_BEFORE" ]; then
  printf '  FAIL  %s\n          both attempts name %s\n' \
         "the replacement attempt names the process that died (SPEC.md 6.4)" "$DISPATCHER_BEFORE"
  FAILED=$((FAILED + 1))
else
  printf '  ok    %s\n' "the replacement attempt was dispatched by a different orchestrator (SPEC.md 6.4)"
  PASSED=$((PASSED + 1))
fi
check "ownership was released when the run left RUNNING (SPEC.md 8.7)" \
      "SELECT owner_id IS NULL AND claimed_at IS NULL FROM runs WHERE run_id = :'run';"

say ""
say "SPEC.md 13.2.1 is a published NON-guarantee: a re-dispatch may re-run a step"
say "that was actually executing, so the worker must be idempotent on step_id."
say "This is how often each step reached the worker - the slow one was called"
say "twice, and that is contract, not a defect:"
# The worker publishes no host port, for the same reason postgres does not, so
# this goes through the container exactly as psql does.
docker compose exec -T worker wget -q -O- http://127.0.0.1:9090/calls 2>/dev/null \
  | jq -c '.calls' || say "  (the worker did not answer)"

# ---------------------------------------------------------------------------
# 4. Leg 2, second half - the crash must not have touched the DLQ'd run
# ---------------------------------------------------------------------------

hr "4. LEG 2 (second half) - THE DLQ'd RUN IS UNTOUCHED (SPEC.md 13.2.7)"

RUN="$DLQ_RUN"
DLQ_AFTER=$(qraw "$RUN_DIGEST")
say "  digest before the crash: $DLQ_BEFORE"
say "  digest after recovery:   $DLQ_AFTER"

say ""
say "Assertions:"
same  "not one byte of the DLQ'd run changed across the crash (SPEC.md 13.2.7)" \
      "$DLQ_BEFORE" "$DLQ_AFTER"
check "it is still in DLQ (SPEC.md 5.5 L4)" \
      "SELECT status = 'DLQ' FROM runs WHERE run_id = :'run';"
check "it was never claimed - the sweep filters on status = RUNNING (SPEC.md 8.6)" \
      "SELECT owner_id IS NULL AND claimed_at IS NULL FROM runs WHERE run_id = :'run';"
check "recovery did not replay it (SPEC.md 14 is milestone delta)" \
      "SELECT replay_count = 0 FROM runs WHERE run_id = :'run';"
check "still exactly one dead-letter entry (SPEC.md 12.4)" \
      "SELECT count(*) = 1 FROM dead_letter_queue WHERE run_id = :'run';"

# ---------------------------------------------------------------------------
# 5. Leg 3 - a clean shutdown releases
# ---------------------------------------------------------------------------

hr "5. LEG 3 - A CLEAN SHUTDOWN RELEASES OWNERSHIP (SPEC.md 8.7)"

begin workflow-clean-shutdown.json || exit 1
wait_attempt_running running-when-asked-to-stop >/dev/null || exit 1
say "  the step is on the wire; stopping the orchestrator politely this time"
say ""
say "  \$ docker compose stop orchestrator      # SIGTERM, not SIGKILL"
docker compose stop -t 30 orchestrator >/dev/null 2>&1
OWNER_AFTER_STOP=$(owner_of)
say "  runs.owner_id after the process exited: ${OWNER_AFTER_STOP:-<empty, which is what SPEC.md 8.7 requires>}"

say ""
say "Assertions:"
if [ -z "$OWNER_AFTER_STOP" ]; then
  printf '  ok    %s\n' "a clean shutdown released ownership at once (SPEC.md 8.7)"
  PASSED=$((PASSED + 1))
else
  printf '  FAIL  %s\n          still owned by %s\n' \
         "a clean shutdown must release ownership (SPEC.md 8.7)" "$OWNER_AFTER_STOP"
  FAILED=$((FAILED + 1))
fi
say ""
say "  Compare leg 1, where a SIGKILL correctly left ownership in place. SPEC.md"
say "  8.7 calls release \"an optimisation that makes failover immediate rather"
say "  than lease_ttl later; correctness does not depend on it\" - and this pair"
say "  of legs is what that sentence looks like from a terminal."

start_orchestrator || exit 1
state=$(wait_terminal) || exit 1
say "  run reached: $state"
check "the released run was picked up again and finished (SPEC.md 13.1.1)" \
      "SELECT status = 'DONE' FROM runs WHERE run_id = :'run';"

# ---------------------------------------------------------------------------
# 6. Leg 4 - a crash loop converges
# ---------------------------------------------------------------------------

hr "6. LEG 4 - A CRASH LOOP CONVERGES TO DLQ (SPEC.md 13.1.4)"

say "The step is killed on every attempt it is ever given. SPEC.md 12.2: every"
say "dispatch increments a persisted counter BEFORE the work begins, so no crash"
say "afterwards can undo it. If that were false, this loop would never end."
begin workflow-crash-loop.json || exit 1

for i in $(seq 1 "$MAX_ATTEMPTS"); do
  say ""
  say "  --- kill $i of $MAX_ATTEMPTS ---"
  say "  waiting for attempt $i to be dispatched (not merely for some attempt to"
  say "  be RUNNING - after a kill the dead one still is)"
  wait_attempt_no killed-every-time "$i" >/dev/null || exit 1
  kill_orchestrator
  start_orchestrator || exit 1
  say "  attempt_count is now: $(q "SELECT attempt_count FROM steps WHERE run_id = :'run';")"
done

state=$(wait_terminal) || exit 1
say ""
say "  run reached: $state"

show "SELECT attempt_no, status, failure_reason, dispatched_by
        FROM attempts WHERE run_id = :'run' ORDER BY attempt_no;"
show "SELECT reason, attempt_count, replay_round FROM dead_letter_queue WHERE run_id = :'run';"

say ""
say "Assertions (SPEC.md 13.1.4, 12.2):"
check "the run converged to DLQ rather than looping forever" \
      "SELECT status = 'DLQ' FROM runs WHERE run_id = :'run';"
check "exactly $MAX_ATTEMPTS attempts - one per unit of budget, burned at dispatch" \
      "SELECT count(*) = $MAX_ATTEMPTS AND bool_and(status = 'FAILED')
         FROM attempts WHERE run_id = :'run';"
check "every attempt is orphaned - each was expired with its dispatcher dead (SPEC.md 5.3)" \
      "SELECT bool_and(failure_reason = 'orphaned') FROM attempts WHERE run_id = :'run';"
check "the step is DLQ with attempt_count = $MAX_ATTEMPTS" \
      "SELECT status = 'DLQ' AND attempt_count = $MAX_ATTEMPTS FROM steps WHERE run_id = :'run';"
check "one worker-side dead-letter entry naming the step (SPEC.md 12.3 L4)" \
      "SELECT count(*) = 1 FROM dead_letter_queue
        WHERE run_id = :'run' AND reason = 'worker_budget_exhausted' AND step_id IS NOT NULL;"

# ---------------------------------------------------------------------------
# 7. Leg 5 - storage unreachable at startup
# ---------------------------------------------------------------------------

hr "7. LEG 5 - STORAGE UNREACHABLE AT STARTUP (SPEC.md 13.1.5)"

say "SPEC.md 13.1.5: fail fast - non-zero exit, and an error message that names"
say "storage as the cause. This is the only one of SPEC.md 13.1's six situations"
say "that cannot be produced by killing something, so it gets its own config"
say "file naming a database host that does not resolve."
say ""
say '  $ docker compose run --rm --no-deps orchestrator --config /etc/piton/piton-badstorage.yaml'
say ""
BADBOOT=$(docker compose run --rm --no-deps --entrypoint /usr/local/bin/piton orchestrator \
            --config /etc/piton/piton-badstorage.yaml 2>&1)
BADBOOT_EXIT=$?
say "  exit status: $BADBOOT_EXIT"
say "  output:"
printf '%s\n' "$BADBOOT" | sed 's/^/    /'

say ""
say "Assertions:"
if [ "$BADBOOT_EXIT" -ne 0 ]; then
  printf '  ok    %s\n' "it exited non-zero (SPEC.md 13.1.5)"; PASSED=$((PASSED + 1))
else
  printf '  FAIL  %s\n' "an orchestrator that cannot reach storage must exit non-zero"
  FAILED=$((FAILED + 1))
fi
if printf '%s' "$BADBOOT" | grep -qiE 'storage|postgres|database|dsn'; then
  printf '  ok    %s\n' "the message names storage as the cause (SPEC.md 13.1.5)"; PASSED=$((PASSED + 1))
else
  printf '  FAIL  %s\n' "the message must name storage as the cause"
  FAILED=$((FAILED + 1))
fi

# ---------------------------------------------------------------------------
# 8. Across the legs
# ---------------------------------------------------------------------------

hr "8. ACROSS ALL FIVE LEGS"

RUN=""
show "SELECT orchestrator_id, last_seen_at,
             last_seen_at > now() - interval '$LEASE_TTL seconds' AS live
        FROM orchestrators ORDER BY last_seen_at;"

say ""
say "Assertions:"
check "every process that ran left a row (SPEC.md 4.3)" \
      "SELECT count(*) > 1 FROM orchestrators;"
check "exactly one orchestrator is live (SPEC.md 8.7)" \
      "SELECT count(*) = 1 FROM orchestrators
        WHERE last_seen_at > now() - interval '$LEASE_TTL seconds';"
check "no run outside RUNNING holds owner_id or claimed_at (SPEC.md 6.2, 8.7)" \
      "SELECT count(*) = 0 FROM runs WHERE status <> 'RUNNING'
          AND (owner_id IS NOT NULL OR claimed_at IS NOT NULL);"
check "nothing was replayed (SPEC.md 14 is milestone delta)" \
      "SELECT bool_and(replay_count = 0) FROM runs;"
check "no planner budget was consumed - the static planner cannot fail (SPEC.md 12.1)" \
      "SELECT bool_and(planner_attempt_count = 0) FROM runs;"

# ---------------------------------------------------------------------------
# 9. Verdict
# ---------------------------------------------------------------------------

hr "9. VERDICT"

printf 'passed: %d\nfailed: %d\n\n' "$PASSED" "$FAILED"
if [ "$FAILED" -ne 0 ]; then
  say "Milestone beta did NOT land. Each FAIL names the SPEC.md rule it came from;"
  say "SPEC.md wins over the code (CLAUDE.md 5.2)."
  exit 1
fi
say "Every assertion held. This is CLAUDE.md 4 step 5 evidence only when the"
say "OWNER has watched it - a green run nobody looked behind is not evidence."
exit 0
