package bootstrap

import (
	"context"
	"log/slog"
	"time"

	"go.uber.org/fx"
	"golang.org/x/sync/errgroup"

	"github.com/medbottle/betting-wallet-go/internal/adapters/httpapi"
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
// Start order (ARCHITECTURE §14): reference worker, then HTTP. The outbox
// relay and the consumer join the same hook in their slices.
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
}

func registerRunners(p runnersParams) {
	logger := p.Logger.With(slog.String("component", "runners"))
	p.LC.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			for _, role := range []struct {
				name    string
				enabled bool
			}{
				{"outbox_relay", p.Cfg.RunOutbox},
				{"consumer", p.Cfg.RunConsumer},
			} {
				if role.enabled {
					logger.Info("role enabled but not implemented yet", slog.String("role", role.name))
				}
			}
			if p.Cfg.RunReferenceWorker {
				if err := p.ReferenceWorker.Start(); err != nil {
					return err
				}
			}
			if !p.Cfg.RunHTTP {
				logger.Info("http role disabled")
				return nil
			}
			if err := p.HTTP.Start(ctx); err != nil {
				// Fx does not call OnStop for a hook whose OnStart failed:
				// stop what already started.
				_ = p.ReferenceWorker.Stop(ctx)
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
			err := g.Wait()
			logger.Info("runners stopped")
			return err
		},
	})
}
