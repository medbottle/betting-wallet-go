// Package httpapi is the HTTP transport: router, handlers, DTOs and error
// mapping. In this slice it serves only the public health endpoints.
package httpapi

import (
	"net/http"
	"time"
)

// NewRouter builds the root handler. Every route runs under
// handlerTimeout (ARCHITECTURE §14).
func NewRouter(readiness ReadinessReporter, handlerTimeout time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", handleLive)
	mux.HandleFunc("GET /health/ready", handleReady(readiness))
	return http.TimeoutHandler(mux, handlerTimeout, `{"error":{"code":"TEMPORARILY_UNAVAILABLE","message":"handler timeout","retryable":true}}`)
}
