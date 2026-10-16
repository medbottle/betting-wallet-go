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
