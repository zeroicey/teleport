package store

import (
	"context"
	"strings"
	"testing"

	"github.com/zeroicey/teleport/backend/internal/store/storetest"
)

// The tests here guard the migration-content check in MigratePG.
//
// That check exists because of a real incident: `teleport_test` had been created
// from an uncommitted revision of the merged baseline, so the live schema kept
// `CHECK ((metadata)::json IS NOT NULL)` while the committed file said
// `CHECK (metadata IS JSON)`. Nothing noticed, because the version is recorded
// by filename and a merged baseline is never replayed, and because every other
// drift guard compares files to files — a live database is not a participant.
// One of the real-database tests was in fact failing while the project recorded
// it as verified.
// See .ai/pitfalls/cases/2026-10-16-merged-baseline-hides-a-stale-test-db.md.

// TestPGMigrateRecordsAChecksum pins the positive half: a freshly migrated
// database records the exact hash of the migration file that built it.
func TestPGMigrateRecordsAChecksum(t *testing.T) {
	pool := storetest.Pool(t, MigratePG)
	ctx := context.Background()

	names, err := pgMigrationNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no migrations found")
	}

	for _, name := range names {
		body, err := pgMigrationFS.ReadFile("migrations_pg/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var recorded string
		if err := pool.QueryRow(ctx,
			`SELECT checksum FROM schema_migrations WHERE version = $1`, name).Scan(&recorded); err != nil {
			t.Fatalf("read checksum for %s: %v", name, err)
		}
		if recorded != migrationChecksum(body) {
			t.Errorf("%s recorded checksum %q, want %q", name, recorded, migrationChecksum(body))
		}
		// Length is asserted separately so that a checksum which is present but
		// truncated (say, a prefix stored for readability) fails here rather than
		// silently weakening every later comparison.
		if len(recorded) != 64 {
			t.Errorf("%s recorded a %d-character checksum, want 64 hex characters", name, len(recorded))
		}
	}
}

// TestPGMigrateRefusesWhenARecordedMigrationChanged is the guard for the edit
// that caused the incident: the file changes while the database still holds the
// previous revision's schema.
//
// The tampering is applied to the recorded row rather than to the embedded file,
// which is the same comparison from the other side — and it is the only side a
// test can reach without rewriting the binary.
func TestPGMigrateRefusesWhenARecordedMigrationChanged(t *testing.T) {
	pool := storetest.Pool(t, MigratePG) // migrates, so there is a recorded row
	ctx := context.Background()

	names, err := pgMigrationNames()
	if err != nil || len(names) == 0 {
		t.Fatalf("list migrations: %v (names=%v)", err, names)
	}
	target := names[0]

	if _, err := pool.Exec(ctx,
		`UPDATE schema_migrations SET checksum = $1 WHERE version = $2`,
		strings.Repeat("0", 64), target,
	); err != nil {
		t.Fatal(err)
	}

	err = MigratePG(ctx, pool)
	if err == nil {
		t.Fatalf("MigratePG accepted a database whose recorded checksum for %s no longer matches "+
			"the embedded file; the live schema would be served as if it matched", target)
	}
	if !strings.Contains(err.Error(), "has changed since it was applied") {
		t.Errorf("MigratePG refused, but not for the right reason: %v\n"+
			"(an assertion that merely expects an error cannot tell this guard apart from a "+
			"connection failure)", err)
	}
}

// TestPGMigrateRefusesAPreChecksumDatabase is the one that would have caught the
// original incident directly.
//
// A database migrated before this check existed has an empty checksum after the
// backfill ALTER. Empty must mean "cannot be verified", not "matches": treating
// it as a match would leave exactly the unverifiable databases this guard was
// written for, and the incident database was precisely one of those.
func TestPGMigrateRefusesAPreChecksumDatabase(t *testing.T) {
	pool := storetest.Pool(t, MigratePG)
	ctx := context.Background()

	// Simulate the pre-checksum world.
	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET checksum = ''`); err != nil {
		t.Fatal(err)
	}

	err := MigratePG(ctx, pool)
	if err == nil {
		t.Fatal("MigratePG accepted a database with no recorded checksums; a schema from an " +
			"unknown revision of the baseline would be treated as current")
	}
	if !strings.Contains(err.Error(), "did not record a checksum") {
		t.Errorf("MigratePG refused, but not for the right reason: %v", err)
	}
}

// TestPGMigrateIsQuietWhenNothingChanged is the other half of a guard being real:
// it must not fire when it should not, or it will be disabled the first time it
// blocks an unrelated release.
func TestPGMigrateIsQuietWhenNothingChanged(t *testing.T) {
	pool := storetest.Pool(t, MigratePG)
	ctx := context.Background()

	// storetest already migrated once; a second and third run must be no-ops.
	for i := 0; i < 2; i++ {
		if err := MigratePG(ctx, pool); err != nil {
			t.Fatalf("re-running migrations against an unchanged database failed on pass %d: %v", i+1, err)
		}
	}
}
