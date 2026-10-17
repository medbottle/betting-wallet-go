package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/medbottle/betting-wallet-go/internal/app"
)

func TestClassify(t *testing.T) {
	pg := func(code, constraint string) error {
		return fmt.Errorf("exec: %w", &pgconn.PgError{Code: code, ConstraintName: constraint, Message: "boom"})
	}
	tests := []struct {
		name string
		err  error
		want error // nil: returned unchanged
	}{
		{"serialization failure", pg("40001", ""), app.ErrTransient},
		{"deadlock", pg("40P01", ""), app.ErrTransient},
		{"lock timeout", pg("55P03", ""), app.ErrTransient},
		{"query canceled (statement_timeout)", pg("57014", ""), app.ErrTransient},
		{"connection exception", pg("08006", ""), app.ErrTransient},
		{"terminated by an administrator", pg("57P01", ""), app.ErrTransient},
		{"crash shutdown", pg("57P02", ""), app.ErrTransient},
		{"cannot connect now", pg("57P03", ""), app.ErrTransient},
		{"too many connections", pg("53300", ""), app.ErrTransient},
		{"internal error", pg("XX000", ""), nil},
		{"context cancelled", fmt.Errorf("query: %w", context.Canceled), app.ErrTransient},
		{"context deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), app.ErrTransient},
		{"duplicate wallet", pg("23505", "wallets_player_currency_key"), app.ErrWalletAlreadyExists},
		{"other unique", pg("23505", "wager_transactions_one_reversal_per_reference"), app.ErrInvariantViolation},
		{"check", pg("23514", "wallets_balance_non_negative"), app.ErrInvariantViolation},
		{"foreign key", pg("23503", "wager_transactions_wallet_fk"), app.ErrInvariantViolation},
		{"trigger guard", pg("23000", ""), app.ErrInvariantViolation},
		{"read only transaction", pg("25006", ""), nil},
		{"plain error", errors.New("boom"), nil},
	}
	for _, tt := range tests {
		got := classify(tt.err)
		if tt.want == nil {
			if got != tt.err {
				t.Errorf("%s: classify changed the error to %v", tt.name, got)
			}
			continue
		}
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: classify = %v, want %v", tt.name, got, tt.want)
		}
		if !errors.Is(got, errors.Unwrap(tt.err)) && !errors.Is(got, tt.err) {
			t.Errorf("%s: cause lost", tt.name)
		}
	}
	if classify(nil) != nil {
		t.Error("classify(nil) must be nil")
	}
	// Context errors stay detectable.
	if got := classify(context.Canceled); !errors.Is(got, context.Canceled) {
		t.Errorf("context.Canceled lost: %v", got)
	}
}

func TestNotFound(t *testing.T) {
	if err := notFound(pgx.ErrNoRows, "x"); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("ErrNoRows = %v", err)
	}
	if err := notFound(&pgconn.PgError{Code: "40P01"}, "x"); !errors.Is(err, app.ErrTransient) {
		t.Errorf("deadlock = %v", err)
	}
}
