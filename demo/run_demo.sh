#!/usr/bin/env bash
# StateFlow Interactive Demo — LLM Planner mode
# Proves three reliability claims across three scenarios.
#
# Everything (Postgres, StateFlow, workers, the LLM planner adapter) runs as
# docker compose services — the base stack (docker-compose.yml) plus the demo
# overlay (docker-compose.demo.yml). This script drives that stack from the
# host: curl hits localhost on the compose-published ports, and orchestrator
# restarts are compose service lifecycle commands, not local processes.
set -euo pipefail

# ── Paths ────────────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

COMPOSE_ARGS=(-f "$PROJECT_ROOT/docker-compose.yml" -f "$PROJECT_ROOT/docker-compose.demo.yml")
dc() { docker compose "${COMPOSE_ARGS[@]}" "$@"; }

# ── Config ────────────────────────────────────────────────────────────────────

SF_URL="http://localhost:8080"
DB_USER="stateflow"
DB_NAME="stateflow"

# ── Colors ────────────────────────────────────────────────────────────────────

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

# ── Output helpers ────────────────────────────────────────────────────────────

info()    { echo -e "${CYAN}   ℹ  $*${NC}"; }
success() { echo -e "${GREEN}   ✓  $*${NC}"; }
warn()    { echo -e "${YELLOW}   ⚠  $*${NC}"; }
error()   { echo -e "${RED}   ✗  $*${NC}"; }
pause()   {
    echo
    local msg="${1:-press Enter to continue}"
    read -rp "$(echo -e "${BOLD}   ▶  ${msg} ...${NC}")" _ || true
    echo
}
header() {
    echo
    echo -e "${BOLD}${CYAN}══════════════════════════════════════════════════════${NC}"
    echo -e "${BOLD}${CYAN}  $*${NC}"
    echo -e "${BOLD}${CYAN}══════════════════════════════════════════════════════${NC}"
    echo
}

# ── Cleanup (trap) ────────────────────────────────────────────────────────────
# Stops the demo-managed services this script starts/restarts. Postgres is
# shared, persistent infrastructure and is left running.

cleanup() {
    echo
    info "Cleaning up..."
    dc stop stateflow step1 step2 llm-adapter >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# ── Build ─────────────────────────────────────────────────────────────────────

build() {
    info "Building images (stateflow + demo workers/adapter)..."
    dc build stateflow step1 step2 llm-adapter >/dev/null \
        && success "Images built" \
        || { error "Build failed"; exit 1; }
}

# ── Database ──────────────────────────────────────────────────────────────────

setup_db() {
    info "Ensuring Postgres is up and schema is applied..."
    dc up -d postgres >/dev/null

    local deadline=$(($(date +%s) + 30))
    while [[ $(date +%s) -lt $deadline ]]; do
        dc exec -T postgres pg_isready -U "$DB_USER" -d "$DB_NAME" >/dev/null 2>&1 && break
        sleep 0.5
    done
    success "Postgres ready"
}

reset_db() {
    dc exec -T postgres psql -U "$DB_USER" -d "$DB_NAME" \
        -c "TRUNCATE workflows CASCADE;" >/dev/null 2>&1 || true
    success "Database reset"
}

pg_query() {
    local sql=$1
    dc exec -T postgres psql -U "$DB_USER" -d "$DB_NAME" -c "$sql"
}

# ── Workers ───────────────────────────────────────────────────────────────────
# step1/step2 are the generic worker.py service, configured per-scenario via
# STEP1_DELAY/STEP2_DELAY (read by docker-compose.demo.yml's environment:).

# wait_healthy <service> [<timeout_s>] — polls the service's own container
# healthcheck (docker-compose.demo.yml) rather than a host-side port probe.
# On Docker Desktop the host-published port can accept a bare TCP connect
# slightly before the app inside actually binds its listen socket, which
# would let a container-to-container caller (e.g. stateflow -> step1) race
# ahead of a truly-ready worker.
wait_healthy() {
    local service=$1 timeout=${2:-15}
    local cid status
    cid=$(dc ps -q "$service")
    local deadline=$(($(date +%s) + timeout))
    while [[ $(date +%s) -lt $deadline ]]; do
        status=$(docker inspect -f '{{.State.Health.Status}}' "$cid" 2>/dev/null || echo "")
        [[ "$status" == "healthy" ]] && return 0
        sleep 0.3
    done
    return 1
}

start_worker() {
    local name=$1 delay=${2:-1}
    local port
    case "$name" in
        step1) port=5010 ;;
        step2) port=5011 ;;
        *) error "unknown worker '$name'"; exit 1 ;;
    esac

    local delay_var="${name^^}_DELAY"
    env "${delay_var}=${delay}" docker compose "${COMPOSE_ARGS[@]}" up -d --force-recreate "$name" >/dev/null

    wait_healthy "$name" 15 || { error "Worker '${name}' did not become healthy in time"; exit 1; }
    success "Worker '${name}' ready on :${port}  delay=${delay}s"
}

stop_worker() {
    local name=$1
    dc stop "$name" >/dev/null 2>&1 || true
    info "Worker '${name}' stopped"
}

count_worker_calls() {
    local name=$1
    local n
    n=$(dc logs --no-log-prefix "$name" 2>/dev/null | grep -c "\[WORKER:${name}\] received step") || true
    echo "${n:-0}"
}

# ── Orchestrator ──────────────────────────────────────────────────────────────

start_orchestrator() {
    dc up -d stateflow >/dev/null

    local deadline=$(($(date +%s) + 30))
    while [[ $(date +%s) -lt $deadline ]]; do
        local code
        code=$(curl -s -o /dev/null -w "%{http_code}" "$SF_URL/runs/__probe__" 2>/dev/null) || true
        if [[ "$code" == "404" || "$code" == "200" ]]; then break; fi
        sleep 0.3
    done
    success "StateFlow ready on :8080"
}

stop_orchestrator() {
    dc stop stateflow >/dev/null 2>&1 || true
    info "Orchestrator stopped"
}

kill_orchestrator() {
    warn "KILLING orchestrator with 'docker compose kill' (SIGKILL)"
    dc kill stateflow >/dev/null 2>&1 || true
}

# ── LLM Adapter ───────────────────────────────────────────────────────────────

start_llm_adapter() {
    dc up -d --force-recreate llm-adapter >/dev/null

    wait_healthy llm-adapter 15 || { error "LLM adapter did not become healthy in time"; exit 1; }
    success "LLM adapter ready on :9000"
}

stop_llm_adapter() {
    dc stop llm-adapter >/dev/null 2>&1 || true
    info "LLM adapter stopped"
}

count_adapter_calls() {
    local n
    n=$(dc logs --no-log-prefix llm-adapter 2>/dev/null | grep -c "\[ADAPTER\] Planner called") || true
    echo "${n:-0}"
}

# ── StateFlow API helpers ──────────────────────────────────────────────────────

# create_http_workflow <name> <planner_url> → workflow_id
create_http_workflow() {
    local name=$1 url=$2
    curl -sf -X POST "$SF_URL/workflows" \
        -H "Content-Type: application/json" \
        -d "{\"name\":\"$name\",\"planner_type\":\"http\",\"planner_config\":{\"url\":\"$url\"}}" \
    | python3 -c "import json,sys; print(json.load(sys.stdin)['workflow_id'])"
}

# start_run <workflow_id> [<workflow_input_json>] → run_id
start_run() {
    local wf_id=$1 input=${2:-'{}'}
    curl -sf -X POST "$SF_URL/workflows/$wf_id/runs" \
        -H "Content-Type: application/json" \
        -d "{\"workflow_input\":$input}" \
    | python3 -c "import json,sys; print(json.load(sys.stdin)['run_id'])"
}

# poll_status <run_id> [<timeout_s>] → prints final JSON, returns 0 on terminal state
poll_status() {
    local run_id=$1 timeout=${2:-120}
    local deadline=$(($(date +%s) + timeout))
    while [[ $(date +%s) -lt $deadline ]]; do
        local resp status
        resp=$(curl -sf "$SF_URL/runs/$run_id" 2>/dev/null) || { sleep 1; continue; }
        status=$(echo "$resp" | python3 -c "import json,sys; print(json.load(sys.stdin)['status'])" 2>/dev/null) || { sleep 1; continue; }
        if [[ "$status" == "DONE" || "$status" == "DLQ" ]]; then
            echo "$resp"
            return 0
        fi
        sleep 1
    done
    error "Timed out (${timeout}s) waiting for run ${run_id}"
    return 1
}

# wait_for_step_status <run_id> <step_name> <status> [<timeout_s>] → 0 when matched
wait_for_step_status() {
    local run_id=$1 step=$2 want_status=$3 timeout=${4:-60}
    local deadline=$(($(date +%s) + timeout))
    while [[ $(date +%s) -lt $deadline ]]; do
        local st
        st=$(curl -sf "$SF_URL/runs/$run_id" 2>/dev/null | python3 -c "
import json,sys
d=json.load(sys.stdin)
for s in d.get('steps',[]):
    if s['step_name']=='$step': print(s['status']); sys.exit(0)
print('PENDING')
" 2>/dev/null) || { sleep 1; continue; }
        [[ "$st" == "$want_status" ]] && return 0
        sleep 1
    done
    return 1
}

show_status() {
    local run_id=$1
    local resp
    resp=$(curl -sf "$SF_URL/runs/$run_id" 2>/dev/null) || { warn "Could not fetch status for $run_id"; return; }
    echo
    echo -e "${BOLD}   Run: ${run_id}${NC}"
    echo "$resp" | python3 -c "
import json, sys
d = json.load(sys.stdin)
status = d['status']
color = '\033[0;32m' if status == 'DONE' else '\033[0;31m'
nc = '\033[0m'
bold = '\033[1m'
print(f'   Status: {color}{bold}{status}{nc}')
for s in d.get('steps', []):
    icon = '✓' if s['status'] == 'DONE' else ('✗' if s['status'] == 'DLQ' else '●')
    c = '\033[0;32m' if s['status'] == 'DONE' else ('\033[0;31m' if s['status'] == 'DLQ' else '\033[1;33m')
    print(f'   {c}[{s[\"status\"]:8}] {icon} {s[\"step_name\"]}{nc}')
"
    echo
}

show_dlq() {
    local resp
    resp=$(curl -sf "$SF_URL/dlq" 2>/dev/null) || { warn "Could not fetch DLQ"; return; }
    echo
    echo -e "${BOLD}   DLQ Entries:${NC}"
    echo "$resp" | python3 -c "
import json, sys
d = json.load(sys.stdin)
entries = d.get('entries', [])
if not entries:
    print('   (empty)')
else:
    for e in entries:
        step = e.get('step_id') or 'N/A'
        print(f'   ID={e[\"id\"]}  run_id={e[\"run_id\"]}  reason={e[\"reason\"]}  step={step}')
"
    echo
}

get_dlq_entry_id() {
    curl -sf "$SF_URL/dlq" 2>/dev/null | \
        python3 -c "import json,sys; d=json.load(sys.stdin); e=d.get('entries',[]); print(e[0]['id'] if e else '')"
}

# check_dlq_worker_retry_exhausted <run_id> — asserts the DLQ entry for
# run_id has reason=worker_retry_exhausted (whitepaper §7.1/§11) and that its
# context carries the per-attempt failure reasons (worker_reported / timeout /
# malformed / orphaned) that led to exhausting the retry budget. Exits 1 on
# any check failure.
check_dlq_worker_retry_exhausted() {
    local run_id=$1
    local resp
    resp=$(curl -sf "$SF_URL/dlq" 2>/dev/null) || { error "FAIL — could not fetch DLQ"; return 1; }
    echo "$resp" | python3 -c "
import json, sys
run_id = '$run_id'
d = json.load(sys.stdin)
entries = [e for e in d.get('entries', []) if e.get('run_id') == run_id]
if not entries:
    print('FAIL — no DLQ entry found for run_id=' + run_id)
    sys.exit(1)
e = entries[0]
if e.get('reason') != 'worker_retry_exhausted':
    print(f\"FAIL — DLQ reason = {e.get('reason')!r}, want 'worker_retry_exhausted'\")
    sys.exit(1)
ctx = e.get('context')
if isinstance(ctx, str):
    ctx = json.loads(ctx) if ctx else {}
ctx_str = json.dumps(ctx)
known_reasons = ('worker_reported', 'timeout', 'malformed', 'orphaned')
if not any(r in ctx_str for r in known_reasons):
    print(f'FAIL — DLQ context has no per-attempt failure reason (context={ctx_str})')
    sys.exit(1)
found = [r for r in known_reasons if r in ctx_str]
print(f'OK — DLQ reason=worker_retry_exhausted, context carries per-attempt reason(s): {found}')
" || return 1
    return 0
}

replay_dlq() {
    local entry_id=$1
    curl -sf -X POST "$SF_URL/dlq/$entry_id/replay" \
        -H "Content-Type: application/json" -d '{}' \
    | python3 -c "import json,sys; print(json.load(sys.stdin).get('run_id',''))"
}

# ── Scenario 1: Happy Path (LLM Planner) ─────────────────────────────────────

scenario_happy_path_llm() {
    header "Scenario 1: Happy Path (LLM / HTTP Planner)"
    info "Claim: StateFlow can be driven by any HTTP endpoint as the planner."
    info "Using llm_adapter.py in DUMMY mode (no API key needed) — 2-step pipeline."
    info "  step1 → :5010   step2 → :5011"
    echo

    setup_db
    reset_db

    start_worker step1 1
    start_worker step2 1
    start_llm_adapter
    start_orchestrator

    pause "Workers + LLM adapter up. Submitting workflow."

    local wf_id run_id
    wf_id=$(create_http_workflow "llm-happy-path" "http://llm-adapter:9000/decide")
    run_id=$(start_run "$wf_id" '{"task":"process document"}')
    info "run_id = $run_id"

    info "Polling for completion..."
    local result
    result=$(poll_status "$run_id" 60)
    local status
    status=$(echo "$result" | python3 -c "import json,sys; print(json.load(sys.stdin)['status'])")

    show_status "$run_id"

    local adapter_calls
    adapter_calls=$(count_adapter_calls)
    info "LLM adapter was called ${adapter_calls} time(s)  (expected: 3)"
    info "  call 1: history=[]             → decides step1 → :5010"
    info "  call 2: history=[step1]        → decides step2 → :5011"
    info "  call 3: history=[step1,step2]  → done"

    if [[ "$status" == "DONE" ]]; then
        success "PASS — LLM-driven pipeline completed; adapter called ${adapter_calls}×"
    else
        error "FAIL — Expected DONE, got $status"
    fi

    stop_orchestrator
    stop_worker step1; stop_worker step2
    stop_llm_adapter
    reset_db
}

# ── Scenario 2: Worker Crash & DLQ Replay (LLM Planner) ──────────────────────

scenario_worker_crash_llm() {
    header "Scenario 2: Worker Crash & DLQ Replay (LLM Planner)"
    info "Claim: A failed step lands in DLQ. Replay resumes — step1 not re-run."
    info "  step1 → :5010 (UP)   step2 → :5011 (intentionally DOWN)"
    echo

    setup_db
    reset_db

    # Only step1 worker is started; step2 (port 5011) is intentionally absent.
    start_worker step1 1
    stop_worker step2
    start_llm_adapter
    start_orchestrator

    warn "Worker for step2 (port 5011) is DOWN — step2 will fail 3 times then enter DLQ"
    pause "Submitting workflow. step2 retries 3×(~5s each) ≈ 15s before DLQ."

    local wf_id run_id
    wf_id=$(create_http_workflow "llm-worker-crash" "http://llm-adapter:9000/decide")
    run_id=$(start_run "$wf_id" '{"task":"worker crash test"}')
    info "run_id = $run_id"

    info "Waiting for run to reach terminal state (step2 → DLQ)..."
    local result
    result=$(poll_status "$run_id" 90) || result="{}"

    show_status "$run_id"
    show_dlq

    local dlq_id
    dlq_id=$(get_dlq_entry_id)
    if [[ -z "$dlq_id" ]]; then
        error "FAIL — No DLQ entry found"
        stop_orchestrator; stop_worker step1; stop_llm_adapter; reset_db
        return 1
    fi
    success "DLQ entry found: id=${dlq_id}"

    info "Checking DLQ reason and per-attempt context..."
    local dlq_check_output dlq_check_status
    dlq_check_output=$(check_dlq_worker_retry_exhausted "$run_id"); dlq_check_status=$?
    if [[ $dlq_check_status -ne 0 ]]; then
        error "$dlq_check_output"
        stop_orchestrator; stop_worker step1; stop_llm_adapter; reset_db
        return 1
    fi
    success "$dlq_check_output"

    local step1_calls
    step1_calls=$(count_worker_calls step1)
    info "step1 invocation count before replay: ${step1_calls}"

    pause "Starting step2 worker now, then replaying DLQ entry ${dlq_id}."

    start_worker step2 1

    info "Replaying DLQ entry ${dlq_id}..."
    replay_dlq "$dlq_id" >/dev/null
    info "Replay submitted — polling for completion..."

    result=$(poll_status "$run_id" 60)
    local status
    status=$(echo "$result" | python3 -c "import json,sys; print(json.load(sys.stdin)['status'])")

    show_status "$run_id"

    step1_calls=$(count_worker_calls step1)
    info "step1 final invocation count: ${step1_calls}  (must be 1 — not re-run after replay)"

    if [[ "$status" == "DONE" ]] && [[ "$step1_calls" -eq 1 ]]; then
        success "PASS — Run completed after DLQ replay; step1 not re-run (called ${step1_calls}×)"
    else
        error "FAIL — status=${status}, step1_calls=${step1_calls} (want DONE + 1)"
    fi

    stop_orchestrator
    stop_worker step1; stop_worker step2
    stop_llm_adapter
    reset_db
}

# ── Scenario 3: Orchestrator Crash & Recovery (LLM Planner) ──────────────────

scenario_orchestrator_crash_llm() {
    header "Scenario 3: Orchestrator Crash & Recovery (LLM Planner)"
    info "Claim: After crash recovery, the planner is NOT re-called for already-decided steps."
    info "Kill point: while step1 is dispatched (5s worker). step1 is RUNNING in DB."
    info "Proof: total planner calls ≤ 3 (same as no-crash; no extra call on re-dispatch)."
    echo

    setup_db
    reset_db

    start_worker step1 5   # 5s delay gives crash window
    start_worker step2 1
    start_llm_adapter
    start_orchestrator

    pause "Workers (step1=5s delay) + LLM adapter up. Submitting workflow."

    local wf_id run_id
    wf_id=$(create_http_workflow "llm-crash-demo" "http://llm-adapter:9000/decide")
    run_id=$(start_run "$wf_id" '{"task":"crash recovery test"}')
    info "run_id = $run_id"

    # Wait for planner to decide step1 and dispatch it (adapter calls ≥ 1).
    info "Waiting for step1 to be dispatched (~1s after submit)..."
    sleep 3

    kill_orchestrator
    warn "Orchestrator dead. step1 is RUNNING in DB (Barrier 1 fired; Barrier 2 not yet)."

    pause "Inspect Postgres state, then restart orchestrator."

    echo -e "${BOLD}   Raw Postgres state:${NC}"
    pg_query "SELECT step_name, status, (current_attempt_id IS NOT NULL) AS dispatched FROM steps WHERE run_id = '$run_id' ORDER BY seq;"

    pause "Restarting orchestrator. Recovery must re-dispatch step1 WITHOUT re-calling planner."

    local restart_marker
    restart_marker="$(date -u +%Y-%m-%dT%H:%M:%S)"
    start_orchestrator

    sleep 1
    echo -e "${BOLD}   Recovery log:${NC}"
    dc logs --no-log-prefix --since "$restart_marker" stateflow 2>/dev/null | grep -i recovery | head -10 | sed 's/^/   /' || true
    echo

    info "Polling for completion..."
    local result
    result=$(poll_status "$run_id" 90)
    local status
    status=$(echo "$result" | python3 -c "import json,sys; print(json.load(sys.stdin)['status'])")

    show_status "$run_id"

    local adapter_calls
    adapter_calls=$(count_adapter_calls)
    info "LLM adapter call count: ${adapter_calls}"
    info "Expected ≤ 3 (step1 decide, step2 decide, done check)."
    info "If > 3: step1 was re-decided after recovery — Barrier 1 violation."

    if [[ "$status" == "DONE" ]] && [[ "$adapter_calls" -le 3 ]]; then
        success "PASS — Recovery complete; adapter called ${adapter_calls}× (≤3 — no extra re-decision)"
    else
        error "FAIL — status=${status}, adapter_calls=${adapter_calls} (want DONE and ≤3)"
    fi

    stop_orchestrator
    stop_worker step1; stop_worker step2
    stop_llm_adapter
    reset_db
}

# ── Menu ──────────────────────────────────────────────────────────────────────

show_menu() {
    echo
    echo -e "${BOLD}${CYAN}   StateFlow Interactive Demo  (LLM Planner)${NC}"
    echo -e "${BOLD}${CYAN}   ══════════════════════════════════════════${NC}"
    echo -e "   ${BOLD}1)${NC} Happy Path"
    echo -e "   ${BOLD}2)${NC} Worker Crash & DLQ Replay"
    echo -e "   ${BOLD}3)${NC} Orchestrator Crash & Recovery"
    echo -e "   ${BOLD}A)${NC} Run All Scenarios"
    echo -e "   ${BOLD}Q)${NC} Quit"
    echo
}

# ── Main ──────────────────────────────────────────────────────────────────────

main() {
    # Verify prerequisites.
    if ! command -v docker >/dev/null 2>&1; then
        error "docker is not installed or not on PATH."
        exit 1
    fi
    if ! docker compose version >/dev/null 2>&1; then
        error "'docker compose' (v2 plugin) is not available."
        exit 1
    fi
    if [[ ! -f "$PROJECT_ROOT/docker-compose.demo.yml" ]]; then
        error "docker-compose.demo.yml not found at $PROJECT_ROOT/docker-compose.demo.yml"
        exit 1
    fi

    build

    while true; do
        show_menu
        read -rp "   Choice: " choice
        case "${choice,,}" in
            1) scenario_happy_path_llm ;;
            2) scenario_worker_crash_llm ;;
            3) scenario_orchestrator_crash_llm ;;
            a)
                scenario_happy_path_llm
                scenario_worker_crash_llm
                scenario_orchestrator_crash_llm
                echo
                success "All scenarios complete."
                ;;
            q) info "Goodbye."; exit 0 ;;
            *) warn "Unknown choice: $choice" ;;
        esac
    done
}

main "$@"
