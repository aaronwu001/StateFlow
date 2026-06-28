// Command stateflow is the StateFlow server entry point.
// It wires all components, runs crash recovery, then starts the HTTP server.
//
// Startup order (DESIGN.md §9.3):
//  1. Open Postgres connection.
//  2. Run RecoverRuns — resumes any RUNNING runs from before a crash.
//  3. Start HTTP server — begins accepting new runs.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/aaronwu000/stateflow/internal/api"
	"github.com/aaronwu000/stateflow/internal/core"
	"github.com/aaronwu000/stateflow/internal/orchestrator"
	"github.com/aaronwu000/stateflow/internal/planner"
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
	asyncT := transport.NewAsyncTransport()
	routedT := &transport.MultiTransport{Sync: syncT, Async: asyncT}
	retry := orchestrator.DefaultRetryPolicy()

	// buildLoop constructs a fully configured Loop for the given run.
	buildLoop := func(runID core.RunID, workflowInput json.RawMessage, plannerType string, plannerConfig json.RawMessage) (*orchestrator.Loop, error) {
		p, err := buildPlanner(plannerType, plannerConfig)
		if err != nil {
			return nil, err
		}
		return &orchestrator.Loop{
			RunID:         runID,
			WorkflowInput: workflowInput,
			Store:         s,
			Planner:       p,
			Transport:     routedT,
			Retry:         retry,
		}, nil
	}

	// startLoop is injected into the API server. It builds the loop and starts
	// a goroutine. Errors (e.g., unknown planner_type) are logged but not fatal
	// to the server; the run will stay RUNNING until recovery or operator action.
	startLoop := func(loopCtx context.Context, runID core.RunID, workflowInput json.RawMessage, plannerType string, plannerConfig json.RawMessage) {
		l, err := buildLoop(runID, workflowInput, plannerType, plannerConfig)
		if err != nil {
			slog.Error("startLoop: build loop", "run_id", string(runID), "err", err)
			return
		}
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
	n, err := orchestrator.RecoverRuns(ctx, db, func(runID core.RunID, workflowInput json.RawMessage) *orchestrator.Loop {
		// Look up the workflow config for this run.
		var plannerType string
		var plannerConfig json.RawMessage
		if err := db.QueryRowContext(ctx, `
			SELECT w.planner_type, w.planner_config
			FROM runs r JOIN workflows w ON r.workflow_id = w.workflow_id
			WHERE r.run_id = $1
		`, string(runID)).Scan(&plannerType, &plannerConfig); err != nil {
			slog.Error("recovery: get workflow", "run_id", string(runID), "err", err)
			return nil
		}
		l, err := buildLoop(runID, workflowInput, plannerType, plannerConfig)
		if err != nil {
			slog.Error("recovery: build loop", "run_id", string(runID), "err", err)
			return nil
		}
		return l
	})
	if err != nil {
		slog.Error("recovery failed", "err", err)
		os.Exit(1)
	}
	slog.Info("recovery complete", "resumed", n)

	// Step 2: Start HTTP server.
	srv := api.New(db, asyncT, ctx, startLoop)

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

// buildPlanner instantiates the correct NextStepPlanner implementation.
// Only "static" is supported in MVP; "http" is deferred to Session 10.
func buildPlanner(plannerType string, plannerConfig json.RawMessage) (core.NextStepPlanner, error) {
	switch plannerType {
	case "static":
		// NewStaticPlanner accepts YAML; yaml.v3 also parses JSON, so the JSONB
		// bytes from Postgres work directly.
		return planner.NewStaticPlanner(plannerConfig)
	default:
		return nil, fmt.Errorf("unsupported planner_type %q (http planner is Session 10)", plannerType)
	}
}
