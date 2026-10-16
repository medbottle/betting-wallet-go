//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/medbottle/betting-wallet-go/internal/adapters/postgres"
	"github.com/medbottle/betting-wallet-go/migrations"
)

func TestMigrationsUpDownUp(t *testing.T) {
	urls := startPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	m, err := postgres.NewMigrator(urls.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if _, _, err := m.Version(); !errors.Is(err, postgres.ErrNoVersion) {
		t.Fatalf("fresh database: Version err = %v, want ErrNoVersion", err)
	}

	latest, err := migrations.Latest()
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	assertVersion(t, m, latest)
	assertSchemaInfo(t, ctx, urls.app, true)

	// Up again is a no-op.
	if err := m.Up(ctx); err != nil {
		t.Fatalf("second up: %v", err)
	}

	if err := m.Down(ctx, int(latest)); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, _, err := m.Version(); !errors.Is(err, postgres.ErrNoVersion) {
		t.Fatalf("after full down: Version err = %v, want ErrNoVersion", err)
	}
	assertSchemaInfo(t, ctx, urls.owner, false)

	if err := m.Up(ctx); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	assertVersion(t, m, latest)
}

func assertVersion(t *testing.T, m *postgres.Migrator, want uint) {
	t.Helper()
	v, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if v != want || dirty {
		t.Fatalf("version = %d dirty=%t, want %d clean", v, dirty, want)
	}
}

// assertSchemaInfo checks the 0001 table. Reading it as wallet_app also
// proves the default SELECT grant from the role init script works.
func assertSchemaInfo(t *testing.T, ctx context.Context, url string, wantExists bool) {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.schema_info') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists != wantExists {
		t.Fatalf("schema_info exists = %t, want %t", exists, wantExists)
	}
	if !wantExists {
		return
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM schema_info`).Scan(&n); err != nil {
		t.Fatalf("read schema_info: %v", err)
	}
	if n != 1 {
		t.Fatalf("schema_info rows = %d, want 1", n)
	}
}
