//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/goleak"

	"github.com/medbottle/betting-wallet-go/internal/adapters/httpapi"
	"github.com/medbottle/betting-wallet-go/internal/adapters/postgres"
	"github.com/medbottle/betting-wallet-go/internal/bootstrap"
)

// unreachableOIDC points the IdP settings at a closed port, for tests that
// must fail before (or exactly at) the JWKS fetch.
var unreachableOIDC = keycloakEnv{
	Issuer:  "http://127.0.0.1:1/realms/wagering",
	JWKSURL: "http://127.0.0.1:1/realms/wagering/protocol/openid-connect/certs",
}

// sqsEnv points the app at a broker endpoint and its events queue.
type sqsEnv struct{ Endpoint, Queue string }

// unreachableSQS is for tests that fail before the queue is resolved.
var unreachableSQS = sqsEnv{Endpoint: "http://127.0.0.1:1", Queue: "wallet-events.fifo"}

func setSQSEnv(t *testing.T, s sqsEnv) {
	t.Setenv("SQS_ENDPOINT", s.Endpoint)
	t.Setenv("SQS_EVENTS_QUEUE", s.Queue)
	t.Setenv("SQS_PUBLISHER_ACCESS_KEY_ID", publisherKey)
	t.Setenv("SQS_PUBLISHER_SECRET_ACCESS_KEY", publisherSecret)
	t.Setenv("SQS_CONSUMER_ACCESS_KEY_ID", consumerKey)
	t.Setenv("SQS_CONSUMER_SECRET_ACCESS_KEY", consumerSecret)
	t.Setenv("SQS_PROVIDER_SENDERS", providerAKey+"=provider-a,"+providerBKey+"=provider-b")
	t.Setenv("SQS_OPERATOR_SENDER_ID", operatorKey)
}

func setAppEnv(t *testing.T, databaseURL string, kc keycloakEnv) {
	setSQSEnv(t, unreachableSQS)
	t.Setenv("DATABASE_URL", databaseURL)
	t.Setenv("OIDC_ISSUER", kc.Issuer)
	t.Setenv("OIDC_JWKS_URL", kc.JWKSURL)
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("METRICS_ADDR", "") // tests that need it pick a free port
	t.Setenv("READINESS_INTERVAL", "200ms")
	t.Setenv("READINESS_CHECK_TIMEOUT", "200ms")
	t.Setenv("READINESS_DRAIN_DELAY", "1s")
	t.Setenv("LOG_LEVEL", "warn")
}

// TestFxStartFailsOnUnmigratedSchema checks the startup schema check
// (ARCHITECTURE §14 step 2) fails fast and leaves nothing running.
func TestFxStartFailsOnUnmigratedSchema(t *testing.T) {
	urls := startPostgres(t)
	setAppEnv(t, urls.app, unreachableOIDC)
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

// TestFxStartFailsWithoutJWKS: the first JWKS fetch must succeed (fail
// fast, ARCHITECTURE §11 and §14 step 3), and the failed start releases
// the pool and everything else.
func TestFxStartFailsWithoutJWKS(t *testing.T) {
	urls := startPostgres(t)
	migrateUp(t, urls.owner)
	setAppEnv(t, urls.app, unreachableOIDC)
	leaks := goleak.IgnoreCurrent()

	app := fx.New(bootstrap.Options())
	if err := app.Err(); err != nil {
		t.Fatalf("build graph: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := app.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "initial JWKS fetch") {
		t.Fatalf("Start err = %v, want the initial JWKS fetch to fail the start", err)
	}
	goleak.VerifyNone(t, leaks)
}

// TestFxStartFailsWithoutQueue: the queue URL is resolved at start
// (ARCHITECTURE §14 step 4); a missing queue fails the start and leaks
// nothing.
func TestFxStartFailsWithoutQueue(t *testing.T) {
	urls := startPostgres(t)
	migrateUp(t, urls.owner)
	setAppEnv(t, urls.app, startKeycloak(t))
	setSQSEnv(t, sqsEnv{Endpoint: startMiniStack(t), Queue: "does-not-exist.fifo"})
	leaks := goleak.IgnoreCurrent()

	app := fx.New(bootstrap.Options())
	if err := app.Err(); err != nil {
		t.Fatalf("build graph: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := app.Start(ctx); err == nil || !strings.Contains(err.Error(), "resolve queue does-not-exist.fifo") {
		t.Fatalf("Start err = %v, want the queue resolution to fail the start", err)
	}
	goleak.VerifyNone(t, leaks)
}

// TestFxLifecycle starts the full graph against a migrated database,
// checks the health endpoints, verifies readiness drains before the server
// stops, and asserts that stopping leaves no goroutines behind.
func TestFxLifecycle(t *testing.T) {
	urls := startPostgres(t)
	migrateUp(t, urls.owner)
	setAppEnv(t, urls.app, startKeycloak(t))
	setSQSEnv(t, sqsEnv{Endpoint: startMiniStack(t), Queue: "wallet-events.fifo"})
	metricsAddr := freeAddr(t)
	t.Setenv("METRICS_ADDR", metricsAddr)

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

	// The metrics listener serves the Prometheus exposition (ARCHITECTURE §25).
	resp, err := client.Get("http://" + metricsAddr + "/metrics")
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	exposition, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(exposition), "outbox_lag_seconds") ||
		!strings.Contains(string(exposition), "go_goroutines") {
		t.Fatalf("metrics: %d %.300s", resp.StatusCode, exposition)
	}

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
	if _, err := client.Get("http://" + metricsAddr + "/metrics"); err == nil {
		t.Fatal("metrics listener still accepting connections after stop")
	}
	client.CloseIdleConnections()
	goleak.VerifyNone(t, leaks)
}

// freeAddr returns a loopback address with a port that was free a moment
// ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
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
