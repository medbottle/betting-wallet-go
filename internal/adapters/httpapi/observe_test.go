package httpapi

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/medbottle/betting-wallet-go/internal/platform/health"
)

type recordedRequest struct {
	route string
	code  int
}

type recordingObserver struct {
	mu   sync.Mutex
	seen []recordedRequest
}

func (o *recordingObserver) ObserveHTTP(route string, code int, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, recordedRequest{route, code})
}

// TestObserverUsesRoutePatterns: the label is the ServeMux pattern, never
// the raw path with ids (ARCHITECTURE §25).
func TestObserverUsesRoutePatterns(t *testing.T) {
	obs := &recordingObserver{}
	h := NewRouter(Deps{Readiness: readyStub{}, HandlerTimeout: time.Second, Observer: obs})
	for _, path := range []string{"/health/live", "/health/ready", "/nope/0192f291-27dd-7d3f-8071-5f8685deef37"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	want := []recordedRequest{{"GET /health/live", 200}, {"GET /health/ready", 200}, {"unmatched", 404}}
	if len(obs.seen) != len(want) {
		t.Fatalf("seen %v", obs.seen)
	}
	for i := range want {
		if obs.seen[i] != want[i] {
			t.Errorf("request %d: %+v, want %+v", i, obs.seen[i], want[i])
		}
	}
}

type readyStub struct{}

func (readyStub) Report() health.Report {
	return health.Report{Ready: true, Checks: map[string]health.CheckState{}}
}
