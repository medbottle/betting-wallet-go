package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/domain/event"
	"github.com/medbottle/betting-wallet-go/internal/domain/money"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
)

// Isolation selects the SQL isolation level of a transaction
// (ARCHITECTURE §3).
type Isolation int

const (
	// ReadCommitted with explicit row locks: the default for business
	// transactions.
	ReadCommitted Isolation = iota
	// RepeatableReadReadOnly gives one snapshot for the whole transaction:
	// used by reconciliation.
	RepeatableReadReadOnly
)

// TxOptions configures one transaction.
type TxOptions struct {
	Isolation Isolation
	// LockTimeout bounds every lock wait (SET LOCAL lock_timeout). Zero
	// uses the runner's default (5s, ARCHITECTURE §4).
	LockTimeout time.Duration
	// StatementTimeout, when positive, is applied with SET LOCAL
	// statement_timeout. Zero leaves the server setting (see
	// docs/OPEN-QUESTIONS.md Q1).
	StatementTimeout time.Duration
}

// TxRunner runs fn in one SQL transaction (unit of work, ARCHITECTURE §3).
// fn returning nil commits; an error or a panic rolls back. Every Repos
// method used inside fn joins that transaction, so the boundary is visible
// at the call site.
type TxRunner interface {
	InTx(ctx context.Context, opts TxOptions, fn func(ctx context.Context, r Repos) error) error
}

// Repos groups the repositories bound to one transaction.
type Repos interface {
	Wallets() WalletRepository
	Transactions() TransactionRepository
	Ledger() LedgerRepository
	Inbox() InboxRepository
	Outbox() OutboxRepository
}

// WalletIdentity is the immutable part of a wallet.
type WalletIdentity struct {
	PlayerID uuid.UUID
	Currency money.Currency
}

// WalletReader serves the non-locking pre-read of ARCHITECTURE §4 step 0,
// done before BEGIN so input errors are returned without persisting
// anything. It is safe because player and currency are immutable and
// wallets are never removed (both enforced by triggers).
type WalletReader interface {
	// GetIdentity returns ErrNotFound for an unknown wallet.
	GetIdentity(ctx context.Context, walletID uuid.UUID) (WalletIdentity, error)
}

// WalletRepository persists the wallet aggregate.
type WalletRepository interface {
	// Insert returns ErrWalletAlreadyExists when the player already has a
	// wallet in that currency.
	Insert(ctx context.Context, w *wallet.Wallet) error
	// GetForUpdate locks the wallet row (SELECT ... FOR UPDATE) for the rest
	// of the transaction. ErrNotFound for an unknown wallet.
	GetForUpdate(ctx context.Context, walletID uuid.UUID) (*wallet.Wallet, error)
	// UpdateBalance writes balance, version and updated_at, conditioned on
	// the version read under lock. Zero rows affected is
	// ErrInvariantViolation (ARCHITECTURE §4).
	UpdateBalance(ctx context.Context, w *wallet.Wallet, expectedVersion int64) error
}

// TransactionRepository persists wager transactions.
type TransactionRepository interface {
	// InsertPending inserts a PENDING external operation with ON CONFLICT
	// DO NOTHING. inserted is false when a row with the same id, (provider,
	// key) or (provider, external id) exists; a concurrent uncommitted
	// duplicate makes the insert wait for it (ARCHITECTURE §6).
	InsertPending(ctx context.Context, t *wager.WagerTransaction) (inserted bool, err error)
	// Insert inserts the internal OPENING, already PROCESSED.
	Insert(ctx context.Context, t *wager.WagerTransaction) error
	// FindByIdempotencyKey and FindByExternalID read without locking.
	// ErrNotFound when absent.
	FindByIdempotencyKey(ctx context.Context, providerID, key string) (*wager.WagerTransaction, error)
	FindByExternalID(ctx context.Context, providerID, externalTransactionID string) (*wager.WagerTransaction, error)
	// HasProcessedReversal reports whether the operation already has a
	// PROCESSED REFUND or ROLLBACK (ARCHITECTURE §7).
	HasProcessedReversal(ctx context.Context, referenceTransactionID uuid.UUID) (bool, error)
	// Update writes the mutable state (status, outcome, scheduling,
	// resolved reference). Exactly one row must change.
	Update(ctx context.Context, t *wager.WagerTransaction) error
	// NudgeDependents moves next_attempt_at to now() for PENDING_REFERENCE
	// operations waiting on (provider, external id), skipping rows another
	// transaction holds (FOR UPDATE SKIP LOCKED, ARCHITECTURE §8).
	NudgeDependents(ctx context.Context, providerID, externalTransactionID string) (int64, error)
}

// LedgerFold is the balance rebuilt from a wallet's ledger.
type LedgerFold struct {
	Balance money.Money
	Entries int64
}

// LedgerRepository persists the append-only ledger.
type LedgerRepository interface {
	// Insert refuses an entry that is not Valid.
	Insert(ctx context.Context, e wallet.LedgerEntry) error
	// Fold sums credits minus debits with checked Money arithmetic.
	Fold(ctx context.Context, walletID uuid.UUID, currency money.Currency) (LedgerFold, error)
}

// InboxMessage is one consumed SQS message (ARCHITECTURE §9).
type InboxMessage struct {
	ConsumerName  string
	MessageID     string
	PayloadHash   wager.PayloadHash
	TransactionID *uuid.UUID
	Outcome       *wager.Status
	ReceivedAt    time.Time
	ProcessedAt   *time.Time
}

// InboxRepository persists the consumer inbox.
type InboxRepository interface {
	// Insert records a received message with ON CONFLICT DO NOTHING;
	// inserted is false for a redelivered messageId.
	Insert(ctx context.Context, m InboxMessage) (inserted bool, err error)
	// Get returns ErrNotFound when absent.
	Get(ctx context.Context, consumerName, messageID string) (InboxMessage, error)
	// Complete marks the message handled. It must not be completed yet.
	Complete(ctx context.Context, consumerName, messageID string, transactionID uuid.UUID, outcome wager.Status, processedAt time.Time) error
}

// OutboxRepository persists integration events for the relay.
type OutboxRepository interface {
	// Insert refuses envelopes that are not Valid.
	Insert(ctx context.Context, events ...event.Envelope) error
}

// Clock supplies the current time (UTC).
type Clock interface {
	Now() time.Time
}

// IDGenerator supplies new identifiers (UUIDv7).
type IDGenerator interface {
	NewID() uuid.UUID
}
