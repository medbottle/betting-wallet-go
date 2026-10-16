//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/domain/event"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
)

func TestTxRunner(t *testing.T) {
	t.Parallel()
	env := newRepoEnv(t)

	newWallet := func(t *testing.T) *wallet.Wallet {
		res, err := wager.OpenWallet(wager.OpenWalletParams{
			WalletID: env.ids(), PlayerID: env.ids(), InitialBalance: brlAmount(t, "0.00"),
			CorrelationID: "c", Now: now(), NewID: env.ids,
		})
		if err != nil {
			t.Fatal(err)
		}
		return res.Wallet
	}
	exists := func(t *testing.T, id uuid.UUID) bool {
		_, err := env.reader.GetIdentity(ctxT(t), id)
		if err != nil && !errors.Is(err, app.ErrNotFound) {
			t.Fatal(err)
		}
		return err == nil
	}

	t.Run("commit", func(t *testing.T) {
		w := newWallet(t)
		if err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			return r.Wallets().Insert(ctx, w)
		}); err != nil {
			t.Fatal(err)
		}
		if !exists(t, w.ID()) {
			t.Fatal("committed wallet not found")
		}
	})

	t.Run("rollback on error", func(t *testing.T) {
		w := newWallet(t)
		boom := errors.New("boom")
		err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			if err := r.Wallets().Insert(ctx, w); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the callback's error unchanged", err)
		}
		if exists(t, w.ID()) {
			t.Fatal("rolled back wallet is visible")
		}
	})

	t.Run("rollback on panic, then re-panic", func(t *testing.T) {
		w := newWallet(t)
		func() {
			defer func() {
				if p := recover(); p != "kaboom" {
					t.Fatalf("recovered %v, want the original panic", p)
				}
			}()
			_ = env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
				if err := r.Wallets().Insert(ctx, w); err != nil {
					return err
				}
				panic("kaboom")
			})
		}()
		if exists(t, w.ID()) {
			t.Fatal("wallet from panicking transaction is visible")
		}
	})

	t.Run("repeatable read read-only refuses writes", func(t *testing.T) {
		err := env.runner.InTx(ctxT(t), app.TxOptions{Isolation: app.RepeatableReadReadOnly}, func(ctx context.Context, r app.Repos) error {
			return r.Wallets().Insert(ctx, newWallet(t))
		})
		if pgCode(err) != "25006" { // read_only_sql_transaction
			t.Fatalf("err = %v, want SQLSTATE 25006", err)
		}
	})

	t.Run("repeatable read sees one snapshot", func(t *testing.T) {
		w := env.openWallet(t, "100.00")
		err := env.runner.InTx(ctxT(t), app.TxOptions{Isolation: app.RepeatableReadReadOnly}, func(ctx context.Context, r app.Repos) error {
			before, err := r.Ledger().Fold(ctx, w.ID(), w.Currency())
			if err != nil {
				return err
			}
			// Another transaction commits a bet in the meantime.
			env.commitSubmit(t, rawFor(w, "BET", "25.00", "snap-bet", ""))
			after, err := r.Ledger().Fold(ctx, w.ID(), w.Currency())
			if err != nil {
				return err
			}
			if after.Entries != before.Entries || !after.Balance.Equal(before.Balance) {
				t.Errorf("snapshot moved: %d/%s then %d/%s", before.Entries, before.Balance, after.Entries, after.Balance)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("cancelled context is transient", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := env.runner.InTx(ctx, app.TxOptions{}, func(context.Context, app.Repos) error { return nil })
		if !errors.Is(err, app.ErrTransient) || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want ErrTransient wrapping context.Canceled", err)
		}
	})
}

func TestWalletRepository(t *testing.T) {
	t.Parallel()
	env := newRepoEnv(t)

	t.Run("insert, pre-read and locked read round trip", func(t *testing.T) {
		w := env.openWallet(t, "1000.00")
		id, err := env.reader.GetIdentity(ctxT(t), w.ID())
		if err != nil || id.PlayerID != w.PlayerID() || id.Currency != w.Currency() {
			t.Fatalf("identity = %+v, %v", id, err)
		}
		err = env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			got, err := r.Wallets().GetForUpdate(ctx, w.ID())
			if err != nil {
				return err
			}
			if got.Snapshot() != w.Snapshot() {
				t.Errorf("round trip\n got %+v\nwant %+v", got.Snapshot(), w.Snapshot())
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unknown wallet", func(t *testing.T) {
		if _, err := env.reader.GetIdentity(ctxT(t), uuid.New()); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("reader: %v", err)
		}
		err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			_, err := r.Wallets().GetForUpdate(ctx, uuid.New())
			return err
		})
		if !errors.Is(err, app.ErrNotFound) {
			t.Errorf("GetForUpdate: %v", err)
		}
	})

	t.Run("second wallet for player and currency", func(t *testing.T) {
		w := env.openWallet(t, "0.00")
		res, err := wager.OpenWallet(wager.OpenWalletParams{
			WalletID: env.ids(), PlayerID: w.PlayerID(), InitialBalance: brlAmount(t, "5.00"),
			CorrelationID: "c", Now: now(), NewID: env.ids,
		})
		if err != nil {
			t.Fatal(err)
		}
		err = env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			return persistOpening(ctx, r, res)
		})
		if !errors.Is(err, app.ErrWalletAlreadyExists) {
			t.Fatalf("err = %v, want ErrWalletAlreadyExists", err)
		}
	})

	t.Run("update conditioned on version", func(t *testing.T) {
		w := env.openWallet(t, "100.00")
		err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			locked, err := r.Wallets().GetForUpdate(ctx, w.ID())
			if err != nil {
				return err
			}
			if _, err := locked.Debit(env.ids(), env.ids(), brlAmount(t, "10.00"), now()); err != nil {
				return err
			}
			// Stale expected version: zero rows, invariant violation.
			if err := r.Wallets().UpdateBalance(ctx, locked, 7); !errors.Is(err, app.ErrInvariantViolation) {
				t.Errorf("stale version: %v", err)
			}
			return r.Wallets().UpdateBalance(ctx, locked, 1)
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestTransactionRepository(t *testing.T) {
	t.Parallel()
	env := newRepoEnv(t)

	t.Run("insert pending is idempotent", func(t *testing.T) {
		w := env.openWallet(t, "100.00")
		raw := rawFor(w, "BET", "25.00", "idem-1", "")
		first, _ := env.commitSubmit(t, raw)

		// Same key and external id under a new internal id: not inserted.
		err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			_, _, err := submit(ctx, r, env.ids, raw, now())
			return err
		})
		if !errors.Is(err, errDuplicate) {
			t.Fatalf("duplicate: %v", err)
		}
		err = env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			byKey, err := r.Transactions().FindByIdempotencyKey(ctx, "provider-a", raw.IdempotencyKey)
			if err != nil {
				return err
			}
			byExt, err := r.Transactions().FindByExternalID(ctx, "provider-a", raw.ExternalTransactionID)
			if err != nil {
				return err
			}
			if byKey.ID() != first.ID() || byExt.ID() != first.ID() {
				t.Errorf("lookups returned %s / %s, want %s", byKey.ID(), byExt.ID(), first.ID())
			}
			_, err = r.Transactions().FindByExternalID(ctx, "provider-b", raw.ExternalTransactionID)
			if !errors.Is(err, app.ErrNotFound) {
				t.Errorf("other provider: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("round trip through the domain", func(t *testing.T) {
		w := env.openWallet(t, "100.00")
		bet, _ := env.commitSubmit(t, rawFor(w, "BET", "25.00", "rt-bet", ""))
		refund, out := env.commitSubmit(t, rawFor(w, "REFUND", "25.00", "rt-refund", "rt-bet"))
		if out.Status != wager.StatusProcessed {
			t.Fatalf("refund %s %s", out.Status, out.FailureCode)
		}
		err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			for _, want := range []*wager.WagerTransaction{bet, refund} {
				got, err := r.Transactions().FindByExternalID(ctx, "provider-a", want.ExternalTransactionID())
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(got.Snapshot(), want.Snapshot()) {
					t.Errorf("round trip of %s\n got %+v\nwant %+v", want.ExternalTransactionID(), got.Snapshot(), want.Snapshot())
				}
			}
			reversed, err := r.Transactions().HasProcessedReversal(ctx, bet.ID())
			if err != nil || !reversed {
				t.Errorf("bet reversed = %v, %v", reversed, err)
			}
			reversed, err = r.Transactions().HasProcessedReversal(ctx, refund.ID())
			if err != nil || reversed {
				t.Errorf("refund reversed = %v, %v", reversed, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("terminal row cannot be rewritten", func(t *testing.T) {
		w := env.openWallet(t, "100.00")
		bet, _ := env.commitSubmit(t, rawFor(w, "BET", "25.00", "frozen-bet", ""))
		err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			return r.Transactions().Update(ctx, bet)
		})
		if !errors.Is(err, app.ErrInvariantViolation) {
			t.Fatalf("err = %v, want ErrInvariantViolation from the trigger", err)
		}
	})

	t.Run("wrong insert method is refused", func(t *testing.T) {
		w := env.openWallet(t, "100.00")
		bet, _ := env.commitSubmit(t, rawFor(w, "BET", "1.00", "wrong-insert", ""))
		err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			if err := r.Transactions().Insert(ctx, bet); !errors.Is(err, app.ErrInvariantViolation) {
				t.Errorf("Insert(external): %v", err)
			}
			if _, err := r.Transactions().InsertPending(ctx, bet); !errors.Is(err, app.ErrInvariantViolation) {
				t.Errorf("InsertPending(processed): %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestPendingReferenceAndNudge(t *testing.T) {
	t.Parallel()
	env := newRepoEnv(t)
	w := env.openWallet(t, "100.00")

	refund, out := env.commitSubmit(t, rawFor(w, "REFUND", "25.00", "late-refund", "late-bet"))
	if out.Status != wager.StatusPendingReference {
		t.Fatalf("refund %s", out.Status)
	}

	t.Run("locked dependent is skipped without blocking", func(t *testing.T) {
		release := holdTx(t, env.db.owner, `SELECT 1 FROM wager_transactions WHERE id = $1 FOR UPDATE`, refund.ID())
		defer release()
		start := time.Now()
		err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			n, err := r.Transactions().NudgeDependents(ctx, "provider-a", "late-bet")
			if n != 0 {
				t.Errorf("nudged %d locked rows", n)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited > time.Second {
			t.Fatalf("nudge waited %v on a locked row", waited)
		}
	})

	t.Run("bet arrives and nudges the refund", func(t *testing.T) {
		env.commitSubmit(t, rawFor(w, "BET", "25.00", "late-bet", ""))
		err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			got, err := r.Transactions().FindByExternalID(ctx, "provider-a", "late-refund")
			if err != nil {
				return err
			}
			if got.Status() != wager.StatusPendingReference {
				t.Errorf("refund status %s", got.Status())
			}
			if got.NextAttemptAt().After(time.Now().Add(time.Minute)) {
				t.Errorf("next attempt %v was not moved forward", got.NextAttemptAt())
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestLedgerInboxOutbox(t *testing.T) {
	t.Parallel()
	env := newRepoEnv(t)

	t.Run("ledger fold matches the stored balance", func(t *testing.T) {
		w := env.openWallet(t, "100.00")
		env.commitSubmit(t, rawFor(w, "BET", "25.00", "fold-bet", ""))
		env.commitSubmit(t, rawFor(w, "WIN", "40.00", "fold-win", ""))
		env.commitSubmit(t, rawFor(w, "LOSS", "0.00", "fold-loss", ""))
		err := env.runner.InTx(ctxT(t), app.TxOptions{Isolation: app.RepeatableReadReadOnly}, func(ctx context.Context, r app.Repos) error {
			fold, err := r.Ledger().Fold(ctx, w.ID(), w.Currency())
			if err != nil {
				return err
			}
			if fold.Balance.String() != "115.00" || fold.Entries != 3 {
				t.Errorf("fold = %s over %d entries, want 115.00 over 3", fold.Balance, fold.Entries)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("uninitialised values are refused before SQL", func(t *testing.T) {
		err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			if err := r.Ledger().Insert(ctx, wallet.LedgerEntry{}); !errors.Is(err, app.ErrInvariantViolation) {
				t.Errorf("ledger: %v", err)
			}
			if err := r.Outbox().Insert(ctx, event.Envelope{}); !errors.Is(err, app.ErrInvariantViolation) {
				t.Errorf("outbox: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("outbox keeps the payload content", func(t *testing.T) {
		w := env.openWallet(t, "100.00")
		_, out := env.commitSubmit(t, rawFor(w, "BET", "25.00", "outbox-bet", ""))
		for _, e := range out.Events {
			var stored []byte
			err := env.db.app.QueryRow(ctxT(t), `SELECT payload FROM outbox_events WHERE event_id = $1`, e.EventID()).Scan(&stored)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(stored, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(e.Payload(), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s payload changed\n got %s\nwant %s", e.Type(), stored, e.Payload())
			}
		}
	})

	t.Run("inbox insert, redelivery and completion", func(t *testing.T) {
		w := env.openWallet(t, "100.00")
		bet, _ := env.commitSubmit(t, rawFor(w, "BET", "1.00", "inbox-bet", ""))
		var hash wager.PayloadHash
		copy(hash[:], "0123456789abcdef0123456789abcdef")
		msg := app.InboxMessage{ConsumerName: "wager-transactions", MessageID: "msg-" + uuid.NewString(), PayloadHash: hash, ReceivedAt: now()}

		err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			inserted, err := r.Inbox().Insert(ctx, msg)
			if err != nil || !inserted {
				t.Fatalf("first insert = %v, %v", inserted, err)
			}
			if inserted, err = r.Inbox().Insert(ctx, msg); err != nil || inserted {
				t.Fatalf("redelivery insert = %v, %v", inserted, err)
			}
			return r.Inbox().Complete(ctx, msg.ConsumerName, msg.MessageID, bet.ID(), wager.StatusProcessed, now())
		})
		if err != nil {
			t.Fatal(err)
		}
		err = env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			got, err := r.Inbox().Get(ctx, msg.ConsumerName, msg.MessageID)
			if err != nil {
				return err
			}
			if got.PayloadHash != hash || got.Outcome == nil || *got.Outcome != wager.StatusProcessed || *got.TransactionID != bet.ID() || got.ProcessedAt == nil {
				t.Errorf("inbox row = %+v", got)
			}
			if err := r.Inbox().Complete(ctx, msg.ConsumerName, msg.MessageID, bet.ID(), wager.StatusProcessed, now()); !errors.Is(err, app.ErrInvariantViolation) {
				t.Errorf("second completion: %v", err)
			}
			_, err = r.Inbox().Get(ctx, msg.ConsumerName, "missing")
			if !errors.Is(err, app.ErrNotFound) {
				t.Errorf("missing message: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

// TestFinancialAtomicity: everything of an operation commits together or
// not at all, and the stored balance always equals the ledger.
func TestFinancialAtomicity(t *testing.T) {
	t.Parallel()
	env := newRepoEnv(t)

	t.Run("failure after all writes leaves nothing", func(t *testing.T) {
		res, err := wager.OpenWallet(wager.OpenWalletParams{
			WalletID: env.ids(), PlayerID: env.ids(), InitialBalance: brlAmount(t, "100.00"),
			CorrelationID: "c", Now: now(), NewID: env.ids,
		})
		if err != nil {
			t.Fatal(err)
		}
		boom := errors.New("crash before commit")
		err = env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			if err := persistOpening(ctx, r, res); err != nil {
				return err
			}
			if _, _, err := submit(ctx, r, env.ids, rawFor(res.Wallet, "BET", "25.00", "atomic-bet", ""), now()); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		var n int
		if err := env.db.owner.QueryRow(ctxT(t), `
			SELECT (SELECT count(*) FROM wallets WHERE id = $1)
			     + (SELECT count(*) FROM wager_transactions WHERE wallet_id = $1)
			     + (SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1)
			     + (SELECT count(*) FROM outbox_events WHERE aggregate_id = $1)`, res.Wallet.ID()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d rows survived the rollback", n)
		}
	})

	t.Run("committed balance equals ledger", func(t *testing.T) {
		w := env.openWallet(t, "100.00")
		env.commitSubmit(t, rawFor(w, "BET", "80.00", "two-80-a", ""))
		_, out := env.commitSubmit(t, rawFor(w, "BET", "80.00", "two-80-b", ""))
		if out.Status != wager.StatusRejected || out.FailureCode != wager.CodeInsufficientFunds {
			t.Fatalf("second bet %s %s", out.Status, out.FailureCode)
		}
		var stored int64
		var debits int
		if err := env.db.app.QueryRow(ctxT(t), `SELECT balance_minor FROM wallets WHERE id = $1`, w.ID()).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if err := env.db.app.QueryRow(ctxT(t), `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID()).Scan(&debits); err != nil {
			t.Fatal(err)
		}
		err := env.runner.InTx(ctxT(t), app.TxOptions{Isolation: app.RepeatableReadReadOnly}, func(ctx context.Context, r app.Repos) error {
			fold, err := r.Ledger().Fold(ctx, w.ID(), w.Currency())
			if err != nil {
				return err
			}
			if fold.Balance.Minor() != stored || stored != 2000 || debits != 1 {
				t.Errorf("stored %d, ledger %s, debits %d; want 2000 = 20.00 and one debit", stored, fold.Balance, debits)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
