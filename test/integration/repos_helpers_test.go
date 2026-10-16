//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medbottle/betting-wallet-go/internal/adapters/postgres"
	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/domain/money"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
	"github.com/medbottle/betting-wallet-go/internal/platform/idgen"
)

const refTTL = 10 * time.Minute

// testStatementTimeout is the production default (DB_STATEMENT_TIMEOUT).
const testStatementTimeout = 10 * time.Second

// repoEnv is a migrated database with the adapter wired as the
// application role.
type repoEnv struct {
	db     schemaDB
	runner *postgres.TxRunner
	reader *postgres.Reader
	ids    func() uuid.UUID
}

func newRepoEnv(t *testing.T) repoEnv {
	t.Helper()
	db := newSchemaDB(t)
	gen := idgen.UUIDv7{}
	return repoEnv{
		db:     db,
		runner: postgres.NewTxRunner(db.app, 5*time.Second, testStatementTimeout),
		reader: postgres.NewReader(db.app),
		ids:    gen.NewID,
	}
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// now is microsecond precision, like timestamptz, so round trips compare
// exactly.
func now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

func brlAmount(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// persistOpening writes everything OpenWallet produced, in FK order.
func persistOpening(ctx context.Context, r app.Repos, res wager.OpenWalletResult) error {
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
}

// openWallet commits a new wallet with balance and returns it.
func (e repoEnv) openWallet(t *testing.T, balance string) *wallet.Wallet {
	t.Helper()
	res, err := wager.OpenWallet(wager.OpenWalletParams{
		WalletID: e.ids(), PlayerID: e.ids(), InitialBalance: brlAmount(t, balance),
		CorrelationID: "corr-open", Now: now(), NewID: e.ids,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
		return persistOpening(ctx, r, res)
	}); err != nil {
		t.Fatalf("open wallet: %v", err)
	}
	return res.Wallet
}

// rawFor builds a valid request on w.
func rawFor(w *wallet.Wallet, kind, amount, externalID string, ref string) wager.RawRequest {
	r := wager.RawRequest{
		ProviderID: "provider-a", ExternalTransactionID: externalID, IdempotencyKey: "provider-a:" + externalID,
		PlayerID: w.PlayerID().String(), WalletID: w.ID().String(), RoundID: "round-1", GameID: "fortune-chimp",
		Kind: kind, Amount: amount, Currency: "BRL",
	}
	if ref != "" {
		r.ReferenceExternalTransactionID = &ref
	}
	return r
}

// submit is the synchronous flow of ARCHITECTURE §4 steps 1-4, used here to
// exercise the repositories together (the real use case comes later):
// insert own row, lock wallet, read reference, apply, persist, nudge.
func submit(ctx context.Context, r app.Repos, ids func() uuid.UUID, raw wager.RawRequest, at time.Time) (*wager.WagerTransaction, wager.Outcome, error) {
	req, err := wager.NewRequest(raw)
	if err != nil {
		return nil, wager.Outcome{}, err
	}
	tx, err := wager.NewExternal(req, ids(), "corr-"+raw.ExternalTransactionID, at)
	if err != nil {
		return nil, wager.Outcome{}, err
	}
	inserted, err := r.Transactions().InsertPending(ctx, tx)
	if err != nil {
		return nil, wager.Outcome{}, err
	}
	if !inserted {
		return nil, wager.Outcome{}, errDuplicate
	}
	w, err := r.Wallets().GetForUpdate(ctx, req.WalletID())
	if err != nil {
		return nil, wager.Outcome{}, err
	}
	var ref wager.ReferenceState
	if refExt, ok := req.ReferenceExternalTransactionID(); ok {
		found, err := r.Transactions().FindByExternalID(ctx, req.ProviderID(), refExt)
		switch {
		case err == nil:
			ref.Transaction = found
			if ref.Reversed, err = r.Transactions().HasProcessedReversal(ctx, found.ID()); err != nil {
				return nil, wager.Outcome{}, err
			}
		case !errors.Is(err, app.ErrNotFound):
			return nil, wager.Outcome{}, err
		}
	}
	version := w.Version()
	out, err := wager.Apply(wager.ApplyParams{
		Transaction: tx, Wallet: w, Reference: ref, ReferenceTTL: refTTL,
		NextAttemptAt: at.Add(time.Hour), Now: at, NewID: ids,
	})
	if err != nil {
		return nil, wager.Outcome{}, err
	}
	if out.Entry != nil {
		if err := r.Wallets().UpdateBalance(ctx, w, version); err != nil {
			return nil, wager.Outcome{}, err
		}
		if err := r.Ledger().Insert(ctx, *out.Entry); err != nil {
			return nil, wager.Outcome{}, err
		}
	}
	if err := r.Transactions().Update(ctx, tx); err != nil {
		return nil, wager.Outcome{}, err
	}
	if err := r.Outbox().Insert(ctx, out.Events...); err != nil {
		return nil, wager.Outcome{}, err
	}
	if tx.Status().Terminal() {
		if _, err := r.Transactions().NudgeDependents(ctx, tx.ProviderID(), tx.ExternalTransactionID()); err != nil {
			return nil, wager.Outcome{}, err
		}
	}
	return tx, out, nil
}

var errDuplicate = errors.New("duplicate submission")

// commitSubmit runs submit in its own transaction.
func (e repoEnv) commitSubmit(t *testing.T, raw wager.RawRequest) (*wager.WagerTransaction, wager.Outcome) {
	t.Helper()
	var (
		tx  *wager.WagerTransaction
		out wager.Outcome
	)
	err := e.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
		var err error
		tx, out, err = submit(ctx, r, e.ids, raw, now())
		return err
	})
	if err != nil {
		t.Fatalf("submit %s: %v", raw.ExternalTransactionID, err)
	}
	return tx, out
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// holdTx opens a raw transaction on pool, runs sql (typically a lock), and
// returns a release function that rolls it back.
func holdTx(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("hold: %v", err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = tx.Rollback(ctx)
		}
	}
	t.Cleanup(release)
	return release
}
