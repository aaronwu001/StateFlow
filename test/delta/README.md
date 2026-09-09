# Milestone δ — the automated suite

This directory holds the automated suite for milestone δ: **replay**. It is **not** the demo script;
`test/alpha/README.md` states that split once and it is not repeated here.

Both run against `demos/delta/docker-compose.yml` — `CLAUDE.md § 5.5.4` forbids the suite from
defining an environment of its own.

```bash
./test/delta/run.sh              # the group
./test/delta/run.sh -run Gate    # any go test flag passes straight through
go test -short ./...             # skips it; needs no docker
```

## What δ is scoped to, and by whose ruling

δ demonstrates **worker-side replay only** — `SPEC.md § 12.3`'s left-hand column, *"what replay
resumes: re-dispatching that step"*. The planner-side column (**L5**) is left to milestone ζ.

The reason is `SPEC.md § 12.1`, not convenience: *"the static planner simply cannot fail at run time
— § 6.1 validates its steps at submission, and it holds no state and makes no network call, so
`planner_attempt_count` never leaves 0"*, and § 6.1 says it never answers `fail`. **L5 is therefore
unreachable with the planners that exist.** The only way to enter it today is to point
`planner_type: "http"` at an address nothing answers, and the `error_text` that lands in the
dead-letter entry then names an unbuilt milestone rather than a mechanism — which is exactly the
ground on which the owner removed the same leg from γ (R32-a). The owner ruled the same way for δ.

`TestDeltaIsWorkerSideOnly` asserts that absence rather than leaving it silent.

## Where the assertions come from — and what they deliberately do not cite

`SPEC.md § 18.4` **does not exist.** `CLAUDE.md § 2` rule 1 forbids writing it unsolicited, so **no
assertion in this suite cites a demo script.** Every one traces to a section that is already
ratified:

| Section | What it settles |
|---|---|
| `§ 14` | The whole of replay: the gate, the transaction, `run_id`, the accepted limitation, the record |
| `§ 10.1` | `POST /runs/{run_id}/replay` |
| `§ 10.5` | The rejection body, and the code table |
| `§ 12.2`, `§ 12.3` | The DLQ transaction, and which side a replay resumes |
| `§ 6.2`, `§ 6.3`, `§ 6.4`, `§ 6.5`, `§ 6.7` | `replay_count`, `attempt_count` vs the `attempts` rows, `attempt_no`, `replay_round`, append-only |
| `§ 5.1`, `§ 5.3`, `§ 5.5`, `§ 5.6` | Terminal states, failure reasons, L2/L3/L4, the impossible combinations |
| `§ 4.2`, `§ 9.7`, `§ 11.1` | Budget burned at dispatch, the legal mode combination, a *total* attempt count |

This is R34-e's arrangement, and it has the property β's suite has: the suite's authority does not
depend on the demo script at all.

## The six legs

| Leg | What happens | Rule |
|---|---|---|
| 1 | A worker-side DLQ run is replayed; the step is re-dispatched, succeeds, and the run walks on to the step that never existed before | `§ 14`, `§ 12.3` |
| 2 | Two rounds: round 0 dies at step 1, round 1 dies at step 2, round 2 finishes. Step 1 is never revisited | `§ 14`'s accepted limitation, `§ 6.5` |
| 3 | Eight replays of one DLQ'd run, fired at once. Exactly one wins | `§ 14`'s idempotency gate |
| 4 | A replay is caught **in flight**: run `RUNNING`, step `RUNNING`, budget reset then burned once — and a second replay is refused *because of what the run is now* | `§ 14`, `§ 5.5` L2, `§ 10.5` |
| 5 | Replaying a `DONE` run is a 409 that states the truth about the run **and** its step | `§ 14`, `§ 10.5` |
| 6 | Replaying a `run_id` that names no run is a 404 | `§ 10.5` |

**Legs 3 and 4 are the pair that pins `§ 14`'s gate.** § 14 states the gate twice and in the
negative: *"the idempotency gate is 'is this run in DLQ right now'. **Not** 'has this run been
replayed before'."* Leg 3 shows the first half — two callers race and the transaction settles it.
Leg 4 shows the second — the run *has* been replayed, and the refusal it gets says `RUNNING`, not
"already replayed". An implementation gated on `replay_count = 0` would pass leg 3 and fail leg 4.

**Leg 4 is the only leg that must be observed while it happens.** `§ 14` puts the replayed step back
into `RUNNING`, and a step that succeeded in milliseconds would be `DONE` before any test function
ran. Its worker therefore sleeps 30 s **on the success path only**, so round 0 still reaches DLQ
promptly and only the replayed dispatch is slow. The reading is one query returning seven values, so
that they are consistent with one another rather than straddling the moment the attempt finished.

## Why the fixture is a script and the tests only assert

Same shape as β's, for the same reason: what δ demonstrates is partly only true while it is
happening. `TestMain` drives the six legs and records what it observed — with real queries at the
moment they were true — and each test asserts one rule against that record plus the final state of
the database. R34-i is the standing warning behind this: a fixture that proceeds from a state it did
not verify produces a suite that passes while testing almost nothing, so every leg checks that it
actually reached DLQ before it replays anything.

## Why this is one group

`CLAUDE.md § 5.5.3` forces α's `ownership` group and the whole of β to stand alone because they
manipulate **global** coordination state — `runs.owner_id` across runs, and the `orchestrators`
table. δ does not: `§ 14` clears `owner_id` and `claimed_at` on **one** run, inside that run's own
replay transaction, and nothing here kills a process, expires a lease or reads `orchestrators`. The
legs still run one after another, because `§ 4.4`'s deployment shape is one orchestrator and a
failure explainable by contention explains nothing.

Leg 3 is concurrent *within itself*. That is the leg's point, not an exception: `§ 14`'s gate is a
claim about two callers racing for one run, and it cannot be reproduced by two callers taking turns.

## What δ does not demonstrate, and why

- **`§ 14`'s `CANCELLED` row.** Cancellation is `§ 15`, ordered at milestone **ι**, so
  `POST /runs/{run_id}/cancel` does not exist. Staging a `CANCELLED` row by hand would mean the
  suite writing a state the product cannot reach, which is not evidence about the product.
  `TestWhatDeltaDoesNotDemonstrate` asserts the absence instead.
- **Planner-side replay (L5).** See the scope section above.
- **The success code of a performed replay.** `§ 10.5` enumerates codes for *refusals* only, and no
  ruling fixes what a performed replay returns. The suite accepts any 2xx and asserts the **effect
  in the database**, which is specified. R34-m is the precedent.
- **The `error` slug of a 404.** `§ 10.5` prints one body in full and it is a 409. The 404's code is
  asserted; its slug is not, because fixing a string here would invent a contract no ruling covers.

## Two places where `SPEC.md` is thinner than the suite would like

Recorded here rather than resolved, because `CLAUDE.md § 2` rule 1 and `§ 5.2` both say the same
thing: SPEC changes first, the test second.

1. **`attempt_no` across replay rounds.** `§ 14` resets `attempt_count` and leaves the `attempts`
   rows in place; `§ 6.4` calls `attempt_no` *"1-based ordering within the step"*. It does not say in
   so many words that a replayed round continues the numbering — but a round that restarted at 1
   would give one step two rows numbered 1, and the column would no longer be an ordering. The suite
   asserts contiguity on that reading (`TestAttemptNumbersStayContiguousAcrossRounds`).
2. **`replay_count` is described as *"number of completed replay rounds"* (`§ 6.2`)** while `§ 14`
   increments it as the replay *begins*. The behaviour is unambiguous — § 14 dictates the
   transaction's contents, and `§ 6.5`'s definition of `replay_round` only works if the increment
   comes first — so the suite tests § 14's reading. It is the word *completed* that is loose.
