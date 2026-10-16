// Package wallets holds the wallet use cases: open, read, ledger listing
// and reconciliation (CHALLENGE §9). Permission checks belong to the
// transport, which calls these only for the internal service.
package wallets

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/domain/money"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
)

// Ledger page sizes (ARCHITECTURE §20, Q9): the CHALLENGE example uses 50.
const (
	DefaultLedgerLimit = 50
	MaxLedgerLimit     = 200
)

// Service implements the wallet use cases.
type Service struct {
	tx      app.TxRunner
	wallets app.WalletReader
	ledger  app.LedgerReader
	clock   app.Clock
	ids     app.IDGenerator
	logger  *slog.Logger
	metrics app.Metrics
}

// NewService wires the use cases to their ports. A nil metrics discards
// the measurements.
func NewService(tx app.TxRunner, wallets app.WalletReader, ledger app.LedgerReader, clock app.Clock, ids app.IDGenerator, logger *slog.Logger,
	metrics app.Metrics) *Service {
	return &Service{tx: tx, wallets: wallets, ledger: ledger, clock: clock, ids: ids, logger: logger, metrics: app.OrNop(metrics)}
}

// OpenCommand is POST /wallets, as received.
type OpenCommand struct {
	PlayerID      string
	Amount        string
	Currency      string
	CorrelationID string
}

// Open opens a wallet. A positive initial balance commits the PROCESSED
// OPENING, its credit entry and both events with the wallet (CHALLENGE §9).
// A second wallet for the same player and currency is
// WALLET_ALREADY_EXISTS.
func (s *Service) Open(ctx context.Context, cmd OpenCommand) (*wallet.Wallet, error) {
	playerID, err := wager.ParseID("playerId", cmd.PlayerID)
	if err != nil {
		return nil, err
	}
	initial, err := wager.ParseMoney("initialBalance", cmd.Amount, cmd.Currency)
	if err != nil {
		return nil, err
	}
	res, err := wager.OpenWallet(wager.OpenWalletParams{
		WalletID: s.ids.NewID(), PlayerID: playerID, InitialBalance: initial,
		CorrelationID: cmd.CorrelationID, Now: s.clock.Now(), NewID: s.ids.NewID,
	})
	if err != nil {
		return nil, err
	}
	err = s.tx.InTx(ctx, app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
		if err := r.Wallets().Insert(ctx, res.Wallet); err != nil {
			return err
		}
		if res.Opening != nil {
			if err := r.Transactions().Insert(ctx, res.Opening); err != nil {
				return err
			}
			if err := r.Ledger().Insert(ctx, *res.Entry); err != nil {
				return err
			}
		}
		return r.Outbox().Insert(ctx, res.Events...)
	})
	if errors.Is(err, app.ErrWalletAlreadyExists) {
		s.metrics.Conflict(wager.CodeWalletAlreadyExists)
		return nil, &wager.ConflictError{Code: wager.CodeWalletAlreadyExists, Msg: "the player already has a wallet in this currency"}
	}
	if err != nil {
		return nil, err
	}
	s.logger.Info("wallet opened", slog.String("walletId", res.Wallet.ID().String()), slog.String("correlationId", cmd.CorrelationID))
	return res.Wallet, nil
}

// Get returns the wallet, or WALLET_NOT_FOUND.
func (s *Service) Get(ctx context.Context, walletID uuid.UUID) (*wallet.Wallet, error) {
	w, err := s.wallets.Get(ctx, walletID)
	if errors.Is(err, app.ErrNotFound) {
		return nil, walletNotFound()
	}
	return w, err
}

// LedgerPage is one page of a wallet's ledger in version order.
type LedgerPage struct {
	Entries []wallet.LedgerEntry
	// NextCursor is empty on the last page.
	NextCursor string
}

// Ledger lists entries after the opaque cursor (base64 of the last
// wallet_version seen; empty for the first page). limit 0 means the
// default; above the maximum is INVALID_REQUEST.
func (s *Service) Ledger(ctx context.Context, walletID uuid.UUID, cursor string, limit int) (LedgerPage, error) {
	switch {
	case limit == 0:
		limit = DefaultLedgerLimit
	case limit < 0 || limit > MaxLedgerLimit:
		return LedgerPage{}, &wager.ValidationError{Code: wager.CodeInvalidRequest, Field: "limit", Msg: fmt.Sprintf("must be between 1 and %d", MaxLedgerLimit)}
	}
	after, err := decodeCursor(cursor)
	if err != nil {
		return LedgerPage{}, err
	}
	if _, err := s.wallets.GetIdentity(ctx, walletID); err != nil {
		if errors.Is(err, app.ErrNotFound) {
			return LedgerPage{}, walletNotFound()
		}
		return LedgerPage{}, err
	}
	entries, err := s.ledger.List(ctx, walletID, after, limit+1)
	if err != nil {
		return LedgerPage{}, err
	}
	page := LedgerPage{Entries: entries}
	if len(entries) > limit {
		page.Entries = entries[:limit]
		page.NextCursor = encodeCursor(page.Entries[limit-1].WalletVersion())
	}
	return page, nil
}

func encodeCursor(version int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(version, 10)))
}

func decodeCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err == nil {
		if v, perr := strconv.ParseInt(string(raw), 10, 64); perr == nil && v >= 1 {
			return v, nil
		}
	}
	return 0, &wager.ValidationError{Code: wager.CodeInvalidRequest, Field: "cursor", Msg: "invalid cursor"}
}

// Reconciliation compares the stored balance with the balance rebuilt from
// the ledger (CHALLENGE §9).
type Reconciliation struct {
	WalletID          uuid.UUID
	StoredBalance     money.Money
	CalculatedBalance money.Money
	// Difference is stored minus calculated.
	Difference     money.Money
	Consistent     bool
	CheckedEntries int64
}

// Reconcile reads the wallet and folds its ledger in one REPEATABLE READ
// READ ONLY snapshot (ARCHITECTURE §3). It never changes the balance; a
// divergence is reported in the result and logged.
func (s *Service) Reconcile(ctx context.Context, walletID uuid.UUID, correlationID string) (Reconciliation, error) {
	var rec Reconciliation
	err := s.tx.InTx(ctx, app.TxOptions{Isolation: app.RepeatableReadReadOnly}, func(ctx context.Context, r app.Repos) error {
		w, err := r.Wallets().Get(ctx, walletID)
		if err != nil {
			return err
		}
		fold, err := r.Ledger().Fold(ctx, walletID, w.Currency())
		if err != nil {
			return err
		}
		diff, err := w.Balance().Sub(fold.Balance)
		if err != nil {
			return fmt.Errorf("%w: reconciliation difference: %w", app.ErrInvariantViolation, err)
		}
		rec = Reconciliation{
			WalletID: walletID, StoredBalance: w.Balance(), CalculatedBalance: fold.Balance,
			Difference: diff, Consistent: diff.IsZero(), CheckedEntries: fold.Entries,
		}
		return nil
	})
	if errors.Is(err, app.ErrNotFound) {
		return Reconciliation{}, walletNotFound()
	}
	if err != nil {
		return Reconciliation{}, err
	}
	if !rec.Consistent {
		s.metrics.ReconciliationDivergence()
		s.logger.Error("reconciliation divergence",
			slog.String("walletId", walletID.String()), slog.String("correlationId", correlationID),
			slog.String("storedBalance", rec.StoredBalance.String()), slog.String("calculatedBalance", rec.CalculatedBalance.String()),
			slog.String("difference", rec.Difference.String()), slog.Int64("checkedEntries", rec.CheckedEntries))
	}
	return rec, nil
}

func walletNotFound() error {
	return &wager.ValidationError{Code: wager.CodeWalletNotFound, Field: "walletId", Msg: "wallet not found"}
}
