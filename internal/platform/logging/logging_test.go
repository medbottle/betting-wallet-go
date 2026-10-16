package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestNewWritesJSON(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, slog.LevelInfo)
	l.Debug("hidden")
	l.Info("hello", slog.String("walletId", "w-1"))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not a single JSON record: %v: %q", err, buf.String())
	}
	if rec["msg"] != "hello" || rec["walletId"] != "w-1" || rec["service"] != "wager-wallet" {
		t.Errorf("unexpected record: %v", rec)
	}
}
