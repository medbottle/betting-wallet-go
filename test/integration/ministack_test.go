//go:build integration

package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/medbottle/betting-wallet-go/internal/adapters/sqspublisher"
)

// ministackImage must match docker-compose.yml.
const ministackImage = "ministackorg/ministack:1.5.27"

// Test credentials (local values; the emulator does not check them, see
// TestBrokerCredentialSeparation and TestConsumerCredentialSeparation).
const (
	publisherKey    = "wallet-publisher"
	publisherSecret = "wallet-publisher-local-secret"
	consumerKey     = "wallet-consumer"
	consumerSecret  = "wallet-consumer-local-secret"
	operatorKey     = "wallet-operator"
	operatorSecret  = "wallet-operator-local-secret"
	readerKey       = "test-reader"
	readerSecret    = "test-reader-secret"
)

var (
	msOnce      sync.Once
	msEndpoint  string
	msErr       error
	msContainer testcontainers.Container
)

// startMiniStack runs one real MiniStack for the package, provisioned by
// the same init script as compose (deploy/ministack/init-queues.sh).
func startMiniStack(t *testing.T) string {
	t.Helper()
	msOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		script, err := filepath.Abs(filepath.Join(repoRoot(t), "deploy", "ministack", "init-queues.sh"))
		if err != nil {
			msErr = err
			return
		}
		c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        ministackImage,
				Env:          map[string]string{"MINISTACK_REGION": "us-east-1"},
				ExposedPorts: []string{"4566/tcp"},
				Files: []testcontainers.ContainerFile{{
					HostFilePath:      script,
					ContainerFilePath: "/etc/localstack/init/ready.d/init-queues.sh",
					FileMode:          0o755,
				}},
				WaitingFor: wait.ForLog("init-queues: queues ready").WithStartupTimeout(2 * time.Minute),
			},
			Started: true,
		})
		msContainer = c
		if err != nil {
			msErr = fmt.Errorf("start ministack: %w", err)
			return
		}
		host, err := c.Host(ctx)
		if err != nil {
			msErr = err
			return
		}
		port, err := c.MappedPort(ctx, "4566/tcp")
		if err != nil {
			msErr = err
			return
		}
		msEndpoint = fmt.Sprintf("http://%s:%s", host, port.Port())
	})
	if msErr != nil {
		t.Fatal(msErr)
	}
	return msEndpoint
}

func sqsClient(endpoint, key, secret string) *sqs.Client {
	return sqspublisher.NewClient(sqspublisher.ClientConfig{Endpoint: endpoint, Region: "us-east-1", AccessKeyID: key, SecretAccessKey: secret})
}

// newEventsQueue creates a FIFO queue private to one test, configured like
// wallet-events.fifo (explicit deduplication ids), and returns its name.
func newEventsQueue(t *testing.T, endpoint string) string {
	t.Helper()
	name := "events-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16] + ".fifo"
	_, err := sqsClient(endpoint, readerKey, readerSecret).CreateQueue(ctxT(t), &sqs.CreateQueueInput{
		QueueName: aws.String(name),
		Attributes: map[string]string{
			"FifoQueue": "true", "ContentBasedDeduplication": "false", "VisibilityTimeout": "30",
		},
	})
	if err != nil {
		t.Fatalf("create queue: %v", err)
	}
	return name
}

// receivedMessage is what a downstream consumer sees.
type receivedMessage struct {
	Body            string
	GroupID         string
	DeduplicationID string
	EventType       string
	EventVersion    string
}

// drain receives (and deletes) messages until want arrived or the time is
// up; it keeps reading briefly after that to catch duplicates.
func drain(t *testing.T, endpoint, queue string, want int, timeout time.Duration) []receivedMessage {
	t.Helper()
	client := sqsClient(endpoint, readerKey, readerSecret)
	url, err := sqspublisher.ResolveQueueURL(ctxT(t), client, queue)
	if err != nil {
		t.Fatal(err)
	}
	var got []receivedMessage
	deadline := time.Now().Add(timeout)
	extra := time.Time{}
	for time.Now().Before(deadline) && (extra.IsZero() || time.Now().Before(extra)) {
		out, err := client.ReceiveMessage(ctxT(t), &sqs.ReceiveMessageInput{
			QueueUrl:                    aws.String(url),
			MaxNumberOfMessages:         10,
			WaitTimeSeconds:             1,
			MessageAttributeNames:       []string{"All"},
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
		})
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
		for _, m := range out.Messages {
			got = append(got, receivedMessage{
				Body:            aws.ToString(m.Body),
				GroupID:         m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)],
				DeduplicationID: m.Attributes[string(types.MessageSystemAttributeNameMessageDeduplicationId)],
				EventType:       aws.ToString(m.MessageAttributes["eventType"].StringValue),
				EventVersion:    aws.ToString(m.MessageAttributes["eventVersion"].StringValue),
			})
			if _, err := client.DeleteMessage(ctxT(t), &sqs.DeleteMessageInput{QueueUrl: aws.String(url), ReceiptHandle: m.ReceiptHandle}); err != nil {
				t.Fatalf("delete: %v", err)
			}
		}
		if len(got) >= want && extra.IsZero() {
			extra = time.Now().Add(1500 * time.Millisecond)
		}
	}
	return got
}
