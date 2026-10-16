//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medbottle/betting-wallet-go/internal/adapters/postgres"
	"github.com/medbottle/betting-wallet-go/internal/adapters/sqspublisher"
	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/app/messaging"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
	"github.com/medbottle/betting-wallet-go/internal/platform/clock"
)

// relayFor builds a relay on runner that publishes to queue at endpoint.
func relayFor(t *testing.T, runner app.TxRunner, endpoint, queue, owner string, mutate ...func(*messaging.RelayConfig)) *messaging.Relay {
	t.Helper()
	pub := sqspublisher.New(sqsClient(endpoint, publisherKey, publisherSecret), queue)
	if err := pub.Resolve(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	return relayWith(runner, pub, owner, mutate...)
}

func relayWith(runner app.TxRunner, pub app.EventPublisher, owner string, mutate ...func(*messaging.RelayConfig)) *messaging.Relay {
	cfg := messaging.RelayConfig{
		Owner: owner, Lease: 30 * time.Second, BatchSize: 10, PublishTimeout: 5 * time.Second,
		BackoffBase: 50 * time.Millisecond, BackoffMax: 200 * time.Millisecond, MaxAttempts: 20,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	return messaging.NewRelay(runner, pub, clock.System{}, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

// relayUntilIdle runs RelayOnce until a cycle finds nothing due.
func relayUntilIdle(t *testing.T, r *messaging.Relay) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		did, err := r.RelayOnce(ctxT(t))
		if err != nil {
			t.Fatalf("relay: %v", err)
		}
		if !did {
			return
		}
	}
	t.Fatal("relay never became idle")
}

type relayRow struct {
	EventID     string
	AggregateID string
	EventType   string
	Published   bool
	Parked      bool
	Attempts    int
	LockedBy    string
	LastError   string
}

func outboxRows(t *testing.T, owner *pgxpool.Pool, where string, args ...any) map[string]relayRow {
	t.Helper()
	rows, err := owner.Query(ctxT(t), `SELECT event_id::text, aggregate_id::text, event_type, published_at IS NOT NULL, parked_at IS NOT NULL,
		attempts, coalesce(locked_by, ''), coalesce(last_error, '') FROM outbox_events WHERE `+where, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]relayRow{}
	for rows.Next() {
		var r relayRow
		if err := rows.Scan(&r.EventID, &r.AggregateID, &r.EventType, &r.Published, &r.Parked, &r.Attempts, &r.LockedBy, &r.LastError); err != nil {
			t.Fatal(err)
		}
		out[r.EventID] = r
	}
	return out
}

func bodyEventID(t *testing.T, body string) string {
	t.Helper()
	var env struct {
		EventID string `json:"eventId"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("message body is not the envelope: %v: %s", err, body)
	}
	return env.EventID
}

// TestOutboxRelayPublishes: committed events reach the queue with the
// routing of ARCHITECTURE §10.
func TestOutboxRelayPublishes(t *testing.T) {
	t.Parallel()
	endpoint := startMiniStack(t)
	e := newAPIEnv(t)
	queue := newEventsQueue(t, endpoint)
	w := e.openWallet(t, "100.00")
	e.submit(t, "provider-a", op(w, "provider-a", "BET", "25.00", "pub-bet"))

	relayUntilIdle(t, relayFor(t, e.repo.runner, endpoint, queue, "relay-1"))

	rows := outboxRows(t, e.repo.db.owner, `aggregate_id = $1`, w.ID)
	if len(rows) != 4 {
		t.Fatalf("outbox rows = %d, want 4 (opening and bet: processed + balance changed each)", len(rows))
	}
	msgs := drain(t, endpoint, queue, len(rows), 10*time.Second)
	if len(msgs) != len(rows) {
		t.Fatalf("received %d messages, want %d", len(msgs), len(rows))
	}
	for _, m := range msgs {
		id := bodyEventID(t, m.Body)
		row, ok := rows[id]
		if !ok {
			t.Fatalf("message for unknown event %s", id)
		}
		if m.DeduplicationID != id || m.GroupID != w.ID || m.EventType != row.EventType || m.EventVersion != "1" {
			t.Errorf("routing of %s: group %q dedup %q type %q version %q", id, m.GroupID, m.DeduplicationID, m.EventType, m.EventVersion)
		}
		if !row.Published || row.LockedBy != "relay-1" || row.Attempts != 1 {
			t.Errorf("row %s after publishing: %+v", id, row)
		}
	}
}

// TestOutboxNeverPublishesBeforeCommit: rows of an open transaction are
// invisible to the relay; they are published once committed (CLAUDE.md,
// CHALLENGE §5.4).
func TestOutboxNeverPublishesBeforeCommit(t *testing.T) {
	t.Parallel()
	endpoint := startMiniStack(t)
	env := newRepoEnv(t)
	queue := newEventsQueue(t, endpoint)
	relay := relayFor(t, env.runner, endpoint, queue, "relay-1")

	res, err := wager.OpenWallet(wager.OpenWalletParams{
		WalletID: env.ids(), PlayerID: env.ids(), InitialBalance: brlAmount(t, "50.00"), CorrelationID: "c", Now: now(), NewID: env.ids,
	})
	if err != nil {
		t.Fatal(err)
	}
	written, commit := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- env.runner.InTx(context.Background(), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			if err := persistOpening(ctx, r, res); err != nil {
				return err
			}
			close(written)
			<-commit
			return nil
		})
	}()
	<-written
	if did, err := relay.RelayOnce(ctxT(t)); err != nil || did {
		t.Fatalf("relay saw uncommitted events: did=%v err=%v", did, err)
	}
	close(commit)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	relayUntilIdle(t, relay)
	if msgs := drain(t, endpoint, queue, 2, 10*time.Second); len(msgs) != 2 {
		t.Fatalf("after commit received %d messages, want 2", len(msgs))
	}
}

// TestOutboxTwoRelaysContend: CHALLENGE §13 item 6. Events committed while
// no relay ran (crash between commit and publish) are taken over by two
// relays with separate pools; every event is published exactly once.
func TestOutboxTwoRelaysContend(t *testing.T) {
	t.Parallel()
	endpoint := startMiniStack(t)
	env := newRepoEnv(t)
	queue := newEventsQueue(t, endpoint)
	for i := 0; i < 50; i++ {
		env.openWallet(t, "10.00") // 2 events each
	}
	pool, err := pgxpool.New(ctxT(t), env.db.app.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	relays := []*messaging.Relay{
		relayFor(t, env.runner, endpoint, queue, "relay-a"),
		relayFor(t, postgres.NewTxRunner(pool, 5*time.Second, testStatementTimeout), endpoint, queue, "relay-b"),
	}
	var wg sync.WaitGroup
	for _, r := range relays {
		wg.Add(1)
		go func(r *messaging.Relay) {
			defer wg.Done()
			relayUntilIdle(t, r)
		}(r)
	}
	wg.Wait()

	rows := outboxRows(t, env.db.owner, `true`)
	owners := map[string]int{}
	for _, r := range rows {
		if !r.Published || r.Attempts != 1 {
			t.Fatalf("row %s: %+v", r.EventID, r)
		}
		owners[r.LockedBy]++
	}
	if len(rows) != 100 || owners["relay-a"] == 0 || owners["relay-b"] == 0 {
		t.Fatalf("rows = %d, per relay = %v; want 100 shared by both relays", len(rows), owners)
	}
	msgs := drain(t, endpoint, queue, 100, 30*time.Second)
	seen := map[string]int{}
	for _, m := range msgs {
		seen[bodyEventID(t, m.Body)]++
	}
	if len(msgs) != 100 || len(seen) != 100 {
		t.Fatalf("received %d messages with %d distinct event ids, want 100 and 100", len(msgs), len(seen))
	}
}

// TestOutboxRecoversBetweenPublishAndConfirm: a relay publishes and dies
// before marking. After its lease expires another relay republishes with
// the same eventId; the dead relay can no longer mark the rows.
func TestOutboxRecoversBetweenPublishAndConfirm(t *testing.T) {
	t.Parallel()
	endpoint := startMiniStack(t)
	env := newRepoEnv(t)
	queue := newEventsQueue(t, endpoint)
	w := env.openWallet(t, "10.00")

	// The dead relay: claims with a short lease, publishes, never marks.
	var claimed []app.OutboxRecord
	if err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
		var err error
		claimed, err = r.Outbox().ClaimBatch(ctx, "dead-relay", 300*time.Millisecond, 10)
		return err
	}); err != nil || len(claimed) != 2 {
		t.Fatalf("claim: %d rows, %v", len(claimed), err)
	}
	pub := sqspublisher.New(sqsClient(endpoint, publisherKey, publisherSecret), queue)
	if err := pub.Resolve(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	events := make([]app.OutboundEvent, len(claimed))
	for i, c := range claimed {
		events[i] = app.OutboundEvent{EventID: c.EventID, GroupID: c.AggregateID.String(), EventType: c.EventType, EventVersion: c.EventVersion, Body: c.Payload}
	}
	if res, err := pub.PublishBatch(ctxT(t), events); err != nil || res[0].Err != nil || res[1].Err != nil {
		t.Fatalf("dead relay publish: %v %v", res, err)
	}

	relay := relayFor(t, env.runner, endpoint, queue, "relay-2")
	if did, err := relay.RelayOnce(ctxT(t)); err != nil || did {
		t.Fatalf("rows taken while the lease was still valid: did=%v err=%v", did, err)
	}
	time.Sleep(400 * time.Millisecond) // the lease expires
	relayUntilIdle(t, relay)

	for _, r := range outboxRows(t, env.db.owner, `aggregate_id = $1`, w.ID()) {
		if !r.Published || r.LockedBy != "relay-2" || r.Attempts != 2 {
			t.Errorf("row after takeover: %+v", r)
		}
	}
	if err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
		held, err := r.Outbox().MarkPublished(ctx, "dead-relay", claimed[0].EventID)
		if held {
			t.Error("the dead relay marked a row it no longer holds")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Republication keeps the eventId: whatever the broker delivers carries
	// only the two original ids (FIFO deduplication may collapse the copies).
	msgs := drain(t, endpoint, queue, 2, 10*time.Second)
	ids := map[string]bool{}
	for _, m := range msgs {
		id := bodyEventID(t, m.Body)
		if id != m.DeduplicationID {
			t.Errorf("dedup id %s for event %s", m.DeduplicationID, id)
		}
		ids[id] = true
	}
	if len(ids) != 2 || !ids[claimed[0].EventID.String()] || !ids[claimed[1].EventID.String()] {
		t.Fatalf("received event ids %v, want exactly the 2 originals", ids)
	}
	t.Logf("received %d messages for 2 events (copies collapsed by FIFO deduplication: %v)", len(msgs), len(msgs) == 2)
}

// TestOutboxBackoffAndPark: with the broker unreachable, attempts grow with
// backoff and the row is parked at the limit, then never claimed again.
func TestOutboxBackoffAndPark(t *testing.T) {
	t.Parallel()
	env := newRepoEnv(t)
	w := env.openWallet(t, "10.00")
	unreachable := sqspublisher.New(sqsClient("http://127.0.0.1:1", publisherKey, publisherSecret), "wallet-events.fifo")
	// Resolution would fail too; publish against a stub URL to exercise the
	// whole-call failure path.
	relay := relayWith(env.runner, urlStub{unreachable}, "relay-1", func(c *messaging.RelayConfig) { c.MaxAttempts = 3 })

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_, _ = relay.RelayOnce(ctxT(t))
		rows := outboxRows(t, env.db.owner, `aggregate_id = $1`, w.ID())
		parked := 0
		for _, r := range rows {
			if r.Parked {
				parked++
			}
		}
		if parked == len(rows) {
			for _, r := range rows {
				if r.Attempts != 3 || r.LastError == "" || r.Published {
					t.Fatalf("parked row: %+v", r)
				}
			}
			if did, _ := relay.RelayOnce(ctxT(t)); did {
				t.Fatal("parked rows were claimed again")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("rows not parked: %+v", outboxRows(t, env.db.owner, `aggregate_id = $1`, w.ID()))
}

// urlStub publishes through a publisher whose queue was never resolved,
// i.e. every call fails as a whole.
type urlStub struct{ p *sqspublisher.Publisher }

func (u urlStub) PublishBatch(ctx context.Context, events []app.OutboundEvent) ([]app.PublishResult, error) {
	return u.p.PublishBatch(ctx, events)
}

// TestOutboxPartialFailure covers per-entry results, which a healthy
// emulator does not produce on demand: a fake publisher rejects every
// other entry. Postgres is real; only the broker response is scripted.
func TestOutboxPartialFailure(t *testing.T) {
	t.Parallel()
	env := newRepoEnv(t)
	for i := 0; i < 3; i++ {
		env.openWallet(t, "10.00")
	}
	pub := &scriptedPublisher{}
	relay := relayWith(env.runner, pub, "relay-1", func(c *messaging.RelayConfig) { c.BackoffBase, c.BackoffMax = time.Hour, time.Hour })
	if did, err := relay.RelayOnce(ctxT(t)); !did || err != nil {
		t.Fatalf("relay: did=%v err=%v", did, err)
	}
	rows := outboxRows(t, env.db.owner, `true`)
	published, rescheduled := 0, 0
	for _, r := range rows {
		switch {
		case pub.failed[r.EventID]:
			rescheduled++
			if r.Published || r.LastError != "InvalidParameterValue: scripted" || r.Attempts != 1 {
				t.Errorf("failed entry: %+v", r)
			}
		default:
			published++
			if !r.Published {
				t.Errorf("successful entry not marked: %+v", r)
			}
		}
	}
	if published != 3 || rescheduled != 3 {
		t.Fatalf("published %d, rescheduled %d; want 3 and 3", published, rescheduled)
	}
	if did, _ := relay.RelayOnce(ctxT(t)); did {
		t.Fatal("rescheduled entries claimed before their backoff")
	}
}

type scriptedPublisher struct {
	mu     sync.Mutex
	failed map[string]bool
}

func (s *scriptedPublisher) PublishBatch(_ context.Context, events []app.OutboundEvent) ([]app.PublishResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed == nil {
		s.failed = map[string]bool{}
	}
	out := make([]app.PublishResult, len(events))
	for i, e := range events {
		out[i].EventID = e.EventID
		if i%2 == 1 {
			out[i].Err, out[i].SenderFault = errors.New("InvalidParameterValue: scripted"), true
			s.failed[e.EventID.String()] = true
		}
	}
	return out, nil
}

// TestSQSReadiness: the readiness probe reports the queue.
func TestSQSReadiness(t *testing.T) {
	t.Parallel()
	endpoint := startMiniStack(t)
	ok := sqspublisher.New(sqsClient(endpoint, publisherKey, publisherSecret), "wallet-events.fifo")
	if err := ok.Resolve(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if err := ok.HealthCheck(ctxT(t)); err != nil {
		t.Errorf("healthy broker: %v", err)
	}
	down := sqspublisher.NewClient(sqspublisher.ClientConfig{Endpoint: "http://127.0.0.1:1", Region: "us-east-1", AccessKeyID: "x", SecretAccessKey: "y",
		HTTPClient: &http.Client{Timeout: time.Second}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := down.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("wallet-events.fifo")}); err == nil {
		t.Error("unreachable broker answered")
	}
	if err := sqspublisher.New(down, "wallet-events.fifo").HealthCheck(ctx); err == nil {
		t.Error("unresolved queue reported healthy")
	}
}

// TestBrokerCredentialSeparation is the negative test of ARCHITECTURE §10:
// the publisher's credentials must not receive from the inbound queue. The
// emulator is checked first; if it does not enforce IAM the test is skipped
// with that reason, as §10 requires.
func TestBrokerCredentialSeparation(t *testing.T) {
	t.Parallel()
	endpoint := startMiniStack(t)
	client := sqsClient(endpoint, publisherKey, publisherSecret)
	url, err := sqspublisher.ResolveQueueURL(ctxT(t), client, "wager-transactions.fifo")
	if err == nil {
		_, err = client.ReceiveMessage(ctxT(t), &sqs.ReceiveMessageInput{QueueUrl: aws.String(url), WaitTimeSeconds: 0})
	}
	if err == nil {
		bogus := sqsClient(endpoint, "nobody-"+uuid.NewString(), "wrong-secret")
		_, bogusErr := bogus.ListQueues(ctxT(t), &sqs.ListQueuesInput{})
		t.Skip(fmt.Sprintf("MiniStack does not enforce IAM: the publisher's credentials could ReceiveMessage on the inbound queue "+
			"(unknown credentials accepted too: %v). Credential separation is configured but cannot be verified against this emulator "+
			"(ARCHITECTURE §10/§22 limitation).", bogusErr == nil))
	}
	t.Logf("receive with publisher credentials refused: %v", err)
}
