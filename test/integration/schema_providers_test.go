//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestSchemaProviders(t *testing.T) {
	t.Parallel()
	db := newSchemaDB(t)

	t.Run("seeded and readable by the app", func(t *testing.T) {
		expectOK(t, db.app, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT provider_id FROM providers WHERE active ORDER BY provider_id`)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			if len(ids) != 2 || ids[0] != "provider-a" || ids[1] != "provider-b" {
				t.Errorf("providers = %v, want [provider-a provider-b]", ids)
			}
			return nil
		})
	})

	t.Run("app cannot insert", func(t *testing.T) {
		expectErr(t, db.app, want{code: codePrivilege},
			exec(`INSERT INTO providers (provider_id) VALUES ('provider-c')`))
	})

	t.Run("app cannot update", func(t *testing.T) {
		expectErr(t, db.app, want{code: codePrivilege},
			exec(`UPDATE providers SET active = false WHERE provider_id = 'provider-a'`))
	})

	t.Run("app cannot delete", func(t *testing.T) {
		expectErr(t, db.app, want{code: codePrivilege},
			exec(`DELETE FROM providers WHERE provider_id = 'provider-a'`))
	})

	t.Run("id format", func(t *testing.T) {
		expectErr(t, db.owner, want{code: codeCheck, constraint: "providers_id_format"},
			exec(`INSERT INTO providers (provider_id) VALUES ('bad id')`))
	})

	t.Run("truncate blocked even for owner", func(t *testing.T) {
		expectErr(t, db.owner, want{code: codeGuard, msg: "TRUNCATE is not allowed"},
			exec(`TRUNCATE providers CASCADE`))
	})
}
