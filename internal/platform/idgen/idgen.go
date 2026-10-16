// Package idgen generates UUIDv7 identifiers: time-ordered, so new rows
// cluster at the end of their indexes.
package idgen

import "github.com/google/uuid"

// UUIDv7 generates version 7 UUIDs.
type UUIDv7 struct{}

// NewID implements app.IDGenerator. uuid.NewV7 fails only when the system
// random source fails, which leaves the process unable to work at all, so
// that case stops the process.
func (UUIDv7) NewID() uuid.UUID { return uuid.Must(uuid.NewV7()) }
