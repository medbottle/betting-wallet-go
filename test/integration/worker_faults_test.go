//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
)

// TestWorkerRetriesTerminatedConnection: a transient database failure in
// the middle of a worker iteration rolls back and is retried; it never
// turns a valid pending operation into FAILED (ARCHITECTURE §27, H-1).
// The worker's session is terminated with pg_terminate_backend (SQLSTATE
// 57P01, as in a fast shutdown or failover) while it waits for the wallet
// lock; the REFUND stays PENDING_REFERENCE and is applied once its BET
// arrives.
func TestWorkerRetriesTerminatedConnection(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t, withReference(10*time.Minute, testBackoffBase, testBackoffMax))
	w := e.openWallet(t, "100.00")
	if r := e.submit(t, "provider-a", op(w, "provider-a", "REFUND", "5.00", "kill-refund", "kill-bet")); r.Status != http.StatusAccepted {
		t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}

	release := holdTx(t, e.repo.db.owner, `SELECT 1 FROM wallets WHERE id = $1 FOR NO KEY UPDATE`, w.ID)
	startWorker(t, "reference", e.wagering, 1)

	// wallet_app may terminate its own role's sessions.
	var pid int32
	deadline := time.Now().Add(4 * time.Second)
	for pid == 0 {
		err := e.repo.db.app.QueryRow(ctxT(t), `SELECT pid FROM pg_stat_activity
			WHERE usename = 'wallet_app' AND wait_event_type = 'Lock' AND query LIKE '%FROM wallets WHERE id%'
			LIMIT 1`).Scan(&pid)
		if err != nil && time.Now().After(deadline) {
			t.Fatalf("the worker never waited on the wallet lock: %v", err)
		}
		if pid == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	var terminated bool
	if err := e.repo.db.app.QueryRow(ctxT(t), `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate %d: %v %v", pid, terminated, err)
	}
	release()

	// Retried, still waiting for its reference.
	time.Sleep(500 * time.Millisecond)
	if r := e.call(t, http.MethodGet, "/providers/provider-a/wagering/transactions/kill-refund", "provider-a", nil); r.str("status") != "PENDING_REFERENCE" {
		t.Fatalf("a terminated connection changed the operation: %s", r.Raw)
	}
	if bet := e.submit(t, "provider-a", op(w, "provider-a", "BET", "5.00", "kill-bet")); bet.Status != http.StatusOK {
		t.Fatalf("bet: %d %s", bet.Status, bet.Raw)
	}
	e.waitStatus(t, "provider-a", "provider-a", "kill-refund", "PROCESSED", 5*time.Second)
	e.assertConsistent(t, w, "100.00")
}

// TestWorkerRetriesUnclassifiedError: an error the adapter does not
// classify (here an injected XX000) is retried like a transient one, as in
// Submit (A6); only an invariant violation is recorded as FAILED
// (ARCHITECTURE §27, H-1).
func TestWorkerRetriesUnclassifiedError(t *testing.T) {
	t.Parallel()
	armed := &atomic.Bool{}
	e := newAPIEnv(t, withReference(10*time.Minute, testBackoffBase, testBackoffMax),
		withRunner(func(repo repoEnv) app.TxRunner { return lockFaultRunner{inner: repo.runner, armed: armed} }))
	w := e.openWallet(t, "100.00")
	if r := e.submit(t, "provider-a", op(w, "provider-a", "REFUND", "5.00", "xx-refund", "xx-bet")); r.Status != http.StatusAccepted {
		t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}
	armed.Store(true)
	startWorker(t, "reference", e.wagering, 1)
	deadline := time.Now().Add(5 * time.Second)
	for armed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the worker never hit the injected fault")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if r := e.call(t, http.MethodGet, "/providers/provider-a/wagering/transactions/xx-refund", "provider-a", nil); r.str("status") != "PENDING_REFERENCE" {
		t.Fatalf("an unclassified error changed the operation: %s", r.Raw)
	}
	if bet := e.submit(t, "provider-a", op(w, "provider-a", "BET", "5.00", "xx-bet")); bet.Status != http.StatusOK {
		t.Fatalf("bet: %d %s", bet.Status, bet.Raw)
	}
	e.waitStatus(t, "provider-a", "provider-a", "xx-refund", "PROCESSED", 5*time.Second)
	e.assertConsistent(t, w, "100.00")
}

// lockFaultRunner makes the next wallet lock fail with an error the
// adapter leaves unclassified.
type lockFaultRunner struct {
	inner app.TxRunner
	armed *atomic.Bool
}

func (f lockFaultRunner) InTx(ctx context.Context, opts app.TxOptions, fn func(ctx context.Context, r app.Repos) error) error {
	return f.inner.InTx(ctx, opts, func(ctx context.Context, r app.Repos) error {
		return fn(ctx, lockFaultRepos{Repos: r, armed: f.armed})
	})
}

type lockFaultRepos struct {
	app.Repos
	armed *atomic.Bool
}

func (r lockFaultRepos) Wallets() app.WalletRepository {
	return lockFaultWallets{WalletRepository: r.Repos.Wallets(), armed: r.armed}
}

type lockFaultWallets struct {
	app.WalletRepository
	armed *atomic.Bool
}

func (w lockFaultWallets) GetForUpdate(ctx context.Context, walletID uuid.UUID) (*wallet.Wallet, error) {
	if w.armed.CompareAndSwap(true, false) {
		return nil, fmt.Errorf("injected: %w", &pgconn.PgError{Code: "XX000", Message: "internal error"})
	}
	return w.WalletRepository.GetForUpdate(ctx, walletID)
}
