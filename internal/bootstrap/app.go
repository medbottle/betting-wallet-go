package bootstrap

import (
	"log/slog"

	"go.uber.org/fx"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/app/wagering"
	"github.com/medbottle/betting-wallet-go/internal/app/wallets"
	"github.com/medbottle/betting-wallet-go/internal/platform/config"
)

// AppModule provides the use cases, shared by HTTP, the SQS consumer and
// the reference worker.
var AppModule = fx.Module("app",
	fx.Provide(
		newWageringService,
		newWalletService,
	),
)

func newWageringService(cfg config.Config, tx app.TxRunner, walletReader app.WalletReader, providers app.ProviderReader,
	transactions app.TransactionReader, clock app.Clock, ids app.IDGenerator, logger *slog.Logger, metrics app.Metrics) *wagering.Service {
	return wagering.NewService(tx, walletReader, providers, transactions, clock, ids, wagering.Config{
		ReferenceTTL: cfg.ReferenceTTL,
		Backoff:      wagering.Backoff{Base: cfg.ReferenceBackoffBase, Max: cfg.ReferenceBackoffMax},
	}, logger.With(slog.String("component", "wagering")), metrics)
}

func newWalletService(tx app.TxRunner, walletReader app.WalletReader, ledger app.LedgerReader, clock app.Clock,
	ids app.IDGenerator, logger *slog.Logger, metrics app.Metrics) *wallets.Service {
	return wallets.NewService(tx, walletReader, ledger, clock, ids, logger.With(slog.String("component", "wallets")), metrics)
}
