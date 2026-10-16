package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/medbottle/betting-wallet-go/internal/platform/health"
)

type fakeReporter struct{ rep health.Report }

func (f fakeReporter) Report() health.Report { return f.rep }

func TestHealthEndpoints(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		rep        health.Report
		wantCode   int
		wantStatus string
	}{
		{"live", "/health/live", health.Report{}, http.StatusOK, "ok"},
		{"ready", "/health/ready", health.Report{Ready: true, Checks: map[string]health.CheckState{"postgres": health.StateOK}}, http.StatusOK, "ready"},
		{"not ready", "/health/ready", health.Report{Checks: map[string]health.CheckState{"postgres": health.StateFail}}, http.StatusServiceUnavailable, "not_ready"},
		{"draining", "/health/ready", health.Report{Draining: true, Checks: map[string]health.CheckState{"postgres": health.StateOK}}, http.StatusServiceUnavailable, "draining"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewRouter(fakeReporter{tt.rep}, time.Second)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != tt.wantCode {
				t.Errorf("code = %d, want %d", rec.Code, tt.wantCode)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
			var body struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body: %v", err)
			}
			if body.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", body.Status, tt.wantStatus)
			}
		})
	}
}

func TestHealthRejectsOtherMethods(t *testing.T) {
	h := NewRouter(fakeReporter{}, time.Second)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/health/live", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d, want 405", rec.Code)
	}
}
