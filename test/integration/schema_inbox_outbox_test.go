//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func inboxRow(r row) tableRow  { return tableRow{"inbox_messages", r} }
func outboxRow(r row) tableRow { return tableRow{"outbox_events", r} }

func newInbox() row {
	return row{
		"consumer_name": "wager-transactions",
		"message_id":    "msg-" + uuid.NewString(),
		"payload_hash":  hash32(),
	}
}

func newOutbox(w row) row {
	id := uuid.New()
	return row{
		"event_id":       id,
		"aggregate_type": "Wallet",
		"aggregate_id":   w["id"],
		"event_type":     "WalletBalanceChanged",
		"event_version":  int32(1),
		"correlation_id": "corr-1",
		"occurred_at":    time.Now(),
		"payload":        `{"eventId":"` + id.String() + `","eventType":"WalletBalanceChanged"}`,
	}
}

func TestSchemaInbox(t *testing.T) {
	t.Parallel()
	db := newSchemaDB(t)

	t.Run("insert then complete as app", func(t *testing.T) {
		w := newWallet()
		bet := processed(externalTx(w, "BET"), 7500, 2)
		m := newInbox()
		expectOK(t, db.app, then(
			ins(walletRow(w), wagerRow(bet), inboxRow(m)),
			exec(`UPDATE inbox_messages SET transaction_id = $3, outcome = 'PROCESSED', processed_at = now()
			      WHERE consumer_name = $1 AND message_id = $2`, m["consumer_name"], m["message_id"], bet["id"]),
		))
	})

	t.Run("duplicate message id", func(t *testing.T) {
		m := newInbox()
		expectErr(t, db.app, want{code: codeUnique, constraint: "inbox_messages_pkey"},
			ins(inboxRow(m), inboxRow(newInbox().with("message_id", m["message_id"]))))
	})

	t.Run("same message id, other consumer", func(t *testing.T) {
		m := newInbox()
		expectOK(t, db.app, ins(inboxRow(m), inboxRow(newInbox().with("message_id", m["message_id"], "consumer_name", "other"))))
	})

	t.Run("outcome without processed_at", func(t *testing.T) {
		expectErr(t, db.app, want{code: codeCheck, constraint: "inbox_messages_completion"},
			ins(inboxRow(newInbox().with("outcome", "PROCESSED"))))
	})

	t.Run("completed without transaction", func(t *testing.T) {
		expectErr(t, db.app, want{code: codeCheck, constraint: "inbox_messages_completion_has_transaction"},
			ins(inboxRow(newInbox().with("outcome", "PROCESSED", "processed_at", time.Now()))))
	})

	t.Run("unknown outcome", func(t *testing.T) {
		w := newWallet()
		bet := processed(externalTx(w, "BET"), 7500, 2)
		expectErr(t, db.app, want{code: codeCheck, constraint: "inbox_messages_outcome_valid"},
			ins(walletRow(w), wagerRow(bet), inboxRow(newInbox().with("transaction_id", bet["id"], "outcome", "DUPLICATE", "processed_at", time.Now()))))
	})

	t.Run("hash must be 32 bytes", func(t *testing.T) {
		expectErr(t, db.app, want{code: codeCheck, constraint: "inbox_messages_payload_hash_sha256"},
			ins(inboxRow(newInbox().with("payload_hash", make([]byte, 16)))))
	})

	t.Run("completed message is frozen", func(t *testing.T) {
		w := newWallet()
		bet := processed(externalTx(w, "BET"), 7500, 2)
		m := newInbox().with("transaction_id", bet["id"], "outcome", "PROCESSED", "processed_at", time.Now())
		expectErr(t, db.owner, want{code: codeGuard, msg: "completed message"}, then(
			ins(walletRow(w), wagerRow(bet), inboxRow(m)),
			exec(`UPDATE inbox_messages SET outcome = 'REJECTED' WHERE message_id = $1`, m["message_id"]),
		))
	})

	t.Run("hash immutable while pending", func(t *testing.T) {
		m := newInbox()
		expectErr(t, db.owner, want{code: codeGuard, msg: "immutable"}, then(
			ins(inboxRow(m)),
			exec(`UPDATE inbox_messages SET payload_hash = $2 WHERE message_id = $1`, m["message_id"], hash32()),
		))
	})

	t.Run("delete blocked for owner", func(t *testing.T) {
		m := newInbox()
		expectErr(t, db.owner, want{code: codeGuard, msg: "DELETE is not allowed"}, then(
			ins(inboxRow(m)),
			exec(`DELETE FROM inbox_messages WHERE message_id = $1`, m["message_id"]),
		))
	})
	t.Run("truncate blocked for owner", func(t *testing.T) {
		expectErr(t, db.owner, want{code: codeGuard, msg: "TRUNCATE is not allowed"}, exec(`TRUNCATE inbox_messages`))
	})
	t.Run("delete not granted to app", func(t *testing.T) {
		expectErr(t, db.app, want{code: codePrivilege}, exec(`DELETE FROM inbox_messages`))
	})
}

func TestSchemaOutbox(t *testing.T) {
	t.Parallel()
	db := newSchemaDB(t)

	t.Run("insert, claim, fail, reclaim and publish as app", func(t *testing.T) {
		w := newWallet()
		e := newOutbox(w)
		claim := `UPDATE outbox_events SET locked_by = $2, locked_until = now() + interval '30 seconds', attempts = attempts + 1
		          WHERE event_id = $1`
		expectOK(t, db.app, then(
			ins(walletRow(w), outboxRow(e)),
			exec(claim, e["event_id"], "instance-a"),
			exec(`UPDATE outbox_events SET next_attempt_at = now() + interval '2 seconds', locked_until = NULL, last_error = 'boom'
			      WHERE event_id = $1 AND locked_by = 'instance-a'`, e["event_id"]),
			exec(claim, e["event_id"], "instance-b"),
			exec(`UPDATE outbox_events SET published_at = now() WHERE event_id = $1 AND locked_by = 'instance-b'`, e["event_id"]),
		))
	})

	t.Run("park and manual unpark", func(t *testing.T) {
		w := newWallet()
		e := newOutbox(w)
		expectOK(t, db.app, then(
			ins(walletRow(w), outboxRow(e)),
			exec(`UPDATE outbox_events SET parked_at = now() WHERE event_id = $1`, e["event_id"]),
			exec(`UPDATE outbox_events SET parked_at = NULL, attempts = 0 WHERE event_id = $1`, e["event_id"]),
		))
	})

	t.Run("payload must be an object", func(t *testing.T) {
		expectErr(t, db.app, want{code: codeCheck, constraint: "outbox_events_payload_is_object"},
			ins(outboxRow(newOutbox(newWallet()).with("payload", `[1,2]`))))
	})

	t.Run("unknown event type", func(t *testing.T) {
		expectErr(t, db.app, want{code: codeCheck, constraint: "outbox_events_event_type_valid"},
			ins(outboxRow(newOutbox(newWallet()).with("event_type", "SomethingHappened"))))
	})

	t.Run("published and parked", func(t *testing.T) {
		expectErr(t, db.app, want{code: codeCheck, constraint: "outbox_events_published_or_parked"},
			ins(outboxRow(newOutbox(newWallet()).with("published_at", time.Now(), "parked_at", time.Now()))))
	})

	immutable := map[string]string{
		"payload":       `payload = '{"eventId":"tampered"}'`,
		"event_id":      `event_id = gen_random_uuid()`,
		"event_type":    `event_type = 'WagerTransactionProcessed'`,
		"event_version": `event_version = 2`,
		"aggregate_id":  `aggregate_id = gen_random_uuid()`,
		"occurred_at":   `occurred_at = occurred_at - interval '1 second'`,
		"correlation":   `correlation_id = 'other'`,
		"causation":     `causation_id = 'other'`,
	}
	for name, set := range immutable {
		t.Run(name+" is immutable", func(t *testing.T) {
			e := newOutbox(newWallet())
			expectErr(t, db.owner, want{code: codeGuard, msg: "immutable"}, then(
				ins(outboxRow(e)),
				exec(`UPDATE outbox_events SET `+set+` WHERE event_id = $1`, e["event_id"]),
			))
		})
	}

	t.Run("publication cannot be undone", func(t *testing.T) {
		e := newOutbox(newWallet()).with("published_at", time.Now())
		expectErr(t, db.owner, want{code: codeGuard, msg: "published_at"}, then(
			ins(outboxRow(e)),
			exec(`UPDATE outbox_events SET published_at = NULL WHERE event_id = $1`, e["event_id"]),
		))
	})

	t.Run("delete blocked for owner", func(t *testing.T) {
		e := newOutbox(newWallet())
		expectErr(t, db.owner, want{code: codeGuard, msg: "DELETE is not allowed"}, then(
			ins(outboxRow(e)),
			exec(`DELETE FROM outbox_events WHERE event_id = $1`, e["event_id"]),
		))
	})
	t.Run("truncate blocked for owner", func(t *testing.T) {
		expectErr(t, db.owner, want{code: codeGuard, msg: "TRUNCATE is not allowed"}, exec(`TRUNCATE outbox_events`))
	})
	t.Run("delete not granted to app", func(t *testing.T) {
		expectErr(t, db.app, want{code: codePrivilege}, exec(`DELETE FROM outbox_events`))
	})
}
