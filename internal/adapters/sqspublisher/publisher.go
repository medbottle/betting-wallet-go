// Package sqspublisher sends outbox events to the events FIFO queue
// (ARCHITECTURE §9/§10): MessageGroupId = aggregateId (wallet),
// MessageDeduplicationId = eventId, body = the frozen envelope, with
// eventType and eventVersion as message attributes for routing (§22).
package sqspublisher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/medbottle/betting-wallet-go/internal/app"
)

// ClientConfig builds an SQS client for one set of credentials.
type ClientConfig struct {
	// Endpoint overrides the AWS endpoint (the emulator); empty uses AWS.
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	HTTPClient      *http.Client
}

// NewClient returns an SQS client with static credentials. Shared AWS
// config files and environment are deliberately not read, so each role
// uses exactly the credentials it was given (ARCHITECTURE §10).
func NewClient(cfg ClientConfig) *sqs.Client {
	opts := sqs.Options{
		Region:      cfg.Region,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")),
	}
	if cfg.HTTPClient != nil {
		opts.HTTPClient = cfg.HTTPClient
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	return sqs.New(opts)
}

// Publisher implements app.EventPublisher on one queue, resolved by name at
// startup (ARCHITECTURE §14 step 4).
type Publisher struct {
	client    *sqs.Client
	queueName string
	queueURL  atomic.Pointer[string]
}

var _ app.EventPublisher = (*Publisher)(nil)

// ResolveQueueURL looks a queue up by name in the caller's own account.
func ResolveQueueURL(ctx context.Context, client *sqs.Client, name string) (string, error) {
	return ResolveQueueURLInAccount(ctx, client, name, "")
}

// ResolveQueueURLInAccount looks a queue up by name in ownerAccountID
// (QueueOwnerAWSAccountId), for a caller from another account; an empty
// owner is the caller's own account.
func ResolveQueueURLInAccount(ctx context.Context, client *sqs.Client, name, ownerAccountID string) (string, error) {
	in := &sqs.GetQueueUrlInput{QueueName: aws.String(name)}
	if ownerAccountID != "" {
		in.QueueOwnerAWSAccountId = aws.String(ownerAccountID)
	}
	out, err := client.GetQueueUrl(ctx, in)
	if err != nil {
		return "", fmt.Errorf("sqs: resolve queue %s: %w", name, err)
	}
	return aws.ToString(out.QueueUrl), nil
}

// New returns a publisher for the queue named queueName; call Resolve
// before publishing.
func New(client *sqs.Client, queueName string) *Publisher {
	return &Publisher{client: client, queueName: queueName}
}

// Resolve looks the queue URL up. A missing queue fails the start.
func (p *Publisher) Resolve(ctx context.Context) error {
	u, err := ResolveQueueURL(ctx, p.client, p.queueName)
	if err != nil {
		return err
	}
	p.queueURL.Store(&u)
	return nil
}

func (p *Publisher) url() (string, error) {
	if u := p.queueURL.Load(); u != nil {
		return *u, nil
	}
	return "", fmt.Errorf("sqs: queue %s not resolved", p.queueName)
}

// PublishBatch implements app.EventPublisher with one SendMessageBatch.
// Entry ids are the batch positions; results come back in input order.
func (p *Publisher) PublishBatch(ctx context.Context, events []app.OutboundEvent) ([]app.PublishResult, error) {
	if len(events) == 0 {
		return nil, nil
	}
	if len(events) > 10 {
		return nil, errors.New("sqs: at most 10 entries per batch")
	}
	entries := make([]types.SendMessageBatchRequestEntry, len(events))
	for i, e := range events {
		entries[i] = types.SendMessageBatchRequestEntry{
			Id:                     aws.String(strconv.Itoa(i)),
			MessageBody:            aws.String(string(e.Body)),
			MessageGroupId:         aws.String(e.GroupID),
			MessageDeduplicationId: aws.String(e.EventID.String()),
			MessageAttributes: map[string]types.MessageAttributeValue{
				"eventType":    {DataType: aws.String("String"), StringValue: aws.String(e.EventType)},
				"eventVersion": {DataType: aws.String("Number"), StringValue: aws.String(strconv.Itoa(e.EventVersion))},
			},
		}
	}
	queueURL, err := p.url()
	if err != nil {
		return nil, err
	}
	out, err := p.client.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{QueueUrl: aws.String(queueURL), Entries: entries})
	if err != nil {
		return nil, fmt.Errorf("sqs: send batch: %w", err)
	}
	results := make([]app.PublishResult, len(events))
	seen := make([]bool, len(events))
	for i, e := range events {
		results[i].EventID = e.EventID
	}
	for _, ok := range out.Successful {
		if i, err := strconv.Atoi(aws.ToString(ok.Id)); err == nil && i >= 0 && i < len(events) {
			seen[i] = true
		}
	}
	for _, f := range out.Failed {
		i, err := strconv.Atoi(aws.ToString(f.Id))
		if err != nil || i < 0 || i >= len(events) {
			continue
		}
		seen[i] = true
		results[i].Err = fmt.Errorf("sqs: %s: %s", aws.ToString(f.Code), aws.ToString(f.Message))
		results[i].SenderFault = f.SenderFault
	}
	for i := range events {
		if !seen[i] {
			// Neither successful nor failed: treat as failed so it is retried.
			results[i].Err = errors.New("sqs: entry missing from the batch response")
		}
	}
	return results, nil
}

// HealthCheck is a readiness probe that reads one attribute of the queue
// (ARCHITECTURE §10: readiness checks PostgreSQL and SQS).
func (p *Publisher) HealthCheck(ctx context.Context) error {
	queueURL, err := p.url()
	if err != nil {
		return err
	}
	_, err = p.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(queueURL),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	if err != nil {
		return fmt.Errorf("sqs: %w", err)
	}
	return nil
}
