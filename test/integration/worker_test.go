//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medbottle/betting-wallet-go/internal/adapters/postgres"
	"github.com/medbottle/betting-wallet-go/internal/app/wagering"
	"github.com/medbottle/betting-wallet-go/internal/platform/clock"
	"github.com/medbottle/betting-wallet-go/internal/platform/idgen"
	"github.com/medbottle/betting-wallet-go/internal/platform/worker"
)

// Short settings so the worker tests run in seconds: backoff 50ms..200ms,
// TTL 2s for expiry (ARCHITECTURE §8 test value).
const (
	testBackoffBase = 50 * time.Millisecond
	testBackoffMax  = 200 * time.Millisecond
	testTTL         = 2 * time.Second
)

// startWorker runs a reference worker on svc until the test ends.
func startWorker(t *testing.T, name string, svc *wagering.Service, concurrency int) *worker.Runner {
	t.Helper()
	r := worker.New(worker.Config{Name: name, Concurrency: concurrency, Idle: 50 * time.Millisecond}, svc.ResolveNext,
		slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := r.Stop(ctx); err != nil {
			t.Errorf("stop %s: %v", name, err)
		}
	})
	return r
}

// waitStatus polls the provider read endpoint until the operation reaches
// want, and returns the final response.
func (e apiEnv) waitStatus(t *testing.T, client, provider, externalID, want string, timeout time.Duration) response {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		r := e.call(t, http.MethodGet, "/providers/"+provider+"/wagering/transactions/"+externalID, client, nil)
		if r.str("status") == want {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s stayed %s (want %s): %s", externalID, r.str("status"), want, r.Raw)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// events counts outbox events of a type for one transaction.
func (e apiEnv) events(t *testing.T, eventType, transactionID string) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM outbox_events WHERE event_type = $1 AND payload->'data'->>'transactionId' = $2`, eventType, transactionID)
}

// TestWorkerResolvesLateReference: CHALLENGE §13 item 7, resolution path.
// A REFUND and a WIN arrive before their BET; once the BET is processed the
// worker applies both, each exactly once.
func TestWorkerResolvesLateReference(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t, withReference(10*time.Minute, testBackoffBase, testBackoffMax))
	startWorker(t, "reference", e.wagering, 4)
	w := e.openWallet(t, "100.00")

	refund := e.submit(t, "provider-a", op(w, "provider-a", "REFUND", "25.00", "late-refund", "late-bet"))
	win := e.submit(t, "provider-a", op(w, "provider-a", "WIN", "5.00", "late-win", "late-bet"))
	if refund.Status != http.StatusAccepted || win.Status != http.StatusAccepted {
		t.Fatalf("early operations: %d %d", refund.Status, win.Status)
	}
	// Retried by the worker meanwhile, still waiting.
	time.Sleep(300 * time.Millisecond)
	if r := e.call(t, http.MethodGet, "/providers/provider-a/wagering/transactions/late-refund", "provider-a", nil); r.str("status") != "PENDING_REFERENCE" {
		t.Fatalf("refund resolved before its bet: %s", r.Raw)
	}

	if bet := e.submit(t, "provider-a", op(w, "provider-a", "BET", "25.00", "late-bet")); bet.Status != http.StatusOK {
		t.Fatalf("bet: %d %s", bet.Status, bet.Raw)
	}
	gotRefund := e.waitStatus(t, "provider-a", "provider-a", "late-refund", "PROCESSED", 5*time.Second)
	e.waitStatus(t, "provider-a", "provider-a", "late-win", "PROCESSED", 5*time.Second)
	if gotRefund.str("referenceTransactionId") == "" {
		t.Errorf("resolved reference not recorded: %s", gotRefund.Raw)
	}

	for _, id := range []string{refund.str("transactionId"), win.str("transactionId")} {
		if n := e.events(t, "WagerTransactionPendingReference", id); n != 1 {
			t.Errorf("%s: %d PendingReference events, want 1 (emitted on entry only)", id, n)
		}
		if n := e.events(t, "WagerTransactionProcessed", id); n != 1 {
			t.Errorf("%s: %d Processed events, want 1", id, n)
		}
		if n := e.events(t, "WalletBalanceChanged", id); n != 1 {
			t.Errorf("%s: %d BalanceChanged events, want 1", id, n)
		}
		if n := e.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id = $1`, id); n != 1 {
			t.Errorf("%s: %d ledger entries, want 1", id, n)
		}
	}
	// 100 - 25 (bet) + 25 (refund) + 5 (win)
	e.assertConsistent(t, w, "105.00")
}

// TestWorkerExpiresReference: CHALLENGE §13 item 7, expiry path.
func TestWorkerExpiresReference(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t, withReference(testTTL, testBackoffBase, testBackoffMax))
	startWorker(t, "reference", e.wagering, 4)

	t.Run("reference never arrives", func(t *testing.T) {
		w := e.openWallet(t, "100.00")
		r := e.submit(t, "provider-a", op(w, "provider-a", "REFUND", "10.00", "orphan-refund", "never-bet"))
		got := e.waitStatus(t, "provider-a", "provider-a", "orphan-refund", "REJECTED", testTTL+3*time.Second)
		if got.str("failureCode") != "REFERENCE_NOT_FOUND" || got.amount("balance") != "100.00" {
			t.Fatalf("expired: %s", got.Raw)
		}
		if n := e.events(t, "WagerTransactionRejected", r.str("transactionId")); n != 1 {
			t.Errorf("rejection events = %d, want 1", n)
		}
		replay := e.submit(t, "provider-a", op(w, "provider-a", "REFUND", "10.00", "orphan-refund", "never-bet"))
		if replay.Status != http.StatusUnprocessableEntity || replay.Body["idempotentReplay"] != true || replay.str("failureCode") != "REFERENCE_NOT_FOUND" {
			t.Errorf("replay after expiry: %d %s", replay.Status, replay.Raw)
		}
		e.assertConsistent(t, w, "100.00")
	})

	t.Run("reference exists but is still pending", func(t *testing.T) {
		w := e.openWallet(t, "100.00")
		refund := e.submit(t, "provider-a", op(w, "provider-a", "REFUND", "10.00", "stuck-refund", "missing-bet"))
		e.submit(t, "provider-a", op(w, "provider-a", "ROLLBACK", "10.00", "rollback-of-stuck", "stuck-refund"))
		// Keep the REFUND claimed by "another worker" so it cannot expire
		// first; the ROLLBACK reaches its deadline with the REFUND pending.
		release := holdTx(t, e.repo.db.owner, `SELECT 1 FROM wager_transactions WHERE id = $1 FOR UPDATE`, refund.str("transactionId"))
		got := e.waitStatus(t, "provider-a", "provider-a", "rollback-of-stuck", "REJECTED", testTTL+3*time.Second)
		release()
		if got.str("failureCode") != "REFERENCE_UNRESOLVED_TIMEOUT" {
			t.Fatalf("rollback: %s", got.Raw)
		}
		e.waitStatus(t, "provider-a", "provider-a", "stuck-refund", "REJECTED", testTTL+3*time.Second)
		e.assertConsistent(t, w, "100.00")
	})
}

// TestWorkerContention: two independent workers (separate pools, so
// separate connections) on the same rows never apply an operation twice.
func TestWorkerContention(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t, withReference(10*time.Minute, testBackoffBase, testBackoffMax))

	// A second "instance": its own pool and service on the same database.
	pool, err := pgxpool.New(ctxT(t), e.repo.db.app.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	reader := postgres.NewReader(pool)
	other := wagering.NewService(postgres.NewTxRunner(pool, 5*time.Second, testStatementTimeout), reader, reader, reader, clock.System{}, idgen.UUIDv7{},
		wagering.Config{ReferenceTTL: 10 * time.Minute, Backoff: wagering.Backoff{Base: testBackoffBase, Max: testBackoffMax}}, e.logger)

	const n = 20
	wallets := make([]apiWallet, n)
	refunds := make([]string, n)
	for i := range wallets {
		wallets[i] = e.openWallet(t, "100.00")
		r := e.submit(t, "provider-a", op(wallets[i], "provider-a", "REFUND", "10.00", fmt.Sprintf("c-refund-%d", i), fmt.Sprintf("c-bet-%d", i)))
		refunds[i] = r.str("transactionId")
	}
	startWorker(t, "reference-1", e.wagering, 4)
	startWorker(t, "reference-2", other, 4)
	for i, w := range wallets {
		e.submit(t, "provider-a", op(w, "provider-a", "BET", "10.00", fmt.Sprintf("c-bet-%d", i)))
	}
	for i := range wallets {
		e.waitStatus(t, "provider-a", "provider-a", fmt.Sprintf("c-refund-%d", i), "PROCESSED", 10*time.Second)
	}
	for i, id := range refunds {
		if c := e.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id = $1`, id); c != 1 {
			t.Errorf("refund %d: %d ledger entries", i, c)
		}
		if c := e.events(t, "WagerTransactionProcessed", id); c != 1 {
			t.Errorf("refund %d: %d Processed events", i, c)
		}
		e.assertConsistent(t, wallets[i], "100.00")
	}
}

// TestWorkerRecoversAbandonedClaim: a worker that claimed a row and died
// (its transaction never commits) holds nothing once rolled back; another
// worker then resolves the row. While claimed, the row is skipped, not
// waited for.
func TestWorkerRecoversAbandonedClaim(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t, withReference(10*time.Minute, testBackoffBase, testBackoffMax))
	w := e.openWallet(t, "100.00")
	refund := e.submit(t, "provider-a", op(w, "provider-a", "REFUND", "10.00", "abandoned", "abandoned-bet"))
	e.submit(t, "provider-a", op(w, "provider-a", "BET", "10.00", "abandoned-bet"))

	// The dead worker's claim.
	release := holdTx(t, e.repo.db.owner, `SELECT 1 FROM wager_transactions WHERE id = $1 FOR UPDATE`, refund.str("transactionId"))
	other := e.openWallet(t, "100.00")
	e.submit(t, "provider-a", op(other, "provider-a", "REFUND", "1.00", "other-refund", "other-bet"))
	e.submit(t, "provider-a", op(other, "provider-a", "BET", "1.00", "other-bet"))
	startWorker(t, "reference", e.wagering, 2)

	// The worker is not blocked by the claimed row: it resolves the other.
	e.waitStatus(t, "provider-a", "provider-a", "other-refund", "PROCESSED", 5*time.Second)
	if r := e.call(t, http.MethodGet, "/providers/provider-a/wagering/transactions/abandoned", "provider-a", nil); r.str("status") != "PENDING_REFERENCE" {
		t.Fatalf("claimed row processed while held: %s", r.Raw)
	}
	release() // the dead worker's transaction aborts
	e.waitStatus(t, "provider-a", "provider-a", "abandoned", "PROCESSED", 5*time.Second)
	e.assertConsistent(t, w, "100.00")
	e.assertConsistent(t, other, "100.00")
}
