package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/medbottle/betting-wallet-go/internal/app"
)

// transientCodes are the SQLSTATEs ARCHITECTURE §4 treats as transient:
// rollback and retry (HTTP 503, SQS redelivery).
var transientCodes = map[string]bool{
	"40001": true, // serialization_failure
	"40P01": true, // deadlock_detected
	"55P03": true, // lock_not_available (lock_timeout)
	"57014": true, // query_canceled (statement_timeout or a cancel; ARCHITECTURE §24, Q2)
}

// unavailableCodes are server-side outages, also transient: the connection
// was terminated or the server cannot take it (ARCHITECTURE §27, H-1).
// Class 53 (insufficient resources) is matched by prefix. They are not
// concurrency conflicts, so they stay out of the metric of transientCodes.
var unavailableCodes = map[string]bool{
	"57P01": true, // admin_shutdown (fast shutdown, pg_terminate_backend)
	"57P02": true, // crash_shutdown
	"57P03": true, // cannot_connect_now (starting up or shutting down)
}

// constraintSentinels maps unique constraints that carry a business meaning
// to their sentinel; every other integrity violation is an invariant
// violation.
var constraintSentinels = map[string]error{
	"wallets_player_currency_key": app.ErrWalletAlreadyExists,
}

// classify wraps err with the app sentinel it belongs to, keeping the cause
// for logs. Errors that are not database errors are returned unchanged.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", app.ErrTransient, err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case transientCodes[pgErr.Code], unavailableCodes[pgErr.Code],
			strings.HasPrefix(pgErr.Code, "08"), // connection exception
			strings.HasPrefix(pgErr.Code, "53"): // insufficient resources
			return fmt.Errorf("%w: %w", app.ErrTransient, err)
		case constraintSentinels[pgErr.ConstraintName] != nil && pgErr.Code == "23505":
			return fmt.Errorf("%w: %w", constraintSentinels[pgErr.ConstraintName], err)
		case strings.HasPrefix(pgErr.Code, "23"): // integrity_constraint_violation class, triggers included
			return fmt.Errorf("%w: %w", app.ErrInvariantViolation, err)
		}
		return err
	}
	var connectErr *pgconn.ConnectError
	var netErr net.Error
	if pgconn.Timeout(err) || pgconn.SafeToRetry(err) || errors.As(err, &connectErr) || errors.As(err, &netErr) {
		return fmt.Errorf("%w: %w", app.ErrTransient, err)
	}
	return err
}

// notFound turns pgx.ErrNoRows into app.ErrNotFound and classifies the rest.
func notFound(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", app.ErrNotFound, what)
	}
	return classify(err)
}

// corrupt reports a row that does not rehydrate into a valid domain value.
func corrupt(err error, what string) error {
	return fmt.Errorf("%w: %s: %w", app.ErrInvariantViolation, what, err)
}
