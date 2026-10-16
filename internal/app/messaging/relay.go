// Package messaging holds the outbox relay. The relay publishes committed
// outbox rows; it never publishes inside a database transaction (CLAUDE.md,
// ARCHITECTURE §9). The inbox lives in wagering.Submit (ARCHITECTURE §23).
package messaging

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/medbottle/betting-wallet-go/internal/app"
)

// RelayConfig holds the relay settings (ARCHITECTURE §9).
type RelayConfig struct {
	// Owner is this process's lease owner (locked_by): unique per process.
	Owner          string
	Lease          time.Duration
	BatchSize      int
	PublishTimeout time.Duration
	BackoffBase    time.Duration
	BackoffMax     time.Duration
	MaxAttempts    int
	// Jitter returns a value in [0, n); nil uses math/rand/v2.
	Jitter func(n int64) int64
}

// Relay publishes outbox rows.
type Relay struct {
	tx        app.TxRunner
	publisher app.EventPublisher
	clock     app.Clock
	cfg       RelayConfig
	logger    *slog.Logger
}

// NewRelay wires the relay to its ports.
func NewRelay(tx app.TxRunner, publisher app.EventPublisher, clock app.Clock, cfg RelayConfig, logger *slog.Logger) *Relay {
	return &Relay{tx: tx, publisher: publisher, clock: clock, cfg: cfg, logger: logger}
}

// RelayOnce is one relay cycle (ARCHITECTURE §9):
//
//  1. claim up to BatchSize due rows with a lease, in a short transaction
//     that commits before anything is sent;
//  2. send them with one SendMessageBatch, outside any transaction, bounded
//     by PublishTimeout;
//  3. per entry: mark published (only while this owner still holds the
//     row), or reschedule with backoff, or park when attempts are
//     exhausted. A failure of the whole call reschedules the whole batch.
//
// If the process dies between 2 and 3, the lease expires and another relay
// republishes with the same eventId. didWork is false when nothing was due.
func (r *Relay) RelayOnce(ctx context.Context) (didWork bool, err error) {
	var batch []app.OutboxRecord
	err = r.tx.InTx(ctx, app.TxOptions{}, func(ctx context.Context, repos app.Repos) error {
		var err error
		batch, err = repos.Outbox().ClaimBatch(ctx, r.cfg.Owner, r.cfg.Lease, r.cfg.BatchSize)
		return err
	})
	if err != nil || len(batch) == 0 {
		return false, err
	}

	events := make([]app.OutboundEvent, len(batch))
	for i, rec := range batch {
		events[i] = app.OutboundEvent{
			EventID: rec.EventID, GroupID: rec.AggregateID.String(),
			EventType: rec.EventType, EventVersion: rec.EventVersion, Body: rec.Payload,
		}
	}
	pubCtx, cancel := context.WithTimeout(ctx, r.cfg.PublishTimeout)
	results, pubErr := r.publisher.PublishBatch(pubCtx, events)
	cancel()
	if pubErr == nil && len(results) != len(batch) {
		pubErr = fmt.Errorf("publisher returned %d results for %d events", len(results), len(batch))
	}
	if pubErr != nil {
		results = make([]app.PublishResult, len(batch))
		for i, rec := range batch {
			results[i] = app.PublishResult{EventID: rec.EventID, Err: pubErr}
		}
	}

	// Record outcomes even if ctx was cancelled meanwhile (shutdown): a
	// published row left unmarked would only be republished after the lease.
	recordCtx, cancelRecord := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.PublishTimeout)
	defer cancelRecord()
	if err := r.record(recordCtx, batch, results); err != nil {
		return true, err
	}
	return true, pubErr
}

func (r *Relay) record(ctx context.Context, batch []app.OutboxRecord, results []app.PublishResult) error {
	now := r.clock.Now()
	return r.tx.InTx(ctx, app.TxOptions{}, func(ctx context.Context, repos app.Repos) error {
		for i, rec := range batch {
			res := results[i]
			if res.Err == nil {
				held, err := repos.Outbox().MarkPublished(ctx, r.cfg.Owner, rec.EventID)
				if err != nil {
					return err
				}
				if !held {
					// The lease expired and another relay took the row; it
					// republishes with the same eventId (at-least-once).
					r.logger.Warn("outbox lease lost before marking", slog.String("eventId", rec.EventID.String()))
				}
				continue
			}
			lastErr := res.Err.Error()
			if rec.Attempts >= r.cfg.MaxAttempts {
				if err := repos.Outbox().Park(ctx, r.cfg.Owner, rec.EventID, lastErr); err != nil {
					return err
				}
				// The outbox_parked_total metric comes with the metrics
				// slice; until then this error log is the alert.
				r.logger.Error("outbox event parked after exhausting attempts",
					slog.String("eventId", rec.EventID.String()), slog.String("eventType", rec.EventType),
					slog.Int("attempts", rec.Attempts), slog.Bool("senderFault", res.SenderFault), slog.String("error", lastErr))
				continue
			}
			if err := repos.Outbox().Reschedule(ctx, r.cfg.Owner, rec.EventID, r.next(now, rec.Attempts), lastErr); err != nil {
				return err
			}
		}
		return nil
	})
}

// next is now + min(base * 2^attempts, max) + jitter (ARCHITECTURE §9),
// with the additive jitter drawn from [0, base) (ARCHITECTURE §22).
func (r *Relay) next(now time.Time, attempts int) time.Time {
	d := r.cfg.BackoffMax
	if attempts < 62 {
		if step := r.cfg.BackoffBase << attempts; step > 0 && step < r.cfg.BackoffMax {
			d = step
		}
	}
	jitter := r.cfg.Jitter
	if jitter == nil {
		jitter = rand.Int64N
	}
	if r.cfg.BackoffBase > 0 {
		d += time.Duration(jitter(int64(r.cfg.BackoffBase)))
	}
	return now.Add(d)
}
