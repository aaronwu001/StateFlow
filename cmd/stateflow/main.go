// Command stateflow is the StateFlow server entry point.
// It wires all components, runs crash recovery, then starts the HTTP server.
//
// Startup order (whitepaper §5, §8.3):
//  1. Open Postgres connection.
//  2. Run RecoverRuns — resumes any RUNNING runs from before a crash.
//  3. Start HTTP server — begins accepting new runs.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/aaronwu000/stateflow/internal/api"
	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/orchestrator"
	"github.com/aaronwu000/stateflow/internal/store"
	"github.com/aaronwu000/stateflow/internal/transport"
)

func main() {
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

	s := store.New(db)
	syncT := transport.NewSyncTransport()
	asyncT := transport.NewAsyncTransport(s)
	routedT := &transport.MultiTransport{Sync: syncT, Async: asyncT}
	retry := orchestrator.DefaultRetryPolicy()

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
