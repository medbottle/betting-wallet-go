// Package bootstrap composes the application with Uber Fx. It is the only
// package under internal/ that imports go.uber.org/fx.
//
// Module order matters: Fx runs OnStart hooks in registration order and
// OnStop hooks in reverse (ARCHITECTURE §14). The order below gives:
//
//	start: config validated -> pool ping + schema check -> JWKS fetch -> readiness loop -> runners
//	stop:  runners (drain, then parallel shutdown) -> readiness loop -> pool close
package bootstrap

import (
	"log/slog"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
)

// StopTimeout is the total shutdown budget (ARCHITECTURE §14). The compose
// stop_grace_period (35s) must stay above it.
const StopTimeout = 30 * time.Second

// Options returns the full application graph. extra is appended last, which
// lets tests add fx.Populate or fx.Decorate without changing the wiring.
func Options(extra ...fx.Option) fx.Option {
	return fx.Options(
		fx.StopTimeout(StopTimeout),
		fx.WithLogger(func(l *slog.Logger) fxevent.Logger {
			return &fxevent.SlogLogger{Logger: l.With(slog.String("component", "fx"))}
		}),
		ConfigModule,
		LoggingModule,
		PostgresModule,
		RepositoryModule,
		AuthModule,
		HealthModule,
		HTTPModule,
		RunnersModule,
		fx.Options(extra...),
	)
}
