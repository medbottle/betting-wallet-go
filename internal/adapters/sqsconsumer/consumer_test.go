package sqsconsumer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/goleak"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/app/wagering"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
	"github.com/medbottle/betting-wallet-go/internal/platform/health"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// fakeSQS records every call. Receive hands out the queued batches once,
// then blocks until ctx ends, like a long poll on an empty queue.
type fakeSQS struct {
	mu         sync.Mutex
	batches    [][]types.Message
	deleted    []string
	visibility map[string][]int32
	sent       []*sqs.SendMessageInput
	sendErr    error
	receives   int
	// lastReceive is the input of the latest ReceiveMessage.
	lastReceive *sqs.ReceiveMessageInput
	// nonBlocking makes an empty receive return at once (the redrive's
	// short poll on a drained DLQ).
	nonBlocking bool
}

func newFakeSQS(batches ...[]types.Message) *fakeSQS {
	return &fakeSQS{batches: batches, visibility: map[string][]int32{}}
}

func (f *fakeSQS) GetQueueUrl(_ context.Context, in *sqs.GetQueueUrlInput, _ ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error) {
	return &sqs.GetQueueUrlOutput{QueueUrl: aws.String("url/" + aws.ToString(in.QueueName))}, nil
}

func (f *fakeSQS) ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.mu.Lock()
	f.receives++
	f.lastReceive = in
	if len(f.batches) > 0 {
		b := f.batches[0]
		f.batches = f.batches[1:]
		f.mu.Unlock()
		return &sqs.ReceiveMessageOutput{Messages: b}, nil
	}
	nb := f.nonBlocking
	f.mu.Unlock()
	if nb {
		return &sqs.ReceiveMessageOutput{}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakeSQS) DeleteMessage(_ context.Context, in *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, aws.ToString(in.ReceiptHandle))
	return &sqs.DeleteMessageOutput{}, nil
}

func (f *fakeSQS) ChangeMessageVisibility(_ context.Context, in *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := aws.ToString(in.ReceiptHandle)
	f.visibility[h] = append(f.visibility[h], in.VisibilityTimeout)
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

func (f *fakeSQS) SendMessage(_ context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	f.sent = append(f.sent, in)
	return &sqs.SendMessageOutput{}, nil
}

func (f *fakeSQS) snapshot() (deleted []string, visibility map[string][]int32, sent []*sqs.SendMessageInput) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := map[string][]int32{}
	for k, s := range f.visibility {
		v[k] = append([]int32(nil), s...)
	}
	return append([]string(nil), f.deleted...), v, append([]*sqs.SendMessageInput(nil), f.sent...)
}

// fakeSubmitter answers per messageId and records the call order.
type fakeSubmitter struct {
	mu      sync.Mutex
	calls   []string
	results map[string]error
	dups    map[string]bool
	block   map[string]chan struct{}
}

func (s *fakeSubmitter) Submit(ctx context.Context, cmd wagering.SubmitCommand) (wagering.SubmitResult, error) {
	id := cmd.Inbox.MessageID
	s.mu.Lock()
	s.calls = append(s.calls, id)
	err, dup, block := s.results[id], s.dups[id], s.block[id]
	s.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return wagering.SubmitResult{}, fmt.Errorf("%w: %w", app.ErrTransient, ctx.Err())
		}
	}
	if cmd.Inbox.ConsumerName != ConsumerName || cmd.CorrelationID != id || cmd.CausationID == nil || *cmd.CausationID != id {
		return wagering.SubmitResult{}, errors.New("command not built from the envelope")
	}
	return wagering.SubmitResult{Duplicate: dup}, err
}

func (s *fakeSubmitter) called() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

type fakeReadiness struct {
	mu      sync.Mutex
	ready   bool
	checks  int
	onCheck func()
}

func (r *fakeReadiness) Report() health.Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	return health.Report{Ready: r.ready}
}

func (r *fakeReadiness) CheckNow(context.Context) {
	r.mu.Lock()
	r.checks++
	f := r.onCheck
	r.mu.Unlock()
	if f != nil {
		f()
	}
}

func (r *fakeReadiness) checkCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checks
}

func (r *fakeReadiness) set(ready bool) {
	r.mu.Lock()
	r.ready = ready
	r.mu.Unlock()
}

// countingMetrics records the consumer's retries and DLQ sends.
type countingMetrics struct {
	app.NopMetrics
	mu      sync.Mutex
	retries int
	dlq     map[wager.FailureCode]int
}

func (m *countingMetrics) Retry(component string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if component == app.RetryConsumer {
		m.retries++
	}
}

func (m *countingMetrics) DeadLettered(code wager.FailureCode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dlq == nil {
		m.dlq = map[wager.FailureCode]int{}
	}
	m.dlq[code]++
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) }

// Broker identities of the tests (SenderId principals).
const (
	senderA    = "111111111111"      // provider-a
	senderB    = "222222222222"      // provider-b
	roleSender = "AROAEXAMPLEROLE01" // provider-a, as an IAM role
	operator   = "333333333333"      // the redrive operator
)

var testSenders = map[string]string{senderA: "provider-a", senderB: "provider-b", roleSender: "provider-a"}

// msg is validBody (provider-a) under envID, sent by provider-a.
func msg(envID, group string, receiveCount int) types.Message {
	body := strings.Replace(validBody, `"msg-123"`, `"`+envID+`"`, 1)
	return rawMsg(envID, body, group, receiveCount)
}

func rawMsg(handle, body, group string, receiveCount int) types.Message {
	attrs := map[string]string{"ApproximateReceiveCount": fmt.Sprint(receiveCount), "SenderId": senderA}
	if group != "" {
		attrs["MessageGroupId"] = group
	}
	return types.Message{MessageId: aws.String("sqs-" + handle), ReceiptHandle: aws.String(handle), Body: aws.String(body), Attributes: attrs}
}

// withSender sets the SenderId SQS reports; "" removes it.
func withSender(m types.Message, sender string) types.Message {
	attrs := map[string]string{}
	for k, v := range m.Attributes {
		attrs[k] = v
	}
	delete(attrs, "SenderId")
	if sender != "" {
		attrs["SenderId"] = sender
	}
	m.Attributes = attrs
	return m
}

// withOriginalSender sets the originalSenderId message attribute.
func withOriginalSender(m types.Message, sender string) types.Message {
	m.MessageAttributes = map[string]types.MessageAttributeValue{AttrOriginalSenderID: strAttr(sender)}
	return m
}

func newTestConsumer(t *testing.T, api *fakeSQS, sub *fakeSubmitter, ready *fakeReadiness) *Consumer {
	t.Helper()
	return newTestConsumerWith(t, api, sub, ready, nil)
}

func newTestConsumerWith(t *testing.T, api *fakeSQS, sub *fakeSubmitter, ready *fakeReadiness, m app.Metrics) *Consumer {
	t.Helper()
	if sub.results == nil {
		sub.results = map[string]error{}
	}
	c := New(api, sub, ready, fixedClock{}, Config{
		Queue: "in.fifo", DLQ: "dlq.fifo", WaitSeconds: 20, MaxMessages: 10,
		HandlerTimeout: 2 * time.Second, BackoffMax: time.Minute, PauseInterval: 5 * time.Millisecond,
		Senders: testSenders, OperatorSenderID: operator,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), m)
	if err := c.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

// runUntil starts c, waits for cond, then stops it.
func runUntil(t *testing.T, c *Consumer, cond func() bool) {
	t.Helper()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			_ = c.Stop(context.Background())
			t.Fatal("condition not reached")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOutcomes(t *testing.T) {
	api := newFakeSQS([]types.Message{
		msg("ok", "w1", 1),
		msg("dup", "w2", 1),
		msg("invalid-amount", "w3", 1),
		msg("conflict", "w4", 1),
		msg("provider", "w5", 1),
		msg("hash", "w6", 1),
		rawMsg("garbage", "not json", "w7", 1),
	})
	sub := &fakeSubmitter{
		results: map[string]error{
			"invalid-amount": &wager.ValidationError{Code: wager.CodeInvalidAmountForKind},
			"conflict":       &wager.ConflictError{Code: wager.CodeIdempotencyKeyConflict},
			"provider":       fmt.Errorf("%w: x", app.ErrProviderNotAllowed),
			"hash":           fmt.Errorf("%w: hash", app.ErrInboxHashMismatch),
		},
		dups: map[string]bool{"dup": true},
	}
	m := &countingMetrics{}
	c := newTestConsumerWith(t, api, sub, &fakeReadiness{ready: true}, m)
	runUntil(t, c, func() bool { d, _, _ := api.snapshot(); return len(d) == 7 })
	if m.dlq[wager.CodeInvalidRequest] != 2 || m.dlq[wager.CodeInvalidAmountForKind] != 1 || m.dlq["FORBIDDEN"] != 1 || m.retries != 0 {
		t.Errorf("metrics: dlq %v, retries %d", m.dlq, m.retries)
	}

	_, vis, sent := api.snapshot()
	if len(vis) != 0 {
		t.Fatalf("no visibility change expected: %v", vis)
	}
	wantCodes := map[string]string{
		"sqs-invalid-amount": string(wager.CodeInvalidAmountForKind),
		"sqs-conflict":       string(wager.CodeIdempotencyKeyConflict),
		"sqs-provider":       "FORBIDDEN",
		"sqs-hash":           string(wager.CodeInvalidRequest),
		"sqs-garbage":        string(wager.CodeInvalidRequest),
	}
	if len(sent) != len(wantCodes) {
		t.Fatalf("DLQ sends = %d, want %d", len(sent), len(wantCodes))
	}
	for _, in := range sent {
		orig := aws.ToString(in.MessageAttributes[AttrOriginalMessageID].StringValue)
		if aws.ToString(in.QueueUrl) != "url/dlq.fifo" || aws.ToString(in.MessageDeduplicationId) != orig ||
			aws.ToString(in.MessageAttributes[AttrFailureClass].StringValue) != FailureClassPermanent ||
			aws.ToString(in.MessageAttributes[AttrFailureCode].StringValue) != wantCodes[orig] {
			t.Errorf("DLQ send for %s: %+v", orig, in)
		}
		_, hasEnv := in.MessageAttributes[AttrEnvelopeMessageID]
		if hasEnv == (orig == "sqs-garbage") {
			t.Errorf("%s: envelopeMessageId present = %v", orig, hasEnv)
		}
		if got := aws.ToString(in.MessageAttributes[AttrOriginalSenderID].StringValue); got != senderA {
			t.Errorf("%s: the DLQ copy names sender %q, want %s", orig, got, senderA)
		}
	}
	if s := c.Stats(); s.Handled != 1 || s.Duplicates != 1 || s.DeadLetter != 5 {
		t.Fatalf("stats %+v", s)
	}
	if got := sub.called(); len(got) != 6 {
		t.Fatalf("garbage must not reach the use case: %v", got)
	}
}

// TestSenderDecidesTheProvider: the provider a message acts for comes from
// the SenderId SQS reports, never from data.providerId alone (ARCHITECTURE
// §10/§11). validBody names provider-a. A refused message goes to the DLQ
// before the use case runs, so nothing is persisted.
func TestSenderDecidesTheProvider(t *testing.T) {
	cases := []struct {
		m      types.Message
		code   string // "" when the use case runs
		sender string // originalSenderId of the DLQ copy
	}{
		{msg("own", "w1", 1), "", ""},
		{withSender(msg("other-provider", "w2", 1), senderB), "PROVIDER_MISMATCH", senderB},
		{withSender(msg("unknown", "w3", 1), "999999999999"), "FORBIDDEN", "999999999999"},
		// The sender is authenticated before the envelope is parsed.
		{withSender(rawMsg("unknown-garbage", "not json", "w4", 1), "999999999999"), "FORBIDDEN", "999999999999"},
		{withSender(msg("no-sender", "w5", 1), ""), "FORBIDDEN", ""},
		// Only the operator may name the original sender.
		{withOriginalSender(withSender(msg("forged", "w6", 1), senderB), senderA), "PROVIDER_MISMATCH", senderB},
		{withOriginalSender(withSender(msg("redriven", "w7", 1), operator), senderA), "", ""},
		{withOriginalSender(withSender(msg("redriven-other", "w8", 1), operator), senderB), "PROVIDER_MISMATCH", senderB},
		{withSender(msg("operator-alone", "w9", 1), operator), "FORBIDDEN", ""},
		// An IAM role's SenderId is <role id>:<session>; the role id counts.
		{withSender(msg("role-session", "w10", 1), roleSender+":any-session"), "", ""},
	}
	var batch []types.Message
	for _, tc := range cases {
		batch = append(batch, tc.m)
	}
	api := newFakeSQS(batch)
	sub := &fakeSubmitter{}
	m := &countingMetrics{}
	c := newTestConsumerWith(t, api, sub, &fakeReadiness{ready: true}, m)
	runUntil(t, c, func() bool { d, _, _ := api.snapshot(); return len(d) == len(cases) })

	api.mu.Lock()
	in := api.lastReceive
	api.mu.Unlock()
	if !slices.Contains(in.MessageSystemAttributeNames, types.MessageSystemAttributeNameSenderId) ||
		!slices.Contains(in.MessageAttributeNames, AttrOriginalSenderID) {
		t.Fatalf("receive must ask for SenderId and %s: %+v", AttrOriginalSenderID, in)
	}
	_, _, sent := api.snapshot()
	dlq := map[string]*sqs.SendMessageInput{}
	for _, s := range sent {
		dlq[aws.ToString(s.MessageAttributes[AttrOriginalMessageID].StringValue)] = s
	}
	var submitted []string
	for _, tc := range cases {
		id := aws.ToString(tc.m.MessageId)
		s, dead := dlq[id]
		if tc.code == "" {
			if dead {
				t.Errorf("%s: dead-lettered with %s", id, aws.ToString(s.MessageAttributes[AttrFailureCode].StringValue))
			}
			submitted = append(submitted, strings.TrimPrefix(id, "sqs-"))
			continue
		}
		if !dead {
			t.Errorf("%s: not dead-lettered, want %s", id, tc.code)
			continue
		}
		if got := aws.ToString(s.MessageAttributes[AttrFailureCode].StringValue); got != tc.code ||
			aws.ToString(s.MessageAttributes[AttrFailureClass].StringValue) != FailureClassPermanent {
			t.Errorf("%s: failure %s/%s, want %s/PERMANENT", id, got, aws.ToString(s.MessageAttributes[AttrFailureClass].StringValue), tc.code)
		}
		if got := aws.ToString(s.MessageAttributes[AttrOriginalSenderID].StringValue); got != tc.sender {
			t.Errorf("%s: DLQ copy names sender %q, want %q", id, got, tc.sender)
		}
	}
	got := sub.called()
	sort.Strings(got)
	sort.Strings(submitted)
	if !slices.Equal(got, submitted) {
		t.Fatalf("use case ran for %v, want only %v", got, submitted)
	}
	if m.dlq[codeProviderMismatch] != 3 || m.dlq[codeForbidden] != 4 {
		t.Errorf("DLQ metrics %v", m.dlq)
	}
}

func TestStartRequiresSenders(t *testing.T) {
	api := newFakeSQS()
	c := New(api, &fakeSubmitter{}, &fakeReadiness{}, fixedClock{}, Config{
		Queue: "in.fifo", DLQ: "dlq.fifo", HandlerTimeout: time.Second, PauseInterval: time.Second, MaxMessages: 1,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err := c.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err == nil {
		_ = c.Stop(context.Background())
		t.Fatal("started without any trusted sender")
	}
}

func TestDLQGroupFallsBackWithoutGroupID(t *testing.T) {
	api := newFakeSQS([]types.Message{rawMsg("g", "{", "", 1)})
	c := newTestConsumer(t, api, &fakeSubmitter{}, &fakeReadiness{ready: true})
	runUntil(t, c, func() bool { d, _, _ := api.snapshot(); return len(d) == 1 })
	_, _, sent := api.snapshot()
	if aws.ToString(sent[0].MessageGroupId) != fallbackGroup {
		t.Fatalf("group = %s", aws.ToString(sent[0].MessageGroupId))
	}
}

func TestDLQSendFailureKeepsMessage(t *testing.T) {
	api := newFakeSQS([]types.Message{rawMsg("g", "{", "w1", 3)})
	api.sendErr = errors.New("boom")
	c := newTestConsumer(t, api, &fakeSubmitter{}, &fakeReadiness{ready: true})
	runUntil(t, c, func() bool { _, v, _ := api.snapshot(); return len(v["g"]) == 1 })
	d, v, _ := api.snapshot()
	if len(d) != 0 || v["g"][0] != 8 {
		t.Fatalf("deleted %v, visibility %v", d, v)
	}
}

func TestTransientBackoffAndGroupRelease(t *testing.T) {
	// w1: a, b(transient), c, d -> a deleted, b backoff 2^4=16s, c and d
	// released untouched. w2 continues independently.
	api := newFakeSQS([]types.Message{
		msg("a", "w1", 1), msg("x", "w2", 1), msg("b", "w1", 4), msg("c", "w1", 1), msg("y", "w2", 1), msg("d", "w1", 1),
	})
	ready := &fakeReadiness{ready: true}
	sub := &fakeSubmitter{results: map[string]error{"b": fmt.Errorf("%w: lock timeout", app.ErrTransient)}}
	m := &countingMetrics{}
	c := newTestConsumerWith(t, api, sub, ready, m)
	runUntil(t, c, func() bool { d, v, _ := api.snapshot(); return len(d) == 3 && len(v) == 3 })
	if m.retries != 1 {
		t.Errorf("retries metric = %d, want 1 (released messages are not retries)", m.retries)
	}

	d, v, _ := api.snapshot()
	sort.Strings(d)
	if strings.Join(d, ",") != "a,x,y" {
		t.Fatalf("deleted %v", d)
	}
	if v["b"][0] != 16 || v["c"][0] != 0 || v["d"][0] != 0 {
		t.Fatalf("visibility %v", v)
	}
	for _, id := range sub.called() {
		if id == "c" || id == "d" {
			t.Fatalf("%s ran after a transient failure in its group", id)
		}
	}
	if ready.checkCount() != 1 {
		t.Fatalf("a database failure must force a readiness check, got %d", ready.checkCount())
	}
}

func TestBackoffCappedAndUnclassifiedIsTransient(t *testing.T) {
	api := newFakeSQS([]types.Message{msg("a", "w1", 9)})
	ready := &fakeReadiness{ready: true}
	sub := &fakeSubmitter{results: map[string]error{"a": errors.New("unclassified")}}
	c := newTestConsumer(t, api, sub, ready)
	runUntil(t, c, func() bool { _, v, _ := api.snapshot(); return len(v) == 1 })
	_, v, sent := api.snapshot()
	if v["a"][0] != 60 || len(sent) != 0 {
		t.Fatalf("visibility %v, DLQ %d", v, len(sent))
	}
	if ready.checkCount() != 0 {
		t.Fatal("only database (ErrTransient) failures force a readiness check")
	}
}

func TestGroupOrderAndParallelism(t *testing.T) {
	// w1's first message blocks until w2's message ran: groups are
	// parallel. Within w1, order is preserved.
	api := newFakeSQS([]types.Message{msg("a1", "w1", 1), msg("b1", "w2", 1), msg("a2", "w1", 1), msg("a3", "w1", 1)})
	gate := make(chan struct{})
	sub := &fakeSubmitter{block: map[string]chan struct{}{"a1": gate}}
	c := newTestConsumer(t, api, sub, &fakeReadiness{ready: true})
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { d, _, _ := api.snapshot(); return len(d) == 1 })
	close(gate)
	waitFor(t, func() bool { d, _, _ := api.snapshot(); return len(d) == 4 })
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	var w1 []string
	for _, id := range sub.called() {
		if strings.HasPrefix(id, "a") {
			w1 = append(w1, id)
		}
	}
	if strings.Join(w1, ",") != "a1,a2,a3" {
		t.Fatalf("group order %v", w1)
	}
}

func TestPausesWhileNotReady(t *testing.T) {
	api := newFakeSQS([]types.Message{msg("a", "w1", 1)})
	ready := &fakeReadiness{}
	c := newTestConsumer(t, api, &fakeSubmitter{}, ready)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	api.mu.Lock()
	receives := api.receives
	api.mu.Unlock()
	if receives != 0 {
		t.Fatalf("received %d times while not ready", receives)
	}
	ready.set(true)
	waitFor(t, func() bool { d, _, _ := api.snapshot(); return len(d) == 1 })
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStopFinishesInFlightAndReleasesTheRest(t *testing.T) {
	api := newFakeSQS([]types.Message{msg("a", "w1", 1), msg("b", "w1", 1), msg("c", "w1", 1)})
	gate := make(chan struct{})
	sub := &fakeSubmitter{block: map[string]chan struct{}{"a": gate}}
	c := newTestConsumer(t, api, sub, &fakeReadiness{ready: true})
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(sub.called()) == 1 })
	stopped := make(chan error, 1)
	go func() { stopped <- c.Stop(context.Background()) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a handler was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	d, v, _ := api.snapshot()
	if strings.Join(d, ",") != "a" || v["b"][0] != 0 || v["c"][0] != 0 {
		t.Fatalf("deleted %v, visibility %v", d, v)
	}
	if got := sub.called(); len(got) != 1 {
		t.Fatalf("messages started after Stop: %v", got)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("done not closed")
	}
}

func TestStartRequiresResolve(t *testing.T) {
	c := New(newFakeSQS(), &fakeSubmitter{}, &fakeReadiness{}, fixedClock{}, Config{HandlerTimeout: time.Second, PauseInterval: time.Second, MaxMessages: 1},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err := c.Start(); err == nil {
		t.Fatal("started without queue URLs")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
