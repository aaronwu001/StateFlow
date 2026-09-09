# StateFlow v2 — Grilling Log

Purpose: capture the owner's rulings that will produce `CLAUDE.md` + the new minimal spec.
Format: one round per section. Question → owner's ruling → consequence.
Nothing here is spec. Once a ruling lands in `CLAUDE.md` or the spec, it stays here only as rationale.

Started: 2026-08-08

---

## Round 0 — What I read (evidence, not claims)

Inventory of `./docs` (3,800 lines, 10 files):

| File | Lines | Self-declared status | Reality |
|---|---|---|---|
| `StateFlow_Whitepaper_v1_0.md` | 610 | "Authoritative design as of v1.0" | Lags implementation in ~12 places |
| `WHITEPAPER_V1_1_PATCHES.md` | 249 | Patch list to be merged into the above | Never merged; CN-language |
| `BEHAVIOR_MATRIX.md` | 398 | "Authoritative behavior spec, wins all conflicts" | ~150 assertion rows, CN-language, references `spec/` path that does not exist here |
| `StateFlow_Rules_Consolidation_v3_EN.md` | 334 | "Reference, not authority" (was authority in v3) | Stale; 5 ratified changes missing |
| `CLAUDE-old.md` | 344 | Development discipline | Contradicts BEHAVIOR_MATRIX on `retry_limit` location |
| `BACKLOG.md` | 43 | "Not yet true" | 16 whitepaper repairs owed + 9 future items |
| `OPERATIONAL_FACTS.md` | 826 | How to run it; no behavior | Environment-specific, dated 2026-08-03 |
| `USER_MANUAL.md` | 549 | User-facing | Not read in full yet |
| `README.md` | 289 | Public README | Contains a 4th copy of the API table |
| `HANDOFF_23AB.md` | 158 | Session handoff | Template with `<<< >>>` placeholders unfilled |

**Structural finding:** there are four separate registries for "things not settled" —
whitepaper §18 (Temporary Design Registry), BEHAVIOR_MATRIX §K (explicit non-guarantees),
BEHAVIOR_MATRIX §M (裁定紀錄 / rulings), and `BACKLOG.md`. Plus §21 (Roadmap).
Any single question ("is X guaranteed?") can be answered differently by four documents.
This — not the technical content — is the actual defect.

**Second structural finding:** the API surface is stated in full in 4 places
(whitepaper §12–13, CLAUDE-old, README, USER_MANUAL) with drift between them.

**Third finding:** `/home/aaronwu/stateflowv2` contains **only** `docs/`. It is not a git
repo. There is no code and no tests here. The referenced implementation lives elsewhere
(`~/Projects/StateFlow` per OPERATIONAL_FACTS §0), and `spec/BEHAVIOR_MATRIX.md` — the
path every doc calls authoritative — does not exist at that path in this tree.

---

## Round 1 — Rulings (2026-08-08)

| # | Question | Ruling | Consequence |
|---|---|---|---|
| 1 | Rewrite vs. continue old repo | **Full rewrite.** Reason given: "so that I can understand it" | New repo at `/home/aaronwu/stateflowv2`. No code is ported by copy-paste |
| 2 | Old repo access | **Readable at `~/Projects/StateFlow`, reference only.** Explicit warning: do not let it hallucinate us into its assumptions — **especially when writing tests**; tests must be derived from SPEC, never from reading old test files | Reading old impl for "how they did X in Go" is allowed; reading old tests to write new tests is not |
| 3 | Primary audience | **Learning + demonstrating the ability to independently design and build under distributed-systems assumptions.** Rationale is therefore first-class, not decoration | Every non-obvious rule in SPEC carries a `Why:` |
| 4 | File set | **`SPEC.md` + `CLAUDE.md`, nothing else.** Open problem raised by owner: the grilling log must not become a third source of confusion — needs a hard separation mechanism | Answered in Round 2 Q1 |
| 5 | Old BEHAVIOR_MATRIX's ~150 rows | **Do not inherit.** Re-derive a minimal set. Conflicts with other docs are surfaced and ruled one by one. **Design principle: start from the simplest thing that works; expand only when a concrete need appears — do not treat every old feature as a parallel requirement.** Shipping something with small known bugs earlier beats shipping something complete later | Feature scope is subtractive by default. The burden of proof is on including something |
| 6 | Where assumptions live | **`SPEC.md § Open Assumptions`** for unsettled/unguaranteed things; **`CLAUDE.md`** for future work | Four old registries (§18, §K, §M, BACKLOG) collapse into two locations |
| 7 | Unit of "one feature" | **Not TX — a user-visible capability, demoable end to end.** Owner's own ordering: (1) basic happy path runs; (2) kill a worker → see retries → see it land in DLQ; (3) replay works in its variants; (4) static vs. custom planner configuration and switching; … Complexity like input-field validation is pushed to the back deliberately | Milestones = demo scenarios, not layers. See Round 2 Q3 |
| 8 | Old ceremony (read-only spec, sha256 freeze, blind-test sessions, handoff docs) | **All cut.** What matters is features landing. Test discipline reduces to: tests must not be written by reading the code. **New hard requirement: the owner must be able to run the system by hand and see inside it — database state and, ideally, other runtime state — not merely watch an automated suite pass.** Automated tests exist to guarantee that what he sees by hand is the expected behavior | Observability-for-the-operator becomes a real requirement, not a Phase-2 nicety |
| 9 | Who may edit SPEC | **Owner only, on an explicit ruling.** Claude proposes; Claude never edits | The one piece of ceremony that survives |
| 10 | Tech stack | **Unchanged: Go + Postgres + Docker Compose + golang-migrate** | Not a source of the problem |

---

## Round 2 — Rulings (2026-08-08)

| # | Question | Ruling |
|---|---|---|
| 1 | Separation mechanism for the three files | **Split by voice, not by topic.** `SPEC.md` = "the system MUST…" + `Why:`. `CLAUDE.md` = "you (Claude) MUST…" + milestones + future work. `notes/` = "what we discussed and decided". **Amendment by owner: do not bother deleting superseded log entries — the only rule that matters is that `notes/` may never be cited as justification.** Deleting is maintenance cost for no benefit |
| 2 | Old `./docs` | **Move to `legacy/` + `legacy/README.md` marking it VOID.** Reference only: read it for "how they thought", never for "so we do the same" |
| 3 | Milestone list | **Not settled.** Owner's list was an example, not an inventory. Requires a full feature brainstorm first (Round 3). **Explicit owner request: the run × last_step combination table and its recovery rules — his own design, which he likes — must be reviewed and settled BEFORE any implementation starts, because it is where the edge cases live. If it audits clean, adopt it wholesale** |
| 4 | Async priority | **Async is not deferrable in design, only in implementation.** It determines the practicality of the product and is the source of most edge cases. Sync-first is acceptable **only if** every message carries an explicit `connection_mode` field and the spec's field formats are defined per mode from day one — otherwise sync assumptions bake in and later async work forces a format break |
| 5 | Operator observability | **Terminal access to database truth. No UI.** A web page adds complexity for no gain here |
| 6 | `step_id` | **UUID is the identity. `step_name` is a non-unique, optional display label** — a lazy planner may omit it entirely |
| 7 | Message shapes / who assembles worker input | **Open — the hardest design question in this project.** Owner's position, in his words: StateFlow stores everything; the planner decides what context flows onward; no LLM should see more than it needs; StateFlow itself should not be in the business of shaping outputs. Unresolved sub-question he raised himself: what does the planner receive after a worker finishes? Requires a concrete proposal (Round 3) |
| 8a | `output_field` | Depends on Q7 |
| 8b | history size limits | **Required eventually, low priority** |
| 8c | retry mechanics | **Implement later, but put the columns and structures in the schema from the start.** Early milestones demo with `retry_limit = 0` (no retry) rather than with a missing field |
| 8d | per-step `retry_limit` / `timeout` override | **A selling point — mid priority.** After core recovery, not at the very end |
| 8e | in-process sweeper | **Deferred pending explanation** — owner does not remember how it differs from recovery; re-derive from the old docs and re-decide |
| 8f | late-result salvage | **Permanently out.** Once retried or DLQ'd, the old reply is rejected by CAS on `step_id` + `attempt_id` |
| 8g | multi-replica | **Must be possible later — a selling point**, because the orchestrator is stateless: partition workflows across orchestrators, all writing to one DB. Do not design anything that forecloses it |
| 8h | **NEW: storage abstraction layer** | **Required from the start.** One relational backend today (Postgres), but the interface must not assume JSONB or any Postgres-specific type. A future implementer on another relational DB must be able to choose their own encoding |
| 8i | auth | Out of scope |
| 9 | Config layering | Workflow-level config set at creation. **Run-level override: designed-in now (the run API accepts parameters), unused initially.** Step-level override: same pattern, low priority. **The planner gets its own retry limit and timeout, configured at the workflow level under clearer names** — these determine when a run goes to DLQ because of the planner rather than a worker |
| 10 | Project name | **Rename.** The old StateFlow repo gets a deprecation notice pointing at the new one. Need an unused name meaning: lightweight, general-purpose orchestration engine, quick for teams to adopt, and it remembers every step in a plain relational schema |

---

## Round 3 — Rulings (2026-08-09)

### Settled

| # | Topic | Ruling |
|---|---|---|
| R3-1 | Combination-table dependencies 2 & 3 (seq assigned by one writer; every state write is exactly one transaction) | **Accepted as load-bearing.** They must appear prominently in SPEC, not buried |
| R3-2 | Reserved lease columns on `runs` | **Accepted** — add now, implement later. Follow-up question answered in Round 4: nullable, not required |
| R3-3 | Sync worker receiving a **raw** (un-enveloped) body | **NOT cut.** Owner considers "any unmodifiable HTTP endpoint is a valid worker" a major selling point. It becomes an add-on with relatively high priority, behind a per-step `dispatch_style` field that is designed in from day one |
| R3-4 | Exponential backoff | Later, low priority. Competes with worker-requested retry delay for the same slot |
| R3-5 | Named worker registry | Very low priority |
| R3-6 | `GET /ui` | **Not cut, only deprioritized.** May still be useful for demos. Terminal-first is the requirement, not terminal-only |
| R3-7 | Structured logging | **Probably unnecessary** — the database already holds the state. (Round 4 refines: keep error *text* in the DB, keep stdout minimal) |
| R3-8 | Prometheus metrics | **Dropped.** Owner: out of the blue and ambiguous. Claude over-reached |
| R3-9 | Run cancellation | **In.** Introduce `CANCELLED` and nothing else. Kill the driver. If last step was RUNNING it becomes CANCELLED; if it was DONE it stays DONE. Result: `run=CANCELLED, last_step=CANCELLED|DONE`. **Only a RUNNING run may be cancelled** |
| R3-10 | Sweeper now + reserved columns | **Agreed** (superseded by Round 4's stronger proposal — see R4-1) |
| R3-11 | Milestone order | **DLQ before crash recovery.** Reason: the crash-recovery demo's point is partly that DLQ'd content is *not* touched by recovery — it must wait for an explicit replay. You cannot show that until DLQ exists |
| R3-12 | Planner response format | The old project's biggest time sink was arguing about syntactic/semantic validation of planner responses. **Mitigation: write the expected format explicitly and completely up front**, so validation becomes mechanical rather than a design discussion |
| R3-13 | Project name | `Cairn` is the current front-runner but the search continues. Target: as immediately understandable as "breadcrumb", but more explicitly about **workflows — especially non-deterministic / AI ones — that are crash-safe**, and readable as an orchestration engine. Two words acceptable |

### Owner's questions carried into Round 4

| # | Question as asked | Where answered |
|---|---|---|
| R3-Q1 | Is there an implementation that does the sweeper's job now but supports multi-replica later? Should we adopt it now, or must we do the naive sweeper first? | R4-1 |
| R3-Q2 | Should we maintain a stored `last_step` field for faster reads? Want the trade-off before ruling | R4-2 |
| R3-Q3 | "Fan-out is very hard under this data structure; DAG would need a whole new one." Is that assumption wrong? What are the design options? Market use may need fan-out | R4-3 |
| R3-Q4 | Besides a claiming algorithm or a load balancer in front, what other solutions exist for two orchestrators claiming the same run? | R4-1 |
| R3-Q5 | Should `runs.owner_id` be required? Required is complex; not required means a later schema change | R4-1 |
| R3-Q6 | Planner context: two-phase HTTP conversation (planner holds state) vs. planner calling back to an orchestrator read endpoint. Owner prefers the latter. What is actually *in* the orchestrator→planner message? Trade-off between "planner assembles the whole input" and "orchestrator assembles from the planner's references"? | R4-4 |
| R3-Q7 | What is `retry_after_seconds` (B5), and isn't it the same thing as per-step retry overrides (B6)? | R4-5 |
| R3-Q8 | Recovery (C1/C2/C4) must state explicitly what it **can** and **cannot** handle | R4-6 |
| R3-Q9 | Replay: if a replay exhausts the budget again and returns to DLQ, is it the same `run_id`? Can it be replayed again? Where exactly does replay idempotency bite? | R4-7 |
| R3-Q10 | Manual step re-run / output rewrite — same `run_id` or not? | R4-8 |
| R3-Q11 | Cancellation — how is it stored without adding states or breaking existing logic? | R4-9 |
| R3-Q12 | What are G2 (submission-time validation) and G4 (YAML/JSON boundary)? | R4-10 |
| R3-Q13 | Timeout / heartbeat / backoff / configurable limits — give several combinations and let the owner order them | R4-11 |

---

## Round 4 — Claude's analysis (input to SPEC; not itself spec)

### R4-1 · Lease-based claiming replaces both the sweeper and startup recovery

**Finding: the multi-replica-safe design is *simpler* than the single-replica one, not harder.**

Startup recovery and the sweeper are the same operation — "find runs that are RUNNING but not being driven, and drive them." Under one process you can approximate "not being driven" locally. Under N processes you cannot. The standard fix removes the approximation entirely:

```
runs.owner_id         TEXT        NULL   -- which orchestrator instance drives this run
runs.lease_expires_at TIMESTAMPTZ NULL
```

Claiming is one atomic statement, correct for N=1 and N=many alike:

```sql
UPDATE runs SET owner_id = :me, lease_expires_at = now() + :lease
WHERE status = 'RUNNING' AND (owner_id IS NULL OR lease_expires_at < now())
RETURNING run_id
```

Consequences:
- **Startup recovery stops being a separate code path** — it is just the first sweep.
- **No load balancer is needed.** Claiming *is* the load balancer (work-stealing): whoever has capacity claims. A balancer in front would be strictly worse — it needs liveness knowledge, and a dead replica's runs stay stranded until membership converges.
- **Fencing:** every state-writing transaction carries `AND runs.owner_id = :me`. Zero rows affected ⇒ this goroutine lost the lease ⇒ it stops. This is the same mechanism cancellation needs (R4-9) — one primitive, two uses.
- **Renewal is one UPDATE per interval for all owned runs**, not per run.
- `owner_id` is **nullable**: NULL means unclaimed. The "required vs. not" worry dissolves.
- Portable: a conditional UPDATE exists in every relational database, so it does not violate the storage-abstraction rule.

Alternatives considered and rejected: Postgres advisory locks (backend-specific — violates the abstraction rule); consistent hashing / balancer (needs membership, strands work on death); external queue (second source of truth); leader election (no horizontal scale).

### R4-2 · `last_step`: derive it, do not store it

- Derived: `SELECT status FROM steps WHERE run_id=? ORDER BY seq DESC LIMIT 1` on index `(run_id, seq)` — an index seek, sub-millisecond, and read once per sweep rather than per request.
- Stored pointer: saves microseconds, and introduces the possibility that the pointer and reality disagree. **The combination table is the entire recovery design; a stale pointer turns it into a lie.**
- It is a pure function of immutable data, so if profiling ever justifies it, it can be materialized later and backfilled safely. Nothing is foreclosed.

**Recommendation: derive.**

### R4-3 · Fan-out — the owner's assumption is half right

Correct half: a **full DAG** does need a new structure. `seq` as a total order becomes meaningless, the recovery branch key (`last_step`) becomes undefined, and the planner contract must grow dependency edges. That is a rewrite of the loop, not an addition.

Wrong half: **fan-out in general does not.** What actually blocks it is one sentence in the design — "the recovery unit is *the step* at max seq." Change it to "the recovery unit is *the set of steps* at max seq" and today's serial system is simply the case where every set has exactly one member.

| Option | Shape | Cost now | Cost later | Ceiling |
|---|---|---|---|---|
| **1. Serial forever** | as today | 0 | — | No cross-step parallelism ever |
| **2. Parallel group at one `seq`** | steps carry `seq` + `group_index`; the frontier advances when *all* members at max seq are DONE | **0 if the recovery unit is defined as a set from day one**; the schema gains one nullable column | Moderate: dispatch N, wait for all, per-member attempts already work unchanged | No cross-group dependencies; a group is atomic in the frontier |
| **3. Full DAG** | `steps.depends_on[]`, no `seq`, frontier = steps whose deps are DONE | High | High | None |

**Recommendation: adopt Option 2's *definition* now (recovery unit = the max-`seq` set), implement it later, and treat Option 3 as a different product.** This costs nothing today and buys the fan-out option outright. It covers the realistic cases — map over N items, call 3 models in parallel — which is most of what "fan-out" means in AI pipelines.

### R4-4 · Planner context and who assembles the worker's input

**Two-phase conversation vs. planner-fetches: the owner's preference (planner fetches) is right, and for a stronger reason than stated.** A two-phase HTTP conversation forces the planner to be *stateful* — it must know whether it is answering "here is the catalog" or "here is the next step". Every stateful planner then needs its own crash story. The fetch model keeps the planner a pure function: one request in, one decision out; anything it wants, it pulls.

It is also not really "another port" — it is the same read API the operator uses.

Costs to state plainly:
- The planner needs network access **back** to the orchestrator. Trivial for the built-in static planner; a real deployment constraint for a third-party planner behind a firewall.
- A planner failing because *the read API* was unreachable is indistinguishable, from the orchestrator's side, from a planner that is simply broken. Both consume planner budget. Diagnosis comes from the recorded per-attempt error text.
- Optional later mitigation: inline outputs below some size into the catalog so simple planners never need to fetch. This is the old 2 KB rule reborn — but as an optimization, never a requirement.

**Message shapes.**

Orchestrator → planner:
```json
{
  "run_id": "...",
  "workflow_input": { },
  "history": [
    {"step_id":"...", "name":"ocr", "seq":1, "status":"DONE",
     "output_bytes":14203, "completed_at":"..."}
  ],
  "fetch_base_url": "http://orchestrator:8080"
}
```

Planner → orchestrator: `{"status":"continue","step":{…}}` | `{"status":"done"}` | `{"status":"fail","reason":"…"}`

**Who assembles the worker's input — the trade-off the owner asked for:**

| | Planner assembles (literal payload in the StepSpec) | Orchestrator assembles (planner supplies references) |
|---|---|---|
| Planner must fetch past outputs | Always — to copy them forward | Only when it wants to *reason* about them |
| Planner→orchestrator message size | Large; carries the whole payload | Small; carries references |
| Duplication in the database | The same bytes stored again inside `steps.decision` | References only |
| Can transform data between steps | Yes, fully | No — the worker receives prior outputs as they were |
| LLM planner token cost | High | Low |
| Who is at fault for wrong worker input | The planner | Split: planner chose the references, orchestrator assembled |

**Recommendation: both, because they compose and neither is expensive.** `params` = the planner's literal values (small, config-like). `input_from` = references for bulk data, defaulting to *all completed steps*. A planner that genuinely needs to reshape bulk data fetches it and inlines it into `params` — so no capability is missing, and the cheap path is the default path.

### R4-5 · `retry_after_seconds` (B5) is not the same as per-step overrides (B6)

Three distinct knobs that the old documents kept conflating — this is exactly the "unclear responsibility split" the owner diagnosed:

| Knob | Question it answers | Who sets it | When |
|---|---|---|---|
| **retry limit (X)** | How many failures before DLQ | User (workflow, later step/run) | At config time |
| **attempt timeout** | How long may one attempt live before it is declared failed | User (workflow, later step/run) | At config time |
| **retry delay** | How long to wait after a failure verdict before the next attempt | User default (+ `retry_after_seconds`: the *worker* asks at runtime for a longer wait, e.g. it was rate-limited; the system takes the larger of the two, never the smaller) | Per failure |

B6 is the first two rows; B5 is the third row's runtime override. Different axes, different actors.

### R4-6 · What recovery can and cannot do (must be stated explicitly in SPEC)

**Handles:**
1. Orchestrator killed at any instant — in-flight runs resume; completed steps never re-run.
2. A single run's driver dying (storage blip, panic) while the process lives — reclaimed by the next sweep.
3. A crash *during* recovery — re-entrant, no double-counting (an already-claimed attempt is no longer RUNNING).
4. A crash loop — each pass burns one unit of retry budget, so every in-flight step converges monotonically to DLQ. Unbounded retry is structurally impossible.
5. Storage unreachable at startup — fail fast, non-zero exit, an error message that names storage as the cause.
6. Storage unreachable at runtime — runs orphan intact, reclaimed when storage returns.

**Does not handle — these are non-guarantees, and must be published as such:**
1. **Duplicate worker execution.** A re-dispatch may re-run a step that actually completed but whose result was never persisted. The worker must be idempotent on `step_id`. Worst case, up to X concurrent duplicates of one step.
2. **Work lost in flight.** A result computed but not persisted is gone and will be recomputed.
3. **Side effects already committed by the worker** (mail sent, payment made) — never rolled back.
4. **Planner non-determinism across a crash.** If the crash preceded persistence of the planner's answer, the planner is asked again and an LLM may answer differently. Legal by design: the "asked exactly once" guarantee covers *persisted* decisions only.
5. **Loss or corruption of the database itself.** Zero recovery — the DB is deliberately both the single source of truth and the single point of failure.
6. **Two orchestrators claiming the same run** — until leases are implemented.
7. **A worker that hangs with an effectively infinite timeout.** Nothing ever declares failure, so nothing ever recovers. This is why the default timeout must be finite.

### R4-7 · Replay idempotency — the real rule

The guard is **not** "has this run been replayed before". It is **"is this run in DLQ right now"**.

- Run currently in DLQ → replay proceeds; the transaction that takes it out of DLQ is itself the idempotency gate, so a double-click has exactly one winner.
- Run not in DLQ (RUNNING / DONE / CANCELLED) → 409, and the message states the actual current status.
- Therefore the owner's scenario resolves cleanly: replay → budget exhausted again → back to DLQ (a **second** DLQ row, **same `run_id`**) → **replay is allowed again**, as many times as it lands there. `run_id` never changes; `dead_letter_queue` accumulates history rows.

**Design correction to the old system:** it exposed `POST /dlq/{entry_id}/replay`, then needed a patch rule ("derive the worker-side/planner-side branch from current state, not from the entry") because an old entry and current reality diverge after multiple rounds. **Replay should target the run: `POST /runs/{run_id}/replay`.** The DLQ entry is history; the run is the thing you act on. This deletes the whole class of confusion.

### R4-8 · Manual step re-run / output rewrite

Same `run_id` — a run is the unit of history, and forking it would duplicate step history and break "one run, one story".

The hard part is not identity, it is **truncating the frontier**: to re-run step 3 of 6 you must make steps 4–6 stop counting.

| Option | Consequence |
|---|---|
| Delete the later steps | Destroys the audit trail. Rejected |
| Add `steps.superseded_at`, exclude superseded steps from history and from `last_step` | Works; adds one concept |
| Do not support it in v2 core — support only "replay the DLQ'd step", which needs no truncation because the DLQ'd step *is* the last step | Free |

**Recommendation: the third now, the second later if a real need appears.** Manual output rewrite (an UPDATE on a DONE step's output) is a separate, easier feature and also low priority.

### R4-9 · Cancellation, stored

Following the owner's ruling: `CANCELLED` is added to run and step. New legal combinations: `run=CANCELLED, last_step=CANCELLED` and `run=CANCELLED, last_step=DONE` — both untouched by recovery.

The open sub-question is the **attempt** underneath a cancelled step. Proposal: `attempt → FAILED(reason='cancelled')`, and **`attempt_count` is not incremented** — cancellation is not the worker's failure, and the step is terminal anyway, so the budget is irrelevant.

Implementation note: cancel races the driver goroutine. It resolves with the fencing predicate from R4-1 — cancel is one transaction conditioned on `run.status='RUNNING'`; the driver's next state write finds zero rows affected and exits. **One mechanism (fencing), three uses: lease hand-off, cancellation, and stale-writer suppression.**

Open: should a run in DLQ also be cancellable, giving DLQ an exit other than replay? Owner said RUNNING only; worth re-checking.

### R4-10 · What G2 and G4 were

**G2 — submission-time validation.** Reject a bad workflow definition at `POST /workflows` with a 400, instead of discovering it mid-run. The failures it catches: `planner_type: "htp"` (typo); `planner_type=http` with no URL; an unknown key like `retrylimit` silently ignored so the user believes a setting took effect; `retry_limit: "3"` as a string silently coerced to 3. The old project's governing principle, worth keeping: **better to be too strict and have users complain we do not support something, than too lax and let them fail silently.** Anything decidable at submission time must not be deferred to run time, where it costs budget, produces DLQ entries, and wastes triage.

**G4 — file format vs. wire format.** Config *files* on disk are YAML (comments allowed — worth a lot for demos and for a portfolio reader); the HTTP API body is JSON; the database stores JSON. The old repo shipped `.yaml` files whose contents were actually JSON — an extension lying about its content. The rule is one line: no file whose extension disagrees with its content.

### R4-11 · Timeout / liveness — combinations to choose from

Mechanisms available:

- **(a)** Finite default timeout + user-configured timeout per workflow (later per step/run). The user estimates how long the work takes.
- **(b)** Worker **heartbeat** extends the deadline: the worker pings `POST /tasks/heartbeat {step_id, attempt_id}` every N seconds; the effective deadline becomes `last_heartbeat + grace`.
- **(c)** Worker-requested retry delay (`retry_after_seconds`) — for rate limits.
- **(d)** Exponential backoff on the retry delay.
- **(e)** Progress payload on the heartbeat (`{"progress":0.4,"note":"page 12/30"}`), stored for observability.

| Combination | Contents | Time to detect a dead worker | Cost |
|---|---|---|---|
| **Punch 1** | (a) only | Up to the full configured timeout — for a 3-hour job, 3 hours | zero |
| **Punch 2** | (a) + (b) | ~grace (e.g. 90s), **independent of task length** | small: one endpoint, one column, dynamic deadline |
| **Punch 3** | Punch 2 + (e) | same, plus visible progress | small increment |
| **Punch 4** | Punch 2 + (c) + (d) | same, plus rate-limit friendliness | medium |

**Structural finding that outranks all of the above — persist `attempts.deadline_at`.**

The old design kept each attempt's timer **in process memory only**. That single choice is what forced the `orphaned` failure reason to exist: when the process died, the timer died with it, so nothing in the world would ever pronounce that attempt failed, and recovery had to claim it by hand.

Store the deadline as a column instead, and:
- **Timeout and orphan-claiming become one mechanism**: any live orchestrator's sweep expires any attempt whose `deadline_at` has passed. `orphaned` survives only as a diagnostic label ("was its owner alive?"), never as a separate mechanism.
- Timeouts are enforced correctly across **multiple replicas** — impossible with an in-memory timer.
- Heartbeats become an `UPDATE attempts SET deadline_at = now() + grace` — nothing else changes.
- The frontier's liveness stops depending on any process staying alive.

Refinement worth taking: on claiming a run, an orchestrator may expire a **sync**-mode attempt immediately (its HTTP connection died with the previous owner, so no report can ever arrive) but should let an **async**-mode attempt run to its deadline (its callback can still arrive and be honoured by the new owner). `connection_mode` is already in the persisted decision, so the rule costs nothing — and it recovers work the old design threw away.

---

## Round 4 — Rulings (2026-08-09)

| # | Topic | Ruling |
|---|---|---|
| R4-a | Persist `attempts.deadline_at`; timeout and orphan-claiming become one mechanism | **Adopted** |
| R4-b | Lease-based claiming | **Adopted in principle**, conditional on understanding lease semantics, fencing, and how cancellation uses it (explained in Round 5) |
| R4-c | Timeout is no longer exact | **Accepted trade-off — must be written down.** A timeout is a *lower bound* on when failure is declared, not an exact instant |
| R4-d | Parallel-group / fan-out pre-definition | **Leaning against pre-committing.** Owner's objection, which is correct: a same-`seq` group only buys "parallel single steps at one level", not a real DAG, and the model assumes branch depth is exactly one worker step. Decision: **write the definitions on the non-fan-out architecture**, with an explicit note that DAG may come later and its design is unexplored. No `group_index` column |
| R4-e | DAG vs. LLM planners | Owner's read — a real DAG suits a static planner, not an LLM one — is essentially right (see R5-3) |
| R4-f | Replay targets the run, not the DLQ entry | **Adopted, with its limitation written down:** if round 1 failed at step 1 and round 2 failed at step 2, replay resumes from step 2 and never revisits step 1. Accepted because it is unambiguous. **Requirement: every replay round must leave an inspectable record** |
| R4-g | Two-table discipline | `runs` / `steps` hold **current status**, one row each, mutated in place. `dead_letter_queue` is **append-only history**, never modified, many rows per run |
| R4-h | Manual step re-run / output rewrite | **v2 core supports only "replay the step that is in DLQ"** — i.e. the run resumes from its furthest successful point. Because a replay always resumes at the latest reached step, "round 2 fails earlier than round 1" cannot arise. This is a convenience-driven scope cut, not a principle; revisit if a real need appears |
| R4-i | Cancelling a step whose attempt already finished | If the attempt is DONE, the step is DONE — leave both alone. Only `run` becomes CANCELLED |
| R4-j | G2 submission-time validation / G4 format boundary | **Both accepted.** Disk config = YAML; HTTP body = JSON; DB = JSON. No file whose extension disagrees with its content |
| R4-k | Recovery non-guarantees | Accepted, **plus one more to state explicitly: recovery never auto-replays a DLQ'd run.** Reaching DLQ means a human decides |
| R4-l | Cancelling a run that is in DLQ | **Allowed.** Exact state combination to be settled — see R5-5 |
| R4-m | Heartbeat (punch 2) | **Deferred.** Not strictly necessary; progress reporting and exponential backoff likewise go to the later list |
| R4-n | Raw (un-enveloped) worker body | **Both `sync` and `async` must eventually support it**, so that a worker already written in its own style needs no modification at all. They may land at different times. **`sync` raw is the priority** — async raw would force the orchestrator to conform to the worker's own callback shape, which is complex; the alternative is that the user writes a small adapter. Record both paths now |
| R4-o | Project name | **Piton.** New repository |

---

## Round 5 — Claude's analysis (input to SPEC; not itself spec)

### R5-1 · What the sweep now does, and why timeout stops being exact

The sweep carries **two** jobs that used to be separate: claiming unowned runs, and expiring attempts whose `deadline_at` has passed.

**Consequence to publish:** a timeout is a *lower bound*, not an exact instant. An attempt with a 60s timeout is declared failed somewhere in `[deadline, deadline + sweep_interval]`.

But this only applies when nobody is actively driving the run. Refinement worth keeping:

- **Owner alive** → the driver goroutine is already blocked waiting for the response, so it enforces its own in-memory timer and declares failure **precisely** at the deadline.
- **Owner dead / never claimed** → the sweep is the backstop, bounded by the sweep interval.

So the in-memory timer is a *latency optimisation* and the persisted `deadline_at` is the *correctness backstop*. Two mechanisms, clearly ranked, neither load-bearing for the other. State it that way, because "which one is authoritative?" is exactly the kind of ambiguity that produced the old documents' mess: **the database is authoritative; the in-memory timer only makes the common case fast.**

### R5-2 · Why a lease, and what fencing actually is

**Why ownership must expire.** If `owner_id` were set with no expiry, a dead orchestrator would own its runs forever and no one could take them — you would then need something to detect its death and clear the field, which is the original problem again. A lease is **ownership that decays unless renewed**, so renewal *is* the liveness signal. (Note the symmetry: a lease renewal is the orchestrator's heartbeat, exactly as a worker heartbeat would be the worker's.)

Length is a straight trade-off: shorter lease = faster failover, more renewal writes. 30s lease renewed every 10s is a reasonable default. Renewal is **one** UPDATE covering every run this instance owns, not one per run.

**Fencing** = every state-writing transaction asserts, in the same statement, that the writer still holds the run:

```sql
UPDATE steps SET status='DONE', output=:out
WHERE step_id=:sid
  AND EXISTS (SELECT 1 FROM runs
              WHERE run_id=:rid AND status='RUNNING'
                AND owner_id=:me AND lease_expires_at > now());
```

Zero rows affected ⇒ I no longer have the right to write ⇒ abort the transaction, stop the goroutine, dispatch nothing further. No inter-process coordination, no signalling, no killing threads from outside: **a writer discovers it has been superseded at its next write.**

**Cancellation is the same primitive.** Cancel is one transaction:

```sql
UPDATE runs SET status='CANCELLED', owner_id=NULL
WHERE run_id=:rid AND status IN ('RUNNING','DLQ');
-- plus: last step RUNNING → CANCELLED, its RUNNING attempt → FAILED('cancelled')
```

Once it commits, the old driver's next write hits the fence and the goroutine exits on its own. Cancelling the driver's Go `context` as well is a *speed* optimisation, never the correctness mechanism.

**Non-guarantee this creates, and it must be published:** cancellation stops the orchestrator from advancing the run; it does **not** abort work already handed to a worker. A worker dispatched moments before the cancel keeps running to completion, and its report is simply refused. "Cancelled" means "this run will make no further progress", not "everything stopped instantly".

### R5-3 · Fan-out and DAG — answering the owner's objection

**How `group_index` would have worked:** steps carry `(seq, group_index)`; all steps sharing a `seq` are dispatched together; the frontier advances — i.e. the planner is asked again — only once **every** member at max `seq` is DONE. Recovery's branch key becomes "the set of steps at max `seq`": any member RUNNING ⇒ the claim path; all DONE ⇒ ask the planner; any DLQ ⇒ the run is in DLQ.

**The owner's objection is correct.** That model buys a fan-out/fan-in **barrier**, not a DAG. Every branch is exactly one worker step deep, and branches cannot depend on one another.

**On DAG vs. LLM planners** — the owner's instinct is right, and the reason is worth recording: a real DAG is a *graph declared up front*, which is inherently static-planner territory. Asking an LLM to emit a full dependency graph contradicts this system's entire thesis, which is deciding **one step at a time** with full knowledge of everything that already happened. What an LLM *can* naturally emit is "these three things can run now" — which is the group model, not the DAG model. So if fan-out ever lands, the group model is the LLM-compatible shape and a DAG is a different product.

**Decision taken:** no `group_index`, no pre-commitment. One free hedge is still on the table — phrasing the recovery rule as "the frontier is the set of steps at the maximum `seq`, and in v1 that set always has exactly one member." That is wording only: no schema, no code, no test. Owner to rule.

### R5-4 · Replay rounds must be inspectable

The owner requires that every replay round leave a record a human can check. Two ways:

1. **Reconstruct** — bucket attempts by `created_at` against the timestamps of the `dead_letter_queue` rows. Free, and what the old design chose. Awkward to do by eye in a terminal.
2. **`runs.replay_count`**, one integer, incremented inside the replay transaction. Trivially inspectable; makes "which round was this?" a column instead of a puzzle. Cost: one column, one increment, no new concept.

Recommendation: **take (2)**. It directly serves the owner's requirement that he can sit at a terminal and see what happened.

### R5-5 · The complete run × last_step table (this belongs in SPEC, prominently)

**Convention that collapses the table:** *a run with no steps is defined to have `last_step = DONE`.* It behaves identically to a completed last step everywhere — the planner is asked next — so it needs no separate row. This single sentence removes four cells.

**Two cells the old design never listed, found in this audit:**
- `run=DONE, last_step=DONE(none)` — the planner answers `done` on its very first call. An empty pipeline is legal.
- `run=DLQ, last_step=DONE(none)` — the planner answers `fail`, or exhausts its budget, before any step exists.

Both are already covered once "no steps ⇒ last_step=DONE" is adopted. Without that convention they are holes.

**Legal combinations — 8, and recovery only ever performs one of two actions:**

| # | run | last_step | Meaning | Action when claimed |
|---|---|---|---|---|
| L1 | RUNNING | DONE | Waiting on a planner decision (includes the no-steps case) | **Ask the planner** |
| L2 | RUNNING | RUNNING | Waiting on a worker, or its owner died | **The claim path:** expire the overdue/orphaned attempt → budget check → re-dispatch, or it already went to DLQ inside the claim |
| L3 | DONE | DONE | Cleanly finished | Never scanned |
| L4 | DLQ | DLQ | Worker-side DLQ | Never scanned. Exit: replay, or cancel |
| L5 | DLQ | DONE | Planner-side DLQ | Never scanned. Exit: replay, or cancel |
| L6 | CANCELLED | CANCELLED | Cancelled while a step was in flight | Never scanned |
| L7 | CANCELLED | DONE | Cancelled while waiting on the planner, or cancelled out of a planner-side DLQ | Never scanned |
| L8 | CANCELLED | DLQ | Cancelled out of a worker-side DLQ; the step keeps its DLQ verdict as a historical fact | Never scanned |

**Impossible combinations, and why each is impossible:**

| run | last_step | Why it cannot exist |
|---|---|---|
| RUNNING | DLQ | `step→DLQ` and `run→DLQ` are one transaction; no instant separates them |
| RUNNING | CANCELLED | `step→CANCELLED` occurs only inside the cancel transaction, which sets `run→CANCELLED` in the same commit |
| DONE | RUNNING | `run→DONE` is produced only by the planner answering `done`, whose precondition is `last_step=DONE` |
| DONE | DLQ | same |
| DONE | CANCELLED | same |
| DLQ | RUNNING | Worker-side DLQ sets both in one transaction; planner-side DLQ requires `last_step=DONE` |
| DLQ | CANCELLED | Cancel always sets `run=CANCELLED`, never `run=DLQ` |
| CANCELLED | RUNNING | The cancel transaction terminates a RUNNING last step in the same commit |

**The uniform cancel rule** (one sentence, covers all three cancelled combinations without special cases):

> `run → CANCELLED`. If `last_step = RUNNING`, then `last_step → CANCELLED` and its RUNNING attempt → `FAILED(cancelled)` with `attempt_count` **unchanged**. Otherwise the last step keeps the terminal state it already had (DONE or DLQ). A DLQ verdict is a historical fact and is never rewritten.

### R5-6 · Heartbeat, correcting a misunderstanding

The heartbeat proposed is **worker → orchestrator push**: the worker calls `POST /tasks/heartbeat {step_id, attempt_id}`, and the orchestrator does `UPDATE attempts SET deadline_at = now() + grace`. **The orchestrator never polls workers**, and it is not a database poll either.

The alternative — the orchestrator polling a status endpoint on each worker — requires every worker to expose one and costs one request per in-flight attempt per interval. Push is cheaper and puts the obligation on the party that actually knows whether it is still working. Either way it requires worker cooperation, which is why it is deferred.

### R5-7 · Where the backlog lives

Options: a section in `CLAUDE.md`, a separate `BACKLOG.md`, or the final milestone.

**It must not be the final milestone** — a milestone is scheduled work, and backlog items are by definition unscheduled. Labelling them as a milestone quietly promises they will be built.

Recommendation: **`CLAUDE.md § Backlog (unscheduled — not specification)`**, keeping the two-file rule intact. `CLAUDE.md` is already the "what to do next" document, so it is the natural home. Split it into its own file only if it outgrows roughly fifty items.
*(Superseded by R5 ruling: milestones live in `SPEC.md`, unscheduled work lives in `BACKLOG.md`, `CLAUDE.md` holds neither.)*

---

## Round 5 — Rulings (2026-08-09)

| # | Topic | Ruling |
|---|---|---|
| R5-a | Fan-out definition | **Do not touch the current definition at all**, not even in wording. The "frontier is a set" phrasing moves into the backlog as part of the fan-out item |
| R5-b | Fan-out vs. DAG are two separate backlog items | **Fan-out** = an extension of the current model. **DAG** = a new, independent static mode. Both provisional and revisable |
| R5-c | Worker heartbeat | **Dropped entirely**, not merely deferred |
| R5-d | `runs.replay_count` | **Adopted** |
| R5-e | Where milestones and backlog live | **Milestones live in `SPEC.md`** as the planned sequence. **Unscheduled work lives in `BACKLOG.md`.** `CLAUDE.md` holds neither |
| R5-f | Milestone order (final) | α → γ → β → δ → **ζ (custom HTTP/LLM planner, pulled forward to sit right after replay)** → θ (sync raw body) → ε (async envelope) → η (per-step/run overrides) → ι (cancel + submission-time validation) |
| R5-g | Project name | **Piton**, confirmed after an occupancy check: no collision inside workflow orchestration; collisions exist outside it (OpenPiton at Princeton — hardware; a dormant `piton` package on PyPI; Piton Labs — a dev shop) |
| R5-h | Retention / purge | Not required for sweep correctness (see R6-3). Offered as a housekeeping backlog item only |

### Owner's questions carried into Round 6

| # | Question | Where answered |
|---|---|---|
| R5-Q1 | How exactly does the sweep find an unowned run or a timed-out attempt — is it scanning the DB rather than some in-process registry? And what actually causes a goroutine to go missing? | R6-1 |
| R5-Q2 | If the lease is not refreshed on every state write, a long run's lease expires, a different orchestrator claims it — but the planner's and worker's traffic still goes to the old one. Doesn't that make any run longer than the lease effectively an orphan? Would the new owner re-dispatch work that was already done? | R6-2 |
| R5-Q3 | Since DONE/CANCELLED/DLQ runs stay in the same table, won't the sweep scan more and more rows until it explodes? Is a user-run purge the answer? | R6-3 |

---

## Round 6 — Claude's analysis (input to SPEC; not itself spec)

### R6-1 · What the sweep scans, and how a driver goes missing

The sweep reads **the database only**. It has no in-process registry of live goroutines, and deliberately so: an in-process registry cannot see other replicas, and it is lost on restart. `owner_id` + `lease_expires_at` **are** the database-visible representation of "is anyone driving this", which is why they must exist as columns rather than as memory.

A driver goroutine disappears without the run reaching a terminal state whenever any of these happen:

- a storage write fails and the driver returns the error (the old design's explicit rule was that the driver dies here);
- a panic anywhere in the driver — a bug parsing a planner response, a nil dereference;
- the process is killed: deploy, rolling restart, OOM kill, `docker compose down`, machine reboot;
- a network partition to the database that outlasts the driver's patience.

None of these are exotic. "The goroutine mysteriously vanished" is really "any error path that returns before the run is terminal", and there are always more of those than anyone enumerates — which is precisely the argument for a database-level backstop rather than careful goroutine bookkeeping.

### R6-2 · Lease renewal — answering the owner's strongest objection so far

**The premise in the question is wrong, and the correction matters: the lease is NOT renewed as a side effect of writing run state. It is renewed by a dedicated ticker, unconditionally, whether or not the run is making progress.**

```sql
-- every ~10s, one statement covering every run this instance owns
UPDATE runs SET lease_expires_at = now() + interval '30 s'
WHERE owner_id = :me AND status = 'RUNNING';
```

Renewal is a **liveness signal, not a progress signal**. A run sitting on a single three-hour worker call keeps its lease the entire time, because the process holding it is alive and ticking. So "any run longer than the lease becomes an orphan" does not happen. This distinction — liveness versus progress — is the whole reason the lease is a separate concept from the state writes.

**What happens in the case the owner was really worried about** — the owner process is alive but stalled past its lease (long GC pause, VM suspend, database partition):

1. Another orchestrator claims the run.
2. The stalled owner wakes and tries to write. Its transaction carries `AND owner_id=:me AND lease_expires_at > now()`, so it affects zero rows. It aborts and stops. **Correctness holds without any communication between the two processes.**
3. The new owner re-dispatches. **Yes — the worker may perform the same work twice.** That is the published at-least-once contract, absorbed by worker idempotency keyed on `step_id`. It is the same duplication a crash already causes; a stalled owner is simply a crash that came back.

**The real problem hiding inside the question, which the owner half-found: where do async callbacks go?**

In async mode the worker POSTs back to a callback URL. With several replicas behind a load balancer, that callback can land on a replica that does not own the run. The old design made this fatal by construction: the callback handler pushed the result into an **in-memory channel** owned by the driver goroutine. On the wrong replica there is no channel, so the result is lost.

The fix follows from what has already been decided. Once `deadline_at` is a column and every write is CAS-guarded and fenced, the database — not the process layout — arbitrates who wins. So:

- **the callback handler applies the report directly, under CAS + fencing.** Any replica can accept any callback.
- **the driver awaiting an async result polls the attempt row** (say once a second) instead of blocking on an in-memory channel.

This deletes, all at once: the channel registry, the rule that the timer must scrub the registry when it fires, the "single writer" rule's process-level enforcement, and the wrong-replica callback loss. Polling costs one cheap indexed query per second per in-flight async attempt, which for this system's workload is nothing. An in-process fast path can be added later as an optimisation — never as the mechanism.

**The unifying principle this produces, and it should open the SPEC:**

> **The database is the only coordination mechanism. Every in-memory structure is a cache that may vanish at any instant without affecting correctness.**

Three consequences, all already decided independently, now visibly the same idea:
- the attempt deadline lives in a column; the in-memory timer is only a latency optimisation;
- run ownership lives in a column with an expiry; process identity means nothing on its own;
- an async result is delivered through a row; a channel is only a latency optimisation.

### R6-3 · The sweep does not degrade as history accumulates

The sweep predicate is `status='RUNNING' AND (owner_id IS NULL OR lease_expires_at < now())`. With a **partial index on `runs(status) WHERE status='RUNNING'`**, terminal runs are not in the index at all — a DONE run from six months ago costs literally nothing. Cost is proportional to the number of **currently running** runs, which is bounded by concurrency, not by history.

For a backend without partial indexes, a plain index on `status` still seeks straight to the RUNNING range; the cost is the same order. So this does not violate the storage-abstraction rule.

**Therefore no purge is needed for correctness or for sweep performance.** Retention remains worth offering eventually for disk usage, and belongs in the backlog as housekeeping — not as something a user must do to keep the system healthy.

One rule the sweep must state explicitly: it filters on `status='RUNNING'`, so CANCELLED, DONE and DLQ runs are never claimed. Cancellation's `owner_id = NULL` is therefore belt-and-braces, not the mechanism.

---

## Round 7 — Claude's analysis (input to SPEC; not itself spec)

### R7-1 · The lease, specified properly (owner was right that it was never defined)

The earlier sketch put `lease_expires_at` on `runs` and renewed it with a ticking UPDATE across every owned run. That is workable but bad: it rewrites every RUNNING run row every ten seconds, producing continuous dead-tuple churn on the busiest table in the system, and it blurs "run state" with "coordination metadata".

**Better design — liveness belongs to the orchestrator, not to the run.**

```sql
orchestrators (
  orchestrator_id  TEXT PRIMARY KEY,      -- generated once at process boot
  started_at       TIMESTAMPTZ NOT NULL,
  last_seen_at     TIMESTAMPTZ NOT NULL   -- the only column a heartbeat touches
)

runs.owner_id   TEXT NULL REFERENCES orchestrators(orchestrator_id)
runs.claimed_at TIMESTAMPTZ NULL
```

- The heartbeat is **one row per process**, not one per run: `UPDATE orchestrators SET last_seen_at = now() WHERE orchestrator_id = :me`, every ~10s. O(1) regardless of load.
- An orchestrator is **live** iff `last_seen_at > now() - lease_ttl` (default 30s).
- `runs.owner_id` changes only on **claim** and **release** — meaningful, rare events, never a ticking write.
- Claim (one atomic statement, so two sweeps cannot both win):

```sql
UPDATE runs r SET owner_id = :me, claimed_at = now()
WHERE r.status = 'RUNNING'
  AND (r.owner_id IS NULL
       OR NOT EXISTS (SELECT 1 FROM orchestrators o
                      WHERE o.orchestrator_id = r.owner_id
                        AND o.last_seen_at > now() - :ttl))
RETURNING r.run_id;
```

**The fence becomes simpler than the earlier sketch.** Every business transaction opens with:

```sql
SELECT 1 FROM runs WHERE run_id = :rid AND owner_id = :me FOR UPDATE;   -- 0 rows ⇒ abort
```

No expiry check is needed here: if this process stalled and someone else claimed the run, `owner_id` is no longer `:me` and the fence fires. If nobody claimed it, this process is still the rightful owner and proceeding is correct. `FOR UPDATE` takes the row lock, which serialises the fence against a concurrent claim — without it, a claim committing mid-transaction could leave two drivers briefly believing they own the run.

**Carve-out that must be stated, or it becomes the next ambiguity:** the closed transaction ledger governs **business state** — run/step/attempt status, outputs, budgets. The coordination columns (`orchestrators.last_seen_at`, `runs.owner_id`, `runs.claimed_at`) are **not** business state and are written only by claim, heartbeat, and release. Without this sentence, "the heartbeat is a state write outside the ledger" reads as a violation.

### R7-2 · Async callback: who continues the run?

Scenario: orchestrator A dispatched the attempt; the worker's callback lands on orchestrator B.

**Ownership does not move. B applies the result; A continues driving.**

Rejected alternative — B steals ownership on callback — because it would fence A off mid-run for no reason, make ownership ping-pong with traffic routing, and hand runs to instances that never asked for them. Ownership must change only when the owner is absent or dead.

This requires one explicit exception to the fencing rule, and the distinction is principled:

| Kind of write | Requires ownership? | Guarded by |
|---|---|---|
| **Driving** — asking the planner, creating a step, dispatching, replaying, cancelling | **Yes** | the ownership fence |
| **Report application** — turning an arriving success/failure into attempt+step state | **No** | CAS on the attempt (`attempt_id = current_attempt_id AND status = 'RUNNING'`) |

The justification: a report is a **fact about work that happened**, not a **decision about what the run should do next**. Facts may be recorded by whoever receives them; decisions belong to the owner. CAS already guarantees exactly one writer per attempt outcome, so a non-owner cannot corrupt anything — and A learns about it on its next poll of the attempt row.

**This is also the answer to "how does A find out?"** — it is exactly why the driver polls the attempt row instead of waiting on an in-memory channel.

### R7-3 · Sweepers, plurality and atomicity

Yes — **every orchestrator instance runs its own sweep**; there is no designated sweeper. The owner's instinct is right that concurrent sweeps must not corrupt each other, and the claim is a single atomic `UPDATE … RETURNING` taking a row lock, so exactly one instance wins each run.

One simplification worth taking: **the sweep only claims. It never touches business state.** Expiring overdue attempts, checking budgets and re-dispatching are done afterwards by the driver that now owns the run, through the normal fenced path. This keeps "business writes require ownership" true with only the single report-application exception above, instead of scattering exceptions.

Consequently: an overdue attempt on a run owned by a *live* orchestrator is expired by that owner (its own timer or poll); `attempts.deadline_at` is the backstop consulted by a **new** owner at claim time.

### R7-4 · Required indexes are part of the storage contract

Because the storage layer is an abstraction with potentially several backends, "`runs.status` is indexed" is not an implementation footnote — a backend without it is not a conforming implementation. It belongs in a short **Storage requirements** section of SPEC (required indexes and the atomicity obligations), not scattered through the behavioural rules where it would be noise.

---

## Round 7 — Rulings (2026-08-10)

Context: the Round 7 exchange was lost and reconstructed from `./temp` (a truncated terminal
capture) plus this log. The analysis in R7-1…R7-4 above was intact; only the rulings were missing.

| # | Topic | Ruling |
|---|---|---|
| R7-a | Lease design | **The `orchestrators` liveness table (R7-1), not `runs.lease_expires_at`.** Heartbeat is O(1) — one row per process, not one per run. `runs` is not polluted by ticking writes. The fence simplifies to `SELECT 1 … FOR UPDATE` with no expiry check |
| R7-b | Storage JSON handling | **The storage interface takes and returns `[]byte`** — an opaque JSON document. The Postgres implementation stores `jsonb` internally; an implementer on another relational DB picks their own encoding (TEXT / BLOB / native JSON). Cost accepted: the Go layer gets no JSONB query capability (unused today) |
| R7-c | Repository landing | **SPEC first.** `git init`, moving `docs/` into `legacy/`, and `go.mod` all wait until SPEC is settled. Physical location is already `/home/aaronwu/Projects/piton` (the owner moved it; `/home/aaronwu/stateflowv2` no longer exists). Proposed Go module path `github.com/aaronwu001/piton` — not yet ruled |
| R7-d | Fencing exception for report application | **Narrow version adopted.** The exception exists **only** for an async callback landing on a non-owner, and it may write **only the `attempts` table** (that attempt's terminal state). All writes to `steps` and `runs` require ownership. The CAS predicate is `attempt_id = :aid AND status = 'RUNNING'`; no comparison against `steps.current_attempt_id` is needed, because a superseded attempt has already been moved out of `RUNNING` |
| R7-d′ | Priority of the multi-replica story | **Multi-replica orchestration is not a milestone.** It is the "must not be foreclosed" constraint of R2-8g, not something to build. R7-d's *rule* goes into SPEC now for exactly one reason: it determines the async delivery mechanism (the driver polls the attempt row rather than blocking on an in-memory channel), which would be awkward to retrofit. With a single replica the exception is never exercised |
| R7-e | Config field naming and defaults | **Adopted** (see table below). Additional requirement: `step_max_attempts` is a **total attempt count**, not a retry count — SPEC must say so explicitly |
| R7-f | Run-creation API | **Keep the `overrides` sub-object**; any non-empty value is a 400 until milestone η |
| R7-g | Planner message shapes / who assembles worker input / `output_field` | **All adopted.** (a) planner fetches from the read API rather than a two-phase conversation; (b) `params` *and* `input_from` coexist; (c) omitted `input_from` ⇒ the previous step only; (d) **`output_field` is cut** — the whole worker response body is stored verbatim as the step output |
| R7-h | run × last_step combination table | **Adopted wholesale** — L1–L8, the uniform cancel rule, and the "a run with no steps has `last_step = DONE`" convention. Goes into SPEC prominently. This discharges the R2-Q3 requirement that the table be settled before implementation starts |
| R7-i | `orchestrators` lifecycle | **No foreign key** — `runs.owner_id` is a plain string. **`orchestrator_id` is a UUID generated at process boot.** Dead-row cleanup is **backlog housekeeping**, not a correctness requirement |
| R7-j | SPEC opening principle | **Adopted:** *The database is the only coordination mechanism. Every in-memory structure is a cache that may vanish at any instant without affecting correctness.* |
| R7-k | Writing discipline for SPEC | **Definitions and architecture come first, written concisely and clearly**, so that later rules have unambiguous referents. A term that carries a definition (e.g. "StepSpec") may not appear before that definition |
| R7-l | Scope discipline | SPEC is written in full so nothing is forgotten and it can be picked up later — **but the focus stays on the early part: whatever is written must be demoable, testable, and runnable by hand.** Operative test for admitting anything now: **would adding it later be awkward?** If not, it goes to BACKLOG |

**Config fields adopted under R7-e:**

| Level | Field | Default | Meaning |
|---|---|---|---|
| workflow | `step_timeout_seconds` | 300 | Upper bound on one attempt |
| workflow | `step_max_attempts` | 3 | Total attempts before the step goes to DLQ (0 = no execution at all is *not* the meaning; SPEC must define the edge) |
| workflow | `step_retry_delay_seconds` | 0 | Wait between a failure verdict and the next attempt |
| workflow | `planner_timeout_seconds` | 30 | Upper bound on one planner call |
| workflow | `planner_max_attempts` | 3 | Planner failures before the run goes to DLQ |
| StepSpec | `timeout_seconds` / `max_attempts` | null | Override the two step-level values (fields present now, implemented in η) |

**Why two symmetric prefixes:** `step_*` and `planner_*` make it visible at a glance which knob
governs which actor. The old system's `retry_limit` / `RETRY_MAX_ATTEMPTS` / a hard-coded 30s was
precisely the "you cannot tell what governs what" failure.

---

## Round 8 — Claude's analysis (input to SPEC; not itself spec)

### R8-1 · Fencing and CAS are the same primitive (the owner asked; the earlier wording was bad)

The owner's objection — *"shouldn't it just be: check whether the run is still mine, and walk away
if it isn't?"* — is exactly right, and describes the actual behaviour. The earlier phrasing
("every transaction **asserts** it still holds the run") read as *claiming* possession rather than
*testing* it.

**Why it cannot be two separate steps.** A check followed by a write has a gap:

```sql
SELECT owner_id FROM runs WHERE run_id = :rid;   -- (1) still mine, good
                                                 -- (2) ← someone else claims it here
UPDATE steps SET status = 'DONE' WHERE ...;      -- (3) no longer entitled, but the write lands
```

That is TOCTOU (time-of-check to time-of-use). The requirement is therefore not "check" but
**"check and write, with no window in which the answer can change."** Two ways to get it:

- **(A) Put the condition inside the writing statement** — the original R4-1 / R5-2 shape:
  `UPDATE steps … WHERE step_id=:sid AND EXISTS (SELECT 1 FROM runs WHERE run_id=:rid AND owner_id=:me)`.
  No gap, but a business transaction touches `steps`, `attempts` and `runs`, so the predicate must
  be repeated in every statement — noisy, and **one forgotten statement breaks it**.
- **(B) Take a row lock once at the top of the transaction** — the R7-1 shape, and what we adopted:
  `SELECT 1 FROM runs WHERE run_id=:rid AND owner_id=:me FOR UPDATE`. `FOR UPDATE` makes a
  concurrent claim block on that row until we commit or abort, so **one statement protects the whole
  transaction** instead of relying on the author remembering.

**Definitions, stated once so they stop being confusing:**

- **CAS (compare-and-set)** is the general primitive: a write whose effect is conditional on the
  current state matching an expectation, evaluated atomically with the write. In SQL that is
  `UPDATE … WHERE <expected state>`; zero rows affected means the expectation was wrong.
- **Fencing** is *a specific use* of that primitive, where the condition is about **ownership**.
  The name comes from the distributed-systems "fencing token": the point of a fence is to keep a
  superseded process out.

**So: fencing IS CAS — the same mechanism with a different predicate.** The system has two
predicates, and the distinction is worth stating in SPEC exactly this way:

| | **Ownership fence** | **Attempt CAS** |
|---|---|---|
| Question the condition asks | "Am I still this run's owner?" | "Has this attempt's outcome not been written yet?" |
| Row it tests | the `runs` row | the `attempts` row |
| SQL | `WHERE run_id=:rid AND owner_id=:me` | `WHERE attempt_id=:aid AND status='RUNNING'` |
| What it prevents | two drivers making **decisions** about one run | one attempt's outcome being written twice (including a late report) |
| Meaning of zero rows | I am out — abort, stop the goroutine, dispatch nothing | someone already recorded this — discard the report, reply 409 |
| Who must satisfy it | every write that constitutes a **decision** | every write of an attempt outcome, owner or not |

R7-d's exception, restated in these terms: **an async callback landing on a non-owner checks only
the second predicate, not the first.** That is safe because the only row it can touch is one that
already admits exactly one winner.

**Storage-contract obligation this creates** (for the SPEC's Storage Requirements section): a
conforming backend must re-evaluate a blocked `UPDATE`'s `WHERE` clause after the lock is released.
Postgres does this under READ COMMITTED. Without it, a claim would decide using a stale snapshot.

### R8-2 · `dispatch_style` — the two modes, which had never been spelled out

| | `envelope` | `raw` |
|---|---|---|
| What the worker receives | Piton's own JSON envelope (`run_id`, `step_id`, `attempt_id`, `connection_mode`, `params`, `inputs`, and `callback_url` when async) | **only the payload — no Piton fields at all** |
| Must the worker be written for Piton? | Yes — it must parse the envelope | **No** |
| Reason it exists | Normal mode; full functionality | R3-3: **any unmodifiable HTTP endpoint is a valid worker** (an existing internal API, a third-party API) — you cannot add fields to its request body |
| How bulk data reaches it | `inputs`, assembled by the orchestrator from `input_from` | Only by the planner fetching it and inlining it into `params` — the body has no second field |
| Milestone | α onward | θ |

Mechanical consequence: **`async` + `raw` cannot work** — a raw body has nowhere to put
`callback_url`, and R4-n already noted that async raw would force the orchestrator to conform to the
worker's own callback shape. The user's alternative is a small adapter. → BACKLOG.

So there are **three** legal `connection_mode` × `dispatch_style` combinations, not four.

### R8-3 · The complete message catalogue

Planner calls are **always synchronous** — the planner is a pure function: one request in, one
decision out.

**M1 · orchestrator → planner**

```json
POST {planner_url}
{
  "run_id": "018f...",
  "workflow_input": { "doc_url": "s3://..." },
  "history": [
    { "step_id": "018f...", "step_name": "ocr", "seq": 1, "status": "DONE",
      "output_bytes": 14203, "attempt_count": 2, "completed_at": "2026-08-10T12:00:00Z" }
  ],
  "fetch_base_url": "http://orchestrator:8080"
}
```

`history` is a **catalogue only** — it never carries outputs; that is what `output_bytes` is for.
Because the planner is asked only in state L1 (`last_step = DONE`), every step in `history` is DONE.
That is not a coincidence; it follows from the combination table.
*(The presence of `history` at all is reopened in Round 9 — see R9-1.)*

**M2 · planner → orchestrator** — three responses. Note that `StepSpec` is **one field of the
response**, not the response itself:

```json
{ "status": "continue", "step": { ...StepSpec... } }
{ "status": "done" }
{ "status": "fail", "reason": "cannot determine the next step from the document" }
```

`StepSpec`:

```jsonc
{
  "step_name": "ocr",                     // optional display label, non-unique (R2-6)
  "worker_url": "https://...",
  "connection_mode": "sync" | "async",    // R2-4: every message carries it explicitly
  "dispatch_style": "envelope" | "raw",   // R3-3 / R4-n
  "params": { },                          // the planner's literal, config-like values
  "input_from": ["<step_id>", ...],       // omitted ⇒ the previous step; [] ⇒ nothing
  "timeout_seconds": null,                // override, implemented in η
  "max_attempts": null
}
```

**M3 · planner → orchestrator (fetch)** — the same read API the operator uses; see R8-b below.

**M4 · orchestrator → worker** — the three legal combinations:

```json
// M4a · sync + envelope (α)
POST {worker_url}
{ "run_id": "018f...", "step_id": "018f...", "attempt_id": "018f...",
  "connection_mode": "sync",
  "params": { "lang": "zh-TW" },
  "inputs": { "018f...": { "text": "..." } } }

// M4b · async + envelope (ε)
POST {worker_url}
{ "run_id": "018f...", "step_id": "018f...", "attempt_id": "018f...",
  "connection_mode": "async",
  "params": { }, "inputs": { },
  "callback_url": "http://orchestrator:8080/callbacks/018f..." }
```

```
// M4c · sync + raw (θ)
POST {worker_url}
{ "lang": "zh-TW" }        ← params, verbatim. No Piton fields whatsoever.
```

**M5 · worker → orchestrator (result)**

- **M5a · sync + envelope** — the HTTP response body:
  `{"status":"success","output":{ }}` or `{"status":"failure","error":"…"}`.
  A transport-level failure (non-2xx, timeout, connection refused) is **always** a failure
  regardless of body. A business-level failure and a transport-level failure **burn one attempt
  alike**.
- **M5b · sync + raw** — no envelope is available, so only HTTP speaks: 2xx ⇒ success and the
  **entire response body verbatim is the output**; non-2xx ⇒ failure, with the (truncated) body
  stored as error text.
- **M5c · async + envelope** — `POST /callbacks/{attempt_id}` with
  `{"attempt_id":"…","status":"success","output":{ }}`. The immediate HTTP response to the dispatch
  (M4b) means only "accepted": 2xx ⇒ the attempt enters RUNNING and awaits the callback; non-2xx ⇒
  **the attempt fails immediately** (the worker refused the job).

---

## Round 8 — Rulings (2026-08-10)

| # | Topic | Ruling |
|---|---|---|
| R8-a | `callback_url` in sync mode | **Omit the field entirely.** Not an empty string — an empty string is a URL-shaped slot that invites a worker to use it, forcing us to define what happens then. Unambiguous because R2-4 already requires every message to carry an explicit `connection_mode`: `sync` ⇒ no `callback_url`, reply over the HTTP response; `async` ⇒ always present |
| R8-b | Read API surface | **Adopted, conditional on R9-1.** `GET /runs/{run_id}` · `GET /runs/{run_id}/steps` · `GET /steps/{step_id}` · `GET /steps/{step_id}/output`. The two-layer split is deliberate: the catalogue is cheap and fetchable whole, outputs may be large and are fetched individually, so a planner can pull only what it needs. `/output` returns the stored bytes verbatim, per R7-b. **If R9-1 removes `history` from M1, this surface must be revisited** |
| R8-c | `history` row fields | **Adopted:** `step_id`, `step_name`, `seq`, `status`, `output_bytes`, `completed_at`, **`attempt_count`**. `attempt_count` is included because it is the planner's *only* window onto failure history (attempts are not in the catalogue), letting it react to a flaky worker, at the cost of one already-existing integer |
| R8-d | `async` + `raw` | **Not supported in v2 core** — mechanically impossible (nowhere to put `callback_url`). → BACKLOG |
| R8-e | `retry_after_seconds` (R4-5's third knob) | **Not in v2 core → BACKLOG.** Judged by R7-l: it is an optional field in a response body; adding it later breaks no existing worker, touches no schema (`step_retry_delay_seconds` already exists) and touches no state machine. Contrast with R8-d / R9-Q1, which are *semantics* and would be breaking |
| R8-f | Which `dispatch_style` the core builds first | **`envelope` first.** It is the easier one to develop and the one with full functionality; `raw` stays at milestone θ as already ordered in R5-f |
| R8-g | `input_from` under `raw` — **Claude's proposal overruled** | Claude proposed treating a non-empty `input_from` in raw mode as a planner failure (invoking G2). **The owner rejected this: in raw mode we must faithfully accept what the planner sent and must not assume it is an error.** A warning was floated and dismissed as unnecessary extra machinery. Exact reading pending — see R9-Q1 |

### Standing instructions issued by the owner (2026-08-10)

| # | Instruction |
|---|---|
| S-1 | **This log is updated after every exchange, without exception.** All documents are written **in English**. If anything is uncertain, ask rather than assume |
| S-2 | The log records **what was discussed, what was eliminated, what was chosen, and the reasoning** — written plainly enough that ideas cannot contaminate one another later |
| S-3 | Every message field and every DB schema column must eventually be documented with its meaning and its conditional logic (e.g. which fields exist in which mode). **Where this lives is undecided** — `notes/` is the leading candidate, because SPEC should be the must-follow document and carry no noise, only necessary information. Open |

---

## Round 9 — Claude's analysis (input to SPEC; not itself spec)

### R9-1 · Should `history` be removed from the orchestrator → planner message?

Owner's proposal: send only `run_id` (plus `fetch_base_url`); the planner first calls the read API
for the history, then decides which step outputs it wants. Benefit claimed: small messages, no huge
payload ever pushed. Cost: one extra round trip per planner call.

**Size, measured.** `history` is a **catalogue only** — it never carries outputs; that is precisely
what `output_bytes` is for. One row is roughly 180 bytes of JSON, so it grows with the **number of
steps**, not with data volume: ~1.8 KB at 10 steps, ~9 KB at 50, ~36 KB at 200. The "avoid huge
messages" concern was already largely solved when the catalogue was designed.

Worth naming: the genuinely unbounded field in M1 is **not** `history`, it is `workflow_input` —
user-supplied JSON of arbitrary size, resent on **every** planner call.

**The decisive argument: fetching saves nothing for either kind of planner.**
- An **LLM planner** needs the history to decide, and will put it in its prompt. Moving it to a
  fetch removes zero bytes and zero tokens — the same data arrives by a longer path.
- A planner that needs **no** history (a static planner keyed on `seq`) is the built-in one, which
  is likely in-process and makes no HTTP call at all.

So the benefit column is, in this design, close to empty.

**The cost column has three real entries.**
1. **Reliability regression.** Today only planner calls that need outputs depend on the read API.
   Removing `history` makes **every** call depend on it. R4-4 already recorded that a planner
   failing because the read API was unreachable is indistinguishable from a broken planner — both
   burn `planner_max_attempts` and push the run to DLQ. This change promotes that failure mode from
   occasional to universal.
2. **Loss of self-containment — this hits R1-3 and R1-8 directly.** A captured request carrying
   `history` is a reproducible, inspectable record of what the planner saw. `{"run_id":"018f…"}` is
   a *pointer*: without a live orchestrator in the same state you cannot reconstruct the decision.
   The planner request is the single most valuable message in the system to be able to look at — it
   is the snapshot of what the system believed at that moment.
3. **Round trips double** on every planner call.

**The owner's instinct is right on one axis — unbounded growth — but the fix is a cap, not
removal.** That axis was already ruled in R2-8b ("history size limits: required eventually").

**Considered and eliminated:** a workflow-level knob `planner_history: inline | fetch`. Rejected —
it is a setting nobody asked for that forces two code paths to be maintained forever, and R1-5 puts
the burden of proof on inclusion.

---

## Round 9 — Rulings (2026-08-10)

| # | Topic | Ruling |
|---|---|---|
| R9-a | `history` in M1 | **Kept, and no knob is offered.** The message carries the catalogue as originally designed. **A cap is added now: the most recent N steps, with N = 100**, stated explicitly in the documentation. Making N configurable is a BACKLOG item |
| R9-b | Truncation contract | **Option (c): one sentence in the planner contract, no `history_truncated` field** — *"history MAY be truncated; a planner MUST NOT assume it is complete; use the read API for the full list."* A boolean field is the kind of extra machinery already rejected in R8-g; a sentence achieves the same contract expectation at zero cost. Since R9-a implements the cap now, this sentence describes actual behaviour rather than only future-proofing |
| R9-b′ | Consequence spotted by the owner | Because a planner must be able to fetch what the catalogue omits, **the orchestrator's read API is required early, not late.** SPEC must therefore state the **complete orchestrator API surface** clearly and up front |
| R9-c | `input_from` under `raw` — **R8-g reversed** | Two positions were distinguished and the owner ruled on both. **(i) A StepSpec-level `input_from` together with `dispatch_style: "raw"` is an error** — "not expected in raw form" — treated as a planner failure. This reverses R8-g and restores Claude's original G2-based proposal, after the two possible readings were laid out. **(ii) A key literally named `input_from` *inside* `params` is ordinary data** and is sent through verbatim, becoming a top-level key of the raw body; `raw` does not interpret payload content. **(iii)** The orchestrator → worker envelope has no `input_from` field at all, so that leg needs no rule |
| R9-d | Where field/schema documentation lives (discharges S-3) | **Split by voice, per R2-1.** **SPEC** carries the *normative* part — which fields exist, their types, whether they are required, and **which fields exist in which mode**. This is necessary information, not noise: without it a planner author cannot conform and SPEC is not implementable. **`notes/`** carries the *narrative* part — why a field exists, what was eliminated, worked examples, walkthroughs. Test: a field table is a MUST (SPEC); a walkthrough that aids understanding is notes. R1-3 already permits a short `Why:` on any non-obvious SPEC rule, so brief rationale stays; long derivations move out |
| R9-e | Repository and module path | **`git init` done now** at `/home/aaronwu/Projects/piton`; `docs/` moved to `legacy/`. Go module path **`github.com/aaronwu001/piton`** (matches the configured git user). No `go.mod` written yet — **Go is not installed on this machine**, and guessing a toolchain version is worse than writing nothing |
| R9-f | Delivery order for the document set | **SPEC skeleton first** (section headings + one sentence each) → owner reviews the architecture → content is filled in → an external spec review pass → only then does development start |

### Standing instruction added

| # | Instruction |
|---|---|
| S-4 | Documents must be written so that a newcomer can use them as reference material and understand them: complete architecture, clear narration, English throughout |

---

## Round 10 — Rulings (2026-08-22)

Context: the previous session's live context was lost. This round begins with Claude reconstructing
current document state from this log alone (per the standing rule that GRILLING_LOG, not memory, is
authoritative) and reading it back to the owner. The owner then asked that remaining open items be
converted into discrete, one-at-a-time decisions rather than open-ended discussion — the format this
round and Round 11 adopt.

| # | Topic | Ruling |
|---|---|---|
| R10-a | SPEC.md 19-section skeleton | **Approved as-is.** Content is now filled in section by section; each section is presented for explicit owner approval before the next is started (CLAUDE.md hard rule 1) |
| R10-b | §10.5 — `GET /runs/{run_id}/dlq` shape | **Kept as its own endpoint**, not merged into `GET /runs/{run_id}`. DLQ history can accumulate across replay rounds; a separate endpoint keeps the base run read cheap and fetches DLQ detail only when wanted |
| R10-c | §18 — Milestone α/β/γ/δ content and order | **Confirmed as reconstructed.** α = happy path (static planner, sync envelope worker, run reaches DONE); γ = retries/DLQ (kill a worker, watch attempts burn, land in DLQ); β = crash recovery (kill the orchestrator mid-run, resume; DLQ content untouched); δ = replay in its variants. Discharges the ⚠ in SPEC.md §18 |
| R10-d | "External review pass" (R9-f, previously undefined) | **Resolved: it means the owner personally re-reading the complete filled-in SPEC end to end, after every section has already been individually approved — not a separate reviewer, tool, or process** |

---

## Round 11 — Rulings (2026-08-22)

Context: continuation of Round 10's one-question-at-a-time format, covering deployment and test
infrastructure — a topic with **zero prior discussion anywhere in this log** before this round.
Raised by the owner: orchestrator and database each run in their own container; the database choice
(Postgres today) is itself a config-file value, in support of the storage abstraction layer already
ratified in R7-b; demo scenarios need a config file plus a run script usable by hand from a terminal;
automated tests need a managed lifecycle around that same config.

| # | Topic | Ruling |
|---|---|---|
| R11-a | Automated test environment lifecycle | **Grouped by test file / scenario.** One docker-compose environment is brought up per test file (or per milestone scenario), every test in that file/group runs against it, then it is torn down (including a volume wipe, so the next group starts clean) before the next file/group starts. Groups never share a live environment with each other. Concurrency/fencing tests — which manipulate global coordination state (`runs.owner_id`, `orchestrators`) — form their own group and are never interleaved with unrelated tests |
| R11-b | Where this content lives | **Split by voice, following the existing document separation.** Deployment shape and config-file structure — orchestrator/DB container topology, the config file that declares the storage backend, one docker-compose definition per milestone/scenario — is **system architecture and belongs in `SPEC.md`**, extending §4.4. How automated tests are run against that shape — the grouped-lifecycle rule of R11-a, test scripts, teardown discipline — is **development methodology and belongs in `CLAUDE.md`**, extending §5 (Testing discipline) |
| R11-c | Demo/test config-file organization | **Adopted.** One directory per milestone/scenario, holding one `docker-compose.yml` plus one hand-run demo script; automated test scripts reference that same compose file rather than each defining their own environment. The owner's manual demo run and the automated suite run against the identical environment definition |
| R11-d | Timing | **Decide the big-picture architecture now** — container split, config-file organization principle, the test-lifecycle grouping rule — as part of the ongoing section-by-section SPEC/CLAUDE.md fill. **Defer specifics** — exact services/images per milestone, any future Kubernetes manifests, per-scenario detail — to when Milestone α implementation actually starts |

---

## Round 12 — Rulings (2026-08-22)

Context: the owner reframed the immediate goal — **reach a demoable alpha quickly**, filling in only
what the alpha needs, *except* that any architecture later features depend on must still be settled
and built now. Claude analysed the skeleton against this log and found that the §3 admission test
("would adding this later be awkward?") admits nearly every section: definitions, the state table,
the schema, the wire formats and the concurrency rules are all either load-bearing for α or
contracts other people build against. Only per-milestone demo detail failed the test. The owner
therefore chose to have the whole SPEC filled in at once and reviewed in a single pass.

| # | Topic | Ruling |
|---|---|---|
| R12-a | **Section-by-section approval (R10-a) is overruled** | SPEC is filled in **completely in one pass** and reviewed by the owner in one sitting. Reason given: speed toward a demoable alpha. R10-a's protection is not abandoned — the owner may still reject any individual section and have it redone in isolation; what is dropped is the requirement to approve each section *before the next is written* |
| R12-b | `step_max_attempts` / `planner_max_attempts` edge case (the open question left by R7-e) | **A value below 1 is rejected at submission time with a 400.** This is the G2 principle (R4-10) applied to configuration: it removes the need to define what "zero attempts" would mean, because a step that is created but never executed has no useful semantics. → `SPEC.md § 11.1`, `§ 16` |
| R12-c | Demo scripts for milestones other than α | **Only α's demo script is written now.** The other eight keep the R5-f order table plus a one-line capability statement. Reason: a demo script is an operational verification artefact, not an architectural commitment — writing the later ones now would mean inventing detail that no ruling covers, and CLAUDE.md §4 already places the demo script at the start of each milestone's own work |
| R12-d | Scope of the fill | Every section of the 19-section skeleton is filled from rulings already in this log. Where this log was **silent** and α or the schema needed an answer, Claude made the minimal inference, marked it **⚠ in place**, and collected all of them in `SPEC.md § 19.1` as O1–O8 for an explicit ruling. **These eight are the only places where SPEC contains something this log did not decide** |
| R12-e | R11-a transcribed into CLAUDE.md | Per R11-b's ruling that test-lifecycle discipline belongs in `CLAUDE.md § 5`, R11-a is written there as **§ 5.5 Test environment lifecycle**. `SPEC.md § 4.4` carries the environment *shape*; CLAUDE.md carries how the suite is run against it |

### Derivations worth recording (why certain columns exist the way they do)

These were not separate rulings; they fell out of existing ones while writing §6, and are recorded
so they are not re-litigated:

| Finding | Consequence |
|---|---|
| A cancelled attempt does not burn budget (R5-5's uniform cancel rule), so `steps.attempt_count` and `COUNT(attempts)` legitimately differ | `attempt_count` **must** be a stored column, not a derived count. Deriving it would make cancellation silently consume budget |
| The R7-d exception permits a non-owner callback to write **only** the `attempts` table | `attempts.output` **must** exist. A successful async callback landing on a non-owner needs somewhere to deposit the result; the owner promotes it to `steps.output` on its next poll |
| The R4-11 claim-time refinement (expire a sync attempt immediately, let an async attempt run to its deadline) is evaluated by a *new* owner | `connection_mode` **must** be persisted on the attempt row, not only inside `steps.decision` |
| The callback endpoint is addressed by `attempt_id` alone | `attempts.run_id` is denormalised so the handler locates the run without a join |

---

## Round 13 — Rulings (2026-08-22)

Context: the owner directed a read of `~/Projects/StateFlow` — the old project's **complete
implementation** (one migration, ~10 Go files under `internal/{core,planner,orchestrator,transport,store,api}`)
— to test the inferences Round 12 had marked ⚠, and specifically the areas Claude had flagged as
needing careful review: O1–O8, the §6 column lists, and §4.2's commit ordering.

**Method, and the rule that governed it (CLAUDE.md §7).** The old code was read as *evidence that a
shape is implementable*, never as *a reason Piton behaves a certain way*. Nothing below is in SPEC
because StateFlow did it. Where the two agree, the agreement is recorded as independent convergence;
where SPEC gained something, the justification is stated in Piton's own terms and the old code
appears only as the illustration of what goes wrong without it.

### Independent convergence — inferences that survived, unchanged

The old implementation, under different design pressure and with no lease, no persisted deadline and
no cancellation, nevertheless arrived at the same shape in these places. **No SPEC change.**

| SPEC | What the old implementation does |
|---|---|
| §4.2 — the attempt row commits **before** the outbound HTTP call | Identical, and deliberate there too: `loop.go:286-295` labels it "Barrier 1 (TX1): persist decision + first attempt BEFORE dispatch"; TX1 commits at `postgres.go:173` and only then does `loop.go:356` dispatch. The retry and replay paths keep the same order |
| §11.1 — `step_max_attempts` is a **total** count | `postgres.go:306-318`: counter starts at 0, +1 per failure, `>= retryLimit` ⇒ DLQ. So limit 1 = one dispatch, no retry. **Their own USER_MANUAL.md:236 documented it as `(X+1)`, contradicting their code** — a direct instance of the confusion R7-e demanded SPEC state loudly |
| O1 — static plan as an ordered array inside the workflow row | `planner/static.go`: steps live in the workflow's config JSON; `Decide` indexes by the count of completed steps; past the end it answers `done`. No separate table, no file on disk |
| O7 — retry delay is in-memory and not preserved across a crash | `loop.go:419-429` uses `time.After`; there is no column. After a restart they skip the wait entirely, reasoning that "the crash itself already provided more than enough cooldown" |
| O2 — a distinct "reply could not be parsed" failure class is needed | They call it `malformed`, one of four CHECK-constrained values |
| §6.3 — `attempt_count` stored on the step, not counted from `attempts` | Same column, incremented in the failure transaction |
| §14 — replay resets `attempt_count` to 0 | Same |
| §12.1 — a planner `fail` verdict consumes no budget | Same |

### Rulings

| # | Topic | Ruling |
|---|---|---|
| R13-a | O3 — the planner budget column | **Promoted from inference to a specified rule** (§6.2). The old design's planner budget is a loop counter in memory that resets on restart, so a crash loop against a broken planner **never converges to DLQ**. SPEC §12.2 asserts unbounded retry is structurally impossible; that assertion is only true because this counter is persisted. It is §1 applied to a budget, not a preference. **O3 is discharged** |
| R13-b | `runs.updated_at` / `steps.updated_at` | **Both deleted.** A column that every business transaction must remember to write is the same failure mode §8.2 rejects for the ownership predicate — one forgotten transaction and it silently lies. The old schema proves it is not hypothetical: their step-creation and step-success transactions never touch the `runs` row, so `runs.updated_at` does not advance while a run makes progress. `created_at`, `steps.completed_at`, `attempts.started_at`/`finished_at` and `dead_letter_queue.created_at` each record one specific event and cannot drift |
| R13-c | Dead-letter granularity | **`kind ∈ {worker, planner}` replaced by a five-value `reason`**: `worker_budget_exhausted`, `planner_unreachable`, `planner_invalid_response`, `planner_budget_exhausted`, `planner_declared_fail`. The three planner causes demand three different responses from the operator — fix the network, fix the planner, accept its judgement — and free text cannot be filtered. Admitted now rather than later because `dead_letter_queue` is append-only: a column added after the fact is permanently blank for exactly the history that mattered. `step_id IS NULL` still distinguishes the two sides, so no second column |
| R13-d | Unbounded run length | **Published as a non-guarantee in §19.2, not fixed.** A planner that answers `continue` forever is never caught by any budget, because the budget counts failures and it is not failing. A `max_steps` limit is an optional config field, so by the §3 admission test it goes to BACKLOG; what would be costly is discovering the property later and reading it as a defect |
| R13-e | `status` ↔ `failure_reason` | **Made a backend-enforced invariant on `attempts`** (§6.4): FAILED ⇒ reason present, not-FAILED ⇒ reason absent, plus `finished_at` present iff not RUNNING. §17 promises the database explains itself; a FAILED row with no reason breaks that promise, and a reason on a DONE row is a contradiction a later reader will believe |
| R13-f | Completion signal | **§6.3 now states that completion is `status = 'DONE'` and nothing else** — no rule or query may read "output is present" as "the step finished". A worker may legitimately return the JSON document `null`, and a backend may encode that indistinguishably from absent. The old schema documented output-nullness *as* its completion test while its own writer substituted a JSON `null` for an empty value |
| R13-g | `timeout` vs `transport_error` | **Definition tightened in §5.3: the clock decides, not the error's shape.** `timeout` means `deadline_at` has passed; anything before that is `transport_error`. The old loop classified every dispatch error — connection refused included — as `timeout`, because under a deadline-bearing context both surface as one error from one call. The operator then misreads a dead worker as a slow one, and the two point at different fixes |
| R13-h | `attempts.dispatch_style` | **Removed.** §8.6's claim-time rule branches on `connection_mode` and must do so in SQL before any decision document is parsed, so that column stays. Nothing branches on `dispatch_style` outside dispatching, where `steps.decision` has necessarily already been read |
| R13-i | Static planner steps | **Each element of `planner_static_steps` is a StepSpec and is validated as one, by §9.4 and §9.8, at `POST /workflows`** (§6.1). One type, one validator; a static step carrying `timeout_seconds` is a 400 until η exactly as from an HTTP planner. **§12.1 additionally states that the static planner needs no exemption from the budget rules** — it cannot fail at run time once its steps are validated at submission, so no special case should be written. The old project had both defects this closes: a reduced four-field static step form with unknown keys silently dropped, and a hard-coded branch skipping the planner budget for static planners. Their consequence was that an invalid static plan produced a run stuck `RUNNING` forever, reclaimed and re-failed by every sweep. **O8 is discharged** |
| R13-j | Milestone α's endpoint scope | **§18.1 now names the five endpoints α implements.** §10.2's full read surface stays in SPEC as a contract (R9-b′), but α's planner is in-process and fetches nothing, so the remaining read endpoints land with ζ — the first milestone with a planner able to call them |

### Divergences confirmed as intended — recorded so they are not re-opened

Each of these is a place where Piton deliberately differs, and seeing the old code made the cost of
the old choice concrete:

| Piton | Old implementation | Consequence there |
|---|---|---|
| `attempts.deadline_at` (R4-a) | No column; a Go-local `time.Now()` plus a context deadline, explicitly "never persisted" | The timer dies with the process; `orphaned` had to become a real recovery mechanism rather than a label |
| Lease + `orchestrators` table (R7-a) | An in-process `map[RunID]struct{}`, self-described as not needing to survive a restart | Single replica by construction |
| Async result delivered through a row (R7-d) | An in-memory channel registry keyed by **step id**, self-described as: after a restart, recovery re-dispatches rather than reconnecting | **Async work already completed is discarded on crash.** This is what §8.4's `attempts.output` exists to prevent |
| `runs.replay_count` (R5-d) | Absent; the round is inferred from the newest dead-letter row's timestamp | Exactly the reconstruct-by-timestamp approach R5-4 rejected |
| UUID identity, optional `step_name` (R2-6) | `step_id` is literally `"{run_id}:{step_name}"`, name NOT NULL | Two steps sharing a name collide on the primary key; the async handler recovers a run id by string-splitting a step id |
| `steps(run_id, seq)` **unique** (§7.2) | No unique constraint, only a non-unique composite index | Uniqueness rests entirely on "one goroutine per run" — untrue the moment a second replica exists |
| Symmetric `step_*` / `planner_*` knobs (R7-e) | Three competing retry settings, one of which is dead code whose computed verdict is discarded at the call site | The "you cannot tell what governs what" failure R7-e was reacting to |
| `output_field` cut (R7-g) | Present, sync-only, top-level key lookup | — |

---

## Round 14 — Rulings (2026-08-24)

Context: Claude listed the remaining ⚠ inferences as discrete questions. The owner ruled on five and
sent three back as not clearly enough posed (the static-planner definition, retry-delay persistence,
and what `overrides` was for). Those three are carried into Round 15.

| # | Topic | Ruling |
|---|---|---|
| R14-a | `invalid_response` (was O2) | **Kept as its own `failure_reason`.** The three values name three different repairs — the worker's business logic, its output format, the network — and the operator chooses his next move from this column. Adding an enumerated value later would leave every earlier attempt mis-classified. **O2 discharged.** The name `invalid_response` over the old project's `malformed` was not separately ruled; Claude kept the more self-describing one |
| R14-b | `error_text` truncation (was O4) | **4 KB**, and **recorded as a future client-adjustable setting** → `BACKLOG.md` B16. Interpretation Claude applied where the ruling was silent: **one limit covers both `attempts.error_text` and `dead_letter_queue.error_text`**, and truncation is the orchestrator's job, never the backend's. **O4 discharged** |
| R14-c | Unknown top-level key in a StepSpec (was O5) | **A planner failure.** Owner's reasoning: the planner did not write the fields the orchestrator expects. The forward-compatibility cost was stated in the question and accepted — a planner that adds a field of its own breaks against an orchestrator that has not learned it yet, with no escape hatch. **O5 discharged** |
| R14-d | Error response body (was O6) | **Carries both entities: `run_id` + `run_status` **and** `step_id` + `step_status`.** Owner: "we would rather record more." Interpretation Claude applied: a rejection carries the identifier and status of every entity the request named or would have touched, **omitting only those that do not exist** — a `POST /workflows` rejection has no run to describe. `error` is a stable machine-readable slug, `message` is human-readable and may change. **O6 discharged** |
| R14-e | Sweep interval | **5 seconds**, and **recorded as a future client-adjustable setting** → `BACKLOG.md` B17. It is the width of §13.3's uncertainty window, so it is chosen to be negligible against any realistic `step_timeout_seconds` |

### Carried into Round 15

| # | Question | Why it was returned |
|---|---|---|
| R14-Q1 | The static planner's definition (O1) | The question was posed as a storage-layout choice without first stating what a static planner *is* and how it contrasts with the HTTP one. Re-asked with the model stated and the specific gaps named |
| R14-Q2 | Retry-delay persistence (O7) | The question did not make clear that the two options differ **only** when the orchestrator crashes mid-delay. Re-asked as a timeline |
| R14-Q3 | Whether `runs.overrides` needs a column now | The owner did not recall what `overrides` was for. It is R2-9 / R7-f's config layering; re-asked with that context restored |

---

## Round 15 — Rulings (2026-08-24)

| # | Topic | Ruling |
|---|---|---|
| R15-a | Retry-delay persistence (was O7, R14-Q2) | **Not persisted.** The delay lives in the driver's memory; a crash discards its remainder and the next owner dispatches immediately. Owner's reasoning: *"we do not need to guarantee too much right now — only that the core functions work."* The cost is written into §11.1 rather than hidden: a delay that existed because a worker was rate-limiting disappears exactly when the worker can least afford it. The remedy is named there too — a nullable `steps.next_attempt_at` where `NULL` means "dispatchable now", addable later with no backfill and no change of meaning. **O7 discharged, and §19.1 now holds only O1** |
| R15-b | `runs.overrides` column (R14-Q3) | **No column until η.** The API field stays exactly as R7-f ruled — `overrides` is accepted in the run-creation body and any non-empty value is a 400 — because a request shape is a contract others build against. The *column* is invisible to every caller, can hold nothing before η by construction, and is a nullable addition whenever wanted; creating it now would only show an operator a permanently empty column. **This reverses Claude's earlier inference**, which had leaned on R2-8c; that ruling's reasoning was that an early milestone should demo with `retry_limit = 0` rather than a missing field — i.e. the *field* keeps demos consistent — and `overrides` has no demo to be consistent with |

Note on where R15-b was recorded: run-level overrides are **scheduled work** (milestone η), so the
statement lives in `SPEC.md § 11.2`, not in `BACKLOG.md`. `BACKLOG.md` is for unscheduled items;
putting a milestone's content there would invert the separation R5-e established. This differs from
R14-b and R14-e, whose "make it configurable later" halves went to BACKLOG precisely because they
are not scheduled.



---

## Round 16 — Claude's analysis (input; not itself spec)

Context: the owner asked (a) where the project stands, (b) what the next step is, and (c) whether
his intended working loop — *"focus on one unit at a time; Claude writes tests from SPEC; owner
reviews the tests; Claude implements to meet them; owner reviews the implementation; then deploy and
run the demo"* — matches the flow already agreed in `CLAUDE.md § 4`. No ruling was issued; this
entry records the assessment put to him.

### R16-1 · State of the repository

| Thing | State |
|---|---|
| `SPEC.md` | Fully written, §0–§19 + Appendix A. Exactly **one** ⚠ remains: `planner_static_steps` / the static-planner definition (§6.1, §19.1 O1) |
| `GRILLING_LOG.md` | Through Round 15 (2026-08-24) |
| `BACKLOG.md` | Populated, B1–B17 |
| Code | **None.** No `go.mod`, no `demos/`, no migrations. Go is not installed |
| Git | One commit (the skeleton). All four documents, including the fully-written SPEC, are uncommitted |
| `temp` | Stale — dated 2026-08-10, still describes SPEC as a skeleton. Not an authority (§1 `notes/` rule) and should be deleted or rewritten |

### R16-2 · What blocks milestone α under `CLAUDE.md § 4` step 1

1. **O1 is the static planner**, which is α's planner. §4 step 1 forbids starting a milestone whose
   SPEC dependency is unratified. §6.1 already carries a written-out inferred definition; the owner
   need only confirm or amend it.
2. **Toolchain.** `CLAUDE.md § 8` requires Go installed and `go.mod` written with a real version.
   Unresolved alongside it: the working tree is a WSL path reached over UNC while the shell is
   Windows-side, so where Go, Docker and `golang-migrate` actually execute has to be settled once.
3. The documents are uncommitted.

### R16-3 · The owner's loop measured against `CLAUDE.md § 4`

**Matches:** tests derived from SPEC before the implementation exists, and the owner reviewing those
tests before implementation starts, are §4 steps 3–4 and §5.1 exactly. The ordering is what stops
the §5.1 table's third row — a test written by reading the code — from ever occurring.

**Three divergences named:**

| # | Divergence | Rule it meets |
|---|---|---|
| a | **"Unit."** §4's unit of work is a *milestone — a demo scenario*, deliberately not a layer. If "unit" means storage layer, then driver, then HTTP, the checkpoint becomes "the storage tests are green", which §4 step 5 says is not evidence a milestone landed. Slicing work inside α is fine; slicing the *checkpoint* is not | §4 |
| b | **The demo script comes before the tests**, not after. α's script exists as prose in §18.1, but `demos/alpha/` — `docker-compose.yml`, `piton.yaml`, `workflow.json`, `demo.sh` — does not exist, and §5.5.4 requires the automated suite to reference that same compose file. The environment is therefore a *precondition* of writing tests, not a deployment step that follows them | §4 step 2, §4.4, §5.5.4 |
| c | **"Owner reviews the implementation, then deploy"** omits §4 step 5: the owner **runs the demo by hand and inspects database truth from a terminal**. Neither a code review nor a green suite substitutes for it. The demo is the acceptance gate, not the victory lap | §4 step 5, §6 |

### R16-4 · Review granularity — the recommendation put to the owner

A full write-tests → review → implement → review cycle per code unit would cost four or five
separate owner reviews across α, and α's tests are dominated by storage and concurrency cases that
are mutually entangled, so each review would rebuild context already built. The recommendation was
**one review of α's whole derived test list**, then batched implementation, with the §5.5 test
groups (schema/CRUD; claiming-fence-CAS-sweep in isolation; validation; end-to-end) as the fallback
seams if the owner wants tighter control. Awaiting his decision.

## Round 16 — Rulings (2026-09-01)

| # | Topic | Ruling |
|---|---|---|
| R16-a | Where Go, Docker and the toolchain run | **Inside WSL.** The Windows side is an editor only. The owner further ruled that **this does not belong in `SPEC.md`** — it is a fact about this machine, not behaviour of the system — so it was recorded in `CLAUDE.md § 8` instead. A conforming Piton does not care what OS built it |

Carried: O1 was re-put to the owner in plain terms (what "ratified", "O1" and "ruling" each mean in
this project's vocabulary), with §6.1's inferred static-planner definition quoted verbatim for a
yes/amend answer. Not yet ruled.

---

## Round 17 — Claude's analysis (input to SPEC; not itself spec)

Context: the owner ruled that **the HTTP planner is built first and the static planner is deferred**,
and asked for the trade-off between declaring the planner-kind discriminator now versus adding it
later. No SPEC edit has been made; three questions were put back to him.

### R17-1 · The field already exists

`planner_type` is already in `SPEC.md § 6.1` and already in §16's submission-time validation list.
The owner's instinct matched what was written, so "design it now vs later" carries no change cost
here. The live questions are narrower: what the value is called, what `static` returns today, and
which columns are created now.

### R17-2 · Why declaring it now is right anyway (the analysis he asked for)

| | Declare now, accept one value | Add the field later |
|---|---|---|
| §3 admission test | **Yes — awkward later.** It is a discriminator in a request body that operators and their scripts build against | Adding a required discriminator to a published request shape is a breaking change |
| Validation posture | §16 is a whitelist from day one: an unknown `planner_type` is a 400, including the typo `"htp"` | Day-one orchestrator is silent about planner kind; when `static` arrives, an existing workflow's kind must be *inferred* (has `planner_url`, therefore http) — inferred state where declared state was available |
| Cost | An enum with one legal value reads as unfinished, and one dead rejection branch is carried for a few milestones. A default (`http` when omitted) removes the ergonomic cost | — |

**Precedent it follows:** R15-b's `overrides` — the API field ships now and an illegal value is a
400, while the *column* waits. The same split applies here with one refinement: `planner_type` and
`planner_url` need real columns immediately because the HTTP planner uses them, whereas
**`planner_static_steps` should not be created until the static milestone** — by R15-b's own
reasoning, a column that can hold nothing by construction only shows an operator a permanently empty
column. This also means **O1 stops blocking development**: with static out of α, the static
planner's definition becomes a question for a later milestone.

### R17-3 · Naming — `http` recommended over `dynamic`

`dynamic` names the behaviour; `http` names the mechanism. Every other enum in this document names
the mechanism — `connection_mode: sync|async`, `dispatch_style: envelope|raw`. `dynamic` is also
inaccurate as an opposite of `static`: the static planner is *also* asked once per step; what
differs is where the answer comes from — a URL, or an array inside the workflow. Recommended, not
ruled.

### R17-4 · The decision enlarges α, and that must be said plainly

The static planner was first *because* it runs in process, cannot fail, and is fully validated at
submission. Swapping it out pulls into α: a fourth container for the planner; the M1/M2 wire
protocol (§9.2, §9.3), which α previously did not touch at all; run-time StepSpec validation (§9.8)
that submission-time validation had made unnecessary; and the possibility of planner failure, which
brings `planner_max_attempts`, `runs.planner_attempt_count` and planner-side DLQ (L5) — §12.2 and
§12.3, previously γ's content.

Two ways to bound it were put to the owner:

- **A — α stays happy-path only.** Planner failure is left to γ. Rejected in the recommendation: a
  mistyped `planner_url` on day one leaves a run sitting `RUNNING` forever, reclaimed and re-failed
  by every sweep — precisely the state §6.1's *"Why validation happens at submission"* paragraph
  exists to make unreachable.
- **B — α includes the planner budget and planner-side DLQ (L5).** α grows by roughly a third and
  gains a second demo leg ("point `planner_url` at a dead address, watch the run land in DLQ"); γ
  is then reduced to the worker-side half. **Recommended.**

### R17-5 · Carried to the owner

1. `http` or `dynamic` as the value name.
2. Bound α by A or B.
3. Where the static planner goes in `SPEC.md § 18` — swapped with ζ, moved to the end, or out of the
   milestone list entirely — and what `planner_type: "static"` returns until then (recommended: 400,
   `"not yet supported"`).

---

## Round 18 — Rulings (2026-09-01)

| # | Topic | Ruling |
|---|---|---|
| R18-a | Planner order | **Reversed back: the static planner stays in milestone α and the HTTP planner stays at ζ**, as `SPEC.md § 18` already has it. The owner had briefly ruled the opposite in Round 17 and withdrew it on recalling why static was first. **Round 17 is superseded in full and left in place** (`CLAUDE.md § 1`: superseded entries are not deleted, and the log is never an authority). Its three carried questions — the `http`/`dynamic` naming, bounding α by A or B, and where static sits — all lapse: SPEC as written already answers them |
| R18-b | Development starts | **Now.** No SPEC edit results from this round; §6.1, §16, §18.1 stand unchanged |

Note: O1 (§19.1, the static planner's definition) therefore returns to blocking α, exactly as
R16-2 described. It was re-put to the owner in plain terms at the end of Round 16 and has still not
been ruled on. The owner then asked, before starting, for a plain-language walkthrough of the data
model, of where a static step lives inside it, and of whether the static planner is a server or a
file — which is, in substance, the same question O1 asks. His answer to that walkthrough is expected
to discharge O1.

---

## Round 19 — Rulings (2026-09-01)

| # | Topic | Ruling |
|---|---|---|
| R19-a | **O1 — the static planner's definition** | **Ratified.** Put to the owner in plain language: `planner_static_steps` is an ordered array of StepSpecs living on the `workflows` row; a `steps` row is what one of those elements produces when executed, and `steps.decision` stores that element verbatim; the planner itself is **neither a server nor a file** — it is a function inside the orchestrator process that returns `planner_static_steps[n]` for `n` = the run's current step count, `done` at the end, never `fail`; `workflow.json` is only the transport for the operator's `curl`. Owner: *"這樣沒錯."* **O1 discharged — §19.1 is now empty, and no unratified inference remains anywhere in SPEC** |
| R19-b | Transcription | On that ruling, `SPEC.md § 6.1` lost its ⚠ marker and §19.1 was rewritten to record that nothing is outstanding. **No behavioural text changed** — the definition ratified is the one that was already written |

### Machine setup performed (not spec; recorded because `CLAUDE.md § 8` flagged it as a blocker)

- **Go 1.27.0** (released 2026-08-18) installed to `/usr/local/go` inside WSL Ubuntu 26.04.
  SHA-256 `675c26c4…0685` verified against `go.dev/dl`'s published value. `PATH` extended via
  `/etc/profile.d/go.sh`.
- **Docker is not usable yet.** Docker Desktop is installed on the Windows side but is not running,
  and WSL integration is not enabled for this distro, so no `docker` binary exists inside Ubuntu.
  This blocks everything in `CLAUDE.md § 5.3` — every ownership, claiming, fencing, CAS and sweep
  test must run against a real Postgres from `docker compose`. Put to the owner as a choice between
  enabling Docker Desktop's WSL integration and installing Docker Engine natively inside WSL.

## Round 19 — Environment established (2026-09-01)

| # | Topic | Ruling / outcome |
|---|---|---|
| R19-c | Docker | **Docker Engine installed natively inside WSL**, not Docker Desktop's WSL integration. Options put to the owner were (a) native `apt` install of `docker-ce`, (b) ticking WSL integration in Docker Desktop. He chose (a). Reasoning offered and accepted: the demo and the suite then depend on nothing outside the distro, which is what `CLAUDE.md § 8`'s "everything executes inside WSL" already commits to, and it removes a step that can be forgotten before every session. Cost stated: two `docker` binaries exist on `PATH`, the native one winning |

**Installed and verified:** Go 1.27.0 (`/usr/local/go`, SHA-256 verified against `go.dev/dl`);
Docker Engine 29.7.2 and Compose v5.5.0 from Docker's own apt repository, under systemd so it starts
with the distro; `aaronwu` added to the `docker` group. `CLAUDE.md § 8`'s ⚠ is discharged and the
section now records what is installed.

**Toolchain proven end to end, not merely installed:**

- `go mod init github.com/aaronwu001/piton` → `go.mod` declaring `go 1.27.0`, a real version.
- `cmd/piton/main.go` — a placeholder entry point — passes `go vet ./...`, builds and runs.
- A throwaway `docker compose` environment brought PostgreSQL 18.6 up to a healthy state, and three
  things `SPEC.md` depends on were exercised against it rather than assumed:
  - a **partial index** (`CREATE INDEX … WHERE status = 'RUNNING'`) — §7.2's sweep index;
  - `SELECT … FOR UPDATE` — §7.3 obligation 2;
  - the **CAS primitive** of §8.1: the first `UPDATE … WHERE status='RUNNING' RETURNING` returned
    one row, the second returned `UPDATE 0` and zero rows. *"Zero rows affected means the
    expectation was wrong, and is not an error condition — it is the answer."* Confirmed on the real
    engine.
- Environment then torn down with `docker compose down -v`, the volume wipe `CLAUDE.md § 5.5`
  requires between groups.

Not done, deliberately: `demos/alpha/` and the migrations. They are the next step, and the compose
file used above was a throwaway precisely so that `demos/alpha/docker-compose.yml` is written once,
as the real one the suite will reference (§5.5.4). Nothing has been committed — the four modified
documents plus `go.mod`, `.gitignore` and `cmd/` are all still in the working tree.

---

## Round 20 — Process confirmation (2026-09-02)

| # | Topic | Ruling / outcome |
|---|---|---|
| R20-a | **Development order** | Owner asked to confirm that the method is "tests first, then code". Confirmed against `CLAUDE.md § 4`, with the precision the question invited: the order is *ratified SPEC sections → demo script → tests derived from SPEC → implementation → owner's hand-run demo → automated suite*. Three points distinguish it from ordinary TDD and were stated back: (1) the binding constraint is the test's **provenance** (`§ 5.1` — SPEC only; never the implementation, never the old project, never an unratified spoken ruling), not the red/green cycle; (2) the **demo script precedes the tests**, not the other way round; (3) the acceptance evidence is step 5, the owner's manual run — `§ 4` states the suite cannot replace it, and a green suite the owner has never looked behind is not evidence a milestone landed. No rule was changed, nothing was eliminated, no SPEC text touched |

---

## Round 21 — `demos/alpha` walkthrough, and the `owner_id` question closed (2026-09-03)

**State of the tree at the start of this round.** `demos/alpha/` exists with its four files —
`docker-compose.yml`, `piton.yaml`, `workflow.json`, `demo.sh` — untracked, and this log carries no
entry for their creation. They are recorded here for the first time. No implementation exists beyond
`cmd/piton/main.go`'s placeholder, so `demo.sh` cannot pass yet; it is the *demo script* of
`CLAUDE.md § 4` step 2, written before the code (R20-a).

| # | Topic | Ruling / outcome |
|---|---|---|
| R21-a | **`runs.owner_id` on a `DONE` run** | `demo.sh` had displayed the column and deliberately asserted nothing about it, on the reasoning that `SPEC.md § 18.1` states no expectation and `§ 8.7` names claim / heartbeat / release as the only three writers of coordination metadata — none of which is the run reaching `DONE`. Owner ruled: **"a DONE run should have no owner_id"** |
| R21-b | **What R21-a actually changed** | *Nothing in the rules.* `SPEC.md § 6.2` already carries the invariant — "`owner_id` is non-`NULL` only while `status = 'RUNNING'`". The ruling confirms the invariant rather than adding one. The script was being over-cautious: it treated a ratified invariant as an open question because the *mechanism* producing it was unwritten |
| R21-c | **`demo.sh` changed accordingly** | Section 6 ("derived from SPEC, beyond 18.1's list") now asserts `owner_id IS NULL`, cited to `§ 6.2`, not to the spoken ruling — `CLAUDE.md § 5.1` permits SPEC as a test's only source. Section 4's display comment and the header block were rewritten to match. `claimed_at` is **not** asserted: `§ 6.2`'s invariant names `owner_id` only, and `§ 14`'s replay clears `owner_id` only, so asserting the pair would be inventing a rule (`CLAUDE.md § 9`) |
| R21-d | **Open — a proposed `SPEC.md § 8.7` amendment** | The invariant holds but no section says *what writes it*. `§ 8.7`: "exactly three operations may write coordination metadata — claim, heartbeat and release." `§ 14` (replay) and `§ 15` (cancel) each separately state that they clear `owner_id`, so the sentence already has two exceptions living outside it. The `DONE` transition is a third and is written nowhere. **Proposed, not applied** (`CLAUDE.md § 2` rule 1): `§ 8.7` gains the `DONE` transition as a writer of `owner_id = NULL`, and the question of whether `claimed_at` is cleared with it is answered at the same time. Awaiting the owner's ruling; no SPEC text touched |
| R21-e | **Demo script vs automated suite** | Owner asked whether the tests are not already finished. Distinction restated: `demo.sh` is `CLAUDE.md § 4` step 2 — one happy-path run, the operator's hand-run artefact, its assertions existing so an unattended run has a verdict. The **automated suite** is steps 3 and 6 — Go tests derived from SPEC whose job is to guarantee that what the owner saw by hand *stays* true, governed by `CLAUDE.md § 5.5` (one compose environment per test file, volume wipe between groups, concurrency and fencing tests in their own group, and § 5.5.4's requirement that they reference `demos/alpha/docker-compose.yml` itself). No ruling sought; nothing changed |

---

## Round 22 — The `owner_id` amendment drafted, and a wider gap found (2026-09-03)

| # | Topic | Ruling / outcome |
|---|---|---|
| R22-a | **R21-d question 1** | Owner ruled: **yes** — the `DONE` transition clears `owner_id`, and `§ 8.7` says so |
| R22-b | **R21-d question 2** | Owner ruled: **yes** — `claimed_at` is cleared with it. Reasoning offered and accepted: without an owner, `claimed_at` is a timestamp pointing at a nonexistent claim, and a SPEC that gives three different answers for the same pair in three places guarantees one implementation site gets it wrong |
| R22-c | **Found while drafting: the ruling cannot stop at `DONE`** | `§ 6.2`'s invariant is *"`owner_id` is non-`NULL` only while `status = 'RUNNING'`"* — it is scoped to the status, not to `DONE`. A run reaching **`DLQ`** (`§ 12.2`, worker-side and planner-side alike) therefore violates the invariant just as a `DONE` run would, and no section clears the pair there either. `CANCELLED` is the only exit already covered, and it covers `owner_id` alone. A `DONE`-only amendment would leave SPEC self-contradictory on the `DLQ` path. The draft is therefore written in the general form — **every transition of a run out of `RUNNING` clears the pair** — which covers `DONE`, `DLQ` and `CANCELLED` in one sentence. This is a widening of what R22-a asked for and is flagged as such; put to the owner |
| R22-d | **Supporting observation, offered as reasoning not as a rule** | Clearing the pair inside the same transaction that makes a run terminal means the driver that wrote it can never take `§ 8.2`'s ownership fence on that run again — its next `SELECT … WHERE owner_id = :me` returns zero rows and it stops silently, which is what `§ 4.2` step 1 already prescribes. The clearing is thus not merely cosmetic tidying of a coordination column; it is the fence agreeing with the state machine |
| R22-e | **Status** | Draft written and shown to the owner; **`SPEC.md` not touched** (`CLAUDE.md § 2` rule 1). Five sites are affected: `§ 8.7` (the "exactly three operations" paragraph), `§ 6.2` (the invariant), `§ 14` (replay), `§ 15` (the cancel statement), and one sentence in `§ 4.3`. Awaiting the owner's nod before any edit |

---

## Round 23 — The amendment landed, and alpha's automated suite written (2026-09-03)

| # | Topic | Ruling / outcome |
|---|---|---|
| R23-a | **R22-c: the general form** | Owner agreed. `SPEC.md` amended in five places so that the rule is stated once and the sites that used to state fragments of it now point at it |
| R23-b | **`SPEC.md § 8.7`** | The paragraph "exactly **three** operations may write coordination metadata" is replaced by a four-row table — **claim**, **heartbeat**, **release**, and **any transition of a run out of `RUNNING`** — the fourth clearing `owner_id` and `claimed_at` in the same transaction as the status change. Two `Why:` paragraphs added: (1) a separate tidy-up pass is a rule that must be remembered at every call site, the class of mistake `§ 8.2` already rejects; (2) the driver that writes the terminal status clears its own ownership, so its next fence returns zero rows and it stops silently — the fence and the state machine agree by construction. Cancellation's clearing is no longer described as "belt-and-braces"; it is this rule applied to one of the three exits |
| R23-c | **`SPEC.md § 6.2`** | Invariant widened to the pair: "`owner_id` and `claimed_at` are non-`NULL` only while `status = 'RUNNING'`, and are always written and cleared as a pair (§8.7)" |
| R23-d | **`SPEC.md § 14`, `§ 15`, `§ 4.3`** | Replay now clears both, with a note that under the new `§ 8.7` a `DLQ` run already holds both as `NULL` so the sentence is a restatement and not a second mechanism. The cancel statement gains `claimed_at = NULL`. `§ 4.3`'s "changes only on the rare, meaningful events of claim and release" gains "and a run becoming terminal" |
| R23-e | **`demos/alpha/demo.sh`** | Section 6 now asserts both `owner_id IS NULL` and `claimed_at IS NULL`, cited to `§ 6.2` and `§ 8.7`. The header block that described the question as open is rewritten to describe it as settled |
| R23-f | **Milestone alpha's automated suite** | Written under `test/alpha/`: a `harness` package (compose lifecycle, `psql` through `docker compose exec`, the HTTP calls of `§ 18.1`, and readers for `piton.yaml` and `workflow.json`), and two groups — `happypath` and `ownership`. `run.sh` runs them with `-p 1` and `-count=1`; `README.md` states what the suite is and is not |
| R23-g | **Why two groups and not one** | `CLAUDE.md § 5.5.1` makes a group one compose environment, and a Go package is the unit that can own a `TestMain`, so one package is one group. `ownership` is separate for the reason `§ 5.5.3` gives: it asserts **global** coordination state — the whole `orchestrators` table and `runs.owner_id` across every run — and "exactly one orchestrator row" is only meaningful when nothing may start a second one. Go runs packages concurrently by default, so `-p 1` is what makes `§ 5.5.2` real rather than intended |
| R23-h | **How the suite reaches the database** | Through `docker compose exec postgres psql`, not a Go driver. `demos/alpha/docker-compose.yml` deliberately publishes no host port for Postgres, and `CLAUDE.md § 5.5.4` forbids the suite from defining an environment of its own to obtain one. Cost accepted: one process per query, which this suite's volume does not notice. Benefit: it is the access path `SPEC.md § 17.1` gives the operator, so an assertion can be re-run by hand at a terminal — and `go.mod` still declares no dependencies |
| R23-i | **Open — does alpha implement `§ 16` validation?** | The suite does **not** assert submission-time validation, and this is flagged rather than silently decided. `§ 16` states its 400s unconditionally, and `§ 11.2`'s "rejected with 400 until milestone eta" only means something if the rejection exists from the first release — but `§ 18`'s milestone table gives *"cancellation and submission-time validation"* to milestone **ι**, and `§ 18.1` neither claims validation for alpha nor lists it among what alpha deliberately omits. Deciding it by writing a test is exactly what `CLAUDE.md § 9` forbids. Put to the owner |
| R23-j | **State** | `gofmt` clean, `go vet` clean, `go build` clean, `go test -short ./...` green (both groups skip without docker). The full suite fails at `TestMain` because milestone alpha has no implementation yet — the expected state, and the point of `§ 4` step 3 |
| R23-k | **Found by running the harness, not by reading it** | The first execution left `postgres` and `worker` running after the bring-up failed, because `TestMain` exited without tearing down. A half-started environment holds the published port 8080 and would make the next group — or the owner's hand-run demo — fail for a reason unrelated to what it was testing. `harness.Up` now captures the orchestrator logs into the error **and then** tears the environment down, so diagnosis survives and no residue does. Verified: both groups run, both fail at `TestMain` as expected, and `docker ps -a` lists nothing afterwards |

---

## Round 24 — Claude's analysis of the validation question (input; not itself spec) (2026-09-03)

Owner asked what `§ 16`'s *"400 for a non-empty `overrides`"* concretely is, and what he has to
decide. No ruling was made in this round; nothing in `SPEC.md` was touched.

| # | Topic | Finding |
|---|---|---|
| R24-a | **What the rule is about** | The configuration-layering feature of `§ 11`. `§ 11.1` defines five workflow-level knobs; `§ 11.2` defines three levels at which they may be set — workflow (implemented), run (`overrides` in the run-creation body), step (`timeout_seconds` / `max_attempts` in a StepSpec). The last two land at milestone **η**. Until then the request *shape* exists and any value in it is a 400: `§ 10.1`'s reason is that the shape of a request is a contract others build against, so adding a sub-object later would be a format change while rejecting a value inside an existing sub-object is not. `runs` has no `overrides` column and `steps` has no override columns until η (`§ 11.2`), so a value could not be honoured even if it were accepted — and `§ 11.2`'s "a rejection is a 400, never silence" is `§ 16`'s governing principle applied to exactly that case |
| R24-b | **R23-i is narrower than stated** | `§ 6.1` — which R23-i did not cite — already settles the largest part of it: *"Every element of `planner_static_steps` is a StepSpec, and is validated as one — by §9.4 and §9.8 — at `POST /workflows`, before any run exists"*, with a correctness argument, not a preference: a malformed static plan discovered at run time leaves a run that "cannot progress and cannot fail … it would sit `RUNNING` forever, reclaimed and re-failed by every sweep. Validating at submission makes that state unreachable." Alpha uses the static planner, so alpha cannot defer this without making a state `SPEC.md` declares unreachable reachable. R23-i's framing — "does alpha validate at all?" — was too wide |
| R24-c | **What is actually left to decide** | Given R24-b forces a full StepSpec validator into alpha, the open question is only whether the *rest* of `§ 16`'s list (planner_type typos, unknown keys, wrong JSON types, the `≥ 1` ranges, and the run-level rules — non-empty `overrides`, missing `input`, unknown key) also lands in alpha, or waits for **ι**. Recommendation put to the owner: land the whole of `§ 16` in alpha, because the expensive piece is already forced and the remainder is a handful of checks on the same parse, several of which are `§ 6.1`'s own column invariants. `§ 18` calls milestones demo scenarios, not layers, so ι keeps its meaning — it *demonstrates* validation rather than introducing it |
| R24-d | **A second, smaller undefined case** | `§ 16` makes a **missing `input`** a 400 but says nothing about a missing or `null` `overrides`; `§ 10.1` shows both keys present. Nothing today depends on it — `demo.sh` and the suite both send `"overrides":{}` explicitly — but it is a wire-contract question and therefore fails `CLAUDE.md § 3`'s "would adding this later be awkward?" test. Put to the owner alongside R24-c |

---

## Round 25 — Rulings (2026-09-03)

| # | Topic | Ruling / outcome |
|---|---|---|
| R25-a | **R24-c, in part** | Owner ruled: **alpha implements the complete StepSpec validation for the static planner** — `§ 9.4`'s required fields and types, and all six rules of `§ 9.8`, applied to every element of `planner_static_steps` at `POST /workflows`. This is `§ 16` rule 3, and it discharges `§ 6.1`'s correctness requirement that a malformed static plan can never reach a driver |
| R25-b | **R24-d, confirmed** | Owner's reading — *"`overrides` is accepted empty, or absent, and anything else is an error"* — was put back and **confirmed as the literal reading of `§ 16`**, not merely a plausible one: `§ 16` enumerates a **missing `input`** as a 400 and pointedly does not enumerate a missing `overrides`, so absence is already legal and needs no new rule. `{}` is the only accepted value; any populated object is a 400 (`§ 11.2`) |
| R25-c | **The one case the reading does not cover** | `"overrides": null` — present, but not an object. Recommendation put to the owner: **accept it as empty**, because the same feature at the *step* level already answers the same question in that direction — `§ 9.4` gives `timeout_seconds` / `max_attempts` the default `null` and permits omission, and `§ 9.8` rule 5 rejects only a **non-`null`** value. A run level that rejected `null` while the step level accepts it would have the two halves of one feature disagree about the same JSON value. Awaiting ruling |
| R25-d | **Still open from R24-c** | Whether `§ 16` rules **1, 2, 4, 5 and 6** (planner_type not in the enum; `planner_url` missing or not an absolute HTTP(S) URL; any unknown top-level key; any configuration field of the wrong JSON type; any `*_max_attempts` or `*_timeout_seconds` below 1, or a negative `step_retry_delay_seconds`) and the run-level *unknown key* rule also land in alpha, or wait for **ι**. R25-a settled rule 3 only. Recommendation unchanged: land them, because the expensive validator is now forced anyway and rule 6 is `§ 6.1`'s own column invariants |

---

## Round 26 — Validation ruled into alpha, and the third test group written (2026-09-03)

| # | Topic | Ruling / outcome |
|---|---|---|
| R26-a | **R25-c** | Owner ruled: **`"overrides": null` counts as absent.** So `{}`, `null` and omission all mean "no overrides", and only a populated object is a 400 |
| R26-b | **R25-d** | Owner ruled: **all of `§ 16` lands in alpha.** Rules 1, 2, 4, 5 and 6 join rule 3, together with the run-level rules |
| R26-c | **Transcription directed** | Owner directed a *short* note in `SPEC.md` and said the tests follow. Two edits, both brief, both applied: `§ 16` gains one sentence — "`overrides` may be `{}`, `null`, or omitted; all three mean 'no overrides'" — with a one-paragraph `Why:` grounding it in `§ 9.4`'s step-level default and `§ 9.8` rule 5's refusal of only a **non-`null`** value, so the two halves of one feature cannot disagree about the same JSON value. `§ 18.1` gains a "**Which validation α implements**" paragraph beside the existing "Which endpoints α implements": all of `§ 16`, and `§ 9.4` / `§ 9.8` in full for every element of `planner_static_steps`, with the `Why:` that `§ 6.1` forces the validator into alpha regardless and that **ι demonstrates validation, it does not introduce it** — consistent with `§ 18`'s own "milestones are demo scenarios, not layers" |
| R26-d | **Third test group** | `test/alpha/validation/` written: `§ 16`'s six workflow rules, `§ 9.8`'s six StepSpec rules against `planner_static_steps[0]`, and the run-creation rules including all three accepted shapes of `overrides`. It starts no run, so it is much the fastest group. `harness.WorkflowJSON` added so every rejected body is `demos/alpha/workflow.json` with **one** mutation — a hand-written invalid document could be invalid for three reasons at once and still produce the 400 the test wanted |
| R26-e | **Positive control** | `TestMain` creates the demo's own workflow before any rejection is asserted, and fails the group if it is refused. Without it every "must be 400" assertion would pass against an orchestrator that rejected everything |
| R26-f | **One thing deliberately not asserted** | `§ 10.5` requires a rejection to carry "the identifier and current status of every entity the request named or would have touched", but says in the same breath that a `POST /workflows` rejection has no run to describe, and `workflows` has no status column at all (`§ 6.1`). What a workflow-level rejection must carry beyond `error` and `message` is therefore not settled, and only those two are asserted. Flagged in the test's own comment rather than guessed (`CLAUDE.md § 9`) |
| R26-g | **State** | `gofmt` clean, `go vet` clean, `go test -short ./...` green across all three groups. The validation group runs, fails at `TestMain` because alpha has no implementation, and leaves no containers behind. `test/alpha/README.md` updated: the group table gains its third row, and the old "this suite does not cover validation" section is replaced by why validation is asserted here rather than at ι |

---

## Round 27 — Milestone α implemented (2026-09-03)

Owner directed: *"implement stage alpha to meet the tests in `test/alpha`"*, with `SPEC.md` and
`test/` explicitly out of bounds. Neither was touched; `git status` confirms both carry only the
modifications that predated this round.

### What was built

| Package | Contents |
|---|---|
| `internal/config` | The one YAML file of `§ 4.4`, read with unknown keys refused |
| `internal/model` | The entities of `§ 3.2`, the enumerations of `§ 5`, `§ 6.4`'s 4 KB truncation, UUID minting |
| `internal/validate` | `§ 16` in full, and `§ 9.4` / `§ 9.8` for every StepSpec |
| `internal/storage` | The interface `§ 7` makes a conformance requirement — opaque `[]byte` for every JSON document |
| `internal/storage/postgres` | The only implementation, plus the embedded migration that is `§ 6` and `§ 7.2` |
| `internal/planner` | `§ 6.1`'s built-in static planner |
| `internal/dispatch` | `§ 9.5`'s envelope and `§ 9.6`'s reply |
| `internal/engine` | `§ 8.6`'s sweep, `§ 8.7`'s heartbeat and release, `§ 4.2`'s driving loop |
| `internal/httpapi` | Exactly the five endpoints `§ 18.1` gives α, and `§ 10.5`'s error shape |
| `cmd/piton` | The boot order `§ 13.1` case 5 and `§ 18.1` fix between them |

### Rulings not needed, decisions taken, and one tension found

| # | Topic | Outcome |
|---|---|---|
| R27-a | **The storage interface is a method per fenced operation, not an exposed transaction** | Every write that constitutes a decision is one method, each opening with `§ 8.2`'s fence and committing as a unit. Handing a transaction handle to the caller instead would move the fence obligation to the call site, which is the mistake `§ 8.2` names: *"one forgotten statement silently breaks the whole guarantee"* |
| R27-b | **Schema constraints carry the invariants, not the caller** | `§ 6.4` asks for exactly this for `attempts` — *"enforced by the backend rather than by the caller"* — and the same reasoning was applied to `§ 6.2`'s and `§ 6.3`'s invariants. `runs` therefore carries a CHECK that `owner_id`/`claimed_at` are a pair and are non-`NULL` only while `RUNNING`, so a forgotten fourth writer (`§ 8.7`) fails loudly instead of leaving the column silently lying |
| R27-c | **A tension between `§ 4.2` and `§ 6.3` over when `steps.attempt_count` is incremented — flagged, not resolved** | `§ 4.2` says dispatch *"insert an `attempts` row …, increment `steps.attempt_count`, **commit**, and only then send the HTTP request"* — the increment is at dispatch. `§ 6.3` says `attempt_count` is *"**Not** the number of `attempts` rows"* and gives as the reason that a cancelled attempt does not burn budget, *"so the two numbers legitimately differ"* — which is only true if the increment happens at **outcome**, excluding `cancelled`. Under increment-at-dispatch the two numbers can never differ. Both readings satisfy every α assertion identically (`attempt_count = 1` on a step with one successful attempt), so the milestone did not turn on it. **`§ 4.2` was followed**, because it is a mechanical procedure and `§ 6.3`'s line is a rationale, and because a budget counted at dispatch can never be under-counted by a crash between the dispatch and its outcome. This becomes decidable at **γ** and load-bearing at **ι**, where cancellation exists. Put to the owner |
| R27-d | **`raw` and `async` dispatch: what an unbuilt mode does at run time** | `§ 9.8` does not reject either — `sync`+`raw` is legal (θ) and `async`+`envelope` is legal (ε), and `§ 19.3` keeps both designed in and unbuilt — so a workflow declaring one is accepted by `§ 16` and can produce a run. Rather than add a submission-time rejection `§ 9.8` does not have (which would also have made `TestRejectsAsyncRaw` and `TestRejectsInputFromInRawMode` pass for the wrong reason), the **dispatcher reports one failed attempt** naming the mode and its milestone. It burns budget, converges to DLQ under `§ 12.2` rather than sitting `RUNNING` forever, and the reason is legible in `attempts.error_text` (`§ 17.3`). One uniform rule, and a five-line deletion when θ and ε land. **This is a decision, not a SPEC rule** |
| R27-e | **`planner_type: "http"` — the same shape of hole, the same answer** | `§ 16` rule 1 accepts `http` as an enumerated value, and the HTTP planner is ζ (`§ 19.3`). A run against one records a planner call that could not be made (`planner_unreachable`, `§ 6.5`), which burns planner budget and reaches planner-side DLQ under `§ 12.2`. Routed through the normal budget path with no special case, because `§ 12.1` forbids exempting any planner from it |
| R27-f | **`§ 4.2`'s retry and worker-side DLQ are implemented although γ demonstrates them** | Same argument the owner accepted in R26-b for `§ 16` and ι: `§ 18` makes milestones demo scenarios, not layers, and the budget check is part of the driving loop itself. Without it a single failed attempt would leave a run that can neither progress nor fail — the state `§ 6.1` calls unreachable |
| R27-g | **`§ 10.5` at workflow level** | Only `error` and `message` are sent for a `POST /workflows` rejection, which is R26-f's finding applied to the implementation rather than a new decision: there is no run to describe, `workflows` has no status column (`§ 6.1`), and the workflow was never created |
| R27-h | **`input` must be a JSON object** | `§ 16` makes a missing `input` a 400 and says nothing about its type. `§ 9.2` types `workflow_input` — which is `runs.input` verbatim — as an object, so a non-object is refused under `§ 16` rule 5's principle. Recorded because it is a wire-contract judgement, though no test turns on it |
| R27-i | **One configuration rule with no SPEC behind it** | `config.Load` refuses a `lease_ttl_seconds` at or below `heartbeat_interval_seconds`. `§ 8.7` derives liveness from the two together and says 30 s over 10 s *"tolerates two missed heartbeats"*; a TTL at or below the interval tolerates none, so a live orchestrator would be declared dead by its own scheduling jitter. This governs a file the operator writes by hand, not the wire, so it is an implementation guard rather than a proposed SPEC rule |
| R27-j | **Dependencies** | `go.mod` previously declared none. Three were added: `gopkg.in/yaml.v3` (`§ 9.1` puts configuration in YAML), `github.com/lib/pq`, and `github.com/golang-migrate/migrate/v4` — the last because `CLAUDE.md § 8` names it in the stack. Migrations are embedded with `go:embed` and applied at boot, since `§ 18.1`'s environment has exactly three services and therefore no migration container |
| R27-k | **Found by running, not by reading** | `lib/pq` binds a `[]byte` parameter as `bytea`, so a JSON document passed as bytes reaches a `jsonb` column as the hex text `\x7b…` and is rejected. Every JSON parameter is therefore bound through one helper that converts to `string`. The interface above it still deals only in bytes, as `§ 7.1` requires — this is the backend's encoding choice, which is precisely what `§ 7.1` leaves to it |
| R27-l | **Result** | `demos/alpha/demo.sh`: **35 assertions, 0 failures**, run end to end. `test/alpha/run.sh`: all three groups green — `happypath` 12 tests, `ownership` 5, `validation` 17 — each against its own `docker compose` environment, torn down with the volume wipe between groups. `gofmt`, `go vet` and `go build` clean; `go test -short ./...` green with no docker. No container was left behind |
| R27-m | **What is still outstanding** | `CLAUDE.md § 4` **step 5**. The owner has not yet run the demo by hand and inspected database truth from a terminal, and a green suite he has never looked behind is not evidence the milestone landed |

---

## Round 28 — R27's four questions ruled, and the cancellation clause examined (2026-09-03)

| # | Topic | Ruling / outcome |
|---|---|---|
| R28-a | **R27-c** | Owner ruled: **`§ 4.2` is correct** — `steps.attempt_count` is incremented at **dispatch**. The implementation already did this, so no code changed. What did change is what the counter *means*, and that has consequences two sentences of `SPEC.md` do not survive — see R28-e |
| R28-b | **R27-d** | Owner ruled: **keep `raw` and `async` open.** No submission-time rejection is added; `§ 9.8` stands as written, and an unbuilt mode is reported as one failed attempt naming its milestone |
| R28-c | **R27-e** | Owner ruled: confirmed. `planner_type: "http"` stays accepted at `POST /workflows` and converges to planner-side DLQ at run time |
| R28-d | **R27-f** | Owner ruled: confirmed. `§ 4.2`'s retry and worker-side DLQ stay in α |

### R28-e · The owner's question: *when is an attempt cancelled?*

Asked so that *"a cancelled attempt does not burn budget"* can be designed rather than assumed.
The answer is narrow, and it exposes two sentences that R28-a has just made false.

**There is exactly one circumstance, and it is milestone ι.** `failure_reason = 'cancelled'` is
written only by `§ 15`'s cancel transaction, and only when it lands on combination **L2** — all
three of these true at the same instant:

1. the operator posts `POST /runs/{run_id}/cancel`;
2. `runs.status` is `RUNNING` or `DLQ` (anything else is a 409, `§ 15`);
3. the derived `last_step` is `RUNNING` **and** that step has a `RUNNING` attempt.

If the run is in `DLQ`, or its last step is `DONE` or `DLQ`, no attempt is touched at all — those are
`§ 5.5`'s L7 and L8, and `§ 5.7` says the last step "keeps the terminal state it already had".

**Nothing else produces the value.** Verified against the implementation and re-derived from SPEC:
a passed deadline gives `timeout` or `orphaned` (`§ 5.3`); a retry cannot supersede a live attempt,
because `§ 8.3`'s CAS moves the previous one out of `RUNNING` before a new row is inserted; an
orchestrator's clean shutdown releases ownership only (`§ 8.7`) and leaves the attempt `RUNNING` to
be expired later as `timeout`/`orphaned`; a worker's own failure report gives `worker_error`; and
replay does not touch `attempts` rows at all (`§ 14`).

| # | Consequence of R28-a | Status |
|---|---|---|
| R28-f | **`§ 5.7` survives verbatim** | Its sentence is *"attempt → `FAILED(cancelled)` with `attempt_count` **unchanged**"*, and its `Why:` is *"cancellation is not the worker's failure … incrementing it would put a misleading number in front of the operator"*. Both are statements about what the **cancel transaction** writes, and under `§ 4.2` it writes no counter. Nothing to amend |
| R28-g | **`§ 5.3`'s table note is now false** | *"Every value below burns one unit of budget except `cancelled`"* was true only under the outcome-time reading R28-a rejected. Under `§ 4.2` the budget is burned by **dispatching**, so a cancelled attempt burned one unit when it was dispatched, exactly like every other attempt — and no `failure_reason` affects the counter, because the counter is not written at outcome time at all. **Amendment proposed, not made** (`CLAUDE.md § 2` rule 1) |
| R28-h | **`§ 6.3`'s `Why:` is now false, but its rule is not** | The rule — *"`attempt_count` is stored rather than derived as `COUNT(attempts)`"* — is right. Its stated reason — *"a cancelled attempt does not burn budget, so the two numbers legitimately differ"* — cannot hold under `§ 4.2`, where the two numbers are always equal within a round. **A stronger reason exists and needs no cancellation: replay.** `§ 14` resets `steps.attempt_count` to 0 while the `attempts` rows survive — they must survive, or `§ 6.2`'s `replay_count` would have nothing to bucket and its own `Why:` ("the owner must be able to see which round a given attempt belonged to") would be unsatisfiable. After one replay round a step can show `count(attempts) = 3` and `attempt_count = 0`. **Amendment proposed, not made** |
| R28-i | **What `attempt_count` now means, stated once** | *The number of attempts **dispatched** for this step in the **current replay round**.* Written in exactly one place — the dispatch transaction of `§ 4.2` — and read in exactly one place, `§ 12.2`'s budget check. `§ 4.2` already accepts the cost this implies: *"if the process dies between the two, the worst case is an attempt that was never dispatched and will time out"* — budget burned for work never done, accepted deliberately |

### R28-j · Failure paths exercised by hand, since nothing in α's suite reaches them

Four probes run against `demos/alpha/docker-compose.yml`, then torn down. None is a new artefact;
they are recorded because they are the first evidence these code paths run at all.

| Probe | Result |
|---|---|
| **Worker-side DLQ** — `worker_url` at a dead port, `step_max_attempts = 3` | 3 attempts, all `FAILED(transport_error)`; step `DLQ` with `attempt_count = 3`; run `DLQ`; `owner_id`/`claimed_at` `NULL`; one `dead_letter_queue` row, `worker_budget_exhausted`, `attempt_count = 3`, `step_id` present. Combination **L4** |
| **`raw` dispatch (θ, unbuilt)** — accepted by `§ 16`, as R28-b requires | 2 attempts, both `FAILED(transport_error)` with `error_text` naming `dispatch_style "raw"` and its milestone; run converges to `DLQ` rather than hanging |
| **`http` planner (ζ, unbuilt)** | `planner_attempt_count` reaches its budget of 2; `last_planner_error` names the milestone; run `DLQ` with **zero steps**; one `dead_letter_queue` row, `planner_unreachable`, `step_id` **NULL**. Combination **L5**, and the planner-side/worker-side split of `§ 12.3` visible in one column |
| **`SIGKILL` mid-attempt, then restart** — `worker_url` at an unroutable address so the attempt stays `RUNNING` | Ownership survived the kill (no clean shutdown, so no release, `§ 8.7`); `orchestrators` shows two rows, the dead one not live; after `lease_ttl` the new process claimed the run (`§ 8.5`) and expired the sync attempt **at claim time** (`§ 8.6`) with `failure_reason = 'orphaned'` — `§ 5.3`'s definition, *"timeout, where the attempt's dispatching orchestrator was not live"*, decided in SQL against the same predicate the claim uses. Attempt 2 was then dispatched by the new orchestrator, `attempt_count = 2`. This is machinery **β** will demonstrate; it is not a claim that β has landed |

| # | Topic | Outcome |
|---|---|---|
| R28-k | **Two amendments awaiting a ruling** | `§ 5.3`'s "except `cancelled`" clause (R28-g) and `§ 6.3`'s `Why:` (R28-h). Both are consequences of R28-a, neither changes any behaviour, and `SPEC.md` has **not** been touched |
| R28-l | **Still outstanding** | `CLAUDE.md § 4` **step 5** — the owner's own hand-run of `demos/alpha/demo.sh` |

---

## Round 29 — Cancellation zeroes the budget; the §4.2 ruling transcribed (2026-09-03)

| # | Topic | Ruling / outcome |
|---|---|---|
| R29-a | **Cancellation and `attempt_count`** | Owner ruled: **a cancel sets the step's `attempt_count` to 0.** Reasoning given: if a run is cancelled the whole thing is over, so the number is not worth a rule. He explicitly did not want time spent on it. Accepted, and it is the simpler rule — see R29-c |
| R29-b | **One correction to the reasoning offered, before it went into SPEC** | The owner's stated ground was *"restart makes it 0 anyway"*. That is not true and could not be written into an authority document: a **restart** deliberately does **not** reset the counter — `§ 6.2` and `§ 12.2` exist precisely so it survives a crash, or a crash loop would never converge. What is true, and what the `Why:` now says, is that the number is **never read again**: `CANCELLED` is terminal (`§ 5.1`) and `§ 14` replays only a run that is in `DLQ`, so no budget check can ever consult it |
| R29-c | **The ruling rescues `§ 6.3`** | R28-h had found `§ 6.3`'s `Why:` false under R28-a — with the increment at dispatch, `COUNT(attempts)` and `attempt_count` can never differ within a round. Zeroing on cancel makes them differ again (three attempt rows, `attempt_count = 0`), so the original rationale holds, now stated over both of the two things that reset the counter — cancellation (`§ 5.7`) and replay (`§ 14`) |

### R29-d · The transcription — five sites, all directed by R28-a and R29-a

`CLAUDE.md § 2` rule 1 permits this: the owner directed that his rulings be written into `SPEC.md`.

| Site | Change |
|---|---|
| **`§ 5.7`** | *"with `attempt_count` **unchanged**"* → *"and that step's `attempt_count` is **set to 0**"*. The `Why:` is rewritten on R29-b's ground, and a second `Why:` is added answering the obvious objection — this is not a refund of `§ 4.2`'s dispatch-time budget; the attempt rows and their `failure_reason` stay, and only the counter that would govern a future dispatch is cleared, of which there is none |
| **`§ 5.3`, lead sentence** | *"Every value below burns one unit of budget except `cancelled`"* was false under R28-a. Replaced by: no `failure_reason` changes `steps.attempt_count`, because `§ 4.2` burns budget at **dispatch**, not at the outcome; `cancelled` is the exception in the other direction, zeroing it |
| **`§ 5.3`, `cancelled` row** | *"**Does not burn budget**"* → *"**The step's `attempt_count` is zeroed** (§5.7)"* |
| **`§ 6.3`** | `Why:` restated over cancellation **and** replay, both of which reset the counter while leaving the `attempts` rows in place |
| **`§ 15`** | The SQL sketch's comment now names the zeroing, since that sketch is what an implementer copies |

| # | Topic | Outcome |
|---|---|---|
| R29-e | **A sixth site found while transcribing, and corrected** | `§ 12.2`'s budget diagram still read *"attempt fails → `steps.attempt_count += 1`"* — a direct contradiction of the `§ 4.2` reading the owner had just ratified in R28-a, and the original source of R27-c. Corrected to `attempt dispatched → += 1`, with the failure line reduced to the budget check it actually is. The paragraph beneath — *"every failure … increments a persisted counter"* — was reworded to *"every dispatch increments a persisted counter **before the work begins**"*, which is both accurate and a **stronger** claim: the counter moves before the crash can happen rather than after. One sentence was added noting that the two counters are incremented at different moments on purpose — a worker attempt has a dispatch to hang the increment on, a planner call has no row and no step, so its failure is the only event there is. Flagged to the owner as a site he did not name |
| R29-f | **No code changed** | Cancellation is milestone **ι** and no line of it is written. The schema needs nothing either: `steps_attempt_count CHECK (attempt_count >= 0)` already admits 0. The `§ 12.2` correction describes what `internal/storage/postgres/driver.go` already does |
| R29-g | **Owner's second point: where step detail is read** | Owner ruled `GET /runs/{run_id}/steps` is where step internals are inspected. `decision` therefore stays in that response. `GET /runs/{run_id}` keeps the lighter summary. Noted back to him, unresolved by SPEC and not invented: α exposes no step **output content** over HTTP, only `output_bytes` — `§ 18.1` puts `GET /steps/{step_id}/output` in **ζ**, so output is read from the database, which is what `§ 17.1` makes the interface |
| R29-h | **Owner asked whether manual testing needs a file of its own** | Answered: **no new file recommended.** Three of the four suggested exercises are already carried by files that exist — `demos/alpha/demo.sh`'s header, `demos/alpha/workflow.json`, and the commented echo worker inside `demos/alpha/docker-compose.yml`. The fourth — the failure-path probes of R28-j — is **γ**'s and **β**'s material, and `SPEC.md § 4.4` already says where it belongs: *"each milestone or demo scenario owns one directory containing one `docker-compose.yml` and one hand-run demo script"*. Writing it now would be writing γ's demo early, which fails `CLAUDE.md § 3`'s admission test, and would add a fifth voice to `CLAUDE.md § 1`'s document set. R28-j's table stands as the record that those paths were exercised; it is explanatory only and is authority for nothing |
| R29-i | **Still outstanding** | `CLAUDE.md § 4` **step 5** — the owner's own hand-run of `demos/alpha/demo.sh` |

---

## Round 30 — α committed and pushed; how α is verified by hand; γ proposed, not started (2026-09-05)

| # | Topic | Ruling / outcome |
|---|---|---|
| R30-a | **Owner directed: commit and push the α work** | Done. One commit, `Implement milestone alpha`, carrying `demos/alpha/`, `test/alpha/`, `internal/`, `cmd/piton/main.go`, and the round 28–29 SPEC amendments. `go build ./...` and `go vet ./...` were run inside WSL first (`CLAUDE.md § 8`); the docker-backed suite was **not** run as part of committing, and the commit message says so |
| R30-b | **The commit message states what the commit is not** | It ends with *"`CLAUDE.md § 4` step 5 — the owner has not yet run `demos/alpha/demo.sh` by hand — is still outstanding. This commit is not an acceptance claim."* A commit is a record that code exists, not evidence a milestone landed. `§ 4` step 5 is not replaceable by step 6, and certainly not by `git push` |
| R30-c | **Owner asked how he can hand-test α himself** | Answered with three paths, all already carried by files in the tree — no new artefact was created, consistent with R29-h. (1) `cd demos/alpha && ./demo.sh` runs `§ 18.1` unattended and prints its five queries. (2) The same thing typed by hand: the `curl` sequence of `§ 18.1`, then `docker compose exec postgres psql` for the five `SELECT`s — this is the path `§ 17.1` makes the operator's interface, and the compose file publishes no host port for Postgres deliberately. (3) `./test/alpha/run.sh` for the automated suite, which is step 6 and not acceptance evidence |
| R30-d | **Owner directed: start the next stage** | `SPEC.md § 18`'s order puts **γ** — retries and the dead-letter queue — next. Its behaviour is fully ratified (`§ 12` in whole, `§ 5.3`, `§ 8.3`, `§ 12.3`'s worker-side/planner-side split), so nothing about *what γ does* is open |
| R30-e | **What is open, and why work has not begun** | `§ 18` says each milestone's demo script *"is written when that milestone starts"*, and only α's exists. `CLAUDE.md § 4` step 2 makes the demo script the first thing γ needs, and `CLAUDE.md § 2` rule 1 forbids writing it into `SPEC.md` unsolicited — a demo script fixes what the operator must see, which is a definition. **A § 18.2 draft was proposed to the owner and awaits his ruling.** Nothing was written to `SPEC.md` |
| R30-f | **What the draft proposes, in outline** | A `demos/gamma/` directory beside α's, with a worker that fails on demand; the run walks to a step whose worker refuses, `steps.attempt_count` climbs to `step_max_attempts`, each attempt is `FAILED` with a `failure_reason` from `§ 5.3`, the step and then the run reach `DLQ`, and exactly one `dead_letter_queue` row appears with the fields `§ 12.4` requires. Two open questions were put to the owner with it: whether γ demonstrates the planner-side DLQ path of `§ 12.3` as well as the worker-side one, and whether the failing worker fails by refusing (an HTTP error) or by being unreachable (a transport error) — R28-j exercised the unreachable variant by hand already |
| R30-g | **Still outstanding** | `CLAUDE.md § 4` **step 5** for α — the owner's own hand-run — carried forward from R28-l and R29-i, and now the `§ 18.2` ruling of R30-e |

---

## Round 31 — Why γ precedes β; both worker failure modes ruled in; planner-side DLQ ruled in (2026-09-05)

| # | Topic | Ruling / outcome |
|---|---|---|
| R31-a | **Owner asked why the next milestone is γ and not β** | Answered: the Greek letters are **names, not an order**. `SPEC.md § 18`'s table has an explicit `Order` column, and it reads α → γ → β → δ → ζ → θ → ε → η → ι. That order is the owner's own — **R5-f** fixed it and **R10-c** confirmed it against the reconstruction. `§ 18` also states the reason in the document itself: *"the crash-recovery demo's point is partly that DLQ'd content is not touched by recovery and must wait for an explicit replay. That cannot be shown until DLQ exists."* β without γ could only demonstrate half of β. Nothing was changed; the question was answered from ratified text |
| R31-b | **Owner ruled: both worker failure modes are demonstrated** | *"活著但回錯誤"* and *"連不上"* are both in γ. They are different rows of `§ 5.3` and the operator reads a different repair from each |
| R31-c | **A third variant folded in without asking, and why** | *"Alive but returns an error"* is two distinct `§ 5.3` values, not one: a worker that replies **in Piton's envelope** that the work failed is `worker_error`; a worker that replies **HTTP 500** is `transport_error`, because `§ 5.3` defines `transport_error` as *"the HTTP exchange did not produce a usable reply — non-2xx, connection refused, DNS failure, connection reset"*. Both are demonstrated. This was a reading of the owner's ruling, not an extension of it: `§ 5.3`'s `Why:` says the three values *"name three different repairs — the worker's business logic, the worker's output format, the network — and the operator picks his next move from this column"*, so a demo that showed only one of them would leave the column's whole purpose unexercised. The unreachable leg additionally pins `§ 5.3`'s clock rule: *"a connection refused at second 3 of a 300-second budget is `transport_error`"*, not `timeout` |
| R31-d | **Owner ruled: γ demonstrates planner-side DLQ as well** | Accepted. This is `§ 12.3`'s L5 branch — run `DLQ` with `last_step = DONE`, `dead_letter_queue.step_id` `NULL`, `attempt_count` taken from `runs.planner_attempt_count` |
| R31-e | **What that ruling costs, put back to the owner before any code** | The planner-side path **cannot be reached with the planners that exist**. `§ 12.1` states it directly: the static planner *"simply cannot fail at run time"* — `§ 6.1` validates its steps at submission, it holds no state and makes no network call, so `planner_attempt_count` never leaves 0. Reaching L5 therefore needs a planner that can be pointed at an address that does not answer, which is the **HTTP planner client of ζ**. R28-j's probe reached L5 only because `planner_type: "http"` is unbuilt and reports an error saying so — the mechanism was real, but the `error_text` said *"ζ not built"*, so it demonstrated the wrong cause. **Recommendation put to the owner: build the transport half of the HTTP planner client in γ** — issue the call, and treat unreachable / timeout / non-2xx as a planner failure — and leave `§ 9.3` parsing, `§ 9.8` validation, `planner_invalid_response` and `planner_declared_fail` to ζ. The precedent is R26-b and R27-f, which the owner has already accepted twice: `§ 18` makes milestones demo scenarios, not layers |
| R31-f | **§ 18.2 draft, still not written to SPEC** | Five legs against one environment: a step that fails twice then succeeds (`attempt_count = 2` on a `DONE` step — the only place a *recovering* retry is visible); `worker_error` to DLQ; `transport_error` by HTTP 500; `transport_error` by connection refused; and the planner-side L5 leg of R31-d. `CLAUDE.md § 2` rule 1 — `SPEC.md` is untouched and awaits the owner's word |
| R31-g | **Still outstanding** | `CLAUDE.md § 4` **step 5** for α (R28-l, R29-i, R30-g); the `§ 18.2` ruling; and now R31-e's scope question |

---

## Round 32 — Planner-side DLQ dropped from γ; γ is worker-side only (2026-09-05)

| # | Topic | Ruling / outcome |
|---|---|---|
| R32-a | **Owner reversed R31-d: γ does not demonstrate planner-side DLQ** | Ground given: it has no substantive meaning yet, because no real planner exists. It is done when a planner is introduced. Accepted, and it is the better ruling — R31-e had already found that the only way to reach L5 today is to point `planner_type: "http"` at a dead address, which demonstrates *"ζ is not built"* rather than *"the planner could not be reached"*. A demo leg whose `error_text` names an unbuilt milestone is not evidence of the mechanism it claims to show |
| R32-b | **R31-e's scope question is therefore withdrawn, not answered** | No part of ζ's HTTP planner client is pulled forward. γ builds nothing outside `§ 12`'s worker side. This is `CLAUDE.md § 5`'s subtractive default doing its job: the burden of proof was on *including* the leg, and the owner found it not met |
| R32-c | **Nothing is deleted, and no SPEC rule changes** | `§ 12.3`'s planner-side column, `§ 6.5`'s four `planner_*` reasons and `§ 12.1`'s planner budget rules all stand — they are design, and `§ 19.3` already records the HTTP planner as *"designed in, built at ζ"*. The **code** also stands: the planner-side path is implemented today (R28-j reached L5 by hand). What the owner ruled out is a **demo leg and its tests**, not a behaviour and not an implementation |
| R32-d | **Where the planner-side demonstration goes** | ζ — the first milestone with a planner that can genuinely fail. No new entry is needed anywhere: `§ 19.3` already points the HTTP planner at ζ, and `§ 18` says each milestone's demo script is written when that milestone starts, so ζ's will state it then. `BACKLOG.md` is not the right home either — this is not unscheduled, it is scheduled at ζ |
| R32-e | **γ's demo is now four legs, all worker-side** | (1) a step that fails twice and succeeds on its third and last attempt — `DONE` with `attempt_count = 3`, which is also the only demonstration that `§ 11.1`'s *"`step_max_attempts` is a **total** attempt count, not a retry count"* is implemented as stated; (2) `worker_error` to DLQ; (3) `transport_error` by HTTP 500; (4) `transport_error` by connection refused, which pins `§ 5.3`'s clock rule. All four reach `§ 12.3`'s **L4** except the first, which reaches no DLQ at all |
| R32-f | **§ 18.2 draft put to the owner in full, still not written to SPEC** | Every question γ had is now ruled on, so the draft is complete text rather than an outline. `CLAUDE.md § 2` rule 1 — `SPEC.md` is untouched and awaits the owner's word to transcribe |
| R32-g | **Still outstanding** | `CLAUDE.md § 4` **step 5** for α — the owner's own hand-run — carried since R28-l; and the `§ 18.2` ruling |

---

## Round 33 — §18.2 transcribed; γ's demo and suite written; γ needed no implementation (2026-09-05)

| # | Topic | Ruling / outcome |
|---|---|---|
| R33-a | **Owner directed: write §18.2 into SPEC, then write the tests** | Both done. `CLAUDE.md § 2` rule 1 permits the transcription: the owner directed it, and the text transcribed is the draft he had already ruled on in R31–R32, word for word |
| R33-b | **One consequential edit made beyond the dictated text, and flagged** | `§ 18`'s closing paragraph read *"Only α's is written now; the rest are written when that milestone starts."* Adding `§ 18.2` made that sentence false, and a self-contradicting authority document is worse than either reading. Changed to *"α's and γ's are written; the rest are written when that milestone starts."* Nothing else in `SPEC.md` was touched. Reported to the owner rather than left for him to find |
| R33-c | **`demos/gamma/` — one environment, four legs** | `docker-compose.yml` (postgres, orchestrator, one worker), `piton.yaml`, four workflow files and `demo.sh`. The worker's behaviour is selected per step through `params.mode`, which `§ 18.2` requires; `worker_error` is a 200 carrying a failure envelope (`§ 9.6`) and `http_500` is a non-2xx, because `§ 5.3` puts those in **different rows** and a worker that conflated them would make legs 2 and 3 the same leg |
| R33-d | **The unreachable leg needed no service and no code** | `workflow-unreachable.json` points `worker_url` at port 9099 of the worker container, where nothing listens. Taking a container away instead would have meant modifying the environment mid-demo, and `§ 18.2` requires all four legs to run against one environment |
| R33-e | **Why each DLQ leg declares a second static step it never reaches** | `§ 18.2` requires *"no step created after it"*. A workflow ending at the failing step could not show that — there would be nothing the planner could have created even if it had been asked. The second element makes the absence evidence rather than a tautology, and `TestNoStepIsCreatedAfterTheFailingOne` fails loudly if a workflow file ever loses it |
| R33-f | **`test/gamma/` — one group, not four** | `CLAUDE.md § 5.5.1` brings up one environment *"per test file (or per milestone scenario)"*, and `§ 18.2` is one scenario. `§ 5.5.3` — the rule that forces α's `ownership` group to stand alone — does not apply: nothing in γ touches `orchestrators` or another run's `owner_id`. The two tests that speak about every run at once assert invariants that must hold whichever runs exist |
| R33-g | **`test/gamma/harness` duplicates `test/alpha/harness`, deliberately** | They differ in which demo directory they point at, so a shared package was possible. Not done: α's suite is the guard on a milestone the owner is about to verify by hand, and refactoring it to serve γ would put that guard at risk for a later milestone's convenience. Recorded as **B18** in `BACKLOG.md`, which is authority on nothing — the third milestone wanting the same plumbing is where the trade turns |
| R33-h | **Results** | `./test/gamma/run.sh` — 14 tests, all pass, 69 s, legs ending `DONE / DLQ / DLQ / DLQ`. `demos/gamma/demo.sh --down` — 53 assertions, 0 failures. `gofmt`, `go build ./...` and `go vet ./...` clean |
| R33-i | **The finding that matters more than the green: γ required no implementation** | Not one line of `internal/` or `cmd/` changed. **R27-f predicted this exactly** — `§ 4.2`'s retry and worker-side DLQ were implemented during α because `§ 18` makes milestones demo scenarios rather than layers, and because without the budget check a single failed attempt leaves a run that can neither progress nor fail. So this round is not a red-to-green cycle: the tests were derived from `SPEC.md` alone (`CLAUDE.md § 5.1`), but the code they exercise was already there, and what γ adds is the **demonstration and the guard**, not the behaviour. Stated plainly so that a green suite is not mistaken for evidence of work it did not do |
| R33-j | **Still outstanding** | `CLAUDE.md § 4` **step 5** for α (carried since R28-l) and now for γ. Neither milestone is landed until the owner has run its `demo.sh` by hand and looked behind it |

---

## Round 34 — α and γ hand-verified; β built; a defect found in beta's own fixture (2026-09-06)

| # | Topic | Ruling / outcome |
|---|---|---|
| R34-a | **Owner reported his hand-run of α and γ complete** | `CLAUDE.md § 4` **step 5** is therefore discharged for both, after being carried since R28-l. No discrepancy was reported. It is now outstanding for **β** only |
| R34-b | **Two questions answered from live data rather than from memory** | The owner asked why the flaky step's three attempts appeared to carry the same `error_text`. They do not: attempts 1 and 2 differ by `call 1` / `call 2`, and attempt 3 carries **none** — it succeeded, and `§ 6.4` invariant 2 forbids a reason on a non-`FAILED` row. The cause was a defect in a query **this agent** had given him: `left(error_text, 70)` cut six characters before the part that distinguishes the rows. He also asked what `fail_then_succeed` was claiming, since nothing knows at attempt 1 whether attempt 3 will succeed — answered: it is the value of `params.mode`, the demo worker's own instruction echoed back in its own message, and Piton predicts nothing |
| R34-c | **`transport_error` versus `timeout`, asked and answered twice** | The first answer was too long and was rejected; the short one stands. **Behaviourally they differ in nothing** — `§ 5.3` makes `failure_reason` a diagnostic label and no value changes `steps.attempt_count`. They differ in what the operator must repair, and the decision is made in one SQL `CASE` at the moment of the write, against the row's own `deadline_at`. The dispatcher deliberately returns an **empty** reason so that the database, not Go, decides |
| R34-d | **Owner directed: start the next stage's tests, directly** | `SPEC.md § 18`'s order puts **β** — crash recovery — next. Its behaviour is fully ratified: `§ 13.1` (six situations), `§ 13.2` (the non-guarantees), `§ 8.5`, `§ 8.6`, `§ 8.7`, `§ 5.3`, `§ 5.5`, `§ 12.2`. Work started without a further round of questions, under stated assumptions, because every rule the tests assert was already ruled on |
| R34-e | **`SPEC.md` was NOT touched, and the tests were structured so that it did not have to be** | `§ 18.3`'s demo script is the only thing β needs that does not exist, and `CLAUDE.md § 2` rule 1 forbids writing it unsolicited. So **no assertion cites `§ 18.3`** — every one traces to a section already ratified. A draft is put to the owner separately. This is a better arrangement than γ's in one respect: the suite's authority does not depend on the demo script at all |
| R34-f | **Five legs, and why legs 1 and 2 share one crash** | (1) `SIGKILL` mid-attempt, the run resumes, the completed step never re-runs, the in-flight attempt is `orphaned` and its replacement names a different orchestrator; (2) a run already in `DLQ` **before** the crash is byte-identical after it; (3) a clean `SIGTERM` releases ownership at once; (4) killed on every attempt, the run converges to `DLQ`; (5) storage unreachable at startup, non-zero exit. The DLQ'd run is created **before** the kill so that one restart must both resume one run and leave another alone — the half of β that could not exist before γ, and the reason `§ 18` orders γ first |
| R34-g | **Legs 1 and 3 are a matched pair, and that pair is the whole coordination story** | After `SIGKILL`, `runs.owner_id` still holds the dead process's id — `§ 8.7` names four writers of coordination metadata and all four live inside an orchestrator process. After `SIGTERM` the column is already `NULL`. `§ 8.7` calls release *"an optimisation that makes failover immediate rather than `lease_ttl` later; correctness does not depend on it"*, and these two legs are what that sentence looks like from a terminal |
| R34-h | **Three environment decisions, none of them convenience** | **`restart: "no"`** on the orchestrator — Docker restarting it would take the demonstration away from the operator, and the recovery under test is Piton's (`§ 13`), never the container runtime's. **Faster timings** (sweep 2 s, heartbeat 2 s, lease TTL 6 s against `§ 8.6`/`§ 8.7`'s 5/10/30) — `§ 8.5`'s claim cannot take a run from a live owner, so every kill is followed by a mandatory wait for the lease to expire; the numbers change how *long* failover takes and never what recovery *does*, `§ 4.4` makes all three configuration, and the 3x heartbeat-to-TTL ratio is preserved. **A second configuration file** naming an unresolvable host, because `§ 13.1.5` is the only one of the six situations that cannot be produced by killing something |
| R34-i | **A defect found in this agent's own fixture, and what it would have cost** | The first suite run failed one test: the crash-loop run reached `DONE` instead of `DLQ`. **The implementation was not at fault.** The fixture waited for *"an attempt of this step is RUNNING"* before each kill — but after a `SIGKILL` the killed attempt **is still RUNNING in the database**, because the process that would have expired it died with it. So the condition was true the instant the next orchestrator booted, long before `§ 8.5` let it claim anything, and kills 2 and 3 landed on a process that had dispatched nothing. Three kills, one attempt, budget never exhausted. Fixed by waiting for a **specific `attempt_no`**. Recorded because the failure mode is the dangerous kind: with a weaker assertion the suite would have passed while testing almost nothing |
| R34-j | **Results** | `./test/beta/run.sh` — 11 tests, all pass, 143 s. `demos/beta/demo.sh --down` — 28 assertions, 0 failures. `gofmt`, `go build ./...`, `go vet ./...` clean |
| R34-k | **β required no implementation either** | `git status` after the whole milestone shows **only** new files under `demos/beta/` and `test/beta/`. Not one line of `internal/` or `cmd/` changed, for the third milestone running. The same reasoning as R27-f and R33-i: `§ 8` and `§ 13` are the concurrency-control design α had to implement in order to have a correct driving loop at all. β adds the demonstration and the guard |
| R34-l | **Two of `§ 13.1`'s six situations are deliberately not covered** | `§ 13.1.2` (one run's driver dies while the process lives) — there is no external handle on a single driver, and staging it would mean adding a test-only hook to the product for the benefit of its test. `§ 13.1.6` (storage unreachable at runtime) — stageable, but its assertion is the same state leg 1 already reaches, by a slower and far more timing-dependent path. `§ 13.1.3` (a crash **during** recovery) is exercised without being isolated: leg 4's three kills each land on a process that has just recovered the previous one. Recorded rather than left silent |
| R34-m | **One assertion is deliberately looser than the rest** | `§ 13.1.5` requires *"an error message that names storage as the cause"* and fixes no wording, so the test checks the exit code exactly and accepts any of `storage`, `postgres`, `database`, `dsn`. A fixed string would be inventing a contract no ruling covers (`CLAUDE.md § 9`) |
| R34-n | **Still outstanding** | The `§ 18.3` ruling, and `CLAUDE.md § 4` **step 5** for β — the owner's own hand-run of `demos/beta/demo.sh` |

---

## Round 35 — "driver" was already defined; §18.3 ratified and transcribed (2026-09-06)

| # | Topic | Ruling / outcome |
|---|---|---|
| R35-a | **Owner asked whether "driver" is defined anywhere in SPEC** | It is. `§ 4.2`, first line under the loop: *"A driver is the unit of work inside the orchestrator that advances one owned run."* `Appendix A` indexes it as *"Driver / the driving loop — §4.2"*. It is not in `§ 3` because `§ 3.1` enumerates **actors** — operator, orchestrator, planner, worker — and a driver is not an actor: it is a unit of work inside one of them, which is why it is defined where the loop it drives is defined. **No SPEC change made or needed**, and none was proposed |
| R35-b | **Owner ruled: §18.3 is accepted — write it into SPEC** | Transcribed. `CLAUDE.md § 2` rule 1 permits it: the owner directed the edit, and the text follows what round 34 built and reported, leg for leg |
| R35-c | **The same consequential sentence updated a second time** | `§ 18`'s closing paragraph now reads *"α's, γ's and β's are written; the rest are written when that milestone starts."* Same reason as R33-b — adding a demo script makes the previous count false, and a self-contradicting authority document is worse than either reading |
| R35-d | **What § 18.3 fixes that the code did not already** | Three things are now rules rather than choices this agent made: `restart: "no"` on the orchestrator, because a runtime that restarts it takes the demonstration away from the operator; β's faster coordination timings, with the reason (`§ 8.5` cannot claim from a live owner, so every kill waits for the lease) and the limit (they change how *long* failover takes, never what recovery *does*); and the two situations of `§ 13.1` that β does not demonstrate, `13.1.2` and `13.1.6`, each with its reason. That last is the point of writing it down at all — an unlisted absence reads as an oversight |
| R35-e | **Still outstanding** | `CLAUDE.md § 4` **step 5** for β — the owner's own hand-run of `demos/beta/demo.sh`. Nothing else |

---

## Round 36 — β hand-verified; δ is next, and is the first milestone with real implementation work (2026-09-06)

| # | Topic | Ruling / outcome |
|---|---|---|
| R36-a | **Owner ran β by hand and reported no problem** | `CLAUDE.md § 4` **step 5** is discharged for β. α, γ and β are now all verified by hand. Nothing is outstanding on any landed milestone |
| R36-b | **Three things he could not read, explained** | (i) *"four writers of coordination metadata"* — the point is not that a crash is designed to preserve `owner_id`, but that **nothing exists that could clear it**: all four writers are code inside an orchestrator process and `SIGKILL` runs none of them. (ii) The repeated `waiting for storage` lines in leg 5 — a bounded 30-second retry before the non-zero exit. (iii) The `orchestrators` table — a fresh row per boot, `last_seen_at` advanced every 2 s by the live process only, read in exactly two places: `§ 8.5`'s claim and `§ 5.3`'s `orphaned`-versus-`timeout` decision |
| R36-c | **A tension flagged rather than fixed: "fail fast" versus a 30-second retry** | `§ 13.1.5` says *"fail fast: non-zero exit, and an error message that names storage as the cause"* and fixes no duration. `cmd/piton/main.go` retries the first `Ping` for 30 seconds, logging once a second, then exits non-zero — with a stated reason: a container starting beside its database is not the failure case `§ 13.1.5` describes. Both observable properties hold, so this is not a violation. **Put to the owner as his call**, not changed: if 30 s is not "fast", `SPEC.md` names a bound first and the code follows (`CLAUDE.md § 5.2`) |
| R36-d | **Next milestone: δ — replay in its variants** | `SPEC.md § 18`'s order: α → γ → β → **δ**. Ratified and needed: `§ 14` in whole, `§ 10.1` (`POST /runs/{run_id}/replay`), `§ 10.5` (the 409), `§ 6.2` (`replay_count`), `§ 6.5` (`replay_round`), `§ 12.3` (which side replay resumes), `§ 5.5` (L4 and L5 are the only entry states). Missing, as before: `§ 18.4`'s demo script, which is the owner's to rule on |
| R36-e | **δ breaks the pattern of the last three milestones: it needs real code** | γ and β required no implementation because `§ 4.2`, `§ 8` and `§ 12` had to be built for α to have a correct driving loop at all. Replay is not like that: `POST /runs/{run_id}/replay` **does not exist**. `grep -rn replay internal/ cmd/` finds only `replay_count` being stored and read, and `replay_round` being stamped onto dead-letter entries — no route, no handler, no storage method. δ is therefore the first milestone since α whose step 4 is not empty, and the first where a suite derived from `SPEC.md` will legitimately fail before the implementation exists (`CLAUDE.md § 4` step 3, R20-a) |
| R36-f | **Owner is taking δ to a separate session** | Recorded so that the next session's context is not mistaken for this one's. What it needs to know is R36-d and R36-e |

---

## Round 37 — δ's environment and suite written; the suite is red, and red in the right place (2026-09-08)

| # | Topic | Ruling / outcome |
|---|---|---|
| R37-a | **Owner ruled: δ demonstrates worker-side replay only; L5 is left to ζ** | Put to him before any file was written, because the leg list depends on it. The ground is `SPEC.md § 12.1` and `§ 6.1`: the static planner never answers `fail` and *"simply cannot fail at run time… so `planner_attempt_count` never leaves 0"*, so **L5 is unreachable with the planners that exist**. The only way in today is `planner_type: "http"` pointed at a dead address, whose `error_text` names an unbuilt milestone rather than a mechanism. This is R32-a's ruling for γ, applied a second time to the same situation — and it is `CLAUDE.md § 5`'s subtractive default doing its job twice |
| R37-b | **Owner ruled: this round produces the environment and the tests only, run red** | `CLAUDE.md § 4` step 3 in its literal form — the tests exist before the implementation. Step 4 is his to call separately. R36-e predicted this is the first milestone since α where step 4 is not empty, and that prediction is now confirmed by a running system rather than by `grep` |
| R37-c | **`SPEC.md` was NOT touched, and the suite was structured so that it did not have to be** | `§ 18.4` does not exist and `CLAUDE.md § 2` rule 1 forbids writing it unsolicited. **No assertion in `test/delta` cites a demo script**; every one traces to `§ 14`, `§ 10.1`, `§ 10.5`, `§ 12.2`, `§ 12.3`, `§ 6.2`–`§ 6.5`, `§ 6.7`, `§ 5.1`, `§ 5.3`, `§ 5.5`, `§ 5.6`, `§ 4.2`, `§ 9.7` or `§ 11.1`. R34-e is the precedent, and it has the same benefit here: the suite's authority does not depend on the demo script at all. A draft `demo.sh` is written and is a proposal, not a transcription |
| R37-d | **Six legs, and why legs 3 and 4 are a matched pair** | (1) a worker-side DLQ run replayed, its step re-dispatched, the run walking on to a step that could not exist before; (2) two rounds, dying at step 1 then step 2, with step 1 never revisited; (3) eight concurrent replays, exactly one winner; (4) a replay caught **in flight**, and a second replay refused with `409 RUNNING`; (5) a `DONE` run refused; (6) an unknown `run_id` refused with 404. **§ 14 states its gate twice, the second time in the negative** — *"is this run in DLQ right now. **Not** 'has this run been replayed before'"* — and legs 3 and 4 are the two halves of that sentence. An implementation gated on `replay_count = 0` passes leg 3 and fails leg 4 |
| R37-e | **Leg 4 is the only leg that must be observed while it happens, and the fixture is shaped around that** | `§ 14` puts the replayed step back into `RUNNING`, and a step that succeeded in milliseconds would be `DONE` before any test function ran. So δ's worker sleeps 30 s **on its success path only** — round 0 still reaches DLQ promptly, and only the replayed dispatch is slow — and the reading is **one query returning seven values**, so they are consistent with one another rather than straddling the moment the attempt finished. Beta's arrangement (R34-i's lesson) applied to a different transient |
| R37-f | **`attempt_count = 0` is asserted by inference, not by catching it** | `§ 14` resets it and `§ 4.2` burns one unit at dispatch, so a reading of **1** beside a *second* attempt row can only mean the counter was put back to 0 first — had it not been, `§ 12.2`'s budget check would have refused the dispatch and that attempt would not exist. Polling for a literal 0 was rejected: the window is up to one sweep wide, so the test would pass or fail on timing, which is the flaky-assertion failure mode R34-i warns about |
| R37-g | **Three places where the suite deliberately asserts nothing** | The **success code** of a performed replay (`§ 10.5` enumerates refusal codes only, so any 2xx is accepted and the *effect in the database* is asserted instead); the **`error` slug of a 404** (`§ 10.5` prints one body in full and it is a 409); and `§ 14`'s **`CANCELLED` row** (cancellation is `§ 15` at ι, and staging a `CANCELLED` row by hand would mean the suite writing a state the product cannot reach). R34-m is the precedent for all three. Each absence is asserted rather than left silent |
| R37-h | **Two places where `SPEC.md` is thinner than the suite would like — flagged, not fixed** | (i) **`attempt_no` across rounds.** `§ 14` resets `attempt_count` and leaves the `attempts` rows in place; `§ 6.4` calls `attempt_no` *"1-based ordering within the step"* but does not say a replayed round continues the numbering. A round restarting at 1 would give one step two rows numbered 1, and the column would stop being an ordering — so the suite asserts contiguity on that reading. (ii) **`§ 6.2` calls `replay_count` "number of completed replay rounds"** while `§ 14` increments it as the replay *begins*; `§ 6.5`'s definition of `replay_round` only works if the increment comes first, so the behaviour is unambiguous and it is the word *completed* that is loose. `CLAUDE.md § 5.2`: SPEC changes first, the test second — neither was touched |
| R37-i | **Results: red, and red in exactly one place** | `./test/delta/run.sh` — the environment came up, migrations applied, the orchestrator served, and **leg 1's run reached `DLQ` under its own power**, which is what proves δ's compose file, worker, workflows and harness all work. The fixture then stopped at the first line that needed the milestone: `POST /runs/{run_id}/replay` returned **`404 page not found`** — Go's default mux answer for a route that was never registered. No test function ran, which is the correct shape for a suite whose subject does not exist yet. `gofmt`, `go build ./...`, `go vet ./...` clean; `bash -n demos/delta/demo.sh` clean; all five workflow files parse |
| R37-j | **β's environment was taken down, with the owner's permission and not without it** | It had been left up by his hand-run (`demo.sh` without `--down`) and held port 8080, so δ's environment could not start. Asked rather than assumed: it was the environment he was inspecting, and R36-a's hand-verification does not make it disposable. He agreed |
| R37-k | **`BACKLOG.md` B18's own trigger condition has been passed** | B18 said *"the third milestone that wants the same plumbing is the point at which the trade turns — two copies is duplication, three is a pattern"*. `test/delta/harness` is the **fourth** copy. The entry was updated to record that rather than left saying something that is no longer true. Not acted on: three of the four guard milestones the owner has verified by hand, which is the same reason R33-g gave for not touching them, and `BACKLOG.md` is authority on nothing. **The trade is now his to call** |
| R37-l | **Still outstanding** | `CLAUDE.md § 4` **step 4** for δ — the implementation, which does not exist; the `§ 18.4` ruling; and then step 5, the owner's hand-run |

---

## Round 38 — δ implemented; the suite is green, and two of its three failures were the suite's own (2026-09-08)

| # | Topic | Ruling / outcome |
|---|---|---|
| R38-a | **Owner directed: implement** | `CLAUDE.md § 4` step 4, against the suite round 37 had already derived from `SPEC.md` alone. This is the first milestone since α whose step 4 was not empty, as R36-e predicted |
| R38-b | **Three files changed, and nothing else** | `internal/storage/storage.go` (a new `ErrNotInDLQ` sentinel and the `ReplayRun` method), `internal/storage/postgres/postgres.go` (the transaction), `internal/httpapi/api.go` (the route, the handler, and `slugConflict`). **`internal/engine` was not touched.** `§ 14` leaves the run in `§ 5.5`'s **L2** — run `RUNNING`, last step `RUNNING`, `attempt_count` 0 — and the driving loop's existing claim path already re-dispatches from exactly there once the budget check passes. `§ 12.3`'s *"what replay resumes: re-dispatching that step"* is therefore not a new code path but the one α had to build anyway, which is the same finding as R27-f, R33-i and R34-k arriving from the other direction |
| R38-c | **The gate is one conditional `UPDATE`, and nothing outside it decides the winner** | `UPDATE runs SET … WHERE run_id = $1 AND status = 'DLQ' RETURNING replay_count`. `§ 14` says *"the transaction that takes the run out of DLQ **is** the gate"*, and this is that sentence as SQL. It is a CAS in `§ 8.1`'s sense, and it works because of `§ 7.3` obligation 3 — *"an `UPDATE` blocked by a row lock must re-evaluate its `WHERE` clause after the lock is released"* — so the second of two simultaneous replays blocks, wakes, sees `RUNNING`, affects zero rows and is refused. A `SELECT` followed by an `UPDATE` is the shape `§ 8.1` rejects outright, and was not written |
| R38-d | **No ownership fence, and why that is not an omission** | `§ 8.2`'s fence asks *"am I still this run's owner?"* and is asked by a driver about a run it drives. A replay is the operator's, and a run in DLQ has no owner to test: `§ 6.2` makes `owner_id` non-`NULL` only while `status = 'RUNNING'`, and `§ 8.7`'s fourth writer cleared it in the same transaction that wrote the DLQ verdict. `§ 14` names its own gate in its place. `§ 14`'s *"clear `owner_id` and `claimed_at`"* is still written, because `§ 14` asks for it and says why it costs nothing — *"a `DLQ` run already holds both as `NULL`, so this is a restatement for the reader, not a second mechanism"* — so it cannot become a fifth writer of coordination metadata |
| R38-e | **A tension between `§ 8.1` and `§ 14`, flagged rather than resolved** | `§ 8.1` says *"Piton has exactly **two** CAS predicates"*, and δ's gate is a third — `run_id = :rid AND status = 'DLQ'`. It is not a contradiction on the plain reading: `§ 8.1`'s sentence closes with *"everything **in this section** is one of them"*, and `§ 14` defines its own gate outside `§ 8`. But an implementer counting CAS predicates in this codebase will now find three where SPEC says two. **Put to the owner as his call** — if `§ 8.1` should acknowledge the replay gate, `SPEC.md` says so first and nothing in the code changes (`CLAUDE.md § 2` rule 1, `§ 5.2`) |
| R38-f | **First suite run: 18 passed, 3 failed — and the implementation caused none of the three** | Recorded in full because "the tests went red" and "the code is wrong" are not the same sentence, and R34-i is the standing reminder that the difference is where the danger lives |
| R38-g | **Failures 1 and 2 were invalid SQL this agent had written** | `SELECT … FROM generate_series(0, count(*) - 1)` — Postgres answers *"aggregate functions are not allowed in functions in FROM"*, so the query never ran at all. Rewritten with `row_number() OVER (PARTITION BY …)`, which asserts the identical claim: sorted within each group the values read 0, 1, 2 … with no gap (the equality breaks) and no repeat (`row_number()` is distinct where the column would not be). **The assertion's meaning is unchanged; only the SQL expressing it changed** — this is not `CLAUDE.md § 5.2`'s forbidden move, because nothing was weakened to fit observed behaviour. The same two queries were wrong in `demos/delta/demo.sh` and were fixed there too |
| R38-h | **Failure 3 was a boolean this agent had encoded wrongly, and `demo.sh` is what proved it** | `TestReplayPutsTheRunAndItsStepBackIntoRunning` reported that a replayed run was not owned. `demo.sh` asserts the same thing and **passed**, on the same environment, which located the difference precisely: a boolean read as **its own column** prints `t` under `psql -At`, while the same boolean **concatenated into a string** prints `true`. The suite packs seven values into one row — one query, so that seven readings cannot straddle the moment the attempt finished — and therefore took the second path. Verified against a live Postgres rather than deduced: `SELECT (1 IS NOT NULL)` → `t`; `SELECT 5 \|\| chr(124) \|\| (1 IS NOT NULL) \|\| chr(124) \|\| 7` → `5\|true\|7`. Fixed by emitting `owned` / `unowned` from a `CASE`, so the snapshot no longer depends on which of the two renderings applies. **The run was correctly owned throughout; the assertion still demands that it is** |
| R38-i | **Why that one is worth writing down** | It is the failure mode R34-i named, wearing the opposite face. There, a weak fixture would have passed while testing almost nothing; here, a correct implementation was reported broken by the test's own plumbing. Both are cases where the green/red signal says something other than what it appears to, and both were only caught because a second, independent path existed — the hand-run demo (`CLAUDE.md § 4` step 5) checking the automated suite rather than the other way round |
| R38-j | **Results** | `./test/delta/run.sh` — **21 tests and 9 subtests, all pass, 72 s**, legs ending `DONE / DONE / DONE / DONE / DONE`, five runs and five dead-letter entries that survived every replay. `demos/delta/demo.sh` — **51 assertions, 0 failures**. `gofmt`, `go build ./...`, `go vet ./...` clean. No container or volume left behind |
| R38-k | **Two separate execute-bit facts, and only one of them was real** | (i) `demos/delta/demo.sh` genuinely lost its `+x` when this agent edited it from the Windows side — the edit rewrites the file and drops the mode — and the next run failed with `Permission denied`, which at first glance is indistinguishable from a demo that fails. Restored with `chmod +x` inside WSL. (ii) The six pre-existing `M demos/*/demo.sh` and `M test/*/run.sh` entries were **not** real changes: `git` run from the **Windows** side over the `\\wsl.localhost` UNC path cannot see ext4 permission bits and reports every file as `100644`. Run from inside WSL, `git status` shows those six files unmodified. Recorded because the first reading — "something stripped the execute bit off six landed scripts" — was wrong, and because it is one more instance of `CLAUDE.md § 8`'s rule earning its place: **the Windows side is an editor only, and that now demonstrably includes `git status`** |
| R38-l | **Still outstanding** | `CLAUDE.md § 4` **step 5** for δ — the owner's own hand-run — and the `§ 18.4` ruling. R38-e is his call too, and the B18 question R37-k raised is unchanged |
| R38-m | **Dead code this agent had introduced, removed after the green** | Four things existed that nothing used: `harness.RunDigest` (leg 1's append-only assertion is served by the narrower `DLQDigest`), `harness.SweepInterval` and its `configSeconds` reader (β needs those because its waits are bounded by a lease that must expire; every δ wait polls for the state itself, so there is nothing to keep in step with `piton.yaml`), and `demo.sh`'s `replay_body` shell function. Removed rather than left, per `CLAUDE.md § 5`'s subtractive default — unused scaffolding in a test harness reads as a capability the suite has and does not. `Scalar` and `Int` in `assert.go` were **kept** although δ uses neither: they are part of the assertion vocabulary the other three harnesses share, and diverging this copy would make B18's eventual merge harder rather than easier |

---

## Round 39 — δ re-verified after a dead-code sweep; the owner is directed to the hand-run (2026-09-08)

| # | Topic | Ruling / outcome |
|---|---|---|
| R39-a | **Owner asked for replies in Chinese** | Recorded as a standing preference. It covers conversation prose only: everything written into the repository stays English under `CLAUDE.md § 7`, so `SPEC.md`, this log, code comments, test names and `demo.sh` output are unaffected |
| R39-b | **Dead code δ had accumulated is gone from the tree** | `harness.RunDigest`, `harness.SweepInterval`, `harness.configSeconds` (and with them the `regexp` / `strconv` imports), the stranded `SlugNotFound` doc comment, and `demo.sh`'s unused `replay_body`. All were introduced by this agent in round 37 and never used by any assertion. **These edits were not made by this agent in this session** — they were found already applied on disk — so the removal is recorded as a fact about the tree rather than claimed as work done here |
| R39-c | **The suite was re-run before the owner was pointed at the hand-run, and that is a rule rather than caution** | `CLAUDE.md § 5.4`: tests run "always before the owner is asked to look at a milestone by hand". Files had changed after the last green run, so the previous green no longer described the tree. **21 tests, 9 subtests, all pass, 80 s**; `gofmt`, `go build ./...`, `go vet ./...` clean; no container or volume left behind |
| R39-d | **Owner asked which of the two he should run by hand** | Answered: the **demo**, not the suite. `CLAUDE.md § 4` step 5 is his and is "not replaceable by step 6", and `SPEC.md § 17.4` is the sentence behind it — "a green suite the owner has never seen behind is not evidence that a milestone landed". Round 38 had just demonstrated the same point from the other direction: it was `demo.sh` passing while the suite failed that located R38-h's defect in the suite's own plumbing |
| R39-e | **What he was told to look at, and why those three** | (i) `attempt_count = 1` beside three `attempts` rows — the divergence `§ 6.3` exists to permit, and the reason that column is stored rather than derived; (ii) leg 2's two entries, `replay_round` 0 and 1 naming different steps, with the first step untouched by the second round — `§ 14`'s accepted limitation seen rather than asserted; (iii) five `DONE` runs beside five surviving dead-letter entries — `§ 6.7`'s append-only rule at the one moment something would be tempted to tidy the entry away, since what it recorded is no longer true |
| R39-f | **Still outstanding** | `CLAUDE.md § 4` **step 5** for δ; the `§ 18.4` ruling; R38-e (whether `§ 8.1`'s "exactly two CAS predicates" should acknowledge the replay gate); and R37-k (B18's four harness copies) |

---

## Round 40 — δ committed and pushed; ζ scoped; `fetch_base_url` given a home and transcribed (2026-09-09)

| # | Topic | Ruling / outcome |
|---|---|---|
| R40-a | **δ is closed, committed and pushed** | The owner reported δ complete, which discharges `CLAUDE.md § 4` **step 5** — the item carried since R38-l and R39-f. Committed as `dee2531`: the implementation, `demos/delta/`, `test/delta/`, rounds 37–39 and `BACKLOG.md`'s B18 update, in one commit, then pushed to `origin/main` |
| R40-b | **R38-k's rule earned its place a second time** | A Windows-side `git status` again reported six modified `demos/*/demo.sh` and `test/*/run.sh` files. From inside WSL the tree held five modified files and two untracked directories and nothing else. The UNC path cannot see ext4 permission bits, so **every** git operation in this round was run through `wsl.exe`, not from the Windows side |
| R40-c | **Next milestone: ζ — a planner that is an external HTTP service** | Until now "which step comes next" is a list fixed at `POST /workflows` and nothing can change its mind. ζ makes the decision a synchronous HTTP call to a service the user owns, which may be an LLM. `SPEC.md § 18` puts it fifth for a stated reason: it is the first milestone that makes the system useful to somebody other than its author |
| R40-d | **ζ needs real code, and more of it than δ did** | `internal/planner` holds the static planner only, and `internal/engine` answers an `http` workflow with *"milestone zeta and is not implemented in this build"*, reported as a planner call that could not be made — so the run burns planner budget and converges to DLQ under `§ 12.2` rather than hanging. What is missing is a planner client: M1 out (`§ 9.2`), M2 parsed (`§ 9.3`), `planner_timeout_seconds`, the budget path, and the four `planner_*` dead-letter reasons of `§ 6.5` |
| R40-e | **Owner ruled: tests are written from `SPEC.md`, and a demo script is never a source** | He asked what "a test citing a demo script" even meant. Answered: a demo script is written by this agent, so a test derived from one is this agent checking its own homework — `CLAUDE.md § 5.1` in plain words. His answer settles ζ the way R37-c settled δ: `test/zeta` will trace every assertion to a ratified section, and `demos/zeta/demo.sh` is a proposal until he rules on it |
| R40-f | **Owner ruled: "switching between planners" means the choice made at workflow creation, and nothing else** | `SPEC.md § 18`'s ζ row says *"and switching between planners"*, and no section anywhere defines it — `§ 10.1` has no endpoint that modifies a workflow. The reading is now fixed: **you choose `static` or `http` when you create the workflow. That is it.** No new endpoint, no new wire shape. ζ's demo shows one `static` workflow and one `http` workflow over the same workers, both reaching `DONE` — the engine not caring who decides |
| R40-g | **Owner ruled: no real LLM inside the automated suite** | An LLM call is non-deterministic, needs a key and needs the network, and a suite that answers differently on different days guarantees nothing. If a live LLM is wanted it is a manual leg outside the suite |
| R40-h | **How a dynamic planner is proved rather than merely shown** | The owner's own shape: a first worker produces something, and the planner picks the second worker from it. The fixture that makes it an argument: the `classify` worker **alternates** its answer between two values from its own internal state, and **two runs are started with identical input**. The planner receives a catalogue that carries `output_bytes` and no content (`§ 9.2`), so to branch at all it must fetch the output through the read API (`§ 10.2`). Two runs with identical input taking different paths is therefore proof that the fetch happened — with no fetch the planner holds nothing that differs between them — and a static planner cannot produce it by construction. A second, direct reading is available too: the demo planner is our own fixture and can expose a count of what it fetched |
| R40-i | **A gap in `SPEC.md` found before any file was written: `fetch_base_url` had no source** | `§ 9.2` makes the field **required** in every M1 — it is how the planner is told where to read the outputs the catalogue omits — but `§ 4.4`'s list of what the configuration file must declare does not mention it, `internal/config` has no such key, and the string appears nowhere in the tree. ζ cannot be built without deciding where it comes from, and "the orchestrator's own externally visible address" is a definition, so `CLAUDE.md § 9` forbade choosing one silently |
| R40-j | **Three candidate homes were put to the owner, with the cost of each** | (甲) the operator supplies it at `POST /workflows`, required iff `planner_type = 'http'`; (乙) the orchestrator's configuration file holds it and the value is stamped onto the workflow row at creation; (丙) the configuration file holds it and it is read at each planner call. A fourth possibility — dropping the field and letting each planner know its own orchestrator — was analysed and rejected in the same exchange: it does not remove the network requirement, only moves who holds the string, and its failure mode is a planner pointed at another deployment, where reads **succeed** against the wrong database and produce a wrong decision instead of an error |
| R40-k | **Owner ruled: 甲** | Two reasons recorded. (i) It is one half of a pair: `planner_url` is where the orchestrator calls this workflow's planner and `fetch_base_url` is where that planner reads back — one relationship, two directions, settled at the same moment, in the same row, expiring together. (ii) A single process-wide value cannot give two workflows two different routes to the same orchestrator, and a planner inside the cluster and one hosted outside it do not reach the read API by the same address |
| R40-l | **The cost of 甲, accepted with his eyes open** | `§ 10.1` has no endpoint that modifies a workflow (R40-f), so a deployment that changes its externally visible address strands every existing `http` workflow — including the **replay** of a run sitting in DLQ under one, which re-asks the planner and hands it an address that no longer answers. The remedy is a new workflow. This is **not a new class of cost**: `planner_url` is already immutable in the same way and fails the same way when a planner moves, so 甲 adds no concept — whereas 丙 would have made one address of the pair mutable and the other not |
| R40-m | **`SPEC.md` transcribed on the owner's direction — four places** | `CLAUDE.md § 2` rule 1: he ruled, and the edits were put to him before they were made. (i) `§ 6.1` gains the `fetch_base_url` column, *"required iff `planner_type = 'http'`; must be absent otherwise"*, placed beside `planner_url`; (ii) `§ 6.1`'s invariant sentence now names both `http` columns together; (iii) a **Why** paragraph in `§ 6.1` records R40-k's reasons and R40-l's accepted cost in full; (iv) `§ 16` rule 2 requires it and rule 3 rejects it under `static`; (v) `§ 9.2`'s field table says the value is the workflow's column, verbatim. **`§ 4.4`'s configuration list was NOT touched** — 甲 makes it moot, and the question of adding a line to it, raised earlier in the same exchange, died with the ruling |
| R40-n | **Rule numbering in `§ 16` was extended, never renumbered** | `internal/validate/validate.go` cites *"SPEC.md 16 rule 2"* in its error text, so inserting a new numbered item would have silently renamed a rule the code points at. Rules 2 and 3 were widened instead |
| R40-o | **Why the schema change is a new migration and not an edit to the existing one** | Asked by the owner, because "the applied migration may not be edited" and "a column is being added" sounded contradictory. `golang-migrate` decides what to run **by version number, not by content**: a database that has recorded version 1 will never re-read file 1, so editing it leaves existing databases without the column while fresh ones get it — two databases both calling themselves version 1 with different shapes. A new `000002` is an instruction every database runs exactly once, so all of them converge. Not academic here: R37-j's β environment survived a session, so long-lived databases on this machine are a thing that happens |
| R40-p | **What ζ inherits, twice deferred** | The planner-side dead-letter demonstration, dropped from γ by R32-d and from δ by R37-a because the static planner cannot fail. It is due now: the four `planner_*` reasons of `§ 6.5`, the **L5** combination of `§ 5.5`, and `§ 14`'s replay of an L5 run — which resumes by **asking the planner again**, not by re-dispatching a step, and is the other column of `§ 12.3` |
| R40-q | **Still outstanding** | `§ 18.4` — δ's demo script, never ruled on; `§ 18.5` — ζ's, which does not exist and will be handled as δ's was (R37-c); R38-e — whether `§ 8.1`'s *"exactly two CAS predicates"* should acknowledge the replay gate; R37-k — B18's four harness copies. None is blocking ζ |
