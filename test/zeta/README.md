# Milestone ζ — the automated suite

This directory holds the automated suite for milestone ζ: **the HTTP planner**. It is **not** the
demo script; `test/alpha/README.md` states that split once and it is not repeated here.

Both run against `demos/zeta/docker-compose.yml` — `CLAUDE.md § 5.5.4` forbids the suite from
defining an environment of its own.

```bash
./test/zeta/run.sh              # the group
./test/zeta/run.sh -run Gate    # any go test flag passes straight through
go test -short ./...            # skips it; needs no docker
```

## What ζ is

Until ζ, the only planner is the built-in static one: a workflow's steps are a list fixed at
`POST /workflows` and nothing can change its mind. ζ adds the **HTTP planner** — the orchestrator
asks an external HTTP service, synchronously, what the next step is (`SPEC.md § 9.2`, `§ 9.3`), and
that service can branch on what earlier steps actually produced by reading them back through the
read API (`SPEC.md § 10.2`) rather than being sent them inline.

`GRILLING_LOG.md R40-f` fixes the scope precisely: *"switching between planners" means the choice
made at workflow creation, and nothing else* — `static` or `http`, chosen once in the workflow's
`planner_type` column (`SPEC.md § 6.1`), and no endpoint to change it afterward. ζ's demo is one
static workflow and one http workflow run over the same workers, both reaching `DONE` — the engine
not caring who decided the steps.

`R40-p` adds one more thing ζ owes, twice deferred: the **planner-side dead-letter demonstration**,
dropped from γ (`R32-a`) and from δ (`R37-a`) because the static planner cannot fail at run time
(`SPEC.md § 12.1`). ζ is the first milestone with a planner that genuinely can — an unreachable or
misbehaving HTTP planner burns `planner_attempt_count` and reaches DLQ under `SPEC.md § 12.2` with
**zero steps**, combination **L5** of `SPEC.md § 5.5`. `SPEC.md § 14`'s replay of an L5 run resumes
by *asking the planner again*, not by re-dispatching a step — the other column of `§ 12.3` from the
one δ's suite exercises.

## Where the environment's fourth service comes from

`demos/zeta/docker-compose.yml` is being written separately from this scaffolding and may not exist
yet. It adds a fourth service, `planner` — a fixture HTTP planner the suite talks to directly at
`harness.PlannerBaseURL` (`http://localhost:9100`) as well as indirectly, through the orchestrator,
as `planner_url`. This directory does not bring that environment up; `harness.Up` does, exactly as
in every earlier milestone, against whatever `demos/zeta/docker-compose.yml` turns out to define.

## Group structure

`test/zeta/planner` will be the suite's single group. `CLAUDE.md § 5.5.3` forces a group of its own
only for concurrency and fencing work that manipulates global coordination state
(`runs.owner_id`, `orchestrators`); ζ has no such leg planned, so — like δ before it — one group is
the right shape unless a specific concurrent assertion later requires otherwise.

## This suite is currently expected to FAIL

At the time of writing, **milestone ζ has no implementation.** `internal/planner` holds only the
static planner, and `internal/engine` answers any `http`-typed workflow with a message stating that
milestone zeta is not implemented in this build (`SPEC.md § 19.3`), reported as a planner call that
could not be made so the run still converges to DLQ rather than hanging forever.

This is `CLAUDE.md § 4` step 3 working as intended: tests are derived from `SPEC.md` *before* the
implementation exists, so that the suite is a specification the implementation is written against
rather than a description of what the implementation happens to do. A red suite at this stage is not
a defect in the scaffolding — it is the expected state until `CLAUDE.md § 4` step 4 is done.

No test cases are described here because none are written yet. `test/zeta/planner/*_test.go` and the
demo environment that backs it are separate, later work.
