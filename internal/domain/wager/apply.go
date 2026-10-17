package wager

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/domain/event"
	"github.com/medbottle/betting-wallet-go/internal/domain/money"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
)

// ReferenceState is what the caller found for the operation's reference,
// looked up by (providerId, referenceExternalTransactionId) after the
// wallet lock was taken (ARCHITECTURE §4).
type ReferenceState struct {
	// Transaction is the referenced operation, or nil if it has not arrived.
	Transaction *WagerTransaction
	// Reversed reports whether the referenced operation already has a
	// PROCESSED REFUND or ROLLBACK.
	Reversed bool
}

// ApplyParams are the inputs of Apply.
type ApplyParams struct {
	Transaction *WagerTransaction
	// Wallet is the transaction's wallet, loaded under lock.
	Wallet    *wallet.Wallet
	Reference ReferenceState
	// ReferenceTTL sets the deadline (created_at + TTL) the first time the
	// operation waits for its reference.
	ReferenceTTL time.Duration
	// NextAttemptAt is when to retry if the reference is still unavailable;
	// the caller computes it (backoff with jitter, ARCHITECTURE §8).
	NextAttemptAt time.Time
	CausationID   *string
	Now           time.Time
	// NewID supplies ledger entry and event ids (UUIDv7 in production).
	NewID func() uuid.UUID
}

// Outcome is the result of Apply. Business rejections are outcomes, not
// errors: they are persisted and replayed.
type Outcome struct {
	Status      Status
	FailureCode FailureCode
	// Entry is the ledger entry to insert, if money moved.
	Entry *wallet.LedgerEntry
	// Events are the outbox records to insert in the same transaction.
	Events []event.Envelope
}

// Apply evaluates a PENDING or PENDING_REFERENCE external operation against
// its wallet and reference, implementing CHALLENGE §7 and ARCHITECTURE §7:
//
//   - a missing or still pending reference waits (PENDING_REFERENCE, with
//     WagerTransactionPendingReference emitted only on entry) until the
//     deadline, then is rejected with REFERENCE_NOT_FOUND or
//     REFERENCE_UNRESOLVED_TIMEOUT; a reference that became resolvable is
//     applied even after the deadline;
//   - a resolved reference is checked in this order: status, kind,
//     provider/player/wallet/currency/round, amount, already reversed (a
//     WIN on a reversed BET is REFERENCE_NOT_PROCESSED); then funds;
//   - BET debits; WIN and REFUND credit; LOSS moves nothing; ROLLBACK
//     credits a BET back and debits a WIN or REFUND back.
//
// On success the transaction and wallet are updated in memory. An error
// (category A mismatch, invalid state, broken invariant) means nothing
// must be persisted; the caller discards both and rolls back.
func Apply(p ApplyParams) (Outcome, error) {
	t, w := p.Transaction, p.Wallet
	if t == nil || w == nil || p.NewID == nil || p.Now.IsZero() {
		return Outcome{}, invalidTx("apply needs a transaction, a wallet, an id source and the time")
	}
	if t.origin != OriginExternal {
		return Outcome{}, invalidTx("apply handles external operations only")
	}
	if t.status != StatusPending && t.status != StatusPendingReference {
		return Outcome{}, fmt.Errorf("%w: cannot apply a %s transaction", ErrInvalidTransition, t.status)
	}
	if w.ID() != t.walletID {
		return Outcome{}, invalidTx("wallet %s loaded for a transaction on %s", w.ID(), t.walletID)
	}
	if w.PlayerID() != t.playerID {
		return Outcome{}, invalid(CodePlayerWalletMismatch, "playerId", "player does not own the wallet")
	}
	if w.Currency() != t.money.Currency() {
		return Outcome{}, invalid(CodeCurrencyMismatch, "money.currency", "wallet is %s", w.Currency())
	}
	now := p.Now.UTC()
	f := eventFactory{tx: t, newID: p.NewID, causation: p.CausationID, now: now}

	var ref *WagerTransaction
	if t.referenceExternalID != nil {
		ref = p.Reference.Transaction
		if ref != nil && ref.externalTransactionID != *t.referenceExternalID {
			return Outcome{}, invalidTx("reference lookup returned %q for %q", ref.externalTransactionID, *t.referenceExternalID)
		}
		if ref == nil || !ref.status.Terminal() {
			return waitOrExpire(p, t, ref, f, now)
		}
		if err := t.resolveReference(ref.id); err != nil {
			return Outcome{}, err
		}
		if code := checkReference(t, ref, p.Reference.Reversed); code != "" {
			return reject(t, w, code, f, now)
		}
	}

	entry, code, err := move(t, w, ref, p.NewID(), now)
	if err != nil {
		return Outcome{}, err
	}
	if code != "" {
		return reject(t, w, code, f, now)
	}
	if err := t.MarkProcessed(Result{Balance: w.Balance(), WalletVersion: w.Version()}, now); err != nil {
		return Outcome{}, err
	}
	events, err := f.completedEvents(entry)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Status: t.status, Entry: entry, Events: events}, nil
}

// checkReference returns the rejection code for a terminal reference, or ""
// when it may be applied.
func checkReference(t, ref *WagerTransaction, reversed bool) FailureCode {
	if ref.status != StatusProcessed {
		return CodeReferenceNotProcessed
	}
	if !referenceKindAllowed(t.kind, ref) {
		return CodeReferenceKindNotAllowed
	}
	// CHALLENGE §7: provider, player, wallet, currency and round must all
	// agree. Player and currency also follow from the wallet in the schema;
	// they are compared here so the rule holds for in-memory values too.
	if ref.providerID != t.providerID || ref.playerID != t.playerID || ref.walletID != t.walletID ||
		ref.money.Currency() != t.money.Currency() || ref.roundID != t.roundID {
		return CodeReferenceMismatch
	}
	if t.kind.IsReversal() {
		if !t.money.Equal(ref.money) {
			return CodeReferenceAmountMismatch
		}
		if reversed {
			return CodeAlreadyReversed
		}
	}
	// A WIN pays out on a BET that still stands: a refunded or rolled-back
	// BET no longer counts as processed (ARCHITECTURE §7, §27 L-1).
	if t.kind == KindWin && reversed {
		return CodeReferenceNotProcessed
	}
	return ""
}

// referenceKindAllowed: WIN and REFUND reference a BET; ROLLBACK references
// a BET, WIN or REFUND. Internal operations are never valid references.
func referenceKindAllowed(k Kind, ref *WagerTransaction) bool {
	if ref.origin != OriginExternal {
		return false
	}
	switch k {
	case KindWin, KindRefund:
		return ref.kind == KindBet
	case KindRollback:
		return ref.kind == KindBet || ref.kind == KindWin || ref.kind == KindRefund
	}
	return false
}

// move applies the money movement of t. A non-empty code is a business
// rejection; an error is an invariant problem.
func move(t *WagerTransaction, w *wallet.Wallet, ref *WagerTransaction, entryID uuid.UUID, now time.Time) (*wallet.LedgerEntry, FailureCode, error) {
	var (
		entry wallet.LedgerEntry
		err   error
	)
	switch {
	case t.kind == KindLoss:
		return nil, "", nil
	case t.kind == KindBet:
		entry, err = w.Debit(t.id, entryID, t.money, now)
		if errors.Is(err, wallet.ErrInsufficientFunds) {
			return nil, CodeInsufficientFunds, nil
		}
	case t.kind == KindRollback && ref.kind != KindBet:
		entry, err = w.Debit(t.id, entryID, t.money, now)
		if errors.Is(err, wallet.ErrInsufficientFunds) {
			return nil, CodeReversalInsufficientFunds, nil
		}
	case t.kind == KindWin, t.kind == KindRefund, t.kind == KindRollback:
		entry, err = w.Credit(t.id, entryID, t.money, now)
		if errors.Is(err, wallet.ErrAmountOverflow) {
			return nil, CodeAmountOverflow, nil
		}
	default:
		return nil, "", invalidTx("no movement rule for %s", t.kind)
	}
	if err != nil {
		return nil, "", err
	}
	return &entry, "", nil
}

func waitOrExpire(p ApplyParams, t, ref *WagerTransaction, f eventFactory, now time.Time) (Outcome, error) {
	deadline, ok := t.ReferenceDeadline()
	if !ok {
		if p.ReferenceTTL <= 0 {
			return Outcome{}, invalidTx("reference TTL must be positive")
		}
		deadline = t.createdAt.Add(p.ReferenceTTL)
	}
	if !now.Before(deadline) {
		code := CodeReferenceNotFound
		if ref != nil {
			code = CodeReferenceUnresolvedTimeout
		}
		return reject(t, p.Wallet, code, f, now)
	}
	if p.NextAttemptAt.IsZero() {
		return Outcome{}, invalidTx("next attempt must be set while waiting for a reference")
	}
	if t.status == StatusPendingReference {
		if err := t.Reschedule(p.NextAttemptAt, now); err != nil {
			return Outcome{}, err
		}
		return Outcome{Status: t.status}, nil
	}
	if err := t.AwaitReference(deadline, p.NextAttemptAt, now); err != nil {
		return Outcome{}, err
	}
	e, err := f.pendingReference()
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Status: t.status, Events: []event.Envelope{e}}, nil
}

func reject(t *WagerTransaction, w *wallet.Wallet, code FailureCode, f eventFactory, now time.Time) (Outcome, error) {
	if err := t.Reject(code, Result{Balance: w.Balance(), WalletVersion: w.Version()}, now); err != nil {
		return Outcome{}, err
	}
	events, err := f.completedEvents(nil)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Status: t.status, FailureCode: code, Events: events}, nil
}

// OpenWalletParams opens a wallet (POST /wallets).
type OpenWalletParams struct {
	WalletID       uuid.UUID
	PlayerID       uuid.UUID
	InitialBalance money.Money
	CorrelationID  string
	CausationID    *string
	Now            time.Time
	// NewID supplies the OPENING transaction, ledger entry and event ids.
	NewID func() uuid.UUID
}

// OpenWalletResult holds everything to persist in the wallet's commit.
type OpenWalletResult struct {
	Wallet *wallet.Wallet
	// Opening, Entry and Events are set only for a positive initial
	// balance: a PROCESSED OPENING, its credit entry, and
	// WagerTransactionProcessed plus WalletBalanceChanged.
	Opening *WagerTransaction
	Entry   *wallet.LedgerEntry
	Events  []event.Envelope
}

// OpenWallet opens a wallet at version 1 (CHALLENGE §9). A zero initial
// balance creates no OPENING, no ledger entry and no events.
func OpenWallet(p OpenWalletParams) (OpenWalletResult, error) {
	if p.NewID == nil || p.CorrelationID == "" {
		return OpenWalletResult{}, invalidTx("open wallet needs an id source and a correlation id")
	}
	if !p.InitialBalance.Valid() {
		return OpenWalletResult{}, invalid(CodeInvalidMoney, "initialBalance", "is required")
	}
	if p.InitialBalance.IsNegative() {
		return OpenWalletResult{}, invalid(CodeInvalidMoney, "initialBalance", "must not be negative")
	}
	if p.InitialBalance.IsZero() {
		w, _, err := wallet.Open(wallet.OpenParams{ID: p.WalletID, PlayerID: p.PlayerID, InitialBalance: p.InitialBalance, Now: p.Now})
		if err != nil {
			return OpenWalletResult{}, err
		}
		return OpenWalletResult{Wallet: w}, nil
	}

	txID, entryID := p.NewID(), p.NewID()
	w, entry, err := wallet.Open(wallet.OpenParams{
		ID: p.WalletID, PlayerID: p.PlayerID, InitialBalance: p.InitialBalance,
		OpeningTransactionID: txID, OpeningEntryID: entryID, Now: p.Now,
	})
	if err != nil {
		return OpenWalletResult{}, err
	}
	opening, err := newOpening(txID, p.WalletID, p.PlayerID, p.InitialBalance, p.CorrelationID, p.Now)
	if err != nil {
		return OpenWalletResult{}, err
	}
	if err := opening.MarkProcessed(Result{Balance: w.Balance(), WalletVersion: w.Version()}, p.Now); err != nil {
		return OpenWalletResult{}, err
	}
	f := eventFactory{tx: opening, newID: p.NewID, causation: p.CausationID, now: p.Now.UTC()}
	events, err := f.completedEvents(entry)
	if err != nil {
		return OpenWalletResult{}, err
	}
	return OpenWalletResult{Wallet: w, Opening: opening, Entry: entry, Events: events}, nil
}
