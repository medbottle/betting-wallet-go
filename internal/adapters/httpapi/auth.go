package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/app/authz"
)

// TokenVerifier validates a bearer token and returns the caller.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (authz.Principal, error)
}

// RequireAuth rejects requests without a valid bearer token with 401,
// WWW-Authenticate: Bearer error="invalid_token" and UNAUTHENTICATED
// (ARCHITECTURE §11/§12). A token that could not be checked because the
// IdP is unreachable (app.ErrTransient) answers 503 TEMPORARILY_UNAVAILABLE,
// retryable (§24, Q5). On success the Principal is put in the request
// context for the handlers' permission checks. Tokens are never logged.
func RequireAuth(v TokenVerifier, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				unauthenticated(w)
				return
			}
			p, err := v.Verify(r.Context(), raw)
			if errors.Is(err, app.ErrTransient) {
				logger.Warn("token verification unavailable",
					slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.String("reason", err.Error()))
				writeError(w, http.StatusServiceUnavailable, codeTemporarilyUnavailable, "token verification temporarily unavailable", true)
				return
			}
			if err != nil {
				// The cause names the failed check (issuer, expiry, ...),
				// never the token.
				logger.Info("authentication failed",
					slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.String("reason", err.Error()))
				unauthenticated(w)
				return
			}
			next.ServeHTTP(w, r.WithContext(authz.WithPrincipal(r.Context(), p)))
		})
	}
}

// bearerToken extracts the token from "Bearer <token>" (scheme is
// case-insensitive, RFC 6750).
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func unauthenticated(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "missing or invalid bearer token", false)
}
