package bootstrap

import (
	"context"
	"log/slog"
	"time"

	"go.uber.org/fx"
	"golang.org/x/sync/errgroup"

	"github.com/medbottle/betting-wallet-go/internal/adapters/httpapi"
	"github.com/medbottle/betting-wallet-go/internal/platform/config"
	"github.com/medbottle/betting-wallet-go/internal/platform/health"
)

// runnersShutdownBudget bounds the parallel shutdown group (§14: 2s-22s).
const runnersShutdownBudget = 20 * time.Second

// RunnersModule registers the single "runners" hook last, so on stop it
// runs first: readiness flips to draining, the drain delay elapses, then
// every runner shuts down concurrently while the pool is still open.
//
// Only the HTTP server exists in this slice. The consumer, outbox relay and
// reference worker join the same hook in their slices.
var RunnersModule = fx.Module("runners",
	fx.Invoke(registerRunners),
)

type runnersParams struct {
	fx.In
	LC        fx.Lifecycle
	Cfg       config.Config
	Logger    *slog.Logger
	Readiness *health.Readiness
	HTTP      *httpapi.Server
}

func registerRunners(p runnersParams) {
	logger := p.Logger.With(slog.String("component", "runners"))
	p.LC.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			for _, role := range []struct {
				name    string
				enabled bool
			}{
				{"reference_worker", p.Cfg.RunReferenceWorker},
				{"outbox_relay", p.Cfg.RunOutbox},
				{"consumer", p.Cfg.RunConsumer},
			} {
				if role.enabled {
					logger.Info("role enabled but not implemented yet", slog.String("role", role.name))
				}
			}
			if !p.Cfg.RunHTTP {
				logger.Info("http role disabled")
				return nil
			}
			return p.HTTP.Start(ctx)
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
			err := g.Wait()
			logger.Info("runners stopped")
			return err
		},
	})
}
