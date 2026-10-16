package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medbottle/betting-wallet-go/internal/app"
)

// rollbackTimeout bounds a rollback issued after the caller's context is
// already cancelled.
const rollbackTimeout = 5 * time.Second

// TxRunner implements app.TxRunner on a pgx pool.
type TxRunner struct {
	pool             *pgxpool.Pool
	lockTimeout      time.Duration
	statementTimeout time.Duration
	metrics          app.Metrics
}

var _ app.TxRunner = (*TxRunner)(nil)

// NewTxRunner returns a runner whose transactions use lockTimeout and
// statementTimeout unless TxOptions overrides them (ARCHITECTURE §4, §24).
// Rollbacks caused by a concurrency or timeout SQLSTATE are counted on
// metrics (nil discards them).
func NewTxRunner(pool *pgxpool.Pool, lockTimeout, statementTimeout time.Duration, metrics app.Metrics) *TxRunner {
	return &TxRunner{pool: pool, lockTimeout: lockTimeout, statementTimeout: statementTimeout, metrics: app.OrNop(metrics)}
}

// InTx implements app.TxRunner. It does not retry: a transient error rolls
// back and is returned as app.ErrTransient, and the transport retries (HTTP
// 503, SQS redelivery; ARCHITECTURE §5).
func (r *TxRunner) InTx(ctx context.Context, opts app.TxOptions, fn func(ctx context.Context, repos app.Repos) error) (err error) {
	defer func() { r.observe(err) }()
	txOpts := pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}
	if opts.Isolation == app.RepeatableReadReadOnly {
		txOpts = pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	}
	tx, err := r.pool.BeginTx(ctx, txOpts)
	if err != nil {
		return classify(err)
	}

	done := false
	defer func() {
		if done {
			return
		}
		// Error or panic: roll back even if ctx is already cancelled.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_ = tx.Rollback(rctx)
		if p := recover(); p != nil {
			panic(p)
		}
	}()

	lockTimeout := opts.LockTimeout
	if lockTimeout <= 0 {
		lockTimeout = r.lockTimeout
	}
	if err := setLocal(ctx, tx, "lock_timeout", lockTimeout); err != nil {
		return err
	}
	statementTimeout := opts.StatementTimeout
	if statementTimeout <= 0 {
		statementTimeout = r.statementTimeout
	}
	if err := setLocal(ctx, tx, "statement_timeout", statementTimeout); err != nil {
		return err
	}

	if err := fn(ctx, repos{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		// pgx closes the transaction on a failed commit; the outcome of a
		// commit lost with the connection is unknown, which idempotency
		// makes safe to retry.
		done = true
		return classify(err)
	}
	done = true
	return nil
}

// observe counts a transaction lost to a concurrency or timeout SQLSTATE.
func (r *TxRunner) observe(err error) {
	var pgErr *pgconn.PgError
	if err != nil && errors.As(err, &pgErr) && transientCodes[pgErr.Code] {
		r.metrics.DBConflict(pgErr.Code)
	}
}

// setLocal applies a timeout for the current transaction. SET does not take
// bind parameters, so the value is formatted as integer milliseconds.
func setLocal(ctx context.Context, tx pgx.Tx, name string, d time.Duration) error {
	if d < time.Millisecond {
		return fmt.Errorf("postgres: %s must be at least 1ms, got %s", name, d)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL "+name+" = "+strconv.FormatInt(d.Milliseconds(), 10)); err != nil {
		return classify(err)
	}
	return nil
}

// repos binds every repository to one transaction.
type repos struct{ tx pgx.Tx }

func (r repos) Wallets() app.WalletRepository           { return walletRepo{r.tx} }
func (r repos) Transactions() app.TransactionRepository { return transactionRepo{r.tx} }
func (r repos) Ledger() app.LedgerRepository            { return ledgerRepo{r.tx} }
func (r repos) Inbox() app.InboxRepository              { return inboxRepo{r.tx} }
func (r repos) Outbox() app.OutboxRepository            { return outboxRepo{r.tx} }
