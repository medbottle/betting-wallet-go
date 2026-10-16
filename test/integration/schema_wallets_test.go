//go:build integration

package integration

import (
	"testing"

	"github.com/google/uuid"
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
