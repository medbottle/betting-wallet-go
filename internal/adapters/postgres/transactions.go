package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/domain/money"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
)

type transactionRepo struct{ tx pgx.Tx }

// txColumns is the column list shared by inserts and selects; txArgs and
// scanTx follow the same order.
const txColumns = `id, origin, kind, status, wallet_id, player_id, currency, amount_minor,
	provider_id, external_transaction_id, idempotency_key, payload_hash, hash_version,
	round_id, game_id, reference_external_transaction_id, reference_transaction_id,
	failure_code, result_balance_minor, result_wallet_version,
	attempts, next_attempt_at, reference_deadline_at,
	correlation_id, created_at, updated_at, completed_at`

const txPlaceholders = `$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17,
	$18, $19, $20, $21, $22, $23, $24, $25, $26, $27`

func txArgs(s wager.Snapshot) []any {
	var hash []byte
	if s.PayloadHash != nil {
		hash = s.PayloadHash[:]
	}
	var failure *string
	if s.FailureCode != nil {
		v := string(*s.FailureCode)
		failure = &v
	}
	var resultBalance *int64
	if s.ResultBalance != nil {
		v := s.ResultBalance.Minor()
		resultBalance = &v
	}
	return []any{
		s.ID, string(s.Origin), string(s.Kind), string(s.Status), s.WalletID, s.PlayerID,
		s.Money.Currency().String(), s.Money.Minor(),
		s.ProviderID, s.ExternalTransactionID, s.IdempotencyKey, hash, s.HashVersion,
		s.RoundID, s.GameID, s.ReferenceExternalTransactionID, s.ReferenceTransactionID,
		failure, resultBalance, s.ResultWalletVersion,
		s.Attempts, s.NextAttemptAt, s.ReferenceDeadlineAt,
		s.CorrelationID, s.CreatedAt, s.UpdatedAt, s.CompletedAt,
	}
}

func scanTx(row pgx.Row) (*wager.WagerTransaction, error) {
	var (
		s                            wager.Snapshot
		origin, kind, status, code   string
		amount                       int64
		hash                         []byte
		failure                      *string
		resultBalance                *int64
		attempts                     int32
		nextAttempt, created, update time.Time
	)
	err := row.Scan(
		&s.ID, &origin, &kind, &status, &s.WalletID, &s.PlayerID, &code, &amount,
		&s.ProviderID, &s.ExternalTransactionID, &s.IdempotencyKey, &hash, &s.HashVersion,
		&s.RoundID, &s.GameID, &s.ReferenceExternalTransactionID, &s.ReferenceTransactionID,
		&failure, &resultBalance, &s.ResultWalletVersion,
		&attempts, &nextAttempt, &s.ReferenceDeadlineAt,
		&s.CorrelationID, &created, &update, &s.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	c, err := money.ParseCurrency(code)
	if err != nil {
		return nil, corrupt(err, "transaction currency")
	}
	if s.Money, err = money.FromMinor(amount, c); err != nil {
		return nil, corrupt(err, "transaction amount")
	}
	if hash != nil {
		if len(hash) != len(wager.PayloadHash{}) {
			return nil, corrupt(errors.New("payload hash length"), "transaction "+s.ID.String())
		}
		var h wager.PayloadHash
		copy(h[:], hash)
		s.PayloadHash = &h
	}
	if failure != nil {
		fc := wager.FailureCode(*failure)
		s.FailureCode = &fc
	}
	if resultBalance != nil {
		rb, err := money.FromMinor(*resultBalance, c)
		if err != nil {
			return nil, corrupt(err, "transaction result balance")
		}
		s.ResultBalance = &rb
	}
	s.Origin, s.Kind, s.Status = wager.Origin(origin), wager.Kind(kind), wager.Status(status)
	s.Attempts, s.NextAttemptAt, s.CreatedAt, s.UpdatedAt = int(attempts), nextAttempt, created, update

	t, err := wager.Rehydrate(s)
	if err != nil {
		return nil, corrupt(err, "transaction "+s.ID.String())
	}
	return t, nil
}

func (r transactionRepo) InsertPending(ctx context.Context, t *wager.WagerTransaction) (bool, error) {
	s := t.Snapshot()
	if s.Status != wager.StatusPending || s.Origin != wager.OriginExternal {
		return false, fmt.Errorf("%w: InsertPending needs a PENDING external transaction, got %s %s", app.ErrInvariantViolation, s.Origin, s.Status)
	}
	var id uuid.UUID
	err := r.tx.QueryRow(ctx, `INSERT INTO wager_transactions (`+txColumns+`) VALUES (`+txPlaceholders+`)
		ON CONFLICT DO NOTHING RETURNING id`, txArgs(s)...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, classify(err)
	}
	return true, nil
}

func (r transactionRepo) Insert(ctx context.Context, t *wager.WagerTransaction) error {
	s := t.Snapshot()
	if s.Origin != wager.OriginInternal {
		return fmt.Errorf("%w: Insert is for the internal OPENING; use InsertPending", app.ErrInvariantViolation)
	}
	_, err := r.tx.Exec(ctx, `INSERT INTO wager_transactions (`+txColumns+`) VALUES (`+txPlaceholders+`)`, txArgs(s)...)
	return classify(err)
}

func (r transactionRepo) FindByIdempotencyKey(ctx context.Context, providerID, key string) (*wager.WagerTransaction, error) {
	t, err := scanTx(r.tx.QueryRow(ctx, `SELECT `+txColumns+` FROM wager_transactions
		WHERE provider_id = $1 AND idempotency_key = $2`, providerID, key))
	if err != nil {
		return nil, notFound(err, "transaction with idempotency key")
	}
	return t, nil
}

func (r transactionRepo) FindByExternalID(ctx context.Context, providerID, externalTransactionID string) (*wager.WagerTransaction, error) {
	t, err := scanTx(r.tx.QueryRow(ctx, `SELECT `+txColumns+` FROM wager_transactions
		WHERE provider_id = $1 AND external_transaction_id = $2`, providerID, externalTransactionID))
	if err != nil {
		return nil, notFound(err, "transaction "+providerID+"/"+externalTransactionID)
	}
	return t, nil
}

func (r transactionRepo) HasProcessedReversal(ctx context.Context, referenceTransactionID uuid.UUID) (bool, error) {
	var exists bool
	err := r.tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM wager_transactions
		WHERE reference_transaction_id = $1 AND status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK'))`,
		referenceTransactionID).Scan(&exists)
	return exists, classify(err)
}

func (r transactionRepo) Update(ctx context.Context, t *wager.WagerTransaction) error {
	a := txArgs(t.Snapshot())
	// Indexes into txArgs: 0 id, 3 status, 16 reference_transaction_id,
	// 17 failure_code, 18 result_balance_minor, 19 result_wallet_version,
	// 20 attempts, 21 next_attempt_at, 22 reference_deadline_at,
	// 25 updated_at, 26 completed_at.
	tag, err := r.tx.Exec(ctx, `
		UPDATE wager_transactions SET
			status = $2, reference_transaction_id = $3, failure_code = $4,
			result_balance_minor = $5, result_wallet_version = $6,
			attempts = $7, next_attempt_at = $8, reference_deadline_at = $9,
			updated_at = $10, completed_at = $11
		WHERE id = $1`,
		a[0], a[3], a[16], a[17], a[18], a[19], a[20], a[21], a[22], a[25], a[26])
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: transaction %s not found for update", app.ErrInvariantViolation, t.ID())
	}
	return nil
}

func (r transactionRepo) NudgeDependents(ctx context.Context, providerID, externalTransactionID string) (int64, error) {
	tag, err := r.tx.Exec(ctx, `
		UPDATE wager_transactions SET next_attempt_at = now()
		WHERE id IN (
			SELECT id FROM wager_transactions
			WHERE provider_id = $1 AND reference_external_transaction_id = $2
				AND status = 'PENDING_REFERENCE'
			FOR UPDATE SKIP LOCKED)`, providerID, externalTransactionID)
	if err != nil {
		return 0, classify(err)
	}
	return tag.RowsAffected(), nil
}
