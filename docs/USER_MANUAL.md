# StateFlow User Manual

This document covers the two things you must understand to use StateFlow with
a live agent or LLM planner:

1. [LLM Planner — Prompt Template & Contract](#1-llm-planner--prompt-template--contract)
2. [Worker Idempotency Contract](#2-worker-idempotency-contract)

---

## 1. LLM Planner — Prompt Template & Contract

StateFlow supports any HTTP endpoint as its "next-step planner" — including an
LLM you host behind a thin adapter.  This section gives you the system prompt
to paste into that adapter and the exact JSON contract your LLM must follow.

### 1.1 What StateFlow sends your planner

On every step, StateFlow POSTs to your planner URL with a JSON body:

```json
{
  "run_id": "run-abc-123",
  "workflow_input": { "doc": "report.pdf" },
  "history": [
    {
      "name":   "ocr",
      "status": "done",
      "output": { "pages": 3, "text": "..." }
    },
    {
      "name":   "ner",
      "status": "done",
      "output": { "entities": ["Alice", "Acme Corp"] }
    }
  ]
}
```

- `workflow_input` — the payload the caller passed when starting the run.
- `history` — every completed step in order, each with its full `output`.
  The history grows by one entry after each successful step.

### 1.2 What your planner must return

Your planner must respond with **exactly one JSON object and nothing else**
(no markdown fences, no explanatory prose):

**Continue** — dispatch the next step:

```json
{
  "status": "continue",
  "step": {
    "name":            "summarize",
    "worker_url":      "http://my-worker/summarize",
    "mode":            "sync",
    "timeout_seconds": 30,
    "input":           { "entities": ["Alice", "Acme Corp"] }
  }
}
```

**Done** — the run is complete:

```json
{ "status": "done" }
```

**Fail** — the run cannot proceed (routes to DLQ):

```json
{ "status": "fail" }
```

#### Required fields when `status = "continue"`

| Field | Required | Description |
|-------|----------|-------------|
| `step.name` | yes | Unique step name within the run |
| `step.worker_url` | yes | HTTP endpoint StateFlow will call |
| `step.mode` | yes | `"sync"` or `"async"` |
| `step.timeout_seconds` | recommended | Defaults to 30s if absent |
| `step.input` | optional | JSON payload forwarded to the worker |
| `step.output_field` | optional | For sync workers: extract one field as the step output (reduces context size for subsequent planner calls) |

### 1.3 Step modes

| Mode | Worker contract | Use when |
|------|----------------|----------|
| `sync` | StateFlow POSTs and holds the connection open; worker returns result in response body. | Worker responds quickly (<30s), or you cannot modify the worker. |
| `async` | StateFlow POSTs and expects **HTTP 202**; worker calls back later via `POST /tasks/complete`. | Long-running work (LLM inference, batch jobs, external APIs). |

### 1.4 System prompt template

Paste this into your LLM adapter's system prompt.  Customise the bracketed
sections for your domain:

---

```
You are a workflow planner for [DESCRIBE YOUR PIPELINE].

You will receive a JSON object with:
  - "workflow_input": the original task description
  - "history": the list of steps completed so far, each with "name", "status", and "output"

Your job: decide the NEXT step to run, or declare the workflow done or failed.

Available workers:
[LIST YOUR WORKERS AND WHAT THEY DO, e.g.:]
  - http://my-service/ocr     Extracts text from a PDF. Input: {"doc_url": "..."}
  - http://my-service/ner     Identifies named entities.  Input: {"text": "..."}
  - http://my-service/summarize  Summarises text + entities. Input: {"text": "...", "entities": [...]}

Rules you MUST follow:
1. Respond with ONLY a JSON object. No explanation, no markdown fences, no prose.
2. The JSON must have a "status" field: "continue", "done", or "fail".
3. If status is "continue", include a "step" object with:
   - "name": a unique name for this step
   - "worker_url": the worker's HTTP endpoint
   - "mode": "sync" (worker responds inline) or "async" (worker calls back later)
   - "timeout_seconds": how long to wait (default 30 for sync, 60 for async)
   - "input": the JSON payload the worker needs
4. If the workflow is complete, respond with {"status": "done"}.
5. If the workflow cannot proceed (unrecoverable error), respond with {"status": "fail"}.
6. Do not repeat a step that appears in "history" with status "done".

Example response when starting step "ocr":
{"status":"continue","step":{"name":"ocr","worker_url":"http://my-service/ocr","mode":"sync","timeout_seconds":30,"input":{"doc_url":"https://example.com/report.pdf"}}}

Example response when all steps are done:
{"status":"done"}
```

---

### 1.5 Acceptance criteria (output validation)

StateFlow validates every planner response.  Responses that fail any check are
**retried up to 2 times** (3 attempts total, 30s timeout each).  If all attempts
fail, the run is routed to the DLQ with `reason = planner_failed`.

| Check | Failure examples |
|-------|-----------------|
| Valid JSON | `json: cannot unmarshal...` |
| `status` field present | `{}` or `{"decision":"continue"}` |
| No trailing content | `{"status":"done"} Here's my reasoning...` |
| No markdown fences | ` ```json\n{"status":"done"}\n``` ` |
| `step.worker_url` present when `status=continue` | `{"status":"continue","step":{"name":"x"}}` |
| `step.mode` present when `status=continue` | `{"status":"continue","step":{"worker_url":"..."}}` |

**Common failure mode**: LLMs often wrap responses in markdown code fences or
append a brief explanation.  Your adapter must strip these before returning the
response to StateFlow.  The simplest approach: extract the first JSON object
from the response using a regex, then validate it.

### 1.6 Wiring an HTTP planner

Create a workflow with `planner_type = "http"`:

```bash
curl -X POST http://localhost:8080/workflows \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "my-llm-pipeline",
    "planner_type": "http",
    "planner_config": {
      "url": "http://my-llm-adapter/decide"
    }
  }'
```

`planner_config` fields:

| Field | Default | Description |
|-------|---------|-------------|
| `url` | required | Full URL of your planner endpoint |
| `timeout_seconds` | 30 | Per-attempt timeout |
| `max_retries` | 2 | Additional attempts on failure (3 total) |

### 1.7 Example LLM adapter (Python, 30 lines)

```python
from flask import Flask, request, jsonify
import anthropic, re, json

app   = Flask(__name__)
client = anthropic.Anthropic()   # reads ANTHROPIC_API_KEY from env

SYSTEM_PROMPT = """...(paste §1.4 template here)..."""

@app.route("/decide", methods=["POST"])
def decide():
    run_state = request.get_json()
    msg = client.messages.create(
        model="claude-sonnet-4-6",
        max_tokens=512,
        system=SYSTEM_PROMPT,
        messages=[{"role": "user", "content": json.dumps(run_state)}],
    )
    raw = msg.content[0].text
    # Strip markdown fences if the LLM added them.
    m = re.search(r'\{.*\}', raw, re.DOTALL)
    decision = json.loads(m.group()) if m else json.loads(raw)
    return jsonify(decision)

if __name__ == "__main__":
    app.run(port=9000)
```

Then point the workflow at `http://localhost:9000/decide`.

---

## 2. Worker Idempotency Contract

### 2.1 StateFlow guarantees at-least-once, not exactly-once

**This is the most important thing to understand.**

StateFlow checkpoints every step result before advancing.  However, it cannot
guarantee that a worker runs *exactly* once:

- **Sync crash:** the orchestrator calls a sync worker and holds the connection.
  If the orchestrator crashes while waiting, the worker may have already
  completed — but StateFlow lost the response.  On restart, StateFlow sees the
  step as `RUNNING` with no recorded output and **re-dispatches it**.  The
  worker may execute twice.

- **Async re-dispatch:** if the orchestrator crashes after dispatching an async
  worker (and before the callback arrives), it re-dispatches the same step on
  restart with a new `attempt_id`.  A fast worker that already called back will
  have its callback rejected (superseded `attempt_id`); a slow worker will be
  called a second time.

**Workers must be idempotent.  This is the client's responsibility, not
StateFlow's.**  StateFlow provides the tools to make this straightforward.

### 2.2 What StateFlow provides

On every dispatch, StateFlow includes two stable identifiers:

| Identifier | Scope | Value |
|-----------|-------|-------|
| `step_id` | constant across all retries of a step | `"{run_id}:{step_name}"` |
| `attempt_id` | new UUID every dispatch | fresh UUID per dispatch |

**For async workers**, both identifiers arrive in the POST body:

```json
{
  "step_id":    "run-abc-123:ocr",
  "attempt_id": "550e8400-e29b-41d4-a716-446655440000",
  "input":      { "doc": "report.pdf" }
}
```

**For sync workers**, StateFlow sends only `step.input` as the POST body (the
worker need not know about StateFlow's internals).  Sync workers can derive an
idempotency key from the input content itself.

### 2.3 How to implement idempotency

#### Async workers (recommended: use `step_id`)

```python
_cache = {}   # step_id -> result  (use Redis or DB in production)

@app.route("/run", methods=["POST"])
def run():
    body       = request.get_json()
    step_id    = body["step_id"]
    attempt_id = body["attempt_id"]

    if step_id in _cache:
        # Already did this work.  Re-send the callback with the NEW attempt_id.
        threading.Thread(
            target=callback, args=(step_id, attempt_id, _cache[step_id])
        ).start()
        return jsonify({"accepted": True, "idempotent": True}), 202

    # Fresh execution.
    threading.Thread(target=process, args=(step_id, attempt_id, body["input"])).start()
    return jsonify({"accepted": True}), 202
```

Key point: use `step_id` (constant across retries) as the cache key, not
`attempt_id` (which changes each dispatch).

#### Sync workers (use input hash or external key)

```python
import hashlib, json

_cache = {}

@app.route("/run", methods=["POST"])
def run():
    body = request.get_json()
    key  = hashlib.sha256(json.dumps(body, sort_keys=True).encode()).hexdigest()

    if key in _cache:
        return jsonify(_cache[key])   # return previous result

    result = do_work(body)
    _cache[key] = result
    return jsonify(result)
```

For sync workers, StateFlow passes `workflow_input + history` as-is, so the
input is deterministic for a given step position — hashing it is a reliable
idempotency key **as long as your input has no non-deterministic fields**
(timestamps, random IDs, floating-point ordering, etc.).  If the input can
vary across retries, prefer an external stable key (e.g., derive one from
`workflow_input` alone, or switch to async mode where `step_id` is available).

The async pattern (using `step_id`) is more robust and is recommended whenever
you can modify the worker.

### 2.4 Production recommendations

| Concern | Recommendation |
|---------|---------------|
| In-memory cache lost on restart | Use Redis or your DB; key on `step_id` |
| Cache entry never expires | Set TTL ≥ `max_run_duration + retry_delay` |
| Expensive work already started | Checkpoint partial results; skip on re-dispatch |
| External API without idempotency | Pass `step_id` as your own idempotency header (e.g. `Idempotency-Key`) |
| Database writes | Upsert on a natural key derived from the input; or check-then-insert in a transaction |

### 2.5 Why not exactly-once?

Exactly-once delivery across arbitrary external HTTP workers requires
distributed transactions or two-phase commit, which contradicts StateFlow's
lightweight positioning and would force workers to implement a protocol.
At-least-once with idempotent workers is the industry-standard trade-off
(used by Kafka, SQS, Temporal, and all major durable-execution systems).
The contract is explicit and the tools are provided — nothing is hidden.

### 2.6 The dedup guard for superseded callbacks

When StateFlow re-dispatches a step with a new `attempt_id`, it updates
`current_attempt_id` in the database.  If the old worker's callback arrives
late (carrying the old `attempt_id`), the API handler detects the mismatch
and acknowledges it with HTTP 200 — but **ignores it**.  The run state is
not corrupted.  You will see this in the logs:

```
INFO callback: superseded attempt_id, ignoring  step_id=...  attempt_id=...
```

This is the expected, correct behaviour — not an error.
