//go:build multiinstance

// Package multiinstance runs the CHALLENGE §13 concurrency and recovery
// scenarios against three independent app processes on the compose stack
// (docker-compose.yml + docker-compose.multi.yml), with real Postgres,
// Keycloak and MiniStack, and simulates the failures of CHALLENGE §3 with
// docker kill/stop/pause (ARCHITECTURE §25).
//
// TestMain starts the stack under its own project name and removes it at
// the end; set MULTIINSTANCE_KEEP_STACK=1 to leave it running.
package multiinstance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medbottle/betting-wallet-go/internal/adapters/sqspublisher"
)

const (
	project     = "wager-wallet-multi"
	keycloakURL = "http://localhost:8080/realms/wagering/protocol/openid-connect/token"
	sqsEndpoint = "http://localhost:4566"
	// Owner role (local default, docker-compose.yml): reads every table
	// for the invariant checks; the app itself uses wallet_app.
	ownerURL      = "postgres://wallet_owner:wallet_owner_local@localhost:5432/wallet?sslmode=disable"
	inboundQueue  = "wager-transactions.fifo"
	inboundDLQ    = "wager-transactions-dlq.fifo"
	eventsQueue   = "wallet-events.fifo"
	referenceTTL  = 8 * time.Second // docker-compose.multi.yml
	readyTimeout  = 5 * time.Minute
	settleTimeout = 3 * time.Minute
)

// instance is one app process.
type instance struct {
	Service string
	BaseURL string
}

var instances = []instance{
	{"app", "http://localhost:8081"},
	{"app-2", "http://localhost:8082"},
	{"app-3", "http://localhost:8083"},
}

var (
	root string
	db   *pgxpool.Pool
	sqsc *sqs.Client
	qURL = map[string]string{}
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	var err error
	if root, err = repoRoot(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if out, err := compose("up", "-d", "--build"); err != nil {
		fmt.Fprintf(os.Stderr, "compose up failed (is the dev stack still running on the same ports?): %v\n%s\n", err, out)
		_, _ = compose("down", "-v", "--remove-orphans")
		return 1
	}
	if os.Getenv("MULTIINSTANCE_KEEP_STACK") != "1" {
		defer func() {
			if out, err := compose("down", "-v", "--remove-orphans"); err != nil {
				fmt.Fprintf(os.Stderr, "compose down: %v\n%s\n", err, out)
			}
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	defer cancel()
	for _, in := range instances {
		if err := waitReady(ctx, in); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if db, err = pgxpool.New(ctx, ownerURL); err != nil {
		fmt.Fprintln(os.Stderr, "connect postgres:", err)
		return 1
	}
	defer db.Close()
	sqsc = sqspublisher.NewClient(sqspublisher.ClientConfig{Endpoint: sqsEndpoint, Region: "us-east-1", AccessKeyID: "test", SecretAccessKey: "test"})
	for _, q := range []string{inboundQueue, inboundDLQ, eventsQueue} {
		if qURL[q], err = sqspublisher.ResolveQueueURL(ctx, sqsc, q); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return m.Run()
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// compose runs docker compose on the multi-instance project.
func compose(args ...string) (string, error) {
	full := append([]string{"compose", "-p", project, "-f", "docker-compose.yml", "-f", "docker-compose.multi.yml"}, args...)
	cmd := exec.Command("docker", full...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustCompose(t *testing.T, args ...string) {
	t.Helper()
	if out, err := compose(args...); err != nil {
		t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// kill stops an instance abruptly (SIGKILL: no drain, no deferred work).
func kill(t *testing.T, service string) {
	t.Helper()
	mustCompose(t, "kill", "-s", "SIGKILL", service)
}

// restart starts a stopped service again and waits for its readiness.
func restart(t *testing.T, services ...string) {
	t.Helper()
	mustCompose(t, append([]string{"start"}, services...)...)
	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	defer cancel()
	for _, in := range instances {
		for _, s := range services {
			if in.Service == s {
				if err := waitReady(ctx, in); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func readyStatus(in instance) int {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(in.BaseURL + "/health/ready")
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

func waitReady(ctx context.Context, in instance) error {
	for {
		if readyStatus(in) == http.StatusOK {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s not ready: %w", in.Service, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// waitFor polls cond every 200ms until it holds or timeout passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---------------------------------------------------------------- tokens

var tokens struct {
	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	value string
	exp   time.Time
}

// token returns a client_credentials token for a realm client (local test
// secrets, deploy/keycloak/realm-wagering.json), cached until shortly
// before it expires.
func token(t *testing.T, client string) string {
	t.Helper()
	tokens.mu.Lock()
	defer tokens.mu.Unlock()
	if tokens.cache == nil {
		tokens.cache = map[string]cachedToken{}
	}
	if c, ok := tokens.cache[client]; ok && time.Until(c.exp) > 30*time.Second {
		return c.value
	}
	resp, err := http.PostForm(keycloakURL, url.Values{
		"grant_type": {"client_credentials"}, "client_id": {client}, "client_secret": {client + "-local-secret"},
	})
	if err != nil {
		t.Fatalf("token for %s: %v", client, err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.AccessToken == "" {
		t.Fatalf("token for %s: status %d, %v", client, resp.StatusCode, err)
	}
	tokens.cache[client] = cachedToken{body.AccessToken, time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)}
	return body.AccessToken
}

// ---------------------------------------------------------------- HTTP

type response struct {
	Status int
	Body   map[string]any
	Raw    []byte
}

func (r response) str(key string) string {
	v, _ := r.Body[key].(string)
	return v
}

func (r response) amount(key string) string {
	m, _ := r.Body[key].(map[string]any)
	v, _ := m["amount"].(string)
	return v
}

func (r response) errorCode() string {
	e, _ := r.Body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// call sends a request to one instance; headers are pairs. A transport
// error is returned as status 0 (an instance may be down on purpose).
func call(t *testing.T, in instance, method, path, client string, body any, headers ...string) response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, in.BaseURL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token(t, client))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return response{Status: 0, Raw: []byte(err.Error())}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := response{Status: resp.StatusCode, Raw: raw}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

type wallet struct{ ID, PlayerID string }

func openWallet(t *testing.T, in instance, balance string) wallet {
	t.Helper()
	player := uuid.NewString()
	r := call(t, in, http.MethodPost, "/wallets", "wallet-internal", map[string]any{
		"playerId": player, "initialBalance": map[string]string{"amount": balance, "currency": "BRL"},
	})
	if r.Status != http.StatusCreated {
		t.Fatalf("open wallet on %s: %d %s", in.Service, r.Status, r.Raw)
	}
	return wallet{ID: r.str("id"), PlayerID: player}
}

// op builds a submission for provider-a.
func op(w wallet, kind, amount, externalID string, ref ...string) map[string]any {
	b := map[string]any{
		"providerId": "provider-a", "externalTransactionId": externalID, "playerId": w.PlayerID, "walletId": w.ID,
		"roundId": "round-1", "gameId": "fortune-chimp", "kind": kind,
		"money": map[string]string{"amount": amount, "currency": "BRL"},
	}
	if len(ref) > 0 {
		b["referenceExternalTransactionId"] = ref[0]
	}
	return b
}

func submit(t *testing.T, in instance, body map[string]any) response {
	t.Helper()
	return call(t, in, http.MethodPost, "/wagering/transactions", "provider-a", body,
		"Idempotency-Key", fmt.Sprintf("provider-a:%s", body["externalTransactionId"]))
}

func getOp(t *testing.T, in instance, externalID string) response {
	t.Helper()
	return call(t, in, http.MethodGet, "/providers/provider-a/wagering/transactions/"+externalID, "provider-a", nil)
}

// waitStatus polls the provider read endpoint, rotating over the
// instances that answer, until externalID reaches want.
func waitStatus(t *testing.T, externalID, want string, timeout time.Duration) response {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for i := 0; ; i++ {
		r := getOp(t, instances[i%len(instances)], externalID)
		if r.Status == http.StatusOK && r.str("status") == want {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not reach %s: %d %s", externalID, want, r.Status, r.Raw)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- SQS

// envelope wraps an op as the CHALLENGE §10 message.
func envelope(t *testing.T, messageID string, body map[string]any) string {
	t.Helper()
	data := map[string]any{}
	for k, v := range body {
		data[k] = v
	}
	data["idempotencyKey"] = fmt.Sprintf("provider-a:%s", body["externalTransactionId"])
	raw, err := json.Marshal(map[string]any{
		"messageId": messageID, "type": "WagerTransactionRequested", "occurredAt": "2026-09-08T12:00:00.000Z", "data": data,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// sendSQS publishes to the inbound queue with a fresh deduplication id, so
// a repeated envelope is a delivery the broker cannot drop.
func sendSQS(t *testing.T, body, group string) {
	t.Helper()
	if _, err := sqsc.SendMessage(ctxT(t), &sqs.SendMessageInput{
		QueueUrl: aws.String(qURL[inboundQueue]), MessageBody: aws.String(body),
		MessageGroupId: aws.String(group), MessageDeduplicationId: aws.String(uuid.NewString()),
	}); err != nil {
		t.Fatal(err)
	}
}

// depth is visible + in-flight messages.
func depth(t *testing.T, queue string) int {
	t.Helper()
	out, err := sqsc.GetQueueAttributes(ctxT(t), &sqs.GetQueueAttributesInput{QueueUrl: aws.String(qURL[queue]),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible}})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := strconv.Atoi(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)])
	b, _ := strconv.Atoi(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible)])
	return a + b
}

// ---------------------------------------------------------------- invariants

func count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(ctxT(t), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertWallets checks, for each wallet, after the system settled:
//   - no operation left PENDING or PENDING_REFERENCE;
//   - reconciliation consistent (stored balance = ledger fold) with the
//     expected balance when given, and never negative;
//   - exactly one ledger entry per PROCESSED operation that moves money,
//     and none for anything else (no duplicated movement);
//   - every outbox event published, none parked, and each one received
//     on wallet-events.fifo (no event lost after its commit).
func assertWallets(t *testing.T, want map[string]string, wallets ...wallet) {
	t.Helper()
	ids := make([]string, len(wallets))
	for i, w := range wallets {
		ids[i] = w.ID
	}
	waitFor(t, settleTimeout, "operations to settle", func() bool {
		return count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id = ANY($1::uuid[]) AND status IN ('PENDING','PENDING_REFERENCE')`, ids) == 0
	})
	for i, w := range wallets {
		r := call(t, instances[i%len(instances)], http.MethodPost, "/wallets/"+w.ID+"/reconciliation", "wallet-internal", nil)
		if r.Status != http.StatusOK || r.Body["consistent"] != true || strings.HasPrefix(r.amount("storedBalance"), "-") {
			t.Errorf("reconciliation of %s: %d %s", w.ID, r.Status, r.Raw)
		}
		if exp, ok := want[w.ID]; ok && r.amount("storedBalance") != exp {
			t.Errorf("wallet %s balance %s, want %s", w.ID, r.amount("storedBalance"), exp)
		}
		moved := count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND status = 'PROCESSED' AND kind <> 'LOSS'`, w.ID)
		entries := count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, w.ID)
		if moved != entries {
			t.Errorf("wallet %s: %d money-moving PROCESSED operations but %d ledger entries", w.ID, moved, entries)
		}
	}
	assertEventsDelivered(t, ids)
}

// delivered remembers every eventId read from wallet-events.fifo during the
// run: a drain for one test also consumes other wallets' events, and a
// later check must still find them.
var delivered struct {
	mu   sync.Mutex
	seen map[string]bool
}

// assertEventsDelivered waits for the outbox of the wallets to drain, then
// reads wallet-events.fifo until every published eventId was received.
// Repeats are allowed (at-least-once) and logged.
func assertEventsDelivered(t *testing.T, walletIDs []string) {
	t.Helper()
	waitFor(t, settleTimeout, "outbox to drain", func() bool {
		return count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = ANY($1::uuid[]) AND published_at IS NULL`, walletIDs) == 0
	})
	if n := count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = ANY($1::uuid[]) AND parked_at IS NOT NULL`, walletIDs); n != 0 {
		t.Errorf("%d events parked", n)
	}
	rows, err := db.Query(ctxT(t), `SELECT event_id::text FROM outbox_events WHERE aggregate_id = ANY($1::uuid[])`, walletIDs)
	if err != nil {
		t.Fatal(err)
	}
	pending := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		pending[id] = true
	}
	rows.Close()
	total := len(pending)
	repeats := 0
	delivered.mu.Lock()
	defer delivered.mu.Unlock()
	if delivered.seen == nil {
		delivered.seen = map[string]bool{}
	}
	seen := delivered.seen
	for id := range pending {
		if seen[id] {
			delete(pending, id)
		}
	}
	deadline := time.Now().Add(settleTimeout)
	for len(pending) > 0 && time.Now().Before(deadline) {
		out, err := sqsc.ReceiveMessage(ctxT(t), &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(qURL[eventsQueue]), MaxNumberOfMessages: 10, WaitTimeSeconds: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range out.Messages {
			var env struct {
				EventID string `json:"eventId"`
			}
			_ = json.Unmarshal([]byte(aws.ToString(m.Body)), &env)
			if seen[env.EventID] {
				repeats++
			}
			seen[env.EventID] = true
			delete(pending, env.EventID)
			if _, err := sqsc.DeleteMessage(ctxT(t), &sqs.DeleteMessageInput{QueueUrl: aws.String(qURL[eventsQueue]), ReceiptHandle: m.ReceiptHandle}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(pending) > 0 {
		t.Errorf("%d of %d committed events never reached %s", len(pending), total, eventsQueue)
	}
	t.Logf("%d events delivered, %d repeated deliveries", total, repeats)
}
