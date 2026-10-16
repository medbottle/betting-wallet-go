//go:build integration

// Package integration runs against real containers (testcontainers-go).
// Run with: go test -tags=integration ./test/integration/...
package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// postgresImage must match docker-compose.yml.
const postgresImage = "postgres:18.6-alpine3.24"

const (
	ownerPassword = "owner_test_pw"
	appPassword   = "app_test_pw"
)

type pgURLs struct {
	owner string // wallet_owner: migrations
	app   string // wallet_app: application
}

// startPostgres starts a fresh Postgres initialised with the same role
// script compose uses, and returns connection URLs for both roles.
func startPostgres(t *testing.T) pgURLs {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithInitScripts(filepath.Join(repoRoot(t), "deploy", "postgres", "init", "01-roles.sh")),
		testcontainers.WithEnv(map[string]string{
			"WALLET_DB":             "wallet",
			"WALLET_OWNER_PASSWORD": ownerPassword,
			"WALLET_APP_PASSWORD":   appPassword,
		}),
		tcpostgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("postgres host: %v", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("postgres port: %v", err)
	}
	url := func(user, pw string) string {
		return fmt.Sprintf("postgres://%s:%s@%s:%s/wallet?sslmode=disable", user, pw, host, port.Port())
	}
	return pgURLs{owner: url("wallet_owner", ownerPassword), app: url("wallet_app", appPassword)}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}
