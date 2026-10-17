// Package logging builds the process-wide structured JSON logger.
package logging

import (
	"io"
	"log/slog"
)

// New returns a JSON slog.Logger writing to w at the given level.
// Credentials, tokens and full financial payloads must never be logged
// through it (CHALLENGE §12). Durations are written as text ("1.5s"),
// not as nanosecond integers.
func New(w io.Writer, level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: readableDuration})
	return slog.New(h).With(slog.String("service", "wager-wallet"))
}

func readableDuration(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindDuration {
		return slog.String(a.Key, a.Value.Duration().String())
	}
	return a
}
