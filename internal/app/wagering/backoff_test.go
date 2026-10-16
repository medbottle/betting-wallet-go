package wagering

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	t0 := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	var gotN []int64
	b := Backoff{Base: time.Second, Max: time.Minute, Jitter: func(n int64) int64 { gotN = append(gotN, n); return n - 1 }}

	wantCaps := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 32 * time.Second, time.Minute, time.Minute, time.Minute}
	for i, attempts := range []int{0, 1, 2, 5, 6, 30, 1000} {
		next := b.Next(t0, attempts)
		if time.Duration(gotN[i]) != wantCaps[i] {
			t.Errorf("attempts %d: jitter range %v, want %v", attempts, time.Duration(gotN[i]), wantCaps[i])
		}
		if !next.Before(t0.Add(wantCaps[i])) || next.Before(t0) {
			t.Errorf("attempts %d: next %v outside [now, now+%v)", attempts, next.Sub(t0), wantCaps[i])
		}
	}

	// Default jitter stays inside the range.
	b.Jitter = nil
	for i := 0; i < 100; i++ {
		if d := b.Next(t0, 3).Sub(t0); d < 0 || d >= 8*time.Second {
			t.Fatalf("full jitter out of range: %v", d)
		}
	}
}
