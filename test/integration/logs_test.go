//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestLogsCarryIdsWithoutSecrets: CHALLENGE §12. The JSON logs carry the
// ids that trace an operation (correlationId, messageId, transactionId,
// walletId, providerId) over HTTP and SQS, and never a credential or a full
// payload. The stack logs through the production logger (logging.New).
func TestLogsCarryIdsWithoutSecrets(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	e := newAPIEnv(t, withLogs(logs))
	q := newInboundQueues(t, 60, 10)
	startConsumer(t, e, q, consumerOpts{})
	w := e.openWallet(t, "100.00")

	httpBody := op(w, "provider-a", "BET", "10.00", "log-http")
	viaHTTP := e.call(t, http.MethodPost, "/wagering/transactions", "provider-a", httpBody,
		"Idempotency-Key", "provider-a:log-http", "X-Correlation-Id", "corr-log-1")
	if viaHTTP.Status != http.StatusOK {
		t.Fatalf("http: %d %s", viaHTTP.Status, viaHTTP.Raw)
	}
	sqsBody := envelope(t, "msg-log-1", op(w, "provider-a", "BET", "5.00", "log-sqs"))
	q.send(t, sqsBody, w.ID)
	viaSQS := e.waitStatus(t, "provider-a", "provider-a", "log-sqs", "PROCESSED", 15*time.Second)

	// A refused token is logged with its reason, never the token.
	const forged = "eyJhbGciOiJSUzI1NiJ9.forged-claims.forged-signature"
	if r := e.call(t, http.MethodGet, "/wagering/transactions/"+uuid.NewString(), "", nil, "Authorization", "Bearer "+forged); r.Status != http.StatusUnauthorized {
		t.Fatalf("forged token: %d", r.Status)
	}

	if got := logs.find(t, "wager transaction submitted", map[string]string{
		"correlationId": "corr-log-1", "transactionId": viaHTTP.str("transactionId"), "walletId": w.ID, "providerId": "provider-a",
	}); len(got) != 1 {
		t.Errorf("HTTP submission log records with the ids = %d, want 1", len(got))
	}
	if got := logs.find(t, "wager transaction submitted", map[string]string{
		"messageId": "msg-log-1", "correlationId": "msg-log-1", "transactionId": viaSQS.str("transactionId"), "walletId": w.ID, "providerId": "provider-a",
	}); len(got) != 1 {
		t.Errorf("SQS submission log records with the ids = %d, want 1", len(got))
	}
	if got := logs.find(t, "authentication failed", nil); len(got) == 0 {
		t.Error("the refused token was not logged")
	}

	rawHTTP, err := json.Marshal(httpBody)
	if err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{
		"forged token": forged, "HTTP body": string(rawHTTP), "SQS body": sqsBody, "player id": w.PlayerID,
		"bearer scheme": "Bearer ",
	}
	for client, token := range e.tokens {
		secrets["token of "+client] = token
	}
	lines, _ := logs.lines(t)
	if len(lines) == 0 {
		t.Fatal("nothing was logged")
	}
	for _, line := range lines {
		for what, s := range secrets {
			if strings.Contains(line, s) {
				t.Errorf("a log line carries the %s: %s", what, line)
			}
		}
	}
}
