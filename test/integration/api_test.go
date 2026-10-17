//go:build integration

package integration

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/adapters/postgres"
	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
)

func TestAPIAuthentication(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	w := e.openWallet(t, "100.00")
	routes := []struct{ method, path string }{
		{http.MethodPost, "/wagering/transactions"},
		{http.MethodGet, "/wagering/transactions/" + uuid.NewString()},
		{http.MethodGet, "/providers/provider-a/wagering/transactions/x"},
		{http.MethodPost, "/wallets"},
		{http.MethodGet, "/wallets/" + w.ID},
		{http.MethodGet, "/wallets/" + w.ID + "/ledger"},
		{http.MethodPost, "/wallets/" + w.ID + "/reconciliation"},
	}
	for _, rt := range routes {
		r := e.call(t, rt.method, rt.path, "", "{}")
		if r.Status != http.StatusUnauthorized || r.errorCode() != "UNAUTHENTICATED" || r.Header.Get("WWW-Authenticate") != `Bearer error="invalid_token"` {
			t.Errorf("%s %s without token: %d %s", rt.method, rt.path, r.Status, r.Raw)
		}
		r = e.call(t, rt.method, rt.path, "", "{}", "Authorization", "Bearer forged.token.value")
		if r.Status != http.StatusUnauthorized {
			t.Errorf("%s %s with a forged token: %d", rt.method, rt.path, r.Status)
		}
	}
	if r := e.call(t, http.MethodGet, "/health/live", "", nil); r.Status != http.StatusOK {
		t.Errorf("/health/live must stay public, got %d", r.Status)
	}
	if n := e.count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND origin = 'EXTERNAL'`, w.ID); n != 0 {
		t.Fatalf("unauthenticated calls left %d transactions", n)
	}
}

func TestAPIWallets(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)

	t.Run("open with balance", func(t *testing.T) {
		player := uuid.NewString()
		r := e.call(t, http.MethodPost, "/wallets", "wallet-internal", map[string]any{
			"playerId": player, "initialBalance": map[string]string{"amount": "1000.00", "currency": "BRL"},
		})
		if r.Status != http.StatusCreated || r.str("playerId") != player || r.amount("balance") != "1000.00" || r.Body["version"] != float64(1) {
			t.Fatalf("open: %d %s", r.Status, r.Raw)
		}
		if r.Header.Get("Location") != "/wallets/"+r.str("id") {
			t.Errorf("Location = %q", r.Header.Get("Location"))
		}
		get := e.call(t, http.MethodGet, "/wallets/"+r.str("id"), "wallet-internal", nil)
		if get.Status != http.StatusOK || get.amount("balance") != "1000.00" {
			t.Errorf("get: %d %s", get.Status, get.Raw)
		}
		if n := e.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, r.str("id")); n != 2 {
			t.Errorf("opening outbox events = %d, want 2", n)
		}
		e.assertConsistent(t, apiWallet{ID: r.str("id"), PlayerID: player}, "1000.00")
	})

	t.Run("zero balance creates no opening", func(t *testing.T) {
		w := e.openWallet(t, "0.00")
		if n := e.count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1`, w.ID); n != 0 {
			t.Errorf("zero opening created %d transactions", n)
		}
		e.assertConsistent(t, w, "0.00")
	})

	t.Run("second wallet for the player conflicts", func(t *testing.T) {
		w := e.openWallet(t, "1.00")
		r := e.call(t, http.MethodPost, "/wallets", "wallet-internal", map[string]any{
			"playerId": w.PlayerID, "initialBalance": map[string]string{"amount": "5.00", "currency": "BRL"},
		})
		if r.Status != http.StatusConflict || r.errorCode() != "WALLET_ALREADY_EXISTS" {
			t.Fatalf("duplicate: %d %s", r.Status, r.Raw)
		}
	})

	t.Run("invalid input", func(t *testing.T) {
		cases := map[string]struct {
			body string
			code string
		}{
			"amount as JSON number": {`{"playerId":"` + uuid.NewString() + `","initialBalance":{"amount":10.00,"currency":"BRL"}}`, "INVALID_MONEY"},
			"negative amount":       {`{"playerId":"` + uuid.NewString() + `","initialBalance":{"amount":"-1.00","currency":"BRL"}}`, "INVALID_MONEY"},
			"unsupported currency":  {`{"playerId":"` + uuid.NewString() + `","initialBalance":{"amount":"1.00","currency":"EUR"}}`, "UNSUPPORTED_CURRENCY"},
			"bad player id":         {`{"playerId":"player-1","initialBalance":{"amount":"1.00","currency":"BRL"}}`, "INVALID_REQUEST"},
			"unknown field":         {`{"playerId":"` + uuid.NewString() + `","initialBalance":{"amount":"1.00","currency":"BRL"},"x":1}`, "INVALID_REQUEST"},
			"malformed JSON":        {`{"playerId":`, "INVALID_REQUEST"},
			"missing balance":       {`{"playerId":"` + uuid.NewString() + `"}`, "INVALID_MONEY"},
		}
		for name, c := range cases {
			r := e.call(t, http.MethodPost, "/wallets", "wallet-internal", c.body)
			if r.Status != http.StatusBadRequest || r.errorCode() != c.code {
				t.Errorf("%s: %d %s, want 400 %s", name, r.Status, r.Raw, c.code)
			}
		}
	})

	t.Run("wallet operations are internal only", func(t *testing.T) {
		w := e.openWallet(t, "1.00")
		for _, c := range []struct{ method, path string }{
			{http.MethodPost, "/wallets"},
			{http.MethodGet, "/wallets/" + w.ID},
			{http.MethodGet, "/wallets/" + w.ID + "/ledger"},
			{http.MethodPost, "/wallets/" + w.ID + "/reconciliation"},
		} {
			r := e.call(t, c.method, c.path, "provider-a", `{"playerId":"`+uuid.NewString()+`","initialBalance":{"amount":"1.00","currency":"BRL"}}`)
			if r.Status != http.StatusForbidden || r.errorCode() != "FORBIDDEN" {
				t.Errorf("provider on %s %s: %d %s", c.method, c.path, r.Status, r.Raw)
			}
		}
	})

	t.Run("unknown and malformed wallet ids", func(t *testing.T) {
		if r := e.call(t, http.MethodGet, "/wallets/"+uuid.NewString(), "wallet-internal", nil); r.Status != http.StatusNotFound || r.errorCode() != "WALLET_NOT_FOUND" {
			t.Errorf("unknown: %d %s", r.Status, r.Raw)
		}
		if r := e.call(t, http.MethodGet, "/wallets/not-a-uuid", "wallet-internal", nil); r.Status != http.StatusBadRequest || r.errorCode() != "INVALID_REQUEST" {
			t.Errorf("malformed: %d %s", r.Status, r.Raw)
		}
	})
}

func TestAPISubmit(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)

	t.Run("processed, then replayed with the original balance", func(t *testing.T) {
		w := e.openWallet(t, "1000.00")
		bet := op(w, "provider-a", "BET", "25.00", "tx-123")
		r := e.submit(t, "provider-a", bet)
		if r.Status != http.StatusOK || r.str("status") != "PROCESSED" || r.amount("balance") != "975.00" || r.Body["idempotentReplay"] != false {
			t.Fatalf("bet: %d %s", r.Status, r.Raw)
		}
		e.submit(t, "provider-a", op(w, "provider-a", "BET", "100.00", "tx-124"))
		replay := e.submit(t, "provider-a", bet)
		if replay.Status != http.StatusOK || replay.Body["idempotentReplay"] != true || replay.amount("balance") != "975.00" || replay.str("transactionId") != r.str("transactionId") {
			t.Fatalf("replay must return the original outcome: %d %s", replay.Status, replay.Raw)
		}
		e.assertConsistent(t, w, "875.00")
	})

	t.Run("conflicts", func(t *testing.T) {
		w := e.openWallet(t, "1000.00")
		e.submit(t, "provider-a", op(w, "provider-a", "BET", "25.00", "c-1"))
		changed := op(w, "provider-a", "BET", "30.00", "c-1")
		if r := e.submit(t, "provider-a", changed); r.Status != http.StatusConflict || r.errorCode() != "IDEMPOTENCY_KEY_CONFLICT" {
			t.Errorf("same key, other payload: %d %s", r.Status, r.Raw)
		}
		same := op(w, "provider-a", "BET", "25.00", "c-1")
		if r := e.call(t, http.MethodPost, "/wagering/transactions", "provider-a", same, "Idempotency-Key", "another-key"); r.Status != http.StatusConflict || r.errorCode() != "EXTERNAL_ID_CONFLICT" {
			t.Errorf("same external id, other key: %d %s", r.Status, r.Raw)
		}
		e.assertConsistent(t, w, "975.00")
	})

	t.Run("input errors persist nothing", func(t *testing.T) {
		w := e.openWallet(t, "100.00")
		usdBody := op(w, "provider-a", "BET", "1.00", "in-usd")
		usdBody["money"] = map[string]string{"amount": "1.00", "currency": "USD"}
		otherPlayer := op(w, "provider-a", "BET", "1.00", "in-player")
		otherPlayer["playerId"] = uuid.NewString()
		missingWallet := op(w, "provider-a", "BET", "1.00", "in-wallet")
		missingWallet["walletId"] = uuid.NewString()
		cases := []struct {
			name   string
			body   any
			key    string
			status int
			code   string
		}{
			{"missing idempotency key", op(w, "provider-a", "BET", "1.00", "in-1"), "", 400, "MISSING_IDEMPOTENCY_KEY"},
			{"opening kind", op(w, "provider-a", "OPENING", "1.00", "in-2"), "k2", 400, "UNSUPPORTED_KIND"},
			{"loss with amount", op(w, "provider-a", "LOSS", "1.00", "in-3"), "k3", 400, "INVALID_AMOUNT_FOR_KIND"},
			{"bet with zero", op(w, "provider-a", "BET", "0.00", "in-4"), "k4", 400, "INVALID_AMOUNT_FOR_KIND"},
			{"refund without reference", op(w, "provider-a", "REFUND", "1.00", "in-5"), "k5", 400, "MISSING_REFERENCE"},
			{"bet with reference", op(w, "provider-a", "BET", "1.00", "in-6", "x"), "k6", 400, "REFERENCE_NOT_ALLOWED"},
			{"amount as JSON number", strings.Replace(mustJSON(t, op(w, "provider-a", "BET", "1.00", "in-7")), `"amount":"1.00"`, `"amount":1.00`, 1), "k7", 400, "INVALID_MONEY"},
			{"scale", op(w, "provider-a", "BET", "1.5", "in-8"), "k8", 400, "INVALID_MONEY"},
			{"currency mismatch", usdBody, "k9", 400, "CURRENCY_MISMATCH"},
			{"player mismatch", otherPlayer, "k10", 400, "PLAYER_WALLET_MISMATCH"},
			{"wallet not found", missingWallet, "k11", 404, "WALLET_NOT_FOUND"},
		}
		for _, c := range cases {
			var headers []string
			if c.key != "" {
				headers = []string{"Idempotency-Key", c.key}
			}
			r := e.call(t, http.MethodPost, "/wagering/transactions", "provider-a", c.body, headers...)
			if r.Status != c.status || r.errorCode() != c.code || r.Body["error"].(map[string]any)["retryable"] != false {
				t.Errorf("%s: %d %s, want %d %s", c.name, r.Status, r.Raw, c.status, c.code)
			}
		}
		if n := e.count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND origin = 'EXTERNAL'`, w.ID); n != 0 {
			t.Fatalf("input errors persisted %d transactions", n)
		}
		e.assertConsistent(t, w, "100.00")
	})

	t.Run("rejection, loss, reversal and pending reference", func(t *testing.T) {
		w := e.openWallet(t, "100.00")
		if r := e.submit(t, "provider-a", op(w, "provider-a", "BET", "150.00", "big")); r.Status != http.StatusUnprocessableEntity || r.str("status") != "REJECTED" ||
			r.str("failureCode") != "INSUFFICIENT_FUNDS" || r.amount("balance") != "100.00" {
			t.Errorf("rejected bet: %d %s", r.Status, r.Raw)
		}
		if r := e.submit(t, "provider-a", op(w, "provider-a", "LOSS", "0.00", "loss")); r.Status != http.StatusOK || r.amount("balance") != "100.00" {
			t.Errorf("loss: %d %s", r.Status, r.Raw)
		}
		e.submit(t, "provider-a", op(w, "provider-a", "BET", "40.00", "bet-1"))
		if r := e.submit(t, "provider-a", op(w, "provider-a", "ROLLBACK", "40.00", "rb-1", "bet-1")); r.Status != http.StatusOK || r.amount("balance") != "100.00" {
			t.Errorf("rollback: %d %s", r.Status, r.Raw)
		}
		if r := e.submit(t, "provider-a", op(w, "provider-a", "REFUND", "40.00", "rf-1", "bet-1")); r.Status != http.StatusUnprocessableEntity || r.str("failureCode") != "ALREADY_REVERSED" {
			t.Errorf("refund of a rolled back bet: %d %s", r.Status, r.Raw)
		}

		r := e.submit(t, "provider-a", op(w, "provider-a", "REFUND", "10.00", "early-refund", "bet-later"))
		loc := r.Header.Get("Location")
		if r.Status != http.StatusAccepted || r.str("status") != "PENDING_REFERENCE" || r.Body["balance"] != nil ||
			loc != "/providers/provider-a/wagering/transactions/early-refund" {
			t.Fatalf("pending reference: %d %s (Location %q)", r.Status, r.Raw, loc)
		}
		follow := e.call(t, http.MethodGet, loc, "provider-a", nil)
		if follow.Status != http.StatusOK || follow.str("status") != "PENDING_REFERENCE" || follow.str("referenceDeadlineAt") == "" {
			t.Errorf("follow Location: %d %s", follow.Status, follow.Raw)
		}
		if replay := e.submit(t, "provider-a", op(w, "provider-a", "REFUND", "10.00", "early-refund", "bet-later")); replay.Status != http.StatusAccepted || replay.Body["idempotentReplay"] != true {
			t.Errorf("pending replay keeps 202: %d %s", replay.Status, replay.Raw)
		}
		e.assertConsistent(t, w, "100.00")
	})

	t.Run("provider mismatch has no effect", func(t *testing.T) {
		w := e.openWallet(t, "100.00")
		r := e.submit(t, "provider-b", op(w, "provider-a", "BET", "10.00", "spoof"))
		if r.Status != http.StatusForbidden || r.errorCode() != "PROVIDER_MISMATCH" {
			t.Fatalf("spoofed providerId: %d %s", r.Status, r.Raw)
		}
		if r := e.submit(t, "wallet-internal", op(w, "provider-a", "BET", "10.00", "internal")); r.Status != http.StatusForbidden || r.errorCode() != "FORBIDDEN" {
			t.Errorf("internal service submitting: %d %s", r.Status, r.Raw)
		}
		if n := e.count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND origin = 'EXTERNAL'`, w.ID); n != 0 {
			t.Fatalf("forbidden submissions persisted %d transactions", n)
		}
		e.assertConsistent(t, w, "100.00")
	})

	t.Run("inactive provider is rejected", func(t *testing.T) {
		w := e.openWallet(t, "100.00")
		if _, err := e.repo.db.owner.Exec(ctxT(t), `UPDATE providers SET active = false WHERE provider_id = 'provider-b'`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = e.repo.db.owner.Exec(ctxT(t), `UPDATE providers SET active = true WHERE provider_id = 'provider-b'`)
		}()
		if r := e.submit(t, "provider-b", op(w, "provider-b", "BET", "10.00", "inactive")); r.Status != http.StatusForbidden || r.errorCode() != "FORBIDDEN" {
			t.Fatalf("inactive provider: %d %s", r.Status, r.Raw)
		}
		e.assertConsistent(t, w, "100.00")
	})

	t.Run("correlation id is echoed or generated", func(t *testing.T) {
		w := e.openWallet(t, "100.00")
		body := op(w, "provider-a", "BET", "1.00", "corr-1")
		r := e.call(t, http.MethodPost, "/wagering/transactions", "provider-a", body, "Idempotency-Key", "provider-a:corr-1", "X-Correlation-Id", "trace-abc")
		if r.Header.Get("X-Correlation-Id") != "trace-abc" {
			t.Errorf("echo = %q", r.Header.Get("X-Correlation-Id"))
		}
		var stored string
		if err := e.repo.db.owner.QueryRow(ctxT(t), `SELECT correlation_id FROM wager_transactions WHERE external_transaction_id = 'corr-1'`).Scan(&stored); err != nil || stored != "trace-abc" {
			t.Errorf("stored correlation = %q, %v", stored, err)
		}
		if g := e.call(t, http.MethodGet, "/health/live", "", nil).Header.Get("X-Correlation-Id"); g == "" {
			t.Error("no generated correlation id")
		}
	})
}

func TestAPIReadsAndIsolation(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	w := e.openWallet(t, "100.00")
	created := e.submit(t, "provider-a", op(w, "provider-a", "BET", "10.00", "iso-1"))
	id := created.str("transactionId")

	byID := e.call(t, http.MethodGet, "/wagering/transactions/"+id, "provider-a", nil)
	if byID.Status != http.StatusOK || byID.str("status") != "PROCESSED" || byID.amount("balance") != "90.00" || byID.str("providerId") != "provider-a" || byID.Body["walletVersion"] != float64(2) {
		t.Fatalf("own transaction: %d %s", byID.Status, byID.Raw)
	}
	if r := e.call(t, http.MethodGet, "/wagering/transactions/"+id, "provider-b", nil); r.Status != http.StatusNotFound || r.errorCode() != "TRANSACTION_NOT_FOUND" {
		t.Errorf("other provider by id: %d %s", r.Status, r.Raw)
	}
	if r := e.call(t, http.MethodGet, "/providers/provider-a/wagering/transactions/iso-1", "provider-b", nil); r.Status != http.StatusNotFound {
		t.Errorf("other provider's path: %d %s", r.Status, r.Raw)
	}
	if r := e.call(t, http.MethodGet, "/providers/provider-a/wagering/transactions/iso-1", "provider-a", nil); r.Status != http.StatusOK || r.str("transactionId") != id {
		t.Errorf("own path: %d %s", r.Status, r.Raw)
	}
	for _, path := range []string{"/wagering/transactions/" + id, "/providers/provider-a/wagering/transactions/iso-1"} {
		if r := e.call(t, http.MethodGet, path, "wallet-internal", nil); r.Status != http.StatusOK {
			t.Errorf("internal read %s: %d", path, r.Status)
		}
	}
	if r := e.call(t, http.MethodGet, "/wagering/transactions/"+uuid.NewString(), "provider-a", nil); r.Status != http.StatusNotFound {
		t.Errorf("unknown id: %d", r.Status)
	}

	// Replays never cross providers: provider-b sending provider-a's key
	// and external id under its own providerId is a separate operation.
	mine := op(w, "provider-b", "BET", "10.00", "iso-1")
	r := e.call(t, http.MethodPost, "/wagering/transactions", "provider-b", mine, "Idempotency-Key", "provider-a:iso-1")
	if r.Status != http.StatusOK || r.Body["idempotentReplay"] != false || r.str("transactionId") == id {
		t.Errorf("provider-b with provider-a's key: %d %s", r.Status, r.Raw)
	}
	e.assertConsistent(t, w, "80.00")
}

func TestAPILedgerAndReconciliation(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	w := e.openWallet(t, "100.00")
	for i, amount := range []string{"1.00", "2.00", "3.00", "4.00"} {
		e.submit(t, "provider-a", op(w, "provider-a", "BET", amount, "page-"+string(rune('a'+i))))
	}

	var versions []float64
	cursor := ""
	pages := 0
	for {
		path := "/wallets/" + w.ID + "/ledger?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := e.call(t, http.MethodGet, path, "wallet-internal", nil)
		if r.Status != http.StatusOK {
			t.Fatalf("ledger page: %d %s", r.Status, r.Raw)
		}
		for _, raw := range r.Body["entries"].([]any) {
			versions = append(versions, raw.(map[string]any)["walletVersion"].(float64))
		}
		pages++
		cursor = r.str("nextCursor")
		if cursor == "" {
			break
		}
	}
	if pages != 3 || len(versions) != 5 {
		t.Fatalf("pages = %d, entries = %d, want 3 pages of 5 entries", pages, len(versions))
	}
	for i, v := range versions {
		if v != float64(i+1) {
			t.Fatalf("versions = %v, want 1..5 in order", versions)
		}
	}
	if r := e.call(t, http.MethodGet, "/wallets/"+w.ID+"/ledger", "wallet-internal", nil); len(r.Body["entries"].([]any)) != 5 || r.str("nextCursor") != "" {
		t.Errorf("default page: %s", r.Raw)
	}
	for _, q := range []string{"?limit=0", "?limit=abc", "?limit=201", "?cursor=not-base64!", "?cursor=YWJj"} {
		if r := e.call(t, http.MethodGet, "/wallets/"+w.ID+"/ledger"+q, "wallet-internal", nil); r.Status != http.StatusBadRequest || r.errorCode() != "INVALID_REQUEST" {
			t.Errorf("ledger%s: %d %s", q, r.Status, r.Raw)
		}
	}
	if r := e.call(t, http.MethodGet, "/wallets/"+uuid.NewString()+"/ledger", "wallet-internal", nil); r.Status != http.StatusNotFound {
		t.Errorf("unknown wallet ledger: %d", r.Status)
	}

	rec := e.call(t, http.MethodPost, "/wallets/"+w.ID+"/reconciliation", "wallet-internal", nil)
	if rec.Status != http.StatusOK || rec.Body["consistent"] != true || rec.amount("storedBalance") != "90.00" || rec.amount("difference") != "0.00" || rec.Body["checkedEntries"] != float64(5) {
		t.Fatalf("reconciliation: %d %s", rec.Status, rec.Raw)
	}

	// Tamper with the stored balance as the owner (the trigger demands a
	// version bump): reconciliation reports it and changes nothing.
	if _, err := e.repo.db.owner.Exec(ctxT(t), `UPDATE wallets SET balance_minor = balance_minor + 100, version = version + 1 WHERE id = $1`, w.ID); err != nil {
		t.Fatal(err)
	}
	rec = e.call(t, http.MethodPost, "/wallets/"+w.ID+"/reconciliation", "wallet-internal", nil)
	if rec.Status != http.StatusOK || rec.Body["consistent"] != false || rec.amount("storedBalance") != "91.00" || rec.amount("calculatedBalance") != "90.00" || rec.amount("difference") != "1.00" {
		t.Fatalf("divergence: %d %s", rec.Status, rec.Raw)
	}
	if got := e.call(t, http.MethodGet, "/wallets/"+w.ID, "wallet-internal", nil).amount("balance"); got != "91.00" {
		t.Errorf("reconciliation changed the balance to %s", got)
	}
}

func TestAPITransientAndFailed(t *testing.T) {
	t.Parallel()

	t.Run("lock timeout answers 503 and persists nothing", func(t *testing.T) {
		e := newAPIEnv(t, withRunner(func(repo repoEnv) app.TxRunner {
			return postgres.NewTxRunner(repo.db.app, 300*time.Millisecond, testStatementTimeout, nil)
		}))
		w := e.openWallet(t, "100.00")
		release := holdTx(t, e.repo.db.owner, `SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE`, w.ID)
		body := op(w, "provider-a", "BET", "10.00", "busy")
		r := e.submit(t, "provider-a", body)
		if r.Status != http.StatusServiceUnavailable || r.errorCode() != "TEMPORARILY_UNAVAILABLE" ||
			r.Body["error"].(map[string]any)["retryable"] != true || r.Header.Get("Retry-After") != "2" {
			t.Fatalf("busy wallet: %d %s (Retry-After %q)", r.Status, r.Raw, r.Header.Get("Retry-After"))
		}
		release()
		retry := e.submit(t, "provider-a", body)
		if retry.Status != http.StatusOK || retry.Body["idempotentReplay"] != false {
			t.Fatalf("retry after 503 must process (nothing was persisted): %d %s", retry.Status, retry.Raw)
		}
		e.assertConsistent(t, w, "90.00")
	})

	t.Run("invariant violation is recorded as FAILED", func(t *testing.T) {
		armed := &atomic.Bool{}
		e := newAPIEnv(t, withRunner(func(repo repoEnv) app.TxRunner {
			return faultyRunner{inner: repo.runner, armed: armed}
		}))
		w := e.openWallet(t, "100.00")
		armed.Store(true)
		body := op(w, "provider-a", "BET", "10.00", "boom")
		r := e.submit(t, "provider-a", body)
		if r.Status != http.StatusInternalServerError || r.str("status") != "FAILED" || r.str("failureCode") != "INTERNAL_INVARIANT_VIOLATION" {
			t.Fatalf("failed: %d %s", r.Status, r.Raw)
		}
		replay := e.submit(t, "provider-a", body)
		if replay.Status != http.StatusInternalServerError || replay.Body["idempotentReplay"] != true || replay.str("transactionId") != r.str("transactionId") {
			t.Errorf("FAILED replay: %d %s", replay.Status, replay.Raw)
		}
		if r := e.call(t, http.MethodPost, "/wagering/transactions", "provider-a", body, "Idempotency-Key", "new-key"); r.Status != http.StatusConflict || r.errorCode() != "EXTERNAL_ID_CONFLICT" {
			t.Errorf("FAILED blocks the external id: %d %s", r.Status, r.Raw)
		}
		if n := e.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, w.ID); n != 2 {
			t.Errorf("outbox events = %d, want only the opening's 2", n)
		}
		e.assertConsistent(t, w, "100.00")
	})

	// A domain invariant error is category D like a database one: FAILED
	// is recorded and replayed, never answered as a retryable 503
	// (ARCHITECTURE §5, §26).
	assertRecordedFailed := func(t *testing.T, e apiEnv, w apiWallet, ext string) {
		t.Helper()
		body := op(w, "provider-a", "BET", "10.00", ext)
		r := e.submit(t, "provider-a", body)
		if r.Status != http.StatusInternalServerError || r.str("status") != "FAILED" || r.str("failureCode") != "INTERNAL_INVARIANT_VIOLATION" {
			t.Fatalf("failed: %d %s", r.Status, r.Raw)
		}
		replay := e.submit(t, "provider-a", body)
		if replay.Status != http.StatusInternalServerError || replay.Body["idempotentReplay"] != true || replay.str("transactionId") != r.str("transactionId") {
			t.Errorf("FAILED replay: %d %s", replay.Status, replay.Raw)
		}
		if n := e.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, w.ID); n != 2 {
			t.Errorf("outbox events = %d, want only the opening's 2", n)
		}
		e.assertConsistent(t, w, "100.00")
	}

	t.Run("invalid transaction from the domain is recorded as FAILED", func(t *testing.T) {
		armed, other := &atomic.Bool{}, &uuid.UUID{}
		e := newAPIEnv(t, withRunner(func(repo repoEnv) app.TxRunner {
			return wrongWalletRunner{inner: repo.runner, armed: armed, other: other}
		}))
		w, decoy := e.openWallet(t, "100.00"), e.openWallet(t, "50.00")
		*other = uuid.MustParse(decoy.ID)
		armed.Store(true)
		assertRecordedFailed(t, e, w, "wrong-wallet")
		e.assertConsistent(t, decoy, "50.00")
	})

	t.Run("invalid transition from the domain is recorded as FAILED", func(t *testing.T) {
		armed := &atomic.Bool{}
		e := newAPIEnv(t, withRunner(func(repo repoEnv) app.TxRunner {
			return faultyRunner{inner: repo.runner, armed: armed, fault: wager.ErrInvalidTransition}
		}))
		w := e.openWallet(t, "100.00")
		armed.Store(true)
		assertRecordedFailed(t, e, w, "bad-transition")
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := jsonMarshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
