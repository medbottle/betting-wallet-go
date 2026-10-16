//go:build multiinstance

package multiinstance

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestConsumerKilledUnderLoad: CHALLENGE §3 (abrupt stop before or after a
// commit) and §13 item 5, with real kills. 20 wallets get 3 BETs each over
// SQS, every message sent twice; app-2 is SIGKILLed while the consumers
// work and started again. Messages it had in flight come back after the
// visibility timeout; whatever it committed comes back as an inbox
// duplicate. Each operation is applied exactly once.
func TestConsumerKilledUnderLoad(t *testing.T) {
	var wallets []wallet
	want := map[string]string{}
	var ids []string
	for i := range 20 {
		w := openWallet(t, instances[i%3], "10.00")
		wallets = append(wallets, w)
		want[w.ID] = "7.00"
	}
	for _, w := range wallets {
		for range 3 {
			id := ext("killsqs")
			ids = append(ids, id)
			body := envelope(t, "msg-"+id, op(w, "BET", "1.00", id))
			sendSQS(t, body, w.ID)
			sendSQS(t, body, w.ID)
		}
	}
	waitFor(t, time.Minute, "some messages handled", func() bool {
		return count(t, `SELECT count(*) FROM inbox_messages WHERE message_id LIKE 'msg-killsqs-%' AND processed_at IS NOT NULL`) >= 10
	})
	kill(t, "app-2")
	t.Log("app-2 killed while the consumers were working through the queue")
	time.Sleep(2 * time.Second)
	restart(t, "app-2")

	// In-flight messages of the killed process return after 60s.
	waitFor(t, settleTimeout, "inbound queue to drain", func() bool { return depth(t, inboundQueue) == 0 })
	for _, id := range ids {
		if n := count(t, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = $1 AND status = 'PROCESSED'`, id); n != 1 {
			t.Fatalf("%s: %d processed transactions", id, n)
		}
	}
	assertWallets(t, want, wallets...)
	if n := depth(t, inboundDLQ); n != 0 {
		t.Fatalf("DLQ has %d messages", n)
	}
}

// TestOutboxRelaysKilledUnderLoad: CHALLENGE §11 and §13 item 6 with three
// relays and real kills. HTTP load on all instances produces events while
// app-3 is SIGKILLed mid-way (between claim, publish and mark, whatever it
// was doing). Every committed event reaches the events queue, republished
// ones with the same eventId.
func TestOutboxRelaysKilledUnderLoad(t *testing.T) {
	var wallets []wallet
	want := map[string]string{}
	for i := range 20 {
		w := openWallet(t, instances[i%3], "10.00")
		wallets = append(wallets, w)
		want[w.ID] = "6.00"
	}
	var wg sync.WaitGroup
	for i, w := range wallets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 4 {
				// app-3 may be down: retry on the next instance, which is what
				// a load balancer would do. The same key makes it safe.
				body := op(w, "BET", "1.00", ext("killrelay"))
				for k := 0; ; k++ {
					r := submit(t, instances[(i+j+k)%3], body)
					if r.Status == http.StatusOK {
						break
					}
					if k > 6 {
						t.Errorf("wallet %d bet %d: %d %s", i, j, r.Status, r.Raw)
						return
					}
					time.Sleep(200 * time.Millisecond)
				}
			}
		}()
		if i == 8 {
			kill(t, "app-3")
			t.Log("app-3 killed under load")
		}
	}
	wg.Wait()
	restart(t, "app-3")
	assertWallets(t, want, wallets...)
}

// TestReferenceResolvedAfterInstanceKill: CHALLENGE §7 and §13 item 7. A
// REFUND and a ROLLBACK are accepted as PENDING_REFERENCE by app-3, which
// is then killed. The REFUND's BET arrives on app; another instance's
// worker resolves the REFUND. The ROLLBACK's reference never arrives and
// another instance expires it as REFERENCE_NOT_FOUND after the TTL.
func TestReferenceResolvedAfterInstanceKill(t *testing.T) {
	w := openWallet(t, instances[0], "100.00")
	betID, refundID, rollbackID := ext("refbet"), ext("refund"), ext("rollback")
	if r := submit(t, instances[2], op(w, "REFUND", "20.00", refundID, betID)); r.Status != http.StatusAccepted {
		t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}
	if r := submit(t, instances[2], op(w, "ROLLBACK", "5.00", rollbackID, ext("never"))); r.Status != http.StatusAccepted {
		t.Fatalf("rollback: %d %s", r.Status, r.Raw)
	}
	kill(t, "app-3")
	defer func() {
		if readyStatus(instances[2]) != http.StatusOK {
			restart(t, "app-3")
		}
	}()

	if r := submit(t, instances[0], op(w, "BET", "20.00", betID)); r.Status != http.StatusOK {
		t.Fatalf("bet: %d %s", r.Status, r.Raw)
	}
	waitStatus2(t, refundID, "PROCESSED", 30*time.Second)
	rb := waitStatus2(t, rollbackID, "REJECTED", referenceTTL+30*time.Second)
	if rb.str("failureCode") != "REFERENCE_NOT_FOUND" {
		t.Fatalf("expired rollback: %s", rb.Raw)
	}
	restart(t, "app-3")
	assertWallets(t, map[string]string{w.ID: "100.00"}, w)
}

// waitStatus2 is waitStatus over the instances that are up (app and app-2).
func waitStatus2(t *testing.T, externalID, want string, timeout time.Duration) response {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for i := 0; ; i++ {
		r := getOp(t, instances[i%2], externalID)
		if r.Status == http.StatusOK && r.str("status") == want {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not reach %s: %d %s", externalID, want, r.Status, r.Raw)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestRestartAllInstances: CHALLENGE §13 item 8. With a processed
// operation, a pending reference and messages queued while everything is
// down, all three processes are killed and started again. Replays return
// the original result (balance as observed then), the pending reference
// resolves, the queued messages are processed once, and the ledger
// matches. There is no asynchronous accept to interrupt: PENDING is never
// committed on the synchronous path (ARCHITECTURE §5, §25 G9).
func TestRestartAllInstances(t *testing.T) {
	w := openWallet(t, instances[0], "100.00")
	betID, winID, lateID := ext("rs-bet"), ext("rs-win"), ext("rs-late")
	first := submit(t, instances[0], op(w, "BET", "10.00", betID))
	if first.Status != http.StatusOK {
		t.Fatalf("bet: %d %s", first.Status, first.Raw)
	}
	if r := submit(t, instances[1], op(w, "WIN", "7.00", winID, lateID)); r.Status != http.StatusAccepted {
		t.Fatalf("win: %d %s", r.Status, r.Raw)
	}

	kill(t, "app")
	kill(t, "app-2")
	kill(t, "app-3")
	var queued []string
	for range 3 {
		id := ext("rs-sqs")
		queued = append(queued, id)
		sendSQS(t, envelope(t, "msg-"+id, op(w, "BET", "1.00", id)), w.ID)
	}
	restart(t, "app", "app-2", "app-3")

	for _, in := range instances {
		r := submit(t, in, op(w, "BET", "10.00", betID))
		if r.Status != http.StatusOK || r.str("transactionId") != first.str("transactionId") ||
			r.amount("balance") != "90.00" || r.Body["idempotentReplay"] != true {
			t.Fatalf("replay on %s after restart: %d %s", in.Service, r.Status, r.Raw)
		}
	}
	if r := submit(t, instances[2], op(w, "BET", "2.00", lateID)); r.Status != http.StatusOK {
		t.Fatalf("late bet: %d %s", r.Status, r.Raw)
	}
	waitStatus(t, winID, "PROCESSED", 30*time.Second)
	for _, id := range queued {
		waitStatus(t, id, "PROCESSED", time.Minute)
	}
	// 100 - 10 - 2 + 7 - 3 x 1
	assertWallets(t, map[string]string{w.ID: "92.00"}, w)
}

// TestPostgresOutage: CHALLENGE §3, PostgreSQL temporarily unavailable.
// While it is stopped every instance reports not ready, HTTP answers 503
// retryable, and the consumers pause, so queued messages do not use up
// attempts or reach the DLQ. After it returns everything is processed once.
func TestPostgresOutage(t *testing.T) {
	w := openWallet(t, instances[0], "50.00")
	mustCompose(t, "stop", "postgres")
	restarted := false
	defer func() {
		if !restarted {
			mustCompose(t, "start", "postgres")
		}
	}()
	waitFor(t, 30*time.Second, "all instances not ready", func() bool {
		for _, in := range instances {
			if readyStatus(in) != http.StatusServiceUnavailable {
				return false
			}
		}
		return true
	})
	httpID := ext("pg-http")
	r := submit(t, instances[1], op(w, "BET", "5.00", httpID))
	if r.Status != http.StatusServiceUnavailable || r.errorCode() != "TEMPORARILY_UNAVAILABLE" || r.Body["error"].(map[string]any)["retryable"] != true {
		t.Fatalf("submit during the outage: %d %s", r.Status, r.Raw)
	}
	var queued []string
	for range 5 {
		id := ext("pg-sqs")
		queued = append(queued, id)
		sendSQS(t, envelope(t, "msg-"+id, op(w, "BET", "1.00", id)), w.ID)
	}
	time.Sleep(10 * time.Second) // well past several poll cycles
	if n := depth(t, inboundDLQ); n != 0 {
		t.Fatalf("DLQ has %d messages during the outage", n)
	}
	if n := depth(t, inboundQueue); n != len(queued) {
		t.Fatalf("inbound queue depth %d during the outage, want %d untouched", n, len(queued))
	}

	mustCompose(t, "start", "postgres")
	restarted = true
	ctx := ctxT(t)
	for _, in := range instances {
		if err := waitReady(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	// The client retries the 503 with the same key.
	if r := submit(t, instances[1], op(w, "BET", "5.00", httpID)); r.Status != http.StatusOK || r.Body["idempotentReplay"] == true {
		t.Fatalf("retry after the outage: %d %s", r.Status, r.Raw)
	}
	for _, id := range queued {
		waitStatus(t, id, "PROCESSED", time.Minute)
	}
	if n := depth(t, inboundDLQ); n != 0 {
		t.Fatalf("DLQ has %d messages", n)
	}
	assertWallets(t, map[string]string{w.ID: "40.00"}, w)
}

// TestSQSOutage: CHALLENGE §3, SQS temporarily unavailable. MiniStack is
// frozen (docker pause; a restart would wipe its in-memory queues, which
// real SQS never does). Readiness fails, HTTP operations still commit with
// their events in the outbox, and once SQS is back every event is
// published with its original eventId.
func TestSQSOutage(t *testing.T) {
	w := openWallet(t, instances[0], "50.00")
	mustCompose(t, "pause", "ministack")
	paused := true
	defer func() {
		if paused {
			mustCompose(t, "unpause", "ministack")
		}
	}()
	waitFor(t, 30*time.Second, "all instances not ready", func() bool {
		for _, in := range instances {
			if readyStatus(in) != http.StatusServiceUnavailable {
				return false
			}
		}
		return true
	})
	for i := range 6 {
		if r := submit(t, instances[i%3], op(w, "BET", "1.00", ext("sqs-down"))); r.Status != http.StatusOK {
			t.Fatalf("bet %d during the SQS outage: %d %s", i, r.Status, r.Raw)
		}
	}
	waitFor(t, 30*time.Second, "publish attempts to fail", func() bool {
		return count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND published_at IS NULL AND attempts > 0`, w.ID) > 0
	})
	time.Sleep(10 * time.Second)
	mustCompose(t, "unpause", "ministack")
	paused = false
	ctx := ctxT(t)
	for _, in := range instances {
		if err := waitReady(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	assertWallets(t, map[string]string{w.ID: "44.00"}, w)
}
