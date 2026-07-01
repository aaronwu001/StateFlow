"""
Generic StateFlow demo worker.

Configurable via environment variables:
  WORKER_NAME   (default: worker) — identifies this worker in logs
  WORKER_PORT   (default: 5010)  — port to listen on
  WORKER_DELAY  (default: 1)     — seconds to sleep (simulates processing)

Endpoint: POST /run
  - Logs receipt with a timestamp (greppable for invocation counts)
  - Sleeps for WORKER_DELAY seconds
  - Returns {"worker": name, "echo": <input body>, "processed_at": "<ISO timestamp>"}

The processed_at timestamp is the demo proof that a step was or was NOT re-run:
if it matches the timestamp from the first run, the step was replayed from the DB,
not re-executed by the worker.
"""

import os
import sys
import time
from datetime import datetime, timezone
from flask import Flask, request, jsonify
import logging

WORKER_NAME  = os.environ.get("WORKER_NAME", "worker")
WORKER_PORT  = int(os.environ.get("WORKER_PORT", "5010"))
WORKER_DELAY = float(os.environ.get("WORKER_DELAY", "1"))

app = Flask(__name__)

# Suppress Flask's default request log — we write our own.
log = logging.getLogger("werkzeug")
log.setLevel(logging.ERROR)


def _log(msg: str) -> None:
    line = f"[WORKER:{WORKER_NAME}] {msg}"
    print(line, flush=True)


@app.route("/run", methods=["POST"])
def run():
    body = request.get_json(force=True, silent=True) or {}
    ts = datetime.now(timezone.utc).isoformat()

    # This line is the grep target for invocation counting in run_demo.sh:
    #   grep -c "[WORKER:step1] received step" /tmp/worker_step1.log
    _log(f"received step at {ts}  input_keys={sorted(body.keys())}")

    time.sleep(WORKER_DELAY)

    result = {
        "worker":       WORKER_NAME,
        "echo":         body,
        "processed_at": ts,
    }
    _log(f"done  processed_at={ts}")
    return jsonify(result)


if __name__ == "__main__":
    _log(f"listening on :{WORKER_PORT}  delay={WORKER_DELAY}s")
    app.run(port=WORKER_PORT, debug=False, use_reloader=False)
