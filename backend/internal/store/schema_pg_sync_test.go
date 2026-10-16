package store

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestPGSnapshotMatchesMigrations is the drift guard for the PostgreSQL schema.
//
// It replaces the role `scripts/check-schema-sync.mjs` plays for SQLite: the
// binary embeds `migrations_pg/*.sql` and applies those, while `schema.pg.sql` at
// the repository root is the readable snapshot a human runs with `psql -f` and
// reads when asking "what does this service create?". If they diverge, the
// snapshot lies about the running schema.
//
// It is a Go test rather than a Node script because this half of the schema has
// nothing to do with the frontend, and the gate already runs `go test`.
//
// The comparison is byte-for-byte: the snapshot is *generated* by concatenating
// the migrations (scripts/gen-schema-pg.sh), so anything other than an exact
// match means it was not regenerated after an edit. Comment-level differences
// are therefore failures too — which is the point, since a stale comment in a
// schema snapshot is exactly the kind of thing that misleads the next reader.
func TestPGSnapshotMatchesMigrations(t *testing.T) {
	want, err := pgMigrationBody()
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("no PostgreSQL migrations found; the embed pattern is broken")
	}

	// The snapshot lives at the repository root. This test runs in the package
	// directory, so the path is three levels up (store → internal → backend →
	// repo root).
	path := filepath.Join("..", "..", "..", "schema.pg.sql")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\n(run scripts/gen-schema-pg.sh)", path, err)
	}

	if string(got) != want {
		t.Errorf("%s does not match the concatenated migrations.\n"+
			"The snapshot is generated: run scripts/gen-schema-pg.sh and commit the result.\n"+
			"snapshot: %d bytes, migrations: %d bytes",
			path, len(got), len(want))
	}
}

// pgMigrationNameRE is the naming convention the migrator's ordering depends on.
//
// Migrations are applied in *filename* order, and that only equals *creation*
// order because the numeric prefix is zero-padded to a fixed width — `10_x.sql`
// would sort before `2_y.sql`. Four digits is enough that nobody will hit the
// limit, but the width is only useful if it is enforced, so it is enforced here.
var pgMigrationNameRE = regexp.MustCompile(`^[0-9]{4}_[a-z0-9_]+\.sql$`)

// TestPGMigrationNamesAreWellFormed keeps the ordering assumption above honest.
func TestPGMigrationNamesAreWellFormed(t *testing.T) {
	names, err := pgMigrationNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no migrations found")
	}

	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !pgMigrationNameRE.MatchString(name) {
			t.Errorf("migration %q must match %s: the numeric prefix is zero-padded so that "+
				"filename order equals application order", name, pgMigrationNameRE)
		}
		if seen[name] {
			t.Errorf("duplicate migration name %q", name)
		}
		seen[name] = true
	}
}
