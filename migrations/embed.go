// Package migrations embeds the versioned SQL migrations so cmd/migrate and
// the application's startup schema check use exactly the same set.
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
)

// FS holds every NNNN_name.up.sql / NNNN_name.down.sql file.
//
//go:embed *.sql
var FS embed.FS

var upFile = regexp.MustCompile(`^(\d+)_[a-z0-9_]+\.up\.sql$`)

// Latest returns the highest migration version present in FS. The
// application refuses to start against a database older than this.
func Latest() (uint, error) {
	return latest(FS)
}

func latest(fsys fs.FS) (uint, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return 0, fmt.Errorf("read migrations: %w", err)
	}
	var max uint
	for _, e := range entries {
		m := upFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		if uint(v) > max {
			max = uint(v)
		}
	}
	if max == 0 {
		return 0, fmt.Errorf("no migrations found")
	}
	return max, nil
}
