package bootstrap

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"go.uber.org/fx"

	"github.com/medbottle/betting-wallet-go/internal/adapters/httpapi"
	"github.com/medbottle/betting-wallet-go/internal/adapters/postgres"
	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/platform/config"
	"github.com/medbottle/betting-wallet-go/internal/platform/metrics"
)

// metricsFlushTimeout is the "flush metrics" budget of ARCHITECTURE §14.
// With a pull model nothing is pushed; it bounds the listener shutdown.
const metricsFlushTimeout = time.Second

// outboxGaugeTimeout bounds the scrape-time outbox query (§25, G7).
const outboxGaugeTimeout = time.Second

// MetricsModule provides the Prometheus registry as app.Metrics and serves
// it on its own listener, METRICS_ADDR (ARCHITECTURE §25, G1/G2). The hook
// is registered after the pool's, so it stops before the pool closes and
// after the runners, keeping the final counts scrapeable during the drain.
var MetricsModule = fx.Module("metrics",
	fx.Provide(
		newMetrics,
		func(r *metrics.Registry) app.Metrics { return r },
		newMetricsServer,
	),
	fx.Invoke(registerMetricsServer),
)

func newMetrics(reader *postgres.Reader, clock app.Clock, logger *slog.Logger) *metrics.Registry {
	r := metrics.New()
	r.RegisterOutbox(reader.OutboxBacklog, clock.Now, outboxGaugeTimeout, logger.With(slog.String("component", "metrics")))
	return r
}

// metricsServer is nil when METRICS_ADDR is empty.
type metricsServer struct{ *httpapi.Server }

func newMetricsServer(cfg config.Config, r *metrics.Registry, logger *slog.Logger) metricsServer {
	if cfg.MetricsAddr == "" {
		return metricsServer{}
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", r.Handler())
	return metricsServer{httpapi.NewServer(cfg.MetricsAddr, mux, cfg.HTTPReadHeaderTimeout, cfg.HTTPHandlerTimeout,
		logger.With(slog.String("component", "metrics")))}
}

func registerMetricsServer(lc fx.Lifecycle, s metricsServer, logger *slog.Logger) {
	if s.Server == nil {
		logger.Info("metrics listener disabled")
		return
	}
	lc.Append(fx.Hook{
		OnStart: s.Start,
		OnStop: func(ctx context.Context) error {
			sctx, cancel := context.WithTimeout(ctx, metricsFlushTimeout)
			defer cancel()
			return s.Shutdown(sctx)
		},
	})
}
