package bootstrap

import (
	"context"
	"log/slog"
	"time"

	"go.uber.org/fx"
	"golang.org/x/sync/errgroup"

	"github.com/medbottle/betting-wallet-go/internal/adapters/httpapi"
	"github.com/medbottle/betting-wallet-go/internal/adapters/sqsconsumer"
	"github.com/medbottle/betting-wallet-go/internal/app/wagering"
	"github.com/medbottle/betting-wallet-go/internal/platform/config"
	"github.com/medbottle/betting-wallet-go/internal/platform/health"
	"github.com/medbottle/betting-wallet-go/internal/platform/worker"
)

// runnersShutdownBudget bounds the parallel shutdown group (§14: 2s-22s).
const runnersShutdownBudget = 20 * time.Second

// RunnersModule registers the single "runners" hook last, so on stop it
// runs first: readiness flips to draining, the drain delay elapses, then
// every runner shuts down concurrently while the pool is still open.
//
// Start order (ARCHITECTURE §14): reference worker, outbox relay,
// consumer, then HTTP.
var RunnersModule = fx.Module("runners",
	fx.Invoke(registerRunners),
)

// ReferenceWorkerModule provides the PENDING_REFERENCE worker (ARCHITECTURE
// §8). It is started by the runners hook only when RUN_REFERENCE_WORKER is
// set.
var ReferenceWorkerModule = fx.Module("reference_worker",
	fx.Provide(newReferenceWorker),
)

// referenceWorker names the runner so other workers can join the graph.
type referenceWorker struct{ *worker.Runner }

func newReferenceWorker(cfg config.Config, svc *wagering.Service, logger *slog.Logger) referenceWorker {
	return referenceWorker{worker.New(worker.Config{
		Name:        "reference_worker",
		Concurrency: cfg.ReferenceWorkerConcurrency,
		Idle:        cfg.ReferenceWorkerPollInterval,
	}, svc.ResolveNext, logger)}
}

type runnersParams struct {
	fx.In
	LC              fx.Lifecycle
	Cfg             config.Config
	Logger          *slog.Logger
	Readiness       *health.Readiness
	HTTP            *httpapi.Server
	ReferenceWorker referenceWorker
	OutboxRelay     outboxRelay
	Consumer        *sqsconsumer.Consumer
}

func registerRunners(p runnersParams) {
	logger := p.Logger.With(slog.String("component", "runners"))
	p.LC.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// stopStarted undoes a partial start: Fx does not call OnStop for
			// a hook whose OnStart failed.
			stopStarted := func() {
				_ = p.ReferenceWorker.Stop(ctx)
				_ = p.OutboxRelay.Stop(ctx)
				_ = p.Consumer.Stop(ctx)
			}
			if p.Cfg.RunReferenceWorker {
				if err := p.ReferenceWorker.Start(); err != nil {
					return err
				}
			}
			if p.Cfg.RunOutbox {
				if err := p.OutboxRelay.Start(); err != nil {
					stopStarted()
					return err
				}
			}
			if p.Cfg.RunConsumer {
				if err := p.Consumer.Start(); err != nil {
					stopStarted()
					return err
				}
			}
			if !p.Cfg.RunHTTP {
				logger.Info("http role disabled")
				return nil
			}
			if err := p.HTTP.Start(ctx); err != nil {
				stopStarted()
				return err
			}
			return nil
		},
		OnStop: func(ctx context.Context) error {
			p.Readiness.SetDraining()
			if d := p.Cfg.ReadinessDrainDelay; d > 0 {
				t := time.NewTimer(d)
				select {
				case <-t.C:
				case <-ctx.Done():
					t.Stop()
				}
			}

			gctx, cancel := context.WithTimeout(ctx, runnersShutdownBudget)
			defer cancel()
			var g errgroup.Group
			if p.Cfg.RunHTTP {
				g.Go(func() error { return p.HTTP.Shutdown(gctx) })
			}
			// The worker's in-flight iteration sees a cancelled context and
			// rolls back, releasing its claimed row (ARCHITECTURE §14).
			g.Go(func() error { return p.ReferenceWorker.Stop(gctx) })
			// An in-flight publish keeps its own timeout; an unmarked batch
			// is republished after its lease (ARCHITECTURE §14).
			g.Go(func() error { return p.OutboxRelay.Stop(gctx) })
			// Draining already paused polling; handlers in flight finish
			// within their deadline and unstarted messages are released
			// (ARCHITECTURE §14).
			g.Go(func() error { return p.Consumer.Stop(gctx) })
			err := g.Wait()
			logger.Info("runners stopped")
			return err
		},
	})
}
