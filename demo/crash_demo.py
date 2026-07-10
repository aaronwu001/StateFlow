#!/usr/bin/env python
"""
StateFlow Crash-Recovery Demo
==============================
Proves the headline promise in one script:

  1. Start a 3-step workflow (OCR → NER → Summarize).
  2. Kill the orchestrator while step 2 (NER, async) is mid-flight.
  3. Restart.  Recovery fires.
  4. Step 3 runs.  Steps 1-2 do NOT re-run.

Everything (Postgres, StateFlow, the three workers) runs as docker compose
services — the base stack (docker-compose.yml) plus the demo overlay
(docker-compose.demo.yml).

Prerequisites (must be running before this script):
  docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d

Usage (from the project root or from demo/ — path-independent):
  python demo/crash_demo.py
"""

import atexit
import pathlib
import subprocess
import sys
import time

# Force UTF-8 on Windows (default console is cp1252 which breaks box-drawing chars).
if hasattr(sys.stdout, "reconfigure"):
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
if hasattr(sys.stderr, "reconfigure"):
    sys.stderr.reconfigure(encoding="utf-8", errors="replace")

import requests

# ── Paths ────────────────────────────────────────────────────────────────────

DEMO_DIR     = pathlib.Path(__file__).parent.resolve()
PROJECT_ROOT = DEMO_DIR.parent

COMPOSE_BASE = [
    "-f", str(PROJECT_ROOT / "docker-compose.yml"),
    "-f", str(PROJECT_ROOT / "docker-compose.demo.yml"),
]

# ── Config ───────────────────────────────────────────────────────────────────

STATEFLOW_URL = "http://localhost:8080"
DB_USER       = "stateflow"
DB_NAME       = "stateflow"

WORKER_SERVICES = ["ocr-worker", "ner-worker", "summarize-worker"]
WORKER_URLS = {
    "ocr":       "http://ocr-worker:5001/run",
    "ner":       "http://ner-worker:5002/run",
    "summarize": "http://summarize-worker:5003/run",
}
WORKER_PORTS = {"ocr": 5001, "ner": 5002, "summarize": 5003}  # host-mapped, for readiness polling

# ── Output helpers ───────────────────────────────────────────────────────────

def _p(msg, **kw):
    print(msg, **kw)
    sys.stdout.flush()

def banner(title):
    _p(f"\n{'═'*64}")
    _p(f"   {title}")
    _p(f"{'═'*64}")

def section(label, msg):
    _p(f"\n{'─'*64}")
    _p(f"  [{label}] {msg}")
    _p(f"{'─'*64}")

def info(msg):  _p(f"     {msg}")
def ok(msg):    _p(f"  ✅ {msg}")
def boom(msg):  _p(f"\n  💥 {msg}")
def revive(msg):_p(f"\n  🔄 {msg}")

# ── docker compose helper ────────────────────────────────────────────────────

def compose(*args, check=True):
    """Run `docker compose -f docker-compose.yml -f docker-compose.demo.yml <args>`."""
    cmd = ["docker", "compose", *COMPOSE_BASE, *args]
    r = subprocess.run(cmd, cwd=str(PROJECT_ROOT), capture_output=True, text=True)
    if check and r.returncode != 0:
        _p(f"ERROR: {' '.join(cmd)}")
        _p(r.stdout)
        _p(r.stderr)
        sys.exit(1)
    return r

# ── Build ────────────────────────────────────────────────────────────────────

def build():
    section("BUILD", "docker compose build (stateflow + demo workers)")
    compose("build", "stateflow", *WORKER_SERVICES)
    ok("Images built")

# ── Database ─────────────────────────────────────────────────────────────────

def setup_db():
    section("DB", f"Resetting '{DB_NAME}' in the compose Postgres service")
    compose("up", "-d", "postgres")

    deadline = time.time() + 30
    while time.time() < deadline:
        r = compose("exec", "-T", "postgres", "pg_isready", "-U", DB_USER, "-d", DB_NAME, check=False)
        if r.returncode == 0:
            break
        time.sleep(0.5)
    else:
        _p("ERROR: Postgres did not become ready in time")
        sys.exit(1)

    compose("exec", "-T", "postgres", "psql", "-U", DB_USER, "-d", DB_NAME,
             "-c", "TRUNCATE workflows CASCADE;")

    ok(f"Schema clean — '{DB_NAME}' ready")

# ── Workers ──────────────────────────────────────────────────────────────────

def wait_healthy(service: str, timeout: float = 30):
    """Poll the service's own container healthcheck (docker-compose.demo.yml)
    rather than probing the host-published port: on Docker Desktop the
    host-side port can accept connections slightly before the app inside the
    container actually binds its listen socket, which would let stateflow
    (a container-to-container caller) race ahead of a truly-ready worker."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        cid = compose("ps", "-q", service, check=False).stdout.strip()
        if cid:
            h = subprocess.run(
                ["docker", "inspect", "-f", "{{.State.Health.Status}}", cid],
                capture_output=True, text=True,
            )
            if h.stdout.strip() == "healthy":
                return
        time.sleep(0.3)
    _p(f"ERROR: {service} did not become healthy in time")
    sys.exit(1)

def start_workers():
    section("WORKERS", "Starting worker containers (ocr-worker, ner-worker, summarize-worker)")
    compose("up", "-d", "--force-recreate", *WORKER_SERVICES)

    info("Waiting for workers to be ready...")
    for name in WORKER_SERVICES:
        wait_healthy(name)

    ok(f"Workers ready  OCR:{WORKER_PORTS['ocr']}  NER:{WORKER_PORTS['ner']}  "
       f"Summarize:{WORKER_PORTS['summarize']}")

_log_stream_proc = None

def start_log_stream():
    """Stream container logs into this terminal so the demo's [OCR]/[NER]/[SUMMARIZE]
    and [RECOVERY] markers still appear live, interleaved with this script's own
    output. Started only once the stateflow container definitely exists."""
    global _log_stream_proc
    _log_stream_proc = subprocess.Popen(
        ["docker", "compose", *COMPOSE_BASE, "logs", "-f", "--no-log-prefix",
         "--tail", "0", *WORKER_SERVICES, "stateflow"],
        cwd=str(PROJECT_ROOT),
    )

# ── StateFlow service ─────────────────────────────────────────────────────────

def _container_id(service: str) -> str:
    r = compose("ps", "-q", service, check=False)
    return r.stdout.strip()[:12] or "?"

def start_stateflow(label="StateFlow"):
    compose("up", "-d", "stateflow")

    for _ in range(30):
        try:
            r = requests.get(f"{STATEFLOW_URL}/runs/__probe__", timeout=1)
            if r.status_code in (200, 404):
                break
        except Exception:
            pass
        time.sleep(0.3)
    else:
        _p("ERROR: StateFlow did not start in time")
        sys.exit(1)

    ok(f"{label} ready on :8080  container={_container_id('stateflow')}")

def kill_stateflow():
    cid = _container_id("stateflow")
    compose("kill", "stateflow")
    return cid

# ── StateFlow HTTP API ────────────────────────────────────────────────────────

def create_workflow() -> str:
    planner_config = {
        "steps": [
            {
                "name":            "ocr",
                "worker_url":      WORKER_URLS["ocr"],
                "mode":            "sync",
                "timeout_seconds": 30,
            },
            {
                "name":            "ner",
                "worker_url":      WORKER_URLS["ner"],
                "mode":            "async",
                "timeout_seconds": 60,
            },
            {
                "name":            "summarize",
                "worker_url":      WORKER_URLS["summarize"],
                "mode":            "sync",
                "timeout_seconds": 30,
            },
        ]
    }
    r = requests.post(f"{STATEFLOW_URL}/workflows", json={
        "name":           "crash-demo-pipeline",
        "planner_type":   "static",
        "planner_config": planner_config,
    })
    r.raise_for_status()
    return r.json()["workflow_id"]


def start_run(workflow_id: str) -> str:
    r = requests.post(f"{STATEFLOW_URL}/workflows/{workflow_id}/runs", json={
        "workflow_input": {"doc": "quarterly_report_2026.pdf"},
    })
    r.raise_for_status()
    return r.json()["run_id"]


def get_run(run_id: str) -> dict:
    r = requests.get(f"{STATEFLOW_URL}/runs/{run_id}", timeout=5)
    r.raise_for_status()
    return r.json()


def poll_until(run_id: str, predicate, timeout: float = 60) -> dict:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            data = get_run(run_id)
            if predicate(data):
                return data
        except Exception:
            pass
        time.sleep(0.4)
    raise TimeoutError(f"run {run_id} did not satisfy condition within {timeout}s")


def step_done(data: dict, name: str) -> bool:
    return any(
        s["step_name"] == name and s["status"] == "DONE"
        for s in data.get("steps", [])
    )


def step_running(data: dict, name: str) -> bool:
    return any(
        s["step_name"] == name and s["status"] == "RUNNING"
        for s in data.get("steps", [])
    )

# ── Postgres verification helpers (whitepaper §8.3 orphan-claim proof) ─────────
# crash_demo.py proves recovery not just by watching the run reach DONE, but by
# reading the DB directly for the invariants the whitepaper actually promises:
# exactly one orphaned attempt on the mid-flight step, and the step's Barrier-1
# decision (created_at + decision JSONB) unchanged across the crash — which is
# the direct proof the planner was asked exactly once for that step (recovery's
# "last_step=running" branch never re-asks the planner; it orphan-claims and
# re-dispatches the persisted decision, whitepaper §8.3).

def psql(sql: str) -> list:
    """Run a SQL query via `docker compose exec postgres psql`, return non-empty
    tab-separated rows (no header, no footer)."""
    r = compose("exec", "-T", "postgres", "psql", "-U", DB_USER, "-d", DB_NAME,
                "-t", "-A", "-F", "\t", "-c", sql)
    return [ln for ln in r.stdout.strip().splitlines() if ln.strip() != ""]

def psql_row(sql: str):
    rows = psql(sql)
    return rows[0].split("\t") if rows else None

def psql_scalar(sql: str):
    rows = psql(sql)
    return rows[0].strip() if rows else None

def count_log_lines(service: str, needle: str) -> int:
    """Count occurrences of an ASCII-only `needle` across `service`'s full
    compose logs (a fresh, non-streaming call — independent of the live
    `--tail 0` log_stream Popen, which only writes to this terminal)."""
    r = compose("logs", "--no-log-prefix", service, check=False)
    return r.stdout.count(needle)

# ── Cleanup ───────────────────────────────────────────────────────────────────

def cleanup():
    if _log_stream_proc and _log_stream_proc.poll() is None:
        _log_stream_proc.kill()
    compose("stop", "stateflow", *WORKER_SERVICES, check=False)

atexit.register(cleanup)

# ── Main ──────────────────────────────────────────────────────────────────────

def main():
    banner("StateFlow  —  Crash-Recovery Demo")
    _p("  Proves: kill orchestrator mid-run → restart → completed steps NOT re-run\n")

    # 1. Build + DB + workers
    build()
    setup_db()
    start_workers()

    # 2. Start orchestrator
    section("START", "Starting StateFlow orchestrator (first boot)")
    start_stateflow("StateFlow (boot 1)")
    start_log_stream()

    # 3. Create workflow + run
    section("RUN", "Creating 3-step workflow and launching run")
    wf_id  = create_workflow()
    run_id = start_run(wf_id)
    info(f"workflow_id : {wf_id}")
    info(f"run_id      : {run_id}")
    _p("")
    info("Watch above: [OCR] logs will appear for step 1 (sync, 2s)...")

    # 4. Wait for OCR (step 1) to complete
    poll_until(run_id, lambda d: step_done(d, "ocr"), timeout=30)
    ok("Step 1 (OCR, sync) DONE  ✓")

    # 5. Wait for NER (step 2) to be dispatched (RUNNING status appears in DB)
    info("Waiting for step 2 (NER, async) to be dispatched...")
    poll_until(run_id, lambda d: step_running(d, "ner"), timeout=15)
    info("NER dispatched — it is sleeping 5s before sending its callback")

    # Capture NER's Barrier-1 record (created_at + decision) BEFORE the kill.
    # Recovery's "last_step=running" path (whitepaper §8.3) must never touch
    # this row's created_at/decision — only orphan-claim the attempt and
    # dispatch a NEW attempt. If either field differs after recovery, the step
    # row was re-created, which would mean the planner was re-asked for "ner".
    pre_ner = psql_row(
        f"SELECT created_at, decision FROM steps "
        f"WHERE run_id='{run_id}' AND step_name='ner'"
    )
    if not pre_ner:
        _p("ERROR: could not read pre-crash NER step row from Postgres")
        sys.exit(1)
    pre_ner_created_at, pre_ner_decision = pre_ner[0], pre_ner[1]

    # 6. Kill orchestrator while NER is mid-flight
    time.sleep(1.0)   # brief pause to let the dispatch POST reach the NER worker
    cid = kill_stateflow()
    boom(f"KILLING ORCHESTRATOR  —  container {cid}")
    boom("NER's async callback channel dies with the process")
    boom("DB still shows step 2 RUNNING (no output); step 3 never started")
    _p("")

    # 7. Wait for NER's first background thread to finish and cache its result
    info("Waiting 5s for NER's background thread to complete and cache its result.")
    info("(NER's callback attempt will fail — orchestrator is down. That is expected.)")
    for remaining in range(5, 0, -1):
        _p(f"     ⏳ {remaining}s ...", end="\r")
        time.sleep(1)
    _p("     ⏳ 0s     ")

    # 8. Restart orchestrator — recovery fires
    revive("RESTARTING ORCHESTRATOR  —  RecoverRuns fires at startup")
    section("RECOVERY", "StateFlow boot 2 — watch for recovery log lines")
    _p("  ┌─ Expected log: msg=\"[RECOVERY] found in-progress runs\" count=1")
    _p("  └─ Expected log: msg=\"[RECOVERY] resuming run\" steps_done=1 pending_step=ner")
    _p("")
    start_stateflow("StateFlow (boot 2 — recovery)")

    # 9. Poll until run reaches terminal status
    info("Polling until run completes...")
    data = poll_until(run_id, lambda d: d.get("status") != "RUNNING", timeout=30)

    # 10. Verify the orphan-claim path (whitepaper §8.3), the idempotency-cache
    #     absorption of the re-dispatch, and the planner-asked-at-most-once
    #     guarantee (Barrier 1) — DB/log assertions, not just a visual proof.
    section("VERIFY", "Orphan claim, idempotency-cache absorption, planner-asked-once")

    orphaned_count = psql_scalar(
        f"SELECT count(*) FROM attempts a JOIN steps s ON a.step_id = s.step_id "
        f"WHERE s.run_id='{run_id}' AND s.step_name='ner' "
        f"AND a.status='FAILED' AND a.failure_reason='orphaned'"
    )
    if orphaned_count != "1":
        _p(f"  ❌ FAIL — expected exactly 1 orphaned attempt for step 'ner', got {orphaned_count!r}")
        sys.exit(1)
    ok("NER step's attempt history shows exactly ONE attempt with failure_reason='orphaned' "
       "(recovery claimed the crash-killed attempt, per the combination table's "
       "run=RUNNING/last_step=RUNNING row)")

    post_ner = psql_row(
        f"SELECT created_at, decision FROM steps "
        f"WHERE run_id='{run_id}' AND step_name='ner'"
    )
    if post_ner is None:
        _p("  ❌ FAIL — NER step row vanished after restart")
        sys.exit(1)
    if post_ner[0] != pre_ner_created_at or post_ner[1] != pre_ner_decision:
        _p("  ❌ FAIL — NER step's created_at/decision changed across the crash — "
           "the step row was re-created, meaning the planner was asked again for 'ner'")
        _p(f"       before: created_at={pre_ner_created_at!r} decision={pre_ner_decision!r}")
        _p(f"       after:  created_at={post_ner[0]!r} decision={post_ner[1]!r}")
        sys.exit(1)
    ok("NER step's Barrier-1 record (created_at + decision) is byte-identical before and "
       "after the crash — the planner was asked for 'ner' exactly once, never re-asked on recovery")

    extraction_starts = count_log_lines("ner-worker", "Starting entity extraction")
    if extraction_starts != 1:
        _p(f"  ❌ FAIL — expected NER's actual extraction work to run exactly once, "
           f"saw {extraction_starts} 'Starting entity extraction' log lines "
           f"(idempotency cache did not absorb the crash re-dispatch)")
        sys.exit(1)
    ok("NER worker's actual extraction work ran exactly ONCE — the post-recovery re-dispatch "
       "hit the worker's step_id-keyed idempotency cache instead of re-processing "
       "(no duplicate side effect)")

    # 11. Results
    banner("DEMO COMPLETE")
    status = data.get("status")
    steps  = data.get("steps", [])

    _p(f"\n  Run status : {status}")
    _p(f"\n  Steps:")
    for s in steps:
        _p(f"    [{s['status']:6}] {s['step_name']}")

    _p("")
    _p("  PROOF THAT STEPS 1-2 WERE NOT RE-RUN AFTER RESTART")
    _p("  " + "─"*60)
    _p("  Search the terminal log above for these markers:\n")
    _p("  BEFORE CRASH:")
    _p("    [OCR] 🔍  ...  appears ONCE  (step 1 done before kill)")
    _p("    [NER]  🏷️  ...  appears ONCE  (step 2 in-flight when killed)")
    _p("    [NER]  ⚠️  Callback failed    (orchestrator was down)")
    _p("")
    _p("  AFTER RESTART:")
    _p("    msg=\"[RECOVERY] resuming run\"    (recovery found the RUNNING run)")
    _p("    [NER]  ⚡ Already processed   (re-dispatch, idempotency cache hit)")
    _p("    [NER]  📤 Callback delivered  (new attempt_id → StateFlow)")
    _p("    [SUMMARIZE] ✍️  ...            (step 3 runs for the FIRST time)")
    _p("")
    _p("  ABSENT (proves no re-run):")
    _p("    [OCR] 🔍 does NOT appear again after the restart banner")
    _p("    [NER]  🏷️  does NOT appear again (cache path used instead)")

    _p("")
    if status == "DONE":
        ok("Crash-recovery demo successful — the run completed without re-running done steps.")
        return 0
    else:
        _p(f"  ⚠️  Run ended with status {status!r} — check logs above.")
        return 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        _p("\nInterrupted — cleaning up.")
        sys.exit(0)
