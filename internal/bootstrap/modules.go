package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/medbottle/betting-wallet-go/internal/adapters/httpapi"
	"github.com/medbottle/betting-wallet-go/internal/adapters/postgres"
	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/app/wagering"
	"github.com/medbottle/betting-wallet-go/internal/app/wallets"
	"github.com/medbottle/betting-wallet-go/internal/platform/config"
	"github.com/medbottle/betting-wallet-go/internal/platform/health"
	"github.com/medbottle/betting-wallet-go/internal/platform/logging"
	"github.com/medbottle/betting-wallet-go/migrations"
)

// poolCloseTimeout is the pool close budget (ARCHITECTURE §14).
const poolCloseTimeout = 3 * time.Second

// readinessChecksGroup collects health.Check values from every module.
const readinessChecksGroup = `group:"readiness_checks"`

// ConfigModule loads and validates the configuration. The Invoke forces
// loading before anything else, so a bad config fails fast (§14 step 1).
var ConfigModule = fx.Module("config",
	fx.Provide(config.Load),
	fx.Invoke(func(config.Config) {}),
)

// LoggingModule provides the JSON logger.
var LoggingModule = fx.Module("logging",
	fx.Provide(newLogger),
)

func newLogger(cfg config.Config) (*slog.Logger, error) {
	level, err := cfg.SlogLevel()
	if err != nil {
		return nil, err
	}
	return logging.New(os.Stdout, level), nil
}

// PostgresModule provides the pgx pool and its readiness check. The hook
// pings and verifies the schema version on start (§14 step 2) and closes
// the pool on stop, after every component that uses it has stopped.
var PostgresModule = fx.Module("postgres",
	fx.Provide(
		newPool,
		fx.Annotate(newPostgresCheck, fx.ResultTags(readinessChecksGroup)),
	),
	fx.Invoke(registerPoolHooks),
)

func newPool(cfg config.Config) (*pgxpool.Pool, error) {
	// NewPool does not connect, so a background context is fine here; the
	// connection is verified in OnStart under the start deadline.
	return postgres.NewPool(context.Background(), cfg.DatabaseURL, cfg.DBMaxConns)
}

func newPostgresCheck(pool *pgxpool.Pool) health.Check {
	return health.Check{Name: "postgres", Fn: postgres.HealthCheck(pool)}
}

func registerPoolHooks(lc fx.Lifecycle, cfg config.Config, pool *pgxpool.Pool, logger *slog.Logger) {
	closePool := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, poolCloseTimeout)
		defer cancel()
		if err := postgres.Close(ctx, pool); err != nil {
			return err
		}
		logger.Info("postgres pool closed")
		return nil
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := verifyPostgres(ctx, cfg, pool, logger); err != nil {
				// Fx does not call OnStop for a hook whose OnStart failed,
				// so release the pool here.
				_ = closePool(context.Background())
				return err
			}
			return nil
		},
		OnStop: closePool,
	})
}

func verifyPostgres(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.DBStartupTimeout)
	defer cancel()
	if err := postgres.Ping(ctx, pool); err != nil {
		return err
	}
	want, err := migrations.Latest()
	if err != nil {
		return err
	}
	got, err := postgres.CheckSchemaVersion(ctx, pool, want)
	if err != nil {
		return err
	}
	logger.Info("postgres ready", slog.Uint64("schemaVersion", uint64(got)), slog.Uint64("expectedSchemaVersion", uint64(want)))
	return nil
}

// HealthModule provides the readiness tracker and runs its loop (§14
// step 5). SQS is not checked yet; its check joins the readiness_checks
// group in the SQS slice.
var HealthModule = fx.Module("health",
	fx.Provide(newReadiness),
	fx.Invoke(registerReadinessLoop),
)

type readinessParams struct {
	fx.In
	Cfg    config.Config
	Logger *slog.Logger
	Checks []health.Check `group:"readiness_checks"`
}

func newReadiness(p readinessParams) *health.Readiness {
	// TODO(sqs slice): add the SQS readiness check.
	return health.NewReadiness(p.Checks, p.Cfg.ReadinessInterval, p.Cfg.ReadinessCheckTimeout, p.Logger)
}

func registerReadinessLoop(lc fx.Lifecycle, r *health.Readiness) {
	// The loop owns its context; the OnStart context only bounds startup.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				defer close(done)
				r.Run(ctx)
			}()
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			cancel()
			select {
			case <-done:
				return nil
			case <-stopCtx.Done():
				return fmt.Errorf("readiness loop: %w", stopCtx.Err())
			}
		},
	})
}

// HTTPModule provides the router and the server. The server is started and
// stopped by the runners hook, not here.
var HTTPModule = fx.Module("http",
	fx.Provide(
		newRouter,
		newHTTPServer,
	),
)

type routerParams struct {
	fx.In
	Cfg       config.Config
	Readiness *health.Readiness
	Verifier  httpapi.TokenVerifier
	Wagering  *wagering.Service
	Wallets   *wallets.Service
	IDs       app.IDGenerator
	Logger    *slog.Logger
}

func newRouter(p routerParams) http.Handler {
	return httpapi.NewRouter(httpapi.Deps{
		Readiness:      p.Readiness,
		Verifier:       p.Verifier,
		Wagering:       p.Wagering,
		Wallets:        p.Wallets,
		NewID:          p.IDs.NewID,
		Logger:         p.Logger.With(slog.String("component", "http")),
		HandlerTimeout: p.Cfg.HTTPHandlerTimeout,
	})
}

func newHTTPServer(cfg config.Config, h http.Handler, logger *slog.Logger) *httpapi.Server {
	return httpapi.NewServer(cfg.HTTPAddr, h, cfg.HTTPReadHeaderTimeout, cfg.HTTPHandlerTimeout, logger.With(slog.String("component", "http")))
}
