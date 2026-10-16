package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/domain/money"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
)

// WalletReader implements app.WalletReader on the pool, outside any
// transaction (ARCHITECTURE §4 step 0).
type WalletReader struct{ pool *pgxpool.Pool }

var _ app.WalletReader = (*WalletReader)(nil)

// NewWalletReader returns a reader on pool.
func NewWalletReader(pool *pgxpool.Pool) *WalletReader { return &WalletReader{pool: pool} }

// GetIdentity implements app.WalletReader.
func (r *WalletReader) GetIdentity(ctx context.Context, walletID uuid.UUID) (app.WalletIdentity, error) {
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

type walletRepo struct{ tx pgx.Tx }

func (r walletRepo) Insert(ctx context.Context, w *wallet.Wallet) error {
	_, err := r.tx.Exec(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		w.ID(), w.PlayerID(), w.Currency().String(), w.Balance().Minor(), w.Version(), w.CreatedAt(), w.UpdatedAt())
	return classify(err)
}

// GetForUpdate takes the per-wallet lock with FOR NO KEY UPDATE, not FOR
// UPDATE: writers on the same wallet still queue on it, but it does not
// conflict with the FOR KEY SHARE lock that the foreign key check of a
// wager_transactions insert takes on the wallet row. With FOR UPDATE, two
// operations that each inserted their own row first deadlock on the wallet
// (ARCHITECTURE §4).
func (r walletRepo) GetForUpdate(ctx context.Context, walletID uuid.UUID) (*wallet.Wallet, error) {
	var (
		s        wallet.Snapshot
		code     string
		minor    int64
		created  time.Time
		updated  time.Time
		version  int64
		playerID uuid.UUID
	)
	err := r.tx.QueryRow(ctx, `
		SELECT player_id, currency, balance_minor, version, created_at, updated_at
		FROM wallets WHERE id = $1
		FOR NO KEY UPDATE`, walletID).Scan(&playerID, &code, &minor, &version, &created, &updated)
	if err != nil {
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
	s = wallet.Snapshot{ID: walletID, PlayerID: playerID, Balance: balance, Version: version, CreatedAt: created, UpdatedAt: updated}
	w, err := wallet.Rehydrate(s)
	if err != nil {
		return nil, corrupt(err, "wallet "+walletID.String())
	}
	return w, nil
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
