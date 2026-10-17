//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/adapters/sqsconsumer"
	"github.com/medbottle/betting-wallet-go/internal/adapters/sqspublisher"
	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/app/wagering"
	"github.com/medbottle/betting-wallet-go/internal/platform/clock"
	"github.com/medbottle/betting-wallet-go/internal/platform/health"
	"github.com/medbottle/betting-wallet-go/internal/platform/idgen"
)

// inboundQueues is a source queue and its DLQ private to one test,
// configured like wager-transactions.fifo.
type inboundQueues struct {
	endpoint         string
	queue, dlq       string
	queueURL, dlqURL string
	admin            *sqs.Client
}

func newInboundQueues(t *testing.T, visibility, maxReceive int) inboundQueues {
	t.Helper()
	endpoint := startMiniStack(t)
	admin := sqsClient(endpoint, readerKey, readerSecret)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	q := inboundQueues{endpoint: endpoint, queue: "in-" + suffix + ".fifo", dlq: "in-" + suffix + "-dlq.fifo", admin: admin}
	fifo := map[string]string{"FifoQueue": "true", "ContentBasedDeduplication": "false", "VisibilityTimeout": strconv.Itoa(visibility)}
	out, err := admin.CreateQueue(ctxT(t), &sqs.CreateQueueInput{QueueName: aws.String(q.dlq), Attributes: fifo})
	if err != nil {
		t.Fatal(err)
	}
	q.dlqURL = aws.ToString(out.QueueUrl)
	attrs, err := admin.GetQueueAttributes(ctxT(t), &sqs.GetQueueAttributesInput{QueueUrl: out.QueueUrl,
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}})
	if err != nil {
		t.Fatal(err)
	}
	src := map[string]string{
		"FifoQueue": "true", "ContentBasedDeduplication": "false", "VisibilityTimeout": strconv.Itoa(visibility),
		"RedrivePolicy": fmt.Sprintf(`{"deadLetterTargetArn":"%s","maxReceiveCount":"%d"}`, attrs.Attributes["QueueArn"], maxReceive),
	}
	out, err = admin.CreateQueue(ctxT(t), &sqs.CreateQueueInput{QueueName: aws.String(q.queue), Attributes: src})
	if err != nil {
		t.Fatal(err)
	}
	q.queueURL = aws.ToString(out.QueueUrl)
	return q
}

// send publishes body as provider-a's producer would; dedup defaults to a
// fresh id so tests can prove redeliveries that SQS itself would not drop.
func (q inboundQueues) send(t *testing.T, body, group string) {
	t.Helper()
	q.sendAs(t, providerAKey, providerASecret, body, group, "")
}

// sendAs publishes body with the given broker credentials, naming
// originalSender in originalSenderId when it is not empty.
func (q inboundQueues) sendAs(t *testing.T, key, secret, body, group, originalSender string) {
	t.Helper()
	in := &sqs.SendMessageInput{
		QueueUrl: aws.String(q.queueURL), MessageBody: aws.String(body),
		MessageGroupId: aws.String(group), MessageDeduplicationId: aws.String(uuid.NewString()),
	}
	if originalSender != "" {
		in.MessageAttributes = map[string]types.MessageAttributeValue{
			sqsconsumer.AttrOriginalSenderID: {DataType: aws.String("String"), StringValue: aws.String(originalSender)},
		}
	}
	if _, err := sqsClient(q.endpoint, key, secret).SendMessage(ctxT(t), in); err != nil {
		t.Fatal(err)
	}
}

// depth is visible + in-flight messages of a queue.
func (q inboundQueues) depth(t *testing.T, url string) int {
	t.Helper()
	out, err := q.admin.GetQueueAttributes(ctxT(t), &sqs.GetQueueAttributesInput{QueueUrl: aws.String(url),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible}})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := strconv.Atoi(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)])
	b, _ := strconv.Atoi(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible)])
	return a + b
}

// dlqMessage is one DLQ entry as an operator sees it.
type dlqMessage struct {
	Body, GroupID, DedupID string
	Attrs                  map[string]string
}

// readDLQ receives until want messages were seen, then makes them visible
// again (nothing is deleted).
func (q inboundQueues) readDLQ(t *testing.T, want int, timeout time.Duration) []dlqMessage {
	t.Helper()
	var got []dlqMessage
	var handles []*string
	defer func() {
		for _, h := range handles {
			if _, err := q.admin.ChangeMessageVisibility(ctxT(t), &sqs.ChangeMessageVisibilityInput{
				QueueUrl: aws.String(q.dlqURL), ReceiptHandle: h, VisibilityTimeout: 0}); err != nil {
				t.Errorf("release DLQ message: %v", err)
			}
		}
	}()
	deadline := time.Now().Add(timeout)
	for len(got) < want && time.Now().Before(deadline) {
		out, err := q.admin.ReceiveMessage(ctxT(t), &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(q.dlqURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1, VisibilityTimeout: 30,
			MessageAttributeNames: []string{"All"}, MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range out.Messages {
			d := dlqMessage{Body: aws.ToString(m.Body), GroupID: m.Attributes["MessageGroupId"], DedupID: m.Attributes["MessageDeduplicationId"], Attrs: map[string]string{}}
			for k, v := range m.MessageAttributes {
				d.Attrs[k] = aws.ToString(v.StringValue)
			}
			got = append(got, d)
			handles = append(handles, m.ReceiptHandle)
		}
	}
	return got
}

// toggleReadiness is the consumer's readiness gate under test control.
type toggleReadiness struct {
	ready  atomic.Bool
	checks atomic.Int64
}

func readyGate() *toggleReadiness {
	r := &toggleReadiness{}
	r.ready.Store(true)
	return r
}

func (r *toggleReadiness) Report() health.Report    { return health.Report{Ready: r.ready.Load()} }
func (r *toggleReadiness) CheckNow(context.Context) { r.checks.Add(1) }

type consumerOpts struct {
	runner     app.TxRunner
	readiness  sqsconsumer.Readiness
	api        sqsconsumer.API
	backoffMax time.Duration
	metrics    app.Metrics
}

// startConsumer runs a consumer on q until the test ends (or until the
// returned stop is called). Its use case is e's, or one on opts.runner.
func startConsumer(t *testing.T, e apiEnv, q inboundQueues, opts consumerOpts) (*sqsconsumer.Consumer, func()) {
	t.Helper()
	svc := e.wagering
	if opts.runner != nil {
		svc = wagering.NewService(opts.runner, e.repo.reader, e.repo.reader, e.repo.reader, clock.System{}, idgen.UUIDv7{},
			wagering.Config{ReferenceTTL: refTTL, Backoff: wagering.Backoff{Base: testBackoffBase, Max: testBackoffMax}}, e.logger, opts.metrics)
	}
	if opts.readiness == nil {
		opts.readiness = readyGate()
	}
	if opts.api == nil {
		opts.api = sqsClient(q.endpoint, consumerKey, consumerSecret)
	}
	if opts.backoffMax == 0 {
		opts.backoffMax = time.Minute
	}
	c := sqsconsumer.New(opts.api, svc, opts.readiness, clock.System{}, sqsconsumer.Config{
		Queue: q.queue, DLQ: q.dlq, WaitSeconds: 1, MaxMessages: 10,
		HandlerTimeout: 15 * time.Second, BackoffMax: opts.backoffMax, PauseInterval: 50 * time.Millisecond,
		Senders: providerSenders, OperatorSenderID: operatorKey,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)), opts.metrics)
	if err := c.Resolve(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := c.Stop(ctx); err != nil {
				t.Errorf("stop consumer: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return c, stop
}

// envelope wraps an op body (see op) as the CHALLENGE §10 message.
func envelope(t *testing.T, messageID string, body map[string]any) string {
	t.Helper()
	data := map[string]any{}
	for k, v := range body {
		data[k] = v
	}
	data["idempotencyKey"] = fmt.Sprintf("%s:%s", body["providerId"], body["externalTransactionId"])
	raw, err := json.Marshal(map[string]any{
		"messageId": messageID, "type": "WagerTransactionRequested", "occurredAt": "2026-09-08T12:00:00.000Z", "data": data,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

type consumedInbox struct {
	TransactionID *string
	Outcome       *string
	Processed     bool
}

func (e apiEnv) inbox(t *testing.T, messageID string) (consumedInbox, bool) {
	t.Helper()
	var r consumedInbox
	err := e.repo.db.owner.QueryRow(ctxT(t), `SELECT transaction_id::text, outcome, processed_at IS NOT NULL
		FROM inbox_messages WHERE consumer_name = $1 AND message_id = $2`, sqsconsumer.ConsumerName, messageID).
		Scan(&r.TransactionID, &r.Outcome, &r.Processed)
	if err != nil {
		return consumedInbox{}, false
	}
	return r, true
}

// TestConsumerProcessesMessage: the CHALLENGE §10 message is processed
// through the shared use case, the inbox is completed in the same commit,
// and the message is deleted only afterwards.
func TestConsumerProcessesMessage(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	q := newInboundQueues(t, 60, 10)
	startConsumer(t, e, q, consumerOpts{})
	w := e.openWallet(t, "100.00")

	q.send(t, envelope(t, "msg-bet-1", op(w, "provider-a", "BET", "25.00", "sqs-bet-1")), w.ID)
	r := e.waitStatus(t, "provider-a", "provider-a", "sqs-bet-1", "PROCESSED", 15*time.Second)
	waitFor(t, 10*time.Second, "message deleted", func() bool { return q.depth(t, q.queueURL) == 0 })

	row, ok := e.inbox(t, "msg-bet-1")
	if !ok || !row.Processed || row.Outcome == nil || *row.Outcome != "PROCESSED" || row.TransactionID == nil || *row.TransactionID != r.str("transactionId") {
		t.Fatalf("inbox row %+v (found %v), transaction %s", row, ok, r.str("transactionId"))
	}
	all := e.count(t, `SELECT count(*) FROM outbox_events WHERE payload->'data'->>'transactionId' = $1`, r.str("transactionId"))
	caused := e.count(t, `SELECT count(*) FROM outbox_events WHERE payload->'data'->>'transactionId' = $1
		AND causation_id = 'msg-bet-1' AND correlation_id = 'msg-bet-1'`, r.str("transactionId"))
	if all == 0 || caused != all {
		t.Fatalf("%d of the transaction's %d events carry the messageId as causation and correlation", caused, all)
	}
	e.assertConsistent(t, w, "75.00")
}

// TestConsumerDuplicateDeliveries: CHALLENGE §13 item 4. The same envelope
// delivered three times with different SQS deduplication ids (so the
// broker cannot drop them) is applied once; the copies are duplicates.
func TestConsumerDuplicateDeliveries(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	q := newInboundQueues(t, 60, 10)
	c, _ := startConsumer(t, e, q, consumerOpts{})
	w := e.openWallet(t, "100.00")

	body := envelope(t, "msg-dup-1", op(w, "provider-a", "BET", "10.00", "sqs-dup-1"))
	for range 3 {
		q.send(t, body, w.ID)
	}
	waitFor(t, 20*time.Second, "all three deliveries handled", func() bool {
		s := c.Stats()
		return s.Handled == 1 && s.Duplicates == 2
	})
	waitFor(t, 10*time.Second, "queue empty", func() bool { return q.depth(t, q.queueURL) == 0 })
	if n := e.count(t, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = 'sqs-dup-1'`); n != 1 {
		t.Fatalf("transactions = %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, w.ID); n != 2 {
		t.Fatalf("ledger entries = %d, want opening + one debit", n)
	}
	e.assertConsistent(t, w, "90.00")
}

// TestConsumerDeadLetters: invalid and permanent messages go to the DLQ
// explicitly with their failure attributes, are removed from the source
// queue, and persist nothing (ARCHITECTURE §10). The redrive tool leaves
// them there (failureClass=PERMANENT).
func TestConsumerDeadLetters(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	q := newInboundQueues(t, 60, 10)
	c, _ := startConsumer(t, e, q, consumerOpts{})
	w := e.openWallet(t, "100.00")

	// A processed message, then the same messageId with another payload.
	q.send(t, envelope(t, "msg-reused", op(w, "provider-a", "BET", "5.00", "dl-ok")), "g-ok")
	e.waitStatus(t, "provider-a", "provider-a", "dl-ok", "PROCESSED", 15*time.Second)
	before := e.count(t, `SELECT count(*) FROM wager_transactions`)

	if _, err := e.repo.db.owner.Exec(ctxT(t), `UPDATE providers SET active = false WHERE provider_id = 'provider-b'`); err != nil {
		t.Fatal(err)
	}

	amountAsNumber := strings.Replace(envelope(t, "msg-number", op(w, "provider-a", "BET", "5.00", "dl-number")), `"amount":"5.00"`, `"amount":5.00`, 1)
	conflict := op(w, "provider-a", "BET", "7.00", "dl-ok") // same key as the processed one, another amount
	type dlCase struct {
		key, secret, body, code, envID string
		sender                         string // the originalSenderId of the DLQ copy
	}
	fromA := func(body, code, envID string) dlCase {
		return dlCase{providerAKey, providerASecret, body, code, envID, providerAKey}
	}
	cases := map[string]dlCase{
		"hash":     fromA(envelope(t, "msg-reused", op(w, "provider-a", "BET", "6.00", "dl-ok")), "INVALID_REQUEST", "msg-reused"),
		"json":     fromA(`{"messageId": "msg-broken", `, "INVALID_REQUEST", ""),
		"opening":  fromA(envelope(t, "msg-opening", op(w, "provider-a", "OPENING", "5.00", "dl-opening")), "UNSUPPORTED_KIND", "msg-opening"),
		"provider": fromA(envelope(t, "msg-provider", op(w, "provider-z", "BET", "5.00", "dl-provider")), "PROVIDER_MISMATCH", "msg-provider"),
		"number":   fromA(amountAsNumber, "INVALID_MONEY", "msg-number"),
		"conflict": fromA(envelope(t, "msg-conflict", conflict), "IDEMPOTENCY_KEY_CONFLICT", "msg-conflict"),
		// A key of the default account is no provider identity.
		"unknown-sender": {readerKey, readerSecret, envelope(t, "msg-unknown", op(w, "provider-a", "BET", "5.00", "dl-unknown")),
			"FORBIDDEN", "msg-unknown", queueOwner},
		"inactive-provider": {providerBKey, providerBSecret, envelope(t, "msg-inactive", op(w, "provider-b", "BET", "5.00", "dl-inactive")),
			"FORBIDDEN", "msg-inactive", providerBKey},
	}
	for name, tc := range cases {
		q.sendAs(t, tc.key, tc.secret, tc.body, "g-"+name, "")
	}
	waitFor(t, 20*time.Second, "dead-lettered", func() bool { return c.Stats().DeadLetter == int64(len(cases)) })
	waitFor(t, 10*time.Second, "source queue empty", func() bool { return q.depth(t, q.queueURL) == 0 })

	got := q.readDLQ(t, len(cases), 15*time.Second)
	if len(got) != len(cases) {
		t.Fatalf("DLQ has %d messages, want %d", len(got), len(cases))
	}
	for _, m := range got {
		name := strings.TrimPrefix(m.GroupID, "g-")
		tc, ok := cases[name]
		if !ok {
			t.Errorf("unexpected DLQ group %s", m.GroupID)
			continue
		}
		if m.Body != tc.body || m.Attrs["failureClass"] != "PERMANENT" || m.Attrs["failureCode"] != tc.code ||
			m.Attrs["envelopeMessageId"] != tc.envID || m.Attrs["originalMessageId"] == "" || m.DedupID != m.Attrs["originalMessageId"] ||
			m.Attrs["originalSenderId"] != tc.sender {
			t.Errorf("%s: %+v", name, m)
		}
	}
	if after := e.count(t, `SELECT count(*) FROM wager_transactions`); after != before {
		t.Fatalf("permanent failures persisted transactions: %d -> %d", before, after)
	}
	if n := e.count(t, `SELECT count(*) FROM inbox_messages WHERE message_id <> 'msg-reused'`); n != 0 {
		t.Fatalf("permanent failures persisted %d inbox rows", n)
	}
	e.assertConsistent(t, w, "95.00")

	// The redrive tool leaves the permanent messages in the DLQ.
	res, err := sqsconsumer.Redrive(ctxT(t), sqsClient(q.endpoint, operatorKey, operatorSecret), q.dlqURL, q.queueURL, operatorKey, time.Now)
	if err != nil || res.Redriven != 0 || res.Skipped != len(cases) {
		t.Fatalf("redrive = %+v, %v", res, err)
	}
}

// TestConsumerProviderIsTheSender: CHALLENGE §2 and §14 over SQS. The
// provider a message acts for comes from its broker identity, the SenderId
// MiniStack reports, never from data.providerId alone (ARCHITECTURE
// §10/§11). provider-a's credentials cannot submit, replay or reverse
// anything for provider-b: each attempt goes to the DLQ as a permanent
// failure, and no transaction, inbox, ledger or outbox row is written.
func TestConsumerProviderIsTheSender(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	q := newInboundQueues(t, 60, 10)
	c, _ := startConsumer(t, e, q, consumerOpts{})
	w := e.openWallet(t, "100.00")

	// provider-b's own bet, sent with provider-b's credentials.
	betB := op(w, "provider-b", "BET", "10.00", "b-bet")
	q.sendAs(t, providerBKey, providerBSecret, envelope(t, "msg-b-bet", betB), w.ID, "")
	original := e.waitStatus(t, "provider-b", "provider-b", "b-bet", "PROCESSED", 15*time.Second)
	waitFor(t, 10*time.Second, "provider-b's message handled", func() bool { return c.Stats().Handled == 1 })
	rows := func() [4]int {
		return [4]int{
			e.count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1`, w.ID),
			e.count(t, `SELECT count(*) FROM inbox_messages`),
			e.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, w.ID),
			e.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, w.ID),
		}
	}
	before := rows()

	type attempt struct {
		key, secret, body, forged, code, sender string
	}
	fromA := func(body, forged string) attempt {
		return attempt{providerAKey, providerASecret, body, forged, "PROVIDER_MISMATCH", providerAKey}
	}
	attempts := map[string]attempt{
		// A new operation in provider-b's name.
		"new": fromA(envelope(t, "msg-a-new", op(w, "provider-b", "BET", "20.00", "a-as-b")), ""),
		// provider-b's key and payload under a new messageId: a replay of
		// provider-b's operation.
		"replay": fromA(envelope(t, "msg-a-replay", betB), ""),
		// A rollback of provider-b's bet: it would credit the player.
		"reversal": fromA(envelope(t, "msg-a-reversal", op(w, "provider-b", "ROLLBACK", "10.00", "a-rollback", "b-bet")), ""),
		// Naming provider-b's identity as the original sender: only the
		// redrive operator is trusted with that attribute.
		"forged": fromA(envelope(t, "msg-a-forged", op(w, "provider-b", "BET", "20.00", "a-forged")), providerBKey),
		// The operator's own messages act for no provider.
		"operator": {operatorKey, operatorSecret, envelope(t, "msg-operator", op(w, "provider-b", "BET", "20.00", "op-as-b")), "", "FORBIDDEN", ""},
	}
	for name, a := range attempts {
		q.sendAs(t, a.key, a.secret, a.body, "g-"+name, a.forged)
	}
	waitFor(t, 20*time.Second, "attempts dead-lettered", func() bool { return c.Stats().DeadLetter == int64(len(attempts)) })
	waitFor(t, 10*time.Second, "source queue empty", func() bool { return q.depth(t, q.queueURL) == 0 })

	got := q.readDLQ(t, len(attempts), 15*time.Second)
	if len(got) != len(attempts) {
		t.Fatalf("DLQ has %d messages, want %d", len(got), len(attempts))
	}
	for _, m := range got {
		name := strings.TrimPrefix(m.GroupID, "g-")
		a, ok := attempts[name]
		if !ok || m.Body != a.body || m.Attrs["failureClass"] != "PERMANENT" || m.Attrs["failureCode"] != a.code ||
			m.Attrs["originalSenderId"] != a.sender {
			t.Errorf("%s: %+v", name, m)
		}
	}
	if after := rows(); after != before {
		t.Fatalf("refused messages wrote rows (transactions, inbox, ledger, outbox): %v -> %v", before, after)
	}
	if s := c.Stats(); s.Handled != 1 || s.Duplicates != 0 {
		t.Fatalf("stats %+v: only provider-b's own message may reach the use case", s)
	}
	again := e.call(t, http.MethodGet, "/providers/provider-b/wagering/transactions/b-bet", "provider-b", nil)
	if again.str("transactionId") != original.str("transactionId") || again.str("status") != "PROCESSED" || again.amount("balance") != "90.00" {
		t.Fatalf("provider-b's operation changed: %s", again.Raw)
	}
	e.assertConsistent(t, w, "90.00")
}

// TestConsumerRecordsInvariantViolationAsFailed: a domain invariant error
// over SQS is recorded as FAILED like over HTTP. The inbox is completed with
// FAILED and the message deleted; it is neither retried nor dead-lettered
// (ARCHITECTURE §5, §23 G6, §26).
func TestConsumerRecordsInvariantViolationAsFailed(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	q := newInboundQueues(t, 60, 10)
	armed, other := &atomic.Bool{}, &uuid.UUID{}
	c, _ := startConsumer(t, e, q, consumerOpts{runner: wrongWalletRunner{inner: e.repo.runner, armed: armed, other: other}})
	w, decoy := e.openWallet(t, "100.00"), e.openWallet(t, "50.00")
	*other = uuid.MustParse(decoy.ID)
	armed.Store(true)

	q.send(t, envelope(t, "msg-broken", op(w, "provider-a", "BET", "10.00", "sqs-broken")), w.ID)
	r := e.waitStatus(t, "provider-a", "provider-a", "sqs-broken", "FAILED", 15*time.Second)
	if r.str("failureCode") != "INTERNAL_INVARIANT_VIOLATION" {
		t.Fatalf("failed transaction: %s", r.Raw)
	}
	waitFor(t, 10*time.Second, "message deleted", func() bool { return q.depth(t, q.queueURL) == 0 })
	if row, ok := e.inbox(t, "msg-broken"); !ok || row.Outcome == nil || *row.Outcome != "FAILED" {
		t.Fatalf("inbox %+v (found %v)", row, ok)
	}
	if s := c.Stats(); s.Handled != 1 || s.Retried != 0 || s.DeadLetter != 0 {
		t.Fatalf("stats %+v: want one handled message, no retry, no DLQ", s)
	}
	e.assertConsistent(t, w, "100.00")
	e.assertConsistent(t, decoy, "50.00")
}

// TestConsumerBusinessRejection: a confirmed rejection is terminal; it is
// committed and the message removed (CHALLENGE §10).
func TestConsumerBusinessRejection(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	q := newInboundQueues(t, 60, 10)
	startConsumer(t, e, q, consumerOpts{})
	w := e.openWallet(t, "10.00")

	q.send(t, envelope(t, "msg-broke", op(w, "provider-a", "BET", "25.00", "sqs-broke")), w.ID)
	r := e.waitStatus(t, "provider-a", "provider-a", "sqs-broke", "REJECTED", 15*time.Second)
	if r.str("failureCode") != "INSUFFICIENT_FUNDS" {
		t.Fatalf("failure code %s", r.Raw)
	}
	waitFor(t, 10*time.Second, "message deleted", func() bool { return q.depth(t, q.queueURL) == 0 })
	if row, _ := e.inbox(t, "msg-broke"); row.Outcome == nil || *row.Outcome != "REJECTED" {
		t.Fatalf("inbox %+v", row)
	}
	e.assertConsistent(t, w, "10.00")
}

// TestConsumerPendingReference: a WIN before its BET completes the inbox
// as PENDING_REFERENCE and the message is removed; the worker takes over
// once the BET arrives (ARCHITECTURE §9).
func TestConsumerPendingReference(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t, withReference(refTTL, testBackoffBase, testBackoffMax))
	q := newInboundQueues(t, 60, 10)
	startConsumer(t, e, q, consumerOpts{})
	startWorker(t, "consumer-ref-worker", e.wagering, 1)
	w := e.openWallet(t, "100.00")

	q.send(t, envelope(t, "msg-win", op(w, "provider-a", "WIN", "40.00", "sqs-win", "sqs-bet")), "group-win")
	e.waitStatus(t, "provider-a", "provider-a", "sqs-win", "PENDING_REFERENCE", 15*time.Second)
	waitFor(t, 10*time.Second, "message deleted", func() bool { return q.depth(t, q.queueURL) == 0 })
	if row, _ := e.inbox(t, "msg-win"); row.Outcome == nil || *row.Outcome != "PENDING_REFERENCE" {
		t.Fatalf("inbox %+v", row)
	}

	q.send(t, envelope(t, "msg-bet", op(w, "provider-a", "BET", "20.00", "sqs-bet")), "group-bet")
	e.waitStatus(t, "provider-a", "provider-a", "sqs-win", "PROCESSED", 15*time.Second)
	e.assertConsistent(t, w, "120.00")
}

// TestHTTPAndSQSCrossover: CHALLENGE §10/§13. The same operation through
// HTTP and SQS (same key and payload) is applied once, in either order and
// concurrently; both transports see the one result.
func TestHTTPAndSQSCrossover(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	q := newInboundQueues(t, 60, 10)
	c, _ := startConsumer(t, e, q, consumerOpts{})
	w := e.openWallet(t, "100.00")

	// HTTP first, then SQS: the message replays the HTTP result.
	httpFirst := op(w, "provider-a", "BET", "10.00", "x-http-first")
	if r := e.submit(t, "provider-a", httpFirst); r.Status != http.StatusOK {
		t.Fatalf("http: %d %s", r.Status, r.Raw)
	}
	q.send(t, envelope(t, "msg-x1", httpFirst), w.ID)

	// SQS first, then HTTP: the request replays the message's result.
	sqsFirst := op(w, "provider-a", "BET", "20.00", "x-sqs-first")
	q.send(t, envelope(t, "msg-x2", sqsFirst), w.ID)
	e.waitStatus(t, "provider-a", "provider-a", "x-sqs-first", "PROCESSED", 15*time.Second)
	if r := e.submit(t, "provider-a", sqsFirst); r.Status != http.StatusOK || r.Body["idempotentReplay"] != true {
		t.Fatalf("http replay: %d %s", r.Status, r.Raw)
	}

	// Concurrent: the message and ten HTTP requests race.
	concurrent := op(w, "provider-a", "BET", "30.00", "x-concurrent")
	q.send(t, envelope(t, "msg-x3", concurrent), w.ID)
	var wg sync.WaitGroup
	var replays, fresh atomic.Int64
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := e.submit(t, "provider-a", concurrent)
			if r.Status != http.StatusOK {
				t.Errorf("concurrent http: %d %s", r.Status, r.Raw)
				return
			}
			if r.Body["idempotentReplay"] == true {
				replays.Add(1)
			} else {
				fresh.Add(1)
			}
		}()
	}
	wg.Wait()
	waitFor(t, 20*time.Second, "messages handled", func() bool { return c.Stats().Handled == 3 })
	waitFor(t, 10*time.Second, "queue empty", func() bool { return q.depth(t, q.queueURL) == 0 })

	for _, ext := range []string{"x-http-first", "x-sqs-first", "x-concurrent"} {
		if n := e.count(t, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = $1`, ext); n != 1 {
			t.Fatalf("%s: %d transactions", ext, n)
		}
		if n := e.count(t, `SELECT count(*) FROM wallet_ledger_entries l JOIN wager_transactions w ON w.id = l.transaction_id WHERE w.external_transaction_id = $1`, ext); n != 1 {
			t.Fatalf("%s: %d ledger entries", ext, n)
		}
	}
	if fresh.Load() > 1 {
		t.Fatalf("%d HTTP requests applied the concurrent operation", fresh.Load())
	}
	for _, id := range []string{"msg-x1", "msg-x2", "msg-x3"} {
		if row, ok := e.inbox(t, id); !ok || row.Outcome == nil || *row.Outcome != "PROCESSED" {
			t.Fatalf("inbox %s: %+v", id, row)
		}
	}
	e.assertConsistent(t, w, "40.00")
}

// dropFirstDelete simulates a process that dies after its commit and
// before DeleteMessage: the first delete never reaches SQS.
type dropFirstDelete struct {
	*sqs.Client
	dropped atomic.Bool
}

func (d *dropFirstDelete) DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	if d.dropped.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("simulated crash before delete")
	}
	return d.Client.DeleteMessage(ctx, in, optFns...)
}

// TestConsumerCommitThenDieBeforeDelete: CHALLENGE §13 item 5. The message
// comes back after its visibility timeout and is recognised as a
// duplicate: the operation is applied once.
func TestConsumerCommitThenDieBeforeDelete(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	q := newInboundQueues(t, 2, 10)
	api := &dropFirstDelete{Client: sqsClient(q.endpoint, consumerKey, consumerSecret)}
	c, _ := startConsumer(t, e, q, consumerOpts{api: api})
	w := e.openWallet(t, "100.00")

	q.send(t, envelope(t, "msg-crash", op(w, "provider-a", "BET", "15.00", "sqs-crash")), w.ID)
	waitFor(t, 20*time.Second, "redelivered as a duplicate", func() bool {
		s := c.Stats()
		return s.Handled == 1 && s.Duplicates == 1
	})
	waitFor(t, 10*time.Second, "queue empty", func() bool { return q.depth(t, q.queueURL) == 0 })
	if n := e.count(t, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = 'sqs-crash'`); n != 1 {
		t.Fatalf("transactions = %d", n)
	}
	e.assertConsistent(t, w, "85.00")
}

// transientRunner fails every transaction with ErrTransient while down.
type transientRunner struct {
	inner app.TxRunner
	down  *atomic.Bool
	calls *atomic.Int64
}

func (r transientRunner) InTx(ctx context.Context, opts app.TxOptions, fn func(ctx context.Context, r app.Repos) error) error {
	r.calls.Add(1)
	if r.down.Load() {
		return fmt.Errorf("%w: database down (simulated)", app.ErrTransient)
	}
	return r.inner.InTx(ctx, opts, fn)
}

// TestConsumerTransientRetriesThenRedrive: a transient failure keeps the
// message with growing visibility; after maxReceiveCount receives the
// redrive policy moves it to the DLQ without failureClass; once the cause
// is fixed, cmd/dlqredrive's logic sends it back and it is processed
// (ARCHITECTURE §10). The resend comes from the operator's own identity,
// which resolves the queues in their owner's account, and names provider-a
// as the original sender, which the consumer accepts from the operator.
func TestConsumerTransientRetriesThenRedrive(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	q := newInboundQueues(t, 30, 2)
	down, calls := &atomic.Bool{}, &atomic.Int64{}
	down.Store(true)
	gate := readyGate()
	c, _ := startConsumer(t, e, q, consumerOpts{runner: transientRunner{inner: e.repo.runner, down: down, calls: calls}, readiness: gate, backoffMax: time.Second})
	w := e.openWallet(t, "100.00")

	body := envelope(t, "msg-transient", op(w, "provider-a", "BET", "12.00", "sqs-transient"))
	q.send(t, body, w.ID)
	waitFor(t, 30*time.Second, "moved to the DLQ by the redrive policy", func() bool { return q.depth(t, q.dlqURL) == 1 })
	if s := c.Stats(); s.Retried < 2 || s.DeadLetter != 0 {
		t.Fatalf("stats %+v: want >= 2 transient retries and no explicit DLQ send", s)
	}
	if gate.checks.Load() < 2 {
		t.Fatalf("transient database failures must force readiness checks, got %d", gate.checks.Load())
	}
	if n := e.count(t, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = 'sqs-transient'`); n != 0 {
		t.Fatal("a transient failure persisted the operation")
	}

	down.Store(false)
	operator := sqsClient(q.endpoint, operatorKey, operatorSecret)
	queueURL, err := sqspublisher.ResolveQueueURLInAccount(ctxT(t), operator, q.queue, queueOwner)
	if err != nil || queueURL != q.queueURL {
		t.Fatalf("operator resolves %q, %v; want %s", queueURL, err, q.queueURL)
	}
	dlqURL, err := sqspublisher.ResolveQueueURLInAccount(ctxT(t), operator, q.dlq, queueOwner)
	if err != nil || dlqURL != q.dlqURL {
		t.Fatalf("operator resolves %q, %v; want %s", dlqURL, err, q.dlqURL)
	}
	res, err := sqsconsumer.Redrive(ctxT(t), operator, dlqURL, queueURL, operatorKey, time.Now)
	if err != nil || res.Redriven != 1 || res.Skipped != 0 {
		t.Fatalf("redrive = %+v, %v", res, err)
	}
	e.waitStatus(t, "provider-a", "provider-a", "sqs-transient", "PROCESSED", 15*time.Second)
	waitFor(t, 10*time.Second, "queues empty", func() bool { return q.depth(t, q.queueURL) == 0 && q.depth(t, q.dlqURL) == 0 })
	if s := c.Stats(); s.DeadLetter != 0 {
		t.Fatalf("the redriven message was refused: %+v", s)
	}
	e.assertConsistent(t, w, "88.00")
}

// TestConsumerPausesWhileNotReady: while readiness fails nothing is
// received, so receive counts do not advance (ARCHITECTURE §10).
func TestConsumerPausesWhileNotReady(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	q := newInboundQueues(t, 60, 10)
	gate := readyGate()
	gate.ready.Store(false)
	startConsumer(t, e, q, consumerOpts{readiness: gate})
	w := e.openWallet(t, "100.00")

	q.send(t, envelope(t, "msg-paused", op(w, "provider-a", "BET", "1.00", "sqs-paused")), w.ID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		out, err := q.admin.GetQueueAttributes(ctxT(t), &sqs.GetQueueAttributesInput{QueueUrl: aws.String(q.queueURL),
			AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessagesNotVisible}})
		if err != nil {
			t.Fatal(err)
		}
		if out.Attributes["ApproximateNumberOfMessagesNotVisible"] != "0" {
			t.Fatal("message received while readiness was failing")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := e.count(t, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = 'sqs-paused'`); n != 0 {
		t.Fatal("processed while paused")
	}
	gate.ready.Store(true)
	e.waitStatus(t, "provider-a", "provider-a", "sqs-paused", "PROCESSED", 15*time.Second)
}

// gatedRunner blocks the first transaction until released.
type gatedRunner struct {
	inner   app.TxRunner
	entered chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (g gatedRunner) InTx(ctx context.Context, opts app.TxOptions, fn func(ctx context.Context, r app.Repos) error) error {
	first := false
	g.once.Do(func() { first = true })
	if first {
		close(g.entered)
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.inner.InTx(ctx, opts, fn)
}

// TestConsumerStopFinishesInFlight: on SIGTERM the consumer stops polling,
// the handler in flight commits within its deadline and its message is
// deleted (CHALLENGE §10, ARCHITECTURE §14).
func TestConsumerStopFinishesInFlight(t *testing.T) {
	t.Parallel()
	e := newAPIEnv(t)
	q := newInboundQueues(t, 60, 10)
	g := gatedRunner{inner: e.repo.runner, entered: make(chan struct{}), release: make(chan struct{}), once: &sync.Once{}}
	c, stop := startConsumer(t, e, q, consumerOpts{runner: g})
	w := e.openWallet(t, "100.00")

	q.send(t, envelope(t, "msg-sigterm", op(w, "provider-a", "BET", "9.00", "sqs-sigterm")), w.ID)
	select {
	case <-g.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("message not picked up")
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned with a handler in flight")
	case <-time.After(300 * time.Millisecond):
	}
	close(g.release)
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("Stop did not return")
	}
	<-c.Done()
	e.waitStatus(t, "provider-a", "provider-a", "sqs-sigterm", "PROCESSED", 5*time.Second)
	if d := q.depth(t, q.queueURL); d != 0 {
		t.Fatalf("queue depth after stop = %d", d)
	}
}

// TestConsumerCredentialSeparation is the consumer side of ARCHITECTURE
// §10: the consumer's credentials must not publish to the events queue.
// MiniStack enforces IAM only with AUTH=true, which the project does not
// use, so in the default mode the test is skipped with that reason (A12).
func TestConsumerCredentialSeparation(t *testing.T) {
	t.Parallel()
	endpoint := startMiniStack(t)
	client := sqsClient(endpoint, consumerKey, consumerSecret)
	queue := newEventsQueue(t, endpoint)
	url, err := sqsconsumerResolve(t, client, queue)
	if err == nil {
		_, err = client.SendMessage(ctxT(t), &sqs.SendMessageInput{QueueUrl: aws.String(url), MessageBody: aws.String("{}"),
			MessageGroupId: aws.String("g"), MessageDeduplicationId: aws.String(uuid.NewString())})
	}
	if err == nil {
		t.Skip("MiniStack enforces IAM for SQS only with AUTH=true, which this project does not use: that mode generates the " +
			"access keys, which would have to be provisioned for every role and read by the app and the tests. In the default mode the " +
			"consumer's credentials could SendMessage to an events queue, so credential separation is configured but not verified " +
			"locally (ARCHITECTURE §10, A12).")
	}
	t.Logf("send with consumer credentials refused: %v", err)
}

func sqsconsumerResolve(t *testing.T, client *sqs.Client, queue string) (string, error) {
	t.Helper()
	out, err := client.GetQueueUrl(ctxT(t), &sqs.GetQueueUrlInput{QueueName: aws.String(queue)})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.QueueUrl), nil
}
