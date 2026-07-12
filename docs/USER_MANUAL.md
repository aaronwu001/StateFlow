# StateFlow User Manual

This document covers what you must understand to run a live agent or LLM
planner on StateFlow, and to write workers that survive its at-least-once
delivery model.

1. [LLM Planner — Prompt Template & Contract](#1-llm-planner--prompt-template--contract)
2. [Worker Idempotency Contract](#2-worker-idempotency-contract)
3. [The DLQ: Reasons & Triage](#3-the-dlq-reasons--triage)

> Authoritative design: `docs/StateFlow_Whitepaper_v1_0.md`. This manual is an
> operator-facing derivative of it — on any conflict, the whitepaper governs.

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
      "status": "DONE",
      "output": { "pages": 3, "text": "..." }
    },
    {
      "name":   "ner",
      "status": "DONE",
      "output": { "entities": ["Alice", "Acme Corp"] }
    }
  ]
}
```

- `workflow_input` — the payload the caller passed when starting the run.
- `history` — every completed step in order (by `seq`, never by timestamp or
  name). The history grows by one entry after each successful step. As of
  Phase 2, a given entry's `output` is not guaranteed to be the step's full,
  unmodified output: any single entry over 2KB (marshaled) is replaced with a
  small pointer object (`_truncated`/`size_bytes`/a note pointing at
  `GET /runs/{run_id}` for the full value); on long runs, the oldest entries
  whose output no longer fits a 50KB total budget carry only `name`+`status`,
  with `output` omitted entirely. Nothing changes about what's stored — the
  full output is always available via `GET /runs/{run_id}` — only what's
  sent to the planner on a given `Decide` call is bounded this way.
- **Every status string on the wire is UPPERCASE** (`"DONE"`), identical to
  the value stored in the database. This is binding, not a convention — do
  not match on `"done"` (lowercase) when reading `history`.

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

**Fail** — the run cannot proceed (routes to DLQ with reason
`planner_declared_fail` — this is a legitimate answer, not an error; see §3):

```json
{ "status": "fail" }
```

> Note: `status` on this side of the wire (the planner's own verdict) is
> **lowercase** (`continue`/`done`/`fail`) — a different field with a
> different casing rule than the `history[].status` values you receive
> (§1.1, always UPPERCASE). Do not conflate the two.

#### Required fields when `status = "continue"`

| Field | Required | Description |
|-------|----------|-------------|
| `step.name` | yes | Unique step name within the run |
| `step.worker_url` | yes | HTTP endpoint StateFlow will call |
| `step.mode` | yes | `"sync"` or `"async"` |
| `step.timeout_seconds` | optional | This attempt's lifetime ceiling. `0` or absent ⇒ inherit the workflow's `default_timeout_seconds` (§1.6) ⇒ inherit the system default of **60s** if that is also unset. See §1.3 |
| `step.input` | optional | JSON payload forwarded to the worker |
| `step.output_field` | optional | For sync workers: extract one field as the step output (reduces context size for subsequent planner calls) |

### 1.3 Step modes and the timeout doctrine

| Mode | Worker contract | Use when |
|------|----------------|----------|
| `sync` | StateFlow POSTs and holds the connection open; worker returns result in response body. | Worker responds quickly, or you cannot modify the worker. |
| `async` | StateFlow POSTs and expects **HTTP 202**; worker calls back later via `POST /tasks/complete`. | Long-running work (LLM inference, batch jobs, external APIs). |

**Timeout is one knob with one resolution order, the same for both modes** —
there is no separate "sync default" or "async default":

```
step.timeout_seconds (StepDecision)  >  workflow's default_timeout_seconds  >  60s (system default)
```

The clock starts when the attempt is **persisted** (Barrier 1 — before
dispatch), not when the worker actually receives the request, so the span
"decision exists, result doesn't" is covered end to end by one rule. An
overdue attempt is pronounced `failed(timeout)` — timeout is a failure like
any other and consumes the retry budget (whitepaper §6). Set your timeout
generously above your worker's real-world latency: too short mis-kills (and,
for a billed worker, double-bills) long tasks; too long delays failure
detection. A sync worker sitting behind a load balancer or reverse proxy may
additionally be cut at the proxy's own idle-connection limit (commonly
30–90s) regardless of what you set here — long tasks belong in `async` mode.

### 1.4 System prompt template

Paste this into your LLM adapter's system prompt.  Customise the bracketed
sections for your domain:

---

```
You are a workflow planner for [DESCRIBE YOUR PIPELINE].

You will receive a JSON object with:
  - "workflow_input": the original task description
  - "history": the list of steps completed so far, each with "name", "status" (UPPERCASE, e.g. "DONE"), and "output"

Your job: decide the NEXT step to run, or declare the workflow done or failed.

Available workers:
[LIST YOUR WORKERS AND WHAT THEY DO, e.g.:]
  - http://my-service/ocr     Extracts text from a PDF. Input: {"doc_url": "..."}
  - http://my-service/ner     Identifies named entities.  Input: {"text": "..."}
  - http://my-service/summarize  Summarises text + entities. Input: {"text": "...", "entities": [...]}

Rules you MUST follow:
1. Respond with ONLY a JSON object. No explanation, no markdown fences, no prose.
2. The JSON must have a "status" field: "continue", "done", or "fail" (lowercase).
3. If status is "continue", include a "step" object with:
   - "name": a unique name for this step
   - "worker_url": the worker's HTTP endpoint
   - "mode": "sync" (worker responds inline) or "async" (worker calls back later)
   - "timeout_seconds": how long this attempt may run before it is treated as failed
     (optional — 0 or omitted inherits the workflow's configured default, or 60s
     if the workflow sets none; always prefer setting this explicitly for
     anything slower than a few seconds)
   - "input": the JSON payload the worker needs
4. If the workflow is complete, respond with {"status": "done"}.
5. If the workflow cannot proceed (unrecoverable error), respond with {"status": "fail"}.
   Only use this when the task itself is unworkable (e.g. invalid input) —
   not for a worker being temporarily down, which StateFlow retries on its own.
6. Do not repeat a step whose "name" appears in "history" with status "DONE" (uppercase).

Example response when starting step "ocr":
{"status":"continue","step":{"name":"ocr","worker_url":"http://my-service/ocr","mode":"sync","timeout_seconds":30,"input":{"doc_url":"https://example.com/report.pdf"}}}

Example response when all steps are done:
{"status":"done"}
```

---

### 1.5 Acceptance criteria (output validation)

StateFlow validates every planner response against the criteria below. A
response that fails any check, or a planner call that times out or cannot be
reached, is retried — **3 total attempts, a 30s deadline per call** (this
budget is fixed by the orchestrator and is not configurable via
`planner_config`). Each failed attempt is classified `unreachable` (no valid
response at all — dial error or deadline exceeded) or `malformed` (a response
arrived but fails the checks below); the two share one budget. If all 3
attempts fail, the run is routed to the DLQ with reason `planner_unreachable`
or `planner_malformed` — whichever category the **final** attempt fell into
(every attempt's detail is preserved in the DLQ entry's `context`). See §3 for
the full DLQ reason set and triage guidance.

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
      "url": "http://my-llm-adapter/decide",
      "retry_limit": 3,
      "default_timeout_seconds": 60
    }
  }'
```

`planner_config` is one JSONB blob; which keys apply depends on `planner_type`,
except `retry_limit` and `default_timeout_seconds`, which are workflow-level
settings and apply regardless of planner type (`static` or `http`):

| Field | Applies to | Default | Description |
|-------|-----------|---------|-------------|
| `url` | `http` only | required | Full URL of your planner endpoint |
| `steps` | `static` only | required | The fixed step list (see the static-planner YAML format) |
| `retry_limit` | any | `3` | The worker-side retry budget **X** (whitepaper §7.1): a step's `(X+1)`th attempt failure routes the step (and the run) to the DLQ instead of retrying again |
| `default_timeout_seconds` | any | unset → system default `60` | Workflow-level attempt-timeout override; a step's own `timeout_seconds` (§1.2) overrides this in turn |

There is no `max_retries` or per-planner `timeout_seconds` key: the planner's
own call budget (30s × 3 attempts, §1.5) is fixed by the orchestrator, not
user-configurable in the MVP.

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
  step as `RUNNING` with no recorded output, claims the dead attempt
  (`failure_reason=orphaned`), and **re-dispatches it**.  The worker may
  execute twice.

- **Async re-dispatch:** if the orchestrator crashes after dispatching an async
  worker (and before the callback arrives), it re-dispatches the same step on
  restart with a new `attempt_id`.  A fast worker that already called back will
  have its callback rejected (superseded `attempt_id`); a slow worker will be
  called a second time.

- **Timeout re-dispatch — not just crashes:** the same duplication can happen
  with *no* orchestrator restart at all. If an attempt is slow enough to cross
  its configured timeout, StateFlow pronounces it `failed(timeout)` and
  dispatches a new attempt — even though the original worker call may still be
  running and about to succeed. This is why the concurrency bound in §2.2 is
  not merely a crash-recovery footnote: it is a routine, expected condition
  any time a worker's real latency creeps close to its configured timeout.

**Workers must be idempotent.  This is the client's responsibility, not
StateFlow's.**  StateFlow provides the tools to make this straightforward.

### 2.2 Quantified concurrency: how many duplicate calls to expect

Precisely stated (whitepaper §15, "Operator's Contract"):

> One step produces at most **X** attempts per round (X = `retry_limit`, §1.6;
> default 3), where a "round" is the span between the step's first attempt and
> either its success, its DLQ verdict, or a replay reset. In the worst case —
> every failure is a *timeout mis-kill* while the previous execution is still
> alive and about to complete — **up to X concurrent duplicate invocations of
> the same `step_id` may be running simultaneously.**

This is a hard requirement on your deduplication, not a rare edge case:

- Your dedup mechanism (recommended: a lock or upsert keyed on `step_id`, §2.3)
  **must withstand X-way concurrency**, not merely sequential duplicates
  arriving one after another.
- **The responsibility boundary is explicit:** if the `retry_limit` you
  configure exceeds what your worker's deduplication can actually withstand,
  the resulting duplicate side effects or data corruption are on your side,
  not StateFlow's.
- StateFlow's side of the bargain is unconditional: stale/superseded reports
  are always blocked by CAS (§2.6) before they can touch persisted state — the
  database itself is never polluted by a superseded execution, no matter how
  many concurrent duplicates your worker allows to run.

### 2.3 What StateFlow provides

On every dispatch, StateFlow includes two stable identifiers:

| Identifier | Scope | Value |
|-----------|-------|-------|
| `step_id` | constant across all retries of a step | `"{run_id}:{step_name}"` |
| `attempt_id` | new UUID every dispatch | fresh UUID per dispatch |

**For async workers**, both identifiers arrive in the POST body (the envelope
is unchanged from prior versions):

```json
{
  "step_id":    "run-abc-123:ocr",
  "attempt_id": "550e8400-e29b-41d4-a716-446655440000",
  "input":      { "doc": "report.pdf" }
}
```

**For sync workers**, the POST body is the **bare `input`** — nothing else —
so an unmodifiable external API can consume the call as-is (StateFlow's
zero-modification promise for sync). The two identifiers instead travel as
headers on the same request:

```
POST {worker_url}
X-StateFlow-Step-ID:    run-abc-123:ocr
X-StateFlow-Attempt-ID: 550e8400-e29b-41d4-a716-446655440000

{ "doc": "report.pdf" }
```

A worker that cannot be modified to read headers simply ignores them (they're
unknown headers to it) and still works. A worker you *can* modify should read
`X-StateFlow-Step-ID` and use it as its idempotency key — see §2.4.

### 2.4 How to implement idempotency

#### Async workers (use `step_id` from the body)

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

Key point: use `step_id` (constant across retries) as the cache key, never
`attempt_id` (which changes each dispatch).

#### Sync workers (recommended: the `X-StateFlow-Step-ID` header)

Because sync's POST body is the bare input with no wrapper, a modifiable sync
worker should read the step identity from the header rather than the body:

```python
_cache = {}   # step_id -> result

@app.route("/run", methods=["POST"])
def run():
    body    = request.get_json()
    step_id = request.headers.get("X-StateFlow-Step-ID")

    if step_id and step_id in _cache:
        return jsonify(_cache[step_id])   # return previous result

    result = do_work(body)
    if step_id:
        _cache[step_id] = result
    return jsonify(result)
```

This is precise regardless of what the input looks like — no hashing, no
"identical bytes across retries" assumption. **Prefer this over hashing the
input** whenever your sync worker can read request headers.

#### Sync workers that cannot read headers (fallback: input hash)

Some sync targets are opaque (an external SaaS endpoint you call through a
thin shim, or a worker you truly cannot modify at all). If you control the
shim but not the ability to read StateFlow's headers through it, fall back to
hashing the input body:

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

This is a **fallback, not the recommendation**: it works only as long as the
input has no non-deterministic fields (timestamps, random IDs,
floating-point/set ordering, etc.) — because if the input can vary at all
across retries for the "same" logical step, the hash silently stops
deduplicating. If you can modify the worker at all, read the header instead
(previous subsection); if you can modify it enough to add a header read, you
can also switch to `async` mode, where `step_id` travels in-band in the body.

### 2.5 Production recommendations

| Concern | Recommendation |
|---------|---------------|
| In-memory cache lost on restart | Use Redis or your DB; key on `step_id` |
| Cache entry never expires | Set TTL ≥ `max_run_duration + retry_delay` |
| Expensive work already started | Checkpoint partial results; skip on re-dispatch |
| External API without idempotency | Pass `step_id` as your own idempotency header (e.g. `Idempotency-Key`) |
| Database writes | Upsert on a natural key derived from the input; or check-then-insert in a transaction |
| Concurrency withstand target | Size your dedup mechanism for **X concurrent** duplicates (§2.2), not just sequential ones |

### 2.6 Why not exactly-once?

Exactly-once delivery across arbitrary external HTTP workers requires
distributed transactions or two-phase commit, which contradicts StateFlow's
lightweight positioning and would force workers to implement a protocol.
At-least-once with idempotent workers is the industry-standard trade-off
(used by Kafka, SQS, Temporal, and all major durable-execution systems).
The contract is explicit and the tools are provided — nothing is hidden.

### 2.7 The dedup guard for superseded and late reports

When StateFlow re-dispatches a step with a new `attempt_id`, it updates
`current_attempt_id` in the database. Every report — a sync response or an
async callback — is accepted only if it passes a single atomic check
(**CAS-A**): `attempt_id` must equal the step's current `current_attempt_id`
**and** that attempt must still be `RUNNING`. If either condition fails, the
report is ACKed with HTTP 200 but has **zero effect on stored state**. This
path is currently silent — no log line is emitted for it — so do not grep
logs for a "superseded"/"ignoring" message to detect it; instead confirm the
response is 200 with no corresponding change in attempt/step state (e.g. via
`GET /runs/{id}`), or query attempt history directly.

This is the expected, correct behaviour — not an error. Two situations both
land here:

1. **Superseded by re-dispatch.** The old worker's callback arrives late,
   carrying an `attempt_id` that a newer attempt has already replaced.
2. **A success arriving after a timeout verdict.** If an attempt's timeout
   fires first — the orchestrator pronounces it `failed(timeout)` and (if
   budget allows) has already started a new attempt — a success report for
   the *original* attempt that arrives afterward is **rejected**, even though
   the work genuinely succeeded. StateFlow does not resurrect a
   failed attempt into a done one: by the time the late success lands,
   CAS-A's `status = RUNNING` condition on that attempt no longer holds. The
   cost is one redundant execution per mis-kill (mitigated by generous
   timeouts, §1.3, and by worker idempotency, this section) — deliberately
   accepted in the MVP rather than reopening every invariant to support a
   failed→done resurrection transition.

In both cases the run state is never corrupted: at most one attempt per step
is ever `RUNNING` at a time from StateFlow's point of view, and only its
report can still take effect.

---

## 3. The DLQ: Reasons & Triage

The DLQ (`GET /dlq`, `POST /dlq/{id}/replay`) is a **human queue, not a
discard pile**. Every entry carries one of four reasons — purely informational
(replay mechanics are identical for all four) but essential for deciding
*what to fix* before you replay:

| Reason | Which side | Meaning | Operator action |
|--------|-----------|---------|------------------|
| `worker_retry_exhausted` | Worker | The step's attempts hit the retry budget X (`retry_limit`, §1.6) without success | Read the per-attempt reasons in the DLQ entry's `context` (`worker_reported` / `timeout` / `malformed` / `orphaned` — whitepaper §4.2); fix the worker or the step's timeout; replay |
| `planner_unreachable` | Planner | The planner endpoint could not be reached within 3 attempts (dial error / 30s deadline) | Fix planner connectivity/deployment; replay |
| `planner_malformed` | Planner | The planner responded, but its output failed the §1.5 acceptance criteria on all 3 attempts | Fix the planner's output format (LLM case: the prompt has drifted off-template); replay |
| `planner_declared_fail` | Planner | **The planner explicitly answered `{"status":"fail"}`** — a legitimate verdict, not an error; does not consume any retry budget | **Do not blindly replay.** Change the input data or the planner's logic first — replaying unchanged will most likely reproduce the identical verdict and burn cost for nothing |

The fourth reason is why the field exists at all: without it, an operator
cannot tell "the planner is broken" apart from "the planner looked at this
specific input and judged the task itself unworkable."

**Replay** (`POST /dlq/{id}/replay`) branches automatically on which side
failed:

- **Worker-side** (`worker_retry_exhausted`): resets the step's retry budget
  to 0, creates a fresh attempt, and re-dispatches the worker. Without this
  reset the count would already equal X and the step would return to DLQ
  instantly — replay would be decorative.
- **Planner-side** (the three `planner_*` reasons): re-asks the planner with
  the run's current history. For `planner_declared_fail` specifically, replay
  only makes sense once you have actually changed something — see the table
  above.

A run or step that finishes `DONE` is never labeled, flagged, or annotated by
any of this — a DLQ reason only ever exists on an entry in the
`dead_letter_queue` table, joined in by `GET /runs/{run_id}` as `dlq_reason`
when (and only when) that run's status is `DLQ`.
