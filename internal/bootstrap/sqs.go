package bootstrap

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go.uber.org/fx"

	"github.com/medbottle/betting-wallet-go/internal/adapters/sqspublisher"
	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/app/messaging"
	"github.com/medbottle/betting-wallet-go/internal/platform/config"
	"github.com/medbottle/betting-wallet-go/internal/platform/health"
	"github.com/medbottle/betting-wallet-go/internal/platform/worker"
)

// sqsHTTPTimeout bounds one SQS HTTP request; callers add their own
// context deadlines.
const sqsHTTPTimeout = 10 * time.Second

// SQSModule provides the events publisher with its own credentials
// (ARCHITECTURE §10). Its hook resolves the queue URL on start (§14 step 4)
// and its readiness check joins the readiness loop.
var SQSModule = fx.Module("sqs",
	fx.Provide(
		newPublisher,
		fx.Annotate(func(p *sqspublisher.Publisher) app.EventPublisher { return p }),
		fx.Annotate(newSQSCheck, fx.ResultTags(readinessChecksGroup)),
	),
	fx.Invoke(registerQueueResolution),
)

// publisherTransport is closed on stop so no idle connection outlives the
// application.
type publisherTransport struct{ *http.Transport }

func newPublisher(cfg config.Config) (*sqspublisher.Publisher, publisherTransport) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := sqspublisher.NewClient(sqspublisher.ClientConfig{
		Endpoint:        cfg.SQSEndpoint,
		Region:          cfg.AWSRegion,
		AccessKeyID:     cfg.SQSPublisherAccessKeyID,
		SecretAccessKey: cfg.SQSPublisherSecretAccessKey,
		HTTPClient:      &http.Client{Timeout: sqsHTTPTimeout, Transport: transport},
	})
	return sqspublisher.New(client, cfg.SQSEventsQueue), publisherTransport{transport}
}

func newSQSCheck(p *sqspublisher.Publisher) health.Check {
	return health.Check{Name: "sqs", Fn: p.HealthCheck}
}

func registerQueueResolution(lc fx.Lifecycle, p *sqspublisher.Publisher, t publisherTransport, cfg config.Config, logger *slog.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := p.Resolve(ctx); err != nil {
				// Fx does not call OnStop for a hook whose OnStart failed.
				t.CloseIdleConnections()
				return err
			}
			logger.Info("sqs queue resolved", slog.String("queue", cfg.SQSEventsQueue))
			return nil
		},
		OnStop: func(context.Context) error {
			t.CloseIdleConnections()
			return nil
		},
	})
}

// OutboxRelayModule provides the outbox relay (ARCHITECTURE §9). The runners
// hook starts it only when RUN_OUTBOX is set.
var OutboxRelayModule = fx.Module("outbox_relay",
	fx.Provide(newRelay, newOutboxRelay),
)

func newRelay(cfg config.Config, tx app.TxRunner, publisher app.EventPublisher, clock app.Clock, ids app.IDGenerator, logger *slog.Logger) *messaging.Relay {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return messaging.NewRelay(tx, publisher, clock, messaging.RelayConfig{
		// Unique per process, even with several processes on one host (§22).
		Owner:          host + "-" + ids.NewID().String(),
		Lease:          cfg.OutboxLease,
		BatchSize:      cfg.OutboxBatchSize,
		PublishTimeout: cfg.OutboxPublishTimeout,
		BackoffBase:    cfg.OutboxBackoffBase,
		BackoffMax:     cfg.OutboxBackoffMax,
		MaxAttempts:    cfg.OutboxMaxAttempts,
	}, logger.With(slog.String("component", "outbox_relay")))
}

// outboxRelay names the relay's runner in the graph.
type outboxRelay struct{ *worker.Runner }

func newOutboxRelay(cfg config.Config, relay *messaging.Relay, logger *slog.Logger) outboxRelay {
	return outboxRelay{worker.New(worker.Config{
		Name:        "outbox_relay",
		Concurrency: cfg.OutboxRelayConcurrency,
		Idle:        cfg.OutboxPollInterval,
	}, relay.RelayOnce, logger)}
}
