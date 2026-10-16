// Package logging builds the process-wide structured JSON logger.
package logging

import (
	"io"
	"log/slog"
)

// New returns a JSON slog.Logger writing to w at the given level.
// Credentials, tokens and full financial payloads must never be logged
// through it (CHALLENGE §12).
func New(w io.Writer, level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(h).With(slog.String("service", "wager-wallet"))
}
