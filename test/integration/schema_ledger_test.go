//go:build integration

package integration

import (
	"math"
	"testing"
)

func TestSchemaLedger(t *testing.T) {
	t.Parallel()
	db := newSchemaDB(t)

	// fixture: a wallet opened with 100.00 and a processed 25.00 bet.
	type fixture struct{ w, open, bet row }
	newFixture := func() fixture {
		w := newWallet()
		return fixture{w: w, open: openingTx(w), bet: processed(externalTx(w, "BET"), 7500, 2)}
	}
	base := func(f fixture) []tableRow {
		return []tableRow{walletRow(f.w), wagerRow(f.open), wagerRow(f.bet)}
	}
	withRows := func(f fixture, extra ...tableRow) []tableRow {
		return append(base(f), extra...)
	}

	t.Run("opening credit and bet debit as app", func(t *testing.T) {
		f := newFixture()
		expectOK(t, db.app, ins(withRows(f,
			ledgerRow(ledgerEntry(f.w, f.open, "CREDIT", 10000, 0, 1)),
			ledgerRow(ledgerEntry(f.w, f.bet, "DEBIT", 2500, 10000, 2)),
		)...))
	})

	t.Run("arithmetic must hold", func(t *testing.T) {
		f := newFixture()
		expectErr(t, db.app, want{code: codeCheck, constraint: "wallet_ledger_entries_balance_arithmetic"}, ins(withRows(f,
			ledgerRow(ledgerEntry(f.w, f.bet, "DEBIT", 2500, 10000, 2).with("balance_after_minor", int64(8000))),
		)...))
	})

	t.Run("direction applied the wrong way", func(t *testing.T) {
		f := newFixture()
		expectErr(t, db.app, want{code: codeCheck, constraint: "wallet_ledger_entries_balance_arithmetic"}, ins(withRows(f,
			ledgerRow(ledgerEntry(f.w, f.bet, "DEBIT", 2500, 10000, 2).with("balance_after_minor", int64(12500))),
		)...))
	})

	t.Run("amount must be positive", func(t *testing.T) {
		f := newFixture()
		expectErr(t, db.app, want{code: codeCheck, constraint: "wallet_ledger_entries_amount_positive"}, ins(withRows(f,
			ledgerRow(ledgerEntry(f.w, f.bet, "DEBIT", 0, 10000, 2)),
		)...))
	})

	t.Run("balance after cannot be negative", func(t *testing.T) {
		f := newFixture()
		expectErr(t, db.app, want{code: codeCheck, constraint: "wallet_ledger_entries_balance_after_non_negative"}, ins(withRows(f,
			ledgerRow(ledgerEntry(f.w, f.bet, "DEBIT", 2500, 1000, 2)),
		)...))
	})

	t.Run("credit overflowing bigint is rejected", func(t *testing.T) {
		f := newFixture()
		e := ledgerEntry(f.w, f.bet, "CREDIT", 2500, 0, 2).with("balance_before_minor", int64(math.MaxInt64-1), "balance_after_minor", int64(math.MaxInt64))
		expectErr(t, db.app, want{code: "22003"}, ins(withRows(f, ledgerRow(e))...)) // numeric_value_out_of_range
	})

	t.Run("direction valid", func(t *testing.T) {
		f := newFixture()
		expectErr(t, db.app, want{code: codeCheck, constraint: "wallet_ledger_entries_direction_valid"}, ins(withRows(f,
			ledgerRow(ledgerEntry(f.w, f.bet, "DEBIT", 2500, 10000, 2).with("direction", "SIDEWAYS")),
		)...))
	})

	t.Run("one entry per transaction", func(t *testing.T) {
		f := newFixture()
		expectErr(t, db.app, want{code: codeUnique, constraint: "wallet_ledger_entries_wallet_transaction_key"}, ins(withRows(f,
			ledgerRow(ledgerEntry(f.w, f.bet, "DEBIT", 2500, 10000, 2)),
			ledgerRow(ledgerEntry(f.w, f.bet, "DEBIT", 2500, 7500, 3)),
		)...))
	})

	t.Run("one entry per wallet version", func(t *testing.T) {
		f := newFixture()
		expectErr(t, db.app, want{code: codeUnique, constraint: "wallet_ledger_entries_wallet_version_key"}, ins(withRows(f,
			ledgerRow(ledgerEntry(f.w, f.open, "CREDIT", 10000, 0, 1)),
			ledgerRow(ledgerEntry(f.w, f.bet, "DEBIT", 2500, 10000, 1)),
		)...))
	})

	t.Run("transaction of another wallet", func(t *testing.T) {
		f := newFixture()
		other := newWallet()
		otherBet := processed(externalTx(other, "BET"), 7500, 2)
		expectErr(t, db.app, want{code: codeFK, constraint: "wallet_ledger_entries_transaction_fk"}, ins(withRows(f,
			walletRow(other), wagerRow(otherBet),
			ledgerRow(ledgerEntry(f.w, otherBet, "DEBIT", 2500, 10000, 2)),
		)...))
	})

	t.Run("currency differs from wallet", func(t *testing.T) {
		f := newFixture()
		expectErr(t, db.app, want{code: codeFK}, ins(withRows(f,
			ledgerRow(ledgerEntry(f.w, f.bet, "DEBIT", 2500, 10000, 2).with("currency", "USD")),
		)...))
	})

	// Append-only: the owner hits the triggers, the app lacks the privilege.
	t.Run("update blocked for owner", func(t *testing.T) {
		f := newFixture()
		e := ledgerEntry(f.w, f.open, "CREDIT", 10000, 0, 1)
		expectErr(t, db.owner, want{code: codeGuard, msg: "append-only"}, then(
			ins(withRows(f, ledgerRow(e))...),
			exec(`UPDATE wallet_ledger_entries SET created_at = now() WHERE id = $1`, e["id"]),
		))
	})
	t.Run("delete blocked for owner", func(t *testing.T) {
		f := newFixture()
		e := ledgerEntry(f.w, f.open, "CREDIT", 10000, 0, 1)
		expectErr(t, db.owner, want{code: codeGuard, msg: "append-only"}, then(
			ins(withRows(f, ledgerRow(e))...),
			exec(`DELETE FROM wallet_ledger_entries WHERE id = $1`, e["id"]),
		))
	})
	t.Run("truncate blocked for owner", func(t *testing.T) {
		expectErr(t, db.owner, want{code: codeGuard, msg: "append-only"}, exec(`TRUNCATE wallet_ledger_entries`))
	})
	for name, sql := range map[string]string{
		"update":   `UPDATE wallet_ledger_entries SET created_at = now()`,
		"delete":   `DELETE FROM wallet_ledger_entries`,
		"truncate": `TRUNCATE wallet_ledger_entries`,
	} {
		t.Run(name+" not granted to app", func(t *testing.T) {
			expectErr(t, db.app, want{code: codePrivilege}, exec(sql))
		})
	}
}
