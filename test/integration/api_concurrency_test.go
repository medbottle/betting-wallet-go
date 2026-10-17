//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"
)

var jsonMarshal = json.Marshal

// TestAPIConcurrency covers CHALLENGE §13 concurrency items 1-3 through the
// HTTP API with real tokens: duplicates, the 100.00 vs 2 x 80.00 race and
// independent wallets progressing in parallel.
func TestAPIConcurrency(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)

	t.Run("same bet 50 times in parallel debits once", func(t *testing.T) {
		w := e.openWallet(t, "100.00")
		body := op(w, "provider-a", "BET", "25.00", "dup-50")
		const n = 50
		results := make([]response, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i] = e.goSubmit(t, "provider-a", body)
			}(i)
		}
		wg.Wait()
		fresh, ids := 0, map[string]bool{}
		for _, r := range results {
			if r.Status != http.StatusOK || r.amount("balance") != "75.00" {
				t.Fatalf("duplicate answered %d %s", r.Status, r.Raw)
			}
			if r.Body["idempotentReplay"] == false {
				fresh++
			}
			ids[r.str("transactionId")] = true
		}
		if fresh != 1 || len(ids) != 1 {
			t.Fatalf("fresh outcomes = %d, distinct transactions = %d; want 1 and 1", fresh, len(ids))
		}
		if d := e.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID); d != 1 {
			t.Fatalf("debits = %d, want 1", d)
		}
		e.assertConsistent(t, w, "75.00")
	})

	t.Run("two 80.00 bets on 100.00", func(t *testing.T) {
		w := e.openWallet(t, "100.00")
		bodies := []map[string]any{op(w, "provider-a", "BET", "80.00", "race-a"), op(w, "provider-a", "BET", "80.00", "race-b")}
		results := make([]response, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range bodies {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				results[i] = e.goSubmit(t, "provider-a", bodies[i])
			}(i)
		}
		close(start)
		wg.Wait()
		ok, rejected := 0, 0
		for _, r := range results {
			switch {
			case r.Status == http.StatusOK && r.str("status") == "PROCESSED":
				ok++
			case r.Status == http.StatusUnprocessableEntity && r.str("failureCode") == "INSUFFICIENT_FUNDS" && r.amount("balance") == "20.00":
				rejected++
			default:
				t.Fatalf("unexpected outcome %d %s", r.Status, r.Raw)
			}
		}
		if ok != 1 || rejected != 1 {
			t.Fatalf("processed = %d, rejected = %d", ok, rejected)
		}
		if d := e.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID); d != 1 {
			t.Fatalf("debits = %d, want 1", d)
		}
		// Resending either bet changes nothing.
		for _, b := range bodies {
			e.submit(t, "provider-a", b)
		}
		e.assertConsistent(t, w, "20.00")
	})

	t.Run("different wallets do not wait for each other", func(t *testing.T) {
		busy, free := e.openWallet(t, "100.00"), e.openWallet(t, "100.00")
		release := holdTx(t, e.repo.db.owner, `SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE`, busy.ID)

		blocked := make(chan response, 1)
		go func() { blocked <- e.goSubmit(t, "provider-a", op(busy, "provider-a", "BET", "5.00", "busy-1")) }()

		start := time.Now()
		r := e.submit(t, "provider-a", op(free, "provider-a", "BET", "5.00", "free-1"))
		if r.Status != http.StatusOK || time.Since(start) > time.Second {
			t.Fatalf("free wallet: %d after %v while another wallet was locked", r.Status, time.Since(start))
		}
		select {
		case br := <-blocked:
			t.Fatalf("the locked wallet's bet finished early: %d", br.Status)
		default:
		}
		release()
		if br := <-blocked; br.Status != http.StatusOK {
			t.Fatalf("locked wallet's bet after release: %d %s", br.Status, br.Raw)
		}
		e.assertConsistent(t, busy, "95.00")
		e.assertConsistent(t, free, "95.00")
	})
}
