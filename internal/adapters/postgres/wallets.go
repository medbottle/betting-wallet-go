package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/domain/money"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
)

// querier is what both the pool and a transaction offer; shared queries
// run on either.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const walletColumns = `player_id, currency, balance_minor, version, created_at, updated_at`

// getWallet reads one wallet. lock takes the per-wallet lock with FOR NO
// KEY UPDATE, not FOR UPDATE: writers on the same wallet still queue on it,
// but it does not conflict with the FOR KEY SHARE lock that the foreign key
// check of a wager_transactions insert takes on the wallet row. With FOR
// UPDATE, two operations that each inserted their own row first deadlock on
// the wallet (ARCHITECTURE §4).
func getWallet(ctx context.Context, q querier, walletID uuid.UUID, lock bool) (*wallet.Wallet, error) {
	sql := `SELECT ` + walletColumns + ` FROM wallets WHERE id = $1`
	if lock {
		sql += ` FOR NO KEY UPDATE`
	}
	var (
		code             string
		minor, version   int64
		created, updated time.Time
		playerID         uuid.UUID
	)
	if err := q.QueryRow(ctx, sql, walletID).Scan(&playerID, &code, &minor, &version, &created, &updated); err != nil {
		return nil, notFound(err, "wallet "+walletID.String())
	}
	c, err := money.ParseCurrency(code)
	if err != nil {
		return nil, corrupt(err, "wallet currency")
	}
	balance, err := money.FromMinor(minor, c)
	if err != nil {
		return nil, corrupt(err, "wallet balance")
	}
	w, err := wallet.Rehydrate(wallet.Snapshot{ID: walletID, PlayerID: playerID, Balance: balance, Version: version, CreatedAt: created, UpdatedAt: updated})
	if err != nil {
		return nil, corrupt(err, "wallet "+walletID.String())
	}
	return w, nil
}

type walletRepo struct{ tx pgx.Tx }

func (r walletRepo) Insert(ctx context.Context, w *wallet.Wallet) error {
	_, err := r.tx.Exec(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		w.ID(), w.PlayerID(), w.Currency().String(), w.Balance().Minor(), w.Version(), w.CreatedAt(), w.UpdatedAt())
	return classify(err)
}

func (r walletRepo) Get(ctx context.Context, walletID uuid.UUID) (*wallet.Wallet, error) {
	return getWallet(ctx, r.tx, walletID, false)
}

func (r walletRepo) GetForUpdate(ctx context.Context, walletID uuid.UUID) (*wallet.Wallet, error) {
	return getWallet(ctx, r.tx, walletID, true)
}

func (r walletRepo) UpdateBalance(ctx context.Context, w *wallet.Wallet, expectedVersion int64) error {
	tag, err := r.tx.Exec(ctx, `
		UPDATE wallets SET balance_minor = $2, version = $3, updated_at = $4
		WHERE id = $1 AND version = $5`,
		w.ID(), w.Balance().Minor(), w.Version(), w.UpdatedAt(), expectedVersion)
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: wallet %s is not at version %d", app.ErrInvariantViolation, w.ID(), expectedVersion)
	}
	return nil
}
