package messaging

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/medbottle/betting-wallet-go/internal/app"
)

func TestRelayBackoff(t *testing.T) {
	t0 := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	var jitterN []int64
	r := &Relay{cfg: RelayConfig{BackoffBase: time.Second, BackoffMax: 5 * time.Minute,
		Jitter: func(n int64) int64 { jitterN = append(jitterN, n); return 0 }}}
	cases := map[int]time.Duration{1: 2 * time.Second, 2: 4 * time.Second, 8: 256 * time.Second, 9: 5 * time.Minute, 20: 5 * time.Minute, 100: 5 * time.Minute}
	for attempts, want := range cases {
		if got := r.next(t0, attempts).Sub(t0); got != want {
			t.Errorf("attempts %d: delay %v, want %v", attempts, got, want)
		}
	}
	for _, n := range jitterN {
		if time.Duration(n) != time.Second {
			t.Errorf("jitter drawn from [0, %v), want [0, base)", time.Duration(n))
		}
	}
	// Real jitter stays within [delay, delay+base).
	r.cfg.Jitter = nil
	for i := 0; i < 100; i++ {
		if d := r.next(t0, 1).Sub(t0); d < 2*time.Second || d >= 3*time.Second {
			t.Fatalf("delay %v outside [2s, 3s)", d)
		}
	}
}

// countingRunner records transactions and runs none.
type countingRunner struct{ calls *atomic.Int64 }

func (c countingRunner) InTx(context.Context, app.TxOptions, func(context.Context, app.Repos) error) error {
	c.calls.Add(1)
	return nil
}

// TestRelayPausesWhileBrokerNotReady: no claim (and so no attempt) while
// the broker's readiness check fails (ARCHITECTURE §27, M-1).
func TestRelayPausesWhileBrokerNotReady(t *testing.T) {
	var calls atomic.Int64
	var ready atomic.Bool
	r := NewRelay(countingRunner{&calls}, nil, nil, RelayConfig{BrokerReady: ready.Load},
		slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)
	for range 3 {
		if did, err := r.RelayOnce(context.Background()); did || err != nil {
			t.Fatalf("not ready: did=%v err=%v", did, err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d claims while the broker was not ready", n)
	}
	ready.Store(true)
	if _, err := r.RelayOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d claims once ready, want 1", n)
	}
}
