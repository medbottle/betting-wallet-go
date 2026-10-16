// Package metrics implements app.Metrics with Prometheus on a private
// registry and serves it in the text exposition format (ARCHITECTURE §25).
package metrics

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
)

// Buckets cover 5ms to 10s, the handler and statement timeouts (§25, G8).
var Buckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// Registry holds every instrument.
type Registry struct {
	reg *prometheus.Registry

	outcomes     *prometheus.CounterVec
	duplicates   *prometheus.CounterVec
	conflicts    *prometheus.CounterVec
	dbConflicts  *prometheus.CounterVec
	retries      *prometheus.CounterVec
	deadLettered *prometheus.CounterVec
	parked       prometheus.Counter
	divergences  prometheus.Counter
	processing   *prometheus.HistogramVec
	httpRequests *prometheus.HistogramVec
}

var _ app.Metrics = (*Registry)(nil)

// New registers the instruments and the Go and process collectors.
func New() *Registry {
	r := &Registry{
		reg: prometheus.NewRegistry(),
		outcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_transactions_total", Help: "Operations that reached a new state, by transport, status and failure code.",
		}, []string{"transport", "status", "failure_code"}),
		duplicates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_duplicates_total", Help: "Repeated deliveries answered from the persisted result.",
		}, []string{"transport", "kind"}),
		conflicts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_conflicts_total", Help: "Idempotency, external id and wallet conflicts (category C).",
		}, []string{"code"}),
		dbConflicts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "db_concurrency_conflicts_total", Help: "Transactions rolled back by lock timeout, deadlock, serialization failure or statement timeout.",
		}, []string{"sqlstate"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "retries_total", Help: "Work left for a later attempt with backoff.",
		}, []string{"component"}),
		deadLettered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sqs_dlq_messages_total", Help: "Inbound messages sent to the DLQ explicitly, by failure code.",
		}, []string{"failure_code"}),
		parked: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_parked_total", Help: "Outbox events parked after exhausting their attempts.",
		}),
		divergences: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "reconciliation_divergences_total", Help: "Reconciliations whose stored balance differs from the ledger.",
		}),
		processing: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "wager_processing_duration_seconds", Help: "Duration of one submission or worker iteration.", Buckets: Buckets,
		}, []string{"transport", "outcome"}),
		httpRequests: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_request_duration_seconds", Help: "HTTP requests by route pattern and status code.", Buckets: Buckets,
		}, []string{"route", "code"}),
	}
	r.reg.MustRegister(r.outcomes, r.duplicates, r.conflicts, r.dbConflicts, r.retries, r.deadLettered,
		r.parked, r.divergences, r.processing, r.httpRequests,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return r
}

func (r *Registry) TransactionOutcome(t app.Transport, s wager.Status, code wager.FailureCode) {
	r.outcomes.WithLabelValues(string(t), string(s), string(code)).Inc()
}

func (r *Registry) Duplicate(t app.Transport, k app.DuplicateKind) {
	r.duplicates.WithLabelValues(string(t), string(k)).Inc()
}

func (r *Registry) Conflict(code wager.FailureCode) { r.conflicts.WithLabelValues(string(code)).Inc() }
func (r *Registry) DBConflict(sqlstate string)      { r.dbConflicts.WithLabelValues(sqlstate).Inc() }
func (r *Registry) Retry(component string)          { r.retries.WithLabelValues(component).Inc() }

func (r *Registry) DeadLettered(code wager.FailureCode) {
	r.deadLettered.WithLabelValues(string(code)).Inc()
}

func (r *Registry) OutboxParked()             { r.parked.Inc() }
func (r *Registry) ReconciliationDivergence() { r.divergences.Inc() }

func (r *Registry) ProcessingDuration(t app.Transport, outcome string, d time.Duration) {
	r.processing.WithLabelValues(string(t), outcome).Observe(d.Seconds())
}

// ObserveHTTP records one request; route is the ServeMux pattern.
func (r *Registry) ObserveHTTP(route string, code int, d time.Duration) {
	r.httpRequests.WithLabelValues(route, strconv.Itoa(code)).Observe(d.Seconds())
}

// Handler serves the registry in the Prometheus text format.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

// Backlog reports the unpublished, unparked outbox rows and the oldest
// one's occurred_at (zero when there are none).
type Backlog func(ctx context.Context) (count int64, oldest time.Time, err error)

// RegisterOutbox adds outbox_unpublished and outbox_lag_seconds, computed
// at scrape time with one query bounded by timeout (§25, G7). A failed
// query omits both series for that scrape.
func (r *Registry) RegisterOutbox(backlog Backlog, now func() time.Time, timeout time.Duration, logger *slog.Logger) {
	r.reg.MustRegister(&outboxCollector{backlog: backlog, now: now, timeout: timeout, logger: logger})
}

var (
	outboxLagDesc = prometheus.NewDesc("outbox_lag_seconds",
		"Age of the oldest unpublished outbox event (now - min(occurred_at)); 0 when none. The same global value on every instance.", nil, nil)
	outboxUnpublishedDesc = prometheus.NewDesc("outbox_unpublished",
		"Unpublished, unparked outbox events.", nil, nil)
)

type outboxCollector struct {
	backlog Backlog
	now     func() time.Time
	timeout time.Duration
	logger  *slog.Logger
}

func (c *outboxCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- outboxLagDesc
	ch <- outboxUnpublishedDesc
}

func (c *outboxCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	count, oldest, err := c.backlog(ctx)
	if err != nil {
		c.logger.Warn("outbox backlog query failed", slog.String("error", err.Error()))
		return
	}
	lag := 0.0
	if !oldest.IsZero() {
		if d := c.now().Sub(oldest); d > 0 {
			lag = d.Seconds()
		}
	}
	ch <- prometheus.MustNewConstMetric(outboxLagDesc, prometheus.GaugeValue, lag)
	ch <- prometheus.MustNewConstMetric(outboxUnpublishedDesc, prometheus.GaugeValue, float64(count))
}
