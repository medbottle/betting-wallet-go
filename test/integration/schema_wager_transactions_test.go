//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSchemaWagerTransactions(t *testing.T) {
	t.Parallel()
	db := newSchemaDB(t)

	shape := want{code: codeCheck, constraint: "wager_transactions_origin_shape"}
	amount := want{code: codeCheck, constraint: "wager_transactions_amount_by_kind"}
	reference := want{code: codeCheck, constraint: "wager_transactions_reference_by_kind"}
	failure := want{code: codeCheck, constraint: "wager_transactions_failure_code"}

	// Valid rows, inserted as the application role.
	t.Run("valid rows as app", func(t *testing.T) {
		w := newWallet()
		bet := processed(externalTx(w, "BET"), 7500, 2)
		expectOK(t, db.app, ins(
			walletRow(w),
			wagerRow(openingTx(w)),
			wagerRow(bet),
			wagerRow(externalTx(w, "WIN")),
			wagerRow(externalTx(w, "WIN").with("reference_external_transaction_id", bet["external_transaction_id"], "reference_transaction_id", bet["id"])),
			wagerRow(externalTx(w, "LOSS")),
			wagerRow(externalTx(w, "REFUND")),
			wagerRow(externalTx(w, "ROLLBACK")),
			wagerRow(rejected(externalTx(w, "BET"), "INSUFFICIENT_FUNDS", 7500, 2)),
			wagerRow(externalTx(w, "REFUND").with("status", "PENDING_REFERENCE", "reference_deadline_at", time.Now().Add(time.Minute))),
			wagerRow(externalTx(w, "BET").with("status", "FAILED", "failure_code", "INTERNAL_INVARIANT_VIOLATION", "completed_at", time.Now())),
		))
	})

	// Origin shape.
	shapeCases := map[string]func(w row) row{
		"internal with provider": func(w row) row { return openingTx(w).with("provider_id", "provider-a") },
		"internal with round":    func(w row) row { return openingTx(w).with("round_id", "r") },
		"internal not processed": func(w row) row {
			return openingTx(w).with("status", "PENDING", "result_balance_minor", nil, "result_wallet_version", nil, "completed_at", nil)
		},
		"internal non-opening": func(w row) row { return openingTx(w).with("kind", "BET") },
		"external opening":     func(w row) row { return externalTx(w, "BET").with("kind", "OPENING") },
		"external no hash":     func(w row) row { return externalTx(w, "BET").with("payload_hash", nil) },
		"external no key":      func(w row) row { return externalTx(w, "BET").with("idempotency_key", nil) },
		"external no game":     func(w row) row { return externalTx(w, "BET").with("game_id", nil) },
	}
	for name, mk := range shapeCases {
		t.Run(name, func(t *testing.T) {
			w := newWallet()
			expectErr(t, db.app, shape, ins(walletRow(w), wagerRow(mk(w))))
		})
	}

	// Amount policy by kind.
	t.Run("loss with amount", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, amount, ins(walletRow(w), wagerRow(externalTx(w, "LOSS").with("amount_minor", int64(100)))))
	})
	for _, kind := range []string{"BET", "WIN", "REFUND", "ROLLBACK"} {
		t.Run(strings.ToLower(kind)+" with zero amount", func(t *testing.T) {
			w := newWallet()
			expectErr(t, db.app, amount, ins(walletRow(w), wagerRow(externalTx(w, kind).with("amount_minor", int64(0)))))
		})
	}
	t.Run("opening with zero amount", func(t *testing.T) {
		w := newWallet().with("balance_minor", int64(0))
		expectErr(t, db.app, amount, ins(walletRow(w), wagerRow(openingTx(w))))
	})

	// References.
	for _, kind := range []string{"REFUND", "ROLLBACK"} {
		t.Run(strings.ToLower(kind)+" without reference", func(t *testing.T) {
			w := newWallet()
			expectErr(t, db.app, reference, ins(walletRow(w), wagerRow(externalTx(w, kind).with("reference_external_transaction_id", nil))))
		})
	}
	for _, kind := range []string{"BET", "LOSS"} {
		t.Run(strings.ToLower(kind)+" with reference", func(t *testing.T) {
			w := newWallet()
			expectErr(t, db.app, reference, ins(walletRow(w), wagerRow(externalTx(w, kind).with("reference_external_transaction_id", "x"))))
		})
	}
	t.Run("resolved reference without external reference", func(t *testing.T) {
		w := newWallet()
		bet := processed(externalTx(w, "BET"), 7500, 2)
		expectErr(t, db.app, want{code: codeCheck, constraint: "wager_transactions_reference_resolved"},
			ins(walletRow(w), wagerRow(bet), wagerRow(externalTx(w, "WIN").with("reference_transaction_id", bet["id"]))))
	})
	t.Run("unknown resolved reference", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeFK, constraint: "wager_transactions_reference_fk"},
			ins(walletRow(w), wagerRow(externalTx(w, "REFUND").with("reference_transaction_id", uuid.New()))))
	})

	// Failure codes.
	failureCases := map[string]func(w row) row{
		"rejected without code":       func(w row) row { return rejected(externalTx(w, "BET"), "x", 1, 1).with("failure_code", nil) },
		"rejected with category A":    func(w row) row { return rejected(externalTx(w, "BET"), "INVALID_MONEY", 1, 1) },
		"rejected with category D":    func(w row) row { return rejected(externalTx(w, "BET"), "INTERNAL_INVARIANT_VIOLATION", 1, 1) },
		"rejected with invented code": func(w row) row { return rejected(externalTx(w, "BET"), "NOPE", 1, 1) },
		"failed with category B": func(w row) row {
			return externalTx(w, "BET").with("status", "FAILED", "failure_code", "INSUFFICIENT_FUNDS", "completed_at", time.Now())
		},
		"failed without code": func(w row) row {
			return externalTx(w, "BET").with("status", "FAILED", "completed_at", time.Now())
		},
		"pending with code": func(w row) row { return externalTx(w, "BET").with("failure_code", "INSUFFICIENT_FUNDS") },
		"processed with code": func(w row) row {
			return processed(externalTx(w, "BET"), 1, 2).with("failure_code", "INSUFFICIENT_FUNDS")
		},
	}
	for name, mk := range failureCases {
		t.Run(name, func(t *testing.T) {
			w := newWallet()
			expectErr(t, db.app, failure, ins(walletRow(w), wagerRow(mk(w))))
		})
	}

	// Persisted results and timestamps.
	t.Run("processed without result", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeCheck, constraint: "wager_transactions_result"},
			ins(walletRow(w), wagerRow(processed(externalTx(w, "BET"), 1, 2).with("result_wallet_version", nil))))
	})
	t.Run("pending with result", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeCheck, constraint: "wager_transactions_result"},
			ins(walletRow(w), wagerRow(externalTx(w, "BET").with("result_balance_minor", int64(1)))))
	})
	t.Run("terminal without completed_at", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeCheck, constraint: "wager_transactions_completed_at"},
			ins(walletRow(w), wagerRow(processed(externalTx(w, "BET"), 1, 2).with("completed_at", nil))))
	})
	t.Run("pending with completed_at", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeCheck, constraint: "wager_transactions_completed_at"},
			ins(walletRow(w), wagerRow(externalTx(w, "BET").with("completed_at", time.Now()))))
	})
	t.Run("pending reference without deadline", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeCheck, constraint: "wager_transactions_reference_deadline"},
			ins(walletRow(w), wagerRow(externalTx(w, "REFUND").with("status", "PENDING_REFERENCE"))))
	})

	// Formats.
	t.Run("payload hash must be 32 bytes", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeCheck, constraint: "wager_transactions_payload_hash_sha256"},
			ins(walletRow(w), wagerRow(externalTx(w, "BET").with("payload_hash", make([]byte, 31)))))
	})
	for name, key := range map[string]string{"empty": "", "too long": strings.Repeat("k", 256), "control char": "a\nb", "non-ascii": "chave-é"} {
		t.Run("idempotency key "+name, func(t *testing.T) {
			w := newWallet()
			expectErr(t, db.app, want{code: codeCheck, constraint: "wager_transactions_idempotency_key_format"},
				ins(walletRow(w), wagerRow(externalTx(w, "BET").with("idempotency_key", key))))
		})
	}
	t.Run("idempotency key of 255 printable chars", func(t *testing.T) {
		w := newWallet()
		expectOK(t, db.app, ins(walletRow(w), wagerRow(externalTx(w, "BET").with("idempotency_key", strings.Repeat("~", 254)+" "))))
	})

	// Uniqueness and idempotency scope.
	t.Run("same external id, other key", func(t *testing.T) {
		w := newWallet()
		a := externalTx(w, "BET")
		expectErr(t, db.app, want{code: codeUnique, constraint: "wager_transactions_provider_external_id_key"},
			ins(walletRow(w), wagerRow(a), wagerRow(externalTx(w, "BET").with("external_transaction_id", a["external_transaction_id"]))))
	})
	t.Run("same key, other external id", func(t *testing.T) {
		w := newWallet()
		a := externalTx(w, "BET")
		expectErr(t, db.app, want{code: codeUnique, constraint: "wager_transactions_provider_idempotency_key_key"},
			ins(walletRow(w), wagerRow(a), wagerRow(externalTx(w, "BET").with("idempotency_key", a["idempotency_key"]))))
	})
	t.Run("same external id and key, other provider", func(t *testing.T) {
		w := newWallet()
		a := externalTx(w, "BET")
		expectOK(t, db.app, ins(walletRow(w), wagerRow(a), wagerRow(externalTx(w, "BET").with(
			"provider_id", "provider-b",
			"external_transaction_id", a["external_transaction_id"],
			"idempotency_key", a["idempotency_key"],
		))))
	})
	t.Run("second opening on the same wallet", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeUnique, constraint: "wager_transactions_one_opening_per_wallet"},
			ins(walletRow(w), wagerRow(openingTx(w)), wagerRow(openingTx(w))))
	})

	// One successful reversal of any kind per referenced operation.
	for _, kinds := range [][2]string{{"REFUND", "REFUND"}, {"REFUND", "ROLLBACK"}, {"ROLLBACK", "ROLLBACK"}, {"ROLLBACK", "REFUND"}} {
		t.Run("processed "+strings.ToLower(kinds[0])+" then processed "+strings.ToLower(kinds[1]), func(t *testing.T) {
			w := newWallet()
			bet := processed(externalTx(w, "BET"), 7500, 2)
			expectErr(t, db.app, want{code: codeUnique, constraint: "wager_transactions_one_reversal_per_reference"}, ins(
				walletRow(w), wagerRow(bet),
				wagerRow(processed(reversalOf(w, bet, kinds[0]), 10000, 3)),
				wagerRow(processed(reversalOf(w, bet, kinds[1]), 12500, 4)),
			))
		})
	}
	t.Run("rejected and pending second reversals allowed", func(t *testing.T) {
		w := newWallet()
		bet := processed(externalTx(w, "BET"), 7500, 2)
		expectOK(t, db.app, ins(
			walletRow(w), wagerRow(bet),
			wagerRow(processed(reversalOf(w, bet, "REFUND"), 10000, 3)),
			wagerRow(rejected(reversalOf(w, bet, "ROLLBACK"), "ALREADY_REVERSED", 10000, 3)),
			wagerRow(reversalOf(w, bet, "REFUND")),
		))
	})
	t.Run("rollback of a refund allowed", func(t *testing.T) {
		w := newWallet()
		bet := processed(externalTx(w, "BET"), 7500, 2)
		refund := processed(reversalOf(w, bet, "REFUND"), 10000, 3)
		expectOK(t, db.app, ins(walletRow(w), wagerRow(bet), wagerRow(refund), wagerRow(processed(reversalOf(w, refund, "ROLLBACK"), 7500, 4))))
	})

	// Wallet consistency and provider registry.
	t.Run("currency differs from wallet", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeFK, constraint: "wager_transactions_wallet_fk"},
			ins(walletRow(w), wagerRow(externalTx(w, "BET").with("currency", "USD"))))
	})
	t.Run("player differs from wallet", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeFK, constraint: "wager_transactions_wallet_fk"},
			ins(walletRow(w), wagerRow(externalTx(w, "BET").with("player_id", uuid.New()))))
	})
	t.Run("unknown provider", func(t *testing.T) {
		w := newWallet()
		expectErr(t, db.app, want{code: codeFK, constraint: "wager_transactions_provider_fk"},
			ins(walletRow(w), wagerRow(externalTx(w, "BET").with("provider_id", "provider-z"))))
	})

	// Allowed updates (as app).
	t.Run("pending to processed", func(t *testing.T) {
		w := newWallet()
		bet := externalTx(w, "BET")
		expectOK(t, db.app, then(ins(walletRow(w), wagerRow(bet)), exec(
			`UPDATE wager_transactions SET status = 'PROCESSED', result_balance_minor = 7500, result_wallet_version = 2,
			        completed_at = now(), updated_at = now() WHERE id = $1`, bet["id"])))
	})
	t.Run("pending reference rescheduled then resolved", func(t *testing.T) {
		w := newWallet()
		bet := processed(externalTx(w, "BET"), 7500, 2)
		refund := externalTx(w, "REFUND").with("amount_minor", bet["amount_minor"], "reference_external_transaction_id", bet["external_transaction_id"],
			"status", "PENDING_REFERENCE", "reference_deadline_at", time.Now().Add(time.Minute))
		expectOK(t, db.app, then(
			ins(walletRow(w), wagerRow(bet), wagerRow(refund)),
			exec(`UPDATE wager_transactions SET attempts = attempts + 1, next_attempt_at = now() + interval '2 seconds' WHERE id = $1`, refund["id"]),
			exec(`UPDATE wager_transactions SET status = 'PROCESSED', reference_transaction_id = $2, result_balance_minor = 10000,
			        result_wallet_version = 3, completed_at = now() WHERE id = $1`, refund["id"], bet["id"]),
		))
	})

	// Trigger guards, exercised as the owner.
	terminal := map[string]row{
		"processed": processed(externalTx(newWallet(), "BET"), 7500, 2),
		"rejected":  rejected(externalTx(newWallet(), "BET"), "INSUFFICIENT_FUNDS", 100, 1),
		"failed":    externalTx(newWallet(), "BET").with("status", "FAILED", "failure_code", "INTERNAL_INVARIANT_VIOLATION", "completed_at", time.Now()),
	}
	for name, r := range terminal {
		t.Run(name+" row is frozen", func(t *testing.T) {
			w := newWallet()
			r := r.with("wallet_id", w["id"], "player_id", w["player_id"])
			expectErr(t, db.owner, want{code: codeGuard, msg: "is terminal"}, then(
				ins(walletRow(w), wagerRow(r)),
				exec(`UPDATE wager_transactions SET updated_at = now() WHERE id = $1`, r["id"]),
			))
		})
	}
	immutable := map[string]string{
		"amount":        `amount_minor = amount_minor + 1`,
		"kind":          `kind = 'WIN'`,
		"payload hash":  `payload_hash = '\x00'::bytea || substring(payload_hash from 2)`,
		"external id":   `external_transaction_id = 'other'`,
		"key":           `idempotency_key = 'other'`,
		"round":         `round_id = 'other'`,
		"correlation":   `correlation_id = 'other'`,
		"created_at":    `created_at = created_at - interval '1 second'`,
		"ext reference": `reference_external_transaction_id = 'other'`,
	}
	for name, set := range immutable {
		t.Run("pending "+name+" is immutable", func(t *testing.T) {
			w := newWallet()
			r := externalTx(w, "BET")
			if name == "ext reference" {
				r = externalTx(w, "REFUND")
			}
			expectErr(t, db.owner, want{code: codeGuard, msg: "immutable"}, then(
				ins(walletRow(w), wagerRow(r)),
				exec(`UPDATE wager_transactions SET `+set+` WHERE id = $1`, r["id"]),
			))
		})
	}
	t.Run("pending reference cannot go back to pending", func(t *testing.T) {
		w := newWallet()
		r := externalTx(w, "REFUND").with("status", "PENDING_REFERENCE", "reference_deadline_at", time.Now().Add(time.Minute))
		expectErr(t, db.owner, want{code: codeGuard, msg: "PENDING_REFERENCE -> PENDING"}, then(
			ins(walletRow(w), wagerRow(r)),
			exec(`UPDATE wager_transactions SET status = 'PENDING' WHERE id = $1`, r["id"]),
		))
	})
	t.Run("resolved reference cannot be re-pointed", func(t *testing.T) {
		w := newWallet()
		bet1 := processed(externalTx(w, "BET"), 7500, 2)
		bet2 := processed(externalTx(w, "BET"), 5000, 3)
		win := externalTx(w, "WIN").with("reference_external_transaction_id", bet1["external_transaction_id"], "reference_transaction_id", bet1["id"])
		expectErr(t, db.owner, want{code: codeGuard, msg: "resolved reference"}, then(
			ins(walletRow(w), wagerRow(bet1), wagerRow(bet2), wagerRow(win)),
			exec(`UPDATE wager_transactions SET reference_transaction_id = $2 WHERE id = $1`, win["id"], bet2["id"]),
		))
	})
	t.Run("delete blocked for owner", func(t *testing.T) {
		w := newWallet()
		r := externalTx(w, "BET")
		expectErr(t, db.owner, want{code: codeGuard, msg: "DELETE is not allowed"}, then(
			ins(walletRow(w), wagerRow(r)),
			exec(`DELETE FROM wager_transactions WHERE id = $1`, r["id"]),
		))
	})
	t.Run("delete not granted to app", func(t *testing.T) {
		expectErr(t, db.app, want{code: codePrivilege}, exec(`DELETE FROM wager_transactions`))
	})
	t.Run("truncate blocked for owner", func(t *testing.T) {
		expectErr(t, db.owner, want{code: codeGuard, msg: "TRUNCATE is not allowed"}, exec(`TRUNCATE wager_transactions CASCADE`))
	})
}
