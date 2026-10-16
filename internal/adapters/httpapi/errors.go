package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/app/authz"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
)

// Transport-level codes (ARCHITECTURE §12 and §20).
const (
	codeUnauthenticated         = "UNAUTHENTICATED"
	codeForbidden               = "FORBIDDEN"
	codeProviderMismatch        = "PROVIDER_MISMATCH"
	codeTransactionNotFound     = "TRANSACTION_NOT_FOUND"
	codeTemporarilyUnavailable  = string(wager.CodeTemporarilyUnavailable)
	codeInternalInvariantFailed = string(wager.CodeInternalInvariantViolation)
)

// retryAfterSeconds is sent with every 503 (ARCHITECTURE §20, Q8): one
// readiness interval, so a retry comes after at least one more health
// check.
const retryAfterSeconds = "2"

// errorBody is the error response of ARCHITECTURE §13:
// {"error":{"code","message","retryable"}}.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func writeError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", retryAfterSeconds)
	}
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message, Retryable: retryable}})
}

// writeAppError maps use-case and authorization errors to the contract of
// ARCHITECTURE §13. Only the mapping lives here; the classification is the
// domain's and the application's.
func (h handlers) writeAppError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *wager.ValidationError
	var ce *wager.ConflictError
	switch {
	case errors.As(err, &ve):
		status := http.StatusBadRequest
		if ve.Code == wager.CodeWalletNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, string(ve.Code), ve.Error(), false)
	case errors.As(err, &ce):
		writeError(w, http.StatusConflict, string(ce.Code), ce.Msg, false)
	case errors.Is(err, authz.ErrProviderMismatch):
		writeError(w, http.StatusForbidden, codeProviderMismatch, "providerId does not match the authenticated provider", false)
	case errors.Is(err, authz.ErrForbidden), errors.Is(err, app.ErrProviderNotAllowed):
		writeError(w, http.StatusForbidden, codeForbidden, "not allowed", false)
	case errors.Is(err, authz.ErrNotFound), errors.Is(err, app.ErrNotFound):
		writeError(w, http.StatusNotFound, codeTransactionNotFound, "transaction not found", false)
	case errors.Is(err, app.ErrInvariantViolation):
		h.logError(r, err)
		writeError(w, http.StatusInternalServerError, codeInternalInvariantFailed, "internal invariant violation", false)
	default:
		// Transient by classification, or unclassified: nothing was
		// persisted and every write is idempotent, so a retry is safe.
		h.logError(r, err)
		writeError(w, http.StatusServiceUnavailable, codeTemporarilyUnavailable, "temporarily unavailable, retry later", true)
	}
}

func (h handlers) logError(r *http.Request, err error) {
	h.logger.Error("request failed",
		slog.String("method", r.Method), slog.String("path", r.URL.Path),
		slog.String("correlationId", correlationID(r)), slog.String("error", err.Error()))
}
