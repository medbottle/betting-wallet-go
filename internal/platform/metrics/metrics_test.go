package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
)

func TestInstrumentsCountWithTheirLabels(t *testing.T) {
	r := New()
	r.TransactionOutcome(app.TransportHTTP, wager.StatusRejected, wager.CodeInsufficientFunds)
	r.TransactionOutcome(app.TransportHTTP, wager.StatusRejected, wager.CodeInsufficientFunds)
	r.Duplicate(app.TransportSQS, app.DuplicateInboxMessage)
	r.Conflict(wager.CodeIdempotencyKeyConflict)
	r.DBConflict("55P03")
	r.Retry(app.RetryConsumer)
	r.DeadLettered(wager.CodeInvalidMoney)
	r.OutboxParked()
	r.ReconciliationDivergence()

	checks := []struct {
		name string
		got  float64
	}{
		{"outcome", testutil.ToFloat64(r.outcomes.WithLabelValues("http", "REJECTED", "INSUFFICIENT_FUNDS"))},
		{"duplicate", testutil.ToFloat64(r.duplicates.WithLabelValues("sqs", "inbox_duplicate"))},
		{"conflict", testutil.ToFloat64(r.conflicts.WithLabelValues("IDEMPOTENCY_KEY_CONFLICT"))},
		{"db conflict", testutil.ToFloat64(r.dbConflicts.WithLabelValues("55P03"))},
		{"retry", testutil.ToFloat64(r.retries.WithLabelValues("consumer"))},
		{"dlq", testutil.ToFloat64(r.deadLettered.WithLabelValues("INVALID_MONEY"))},
		{"parked", testutil.ToFloat64(r.parked)},
		{"divergence", testutil.ToFloat64(r.divergences)},
	}
	want := map[string]float64{"outcome": 2}
	for _, c := range checks {
		w, ok := want[c.name]
		if !ok {
			w = 1
		}
		if c.got != w {
			t.Errorf("%s = %v, want %v", c.name, c.got, w)
		}
	}
}

func TestHandlerExposesHistogramsAndOutboxGauges(t *testing.T) {
	r := New()
	r.ProcessingDuration(app.TransportWorker, "PROCESSED", 30*time.Millisecond)
	r.ObserveHTTP("POST /wagering/transactions", 200, 7*time.Millisecond)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	r.RegisterOutbox(func(context.Context) (int64, time.Time, error) {
		return 3, now.Add(-90 * time.Second), nil
	}, func() time.Time { return now }, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`wager_processing_duration_seconds_bucket{outcome="PROCESSED",transport="worker",le="0.05"} 1`,
		`http_request_duration_seconds_count{code="200",route="POST /wagering/transactions"} 1`,
		"outbox_lag_seconds 90",
		"outbox_unpublished 3",
		"go_goroutines",
		"process_",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition lacks %q", want)
		}
	}
}

func TestOutboxGaugesOmittedWhenTheQueryFails(t *testing.T) {
	r := New()
	r.RegisterOutbox(func(context.Context) (int64, time.Time, error) { return 0, time.Time{}, errors.New("db down") },
		time.Now, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "outbox_lag_seconds ") {
		t.Fatalf("code %d; a failed query must omit the gauges without failing the scrape", rec.Code)
	}
}

func TestEmptyOutboxHasZeroLag(t *testing.T) {
	r := New()
	r.RegisterOutbox(func(context.Context) (int64, time.Time, error) { return 0, time.Time{}, nil },
		time.Now, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "outbox_lag_seconds 0") {
		t.Fatal("empty outbox must report lag 0")
	}
}
