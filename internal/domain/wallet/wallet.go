package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/domain/money"
)

// Sentinel errors. Callers classify with errors.Is.
var (
	// ErrInvalidWallet means a wallet would break its invariants, or an
	// operation was called on an uninitialised wallet.
	ErrInvalidWallet = errors.New("wallet: invalid wallet")
	// ErrInvalidAmount means a movement amount that is not positive.
	ErrInvalidAmount = errors.New("wallet: amount must be positive")
	// ErrInsufficientFunds means a debit would make the balance negative.
	ErrInsufficientFunds = errors.New("wallet: insufficient funds")
	// ErrAmountOverflow means a credit would overflow the balance.
	ErrAmountOverflow = errors.New("wallet: amount overflow")
)

// InitialVersion is the version of a newly opened wallet (CHALLENGE §6.2).
const InitialVersion int64 = 1

// Wallet is the financial aggregate root. The balance changes only through
// Debit and Credit, each producing the matching ledger entry and moving the
// version by exactly one.
type Wallet struct {
	id        uuid.UUID
	playerID  uuid.UUID
	balance   money.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// OpenParams opens a wallet. When InitialBalance is positive the opening
// credit entry is produced, which needs OpeningTransactionID and
// OpeningEntryID.
type OpenParams struct {
	ID                   uuid.UUID
	PlayerID             uuid.UUID
	InitialBalance       money.Money
	OpeningTransactionID uuid.UUID
	OpeningEntryID       uuid.UUID
	Now                  time.Time
}

// Open creates a wallet at version 1. A positive initial balance also
// returns its credit entry (0 -> initial, version 1); zero returns none.
func Open(p OpenParams) (*Wallet, *LedgerEntry, error) {
	if p.ID == uuid.Nil || p.PlayerID == uuid.Nil {
		return nil, nil, fmt.Errorf("%w: id and player id must be set", ErrInvalidWallet)
	}
	if !p.InitialBalance.Valid() || p.InitialBalance.IsNegative() {
		return nil, nil, fmt.Errorf("%w: initial balance must be valid and non-negative", ErrInvalidWallet)
	}
	if p.Now.IsZero() {
		return nil, nil, fmt.Errorf("%w: time must be set", ErrInvalidWallet)
	}
	now := p.Now.UTC()
	w := &Wallet{
		id:        p.ID,
		playerID:  p.PlayerID,
		balance:   p.InitialBalance,
		version:   InitialVersion,
		createdAt: now,
		updatedAt: now,
	}
	if p.InitialBalance.IsZero() {
		return w, nil, nil
	}
	zero, err := money.Zero(p.InitialBalance.Currency())
	if err != nil {
		return nil, nil, err
	}
	entry, err := NewLedgerEntry(LedgerEntryParams{
		ID:            p.OpeningEntryID,
		WalletID:      p.ID,
		TransactionID: p.OpeningTransactionID,
		Direction:     Credit,
		Amount:        p.InitialBalance,
		BalanceBefore: zero,
		BalanceAfter:  p.InitialBalance,
		WalletVersion: InitialVersion,
		CreatedAt:     now,
	})
	if err != nil {
		return nil, nil, err
	}
	return w, &entry, nil
}

// Snapshot is the persisted state of a wallet.
type Snapshot struct {
	ID        uuid.UUID
	PlayerID  uuid.UUID
	Balance   money.Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Rehydrate restores a persisted wallet after checking its invariants. It
// changes nothing and produces no entries.
func Rehydrate(s Snapshot) (*Wallet, error) {
	var errs []error
	if s.ID == uuid.Nil || s.PlayerID == uuid.Nil {
		errs = append(errs, errors.New("id and player id must be set"))
	}
	if !s.Balance.Valid() || s.Balance.IsNegative() {
		errs = append(errs, errors.New("balance must be valid and non-negative"))
	}
	if s.Version < InitialVersion {
		errs = append(errs, errors.New("version must be >= 1"))
	}
	if s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || s.UpdatedAt.Before(s.CreatedAt) {
		errs = append(errs, errors.New("timestamps must be set and ordered"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidWallet, err)
	}
	return &Wallet{
		id:        s.ID,
		playerID:  s.PlayerID,
		balance:   s.Balance,
		version:   s.Version,
		createdAt: s.CreatedAt.UTC(),
		updatedAt: s.UpdatedAt.UTC(),
	}, nil
}

// Debit subtracts amount, refusing to go below zero.
func (w *Wallet) Debit(transactionID, entryID uuid.UUID, amount money.Money, now time.Time) (LedgerEntry, error) {
	return w.move(Debit, transactionID, entryID, amount, now)
}

// Credit adds amount, refusing to overflow.
func (w *Wallet) Credit(transactionID, entryID uuid.UUID, amount money.Money, now time.Time) (LedgerEntry, error) {
	return w.move(Credit, transactionID, entryID, amount, now)
}

func (w *Wallet) move(dir Direction, transactionID, entryID uuid.UUID, amount money.Money, now time.Time) (LedgerEntry, error) {
	if err := w.check(); err != nil {
		return LedgerEntry{}, err
	}
	if !amount.Valid() {
		return LedgerEntry{}, fmt.Errorf("%w: uninitialised amount", ErrInvalidAmount)
	}
	if amount.Currency() != w.balance.Currency() {
		return LedgerEntry{}, fmt.Errorf("%w: wallet %s, movement %s", money.ErrCurrencyMismatch, w.balance.Currency(), amount.Currency())
	}
	if !amount.IsPositive() {
		return LedgerEntry{}, fmt.Errorf("%w: %s", ErrInvalidAmount, amount)
	}
	if now.IsZero() {
		return LedgerEntry{}, fmt.Errorf("%w: time must be set", ErrInvalidWallet)
	}

	var after money.Money
	var err error
	if dir == Credit {
		after, err = w.balance.Add(amount)
		if errors.Is(err, money.ErrOverflow) {
			return LedgerEntry{}, fmt.Errorf("%w: %s + %s", ErrAmountOverflow, w.balance, amount)
		}
	} else {
		after, err = w.balance.Sub(amount)
		if err == nil && after.IsNegative() {
			return LedgerEntry{}, fmt.Errorf("%w: balance %s, debit %s", ErrInsufficientFunds, w.balance, amount)
		}
	}
	if err != nil {
		return LedgerEntry{}, err
	}

	entry, err := NewLedgerEntry(LedgerEntryParams{
		ID:            entryID,
		WalletID:      w.id,
		TransactionID: transactionID,
		Direction:     dir,
		Amount:        amount,
		BalanceBefore: w.balance,
		BalanceAfter:  after,
		WalletVersion: w.version + 1,
		CreatedAt:     now,
	})
	if err != nil {
		return LedgerEntry{}, err
	}
	// Commit only after every check passed.
	w.balance = after
	w.version++
	w.updatedAt = now.UTC()
	return entry, nil
}

func (w *Wallet) check() error {
	if w == nil || w.id == uuid.Nil || !w.balance.Valid() {
		return fmt.Errorf("%w: uninitialised wallet", ErrInvalidWallet)
	}
	return nil
}

func (w *Wallet) ID() uuid.UUID            { return w.id }
func (w *Wallet) PlayerID() uuid.UUID      { return w.playerID }
func (w *Wallet) Currency() money.Currency { return w.balance.Currency() }
func (w *Wallet) Balance() money.Money     { return w.balance }
func (w *Wallet) Version() int64           { return w.version }
func (w *Wallet) CreatedAt() time.Time     { return w.createdAt }
func (w *Wallet) UpdatedAt() time.Time     { return w.updatedAt }
func (w *Wallet) Snapshot() Snapshot {
	return Snapshot{ID: w.id, PlayerID: w.playerID, Balance: w.balance, Version: w.version, CreatedAt: w.createdAt, UpdatedAt: w.updatedAt}
}
