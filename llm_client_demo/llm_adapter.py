"""
StateFlow HTTP planner adapter.

Two modes (auto-detected):
  - REAL:  ANTHROPIC_API_KEY is set  -> calls Claude to decide the next step
  - DUMMY: no key                    -> hardcoded 2-step pipeline (for wiring test)

StateFlow calls:  POST /decide
  body: {"run_id": "...", "workflow_input": {...}, "history": [...]}

Must return ONLY a JSON object -- no markdown, no prose.

Usage:
  python llm_client_demo/llm_adapter.py
"""

import json
import os
import re
import sys

from flask import Flask, request, jsonify

app = Flask(__name__)

# ── Detect mode ──────────────────────────────────────────────────────────────

API_KEY = os.environ.get("ANTHROPIC_API_KEY")

if API_KEY:
    import anthropic
    client = anthropic.Anthropic(api_key=API_KEY)
    MODE = "real"
    print("[ADAPTER] mode=REAL  (using Claude claude-sonnet-4-6)")
else:
    MODE = "dummy"
    print("[ADAPTER] mode=DUMMY (ANTHROPIC_API_KEY not set — using hardcoded logic)")
    print("[ADAPTER] Set ANTHROPIC_API_KEY and restart to use a real LLM.")

# ── System prompt (used in REAL mode) ────────────────────────────────────────

SYSTEM_PROMPT = """
You are a workflow planner for a document processing pipeline.

You will receive a JSON object with:
  - "workflow_input": the original task
  - "history": completed steps so far, each with "name", "status", "output"

Available workers:
  - http://localhost:5010/run   An echo worker. Useful for any processing step.
                                Input: any JSON you like (include "_step_name" so logs are readable).

Rules:
1. Respond with ONLY a JSON object. No explanation, no markdown fences, no prose.
2. The JSON must have "status": "continue", "done", or "fail".
3. If "continue", include "step" with "name", "worker_url", "mode" (use "sync"), "input".
4. Run at most 2 steps total (check history length).
5. After 2 steps are done, return {"status": "done"}.
6. Never repeat a step name that appears in history.

Example:
{"status":"continue","step":{"name":"step1","worker_url":"http://localhost:5010/run","mode":"sync","timeout_seconds":10,"input":{"_step_name":"step1","task":"do something"}}}
"""

# ── Planner endpoint ──────────────────────────────────────────────────────────

@app.route("/decide", methods=["POST"])
def decide():
    state = request.get_json(force=True, silent=True)
    if state is None:
        print("[ADAPTER] ERROR: could not parse request body")
        return jsonify({"error": "invalid JSON"}), 400

    history = state.get("history", [])
    # StateFlow sends status as uppercase "DONE" (DB canonical form)
    done_names = [s["name"] for s in history if s.get("status", "").upper() == "DONE"]

    print(f"\n[ADAPTER] Planner called  run_id={state.get('run_id')}  history={done_names}")

    if MODE == "real":
        decision = _decide_with_llm(state)
    else:
        decision = _decide_dummy(state, done_names)

    print(f"[ADAPTER] Decision: {json.dumps(decision)}")
    return jsonify(decision)


def _decide_dummy(state, done_names):
    """Hardcoded 2-step pipeline — no LLM needed."""
    workflow_input = state.get("workflow_input", {})

    if "step1" not in done_names:
        return {
            "status": "continue",
            "step": {
                "name": "step1",
                "worker_url": "http://localhost:5010/run",
                "mode": "sync",
                "timeout_seconds": 10,
                "input": {"_step_name": "step1", "task": workflow_input},
            },
        }
    if "step2" not in done_names:
        step1_output = next(
            (s["output"] for s in state["history"] if s["name"] == "step1"), {}
        )
        return {
            "status": "continue",
            "step": {
                "name": "step2",
                "worker_url": "http://localhost:5010/run",
                "mode": "sync",
                "timeout_seconds": 10,
                "input": {"_step_name": "step2", "previous": step1_output},
            },
        }
    return {"status": "done"}


def _decide_with_llm(state):
    """Call Claude to get the next step decision."""
    msg = client.messages.create(
        model="claude-sonnet-4-6",
        max_tokens=512,
        system=SYSTEM_PROMPT,
        messages=[{"role": "user", "content": json.dumps(state)}],
    )
    raw = msg.content[0].text.strip()
    print(f"[ADAPTER] LLM raw response: {raw}")

    # Strip markdown fences if the LLM wrapped the response
    m = re.search(r"\{.*\}", raw, re.DOTALL)
    if m:
        raw = m.group()

    try:
        return json.loads(raw)
    except json.JSONDecodeError as e:
        print(f"[ADAPTER] ERROR: could not parse LLM response: {e}")
        # StateFlow will retry the planner up to 2 more times
        raise


if __name__ == "__main__":
    print("[ADAPTER] LLM adapter listening on :9000")
    app.run(port=9000, debug=False)
