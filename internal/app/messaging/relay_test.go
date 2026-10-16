package messaging

import (
	"testing"
	"time"
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
