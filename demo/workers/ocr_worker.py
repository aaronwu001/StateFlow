"""
OCR Worker — sync, port 5001.

Receives POST /run from StateFlow (sync transport).
Body: {"workflow_input": {...}, "history": [...]}  (the bare `input` the
planner decided — sync's zero-modification promise, whitepaper §13.1)
Headers: X-StateFlow-Step-ID, X-StateFlow-Attempt-ID
Returns: full JSON response body (stored as step output).

Idempotency (whitepaper §13.1, §15, User Manual §2.3): keys primarily on the
X-StateFlow-Step-ID header — constant across every retry/re-dispatch of this
step, so it is a precise idempotency key with none of the "input must be
byte-identical / no non-deterministic fields" caveats a content hash carries.
Falls back to hashing the input body only if the header is absent (e.g. a
caller that isn't StateFlow, or an older client) — demonstrating the manual's
documented fallback path, not the primary recommendation.
"""

import json
import sys
import time

from flask import Flask, jsonify, request

app = Flask(__name__)
app.logger.disabled = True

# In-memory idempotency cache: cache_key -> result.
# Primary key: the X-StateFlow-Step-ID header (constant across retries).
# Fallback key: a hash of the full input JSON, used only when the header is
# absent — reliable only as long as the input has no non-deterministic fields.
_cache: dict = {}


@app.route("/run", methods=["POST"])
def run():
    body = request.get_json(force=True)
    step_id = request.headers.get("X-StateFlow-Step-ID")
    if step_id:
        cache_key = f"step_id:{step_id}"
    else:
        cache_key = f"input_hash:{json.dumps(body, sort_keys=True)}"

    if cache_key in _cache:
        print(f"[OCR] ⚡ Already processed {cache_key} — returning cached result (idempotent re-dispatch)")
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
    app.run(host="0.0.0.0", port=port, debug=False, use_reloader=False)
