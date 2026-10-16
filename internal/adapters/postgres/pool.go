// Package postgres holds the pgx-based persistence adapter: connection pool,
// schema version check and migration runner. Repositories and the TxRunner
// arrive in later slices.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool builds a pool without connecting; call Ping to verify it.
func NewPool(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// The DSN may contain a password; do not echo it.
		return nil, errors.New("postgres: invalid DATABASE_URL")
	}
	cfg.MaxConns = maxConns
	cfg.ConnConfig.RuntimeParams["application_name"] = "wager-wallet"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}
	return pool, nil
}

// Ping verifies a connection can be acquired and used.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: ping: %w", err)
	}
	return nil
}

// Close closes the pool, waiting at most until ctx expires for acquired
// connections to be released. pgxpool.Close itself has no deadline, so it
// runs in a goroutine; on timeout that goroutine finishes once the last
// connection is returned.
func Close(ctx context.Context, pool *pgxpool.Pool) error {
	done := make(chan struct{})
	go func() {
		pool.Close()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("postgres: close pool: %w", ctx.Err())
	}
}

// Schema version errors returned by CheckSchemaVersion.
var (
	ErrSchemaNotMigrated = errors.New("postgres: schema is not migrated")
	ErrSchemaDirty       = errors.New("postgres: schema migration is dirty")
	ErrSchemaTooOld      = errors.New("postgres: schema version is older than this build expects")
)

// CheckSchemaVersion reads golang-migrate's schema_migrations table and
// fails unless the database is clean and at least at version want. A newer
// version is accepted (expand/contract migrations keep older builds
// working during a rolling deploy); the actual version is returned so the
// caller can log it.
func CheckSchemaVersion(ctx context.Context, pool *pgxpool.Pool, want uint) (uint, error) {
	var (
		version int64
		dirty   bool
	)
	err := pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrSchemaNotMigrated
	case errors.As(err, &pgErr) && pgErr.Code == "42P01": // undefined_table
		return 0, ErrSchemaNotMigrated
	case err != nil:
		return 0, fmt.Errorf("postgres: read schema version: %w", err)
	}
	if version < 0 {
		return 0, ErrSchemaNotMigrated
	}
	got := uint(version)
	if dirty {
		return got, fmt.Errorf("%w (version %d)", ErrSchemaDirty, got)
	}
	if got < want {
		return got, fmt.Errorf("%w (have %d, want >= %d)", ErrSchemaTooOld, got, want)
	}
	return got, nil
}

// HealthCheck returns a readiness probe that pings the pool.
func HealthCheck(pool *pgxpool.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		return Ping(ctx, pool)
	}
}
