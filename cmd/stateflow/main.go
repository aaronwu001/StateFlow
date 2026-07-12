// Command stateflow is the StateFlow server entry point.
// It wires all components, runs crash recovery, then starts the HTTP server.
//
// Startup order (whitepaper §5, §8.3):
//  1. Open Postgres connection.
//  2. Apply pending schema migrations (migrations package, golang-migrate's
//     Go library — Temporary Design Registry item #7, whitepaper §18).
//  3. Run RecoverRuns — resumes any RUNNING runs from before a crash.
//  4. Start HTTP server — begins accepting new runs.
//
// Subcommands: running the binary with no arguments is the default
// server-start behavior above (unchanged, so ENTRYPOINT ["/stateflow"]'s
// ambient behavior in Dockerfile/docker-compose.yml keeps working). Running
// it as `stateflow healthcheck` instead makes a single HTTP GET against this
// same process's own GET /healthz and exits 0/non-zero — see runHealthcheck.
// This exists so a distroless image (no shell, no curl/wget) can still back
// a Dockerfile HEALTHCHECK / docker-compose healthcheck: block by invoking
// the binary itself (Temporary Design Registry item #8, whitepaper §18).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/aaronwu000/stateflow/internal/api"
	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/orchestrator"
	"github.com/aaronwu000/stateflow/internal/store"
	"github.com/aaronwu000/stateflow/internal/transport"
	"github.com/aaronwu000/stateflow/migrations"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		runHealthcheck()
		return
	}
	runServer()
}

// runHealthcheck is the `stateflow healthcheck` subcommand. It HTTP-GETs
// this same process's GET /healthz (internal/api/server.go) on the
// LISTEN_ADDR this process is configured for (same env var runServer reads,
// same ":8080" default) and exits 0 on HTTP 200, non-zero otherwise —
// exactly what a shell-less HEALTHCHECK/healthcheck: probe needs.
func runHealthcheck() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(healthzURL(addr))
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: request failed:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: unhealthy (status %d)\n", resp.StatusCode)
		os.Exit(1)
	}
}

// healthzURL turns a LISTEN_ADDR-shaped value (typically ":8080", i.e. no
// host — bind-all) into a loopback URL this same process can reach itself
// on. An empty host in the listen address means "localhost" for the purpose
// of self-probing.
func healthzURL(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		// Not a valid host:port pair (shouldn't happen since the server
		// binds this exact string); fall back to the documented default.
		return "http://localhost:8080/healthz"
	}
	if host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s:%s/healthz", host, port)
}

// ── Config-driven assembly (registry #6, whitepaper §18) ───────────────────
//
// Owner's binding standing principle (Phase 2 "Design decisions log"):
// "configurable but come with a widely acceptable default setting" — every
// env var below defaults to EXACTLY the value that was hardcoded before this
// session, so a fresh clone with zero new env vars set is byte-for-byte
// behaviorally identical to pre-session. Only genuinely-tunable-today
// parameters got a knob here: the retry policy's MaxRetries/Delay
// (orchestrator.FixedCountPolicy, previously always orchestrator.
// DefaultRetryPolicy()) and the orphan sweeper's interval (previously always
// the unexported orchestrator.defaultSweepInterval, 30s). LISTEN_ADDR and
// DATABASE_URL were already env-configurable before this session and are
// untouched. Nothing else in this file's wiring has a genuine second option
// today (there is exactly one StateStore implementation, one transport
// router shape, etc.) — no plugin-selection mechanism was added for those.
const (
	envRetryMaxAttempts    = "RETRY_MAX_ATTEMPTS"    // orchestrator.FixedCountPolicy.MaxRetries; default below matches DefaultRetryPolicy()
	envRetryDelaySeconds   = "RETRY_DELAY_SECONDS"    // orchestrator.FixedCountPolicy.Delay; default below matches DefaultRetryPolicy()
	envSweepIntervalSecs   = "SWEEP_INTERVAL_SECONDS" // orphan sweeper tick interval; default below matches sweeper.go's defaultSweepInterval
	defaultRetryMaxAttempt = 3                        // == orchestrator.DefaultRetryPolicy().MaxRetries
	defaultRetryDelaySecs  = 5                         // == orchestrator.DefaultRetryPolicy().Delay (5s)
	defaultSweepIntervalS  = 30                        // == sweeper.go's defaultSweepInterval (30s)
)

// assemblyConfig holds the handful of env-var-overridable wiring parameters
// this session made configurable. Loaded once, at startup, before any
// component is constructed — never read from ad hoc os.Getenv calls
// scattered through runServer, so the whole surface of what's configurable
// is visible in one place.
type assemblyConfig struct {
	RetryMaxAttempts int
	RetryDelay       time.Duration
	SweepInterval    time.Duration
}

// loadAssemblyConfig reads the registry-#6 env vars, applying the "widely
// acceptable default" principle: unset ⇒ exactly today's old hardcoded
// value; set but not a positive integer ⇒ fail fast at startup (the same
// posture runServer already takes for a missing/invalid DATABASE_URL) rather
// than silently falling back, so a typo'd override is loud, not silently
// ignored.
func loadAssemblyConfig() assemblyConfig {
	return assemblyConfig{
		RetryMaxAttempts: envPositiveInt(envRetryMaxAttempts, defaultRetryMaxAttempt),
		RetryDelay:       time.Duration(envPositiveInt(envRetryDelaySeconds, defaultRetryDelaySecs)) * time.Second,
		SweepInterval:    time.Duration(envPositiveInt(envSweepIntervalSecs, defaultSweepIntervalS)) * time.Second,
	}
}

// envPositiveInt reads env var name as a positive integer, returning def if
// the variable is unset. It exits the process (fail fast, matching this
// file's existing DATABASE_URL posture) if the variable is set to something
// that isn't a positive integer — a malformed override should be loud at
// startup, not silently reverted to the default.
func envPositiveInt(name string, def int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		slog.Error("invalid env var: must be a positive integer", "var", name, "value", raw)
		os.Exit(1)
	}
	return v
}

func runServer() {
	ctx := context.Background()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		slog.Error("DATABASE_URL not set")
		os.Exit(1)
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		slog.Error("open db", "err", err)
		os.Exit(1)
	}
	if err := db.PingContext(ctx); err != nil {
		slog.Error("ping db", "err", err)
		os.Exit(1)
	}

	// Step 0: apply pending schema migrations via golang-migrate's Go
	// library, before anything reads/writes application tables. This
	// replaces the previous mechanism (Postgres's init-script hook applying
	// migrations/001_initial.sql on first boot only, via
	// docker-compose.yml) — that mechanism could not apply a second
	// migration to an existing volume and required a full down-v reset for
	// every schema change (registry item #7, whitepaper §18).
	// migrations.Apply opens its own short-lived connection from dbURL; it
	// deliberately does not share this process's db handle (see the
	// migrations package doc comment for why).
	if err := migrations.Apply(dbURL); err != nil {
		slog.Error("apply migrations", "err", err)
		os.Exit(1)
	}
	slog.Info("migrations applied")

	cfg := loadAssemblyConfig()
	slog.Info("assembly config",
		"retry_max_attempts", cfg.RetryMaxAttempts,
		"retry_delay", cfg.RetryDelay.String(),
		"sweep_interval", cfg.SweepInterval.String(),
	)

	s := store.New(db)
	syncT := transport.NewSyncTransport()
	asyncT := transport.NewAsyncTransport(s)
	routedT := &transport.MultiTransport{Sync: syncT, Async: asyncT}
	// Was orchestrator.DefaultRetryPolicy() (hardcoded MaxRetries: 3, Delay:
	// 5s) before this session; cfg's fields default to those exact values
	// when RETRY_MAX_ATTEMPTS/RETRY_DELAY_SECONDS are unset (registry #6).
	retry := &orchestrator.FixedCountPolicy{
		MaxRetries: cfg.RetryMaxAttempts,
		Delay:      cfg.RetryDelay,
	}

	// buildLoop constructs a Loop for the given run. It cannot fail: unlike
	// the pre-Session-5 shape, the planner is NOT built here — Loop.Run
	// reconstructs it lazily from the workflow row on every call (whitepaper
	// §12.1), so buildLoop itself is pure wiring. Its signature matches
	// orchestrator.RecoverRuns's makeLoop parameter exactly, so it is passed
	// to RecoverRuns directly below.
	buildLoop := func(runID core.RunID, workflowID core.WorkflowID, workflowInput json.RawMessage) *orchestrator.Loop {
		return &orchestrator.Loop{
			RunID:         runID,
			WorkflowID:    workflowID,
			WorkflowInput: workflowInput,
			Store:         s,
			Transport:     routedT,
			Retry:         retry,
		}
	}

	// startLoop is injected into the API server. It builds the loop and
	// starts a goroutine for it. The server calls this both for a
	// newly-started run and for a DLQ replay (Session 6).
	startLoop := func(loopCtx context.Context, runID core.RunID, workflowID core.WorkflowID, workflowInput json.RawMessage) {
		l := buildLoop(runID, workflowID, workflowInput)
		go func() {
			slog.Info("loop: starting", "run_id", string(runID))
			if err := l.Run(loopCtx); err != nil {
				slog.Error("loop: run ended with error", "run_id", string(runID), "err", err)
			} else {
				slog.Info("loop: run completed", "run_id", string(runID))
			}
		}()
	}

	// Step 1: Crash recovery — resume RUNNING runs before accepting new requests.
	n, err := orchestrator.RecoverRuns(ctx, s, buildLoop)
	if err != nil {
		slog.Error("recovery failed", "err", err)
		os.Exit(1)
	}
	slog.Info("recovery complete", "resumed", n)

	// Step 1.5: start the periodic orphan sweeper (Temporary Design Registry
	// item #4, whitepaper §18). RecoverRuns above only runs once, before this
	// point, and only handles a full orchestrator-process restart. The
	// sweeper covers the different, longer-lived case: a single run's
	// driving goroutine dying while this process stays alive (most commonly
	// a transient Postgres outage) — without it, nothing would ever
	// relaunch that goroutine short of a manual restart. Uses the same
	// buildLoop factory as RecoverRuns, reusing its exact scan/claim logic
	// (internal/orchestrator/sweeper.go). Interval was the hardcoded
	// defaultSweepInterval (30s) before this session; cfg.SweepInterval
	// defaults to that exact value when SWEEP_INTERVAL_SECONDS is unset
	// (registry #6).
	stopSweeper := orchestrator.StartSweeperWithInterval(ctx, s, buildLoop, cfg.SweepInterval)

	// Clean shutdown path for the sweeper's ticker goroutine: on SIGINT/
	// SIGTERM (docker compose stop/down sends SIGTERM), stop the sweeper
	// (its stop() blocks until its goroutine has actually exited — no
	// abandoned in-flight tick) before the process exits, rather than
	// leaving it to be torn down mid-tick along with everything else.
	// Deliberately scoped to just the sweeper: broader graceful shutdown
	// (in-flight HTTP requests, in-flight run-driving goroutines) is not
	// part of this session — see the Session 18 report.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("shutdown signal received, stopping sweeper", "signal", sig.String())
		stopSweeper()
		slog.Info("sweeper stopped, exiting")
		os.Exit(0)
	}()

	// Step 2: Start HTTP server.
	srv := api.New(db, s, asyncT, ctx, startLoop)

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	slog.Info("starting HTTP server", "addr", addr)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		slog.Error("HTTP server", "err", err)
		os.Exit(1)
	}
}
