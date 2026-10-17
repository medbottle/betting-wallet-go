//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSchemaWallets(t *testing.T) {
	t.Parallel()
	db := newSchemaDB(t)

	t.Run("valid wallet as app", func(t *testing.T) {
		expectOK(t, db.app, ins(walletRow(newWallet())))
	})

	t.Run("zero balance allowed", func(t *testing.T) {
		expectOK(t, db.app, ins(walletRow(newWallet().with("balance_minor", int64(0)))))
	})

	t.Run("negative balance", func(t *testing.T) {
		expectErr(t, db.app, want{code: codeCheck, constraint: "wallets_balance_non_negative"},
			ins(walletRow(newWallet().with("balance_minor", int64(-1)))))
	})

	t.Run("version starts at 1", func(t *testing.T) {
		expectErr(t, db.app, want{code: codeCheck, constraint: "wallets_version_positive"},
			ins(walletRow(newWallet().with("version", int64(0)))))
	})

	t.Run("currency format", func(t *testing.T) {
		expectErr(t, db.app, want{code: codeCheck, constraint: "wallets_currency_format"},
			ins(walletRow(newWallet().with("currency", "brl"))))
	})

	t.Run("one wallet per player and currency", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeUnique, constraint: "wallets_player_currency_key"},
			ins(walletRow(w), walletRow(newWallet().with("player_id", w["player_id"]))))
	})

	t.Run("same player, other currency", func(t *testing.T) {
		w := newWallet()
		expectOK(t, db.app, ins(walletRow(w), walletRow(newWallet().with("player_id", w["player_id"], "currency", "USD"))))
	})

	t.Run("debit below zero", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeCheck, constraint: "wallets_balance_non_negative"}, then(
			ins(walletRow(w)),
			exec(`UPDATE wallets SET balance_minor = -1, version = 2 WHERE id = $1`, w["id"]),
		))
	})

	t.Run("balance change with version +1", func(t *testing.T) {
		w := newWallet()
		expectOK(t, db.app, then(
			ins(walletRow(w)),
			exec(`UPDATE wallets SET balance_minor = 2000, version = 2, updated_at = now() WHERE id = $1 AND version = 1`, w["id"]),
		))
	})

	t.Run("balance change without version bump", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeGuard, msg: "exactly 1"}, then(
			ins(walletRow(w)),
			exec(`UPDATE wallets SET balance_minor = 2000 WHERE id = $1`, w["id"]),
		))
	})

	t.Run("balance change with version +2", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeGuard, msg: "exactly 1"}, then(
			ins(walletRow(w)),
			exec(`UPDATE wallets SET balance_minor = 2000, version = 3 WHERE id = $1`, w["id"]),
		))
	})

	t.Run("version change without balance change", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeGuard, msg: "only together with the balance"}, then(
			ins(walletRow(w)),
			exec(`UPDATE wallets SET version = 2 WHERE id = $1`, w["id"]),
		))
	})

	immutable := map[string]string{
		"id":         `UPDATE wallets SET id = $2 WHERE id = $1`,
		"player_id":  `UPDATE wallets SET player_id = $2 WHERE id = $1`,
		"currency":   `UPDATE wallets SET currency = 'USD' WHERE id = $1 AND $2::uuid IS NOT NULL`,
		"created_at": `UPDATE wallets SET created_at = now() - interval '1 day' WHERE id = $1 AND $2::uuid IS NOT NULL`,
	}
	for col, sql := range immutable {
		t.Run("immutable "+col+" (owner)", func(t *testing.T) {
			w := newWallet()
			expectErr(t, db.owner, want{code: codeGuard, msg: "immutable"}, then(
				ins(walletRow(w)),
				exec(sql, w["id"], uuid.New()),
			))
		})
	}

	t.Run("delete blocked for owner", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.owner, want{code: codeGuard, msg: "DELETE is not allowed"}, then(
			ins(walletRow(w)),
			exec(`DELETE FROM wallets WHERE id = $1`, w["id"]),
		))
	})

	t.Run("delete not granted to app", func(t *testing.T) {
		expectErr(t, db.app, want{code: codePrivilege}, exec(`DELETE FROM wallets`))
	})

	t.Run("truncate blocked for owner", func(t *testing.T) {
		expectErr(t, db.owner, want{code: codeGuard, msg: "TRUNCATE is not allowed"}, exec(`TRUNCATE wallets CASCADE`))
	})

	t.Run("truncate not granted to app", func(t *testing.T) {
		expectErr(t, db.app, want{code: codePrivilege}, exec(`TRUNCATE wallets`))
	})
}

// TestSchemaWalletLedgerLink: every balance change commits with its ledger
// entry (CHALLENGE §5.3 and §6.2; migration 0008, ARCHITECTURE §16). The
// check is a deferred constraint trigger: rolled-back cases force it with
// SET CONSTRAINTS ... IMMEDIATE, and the commit cases show it at COMMIT.
func TestSchemaWalletLedgerLink(t *testing.T) {
	t.Parallel()
	db := newSchemaDB(t)
	check := exec(`SET CONSTRAINTS wallets_ledger_link IMMEDIATE`)
	missing := want{code: codeGuard, constraint: "wallets_ledger_link", msg: "without its ledger entry"}

	// opened is a wallet holding 100.00 with its OPENING and credit.
	opened := func() (row, row, []tableRow) {
		w := newWallet()
		open := openingTx(w)
		return w, open, []tableRow{walletRow(w), wagerRow(open), ledgerRow(ledgerEntry(w, open, "CREDIT", 10000, 0, 1))}
	}
	// debit moves an opened wallet from 100.00 to 75.00 at version 2.
	debit := func(w row) func(context.Context, pgx.Tx) error {
		return exec(`UPDATE wallets SET balance_minor = 7500, version = 2 WHERE id = $1`, w["id"])
	}

	t.Run("opening with its credit", func(t *testing.T) {
		_, _, rows := opened()
		expectOK(t, db.app, then(ins(rows...), check))
	})

	t.Run("zero opening needs no entry", func(t *testing.T) {
		expectOK(t, db.app, then(ins(walletRow(newWallet().with("balance_minor", int64(0)))), check))
	})

	t.Run("positive opening without its credit", func(t *testing.T) {
		expectErr(t, db.app, missing, then(ins(walletRow(newWallet())), check))
	})

	t.Run("opening credit to another balance", func(t *testing.T) {
		w := newWallet()
		open := openingTx(w)
		expectErr(t, db.app, missing, then(ins(walletRow(w), wagerRow(open), ledgerRow(ledgerEntry(w, open, "CREDIT", 5000, 0, 1))), check))
	})

	t.Run("balance change with its entry", func(t *testing.T) {
		w, _, rows := opened()
		bet := processed(externalTx(w, "BET"), 7500, 2)
		expectOK(t, db.app, then(ins(rows...), ins(wagerRow(bet)), debit(w),
			ins(ledgerRow(ledgerEntry(w, bet, "DEBIT", 2500, 10000, 2))), check))
	})

	t.Run("entry written before the balance change", func(t *testing.T) {
		w, _, rows := opened()
		bet := processed(externalTx(w, "BET"), 7500, 2)
		expectOK(t, db.app, then(ins(rows...), ins(wagerRow(bet), ledgerRow(ledgerEntry(w, bet, "DEBIT", 2500, 10000, 2))), debit(w), check))
	})

	t.Run("balance change without an entry", func(t *testing.T) {
		w, _, rows := opened()
		expectErr(t, db.app, missing, then(ins(rows...), debit(w), check))
	})

	t.Run("entry from another balance", func(t *testing.T) {
		w, _, rows := opened()
		bet := processed(externalTx(w, "BET"), 7500, 2)
		// Ends at 75.00 like the wallet, but starts at 80.00, not 100.00.
		expectErr(t, db.app, missing, then(ins(rows...), ins(wagerRow(bet), ledgerRow(ledgerEntry(w, bet, "DEBIT", 500, 8000, 2))), debit(w), check))
	})

	t.Run("entry at another version", func(t *testing.T) {
		w, _, rows := opened()
		bet := processed(externalTx(w, "BET"), 7500, 2)
		expectErr(t, db.app, missing, then(ins(rows...), ins(wagerRow(bet), ledgerRow(ledgerEntry(w, bet, "DEBIT", 2500, 10000, 3))), debit(w), check))
	})

	t.Run("applies to the owner too", func(t *testing.T) {
		w, _, rows := opened()
		expectErr(t, db.owner, missing, then(ins(rows...), debit(w), check))
	})

	commit := func(pool *pgxpool.Pool, fn func(context.Context, pgx.Tx) error) error {
		ctx := ctxT(t)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := fn(ctx, tx); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		return tx.Commit(ctx)
	}

	t.Run("checked at commit", func(t *testing.T) {
		w, _, rows := opened()
		if err := commit(db.app, ins(rows...)); err != nil {
			t.Fatalf("open: %v", err)
		}
		err := commit(db.app, debit(w))
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != codeGuard || pgErr.ConstraintName != "wallets_ledger_link" {
			t.Fatalf("commit of a balance change without its entry: %v", err)
		}
		var balance int64
		if err := db.owner.QueryRow(ctxT(t), `SELECT balance_minor FROM wallets WHERE id = $1`, w["id"]).Scan(&balance); err != nil || balance != 10000 {
			t.Fatalf("balance after the refused commit = %d, %v; want 10000", balance, err)
		}
		bet := processed(externalTx(w, "BET"), 7500, 2)
		if err := commit(db.app, then(ins(wagerRow(bet)), debit(w), ins(ledgerRow(ledgerEntry(w, bet, "DEBIT", 2500, 10000, 2))))); err != nil {
			t.Fatalf("commit with the entry: %v", err)
		}
	})
}
