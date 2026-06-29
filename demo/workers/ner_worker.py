"""
NER Worker — async, port 5002.

Receives POST /run from StateFlow (async transport).
Body: {"step_id": "...", "attempt_id": "...", "input": {"workflow_input": ..., "history": ...}}
Returns 202 immediately; calls back POST /tasks/complete when done.

Idempotency: caches result keyed on step_id (NOT attempt_id).
If the orchestrator crashes and re-dispatches this step with a new attempt_id,
the worker detects it has already processed this step_id, skips re-processing,
and immediately sends the callback with the NEW attempt_id.

This is the core of the crash-recovery demo:
  1. First dispatch (attempt A): worker starts 5s processing, orchestrator killed
  2. Re-dispatch (attempt B):    worker finds step_id in cache, sends callback
     with attempt B immediately — no 5s sleep.
  3. The orphaned callback from attempt A (if it fires) is rejected by StateFlow
     because attempt A ≠ current_attempt_id (the dedup guard).
"""

import json
import logging
import os
import sys
import threading
import time

import requests as req_lib
from flask import Flask, jsonify, request

app = Flask(__name__)
app.logger.disabled = True

STATEFLOW_URL = os.environ.get("STATEFLOW_URL", "http://localhost:8080")

# step_id -> result (idempotency cache)
_cache: dict = {}
_lock = threading.Lock()


@app.route("/run", methods=["POST"])
def run():
    body = request.get_json(force=True)
    step_id   = body.get("step_id",   "unknown")
    attempt_id = body.get("attempt_id", "unknown")
    short_att  = attempt_id[:8] + "..."

    with _lock:
        cached = _cache.get(step_id)

    if cached is not None:
        # Already processed this step — re-send callback with the NEW attempt_id.
        # This demonstrates worker idempotency: same work, different dispatch.
        print(f"[NER]  ⚡ Already processed step_id={step_id}")
        print(f"[NER]     Re-sending callback with NEW attempt_id={short_att} (no re-processing)")
        sys.stdout.flush()
        threading.Thread(
            target=_send_callback,
            args=(step_id, attempt_id, cached),
            daemon=True,
        ).start()
        return jsonify({"accepted": True, "idempotent": True}), 202

    print(f"[NER]  🏷️  Starting entity extraction")
    print(f"[NER]     step_id={step_id}  attempt_id={short_att}")
    print(f"[NER]     (async, sleeping 5s to simulate LLM entity extraction)")
    sys.stdout.flush()

    threading.Thread(
        target=_process,
        args=(step_id, attempt_id, body.get("input", {})),
        daemon=True,
    ).start()
    return jsonify({"accepted": True}), 202


def _process(step_id: str, attempt_id: str, input_data: dict):
    time.sleep(5)  # simulate LLM processing

    result = {
        "entities": ["Alice Johnson", "Acme Corp", "New York"],
        "count": 3,
        "model": "ner-v2",
    }

    with _lock:
        _cache[step_id] = result

    print(f"[NER]  ✅ Extraction done — 3 entities found")
    sys.stdout.flush()
    _send_callback(step_id, attempt_id, result)


def _send_callback(step_id: str, attempt_id: str, result: dict):
    url = f"{STATEFLOW_URL}/tasks/complete"
    payload = {"step_id": step_id, "attempt_id": attempt_id, "output": result}
    short_att = attempt_id[:8] + "..."
    try:
        r = req_lib.post(url, json=payload, timeout=5)
        if r.status_code == 200:
            print(f"[NER]  📤 Callback delivered — attempt_id={short_att}  HTTP {r.status_code}")
        else:
            print(f"[NER]  ⚠️  Callback got HTTP {r.status_code} (attempt_id={short_att})")
        sys.stdout.flush()
    except Exception as exc:
        print(f"[NER]  ⚠️  Callback failed: {exc}")
        print(f"[NER]     (Orchestrator is down — expected during crash demo)")
        sys.stdout.flush()


if __name__ == "__main__":
    port = 5002
    print(f"[NER]  🚀 NER Worker ready on :{port}  (async — will callback to {STATEFLOW_URL})")
    sys.stdout.flush()
    log = logging.getLogger("werkzeug")
    log.setLevel(logging.ERROR)
    app.run(port=port, debug=False, use_reloader=False, threaded=True)
