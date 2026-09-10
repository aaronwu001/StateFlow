#!/usr/bin/env bash
#
# Milestone gamma - the demo script.
#
# SPEC.md 18.2 is the authority for everything below. CLAUDE.md 4 step 2 asks
# for "literally: the commands the operator will type in a terminal, and the
# database state he must see afterwards", and CLAUDE.md 5.1 permits exactly one
# source for a test - SPEC.md. Every assertion here cites the section it came
# from. Nothing was derived by reading the implementation.
#
# WHAT IT DEMONSTRATES (SPEC.md 18.2)
#   An operator watches a step attempts burn against a failing worker -
#   recovering on the last one in leg 1, and exhausting step_max_attempts into
#   the dead-letter queue in legs 2, 3 and 4, one for each way a worker fails.
#
#     leg 1  fail twice, succeed on the third and last attempt
#     leg 2  worker replies 200 with a failure envelope   -> worker_error
#     leg 3  worker replies HTTP 500                      -> transport_error
#     leg 4  nothing listening on the port                -> transport_error
#
#   Legs 3 and 4 share a failure_reason on purpose. SPEC.md 5.3 defines
#   transport_error as "the HTTP exchange did not produce a usable reply -
#   non-2xx, connection refused, DNS failure, connection reset", so a demo that
#   showed only one of them would leave half of that definition unexercised.
#
# HOW TO RUN IT
#   cd demos/gamma && ./demo.sh
#
#   Everything runs inside WSL (CLAUDE.md 8). The script needs curl, jq and
#   docker on the host; it needs no psql, because it reaches the database
#   through `docker compose exec postgres psql`.
#
#   Exit status 0 means every assertion held. Any other status means the
#   milestone did not land, and the failing assertion is named on stdout.
#
# WHAT THE SCRIPT PRINTS, AND IN WHICH VOICE
#   Each leg prints SPEC.md 18.2 queries and their output unedited, for the
#   operator eye. That is step 5 of CLAUDE.md 4 - the hand-run inspection the
#   automated suite may never replace. It then asserts.
#
# WHY FOUR RUNS AND NOT ONE
#   A run stops at the first step that exhausts its budget (SPEC.md 12.2:
#   step -> DLQ and run -> DLQ in one transaction), so three ways of failing
#   cannot be shown by one run. All four legs run against ONE environment,
#   which is what SPEC.md 18.2 requires of demo.sh.
#
# WHY EACH DLQ LEG DECLARES A SECOND STATIC STEP THAT IS NEVER CREATED
#   SPEC.md 18.2 requires "no step created after it". A workflow whose
#   planner_static_steps ends at the failing step could not show that: there
#   would be nothing the planner could have created even if it had been asked.
#   The second element makes the absence evidence rather than a tautology.

set -uo pipefail

ORCH="http://localhost:8080"
HEALTH_TIMEOUT=240
RUN_TIMEOUT=120
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

# Read from the workflow files themselves rather than written as literals, so
# that an assertion and the workflow it is about cannot drift apart.
MAX_ATTEMPTS=$(jq '.step_max_attempts' workflow-retry.json)
FAIL_TIMES=$(jq '.planner_static_steps[0].params.fail_times' workflow-retry.json)

RUN=""
PASSED=0
FAILED=0

hr()  { printf '\n===============================================================\n%s\n\n' "$*"; }
say() { printf '%s\n' "$*"; }

# psql against the demo database. SPEC.md 17.1: database truth is the
# interface. Going through `docker compose exec` means the operator needs no
# psql installed and no host port published.
pgx() { docker compose exec -T postgres psql -U piton -d piton -v ON_ERROR_STOP=1 -v run="$RUN" "$@"; }

# SQL is fed on STDIN, never with -c: psql substitutes :'var' only for input it
# reads through its normal lexer.
q() { local sql="$1"; shift; printf '%s\n' "$sql" | pgx "$@" -At | tr -d '[:space:]'; }

# A query printed for the operator eye, in the aligned form psql uses when a
# human is reading.
show() { local sql="$1"; shift; printf '\n%s\n' "$sql"; printf '%s\n' "$sql" | pgx "$@" || true; }

# check <label> <sql returning a boolean> [extra psql args...]
check() {
  local label="$1" sql="$2"; shift 2
  local out=""
  if ! out="$(q "$sql" "$@" 2>&1)"; then
    printf '  FAIL  %s\n          query error: %s\n' "$label" "$out"
    FAILED=$((FAILED + 1)); return 0
  fi
  if [ "$out" = "t" ]; then
    printf '  ok    %s\n' "$label"
    PASSED=$((PASSED + 1))
  else
    printf '  FAIL  %s\n          expected t, got: %s\n' "$label" "${out:-<empty>}"
    FAILED=$((FAILED + 1))
  fi
  return 0
}

finish() {
  local status=$?
  if [ "$TEARDOWN_AT_END" -eq 1 ]; then
    hr "TEARDOWN"
    docker compose down -v --remove-orphans || true
  else
    printf '\nThe environment is still up, so you can look inside it by hand\n'
    printf '(SPEC.md 17 - there is no UI, the terminal is the interface):\n\n'
    printf '  docker compose exec postgres psql -U piton -d piton\n'
    printf '  docker compose logs -f orchestrator\n\n'
    printf 'Tear it down with the volume wipe CLAUDE.md 5.5 requires:\n\n'
    printf '  docker compose down -v\n\n'
  fi
  exit "$status"
}
trap finish EXIT

# ---------------------------------------------------------------------------
# 1. Bring the environment up
# ---------------------------------------------------------------------------

hr "1. ENVIRONMENT"

say "Starting from a clean database (CLAUDE.md 5.5.2)."
docker compose down -v --remove-orphans >/dev/null 2>&1 || true

say "docker compose up -d --build"
say "(There is no migration service, so a 200 from /healthz is what proves the"
say " migrations finished.)"
if ! docker compose up -d --build --wait --wait-timeout "$HEALTH_TIMEOUT"; then
  say ""
  say "FAILED: the environment did not become healthy within ${HEALTH_TIMEOUT}s."
  docker compose ps || true
  docker compose logs --tail=60 orchestrator || true
  exit 1
fi

say ""
say '$ curl -sS localhost:8080/healthz'
deadline=$(( $(date +%s) + HEALTH_TIMEOUT ))
until curl -sS --max-time 5 "$ORCH/healthz"; do
  [ "$(date +%s)" -lt "$deadline" ] || { say ""; say "FAILED: /healthz never answered."; exit 1; }
  sleep 1
done
say ""

# ---------------------------------------------------------------------------
# Helpers that speak SPEC.md 10.1 - the two control calls every leg makes
# ---------------------------------------------------------------------------

# start_leg <workflow file>  ->  sets RUN, or exits non-zero
start_leg() {
  local file="$1" wf="" body=""
  body=$(curl -sS -X POST "$ORCH/workflows" -H 'content-type: application/json' -d @"$file")
  wf=$(printf '%s' "$body" | jq -r '.workflow_id // empty')
  if [ -z "$wf" ]; then
    say "FAILED: POST /workflows did not return a workflow_id for $file"
    say "  body: $body"
    say "  SPEC.md 16 lists every reason this is a 400; the body says which."
    return 1
  fi
  body=$(curl -sS -X POST "$ORCH/workflows/$wf/runs" -H 'content-type: application/json' \
           -d "{\"input\":$RUN_INPUT,\"overrides\":{}}")
  RUN=$(printf '%s' "$body" | jq -r '.run_id // empty')
  if [ -z "$RUN" ]; then
    say "FAILED: POST /workflows/{id}/runs did not return a run_id for $file"
    say "  body: $body"
    return 1
  fi
  printf '  workflow_id=%s\n  run_id=%s\n' "$wf" "$RUN"
  return 0
}

# wait_terminal  ->  prints the state RUN reached, or exits non-zero on timeout
#
# It polls the DATABASE, not the API: SPEC.md 17.1 makes database truth the
# interface, and SPEC.md 10.2 does not fix the JSON field name carrying a run
# status, so polling the API would mean inventing a contract no ruling covers.
wait_terminal() {
  local deadline status
  deadline=$(( $(date +%s) + RUN_TIMEOUT ))
  while :; do
    status=$(q "SELECT status FROM runs WHERE run_id = :'run';")
    case "$status" in
      DONE|DLQ|CANCELLED) printf '%s' "$status"; return 0 ;;
    esac
    if [ "$(date +%s)" -ge "$deadline" ]; then
      # stderr, not stdout: this function is called inside $( ), so anything on
      # stdout would be captured as the state rather than shown to the operator.
      say "" >&2
      say "FAILED: run $RUN was still \"$status\" after ${RUN_TIMEOUT}s." >&2
      docker compose logs --tail=60 orchestrator >&2 || true
      return 1
    fi
    sleep 1
  done
}

# ---------------------------------------------------------------------------
# 2. Leg 1 - the recovering retry (SPEC.md 18.2)
# ---------------------------------------------------------------------------

hr "2. LEG 1 - THE RECOVERING RETRY (workflow-retry.json)"

say "The worker fails ${FAIL_TIMES} times, then succeeds. step_max_attempts is"
say "${MAX_ATTEMPTS}, so the success lands on the last attempt the budget allows."
say ""
start_leg workflow-retry.json || exit 1
state=$(wait_terminal) || exit 1
say "  run reached: $state"

say ""
say "SPEC.md 18.2 - what the operator must see:"
show "SELECT status, attempt_count FROM steps WHERE run_id = :'run';"
show "SELECT attempt_no, status, failure_reason FROM attempts
       WHERE run_id = :'run' ORDER BY attempt_no;"
show "SELECT count(*) FROM dead_letter_queue WHERE run_id = :'run';"
show "SELECT status FROM runs WHERE run_id = :'run';"

say ""
say "Assertions (SPEC.md 18.2):"
check "run reached DONE" \
      "SELECT status = 'DONE' FROM runs WHERE run_id = :'run';"
check "exactly one step" \
      "SELECT count(*) = 1 FROM steps WHERE run_id = :'run';"
check "the step is DONE with attempt_count = ${MAX_ATTEMPTS} (SPEC.md 11.1: a TOTAL attempt count, not a retry count)" \
      "SELECT status = 'DONE' AND attempt_count = ${MAX_ATTEMPTS}
         FROM steps WHERE run_id = :'run';"
check "three attempts: ${FAIL_TIMES} FAILED then DONE (SPEC.md 4.2, budget burned at dispatch)" \
      "SELECT array_agg(status ORDER BY attempt_no) = ARRAY['FAILED','FAILED','DONE']
         FROM attempts WHERE run_id = :'run';"
check "every FAILED attempt names a reason (SPEC.md 6.4 invariant 1)" \
      "SELECT count(*) = 0 FROM attempts
        WHERE run_id = :'run' AND status = 'FAILED' AND failure_reason IS NULL;"
check "the DONE attempt names none (SPEC.md 6.4 invariant 2)" \
      "SELECT count(*) = 0 FROM attempts
        WHERE run_id = :'run' AND status <> 'FAILED' AND failure_reason IS NOT NULL;"
check "finished_at set on every attempt that left RUNNING (SPEC.md 6.4 invariant 3)" \
      "SELECT count(*) = 0 FROM attempts
        WHERE run_id = :'run' AND (status = 'RUNNING') <> (finished_at IS NULL);"
check "no dead-letter entry (SPEC.md 18.2)" \
      "SELECT count(*) = 0 FROM dead_letter_queue WHERE run_id = :'run';"
check "the step carries output (SPEC.md 6.3: set when status = DONE)" \
      "SELECT output IS NOT NULL FROM steps WHERE run_id = :'run';"

# ---------------------------------------------------------------------------
# 3. Legs 2, 3 and 4 - worker-side DLQ (SPEC.md 18.2, 12.3 L4)
# ---------------------------------------------------------------------------

# dlq_leg <label> <workflow file> <expected failure_reason>
dlq_leg() {
  local label="$1" file="$2" reason="$3" state=""

  hr "$label ($file)"
  start_leg "$file" || return 1
  state=$(wait_terminal) || return 1
  say "  run reached: $state"

  say ""
  say "SPEC.md 18.2 - what the operator must see:"
  show "SELECT status, owner_id, claimed_at FROM runs WHERE run_id = :'run';"
  show "SELECT seq, step_name, status, attempt_count FROM steps
         WHERE run_id = :'run' ORDER BY seq;"
  show "SELECT attempt_no, status, failure_reason,
               left(coalesce(error_text, ''), 60) AS error_text_60
          FROM attempts WHERE run_id = :'run' ORDER BY attempt_no;"
  show "SELECT reason, step_id, replay_round
          FROM dead_letter_queue WHERE run_id = :'run';"
  say "SPEC.md 6.5: the entry no longer copies the budget consumed. How much"
  say "this round burned is a count of the attempts carrying its replay_round,"
  say "read from the table that owns the number rather than from a copy."
  show "SELECT a.replay_round, count(*) AS attempts_in_that_round
          FROM attempts a WHERE a.run_id = :'run' GROUP BY 1 ORDER BY 1;"

  say ""
  say "Assertions (SPEC.md 18.2):"
  check "run reached DLQ" \
        "SELECT status = 'DLQ' FROM runs WHERE run_id = :'run';"
  check "owner_id and claimed_at are both NULL (SPEC.md 6.2, 8.7: a run that left RUNNING is not owned)" \
        "SELECT owner_id IS NULL AND claimed_at IS NULL FROM runs WHERE run_id = :'run';"
  check "exactly one step exists - no step was created after the failing one (SPEC.md 18.2)" \
        "SELECT count(*) = 1 FROM steps WHERE run_id = :'run';"
  check "the failing step is DLQ with attempt_count = ${MAX_ATTEMPTS}" \
        "SELECT status = 'DLQ' AND attempt_count = ${MAX_ATTEMPTS}
           FROM steps WHERE run_id = :'run' AND seq = 1;"
  check "${MAX_ATTEMPTS} attempts, all FAILED" \
        "SELECT count(*) = ${MAX_ATTEMPTS} AND bool_and(status = 'FAILED')
           FROM attempts WHERE run_id = :'run';"
  check "every attempt carries failure_reason = $reason (SPEC.md 5.3)" \
        "SELECT bool_and(failure_reason = :'reason')
           FROM attempts WHERE run_id = :'run';" -v reason="$reason"
  check "every attempt is sync (SPEC.md 9.7: the only combination legal before epsilon)" \
        "SELECT bool_and(connection_mode = 'sync') FROM attempts WHERE run_id = :'run';"
  check "finished_at set on every attempt (SPEC.md 6.4 invariant 3)" \
        "SELECT bool_and(finished_at IS NOT NULL) FROM attempts WHERE run_id = :'run';"
  check "exactly one dead-letter entry (SPEC.md 12.4: one per round the run lands in DLQ)" \
        "SELECT count(*) = 1 FROM dead_letter_queue WHERE run_id = :'run';"
  check "the entry is worker_budget_exhausted and names the failed step (SPEC.md 12.3 L4)" \
        "SELECT reason = 'worker_budget_exhausted'
                AND step_id = (SELECT step_id FROM steps WHERE run_id = :'run' AND seq = 1)
           FROM dead_letter_queue WHERE run_id = :'run';"
  check "the entry records replay_round = 0 (SPEC.md 6.5)" \
        "SELECT replay_round = 0 FROM dead_letter_queue WHERE run_id = :'run';"
  check "that round burned ${MAX_ATTEMPTS} attempts, counted from the rows that carry it (SPEC.md 6.4, 6.5)" \
        "SELECT count(*) = ${MAX_ATTEMPTS}
           FROM attempts a JOIN dead_letter_queue d ON d.step_id = a.step_id
          WHERE d.run_id = :'run' AND a.replay_round = d.replay_round;"
  check "the entry explains itself (SPEC.md 17: the database explains itself)" \
        "SELECT length(error_text) > 0 FROM dead_letter_queue WHERE run_id = :'run';"
  return 0
}

dlq_leg "3. LEG 2 - THE WORKER REPORTS FAILURE  -> worker_error" \
        workflow-worker-error.json worker_error || exit 1

dlq_leg "4. LEG 3 - THE WORKER REPLIES HTTP 500 -> transport_error" \
        workflow-http-500.json transport_error || exit 1

dlq_leg "5. LEG 4 - NOTHING IS LISTENING        -> transport_error" \
        workflow-unreachable.json transport_error || exit 1

# Leg 4 alone can show SPEC.md 5.3 clock rule, so the assertion belongs here
# rather than inside dlq_leg: the connection is refused instantly, far inside
# step_timeout_seconds, and the attempt must NOT be labelled timeout.
say ""
say "Leg 4 only - SPEC.md 5.3: timeout and transport_error are decided by the"
say "clock, not by the shape of the error."
check "no attempt of this run is labelled timeout, though nothing ever answered" \
      "SELECT count(*) = 0 FROM attempts
        WHERE run_id = :'run' AND failure_reason IN ('timeout', 'orphaned');"
check "every attempt finished well inside its deadline" \
      "SELECT bool_and(finished_at < deadline_at) FROM attempts WHERE run_id = :'run';"

# ---------------------------------------------------------------------------
# 6. Across all four legs
# ---------------------------------------------------------------------------

hr "6. ACROSS ALL FOUR LEGS"

RUN=""   # these queries are about the whole database, not one run
say "SPEC.md 5.6 calls run = RUNNING, last_step = DLQ impossible, because"
say "SPEC.md 12.2 writes the step, the run and the dead-letter entry in one"
say "transaction. Gamma is the first milestone that could ever have seen it."
show "SELECT r.status AS run_status, count(s.step_id) AS steps,
             count(d.dlq_id) AS dlq_entries
        FROM runs r
        LEFT JOIN steps s ON s.run_id = r.run_id
        LEFT JOIN dead_letter_queue d ON d.run_id = r.run_id
       GROUP BY r.run_id, r.status ORDER BY r.status;"

say ""
say "Assertions:"
check "four runs exist: one DONE, three DLQ (SPEC.md 18.2)" \
      "SELECT count(*) FILTER (WHERE status = 'DONE') = 1
          AND count(*) FILTER (WHERE status = 'DLQ') = 3
          AND count(*) = 4
         FROM runs;"
check "no run is RUNNING with a DLQ last step (SPEC.md 5.6 - impossible)" \
      "SELECT count(*) = 0 FROM runs r
        WHERE r.status = 'RUNNING'
          AND (SELECT s.status FROM steps s WHERE s.run_id = r.run_id
                ORDER BY s.seq DESC LIMIT 1) = 'DLQ';"
check "every DLQ run has exactly one dead-letter entry (SPEC.md 12.4)" \
      "SELECT bool_and(n = 1) FROM (
         SELECT count(d.dlq_id) AS n FROM runs r
           LEFT JOIN dead_letter_queue d ON d.run_id = r.run_id
          WHERE r.status = 'DLQ' GROUP BY r.run_id) x;"
check "the DONE run has none (SPEC.md 18.2)" \
      "SELECT count(*) = 0 FROM dead_letter_queue d
         JOIN runs r ON r.run_id = d.run_id WHERE r.status = 'DONE';"
check "no planner budget was consumed - the static planner cannot fail (SPEC.md 12.1)" \
      "SELECT bool_and(planner_attempt_count = 0) FROM runs;"
check "no run has been replayed (SPEC.md 14 is milestone delta)" \
      "SELECT bool_and(replay_count = 0) FROM runs;"

# ---------------------------------------------------------------------------
# 7. Verdict
# ---------------------------------------------------------------------------

hr "7. VERDICT"

printf 'passed: %d\nfailed: %d\n\n' "$PASSED" "$FAILED"
if [ "$FAILED" -ne 0 ]; then
  say "Milestone gamma did NOT land. Each FAIL above names the SPEC.md rule it"
  say "came from; SPEC.md wins over the code (CLAUDE.md 5.2)."
  exit 1
fi
say "Every assertion held. This is CLAUDE.md 4 step 5 evidence only when the"
say "OWNER has watched it - a green run nobody looked behind is not evidence."
exit 0
