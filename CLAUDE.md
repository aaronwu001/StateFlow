# CLAUDE.md — Working Agreement

This file says what **you (Claude)** must do. It does not describe the system; `SPEC.md` does that.
It does not hold milestones (those are in `SPEC.md`) and it does not hold unscheduled work (that is
in `BACKLOG.md`).

---

## 1. The document set

| File | Voice | Authority |
|---|---|---|
| `SPEC.md` | "the system MUST…", each non-obvious rule carrying a `Why:` | **The only authority on behaviour.** If code and SPEC disagree, the code is wrong |
| `CLAUDE.md` | "you (Claude) MUST…" | Authority on how work is done |
| `BACKLOG.md` | "not scheduled, may never happen" | Authority on nothing. A parking lot |
| `notes/` | "what we discussed, what we eliminated, why" | **Explanatory only.** May never be cited as justification for a behaviour |
| `GRILLING_LOG.md` | the record of every ruling and its reasoning | **Explanatory only**, same rule as `notes/` |
| `legacy/` | the previous project's documents | **VOID.** Read for "how they thought", never for "so we do the same" |

The separation is by **voice**, not by topic. A single topic legitimately appears in three files
saying three different kinds of thing about it.

**Superseded entries in `GRILLING_LOG.md` are not deleted.** Deleting them is maintenance cost for
no benefit; the rule that protects against confusion is that the log is never an authority.

---

## 2. Hard rules

1. **You never edit `SPEC.md` on your own initiative.** You propose; the owner rules. When the owner
   directs you to transcribe a specific ruling into SPEC, that direction *is* the ruling — but an
   unsolicited edit, however obviously correct it seems, is not permitted.
2. **You never cite `notes/`, `GRILLING_LOG.md`, or `legacy/` as a reason the system behaves a
   certain way.** If a behaviour is real, it is in SPEC. If it is not in SPEC, it is not a rule yet
   — say so and ask.
3. **You never write a test by reading the implementation.** See §5.
4. **You never copy code from the old project.** Reading it to see how something was expressed in Go
   is allowed; porting by copy-paste is not.
5. **Scope is subtractive by default.** The burden of proof is on *including* something. Shipping
   something smaller with known small gaps earlier beats shipping something complete later.
6. **When you are uncertain, ask.** Do not choose an interpretation and proceed silently. This
   applies especially to anything that will become a definition.
7. **All documents are written in English**, complete in architecture and clear in narration, so
   that someone new can use them as reference material.
8. **`GRILLING_LOG.md` is updated after every exchange with the owner**, without exception,
   recording what was discussed, what was eliminated, what was chosen, and the reasoning.

---

## 3. The admission test for new work

Before adding anything not already in SPEC, answer one question:

> **Would adding this later be awkward?**

- **Yes** — it is a definition, a wire-format semantic, a state-machine rule, or a contract
  expectation that other people will build against. Settle it **now**, even if it will not be
  implemented for several milestones.
- **No** — it is an optional field, an extra endpoint, a performance optimisation, a new default.
  It goes to `BACKLOG.md`, and the current milestone stays small.

This is why some things that will not be built for a long time are nevertheless fully specified,
and why some things that sound useful are deliberately absent.

---

## 4. Development flow

A milestone is a **demo scenario**, not a layer. It is a user-visible capability that can be shown
end to end. Work one milestone at a time, in the order `SPEC.md § Milestones` gives.

For each milestone:

1. **Confirm the SPEC sections it depends on are ratified.** If a rule the milestone needs is
   missing or ambiguous, stop and ask — do not invent it.
2. **Write the demo script first.** Literally: the commands the operator will type in a terminal,
   and the database state he must see afterwards. If you cannot write this, the milestone is not
   understood yet.
3. **Derive the tests from SPEC** — before the implementation exists. See §5.
4. **Implement.**
5. **The owner runs the demo by hand** and inspects database truth from a terminal.
6. **The automated suite then guards it.** Its job is to guarantee that what the owner saw by hand
   stays true.
7. **Sweep `SPEC.md` for statements the landing has made false**, and propose the correction. Today
   that is `SPEC.md § 19.3`'s table — a row leaves it when the thing is built — but the sweep is for
   *any* sentence that describes what exists in the code rather than how the system must behave.
   You propose; the owner rules (§2 rule 1).

Step 5 is not optional and is not replaceable by step 6. A green suite that the owner has never
seen behind is not evidence the milestone landed.

**Why step 7 exists.** `SPEC.md § 19.3` was written once, before any milestone was implemented, and
was never updated. It still announced the HTTP planner as unbuilt four rounds after milestone ζ had
landed and been hand-verified. Nothing caught it, because nothing was responsible for catching it:
every other step of this flow is about making the code match the document, and this is the one place
where the document must be made to match the code. A section that goes stale by someone doing their
job correctly needs a step that says whose job the update is.

---

## 5. Testing discipline

### 5.1 Where a test may come from

| Source | Allowed? |
|---|---|
| `SPEC.md` | **Yes — this is the only legitimate source** |
| The old project's test files | **No.** They encode the old design's assumptions, and importing them silently imports those assumptions |
| The new implementation | **No.** A test written by reading the code tests what the code *does*, not what it *should do*, and will happily certify a bug |
| The owner's spoken ruling, before it reaches SPEC | **No** — get it into SPEC first |

### 5.2 When a test and SPEC disagree

SPEC wins. If you believe SPEC is wrong, say so and stop; the owner rules; SPEC changes first and
the test changes second. Never adjust a test to match the code.

### 5.3 What must be tested against a real database

The correctness argument in `SPEC.md § 8` rests entirely on database semantics — row locks,
conditional updates, the re-evaluation of a blocked `UPDATE`'s predicate, transaction boundaries.
**A fake or mocked storage cannot verify any of it.** Anything that touches ownership, claiming,
fencing, the attempt CAS, or the sweep must be tested against a real Postgres instance from
`docker compose`.

Storage-independent logic may be tested without a database, but it is the minority of this system.

### 5.4 When tests run

Continuously, and always before the owner is asked to look at a milestone by hand.

### 5.5 Test environment lifecycle

`SPEC.md § 4.4` defines the environment (one `docker-compose.yml` plus one hand-run demo script per
milestone directory). This section defines how the automated suite is run against it.

1. **Group by test file / scenario.** One docker-compose environment is brought up per test file (or
   per milestone scenario). Every test in that group runs against it. It is then torn down —
   **including a volume wipe** — before the next group starts.
2. **Groups never share a live environment.** A group starts from a clean database, always.
3. **Concurrency and fencing tests form their own group** and are never interleaved with unrelated
   tests. They manipulate global coordination state (`runs.owner_id`, `orchestrators`), so a test
   running beside them would see a database that no single test put into that condition.
4. **Automated tests reference the milestone's own compose file**, never a private one.
   The owner's manual demo run and the suite must run against an identical environment definition —
   otherwise a green suite proves nothing about what he saw by hand (§4 step 5).

---

## 6. Observability is a requirement, not a nicety

The owner must be able to run the system by hand and see inside it — database state, and ideally
other runtime state — from a **terminal**. There is no UI. Anything that makes the system's state
visible only through the automated suite has failed this requirement.

---

## 7. The old project

`~/Projects/StateFlow` holds the previous implementation. It is **readable, reference only**.

- Allowed: "how did they express X in Go?"
- Not allowed: "they did X, so we do X." Every rule in Piton must be traceable to `SPEC.md`.
- **Especially not allowed when writing tests** (§5.1).

⚠ `~/Projects/sf-impl` and `~/Projects/sf-blind` also exist on this machine. Unless the owner rules
otherwise, treat them under the same rule as `~/Projects/StateFlow`.

---

## 8. Tech stack

Go, Postgres, Docker Compose, `golang-migrate`. This was never a source of the old project's
problems and is not being revisited.

Module path: `github.com/aaronwu001/piton`.

**Installed on this machine, 2026-09-01** (inside WSL Ubuntu 26.04, per the rule below):

| Tool | Version | Where |
|---|---|---|
| Go | 1.27.0 | `/usr/local/go`, on `PATH` via `/etc/profile.d/go.sh` |
| Docker Engine | 29.7.2 | native `apt` install, `/usr/bin/docker` — **not** Docker Desktop |
| Docker Compose | v5.5.0 | `docker compose` plugin |

`go.mod` declares `go 1.27.0` — a real toolchain version, never a guessed one. Docker runs under
systemd (`systemctl is-enabled docker`), so it starts with the distro and needs nothing launched by
hand on the Windows side.

**Everything executes inside WSL** — the Go toolchain, `docker compose`, `golang-migrate`, `psql`
and the test suite. The Windows side is an editor only. Never run a build, a container or a
migration through the Windows-side shell against the UNC path.
**Why this lives here and not in `SPEC.md`:** it is a fact about this machine, not a rule about the
system's behaviour. A conforming Piton does not care what OS built it.

**Everything executes inside WSL** — the Go toolchain, `docker compose`, `golang-migrate`, `psql`
and the test suite. The Windows side is an editor only. Never run a build, a container or a
migration through the Windows-side shell against the UNC path.
**Why this lives here and not in `SPEC.md`:** it is a fact about this machine, not a rule about the
system's behaviour. A conforming Piton does not care what OS built it.

---

## 9. When to stop and ask

Stop and ask — do not proceed on an assumption — when:

- a SPEC rule the current work needs does not exist, or admits two readings;
- you are about to introduce a term that will function as a definition;
- you are about to add something that fails the §3 admission test but seems necessary anyway;
- an old-project behaviour looks load-bearing but has no SPEC rule behind it;
- a test you derived from SPEC fails in a way that suggests SPEC itself is wrong.
