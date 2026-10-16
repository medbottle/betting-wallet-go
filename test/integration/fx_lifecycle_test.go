//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/goleak"

	"github.com/medbottle/betting-wallet-go/internal/adapters/httpapi"
	"github.com/medbottle/betting-wallet-go/internal/adapters/postgres"
	"github.com/medbottle/betting-wallet-go/internal/bootstrap"
)

func setAppEnv(t *testing.T, databaseURL string) {
	t.Setenv("DATABASE_URL", databaseURL)
	// Required since the OIDC settings; nothing fetches the JWKS yet.
	t.Setenv("OIDC_ISSUER", "http://127.0.0.1:1/realms/wagering")
	t.Setenv("OIDC_JWKS_URL", "http://127.0.0.1:1/realms/wagering/protocol/openid-connect/certs")
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("READINESS_INTERVAL", "200ms")
	t.Setenv("READINESS_CHECK_TIMEOUT", "200ms")
	t.Setenv("READINESS_DRAIN_DELAY", "1s")
	t.Setenv("LOG_LEVEL", "warn")
}

// TestFxStartFailsOnUnmigratedSchema checks the startup schema check
// (ARCHITECTURE §14 step 2) fails fast and leaves nothing running.
func TestFxStartFailsOnUnmigratedSchema(t *testing.T) {
	urls := startPostgres(t)
	setAppEnv(t, urls.app)
	leaks := goleak.IgnoreCurrent()

	app := fx.New(bootstrap.Options())
	if err := app.Err(); err != nil {
		t.Fatalf("build graph: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := app.Start(ctx)
	if !errors.Is(err, postgres.ErrSchemaNotMigrated) {
		t.Fatalf("Start err = %v, want ErrSchemaNotMigrated", err)
	}
	goleak.VerifyNone(t, leaks)
}

// TestFxLifecycle starts the full graph against a migrated database,
// checks the health endpoints, verifies readiness drains before the server
// stops, and asserts that stopping leaves no goroutines behind.
func TestFxLifecycle(t *testing.T) {
	urls := startPostgres(t)
	migrateUp(t, urls.owner)
	setAppEnv(t, urls.app)

	// Snapshot after the container is up so testcontainers' own goroutines
	// are not reported.
	leaks := goleak.IgnoreCurrent()

	var srv *httpapi.Server
	app := fx.New(bootstrap.Options(fx.Populate(&srv)))
	if err := app.Err(); err != nil {
		t.Fatalf("build graph: %v", err)
	}

	startCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start: %v", err)
	}

	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	base := fmt.Sprintf("http://%s", srv.Addr())

	if code, status := getHealth(t, client, base+"/health/live"); code != http.StatusOK || status != "ok" {
		t.Fatalf("live = %d %q", code, status)
	}
	waitFor(t, 10*time.Second, "ready", func() bool {
		code, status := getHealth(t, client, base+"/health/ready")
		return code == http.StatusOK && status == "ready"
	})

	stopErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), bootstrap.StopTimeout)
		defer cancel()
		stopErr <- app.Stop(ctx)
	}()

	// During the drain delay the server still answers, but reports draining.
	waitFor(t, 900*time.Millisecond, "draining", func() bool {
		code, status := getHealth(t, client, base+"/health/ready")
		return code == http.StatusServiceUnavailable && status == "draining"
	})

	select {
	case err := <-stopErr:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(bootstrap.StopTimeout + 5*time.Second):
		t.Fatal("stop did not return")
	}

	if _, err := client.Get(base + "/health/live"); err == nil {
		t.Fatal("server still accepting connections after stop")
	}
	client.CloseIdleConnections()
	goleak.VerifyNone(t, leaks)
}

func migrateUp(t *testing.T, ownerURL string) {
	t.Helper()
	m, err := postgres.NewMigrator(ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := m.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
}

func getHealth(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	var body struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.Status
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", timeout, what)
}
