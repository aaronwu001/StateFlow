#!/usr/bin/env bash
# StateFlow Interactive Demo — LLM Planner mode
# Proves three reliability claims across three scenarios.
set -euo pipefail

# ── Paths ────────────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SF_BINARY="$SCRIPT_DIR/stateflow"
LLM_ADAPTER="$SCRIPT_DIR/planner/llm_adapter.py"
WORKER_SCRIPT="$SCRIPT_DIR/workers/worker.py"
MIGRATION_SQL="$PROJECT_ROOT/migrations/001_initial.sql"

# ── Config ────────────────────────────────────────────────────────────────────

SF_URL="http://localhost:8080"
DB_CONTAINER="stateflow-pg-test"
DB_NAME="stateflow_demo"
DB_USER="postgres"
DATABASE_URL="postgresql://postgres:postgres@localhost:5432/${DB_NAME}?sslmode=disable"

# PID tracking
PID_DIR="/tmp/stateflow_demo_$$"
SF_PID=""
LLM_PID=""

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

cleanup() {
    echo
    info "Cleaning up..."
    [[ -n "$SF_PID" ]]  && { kill "$SF_PID"  2>/dev/null || true; wait "$SF_PID"  2>/dev/null || true; SF_PID=""; }
    [[ -n "$LLM_PID" ]] && { kill "$LLM_PID" 2>/dev/null || true; wait "$LLM_PID" 2>/dev/null || true; LLM_PID=""; }
    if [[ -d "$PID_DIR" ]]; then
        for pid_file in "$PID_DIR"/worker_*.pid; do
            [[ -f "$pid_file" ]] || continue
            local pid
            pid=$(cat "$pid_file" 2>/dev/null) || continue
            kill "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
        done
        rm -rf "$PID_DIR"
    fi
}
trap cleanup EXIT INT TERM

# ── Build ─────────────────────────────────────────────────────────────────────

build() {
    info "Building stateflow binary..."
    (cd "$PROJECT_ROOT" && go build -o "$SF_BINARY" ./cmd/stateflow/) \
        && success "Binary built → $SF_BINARY" \
        || { error "Build failed"; exit 1; }
}

# ── Database ──────────────────────────────────────────────────────────────────

setup_db() {
    info "Creating fresh '$DB_NAME' database..."
    docker exec "$DB_CONTAINER" psql -U "$DB_USER" -c "DROP DATABASE IF EXISTS ${DB_NAME};" postgres >/dev/null 2>&1 || true
    docker exec "$DB_CONTAINER" psql -U "$DB_USER" -c "CREATE DATABASE ${DB_NAME};" postgres >/dev/null
    docker exec -i "$DB_CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" < "$MIGRATION_SQL" >/dev/null
    success "Database '$DB_NAME' ready"
}

reset_db() {
    docker exec "$DB_CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" \
        -c "TRUNCATE workflows CASCADE;" >/dev/null 2>&1 || true
    success "Database reset"
}

pg_query() {
    local sql=$1
    docker exec "$DB_CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -c "$sql"
}

# ── Workers ───────────────────────────────────────────────────────────────────

mkdir -p "$PID_DIR"

start_worker() {
    local name=$1 port=$2 delay=${3:-1}
    # Truncate log so invocation counts start fresh each scenario.
    > "/tmp/worker_${name}.log"

    WORKER_NAME="$name" WORKER_PORT="$port" WORKER_DELAY="$delay" \
        python3 "$WORKER_SCRIPT" >> "/tmp/worker_${name}.log" 2>&1 &
    local pid=$!
    echo "$pid" > "$PID_DIR/worker_${name}.pid"

    # Wait for TCP port to open.
    local deadline=$(($(date +%s) + 15))
    while [[ $(date +%s) -lt $deadline ]]; do
        if (echo > /dev/tcp/localhost/$port) 2>/dev/null; then break; fi
        sleep 0.3
    done
    success "Worker '${name}' ready on :${port}  delay=${delay}s  pid=${pid}"
}

stop_worker() {
    local name=$1
    local pid_file="$PID_DIR/worker_${name}.pid"
    [[ -f "$pid_file" ]] || return 0
    local pid
    pid=$(cat "$pid_file")
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    rm -f "$pid_file"
    info "Worker '${name}' stopped"
}

count_worker_calls() {
    local name=$1
    local log_file="/tmp/worker_${name}.log"
    [[ -f "$log_file" ]] || { echo "0"; return; }
    grep -c "\[WORKER:${name}\] received step" "$log_file" 2>/dev/null || echo "0"
}

# ── Orchestrator ──────────────────────────────────────────────────────────────

start_orchestrator() {
    DATABASE_URL="$DATABASE_URL" LISTEN_ADDR=":8080" \
        "$SF_BINARY" >> /tmp/stateflow.log 2>&1 &
    SF_PID=$!
    echo "$SF_PID" > "$PID_DIR/stateflow.pid"

    # Wait for HTTP server to accept connections (404 = server up, route not found).
    local deadline=$(($(date +%s) + 30))
    while [[ $(date +%s) -lt $deadline ]]; do
        local code
        code=$(curl -s -o /dev/null -w "%{http_code}" "$SF_URL/runs/__probe__" 2>/dev/null) || true
        if [[ "$code" == "404" || "$code" == "200" ]]; then break; fi
        sleep 0.3
    done
    success "StateFlow ready on :8080  pid=${SF_PID}"
}

stop_orchestrator() {
    [[ -z "$SF_PID" ]] && return 0
    kill "$SF_PID" 2>/dev/null || true
    wait "$SF_PID" 2>/dev/null || true
    SF_PID=""
    rm -f "$PID_DIR/stateflow.pid"
    info "Orchestrator stopped"
}

kill_orchestrator() {
    [[ -z "$SF_PID" ]] && return 0
    warn "KILLING orchestrator with SIGKILL — pid=${SF_PID}"
    kill -9 "$SF_PID" 2>/dev/null || true
    wait "$SF_PID" 2>/dev/null || true
    SF_PID=""
    rm -f "$PID_DIR/stateflow.pid"
}

# ── LLM Adapter ───────────────────────────────────────────────────────────────

start_llm_adapter() {
    > /tmp/llm_adapter.log
    python3 "$LLM_ADAPTER" >> /tmp/llm_adapter.log 2>&1 &
    LLM_PID=$!
    echo "$LLM_PID" > "$PID_DIR/llm_adapter.pid"

    local deadline=$(($(date +%s) + 15))
    while [[ $(date +%s) -lt $deadline ]]; do
        if (echo > /dev/tcp/localhost/9000) 2>/dev/null; then break; fi
        sleep 0.3
    done
    success "LLM adapter ready on :9000  pid=${LLM_PID}"
}

stop_llm_adapter() {
    [[ -z "$LLM_PID" ]] && return 0
    kill "$LLM_PID" 2>/dev/null || true
    wait "$LLM_PID" 2>/dev/null || true
    LLM_PID=""
    rm -f "$PID_DIR/llm_adapter.pid"
    info "LLM adapter stopped"
}

count_adapter_calls() {
    grep -c "\[ADAPTER\] Planner called" /tmp/llm_adapter.log 2>/dev/null || echo "0"
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
        if [[ "$status" == "DONE" || "$status" == "FAILED" ]]; then
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
    icon = '✓' if s['status'] == 'DONE' else ('✗' if s['status'] in ('FAILED','DLQ') else '●')
    c = '\033[0;32m' if s['status'] == 'DONE' else ('\033[0;31m' if s['status'] in ('FAILED','DLQ') else '\033[1;33m')
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

    start_worker step1 5010 1
    start_worker step2 5011 1
    start_llm_adapter
    > /tmp/stateflow.log
    start_orchestrator

    pause "Workers + LLM adapter up. Submitting workflow."

    local wf_id run_id
    wf_id=$(create_http_workflow "llm-happy-path" "http://localhost:9000/decide")
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

    # Only step1 worker is started; step2 (port 5011) is intentionally absent.
    start_worker step1 5010 1
    start_llm_adapter
    > /tmp/stateflow.log
    start_orchestrator

    warn "Worker for step2 (port 5011) is DOWN — step2 will fail 3 times then enter DLQ"
    pause "Submitting workflow. step2 retries 3×(~5s each) ≈ 15s before DLQ."

    local wf_id run_id
    wf_id=$(create_http_workflow "llm-worker-crash" "http://localhost:9000/decide")
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

    local step1_calls
    step1_calls=$(count_worker_calls step1)
    info "step1 invocation count before replay: ${step1_calls}"

    pause "Starting step2 worker now, then replaying DLQ entry ${dlq_id}."

    start_worker step2 5011 1

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

    start_worker step1 5010 5   # 5s delay gives crash window
    start_worker step2 5011 1
    start_llm_adapter
    > /tmp/stateflow.log
    start_orchestrator

    pause "Workers (step1=5s delay) + LLM adapter up. Submitting workflow."

    local wf_id run_id
    wf_id=$(create_http_workflow "llm-crash-demo" "http://localhost:9000/decide")
    run_id=$(start_run "$wf_id" '{"task":"crash recovery test"}')
    info "run_id = $run_id"

    # Wait for planner to decide step1 and dispatch it (adapter calls ≥ 1).
    info "Waiting for step1 to be dispatched (~1s after submit)..."
    sleep 3

    warn "Killing orchestrator while step1 is running (5s worker, still processing)."
    kill_orchestrator
    warn "Orchestrator dead. step1 is RUNNING in DB (Barrier 1 fired; Barrier 2 not yet)."

    pause "Inspect Postgres state, then restart orchestrator."

    echo -e "${BOLD}   Raw Postgres state:${NC}"
    pg_query "SELECT step_name, status, (current_attempt_id IS NOT NULL) AS dispatched FROM steps WHERE run_id = '$run_id' ORDER BY seq;"

    pause "Restarting orchestrator. Recovery must re-dispatch step1 WITHOUT re-calling planner."

    > /tmp/stateflow.log
    start_orchestrator

    sleep 1
    echo -e "${BOLD}   Recovery log:${NC}"
    grep -i recovery /tmp/stateflow.log 2>/dev/null | head -10 | sed 's/^/   /' || true
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
    if ! docker ps --format '{{.Names}}' 2>/dev/null | grep -q "^${DB_CONTAINER}$"; then
        error "Docker container '${DB_CONTAINER}' is not running."
        info  "Start it with:"
        info  "  docker run -d --name ${DB_CONTAINER} -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=stateflow_test -p 5432:5432 postgres:16-alpine"
        exit 1
    fi

    if [[ ! -f "$WORKER_SCRIPT" ]]; then
        error "Worker script not found at $WORKER_SCRIPT"
        exit 1
    fi

    if [[ ! -f "$LLM_ADAPTER" ]]; then
        error "LLM adapter not found at $LLM_ADAPTER"
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
