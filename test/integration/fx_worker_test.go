//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/goleak"

	"github.com/medbottle/betting-wallet-go/internal/adapters/postgres"
	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/bootstrap"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
	"github.com/medbottle/betting-wallet-go/internal/platform/idgen"
)

// TestFxReferenceWorkerResumesAfterRestart: CHALLENGE §13 item 8 for
// pending references. Instance A starts with the worker and is stopped
// while an operation waits for its reference; the reference arrives while
// no instance runs; instance B resumes and resolves it. Both stops leave no
// goroutines behind.
func TestFxReferenceWorkerResumesAfterRestart(t *testing.T) {
	urls := startPostgres(t)
	migrateUp(t, urls.owner)
	setAppEnv(t, urls.app, startKeycloak(t))
	t.Setenv("RUN_REFERENCE_WORKER", "true")
	t.Setenv("REFERENCE_WORKER_POLL_INTERVAL", "100ms")
	t.Setenv("REFERENCE_BACKOFF_BASE", "50ms")
	t.Setenv("REFERENCE_BACKOFF_MAX", "200ms")

	// Setup and inspection go through the adapter on their own pool.
	pool, err := pgxpool.New(ctxT(t), urls.app)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	runner := postgres.NewTxRunner(pool, 5*time.Second)
	ids := idgen.UUIDv7{}.NewID
	res, err := wager.OpenWallet(wager.OpenWalletParams{
		WalletID: ids(), PlayerID: ids(), InitialBalance: brlAmount(t, "100.00"), CorrelationID: "c", Now: now(), NewID: ids,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error { return persistOpening(ctx, r, res) }); err != nil {
		t.Fatal(err)
	}
	w := res.Wallet

	leaks := goleak.IgnoreCurrent()
	startStop := func(during func()) {
		t.Helper()
		a := fx.New(bootstrap.Options())
		if err := a.Err(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := a.Start(ctx); err != nil {
			t.Fatalf("start: %v", err)
		}
		during()
		if err := a.Stop(ctx); err != nil {
			t.Fatalf("stop: %v", err)
		}
	}
	status := func() wager.Status {
		var s wager.Status
		_ = runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			got, err := r.Transactions().FindByExternalID(ctx, "provider-a", "restart-refund")
			if err == nil {
				s = got.Status()
			}
			return err
		})
		return s
	}

	// Instance A: the refund arrives, waits, and is retried; A stops.
	startStop(func() {
		if err := runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			_, _, err := submit(ctx, r, ids, rawFor(w, "REFUND", "10.00", "restart-refund", "restart-bet"), now())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)
		if s := status(); s != wager.StatusPendingReference {
			t.Fatalf("refund %s while its bet is missing", s)
		}
	})

	// No instance running: the bet commits.
	if err := runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
		_, _, err := submit(ctx, r, ids, rawFor(w, "BET", "10.00", "restart-bet", ""), now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if s := status(); s != wager.StatusPendingReference {
		t.Fatalf("refund %s before any worker ran", s)
	}

	// Instance B resumes the durable pending operation.
	startStop(func() {
		waitFor(t, 10*time.Second, "refund resolved by the restarted instance", func() bool {
			return status() == wager.StatusProcessed
		})
	})
	goleak.VerifyNone(t, leaks)

	var stored int64
	if err := pool.QueryRow(ctxT(t), `SELECT balance_minor FROM wallets WHERE id = $1`, w.ID()).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	err = runner.InTx(ctxT(t), app.TxOptions{Isolation: app.RepeatableReadReadOnly}, func(ctx context.Context, r app.Repos) error {
		fold, err := r.Ledger().Fold(ctx, w.ID(), w.Currency())
		if err != nil {
			return err
		}
		if fold.Balance.Minor() != stored || stored != 10000 {
			t.Errorf("stored %d, ledger %s; want 100.00 for both", stored, fold.Balance)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
