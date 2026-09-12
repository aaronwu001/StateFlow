# Milestone θ — the automated suite

This directory holds the automated suite for milestone θ. It is **not** the demo script; the two
are different steps of `CLAUDE.md § 4` with different jobs.

| | `demos/theta/demo.sh` | this suite |
|---|---|---|
| Which step | 2 — written before the code | 3 to write, 6 to guard |
| Job | fix what the operator must see, and let him see it | guarantee that what he saw **stays** true |
| Acceptance evidence | **yes** (`§ 4` step 5) | no — a green run the owner has never looked behind is not evidence a milestone landed |

Both read the same two interfaces: the HTTP API of `SPEC.md § 10`, and database truth
(`SPEC.md § 17.1`). Both run against `demos/theta/docker-compose.yml` — `CLAUDE.md § 5.5.4` forbids
the suite from defining an environment of its own, so that the owner's hand-run demo and the suite
cannot diverge.

## How to run it

```bash
./test/theta/run.sh              # every group
./test/theta/run.sh -run RawBody # any go test flag passes straight through
go test -short ./...             # skips both groups; needs no docker
```

Everything executes inside WSL (`CLAUDE.md § 8`). No `psql` is needed on the host: the suite reaches
the database through `docker compose exec postgres psql`, the same path `SPEC.md § 17.1` gives the
operator, because `demos/theta/docker-compose.yml` deliberately publishes no host port for Postgres.

## What θ is

`SPEC.md § 18` states the capability in one line: **sync raw body — an unmodifiable HTTP endpoint
works as a worker.** The environment therefore has a fourth service, `rawapi`, written the way a
third-party API is written: it does not know what a run, a step or an attempt is, it never looks for
`status` or `output`, and it has no idea anything is retrying it.

Piton's own envelope worker is still there, because a raw-only environment would show that raw
dispatch works and nothing about what it is for. `SPEC.md § 9.5`'s `inputs` is *"a map from
`step_id` to that step's stored output"* with no exemption for a step that ran in raw mode — so a
raw worker's result must reach the next worker like any other, and that claim needs one of each
inside a single run.

## The groups

`CLAUDE.md § 5.5` defines a group as one docker-compose environment, brought up for a set of tests
and torn down — volume wipe included — before the next group starts. A Go package is the unit that
can own a `TestMain`, so **one package is one group**, and `run.sh` passes `-p 1` so that Go does not
run two of them at once.

| Package | Group | Contains |
|---|---|---|
| `rawpath` | the θ scenario | one run, three steps: the raw body was `params` and **nothing else**, sent as `application/json` (`§ 9.5`); a key named `input_from` *inside* `params` arrived verbatim; the entire response body is the output (`§ 9.6`); a 2xx that happens to look like Piton's failure envelope is still a **success**, stored whole; the envelope step in the same run is still unwrapped; and a raw step's stored output reaches the next worker as `inputs` |
| `rawfailure` | the two failures only raw can produce | a non-2xx is `transport_error` and its body becomes `error_text`, truncated to 4 KB (`§ 9.6`, `§ 6.4`); a 2xx whose body is not JSON is `invalid_response` (`§ 9.6`); both exhaust their budget and land in a worker-side dead-letter entry (`§ 6.5`, `§ 12.2`, `§ 12.3`); and the two are **distinguishable**, which is `§ 5.3`'s reason for the column |

There is no `ownership` group and no `validation` group here. `§ 5.5.3` reserves a separate group
for tests that assert **global** coordination state, and θ asserts none. Validation is already
covered where it belongs: `test/alpha/validation` asserts all six rules of `§ 9.8` — including
rule 3, `async` with `raw`, and rule 4, `input_from` at StepSpec level beside `raw` — and θ adds
nothing to that list. `CLAUDE.md § 5` makes scope subtractive; duplicating those cases here would
mean two places to fix when one rule changes.

## Where the assertions come from

`CLAUDE.md § 5.1` permits exactly one source: `SPEC.md`. Every assertion names the section it came
from, and a failure reports that section rather than only the query that returned false.

Nothing was derived by reading the implementation, and the order is on the record rather than
claimed. When this suite was written raw dispatch did not exist: `internal/dispatch` answered every
raw step with an explicit *"milestone theta and is not implemented in this build"* failure. It was
**run in that state**, and what it reported is what `§ 4` step 3 exists to produce — every
assertion in `rawpath` red, and `rawfailure` reporting that the not-JSON run had been labelled
`transport_error` rather than `invalid_response`, because nothing had been sent to the endpoint at
all. The implementation landed afterwards, and the same assertions went green without one of them
being edited.

One rule in this suite is younger than the rest. `§ 9.6`'s requirement that a raw worker's body be a
valid JSON document was ruled at round 48, because `§ 9.6` promised that the entire body verbatim is
the output while `§ 6.3` and `§ 6.4` call an output *JSON bytes*. The suite asserts the rule as
`SPEC.md` now states it, not as the conversation reached it (`§ 5.1`).

If an assertion here and `SPEC.md` disagree, `SPEC.md` wins (`§ 5.2`). If you believe `SPEC.md` is
wrong, say so and stop — the owner rules, `SPEC.md` changes first, and the assertion changes second.
Never adjust an assertion to match the code.

## A note on "verbatim"

`§ 9.5` and `§ 9.6` both use the word, and the columns it lands in are `JSONB`, which normalises
whitespace and key order. Document equality is therefore the strongest claim these queries can make
— and it is the claim the rules are about: no field added, none removed, none rewritten.

## The harness is the fifth copy

`BACKLOG.md` B18 records that this harness exists once per milestone, and R37-k records what the
duplication has already cost — a fix applied to one copy and not the others. θ's copy is not a
widening of that debt: it differs from α's only where θ differs, in which environment directory it
points at and in taking a workflow file by name, because θ submits three workflows rather than one.
