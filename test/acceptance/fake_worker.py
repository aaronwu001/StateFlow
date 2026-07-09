#!/usr/bin/env python3
"""
Standalone fake WORKER for the acceptance oracles. Stdlib only. No demo deps.

Behaviour is chosen by URL path (the planner points each step at one of these):
  POST /sync/ok      -> 200, body {"result": <echo of input>}
  POST /sync/fail    -> 500 (worker_reported failure)
  POST /async/ok     -> 202, then after DELAY_S seconds POST /tasks/complete to
                        the orchestrator with {step_id, attempt_id, output}
  POST /async/fail   -> 202, then POST /tasks/fail
  GET  /stats        -> {"executions": {step_id: n}}  (real executions, cache misses)

Idempotency: keyed on step_id (sync: X-StateFlow-Step-ID header; async: envelope
step_id). A duplicate dispatch of the same step_id returns the cached output
instead of re-executing — this models the §15 worker contract. Executions counts
cache MISSES only; it is NOT asserted by the oracles (a real crash mid-execution
can legitimately execute twice), it is here for manual inspection.

ENV:
  PORT               default 7101
  DELAY_S            default 6   (async delay; long enough for the crash test to
                                  catch the step mid-flight)
  ORCH_CALLBACK_BASE default http://localhost:8080  (how THIS worker, running on
                                  the host, reaches the orchestrator's callbacks)
"""
import json
import os
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("PORT", "7101"))
DELAY_S = float(os.environ.get("DELAY_S", "6"))
ORCH = os.environ.get("ORCH_CALLBACK_BASE", "http://localhost:8080")

_cache = {}          # step_id -> output
_executions = {}     # step_id -> count of real executions
_lock = threading.Lock()


def _post(url, body):
    data = json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, headers={"Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=10).read()
    except Exception as e:
        print(f"[fake_worker] callback to {url} failed: {e}")


def _execute(step_id, input_obj):
    """Return cached output if seen; else 'run' (echo) and cache it."""
    with _lock:
        if step_id in _cache:
            return _cache[step_id]
        _executions[step_id] = _executions.get(step_id, 0) + 1
        out = {"result": input_obj}
        _cache[step_id] = out
        return out


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _read(self):
        n = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(n) if n else b"{}"
        return json.loads(raw or b"{}")

    def _send(self, code, body=None):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        if body is not None:
            self.wfile.write(json.dumps(body).encode())

    def do_GET(self):
        if self.path == "/stats":
            with _lock:
                self._send(200, {"executions": dict(_executions)})
        else:
            self._send(404)

    def do_POST(self):
        body = self._read()
        path = self.path.split("?")[0]

        if path == "/sync/ok":
            step_id = self.headers.get("X-StateFlow-Step-ID", "unknown")
            # sync: body is the BARE input (no envelope) — see §13.1
            self._send(200, _execute(step_id, body))
        elif path == "/sync/fail":
            self._send(500, {"error": "deliberate sync failure"})
        elif path == "/async/ok":
            step_id = body.get("step_id")
            attempt_id = body.get("attempt_id")
            input_obj = body.get("input")
            self._send(202)
            def finish():
                time.sleep(DELAY_S)
                out = _execute(step_id, input_obj)
                _post(f"{ORCH}/tasks/complete",
                      {"step_id": step_id, "attempt_id": attempt_id, "output": out})
            threading.Thread(target=finish, daemon=True).start()
        elif path == "/async/fail":
            step_id = body.get("step_id")
            attempt_id = body.get("attempt_id")
            self._send(202)
            def fail():
                time.sleep(0.2)
                _post(f"{ORCH}/tasks/fail",
                      {"step_id": step_id, "attempt_id": attempt_id, "error": "deliberate"})
            threading.Thread(target=fail, daemon=True).start()
        else:
            self._send(404)


if __name__ == "__main__":
    print(f"[fake_worker] :{PORT} delay={DELAY_S}s callback_base={ORCH}")
    ThreadingHTTPServer(("0.0.0.0", PORT), H).serve_forever()
