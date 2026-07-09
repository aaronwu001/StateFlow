#!/usr/bin/env python3
"""
Shared harness for the acceptance oracles. Launches the standalone fake planner
and fake worker as local subprocesses, creates a workflow that uses the http
planner, and provides HTTP + psql helpers. No demo dependency.

NETWORKING (host <-> container):
  - The orchestrator runs in a container; the fakes run on the host. The
    orchestrator reaches them at ADVERTISE_HOST (default host.docker.internal).
    * Docker Desktop (Mac/Windows): works out of the box.
    * Linux: start the orchestrator container with
        --add-host=host.docker.internal:host-gateway
      (or set ADVERTISE_HOST to the docker bridge IP).
  - The fake worker reaches the orchestrator's callbacks at ORCH_CALLBACK_BASE
    (default http://localhost:8080 — the host-mapped orchestrator port).
  - The test reaches the orchestrator API at API_BASE (default same port).

ENV (shared):
  API_BASE            default http://localhost:8080
  TEST_DATABASE_URL   required
  ADVERTISE_HOST      default host.docker.internal
  WORKER_PORT         default 7101
  PLANNER_PORT        default 7102
  PLANNER_CONFIG_KEY  default "url"  (CONFIRM: the key inside planner_config
                        that the HTTPPlanner reads for its endpoint — must match
                        Session 5's implementation)
"""
import json
import os
import subprocess
import sys
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
API_BASE = os.environ.get("API_BASE", "http://localhost:8080")
DB_URL = os.environ["TEST_DATABASE_URL"]
ADVERTISE_HOST = os.environ.get("ADVERTISE_HOST", "host.docker.internal")
WORKER_PORT = int(os.environ.get("WORKER_PORT", "7101"))
PLANNER_PORT = int(os.environ.get("PLANNER_PORT", "7102"))
PLANNER_CONFIG_KEY = os.environ.get("PLANNER_CONFIG_KEY", "url")


def die(msg):
    print(f"FAIL: {msg}", file=sys.stderr)
    sys.exit(1)


def ok(msg):
    print(f"  ok: {msg}")


def http_post(path, body):
    data = json.dumps(body).encode()
    req = urllib.request.Request(API_BASE + path, data=data,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=15) as r:
        raw = r.read().decode()
        return json.loads(raw) if raw.strip() else {}


def http_get(path):
    with urllib.request.urlopen(API_BASE + path, timeout=15) as r:
        return json.loads(r.read().decode())


def psql(sql):
    out = subprocess.run(["psql", DB_URL, "-tAF\t", "-c", sql],
                         capture_output=True, text=True)
    if out.returncode != 0:
        die(f"psql error for [{sql}]: {out.stderr.strip()}")
    return [l for l in out.stdout.strip().splitlines() if l != ""]


def scalar(sql):
    rows = psql(sql)
    return rows[0] if rows else None


def worker_url(route):
    return f"http://{ADVERTISE_HOST}:{WORKER_PORT}{route}"


def _wait_port(url, secs=10):
    end = time.time() + secs
    while time.time() < end:
        try:
            urllib.request.urlopen(url, timeout=1)
            return True
        except Exception:
            time.sleep(0.2)
    return False


class Fakes:
    """Context manager: launches fake worker + fake planner, tears them down."""
    def __init__(self, plan, worker_delay_s=6):
        self.plan = plan
        self.worker_delay_s = worker_delay_s
        self.procs = []

    def __enter__(self):
        wenv = dict(os.environ, PORT=str(WORKER_PORT), DELAY_S=str(self.worker_delay_s))
        penv = dict(os.environ, PORT=str(PLANNER_PORT), PLAN_JSON=json.dumps(self.plan))
        self.procs.append(subprocess.Popen(
            [sys.executable, os.path.join(HERE, "fake_worker.py")], env=wenv))
        self.procs.append(subprocess.Popen(
            [sys.executable, os.path.join(HERE, "fake_planner.py")], env=penv))
        if not _wait_port(f"http://localhost:{WORKER_PORT}/stats"):
            die("fake worker did not come up")
        if not _wait_port(f"http://localhost:{PLANNER_PORT}/stats"):
            die("fake planner did not come up")
        return self

    def planner_endpoint(self):
        return f"http://{ADVERTISE_HOST}:{PLANNER_PORT}/"

    def worker_stats(self):
        return http_get_abs(f"http://localhost:{WORKER_PORT}/stats")

    def __exit__(self, *a):
        for p in self.procs:
            p.terminate()
        for p in self.procs:
            try:
                p.wait(timeout=5)
            except Exception:
                p.kill()


def http_get_abs(url):
    with urllib.request.urlopen(url, timeout=10) as r:
        return json.loads(r.read().decode())


def create_workflow(planner_endpoint, name="acceptance", extra_config=None):
    """Create an http-planner workflow. CONFIRM: planner_config key names.
    Per whitepaper §14.1 the workflows table has no retry/timeout columns, so the
    retry limit X and timeout defaults live inside planner_config — pass them via
    extra_config (their exact key names are impl-defined; confirm at Session 6)."""
    cfg = {PLANNER_CONFIG_KEY: planner_endpoint}
    if extra_config:
        cfg.update(extra_config)
    body = {"name": name, "planner_type": "http", "planner_config": cfg}
    resp = http_post("/workflows", body)
    wid = resp.get("workflow_id") or resp.get("id")
    if not wid:
        die(f"no workflow_id in response {resp} — check POST /workflows shape")
    return wid


def start_run(wid, workflow_input):
    resp = http_post(f"/workflows/{wid}/runs", {"workflow_input": workflow_input})
    rid = resp.get("run_id") or resp.get("id")
    if not rid:
        die(f"no run_id in response {resp}")
    return rid
