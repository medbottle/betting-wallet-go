package bootstrap

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"go.uber.org/fx"

	"github.com/medbottle/betting-wallet-go/internal/adapters/sqsconsumer"
	"github.com/medbottle/betting-wallet-go/internal/adapters/sqspublisher"
	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/app/wagering"
	"github.com/medbottle/betting-wallet-go/internal/platform/config"
	"github.com/medbottle/betting-wallet-go/internal/platform/health"
)

// ConsumerModule provides the wager-transactions consumer with its own
// credentials (ARCHITECTURE §10). Its hook resolves the inbound queue and
// DLQ URLs on start (§14 step 4); the runners hook starts it only when
// RUN_CONSUMER is set.
var ConsumerModule = fx.Module("consumer",
	fx.Provide(newConsumer),
	fx.Invoke(registerConsumerQueues),
)

// consumerTransport is closed on stop so no idle connection outlives the
// application.
type consumerTransport struct{ *http.Transport }

func newConsumer(cfg config.Config, svc *wagering.Service, readiness *health.Readiness, clock app.Clock, logger *slog.Logger) (*sqsconsumer.Consumer, consumerTransport) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// The long poll holds a request for up to CONSUMER_WAIT_SECONDS.
	timeout := sqsHTTPTimeout + time.Duration(cfg.ConsumerWaitSeconds)*time.Second
	client := sqspublisher.NewClient(sqspublisher.ClientConfig{
		Endpoint:        cfg.SQSEndpoint,
		Region:          cfg.AWSRegion,
		AccessKeyID:     cfg.SQSConsumerAccessKeyID,
		SecretAccessKey: cfg.SQSConsumerSecretAccessKey,
		HTTPClient:      &http.Client{Timeout: timeout, Transport: transport},
	})
	c := sqsconsumer.New(client, svc, readiness, clock, sqsconsumer.Config{
		Queue:          cfg.SQSInboundQueue,
		DLQ:            cfg.SQSInboundDLQ,
		WaitSeconds:    cfg.ConsumerWaitSeconds,
		MaxMessages:    cfg.ConsumerMaxMessages,
		HandlerTimeout: cfg.ConsumerHandlerTimeout,
		BackoffMax:     cfg.ConsumerBackoffMax,
		PauseInterval:  cfg.ConsumerPauseInterval,
	}, logger)
	return c, consumerTransport{transport}
}

func registerConsumerQueues(lc fx.Lifecycle, c *sqsconsumer.Consumer, t consumerTransport, cfg config.Config, logger *slog.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := c.Resolve(ctx); err != nil {
				// Fx does not call OnStop for a hook whose OnStart failed.
				t.CloseIdleConnections()
				return err
			}
			logger.Info("sqs queues resolved", slog.String("queue", cfg.SQSInboundQueue), slog.String("dlq", cfg.SQSInboundDLQ))
			return nil
		},
		OnStop: func(context.Context) error {
			t.CloseIdleConnections()
			return nil
		},
	})
}
