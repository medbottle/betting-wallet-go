package migrations

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func TestLatest(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_init.up.sql":     {},
		"0001_init.down.sql":   {},
		"0003_ledger.up.sql":   {},
		"0003_ledger.down.sql": {},
		"0002_wallets.up.sql":  {},
		"README.md":            {},
	}
	v, err := latest(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if v != 3 {
		t.Errorf("latest = %d, want 3", v)
	}
}

func TestLatestEmpty(t *testing.T) {
	if _, err := latest(fstest.MapFS{}); err == nil {
		t.Fatal("expected error for empty set")
	}
}

// Every up migration must have a matching down migration so rollback is
// always possible.
func TestEmbeddedMigrationsArePaired(t *testing.T) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	ups := 0
	for name := range names {
		if base, ok := strings.CutSuffix(name, ".up.sql"); ok {
			ups++
			if !names[base+".down.sql"] {
				t.Errorf("%s has no matching down migration", name)
			}
		}
	}
	if ups == 0 {
		t.Fatal("no embedded migrations")
	}
	if _, err := Latest(); err != nil {
		t.Fatal(err)
	}
}
