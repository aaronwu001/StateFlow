"""
Minimal sync worker: echoes its input back as output.
Runs on port 5010 by default.

Used for standalone testing of the LLM adapter without the generic worker.py.
StateFlow sends:  POST /run  body = whatever `step.input` was in the planner decision
Worker returns:   JSON (anything) -- StateFlow stores it as the step output
"""
from flask import Flask, request, jsonify
import time

app = Flask(__name__)

@app.route("/run", methods=["POST"])
def run():
    body = request.get_json()
    name = body.get("_step_name", "unknown")
    print(f"[WORKER] received step={name}  input={body}")
    time.sleep(0.5)  # simulate work
    return jsonify({"echo": body, "status": "completed"})

if __name__ == "__main__":
    print("[WORKER] echo worker listening on :5010")
    app.run(port=5010, debug=False)
