#!/usr/bin/env python3
"""
Standalone fake HTTP PLANNER for the acceptance oracles. Stdlib only.

Implements the planner contract (whitepaper §12): it receives a RunState
{run_id, workflow_input, history} and returns a StepDecision. The plan is a
fixed list of StepSpecs supplied via env; the planner returns plan[len(history)]
as `continue`, and `done` once history is exhausted. Deterministic, no LLM.

It also records, per run_id, how many times each step index was decided, exposed
at GET /stats — lets a test confirm "planner asked once per persisted step".

ENV:
  PORT        default 7102
  PLAN_JSON   required — a JSON list of StepSpec objects, e.g.
              [{"name":"s1","worker_url":"http://host.docker.internal:7101/sync/ok",
                "mode":"sync","timeout_seconds":30,"input":{"x":1}}, ...]
"""
import json
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("PORT", "7102"))
PLAN = json.loads(os.environ["PLAN_JSON"])

_decides = {}   # (run_id, index) -> count
_lock = threading.Lock()


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _read(self):
        n = int(self.headers.get("Content-Length", "0"))
        return json.loads(self.rfile.read(n) or b"{}") if n else {}

    def _send(self, code, body):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(body).encode())

    def do_GET(self):
        if self.path == "/stats":
            with _lock:
                self._send(200, {"decides": {f"{k[0]}#{k[1]}": v for k, v in _decides.items()}})
        else:
            self._send(404, {})

    def do_POST(self):
        state = self._read()
        run_id = state.get("run_id", "?")
        history = state.get("history", [])
        idx = len(history)
        with _lock:
            _decides[(run_id, idx)] = _decides.get((run_id, idx), 0) + 1
        if idx >= len(PLAN):
            self._send(200, {"status": "done"})
        else:
            self._send(200, {"status": "continue", "step": PLAN[idx]})


if __name__ == "__main__":
    print(f"[fake_planner] :{PORT} plan={len(PLAN)} steps")
    ThreadingHTTPServer(("0.0.0.0", PORT), H).serve_forever()
