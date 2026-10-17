//go:build multiinstance

package multiinstance

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ext returns an external id unique to this run.
func ext(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

// TestSameBet50TimesAcrossInstances: CHALLENGE §13 items 1 and 4. The same
// BET arrives 50 times at once, over the three instances' HTTP APIs and
// over SQS (the same envelope repeated, and new envelopes for the same
// operation). One debit, one transaction; every HTTP answer is the same
// result.
func TestSameBet50TimesAcrossInstances(t *testing.T) {
	w := openWallet(t, instances[0], "100.00")
	id := ext("same50")
	bet := op(w, "BET", "10.00", id)
	sameEnvelope := envelope(t, "msg-"+id, bet)

	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	txIDs := map[string]int{}
	fresh := 0
	for i := range 50 {
		// Built here: envelope may t.Fatal, which only the test goroutine may.
		newEnvelope := envelope(t, fmt.Sprintf("msg-%s-%d", id, i), bet)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			switch {
			case i%5 == 3:
				goSendSQS(t, sameEnvelope, w.ID)
			case i%5 == 4:
				goSendSQS(t, newEnvelope, w.ID)
			default:
				r := goSubmit(t, instances[i%3], bet)
				mu.Lock()
				defer mu.Unlock()
				if r.Status != http.StatusOK || r.str("status") != "PROCESSED" || r.amount("balance") != "90.00" {
					t.Errorf("request %d on %s: %d %s", i, instances[i%3].Service, r.Status, r.Raw)
					return
				}
				txIDs[r.str("transactionId")]++
				if r.Body["idempotentReplay"] != true {
					fresh++
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	waitFor(t, settleTimeout, "inbound queue to drain", func() bool { return depth(t, inboundQueue) == 0 })

	if len(txIDs) != 1 || fresh > 1 {
		t.Fatalf("HTTP answers name %d transactions (%v), %d not replays", len(txIDs), txIDs, fresh)
	}
	if n := count(t, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = $1`, id); n != 1 {
		t.Fatalf("transactions = %d", n)
	}
	if n := count(t, `SELECT count(*) FROM inbox_messages WHERE message_id LIKE $1`, "msg-"+id+"%"); n != 11 {
		t.Fatalf("inbox rows = %d, want 11 distinct messageIds (1 repeated envelope + 10 new)", n)
	}
	assertWallets(t, map[string]string{w.ID: "90.00"}, w)
	if n := depth(t, inboundDLQ); n != 0 {
		t.Fatalf("DLQ has %d messages", n)
	}
}

// TestTwoBetsOf80AcrossInstances: CHALLENGE §8 and §13 items 2 and 4.
// 100.00 receives two different 80.00 BETs at the same time through two
// different instances, five times over: one PROCESSED, one
// INSUFFICIENT_FUNDS, 20.00 left, one debit. Replays through the third
// instance return the same results.
func TestTwoBetsOf80AcrossInstances(t *testing.T) {
	var wallets []wallet
	want := map[string]string{}
	for round := range 5 {
		w := openWallet(t, instances[round%3], "100.00")
		wallets = append(wallets, w)
		want[w.ID] = "20.00"
		a, b := op(w, "BET", "80.00", ext("bet80a")), op(w, "BET", "80.00", ext("bet80b"))
		var ra, rb response
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; ra = goSubmit(t, instances[0], a) }()
		go func() { defer wg.Done(); <-start; rb = goSubmit(t, instances[1], b) }()
		close(start)
		wg.Wait()

		ok, rejected := ra, rb
		if ra.Status != http.StatusOK {
			ok, rejected = rb, ra
		}
		if ok.Status != http.StatusOK || ok.amount("balance") != "20.00" ||
			rejected.Status != http.StatusUnprocessableEntity || rejected.str("failureCode") != "INSUFFICIENT_FUNDS" {
			t.Fatalf("round %d: %d %s / %d %s", round, ra.Status, ra.Raw, rb.Status, rb.Raw)
		}
		for _, body := range []map[string]any{a, b} {
			first := ra
			if body["externalTransactionId"] == b["externalTransactionId"] {
				first = rb
			}
			replay := submit(t, instances[2], body)
			if replay.Status != first.Status || replay.str("transactionId") != first.str("transactionId") || replay.Body["idempotentReplay"] != true {
				t.Fatalf("round %d replay: %d %s, first %d %s", round, replay.Status, replay.Raw, first.Status, first.Raw)
			}
		}
		if n := count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID); n != 1 {
			t.Fatalf("round %d: %d debits", round, n)
		}
	}
	assertWallets(t, want, wallets...)
}

// TestDistinctWalletsInParallel: CHALLENGE §13 items 3 and 4, and §5 item 6
// (no global lock). One wallet is held locked in the database; operations
// on 15 other wallets still complete through all three instances while it
// is held, so no process serializes different wallets.
func TestDistinctWalletsInParallel(t *testing.T) {
	blocked := openWallet(t, instances[0], "50.00")
	tx, err := db.Begin(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctxT(t), `SELECT 1 FROM wallets WHERE id = $1 FOR NO KEY UPDATE`, blocked.ID); err != nil {
		t.Fatal(err)
	}
	blockedDone := make(chan response, 1)
	go func() { blockedDone <- goSubmit(t, instances[0], op(blocked, "BET", "1.00", ext("blocked"))) }()

	var wallets []wallet
	want := map[string]string{}
	for i := range 15 {
		w := openWallet(t, instances[i%3], "10.00")
		wallets = append(wallets, w)
		want[w.ID] = "7.00"
	}
	began := time.Now()
	var wg sync.WaitGroup
	for i, w := range wallets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 3 {
				r := goSubmit(t, instances[(i+j)%3], op(w, "BET", "1.00", ext("par")))
				if r.Status != http.StatusOK {
					t.Errorf("wallet %d bet %d: %d %s", i, j, r.Status, r.Raw)
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(began)
	var r response
	select {
	case r = <-blockedDone:
		// Only possible as a lock_timeout (5s) while the lock is held: a
		// transient 503, never a processed operation.
		if r.Status != http.StatusServiceUnavailable || r.errorCode() != "TEMPORARILY_UNAVAILABLE" {
			t.Fatalf("the locked wallet's operation finished while its lock was held: %d %s", r.Status, r.Raw)
		}
		_ = tx.Rollback(ctxT(t))
	default:
		_ = tx.Rollback(ctxT(t))
		r = <-blockedDone
		if r.Status != http.StatusOK && r.Status != http.StatusServiceUnavailable {
			t.Fatalf("blocked wallet: %d %s", r.Status, r.Raw)
		}
	}
	t.Logf("45 operations on 15 wallets took %v while another wallet was locked", elapsed)
	assertWallets(t, want, append(wallets, blocked)...)
}

// TestHTTPAndSQSCrossoverAcrossInstances: CHALLENGE §10 and §13. The same
// operation arrives over SQS and over HTTP on every instance at once: one
// effect, and every HTTP answer names the same transaction.
func TestHTTPAndSQSCrossoverAcrossInstances(t *testing.T) {
	w := openWallet(t, instances[1], "100.00")
	var ids []string
	for range 5 {
		id := ext("cross")
		ids = append(ids, id)
		body := op(w, "BET", "3.00", id)
		var wg sync.WaitGroup
		results := make([]response, len(instances))
		start := make(chan struct{})
		msg := envelope(t, "msg-"+id, body)
		wg.Add(1)
		go func() { defer wg.Done(); <-start; goSendSQS(t, msg, w.ID) }()
		for i, in := range instances {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; results[i] = goSubmit(t, in, body) }()
		}
		close(start)
		wg.Wait()
		for i, r := range results {
			if r.Status != http.StatusOK || r.str("transactionId") != results[0].str("transactionId") {
				t.Fatalf("%s on %s: %d %s", id, instances[i].Service, r.Status, r.Raw)
			}
		}
	}
	waitFor(t, settleTimeout, "inbound queue to drain", func() bool { return depth(t, inboundQueue) == 0 })
	for _, id := range ids {
		if n := count(t, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = $1`, id); n != 1 {
			t.Fatalf("%s: %d transactions", id, n)
		}
		if n := count(t, `SELECT count(*) FROM inbox_messages WHERE message_id = $1 AND outcome = 'PROCESSED'`, "msg-"+id); n != 1 {
			t.Fatalf("%s: inbox not completed with the shared result", id)
		}
	}
	assertWallets(t, map[string]string{w.ID: "85.00"}, w)
}
