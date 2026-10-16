//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SQLSTATE codes asserted by the schema tests.
const (
	codeCheck     = "23514" // check_violation
	codeUnique    = "23505" // unique_violation
	codeFK        = "23503" // foreign_key_violation
	codeGuard     = "23000" // integrity_constraint_violation, raised by our triggers
	codePrivilege = "42501" // insufficient_privilege
)

// schemaDB is a migrated database with one pool per role.
type schemaDB struct {
	owner *pgxpool.Pool // wallet_owner: exercises triggers
	app   *pgxpool.Pool // wallet_app: exercises privileges
}

func newSchemaDB(t *testing.T) schemaDB {
	t.Helper()
	urls := startPostgres(t)
	migrateUp(t, urls.owner)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connect := func(url string) *pgxpool.Pool {
		p, err := pgxpool.New(ctx, url)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(p.Close)
		return p
	}
	return schemaDB{owner: connect(urls.owner), app: connect(urls.app)}
}

// want describes an expected database error. constraint and msg are
// checked only when non-empty.
type want struct {
	code       string
	constraint string
	msg        string
}

// inTx runs fn in a transaction that is always rolled back, so cases never
// leak rows into each other.
func inTx(t *testing.T, pool *pgxpool.Pool, fn func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return fn(ctx, tx)
}

func expectOK(t *testing.T, pool *pgxpool.Pool, fn func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	if err := inTx(t, pool, fn); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func expectErr(t *testing.T, pool *pgxpool.Pool, w want, fn func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	err := inTx(t, pool, fn)
	if err == nil {
		t.Fatalf("expected SQLSTATE %s (%s), got success", w.code, w.constraint)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a Postgres error, got %v", err)
	}
	if pgErr.Code != w.code {
		t.Fatalf("SQLSTATE = %s, want %s: %s (constraint %q)", pgErr.Code, w.code, pgErr.Message, pgErr.ConstraintName)
	}
	if w.constraint != "" && pgErr.ConstraintName != w.constraint {
		t.Fatalf("constraint = %q, want %q: %s", pgErr.ConstraintName, w.constraint, pgErr.Message)
	}
	if w.msg != "" && !strings.Contains(pgErr.Message, w.msg) {
		t.Fatalf("message %q does not contain %q", pgErr.Message, w.msg)
	}
}

// row is a column -> value map for building INSERTs. A nil value is NULL;
// an absent key takes the column default.
type row map[string]any

// with returns a copy of r with the given pairs applied.
func (r row) with(kv ...any) row {
	out := make(row, len(r)+len(kv)/2)
	for k, v := range r {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// without returns a copy of r without the given columns.
func (r row) without(cols ...string) row {
	out := r.with()
	for _, c := range cols {
		delete(out, c)
	}
	return out
}

func insert(ctx context.Context, tx pgx.Tx, table string, r row) error {
	cols := make([]string, 0, len(r))
	for c := range r {
		cols = append(cols, c)
	}
	slices.Sort(cols)
	args := make([]any, len(cols))
	ph := make([]string, len(cols))
	for i, c := range cols {
		args[i] = r[c]
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	_, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(cols, ", "), strings.Join(ph, ", ")), args...)
	return err
}

// insertAll inserts rows in order and stops at the first error.
func insertAll(ctx context.Context, tx pgx.Tx, items ...tableRow) error {
	for _, it := range items {
		if err := insert(ctx, tx, it.table, it.row); err != nil {
			return err
		}
	}
	return nil
}

type tableRow struct {
	table string
	row   row
}

func walletRow(r row) tableRow { return tableRow{"wallets", r} }
func wagerRow(r row) tableRow  { return tableRow{"wager_transactions", r} }
func ledgerRow(r row) tableRow { return tableRow{"wallet_ledger_entries", r} }

// newWallet returns a valid BRL wallet holding 100.00.
func newWallet() row {
	return row{
		"id":            uuid.New(),
		"player_id":     uuid.New(),
		"currency":      "BRL",
		"balance_minor": int64(10000),
		"version":       int64(1),
	}
}

func hash32() []byte {
	h := make([]byte, 32)
	copy(h, uuid.New().String())
	return h
}

// externalTx returns a valid PENDING external operation of the given kind
// on wallet w.
func externalTx(w row, kind string) row {
	ext := "ext-" + uuid.NewString()
	r := row{
		"id":                      uuid.New(),
		"origin":                  "EXTERNAL",
		"kind":                    kind,
		"status":                  "PENDING",
		"wallet_id":               w["id"],
		"player_id":               w["player_id"],
		"currency":                w["currency"],
		"amount_minor":            int64(2500),
		"provider_id":             "provider-a",
		"external_transaction_id": ext,
		"idempotency_key":         "provider-a:" + ext,
		"payload_hash":            hash32(),
		"hash_version":            int16(1),
		"round_id":                "round-1",
		"game_id":                 "fortune-chimp",
		"correlation_id":          "corr-" + ext,
	}
	switch kind {
	case "LOSS":
		r["amount_minor"] = int64(0)
	case "REFUND", "ROLLBACK":
		r["reference_external_transaction_id"] = "ref-" + uuid.NewString()
	}
	return r
}

// processed marks r as PROCESSED with the given observed balance/version.
func processed(r row, balance, version int64) row {
	return r.with(
		"status", "PROCESSED",
		"result_balance_minor", balance,
		"result_wallet_version", version,
		"completed_at", time.Now(),
	)
}

// rejected marks r as REJECTED with code and the observed balance/version.
func rejected(r row, code string, balance, version int64) row {
	return processed(r, balance, version).with("status", "REJECTED", "failure_code", code)
}

// reversalOf returns a PENDING reversal of kind referencing the processed
// operation ref on wallet w, already resolved to ref's internal id.
func reversalOf(w, ref row, kind string) row {
	return externalTx(w, kind).with(
		"amount_minor", ref["amount_minor"],
		"reference_external_transaction_id", ref["external_transaction_id"],
		"reference_transaction_id", ref["id"],
	)
}

// openingTx returns the PROCESSED internal OPENING for wallet w.
func openingTx(w row) row {
	return row{
		"id":                    uuid.New(),
		"origin":                "INTERNAL",
		"kind":                  "OPENING",
		"status":                "PROCESSED",
		"wallet_id":             w["id"],
		"player_id":             w["player_id"],
		"currency":              w["currency"],
		"amount_minor":          w["balance_minor"],
		"result_balance_minor":  w["balance_minor"],
		"result_wallet_version": int64(1),
		"correlation_id":        "corr-open",
		"completed_at":          time.Now(),
	}
}

// ledgerEntry returns a ledger row for transaction t on wallet w.
func ledgerEntry(w, t row, direction string, amount, before, version int64) row {
	after := before + amount
	if direction == "DEBIT" {
		after = before - amount
	}
	return row{
		"id":                   uuid.New(),
		"wallet_id":            w["id"],
		"currency":             w["currency"],
		"transaction_id":       t["id"],
		"direction":            direction,
		"amount_minor":         amount,
		"balance_before_minor": before,
		"balance_after_minor":  after,
		"wallet_version":       version,
	}
}

func exec(sql string, args ...any) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	}
}

// then runs the steps in order and stops at the first error.
func then(steps ...func(context.Context, pgx.Tx) error) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range steps {
			if err := s(ctx, tx); err != nil {
				return err
			}
		}
		return nil
	}
}

func ins(items ...tableRow) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error { return insertAll(ctx, tx, items...) }
}
