package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// CorrelationHeader carries the correlation id in and out (ARCHITECTURE
// §20, Q6).
const CorrelationHeader = "X-Correlation-Id"

type correlationKey struct{}

// withCorrelation takes X-Correlation-Id when it is 1-255 printable ASCII
// characters, otherwise generates one, stores it in the context and echoes
// it on the response.
func withCorrelation(newID func() uuid.UUID, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(CorrelationHeader)
		if !validCorrelationID(id) {
			id = newID().String()
		}
		w.Header().Set(CorrelationHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), correlationKey{}, id)))
	})
}

func correlationID(r *http.Request) string {
	id, _ := r.Context().Value(correlationKey{}).(string)
	return id
}

func validCorrelationID(s string) bool {
	if len(s) == 0 || len(s) > 255 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}
