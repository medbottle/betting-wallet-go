//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medbottle/betting-wallet-go/internal/adapters/postgres"
	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/app/wagering"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
	"github.com/medbottle/betting-wallet-go/internal/platform/clock"
	"github.com/medbottle/betting-wallet-go/internal/platform/idgen"
)

// TestAPIReversalInsufficientFunds: CHALLENGE §7. A ROLLBACK that would
// debit more than the balance is rejected, auditable, with its own code,
// REVERSAL_INSUFFICIENT_FUNDS, not a bet's INSUFFICIENT_FUNDS. Nothing
// moves, and the rejection is replayed.
func TestAPIReversalInsufficientFunds(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	w := e.openWallet(t, "100.00")

	if r := e.submit(t, "provider-a", op(w, "provider-a", "WIN", "50.00", "rv-win")); r.Status != http.StatusOK || r.amount("balance") != "150.00" {
		t.Fatalf("win: %d %s", r.Status, r.Raw)
	}
	if r := e.submit(t, "provider-a", op(w, "provider-a", "BET", "120.00", "rv-bet")); r.Status != http.StatusOK || r.amount("balance") != "30.00" {
		t.Fatalf("bet: %d %s", r.Status, r.Raw)
	}
	// Rolling the 50.00 win back would take the balance to -20.00.
	rollback := op(w, "provider-a", "ROLLBACK", "50.00", "rv-rollback", "rv-win")
	r := e.submit(t, "provider-a", rollback)
	if r.Status != http.StatusUnprocessableEntity || r.str("status") != "REJECTED" || r.str("failureCode") != "REVERSAL_INSUFFICIENT_FUNDS" ||
		r.amount("balance") != "30.00" {
		t.Fatalf("rollback: %d %s", r.Status, r.Raw)
	}
	replay := e.submit(t, "provider-a", rollback)
	if replay.Status != http.StatusUnprocessableEntity || replay.Body["idempotentReplay"] != true || replay.str("transactionId") != r.str("transactionId") {
		t.Errorf("replay: %d %s", replay.Status, replay.Raw)
	}
	audit := e.call(t, http.MethodGet, "/providers/provider-a/wagering/transactions/rv-rollback", "provider-a", nil)
	if audit.str("status") != "REJECTED" || audit.str("failureCode") != "REVERSAL_INSUFFICIENT_FUNDS" {
		t.Errorf("audit: %s", audit.Raw)
	}
	if n := e.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id = $1`, r.str("transactionId")); n != 0 {
		t.Errorf("rejected rollback wrote %d ledger entries", n)
	}
	if n := e.count(t, `SELECT count(*) FROM outbox_events WHERE payload->'data'->>'transactionId' = $1 AND event_type = 'WagerTransactionRejected'`,
		r.str("transactionId")); n != 1 {
		t.Errorf("rejection events = %d, want 1", n)
	}
	e.assertConsistent(t, w, "30.00")
}

// TestWorkerResumesCommittedPending: CHALLENGE §6.3 and §13 item 8. The
// synchronous path never commits PENDING (ARCHITECTURE §5), but a PENDING
// row a process committed before dying is due at once, and another
// instance's worker, with its own pool, applies it exactly once. The
// operation then replays its persisted result.
func TestWorkerResumesCommittedPending(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t, withReference(refTTL, testBackoffBase, testBackoffMax))
	w := e.openWallet(t, "100.00")

	// What a process that died after committing the PENDING row leaves.
	body := op(w, "provider-a", "BET", "25.00", "orphan-bet")
	req, err := wager.NewRequest(wager.RawRequest{
		ProviderID: "provider-a", ExternalTransactionID: "orphan-bet", IdempotencyKey: "provider-a:orphan-bet",
		PlayerID: w.PlayerID, WalletID: w.ID, RoundID: "round-1", GameID: "fortune-chimp",
		Kind: "BET", Amount: "25.00", Currency: "BRL",
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := wager.NewExternal(req, e.repo.ids(), "corr-orphan", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.repo.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
		_, err := r.Transactions().InsertPending(ctx, pending)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := e.call(t, http.MethodGet, "/providers/provider-a/wagering/transactions/orphan-bet", "provider-a", nil); r.str("status") != "PENDING" {
		t.Fatalf("before the worker: %s", r.Raw)
	}

	// Another instance: its own pool and service on the same database.
	pool, err := pgxpool.New(ctxT(t), e.repo.db.app.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	reader := postgres.NewReader(pool)
	other := wagering.NewService(postgres.NewTxRunner(pool, 5*time.Second, testStatementTimeout, nil), reader, reader, reader, clock.System{}, idgen.UUIDv7{},
		wagering.Config{ReferenceTTL: refTTL, Backoff: wagering.Backoff{Base: testBackoffBase, Max: testBackoffMax}}, e.logger, nil)
	startWorker(t, "other-instance", other, 2)

	done := e.waitStatus(t, "provider-a", "provider-a", "orphan-bet", "PROCESSED", 15*time.Second)
	if done.str("transactionId") != pending.ID().String() || done.amount("balance") != "75.00" {
		t.Fatalf("resumed: %s", done.Raw)
	}
	replay := e.submit(t, "provider-a", body)
	if replay.Status != http.StatusOK || replay.Body["idempotentReplay"] != true || replay.str("transactionId") != pending.ID().String() ||
		replay.amount("balance") != "75.00" {
		t.Errorf("replay: %d %s", replay.Status, replay.Raw)
	}
	if n := e.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id = $1`, pending.ID()); n != 1 {
		t.Errorf("ledger entries = %d, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM outbox_events WHERE payload->'data'->>'transactionId' = $1`, pending.ID().String()); n != 2 {
		t.Errorf("events = %d, want WagerTransactionProcessed and WalletBalanceChanged", n)
	}
	e.assertConsistent(t, w, "75.00")
}

// cancellingRunner cancels the transaction's context right after the next
// ledger insert, once: the business transaction has written the balance
// and the entry, and its next statement sees the cancellation.
type cancellingRunner struct {
	inner app.TxRunner
	armed *atomic.Bool
}

func (c cancellingRunner) InTx(ctx context.Context, opts app.TxOptions, fn func(ctx context.Context, r app.Repos) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	return c.inner.InTx(ctx, opts, func(ctx context.Context, r app.Repos) error {
		return fn(ctx, cancellingRepos{Repos: r, armed: c.armed, cancel: cancel})
	})
}

type cancellingRepos struct {
	app.Repos
	armed  *atomic.Bool
	cancel context.CancelFunc
}

func (r cancellingRepos) Ledger() app.LedgerRepository {
	return cancellingLedger{LedgerRepository: r.Repos.Ledger(), armed: r.armed, cancel: r.cancel}
}

type cancellingLedger struct {
	app.LedgerRepository
	armed  *atomic.Bool
	cancel context.CancelFunc
}

func (l cancellingLedger) Insert(ctx context.Context, e wallet.LedgerEntry) error {
	err := l.LedgerRepository.Insert(ctx, e)
	if err == nil && l.armed.CompareAndSwap(true, false) {
		l.cancel()
	}
	return err
}

// TestCancelledContextPersistsNothing: CHALLENGE §6, I/O honours
// cancellation. A context cancelled in the middle of the business
// transaction, after the balance and ledger writes, rolls everything back:
// the request is a retryable 503, no row, entry or event is left, and the
// retry applies the operation once. A context cancelled before the call
// persists nothing either.
func TestCancelledContextPersistsNothing(t *testing.T) {
	t.Parallel()
	armed := &atomic.Bool{}
	e := newAPIEnv(t, withRunner(func(repo repoEnv) app.TxRunner {
		return cancellingRunner{inner: repo.runner, armed: armed}
	}))
	w := e.openWallet(t, "100.00")
	unchanged := func() {
		t.Helper()
		if n := e.count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND origin = 'EXTERNAL'`, w.ID); n != 0 {
			t.Fatalf("cancelled work left %d transactions", n)
		}
		if n := e.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, w.ID); n != 1 {
			t.Fatalf("ledger entries = %d, want only the opening", n)
		}
		if n := e.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, w.ID); n != 2 {
			t.Fatalf("outbox events = %d, want only the opening's 2", n)
		}
		if n := e.count(t, `SELECT count(*) FROM wallets WHERE id = $1 AND version = 1 AND balance_minor = 10000`, w.ID); n != 1 {
			t.Fatal("the wallet changed")
		}
	}

	armed.Store(true)
	body := op(w, "provider-a", "BET", "25.00", "cancelled")
	r := e.submit(t, "provider-a", body)
	if r.Status != http.StatusServiceUnavailable || r.Body["error"].(map[string]any)["retryable"] != true {
		t.Fatalf("cancelled mid-transaction: %d %s", r.Status, r.Raw)
	}
	if armed.Load() {
		t.Fatal("the cancellation was not reached")
	}
	unchanged()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.wagering.Submit(ctx, wagering.SubmitCommand{Raw: wager.RawRequest{
		ProviderID: "provider-a", ExternalTransactionID: "cancelled", IdempotencyKey: "provider-a:cancelled",
		PlayerID: w.PlayerID, WalletID: w.ID, RoundID: "round-1", GameID: "fortune-chimp", Kind: "BET", Amount: "25.00", Currency: "BRL",
	}, CorrelationID: "corr-cancelled"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled before the call: %v", err)
	}
	unchanged()

	retry := e.submit(t, "provider-a", body)
	if retry.Status != http.StatusOK || retry.Body["idempotentReplay"] != false || retry.amount("balance") != "75.00" {
		t.Fatalf("retry: %d %s", retry.Status, retry.Raw)
	}
	e.assertConsistent(t, w, "75.00")
}

// TestAPIWinOfReversedBet: a WIN that references a BET already refunded or
// rolled back is rejected with REFERENCE_NOT_PROCESSED and pays nothing
// (ARCHITECTURE §7, §27 L-1); a WIN on a standing BET is still paid.
func TestAPIWinOfReversedBet(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	w := e.openWallet(t, "100.00")
	for _, s := range []struct{ kind, ext, ref string }{
		{"BET", "wr-bet", ""}, {"REFUND", "wr-refund", "wr-bet"},
		{"BET", "wr-bet2", ""}, {"ROLLBACK", "wr-rollback", "wr-bet2"},
		{"BET", "wr-bet3", ""},
	} {
		var body map[string]any
		if s.ref == "" {
			body = op(w, "provider-a", s.kind, "25.00", s.ext)
		} else {
			body = op(w, "provider-a", s.kind, "25.00", s.ext, s.ref)
		}
		if r := e.submit(t, "provider-a", body); r.Status != http.StatusOK {
			t.Fatalf("%s: %d %s", s.ext, r.Status, r.Raw)
		}
	}
	for _, ref := range []string{"wr-bet", "wr-bet2"} {
		r := e.submit(t, "provider-a", op(w, "provider-a", "WIN", "50.00", "win-of-"+ref, ref))
		if r.Status != http.StatusUnprocessableEntity || r.str("failureCode") != "REFERENCE_NOT_PROCESSED" || r.amount("balance") != "75.00" {
			t.Fatalf("win of reversed %s: %d %s", ref, r.Status, r.Raw)
		}
	}
	if r := e.submit(t, "provider-a", op(w, "provider-a", "WIN", "50.00", "win-of-wr-bet3", "wr-bet3")); r.Status != http.StatusOK || r.amount("balance") != "125.00" {
		t.Fatalf("win of standing bet: %d %s", r.Status, r.Raw)
	}
	e.assertConsistent(t, w, "125.00")
}
