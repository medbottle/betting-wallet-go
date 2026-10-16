package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/domain/event"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
)

type inboxRepo struct{ tx pgx.Tx }

func (r inboxRepo) Insert(ctx context.Context, m app.InboxMessage) (bool, error) {
	if m.ConsumerName == "" || m.MessageID == "" || m.ReceivedAt.IsZero() {
		return false, fmt.Errorf("%w: inbox message needs consumer, message id and receipt time", app.ErrInvariantViolation)
	}
	var consumer string
	err := r.tx.QueryRow(ctx, `
		INSERT INTO inbox_messages (consumer_name, message_id, payload_hash, received_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING RETURNING consumer_name`,
		m.ConsumerName, m.MessageID, m.PayloadHash[:], m.ReceivedAt).Scan(&consumer)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, classify(err)
	}
	return true, nil
}

func (r inboxRepo) Get(ctx context.Context, consumerName, messageID string) (app.InboxMessage, error) {
	var (
		m       = app.InboxMessage{ConsumerName: consumerName, MessageID: messageID}
		hash    []byte
		outcome *string
	)
	err := r.tx.QueryRow(ctx, `
		SELECT payload_hash, transaction_id, outcome, received_at, processed_at
		FROM inbox_messages WHERE consumer_name = $1 AND message_id = $2`,
		consumerName, messageID).Scan(&hash, &m.TransactionID, &outcome, &m.ReceivedAt, &m.ProcessedAt)
	if err != nil {
		return app.InboxMessage{}, notFound(err, "inbox message "+messageID)
	}
	if len(hash) != len(m.PayloadHash) {
		return app.InboxMessage{}, corrupt(errors.New("payload hash length"), "inbox message "+messageID)
	}
	copy(m.PayloadHash[:], hash)
	if outcome != nil {
		s := wager.Status(*outcome)
		m.Outcome = &s
	}
	return m, nil
}

func (r inboxRepo) Complete(ctx context.Context, consumerName, messageID string, transactionID uuid.UUID, outcome wager.Status, processedAt time.Time) error {
	tag, err := r.tx.Exec(ctx, `
		UPDATE inbox_messages SET transaction_id = $3, outcome = $4, processed_at = $5
		WHERE consumer_name = $1 AND message_id = $2 AND processed_at IS NULL`,
		consumerName, messageID, transactionID, string(outcome), processedAt)
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: inbox message %s is missing or already completed", app.ErrInvariantViolation, messageID)
	}
	return nil
}

type outboxRepo struct{ tx pgx.Tx }

// Insert stores each envelope's frozen payload. The column is JSONB
// (ARCHITECTURE §9), so Postgres keeps the parsed document: content is
// preserved exactly, key order and whitespace are not.
func (r outboxRepo) Insert(ctx context.Context, events ...event.Envelope) error {
	for _, e := range events {
		if !e.Valid() {
			return fmt.Errorf("%w: refusing to insert an uninitialised event", app.ErrInvariantViolation)
		}
		var causation *string
		if c, ok := e.CausationID(); ok {
			causation = &c
		}
		_, err := r.tx.Exec(ctx, `
			INSERT INTO outbox_events (event_id, aggregate_type, aggregate_id, event_type, event_version,
				correlation_id, causation_id, occurred_at, payload)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			e.EventID(), e.AggregateType(), e.AggregateID(), string(e.Type()), e.Version(),
			e.CorrelationID(), causation, e.OccurredAt(), e.Payload())
		if err != nil {
			return classify(err)
		}
	}
	return nil
}

// ClaimBatch is the lease claim of ARCHITECTURE §9.
func (r outboxRepo) ClaimBatch(ctx context.Context, owner string, lease time.Duration, limit int) ([]app.OutboxRecord, error) {
	if owner == "" || lease <= 0 || limit < 1 {
		return nil, fmt.Errorf("%w: claim needs an owner, a lease and a limit", app.ErrInvariantViolation)
	}
	rows, err := r.tx.Query(ctx, `
		UPDATE outbox_events
		SET locked_by = $1, locked_until = now() + ($2 * interval '1 millisecond'), attempts = attempts + 1
		WHERE event_id IN (
			SELECT event_id FROM outbox_events
			WHERE published_at IS NULL AND parked_at IS NULL AND next_attempt_at <= now()
				AND (locked_until IS NULL OR locked_until < now())
			ORDER BY next_attempt_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED)
		RETURNING event_id, aggregate_id, event_type, event_version, payload::text, attempts`,
		owner, lease.Milliseconds(), limit)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	var out []app.OutboxRecord
	for rows.Next() {
		var (
			rec     app.OutboxRecord
			payload string
		)
		if err := rows.Scan(&rec.EventID, &rec.AggregateID, &rec.EventType, &rec.EventVersion, &payload, &rec.Attempts); err != nil {
			return nil, classify(err)
		}
		rec.Payload = []byte(payload)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return out, nil
}

func (r outboxRepo) MarkPublished(ctx context.Context, owner string, eventID uuid.UUID) (bool, error) {
	tag, err := r.tx.Exec(ctx, `
		UPDATE outbox_events SET published_at = now(), locked_until = NULL, last_error = NULL
		WHERE event_id = $1 AND locked_by = $2 AND published_at IS NULL`, eventID, owner)
	if err != nil {
		return false, classify(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r outboxRepo) Reschedule(ctx context.Context, owner string, eventID uuid.UUID, next time.Time, lastError string) error {
	_, err := r.tx.Exec(ctx, `
		UPDATE outbox_events SET next_attempt_at = $3, locked_until = NULL, last_error = $4
		WHERE event_id = $1 AND locked_by = $2 AND published_at IS NULL`, eventID, owner, next, lastError)
	return classify(err)
}

func (r outboxRepo) Park(ctx context.Context, owner string, eventID uuid.UUID, lastError string) error {
	_, err := r.tx.Exec(ctx, `
		UPDATE outbox_events SET parked_at = now(), locked_until = NULL, last_error = $3
		WHERE event_id = $1 AND locked_by = $2 AND published_at IS NULL`, eventID, owner, lastError)
	return classify(err)
}
