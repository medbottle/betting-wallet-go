// Package event holds the integration event envelope and one constructor
// per event type (CHALLENGE §11). Constructors fix type and version and
// freeze the JSON payload at construction, so the outbox stores an
// immutable snapshot.
package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/domain/money"
)

// ErrInvalidEvent means an event could not be built from its inputs.
var ErrInvalidEvent = errors.New("event: invalid event")

// Type names an integration event.
type Type string

const (
	TypeWagerTransactionProcessed        Type = "WagerTransactionProcessed"
	TypeWagerTransactionRejected         Type = "WagerTransactionRejected"
	TypeWalletBalanceChanged             Type = "WalletBalanceChanged"
	TypeWagerTransactionPendingReference Type = "WagerTransactionPendingReference"
)

// AggregateTypeWallet is the aggregate of every event: the wallet, which is
// also the SQS MessageGroupId (ARCHITECTURE §10).
const AggregateTypeWallet = "Wallet"

// TimeLayout is RFC 3339 in UTC with millisecond precision, e.g.
// 2026-09-08T12:00:00.000Z.
const TimeLayout = "2006-01-02T15:04:05.000Z07:00"

// FormatTime renders t in TimeLayout, always in UTC.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// Meta carries the envelope fields every constructor needs. EventID comes
// from the caller (a UUIDv7 from the application's ID generator) and is
// kept on every republish.
type Meta struct {
	EventID       uuid.UUID
	AggregateID   uuid.UUID
	CorrelationID string
	CausationID   *string
	OccurredAt    time.Time
}

// Envelope is an immutable integration event.
type Envelope struct {
	eventID       uuid.UUID
	eventType     Type
	version       int
	aggregateID   uuid.UUID
	correlationID string
	causationID   *string
	occurredAt    time.Time
	data          any
	payload       []byte
}

type wire struct {
	EventID       uuid.UUID `json:"eventId"`
	EventType     Type      `json:"eventType"`
	AggregateID   uuid.UUID `json:"aggregateId"`
	CorrelationID string    `json:"correlationId"`
	CausationID   *string   `json:"causationId,omitempty"`
	OccurredAt    string    `json:"occurredAt"`
	Version       int       `json:"version"`
	Data          any       `json:"data"`
}

func build(m Meta, t Type, version int, data any) (Envelope, error) {
	if m.EventID == uuid.Nil || m.AggregateID == uuid.Nil || m.CorrelationID == "" || m.OccurredAt.IsZero() {
		return Envelope{}, fmt.Errorf("%w: event id, aggregate id, correlation id and time must be set", ErrInvalidEvent)
	}
	if m.CausationID != nil && *m.CausationID == "" {
		return Envelope{}, fmt.Errorf("%w: causation id must be omitted rather than empty", ErrInvalidEvent)
	}
	var causation *string
	if m.CausationID != nil {
		c := *m.CausationID
		causation = &c
	}
	occurred := m.OccurredAt.UTC()
	payload, err := json.Marshal(wire{
		EventID:       m.EventID,
		EventType:     t,
		AggregateID:   m.AggregateID,
		CorrelationID: m.CorrelationID,
		CausationID:   causation,
		OccurredAt:    FormatTime(occurred),
		Version:       version,
		Data:          data,
	})
	if err != nil {
		return Envelope{}, fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	return Envelope{
		eventID:       m.EventID,
		eventType:     t,
		version:       version,
		aggregateID:   m.AggregateID,
		correlationID: m.CorrelationID,
		causationID:   causation,
		occurredAt:    occurred,
		data:          data,
		payload:       payload,
	}, nil
}

// Valid reports whether e was built by one of the constructors. The outbox
// must refuse an envelope that is not valid (the zero value).
func (e Envelope) Valid() bool {
	return e.eventID != uuid.Nil && e.aggregateID != uuid.Nil && e.eventType != "" && e.version >= 1 && len(e.payload) > 0
}

func (e Envelope) EventID() uuid.UUID     { return e.eventID }
func (e Envelope) Type() Type             { return e.eventType }
func (e Envelope) Version() int           { return e.version }
func (e Envelope) AggregateType() string  { return AggregateTypeWallet }
func (e Envelope) AggregateID() uuid.UUID { return e.aggregateID }
func (e Envelope) CorrelationID() string  { return e.correlationID }
func (e Envelope) OccurredAt() time.Time  { return e.occurredAt }

// CausationID returns the causation id, if any.
func (e Envelope) CausationID() (string, bool) {
	if e.causationID == nil {
		return "", false
	}
	return *e.causationID, true
}

// Data returns the typed payload (one of the *Data structs below).
func (e Envelope) Data() any { return e.data }

// Payload returns a copy of the frozen JSON envelope.
func (e Envelope) Payload() []byte { return append([]byte(nil), e.payload...) }

// WagerTransactionProcessedData is emitted when an operation completes
// successfully, LOSS and OPENING included. External fields are omitted for
// internal operations.
type WagerTransactionProcessedData struct {
	TransactionID          uuid.UUID   `json:"transactionId"`
	WalletID               uuid.UUID   `json:"walletId"`
	Origin                 string      `json:"origin"`
	Kind                   string      `json:"kind"`
	ProviderID             string      `json:"providerId,omitempty"`
	ExternalTransactionID  string      `json:"externalTransactionId,omitempty"`
	RoundID                string      `json:"roundId,omitempty"`
	GameID                 string      `json:"gameId,omitempty"`
	ReferenceTransactionID *uuid.UUID  `json:"referenceTransactionId,omitempty"`
	Money                  money.Money `json:"money"`
	Balance                money.Money `json:"balance"`
	WalletVersion          int64       `json:"walletVersion"`
}

// WagerTransactionRejectedData is emitted on a definitive business
// rejection.
type WagerTransactionRejectedData struct {
	TransactionID         uuid.UUID   `json:"transactionId"`
	WalletID              uuid.UUID   `json:"walletId"`
	Kind                  string      `json:"kind"`
	ProviderID            string      `json:"providerId"`
	ExternalTransactionID string      `json:"externalTransactionId"`
	FailureCode           string      `json:"failureCode"`
	Money                 money.Money `json:"money"`
	Balance               money.Money `json:"balance"`
	WalletVersion         int64       `json:"walletVersion"`
}

// WalletBalanceChangedData is emitted for every effective balance change.
type WalletBalanceChangedData struct {
	WalletID      uuid.UUID   `json:"walletId"`
	TransactionID uuid.UUID   `json:"transactionId"`
	Direction     string      `json:"direction"`
	Money         money.Money `json:"money"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	WalletVersion int64       `json:"walletVersion"`
}

// WagerTransactionPendingReferenceData is emitted once, when an operation
// starts waiting for its reference.
type WagerTransactionPendingReferenceData struct {
	TransactionID                  uuid.UUID   `json:"transactionId"`
	WalletID                       uuid.UUID   `json:"walletId"`
	Kind                           string      `json:"kind"`
	ProviderID                     string      `json:"providerId"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId"`
	ReferenceDeadlineAt            string      `json:"referenceDeadlineAt"`
	Money                          money.Money `json:"money"`
}

// NewWagerTransactionProcessed builds a WagerTransactionProcessed v1.
func NewWagerTransactionProcessed(m Meta, d WagerTransactionProcessedData) (Envelope, error) {
	if d.TransactionID == uuid.Nil || d.WalletID != m.AggregateID || !d.Money.Valid() || !d.Balance.Valid() || d.WalletVersion < 1 {
		return Envelope{}, fmt.Errorf("%w: incomplete WagerTransactionProcessed data", ErrInvalidEvent)
	}
	return build(m, TypeWagerTransactionProcessed, 1, d)
}

// NewWagerTransactionRejected builds a WagerTransactionRejected v1.
func NewWagerTransactionRejected(m Meta, d WagerTransactionRejectedData) (Envelope, error) {
	if d.TransactionID == uuid.Nil || d.WalletID != m.AggregateID || d.FailureCode == "" || !d.Money.Valid() || !d.Balance.Valid() || d.WalletVersion < 1 {
		return Envelope{}, fmt.Errorf("%w: incomplete WagerTransactionRejected data", ErrInvalidEvent)
	}
	return build(m, TypeWagerTransactionRejected, 1, d)
}

// NewWalletBalanceChanged builds a WalletBalanceChanged v1.
func NewWalletBalanceChanged(m Meta, d WalletBalanceChangedData) (Envelope, error) {
	if d.TransactionID == uuid.Nil || d.WalletID != m.AggregateID || d.Direction == "" ||
		!d.Money.Valid() || !d.BalanceBefore.Valid() || !d.BalanceAfter.Valid() || d.WalletVersion < 1 {
		return Envelope{}, fmt.Errorf("%w: incomplete WalletBalanceChanged data", ErrInvalidEvent)
	}
	return build(m, TypeWalletBalanceChanged, 1, d)
}

// NewWagerTransactionPendingReference builds a
// WagerTransactionPendingReference v1.
func NewWagerTransactionPendingReference(m Meta, d WagerTransactionPendingReferenceData) (Envelope, error) {
	if d.TransactionID == uuid.Nil || d.WalletID != m.AggregateID || d.ReferenceExternalTransactionID == "" || d.ReferenceDeadlineAt == "" || !d.Money.Valid() {
		return Envelope{}, fmt.Errorf("%w: incomplete WagerTransactionPendingReference data", ErrInvalidEvent)
	}
	return build(m, TypeWagerTransactionPendingReference, 1, d)
}
