//go:build integration

package integration

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestSchemaGrants pins the exact table privileges of wallet_app
// (ARCHITECTURE §15/§16): never DELETE or TRUNCATE anywhere, no UPDATE on
// the ledger, read-only on the provider registry and bookkeeping tables.
func TestSchemaGrants(t *testing.T) {
	t.Parallel()
	db := newSchemaDB(t)

	wantGrants := map[string]string{
		"inbox_messages":        "INSERT,SELECT,UPDATE",
		"outbox_events":         "INSERT,SELECT,UPDATE",
		"providers":             "SELECT",
		"schema_info":           "SELECT",
		"schema_migrations":     "SELECT",
		"wager_transactions":    "INSERT,SELECT,UPDATE",
		"wallet_ledger_entries": "INSERT,SELECT",
		"wallets":               "INSERT,SELECT,UPDATE",
	}

	got := map[string]string{}
	expectOK(t, db.owner, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT c.relname, string_agg(a.privilege_type, ',' ORDER BY a.privilege_type)
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
			CROSS JOIN LATERAL aclexplode(c.relacl) a
			JOIN pg_roles r ON r.oid = a.grantee AND r.rolname = 'wallet_app'
			WHERE c.relkind = 'r'
			GROUP BY c.relname`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var table, privs string
			if err := rows.Scan(&table, &privs); err != nil {
				return err
			}
			got[table] = privs
		}
		return rows.Err()
	})

	for _, table := range slices.Sorted(maps.Keys(wantGrants)) {
		if got[table] != wantGrants[table] {
			t.Errorf("%s: wallet_app has %q, want %q", table, got[table], wantGrants[table])
		}
	}
	for table, privs := range got {
		if _, ok := wantGrants[table]; !ok {
			t.Errorf("unexpected grant on %s: %q", table, privs)
		}
	}

	t.Run("app owns nothing and cannot create tables", func(t *testing.T) {
		expectErr(t, db.app, want{code: codePrivilege}, exec(`CREATE TABLE app_scratch (i int)`))
	})
}
