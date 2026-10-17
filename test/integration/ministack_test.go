//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/medbottle/betting-wallet-go/internal/platform/config"
)

// ministackImage must match docker-compose.yml.
const ministackImage = "ministackorg/ministack:1.5.27"

// Broker identities of the tests (ARCHITECTURE §28). MiniStack runs with
// AUTH=true, as in compose: the init script creates each identity and
// writes the keys it generated to the credentials file, which the tests
// read from the container. asAdmin is the queue owner's root key (test),
// the emulator's administrator: the harness creates queues and reads them
// with it, and it acts for no provider.
type brokerID string

const (
	asAdmin     brokerID = "admin"
	asConsumer  brokerID = "consumer"
	asPublisher brokerID = "publisher"
	asOperator  brokerID = "operator"
	asProviderA brokerID = "provider-a"
	asProviderB brokerID = "provider-b"
)

// SenderIds the consumer sees (ARCHITECTURE §10): MiniStack reports the
// sender's account. Providers and the operator send through a role in
// their own account; every user and the admin are in queueOwner.
const (
	providerASender = "111111111111"
	providerBSender = "222222222222"
	operatorSender  = "333333333333"
	queueOwner      = "000000000000"
)

// providerSenders is the SQS_PROVIDER_SENDERS of the tests.
var providerSenders = map[string]string{providerASender: "provider-a", providerBSender: "provider-b"}

// credentialsPath is where init-queues.sh writes the generated keys.
const credentialsPath = "/var/lib/wager-credentials/sqs-credentials.json"

var (
	msOnce      sync.Once
	msEndpoint  string
	msErr       error
	msContainer testcontainers.Container
	msCreds     config.BrokerCredentials
	msClients   sync.Map // brokerID -> *sqs.Client
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
				Env:          map[string]string{"MINISTACK_REGION": "us-east-1", "AUTH": "true"},
				ExposedPorts: []string{"4566/tcp"},
				Files: []testcontainers.ContainerFile{{
					HostFilePath:      script,
					ContainerFilePath: "/etc/localstack/init/ready.d/init-queues.sh",
					FileMode:          0o755,
				}},
				WaitingFor: wait.ForLog("init-queues: identities ready").WithStartupTimeout(2 * time.Minute),
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
		msCreds, msErr = readContainerCredentials(ctx, c)
	})
	if msErr != nil {
		t.Fatal(msErr)
	}
	return msEndpoint
}

// readContainerCredentials reads the keys init-queues.sh generated.
func readContainerCredentials(ctx context.Context, c testcontainers.Container) (config.BrokerCredentials, error) {
	r, err := c.CopyFileFromContainer(ctx, credentialsPath)
	if err != nil {
		return config.BrokerCredentials{}, fmt.Errorf("read %s: %w", credentialsPath, err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return config.BrokerCredentials{}, err
	}
	var creds config.BrokerCredentials
	if err := json.Unmarshal(b, &creds); err != nil {
		return config.BrokerCredentials{}, fmt.Errorf("parse %s: %w", credentialsPath, err)
	}
	return creds, nil
}

// brokerKey is the generated key of id (and the role it acts through).
func brokerKey(t *testing.T, id brokerID) config.BrokerKey {
	t.Helper()
	startMiniStack(t)
	var k config.BrokerKey
	switch id {
	case asAdmin:
		k = config.BrokerKey{AccessKeyID: "test", SecretAccessKey: "test"}
	case asConsumer:
		k = msCreds.Consumer
	case asPublisher:
		k = msCreds.Publisher
	case asOperator:
		k = msCreds.Operator
	default:
		k = msCreds.Providers[string(id)]
	}
	if k.AccessKeyID == "" {
		t.Fatalf("no broker credentials for %s", id)
	}
	return k
}

// sqsAs is an SQS client acting as id, on the package's MiniStack. Clients
// are shared, so a role is assumed once and its session reused.
func sqsAs(t *testing.T, id brokerID) *sqs.Client {
	t.Helper()
	if c, ok := msClients.Load(id); ok {
		return c.(*sqs.Client)
	}
	k := brokerKey(t, id)
	c := sqspublisher.NewClient(sqspublisher.ClientConfig{Endpoint: startMiniStack(t), Region: "us-east-1",
		AccessKeyID: k.AccessKeyID, SecretAccessKey: k.SecretAccessKey, RoleARN: k.RoleARN, RoleSessionName: string(id)})
	actual, _ := msClients.LoadOrStore(id, c)
	return actual.(*sqs.Client)
}

// sqsClient is a client with raw static keys, for the negative cases
// (unknown or wrong credentials, an unreachable endpoint).
func sqsClient(endpoint, key, secret string) *sqs.Client {
	return sqspublisher.NewClient(sqspublisher.ClientConfig{Endpoint: endpoint, Region: "us-east-1", AccessKeyID: key, SecretAccessKey: secret})
}

// applyQueuePolicies gives a test's own inbound queue and DLQ the queue
// policies of wager-transactions.fifo and its DLQ, with the same script.
func applyQueuePolicies(t *testing.T, inbound, dlq string) {
	t.Helper()
	startMiniStack(t)
	url := func(name string) string { return "http://localhost:4566/" + queueOwner + "/" + name }
	code, out, err := msContainer.Exec(ctxT(t), []string{"sh", "/etc/localstack/init/ready.d/init-queues.sh", "queue-policies", url(inbound), url(dlq)})
	if err != nil || code != 0 {
		var msg []byte
		if out != nil {
			msg, _ = io.ReadAll(out)
		}
		t.Fatalf("queue policies for %s: exit %d %v: %s", inbound, code, err, msg)
	}
}

// newEventsQueue creates a FIFO queue private to one test, configured like
// wallet-events.fifo (explicit deduplication ids), and returns its name.
func newEventsQueue(t *testing.T, endpoint string) string {
	t.Helper()
	name := "wallet-events-t-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16] + ".fifo"
	_, err := sqsAs(t, asAdmin).CreateQueue(ctxT(t), &sqs.CreateQueueInput{
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
	client := sqsAs(t, asAdmin)
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
