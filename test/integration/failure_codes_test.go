//go:build integration

package integration

import (
	"context"
	"regexp"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
)

// TestFailureCodesMatchSchema keeps the domain's persisted codes (category
// B for REJECTED, D for FAILED) identical to the wager_transactions CHECK,
// so a code added on one side only fails here.
func TestFailureCodesMatchSchema(t *testing.T) {
	t.Parallel()
	db := newSchemaDB(t)

	var def string
	expectOK(t, db.owner, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'wager_transactions_failure_code'`).Scan(&def)
	})

	var inSchema []string
	for _, m := range regexp.MustCompile(`'([A-Z_]+)'::text`).FindAllStringSubmatch(def, -1) {
		switch m[1] {
		case "REJECTED", "FAILED": // status literals in the CASE
			continue
		}
		inSchema = append(inSchema, m[1])
	}

	var inDomain []string
	for _, c := range append(wager.CodesIn(wager.CategoryRejection), wager.CodesIn(wager.CategoryPermanent)...) {
		inDomain = append(inDomain, string(c))
	}
	slices.Sort(inSchema)
	inSchema = slices.Compact(inSchema)
	slices.Sort(inDomain)
	if !slices.Equal(inSchema, inDomain) {
		t.Fatalf("failure codes drifted\nschema: %v\ndomain: %v", inSchema, inDomain)
	}
}
