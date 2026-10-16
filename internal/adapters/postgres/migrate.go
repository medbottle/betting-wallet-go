package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	// Registers the pgx5:// database driver.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/medbottle/wager-wallet-service/migrations"
)

// ErrNoVersion means no migration has ever been applied.
var ErrNoVersion = errors.New("postgres: no migration applied")

// Migrator applies the embedded migrations. It must connect as the schema
// owner role (wallet_owner), never as the application role.
type Migrator struct {
	m *migrate.Migrate
}

// NewMigrator opens a migrator against databaseURL (postgres:// or
// postgresql:// form).
func NewMigrator(databaseURL string) (*Migrator, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: open embedded source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, toPgx5URL(databaseURL))
	if err != nil {
		// golang-migrate errors can echo the URL; keep the password out.
		return nil, fmt.Errorf("migrate: connect: %s", redact(err.Error(), databaseURL))
	}
	return &Migrator{m: m}, nil
}

// Up applies all pending migrations. It is a no-op when already current.
func (g *Migrator) Up(ctx context.Context) error {
	return g.run(ctx, g.m.Up)
}

// Down rolls back the given number of migrations (steps >= 1).
func (g *Migrator) Down(ctx context.Context, steps int) error {
	if steps < 1 {
		return fmt.Errorf("migrate: steps must be >= 1, got %d", steps)
	}
	return g.run(ctx, func() error { return g.m.Steps(-steps) })
}

// Version returns the current version and dirty flag.
func (g *Migrator) Version() (uint, bool, error) {
	v, dirty, err := g.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, ErrNoVersion
	}
	if err != nil {
		return 0, false, fmt.Errorf("migrate: version: %w", err)
	}
	return v, dirty, nil
}

// Close releases the source and database connections.
func (g *Migrator) Close() error {
	srcErr, dbErr := g.m.Close()
	return errors.Join(srcErr, dbErr)
}

// run executes fn, asking golang-migrate to stop after the current
// migration if ctx is cancelled. golang-migrate has no context support of
// its own; GracefulStop is its cancellation hook.
func (g *Migrator) run(ctx context.Context, fn func() error) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			select {
			case g.m.GracefulStop <- true:
			default:
			}
		case <-done:
		}
	}()
	err := fn()
	if errors.Is(err, migrate.ErrNoChange) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return ctx.Err()
}

func toPgx5URL(u string) string {
	for _, p := range []string{"postgres://", "postgresql://"} {
		if rest, ok := strings.CutPrefix(u, p); ok {
			return "pgx5://" + rest
		}
	}
	return u
}

func redact(msg, databaseURL string) string {
	if databaseURL == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, databaseURL, "<redacted>")
	return strings.ReplaceAll(msg, toPgx5URL(databaseURL), "<redacted>")
}
