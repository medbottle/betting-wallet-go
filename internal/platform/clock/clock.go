// Package clock provides the system clock (UTC).
package clock

import "time"

// System returns the current time in UTC.
type System struct{}

// Now implements app.Clock.
func (System) Now() time.Time { return time.Now().UTC() }
