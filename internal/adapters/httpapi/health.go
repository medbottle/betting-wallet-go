package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/medbottle/betting-wallet-go/internal/platform/health"
)

// ReadinessReporter exposes the cached readiness snapshot.
type ReadinessReporter interface {
	Report() health.Report
}

type liveResponse struct {
	Status string `json:"status"`
}

type readyResponse struct {
	Status string                       `json:"status"`
	Checks map[string]health.CheckState `json:"checks"`
}

// handleLive reports process liveness. It never touches dependencies.
func handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, liveResponse{Status: "ok"})
}

// handleReady reports readiness from the cached snapshot. Check failures
// are exposed only as "fail"; error details stay in the logs because this
// endpoint is public.
func handleReady(r ReadinessReporter) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		rep := r.Report()
		resp := readyResponse{Status: "ready", Checks: rep.Checks}
		code := http.StatusOK
		switch {
		case rep.Draining:
			resp.Status, code = "draining", http.StatusServiceUnavailable
		case !rep.Ready:
			resp.Status, code = "not_ready", http.StatusServiceUnavailable
		}
		writeJSON(w, code, resp)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
