#!/usr/bin/env bash
#
# Milestone theta - the demo script.
#
# CLAUDE.md 4 step 2 asks for "literally: the commands the operator will type in
# a terminal, and the database state he must see afterwards", and CLAUDE.md 5.1
# permits exactly one source for an assertion - SPEC.md. Every check below cites
# the section it came from. Nothing here was derived by reading an
# implementation: at the time this script is written there is none for raw
# dispatch, which is what CLAUDE.md 4 step 2 requires.
#
# WHAT IT DEMONSTRATES
#   SPEC.md 18's table, sixth row: "sync raw body - an unmodifiable HTTP
#   endpoint works as a worker."
#
#   The environment therefore runs a service called `rawapi`, written the way a
#   third-party API is written. It does not know what a run, a step or an
#   attempt is. It never looks for `status` or `output`. It has no idea that
#   anything is retrying it. SPEC.md 9.5 says why raw exists in one sentence -
#   "you cannot add fields to such an endpoint's request body" - and this is
#   that endpoint.
#
# HOW TO RUN IT
#   cd demos/theta && ./demo.sh
#
#   Everything runs inside WSL (CLAUDE.md 8). The script needs curl, jq and
#   docker on the host; it needs no psql, because it reaches the database
#   through `docker compose exec postgres psql`.
#
#   Exit status 0 means every assertion held. Any other status means the
#   milestone did not land, and the failing assertion is named on stdout.
#
# THE THREE LEGS, AND WHY THERE ARE THREE
#   Leg 1  workflow.json              the scenario: two raw steps and one
#                                     envelope step in a single run, ending DONE
#   Leg 2  workflow-raw-non2xx.json   a raw worker answering HTTP 500
#   Leg 3  workflow-raw-not-json.json a raw worker answering 200 with prose
#
#   Legs 2 and 3 are not decoration. SPEC.md 9.6 gives raw mode failure rules
#   that envelope mode does not need - an envelope worker reports failure in a
#   `status` field, so the HTTP code never has to carry the verdict - and the
#   two failures must stay TELLABLE APART, which is SPEC.md 5.3's stated reason
#   for the failure_reason column: the three reasons "name three different
#   repairs - the worker's business logic, the worker's output format, the
#   network".
#
# THE WORKFLOWS, EXPLAINED
#   A .json file cannot carry this comment (SPEC.md 9.1 forbids a .json file
#   whose content is not JSON), so it lives here.
#
#   * planner_type is "static" throughout. Theta is about the worker boundary,
#     not the planner one, so the planner is the one that cannot surprise
#     anybody. SPEC.md 12.1 is why planner_attempt_count must stay 0.
#   * Steps 1 and 2 of workflow.json are sync + raw; step 3 is sync + envelope.
#     SPEC.md 9.7 makes both legal and gives sync + raw to this milestone.
#   * NO StepSpec carries input_from. SPEC.md 9.8 rule 4 rejects input_from
#     beside dispatch_style raw at submission time, and steps 2 and 3 omit it
#     so that SPEC.md 9.4's default - "the previous step only" - is what
#     assembles the inputs map.
#   * Step 1's params DOES contain a key literally named input_from. SPEC.md
#     9.5's last raw paragraph makes that ordinary data, "transmitted verbatim,
#     becoming a top-level key of the raw body". It is in the fixture precisely
#     because it is the rule an implementation is most likely to break by being
#     clever.
#
# THE ONE ASSERTION THAT SURPRISES EVERY READER
#   Step 2's endpoint answers HTTP 200 with a document of the shape
#   {"status":"failure","error":"..."}. In envelope mode that is a worker
#   reporting failure. In raw mode it is nothing of the kind: SPEC.md 9.6's raw
#   row says "any 2xx" succeeds and "the entire response body verbatim is the
#   output". The step must reach DONE with that document stored whole - Piton
#   does not read its own protocol out of a stranger's reply. Section 6 asserts
#   exactly that.
#
# WHY ANY OF THIS IS VISIBLE AT ALL
#   SPEC.md 9.5's raw rules are claims about a wire that leaves no trace. The
#   endpoint therefore answers with what it received - the body, its top-level
#   keys, and the content-type header - so that the wire becomes a value in
#   steps.output, readable in SQL at a terminal (SPEC.md 17.1). Echoing a
#   request back for debugging is something an ordinary API does; it is not
#   Piton knowledge, and nothing in rawapi depends on who called it.

set -euo pipefail

# ---------------------------------------------------------------------------
# 0. Setup
# ---------------------------------------------------------------------------

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

ORCH="localhost:8080"
HEALTH_TIMEOUT=240   # seconds; generous because the first run builds the image
RUN_TIMEOUT=180      # seconds to wait for one run to reach a terminal state
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

# Read from the files themselves so that the fixture and the assertions about
# it cannot drift apart.
N=$(jq '.planner_static_steps | length' workflow.json)
MAX_ATTEMPTS=$(jq '.step_max_attempts' workflow-raw-non2xx.json)
RAW_PARAMS=$(jq -c '.planner_static_steps[0].params' workflow.json)

RUN=""
RUN1=""
RUN2=""
RUN3=""
LAST_STATUS=""
PASSED=0
FAILED=0

hr()  { printf '\n===============================================================\n%s\n\n' "$*"; }
say() { printf '%s\n' "$*"; }

# psql against the demo's own database. SPEC.md 17.1: database truth is the
# interface. Going through `docker compose exec` means the operator needs no
# psql installed and no host port published.
pgx() { docker compose exec -T postgres psql -U piton -d piton -v ON_ERROR_STOP=1 -v run="$RUN" "$@"; }

# SQL is fed on STDIN, never with -c. psql substitutes :'var' only for input it
# reads through its normal lexer; with -c the placeholder is passed through to
# the server untouched and every query below would die on `syntax error at or
# near ":"`. This is not a style choice - it is the difference between the
# assertions running and not running.

# One scalar, unaligned, whitespace stripped.
q() { local sql="$1"; shift; printf '%s\n' "$sql" | pgx "$@" -At | tr -d '[:space:]'; }

# A query printed for the operator's eye, in psql's aligned form.
#
# It ends in `|| true` so that one broken evidence query cannot abort the run -
# and THAT is why every claim this script makes is also a check() below. A show
# that fails prints a psql error where evidence should be, and R49-i records
# what that cost in two other milestones: an assertion is what fails, evidence
# is only what is read.
show() { local sql="$1"; shift; printf '\n%s\n' "$sql"; printf '%s\n' "$sql" | pgx "$@" || true; }

# check <label> <sql returning a boolean>
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

# start_run <workflow file>
#
# Sets RUN to the new run, waits for it to reach a terminal state, and sets
# LAST_STATUS to the state it reached.
#
# It reports through globals rather than by echoing, because `X=$(start_run …)`
# runs the function in a COMMAND-SUBSTITUTION SUBSHELL, and every assignment it
# made - RUN above all - would die with that subshell. The whole script would
# then assert against an empty run_id and psql would answer "invalid input
# syntax for type uuid" fifty times over.
start_run() {
  local file="$1" wf="" run="" status="" deadline
  wf=$(curl -sS -X POST "$ORCH/workflows" -H 'content-type: application/json' \
         -d @"$file" | jq -r .workflow_id)
  if [ -z "$wf" ] || [ "$wf" = "null" ]; then
    say "FAILED: POST /workflows did not return a workflow_id for $file." >&2
    say "SPEC.md 16 and SPEC.md 9.8 list every reason that is a 400." >&2
    curl -sS -X POST "$ORCH/workflows" -H 'content-type: application/json' -d @"$file" >&2 || true
    return 1
  fi
  run=$(curl -sS -X POST "$ORCH/workflows/$wf/runs" -H 'content-type: application/json' \
          -d "{\"input\":$RUN_INPUT,\"overrides\":{}}" | jq -r .run_id)
  if [ -z "$run" ] || [ "$run" = "null" ]; then
    say "FAILED: POST /workflows/$wf/runs did not return a run_id." >&2
    return 1
  fi
  RUN="$run"
  deadline=$(( $(date +%s) + RUN_TIMEOUT ))
  while :; do
    status=$(q "SELECT status FROM runs WHERE run_id = :'run';")
    case "$status" in
      DONE|DLQ|CANCELLED) break ;;
    esac
    if [ "$(date +%s)" -ge "$deadline" ]; then
      say "FAILED: run $RUN was still $status after ${RUN_TIMEOUT}s." >&2
      return 1
    fi
    sleep 1
  done
  LAST_STATUS="$status"
}

diagnose() {
  hr "DIAGNOSTICS"
  say "SPEC.md 17.3 keeps error text in the database, not only in logs. Read the"
  say "database first; the container logs are the second resort."
  show "SELECT status, planner_attempt_count, replay_count
          FROM runs WHERE run_id = :'run';"
  show "SELECT seq, step_name, status, attempt_count FROM steps
         WHERE run_id = :'run' ORDER BY seq;"
  show "SELECT attempt_no, status, connection_mode, failure_reason,
               left(coalesce(error_text, ''), 200) AS error_text_200
          FROM attempts WHERE run_id = :'run' ORDER BY started_at;"
  show "SELECT reason, replay_round, left(error_text, 200) AS error_text_200
          FROM dead_letter_queue WHERE run_id = :'run' ORDER BY created_at;"
  hr "orchestrator logs (last 60 lines)"
  docker compose logs --tail=60 orchestrator || true
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
    printf '  docker compose logs -f rawapi        # what the endpoint was asked\n'
    printf '  curl -sS %s/runs/%s | jq .\n\n' "$ORCH" "${RUN:-RUN_ID}"
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
say "(The orchestrator applies migrations at boot and binds its listener only"
say " afterwards, so a 200 from /healthz is what proves they finished.)"
if ! docker compose up -d --build --wait --wait-timeout "$HEALTH_TIMEOUT"; then
  say ""
  say "FAILED: the environment did not become healthy within ${HEALTH_TIMEOUT}s."
  say ""
  say "If the orchestrator is the service that did not come up, the likely"
  say "reason is simply that milestone theta is not implemented yet: this"
  say "script is written before the code (CLAUDE.md 4 step 2)."
  say ""
  say "If the failure names port 8080, another milestone's environment is still"
  say "up from an earlier hand-run. They all publish 8080. Tear that one down"
  say "with 'docker compose down -v' in its own directory."
  say ""
  docker compose ps || true
  docker compose logs --tail=60 orchestrator || true
  exit 1
fi

# ---------------------------------------------------------------------------
# 2. What the operator types
# ---------------------------------------------------------------------------

hr "2. THE OPERATOR'S COMMANDS"

say ""
say '$ curl -sS localhost:8080/healthz'
deadline=$(( $(date +%s) + HEALTH_TIMEOUT ))
until curl -sS --max-time 5 "$ORCH/healthz"; do
  [ "$(date +%s)" -lt "$deadline" ] || { say ""; say "FAILED: /healthz never answered on the published port."; exit 1; }
  sleep 1
done
say ""

say ""
say '$ WF=$(curl -sS -X POST localhost:8080/workflows \'
say '         -H "content-type: application/json" -d @workflow.json | jq -r .workflow_id)'
say '$ RUN=$(curl -sS -X POST localhost:8080/workflows/$WF/runs \'
say '          -H "content-type: application/json" \'
say "          -d '{\"input\":$RUN_INPUT,\"overrides\":{}}' | jq -r .run_id)"
say ""

start_run workflow.json || { diagnose; exit 1; }
STATUS1="$LAST_STATUS"
RUN1="$RUN"
say "run_id = $RUN1"
say "final status = $STATUS1"

say ""
say '$ curl -sS localhost:8080/runs/$RUN | jq .'
curl -sS "$ORCH/runs/$RUN1" | jq . || true
say ""
say '$ curl -sS localhost:8080/runs/$RUN/steps | jq .'
curl -sS "$ORCH/runs/$RUN1/steps" | jq . || true

# ---------------------------------------------------------------------------
# 3. What the operator must see - printed unedited, for his eye
# ---------------------------------------------------------------------------

hr "3. DATABASE TRUTH (SPEC.md 17.1)"

say "The run, and the three steps it walked."
show "SELECT status, replay_count, planner_attempt_count, owner_id
        FROM runs WHERE run_id = :'run';"
show "SELECT seq, step_name, status, attempt_count,
             decision ->> 'dispatch_style' AS style
        FROM steps WHERE run_id = :'run' ORDER BY seq;"

say ""
say "SPEC.md 9.5: the RAW BODY, as the endpoint received it. This is the whole"
say "milestone in one row - params and nothing else. No run_id, no step_id, no"
say "attempt_id, no connection_mode, no inputs, no callback_url."
show "SELECT jsonb_pretty(output -> 'seen_body') AS raw_body_the_endpoint_received
        FROM steps WHERE run_id = :'run' AND seq = 1;"
show "SELECT output ->> 'seen_content_type' AS content_type_it_was_sent_with
        FROM steps WHERE run_id = :'run' AND seq = 1;"

say ""
say "SPEC.md 9.6: what a RAW step stores - the entire response body, verbatim."
show "SELECT jsonb_pretty(output) AS step_1_output
        FROM steps WHERE run_id = :'run' AND seq = 1;"

say ""
say "Step 2's endpoint answered HTTP 200 with a document that happens to use the"
say "words status and error. SPEC.md 9.6: any 2xx succeeds and the entire body"
say "is the output. Piton does not read its own protocol out of a stranger's"
say "reply, so this step is DONE and the document is stored whole."
show "SELECT status, jsonb_pretty(output) AS step_2_output
        FROM steps WHERE run_id = :'run' AND seq = 2;"

say ""
say "Step 3 is the ENVELOPE step, in the same run. SPEC.md 9.6 stores the"
say "response's output field alone here - so Piton's own wrapper is not in the"
say "row - and SPEC.md 9.5's inputs map carries step 2's stored output into it."
say "A raw worker's result travels exactly like any other."
show "SELECT jsonb_pretty(output -> 'echo' -> 'inputs') AS what_step_3_was_given
        FROM steps WHERE run_id = :'run' AND seq = 3;"

show "SELECT attempt_no, status, connection_mode, failure_reason, replay_round
        FROM attempts WHERE run_id = :'run' ORDER BY started_at;"
show "SELECT count(*) AS dead_letter_entries
        FROM dead_letter_queue WHERE run_id = :'run';"

# ---------------------------------------------------------------------------
# 4. Assertions - the run itself
# ---------------------------------------------------------------------------

hr "4. ASSERTIONS - THE RUN (SPEC.md 18, 5.1, 6.2, 6.3)"

check "the run reached DONE (SPEC.md 18: theta's capability)" \
      "SELECT status = 'DONE' FROM runs WHERE run_id = :'run';"
check "a run that has left RUNNING holds no owner_id (SPEC.md 6.2)" \
      "SELECT owner_id IS NULL FROM runs WHERE run_id = :'run';"
check "nothing replayed this run (SPEC.md 6.2, 14)" \
      "SELECT replay_count = 0 FROM runs WHERE run_id = :'run';"
check "the static planner never failed, so no planner budget was burned (SPEC.md 12.1)" \
      "SELECT planner_attempt_count = 0 FROM runs WHERE run_id = :'run';"
check "runs.input is stored verbatim (SPEC.md 6.2)" \
      "SELECT input = '$RUN_INPUT'::jsonb FROM runs WHERE run_id = :'run';"
check "one step per static StepSpec, $N of them (SPEC.md 9.3)" \
      "SELECT count(*) = $N FROM steps WHERE run_id = :'run';"
check "seq is contiguous from 1 (SPEC.md 3.3)" \
      "SELECT array_agg(seq ORDER BY seq) = (SELECT array_agg(g) FROM generate_series(1, $N) g)
         FROM steps WHERE run_id = :'run';"
check "every step reached DONE (SPEC.md 5.2)" \
      "SELECT bool_and(status = 'DONE') FROM steps WHERE run_id = :'run';"
check "nothing was retried, so every step burned exactly one attempt (SPEC.md 6.3)" \
      "SELECT bool_and(attempt_count = 1) FROM steps WHERE run_id = :'run';"
check "nothing was dead-lettered (SPEC.md 6.5)" \
      "SELECT count(*) = 0 FROM dead_letter_queue WHERE run_id = :'run';"

# ---------------------------------------------------------------------------
# 5. Assertions - the raw REQUEST (SPEC.md 9.5)
# ---------------------------------------------------------------------------

hr "5. ASSERTIONS - THE RAW REQUEST (SPEC.md 9.5)"

say "The negative half matters more than the positive one. An implementation"
say "that sent the envelope AND the params would satisfy 'params arrived'; what"
say "it would not satisfy is that no Piton field arrived at all."
say ""

check "the raw body is params, verbatim (SPEC.md 9.5)" \
      "SELECT (output -> 'seen_body') = '$RAW_PARAMS'::jsonb
         FROM steps WHERE run_id = :'run' AND seq = 1;"
for field in run_id step_id attempt_id connection_mode inputs callback_url; do
  check "the raw body carries NOTHING else: no $field (SPEC.md 9.5)" \
        "SELECT NOT (output -> 'seen_body') ? '$field'
           FROM steps WHERE run_id = :'run' AND seq = 1;"
done
check "the request declared content-type: application/json (SPEC.md 9.5)" \
      "SELECT (output ->> 'seen_content_type') LIKE 'application/json%'
         FROM steps WHERE run_id = :'run' AND seq = 1;"
check "a key named input_from INSIDE params became a top-level key of the body (SPEC.md 9.5)" \
      "SELECT (output -> 'seen_body') ? 'input_from'
         FROM steps WHERE run_id = :'run' AND seq = 1;"
check "and it was transmitted verbatim, not interpreted (SPEC.md 9.5)" \
      "SELECT (output -> 'seen_body' -> 'input_from') = ('$RAW_PARAMS'::jsonb -> 'input_from')
         FROM steps WHERE run_id = :'run' AND seq = 1;"

# ---------------------------------------------------------------------------
# 6. Assertions - the raw RESPONSE (SPEC.md 9.6)
# ---------------------------------------------------------------------------

hr "6. ASSERTIONS - THE RAW RESPONSE (SPEC.md 9.6)"

check "raw stores the ENTIRE response body, so the endpoint's own keys are at the top level" \
      "SELECT output ? 'upper' AND output ? 'seen_body' AND output ? 'seen_content_type'
         FROM steps WHERE run_id = :'run' AND seq = 1;"
check "the endpoint did its own job: it uppercased the text it was given" \
      "SELECT output ->> 'upper' = 'HELLO'
         FROM steps WHERE run_id = :'run' AND seq = 1;"
check "a 2xx that LOOKS like Piton's failure envelope is still a success (SPEC.md 9.6)" \
      "SELECT status = 'DONE' FROM steps WHERE run_id = :'run' AND seq = 2;"
check "and that document is stored whole, words status and error included (SPEC.md 9.6)" \
      "SELECT output ? 'status' AND output ? 'error' AND output ->> 'status' = 'failure'
         FROM steps WHERE run_id = :'run' AND seq = 2;"
check "the attempt behind it is DONE with no failure_reason (SPEC.md 6.4)" \
      "SELECT bool_and(a.status = 'DONE' AND a.failure_reason IS NULL)
         FROM attempts a JOIN steps s USING (step_id)
        WHERE s.run_id = :'run' AND s.seq = 2;"

say ""
say "The other half of SPEC.md 9.6's per-mode rule, in the same run. Either"
say "assertion alone is satisfied by an implementation that treats both modes"
say "the same; together they are what makes the rule observable."
say ""
check "envelope mode stores the output field alone - Piton's wrapper is NOT in the row" \
      "SELECT NOT (output ? 'status') FROM steps WHERE run_id = :'run' AND seq = 3;"
check "what remains is the worker's own result" \
      "SELECT output ? 'echo' FROM steps WHERE run_id = :'run' AND seq = 3;"

say ""
check "inputs holds exactly one entry - input_from omitted means the previous step (SPEC.md 9.4)" \
      "SELECT count(*) = 1 FROM steps s,
              jsonb_object_keys(s.output -> 'echo' -> 'inputs') k
        WHERE s.run_id = :'run' AND s.seq = 3;"
check "inputs is keyed by step_id, and the key is the previous step (SPEC.md 9.5)" \
      "SELECT s3.output -> 'echo' -> 'inputs' ? s2.step_id::text
         FROM steps s3, steps s2
        WHERE s3.run_id = :'run' AND s3.seq = 3
          AND s2.run_id = :'run' AND s2.seq = 2;"
check "the value is that RAW step's stored output - it travels like any other (SPEC.md 9.5)" \
      "SELECT (s3.output -> 'echo' -> 'inputs' -> s2.step_id::text) = s2.output
         FROM steps s3, steps s2
        WHERE s3.run_id = :'run' AND s3.seq = 3
          AND s2.run_id = :'run' AND s2.seq = 2;"

say ""
check "one attempt row per step, all DONE, all sync (SPEC.md 6.4)" \
      "SELECT count(*) = $N AND bool_and(status = 'DONE' AND connection_mode = 'sync'
                                         AND failure_reason IS NULL AND finished_at IS NOT NULL)
         FROM attempts WHERE run_id = :'run';"
check "the attempt carries the same output the owner promoted onto the step (SPEC.md 6.4)" \
      "SELECT bool_and(a.output = s.output) FROM attempts a JOIN steps s USING (step_id)
        WHERE s.run_id = :'run';"

# ---------------------------------------------------------------------------
# 7. Leg 2 - a raw worker answering HTTP 500
# ---------------------------------------------------------------------------

hr "7. LEG 2 - THE ENDPOINT ANSWERS HTTP 500 (SPEC.md 9.6, 5.3)"

say "SPEC.md 9.6: any non-2xx is a failure, and the truncated body is stored as"
say "error text. The endpoint answers a body far longer than SPEC.md 6.4's 4 KB"
say "limit, so that the truncation is observable rather than assumed."
say ""

start_run workflow-raw-non2xx.json || { diagnose; exit 1; }
STATUS2="$LAST_STATUS"
RUN2="$RUN"
say "run_id = $RUN2"
say "final status = $STATUS2"

show "SELECT seq, step_name, status, attempt_count FROM steps
       WHERE run_id = :'run' ORDER BY seq;"
show "SELECT attempt_no, status, failure_reason, octet_length(error_text) AS error_text_bytes
        FROM attempts WHERE run_id = :'run' ORDER BY attempt_no;"
show "SELECT reason, replay_round, left(error_text, 80) AS error_text_80
        FROM dead_letter_queue WHERE run_id = :'run';"

say ""
say "Assertions:"
check "the run stopped in the dead-letter queue (SPEC.md 12.2)" \
      "SELECT status = 'DLQ' FROM runs WHERE run_id = :'run';"
check "step and run went to DLQ together (SPEC.md 12.2, one transaction)" \
      "SELECT status = 'DLQ' FROM steps WHERE run_id = :'run';"
check "the step carries no output (SPEC.md 6.3: set only when DONE)" \
      "SELECT output IS NULL FROM steps WHERE run_id = :'run';"
check "the whole budget was burned: attempt_count = $MAX_ATTEMPTS (SPEC.md 12.2)" \
      "SELECT attempt_count = $MAX_ATTEMPTS FROM steps WHERE run_id = :'run';"
check "one attempt row per dispatch, all FAILED (SPEC.md 6.4)" \
      "SELECT count(*) = $MAX_ATTEMPTS AND bool_and(status = 'FAILED')
         FROM attempts WHERE run_id = :'run';"
check "a non-2xx is transport_error (SPEC.md 5.3)" \
      "SELECT bool_and(failure_reason = 'transport_error') FROM attempts WHERE run_id = :'run';"
check "NOT timeout: the clock decides that, and this endpoint answered at once (SPEC.md 5.3)" \
      "SELECT count(*) = 0 FROM attempts WHERE run_id = :'run' AND failure_reason = 'timeout';"
check "the body reached error_text (SPEC.md 9.6)" \
      "SELECT bool_and(error_text LIKE '%BBBBBBBB%') FROM attempts WHERE run_id = :'run';"
check "error_text is truncated to 4 KB, by the orchestrator before writing (SPEC.md 6.4)" \
      "SELECT bool_and(octet_length(error_text) <= 4096) FROM attempts WHERE run_id = :'run';"
check "exactly one dead-letter entry, worker_budget_exhausted (SPEC.md 6.5, 12.3)" \
      "SELECT count(*) = 1 FROM dead_letter_queue
        WHERE run_id = :'run' AND reason = 'worker_budget_exhausted';"
check "the entry names the step that exhausted its budget (SPEC.md 6.5)" \
      "SELECT d.step_id = s.step_id FROM dead_letter_queue d, steps s
        WHERE d.run_id = :'run' AND s.run_id = :'run';"
check "the entry belongs to round 0 (SPEC.md 6.5, 14)" \
      "SELECT replay_round = 0 FROM dead_letter_queue WHERE run_id = :'run';"

# ---------------------------------------------------------------------------
# 8. Leg 3 - a raw worker answering 200 with prose
# ---------------------------------------------------------------------------

hr "8. LEG 3 - THE ENDPOINT ANSWERS 200 WITH PROSE (SPEC.md 9.6, 5.3)"

say "SPEC.md 9.6: a raw worker's response body MUST be a valid JSON document,"
say "and a 2xx that cannot be parsed as JSON is invalid_response."
say ""
say "This is the rule with the shortest history in the document. It was ruled"
say "because 9.6 promised that the entire body verbatim is the output while 6.3"
say "and 6.4 call an output JSON bytes - and an endpoint answering XML or prose"
say "satisfied one sentence and could not be stored under the other. SPEC.md 9.5"
say "now states the limit too: raw makes any unmodifiable endpoint THAT SPEAKS"
say "JSON a valid worker."
say ""

start_run workflow-raw-not-json.json || { diagnose; exit 1; }
STATUS3="$LAST_STATUS"
RUN3="$RUN"
say "run_id = $RUN3"
say "final status = $STATUS3"

show "SELECT attempt_no, status, failure_reason, left(error_text, 120) AS error_text_120
        FROM attempts WHERE run_id = :'run' ORDER BY attempt_no;"
show "SELECT reason, replay_round FROM dead_letter_queue WHERE run_id = :'run';"

say ""
say "Assertions:"
check "the run stopped in the dead-letter queue (SPEC.md 9.6, 12.2)" \
      "SELECT status = 'DLQ' FROM runs WHERE run_id = :'run';"
check "a 2xx that cannot be parsed as JSON is a FAILED attempt (SPEC.md 9.6)" \
      "SELECT bool_and(status = 'FAILED') FROM attempts WHERE run_id = :'run';"
check "it is invalid_response - a reply arrived and could not be parsed (SPEC.md 9.6, 5.3)" \
      "SELECT bool_and(failure_reason = 'invalid_response') FROM attempts WHERE run_id = :'run';"
check "NOT transport_error: the endpoint was reachable and answered 2xx (SPEC.md 5.3)" \
      "SELECT count(*) = 0 FROM attempts WHERE run_id = :'run' AND failure_reason = 'transport_error';"
check "NOT worker_error: it never spoke Piton's envelope (SPEC.md 5.3)" \
      "SELECT count(*) = 0 FROM attempts WHERE run_id = :'run' AND failure_reason = 'worker_error';"
check "nothing unparseable was stored as an output (SPEC.md 6.3)" \
      "SELECT output IS NULL FROM steps WHERE run_id = :'run';"
check "the body that could not be parsed is kept as diagnostic text (SPEC.md 9.6, 17.3)" \
      "SELECT bool_and(error_text IS NOT NULL AND error_text <> '')
         FROM attempts WHERE run_id = :'run';"
check "exactly one dead-letter entry, worker_budget_exhausted (SPEC.md 6.5, 12.3)" \
      "SELECT count(*) = 1 FROM dead_letter_queue
        WHERE run_id = :'run' AND reason = 'worker_budget_exhausted';"

# ---------------------------------------------------------------------------
# 9. The claim that ties legs 2 and 3 together
# ---------------------------------------------------------------------------

hr "9. THE TWO FAILURES MUST STAY TELLABLE APART (SPEC.md 5.3)"

say "Both runs end identically - DLQ, one worker-side dead-letter entry - and"
say "the only thing that tells the operator which repair to make is"
say "failure_reason. SPEC.md 5.3: the reasons name three different repairs -"
say "the worker's business logic, the worker's output format, the network."
say "An implementation that collapsed these two would pass every assertion in"
say "sections 7 and 8 except this one."
say ""

RUN="$RUN2"
REASON2=$(q "SELECT DISTINCT failure_reason FROM attempts WHERE run_id = :'run';")
RUN="$RUN3"
REASON3=$(q "SELECT DISTINCT failure_reason FROM attempts WHERE run_id = :'run';")

say "leg 2 (HTTP 500)       -> $REASON2"
say "leg 3 (200 with prose) -> $REASON3"
if [ "$REASON2" != "$REASON3" ]; then
  printf '  ok    the two failures are distinguishable (SPEC.md 5.3)\n'
  PASSED=$((PASSED + 1))
else
  printf '  FAIL  the two failures are distinguishable (SPEC.md 5.3)\n'
  printf '          both runs were labelled: %s\n' "$REASON2"
  FAILED=$((FAILED + 1))
fi

# ---------------------------------------------------------------------------
# 10. Summary
# ---------------------------------------------------------------------------

hr "10. RESULT"

say "leg 1  the theta scenario         run $RUN1  -> $STATUS1"
say "leg 2  endpoint answers HTTP 500  run $RUN2  -> $STATUS2"
say "leg 3  endpoint answers prose     run $RUN3  -> $STATUS3"
say ""
say "assertions passed: $PASSED"
say "assertions failed: $FAILED"

if [ "$FAILED" -ne 0 ]; then
  say ""
  say "Milestone theta did NOT land. Each failing assertion above names the"
  say "SPEC.md section it came from; SPEC.md wins (CLAUDE.md 5.2)."
  RUN="$RUN1"
  diagnose
  exit 1
fi

say ""
say "Milestone theta: every assertion held."
say ""
say "CLAUDE.md 4 step 5 is not discharged by this script exiting 0. The owner"
say "reads the database himself, at a terminal - SPEC.md 17.4: a green suite"
say "the owner has never seen behind is not evidence that a milestone landed,"
say "and a green demo is no different."
exit 0
