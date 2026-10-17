// Package httpapi is the HTTP transport: router, handlers, DTOs and error
// mapping (ARCHITECTURE §13). Business routes always sit behind
// RequireAuth; /health/* is public.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/app/wagering"
	"github.com/medbottle/betting-wallet-go/internal/app/wallets"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
)

// WageringService is what the wagering handlers need.
type WageringService interface {
	Submit(ctx context.Context, cmd wagering.SubmitCommand) (wagering.SubmitResult, error)
	Get(ctx context.Context, id uuid.UUID) (*wager.WagerTransaction, error)
	GetByExternalID(ctx context.Context, providerID, externalTransactionID string) (*wager.WagerTransaction, error)
}

// WalletService is what the wallet handlers need.
type WalletService interface {
	Open(ctx context.Context, cmd wallets.OpenCommand) (*wallet.Wallet, error)
	Get(ctx context.Context, walletID uuid.UUID) (*wallet.Wallet, error)
	Ledger(ctx context.Context, walletID uuid.UUID, cursor string, limit int) (wallets.LedgerPage, error)
	Reconcile(ctx context.Context, walletID uuid.UUID, correlationID string) (wallets.Reconciliation, error)
}

// Deps are the router's collaborators. Without a Verifier only the public
// health routes are registered: a business route is never served
// unauthenticated.
type Deps struct {
	Readiness      ReadinessReporter
	Verifier       TokenVerifier
	Wagering       WageringService
	Wallets        WalletService
	NewID          func() uuid.UUID
	Logger         *slog.Logger
	HandlerTimeout time.Duration
	// Observer, when set, records each request's duration by route pattern
	// and status code (ARCHITECTURE §25).
	Observer RequestObserver
}

// RequestObserver records HTTP request durations.
type RequestObserver interface {
	ObserveHTTP(route string, code int, d time.Duration)
}

// NewRouter builds the root handler. Every route runs under the handler
// timeout (ARCHITECTURE §14) and gets a correlation id (§20, Q6).
func NewRouter(d Deps) http.Handler {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.NewID == nil {
		d.NewID = uuid.New
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", handleLive)
	mux.HandleFunc("GET /health/ready", handleReady(d.Readiness))

	if d.Verifier != nil {
		h := handlers{wagering: d.Wagering, wallets: d.Wallets, logger: d.Logger}
		auth := RequireAuth(d.Verifier, d.Logger)
		route := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, auth(fn)) }

		route("POST /wagering/transactions", h.submit)
		route("GET /wagering/transactions/{transactionId}", h.getTransaction)
		route("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", h.getProviderTransaction)

		route("POST /wallets", h.openWallet)
		route("GET /wallets/{walletId}", h.getWallet)
		route("GET /wallets/{walletId}/ledger", h.listLedger)
		route("POST /wallets/{walletId}/reconciliation", h.reconcile)
	}

	timeoutBody := `{"error":{"code":"TEMPORARILY_UNAVAILABLE","message":"handler timeout","retryable":true}}`
	var h http.Handler = mux
	if d.Observer != nil {
		h = observe(d.Observer, mux)
	}
	return withCorrelation(d.NewID, timeoutContract(http.TimeoutHandler(h, d.HandlerTimeout, timeoutBody)))
}

// timeoutContract gives the handler-timeout answer of http.TimeoutHandler
// the same contract as every other 503 (ARCHITECTURE §13, §27 L-3): JSON
// and Retry-After. That answer is the only 503 written without a
// Content-Type; the handlers' own responses always set one, so they pass
// through unchanged.
func timeoutContract(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(timeoutWriter{w}, r)
	})
}

type timeoutWriter struct{ http.ResponseWriter }

func (w timeoutWriter) WriteHeader(code int) {
	if code == http.StatusServiceUnavailable && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", retryAfterSeconds)
	}
	w.ResponseWriter.WriteHeader(code)
}

type handlers struct {
	wagering WageringService
	wallets  WalletService
	logger   *slog.Logger
}
