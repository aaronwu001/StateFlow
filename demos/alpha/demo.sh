#!/usr/bin/env bash
#
# Milestone alpha - the demo script.
#
# SPEC.md 18.1 is the authority for everything below. CLAUDE.md 4 step 2 asks
# for "literally: the commands the operator will type in a terminal, and the
# database state he must see afterwards", and CLAUDE.md 5.1 permits exactly one
# source for a test - SPEC.md. Every assertion here cites the section it came
# from. Nothing here was derived by reading an implementation, because none
# exists yet: this script is written before the code (CLAUDE.md 4, R20-a).
#
# WHAT IT DEMONSTRATES (SPEC.md 18.1)
#   An operator creates a workflow with a static planner, starts a run, and the
#   run walks through every static step against a sync envelope worker and
#   reaches DONE.
#
# HOW TO RUN IT
#   cd demos/alpha && ./demo.sh
#
#   Everything runs inside WSL (CLAUDE.md 8). The script needs curl, jq and
#   docker on the host; it needs no psql, because it reaches the database
#   through `docker compose exec postgres psql`.
#
#   Exit status 0 means every assertion held. Any other status means the
#   milestone did not land, and the failing assertion is named on stdout.
#
# WHAT THE SCRIPT PRINTS, AND IN WHICH VOICE
#   Section 4 prints SPEC.md 18.1's five queries and their output unedited, for
#   the operator's eye. That is step 5 of CLAUDE.md 4 - the hand-run inspection
#   that the automated suite may never replace.
#   Sections 5 and 6 then assert. Section 5 holds only what SPEC.md 18.1
#   literally requires. Section 6 holds checks derived from other ratified SPEC
#   sections, kept separate so the owner can see exactly which are which.
#
# THE WORKFLOW, EXPLAINED
#   workflow.json cannot carry this comment - SPEC.md 9.1 forbids a .json file
#   whose content is not JSON, and JSON has no comments - so it lives here.
#
#   * planner_type is "static". SPEC.md 6.1: the built-in static planner
#     answers planner_static_steps[n] where n is the number of steps the run
#     already has, and answers "done" once n reaches the end of the array. It
#     never answers "fail", which is why SPEC.md 18.1 expects
#     planner_attempt_count = 0 (SPEC.md 12.1 states this explicitly).
#   * Every step is sync + envelope. SPEC.md 9.7 makes that the only legal
#     combination in alpha; sync + raw is theta and async + envelope is epsilon.
#   * timeout_seconds and max_attempts are omitted from every StepSpec, not
#     written as null. SPEC.md 9.8 rule 5 rejects a non-null value before
#     milestone eta, and SPEC.md 9.4 already defaults them to null.
#   * The first step carries "input_from": [], which SPEC.md 9.4 defines as
#     "nothing". Steps 2 and 3 omit input_from, which SPEC.md 9.4 defines as
#     "the previous step only". Those are the only two values a static planner
#     can use - input_from is an array of step_id, and step_ids are UUIDs
#     assigned at run time, which a static array cannot know. Section 6 asserts
#     that both behaved as SPEC.md 9.4 says.
#   * The five configuration numbers are written out at SPEC.md 11.1's own
#     defaults, so the file documents the shape rather than relying on it.
#
# ONE THING THAT SURPRISES EVERY FIRST READER
#   The run is created with input {"text":"hello"}, and no worker ever sees it.
#   SPEC.md 9.5's envelope carries params and inputs and no workflow input; the
#   run's input reaches a planner (SPEC.md 9.2 workflow_input) and the static
#   planner ignores it by construction. The input is therefore asserted where
#   it does live - runs.input, SPEC.md 6.2, "stored verbatim" - and nowhere
#   else. This is correct alpha behaviour, not a gap.
#
# TWO PLACES WHERE SPEC.md 18.1's SQL IS ADJUSTED, AND WHY
#   1. octet_length(output) becomes octet_length(output::text). SPEC.md 7.1
#      says the Postgres implementation stores JSON columns as jsonb, and
#      Postgres has no octet_length(jsonb). The cast is also correct if a
#      backend chose text or bytea instead.
#   2. :run becomes :'run'. Same placeholder; :'run' is psql's form that quotes
#      the substituted value.
#
# THE ONE COLUMN 18.1 SELECTS WITHOUT STATING AN EXPECTATION
#   runs.owner_id. SPEC.md 18.1 prints it and says nothing about it, but
#   SPEC.md 6.2's runs invariants do: "owner_id and claimed_at are non-NULL
#   only while status = 'RUNNING', and are always written and cleared as a
#   pair". The run is DONE, so both must be NULL, and section 6 asserts exactly
#   that.
#
#   SPEC.md 8.7 carries the mechanism that makes the invariant true rather than
#   merely asserted: coordination metadata is written in four places, the
#   fourth being "any transition of a run out of RUNNING", which covers the
#   three exits DONE, DLQ and CANCELLED. Both sections are ratified, so this is
#   a derivation and not an assumption - CLAUDE.md 5.1 permits SPEC as a test's
#   only source.

set -euo pipefail

# ---------------------------------------------------------------------------
# 0. Setup
# ---------------------------------------------------------------------------

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

ORCH="localhost:8080"
HEALTH_TIMEOUT=240   # seconds; generous because the first run builds the image
RUN_TIMEOUT=120      # seconds to wait for the run to reach a terminal state
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

# How many steps the static planner will produce, and how long "recently seen"
# means - both read from the files themselves so they cannot drift apart from
# the assertions that use them.
N=$(jq '.planner_static_steps | length' workflow.json)
LEASE_TTL=$(sed -n 's/^lease_ttl_seconds:[[:space:]]*\([0-9][0-9]*\).*/\1/p' piton.yaml)
[ -n "$LEASE_TTL" ] || { echo "demo.sh: lease_ttl_seconds not found in piton.yaml" >&2; exit 2; }

RUN=""
WF=""
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

diagnose() {
  hr "DIAGNOSTICS"
  say "SPEC.md 17.3 keeps error text in the database, not only in logs. Read the"
  say "database first; the container logs are the second resort."
  show "SELECT status, planner_attempt_count, replay_count
          FROM runs WHERE run_id = :'run';"
  say "SPEC.md 6.8: the planner's own calls. This is where a planner-side"
  say "diagnosis lives, now that SPEC.md 6.2 keeps no copy of it on the run."
  show "SELECT call_no, replay_round, status, answer, failure_reason
          FROM planner_calls WHERE run_id = :'run' ORDER BY call_no;"
  show "SELECT seq, step_name, status, attempt_count FROM steps
         WHERE run_id = :'run' ORDER BY seq;"
  show "SELECT attempt_no, status, connection_mode, failure_reason,
               left(coalesce(error_text, ''), 200) AS error_text_200
          FROM attempts WHERE run_id = :'run' ORDER BY started_at;"
  show "SELECT reason, replay_round, attempt_count,
               left(error_text, 200) AS error_text_200
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
    printf '  docker compose logs -f orchestrator\n'
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
say "(SPEC.md 18.1: migrations run to completion before the orchestrator serves"
say " traffic. There is no migration service, so a 200 from /healthz is what"
say " proves they finished.)"
if ! docker compose up -d --build --wait --wait-timeout "$HEALTH_TIMEOUT"; then
  say ""
  say "FAILED: the environment did not become healthy within ${HEALTH_TIMEOUT}s."
  say ""
  say "If the orchestrator is the service that did not come up, the likely"
  say "reason is simply that milestone alpha is not implemented yet: this"
  say "script is written before the code (CLAUDE.md 4 step 2). For alpha to"
  say "pass, the orchestrator must apply its migrations at boot, then serve"
  say "POST /workflows, POST /workflows/{id}/runs, GET /runs/{run_id},"
  say "GET /runs/{run_id}/steps and GET /healthz (SPEC.md 18.1)."
  say ""
  docker compose ps || true
  docker compose logs --tail=60 orchestrator || true
  exit 1
fi

# ---------------------------------------------------------------------------
# 2. What the operator types - SPEC.md 18.1, verbatim
# ---------------------------------------------------------------------------

hr "2. THE OPERATOR'S COMMANDS (SPEC.md 18.1)"

say ""
say '$ curl -sS localhost:8080/healthz'
deadline=$(( $(date +%s) + HEALTH_TIMEOUT ))
until curl -sS --max-time 5 "$ORCH/healthz"; do
  [ "$(date +%s)" -lt "$deadline" ] || { say ""; say "FAILED: /healthz never answered on the published port."; exit 1; }
  sleep 1
done
say ""

say ""
say '$ WF=$(curl -sS -X POST localhost:8080/workflows -d @workflow.json | jq -r .workflow_id)'
WF_BODY=$(curl -sS -X POST "$ORCH/workflows" \
            -H 'content-type: application/json' -d @workflow.json)
say "$WF_BODY" | jq . 2>/dev/null || say "$WF_BODY"
WF=$(printf '%s' "$WF_BODY" | jq -r '.workflow_id // empty')
if [ -z "$WF" ] || [ "$WF" = "null" ]; then
  say ""
  say "FAILED: POST /workflows did not return a workflow_id."
  say "SPEC.md 16 lists every reason this is a 400; the body above says which."
  exit 1
fi
say "WF=$WF"

say ""
say '$ RUN=$(curl -sS -X POST localhost:8080/workflows/$WF/runs \'
say '         -d '"'"'{"input":{"text":"hello"},"overrides":{}}'"'"' | jq -r .run_id)'
RUN_BODY=$(curl -sS -X POST "$ORCH/workflows/$WF/runs" \
             -H 'content-type: application/json' \
             -d "{\"input\":$RUN_INPUT,\"overrides\":{}}")
say "$RUN_BODY" | jq . 2>/dev/null || say "$RUN_BODY"
RUN=$(printf '%s' "$RUN_BODY" | jq -r '.run_id // empty')
if [ -z "$RUN" ] || [ "$RUN" = "null" ]; then
  say ""
  say "FAILED: POST /workflows/{id}/runs did not return a run_id."
  say "Note that overrides is sent EMPTY: SPEC.md 11.2 makes any non-empty"
  say "value a 400 until milestone eta, and the field exists now only because"
  say "the request shape is a contract others build against."
  exit 1
fi
say "RUN=$RUN"

say ""
say '$ curl -sS localhost:8080/runs/$RUN'
curl -sS "$ORCH/runs/$RUN" | jq . 2>/dev/null || true

say ""
say '$ curl -sS localhost:8080/runs/$RUN/steps'
curl -sS "$ORCH/runs/$RUN/steps" | jq . 2>/dev/null || true

# ---------------------------------------------------------------------------
# 3. Wait for the run to finish
# ---------------------------------------------------------------------------

hr "3. WAITING FOR THE RUN TO REACH A TERMINAL STATE"

# The wait polls the DATABASE, not the API. SPEC.md 17.1 makes database truth
# the interface, and SPEC.md 10.2 does not fix the JSON field name that carries
# a run's status in GET /runs/{run_id} - so polling the API here would mean
# inventing a wire contract that no ruling covers. runs.status IS specified,
# in SPEC.md 5.1 and 6.2.
saw_running=0
final=""
deadline=$(( $(date +%s) + RUN_TIMEOUT ))
while :; do
  final="$(q "SELECT status FROM runs WHERE run_id = :'run';" || true)"
  case "$final" in
    RUNNING) saw_running=1 ;;
    DONE|DLQ|CANCELLED) break ;;
  esac
  if [ "$(date +%s)" -ge "$deadline" ]; then
    say "FAILED: the run was still '$final' after ${RUN_TIMEOUT}s."
    diagnose
    exit 1
  fi
  sleep 1
done

say "run status is now: $final"
# SPEC.md 18.1 writes the expectation as "RUNNING -> DONE". The DONE end state
# is asserted below. Whether this script happened to CATCH the run in RUNNING
# is a race - the echo pipeline finishes in milliseconds - so it is reported as
# an observation and never asserted.
if [ "$saw_running" -eq 1 ]; then
  say "observed the intermediate RUNNING state: yes (timing-dependent, not asserted)"
else
  say "observed the intermediate RUNNING state: no  (timing-dependent, not asserted)"
  say "  SPEC.md 5.1 makes RUNNING the state a run is created in, so this only"
  say "  means the run finished before the first poll."
fi

if [ "$final" != "DONE" ]; then
  say ""
  say "FAILED: SPEC.md 18.1 requires the run to reach DONE; it reached $final."
  diagnose
  exit 1
fi

say ""
say '$ curl -sS localhost:8080/runs/$RUN            # final state'
curl -sS "$ORCH/runs/$RUN" | jq . 2>/dev/null || true
say ""
say '$ curl -sS localhost:8080/runs/$RUN/steps      # final state'
curl -sS "$ORCH/runs/$RUN/steps" | jq . 2>/dev/null || true

# ---------------------------------------------------------------------------
# 4. What the operator must SEE - SPEC.md 18.1's five queries
# ---------------------------------------------------------------------------

hr "4. DATABASE TRUTH (SPEC.md 18.1, for the operator's eye)"

show "SELECT status, replay_count, planner_attempt_count, owner_id
        FROM runs WHERE run_id = :'run';
-- expected: DONE;  replay_count = 0;  planner_attempt_count = 0;  owner_id NULL
--           (owner_id: SPEC.md 6.2's invariant - non-NULL only while RUNNING.
--            18.1 selects the column but states no expectation; section 6
--            asserts it from 6.2.)"

show "SELECT seq, step_name, status, attempt_count, octet_length(output::text)
        FROM steps WHERE run_id = :'run' ORDER BY seq;
-- expected: one row per static step, contiguous seq from 1, every row DONE,
--           attempt_count = 1, output present."

show "SELECT attempt_no, status, connection_mode, failure_reason, finished_at
        FROM attempts WHERE run_id = :'run' ORDER BY attempt_no;
-- expected: one row per step, all DONE, connection_mode = 'sync',
--           failure_reason NULL, finished_at set."

show "SELECT count(*) FROM dead_letter_queue WHERE run_id = :'run';
-- expected: 0"

show "SELECT orchestrator_id, last_seen_at FROM orchestrators;
-- expected: exactly one row, recently seen."

hr "4b. THE PIPELINE, MADE VISIBLE (not part of SPEC.md 18.1's list)"
say "Each step's echoed envelope, so the chaining of SPEC.md 9.4's input_from"
say "can be read off the database directly."
show "SELECT s.seq, s.step_name,
             (s.decision::jsonb) ->  'input_from'                AS decided_input_from,
             (s.output::jsonb)   -> 'echo' -> 'input_step_ids'   AS delivered_inputs,
             (s.output::jsonb)   -> 'echo' ->> 'connection_mode' AS envelope_mode,
             (s.output::jsonb)   -> 'echo' ->> 'has_callback_url' AS had_callback_url
        FROM steps s WHERE s.run_id = :'run' ORDER BY s.seq;"

# ---------------------------------------------------------------------------
# 5. Assertions REQUIRED by SPEC.md 18.1
# ---------------------------------------------------------------------------

hr "5. ASSERTIONS REQUIRED BY SPEC.md 18.1"

say "runs (SPEC.md 18.1, SPEC.md 6.2):"
check "run reached DONE" \
  "SELECT count(*) = 1 FROM runs WHERE run_id = :'run' AND status = 'DONE';"
check "replay_count = 0" \
  "SELECT count(*) = 1 FROM runs WHERE run_id = :'run' AND replay_count = 0;"
check "planner_attempt_count = 0 (SPEC.md 12.1: the static planner cannot fail)" \
  "SELECT count(*) = 1 FROM runs WHERE run_id = :'run' AND planner_attempt_count = 0;"

say ""
say "steps (SPEC.md 18.1, SPEC.md 6.3):"
check "one row per static step ($N)" \
  "SELECT count(*) = $N FROM steps WHERE run_id = :'run';"
check "seq is contiguous from 1 (SPEC.md 3.3)" \
  "SELECT count(*) = $N AND min(seq) = 1 AND max(seq) = $N
          AND count(DISTINCT seq) = $N FROM steps WHERE run_id = :'run';"
check "every step is DONE" \
  "SELECT count(*) FILTER (WHERE status = 'DONE') = $N
     FROM steps WHERE run_id = :'run';"
check "every step has attempt_count = 1" \
  "SELECT count(*) FILTER (WHERE attempt_count = 1) = $N
     FROM steps WHERE run_id = :'run';"
check "every step has an output present" \
  "SELECT count(*) FILTER (WHERE output IS NOT NULL
                             AND octet_length(output::text) > 0) = $N
     FROM steps WHERE run_id = :'run';"
check "every step has completed_at set (SPEC.md 6.3: set exactly when status leaves RUNNING)" \
  "SELECT count(*) FILTER (WHERE completed_at IS NOT NULL) = $N
     FROM steps WHERE run_id = :'run';"

say ""
say "attempts (SPEC.md 18.1, SPEC.md 6.4):"
check "one attempt per step, each numbered 1" \
  "SELECT (SELECT count(*) FROM attempts WHERE run_id = :'run') = $N
      AND (SELECT count(*) FILTER (WHERE attempt_no = 1)
             FROM attempts WHERE run_id = :'run') = $N;"
check "every attempt is DONE" \
  "SELECT count(*) FILTER (WHERE status = 'DONE') = $N
     FROM attempts WHERE run_id = :'run';"
check "every attempt has connection_mode = 'sync' (SPEC.md 9.7)" \
  "SELECT count(*) FILTER (WHERE connection_mode = 'sync') = $N
     FROM attempts WHERE run_id = :'run';"
check "every attempt has failure_reason NULL (SPEC.md 6.4 invariant 2)" \
  "SELECT count(*) FILTER (WHERE failure_reason IS NULL) = $N
     FROM attempts WHERE run_id = :'run';"
check "every attempt has finished_at set (SPEC.md 6.4 invariant 3)" \
  "SELECT count(*) FILTER (WHERE finished_at IS NOT NULL) = $N
     FROM attempts WHERE run_id = :'run';"

say ""
say "dead_letter_queue (SPEC.md 18.1):"
check "no dead-letter entry for this run" \
  "SELECT count(*) = 0 FROM dead_letter_queue WHERE run_id = :'run';"

say ""
say "orchestrators (SPEC.md 18.1, SPEC.md 6.6, SPEC.md 8.7):"
check "exactly one orchestrator row" \
  "SELECT count(*) = 1 FROM orchestrators;"
check "that orchestrator was seen within lease_ttl (${LEASE_TTL}s, from piton.yaml)" \
  "SELECT count(*) = 1 FROM orchestrators
     WHERE last_seen_at > now() - interval '$LEASE_TTL seconds';"

# ---------------------------------------------------------------------------
# 6. Additional assertions, derived from SPEC but beyond 18.1's list
# ---------------------------------------------------------------------------

hr "6. ADDITIONAL ASSERTIONS DERIVED FROM SPEC (beyond 18.1's list)"

say "Kept separate on purpose. SPEC.md 18.1's list above is what the milestone"
say "requires; these come from other ratified sections and cost nothing extra,"
say "because the echo worker already reports what its envelope contained. If"
say "the owner judges them out of scope, this whole section can be deleted"
say "without touching section 5."
say ""

say "state model (SPEC.md 5.4, 5.5):"
check "the run ended in combination L3 - run DONE, derived last_step DONE" \
  "SELECT (SELECT status FROM runs WHERE run_id = :'run') = 'DONE'
      AND (SELECT status FROM steps WHERE run_id = :'run'
            ORDER BY seq DESC LIMIT 1) = 'DONE';"

say ""
say "coordination metadata (SPEC.md 6.2's invariant, SPEC.md 8.7's fourth writer):"
# SPEC.md 6.2: "owner_id and claimed_at are non-NULL only while status =
# 'RUNNING', and are always written and cleared as a pair". The run is DONE, so
# both must be NULL. SPEC.md 8.7 names the mechanism that produces it: any
# transition of a run out of RUNNING clears the pair in the same transaction as
# the status change.
check "a DONE run holds no owner_id (SPEC.md 6.2)" \
  "SELECT owner_id IS NULL FROM runs WHERE run_id = :'run';"
check "a DONE run holds no claimed_at either - the pair is cleared together (SPEC.md 8.7)" \
  "SELECT claimed_at IS NULL FROM runs WHERE run_id = :'run';"

say ""
say "the run's input (SPEC.md 6.2: stored verbatim):"
check "runs.input is exactly what was submitted" \
  "SELECT (input::jsonb) = (:'expected')::jsonb FROM runs WHERE run_id = :'run';" \
  -v expected="$RUN_INPUT"

say ""
say "the planner's decisions (SPEC.md 6.1, 6.3: the StepSpec exactly as returned):"
say "  compared as JSON values, since SPEC.md 7.1 leaves the byte encoding to"
say "  the backend and the Postgres one normalises jsonb."
i=0
while [ "$i" -lt "$N" ]; do
  seq_no=$((i + 1))
  expected_spec=$(jq -c ".planner_static_steps[$i]" workflow.json)
  check "steps.decision at seq $seq_no equals planner_static_steps[$i]" \
    "SELECT (decision::jsonb) = (:'spec')::jsonb
       FROM steps WHERE run_id = :'run' AND seq = $seq_no;" \
    -v spec="$expected_spec"
  i=$((i + 1))
done

say ""
say "the dispatch envelope (SPEC.md 9.5):"
check "every envelope carried connection_mode = 'sync'" \
  "SELECT count(*) FILTER (
            WHERE (output::jsonb) -> 'echo' ->> 'connection_mode' = 'sync') = $N
     FROM steps WHERE run_id = :'run';"
check "no envelope carried callback_url - omitted entirely in sync mode" \
  "SELECT count(*) FILTER (
            WHERE (output::jsonb) -> 'echo' ->> 'has_callback_url' = 'false') = $N
     FROM steps WHERE run_id = :'run';"
check "every envelope carried all six required fields and no unknown field" \
  "SELECT count(*) FILTER (
            WHERE jsonb_array_length(
                    (output::jsonb) -> 'echo' -> 'missing_envelope_fields') = 0
              AND jsonb_array_length(
                    (output::jsonb) -> 'echo' -> 'unknown_envelope_fields') = 0) = $N
     FROM steps WHERE run_id = :'run';"
check "every envelope's run_id and step_id matched the row it belonged to" \
  "SELECT count(*) FILTER (
            WHERE (output::jsonb) -> 'echo' ->> 'run_id'  = run_id::text
              AND (output::jsonb) -> 'echo' ->> 'step_id' = step_id::text) = $N
     FROM steps WHERE run_id = :'run';"
check "every envelope's params matched the StepSpec's params" \
  "SELECT count(*) FILTER (
            WHERE (output::jsonb) -> 'echo' -> 'params'
                = coalesce((decision::jsonb) -> 'params', '{}'::jsonb)) = $N
     FROM steps WHERE run_id = :'run';"

say ""
say "input_from resolution (SPEC.md 9.4, 9.5):"
check "seq 1 declared input_from [] and received nothing" \
  "SELECT ((output::jsonb) -> 'echo' ->> 'input_count')::int = 0
     FROM steps WHERE run_id = :'run' AND seq = 1;"
check "seq 2..$N omitted input_from and received exactly the previous step" \
  "SELECT count(*) = $((N - 1))
     FROM steps s
     JOIN steps p ON p.run_id = s.run_id AND p.seq = s.seq - 1
    WHERE s.run_id = :'run' AND s.seq > 1
      AND ((s.output::jsonb) -> 'echo' ->> 'input_count')::int = 1
      AND (s.output::jsonb) -> 'echo' -> 'input_step_ids'
        = jsonb_build_array(p.step_id::text);"

say ""
say "who dispatched (SPEC.md 6.4):"
check "every attempt was dispatched by the single live orchestrator" \
  "SELECT count(*) FILTER (
            WHERE dispatched_by = (SELECT orchestrator_id FROM orchestrators)) = $N
     FROM attempts WHERE run_id = :'run';"

say ""
say "what alpha deliberately does NOT demonstrate (SPEC.md 18.1), asserted as absence:"
check "no step went to DLQ" \
  "SELECT count(*) = 0 FROM steps WHERE run_id = :'run' AND status = 'DLQ';"
check "no attempt failed - no retry was needed" \
  "SELECT count(*) = 0 FROM attempts WHERE run_id = :'run' AND status = 'FAILED';"
check "no async attempt exists (async is milestone epsilon)" \
  "SELECT count(*) = 0 FROM attempts WHERE run_id = :'run' AND connection_mode = 'async';"

# ---------------------------------------------------------------------------
# 7. Verdict
# ---------------------------------------------------------------------------

hr "7. RESULT"

say "workflow_id : $WF"
say "run_id      : $RUN"
say "passed      : $PASSED"
say "failed      : $FAILED"
say ""

if [ "$FAILED" -eq 0 ]; then
  say "Milestone alpha: every assertion held."
  say ""
  say "CLAUDE.md 4 step 5 is still outstanding and is not replaceable by this"
  say "script: the owner runs the demo by hand and inspects database truth from"
  say "a terminal. A green script he has never looked behind is not evidence"
  say "the milestone landed."
  exit 0
fi

say "Milestone alpha: $FAILED assertion(s) failed."
say ""
say "CLAUDE.md 5.2: SPEC wins. Do not adjust an assertion to match the code."
say "If you believe SPEC.md is wrong, say so and stop - the owner rules, SPEC"
say "changes first, and the assertion changes second."
diagnose
exit 1
