# BACKLOG — unscheduled, not specification

Nothing here is promised. Nothing here may be cited as a reason the system behaves a certain way.
An item leaves this file only by becoming a ratified section of `SPEC.md`.

This file is deliberately **not** a final milestone. A milestone is scheduled work; labelling these
as one would quietly promise they get built.

---

## Capability extensions

| # | Item | Notes |
|---|---|---|
| B1 | **Fan-out** — several steps runnable at the same frontier position | An extension of the current model, not a replacement. The enabling change is one sentence: restate the recovery unit as *"the frontier is the set of steps at the maximum `seq`, and today that set always has exactly one member."* Deliberately **not** pre-committed in SPEC — no `group_index` column, not even the wording |
| B2 | **DAG** — steps with declared dependency edges | A new, independent, static-planner mode; effectively a different product. A real DAG is a graph declared up front, which contradicts this system's thesis of deciding one step at a time. Kept separate from B1 on purpose |
| B3 | **`async` + `raw` dispatch** | Mechanically blocked today: a raw body has nowhere to put `callback_url`, and supporting it would force the orchestrator to conform to each worker's own callback shape. The user's alternative is a small adapter |
| B4 | **`retry_after_seconds`** — a worker asks at runtime for a longer wait before the next attempt, e.g. after being rate-limited | The system would take the larger of this and the configured delay, never the smaller. An optional response-body field: it breaks no existing worker, touches no schema, touches no state machine |
| B5 | **Exponential backoff** on the retry delay | Competes with B4 for the same slot; decide them together |
| B6 | **Named worker registry** | Very low priority |
| B7 | **`GET /ui`** | Not cut, only deprioritised — terminal-first is the requirement, not terminal-only. May still be useful for demos |
| B15 | **`max_steps` — a ceiling on how many steps one run may accumulate** | A planner that answers `continue` forever produces a run that never terminates, and no existing budget catches it: budgets count failures, and such a planner is not failing. `SPEC.md § 19.2` publishes the unbounded length as a non-guarantee. An optional configuration field with a default of "no limit" breaks no existing workflow and touches no state machine, so by the §3 admission test it waits |

## Tuning and housekeeping

| # | Item | Notes |
|---|---|---|
| B8 | **Make the planner history cap configurable** | The cap is fixed at the most recent 100 steps. Making the number a setting is the backlog item; the cap itself ships in core |
| B16 | **Make the `error_text` truncation limit configurable** | Fixed at 4 KB in `SPEC.md § 6.4`, applied to `attempts.error_text` and `dead_letter_queue.error_text` alike. A user who wants longer diagnostics should eventually be able to raise it. Same shape as B8: the limit ships in core, the knob is the backlog item |
| B17 | **Make the sweep interval configurable** | Fixed at 5 s in `SPEC.md § 8.6`. It is already a value in the orchestrator's configuration file (§4.4); this item is about exposing it as a per-deployment setting with a documented range, since it trades failover latency against query rate |
| B9 | **`orchestrators` dead-row cleanup** | Rows accumulate because `orchestrator_id` is a fresh UUID per process boot. Not a correctness or performance problem — the table is only ever hit by primary-key point lookups — so it is pure disk housekeeping. `DELETE FROM orchestrators WHERE last_seen_at < now() - interval '7 days'`, with no ordering requirement now that there is no foreign key |
| B10 | **Retention / purge for terminal runs** | Not required for sweep correctness: with a partial index on running runs, terminal history costs nothing to skip. Worth offering eventually for disk usage only — never as something a user must do to keep the system healthy |

## Convenience

| # | Item | Notes |
|---|---|---|
| B11 | **Manual step re-run and manual output rewrite** | The hard part is not identity — it is truncating the frontier so later steps stop counting. The workable approach is `steps.superseded_at` plus excluding superseded steps from history and from `last_step`; deleting later steps is rejected outright because it destroys the audit trail. v2 core avoids the whole problem by supporting only "replay the step that is in DLQ", which needs no truncation |
| B12 | **Inline small outputs into the planner catalogue** | So that simple planners never need to fetch. This is the old project's 2 KB rule reborn — acceptable **only** as an optimisation, never as a requirement |
| B13 | **In-process fast path for async callbacks** | When a callback happens to land on the run's owner, hand it to the driver directly instead of waiting for the next poll. An optimisation only — the database path must remain the mechanism |
| B14 | **Structured logging** | Probably unnecessary: the database already holds the state. The decision taken instead was to keep error *text* in the database and stdout minimal |
| B18 | **Extract one shared test harness across milestone suites** | `test/gamma/harness` duplicates `test/alpha/harness`; they differ in which demo directory they point at. Not done at γ, deliberately: α's suite is the guard on a milestone the owner verified by hand, and refactoring it to serve a later milestone puts that guard at risk for convenience. The third milestone that wants the same plumbing is the point at which the trade turns — two copies is duplication, three is a pattern. **Update, δ (2026-09-08): there are now four copies** — `test/alpha`, `test/gamma`, `test/beta` and `test/delta` — so the condition this entry set for itself has been passed. Three of the four guard milestones the owner has verified by hand (R34-a, R36-a), which is the same reason not to touch them casually; the trade is now the owner's to call rather than one this entry can keep deferring |

---

## Explicitly rejected — do not re-propose without a new ruling

These were considered and eliminated. They are recorded here so they do not creep back in as
"obvious" additions.

| Item | Why it was rejected |
|---|---|
| **Worker heartbeat** (`POST /tasks/heartbeat`) | Dropped entirely, not merely deferred. It requires worker cooperation, and the persisted attempt deadline already provides the correctness backstop |
| **Progress payload on a heartbeat** | Dropped with the heartbeat |
| **Prometheus metrics** | Out of the blue and ambiguous for this system's actual needs |
| **`output_field`** — extracting one field of a worker response as the step output | Piton does not shape outputs. The whole response body is stored verbatim; reshaping is the planner's job |
| **Late-result salvage** | Once an attempt has been retried or the step has gone to DLQ, the old reply is refused by the attempt CAS. Permanently out |
| **`group_index` column** | Would buy a fan-out/fan-in barrier, not a DAG: every branch exactly one worker step deep, branches unable to depend on each other. Not worth pre-committing to. See B1 |
| **Authentication** | Out of scope |
| **`planner_history: inline \| fetch` workflow knob** | A setting nobody asked for that forces two code paths to be maintained forever |
| **`history_truncated` boolean field** | A sentence in the planner contract achieves the same expectation at zero cost |
| **Treating `input_from` inside `params` as anything but data** | `raw` mode's whole purpose is faithful transmission; it does not interpret payload content |
| **Postgres advisory locks for claiming** | Backend-specific — violates the storage-abstraction rule |
| **A load balancer or consistent hashing in front of the orchestrators** | Needs membership knowledge, and a dead replica's runs stay stranded until membership converges. Claiming *is* the load balancer |
| **An external queue** | A second source of truth |
| **Leader election** | No horizontal scale |
| **A stored `last_step` pointer** | It is a pure function of immutable data. A stale pointer would turn the combination table — the entire recovery design — into a lie, to save microseconds |
| **Two-phase planner conversation** | Forces the planner to be stateful, so every planner then needs its own crash story |
