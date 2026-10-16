package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"go.uber.org/fx"

	"github.com/medbottle/betting-wallet-go/internal/adapters/httpapi"
	"github.com/medbottle/betting-wallet-go/internal/adapters/oidc"
	"github.com/medbottle/betting-wallet-go/internal/platform/config"
)

// jwksHTTPTimeout bounds one JWKS request; each request is also bound by
// its caller's context.
const jwksHTTPTimeout = 5 * time.Second

// AuthModule provides the token verifier. Its hook fetches the JWKS on
// start and fails the start if that fetch fails (ARCHITECTURE §11 and §14
// step 3, after the pool ping and before readiness).
var AuthModule = fx.Module("auth",
	fx.Provide(
		newKeySet,
		fx.Annotate(newVerifier, fx.As(new(httpapi.TokenVerifier))),
	),
	fx.Invoke(registerJWKSFetch),
)

func newKeySet(cfg config.Config) *oidc.KeySet {
	return oidc.NewKeySet(cfg.OIDCJWKSURL, &http.Client{Timeout: jwksHTTPTimeout}, cfg.OIDCJWKSMinRefreshInterval, nil)
}

func newVerifier(cfg config.Config, keys *oidc.KeySet) *oidc.Verifier {
	return oidc.NewVerifier(oidc.Config{Issuer: cfg.OIDCIssuer, Audience: cfg.OIDCAudience}, keys, nil)
}

func registerJWKSFetch(lc fx.Lifecycle, keys *oidc.KeySet, logger *slog.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := keys.Refresh(ctx); err != nil {
				return fmt.Errorf("auth: initial JWKS fetch: %w", err)
			}
			logger.Info("jwks loaded")
			return nil
		},
		OnStop: func(context.Context) error {
			keys.CloseIdleConnections()
			return nil
		},
	})
}
