package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/domain/money"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
)

// Reader runs single statements on the pool, outside any transaction,
// under READ COMMITTED and without locks: the pre-read of ARCHITECTURE §4
// step 0, the provider registry of §11 and the GET endpoints (§20).
type Reader struct{ pool *pgxpool.Pool }

var (
	_ app.WalletReader      = (*Reader)(nil)
	_ app.ProviderReader    = (*Reader)(nil)
	_ app.TransactionReader = (*Reader)(nil)
	_ app.LedgerReader      = (*Reader)(nil)
)

// NewReader returns a reader on pool.
func NewReader(pool *pgxpool.Pool) *Reader { return &Reader{pool: pool} }

// GetIdentity implements app.WalletReader.
func (r *Reader) GetIdentity(ctx context.Context, walletID uuid.UUID) (app.WalletIdentity, error) {
	var (
		playerID uuid.UUID
		code     string
	)
	err := r.pool.QueryRow(ctx, `SELECT player_id, currency FROM wallets WHERE id = $1`, walletID).Scan(&playerID, &code)
	if err != nil {
		return app.WalletIdentity{}, notFound(err, "wallet "+walletID.String())
	}
	c, err := money.ParseCurrency(code)
	if err != nil {
		return app.WalletIdentity{}, corrupt(err, "wallet currency")
	}
	return app.WalletIdentity{PlayerID: playerID, Currency: c}, nil
}

// Get implements app.WalletReader.
func (r *Reader) Get(ctx context.Context, walletID uuid.UUID) (*wallet.Wallet, error) {
	return getWallet(ctx, r.pool, walletID, false)
}

// IsActive implements app.ProviderReader.
func (r *Reader) IsActive(ctx context.Context, providerID string) (bool, error) {
	var active bool
	err := r.pool.QueryRow(ctx, `SELECT active FROM providers WHERE provider_id = $1`, providerID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, classify(err)
	}
	return active, nil
}

// GetByID implements app.TransactionReader.
func (r *Reader) GetByID(ctx context.Context, transactionID uuid.UUID) (*wager.WagerTransaction, error) {
	t, err := scanTx(r.pool.QueryRow(ctx, `SELECT `+txColumns+` FROM wager_transactions WHERE id = $1`, transactionID))
	if err != nil {
		return nil, notFound(err, "transaction "+transactionID.String())
	}
	return t, nil
}

// GetByExternalID implements app.TransactionReader.
func (r *Reader) GetByExternalID(ctx context.Context, providerID, externalTransactionID string) (*wager.WagerTransaction, error) {
	t, err := scanTx(r.pool.QueryRow(ctx, `SELECT `+txColumns+` FROM wager_transactions
		WHERE provider_id = $1 AND external_transaction_id = $2`, providerID, externalTransactionID))
	if err != nil {
		return nil, notFound(err, "transaction "+providerID+"/"+externalTransactionID)
	}
	return t, nil
}

// List implements app.LedgerReader. The order (wallet_version) is stable
// and unique per wallet, so it doubles as the pagination cursor.
func (r *Reader) List(ctx context.Context, walletID uuid.UUID, afterVersion int64, limit int) ([]wallet.LedgerEntry, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, currency, transaction_id, direction, amount_minor, balance_before_minor,
			balance_after_minor, wallet_version, created_at
		FROM wallet_ledger_entries
		WHERE wallet_id = $1 AND wallet_version > $2
		ORDER BY wallet_version
		LIMIT $3`, walletID, afterVersion, limit)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	var out []wallet.LedgerEntry
	for rows.Next() {
		var (
			p                     wallet.LedgerEntryParams
			code, direction       string
			amount, before, after int64
		)
		if err := rows.Scan(&p.ID, &code, &p.TransactionID, &direction, &amount, &before, &after, &p.WalletVersion, &p.CreatedAt); err != nil {
			return nil, classify(err)
		}
		c, err := money.ParseCurrency(code)
		if err != nil {
			return nil, corrupt(err, "ledger currency")
		}
		p.WalletID, p.Direction = walletID, wallet.Direction(direction)
		if p.Amount, err = money.FromMinor(amount, c); err == nil {
			if p.BalanceBefore, err = money.FromMinor(before, c); err == nil {
				p.BalanceAfter, err = money.FromMinor(after, c)
			}
		}
		if err != nil {
			return nil, corrupt(err, "ledger amounts")
		}
		e, err := wallet.RehydrateLedgerEntry(p)
		if err != nil {
			return nil, corrupt(err, "ledger entry "+p.ID.String())
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return out, nil
}
