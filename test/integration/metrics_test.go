//go:build integration

package integration

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/adapters/postgres"
	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/app/messaging"
	"github.com/medbottle/betting-wallet-go/internal/platform/clock"
	"github.com/medbottle/betting-wallet-go/internal/platform/metrics"
)

// scrape reads the registry's exposition into "name{labels}" -> value.
func scrape(t *testing.T, reg *metrics.Registry) map[string]float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	out := map[string]float64{}
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("exposition line %q: %v", line, err)
		}
		out[line[:i]] = v
	}
	return out
}

type downPublisher struct{}

func (downPublisher) PublishBatch(context.Context, []app.OutboundEvent) ([]app.PublishResult, error) {
	return nil, errors.New("broker unreachable (simulated)")
}

// TestMetricsRecorded drives each instrumented path once against real
// Postgres, Keycloak and MiniStack and reads the Prometheus exposition
// (CHALLENGE §12, ARCHITECTURE §25).
func TestMetricsRecorded(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t, withReference(refTTL, testBackoffBase, testBackoffMax))
	reg := e.metrics
	reg.RegisterOutbox(e.repo.reader.OutboxBacklog, time.Now, time.Second, e.logger)
	w := e.openWallet(t, "100.00")

	// HTTP: processed, replay, conflict, rejection, wallet conflict.
	bet := op(w, "provider-a", "BET", "30.00", "m-bet")
	if r := e.submit(t, "provider-a", bet); r.Status != http.StatusOK {
		t.Fatalf("bet: %d %s", r.Status, r.Raw)
	}
	e.submit(t, "provider-a", bet)
	changed := op(w, "provider-a", "BET", "31.00", "m-bet")
	if r := e.submit(t, "provider-a", changed); r.Status != http.StatusConflict {
		t.Fatalf("conflict: %d", r.Status)
	}
	if r := e.submit(t, "provider-a", op(w, "provider-a", "BET", "500.00", "m-broke")); r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("rejection: %d", r.Status)
	}
	if r := e.call(t, http.MethodPost, "/wallets", "wallet-internal", map[string]any{
		"playerId": w.PlayerID, "initialBalance": map[string]string{"amount": "0.00", "currency": "BRL"},
	}); r.Status != http.StatusConflict {
		t.Fatalf("wallet conflict: %d", r.Status)
	}

	// Worker: a WIN waits for its BET, then the worker resolves it.
	if r := e.submit(t, "provider-a", op(w, "provider-a", "WIN", "10.00", "m-win", "m-late")); r.Status != http.StatusAccepted {
		t.Fatalf("win: %d %s", r.Status, r.Raw)
	}
	e.submit(t, "provider-a", op(w, "provider-a", "BET", "5.00", "m-late"))
	startWorker(t, "metrics-worker", e.wagering, 1)
	e.waitStatus(t, "provider-a", "provider-a", "m-win", "PROCESSED", 10*time.Second)

	// SQS: one message, one redelivery, one invalid message.
	q := newInboundQueues(t, 60, 10)
	c, _ := startConsumer(t, e, q, consumerOpts{metrics: reg})
	body := envelope(t, "m-msg", op(w, "provider-a", "BET", "1.00", "m-sqs"))
	q.send(t, body, w.ID)
	q.send(t, body, w.ID)
	q.send(t, `{"messageId":"m-bad"}`, w.ID)
	waitFor(t, 20*time.Second, "consumer outcomes", func() bool {
		s := c.Stats()
		return s.Handled == 1 && s.Duplicates == 1 && s.DeadLetter == 1
	})

	// Database conflict: a lock wait cut by lock_timeout.
	runner := postgres.NewTxRunner(e.repo.db.app, 200*time.Millisecond, testStatementTimeout, reg)
	release := holdTx(t, e.repo.db.app, `SELECT 1 FROM wallets WHERE id = $1 FOR NO KEY UPDATE`, w.ID)
	err := runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
		_, err := r.Wallets().GetForUpdate(ctx, uuid.MustParse(w.ID))
		return err
	})
	release()
	if !errors.Is(err, app.ErrTransient) {
		t.Fatalf("lock wait: %v", err)
	}

	// Reconciliation without divergence.
	e.assertConsistent(t, w, "74.00") // 100 - 30 - 5 + 10 - 1

	got := scrape(t, reg)
	// Unpublished events wait for a relay that is not running yet.
	if got["outbox_unpublished"] < 1 || got["outbox_lag_seconds"] <= 0 {
		t.Errorf("outbox gauges: unpublished %v, lag %v", got["outbox_unpublished"], got["outbox_lag_seconds"])
	}

	// Outbox: a broker outage reschedules, and the last attempt parks.
	relay := messaging.NewRelay(e.repo.runner, downPublisher{}, clock.System{}, messaging.RelayConfig{
		Owner: "metrics-relay", Lease: 30 * time.Second, BatchSize: 10, PublishTimeout: time.Second,
		BackoffBase: time.Millisecond, BackoffMax: time.Millisecond, MaxAttempts: 2,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)), reg)
	waitFor(t, 15*time.Second, "outbox parked", func() bool {
		_, _ = relay.RelayOnce(ctxT(t))
		return e.count(t, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL AND parked_at IS NULL`) == 0
	})
	got = scrape(t, reg)

	want := map[string]float64{
		`wager_transactions_total{failure_code="",status="PROCESSED",transport="http"}`:                  2, // m-bet, m-late
		`wager_transactions_total{failure_code="INSUFFICIENT_FUNDS",status="REJECTED",transport="http"}`: 1,
		`wager_transactions_total{failure_code="",status="PENDING_REFERENCE",transport="http"}`:          1,
		`wager_transactions_total{failure_code="",status="PROCESSED",transport="worker"}`:                1,
		`wager_transactions_total{failure_code="",status="PROCESSED",transport="sqs"}`:                   1,
		`wager_duplicates_total{kind="idempotent_replay",transport="http"}`:                              1,
		`wager_duplicates_total{kind="inbox_duplicate",transport="sqs"}`:                                 1,
		`wager_conflicts_total{code="IDEMPOTENCY_KEY_CONFLICT"}`:                                         1,
		`wager_conflicts_total{code="WALLET_ALREADY_EXISTS"}`:                                            1,
		`sqs_dlq_messages_total{failure_code="INVALID_REQUEST"}`:                                         1,
		`db_concurrency_conflicts_total{sqlstate="55P03"}`:                                               1,
		`outbox_unpublished`: 0,
		`outbox_lag_seconds`: 0,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if got[`retries_total{component="outbox"}`] < 1 || got["outbox_parked_total"] < 1 {
		t.Errorf("outbox retries %v, parked %v", got[`retries_total{component="outbox"}`], got["outbox_parked_total"])
	}
	if got["reconciliation_divergences_total"] != 0 {
		t.Errorf("divergences = %v on a consistent wallet", got["reconciliation_divergences_total"])
	}
	if got[`http_request_duration_seconds_count{code="200",route="POST /wagering/transactions"}`] < 2 ||
		got[`wager_processing_duration_seconds_count{outcome="PROCESSED",transport="worker"}`] != 1 {
		t.Errorf("histograms: http %v, worker %v",
			got[`http_request_duration_seconds_count{code="200",route="POST /wagering/transactions"}`],
			got[`wager_processing_duration_seconds_count{outcome="PROCESSED",transport="worker"}`])
	}
	for k := range got {
		if strings.Contains(k, w.ID) || strings.Contains(k, "provider-a") {
			t.Errorf("high-cardinality label leaked: %s", k)
		}
	}
}
