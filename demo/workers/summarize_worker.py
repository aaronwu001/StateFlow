"""
Summarize Worker — sync, port 5003.

Receives POST /run from StateFlow (sync transport).
Body: {"workflow_input": {...}, "history": [...]}  (includes OCR and NER outputs)
Headers: X-StateFlow-Step-ID, X-StateFlow-Attempt-ID
Returns: summary JSON.

Idempotency: same as OCR (see ocr_worker.py) — keys primarily on the
X-StateFlow-Step-ID header (whitepaper §13.1, User Manual §2.3), falling back
to an input-content hash only if the header is absent.
"""

import json
import logging
import sys
import time

from flask import Flask, jsonify, request

app = Flask(__name__)
app.logger.disabled = True

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
        print(f"[SUMMARIZE] ⚡ Already processed {cache_key} — returning cached result (idempotent re-dispatch)")
        sys.stdout.flush()
        return jsonify(_cache[cache_key])

    history = body.get("history", [])
    step_names = [h["name"] for h in history]
    print(f"[SUMMARIZE] ✍️  Generating summary from history: {step_names}")
    print(f"[SUMMARIZE]    (sync, sleeping 2s to simulate LLM summarization)")
    sys.stdout.flush()

    time.sleep(2)

    # Pull entities from NER output if available (shows output chaining).
    entities = []
    for h in history:
        if h["name"] == "ner" and h.get("output"):
            out = h["output"]
            if isinstance(out, str):
                out = json.loads(out)
            entities = out.get("entities", [])

    summary_text = (
        f"Q3 2026 earnings report processed. "
        f"Key entities: {', '.join(entities) if entities else 'N/A'}. "
        f"Revenue growth confirmed strong."
    )

    result = {"summary": summary_text, "word_count": len(summary_text.split())}
    _cache[cache_key] = result

    print(f"[SUMMARIZE] ✅ Summary ready — {result['word_count']} words")
    sys.stdout.flush()
    return jsonify(result)


if __name__ == "__main__":
    port = 5003
    print(f"[SUMMARIZE] 🚀 Summarize Worker ready on :{port}  (sync — no callback needed)")
    sys.stdout.flush()
    log = logging.getLogger("werkzeug")
    log.setLevel(logging.ERROR)
    app.run(host="0.0.0.0", port=port, debug=False, use_reloader=False)
