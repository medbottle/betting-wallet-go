package postgres

import "testing"

func TestToPgx5URL(t *testing.T) {
	tests := map[string]string{
		"postgres://u:p@h:5432/db?sslmode=disable":   "pgx5://u:p@h:5432/db?sslmode=disable",
		"postgresql://u:p@h:5432/db?sslmode=disable": "pgx5://u:p@h:5432/db?sslmode=disable",
		"pgx5://u@h/db": "pgx5://u@h/db",
	}
	for in, want := range tests {
		if got := toPgx5URL(in); got != want {
			t.Errorf("toPgx5URL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedact(t *testing.T) {
	u := "postgres://owner:secret@h/db"
	got := redact("dial pgx5://owner:secret@h/db: refused", u)
	if got != "dial <redacted>: refused" {
		t.Errorf("redact = %q", got)
	}
}
