// Package sqsconsumer consumes wager-transactions.fifo (ARCHITECTURE §10)
// and hands each message to SubmitWagerTransaction with an inbox entry
// (§9). The provider a message acts for comes from its sender's broker
// identity, never from the body alone (§11). A message is deleted only
// after its handling committed; transient failures change its visibility;
// permanent ones go to the DLQ explicitly.
package sqsconsumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/app/authz"
	"github.com/medbottle/betting-wallet-go/internal/app/wagering"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
	"github.com/medbottle/betting-wallet-go/internal/platform/health"
)

// ConsumerName is the inbox consumer_name (ARCHITECTURE §23, G3).
const ConsumerName = "wager-transactions"

// fallbackGroup is the DLQ MessageGroupId when the received message has
// none (ARCHITECTURE §10).
const fallbackGroup = "invalid-messages"

// opTimeout bounds each delete, visibility change and DLQ send. Their
// context keeps the poll loop's values but not its cancellation
// (context.WithoutCancel), so they still happen during shutdown
// (ARCHITECTURE §14, §23). It is also the margin left after a message's
// handler deadline for settling it within its visibility.
const opTimeout = 5 * time.Second

// DLQ message attributes (ARCHITECTURE §10).
const (
	AttrFailureClass      = "failureClass"
	AttrFailureCode       = "failureCode"
	AttrOriginalMessageID = "originalMessageId"
	AttrEnvelopeMessageID = "envelopeMessageId"
	FailureClassPermanent = "PERMANENT"
)

// AttrOriginalSenderID carries the original producer of a message the
// redrive operator resent. The consumer trusts it only on messages whose
// SenderId is the operator's (ARCHITECTURE §10).
const AttrOriginalSenderID = "originalSenderId"

// Authorization codes of ARCHITECTURE §12, used as the DLQ failureCode.
const (
	codeForbidden        wager.FailureCode = "FORBIDDEN"
	codeProviderMismatch wager.FailureCode = "PROVIDER_MISMATCH"
)

// errUnknownSender means a message's broker identity maps to no provider.
var errUnknownSender = errors.New("sqs: sender is not a registered provider identity")

// API is the part of the SQS client the consumer uses.
type API interface {
	GetQueueUrl(ctx context.Context, in *sqs.GetQueueUrlInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error)
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(ctx context.Context, in *sqs.ChangeMessageVisibilityInput, optFns ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
	SendMessage(ctx context.Context, in *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// Submitter is SubmitWagerTransaction.
type Submitter interface {
	Submit(ctx context.Context, cmd wagering.SubmitCommand) (wagering.SubmitResult, error)
}

// Readiness gates polling (ARCHITECTURE §10: pause while readiness fails).
type Readiness interface {
	Report() health.Report
	CheckNow(ctx context.Context)
}

// Config holds the queue names, timings and trusted senders (ARCHITECTURE
// §10/§23).
type Config struct {
	Queue          string
	DLQ            string
	WaitSeconds    int32
	MaxMessages    int32
	HandlerTimeout time.Duration
	BackoffMax     time.Duration
	PauseInterval  time.Duration
	// Visibility is set on every receive (whole seconds), so the consumer
	// knows when an unfinished message becomes visible again. It must
	// leave room for one handler deadline plus settling.
	Visibility time.Duration
	// Senders maps a producer's broker identity (the SenderId up to any ':')
	// to the provider its messages act for.
	Senders map[string]string
	// OperatorSenderID is the redrive operator's identity: only its messages
	// may name their original sender in AttrOriginalSenderID.
	OperatorSenderID string
}

// Stats counts message outcomes.
type Stats struct {
	Handled    int64 // committed (processed, rejected, pending reference, failed)
	Duplicates int64
	DeadLetter int64
	Retried    int64
	Released   int64
}

// Consumer is the single poller of one instance (ARCHITECTURE §23, G1).
type Consumer struct {
	api       API
	submitter Submitter
	readiness Readiness
	clock     app.Clock
	cfg       Config
	logger    *slog.Logger
	metrics   app.Metrics

	queueURL, dlqURL string

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	started bool

	handled, duplicates, deadLettered, retried, released atomic.Int64
}

// New returns an unstarted consumer; call Resolve before Start. A nil
// metrics discards the measurements.
func New(api API, submitter Submitter, readiness Readiness, clock app.Clock, cfg Config, logger *slog.Logger, metrics app.Metrics) *Consumer {
	return &Consumer{api: api, submitter: submitter, readiness: readiness, clock: clock, cfg: cfg,
		logger: logger.With(slog.String("component", "consumer")), metrics: app.OrNop(metrics)}
}

// Resolve looks both queue URLs up (ARCHITECTURE §14 step 4).
func (c *Consumer) Resolve(ctx context.Context) error {
	for _, q := range []struct {
		name string
		dst  *string
	}{{c.cfg.Queue, &c.queueURL}, {c.cfg.DLQ, &c.dlqURL}} {
		out, err := c.api.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(q.name)})
		if err != nil {
			return fmt.Errorf("sqs: resolve queue %s: %w", q.name, err)
		}
		*q.dst = aws.ToString(out.QueueUrl)
	}
	return nil
}

// Start launches the poll loop on its own context and returns at once.
func (c *Consumer) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return errors.New("consumer: already started")
	}
	if c.queueURL == "" || c.dlqURL == "" {
		return errors.New("consumer: queues not resolved")
	}
	if c.cfg.HandlerTimeout <= 0 || c.cfg.PauseInterval <= 0 || c.cfg.MaxMessages < 1 {
		return errors.New("consumer: handler timeout, pause interval and max messages must be positive")
	}
	if len(c.cfg.Senders) == 0 {
		return errors.New("consumer: no provider sender identities configured")
	}
	if c.cfg.Visibility < c.cfg.HandlerTimeout+opTimeout {
		return fmt.Errorf("consumer: visibility %s must cover the handler timeout %s plus %s to settle", c.cfg.Visibility, c.cfg.HandlerTimeout, opTimeout)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel, c.done, c.started = cancel, make(chan struct{}), true
	go func() {
		defer close(c.done)
		c.loop(ctx)
	}()
	c.logger.Info("consumer started", slog.String("queue", c.cfg.Queue))
	return nil
}

// Stop stops polling and starting messages. Handlers in flight finish
// within their deadline; received messages not started are released with
// visibility 0 (ARCHITECTURE §14). It waits for that, or for ctx.
func (c *Consumer) Stop(ctx context.Context) error {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return nil
	}
	c.cancel()
	done := c.done
	c.mu.Unlock()
	select {
	case <-done:
		s := c.Stats()
		c.logger.Info("consumer stopped", slog.Int64("handled", s.Handled), slog.Int64("duplicates", s.Duplicates),
			slog.Int64("deadLettered", s.DeadLetter), slog.Int64("retried", s.Retried), slog.Int64("released", s.Released))
		return nil
	case <-ctx.Done():
		return fmt.Errorf("consumer: stop: %w", ctx.Err())
	}
}

// Done is closed once the loop has returned after Stop.
func (c *Consumer) Done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done
}

// Stats returns the outcome counters.
func (c *Consumer) Stats() Stats {
	return Stats{Handled: c.handled.Load(), Duplicates: c.duplicates.Load(), DeadLetter: c.deadLettered.Load(),
		Retried: c.retried.Load(), Released: c.released.Load()}
}

// loop polls until ctx is cancelled. A batch is finished (or released)
// before the next receive, so at most MaxMessages are in flight.
func (c *Consumer) loop(ctx context.Context) {
	paused := false
	for ctx.Err() == nil {
		if !c.readiness.Report().Ready {
			if !paused {
				c.logger.Warn("not ready, polling paused")
				paused = true
			}
			c.wait(ctx)
			continue
		}
		if paused {
			c.logger.Info("ready, polling resumed")
			paused = false
		}
		out, err := c.api.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.queueURL),
			MaxNumberOfMessages: c.cfg.MaxMessages,
			WaitTimeSeconds:     c.cfg.WaitSeconds,
			VisibilityTimeout:   int32(c.cfg.Visibility / time.Second),
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{
				types.MessageSystemAttributeNameMessageGroupId, types.MessageSystemAttributeNameApproximateReceiveCount,
				types.MessageSystemAttributeNameSenderId,
			},
			MessageAttributeNames: []string{AttrOriginalSenderID},
		})
		if err != nil {
			if ctx.Err() == nil {
				c.logger.Warn("receive failed", slog.String("error", err.Error()))
				c.wait(ctx)
			}
			continue
		}
		c.handleBatch(ctx, out.Messages, c.clock.Now())
	}
}

func (c *Consumer) wait(ctx context.Context) {
	t := time.NewTimer(c.cfg.PauseInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// handleBatch runs each MessageGroupId's messages in order, groups in
// parallel (ARCHITECTURE §10). The group's remaining messages are released
// untouched after a transient failure, once stopping, or when the next one
// after the first might not finish before the batch's visibility ends: it
// would become visible again while it runs, and another instance could
// take it, or a later message of its group, out of order.
func (c *Consumer) handleBatch(stop context.Context, msgs []types.Message, receivedAt time.Time) {
	// Settling must outlive Stop: it releases what will not run (§14).
	settle := context.WithoutCancel(stop)
	var order []string
	groups := map[string][]types.Message{}
	for _, m := range msgs {
		g := groupOf(m)
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], m)
	}
	var wg sync.WaitGroup
	for _, g := range order {
		wg.Add(1)
		go func(msgs []types.Message) {
			defer wg.Done()
			for i, m := range msgs {
				if stop.Err() != nil {
					c.release(settle, msgs[i:])
					return
				}
				// The first message always fits: Start requires visibility
				// >= handler deadline + settle margin.
				if i > 0 && !c.fitsVisibility(receivedAt) {
					c.logger.Info("group could outlive its visibility, released the rest",
						slog.String("messageGroupId", groupOf(m)), slog.Int("released", len(msgs)-i))
					c.release(settle, msgs[i:])
					return
				}
				if !c.handle(stop, settle, m) {
					c.release(settle, msgs[i+1:])
					return
				}
			}
		}(groups[g])
	}
	wg.Wait()
}

// fitsVisibility reports whether a message of the batch received at
// receivedAt, started now, can finish its handler and be settled before
// its visibility ends.
func (c *Consumer) fitsVisibility(receivedAt time.Time) bool {
	return c.clock.Now().Sub(receivedAt)+c.cfg.HandlerTimeout+opTimeout <= c.cfg.Visibility
}

// handle processes one message and settles it with SQS. It reports false
// when the message was left for redelivery (the group must stop). stop is
// cancelled by Stop; settle is the context of the SQS calls that settle
// the message, which outlive Stop.
//
// The order follows the HTTP transport (ARCHITECTURE §11): authenticate
// the sender, parse the envelope, check its providerId against the
// sender's provider, then run the use case. The first three persist
// nothing and fail permanently.
func (c *Consumer) handle(stop, settle context.Context, m types.Message) bool {
	sqsID := aws.ToString(m.MessageId)
	body := []byte(aws.ToString(m.Body))
	sender := originalSender(m, c.cfg.OperatorSenderID)
	log := c.logger.With(slog.String("sqsMessageId", sqsID), slog.String("senderId", sender))
	principal, err := c.authenticate(sender)
	if err != nil {
		return c.deadLetterCode(settle, m, peekMessageID(body), sender, codeForbidden, err, log)
	}
	providerID, _ := principal.ProviderID()
	log = log.With(slog.String("providerId", providerID))
	p, envID, err := parseEnvelope(body)
	if err != nil {
		return c.deadLetter(settle, m, envID, sender, err, log)
	}
	log = log.With(slog.String("messageId", p.MessageID))
	if err := principal.CanSubmitTransaction(p.Raw.ProviderID); err != nil {
		code := codeForbidden
		if errors.Is(err, authz.ErrProviderMismatch) {
			code = codeProviderMismatch
		}
		return c.deadLetterCode(settle, m, envID, sender, code, fmt.Errorf("%w: the sender acts for %s, the message names %q", err, providerID, p.Raw.ProviderID), log)
	}

	// The handler deadline is independent of Stop: in-flight work finishes
	// or rolls back on its own (ARCHITECTURE §14).
	hctx, cancel := context.WithTimeout(settle, c.cfg.HandlerTimeout)
	res, err := c.submitter.Submit(hctx, wagering.SubmitCommand{
		Raw:           p.Raw,
		CorrelationID: p.MessageID,
		CausationID:   &p.MessageID,
		Inbox:         &wagering.InboxEntry{ConsumerName: ConsumerName, MessageID: p.MessageID, ReceivedAt: c.clock.Now()},
	})
	cancel()
	if err == nil {
		if res.Duplicate {
			c.duplicates.Add(1)
			log.Info("duplicate message, already handled")
		} else {
			c.handled.Add(1)
		}
		c.delete(settle, m, log)
		return true
	}
	if code, ok := permanentCode(err); ok {
		return c.deadLetterCode(settle, m, envID, sender, code, err, log)
	}
	c.retry(stop, settle, m, err, log)
	return false
}

// originalSender is the broker identity a message is attributed to: the
// SenderId SQS reports or, when the redrive operator sent it, the original
// sender it names in AttrOriginalSenderID (ARCHITECTURE §10). No other
// sender is trusted with that attribute. "" when unknown.
func originalSender(m types.Message, operatorSenderID string) string {
	s := principalID(m.Attributes[string(types.MessageSystemAttributeNameSenderId)])
	if s != "" && s == principalID(operatorSenderID) {
		return principalID(aws.ToString(m.MessageAttributes[AttrOriginalSenderID].StringValue))
	}
	return s
}

// principalID is a SenderId up to any ':'. An IAM role's SenderId is
// <role id>:<session name>, and the caller picks the session name; the
// role id is the identity.
func principalID(senderID string) string {
	id, _, _ := strings.Cut(senderID, ":")
	return id
}

// authenticate maps a sender to the provider it acts for. The principal
// holds the provider role only: it may submit operations for that
// provider and nothing else (ARCHITECTURE §11).
func (c *Consumer) authenticate(sender string) (authz.Principal, error) {
	providerID, ok := c.cfg.Senders[sender]
	if sender == "" || !ok {
		return authz.Principal{}, fmt.Errorf("%w: %q", errUnknownSender, sender)
	}
	return authz.NewPrincipal(sender, providerID, []string{string(authz.RoleProvider)})
}

// permanentCode classifies a use case error (ARCHITECTURE §10/§23):
// category A and C errors, an unknown provider, a reused messageId and an
// invariant violation that could not be recorded as FAILED (category D,
// §5) are permanent. Everything else, including unclassified errors, is
// transient (G8).
func permanentCode(err error) (wager.FailureCode, bool) {
	var ve *wager.ValidationError
	var ce *wager.ConflictError
	switch {
	case errors.Is(err, app.ErrTransient):
		return "", false
	case errors.As(err, &ve):
		return ve.Code, true
	case errors.As(err, &ce):
		return ce.Code, true
	case errors.Is(err, app.ErrProviderNotAllowed):
		return codeForbidden, true
	case errors.Is(err, app.ErrInboxHashMismatch):
		return wager.CodeInvalidRequest, true
	case errors.Is(err, app.ErrInvariantViolation):
		return wager.CodeInternalInvariantViolation, true
	}
	return "", false
}

func (c *Consumer) deadLetter(ctx context.Context, m types.Message, envID, sender string, err error, log *slog.Logger) bool {
	code := wager.CodeInvalidRequest
	var ve *wager.ValidationError
	if errors.As(err, &ve) {
		code = ve.Code
	}
	return c.deadLetterCode(ctx, m, envID, sender, code, err, log)
}

// deadLetterCode sends the message to the DLQ, then deletes it. A failed
// send leaves the message for redelivery; the send is deduplicated by the
// SQS MessageId if it is repeated within 5 minutes. The copy names the
// sender it was attributed to, since its own SenderId is the consumer's.
func (c *Consumer) deadLetterCode(ctx context.Context, m types.Message, envID, sender string, code wager.FailureCode, cause error, log *slog.Logger) bool {
	attrs := map[string]types.MessageAttributeValue{
		AttrFailureClass:      strAttr(FailureClassPermanent),
		AttrFailureCode:       strAttr(string(code)),
		AttrOriginalMessageID: strAttr(aws.ToString(m.MessageId)),
	}
	if envID != "" {
		attrs[AttrEnvelopeMessageID] = strAttr(envID)
	}
	if sender != "" {
		attrs[AttrOriginalSenderID] = strAttr(sender)
	}
	sendCtx, cancel := context.WithTimeout(ctx, opTimeout)
	_, err := c.api.SendMessage(sendCtx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.dlqURL),
		MessageBody:            m.Body,
		MessageGroupId:         aws.String(groupOf(m)),
		MessageDeduplicationId: m.MessageId,
		MessageAttributes:      attrs,
	})
	cancel()
	if err != nil {
		log.Error("dead-letter send failed, message left for redelivery", slog.String("failureCode", string(code)), slog.String("error", err.Error()))
		c.retried.Add(1)
		c.metrics.Retry(app.RetryConsumer)
		c.setVisibility(ctx, m, c.backoff(m), log)
		return false
	}
	c.deadLettered.Add(1)
	c.metrics.DeadLettered(code)
	log.Warn("message sent to the DLQ", slog.String("failureCode", string(code)), slog.String("error", cause.Error()))
	c.delete(ctx, m, log)
	return true
}

// retry leaves the message in the queue with min(2^receiveCount, max)
// seconds of visibility; the redrive policy moves it to the DLQ after
// maxReceiveCount receives. Once stopping it is released at once instead,
// so another instance can take it. A database failure triggers an
// immediate readiness check, which pauses polling if it fails. stop tells
// whether the consumer is stopping; the calls run on settle.
func (c *Consumer) retry(stop, settle context.Context, m types.Message, err error, log *slog.Logger) {
	c.retried.Add(1)
	c.metrics.Retry(app.RetryConsumer)
	visibility := c.backoff(m)
	if stop.Err() != nil {
		visibility = 0
	}
	log.Warn("transient failure, message left for redelivery", slog.Int("visibilitySeconds", int(visibility)), slog.String("error", err.Error()))
	c.setVisibility(settle, m, visibility, log)
	if errors.Is(err, app.ErrTransient) {
		ctx, cancel := context.WithTimeout(settle, opTimeout)
		c.readiness.CheckNow(ctx)
		cancel()
	}
}

func (c *Consumer) backoff(m types.Message) int32 {
	n, err := strconv.Atoi(m.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
	if err != nil || n < 1 {
		n = 1
	}
	maxSeconds := int64(c.cfg.BackoffMax / time.Second)
	if n >= 31 || int64(1)<<n > maxSeconds {
		return int32(maxSeconds)
	}
	return int32(1) << n
}

// release returns messages that were received but not started.
func (c *Consumer) release(ctx context.Context, msgs []types.Message) {
	for _, m := range msgs {
		c.released.Add(1)
		c.setVisibility(ctx, m, 0, c.logger.With(slog.String("sqsMessageId", aws.ToString(m.MessageId))))
	}
}

// setVisibility changes the message's visibility within opTimeout.
func (c *Consumer) setVisibility(ctx context.Context, m types.Message, seconds int32, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	if _, err := c.api.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(c.queueURL), ReceiptHandle: m.ReceiptHandle, VisibilityTimeout: seconds,
	}); err != nil {
		// The visibility set on receive still returns it later.
		log.Warn("change visibility failed", slog.String("error", err.Error()))
	}
}

// delete runs only after the handling committed, within opTimeout. If it
// fails the message comes back and the inbox reports it as a duplicate.
func (c *Consumer) delete(ctx context.Context, m types.Message, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	if _, err := c.api.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(c.queueURL), ReceiptHandle: m.ReceiptHandle}); err != nil {
		log.Warn("delete failed, message will be redelivered as a duplicate", slog.String("error", err.Error()))
	}
}

func groupOf(m types.Message) string {
	if g := m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]; g != "" {
		return g
	}
	return fallbackGroup
}

func strAttr(v string) types.MessageAttributeValue {
	return types.MessageAttributeValue{DataType: aws.String("String"), StringValue: aws.String(v)}
}
