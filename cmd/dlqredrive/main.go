// Command dlqredrive moves messages that exhausted their transient retries
// from wager-transactions-dlq.fifo back to wager-transactions.fifo
// (ARCHITECTURE §10, redrive procedure). Messages marked
// failureClass=PERMANENT are left in the DLQ: they need a fix by the
// producer. Each resend names its original producer in originalSenderId,
// which the consumer trusts only from the operator's identity. Confirm the
// cause is resolved (readiness ok) before running it.
//
// Environment: SQS_ENDPOINT (empty for AWS), AWS_REGION, SQS_INBOUND_QUEUE,
// SQS_INBOUND_DLQ, the operator credentials SQS_OPERATOR_ACCESS_KEY_ID /
// SQS_OPERATOR_SECRET_ACCESS_KEY, the operator's own sender id
// SQS_OPERATOR_SENDER_ID, and SQS_QUEUE_OWNER_ACCOUNT_ID when the queues
// belong to another account than the operator's.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/medbottle/betting-wallet-go/internal/adapters/sqsconsumer"
	"github.com/medbottle/betting-wallet-go/internal/adapters/sqspublisher"
)

type config struct {
	Endpoint        string `env:"SQS_ENDPOINT"`
	Region          string `env:"AWS_REGION" envDefault:"us-east-1"`
	Queue           string `env:"SQS_INBOUND_QUEUE" envDefault:"wager-transactions.fifo"`
	DLQ             string `env:"SQS_INBOUND_DLQ" envDefault:"wager-transactions-dlq.fifo"`
	AccessKeyID     string `env:"SQS_OPERATOR_ACCESS_KEY_ID,required,notEmpty"`
	SecretAccessKey string `env:"SQS_OPERATOR_SECRET_ACCESS_KEY,required,notEmpty"`
	// SenderID is the SenderId SQS reports for the operator's messages:
	// a DLQ message from it was resent before, and its originalSenderId is
	// kept (ARCHITECTURE §10).
	SenderID string `env:"SQS_OPERATOR_SENDER_ID,required,notEmpty"`
	// QueueOwnerAccountID resolves the queues of another account
	// (QueueOwnerAWSAccountId); empty is the operator's own account.
	QueueOwnerAccountID string `env:"SQS_QUEUE_OWNER_ACCOUNT_ID"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "dlqredrive:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, out io.Writer) error {
	cfg, err := env.ParseAs[config]()
	if err != nil {
		return err
	}
	client := sqspublisher.NewClient(sqspublisher.ClientConfig{
		Endpoint: cfg.Endpoint, Region: cfg.Region, AccessKeyID: cfg.AccessKeyID, SecretAccessKey: cfg.SecretAccessKey,
	})
	queueURL, err := sqspublisher.ResolveQueueURLInAccount(ctx, client, cfg.Queue, cfg.QueueOwnerAccountID)
	if err != nil {
		return err
	}
	dlqURL, err := sqspublisher.ResolveQueueURLInAccount(ctx, client, cfg.DLQ, cfg.QueueOwnerAccountID)
	if err != nil {
		return err
	}
	res, err := sqsconsumer.Redrive(ctx, client, dlqURL, queueURL, cfg.SenderID, time.Now)
	fmt.Fprintf(out, "redriven=%d skipped_permanent=%d\n", res.Redriven, res.Skipped)
	return err
}
