package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/domain/money"
)

// ErrInvalidLedgerEntry means a ledger entry would break its invariants.
var ErrInvalidLedgerEntry = errors.New("wallet: invalid ledger entry")

// Direction is the side of a ledger entry.
type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

// ParseDirection accepts DEBIT or CREDIT.
func ParseDirection(s string) (Direction, error) {
	d := Direction(s)
	if !d.Valid() {
		return "", fmt.Errorf("%w: direction %q", ErrInvalidLedgerEntry, s)
	}
	return d, nil
}

// Valid reports whether d is DEBIT or CREDIT.
func (d Direction) Valid() bool { return d == Debit || d == Credit }

// LedgerEntry is an immutable movement on a wallet. Corrections are new
// entries; an entry is never changed.
type LedgerEntry struct {
	id            uuid.UUID
	walletID      uuid.UUID
	transactionID uuid.UUID
	direction     Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	walletVersion int64
	createdAt     time.Time
}

// LedgerEntryParams carries every field of an entry.
type LedgerEntryParams struct {
	ID            uuid.UUID
	WalletID      uuid.UUID
	TransactionID uuid.UUID
	Direction     Direction
	Amount        money.Money
	BalanceBefore money.Money
	BalanceAfter  money.Money
	// WalletVersion is the wallet version after this entry.
	WalletVersion int64
	CreatedAt     time.Time
}

// NewLedgerEntry validates and builds a new entry, including
// balanceAfter = balanceBefore ± amount for the direction.
func NewLedgerEntry(p LedgerEntryParams) (LedgerEntry, error) {
	if err := validateEntry(p); err != nil {
		return LedgerEntry{}, err
	}
	return LedgerEntry{
		id:            p.ID,
		walletID:      p.WalletID,
		transactionID: p.TransactionID,
		direction:     p.Direction,
		amount:        p.Amount,
		balanceBefore: p.BalanceBefore,
		balanceAfter:  p.BalanceAfter,
		walletVersion: p.WalletVersion,
		createdAt:     p.CreatedAt.UTC(),
	}, nil
}

// RehydrateLedgerEntry restores a persisted entry. It applies the same
// checks as NewLedgerEntry and has no side effects.
func RehydrateLedgerEntry(p LedgerEntryParams) (LedgerEntry, error) {
	return NewLedgerEntry(p)
}

func validateEntry(p LedgerEntryParams) error {
	var errs []error
	if p.ID == uuid.Nil || p.WalletID == uuid.Nil || p.TransactionID == uuid.Nil {
		errs = append(errs, errors.New("ids must be set"))
	}
	if !p.Direction.Valid() {
		errs = append(errs, fmt.Errorf("direction %q", p.Direction))
	}
	if !p.Amount.IsPositive() {
		errs = append(errs, errors.New("amount must be positive"))
	}
	if !p.BalanceBefore.Valid() || p.BalanceBefore.IsNegative() || !p.BalanceAfter.Valid() || p.BalanceAfter.IsNegative() {
		errs = append(errs, errors.New("balances must be valid and non-negative"))
	}
	if p.WalletVersion < 1 {
		errs = append(errs, errors.New("wallet version must be >= 1"))
	}
	if p.CreatedAt.IsZero() {
		errs = append(errs, errors.New("created_at must be set"))
	}
	if len(errs) == 0 {
		var want money.Money
		var err error
		if p.Direction == Credit {
			want, err = p.BalanceBefore.Add(p.Amount)
		} else {
			want, err = p.BalanceBefore.Sub(p.Amount)
		}
		switch {
		case err != nil:
			errs = append(errs, err)
		case !want.Equal(p.BalanceAfter):
			errs = append(errs, fmt.Errorf("balance_after %s != %s %s %s", p.BalanceAfter, p.BalanceBefore, p.Direction, p.Amount))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidLedgerEntry, err)
	}
	return nil
}

// Valid reports whether e was built by NewLedgerEntry or
// RehydrateLedgerEntry. Persistence must refuse an entry that is not valid
// (the zero value).
func (e LedgerEntry) Valid() bool { return validateEntry(e.params()) == nil }

func (e LedgerEntry) params() LedgerEntryParams {
	return LedgerEntryParams{
		ID: e.id, WalletID: e.walletID, TransactionID: e.transactionID, Direction: e.direction,
		Amount: e.amount, BalanceBefore: e.balanceBefore, BalanceAfter: e.balanceAfter,
		WalletVersion: e.walletVersion, CreatedAt: e.createdAt,
	}
}

func (e LedgerEntry) ID() uuid.UUID              { return e.id }
func (e LedgerEntry) WalletID() uuid.UUID        { return e.walletID }
func (e LedgerEntry) TransactionID() uuid.UUID   { return e.transactionID }
func (e LedgerEntry) Direction() Direction       { return e.direction }
func (e LedgerEntry) Amount() money.Money        { return e.amount }
func (e LedgerEntry) BalanceBefore() money.Money { return e.balanceBefore }
func (e LedgerEntry) BalanceAfter() money.Money  { return e.balanceAfter }
func (e LedgerEntry) WalletVersion() int64       { return e.walletVersion }
func (e LedgerEntry) CreatedAt() time.Time       { return e.createdAt }
