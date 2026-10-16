package bootstrap

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/medbottle/betting-wallet-go/internal/adapters/postgres"
	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/platform/clock"
	"github.com/medbottle/betting-wallet-go/internal/platform/config"
	"github.com/medbottle/betting-wallet-go/internal/platform/idgen"
)

// RepositoryModule provides the persistence ports, the clock and the id
// generator. It registers constructors only; the pool's lifecycle belongs
// to PostgresModule.
var RepositoryModule = fx.Module("repository",
	fx.Provide(
		fx.Annotate(newTxRunner, fx.As(new(app.TxRunner))),
		fx.Annotate(postgres.NewWalletReader, fx.As(new(app.WalletReader))),
		fx.Annotate(func() clock.System { return clock.System{} }, fx.As(new(app.Clock))),
		fx.Annotate(func() idgen.UUIDv7 { return idgen.UUIDv7{} }, fx.As(new(app.IDGenerator))),
	),
)

func newTxRunner(pool *pgxpool.Pool, cfg config.Config) *postgres.TxRunner {
	return postgres.NewTxRunner(pool, cfg.DBLockTimeout)
}
