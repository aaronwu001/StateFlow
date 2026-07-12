package migrations

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver Apply opens below
)

// Apply runs every pending "up" migration against databaseURL, in version
// order, via golang-migrate's Go library. It is idempotent: if the schema
// is already at the latest migration version, it returns nil
// (migrate.ErrNoChange is not treated as an error).
//
// Called from two places, deliberately sharing this exact function so they
// can never drift: cmd/stateflow/main.go at server startup (before
// RecoverRuns), and each Postgres-backed integration test package's
// resetSchema/resetTestSchema helper (internal/store, internal/api,
// internal/orchestrator) after dropping the prior test run's tables.
//
// Apply opens and closes its OWN short-lived *sql.DB connection for the
// duration of the migration run — it deliberately does NOT accept an
// already-open *sql.DB from the caller. golang-migrate's WithInstance-based
// database driver checks a dedicated connection out of whatever pool it is
// given, and that driver's Close() method closes the ENTIRE pool it was
// built from, not just the one connection it checked out (verified against
// the library source: database/pgx/v5.Postgres.Close() and
// database/postgres.Postgres.Close() both call db.Close() unconditionally).
// Sharing the app's long-lived pool here would force a choice between two
// bad outcomes: closing the app's db handle out from under it once Apply
// returns, or leaking one permanently-checked-out connection by skipping
// Close(). A private, fully-owned connection sidesteps both: Close() here
// is always safe because nothing else ever touches this *sql.DB.
func Apply(databaseURL string) error {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("migrations: open db: %w", err)
	}
	defer db.Close()

	srcDriver, err := iofs.New(fs, ".")
	if err != nil {
		return fmt.Errorf("migrations: build source driver: %w", err)
	}

	dbDriver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		return fmt.Errorf("migrations: build database driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", srcDriver, "pgx5", dbDriver)
	if err != nil {
		return fmt.Errorf("migrations: init migrate instance: %w", err)
	}
	defer m.Close() // safe: only closes the private db opened above (see doc comment)

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrations: apply: %w", err)
	}
	return nil
}
