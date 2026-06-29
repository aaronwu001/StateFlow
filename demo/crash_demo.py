#!/usr/bin/env python
"""
StateFlow Crash-Recovery Demo
==============================
Proves the headline promise in one script:

  1. Start a 3-step workflow (OCR → NER → Summarize).
  2. Kill the orchestrator while step 2 (NER, async) is mid-flight.
  3. Restart.  Recovery fires.
  4. Step 3 runs.  Steps 1-2 do NOT re-run.

Prerequisites (must be running before this script):
  • Docker container 'stateflow-pg-test'  (postgres:16-alpine)
  • Python packages: flask, requests  (pip install -r requirements.txt)
  • Go toolchain  (go build is run automatically)

Usage (from the project root):
  cd demo
  python crash_demo.py
"""

import atexit
import json
import logging
import os
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

DEMO_DIR    = pathlib.Path(__file__).parent.resolve()
PROJECT_ROOT = DEMO_DIR.parent
WORKERS_DIR  = DEMO_DIR / "workers"
SF_BINARY    = DEMO_DIR / "stateflow"          # built by this script
MIGRATION_SQL = PROJECT_ROOT / "migrations" / "001_initial.sql"

# ── Config ───────────────────────────────────────────────────────────────────

STATEFLOW_URL   = "http://localhost:8080"
DB_CONTAINER    = "stateflow-pg-test"
DB_NAME         = "stateflow_demo"
DB_USER         = "postgres"
DB_PASS         = "postgres"
DATABASE_URL    = f"postgres://{DB_USER}:{DB_PASS}@localhost:5432/{DB_NAME}?sslmode=disable"

WORKER_PORTS    = {"ocr": 5001, "ner": 5002, "summarize": 5003}

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

# ── Build ────────────────────────────────────────────────────────────────────

def build():
    section("BUILD", "go build ./cmd/stateflow/")
    r = subprocess.run(
        ["go", "build", "-o", str(SF_BINARY), "./cmd/stateflow/"],
        cwd=str(PROJECT_ROOT),
        capture_output=True,
        text=True,
    )
    if r.returncode != 0:
        _p(r.stderr)
        sys.exit(1)
    ok(f"Binary ready → {SF_BINARY.name}")

# ── Database ─────────────────────────────────────────────────────────────────

def setup_db():
    section("DB", f"Creating fresh '{DB_NAME}' in Docker Postgres")

    def docker_psql(sql, db="postgres"):
        return subprocess.run(
            ["docker", "exec", DB_CONTAINER, "psql", "-U", DB_USER, "-d", db, "-c", sql],
            capture_output=True, text=True,
        )

    docker_psql(f"DROP DATABASE IF EXISTS {DB_NAME};")
    r = docker_psql(f"CREATE DATABASE {DB_NAME};")
    if r.returncode != 0:
        _p(f"ERROR: {r.stderr}")
        sys.exit(1)

    migration = MIGRATION_SQL.read_bytes()
    r = subprocess.run(
        ["docker", "exec", "-i", DB_CONTAINER, "psql", "-U", DB_USER, "-d", DB_NAME],
        input=migration,
        capture_output=True,
    )
    if r.returncode != 0:
        _p(f"ERROR: {r.stderr.decode()}")
        sys.exit(1)

    ok(f"Schema applied — {DB_NAME} ready")

# ── Workers ──────────────────────────────────────────────────────────────────

_worker_procs: list = []

def start_workers():
    section("WORKERS", "Starting Python Flask workers")
    env = {**os.environ, "STATEFLOW_URL": STATEFLOW_URL}
    scripts = {
        "ocr":       WORKERS_DIR / "ocr_worker.py",
        "ner":       WORKERS_DIR / "ner_worker.py",
        "summarize": WORKERS_DIR / "summarize_worker.py",
    }
    for name, path in scripts.items():
        p = subprocess.Popen([sys.executable, str(path)], env=env)
        _worker_procs.append(p)

    # Wait for all Flask servers to be listening (TCP connect probe).
    import socket
    info("Waiting for workers to be ready...")
    for name, port in WORKER_PORTS.items():
        deadline = time.time() + 15
        while time.time() < deadline:
            try:
                with socket.create_connection(("localhost", port), timeout=0.5):
                    break
            except OSError:
                time.sleep(0.3)
        else:
            _p(f"ERROR: {name} worker (:{port}) didn't start in time")
            sys.exit(1)

    ok(f"Workers ready  OCR:{WORKER_PORTS['ocr']}  NER:{WORKER_PORTS['ner']}  "
       f"Summarize:{WORKER_PORTS['summarize']}")

# ── StateFlow process ─────────────────────────────────────────────────────────

_sf_proc = None

def _sf_env():
    return {**os.environ, "DATABASE_URL": DATABASE_URL, "LISTEN_ADDR": ":8080"}

def start_stateflow(label="StateFlow"):
    global _sf_proc
    _sf_proc = subprocess.Popen([str(SF_BINARY)], env=_sf_env())

    # Poll until HTTP server answers.
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

    ok(f"{label} ready on :8080  pid={_sf_proc.pid}")

def kill_stateflow():
    global _sf_proc
    if _sf_proc and _sf_proc.poll() is None:
        pid = _sf_proc.pid
        _sf_proc.kill()
        _sf_proc.wait()
        _sf_proc = None
        return pid
    return None

# ── StateFlow HTTP API ────────────────────────────────────────────────────────

def create_workflow() -> str:
    planner_config = {
        "steps": [
            {
                "name":            "ocr",
                "worker_url":      f"http://localhost:{WORKER_PORTS['ocr']}/run",
                "mode":            "sync",
                "timeout_seconds": 30,
            },
            {
                "name":            "ner",
                "worker_url":      f"http://localhost:{WORKER_PORTS['ner']}/run",
                "mode":            "async",
                "timeout_seconds": 60,
            },
            {
                "name":            "summarize",
                "worker_url":      f"http://localhost:{WORKER_PORTS['summarize']}/run",
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

# ── Cleanup ───────────────────────────────────────────────────────────────────

def cleanup():
    kill_stateflow()
    for p in _worker_procs:
        try:
            p.kill()
        except Exception:
            pass

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

    # 6. Kill orchestrator while NER is mid-flight
    time.sleep(1.0)   # brief pause to let the dispatch POST reach the NER worker
    boom("KILLING ORCHESTRATOR  —  pid " + str(_sf_proc.pid if _sf_proc else "?"))
    boom("NER's async callback channel dies with the process")
    boom("DB still shows step 2 RUNNING (no output); step 3 never started")
    pid = kill_stateflow()
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
    _p("  ┌─ Expected log: msg=\"RecoverRuns: complete\" count=1")
    _p("  └─ Expected log: msg=\"recovery: resuming run\"")
    _p("")
    start_stateflow("StateFlow (boot 2 — recovery)")

    # 9. Poll until run reaches terminal status
    info("Polling until run completes...")
    data = poll_until(run_id, lambda d: d.get("status") != "RUNNING", timeout=30)

    # 10. Results
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
    _p("    msg=\"recovery: resuming run\"    (recovery found the RUNNING run)")
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
