package wagering

import (
	"math/rand/v2"
	"time"
)

// Backoff is the retry policy of ARCHITECTURE §8:
// next = now + min(base * 2^attempts, max), with full jitter.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
	// Jitter returns a value in [0, n); nil uses math/rand/v2.
	Jitter func(n int64) int64
}

// Next returns the next attempt time after attempts failed resolutions.
func (b Backoff) Next(now time.Time, attempts int) time.Time {
	d := b.Max
	if attempts < 62 { // beyond that the shift overflows; Max applies anyway
		if step := b.Base << attempts; step > 0 && step < b.Max {
			d = step
		}
	}
	if d <= 0 {
		return now
	}
	jitter := b.Jitter
	if jitter == nil {
		jitter = rand.Int64N
	}
	return now.Add(time.Duration(jitter(int64(d))))
}
