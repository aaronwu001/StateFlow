#!/usr/bin/env bash
# StateFlow Interactive Demo
# Proves three reliability claims across five scenarios.
set -euo pipefail

# ── Paths ────────────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SF_BINARY="$SCRIPT_DIR/stateflow"
LLM_ADAPTER="$PROJECT_ROOT/llm_client_demo/llm_adapter.py"
WORKER_SCRIPT="$SCRIPT_DIR/worker.py"
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

# create_static_workflow <name> <planner_config_json> → workflow_id
create_static_workflow() {
    local name=$1 config=$2
    curl -sf -X POST "$SF_URL/workflows" \
        -H "Content-Type: application/json" \
        -d "{\"name\":\"$name\",\"planner_type\":\"static\",\"planner_config\":$config}" \
    | python3 -c "import json,sys; print(json.load(sys.stdin)['workflow_id'])"
}

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

# wait_for_step_done <run_id> <step_name> [<timeout_s>] → 0 when DONE
wait_for_step_done() {
    local run_id=$1 step=$2 timeout=${3:-60}
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
        [[ "$st" == "DONE" ]] && return 0
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

# ── Static planner config (inline JSON — same as demo/configs/static_3step.yaml) ──

STATIC_3STEP_CONFIG='{
  "steps": [
    {"name":"ocr",      "worker_url":"http://localhost:5010/run","mode":"sync","timeout_seconds":30},
    {"name":"ner",      "worker_url":"http://localhost:5011/run","mode":"sync","timeout_seconds":30},
    {"name":"summarize","worker_url":"http://localhost:5012/run","mode":"sync","timeout_seconds":30}
  ]
}'

# ── Scenario 1: Happy Path (Static Planner) ───────────────────────────────────

scenario_happy_path_static() {
    header "Scenario 1: Happy Path (Static Planner)"
    info "Claim: StateFlow drives a 3-step pipeline to completion."
    info "Steps: ocr (5010) → ner (5011) → summarize (5012)"
    echo

    setup_db

    start_worker ocr 5010 1
    start_worker ner 5011 1
    start_worker summarize 5012 1
    > /tmp/stateflow.log
    start_orchestrator

    pause "All workers and orchestrator are up. Submitting workflow."

    info "Creating workflow + starting run..."
    local wf_id run_id
    wf_id=$(create_static_workflow "happy-path-static" "$STATIC_3STEP_CONFIG")
    run_id=$(start_run "$wf_id" '{"doc":"report.pdf"}')
    info "run_id = $run_id"

    info "Polling for completion..."
    local result status
    result=$(poll_status "$run_id" 60)
    status=$(echo "$result" | python3 -c "import json,sys; print(json.load(sys.stdin)['status'])")

    show_status "$run_id"

    if [[ "$status" == "DONE" ]]; then
        success "PASS — All 3 steps completed, run status = DONE"
    else
        error "FAIL — Expected DONE, got $status"
    fi

    stop_orchestrator
    stop_worker ocr; stop_worker ner; stop_worker summarize
    reset_db
}

# ── Scenario 2: Worker Crash & DLQ Replay ─────────────────────────────────────

scenario_worker_crash() {
    header "Scenario 2: Worker Crash & DLQ Replay"
    info "Claim: A failed step lands in DLQ. Replay resumes the run — one API call."
    info "Proof: After replay, ocr is NOT re-run (called exactly once)."
    echo

    setup_db

    start_worker ocr 5010 1
    # ner (port 5011) is intentionally NOT started — connection refused → fail
    start_worker summarize 5012 1
    > /tmp/stateflow.log
    start_orchestrator

    warn "Worker 'ner' (port 5011) is DOWN — step 2 will fail 3 times then enter DLQ"
    pause "Submitting workflow now. ner will retry 3×(5s delay) ≈ 15s before DLQ."

    local wf_id run_id
    wf_id=$(create_static_workflow "worker-crash-demo" "$STATIC_3STEP_CONFIG")
    run_id=$(start_run "$wf_id" '{"doc":"report.pdf"}')
    info "run_id = $run_id"

    info "Waiting for run to reach terminal state (FAILED → DLQ)..."
    local result
    result=$(poll_status "$run_id" 90) || result="{}"

    show_status "$run_id"
    show_dlq

    local dlq_id
    dlq_id=$(get_dlq_entry_id)
    if [[ -z "$dlq_id" ]]; then
        error "FAIL — No DLQ entry found"
        stop_orchestrator; stop_worker ocr; stop_worker summarize; reset_db
        return 1
    fi
    success "DLQ entry found: id=${dlq_id}"

    pause "Starting 'ner' worker now, then replaying DLQ entry ${dlq_id}."

    start_worker ner 5011 1

    info "Replaying DLQ entry ${dlq_id}..."
    replay_dlq "$dlq_id" >/dev/null
    info "Replay submitted — polling for completion..."

    result=$(poll_status "$run_id" 60)
    local status
    status=$(echo "$result" | python3 -c "import json,sys; print(json.load(sys.stdin)['status'])")

    show_status "$run_id"

    local ocr_calls
    ocr_calls=$(count_worker_calls ocr)
    info "OCR worker invocation count: ${ocr_calls}"

    if [[ "$status" == "DONE" ]] && [[ "$ocr_calls" -eq 1 ]]; then
        success "PASS — Run completed after DLQ replay; OCR not re-run (called ${ocr_calls}×)"
    else
        error "FAIL — status=${status}, ocr_calls=${ocr_calls} (want DONE + 1)"
    fi

    stop_orchestrator
    stop_worker ocr; stop_worker ner; stop_worker summarize
    reset_db
}

# ── Scenario 3: Orchestrator Crash & Recovery ─────────────────────────────────

scenario_orchestrator_crash() {
    header "Scenario 3: Orchestrator Crash & Recovery"
    info "Claim: SIGKILL the orchestrator mid-run. Restart. Completed steps NOT re-run."
    info "Kill point: while 'ner' is dispatched (8s worker gives the window)."
    echo

    setup_db

    start_worker ocr 5010 1   # fast: completes before kill
    start_worker ner 5011 8   # slow: 8s gives crash window
    start_worker summarize 5012 1
    > /tmp/stateflow.log
    start_orchestrator

    pause "Workers up (ner is slow — 8s). Submitting workflow."

    local wf_id run_id
    wf_id=$(create_static_workflow "crash-recovery-demo" "$STATIC_3STEP_CONFIG")
    run_id=$(start_run "$wf_id" '{"doc":"report.pdf"}')
    info "run_id = $run_id"

    info "Waiting for step 'ocr' to reach DONE..."
    if wait_for_step_done "$run_id" "ocr" 30; then
        success "Step 'ocr' is DONE"
    else
        error "Timed out waiting for ocr"
        stop_orchestrator; stop_worker ocr; stop_worker ner; stop_worker summarize; reset_db
        return 1
    fi

    # Brief pause so ner dispatch is in-flight, then kill.
    sleep 1
    echo
    warn "OCR is DONE. NER is now running inside the orchestrator process."
    warn "Killing orchestrator with SIGKILL — NER's in-process channel dies with it."
    kill_orchestrator
    warn "Orchestrator dead. DB still shows ner=RUNNING (no output); summarize=never started."

    pause "Inspect Postgres state. Then we'll restart the orchestrator."

    echo -e "${BOLD}   Raw Postgres state:${NC}"
    pg_query "SELECT step_name, status, (current_attempt_id IS NOT NULL) AS dispatched FROM steps WHERE run_id = '$run_id' ORDER BY seq;"

    pause "Restarting orchestrator. Watch for [RECOVERY] log lines."

    > /tmp/stateflow.log   # fresh log so recovery messages are easy to spot
    start_orchestrator

    sleep 1
    echo -e "${BOLD}   Recovery log (from stateflow.log):${NC}"
    grep -i recovery /tmp/stateflow.log 2>/dev/null | head -10 | sed 's/^/   /' || true
    echo

    info "Polling for completion..."
    local result
    result=$(poll_status "$run_id" 60)
    local status
    status=$(echo "$result" | python3 -c "import json,sys; print(json.load(sys.stdin)['status'])")

    show_status "$run_id"

    local ocr_calls ner_calls
    ocr_calls=$(count_worker_calls ocr)
    ner_calls=$(count_worker_calls ner)
    info "Worker invocation counts: ocr=${ocr_calls}  ner=${ner_calls}"
    info "(ocr must be 1 — it completed before the crash and must not be re-run)"
    info "(ner will be 2 — once before crash [no checkpoint], once after recovery)"

    if [[ "$status" == "DONE" ]] && [[ "$ocr_calls" -eq 1 ]]; then
        success "PASS — Run completed after crash recovery; OCR not re-run (called ${ocr_calls}×)"
    else
        error "FAIL — status=${status}, ocr_calls=${ocr_calls} (want DONE + 1)"
    fi

    stop_orchestrator
    stop_worker ocr; stop_worker ner; stop_worker summarize
    reset_db
}

# ── Scenario 4: Happy Path (LLM Planner) ─────────────────────────────────────

scenario_happy_path_llm() {
    header "Scenario 4: Happy Path (LLM / HTTP Planner)"
    info "Claim: StateFlow can be driven by any HTTP endpoint as the planner."
    info "Using llm_adapter.py in DUMMY mode (no API key needed) — 2-step pipeline."
    echo

    setup_db

    # llm_adapter (dummy) dispatches both steps to port 5010.
    start_worker ocr 5010 1
    start_llm_adapter
    > /tmp/stateflow.log
    start_orchestrator

    pause "Echo worker + LLM adapter up. Submitting workflow."

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
    info "LLM adapter was called ${adapter_calls} time(s)"

    if [[ "$status" == "DONE" ]]; then
        success "PASS — LLM-driven pipeline completed (2 steps); adapter called ${adapter_calls}×"
    else
        error "FAIL — Expected DONE, got $status"
    fi

    stop_orchestrator
    stop_worker ocr
    stop_llm_adapter
    reset_db
}

# ── Scenario 5: Orchestrator Crash (LLM Planner) ─────────────────────────────

scenario_orchestrator_crash_llm() {
    header "Scenario 5: Orchestrator Crash (LLM Planner)"
    info "Claim: After crash recovery, the planner is NOT re-called for already-decided steps."
    info "Kill point: while step1 is dispatched (5s worker). Step1 is RUNNING in DB."
    info "Proof: total planner calls == 3 (same as no-crash; no extra call on re-dispatch)."
    echo

    setup_db

    # Both llm_adapter steps dispatch to port 5010.
    start_worker ocr 5010 5   # 5s delay gives crash window
    start_llm_adapter
    > /tmp/stateflow.log
    start_orchestrator

    pause "Worker (5s delay) + LLM adapter up. Submitting workflow."

    local wf_id run_id
    wf_id=$(create_http_workflow "llm-crash-demo" "http://localhost:9000/decide")
    run_id=$(start_run "$wf_id" '{"task":"crash recovery test"}')
    info "run_id = $run_id"

    # Wait for planner to decide step1 and dispatch it (adapter calls ≥ 1).
    info "Waiting for step1 to be dispatched (~1s)..."
    sleep 3

    warn "Killing orchestrator while step1 is running (5s worker, still processing)."
    kill_orchestrator
    warn "Orchestrator dead. step1 is RUNNING in DB (Barrier 1 fired; Barrier 2 not yet)."

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
    info "Expected ≤ 3 calls (step1 decide, step2 decide, done check)."
    info "If > 3: step1 was re-decided after recovery — that would be a bug."

    if [[ "$status" == "DONE" ]] && [[ "$adapter_calls" -le 3 ]]; then
        success "PASS — Recovery complete; adapter called ${adapter_calls}× (≤3 — no extra re-decision)"
    else
        error "FAIL — status=${status}, adapter_calls=${adapter_calls} (want DONE and ≤3)"
    fi

    stop_orchestrator
    stop_worker ocr
    stop_llm_adapter
    reset_db
}

# ── Menu ──────────────────────────────────────────────────────────────────────

show_menu() {
    echo
    echo -e "${BOLD}${CYAN}   StateFlow Interactive Demo${NC}"
    echo -e "${BOLD}${CYAN}   ══════════════════════════${NC}"
    echo -e "   ${BOLD}1)${NC} Happy Path (Static Planner)"
    echo -e "   ${BOLD}2)${NC} Worker Crash & DLQ Replay"
    echo -e "   ${BOLD}3)${NC} Orchestrator Crash & Recovery"
    echo -e "   ${BOLD}4)${NC} Happy Path (LLM Planner)"
    echo -e "   ${BOLD}5)${NC} Orchestrator Crash (LLM Planner)"
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
        error "demo/worker.py not found at $WORKER_SCRIPT"
        exit 1
    fi

    build

    while true; do
        show_menu
        read -rp "   Choice: " choice
        case "${choice,,}" in
            1) scenario_happy_path_static ;;
            2) scenario_worker_crash ;;
            3) scenario_orchestrator_crash ;;
            4) scenario_happy_path_llm ;;
            5) scenario_orchestrator_crash_llm ;;
            a)
                scenario_happy_path_static
                scenario_worker_crash
                scenario_orchestrator_crash
                scenario_happy_path_llm
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
