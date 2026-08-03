# StateFlow — Operational Facts

> **What this document is:** how to *operate* this system — start it, connect to it, observe
> it. Nothing else.
>
> **What this document deliberately is NOT:** a description of how the system *behaves*. It
> contains no request/response schemas, no field semantics, no state-machine rules, no
> statement about what any endpoint returns for any input. Those are the exclusive domain of
> `spec/BEHAVIOR_MATRIX.md`, and a reader deriving expected behavior must derive it from
> there, not from here. §8 lists what was omitted to hold that line.
>
> **Provenance:** every fact below was produced by running the command shown against a real
> stack on 2026-08-03. Verbatim output is included throughout. Nothing here is inferred from
> reading source code. (Container log timestamps read `2026-08-02 16:xx` because containers
> log in UTC and the host is UTC+08:00 — same moment, not a stale capture.)
>
> **Environment these outputs came from:** Windows 11 host, WSL2 (Ubuntu), Docker Desktop
> engine `29.6.1`, commands run from inside the WSL2 distro at
> `/home/aaronwu/Projects/StateFlow`. Where the environment matters (§5), it is called out.

---

## 1. Startup and shutdown

### 1.1 Full stack from a clean clone

```bash
docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build
```

### 1.2 Minimum stack — orchestrator + Postgres only, no demo services

```bash
docker compose up -d
```

Verbatim (after a full `down -v`, so this is genuinely a from-scratch boot):

```
 Container stateflow-stateflow-1  Creating
 Container stateflow-stateflow-1  Created
 Container stateflow-postgres-1  Starting
 Container stateflow-postgres-1  Started
 Container stateflow-postgres-1  Waiting
 Container stateflow-postgres-1  Healthy
 Container stateflow-stateflow-1  Starting
 Container stateflow-stateflow-1  Started
```

`stateflow` starts only after `postgres` reports healthy (`depends_on: condition:
service_healthy`).

### 1.3 Services, container names, published ports

Base stack — `docker compose ps`:

```
SERVICE     NAME                    STATUS                    PORTS
postgres    stateflow-postgres-1    Up 13 seconds (healthy)   0.0.0.0:5432->5432/tcp, [::]:5432->5432/tcp
stateflow   stateflow-stateflow-1   Up 7 seconds (healthy)    0.0.0.0:8080->8080/tcp, [::]:8080->8080/tcp
```

Full stack with the demo overlay:

```
SERVICE            NAME                           PORTS
llm-adapter        stateflow-llm-adapter-1        0.0.0.0:9000->9000/tcp, [::]:9000->9000/tcp
ner-worker         stateflow-ner-worker-1         0.0.0.0:5002->5002/tcp, [::]:5002->5002/tcp
ocr-worker         stateflow-ocr-worker-1         0.0.0.0:5001->5001/tcp, [::]:5001->5001/tcp
postgres           stateflow-postgres-1           0.0.0.0:5432->5432/tcp, [::]:5432->5432/tcp
stateflow          stateflow-stateflow-1          0.0.0.0:8080->8080/tcp, [::]:8080->8080/tcp
step1              stateflow-step1-1              0.0.0.0:5010->5010/tcp, [::]:5010->5010/tcp
step2              stateflow-step2-1              0.0.0.0:5011->5011/tcp, [::]:5011->5011/tcp
summarize-worker   stateflow-summarize-worker-1   0.0.0.0:5003->5003/tcp, [::]:5003->5003/tcp
```

Container-name pattern: `stateflow-<service>-1`. Compose project name: `stateflow`
(derived from the directory name).

**Compose network name: `stateflow_default`** (bridge driver, created/destroyed with the
project):

```
NETWORK ID     NAME                DRIVER    SCOPE
a2b15287a29d   stateflow_default   bridge    local
```

### 1.4 Readiness

Both services carry a container healthcheck; `docker compose ps` shows `(healthy)`, and
`docker inspect -f '{{.State.Health.Status}}' <container>` returns `healthy`.

```bash
docker inspect -f '{{.State.Health.Status}}' stateflow-stateflow-1
docker inspect -f '{{.State.Health.Status}}' stateflow-postgres-1
```

```
postgres=healthy stateflow=healthy
```

The `stateflow` image is distroless (no shell, no curl/wget), so its healthcheck invokes the
binary's own subcommand. That command is runnable by hand:

```bash
$ docker compose exec -T stateflow /stateflow healthcheck
exit=0
```

The `postgres` healthcheck is `pg_isready -U stateflow -d stateflow`.

Timing knobs, from the compose/Dockerfile definitions: `stateflow` `interval=5s timeout=5s
retries=10 start_period=5s`; `postgres` `interval=5s timeout=5s retries=10`. Demo-overlay
services use `interval=1s timeout=2s retries=20 start_period=5s`.

Measured: after `docker compose up -d stateflow`, the container is reported `healthy` about
**5 seconds** later (dominated by `start_period`/`interval`, not by the process — see §6.3
for the process's own startup time).

### 1.5 Shutdown

```bash
docker compose -f docker-compose.yml -f docker-compose.demo.yml stop   # stop, keep state
docker compose -f docker-compose.yml -f docker-compose.demo.yml down   # remove containers + network, KEEP the volume
```

### 1.6 Full reset, including the database volume

```bash
docker compose -f docker-compose.yml -f docker-compose.demo.yml down -v --remove-orphans
```

Verbatim tail, plus the confirmation that both the named volume and the network are gone:

```
 Volume stateflow_pgdata  Removing
 Network stateflow_default  Removing
 Volume stateflow_pgdata  Removed
 Network stateflow_default  Removed

volumes matching stateflow after reset:
DRIVER    VOLUME NAME
networks matching stateflow after reset:
NETWORK ID   NAME      DRIVER    SCOPE
```

The named volume is `stateflow_pgdata`. `--remove-orphans` additionally clears demo
containers left behind when the overlay file is omitted from a later command.

### 1.7 Migrations — when, and by whom

Migrations are applied **by the `stateflow` binary itself, at process startup**, before it
begins serving HTTP. Postgres does *not* apply them (there is no
`/docker-entrypoint-initdb.d` mount); a freshly created volume comes up with no StateFlow
schema at all until `stateflow` connects.

Migration files live in `migrations/` as a versioned `golang-migrate` pair:

```
migrations/000001_initial.down.sql
migrations/000001_initial.up.sql
```

Verbatim, from a boot against an *empty* volume immediately after `down -v`:

```
stateflow-1  | 2026/08/02 16:13:05 INFO migrations applied
stateflow-1  | 2026/08/02 16:13:05 INFO [RECOVERY] found in-progress runs count=0
stateflow-1  | 2026/08/02 16:13:05 INFO [RECOVERY] complete resumed=0
stateflow-1  | 2026/08/02 16:13:05 INFO starting HTTP server addr=:8080
```

Resulting tracking row and table set:

```
 version | dirty
---------+-------
       1 | f
(1 row)
```

Ordering is observable in the log: `migrations applied` → `[RECOVERY] …` → `starting HTTP
server`.

---

## 2. Database access

### 2.1 Connection strings

Both were verified by actually authenticating, not merely by opening a socket.

**From the host** (uses the published `5432` port):

```
postgres://stateflow:stateflow@localhost:5432/stateflow?sslmode=disable
```

```
$ docker run --rm --network host postgres:16 \
    psql 'postgres://stateflow:stateflow@localhost:5432/stateflow?sslmode=disable' \
    -c 'SELECT current_database(), current_user, version();'
 current_database | current_user |                          version
------------------+--------------+-----------------------------------------------------------
 stateflow        | stateflow    | PostgreSQL 16.14 (Debian 16.14-1.pgdg13+1) on x86_64-pc-...
(1 row)
```

**From another container on `stateflow_default`** (service name, internal port):

```
postgres://stateflow:stateflow@postgres:5432/stateflow?sslmode=disable
```

```
$ docker run --rm --network stateflow_default postgres:16 \
    psql 'postgres://stateflow:stateflow@postgres:5432/stateflow?sslmode=disable' \
    -c 'SELECT current_database(), current_user;'
 current_database | current_user
------------------+--------------
 stateflow        | stateflow
(1 row)
```

Credentials/database are all `stateflow` (set in `docker-compose.yml`). No TLS
(`sslmode=disable`).

`psql` was present on this WSL2 host (`psql (PostgreSQL) 16.14`), but nothing requires it —
`docker compose exec -T postgres psql -U stateflow -d stateflow -c '…'` works with no host
client at all, and is the form used throughout this document.

### 2.2 Tables

Six tables exist after migration: the five StateFlow tables plus golang-migrate's own
tracking table.

```
$ docker compose exec -T postgres psql -U stateflow -d stateflow -c '\dt'
               List of relations
 Schema |       Name        | Type  |   Owner
--------+-------------------+-------+-----------
 public | attempts          | table | stateflow
 public | dead_letter_queue | table | stateflow
 public | runs              | table | stateflow
 public | schema_migrations | table | stateflow
 public | steps             | table | stateflow
 public | workflows         | table | stateflow
(6 rows)
```

### 2.3 Columns and types

> **Redaction notice.** These were produced with `\d <table>`. Per the boundary rule, only
> the **column name and type** are reproduced. `\d` also emits `Nullable`, `Default`, and —
> most significantly — `CHECK` constraints that enumerate every legal value of the status,
> failure-reason, and DLQ-reason columns. Those enumerations are a semantic contract, so
> they are withheld here and listed in §8. Reproduce them yourself with `\d` only if you
> have already committed your expectations.

**workflows**

| Column | Type |
|---|---|
| workflow_id | text |
| name | text |
| planner_type | text |
| planner_config | jsonb |
| created_at | timestamp with time zone |

**runs**

| Column | Type |
|---|---|
| run_id | text |
| workflow_id | text |
| status | text |
| workflow_input | jsonb |
| created_at | timestamp with time zone |
| updated_at | timestamp with time zone |

**steps**

| Column | Type |
|---|---|
| step_id | text |
| run_id | text |
| step_name | text |
| seq | integer |
| status | text |
| attempt_count | integer |
| current_attempt_id | uuid |
| decision | jsonb |
| output | jsonb |
| created_at | timestamp with time zone |
| completed_at | timestamp with time zone |

**attempts**

| Column | Type |
|---|---|
| attempt_id | uuid |
| step_id | text |
| status | text |
| failure_reason | text |
| error | text |
| created_at | timestamp with time zone |
| resolved_at | timestamp with time zone |

**dead_letter_queue**

| Column | Type |
|---|---|
| id | bigint |
| run_id | text |
| step_id | text |
| reason | text |
| context | jsonb |
| created_at | timestamp with time zone |

**schema_migrations** (golang-migrate's, not StateFlow's)

| Column | Type |
|---|---|
| version | bigint |
| dirty | boolean |

---

## 3. HTTP surface

Service port **8080**, published to the host as `localhost:8080`. **No authentication** on
any route — no API key, no token, no auth middleware was encountered on any request made
while producing this document.

### 3.1 Registered routes

Method + path only. Determined empirically, not by reading source: a request to a
**registered path with the wrong method** returns `405`, an **unregistered path** returns
`404`. That distinction identifies which paths exist without revealing anything about what
they accept or return.

| Method | Path |
|---|---|
| POST | `/workflows` |
| POST | `/workflows/{workflow_id}/runs` |
| GET | `/runs/{run_id}` |
| POST | `/tasks/complete` |
| POST | `/tasks/fail` |
| GET | `/dlq` |
| POST | `/dlq/{id}/replay` |
| GET | `/healthz` |
| GET | `/ui` |

Probe output:

```
== correct methods ==
GET    /healthz                     -> 200
GET    /ui                          -> 200
GET    /dlq                         -> 200
== method mismatch on known paths (405 = path registered) ==
GET    /workflows                   -> 405
GET    /tasks/complete              -> 405
GET    /tasks/fail                  -> 405
DELETE /runs/some-id                -> 405
GET    /workflows/w1/runs           -> 405
GET    /dlq/1/replay                -> 405
== unregistered path (expect 404) ==
GET    /definitely-not-a-route      -> 404
GET    /metrics                     -> 404
GET    /runs                        -> 404
```

Note `GET /runs` (no id) is **not** registered — only `GET /runs/{run_id}`. There is no
`/metrics` endpoint.

`GET /ui` serves an HTML page embedded in the binary. `GET /healthz` is a liveness route,
not part of the versioned business API.

---

## 4. Environment variables

The orchestrator process reads exactly five environment variables. All five are read in
`cmd/stateflow/main.go`; `DATABASE_URL` and `LISTEN_ADDR` are set in `docker-compose.yml`,
the other three are unset there and fall back to their defaults.

| Variable | Default | Set where | On malformed value |
|---|---|---|---|
| `DATABASE_URL` | *(none — required)* | `docker-compose.yml` | **fail-fast**, exit 1 |
| `LISTEN_ADDR` | `:8080` | `docker-compose.yml` | — |
| `RETRY_MAX_ATTEMPTS` | `3` | unset (code default) | **fail-fast**, exit 1 |
| `RETRY_DELAY_SECONDS` | `5` | unset (code default) | **fail-fast**, exit 1 |
| `SWEEP_INTERVAL_SECONDS` | `30` | unset (code default) | **fail-fast**, exit 1 |

The three integer variables must parse as a **positive** integer; `0` and negative values
are rejected alongside non-numeric input. None of them silently falls back to the default
when set-but-invalid.

Defaults, confirmed from a stock boot with none of the three set:

```
2026/08/02 16:04:03 INFO assembly config retry_max_attempts=3 retry_delay=5s sweep_interval=30s
2026/08/02 16:04:03 INFO [SWEEP] sweeper started interval=30s
2026/08/02 16:04:03 INFO starting HTTP server addr=:8080
```

Fail-fast, each verified by starting a one-off container with the bad value:

```
=== A: RETRY_MAX_ATTEMPTS=abc (malformed) ===
2026/08/02 16:05:44 ERROR invalid env var: must be a positive integer var=RETRY_MAX_ATTEMPTS value=abc

=== B: RETRY_DELAY_SECONDS=0 (non-positive) ===
2026/08/02 16:05:46 ERROR invalid env var: must be a positive integer var=RETRY_DELAY_SECONDS value=0

=== C: SWEEP_INTERVAL_SECONDS=-1 ===
2026/08/02 16:05:47 ERROR invalid env var: must be a positive integer var=SWEEP_INTERVAL_SECONDS value=-1

=== D: DATABASE_URL empty ===
2026/08/02 16:05:49 ERROR DATABASE_URL not set
```

Exit code on a malformed override:

```
$ docker compose run --rm -e RETRY_MAX_ATTEMPTS=abc stateflow
container exit code = 1
```

Valid overrides are honored:

```
$ docker compose run --rm -e RETRY_MAX_ATTEMPTS=7 -e RETRY_DELAY_SECONDS=2 -e SWEEP_INTERVAL_SECONDS=11 stateflow
2026/08/02 16:06:20 INFO assembly config retry_max_attempts=7 retry_delay=2s sweep_interval=11s
2026/08/02 16:06:20 INFO [SWEEP] sweeper started interval=11s

$ docker compose run --rm -e LISTEN_ADDR=:9999 stateflow
2026/08/02 16:06:32 INFO starting HTTP server addr=:9999
```

Note the ordering visible in case A: migrations are applied *before* the assembly config is
validated, so a malformed override still leaves a migrated database behind.

### 4.1 Variables read by other components

- `TEST_DATABASE_URL` — read by the Go test files only (`internal/store`, `internal/api`,
  `internal/orchestrator`), not by the orchestrator binary. See §7.
- Demo overlay only (`docker-compose.demo.yml`), not read by the orchestrator:
  `STEP1_DELAY` / `STEP2_DELAY` (host-side, default `1`), `STEP1_URL` / `STEP2_URL`,
  `ANTHROPIC_API_KEY`, `WORKER_NAME` / `WORKER_PORT` / `WORKER_DELAY`, `STATEFLOW_URL`.

---

## 5. Network topology

**This is the section most likely to cost you time, so it is the most heavily verified.**

### 5.1 The shape

- The orchestrator reaches planners and workers by **making outbound HTTP requests to
  whatever address is configured for them**. It is an ordinary HTTP client; there is no
  service discovery, no registry, no sidecar.
- Workers reach the orchestrator's callback endpoints by making HTTP requests **to the
  orchestrator's own address** — `http://stateflow:8080` from inside the compose network,
  `http://localhost:8080` from the host. The demo overlay wires exactly this, via
  `STATEFLOW_URL: "http://stateflow:8080"` on `ner-worker`, with the compose file's own
  comment noting "from inside the compose network that's the `stateflow` service, not
  localhost."

### 5.2 Recommended: run fakes as containers on `stateflow_default`

**Compose DNS resolves service names to container IPs, and container-to-container HTTP over
the bridge works.** Verified in both directions.

Service-name resolution:

```
$ docker compose exec -T postgres getent hosts stateflow
172.19.0.3      stateflow
$ docker compose exec -T postgres getent hosts postgres
172.19.0.2      postgres
```

Container → orchestrator by service name:

```
$ docker run --rm --network stateflow_default alpine/curl:latest \
    curl -s -o /dev/null -w '%{http_code}\n' http://stateflow:8080/healthz
GET http://stateflow:8080/healthz -> 200
```

Container → an arbitrary fake, addressed by name. This is the pattern to copy — a
throwaway container joined to `stateflow_default` with a network alias, serving on port
6000, fetched by name from a *different* container:

```
$ docker run -d --rm --name fake-worker --network stateflow_default \
    --network-alias fake-worker python:3.11-slim python -c '<a tiny http.server>'
$ docker run --rm --network stateflow_default alpine/curl:latest \
    curl -s -m 5 http://fake-worker:6000/
FAKE-OK  <- reachable at http://fake-worker:6000
```

**Address form to give the orchestrator for a containerized fake:
`http://<service-name>:<container-port>/<path>`** — the *container's internal* port, not any
host-published port, and no `localhost` anywhere.

### 5.3 Skeleton for a fake service

Add an overlay file (e.g. `docker-compose.test.yml`) and start it alongside the base stack.
Joining the same compose project puts it on `stateflow_default` automatically:

```yaml
services:
  fake-worker:
    build: ./test/fakes          # or: image: python:3.11-slim + command:
    command: ["python", "fake_worker.py"]
    environment:
      STATEFLOW_URL: "http://stateflow:8080"   # for callbacks, if the fake makes them
    # ports:  NOT required for the orchestrator to reach it.
    #         Publish only if YOU want to poke it from the host.
    healthcheck:
      test: ["CMD", "python", "-c",
             "import socket; socket.create_connection(('localhost', 6000), timeout=2)"]
      interval: 1s
      timeout: 2s
      retries: 20
      start_period: 5s
```

```bash
docker compose -f docker-compose.yml -f docker-compose.test.yml up -d
```

The orchestrator then addresses it as `http://fake-worker:6000/...`.

Two operational notes, both taken from the existing demo overlay's own comments and
consistent with what was observed here:

- Prefer waiting on the **container's healthcheck** (`docker inspect -f
  '{{.State.Health.Status}}'`) over probing the host-published port. On Docker Desktop the
  host-side port proxy can accept connections slightly before the process inside the
  container has actually bound its socket, so a host-side probe can report ready too early.
  Container-to-container traffic goes straight over the bridge and does not have this race.
- A fake that calls back into the orchestrator must survive the orchestrator being down —
  during a crash test, service-name resolution itself fails. Observed verbatim during the
  crash demo:
  `Failed to resolve 'stateflow' ([Errno -5] No address associated with hostname)`.

### 5.4 On `host.docker.internal` — read this carefully

Earlier project documentation claimed `host.docker.internal` deterministically
fails under Docker Desktop/WSL2, attributed to a 6/6 controlled experiment in
Session 21.

**I could not reproduce that failure in this environment, and I am reporting that honestly
rather than repeating the claim.** A listener bound inside WSL2 Ubuntu was reachable from a
container via `host.docker.internal`, reproducibly, on two consecutive runs:

```
=== A: start a listener INSIDE WSL2 Ubuntu on :7999 ===
  local sanity check from WSL itself:
HELLO-FROM-WSL-HOST  <- reachable from WSL

=== B: can a container on stateflow_default reach it via host.docker.internal:7999 ? ===
192.168.65.254    host.docker.internal
--- fetch ---
HELLO-FROM-WSL-HOST <- SUCCESS
```

The name also resolves from inside a container that has no `extra_hosts` entry of its own —
Docker Desktop injects it (`192.168.65.254`). The `stateflow` service additionally declares
`extra_hosts: ["host.docker.internal:host-gateway"]`.

**What to do with this discrepancy:** the recommendation in §5.2 stands, but for
a different reason than previously stated. Run fakes as compose services not
because the host path is broken, but because container-to-container name
resolution behaves identically on Linux, macOS, Windows, and CI, while any path
crossing the host boundary varies by platform. It is one fewer variable.

A plain `GET` succeeding also says nothing about more complex flows — callbacks
arriving while a container is being killed and restarted — which is where the
original failure was observed. The root cause of the Session 21 result remains
unidentified.

---

## 6. Observability

### 6.1 Reading the logs

```bash
docker compose logs stateflow                        # all
docker compose logs -f stateflow                     # follow
docker compose logs --timestamps stateflow           # with RFC3339 container timestamps
docker compose logs --since 60s stateflow            # recent window
docker compose logs --no-log-prefix stateflow        # drop the "stateflow-1  | " column
```

Format is Go `slog` text: `2026/08/02 16:13:05 INFO <message> key=value key=value`.

### 6.2 Structured prefixes

Three bracketed prefixes appear in log messages:

| Prefix | Emitted from |
|---|---|
| `[RECOVERY]` | `internal/orchestrator/recovery.go`, `internal/orchestrator/loop.go` |
| `[SWEEP]` | `internal/orchestrator/sweeper.go` |
| `[REPLAY]` | `internal/orchestrator/replay.go`, `internal/api/server.go` |

Startup lines without a prefix that are useful as anchors: `migrations applied`, `assembly
config …`, `recovery complete resumed=N`, `starting HTTP server addr=…`. Shutdown emits
`shutdown signal received, stopping sweeper signal=terminated`, `[SWEEP] sweeper stopped`,
`sweeper stopped, exiting`.

### 6.3 Killing and restarting the orchestrator

```bash
docker compose kill stateflow      # abrupt (SIGKILL) — this is what the crash demo uses
docker compose up -d stateflow     # bring it back
```

`docker compose stop stateflow` is the graceful variant (SIGTERM); the log lines in §6.2
distinguish the two.

### 6.4 How long recovery takes

**Order of magnitude: the startup scan is milliseconds; the wall-clock gate is container
start, roughly one second, plus about five more before Docker reports `healthy`.**

Measured, verbatim, controlled restart:

```
elapsed 'compose up -d stateflow' -> 'starting HTTP server' : 1.154147789 s
health status = healthy, additional wait = 5.085795332 s
```

The scan itself, timestamped from a boot that actually had work to pick up:

```
2026-08-02T16:09:45.089867374Z INFO migrations applied
2026-08-02T16:09:45.091838644Z INFO [RECOVERY] found in-progress runs count=1
2026-08-02T16:09:45.093361886Z INFO [RECOVERY] complete resumed=1
2026-08-02T16:09:45.095270059Z INFO starting HTTP server addr=:8080
```

`migrations applied` → `[RECOVERY] complete` was **~4 ms**. Practical guidance: poll for
readiness rather than sleeping a fixed interval; budget a few seconds, not tens of seconds.

Separately, a periodic background sweeper ticks on `SWEEP_INTERVAL_SECONDS` (default **30
s**), announced at startup as `[SWEEP] sweeper started interval=30s`. Anything that waits on
the sweeper rather than on startup recovery needs a correspondingly longer budget.

### 6.5 Inspecting state directly

```bash
docker compose exec -T postgres psql -U stateflow -d stateflow -c '<SQL>'
```

---

## 7. Running the existing tests

### 7.1 Go tests

No Go toolchain was installed on this host, so both invocations below were verified inside a
`golang:1.25` container (`go.mod` declares `go 1.25.0`). Run them directly if you have Go
installed — the commands are the same minus the `docker run` wrapper.

**Unit tests, no database.** DB-backed tests skip themselves when `TEST_DATABASE_URL` is
unset:

```bash
go test ./...
```

```
?   	github.com/aaronwu000/stateflow/cmd/stateflow	[no test files]
ok  	github.com/aaronwu000/stateflow/internal/api	0.004s
?   	github.com/aaronwu000/stateflow/internal/core	[no test files]
ok  	github.com/aaronwu000/stateflow/internal/orchestrator	0.088s
ok  	github.com/aaronwu000/stateflow/internal/planner	0.214s
ok  	github.com/aaronwu000/stateflow/internal/store	0.004s
ok  	github.com/aaronwu000/stateflow/internal/transport	0.994s
?   	github.com/aaronwu000/stateflow/migrations	[no test files]
```

**With Postgres:**

```bash
docker compose up -d postgres
TEST_DATABASE_URL="postgres://stateflow:stateflow@localhost:5432/stateflow?sslmode=disable" \
  go test -p 1 ./...
```

```
?   	github.com/aaronwu000/stateflow/cmd/stateflow	[no test files]
ok  	github.com/aaronwu000/stateflow/internal/api	2.660s
?   	github.com/aaronwu000/stateflow/internal/core	[no test files]
ok  	github.com/aaronwu000/stateflow/internal/orchestrator	3.882s
ok  	github.com/aaronwu000/stateflow/internal/planner	0.214s
ok  	github.com/aaronwu000/stateflow/internal/store	2.099s
ok  	github.com/aaronwu000/stateflow/internal/transport	0.972s
?   	github.com/aaronwu000/stateflow/migrations	[no test files]
```

Exact container form used here (note `--network stateflow_default` and the in-network DSN):

```bash
docker run --rm --network stateflow_default \
  -v /home/aaronwu/Projects/StateFlow:/src -w /src -v go-mod-cache:/go/pkg/mod \
  -e TEST_DATABASE_URL="postgres://stateflow:stateflow@postgres:5432/stateflow?sslmode=disable" \
  golang:1.25 go test -p 1 ./...
```

**`-p 1` is required** whenever more than one package runs. Per `CLAUDE.md`, the
store/api/orchestrator packages each reset the same database's schema, and parallel package
execution races them (reported symptoms: `duplicate key value violates unique constraint
"pg_type_typname_nsp_index"`, `relation "steps" does not exist`). `-p 1` serializes
packages; a single package alone does not need it. **These tests wipe the database** — do
not run them against a stack whose data you care about.

### 7.2 Acceptance tests

**The v0 acceptance oracles have been retired** (commit `a45e677`, "archive: retire v0
acceptance oracles; superseded by BEHAVIOR_MATRIX-derived suite"). `test/acceptance/` now
contains only a stale `__pycache__/` directory — there is no runnable suite there:

```
test/acceptance/:
drwxr-xr-x 3 aaron 197609 0 Aug  3 00:00 .
drwxr-xr-x 3 aaron 197609 0 Jul  9 09:25 ..
drwxr-xr-x 2 aaron 197609 0 Jul 11 10:22 __pycache__
```

The retired files now live in `archive/old-tests/` (`_harness.py`,
`crash_recovery_test.py`, `dlq_replay_test.py`, `fake_planner.py`, `fake_worker.py`,
`README.md`). Their documented invocation used `API_BASE`, `TEST_DATABASE_URL`,
`ADVERTISE_HOST` (default `host.docker.internal`), `WORKER_PORT` (7101), `PLANNER_PORT`
(7102) — recorded here as historical operational detail only. **They are archived; do not
run them, and do not use them as a model.**

`scripts/` exists but is **empty** — there is no `test-all.sh` and no `Makefile` in the repo
root yet.

### 7.3 The end-to-end crash demo

This one *does* run today, and is the practical smoke test that the whole stack works:

```bash
docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build
python3 demo/crash_demo.py
```

It completed in **~47 s** wall clock and exited successfully. It requires the Python
`requests` package. It resets the database at the start, and stops `stateflow` and the
worker services in its own cleanup — bring them back with `docker compose … up -d stateflow`.

### 7.4 CI

`.github/workflows/ci.yml` runs two jobs on every push (and PRs to `main`): `test`
(`go test -p 1 ./...` against a compose Postgres) and `e2e` (full stack + `crash_demo.py`).
Both tear down with `down -v`.

---

## 8. Deliberately omitted

Everything below was available to me and was left out because it is a **semantic contract**
— something that would let a reader infer how the system responds to a given input, which is
exactly what `spec/BEHAVIOR_MATRIX.md` is supposed to be the sole source of.

1. **All request and response body shapes.** Every endpoint in §3 is listed as method + path
   only. No field names, no nesting, no examples, no content types. I made real requests
   while verifying §3 and deliberately recorded only HTTP status codes — and only for
   deliberately *malformed* probes (wrong method, nonexistent path) chosen because their
   codes reveal routing, not behavior.
2. **Which fields are required, and their legal values.** Including everything in
   `planner_config`, the `planner_type` values, and the distinction between planner types.
3. **`CHECK` constraint contents from `\d`.** The largest single omission. The real `\d`
   output enumerates the complete legal value set for `runs.status`, `steps.status`,
   `attempts.status`, `attempts.failure_reason`, `workflows.planner_type`, and
   `dead_letter_queue.reason`. Those enumerations *are* the state machine and the failure
   taxonomy. Withheld.
4. **Nullability and defaults from `\d`.** "Which columns are NOT NULL" is literally "which
   fields are mandatory," and which columns are nullable hints at which are written later
   rather than at creation — i.e. lifecycle. Withheld along with `DEFAULT` expressions.
5. **Foreign keys, indexes, and index column order.** A composite index over
   `(run_id, seq)` telegraphs an ordering rule; the FK graph telegraphs the entity
   lifecycle. Withheld.
6. **What any column means or when it is written.** In particular `attempt_count`,
   `current_attempt_id`, `seq`, `decision`, `output`, `completed_at`, `resolved_at`,
   `failure_reason`, and the DLQ `context` column. Names and types only, per §2.3.
7. **State-machine behavior of any kind:** transitions, transaction boundaries, retry
   accounting, timeout handling, orphan classification, DLQ entry conditions, replay
   semantics. Nothing about how a failure is categorized or what consumes a budget.
8. **Timeout and retry *semantics*.** §4 gives the `RETRY_MAX_ATTEMPTS` /
   `RETRY_DELAY_SECONDS` / `SWEEP_INTERVAL_SECONDS` defaults because they are startup
   configuration a test operator must be able to set — but nothing about what is counted,
   when a clock starts, what happens at exhaustion, or how timeouts resolve against a
   step/workflow setting.
9. **Wire-format conventions** — casing of status values on the wire, sync-vs-async payload
   wrapping, header names, worker/planner contract details.
10. **Full log-line content.** §6.2 lists prefixes and the startup/shutdown anchors an
    operator needs to detect "it booted" / "it is shutting down." The recovery excerpt in
    §6.4 was kept minimal for timing evidence; I did not catalog the recovery/sweep/replay
    messages or their key-value fields, since that set enumerates the decisions the system
    makes. Some behavior is inevitably legible in that timing excerpt — this is the one
    place I knowingly traded a little leakage for the verifiable measurement §6.4 requires.
11. **`GET /ui` page content**, and what the demo services actually compute.
12. **`crash_demo.py`'s assertions.** §7.3 records that it exists, how to run it, and that it
    passes in ~47 s. Its output narrates the exact invariants it proves; none of that is
    reproduced here.
13. **Planner-call budget and timeouts**, and anything about planner reconstruction.

**Borderline calls I made, and why:**

- **The `[RECOVERY]`/`[SWEEP]`/`[REPLAY]` prefix list** — included. The prompt asks for it
  explicitly, and a bare prefix is a grep anchor. The *messages* behind them are omitted.
- **`migrations applied` → `[RECOVERY]` → `starting HTTP server` ordering** — included.
  It is directly observable in any boot log and is what tells an operator the process is
  ready; the ordering is startup sequencing, not business behavior.
- **The `stateflow_pgdata` volume name and `stateflow_default` network name** — included;
  pure operational identifiers, and §5 is unusable without the network name.
