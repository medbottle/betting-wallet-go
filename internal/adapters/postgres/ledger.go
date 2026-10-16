package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/domain/money"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
)

type ledgerRepo struct{ tx pgx.Tx }

func (r ledgerRepo) Insert(ctx context.Context, e wallet.LedgerEntry) error {
	if !e.Valid() {
		return fmt.Errorf("%w: refusing to insert an uninitialised ledger entry", app.ErrInvariantViolation)
	}
	_, err := r.tx.Exec(ctx, `
		INSERT INTO wallet_ledger_entries (id, wallet_id, currency, transaction_id, direction, amount_minor,
			balance_before_minor, balance_after_minor, wallet_version, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.ID(), e.WalletID(), e.Amount().Currency().String(), e.TransactionID(), string(e.Direction()),
		e.Amount().Minor(), e.BalanceBefore().Minor(), e.BalanceAfter().Minor(), e.WalletVersion(), e.CreatedAt())
	return classify(err)
}

// Fold streams the wallet's entries in version order and sums credits
// minus debits with checked Money arithmetic: no SQL SUM, so no numeric
// conversion and no floating point anywhere.
func (r ledgerRepo) Fold(ctx context.Context, walletID uuid.UUID, currency money.Currency) (app.LedgerFold, error) {
	balance, err := money.Zero(currency)
	if err != nil {
		return app.LedgerFold{}, err
	}
	rows, err := r.tx.Query(ctx, `
		SELECT direction, amount_minor, currency FROM wallet_ledger_entries
		WHERE wallet_id = $1 ORDER BY wallet_version`, walletID)
	if err != nil {
		return app.LedgerFold{}, classify(err)
	}
	defer rows.Close()

	var n int64
	for rows.Next() {
		var (
			direction, code string
			minor           int64
		)
		if err := rows.Scan(&direction, &minor, &code); err != nil {
			return app.LedgerFold{}, classify(err)
		}
		if code != currency.String() {
			return app.LedgerFold{}, corrupt(fmt.Errorf("entry in %s, wallet in %s", code, currency), "ledger currency")
		}
		amount, err := money.FromMinor(minor, currency)
		if err != nil {
			return app.LedgerFold{}, corrupt(err, "ledger amount")
		}
		switch wallet.Direction(direction) {
		case wallet.Credit:
			balance, err = balance.Add(amount)
		case wallet.Debit:
			balance, err = balance.Sub(amount)
		default:
			err = errors.New("unknown direction " + direction)
		}
		if err != nil {
			return app.LedgerFold{}, corrupt(err, "ledger fold")
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return app.LedgerFold{}, classify(err)
	}
	return app.LedgerFold{Balance: balance, Entries: n}, nil
}
