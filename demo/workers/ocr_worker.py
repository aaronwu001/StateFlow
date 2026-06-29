"""
OCR Worker — sync, port 5001.

Receives POST /run from StateFlow (sync transport).
Body: {"workflow_input": {...}, "history": [...]}
Returns: full JSON response body (stored as step output).

Idempotency: caches the result keyed on the full input JSON.
If StateFlow re-dispatches this step (e.g., crash during sync-hold),
the worker returns the cached result without re-running the 2s sleep.
"""

import json
import sys
import time

from flask import Flask, jsonify, request

app = Flask(__name__)
app.logger.disabled = True

# In-memory idempotency cache: input_hash -> result
# The static planner sends (workflow_input + history) as the step input.
# For a given step position, the input is always identical on re-dispatch,
# so the JSON hash is a reliable idempotency key.
_cache: dict = {}


@app.route("/run", methods=["POST"])
def run():
    body = request.get_json(force=True)
    cache_key = json.dumps(body, sort_keys=True)

    if cache_key in _cache:
        print("[OCR] ⚡ Already processed this input — returning cached result (idempotent re-dispatch)")
        sys.stdout.flush()
        return jsonify(_cache[cache_key])

    doc = body.get("workflow_input", {}).get("doc", "unknown")
    print(f"[OCR] 🔍 Processing document: {doc}")
    print(f"[OCR]     (sync, sleeping 2s to simulate text extraction)")
    sys.stdout.flush()

    time.sleep(2)

    result = {
        "pages": 3,
        "text": "Q3 2026 earnings strong. Revenue up 18%. Alice Johnson (CEO) comments...",
        "confidence": 0.98,
    }
    _cache[cache_key] = result

    print(f"[OCR] ✅ Extraction complete — 3 pages, confidence {result['confidence']}")
    sys.stdout.flush()
    return jsonify(result)


if __name__ == "__main__":
    port = 5001
    print(f"[OCR] 🚀 OCR Worker ready on :{port}  (sync — no callback needed)")
    sys.stdout.flush()
    import logging
    log = logging.getLogger("werkzeug")
    log.setLevel(logging.ERROR)
    app.run(port=port, debug=False, use_reloader=False)
